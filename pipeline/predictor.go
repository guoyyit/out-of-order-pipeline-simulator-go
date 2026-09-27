package pipeline

// BranchPredictor supplies a deterministic taken/not-taken prediction at
// fetch time and is notified of outcomes at execute time. It uses a
// direct-mapped table of 2-bit saturating counters indexed by the
// branch PC. The update order is driven entirely by the pipeline, so the
// same program/seed always yields the same predictions.
type BranchPredictor struct {
	counters []uint8 // 0..3; states 0,1 predict not-taken, 2,3 taken
	mask     uint64
	enabled  bool
}

func newBranchPredictor(cfg Config) *BranchPredictor {
	size := 16
	p := &BranchPredictor{
		counters: make([]uint8, size),
		mask:     uint64(size - 1),
		enabled:  cfg.EnablePredictor,
	}
	// Warm the table from the deterministic seed so different seeds can
	// produce different (but reproducible) initial bias.
	r := newRNG(cfg.Seed)
	for i := range p.counters {
		p.counters[i] = uint8(r.intn(4))
	}
	return p
}

func (p *BranchPredictor) index(pc int64) uint64 {
	return (uint64(pc) >> 2) & p.mask
}

// predict returns the predicted direction for the branch at pc.
func (p *BranchPredictor) predict(seq int, pc int64) bool {
	if !p.enabled {
		return false
	}
	return p.counters[p.index(pc)] >= 2
}

// resolve updates the counter with the actual outcome.
func (p *BranchPredictor) resolve(seq int, pc, target int64, taken bool) {
	i := p.index(pc)
	switch {
	case taken && p.counters[i] < 3:
		p.counters[i]++
	case !taken && p.counters[i] > 0:
		p.counters[i]--
	}
}

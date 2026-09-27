package pipeline

// rngSource is a tiny deterministic xorshift64* PRG so that scheduling
// tie-breaks and predictor randomization are reproducible from the seed.
type rngSource struct {
	state uint64
}

func newRNG(seed int64) *rngSource {
	s := uint64(seed)
	if s == 0 {
		s = 0x9E3779B97F4A7C15
	}
	return &rngSource{state: s}
}

func (r *rngSource) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545F4914F6CDD1D
}

// intn returns a deterministic value in [0,n); n must be positive.
func (r *rngSource) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(r.next() % uint64(n))
}

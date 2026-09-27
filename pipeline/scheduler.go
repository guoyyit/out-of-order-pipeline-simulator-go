package pipeline

// IssueEntry tracks one renamed instruction waiting to execute.
type IssueEntry struct {
	Tag       Tag
	Seq       int
	Issued    bool
	Executing bool
	Done      bool
	// Source operands: when the producing tag has written back, the
	// scheduler records the value in SrcV and clears the tag.
	Src1Tag, Src2Tag Tag
	Src1V, Src2V     int64
	// Store data operand (for OpStore).
	SrcStoreTag Tag
	SrcStoreV   int64
}

func (e *IssueEntry) ready() bool {
	return !e.Issued && e.Src1Tag == InvalidTag && e.Src2Tag == InvalidTag && e.SrcStoreTag == InvalidTag
}

// Scheduler is the issue/select window. It is consulted once per cycle;
// ready entries are selected deterministically (oldest tag first, with a
// seed-driven permutation applied to ties so identical seeds always
// yield identical issue orders).
type Scheduler struct {
	cfg     Config
	rng     *rngSource
	entries map[Tag]*IssueEntry
}

func newScheduler(cfg Config) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		rng:     newRNG(cfg.Seed ^ 0x517cc1b727220a95),
		entries: make(map[Tag]*IssueEntry),
	}
}

func (s *Scheduler) reset() {
	s.entries = make(map[Tag]*IssueEntry)
	s.rng = newRNG(s.cfg.Seed ^ 0x517cc1b727220a95)
}

func (s *Scheduler) add(e *IssueEntry) { s.entries[e.Tag] = e }

// writeback broadcasts a completed physical result to all waiting entries.
func (s *Scheduler) writeback(tag Tag, value int64) {
	for _, e := range s.entries {
		if e.Src1Tag == tag {
			e.Src1Tag = InvalidTag
			e.Src1V = value
		}
		if e.Src2Tag == tag {
			e.Src2Tag = InvalidTag
			e.Src2V = value
		}
		if e.SrcStoreTag == tag {
			e.SrcStoreTag = InvalidTag
			e.SrcStoreV = value
		}
	}
}

// remove drops an entry after it is no longer needed.
func (s *Scheduler) remove(tag Tag) { delete(s.entries, tag) }

func (s *Scheduler) get(tag Tag) *IssueEntry { return s.entries[tag] }

func (s *Scheduler) squash(tags map[Tag]bool) {
	for t := range s.entries {
		if tags[t] {
			delete(s.entries, t)
		}
	}
}

// selectReady returns up to width ready, unissued entries in the exact
// order they should issue this cycle.
func (s *Scheduler) selectReady(width int) []*IssueEntry {
	var ready []*IssueEntry
	for _, e := range s.entries {
		if e.ready() {
			ready = append(ready, e)
		}
	}
	// Program order is the canonical order; tags are allocated in
	// program order so a stable sort by tag is deterministic.
	for i := 1; i < len(ready); i++ {
		for j := i; j > 0 && ready[j-1].Tag > ready[j].Tag; j-- {
			ready[j-1], ready[j] = ready[j], ready[j-1]
		}
	}
	if len(ready) > width {
		ready = ready[:width]
	}
	return ready
}

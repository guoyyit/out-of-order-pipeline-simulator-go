package pipeline

import "sort"

// lsqEntry is one load or store reservation held in program order.
type lsqEntry struct {
	Tag        Tag
	Seq        int
	IsStore    bool
	Addr       int64
	HasAddr    bool
	Value      int64
	HasValue   bool // store data ready
	ValueTag   Tag  // producer of store data, if pending
	ValueReady bool // value (or ValueTag writeback) available
	Completed  bool // load has read / store is address+data ready
}

// MemoryUnit owns the architectural data memory and the load/store
// queue. Memory ordering is enforced cooperatively with the pipeline:
//   - a load may execute only when every older store has a known address
//     (PolicyStall) or may speculatively execute and later be replayed if
//     a resolved older store aliases it (PolicyAssumeAlias);
//   - stores never write memory before commit, so in-order commit alone
//     makes WAW and WAR orderings precise and squash recovery trivial.
type MemoryUnit struct {
	cfg   Config
	mem   map[int64]int64
	queue []*lsqEntry // program order
}

func newMemoryUnit(cfg Config) *MemoryUnit {
	return &MemoryUnit{cfg: cfg, mem: make(map[int64]int64)}
}

func (m *MemoryUnit) reset() {
	m.mem = make(map[int64]int64)
	m.queue = nil
}

func (m *MemoryUnit) push(e *lsqEntry) { m.queue = append(m.queue, e) }

// entry returns the LSQ entry for tag.
func (m *MemoryUnit) entry(tag Tag) *lsqEntry {
	for _, e := range m.queue {
		if e.Tag == tag {
			return e
		}
	}
	return nil
}

// olderStoreAdderssKnown reports whether every store older than tag has
// resolved its address.
func (m *MemoryUnit) olderStoresAddressKnown(tag Tag) bool {
	for _, e := range m.queue {
		if e.Tag >= tag {
			break
		}
		if e.IsStore && !e.HasAddr {
			return false
		}
	}
	return true
}

// olderAliasStore returns the closest older store that is known to alias
// addr, or nil.
func (m *MemoryUnit) olderAliasStore(tag Tag, addr int64) *lsqEntry {
	var hit *lsqEntry
	for _, e := range m.queue {
		if e.Tag >= tag {
			break
		}
		if e.IsStore && e.HasAddr && e.Addr == addr {
			hit = e // keep the newest (closest) alias
		}
	}
	return hit
}

// aliasesUnresolvedOlderStore reports that an older, still address-unknown
// store might touch addr (the conservative check used by PolicyStall).
func (m *MemoryUnit) aliasesUnresolvedOlderStore(tag Tag) bool {
	for _, e := range m.queue {
		if e.Tag >= tag {
			break
		}
		if e.IsStore && !e.HasAddr {
			return true
		}
	}
	return false
}

// read performs an architectural read (word granularity).
func (m *MemoryUnit) read(addr int64) int64 { return m.mem[addr] }

// write performs an architectural write; it returns the previous value.
func (m *MemoryUnit) write(addr, value int64) int64 {
	old := m.mem[addr]
	m.mem[addr] = value
	return old
}

// remove drops the entry for tag after commit.
func (m *MemoryUnit) remove(tag Tag) {
	out := m.queue[:0]
	for _, e := range m.queue {
		if e.Tag != tag {
			out = append(out, e)
		}
	}
	m.queue = out
}

// squash drops the entry at boundaryTag (which may itself not be a
// memory op) and every younger memory entry, returning removed entries
// youngest-first. Because tags are globally monotonic, "younger" is
// simply any tag >= boundaryTag, which also handles a boundary that has
// no load/store-queue entry of its own.
func (m *MemoryUnit) squash(boundaryTag Tag) []*lsqEntry {
	keep := m.queue[:0]
	var dropped []*lsqEntry
	for _, e := range m.queue {
		if e.Tag >= boundaryTag {
			dropped = append(dropped, e)
		} else {
			keep = append(keep, e)
		}
	}
	m.queue = keep
	sort.SliceStable(dropped, func(i, j int) bool { return dropped[i].Tag > dropped[j].Tag })
	return dropped
}

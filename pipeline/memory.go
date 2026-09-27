package pipeline

// UnknownAddrPolicy selects how loads are ordered against stores whose
// address is not yet known. The policy is configurable and reproducible.
type UnknownAddrPolicy int

const (
	// PolicyConservative stalls a load until every older store has a known
	// address and has been checked for aliasing.
	PolicyConservative UnknownAddrPolicy = iota
	// PolicyNoAlias assumes unknown-address stores never alias (unsafe,
	// provided only for experimentation).
	PolicyNoAlias
)

// Memory is a flat word-addressed data memory. It is only mutated at
// commit time so uncommitted stores are never observable.
type Memory struct {
	words map[uint64]int64
	size  uint64
}

func newMemory(size uint64) *Memory {
	return &Memory{words: make(map[uint64]int64), size: size}
}

func (m *Memory) valid(addr uint64) bool {
	return addr < m.size
}

func (m *Memory) read(addr uint64) (int64, bool) {
	if !m.valid(addr) {
		return 0, false
	}
	return m.words[addr], true
}

// write commits a store. It is only called in program order at commit.
func (m *Memory) write(addr uint64, v int64) bool {
	if !m.valid(addr) {
		return false
	}
	m.words[addr] = v
	return true
}

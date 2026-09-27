package pipeline

// physReg is a physical register index. Indices 0..NumArchRegs-1 are the
// initial mappings of the architectural registers.
type physReg int

const invalidPhys physReg = -1

// physFile is the physical register file: versioned storage that lets one
// logical register move between versions without losing updates or reading
// stale values. A value is only visible to consumers once its producer has
// written back (ready=true); committed state is derived separately so
// uncommitted values are never observable from outside.
type physFile struct {
	values []int64
	ready  []bool
	free   []physReg
}

func newPhysFile(numArch, numPhys int) *physFile {
	f := &physFile{
		values: make([]int64, numPhys),
		ready:  make([]bool, numPhys),
	}
	// Architectural registers start mapped to physical 0..numArch-1 and
	// are ready with value 0. Everything else is free.
	for i := 0; i < numPhys; i++ {
		if i < numArch {
			f.ready[i] = true
		} else {
			f.free = append(f.free, physReg(i))
		}
	}
	return f
}

// alloc takes a free physical register and marks it not-ready.
func (f *physFile) alloc() physReg {
	n := len(f.free)
	if n == 0 {
		return invalidPhys
	}
	p := f.free[n-1]
	f.free = f.free[:n-1]
	f.ready[p] = false
	f.values[p] = 0
	return p
}

// release returns a physical register to the free list.
func (f *physFile) release(p physReg) {
	if p == invalidPhys {
		return
	}
	f.free = append(f.free, p)
}

func (f *physFile) write(p physReg, v int64) {
	f.values[p] = v
	f.ready[p] = true
}

func (f *physFile) isReady(p physReg) bool { return f.ready[p] }

func (f *physFile) read(p physReg) int64 { return f.values[p] }

// renameTable maps architectural registers to their current physical
// version.
type renameTable struct {
	mapping []physReg
}

func newRenameTable(numArch int) *renameTable {
	t := &renameTable{mapping: make([]physReg, numArch)}
	for i := range t.mapping {
		t.mapping[i] = physReg(i)
	}
	return t
}

func (t *renameTable) lookup(arch int) physReg {
	if arch < 0 || arch >= len(t.mapping) {
		return invalidPhys
	}
	return t.mapping[arch]
}

// rename allocates a new physical version for arch and returns (new, old).
func (t *renameTable) rename(arch int, f *physFile) (physReg, physReg) {
	old := t.mapping[arch]
	nw := f.alloc()
	t.mapping[arch] = nw
	return nw, old
}

package pipeline

// robState is the lifecycle state of a reorder-buffer entry.
type robState int

const (
	robIssued robState = iota
	robExecuting
	robCompleted
)

// robEntry tracks one in-flight instruction. Results stay invisible until
// the entry commits in program order.
type robEntry struct {
	pc       int
	op       Op
	state    robState
	dest     physReg // new physical destination, invalidPhys if none
	oldDest  physReg // previous mapping, reclaimed at commit
	archRd   int     // architectural destination register, -1 if none
	src1     physReg // physical source operands, invalidPhys if none
	src2     physReg
	predPC   int // predicted next pc (branches)
	value    int64
	exc      *Exception
	branch   bool // may change control flow
	resolved bool // branch outcome known
	mispred  bool // predicted path was wrong
	actualPC int  // actual next pc once resolved
	isStore  bool
	isLoad   bool
	addr     uint64
	addrOK   bool
	// store data kept in the ROB until commit so uncommitted stores are
	// never observable in Memory
	storeVal int64
}

// reorderBuffer is a circular in-order queue of robEntry.
type reorderBuffer struct {
	entries []robEntry
	head    int
	count   int
}

func newReorderBuffer(cap int) *reorderBuffer {
	return &reorderBuffer{entries: make([]robEntry, cap)}
}

func (r *reorderBuffer) full() bool  { return r.count == len(r.entries) }
func (r *reorderBuffer) empty() bool { return r.count == 0 }

// push appends an entry at the tail and returns its slot index.
func (r *reorderBuffer) push(e robEntry) int {
	if r.full() {
		return -1
	}
	idx := (r.head + r.count) % len(r.entries)
	r.entries[idx] = e
	r.count++
	return idx
}

// pop removes the head entry.
func (r *reorderBuffer) pop() {
	if r.empty() {
		return
	}
	r.head = (r.head + 1) % len(r.entries)
	r.count--
}

// tail returns the slot index of the youngest entry, or -1 if empty.
func (r *reorderBuffer) tail() int {
	if r.empty() {
		return -1
	}
	return (r.head + r.count - 1) % len(r.entries)
}

// at returns the entry at slot i.
func (r *reorderBuffer) at(i int) *robEntry { return &r.entries[i] }

// squashFrom removes all entries from slot idx up to the tail (youngest
// first), calling release for each so physical registers can be reclaimed.
func (r *reorderBuffer) squashFrom(idx int, release func(*robEntry)) {
	for r.count > 0 {
		t := r.tail()
		r.count--
		e := &r.entries[t]
		if release != nil {
			release(e)
		}
		if t == idx {
			break
		}
	}
}

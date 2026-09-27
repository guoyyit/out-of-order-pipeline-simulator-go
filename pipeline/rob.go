package pipeline

import "errors"

// ROBEntry holds the in-flight architectural state of one instruction.
type ROBEntry struct {
	Tag       Tag
	Seq       int
	PC        int64
	Inst      *Instruction
	Ready     bool // completed and writeback done (may still carry an exception)
	Committed bool
	active    bool

	// Renaming bookkeeping used to undo speculative mappings on squash.
	Rn renameResult

	// Result fields.
	Value   int64
	Addr    int64
	ExcCode ExceptionCode

	// Control flow bookkeeping.
	PredTaken bool
	Taken     bool
	Resolved  bool
	NextPC    int64
}

// ReorderBuffer stores in-flight entries in program order in a circular
// array. The allocated Tag is also the order index; older instructions
// always have smaller tags.
type ReorderBuffer struct {
	slots   []ROBEntry
	size    int
	headIdx int // oldest entry
	tailIdx int // next free slot
	count   int
	next    Tag
	locator map[Tag]int // live tag -> circular slot index
}

// ErrROBFull is returned when no ROB slot can be allocated.
var ErrROBFull = errors.New("reorder buffer full")

func newReorderBuffer(cfg Config) *ReorderBuffer {
	n := cfg.ROBSize
	if n <= 0 {
		n = 16
	}
	return &ReorderBuffer{slots: make([]ROBEntry, n), size: n, next: 0, locator: make(map[Tag]int)}
}

// push allocates a slot, assigns e.Tag and stores the entry.
func (b *ReorderBuffer) push(e *ROBEntry) (Tag, error) {
	if b.count == b.size {
		return InvalidTag, ErrROBFull
	}
	tag := b.next
	b.next++
	e.Tag = tag
	e.active = true
	b.locator[tag] = b.tailIdx
	b.slots[b.tailIdx] = *e
	b.tailIdx = (b.tailIdx + 1) % b.size
	b.count++
	return tag, nil
}

// get returns the live entry for tag, or nil if it is not in the ROB.
// The slot is located relative to head (absolute tag values survive
// squashes and do not track the circular head position).
func (b *ReorderBuffer) get(tag Tag) *ROBEntry {
	if tag < 0 {
		return nil
	}
	idx, ok := b.locator[tag]
	if !ok {
		return nil
	}
	slot := &b.slots[idx]
	if !slot.active || slot.Tag != tag {
		return nil
	}
	return slot
}

// head returns the oldest entry.
func (b *ReorderBuffer) head() (Tag, *ROBEntry) {
	if b.count == 0 {
		return InvalidTag, nil
	}
	e := &b.slots[b.headIdx]
	return e.Tag, e
}

// pop removes and returns the oldest entry.
func (b *ReorderBuffer) pop() *ROBEntry {
	if b.count == 0 {
		return nil
	}
	e := &b.slots[b.headIdx]
	out := *e
	delete(b.locator, out.Tag)
	var zero ROBEntry
	b.slots[b.headIdx] = zero
	b.headIdx = (b.headIdx + 1) % b.size
	b.count--
	return &out
}

// squashAfter invalidates tag and every younger entry, returning the
// removed entries in reverse allocation order so callers can undo
// renames LIFO. When keepBoundary is true the entry at tag is retained
// (used at the precise-exception commit boundary where it is popped by
// the caller); otherwise tag is removed as well (misprediction/load
// replay re-fetch the boundary instruction).
// dropBoundary=true removes tag as well (exceptions/flushes);
// false retains it (resolved mispredicted branch stays to commit).
func (b *ReorderBuffer) squashAfter(tag Tag, dropBoundary bool) []ROBEntry {
	target := b.get(tag)
	if target == nil {
		return nil
	}
	// Count live entries strictly older than tag by position from head.
	n := 0
	idx := b.headIdx
	for i := 0; i < b.count; i++ {
		if b.slots[idx].Tag == tag {
			break
		}
		n++
		idx = (idx + 1) % b.size
	}
	if !dropBoundary {
		n++ // retain the boundary entry itself
	}
	remove := b.count - n
	out := make([]ROBEntry, 0, remove)
	// Walk from the newest live slot backwards to the boundary.
	for b.count > n {
		b.tailIdx = (b.tailIdx - 1 + b.size) % b.size
		out = append(out, b.slots[b.tailIdx])
		delete(b.locator, b.slots[b.tailIdx].Tag)
		var zero ROBEntry
		b.slots[b.tailIdx] = zero
		b.count--
	}
	// Tags are globally monotonic and are deliberately NOT recycled on a
	// squash: re-fetched instructions must get fresh tags so stale
	// scheduler reservations, writeback broadcasts and timers left in
	// other structures can never alias a re-fetched instruction. The
	// circular array only provides storage; tag-to-slot mapping is always
	// computed relative to the current head (see get).
	return out
}

func (b *ReorderBuffer) len() int { return b.count }

func (b *ReorderBuffer) full() bool { return b.count == b.size }

func (b *ReorderBuffer) reset() {
	for i := range b.slots {
		b.slots[i] = ROBEntry{}
	}
	b.headIdx, b.tailIdx, b.count = 0, 0, 0
	b.next = 0
	b.locator = make(map[Tag]int)
}

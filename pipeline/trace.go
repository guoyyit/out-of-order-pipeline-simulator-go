package pipeline

import "fmt"

// TraceKind identifies the kind of a recorded trace event.
type TraceKind int

const (
	TraceFetch TraceKind = iota
	TraceRename
	TraceIssue
	TraceExecute
	TraceWriteback
	TraceCommit
	TraceSquash   // speculative instructions discarded
	TraceRecover  // state restored to a consistent point
	TraceFault    // fault injected
	TraceRollback // fault rollback applied
)

// TraceEvent is one entry of the scheduling/commit trace. With the same
// input and the same random seed the emitted sequence is identical, entry
// by entry, enabling offline replay.
type TraceEvent struct {
	Cycle uint64
	Kind  TraceKind
	PC    int // program-order index of the instruction, -1 if none
	ROB   int // reorder-buffer slot, -1 if none
	Note  string
}

func (e TraceEvent) String() string {
	return fmt.Sprintf("cycle=%d kind=%d pc=%d rob=%d %s", e.Cycle, e.Kind, e.PC, e.ROB, e.Note)
}

// Trace is an ordered list of events.
type Trace struct {
	Events []TraceEvent
}

func (t *Trace) record(e TraceEvent) {
	t.Events = append(t.Events, e)
}

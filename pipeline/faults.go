package pipeline

// FaultKind enumerates externally injectable transient faults.
type FaultKind int

const (
	// FaultFlushAndRestart squashes the selected instruction and every
	// younger in-flight instruction and re-fetches from it, exactly as a
	// misprediction would. Nothing the faulted work wrote becomes
	// visible; the final committed state is identical to a fault-free run.
	FaultFlushAndRestart FaultKind = iota
	// FaultDropResult discards one writeback of the selected instruction,
	// forcing it to re-execute. The instruction eventually commits with
	// the correct value.
	FaultDropResult
)

// FaultSpec requests one deterministic fault injection. A single shot
// fires the first time the instruction with Seq reaches the injection
// point; the simulator consumes the fault so it does not recur on replay.
type FaultSpec struct {
	Kind  FaultKind
	Seq   int
	fired bool
}

func (s *FaultSpec) matches(seq int) bool { return !s.fired && s.Seq == seq }

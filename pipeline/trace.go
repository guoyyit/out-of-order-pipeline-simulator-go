package pipeline

import (
	"bytes"
	"fmt"
)

// Trace is the deterministic record of scheduling and commit activity.
// Two runs with the same program, Config (notably Seed) and fault plan
// produce byte-identical traces; compare with Trace.Equal or Replay.
type Trace struct {
	Events  []Event
	Commits []CommitRecord
}

func newTrace() *Trace {
	return &Trace{}
}

func (t *Trace) reset() {
	t.Events = t.Events[:0]
	t.Commits = t.Commits[:0]
}

func (t *Trace) record(e Event) {
	t.Events = append(t.Events, e)
}

func (t *Trace) commit(c CommitRecord) {
	t.Commits = append(t.Commits, c)
}

// Equal reports whether two traces are identical.
func (t *Trace) Equal(o *Trace) bool {
	if len(t.Events) != len(o.Events) || len(t.Commits) != len(o.Commits) {
		return false
	}
	for i := range t.Events {
		if t.Events[i] != o.Events[i] {
			return false
		}
	}
	for i := range t.Commits {
		if t.Commits[i] != o.Commits[i] {
			return false
		}
	}
	return true
}

// String renders the trace for offline replay/debugging.
func (t *Trace) String() string {
	var b bytes.Buffer
	for _, e := range t.Events {
		fmt.Fprintf(&b, "%d %s seq=%d tag=%d %s\n", e.Cycle, e.Kind, e.Seq, e.Tag, e.Detail)
	}
	for _, c := range t.Commits {
		fmt.Fprintf(&b, "commit cycle=%d seq=%d rd=%d value=%d addr=%d\n",
			c.Cycle, c.Seq, c.Rd, c.Value, c.Addr)
	}
	return b.String()
}

// Replay re-runs the same program/config and verifies the regenerated
// trace equals the recorded one. The provided seed must match the seed
// used originally; it is embedded in cfg.
func Replay(program []Instruction, cfg Config, recorded *Trace, faults []FaultSpec) error {
	p := New(program, cfg)
	for _, f := range faults {
		p.InjectFault(f)
	}
	st := p.Run()
	if !recorded.Equal(st.Trace) {
		return fmt.Errorf("replay mismatch: recorded %d events/%d commits, replay produced %d events/%d commits",
			len(recorded.Events), len(recorded.Commits),
			len(st.Trace.Events), len(st.Trace.Commits))
	}
	return nil
}

package pipeline

import (
	"reflect"
	"testing"
)

// committedPCs extracts the sequence of committed PCs from a trace,
// ignoring cycles so runs with different timing can be compared.
func committedPCs(tr Trace) []int {
	var pcs []int
	for _, e := range tr.Events {
		if e.Kind == TraceCommit {
			pcs = append(pcs, e.PC)
		}
	}
	return pcs
}

// TestFaultInjectionRecovery: a fault discards all uncommitted state and
// rolls back to the commit point; the recovered run must reach exactly
// the same committed state and commit order as a fault-free run.
func TestFaultInjectionRecovery(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 4},
		{Op: OpMov, Rd: 2, Imm: 0},
		{Op: OpMov, Rd: 3, Imm: 1},
		{Op: OpAdd, Rd: 2, Rs1: 2, Rs2: 1}, // 3: loop body
		{Op: OpSub, Rd: 1, Rs1: 1, Rs2: 3}, // 4
		{Op: OpBranch, Cond: 1, Target: 3}, // 5
		{Op: OpStore, Rs1: 2, Addr: 40},    // 6
		{Op: OpHalt},                       // 7
	}
	clean := runMachine(t, prog, Config{Seed: 21})

	// Inject a fault at several different cycles; every recovered run
	// must match the clean run's committed state and commit order.
	for _, at := range []uint64{2, 3, 5, 8, 13} {
		faulty := runMachine(t, prog, Config{Seed: 21, Fault: &FaultInjector{AtCycle: at}})
		if !reflect.DeepEqual(faulty.Retired, clean.Retired) {
			t.Fatalf("fault@%d: regs differ: got %v want %v", at, faulty.Retired, clean.Retired)
		}
		if !reflect.DeepEqual(faulty.Memory, clean.Memory) {
			t.Fatalf("fault@%d: memory differs: got %v want %v", at, faulty.Memory, clean.Memory)
		}
		if !reflect.DeepEqual(committedPCs(faulty.Trace), committedPCs(clean.Trace)) {
			t.Fatalf("fault@%d: commit order differs: got %v want %v",
				at, committedPCs(faulty.Trace), committedPCs(clean.Trace))
		}
		var sawFault, sawRollback bool
		for _, e := range faulty.Trace.Events {
			if e.Kind == TraceFault {
				sawFault = true
			}
			if e.Kind == TraceRollback {
				sawRollback = true
			}
		}
		if !sawFault || !sawRollback {
			t.Fatalf("fault@%d: expected fault and rollback events in trace", at)
		}
		if faulty.Exception != nil {
			t.Fatalf("fault@%d: unexpected exception %v", at, faulty.Exception)
		}
	}
}

// TestFaultLeavesNoPartialWrites: at the moment of injection, uncommitted
// results must not be observable; committed state must be a prefix of the
// clean run's commit sequence.
func TestFaultLeavesNoPartialWrites(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 10},
		{Op: OpMov, Rd: 2, Imm: 20},
		{Op: OpStore, Rs1: 1, Addr: 50},
		{Op: OpStore, Rs1: 2, Addr: 51},
		{Op: OpAdd, Rd: 3, Rs1: 1, Rs2: 2},
		{Op: OpHalt},
	}
	m := New(prog, Config{Seed: 33, Fault: &FaultInjector{AtCycle: 3}})
	// Step until the fault fires, then inspect committed state.
	for m.cycle < 3 {
		m.Step()
	}
	regs, mem := m.CommittedState()
	clean := runMachine(t, prog, Config{Seed: 33})
	// Committed memory at the fault point must be a subset of the final
	// committed memory (no partial or wrong-path writes).
	for a, v := range mem {
		if clean.Memory[a] != v {
			t.Fatalf("partial write visible at fault: mem[%d]=%d, final=%d", a, v, clean.Memory[a])
		}
	}
	for r, v := range regs {
		if clean.Retired[r] != v && v != 0 {
			// A committed register may hold any value that the clean run
			// also committed at some prefix; here registers only gain
			// their final values, so it must match or be unwritten.
			t.Fatalf("register r%d=%d at fault, clean final=%d", r, v, clean.Retired[r])
		}
	}
	res := m.Run()
	if !reflect.DeepEqual(res.Retired, clean.Retired) || !reflect.DeepEqual(res.Memory, clean.Memory) {
		t.Fatalf("post-recovery state differs from clean run")
	}
}

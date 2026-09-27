package pipeline

import (
	"reflect"
	"testing"
)

// TestPreciseException checks that a divide-by-zero stops the machine
// precisely: older instructions stay committed, the faulting instruction
// and everything younger leave no visible trace, and the recovery point
// plus reason are queryable.
func TestPreciseException(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 100},       // 0: committed
		{Op: OpMov, Rd: 2, Imm: 0},         // 1: committed
		{Op: OpDiv, Rd: 3, Rs1: 1, Rs2: 2}, // 2: faulting instruction
		{Op: OpMov, Rd: 4, Imm: 555},       // 3: must not be visible
		{Op: OpStore, Rs1: 1, Addr: 7},     // 4: must not be visible
		{Op: OpHalt},                       // 5
	}
	m := New(prog, Config{Seed: 5})
	res := m.Run()

	if res.Exception == nil || res.Exception.Kind != ExceptionDivideByZero {
		t.Fatalf("expected divide-by-zero exception, got %v", res.Exception)
	}
	if res.Exception.PC != 2 {
		t.Fatalf("exception PC = %d, want 2", res.Exception.PC)
	}
	exc, recoverPC := m.LastException()
	if exc == nil || recoverPC != 2 {
		t.Fatalf("LastException = (%v, %d), want (divide-by-zero, 2)", exc, recoverPC)
	}
	// Older state preserved, younger state invisible.
	wantRegs, wantMem, _ := refRun(prog, 8, 4096)
	if !reflect.DeepEqual(res.Retired, wantRegs) {
		t.Fatalf("regs mismatch: got %v want %v", res.Retired, wantRegs)
	}
	if !reflect.DeepEqual(res.Memory, wantMem) {
		t.Fatalf("memory mismatch: got %v want %v", res.Memory, wantMem)
	}
	if res.Retired[4] != 0 {
		t.Fatalf("post-exception write visible: r4=%d", res.Retired[4])
	}
	if _, ok := res.Memory[7]; ok {
		t.Fatalf("post-exception store visible in memory")
	}
	// Only the two instructions before the fault may commit.
	if res.Committed != 2 {
		t.Fatalf("committed = %d, want 2", res.Committed)
	}
}

// TestInvalidMemoryException checks a load from a bad address raises a
// precise invalid-memory exception.
func TestInvalidMemoryException(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 9},
		{Op: OpLoad, Rd: 2, Addr: 10_000}, // beyond MemSize
		{Op: OpHalt},
	}
	m := New(prog, Config{Seed: 1, MemSize: 64})
	res := m.Run()
	if res.Exception == nil || res.Exception.Kind != ExceptionInvalidMemory {
		t.Fatalf("expected invalid-memory exception, got %v", res.Exception)
	}
	if _, pc := m.LastException(); pc != 1 {
		t.Fatalf("recovery PC = %d, want 1", pc)
	}
	if res.Committed != 1 {
		t.Fatalf("committed = %d, want 1", res.Committed)
	}
}

// TestExceptionReplayConsistency: re-running the same program without the
// faulting tail must reach the same committed state as the exception run.
func TestExceptionReplayConsistency(t *testing.T) {
	full := Program{
		{Op: OpMov, Rd: 1, Imm: 6},
		{Op: OpMov, Rd: 2, Imm: 0},
		{Op: OpDiv, Rd: 3, Rs1: 1, Rs2: 2},
		{Op: OpMov, Rd: 4, Imm: 1},
		{Op: OpHalt},
	}
	prefix := Program{
		{Op: OpMov, Rd: 1, Imm: 6},
		{Op: OpMov, Rd: 2, Imm: 0},
		{Op: OpHalt},
	}
	resFull := runMachine(t, full, Config{Seed: 2})
	resPrefix := runMachine(t, prefix, Config{Seed: 2})
	if !reflect.DeepEqual(resFull.Retired, resPrefix.Retired) {
		t.Fatalf("committed state after exception differs from prefix run: %v vs %v",
			resFull.Retired, resPrefix.Retired)
	}
}

package pipeline

import (
	"reflect"
	"testing"
)

// TestBranchMispredict checks that speculatively issued instructions on
// the wrong path are fully squashed and the machine recovers to the
// branch resolution point.
func TestBranchMispredict(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 1},         // 0: r1 = 1 (branch taken)
		{Op: OpBranch, Cond: 1, Target: 5}, // 1: if r1 goto 5 (predicted not-taken)
		{Op: OpMov, Rd: 2, Imm: 999},       // 2: wrong path, must be squashed
		{Op: OpMov, Rd: 3, Imm: 888},       // 3: wrong path, must be squashed
		{Op: OpMov, Rd: 4, Imm: 777},       // 4: wrong path, must be squashed
		{Op: OpMov, Rd: 2, Imm: 42},        // 5: r2 = 42 (correct path)
		{Op: OpHalt},                       // 6
	}
	res := runMachine(t, prog, Config{Seed: 7, BranchMissLat: 2})
	wantRegs, _, _ := refRun(prog, 8, 4096)
	if !reflect.DeepEqual(res.Retired, wantRegs) {
		t.Fatalf("regs mismatch: got %v want %v", res.Retired, wantRegs)
	}
	if res.Retired[2] != 42 || res.Retired[3] != 0 || res.Retired[4] != 0 {
		t.Fatalf("wrong-path writes visible: %v", res.Retired)
	}
	// The wrong-path instructions must have been fetched and squashed.
	var squash, recover bool
	for _, e := range res.Trace.Events {
		if e.Kind == TraceSquash {
			squash = true
		}
		if e.Kind == TraceRecover {
			recover = true
		}
	}
	if !squash || !recover {
		t.Fatalf("expected squash and recover events in trace")
	}
	// Wrong-path PCs (2,3,4) must never commit.
	for _, e := range res.Trace.Events {
		if e.Kind == TraceCommit && e.PC >= 2 && e.PC <= 4 {
			t.Fatalf("wrong-path instruction committed: %+v", e)
		}
	}
}

// TestBranchNotTaken checks the correctly predicted path runs straight
// through with no squash.
func TestBranchNotTaken(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 0},
		{Op: OpBranch, Cond: 1, Target: 4},
		{Op: OpMov, Rd: 2, Imm: 5},
		{Op: OpHalt},
		{Op: OpMov, Rd: 2, Imm: 9},
	}
	res := runMachine(t, prog, Config{Seed: 3})
	wantRegs, _, _ := refRun(prog, 8, 4096)
	if !reflect.DeepEqual(res.Retired, wantRegs) {
		t.Fatalf("regs mismatch: got %v want %v", res.Retired, wantRegs)
	}
	for _, e := range res.Trace.Events {
		if e.Kind == TraceSquash {
			t.Fatalf("unexpected squash on correctly predicted branch")
		}
	}
}

// TestLoop exercises repeated mispredict-free iterations plus the final
// mispredict when the loop exits.
func TestLoop(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 5},         // 0: r1 = 5 (counter)
		{Op: OpMov, Rd: 2, Imm: 1},         // 1: r2 = 1
		{Op: OpMov, Rd: 3, Imm: 0},         // 2: r3 = 0 (sum)
		{Op: OpAdd, Rd: 3, Rs1: 3, Rs2: 1}, // 3: r3 += r1
		{Op: OpSub, Rd: 1, Rs1: 1, Rs2: 2}, // 4: r1 -= 1
		{Op: OpBranch, Cond: 1, Target: 3}, // 5: if r1 != 0 goto 3
		{Op: OpHalt},                       // 6
	}
	res := runMachine(t, prog, Config{Seed: 11, IssueWidth: 3})
	wantRegs, _, _ := refRun(prog, 8, 4096)
	if !reflect.DeepEqual(res.Retired, wantRegs) {
		t.Fatalf("regs mismatch: got %v want %v", res.Retired, wantRegs)
	}
	if res.Retired[3] != 15 {
		t.Fatalf("sum = %d, want 15", res.Retired[3])
	}
}

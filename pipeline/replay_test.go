package pipeline

import (
	"math/rand"
	"reflect"
	"testing"
)

// TestDeterministicReplay: identical input and seed produce an identical
// trace, entry by entry; committed results are identical even across
// different seeds.
func TestDeterministicReplay(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 7},
		{Op: OpMov, Rd: 2, Imm: 3},
		{Op: OpAdd, Rd: 3, Rs1: 1, Rs2: 2},
		{Op: OpStore, Rs1: 3, Addr: 60},
		{Op: OpLoad, Rd: 4, Addr: 60},
		{Op: OpMul, Rd: 5, Rs1: 4, Rs2: 2},
		{Op: OpBranch, Cond: 1, Target: 8},
		{Op: OpMov, Rd: 6, Imm: 1},
		{Op: OpSub, Rd: 7, Rs1: 5, Rs2: 1},
		{Op: OpHalt},
	}
	a := runMachine(t, prog, Config{Seed: 42})
	b := runMachine(t, prog, Config{Seed: 42})
	if !reflect.DeepEqual(a.Trace.Events, b.Trace.Events) {
		t.Fatalf("same seed: traces differ")
	}
	if !reflect.DeepEqual(a.Retired, b.Retired) || !reflect.DeepEqual(a.Memory, b.Memory) {
		t.Fatalf("same seed: results differ")
	}
	// Different seeds may schedule differently but must commit the same
	// architectural state.
	c := runMachine(t, prog, Config{Seed: 4242})
	if !reflect.DeepEqual(a.Retired, c.Retired) || !reflect.DeepEqual(a.Memory, c.Memory) {
		t.Fatalf("different seed: committed state differs: %v vs %v", a.Retired, c.Retired)
	}
	if !reflect.DeepEqual(committedPCs(a.Trace), committedPCs(c.Trace)) {
		t.Fatalf("different seed: commit order differs")
	}
}

// TestFuzzAgainstReference generates random straight-line programs with
// heavy register reuse and compares committed state with the sequential
// reference for many seeds.
func TestFuzzAgainstReference(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		n := 1 + r.Intn(30)
		prog := make(Program, 0, n+1)
		for i := 0; i < n; i++ {
			switch r.Intn(6) {
			case 0:
				prog = append(prog, Instruction{Op: OpMov, Rd: r.Intn(6), Imm: int64(r.Intn(20))})
			case 1:
				prog = append(prog, Instruction{Op: OpAdd, Rd: r.Intn(6), Rs1: r.Intn(6), Rs2: r.Intn(6)})
			case 2:
				prog = append(prog, Instruction{Op: OpSub, Rd: r.Intn(6), Rs1: r.Intn(6), Rs2: r.Intn(6)})
			case 3:
				prog = append(prog, Instruction{Op: OpMul, Rd: r.Intn(6), Rs1: r.Intn(6), Rs2: r.Intn(6)})
			case 4:
				prog = append(prog, Instruction{Op: OpStore, Rs1: r.Intn(6), Addr: uint64(r.Intn(16))})
			default:
				prog = append(prog, Instruction{Op: OpLoad, Rd: r.Intn(6), Addr: uint64(r.Intn(16))})
			}
		}
		prog = append(prog, Instruction{Op: OpHalt})

		cfg := Config{
			Seed:          seed,
			NumArchRegs:   6,
			NumPhysRegs:   12,
			ROBSize:       8,
			IssueWidth:    1 + r.Intn(3),
			MemSize:       16,
			BranchMissLat: r.Intn(3),
		}
		res := runMachine(t, prog, cfg)
		wantRegs, wantMem, wantExc := refRun(prog, 6, 16)
		if (res.Exception == nil) != (wantExc == nil) {
			t.Fatalf("seed %d: exception mismatch: got %v want %v", seed, res.Exception, wantExc)
		}
		if wantExc != nil && (res.Exception.Kind != wantExc.Kind || res.Exception.PC != wantExc.PC) {
			t.Fatalf("seed %d: exception = %v, want %v", seed, res.Exception, wantExc)
		}
		if !reflect.DeepEqual(res.Retired, wantRegs) {
			t.Fatalf("seed %d: regs mismatch:\n got %v\nwant %v\nprog %v", seed, res.Retired, wantRegs, prog)
		}
		if !reflect.DeepEqual(res.Memory, wantMem) {
			t.Fatalf("seed %d: memory mismatch:\n got %v\nwant %v\nprog %v", seed, res.Memory, wantMem, prog)
		}
	}
}

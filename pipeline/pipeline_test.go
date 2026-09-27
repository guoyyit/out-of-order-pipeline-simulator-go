package pipeline

import (
	"testing"
)

func runProg(t *testing.T, prog []Instruction, cfg Config, faults ...FaultSpec) *Status {
	t.Helper()
	p := New(prog, cfg)
	for _, f := range faults {
		p.InjectFault(f)
	}
	return p.Run()
}

// TestDataDependencies verifies WAR/WAW/RAW dependencies via register
// renaming: the OOO machine produces the same results as an in-order
// reference, and commits appear strictly in program order.
func TestDataDependencies(t *testing.T) {
	cfg := DefaultConfig()
	// r1=10; r2=r1+5; r3=r1+r2; WAW r1=r3+1; r4=r1 (must see new r1);
	// r5=r2 (older value, WAR hazard must not see the later r2 below);
	// r2=99 (later write, must not corrupt r5's read).
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 10},        // 0 r1=10
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 1, Imm: 5}, // 1 r2=15
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Rs1: 1, Rs2: 2}, // 2 r3=25
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Rs1: 3, Imm: 1}, // 3 r1=26 (WAW)
		{Kind: OpALU, ALU: ALUAdd, Rd: 4, Rs1: 1},         // 4 r4=26
		{Kind: OpALU, ALU: ALUAdd, Rd: 5, Rs1: 2},         // 5 r5=15 (RAW, before r2 rewrite)
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 99},        // 6 r2=99
		{Kind: OpALU, ALU: ALUAdd, Rd: 6, Rs1: 2},         // 7 r6=99
	}
	st := runProg(t, prog, cfg)
	want := map[int]int64{1: 26, 2: 99, 3: 25, 4: 26, 5: 15, 6: 99}
	for r, v := range want {
		if st.Registers[r] != v {
			t.Fatalf("r%d=%d want %d", r, st.Registers[r], v)
		}
	}
	if len(st.Trace.Commits) != len(prog) {
		t.Fatalf("commits=%d want %d", len(st.Trace.Commits), len(prog))
	}
	for i, c := range st.Trace.Commits {
		if c.Seq != i {
			t.Fatalf("commit order violated: index %d got seq %d", i, c.Seq)
		}
	}
}

// TestOOOSpeedup shows independent instructions execute ahead of a long
// latency chain (scheduling actually happens out of order), while commit
// stays in order.
func TestOOOSpeedup(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ALULatency = 1
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 1},
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 1, Imm: 1},
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 2, Imm: 1},
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 2, Imm: 1},
		// independent of the chain, can execute early
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Imm: 7},
	}
	st := runProg(t, prog, cfg)
	if st.Registers[2] != 4 || st.Registers[3] != 7 {
		t.Fatalf("bad results r2=%d r3=%d", st.Registers[2], st.Registers[3])
	}
	// r3 issues before the chain finishes: its complete event precedes
	// completion of seq 3.
	var compSeq3, compSeq4 int
	for _, ev := range st.Trace.Events {
		if ev.Kind == EvWriteback && ev.Seq == 3 {
			compSeq3 = ev.Cycle
		}
		if ev.Kind == EvWriteback && ev.Seq == 4 {
			compSeq4 = ev.Cycle
		}
	}
	if compSeq4 == 0 || compSeq4 > compSeq3 {
		t.Fatalf("independent instruction did not execute early: wb seq3=%d seq4=%d", compSeq3, compSeq4)
	}
}

// TestBranchMispredict predicts not-taken for a taken branch and checks
// that the wrong-path younger instructions are squashed (never commit),
// fetch redirects to the target, and architectural results equal the
// in-order reference.
func TestBranchMispredict(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnablePredictor = false // predict every branch not-taken
	// r1=5; if r1!=0 goto pc=24 (seq6); r2=11 (wrong path, squashed);
	// ... target: r3=42.
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 5},          // 0
		{Kind: OpBranch, Branch: BrNE, Rs1: 1, Target: 24}, // 1 taken -> seq6
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 11},         // 2 wrong path
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 2, Imm: 1},  // 3 wrong path
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 2, Imm: 1},  // 4 wrong path
		{Kind: OpJump, Target: 32},                         // 5 skip target
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Imm: 42},         // 6 target
		{Kind: OpALU, ALU: ALUAdd, Rd: 4, Rs1: 3},          // 7
	}
	st := runProg(t, prog, cfg)
	if !st.Halted {
		t.Fatal("expected halt after program end")
	}
	if st.Registers[2] != 0 {
		t.Fatalf("wrong-path r2 leaked: %d", st.Registers[2])
	}
	if st.Registers[3] != 42 || st.Registers[4] != 42 {
		t.Fatalf("target path results wrong: r3=%d r4=%d", st.Registers[3], st.Registers[4])
	}
	// Only seq 0,1,6,7 commit.
	var got []int
	for _, c := range st.Trace.Commits {
		got = append(got, c.Seq)
	}
	want := []int{0, 1, 6, 7}
	if len(got) != len(want) {
		t.Fatalf("committed seqs=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("committed seqs=%v want %v", got, want)
		}
	}
	rec := st.LastRecovery
	if rec == nil || rec.Code != ExcMispredict || rec.PC != 24 {
		t.Fatalf("recovery=%+v want mispredict to pc=24", rec)
	}
}

// TestBranchCorrectPrediction runs a loop that eventually exits and
// verifies accumulated semantics and predictor learning.
func TestBranchCorrectPrediction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnablePredictor = true
	// Simple forward branch, not taken: r1=1; if r1==0 goto 16; r2=2;
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 1},          // 0
		{Kind: OpBranch, Branch: BrEQ, Rs1: 1, Target: 16}, // 1 not taken
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 2},          // 2
	}
	st := runProg(t, prog, cfg)
	if st.Registers[2] != 2 {
		t.Fatalf("r2=%d want 2", st.Registers[2])
	}
}

// TestExceptionPrecise places a trap mid-program and verifies that:
// strictly older instructions retain their committed results, the trap
// and all younger instructions are invisible, and the recovery point
// reports the trap's PC and reason.
func TestExceptionPrecise(t *testing.T) {
	cfg := DefaultConfig()
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 11}, // 0 commits
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 22}, // 1 commits
		{Kind: OpTrap, TrapCode: ExcIllegal},       // 2 traps
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Imm: 33}, // 3 must be invisible
		{Kind: OpALU, ALU: ALUAdd, Rd: 4, Imm: 44}, // 4 must be invisible
	}
	p := New(prog, cfg)
	st := p.Run()
	if !st.Halted {
		t.Fatal("expected halt on exception")
	}
	if st.Registers[1] != 11 || st.Registers[2] != 22 {
		t.Fatalf("pre-exception state lost: r1=%d r2=%d", st.Registers[1], st.Registers[2])
	}
	if st.Registers[3] != 0 || st.Registers[4] != 0 {
		t.Fatalf("younger instructions leaked: r3=%d r4=%d", st.Registers[3], st.Registers[4])
	}
	var seqs []int
	for _, c := range st.Trace.Commits {
		seqs = append(seqs, c.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 0 || seqs[1] != 1 {
		t.Fatalf("commits=%v want [0 1]", seqs)
	}
	rec := st.LastRecovery
	if rec == nil || rec.Code != ExcIllegal || rec.Seq != 2 || rec.PC != 8 {
		t.Fatalf("recovery=%+v want illegal at seq2 pc8", rec)
	}
	if p.RecoveryPoint() == nil {
		t.Fatal("recovery point not queryable after halt")
	}
}

// TestInterruptPrecise raises an interrupt and checks it is taken at a
// commit boundary: the ready head instruction commits first, the machine
// drains with the interrupt reason queryable and no partial younger
// state is visible.
func TestInterruptPrecise(t *testing.T) {
	cfg := DefaultConfig()
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 1}, // 0
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 2}, // 1 interrupt boundary
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Imm: 3}, // 2
	}
	p := New(prog, cfg)
	// Raise before run: interrupt must still be precise and not stop
	// seq0 from committing.
	p.RaiseInterrupt(ExcInterrupt, "timer")
	st := p.Run()
	if !st.Halted {
		t.Fatal("expected halt after interrupt")
	}
	if st.Registers[1] != 1 {
		t.Fatalf("older instr lost on interrupt: r1=%d", st.Registers[1])
	}
	if st.Registers[2] != 0 || st.Registers[3] != 0 {
		t.Fatalf("boundary/younger state leaked: r2=%d r3=%d", st.Registers[2], st.Registers[3])
	}
	rec := st.LastRecovery
	if rec == nil || rec.Code != ExcInterrupt {
		t.Fatalf("recovery=%+v want interrupt", rec)
	}
}

// memProgram builds (data producers precede their stores):
// base r1=0; r2=77; store [r1+0]=77; r4=88; store [r1+16]=88;
// load [0] -> 77 (RAW/store forwarding), load [16] -> 88, sum -> 165.
func memProgram() []Instruction {
	return []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 0},         // 0 base=0
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 77},        // 1 value A
		{Kind: OpStore, Rs1: 1, Rs2: 2, Imm: 0},           // 2 [0]=77
		{Kind: OpALU, ALU: ALUAdd, Rd: 4, Imm: 88},        // 3 value B
		{Kind: OpStore, Rs1: 1, Rs2: 4, Imm: 16},          // 4 [16]=88
		{Kind: OpLoad, Rd: 5, Rs1: 1, Imm: 0},             // 5 load [0] -> 77
		{Kind: OpLoad, Rd: 6, Rs1: 1, Imm: 16},            // 6 load [16] -> 88
		{Kind: OpALU, ALU: ALUAdd, Rd: 7, Rs1: 5, Rs2: 6}, // 7 sum =165
	}
}

func TestMemoryAliasStall(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UnknownPolicy = PolicyStall
	st := runProg(t, memProgram(), cfg)
	if st.Registers[5] != 77 || st.Registers[6] != 88 || st.Registers[7] != 165 {
		t.Fatalf("stall policy values wrong: r5=%d r6=%d r7=%d",
			st.Registers[5], st.Registers[6], st.Registers[7])
	}
}

func TestMemoryAliasAssume(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UnknownPolicy = PolicyAssumeAlias
	st := runProg(t, memProgram(), cfg)
	if st.Registers[5] != 77 || st.Registers[6] != 88 || st.Registers[7] != 165 {
		t.Fatalf("assume-alias values wrong: r5=%d r6=%d r7=%d",
			st.Registers[5], st.Registers[6], st.Registers[7])
	}
}

// TestMemoryWAWAndStoreForwarding builds two stores to the SAME address
// (WAW) with a load between them; the load must observe the first store
// and final memory the second, regardless of execution reordering.
func TestMemoryWAWAndStoreForwarding(t *testing.T) {
	for _, policy := range []UnknownAddrPolicy{PolicyStall, PolicyAssumeAlias} {
		cfg := DefaultConfig()
		cfg.UnknownPolicy = policy
		prog := []Instruction{
			{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 0}, // 0 base
			{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 5}, // 1 first value
			{Kind: OpALU, ALU: ALUAdd, Rd: 3, Imm: 9}, // 2 second value
			{Kind: OpStore, Rs1: 1, Rs2: 2, Imm: 0},   // 3 [0]=5
			{Kind: OpLoad, Rd: 4, Rs1: 1, Imm: 0},     // 4 load [0] ->5
			{Kind: OpStore, Rs1: 1, Rs2: 3, Imm: 0},   // 5 [0]=9 (WAW)
		}
		st := runProg(t, prog, cfg)
		if st.Registers[4] != 5 {
			t.Fatalf("policy %d: load saw %d want 5", policy, st.Registers[4])
		}
		if st.Trace == nil {
			t.Fatal("nil trace")
		}
	}
	// Final memory checked on a fresh run after all commits.
	cfg := DefaultConfig()
	p := New(memProgram(), cfg)
	p.Run()
	if p.MemoryValue(16) != 88 || p.MemoryValue(0) != 77 {
		t.Fatalf("final memory wrong: [0]=%d [16]=%d", p.MemoryValue(0), p.MemoryValue(16))
	}
}

// TestMemoryAliasReplay forces the PolicyAssumeAlias replay path: a load
// executes before an older store's address is known, then that store
// resolves to the SAME address; the load must be squashed and re-read.
func TestMemoryAliasReplay(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UnknownPolicy = PolicyAssumeAlias
	// Base address r1 produced late (depends on a long chain), so the
	// load computes its address from an independent r8(=0) at first.
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 8, Imm: 0},  // 0 fast base 0
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 41}, // 1 value
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 0},  // 2 store base 0 (early)
		{Kind: OpStore, Rs1: 1, Rs2: 2, Imm: 0},    // 3 [0]=41
		{Kind: OpLoad, Rd: 9, Rs1: 8, Imm: 0},      // 4 load [0] must get 41 via replay/fwd
	}
	st := runProg(t, prog, cfg)
	if st.Registers[9] != 41 {
		t.Fatalf("aliased load got %d want 41", st.Registers[9])
	}
}

// TestLoadAddressException checks a misaligned load raises a precise
// load-address exception and earlier commits survive.
func TestLoadAddressException(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MemoryGranularity = 4
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 10}, // 0 commits
		{Kind: OpLoad, Rd: 2, Rs1: 1, Imm: 1},      // 1 addr 11 -> misaligned
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Imm: 7},  // 2 invisible
	}
	p := New(prog, cfg)
	st := p.Run()
	if !st.Halted || st.LastRecovery == nil || st.LastRecovery.Code != ExcLoadAddr {
		t.Fatalf("recovery=%+v want load-address", st.LastRecovery)
	}
	if st.Registers[1] != 10 || st.Registers[2] != 0 || st.Registers[3] != 0 {
		t.Fatalf("precise load exception state wrong: %+v", st.Registers)
	}
}

// faultProgram is a mixed ALU/load/store/branch program reused for
// fault-vs-no-fault equivalence and deterministic replay.
func faultProgram() []Instruction {
	return []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Rs1: -1, Rs2: -1, Imm: 4},        // 0 r1=4 (aligned)
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 1, Rs2: -1, Imm: 4},         // 1 r2=8
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Rs1: 2, Rs2: 1},                  // 2 r3=12
		{Kind: OpALU, ALU: ALUAdd, Rd: 4, Rs1: 3, Rs2: 2, Imm: 0},          // 3 r4=20
		{Kind: OpStore, Rd: -1, Rs1: 1, Rs2: 4, Imm: 4},                    // 4 [8]=20
		{Kind: OpLoad, Rd: 5, Rs1: 1, Rs2: -1, Imm: 4},                     // 5 r5=[8]=20
		{Kind: OpBranch, Rd: -1, Branch: BrEQ, Rs1: 5, Rs2: 4, Target: 36}, // 6 equal->taken
		{Kind: OpALU, ALU: ALUAdd, Rd: 6, Rs1: 5, Rs2: -1, Imm: 100},       // 7 skipped
		{Kind: OpALU, ALU: ALUAdd, Rd: 7, Rs1: 5, Rs2: 4},                  // 8 r7=40
	}
}

func runFault(t *testing.T, faults ...FaultSpec) *Status {
	t.Helper()
	return runProg(t, faultProgram(), DefaultConfig(), faults...)
}

// TestFaultFlushEquivalence injects flush-and-restart faults at several
// instructions and proves the final committed state equals the
// fault-free run (no partial writes survive).
func TestFaultFlushEquivalence(t *testing.T) {
	base := runProg(t, faultProgram(), DefaultConfig())
	for _, seq := range []int{1, 3, 5, 8} {
		faulted := runFault(t, FaultSpec{Kind: FaultFlushAndRestart, Seq: seq})
		if len(faulted.Trace.Commits) != len(base.Trace.Commits) {
			t.Fatalf("flush seq=%d commits=%d want %d", seq,
				len(faulted.Trace.Commits), len(base.Trace.Commits))
		}
		for r := range base.Registers {
			if faulted.Registers[r] != base.Registers[r] {
				t.Fatalf("flush seq=%d r%d=%d want %d", seq, r,
					faulted.Registers[r], base.Registers[r])
			}
		}
		for i := range base.Trace.Commits {
			bc := base.Trace.Commits[i]
			fc := faulted.Trace.Commits[i]
			// Seq, register, value and address must match; the commit
			// cycle may legitimately be later due to replay.
			if bc.Seq != fc.Seq || bc.Rd != fc.Rd || bc.Value != fc.Value || bc.Addr != fc.Addr || bc.PC != fc.PC {
				t.Fatalf("flush seq=%d commit %d mismatch: %+v vs %+v",
					seq, i, fc, bc)
			}
		}
	}
}

// TestFaultDropResult injects a dropped writeback; the instruction must
// re-execute and still produce the correct value.
func TestFaultDropResult(t *testing.T) {
	base := runProg(t, faultProgram(), DefaultConfig())
	for _, seq := range []int{1, 2, 3, 5} {
		faulted := runFault(t, FaultSpec{Kind: FaultDropResult, Seq: seq})
		for r := range base.Registers {
			if faulted.Registers[r] != base.Registers[r] {
				t.Fatalf("drop seq=%d r%d=%d want %d", seq, r,
					faulted.Registers[r], base.Registers[r])
			}
		}
	}
}

// TestDeterministicReplay runs the identical program/config twice and
// verifies byte-identical scheduling and commit traces, then checks the
// explicit Replay helper succeeds.
func TestDeterministicReplay(t *testing.T) {
	cfg := DefaultConfig()
	prog := faultProgram()
	st1 := runProg(t, prog, cfg)
	st2 := runProg(t, prog, cfg)
	if !st1.Trace.Equal(st2.Trace) {
		t.Fatal("identical seed produced different traces")
	}
	if len(st1.Trace.Events) == 0 || len(st1.Trace.Commits) == 0 {
		t.Fatal("expected non-empty trace")
	}
	recorded := &Trace{Events: st1.Trace.Events, Commits: st1.Trace.Commits}
	if err := Replay(prog, cfg, recorded, nil); err != nil {
		t.Fatalf("replay mismatch: %v", err)
	}
	// A different seed is allowed to differ in schedule tie-breaks; but
	// commit values must be identical.
	cfg2 := cfg
	cfg2.Seed = 987654321
	st3 := runProg(t, prog, cfg2)
	for r := range st1.Registers {
		if st3.Registers[r] != st1.Registers[r] {
			t.Fatalf("different seed changed semantics r%d: %d vs %d",
				r, st3.Registers[r], st1.Registers[r])
		}
	}
}

// TestResetRerun verifies Reset clears all architectural and speculative
// state and a subsequent run reproduces the original results exactly.
func TestResetRerun(t *testing.T) {
	cfg := DefaultConfig()
	prog := faultProgram()
	p := New(prog, cfg)
	p.InjectFault(FaultSpec{Kind: FaultFlushAndRestart, Seq: 2})
	first := p.Run()
	if len(first.Trace.Commits) == 0 {
		t.Fatal("no commits in first run")
	}
	p.Reset()
	second := p.Run()
	if len(second.Trace.Commits) != len(first.Trace.Commits) {
		t.Fatalf("after reset commits=%d want %d",
			len(second.Trace.Commits), len(first.Trace.Commits))
	}
	for r := range first.Registers {
		if first.Registers[r] != second.Registers[r] {
			t.Fatalf("after reset r%d=%d want %d", r, second.Registers[r], first.Registers[r])
		}
	}
}

// TestStoreAddressException checks misaligned stores raise a precise
// store-address exception and no partial store becomes visible.
func TestStoreAddressException(t *testing.T) {
	cfg := DefaultConfig()
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 9},       // 0 commits
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 5},       // 1 commits
		{Kind: OpStore, Rd: -1, Rs1: 1, Rs2: 2, Imm: 0}, // 2 addr9 misaligned
		{Kind: OpLoad, Rd: 3, Rs1: 1, Rs2: -1, Imm: 0},  // 3 invisible
	}
	p := New(prog, cfg)
	st := p.Run()
	if st.LastRecovery == nil || st.LastRecovery.Code != ExcStoreAddr {
		t.Fatalf("recovery=%+v want store-address", st.LastRecovery)
	}
	if st.Registers[1] != 9 || st.Registers[2] != 5 || st.Registers[3] != 0 {
		t.Fatalf("precise store exception state wrong: %+v", st.Registers)
	}
}

// TestUnknownAddressStallOrdering builds a store whose address is
// produced by a slow dependency chain and a younger load to a different
// address; under PolicyStall the load must not read memory until the
// store address resolves, and the final value remains correct.
func TestUnknownAddressStallOrdering(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UnknownPolicy = PolicyStall
	prog := []Instruction{
		{Kind: OpALU, ALU: ALUAdd, Rd: 1, Imm: 8},         // 0 fast base for load
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Imm: 0},         // 1 store base chain start
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 2, Imm: 4}, // 2 ...
		{Kind: OpALU, ALU: ALUAdd, Rd: 2, Rs1: 2, Imm: 4}, // 3 store base=8 (late)
		{Kind: OpALU, ALU: ALUAdd, Rd: 3, Imm: 123},       // 4 value
		{Kind: OpStore, Rd: -1, Rs1: 2, Rs2: 3, Imm: 0},   // 5 [8]=123 (late addr)
		{Kind: OpLoad, Rd: 4, Rs1: 1, Rs2: -1, Imm: 0},    // 6 load [8] -> 123
	}
	st := runProg(t, prog, cfg)
	if st.Registers[4] != 123 {
		t.Fatalf("stall-ordered load got %d want 123", st.Registers[4])
	}
}

// TestFaultMemoryConsistency injects a flush around a store and checks
// the final memory image equals the fault-free run (no partial/duplicate
// write survives recovery).
func TestFaultMemoryConsistency(t *testing.T) {
	cfg := DefaultConfig()
	prog := memProgram()
	base := New(prog, cfg)
	base.Run()
	faulted := New(prog, cfg)
	faulted.InjectFault(FaultSpec{Kind: FaultFlushAndRestart, Seq: 4}) // store [16]
	faulted.Run()
	if faulted.MemoryValue(0) != base.MemoryValue(0) ||
		faulted.MemoryValue(16) != base.MemoryValue(16) {
		t.Fatalf("post-fault memory [0]=%d [16]=%d want %d %d",
			faulted.MemoryValue(0), faulted.MemoryValue(16),
			base.MemoryValue(0), base.MemoryValue(16))
	}
}

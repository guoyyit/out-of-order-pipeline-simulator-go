package pipeline

import (
	"reflect"
	"testing"
)

// refRun is a sequential in-order reference interpreter used to check
// that out-of-order execution preserves program semantics.
func refRun(prog Program, numRegs int, memSize uint64) (regs map[int]int64, mem map[uint64]int64, exc *Exception) {
	regs = make(map[int]int64)
	for i := 0; i < numRegs; i++ {
		regs[i] = 0
	}
	mem = make(map[uint64]int64)
	pc := 0
	for pc >= 0 && pc < len(prog) {
		ins := prog[pc]
		next := pc + 1
		switch ins.Op {
		case OpNop:
		case OpAdd:
			regs[ins.Rd] = regs[ins.Rs1] + regs[ins.Rs2]
		case OpSub:
			regs[ins.Rd] = regs[ins.Rs1] - regs[ins.Rs2]
		case OpMul:
			regs[ins.Rd] = regs[ins.Rs1] * regs[ins.Rs2]
		case OpDiv:
			if regs[ins.Rs2] == 0 {
				return regs, mem, &Exception{Kind: ExceptionDivideByZero, PC: pc}
			}
			regs[ins.Rd] = regs[ins.Rs1] / regs[ins.Rs2]
		case OpMov:
			regs[ins.Rd] = ins.Imm
		case OpLoad:
			if ins.Addr >= memSize {
				return regs, mem, &Exception{Kind: ExceptionInvalidMemory, PC: pc}
			}
			regs[ins.Rd] = mem[ins.Addr]
		case OpStore:
			if ins.Addr >= memSize {
				return regs, mem, &Exception{Kind: ExceptionInvalidMemory, PC: pc}
			}
			mem[ins.Addr] = regs[ins.Rs1]
		case OpBranch:
			if regs[ins.Cond] != 0 {
				next = ins.Target
			}
		case OpJump:
			next = ins.Target
		case OpHalt:
			return regs, mem, nil
		default:
			return regs, mem, &Exception{Kind: ExceptionInvalidOpcode, PC: pc}
		}
		pc = next
	}
	return regs, mem, nil
}

func runMachine(t *testing.T, prog Program, cfg Config) Result {
	t.Helper()
	m := New(prog, cfg)
	return m.Run()
}

// TestBasicDependencies covers RAW/WAR/WAW register conflicts: renamed
// versions must not lose updates or yield stale values.
func TestBasicDependencies(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 10},        // r1 = 10
		{Op: OpMov, Rd: 2, Imm: 3},         // r2 = 3
		{Op: OpAdd, Rd: 3, Rs1: 1, Rs2: 2}, // r3 = r1 + r2 (RAW)
		{Op: OpMul, Rd: 1, Rs1: 3, Rs2: 2}, // r1 = r3 * r2 (WAR on r1, WAW)
		{Op: OpSub, Rd: 4, Rs1: 1, Rs2: 3}, // r4 = r1 - r3 (RAW on new r1)
		{Op: OpAdd, Rd: 1, Rs1: 1, Rs2: 4}, // r1 = r1 + r4 (WAW again)
		{Op: OpHalt},
	}
	res := runMachine(t, prog, Config{Seed: 1})
	wantRegs, _, wantExc := refRun(prog, 8, 4096)
	if wantExc != nil || res.Exception != nil {
		t.Fatalf("unexpected exception: ref=%v sim=%v", wantExc, res.Exception)
	}
	if !reflect.DeepEqual(res.Retired, wantRegs) {
		t.Fatalf("regs mismatch: got %v want %v", res.Retired, wantRegs)
	}
	if res.Committed != len(prog) {
		t.Fatalf("committed %d instructions, want %d", res.Committed, len(prog))
	}
	// r1 = (10+3)*3 + ((10+3)*3 - (10+3)) = 39 + 26 = 65
	if res.Retired[1] != 65 {
		t.Fatalf("r1 = %d, want 65", res.Retired[1])
	}
}

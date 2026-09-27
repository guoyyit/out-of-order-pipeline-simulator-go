package pipeline

import "testing"

// TestCommitOrderAndVisibility: commits happen strictly in program order,
// and mid-run CommittedState never exposes uncommitted results.
func TestCommitOrderAndVisibility(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 100},       // 0
		{Op: OpMul, Rd: 2, Rs1: 1, Rs2: 1}, // 1: depends on 0
		{Op: OpMov, Rd: 3, Imm: 7},         // 2: independent
		{Op: OpStore, Rs1: 3, Addr: 70},    // 3
		{Op: OpHalt},                       // 4
	}
	m := New(prog, Config{Seed: 1, IssueWidth: 4})
	lastPC := -1
	for !m.halted {
		m.Step()
		// Committed PCs observed so far must be a strict program-order
		// prefix: track via trace tail.
		tr := m.trace.Events
		for _, e := range tr {
			if e.Kind == TraceCommit {
				if e.PC <= lastPC && e.PC != lastPC {
					t.Fatalf("commit out of program order: pc %d after %d", e.PC, lastPC)
				}
				if e.PC > lastPC {
					lastPC = e.PC
				}
			}
		}
		// Any architectural register not yet committed must read as its
		// old (committed) value, never a speculative one.
		regs, mem := m.CommittedState()
		if lastPC < 1 && regs[2] != 0 {
			t.Fatalf("uncommitted r2=%d visible at pc<=%d", regs[2], lastPC)
		}
		if lastPC < 3 {
			if _, ok := mem[70]; ok {
				t.Fatalf("uncommitted store visible at pc<=%d", lastPC)
			}
		}
	}
	if lastPC != 4 {
		t.Fatalf("last committed pc = %d, want 4", lastPC)
	}
}

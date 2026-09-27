package pipeline

import (
	"reflect"
	"testing"
)

// TestStoreLoadForwarding: a load must observe the value of an older
// uncommitted store to the same address (RAW through memory), never a
// stale memory value.
func TestStoreLoadForwarding(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 77},
		{Op: OpStore, Rs1: 1, Addr: 10}, // mem[10] = 77
		{Op: OpLoad, Rd: 2, Addr: 10},   // r2 = mem[10] -> must forward 77
		{Op: OpAdd, Rd: 3, Rs1: 2, Rs2: 2},
		{Op: OpHalt},
	}
	for _, policy := range []UnknownAddrPolicy{PolicyConservative, PolicyNoAlias} {
		res := runMachine(t, prog, Config{Seed: 4, MemPolicy: policy})
		wantRegs, wantMem, _ := refRun(prog, 8, 4096)
		if !reflect.DeepEqual(res.Retired, wantRegs) {
			t.Fatalf("policy %d: regs mismatch: got %v want %v", policy, res.Retired, wantRegs)
		}
		if !reflect.DeepEqual(res.Memory, wantMem) {
			t.Fatalf("policy %d: memory mismatch: got %v want %v", policy, res.Memory, wantMem)
		}
		if res.Retired[2] != 77 || res.Retired[3] != 154 {
			t.Fatalf("policy %d: forwarding failed: %v", policy, res.Retired)
		}
	}
}

// TestWAWOrdering: two stores to the same address must retire in program
// order; a later load observes the last writer.
func TestWAWOrdering(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 1},
		{Op: OpMov, Rd: 2, Imm: 2},
		{Op: OpStore, Rs1: 1, Addr: 20},
		{Op: OpStore, Rs1: 2, Addr: 20},
		{Op: OpLoad, Rd: 3, Addr: 20},
		{Op: OpHalt},
	}
	res := runMachine(t, prog, Config{Seed: 6})
	if res.Memory[20] != 2 || res.Retired[3] != 2 {
		t.Fatalf("WAW violated: mem[20]=%d r3=%d", res.Memory[20], res.Retired[3])
	}
}

// TestWAROrdering: a load older than a same-address store must read the
// old memory value even if the store executes first.
func TestWAROrdering(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 5},
		{Op: OpStore, Rs1: 1, Addr: 30}, // mem[30] = 5
		{Op: OpLoad, Rd: 2, Addr: 30},   // r2 = 5 (old value)
		{Op: OpMov, Rd: 3, Imm: 9},
		{Op: OpStore, Rs1: 3, Addr: 30}, // mem[30] = 9 (younger store)
		{Op: OpHalt},
	}
	res := runMachine(t, prog, Config{Seed: 8})
	if res.Retired[2] != 5 {
		t.Fatalf("WAR violated: r2=%d, want 5", res.Retired[2])
	}
	if res.Memory[30] != 9 {
		t.Fatalf("final mem[30]=%d, want 9", res.Memory[30])
	}
}

// TestNoAliasIndependent: accesses to different addresses may be
// reordered freely; results still match the reference.
func TestNoAliasIndependent(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 11},
		{Op: OpMov, Rd: 2, Imm: 22},
		{Op: OpStore, Rs1: 1, Addr: 1},
		{Op: OpStore, Rs1: 2, Addr: 2},
		{Op: OpLoad, Rd: 3, Addr: 2},
		{Op: OpLoad, Rd: 4, Addr: 1},
		{Op: OpAdd, Rd: 5, Rs1: 3, Rs2: 4},
		{Op: OpHalt},
	}
	res := runMachine(t, prog, Config{Seed: 9})
	wantRegs, wantMem, _ := refRun(prog, 8, 4096)
	if !reflect.DeepEqual(res.Retired, wantRegs) || !reflect.DeepEqual(res.Memory, wantMem) {
		t.Fatalf("mismatch: got (%v,%v) want (%v,%v)", res.Retired, res.Memory, wantRegs, wantMem)
	}
}

// TestConservativePolicyReproducible: the unknown-address policy is
// configurable and reproducible — identical runs under each policy.
func TestConservativePolicyReproducible(t *testing.T) {
	prog := Program{
		{Op: OpMov, Rd: 1, Imm: 3},
		{Op: OpStore, Rs1: 1, Addr: 5},
		{Op: OpLoad, Rd: 2, Addr: 5},
		{Op: OpLoad, Rd: 3, Addr: 6},
		{Op: OpHalt},
	}
	for _, policy := range []UnknownAddrPolicy{PolicyConservative, PolicyNoAlias} {
		a := runMachine(t, prog, Config{Seed: 12, MemPolicy: policy})
		b := runMachine(t, prog, Config{Seed: 12, MemPolicy: policy})
		if !reflect.DeepEqual(a.Trace.Events, b.Trace.Events) {
			t.Fatalf("policy %d: traces differ across identical runs", policy)
		}
		if !reflect.DeepEqual(a.Retired, b.Retired) {
			t.Fatalf("policy %d: results differ across identical runs", policy)
		}
	}
}

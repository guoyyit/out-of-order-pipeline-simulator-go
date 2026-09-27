// Package pipeline implements a local out-of-order instruction pipeline
// simulator. It reads an in-order instruction sequence, discovers register
// and control dependencies dynamically, schedules instructions out of order
// without changing program semantics, and recovers to a deterministic
// consistent state on exceptions or mispredictions.
package pipeline

// Op identifies the operation an instruction performs.
type Op int

const (
	OpNop Op = iota
	OpAdd
	OpSub
	OpMul
	OpDiv // may raise ErrDivideByZero at execution time
	OpMov
	OpLoad
	OpStore
	OpBranch // conditional branch: if Cond != 0 then PC = Target
	OpJump   // unconditional jump to Target
	OpHalt
)

// Instruction is a single static instruction in program order.
//
// Rd is the destination register (write), Rs1/Rs2 are source registers
// (read). Imm is an immediate operand. Addr is the memory address for
// loads/stores. Target is the branch/jump target (instruction index).
// Cond is the register tested by OpBranch.
type Instruction struct {
	Op     Op
	Rd     int
	Rs1    int
	Rs2    int
	Imm    int64
	Addr   uint64
	Target int
	Cond   int
}

// Program is an instruction sequence in program order.
type Program []Instruction

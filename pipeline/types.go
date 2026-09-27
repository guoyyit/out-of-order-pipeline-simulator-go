// Package pipeline implements a local, in-memory out-of-order instruction
// pipeline simulator. It dynamically detects register and memory data
// dependencies as well as control dependencies, schedules instructions
// out of order while preserving architectural semantics, commits all
// architectural updates in program order, and supports precise recovery
// from branch mispredictions, exceptions/interrupts and injected faults.
//
// The simulator is fully self contained: the instruction stream lives in
// memory and no external devices or network are required.
package pipeline

// Tag identifies a ROB slot / one in-flight instruction version.
type Tag int

// InvalidTag marks an unassigned tag.
const InvalidTag Tag = -1

// PReg is a physical register number produced by register renaming.
type PReg int

// OpKind enumerates the simulated instruction classes.
type OpKind int

const (
	OpALU    OpKind = iota // integer register operation
	OpLoad                 // memory load
	OpStore                // memory store
	OpBranch               // conditional branch (control flow)
	OpJump                 // unconditional jump / call
	OpTrap                 // always raises an exception (e.g. illegal/syscall)
)

// ALUOp enumerates the arithmetic operations understood by OpALU.
type ALUOp int

const (
	ALUAdd ALUOp = iota
	ALUSub
	ALUAnd
	ALUOr
	ALUXor
	ALUSll // shift left logical
	ALUSrl // shift right logical
	ALUSlt // set on less than
)

// ExceptionCode identifies the architectural reason for recovery.
type ExceptionCode int

const (
	ExcNone        ExceptionCode = 0
	ExcTrap        ExceptionCode = 1 // explicit trap instruction
	ExcIllegal     ExceptionCode = 2 // unknown opcode/operands
	ExcLoadAddr    ExceptionCode = 3 // bad load address
	ExcStoreAddr   ExceptionCode = 4 // bad store address
	ExcInterrupt   ExceptionCode = 5 // externally injected interrupt
	ExcMispredict  ExceptionCode = 6 // control-flow misprediction recovery
	ExcMemOrder    ExceptionCode = 7 // conservative memory-order violation replay
	ExcInjected    ExceptionCode = 8 // fault injected by the test/debug hook
	ExcUnknownInst ExceptionCode = 9 // instruction sequence index out of range
)

func (c ExceptionCode) String() string {
	switch c {
	case ExcNone:
		return "none"
	case ExcTrap:
		return "trap"
	case ExcIllegal:
		return "illegal-instruction"
	case ExcLoadAddr:
		return "load-address-misaligned"
	case ExcStoreAddr:
		return "store-address-misaligned"
	case ExcInterrupt:
		return "interrupt"
	case ExcMispredict:
		return "branch-misprediction"
	case ExcMemOrder:
		return "memory-order-violation"
	case ExcInjected:
		return "injected-fault"
	case ExcUnknownInst:
		return "unknown-instruction"
	default:
		return "unknown-exception"
	}
}

// UnknownAddrPolicy selects the conservative behavior when a load/store
// effective address is not yet known while ordering memory operations.
type UnknownAddrPolicy int

const (
	// PolicyStall stalls the younger memory operation until every older
	// store has computed its address (always safe).
	PolicyStall UnknownAddrPolicy = iota
	// PolicyAssumeAlias speculates no aliasing and replays the load if a
	// later-resolved older store turns out to alias it.
	PolicyAssumeAlias
)

// BranchCond enumerates branch conditions for OpBranch (kept separate
// from ALUOp so the two encodings never collide).
type BranchCond int

const (
	BrEQ BranchCond = iota // Rs1 == Rs2
	BrNE                   // Rs1 != Rs2
	BrLT                   // Rs1 <  Rs2 (signed)
	BrGE                   // Rs1 >= Rs2 (signed)
	BrNZ                   // Rs1 != 0
)

// Instruction is a single static instruction in program order.
type Instruction struct {
	Seq      int           // program-order index (0 based)
	PC       int64         // fetch address
	Kind     OpKind        // instruction class
	ALU      ALUOp         // operation for OpALU
	Branch   BranchCond    // condition for OpBranch
	Rd       int           // destination logical register; -1 if none
	Rs1      int           // first source logical register; -1 if none
	Rs2      int           // second source logical register; -1 if none
	Imm      int64         // immediate / displacement
	Target   int64         // branch/jump taken target PC
	TrapCode ExceptionCode // exception raised by OpTrap
}

// Config holds all simulation knobs. The zero value is not valid; use
// DefaultConfig.
type Config struct {
	NumLogicalRegs int
	ROBSize        int
	IssueWidth     int
	CommitWidth    int
	ALULatency     int
	LoadLatency    int
	StoreLatency   int
	BranchLatency  int
	UnknownPolicy  UnknownAddrPolicy
	// MemoryGranularity declares addresses must be multiples of this value;
	// a mismatch raises a precise load/store address exception.
	MemoryGranularity int64
	// EnablePredictor toggles dynamic branch prediction. When false every
	// branch is predicted not-taken.
	EnablePredictor bool
	// Seed makes all scheduling tie-breaks deterministic.
	Seed int64
	// MaxCycles bounds the run as a safety net (0 = unbounded).
	MaxCycles int
}

// DefaultConfig returns a conservative, deterministic configuration.
func DefaultConfig() Config {
	return Config{
		NumLogicalRegs:    32,
		ROBSize:           16,
		IssueWidth:        4,
		CommitWidth:       4,
		ALULatency:        1,
		LoadLatency:       2,
		StoreLatency:      1,
		BranchLatency:     1,
		UnknownPolicy:     PolicyStall,
		MemoryGranularity: 4,
		EnablePredictor:   true,
		Seed:              1,
		MaxCycles:         100000,
	}
}

// Event kinds emitted onto the scheduling/commit trace.
type EventKind int

const (
	EvFetch EventKind = iota
	EvRename
	EvIssue
	EvExecuteStart
	EvComplete
	EvMemAddr
	EvWriteback
	EvCommit
	EvSquash
	EvException
	EvInterrupt
	EvRestart
)

func (e EventKind) String() string { return eventName(e) }

// Event is one entry of the deterministic execution trace.
type Event struct {
	Cycle  int
	Kind   EventKind
	Seq    int
	PC     int64
	Tag    Tag
	Detail string
}

// CommitRecord is the architectural commit-side trace entry. Commit
// records always appear in program order.
type CommitRecord struct {
	Cycle int
	Seq   int
	PC    int64
	Rd    int
	// Value written to the architectural register (0 when Rd < 0).
	Value int64
	// Addr is the effective address for loads/stores, 0 otherwise.
	Addr int64
}

// Recovery describes the most recent precise recovery point and is
// queryable after the run.
type Recovery struct {
	Cycle   int
	Seq     int // instruction that triggered recovery
	PC      int64
	Code    ExceptionCode // architectural reason
	Message string
}

func eventName(e EventKind) string {
	switch e {
	case EvFetch:
		return "fetch"
	case EvRename:
		return "rename"
	case EvIssue:
		return "issue"
	case EvExecuteStart:
		return "execute-start"
	case EvComplete:
		return "complete"
	case EvMemAddr:
		return "memory-address"
	case EvWriteback:
		return "writeback"
	case EvCommit:
		return "commit"
	case EvSquash:
		return "squash"
	case EvException:
		return "exception"
	case EvInterrupt:
		return "interrupt"
	case EvRestart:
		return "restart"
	default:
		return "unknown"
	}
}

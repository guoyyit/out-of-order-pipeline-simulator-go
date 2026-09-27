package pipeline

import "fmt"

// ExceptionKind classifies the reason an instruction raised an exception.
type ExceptionKind int

const (
	ExceptionNone ExceptionKind = iota
	ExceptionDivideByZero
	ExceptionInvalidMemory
	ExceptionInvalidOpcode
)

// Exception records a precise exception: the program-order index of the
// faulting instruction and the reason. It is queryable after recovery.
type Exception struct {
	Kind ExceptionKind
	// PC is the program-order index of the faulting instruction.
	PC int
}

func (e *Exception) Error() string {
	return fmt.Sprintf("exception %s at pc=%d", e.Kind, e.PC)
}

func (k ExceptionKind) String() string {
	switch k {
	case ExceptionDivideByZero:
		return "divide-by-zero"
	case ExceptionInvalidMemory:
		return "invalid-memory"
	case ExceptionInvalidOpcode:
		return "invalid-opcode"
	default:
		return "none"
	}
}

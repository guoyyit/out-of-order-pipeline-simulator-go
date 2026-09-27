# Out-of-Order Pipeline Simulator (Go)

A local, self-contained processor instruction-pipeline simulator. It reads
an in-order instruction sequence, dynamically discovers register data
dependencies and control dependencies, schedules instructions out of order
without changing program semantics, and recovers to a deterministic,
consistent state on branch mispredictions, exceptions, or injected faults.
No external devices or network access are involved.

## Layout

| File | Role |
| --- | --- |
| `pipeline/instruction.go` | Instruction set and `Program` (in-order sequence) |
| `pipeline/machine.go` | Pipeline core: issue/rename, execute, commit, recovery |
| `pipeline/rename.go` | Physical register file + rename table (versioning) |
| `pipeline/rob.go` | Reorder buffer (in-order commit queue) |
| `pipeline/memory.go` | Committed data memory + unknown-address policy |
| `pipeline/exception.go` | Precise exception records |
| `pipeline/trace.go` | Scheduling/commit trace events |
| `pipeline/fault.go` | Deterministic fault injector |

## Instruction set

`Instruction{Op, Rd, Rs1, Rs2, Imm, Addr, Target, Cond}`:

- `OpNop`
- `OpAdd/OpSub/OpMul/OpDiv Rd, Rs1, Rs2` — `OpDiv` raises a precise
  divide-by-zero exception when `Rs2 == 0`
- `OpMov Rd, Imm`
- `OpLoad Rd, [Addr]` / `OpStore Rs1, [Addr]` — invalid addresses raise a
  precise invalid-memory exception
- `OpBranch Cond, Target` — jump to `Target` if register `Cond != 0`
- `OpJump Target`
- `OpHalt`

## Execution semantics

- **Fetch/issue** is in program order. Each issued instruction is renamed:
  its destination architectural register is bound to a fresh physical
  register (a new *version*), and its sources are bound to the physical
  versions of their producers. This removes WAR/WAW hazards while
  preserving RAW data flow; a logical register moving between versions
  never loses an update and never yields a stale value.
- **Execute** is out of order: an instruction runs once its physical
  source operands are ready. When more instructions are ready than the
  execution width, a PRNG seeded by `Config.Seed` picks among them, so
  timing may vary by seed but committed results never do.
- **Commit** is strictly in program order from the head of the reorder
  buffer. Only committed results are externally visible
  (`Result.Retired`, `Result.Memory`, `CommittedState`); uncommitted
  physical-register versions and ROB-held store data are never observable.
- **Memory ordering**: stores write memory only at commit, so WAW/WAR
  through memory cannot be violated. A load may not pass an older store
  to the same address (it stalls, then forwards the store's data). While
  an older store's address is unknown, `Config.MemPolicy` selects the
  behaviour: `PolicyConservative` (default) stalls the load;
  `PolicyNoAlias` proceeds (unsafe, for experimentation). Both are
  deterministic and reproducible.

## Recovery semantics

- **Branch misprediction**: branches are predicted not-taken. When a
  branch resolves taken, every younger (speculative, uncommitted) ROB
  entry is squashed, renames are undone youngest-first, physical registers
  are reclaimed, and fetch restarts at the correct target
  (`Config.BranchMissLat` models the penalty). Committed state is
  untouched.
- **Precise exceptions**: an exception raised during execution is recorded
  in the ROB entry and *taken at commit time*, when all older
  instructions have committed. The faulting instruction and everything
  younger are discarded without becoming visible. The reason and the
  recovery position are queryable via `Result.Exception` and
  `Machine.LastException()` (returns the exception and the program-order
  index to resume from).
- **Fault injection**: `Config.Fault = &FaultInjector{AtCycle: N}` drops
  all uncommitted state at cycle `N` and rolls back to the last committed
  checkpoint (committed registers + committed memory). Execution resumes
  from the oldest uncommitted instruction; the recovered run's committed
  state and commit order are identical to a fault-free run, with no
  partial writes.

## Determinism and replay

Every scheduling and commit decision is recorded as a `TraceEvent`
(fetch, issue, execute, writeback, commit, squash, recover, fault,
rollback). With the same program and the same `Config.Seed`, the emitted
trace is identical entry by entry, supporting offline replay and
historical re-execution. Different seeds may reorder ready instructions
but always produce the same committed architectural state.

## Testing

```
go test ./...
```

Covers: register dependency conflicts (RAW/WAR/WAW), branch
misprediction and loops, precise exception recovery, memory aliasing
(forwarding, WAW, WAR, conservative policy), fault injection and
rollback, commit-order/visibility guarantees, deterministic replay, and a
200-seed randomized fuzz comparison against a sequential reference
interpreter.

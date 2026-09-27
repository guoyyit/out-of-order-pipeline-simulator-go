package pipeline

// Config tunes the simulated machine. All randomness derives from Seed so
// runs are reproducible: identical input and identical Seed produce an
// identical trace, entry by entry.
type Config struct {
	NumArchRegs   int // architectural registers
	NumPhysRegs   int // physical registers (>= NumArchRegs)
	ROBSize       int
	IssueWidth    int // instructions issued/executed per cycle
	MemSize       uint64
	MemPolicy     UnknownAddrPolicy
	Seed          int64
	Fault         *FaultInjector // optional
	MaxCycles     uint64         // safety bound; 0 means default
	BranchMissLat int            // extra fetch-stall cycles on misprediction
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.NumArchRegs <= 0 {
		out.NumArchRegs = 8
	}
	if out.NumPhysRegs < out.NumArchRegs {
		out.NumPhysRegs = out.NumArchRegs * 4
	}
	if out.ROBSize <= 0 {
		out.ROBSize = 32
	}
	if out.IssueWidth <= 0 {
		out.IssueWidth = 2
	}
	if out.MemSize == 0 {
		out.MemSize = 4096
	}
	if out.MaxCycles == 0 {
		out.MaxCycles = 1 << 20
	}
	return out
}

// Result summarises a finished run. Only committed state is exposed;
// uncommitted results are never observable.
type Result struct {
	Cycles    uint64
	Committed int
	Retired   map[int]int64 // committed architectural registers at halt
	Memory    map[uint64]int64
	Exception *Exception // non-nil if the run stopped on a precise exception
	Trace     Trace
}

// Machine is the out-of-order pipeline simulator.
type Machine struct {
	cfg     Config
	prog    Program
	cycle   uint64
	fetchPC int

	phys   *physFile
	rename *renameTable
	rob    *reorderBuffer
	mem    *Memory

	trace     Trace
	committed int
	halted    bool
	fetchDone bool
	missStall int
	lastExc   *Exception
	recoverPC int

	// ckptRegs is the committed architectural register state. It is only
	// written at commit time, so it doubles as the fault-rollback
	// checkpoint.
	ckptRegs map[int]int64

	rng uint64
}

// New builds a machine for the given program.
func New(prog Program, cfg Config) *Machine {
	c := cfg.withDefaults()
	m := &Machine{
		cfg:      c,
		prog:     prog,
		phys:     newPhysFile(c.NumArchRegs, c.NumPhysRegs),
		rename:   newRenameTable(c.NumArchRegs),
		rob:      newReorderBuffer(c.ROBSize),
		mem:      newMemory(c.MemSize),
		ckptRegs: make(map[int]int64),
		rng:      uint64(c.Seed)*2654435761 + 1,
	}
	m.fetchDone = len(prog) == 0
	return m
}

// Run executes until halt, exception, or the cycle bound, and returns the
// result including the full trace.
func (m *Machine) Run() Result {
	for !m.halted && m.cycle < m.cfg.MaxCycles {
		m.Step()
	}
	return m.result()
}

// Step advances the machine one cycle: commit, execute, then issue.
func (m *Machine) Step() {
	if m.halted {
		return
	}
	m.cycle++

	// Fault injection discards all uncommitted state and rolls back to
	// the last committed checkpoint.
	if m.cfg.Fault != nil && m.cfg.Fault.armed(m.cycle) {
		m.cfg.Fault.markFired()
		m.injectFault()
		return
	}

	m.commitStage()
	if m.halted {
		return
	}
	m.executeStage()
	m.issueStage()

	if m.rob.empty() && m.fetchDone {
		m.halted = true
	}
}

// LastException returns the precise exception that stopped the machine and
// the program-order index from which execution resumes after recovery.
// It returns (nil, 0) if no exception occurred.
func (m *Machine) LastException() (*Exception, int) {
	if m.lastExc == nil {
		return nil, 0
	}
	return m.lastExc, m.recoverPC
}

// CommittedState snapshots the currently committed (externally visible)
// architectural registers and memory. Uncommitted results are never
// included.
func (m *Machine) CommittedState() (map[int]int64, map[uint64]int64) {
	regs := make(map[int]int64, m.cfg.NumArchRegs)
	for i := 0; i < m.cfg.NumArchRegs; i++ {
		regs[i] = m.ckptRegs[i]
	}
	mem := make(map[uint64]int64, len(m.mem.words))
	for a, v := range m.mem.words {
		mem[a] = v
	}
	return regs, mem
}

// --- commit stage -------------------------------------------------------

func (m *Machine) commitStage() {
	for !m.rob.empty() {
		h := m.rob.at(m.rob.head)
		if h.state != robCompleted {
			return
		}
		if h.exc != nil {
			// Precise exception: everything older has committed; the
			// faulting instruction and all younger uncommitted state
			// are discarded without becoming visible.
			exc := *h.exc
			pc := h.pc
			m.squashFrom(m.rob.head)
			m.lastExc = &exc
			m.recoverPC = pc
			m.halted = true
			m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceRecover, PC: pc, ROB: -1, Note: "precise-exception:" + exc.Kind.String()})
			return
		}
		if h.archRd >= 0 {
			m.ckptRegs[h.archRd] = h.value
			m.phys.release(h.oldDest)
		}
		if h.isStore {
			m.mem.write(h.addr, h.storeVal)
		}
		m.committed++
		m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceCommit, PC: h.pc, ROB: m.rob.head})
		m.rob.pop()
	}
}

// --- execute stage ------------------------------------------------------

func (m *Machine) executeStage() {
	// Collect ready entries in program order, then pick up to IssueWidth
	// of them using the seeded PRNG. The pick order affects the trace but
	// never the committed result.
	var ready []int
	for i := 0; i < m.rob.count; i++ {
		idx := (m.rob.head + i) % len(m.rob.entries)
		if m.ready(m.rob.at(idx)) {
			ready = append(ready, idx)
		}
	}
	for n := 0; n < m.cfg.IssueWidth && len(ready) > 0; n++ {
		j := int(m.next() % uint64(len(ready)))
		idx := ready[j]
		ready = append(ready[:j], ready[j+1:]...)
		if m.execute(m.rob.at(idx)) {
			// A mispredicted branch squashed everything younger;
			// stop executing this cycle.
			return
		}
	}
}

// ready reports whether an entry's operands and ordering constraints are
// satisfied.
func (m *Machine) ready(e *robEntry) bool {
	if e.state != robIssued {
		return false
	}
	if e.src1 != invalidPhys && !m.phys.isReady(e.src1) {
		return false
	}
	if e.src2 != invalidPhys && !m.phys.isReady(e.src2) {
		return false
	}
	if e.isLoad {
		return m.loadOrdered(e)
	}
	return true
}

// loadOrdered enforces conservative memory ordering: a load may not
// execute while an older store's address is unknown (PolicyConservative)
// or while an older store to the same address has not completed (its data
// must be forwarded). RAW/WAW through memory are never reordered.
func (m *Machine) loadOrdered(load *robEntry) bool {
	for i := 0; i < m.rob.count; i++ {
		e := m.rob.at((m.rob.head + i) % len(m.rob.entries))
		if e == load {
			return true
		}
		if !e.isStore {
			continue
		}
		if !e.addrOK {
			if m.cfg.MemPolicy == PolicyConservative {
				return false
			}
			continue
		}
		if e.addr == load.addr && e.state != robCompleted {
			return false
		}
	}
	return true
}

// execute performs one instruction. It returns true if a mispredicted
// branch was resolved (and the squash performed).
func (m *Machine) execute(e *robEntry) bool {
	m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceExecute, PC: e.pc, ROB: m.slotOf(e)})
	v1 := m.readSrc(e.src1)
	v2 := m.readSrc(e.src2)

	switch e.op {
	case OpNop:
	case OpAdd:
		e.value = v1 + v2
	case OpSub:
		e.value = v1 - v2
	case OpMul:
		e.value = v1 * v2
	case OpDiv:
		if v2 == 0 {
			e.exc = &Exception{Kind: ExceptionDivideByZero, PC: e.pc}
		} else {
			e.value = v1 / v2
		}
	case OpMov:
		e.value = m.prog[e.pc].Imm
	case OpLoad:
		if !m.mem.valid(e.addr) {
			e.exc = &Exception{Kind: ExceptionInvalidMemory, PC: e.pc}
		} else {
			e.value = m.loadValue(e)
		}
	case OpStore:
		e.storeVal = v1
		e.addrOK = true
		if !m.mem.valid(e.addr) {
			e.exc = &Exception{Kind: ExceptionInvalidMemory, PC: e.pc}
		}
	case OpBranch:
		e.resolved = true
		if v1 != 0 {
			e.actualPC = m.prog[e.pc].Target
		} else {
			e.actualPC = e.pc + 1
		}
		if e.actualPC != e.predPC {
			e.mispred = true
		}
	case OpJump:
		e.resolved = true
		e.actualPC = m.prog[e.pc].Target
	case OpHalt:
	default:
		e.exc = &Exception{Kind: ExceptionInvalidOpcode, PC: e.pc}
	}

	if e.archRd >= 0 && e.exc == nil {
		m.phys.write(e.dest, e.value)
	}
	e.state = robCompleted
	m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceWriteback, PC: e.pc, ROB: m.slotOf(e)})

	if e.mispred {
		m.recoverMispredict(e)
		return true
	}
	return false
}

// loadValue resolves a load's data, forwarding from the youngest older
// completed store to the same address when one exists.
func (m *Machine) loadValue(load *robEntry) int64 {
	var fwd *robEntry
	for i := 0; i < m.rob.count; i++ {
		e := m.rob.at((m.rob.head + i) % len(m.rob.entries))
		if e == load {
			break
		}
		if e.isStore && e.addrOK && e.addr == load.addr && e.state == robCompleted {
			fwd = e
		}
	}
	if fwd != nil {
		return fwd.storeVal
	}
	v, _ := m.mem.read(load.addr)
	return v
}

// recoverMispredict squashes every instruction younger than the branch and
// redirects fetch to the correct target. Committed state is untouched.
func (m *Machine) recoverMispredict(br *robEntry) {
	slot := m.slotOf(br)
	younger := m.rob.tail()
	if younger != slot {
		m.squashFrom((slot + 1) % len(m.rob.entries))
	}
	m.fetchPC = br.actualPC
	m.fetchDone = br.actualPC < 0 || br.actualPC >= len(m.prog)
	m.missStall = m.cfg.BranchMissLat
	m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceRecover, PC: br.pc, ROB: slot, Note: "branch-mispredict"})
}

// --- issue stage --------------------------------------------------------

func (m *Machine) issueStage() {
	if m.missStall > 0 {
		m.missStall--
		return
	}
	for n := 0; n < m.cfg.IssueWidth; n++ {
		if m.fetchDone || m.rob.full() {
			return
		}
		pc := m.fetchPC
		if pc < 0 || pc >= len(m.prog) {
			m.fetchDone = true
			return
		}
		ins := m.prog[pc]
		e := robEntry{
			pc:       pc,
			op:       ins.Op,
			state:    robIssued,
			archRd:   -1,
			dest:     invalidPhys,
			oldDest:  invalidPhys,
			src1:     invalidPhys,
			src2:     invalidPhys,
			isStore:  ins.Op == OpStore,
			isLoad:   ins.Op == OpLoad,
			addr:     ins.Addr,
			branch:   ins.Op == OpBranch || ins.Op == OpJump,
			actualPC: -1,
		}
		// Map source operands through the rename table so consumers
		// bind to the exact producer version.
		if readsRs1(ins.Op) {
			e.src1 = m.rename.lookup(ins.Rs1)
		}
		if readsRs2(ins.Op) {
			e.src2 = m.rename.lookup(ins.Rs2)
		}
		if ins.Op == OpBranch {
			e.src1 = m.rename.lookup(ins.Cond)
			e.predPC = pc + 1 // predict not-taken
		}
		// Allocate a new physical version for the destination.
		if writesRd(ins.Op) {
			if len(m.phys.free) == 0 {
				return // no free physical registers; retry next cycle
			}
			nw, old := m.rename.rename(ins.Rd, m.phys)
			e.dest = nw
			e.oldDest = old
			e.archRd = ins.Rd
		}
		if m.rob.push(e) < 0 {
			return
		}
		m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceFetch, PC: pc, ROB: m.rob.tail()})
		m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceIssue, PC: pc, ROB: m.rob.tail()})

		switch ins.Op {
		case OpJump:
			m.fetchPC = ins.Target
		case OpHalt:
			m.fetchDone = true
		default:
			m.fetchPC = pc + 1
		}
		if m.fetchPC < 0 || m.fetchPC >= len(m.prog) {
			m.fetchDone = true
		}
	}
}

// --- recovery helpers ---------------------------------------------------

// squashFrom removes every ROB entry from slot idx to the tail, youngest
// first, undoing renames so the rename table returns to the state at the
// recovery point.
func (m *Machine) squashFrom(idx int) {
	m.rob.squashFrom(idx, func(e *robEntry) {
		if e.archRd >= 0 {
			m.rename.mapping[e.archRd] = e.oldDest
			m.phys.release(e.dest)
		}
		m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceSquash, PC: e.pc, ROB: m.slotOf(e)})
	})
}

// injectFault discards all uncommitted state and resumes from the oldest
// uncommitted instruction. Committed registers (ckptRegs) and committed
// memory are the checkpoint; the run continues deterministically and its
// committed result is identical to a fault-free run.
func (m *Machine) injectFault() {
	m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceFault, PC: -1, ROB: -1, Note: "inject"})
	if !m.rob.empty() {
		pc := m.rob.at(m.rob.head).pc
		m.squashFrom(m.rob.head)
		m.fetchPC = pc
		m.fetchDone = pc < 0 || pc >= len(m.prog)
	}
	m.missStall = 0
	m.trace.record(TraceEvent{Cycle: m.cycle, Kind: TraceRollback, PC: m.fetchPC, ROB: -1, Note: "rollback-to-commit-point"})
}

// --- misc ----------------------------------------------------------------

func (m *Machine) readSrc(p physReg) int64 {
	if p == invalidPhys {
		return 0
	}
	return m.phys.read(p)
}

func (m *Machine) slotOf(e *robEntry) int {
	for i := 0; i < m.rob.count; i++ {
		idx := (m.rob.head + i) % len(m.rob.entries)
		if m.rob.at(idx) == e {
			return idx
		}
	}
	return -1
}

// next is a xorshift64 PRNG step, seeded from Config.Seed.
func (m *Machine) next() uint64 {
	x := m.rng
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	m.rng = x
	return x
}

func (m *Machine) result() Result {
	regs, mem := m.CommittedState()
	return Result{
		Cycles:    m.cycle,
		Committed: m.committed,
		Retired:   regs,
		Memory:    mem,
		Exception: m.lastExc,
		Trace:     m.trace,
	}
}

func readsRs1(op Op) bool {
	switch op {
	case OpAdd, OpSub, OpMul, OpDiv, OpStore:
		return true
	}
	return false
}

func readsRs2(op Op) bool {
	switch op {
	case OpAdd, OpSub, OpMul, OpDiv:
		return true
	}
	return false
}

func writesRd(op Op) bool {
	switch op {
	case OpAdd, OpSub, OpMul, OpDiv, OpMov, OpLoad:
		return true
	}
	return false
}

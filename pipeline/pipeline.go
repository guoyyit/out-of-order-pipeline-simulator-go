package pipeline

import (
	"fmt"
)

// Pipeline is the out-of-order core. It fetches a static program stored
// in memory, renames registers, executes instructions as soon as their
// operands are ready (respecting conservative memory ordering), writes
// results back to the reorder buffer and commits strictly in program
// order. All speculative work above a mispredicted, faulted or trapping
// instruction is squashed before any of it becomes architecturally
// visible.
type Pipeline struct {
	cfg     Config
	program []Instruction

	// Architectural state (only updated at commit).
	archRegs []int64
	mem      *MemoryUnit

	// Speculative machinery.
	rob      *ReorderBuffer
	rat      *RenameTable
	sched    *Scheduler
	pred     *BranchPredictor
	trace    *Trace
	prf      map[PReg]int64 // physical register file
	prfValid map[PReg]bool

	// Fetch control.
	fetchPC    int64 // next static PC to fetch
	stallFetch bool  // drained while resolving control flow / exception

	cycle    int
	halted   bool
	recovery *Recovery

	// Execution timing: tag -> cycle at which result completes.
	completeAt map[Tag]int

	faults     []*FaultSpec
	interrupts []interruptReq

	// Executed loads that ran before all older stores had known
	// addresses (PolicyAssumeAlias); validated as addresses resolve.
	specLoads map[Tag]int64
}

type interruptReq struct {
	code ExceptionCode
	msg  string
}

// New constructs a simulator for a program in program order.
func New(program []Instruction, cfg Config) *Pipeline {
	// Normalize: Seq and PC are positional properties of the in-memory
	// program (instructions are contiguous 4-byte words), not caller
	// inputs, so assign them unconditionally.
	prog := make([]Instruction, len(program))
	copy(prog, program)
	for i := range prog {
		prog[i].Seq = i
		prog[i].PC = int64(i) * 4
		// Only ALU and loads produce a register value. Stores, branches,
		// jumps and traps never write a logical register; normalize a
		// caller-omitted Rd (=0) to -1 for them so they are not treated
		// as writing r0.
		switch prog[i].Kind {
		case OpStore, OpBranch, OpJump, OpTrap:
			prog[i].Rd = -1
		}
		// Branches and stores address through Rs1 only; branch compares
		// Rs1/Rs2. Normalize -1 source operands for classes without them.
	}
	p := &Pipeline{
		cfg:        cfg,
		program:    prog,
		archRegs:   make([]int64, cfg.NumLogicalRegs),
		mem:        newMemoryUnit(cfg),
		rob:        newReorderBuffer(cfg),
		rat:        newRenameTable(cfg),
		sched:      newScheduler(cfg),
		pred:       newBranchPredictor(cfg),
		trace:      newTrace(),
		prf:        make(map[PReg]int64),
		prfValid:   make(map[PReg]bool),
		completeAt: make(map[Tag]int),
		specLoads:  make(map[Tag]int64),
	}
	for r := 0; r < cfg.NumLogicalRegs; r++ {
		p.prfValid[PReg(r)] = true
	}
	return p
}

// Reset returns the simulator to the pre-Run state, retaining the
// program and configuration and clearing all speculative and committed
// architectural state.
func (p *Pipeline) Reset() {
	for i := range p.archRegs {
		p.archRegs[i] = 0
	}
	p.mem.reset()
	p.rob.reset()
	// Rebuild the RAT/free list and physical register file.
	p.rat.reset()
	n := p.cfg.NumLogicalRegs
	p.prf = make(map[PReg]int64)
	p.prfValid = make(map[PReg]bool)
	for r := 0; r < n; r++ {
		p.prfValid[PReg(r)] = true
	}
	p.sched.reset()
	p.trace.reset()
	p.fetchPC = 0
	p.stallFetch = false
	p.cycle = 0
	p.halted = false
	p.recovery = nil
	p.completeAt = make(map[Tag]int)
	p.specLoads = make(map[Tag]int64)
	for _, f := range p.faults {
		f.fired = false
	}
	p.interrupts = nil
}

// InjectFault schedules a deterministic fault to fire during Run.
func (p *Pipeline) InjectFault(f FaultSpec) {
	cp := f
	p.faults = append(p.faults, &cp)
}

// RaiseInterrupt queues a precise asynchronous interrupt. It is taken at
// the next commit boundary: all older instructions commit, then the
// pipeline drains and RecoveryPoint reports the interrupt.
func (p *Pipeline) RaiseInterrupt(code ExceptionCode, msg string) {
	p.interrupts = append(p.interrupts, interruptReq{code: code, msg: msg})
}

// instAt returns the static instruction at pc.
func (p *Pipeline) instAt(pc int64) (*Instruction, bool) {
	idx := int(pc / 4)
	if idx < 0 || idx >= len(p.program) {
		return nil, false
	}
	return &p.program[idx], true
}

// Status reports the externally visible architectural state after Run.
type Status struct {
	Halted       bool
	Cycles       int
	CommittedSeq int
	CommittedPC  int64
	NextPC       int64
	Registers    []int64
	LastRecovery *Recovery
	Trace        *Trace
}

func (p *Pipeline) RegisterValue(reg int) int64 {
	if reg < 0 || reg >= len(p.archRegs) {
		return 0
	}
	return p.archRegs[reg]
}

func (p *Pipeline) MemoryValue(addr int64) int64 { return p.mem.read(addr) }

func (p *Pipeline) RecoveryPoint() *Recovery { return p.recovery }

func (p *Pipeline) SchedulingTrace() []Event { return p.trace.Events }

func (p *Pipeline) CommitTrace() []CommitRecord { return p.trace.Commits }

var _ = fmt.Sprintf

// Run executes until the program is committed (or a precise exception/
// interrupt halts the machine) and returns the architectural status.
// Each simulated cycle runs, in order: completion/writeback, commit,
// then fetch/rename and issue — mirroring a clocked machine.
func (p *Pipeline) Run() *Status {
	for {
		// Detect completion: nothing left to fetch and ROB drained.
		_, atFetch := p.instAt(p.fetchPC)
		if p.rob.len() == 0 && (!atFetch || p.halted) {
			p.halted = true
			return p.buildStatus()
		}
		if p.cfg.MaxCycles > 0 && p.cycle >= p.cfg.MaxCycles {
			p.recovery = &Recovery{Cycle: p.cycle, Code: ExcIllegal,
				Message: "simulation exceeded MaxCycles"}
			p.halted = true
			return p.buildStatus()
		}

		if !p.halted {
			p.complete()
			p.commit()
			if !p.halted {
				p.fetchAndRename()
				p.issue()
			}
		}
		p.cycle++
	}
}

func (p *Pipeline) buildStatus() *Status {
	st := &Status{
		Halted:       p.halted,
		Cycles:       p.cycle,
		NextPC:       p.fetchPC,
		Registers:    append([]int64(nil), p.archRegs...),
		LastRecovery: p.recovery,
		Trace:        p.trace,
	}
	if n := len(p.trace.Commits); n > 0 {
		st.CommittedSeq = p.trace.Commits[n-1].Seq + 1
		st.CommittedPC = p.trace.Commits[n-1].PC
	}
	return st
}

// fetchAndRename moves up to IssueWidth instructions through fetch and
// rename in one cycle. Branches/jumps steer the fetch stream according
// to the predictor; the prediction is recorded in the ROB entry and
// verified at execute time.
func (p *Pipeline) fetchAndRename() {
	if p.stallFetch {
		return
	}
	for i := 0; i < p.cfg.IssueWidth; i++ {
		if p.rob.full() || !p.rat.canRename(1) {
			return
		}
		inst, ok := p.instAt(p.fetchPC)
		if !ok {
			// Out of static instructions: normal end of fetch.
			return
		}
		predTaken := false
		predNextPC := p.fetchPC + 4
		if inst.Kind == OpBranch {
			predTaken = p.pred.predict(inst.Seq, inst.PC)
			if predTaken {
				predNextPC = inst.Target
			}
		} else if inst.Kind == OpJump {
			predTaken = true
			predNextPC = inst.Target
		}
		p.trace.record(Event{Cycle: p.cycle, Kind: EvFetch, Seq: inst.Seq, PC: inst.PC})

		entry := &ROBEntry{
			Seq:       inst.Seq,
			PC:        inst.PC,
			Inst:      inst,
			PredTaken: predTaken,
			NextPC:    predNextPC,
		}
		tag, err := p.rob.push(entry)
		if err != nil {
			return
		}
		rn := p.rat.rename(inst, tag)
		if rn.HasRd {
			p.prfValid[rn.RdP] = false
		}
		entry = p.rob.get(tag)
		entry.Rn = rn
		p.trace.record(Event{Cycle: p.cycle, Kind: EvRename, Seq: inst.Seq, PC: inst.PC, Tag: tag})

		// Build the scheduler reservation.
		ie := &IssueEntry{
			Tag:         tag,
			Seq:         inst.Seq,
			Src1Tag:     p.rat.producerOf(rn.Rs1P),
			Src2Tag:     p.rat.producerOf(rn.Rs2P),
			Src1V:       p.prf[rn.Rs1P],
			Src2V:       p.prf[rn.Rs2P],
			SrcStoreTag: InvalidTag,
		}
		if inst.Rs1 < 0 {
			ie.Src1Tag = InvalidTag
		}
		if inst.Rs2 < 0 {
			ie.Src2Tag = InvalidTag
		}
		// Store data comes from Rs2; the address is computed from Rs1+Imm.
		if inst.Kind == OpStore {
			ie.SrcStoreTag = ie.Src2Tag
			ie.SrcStoreV = ie.Src2V
			ie.Src2Tag = InvalidTag
			ie.Src2V = 0
		}
		// For loads only Rs1 is an address operand.
		if inst.Kind == OpLoad {
			ie.Src2Tag = InvalidTag
			ie.Src2V = 0
		}
		p.sched.add(ie)

		// Traps need no operands or execution: they are detected as soon
		// as they reach the commit head (precise exception).
		if inst.Kind == OpTrap {
			entry.Ready = true
			entry.Resolved = true
			code := inst.TrapCode
			if code == ExcNone {
				code = ExcTrap
			}
			entry.ExcCode = code
		}

		// Track memory ops in program order.
		if inst.Kind == OpLoad || inst.Kind == OpStore {
			p.mem.push(&lsqEntry{Tag: tag, Seq: inst.Seq, IsStore: inst.Kind == OpStore})
		}

		p.fetchPC = predNextPC
	}
}

// issue selects ready instructions and starts their execution timers.
// Memory ordering gates loads/stores here:
//   - PolicyStall: a memory op waits until all older stores have known
//     addresses;
//   - PolicyAssumeAlias: loads may proceed speculatively and are
//     replayed if an older later-resolved store aliases them.
func (p *Pipeline) issue() {
	ready := p.sched.selectReady(p.cfg.IssueWidth)
	for _, ie := range ready {
		e := p.rob.get(ie.Tag)
		if e == nil {
			continue
		}
		if e.Inst.Kind == OpLoad || e.Inst.Kind == OpStore {
			if p.cfg.UnknownPolicy == PolicyStall && p.mem.aliasesUnresolvedOlderStore(ie.Tag) {
				continue
			}
		}
		latency := p.cfg.ALULatency
		switch e.Inst.Kind {
		case OpLoad:
			latency = p.cfg.LoadLatency
		case OpStore:
			latency = p.cfg.StoreLatency
		case OpBranch:
			latency = p.cfg.BranchLatency
		}
		if latency < 1 {
			latency = 1
		}
		ie.Issued = true
		ie.Executing = true
		p.completeAt[ie.Tag] = p.cycle + latency
		p.trace.record(Event{Cycle: p.cycle, Kind: EvIssue, Seq: e.Seq, PC: e.PC, Tag: ie.Tag})
		p.trace.record(Event{Cycle: p.cycle, Kind: EvExecuteStart, Seq: e.Seq, PC: e.PC, Tag: ie.Tag})
	}
}

// complete executes instructions whose timers expired and writes their
// results back. It returns the number of precise recoveries triggered.
func (p *Pipeline) complete() int {
	recoveries := 0
	// Complete in tag (program) order so dependent results published the
	// same cycle observe consistent values.
	var tags []Tag
	for t, at := range p.completeAt {
		if at <= p.cycle {
			tags = append(tags, t)
		}
	}
	for i := 1; i < len(tags); i++ {
		for j := i; j > 0 && tags[j-1] > tags[j]; j-- {
			tags[j-1], tags[j] = tags[j], tags[j-1]
		}
	}
	for _, tag := range tags {
		delete(p.completeAt, tag)
		if p.rob.get(tag) == nil {
			continue // already squashed
		}
		if p.executeOne(tag) {
			recoveries++
		}
	}
	return recoveries
}

// executeOne performs one instruction's execution. It returns true if a
// precise recovery (misprediction, memory-order replay or injected
// fault) was performed, in which case the instruction and all younger
// work have been squashed.
func (p *Pipeline) executeOne(tag Tag) bool {
	e := p.rob.get(tag)
	if e == nil {
		return false
	}
	ie := p.sched.get(tag)
	inst := e.Inst

	// Fault injection point: a flush squashes this instruction and every
	// younger one and re-fetches it, identical to a misprediction. It
	// leaves no partial write; the eventual committed state matches a
	// fault-free run.
	if p.consumeFault(tag, FaultFlushAndRestart) {
		// dropBoundary=true: the faulted instruction is removed and
		// re-fetched from inst.PC; only strictly older commits survive.
		p.recover(tag, inst.PC, ExcInjected, "injected flush-and-restart", true)
		return true
	}

	// Fault injection point for the "drop result" transient: the
	// completion is discarded and the instruction re-issues.
	if p.consumeFault(tag, FaultDropResult) {
		ie.Issued = false
		ie.Executing = false
		p.trace.record(Event{Cycle: p.cycle, Kind: EvSquash, Seq: inst.Seq, PC: inst.PC, Tag: tag,
			Detail: "drop-result replay"})
		return false
	}

	switch inst.Kind {
	case OpALU:
		e.Value = p.evalALU(inst, ie.Src1V, ie.Src2V)
		p.publish(tag, e.Value)
	case OpJump:
		e.Resolved = true
		e.Taken = true
		e.NextPC = inst.Target
		p.publish(tag, 0)
	case OpTrap:
		e.Resolved = true
		e.Ready = true
		code := inst.TrapCode
		if code == ExcNone {
			code = ExcTrap
		}
		e.ExcCode = code
		p.trace.record(Event{Cycle: p.cycle, Kind: EvException, Seq: inst.Seq, PC: inst.PC, Tag: tag,
			Detail: code.String()})
		return false
	case OpBranch:
		return p.resolveBranch(tag, ie)
	case OpLoad:
		return p.executeLoad(tag, ie)
	case OpStore:
		return p.executeStore(tag, ie)
	}
	return false
}

// publish writes a completed result into the PRF, marks the ROB entry
// ready and broadcasts it to waiting scheduler entries.
func (p *Pipeline) publish(tag Tag, value int64) {
	e := p.rob.get(tag)
	if e == nil {
		return
	}
	if ie := p.sched.get(tag); ie != nil {
		ie.Done = true
	}
	if e.Rn.HasRd {
		p.prf[e.Rn.RdP] = value
		p.prfValid[e.Rn.RdP] = true
		p.rat.commitRelease(e.Rn.RdP)
	}
	e.Value = value
	e.Ready = true
	e.Resolved = true
	p.sched.writeback(tag, value)
	p.trace.record(Event{Cycle: p.cycle, Kind: EvWriteback, Seq: e.Seq, PC: e.PC, Tag: tag})
}

func (p *Pipeline) consumeFault(tag Tag, kind FaultKind) bool {
	e := p.rob.get(tag)
	if e == nil {
		return false
	}
	for _, f := range p.faults {
		if !f.fired && f.Kind == kind && f.Seq == e.Seq {
			f.fired = true
			return true
		}
	}
	return false
}

func (p *Pipeline) evalALU(inst *Instruction, a, b int64) int64 {
	switch inst.ALU {
	case ALUAdd:
		// ADD also serves as ADDI: a + b + Imm (b is 0 when Rs2 is absent).
		return a + b + inst.Imm
	case ALUSub:
		return a - b
	case ALUAnd:
		return a & b
	case ALUOr:
		return a | b
	case ALUXor:
		return a ^ b
	case ALUSll:
		return a << uint(b&63)
	case ALUSrl:
		return int64(uint64(a) >> uint(b&63))
	case ALUSlt:
		if a < b {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// resolveBranch evaluates a conditional branch and, on a misprediction,
// squashes the branch together with every younger instruction and
// redirects fetch to the correct next PC. The branch itself is NOT
// committed during recovery; it re-fetches and re-executes.
func (p *Pipeline) resolveBranch(tag Tag, ie *IssueEntry) bool {
	e := p.rob.get(tag)
	inst := e.Inst
	taken := p.branchTaken(inst, ie.Src1V, ie.Src2V)
	next := inst.PC + 4
	if taken {
		next = inst.Target
	}
	e.Taken = taken
	e.Resolved = true
	e.NextPC = next
	p.pred.resolve(inst.Seq, inst.PC, inst.Target, taken)
	p.trace.record(Event{Cycle: p.cycle, Kind: EvComplete, Seq: inst.Seq, PC: inst.PC, Tag: tag})

	if taken != e.PredTaken || (taken && next != predictedTarget(e)) {
		// The branch itself has resolved: publish (it writes no register)
		// so it can commit in order, then squash strictly younger
		// wrong-path work and redirect fetch to the correct PC.
		p.publish(tag, 0)
		p.recover(tag, next, ExcMispredict,
			fmt.Sprintf("branch at pc=%d predicted %t actual %t", inst.PC, e.PredTaken, taken), false)
		return false
	}
	// Correct prediction: finish normally.
	p.publish(tag, 0)
	return false
}

func predictedTarget(e *ROBEntry) int64 {
	if e.PredTaken {
		return e.Inst.Target
	}
	return e.Inst.PC + 4
}

// branchTaken evaluates the branch condition encoded by ALU op against
// the two source operands.
func (p *Pipeline) branchTaken(inst *Instruction, a, b int64) bool {
	switch inst.Branch {
	case BrNE:
		return a != b
	case BrLT:
		return a < b
	case BrGE:
		return a >= b
	case BrNZ:
		return a != 0
	default: // BrEQ
		return a == b
	}
}

// effectiveAddress computes Rs1 + Imm and validates alignment.
func (p *Pipeline) effectiveAddress(inst *Instruction, base int64) (int64, ExceptionCode) {
	addr := base + inst.Imm
	g := p.cfg.MemoryGranularity
	if g > 0 && addr%g != 0 {
		if inst.Kind == OpLoad {
			return addr, ExcLoadAddr
		}
		return addr, ExcStoreAddr
	}
	return addr, ExcNone
}

// executeLoad resolves a load's address and obtains its value. Store-to
// load forwarding honors the closest older aliasing store; under
// PolicyStall the issue gate already guarantees no unresolved older
// store, and under PolicyAssumeAlias speculative loads are remembered
// for replay validation.
func (p *Pipeline) executeLoad(tag Tag, ie *IssueEntry) bool {
	e := p.rob.get(tag)
	inst := e.Inst
	addr, exc := p.effectiveAddress(inst, ie.Src1V)
	lq := p.mem.entry(tag)
	if lq != nil {
		lq.Addr = addr
		lq.HasAddr = true
	}
	e.Addr = addr
	p.trace.record(Event{Cycle: p.cycle, Kind: EvMemAddr, Seq: inst.Seq, PC: inst.PC, Tag: tag})
	p.trace.record(Event{Cycle: p.cycle, Kind: EvComplete, Seq: inst.Seq, PC: inst.PC, Tag: tag})
	if exc != ExcNone {
		e.ExcCode = exc
		e.Ready = true
		e.Resolved = true
		if i := p.sched.get(tag); i != nil {
			i.Done = true
		}
		p.trace.record(Event{Cycle: p.cycle, Kind: EvException, Seq: inst.Seq, PC: inst.PC, Tag: tag,
			Detail: exc.String()})
		return false
	}

	// PolicyAssumeAlias: remember that this load executed before every
	// older store resolved its address; validate on later store resolves.
	if p.cfg.UnknownPolicy == PolicyAssumeAlias && !p.mem.olderStoresAddressKnown(tag) {
		p.specLoads[tag] = addr
	}

	value := p.mem.read(addr)
	if fwd := p.mem.olderAliasStore(tag, addr); fwd != nil && fwd.HasValue {
		value = fwd.Value
	}
	if lq != nil {
		lq.Value = value
		lq.Completed = true
	}
	p.publish(tag, value)
	return false
}

// executeStore resolves the store address/data. Stores never touch
// memory before commit; the entry remembers the value for forwarding.
// When an older store's address resolves here, speculative younger loads
// (PolicyAssumeAlias) are checked and replayed on an alias.
func (p *Pipeline) executeStore(tag Tag, ie *IssueEntry) bool {
	e := p.rob.get(tag)
	inst := e.Inst
	addr, exc := p.effectiveAddress(inst, ie.Src1V)
	sq := p.mem.entry(tag)
	if sq != nil {
		sq.Addr = addr
		sq.HasAddr = true
		sq.Value = ie.SrcStoreV
		sq.HasValue = true
	}
	e.Addr = addr
	e.Value = ie.SrcStoreV
	p.trace.record(Event{Cycle: p.cycle, Kind: EvMemAddr, Seq: inst.Seq, PC: inst.PC, Tag: tag})

	// Validate loads that speculated past this store.
	for lt, laddr := range p.specLoads {
		le := p.mem.entry(lt)
		if le == nil || le.Tag <= tag {
			continue
		}
		if laddr == addr {
			delete(p.specLoads, lt)
			p.replayLoad(lt)
			return true
		}
	}

	if exc != ExcNone {
		e.ExcCode = exc
		e.Ready = true
		e.Resolved = true
		if i := p.sched.get(tag); i != nil {
			i.Done = true
		}
		p.trace.record(Event{Cycle: p.cycle, Kind: EvException, Seq: inst.Seq, PC: inst.PC, Tag: tag,
			Detail: exc.String()})
		return false
	}

	e.Ready = true
	e.Resolved = true
	if i := p.sched.get(tag); i != nil {
		i.Done = true
	}
	p.trace.record(Event{Cycle: p.cycle, Kind: EvComplete, Seq: inst.Seq, PC: inst.PC, Tag: tag})
	return false
}

// replayLoad squashes a speculatively executed load (and its dependents)
// back to the load itself so it re-executes with the correct ordering.
func (p *Pipeline) replayLoad(tag Tag) {
	e := p.rob.get(tag)
	p.recover(tag, e.PC, ExcMemOrder, "memory-order violation replay", false)
}

// recover performs a precise squash of tag and every younger in-flight
// instruction, restores the rename state to the point immediately
// before tag renamed, and redirects fetch to restartPC. No squashed
// instruction has committed (exceptions are detected at execute but
// acted on at commit), so architectural registers/memory are untouched.
// dropBoundary selects whether the boundary instruction is discarded
// after the squash (exceptions/interrupts) versus retained to commit
// (resolved mispredicted branch).
func (p *Pipeline) recover(tag Tag, restartPC int64, code ExceptionCode, msg string, dropBoundary bool) {
	e := p.rob.get(tag)
	if e == nil {
		return
	}
	rec := &Recovery{Cycle: p.cycle, Seq: e.Seq, PC: restartPC, Code: code, Message: msg}
	p.recovery = rec
	p.trace.record(Event{
		Cycle: p.cycle, Kind: EvSquash, Seq: e.Seq, PC: e.PC, Tag: tag,
		Detail: fmt.Sprintf("recover to pc=%d reason=%s: %s", restartPC, code, msg),
	})

	// 1) Drop memory-queue entries tag and younger, restoring any
	//    speculative store data is unnecessary because stores only write
	//    memory at commit.
	droppedMem := p.mem.squash(tag)
	_ = droppedMem

	// 2) Remove ROB entries youngest-first and undo their renames LIFO.
	dropped := p.rob.squashAfter(tag, dropBoundary)
	if dropBoundary {
		// Exception/interrupt boundary: the faulting instruction is not
		// committed and is not re-fetched; drop its reservation as well.
		if b := p.rob.get(tag); b != nil {
			dropped = append(dropped, *b)
			p.rob.pop()
		}
	}
	for _, d := range dropped {
		if d.Rn.HasRd {
			// The physical register allocated by d never became
			// architectural; free it and invalidate its value.
			delete(p.prf, d.Rn.RdP)
			p.prfValid[d.Rn.RdP] = false
			p.rat.restore(d.Inst, d.Rn)
		}
	}

	// 3) Purge scheduler reservations and execution timers.
	live := make(map[Tag]bool)
	_, head := p.rob.head()
	if head != nil {
		ht := head.Tag
		for i := 0; i < p.rob.len(); i++ {
			live[ht+Tag(i)] = true
		}
	}
	for _, d := range dropped {
		live[d.Tag] = false
	}
	p.sched.squash(func() map[Tag]bool {
		dead := make(map[Tag]bool)
		for _, d := range dropped {
			dead[d.Tag] = true
		}
		return dead
	}())
	for _, d := range dropped {
		delete(p.completeAt, d.Tag)
		delete(p.specLoads, d.Tag)
	}
	// After a squash, surviving scheduler entries must re-derive operand
	// readiness from the restored PRF/RAT: any source whose producer was
	// squashed now reads the restored (older) physical value.

	// Any speculative-load bookkeeping belongs to instructions that have
	// now either been dropped or will re-execute; discard it wholesale.
	p.specLoads = make(map[Tag]int64)

	// 4) Redirect fetch; keep the predictor state as updated by the
	//    resolved branch (it has learned the correct outcome).
	p.fetchPC = restartPC
	p.stallFetch = false
	p.trace.record(Event{Cycle: p.cycle, Kind: EvRestart, Seq: e.Seq, PC: restartPC, Tag: tag,
		Detail: code.String()})
}

// commit pops up to CommitWidth ready head entries in program order and
// makes their effects architectural. Exceptions and pending interrupts
// are handled precisely at this boundary: strictly older instructions
// have already committed, while the faulting/interrupted instruction and
// everything younger remains uncommitted and is squashed atomically.
func (p *Pipeline) commit() bool {
	for i := 0; i < p.cfg.CommitWidth; i++ {
		tag, e := p.rob.head()
		if e == nil {
			return false
		}
		// Precise asynchronous interrupt: commit every instruction that
		// has already completed, then take the interrupt strictly before
		// the next (not-yet-committed) instruction. If the head is not
		// ready, wait for it rather than taking the interrupt in the
		// middle of an instruction's lifetime.
		if len(p.interrupts) > 0 {
			if !e.Ready {
				return false
			}
			// Commit the completed head first; the interrupt boundary
			// becomes the following instruction.
			p.commitOne(e)
			_, next := p.rob.head()
			req := p.interrupts[0]
			p.interrupts = p.interrupts[1:]
			if next == nil {
				// Program fully retired before the interrupt point.
				p.halted = true
				p.recovery = &Recovery{Cycle: p.cycle, Code: req.code, Message: req.msg, PC: p.fetchPC}
				return true
			}
			p.recover(next.Tag, next.PC, req.code, req.msg, true)
			p.trace.record(Event{Cycle: p.cycle, Kind: EvInterrupt, Seq: next.Seq, PC: next.PC, Tag: next.Tag,
				Detail: req.msg})
			p.halted = true
			return true
		}
		if !e.Ready {
			return false
		}
		if e.ExcCode != ExcNone {
			// Precise exception: earlier commits stand; this instruction
			// and all younger instructions become invisible atomically.
			p.recover(tag, e.PC, e.ExcCode,
				fmt.Sprintf("exception %s at pc=%d seq=%d", e.ExcCode, e.PC, e.Seq), true)
			p.trace.record(Event{Cycle: p.cycle, Kind: EvException, Seq: e.Seq, PC: e.PC, Tag: tag,
				Detail: e.ExcCode.String()})
			p.halted = true
			return true
		}
		p.commitOne(e)
		_ = tag
	}
	return false
}

// commitOne applies one completed instruction to architectural state,
// records the commit trace and releases its resources.
func (p *Pipeline) commitOne(e *ROBEntry) {
	inst := e.Inst
	switch inst.Kind {
	case OpStore:
		old := p.mem.write(e.Addr, e.Value)
		_ = old
	default:
		if e.Rn.HasRd {
			p.archRegs[inst.Rd] = e.Value
			// The previous committed physical mapping is now dead.
			p.rat.freeReg(e.Rn.OldRdP)
			delete(p.prf, e.Rn.OldRdP)
		}
	}
	p.trace.commit(CommitRecord{Cycle: p.cycle, Seq: inst.Seq, PC: inst.PC,
		Rd: inst.Rd, Value: e.Value, Addr: e.Addr})
	p.trace.record(Event{Cycle: p.cycle, Kind: EvCommit, Seq: inst.Seq, PC: inst.PC, Tag: e.Tag})
	if lq := p.mem.entry(e.Tag); lq != nil {
		p.mem.remove(e.Tag)
	}
	p.sched.remove(e.Tag)
	delete(p.completeAt, e.Tag)
	delete(p.specLoads, e.Tag)
	p.rob.pop()
}

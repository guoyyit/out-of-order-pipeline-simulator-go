package pipeline

// RenameTable is the register alias table (RAT) together with the free
// list of physical registers. Logical register r is initially mapped to
// physical register r; renaming an instruction that writes r allocates a
// fresh physical register and records the previous mapping so it can be
// restored verbatim on a squash.
type RenameTable struct {
	numLogical int
	numExtra   int
	rat        []PReg
	free       []PReg       // stack of free physical registers
	producer   map[PReg]Tag // in-flight writer of each physical register
}

// renameResult captures the renaming of one instruction.
type renameResult struct {
	Tag    Tag
	Rs1P   PReg
	Rs2P   PReg
	RdP    PReg
	OldRdP PReg // previous mapping for Rd (restored on squash)
	HasRd  bool
}

func newRenameTable(cfg Config) *RenameTable {
	n := cfg.NumLogicalRegs
	if n <= 0 {
		n = 32
	}
	rob := cfg.ROBSize
	if rob <= 0 {
		rob = 16
	}
	r := &RenameTable{
		numLogical: n,
		numExtra:   rob,
		rat:        make([]PReg, n),
		free:       make([]PReg, 0, rob),
		producer:   make(map[PReg]Tag),
	}
	for i := 0; i < n; i++ {
		r.rat[i] = PReg(i)
	}
	// One extra physical register per ROB slot is sufficient because at
	// most one in-flight (uncommitted) write per slot can be live.
	for p := n; p < n+rob; p++ {
		r.free = append(r.free, PReg(p))
	}
	return r
}

func (r *RenameTable) lookup(reg int) PReg {
	if reg < 0 || reg >= r.numLogical {
		return PReg(0)
	}
	return r.rat[reg]
}

// canRename reports whether at least n destination mappings are available.
func (r *RenameTable) canRename(n int) bool { return len(r.free) >= n }

func (r *RenameTable) rename(inst *Instruction, tag Tag) renameResult {
	res := renameResult{Tag: tag, HasRd: inst.Rd >= 0 && inst.Rd < r.numLogical}
	if inst.Rs1 >= 0 && inst.Rs1 < r.numLogical {
		res.Rs1P = r.rat[inst.Rs1]
	}
	if inst.Rs2 >= 0 && inst.Rs2 < r.numLogical {
		res.Rs2P = r.rat[inst.Rs2]
	}
	if res.HasRd {
		res.OldRdP = r.rat[inst.Rd]
		res.RdP = r.free[len(r.free)-1]
		r.free = r.free[:len(r.free)-1]
		r.rat[inst.Rd] = res.RdP
		r.producer[res.RdP] = tag
	}
	return res
}

// restore undoes one rename in program-order reverse: it reinstalls the
// previous mapping and returns the allocated register to the free list.
func (r *RenameTable) restore(inst *Instruction, res renameResult) {
	if !res.HasRd {
		return
	}
	if inst.Rd >= 0 && inst.Rd < r.numLogical {
		r.rat[inst.Rd] = res.OldRdP
	}
	delete(r.producer, res.RdP)
	r.free = append(r.free, res.RdP)
}

// producerOf returns the in-flight tag writing p, or InvalidTag if the
// physical register already holds a committed/ready value.
func (r *RenameTable) producerOf(p PReg) Tag {
	if t, ok := r.producer[p]; ok {
		return t
	}
	return InvalidTag
}

// commitRelease marks a physical register's producer gone (the value is
// now architectural and the register is eligible for reuse).
func (r *RenameTable) commitRelease(p PReg) { delete(r.producer, p) }

func (r *RenameTable) freeReg(p PReg) { r.free = append(r.free, p) }

func (r *RenameTable) reset() {
	r.producer = make(map[PReg]Tag)
	for i := range r.rat {
		r.rat[i] = PReg(i)
	}
	n := r.numLogical
	r.free = make([]PReg, 0, r.numExtra)
	// Physical registers 0..n-1 are the committed architectural ones;
	// the extra rename registers are all free again.
	for p := n; p < n+r.numExtra; p++ {
		r.free = append(r.free, PReg(p))
	}
}

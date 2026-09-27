package pipeline

// FaultInjector decides, deterministically from the seed, at which cycle a
// fault is injected. A fault discards all uncommitted state and rolls the
// machine back to the last committed checkpoint; the run then continues
// and must produce exactly the same committed state as a fault-free run.
type FaultInjector struct {
	// AtCycle is the cycle at which the fault fires. 0 disables injection.
	AtCycle uint64
	fired   bool
}

// armed reports whether a fault fires at the given cycle.
func (f *FaultInjector) armed(cycle uint64) bool {
	if f == nil || f.fired || f.AtCycle == 0 {
		return false
	}
	return cycle >= f.AtCycle
}

func (f *FaultInjector) markFired() { f.fired = true }

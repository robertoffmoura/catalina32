package cpu

// Exported helpers for ahead-of-time compiled guest code (see cmd/c32aot). They expose the
// interpreter's own semantics so compiled and interpreted execution stay identical.

// EventStep is the step count at which the run loop must next check limits, preemption or the
// profiler; compiled code never runs past it. Compiled code is entered from Step, after RunUntil
// handled any event due at the current step, so at least one instruction may always run (the
// profiler's next sample is derived from Steps: without this, a function entered at a sample point
// would return without progress and be re-entered forever).
func (c *CPU) EventStep() uint64 { return max(c.nextEventStep(), c.Steps+1) }

func (c *CPU) AluOp32(op int, a, b uint32) (uint32, bool)     { return c.aluOp32(op, a, b) }
func (c *CPU) AluOp8(op int, a, b uint8) (uint8, bool)        { return c.aluOp8(op, a, b) }
func (c *CPU) ShiftOp32(op int, v uint32, count uint8) uint32 { return c.shiftOp32(op, v, count) }
func (c *CPU) XmmF32(r int) float32                           { return c.getXMMF32(r, 0) }
func (c *CPU) SetXmmF32(r int, v float32)                     { c.setXMMF32(r, 0, v) }
func (c *CPU) XmmF64(r int) float64                           { return c.getXMMF64(r, 0) }
func (c *CPU) SetXmmF64(r int, v float64)                     { c.setXMMF64(r, 0, v) }
func (c *CPU) CompareEFlags(a, b float64)                     { c.fpuCompareEFlags(a, b) }

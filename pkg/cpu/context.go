package cpu

// Context is a full snapshot of one guest thread's architectural state.
// It is used by the cooperative/preemptive green-thread scheduler in pkg/win32.
type Context struct {
	Regs      [8]uint32
	EIP       uint32
	Flags     uint32
	FSBase    uint32
	GSBase    uint32
	FPUSt     [8]float64
	FPUTop    int
	FPUCW     uint16
	FPUSW     uint16
	XMM       [8][16]byte
	MM        [8]uint64
	MXCSR     uint32
	CallStack []CallFrame
}

// SaveContext snapshots the current thread state.
func (c *CPU) SaveContext() Context {
	return Context{
		Regs: c.Regs, EIP: c.EIP, Flags: c.Flags,
		FSBase: c.FSBase, GSBase: c.GSBase,
		FPUSt: c.FPUSt, FPUTop: c.FPUTop, FPUCW: c.FPUCW, FPUSW: c.FPUSW,
		XMM: c.XMM, MM: c.MM, MXCSR: c.MXCSR,
		CallStack: c.CallStack,
	}
}

// LoadContext makes ctx the running thread state.
func (c *CPU) LoadContext(ctx *Context) {
	c.Regs = ctx.Regs
	c.EIP = ctx.EIP
	c.Flags = ctx.Flags
	c.FSBase = ctx.FSBase
	c.GSBase = ctx.GSBase
	c.FPUSt = ctx.FPUSt
	c.FPUTop = ctx.FPUTop
	c.FPUCW = ctx.FPUCW
	c.FPUSW = ctx.FPUSW
	c.XMM = ctx.XMM
	c.MM = ctx.MM
	c.MXCSR = ctx.MXCSR
	c.CallStack = ctx.CallStack
}

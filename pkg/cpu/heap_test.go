package cpu

import "testing"

func TestHeapReusesLargeBlocks(t *testing.T) {
	c := NewCPU()
	a := c.Malloc(1 << 20)
	b := c.Malloc(1 << 20)
	top := c.HeapPtr
	c.Free(a)
	c.Free(b) // coalesces with a into one 2 MB extent
	if got := c.Malloc(2 << 20); got != a {
		t.Fatalf("2 MB request got %#x, want the coalesced extent at %#x", got, a)
	}
	if c.HeapPtr != top {
		t.Fatalf("bump pointer moved: %#x -> %#x", top, c.HeapPtr)
	}
	c.Free(a)
	small := c.Malloc(256 << 10) // carved from the front of the extent
	rest := c.Malloc(1<<21 - 256<<10)
	if small != a || rest != a+256<<10 {
		t.Fatalf("split: %#x %#x (extent at %#x)", small, rest, a)
	}
	if c.HeapInUse() != 2<<20 {
		t.Fatalf("in use %d", c.HeapInUse())
	}
}

func TestHeapExhaustionHalts(t *testing.T) {
	c := NewCPU()
	if p := c.Malloc(HeapEndAddr); p != 0 || !c.Halted {
		t.Fatalf("oversized malloc: %#x, halted %v", p, c.Halted)
	}
	c = NewCPU()
	if p := c.Calloc(1<<20, 1<<20); p != 0 || !c.Halted {
		t.Fatalf("overflowing calloc: %#x, halted %v", p, c.Halted)
	}
}

// A REP string instruction that runs out of guest memory stops at the fault instead of running
// out its count (4 billion iterations would take about 20 minutes).
func TestRepStopsAfterFault(t *testing.T) {
	c := NewCPU()
	const code = 0x1000
	copy(c.Mem[code:], []byte{0xF3, 0xAB}) // rep stosd
	c.EIP = code
	c.Regs[RegEDI] = ArenaSize - 8 // two stores fit, the third faults (too long for the bulk path)
	c.Regs[RegECX] = 0xFFFFFFFF
	c.Step()
	if !c.Halted {
		t.Fatal("not halted")
	}
	if c.Regs[RegECX] < 0xFFFFFFF0 {
		t.Fatalf("ran on after the fault: ECX = %#x", c.Regs[RegECX])
	}
}

func TestSSEFormsAndFPUEnvironment(t *testing.T) {
	run := func(code ...byte) *CPU {
		c := NewCPU()
		copy(c.Mem[0x1000:], code)
		c.EIP = 0x1000
		c.setXMMF64(1, 0, 1.5)
		c.setXMMF64(1, 1, 2.75)
		c.setXMMF64(2, 0, 1.75)
		c.setXMMF64(2, 1, 10)
		c.Step()
		return c
	}
	if c := run(0x66, 0x0F, 0x7C, 0xCA); c.getXMMF64(1, 0) != 4.25 || c.getXMMF64(1, 1) != 11.75 { // haddpd xmm1, xmm2
		t.Errorf("HADDPD: %v %v", c.getXMMF64(1, 0), c.getXMMF64(1, 1))
	}
	if c := run(0x66, 0x0F, 0xE6, 0xCA); c.getXMMU32(1, 0) != 1 || int32(c.getXMMU32(1, 1)) != 10 { // cvttpd2dq: 1.75 -> 1
		t.Errorf("CVTTPD2DQ: %d %d", int32(c.getXMMU32(1, 0)), int32(c.getXMMU32(1, 1)))
	}
	if c := run(0xF2, 0x0F, 0xE6, 0xCA); c.getXMMU32(1, 0) != 2 || int32(c.getXMMU32(1, 1)) != 10 { // cvtpd2dq: 1.75 -> 2
		t.Errorf("CVTPD2DQ: %d %d", int32(c.getXMMU32(1, 0)), int32(c.getXMMU32(1, 1)))
	}
	for _, code := range [][]byte{{0x0F, 0xFE, 0xCA}, {0x0F, 0x7E, 0xC8}, {0x0F, 0xE6, 0xCA}, {0x0F, 0x7C, 0xCA}} { // MMX paddd, movd; unprefixed E6/7C
		if c := run(code...); !c.Halted {
			t.Errorf("% x: not rejected", code)
		}
	}

	// FNSTENV / FLDENV round trip, FNSAVE / FRSTOR of the register stack.
	c := NewCPU()
	c.FPUCW, c.FPUSW = 0x027F, 0x0021
	c.FPUPush(3.25)
	c.FPUPush(-7)
	c.fnstenv(0x2000)
	if c.FPUCW != 0x027F|0x3F {
		t.Errorf("FNSTENV did not mask exceptions: %#x", c.FPUCW)
	}
	c.FPUCW, c.FPUSW, c.FPUTop = 0, 0, 3
	c.fldenv(0x2000)
	if c.FPUCW != 0x027F || c.FPUSW != 0x0021 || c.FPUTop != 6 {
		t.Errorf("FLDENV: cw %#x sw %#x top %d", c.FPUCW, c.FPUSW, c.FPUTop)
	}
	copy(c.Mem[0x3000:], []byte{0xDD, 0x33, 0xDD, 0x23}) // fnsave [ebx]; frstor [ebx]
	c.Regs[RegEBX] = 0x4000
	c.EIP = 0x3000
	c.Step()
	if c.FPUCW != 0x037F || c.FPUTop != 0 {
		t.Errorf("FNSAVE did not reinitialise: cw %#x top %d", c.FPUCW, c.FPUTop)
	}
	c.Step()
	if c.FPUGet(0) != -7 || c.FPUGet(1) != 3.25 || c.FPUCW != 0x027F || c.FPUTop != 6 {
		t.Errorf("FRSTOR: st0 %v st1 %v cw %#x top %d", c.FPUGet(0), c.FPUGet(1), c.FPUCW, c.FPUTop)
	}
}

// A host-initiated guest call (qsort comparator, timer) must not pop its caller's call frame.
func TestHostCallKeepsCallStack(t *testing.T) {
	c := NewCPU()
	c.TraceCalls = true
	c.VisitedFuncs = map[uint32]uint64{}
	c.Mem[0x1000] = 0xC3 // ret
	c.Regs[RegESP] = 0x10000
	c.CallStack = []CallFrame{{CallerEIP: 0x500, TargetEIP: 0x600}}
	if _, err := c.CallGuest(0x1000); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallGuestNested(0x1000); err != nil {
		t.Fatal(err)
	}
	if len(c.CallStack) != 1 || c.CallStack[0].TargetEIP != 0x600 {
		t.Fatalf("call stack after host calls: %+v", c.CallStack)
	}
}

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

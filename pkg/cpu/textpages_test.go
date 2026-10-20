package cpu

import "testing"

// Stores to data pages inside the code range keep the decoded-instruction cache; stores to code
// pages flush it.
func TestTextPages(t *testing.T) {
	c := NewCPU()
	c.SetCodeRange(0x100000, 0x200000)
	c.MarkTextPages(0x100000, 0x101000)
	c.dcacheUsed = true
	before := c.CodeCacheFlushes
	c.Write32(0x150000, 1) // data
	if c.CodeCacheFlushes != before {
		t.Fatalf("a data store flushed the cache")
	}
	c.Write32(0x100ffe, 1) // straddles the last code page
	if c.CodeCacheFlushes == before {
		t.Fatalf("a code store did not flush the cache")
	}
}

// PUSH/POP of segment registers (old MSVC code saves DS and ES) keep the stack balanced.
func TestSegmentPushPop(t *testing.T) {
	c := NewCPU()
	code := []byte{0x1E, 0x06, 0x0E, 0x16, 0x17, 0x58, 0x07, 0x1F} // push ds, es, cs, ss; pop ss; pop eax; pop es; pop ds
	copy(c.Mem[0x1000:], code)
	c.EIP = 0x1000
	esp := c.Regs[RegESP]
	for range code {
		c.Step()
	}
	if c.Halted || c.Regs[RegESP] != esp || c.Regs[RegEAX] != 0x17 {
		t.Fatalf("halted %v (%s), esp %#x (want %#x), eax %#x (want the CS selector 0x17)", c.Halted, c.HaltReason, c.Regs[RegESP], esp, c.Regs[RegEAX])
	}
}

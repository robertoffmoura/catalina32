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

package cpu

import (
	"math/rand"
	"testing"
)

// The decoded-instruction cache must do exactly what the full interpreter does: each form runs
// both ways from the same random state, and registers, flags and memory must match.
func TestDCacheMatchesInterpreter(t *testing.T) {
	forms := [][]byte{
		{0x0F, 0xAF, 0xC3},       // imul eax, ebx
		{0x0F, 0xAF, 0x45, 0x10}, // imul eax, [ebp+0x10]
		{0xF7, 0xD1},             // not ecx
		{0xF7, 0xDB},             // neg ebx
		{0xF7, 0x5D, 0x10},       // neg dword [ebp+0x10]
		{0xF7, 0x55, 0x10},       // not dword [ebp+0x10]
		{0x0F, 0x94, 0xC0},       // sete al
		{0x0F, 0x95, 0xC7},       // setne bh
		{0x0F, 0x9C, 0x45, 0x10}, // setl [ebp+0x10]
		{0x0F, 0x97, 0xC1},       // seta cl
	}
	rng := rand.New(rand.NewSource(1))
	const code, data = 0x1000, 0x8000
	for _, f := range forms {
		for trial := 0; trial < 200; trial++ {
			var regs [8]uint32
			for i := range regs {
				regs[i] = rng.Uint32()
				if rng.Intn(4) == 0 {
					regs[i] = uint32(rng.Intn(5)) - 2 // small values: zero, signs, overflow edges
				}
			}
			regs[RegEBP] = data
			regs[RegESP] = 0x9000
			flags := uint32(rng.Intn(0x1000)) & (FlagCF | FlagZF | FlagSF | FlagOF | FlagPF | FlagAF)
			mem := rng.Uint32()
			run := func(cached bool) (*CPU, uint32) {
				c := NewCPU()
				copy(c.Mem[code:], f)
				c.Mem[code+uint32(len(f))] = 0x90
				c.Regs, c.Flags, c.EIP = regs, flags|2, code
				c.Write32(data+0x10, mem)
				if cached {
					c.SetCodeRange(code, code+0x100)
				}
				c.Step()
				return c, c.Read32(data + 0x10)
			}
			a, am := run(false)
			b, bm := run(true)
			if k := b.dcache[code&dcacheMask].kind; k == dSlow {
				t.Fatalf("% x isn't cached", f)
			}
			if a.Regs != b.Regs || a.Flags != b.Flags || am != bm || a.EIP != b.EIP || a.Halted || b.Halted {
				t.Fatalf("% x from regs %08x flags %#x mem %#x:\n interpreter regs %08x flags %#x mem %#x eip %#x\n cache       regs %08x flags %#x mem %#x eip %#x",
					f, regs, flags, mem, a.Regs, a.Flags, am, a.EIP, b.Regs, b.Flags, bm, b.EIP)
			}
		}
	}
}

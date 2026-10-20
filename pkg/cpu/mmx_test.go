package cpu

import (
	"math"
	"testing"
)

// Each MMX form against hand-computed results (MM0 = a, MM1 = b, result in MM0).
func TestMMX(t *testing.T) {
	w := func(x ...uint16) uint64 { // four words, lowest first
		return uint64(x[0]) | uint64(x[1])<<16 | uint64(x[2])<<32 | uint64(x[3])<<48
	}
	d := func(lo, hi uint32) uint64 { return uint64(lo) | uint64(hi)<<32 }
	cases := []struct {
		name string
		code []byte
		a, b uint64
		want uint64
	}{
		{"packssdw", []byte{0x0F, 0x6B, 0xC1}, d(70000, 0xFFFFFFFF /* -1 */), d(5, 0x80000000), w(0x7FFF, 0xFFFF, 5, 0x8000)},
		{"packsswb", []byte{0x0F, 0x63, 0xC1}, w(300, 0xFF00 /* -256 */, 5, 0xFFFF), w(1, 2, 3, 4), 0x04030201_FF05807F},
		{"packuswb", []byte{0x0F, 0x67, 0xC1}, w(300, 0xFFFF, 5, 128), w(0, 0, 0, 0), 0x00000000_8005_00FF},
		{"psrad imm", []byte{0x0F, 0x72, 0xE0, 4}, d(0x80000010, 0x100), 0, d(0xF8000001, 0x10)},
		{"psraw imm", []byte{0x0F, 0x71, 0xE0, 1}, w(0x8000, 4, 0xFFFF, 2), 0, w(0xC000, 2, 0xFFFF, 1)},
		{"psrlq imm", []byte{0x0F, 0x73, 0xD0, 8}, 0x1122334455667788, 0, 0x0011223344556677},
		{"psllw reg", []byte{0x0F, 0xF1, 0xC1}, w(1, 0x8001, 3, 4), 3, w(8, 8, 24, 32)},
		{"pxor", []byte{0x0F, 0xEF, 0xC1}, 0xF0F0F0F0F0F0F0F0, 0xFFFF0000FFFF0000, 0x0F0FF0F00F0FF0F0},
		{"punpcklwd", []byte{0x0F, 0x61, 0xC1}, w(1, 2, 3, 4), w(5, 6, 7, 8), w(1, 5, 2, 6)},
		{"punpckhwd", []byte{0x0F, 0x69, 0xC1}, w(1, 2, 3, 4), w(5, 6, 7, 8), w(3, 7, 4, 8)},
		{"paddw", []byte{0x0F, 0xFD, 0xC1}, w(0xFFFF, 2, 3, 4), w(2, 3, 4, 5), w(1, 5, 7, 9)},
		{"paddsw", []byte{0x0F, 0xED, 0xC1}, w(0x7FFF, 0x8000, 3, 4), w(1, 0xFFFF, 4, 5), w(0x7FFF, 0x8000, 7, 9)},
		{"paddusb", []byte{0x0F, 0xDC, 0xC1}, 0x00000000000000F0, 0x0000000000000020, 0x00000000000000FF},
		{"paddd", []byte{0x0F, 0xFE, 0xC1}, d(0xFFFFFFFF, 1), d(2, 3), d(1, 4)},
		{"psubw", []byte{0x0F, 0xF9, 0xC1}, w(1, 2, 3, 4), w(2, 2, 2, 2), w(0xFFFF, 0, 1, 2)},
		{"pmullw", []byte{0x0F, 0xD5, 0xC1}, w(300, 0xFFFE, 3, 4), w(300, 3, 5, 6), w(uint16(90000&0xFFFF), 0xFFFA, 15, 24)},
		{"pmulhw", []byte{0x0F, 0xE5, 0xC1}, w(300, 0xFFFE, 0x4000, 4), w(300, 3, 0x4000, 6), w(1, 0xFFFF, 0x1000, 0)},
		{"pmaddwd", []byte{0x0F, 0xF5, 0xC1}, w(1, 2, 3, 0xFFFF), w(4, 5, 6, 7), d(14, 11)},
		{"pcmpgtw", []byte{0x0F, 0x65, 0xC1}, w(5, 0xFFFF, 3, 3), w(4, 0, 3, 2), w(0xFFFF, 0, 0, 0xFFFF)},
		{"pcmpeqb", []byte{0x0F, 0x74, 0xC1}, 0x0102030405060708, 0x0100030005000700, 0xFF00FF00FF00FF00},
		{"pand/pandn", []byte{0x0F, 0xDF, 0xC1}, 0xFF00FF00FF00FF00, 0xFFFFFFFF00000000, 0x00FF00FF00000000},
		{"movq mm,mm", []byte{0x0F, 0x6F, 0xC1}, 1, 0x1234567890ABCDEF, 0x1234567890ABCDEF},
		{"pshufw", []byte{0x0F, 0x70, 0xC1, 0x1B}, 0, w(1, 2, 3, 4), w(4, 3, 2, 1)},
	}
	for _, tc := range cases {
		c := NewCPU()
		copy(c.Mem[0x1000:], tc.code)
		c.EIP = 0x1000
		c.MM[0], c.MM[1] = tc.a, tc.b
		c.Step()
		if c.Halted || c.MM[0] != tc.want {
			t.Errorf("%s: %#016x, want %#016x (%s)", tc.name, c.MM[0], tc.want, c.HaltReason)
		}
	}
}

// MOVD/MOVQ with registers and memory, CVTPS2PI/CVTTPS2PI, EMMS.
func TestMMXMovesAndConversions(t *testing.T) {
	c := NewCPU()
	code := []byte{
		0x0F, 0x6E, 0xC3, // movd mm0, ebx
		0x0F, 0x7F, 0x05, 0x00, 0x20, 0x00, 0x00, // movq [0x2000], mm0
		0x0F, 0x6F, 0x0D, 0x08, 0x20, 0x00, 0x00, // movq mm1, [0x2008]
		0x0F, 0x7E, 0xC9, // movd ecx, mm1
		0x0F, 0x2D, 0xD0, // cvtps2pi mm2, xmm0
		0x0F, 0x2C, 0xD8, // cvttps2pi mm3, xmm0
		0x0F, 0x77, // emms
	}
	copy(c.Mem[0x1000:], code)
	c.EIP = 0x1000
	c.Regs[RegEBX] = 0xCAFEBABE
	c.Write64(0x2008, 0x1111222233334444)
	c.setXMMF32(0, 0, 2.5)
	c.setXMMF32(0, 1, -3.7)
	ebx, eax := c.Regs[RegEBX], c.Regs[RegEAX]
	for c.EIP < 0x1000+uint32(len(code)) && !c.Halted {
		c.Step()
	}
	if c.Halted {
		t.Fatal(c.HaltReason)
	}
	if c.Read64(0x2000) != 0xCAFEBABE || c.Regs[RegECX] != 0x33334444 || c.MM[1] != 0x1111222233334444 {
		t.Errorf("moves: mem %#x ecx %#x mm1 %#x", c.Read64(0x2000), c.Regs[RegECX], c.MM[1])
	}
	if int32(c.MM[2]) != 2 || int32(c.MM[2]>>32) != -4 { // round to even: 2.5 -> 2, -3.7 -> -4
		t.Errorf("cvtps2pi: %d %d", int32(c.MM[2]), int32(c.MM[2]>>32))
	}
	if int32(c.MM[3]) != 2 || int32(c.MM[3]>>32) != -3 {
		t.Errorf("cvttps2pi: %d %d", int32(c.MM[3]), int32(c.MM[3]>>32))
	}
	if c.Regs[RegEBX] != ebx || c.Regs[RegEAX] != eax { // (cvtps2pi used to run as cvtss2si: GPRs)
		t.Errorf("general registers changed")
	}
	_ = math.Pi
}

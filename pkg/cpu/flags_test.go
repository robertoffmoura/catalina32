package cpu

import (
	"math/rand"
	"testing"
)

// Reference implementations: the original per-flag SetFlag versions.
func refAddSub(c *CPU, a, b, res uint32, sign uint32, sub bool) {
	if sub {
		c.SetFlag(FlagCF, a < b)
	} else {
		c.SetFlag(FlagCF, res < a)
	}
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&sign) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
	c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
	if sub {
		c.SetFlag(FlagOF, (((a^b)&(a^res))&sign) != 0)
	} else {
		c.SetFlag(FlagOF, ((^(a^b)&(a^res))&sign) != 0)
	}
}

func refLogic(c *CPU, res uint32, sign uint32) {
	c.SetFlag(FlagCF|FlagOF|FlagAF, false)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&sign) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
}

func TestFlagHelpersMatchReference(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	edge := []uint32{0, 1, 0x7f, 0x80, 0xff, 0x7fff, 0x8000, 0xffff, 0x7fffffff, 0x80000000, 0xffffffff}
	pick := func() uint32 {
		if r.Intn(3) == 0 {
			return edge[r.Intn(len(edge))]
		}
		return r.Uint32()
	}
	for i := 0; i < 200000; i++ {
		a, b := pick(), pick()
		init := r.Uint32() & 0xfff
		var got, want CPU
		check := func(name string) {
			if got.Flags != want.Flags {
				t.Fatalf("%s a=%#x b=%#x init=%#x: got %#x want %#x", name, a, b, init, got.Flags, want.Flags)
			}
		}
		got.Flags, want.Flags = init, init
		got.UpdateFlagsAdd32(a, b, a+b)
		refAddSub(&want, a, b, a+b, 0x80000000, false)
		check("add32")
		got.Flags, want.Flags = init, init
		got.UpdateFlagsSub32(a, b, a-b)
		refAddSub(&want, a, b, a-b, 0x80000000, true)
		check("sub32")
		a16, b16 := uint16(a), uint16(b)
		got.Flags, want.Flags = init, init
		got.UpdateFlagsAdd16(a16, b16, a16+b16)
		refAddSub(&want, uint32(a16), uint32(b16), uint32(a16+b16), 0x8000, false)
		check("add16")
		got.Flags, want.Flags = init, init
		got.UpdateFlagsSub16(a16, b16, a16-b16)
		refAddSub(&want, uint32(a16), uint32(b16), uint32(a16-b16), 0x8000, true)
		check("sub16")
		a8, b8 := uint8(a), uint8(b)
		got.Flags, want.Flags = init, init
		got.UpdateFlagsAdd8(a8, b8, a8+b8)
		refAddSub(&want, uint32(a8), uint32(b8), uint32(a8+b8), 0x80, false)
		check("add8")
		got.Flags, want.Flags = init, init
		got.UpdateFlagsSub8(a8, b8, a8-b8)
		refAddSub(&want, uint32(a8), uint32(b8), uint32(a8-b8), 0x80, true)
		check("sub8")
		got.Flags, want.Flags = init, init
		got.UpdateFlagsLogic32(a & b)
		refLogic(&want, a&b, 0x80000000)
		check("logic32")
		got.Flags, want.Flags = init, init
		got.UpdateFlagsLogic16(a16 | b16)
		refLogic(&want, uint32(a16|b16), 0x8000)
		check("logic16")
		got.Flags, want.Flags = init, init
		got.UpdateFlagsLogic8(a8 ^ b8)
		refLogic(&want, uint32(a8^b8), 0x80)
		check("logic8")
	}
}

package cpu

import (
	"encoding/binary"
	"fmt"
	"math"
)

func (c *CPU) getXMMF32(reg int, lane int) float32 {
	bits := binary.LittleEndian.Uint32(c.XMM[reg][lane*4 : lane*4+4])
	return math.Float32frombits(bits)
}

func (c *CPU) setXMMF32(reg int, lane int, val float32) {
	binary.LittleEndian.PutUint32(c.XMM[reg][lane*4:lane*4+4], math.Float32bits(val))
}

func (c *CPU) getXMMF64(reg int, lane int) float64 {
	bits := binary.LittleEndian.Uint64(c.XMM[reg][lane*8 : lane*8+8])
	return math.Float64frombits(bits)
}

func (c *CPU) setXMMF64(reg int, lane int, val float64) {
	binary.LittleEndian.PutUint64(c.XMM[reg][lane*8:lane*8+8], math.Float64bits(val))
}

func (c *CPU) getXMMU32(reg int, lane int) uint32 {
	return binary.LittleEndian.Uint32(c.XMM[reg][lane*4 : lane*4+4])
}

func (c *CPU) setXMMU32(reg int, lane int, val uint32) {
	binary.LittleEndian.PutUint32(c.XMM[reg][lane*4:lane*4+4], val)
}

func (c *CPU) getXMMU64(reg int, lane int) uint64 {
	return binary.LittleEndian.Uint64(c.XMM[reg][lane*8 : lane*8+8])
}

func (c *CPU) setXMMU64(reg int, lane int, val uint64) {
	binary.LittleEndian.PutUint64(c.XMM[reg][lane*8:lane*8+8], val)
}

func (c *CPU) fpuCompare(a, b float64) {
	// Clear C0 (bit 8), C2 (bit 10), C3 (bit 14)
	c.FPUSW &^= (1 << 8) | (1 << 10) | (1 << 14)
	if math.IsNaN(a) || math.IsNaN(b) {
		c.FPUSW |= (1 << 8) | (1 << 10) | (1 << 14)
	} else if a < b {
		c.FPUSW |= (1 << 8)
	} else if a == b {
		c.FPUSW |= (1 << 14)
	}
}

// FPU status word condition codes.
const (
	fpuC0 = 1 << 8
	fpuC1 = 1 << 9
	fpuC2 = 1 << 10
	fpuC3 = 1 << 14
)

// fxam classifies v into C3/C2/C0 (zero, normal, infinity, NaN) with its sign in C1. Registers
// hold float64 values, so nothing is empty, unsupported or denormal in extended precision.
func (c *CPU) fxam(v float64) {
	sw := c.FPUSW &^ (fpuC0 | fpuC1 | fpuC2 | fpuC3)
	if math.Signbit(v) {
		sw |= fpuC1
	}
	switch {
	case math.IsNaN(v):
		sw |= fpuC0
	case math.IsInf(v, 0):
		sw |= fpuC2 | fpuC0
	case v == 0:
		sw |= fpuC3
	default:
		sw |= fpuC2
	}
	c.FPUSW = sw
}

// fprem sets ST(0) to the remainder of ST(0)/ST(1) (FPREM: truncated quotient, FPREM1: rounded)
// in one step (C2 clear: complete) and reports the low three quotient bits in C0, C3 and C1,
// which trigonometric argument reduction uses to pick the octant.
func (c *CPU) fprem(rem func(float64, float64) float64, quot func(float64) float64) {
	a, b := c.FPUGet(0), c.FPUGet(1)
	q := uint64(0)
	if v := math.Abs(quot(a / b)); v < 1<<63 {
		q = uint64(v)
	}
	sw := c.FPUSW &^ (fpuC0 | fpuC1 | fpuC2 | fpuC3)
	if q&4 != 0 {
		sw |= fpuC0
	}
	if q&2 != 0 {
		sw |= fpuC3
	}
	if q&1 != 0 {
		sw |= fpuC1
	}
	c.FPUSW = sw
	c.FPUSet(0, rem(a, b))
}

// fnstenv stores the 28-byte protected-mode FPU environment (control, status and tag words, then
// zeroed instruction/operand pointers) and masks all exceptions, as FNSTENV does. Register tags
// are not tracked: all registers are reported valid.
func (c *CPU) fnstenv(addr uint32) {
	b := c.WBytes(addr, 28)
	if b == nil {
		return
	}
	clear(b)
	binary.LittleEndian.PutUint16(b[0:], c.FPUCW)
	binary.LittleEndian.PutUint16(b[4:], c.FPUSW&0xC7FF|uint16(c.FPUTop&7)<<11)
	c.FPUCW |= 0x3F
}

// fldenv loads the control and status words (and the stack top) from a 28-byte FPU environment.
func (c *CPU) fldenv(addr uint32) {
	b := c.Bytes(addr, 28)
	if b == nil {
		return
	}
	c.FPUCW = binary.LittleEndian.Uint16(b[0:])
	sw := binary.LittleEndian.Uint16(b[4:])
	c.FPUSW, c.FPUTop = sw&0xC7FF, int(sw>>11&7)
}

func (c *CPU) fpuCompareEFlags(a, b float64) {
	c.SetFlag(FlagOF|FlagSF|FlagAF, false)
	if math.IsNaN(a) || math.IsNaN(b) {
		c.SetFlag(FlagZF|FlagPF|FlagCF, true)
	} else if a < b {
		c.SetFlag(FlagCF, true)
		c.SetFlag(FlagZF|FlagPF, false)
	} else if a == b {
		c.SetFlag(FlagZF, true)
		c.SetFlag(FlagCF|FlagPF, false)
	} else {
		c.SetFlag(FlagZF|FlagPF|FlagCF, false)
	}
}

func (c *CPU) fpuRound(v float64) float64 {
	rc := (c.FPUCW >> 10) & 3
	switch rc {
	case 0:
		return math.RoundToEven(v)
	case 1:
		return math.Floor(v)
	case 2:
		return math.Ceil(v)
	case 3:
		return math.Trunc(v)
	}
	return math.RoundToEven(v)
}

func (c *CPU) execFPU(op uint8, segOverride uint32, hasSeg bool) {
	modrm := c.Fetch8()
	mod := (modrm >> 6) & 3
	reg := int((modrm >> 3) & 7)
	rm := int(modrm & 7)

	if mod == 3 {
		switch op {
		case 0xD8:
			a := c.FPUGet(0)
			b := c.FPUGet(rm)
			switch reg {
			case 0:
				c.FPUSet(0, a+b)
			case 1:
				c.FPUSet(0, a*b)
			case 2:
				c.fpuCompare(a, b)
			case 3:
				c.fpuCompare(a, b)
				c.FPUPop()
			case 4:
				c.FPUSet(0, a-b)
			case 5:
				c.FPUSet(0, b-a)
			case 6:
				c.FPUSet(0, a/b)
			case 7:
				c.FPUSet(0, b/a)
			}
		case 0xD9:
			switch reg {
			case 0: // FLD ST(i)
				v := c.FPUGet(rm)
				c.FPUPush(v)
			case 1: // FXCH ST(i)
				v0 := c.FPUGet(0)
				vi := c.FPUGet(rm)
				c.FPUSet(0, vi)
				c.FPUSet(rm, v0)
			case 2: // FNOP
			case 3: // FSTP ST(i)
				v := c.FPUPop()
				if rm > 0 {
					c.FPUSet(rm-1, v)
				}
			case 4:
				switch rm {
				case 0: // FCHS
					c.FPUSet(0, -c.FPUGet(0))
				case 1: // FABS
					c.FPUSet(0, math.Abs(c.FPUGet(0)))
				case 4: // FTST
					c.fpuCompare(c.FPUGet(0), 0.0)
				case 5: // FXAM
					c.fxam(c.FPUGet(0))
				}
			case 5:
				switch rm {
				case 0: // FLD1
					c.FPUPush(1.0)
				case 1: // FLDL2T
					c.FPUPush(math.Log2(10.0))
				case 2: // FLDL2E
					c.FPUPush(math.Log2E)
				case 3: // FLDPI
					c.FPUPush(math.Pi)
				case 4: // FLDLG2
					c.FPUPush(math.Log10(2.0))
				case 5: // FLDLN2
					c.FPUPush(math.Ln2)
				case 6: // FLDZ
					c.FPUPush(0.0)
				}
			case 6:
				switch rm {
				case 0: // F2XM1
					c.FPUSet(0, math.Pow(2, c.FPUGet(0))-1.0)
				case 1: // FYL2X
					x := c.FPUPop()
					y := c.FPUGet(0)
					c.FPUSet(0, y*math.Log2(x))
				case 2: // FPTAN
					c.FPUSet(0, math.Tan(c.FPUGet(0)))
					c.FPUPush(1.0)
					c.FPUSW &^= (1 << 10)
				case 3: // FPATAN
					x := c.FPUPop()
					y := c.FPUGet(0)
					c.FPUSet(0, math.Atan2(y, x))
				case 4: // FXTRACT
					v := c.FPUGet(0)
					frac, exp := math.Frexp(v)
					c.FPUSet(0, float64(exp-1))
					c.FPUPush(frac * 2.0)
				case 5: // FPREM1
					c.fprem(math.Remainder, math.RoundToEven)
				case 6: // FDECSTP
					c.FPUTop = (c.FPUTop - 1) & 7
				case 7: // FINCSTP
					c.FPUTop = (c.FPUTop + 1) & 7
				}
			case 7:
				switch rm {
				case 0: // FPREM
					c.fprem(math.Mod, math.Trunc)
				case 1: // FYL2XP1
					x := c.FPUPop()
					y := c.FPUGet(0)
					c.FPUSet(0, y*math.Log2(x+1.0))
				case 2: // FSQRT
					c.FPUSet(0, math.Sqrt(c.FPUGet(0)))
				case 3: // FSINCOS
					v := c.FPUGet(0)
					s, co := math.Sincos(v)
					c.FPUSet(0, s)
					c.FPUPush(co)
					c.FPUSW &^= (1 << 10)
				case 4: // FRNDINT
					c.FPUSet(0, c.fpuRound(c.FPUGet(0)))
				case 5: // FSCALE
					c.FPUSet(0, c.FPUGet(0)*math.Pow(2.0, math.Trunc(c.FPUGet(1))))
				case 6: // FSIN
					c.FPUSet(0, math.Sin(c.FPUGet(0)))
					c.FPUSW &^= (1 << 10)
				case 7: // FCOS
					c.FPUSet(0, math.Cos(c.FPUGet(0)))
					c.FPUSW &^= (1 << 10)
				}
			}
		case 0xDA:
			switch reg {
			case 0: // FCMOVB
				if c.GetFlag(FlagCF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 1: // FCMOVE
				if c.GetFlag(FlagZF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 2: // FCMOVBE
				if c.GetFlag(FlagCF) || c.GetFlag(FlagZF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 3: // FCMOVU
				if c.GetFlag(FlagPF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 5:
				if rm == 1 { // FUCOMPP
					c.fpuCompare(c.FPUGet(0), c.FPUGet(1))
					c.FPUPop()
					c.FPUPop()
				}
			}
		case 0xDB:
			switch reg {
			case 0: // FCMOVNB
				if !c.GetFlag(FlagCF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 1: // FCMOVNE
				if !c.GetFlag(FlagZF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 2: // FCMOVNBE
				if !c.GetFlag(FlagCF) && !c.GetFlag(FlagZF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 3: // FCMOVNU
				if !c.GetFlag(FlagPF) {
					c.FPUSet(0, c.FPUGet(rm))
				}
			case 4: // FNINIT / FCLEX
				if rm == 3 {
					c.FPUCW = 0x037F
					c.FPUSW = 0
					c.FPUTop = 0
				} else if rm == 2 {
					c.FPUSW &= 0x7F00
				}
			case 5: // FUCOMI
				c.fpuCompareEFlags(c.FPUGet(0), c.FPUGet(rm))
			case 6: // FCOMI
				c.fpuCompareEFlags(c.FPUGet(0), c.FPUGet(rm))
			}
		case 0xDC:
			a := c.FPUGet(rm)
			b := c.FPUGet(0)
			switch reg {
			case 0:
				c.FPUSet(rm, a+b)
			case 1:
				c.FPUSet(rm, a*b)
			case 2:
				c.fpuCompare(b, a)
			case 3:
				c.fpuCompare(b, a)
				c.FPUPop()
			case 4:
				c.FPUSet(rm, a-b)
			case 5:
				c.FPUSet(rm, b-a)
			case 6:
				c.FPUSet(rm, a/b)
			case 7:
				c.FPUSet(rm, b/a)
			}
		case 0xDD:
			switch reg {
			case 0: // FFREE ST(i)
			case 1: // FXCH
				v0 := c.FPUGet(0)
				vi := c.FPUGet(rm)
				c.FPUSet(0, vi)
				c.FPUSet(rm, v0)
			case 2: // FST ST(i)
				c.FPUSet(rm, c.FPUGet(0))
			case 3: // FSTP ST(i)
				v := c.FPUPop()
				if rm > 0 {
					c.FPUSet(rm-1, v)
				}
			case 4: // FUCOM ST(i)
				c.fpuCompare(c.FPUGet(0), c.FPUGet(rm))
			case 5: // FUCOMP ST(i)
				c.fpuCompare(c.FPUGet(0), c.FPUGet(rm))
				c.FPUPop()
			}
		case 0xDE:
			a := c.FPUGet(rm)
			b := c.FPUGet(0)
			switch reg {
			case 0: // FADDP
				c.FPUSet(rm, a+b)
				c.FPUPop()
			case 1: // FMULP
				c.FPUSet(rm, a*b)
				c.FPUPop()
			case 2: // FCOMP
				c.fpuCompare(b, a)
				c.FPUPop()
			case 3: // FCOMPP
				c.fpuCompare(b, c.FPUGet(1))
				c.FPUPop()
				c.FPUPop()
			case 4: // FSUBRP (ST(i) = ST(0) - ST(i))
				c.FPUSet(rm, b-a)
				c.FPUPop()
			case 5: // FSUBP (ST(i) = ST(i) - ST(0))
				c.FPUSet(rm, a-b)
				c.FPUPop()
			case 6: // FDIVRP (ST(i) = ST(0) / ST(i))
				c.FPUSet(rm, b/a)
				c.FPUPop()
			case 7: // FDIVP (ST(i) = ST(i) / ST(0))
				c.FPUSet(rm, a/b)
				c.FPUPop()
			}
		case 0xDF:
			switch reg {
			case 4:
				if rm == 0 { // FNSTSW AX
					sw := (c.FPUSW & 0xC7FF) | (uint16(c.FPUTop&7) << 11)
					c.SetReg16(RegEAX, sw)
				}
			case 5: // FUCOMIP
				c.fpuCompareEFlags(c.FPUGet(0), c.FPUGet(rm))
				c.FPUPop()
			case 6: // FCOMIP
				c.fpuCompareEFlags(c.FPUGet(0), c.FPUGet(rm))
				c.FPUPop()
			}
		}
		return
	}

	// Memory operand (mod != 3)
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	switch op {
	case 0xD8: // float32 memory
		m := float64(c.ReadF32(addr))
		a := c.FPUGet(0)
		switch reg {
		case 0:
			c.FPUSet(0, a+m)
		case 1:
			c.FPUSet(0, a*m)
		case 2:
			c.fpuCompare(a, m)
		case 3:
			c.fpuCompare(a, m)
			c.FPUPop()
		case 4:
			c.FPUSet(0, a-m)
		case 5:
			c.FPUSet(0, m-a)
		case 6:
			c.FPUSet(0, a/m)
		case 7:
			c.FPUSet(0, m/a)
		}
	case 0xD9:
		switch reg {
		case 0: // FLD m32fp
			c.FPUPush(float64(c.ReadF32(addr)))
		case 2: // FST m32fp
			c.WriteF32(addr, float32(c.FPUGet(0)))
		case 3: // FSTP m32fp
			c.WriteF32(addr, float32(c.FPUPop()))
		case 4: // FLDENV m28
			c.fldenv(addr)
		case 5: // FLDCW m16
			c.FPUCW = c.Read16(addr)
		case 6: // FNSTENV m28
			c.fnstenv(addr)
		case 7: // FNSTCW m16
			c.Write16(addr, c.FPUCW)
		}
	case 0xDA: // int32 memory
		m := float64(int32(c.Read32(addr)))
		a := c.FPUGet(0)
		switch reg {
		case 0:
			c.FPUSet(0, a+m)
		case 1:
			c.FPUSet(0, a*m)
		case 2:
			c.fpuCompare(a, m)
		case 3:
			c.fpuCompare(a, m)
			c.FPUPop()
		case 4:
			c.FPUSet(0, a-m)
		case 5:
			c.FPUSet(0, m-a)
		case 6:
			c.FPUSet(0, a/m)
		case 7:
			c.FPUSet(0, m/a)
		}
	case 0xDB:
		switch reg {
		case 0: // FILD m32int
			c.FPUPush(float64(int32(c.Read32(addr))))
		case 1: // FISTTP m32int (SSE3 truncate)
			c.Write32(addr, uint32(ToInt32(math.Trunc(c.FPUPop()))))
		case 2: // FIST m32int
			c.Write32(addr, uint32(ToInt32(c.fpuRound(c.FPUGet(0)))))
		case 3: // FISTP m32int
			c.Write32(addr, uint32(ToInt32(c.fpuRound(c.FPUPop()))))
		case 5: // FLD m80fp (approximate via 80-bit IEEE 754 extended precision)
			if b := c.Bytes(addr, 10); b != nil {
				c.FPUPush(readFloat80(b))
			}
		case 7: // FSTP m80fp
			if b := c.WBytes(addr, 10); b != nil {
				writeFloat80(b, c.FPUPop())
			}
		}
	case 0xDC: // float64 memory
		m := c.ReadF64(addr)
		a := c.FPUGet(0)
		switch reg {
		case 0:
			c.FPUSet(0, a+m)
		case 1:
			c.FPUSet(0, a*m)
		case 2:
			c.fpuCompare(a, m)
		case 3:
			c.fpuCompare(a, m)
			c.FPUPop()
		case 4:
			c.FPUSet(0, a-m)
		case 5:
			c.FPUSet(0, m-a)
		case 6:
			c.FPUSet(0, a/m)
		case 7:
			c.FPUSet(0, m/a)
		}
	case 0xDD:
		switch reg {
		case 0: // FLD m64fp
			c.FPUPush(c.ReadF64(addr))
		case 1: // FISTTP m64int
			c.Write64(addr, uint64(ToInt64(math.Trunc(c.FPUPop()))))
		case 2: // FST m64fp
			c.WriteF64(addr, c.FPUGet(0))
		case 3: // FSTP m64fp
			c.WriteF64(addr, c.FPUPop())
		case 4: // FRSTOR m108
			c.fldenv(addr)
			for i := 0; i < 8; i++ {
				if b := c.Bytes(addr+28+uint32(10*i), 10); b != nil {
					c.FPUSet(i, readFloat80(b))
				}
			}
		case 6: // FNSAVE m108: store, then FNINIT
			c.fnstenv(addr)
			for i := 0; i < 8; i++ {
				if b := c.WBytes(addr+28+uint32(10*i), 10); b != nil {
					writeFloat80(b, c.FPUGet(i))
				}
			}
			c.FPUCW, c.FPUSW, c.FPUTop = 0x037F, 0, 0
		case 7: // FNSTSW m16
			sw := (c.FPUSW & 0xC7FF) | (uint16(c.FPUTop&7) << 11)
			c.Write16(addr, sw)
		}
	case 0xDE: // int16 memory
		m := float64(int16(c.Read16(addr)))
		a := c.FPUGet(0)
		switch reg {
		case 0:
			c.FPUSet(0, a+m)
		case 1:
			c.FPUSet(0, a*m)
		case 2:
			c.fpuCompare(a, m)
		case 3:
			c.fpuCompare(a, m)
			c.FPUPop()
		case 4:
			c.FPUSet(0, a-m)
		case 5:
			c.FPUSet(0, m-a)
		case 6:
			c.FPUSet(0, a/m)
		case 7:
			c.FPUSet(0, m/a)
		}
	case 0xDF:
		switch reg {
		case 0: // FILD m16int
			c.FPUPush(float64(int16(c.Read16(addr))))
		case 1: // FISTTP m16int
			c.Write16(addr, uint16(ToInt16(math.Trunc(c.FPUPop()))))
		case 2: // FIST m16int
			c.Write16(addr, uint16(ToInt16(c.fpuRound(c.FPUGet(0)))))
		case 3: // FISTP m16int
			c.Write16(addr, uint16(ToInt16(c.fpuRound(c.FPUPop()))))
		case 5: // FILD m64int
			c.FPUPush(float64(int64(c.Read64(addr))))
		case 7: // FISTP m64int
			c.Write64(addr, uint64(ToInt64(c.fpuRound(c.FPUPop()))))
		}
	}
}

func readFloat80(b []byte) float64 {
	mant := binary.LittleEndian.Uint64(b[0:8])
	sexp := binary.LittleEndian.Uint16(b[8:10])
	sign := (sexp & 0x8000) != 0
	exp := int(sexp & 0x7FFF)
	if exp == 0 && mant == 0 {
		if sign {
			return math.Copysign(0, -1)
		}
		return 0
	}
	if exp == 0x7FFF {
		if (mant & 0x7FFFFFFFFFFFFFFF) != 0 {
			return math.NaN()
		}
		if sign {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	val := math.Ldexp(float64(mant), exp-16383-63)
	if sign {
		val = -val
	}
	return val
}

func writeFloat80(b []byte, v float64) {
	if v == 0 {
		clear(b[:10])
		if math.Signbit(v) {
			b[9] = 0x80
		}
		return
	}
	sign := uint16(0)
	if math.Signbit(v) {
		sign = 0x8000
		v = -v
	}
	if math.IsNaN(v) {
		binary.LittleEndian.PutUint64(b[0:8], 0xC000000000000000)
		binary.LittleEndian.PutUint16(b[8:10], sign|0x7FFF)
		return
	}
	if math.IsInf(v, 0) {
		binary.LittleEndian.PutUint64(b[0:8], 0x8000000000000000)
		binary.LittleEndian.PutUint16(b[8:10], sign|0x7FFF)
		return
	}
	frac, exp := math.Frexp(v)
	mant := uint64(math.Ldexp(frac, 64))
	biased := uint16(exp - 1 + 16383)
	binary.LittleEndian.PutUint64(b[0:8], mant)
	binary.LittleEndian.PutUint16(b[8:10], sign|biased)
}

func evalSSECompareF32(a, b float32, imm uint8) bool {
	switch imm & 7 {
	case 0: // EQ
		return a == b
	case 1: // LT
		return a < b
	case 2: // LE
		return a <= b
	case 3: // UNORD
		return math.IsNaN(float64(a)) || math.IsNaN(float64(b))
	case 4: // NEQ
		return a != b
	case 5: // NLT
		return !(a < b)
	case 6: // NLE
		return !(a <= b)
	case 7: // ORD
		return !math.IsNaN(float64(a)) && !math.IsNaN(float64(b))
	}
	return false
}

func evalSSECompareF64(a, b float64, imm uint8) bool {
	switch imm & 7 {
	case 0:
		return a == b
	case 1:
		return a < b
	case 2:
		return a <= b
	case 3:
		return math.IsNaN(a) || math.IsNaN(b)
	case 4:
		return a != b
	case 5:
		return !(a < b)
	case 6:
		return !(a <= b)
	case 7:
		return !math.IsNaN(a) && !math.IsNaN(b)
	}
	return false
}

func (c *CPU) readXMMOrMem128(modrm uint8, segOverride uint32, hasSeg bool) [16]byte {
	var out [16]byte
	if (modrm >> 6) == 3 {
		out = c.XMM[modrm&7]
	} else {
		out = c.Read128(c.calcModRMAddr(modrm, segOverride, hasSeg))
	}
	return out
}

func (c *CPU) writeXMMOrMem128(modrm uint8, segOverride uint32, hasSeg bool, val [16]byte) {
	if (modrm >> 6) == 3 {
		c.XMM[modrm&7] = val
	} else {
		c.Write128(c.calcModRMAddr(modrm, segOverride, hasSeg), val)
	}
}

func (c *CPU) readXMMOrMemF32(modrm uint8, segOverride uint32, hasSeg bool) float32 {
	if (modrm >> 6) == 3 {
		return c.getXMMF32(int(modrm&7), 0)
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	return c.ReadF32(addr)
}

func (c *CPU) readXMMOrMemF64(modrm uint8, segOverride uint32, hasSeg bool) float64 {
	if (modrm >> 6) == 3 {
		return c.getXMMF64(int(modrm&7), 0)
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	return c.ReadF64(addr)
}

// mmxOps marks the 0F opcodes that, without a 66/F2/F3 prefix, are MMX instructions on the
// mm0-mm7 registers. MMX is not emulated: they report unimplemented (and halt) instead of
// running as their SSE2 forms on the XMM registers.
var mmxOps = func() (m [256]bool) {
	for _, op := range []uint8{0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6A, 0x6B,
		0x6E, 0x6F, 0x70, 0x74, 0x75, 0x76, 0x7E, 0x7F, 0xC4, 0xC5, 0xD7, 0xE7, 0xF7} {
		m[op] = true
	}
	for op := 0xD1; op <= 0xFE; op++ {
		if op != 0xD6 && op != 0xE6 && op != 0xF0 {
			m[op] = true
		}
	}
	return m
}()

func (c *CPU) execSSE(op2 uint8, opSize16 bool, repF3 bool, repF2 bool, segOverride uint32, hasSeg bool) bool {
	if mmxOps[op2] && !opSize16 && !repF3 && !repF2 {
		return false
	}
	switch op2 {
	case 0x10: // MOVUPS / MOVUPD / MOVSS / MOVSD (load)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // MOVSS
			if (modrm >> 6) == 3 {
				c.setXMMF32(reg, 0, c.getXMMF32(int(modrm&7), 0))
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				clear(c.XMM[reg][:])
				c.setXMMF32(reg, 0, c.ReadF32(addr))
			}
		} else if repF2 { // MOVSD
			if (modrm >> 6) == 3 {
				c.setXMMF64(reg, 0, c.getXMMF64(int(modrm&7), 0))
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				clear(c.XMM[reg][:])
				c.setXMMF64(reg, 0, c.ReadF64(addr))
			}
		} else { // MOVUPS / MOVUPD
			c.XMM[reg] = c.readXMMOrMem128(modrm, segOverride, hasSeg)
		}
		return true

	case 0x11: // MOVUPS / MOVUPD / MOVSS / MOVSD (store)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // MOVSS
			if (modrm >> 6) == 3 {
				c.setXMMF32(int(modrm&7), 0, c.getXMMF32(reg, 0))
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				c.WriteF32(addr, c.getXMMF32(reg, 0))
			}
		} else if repF2 { // MOVSD
			if (modrm >> 6) == 3 {
				c.setXMMF64(int(modrm&7), 0, c.getXMMF64(reg, 0))
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				c.WriteF64(addr, c.getXMMF64(reg, 0))
			}
		} else {
			c.writeXMMOrMem128(modrm, segOverride, hasSeg, c.XMM[reg])
		}
		return true

	case 0x12: // MOVLPS / MOVHLPS / MOVDDUP / MOVSLDUP
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // MOVSLDUP
			src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
			copy(c.XMM[reg][0:4], src[0:4])
			copy(c.XMM[reg][4:8], src[0:4])
			copy(c.XMM[reg][8:12], src[8:12])
			copy(c.XMM[reg][12:16], src[8:12])
		} else if repF2 { // MOVDDUP
			var low uint64
			if (modrm >> 6) == 3 {
				low = c.getXMMU64(int(modrm&7), 0)
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				low = c.Read64(addr)
			}
			c.setXMMU64(reg, 0, low)
			c.setXMMU64(reg, 1, low)
		} else if (modrm >> 6) == 3 { // MOVHLPS
			copy(c.XMM[reg][0:8], c.XMM[modrm&7][8:16])
		} else { // MOVLPS / MOVLPD
			addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
			binary.LittleEndian.PutUint64(c.XMM[reg][0:8], c.Read64(addr))
		}
		return true

	case 0x13: // MOVLPS / MOVLPD store
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
		c.Write64(addr, binary.LittleEndian.Uint64(c.XMM[reg][0:8]))
		return true

	case 0x14: // UNPCKLPS / UNPCKLPD
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		dst := c.XMM[reg]
		if opSize16 { // UNPCKLPD
			copy(c.XMM[reg][0:8], dst[0:8])
			copy(c.XMM[reg][8:16], src[0:8])
		} else { // UNPCKLPS
			copy(c.XMM[reg][0:4], dst[0:4])
			copy(c.XMM[reg][4:8], src[0:4])
			copy(c.XMM[reg][8:12], dst[4:8])
			copy(c.XMM[reg][12:16], src[4:8])
		}
		return true

	case 0x15: // UNPCKHPS / UNPCKHPD
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		dst := c.XMM[reg]
		if opSize16 { // UNPCKHPD
			copy(c.XMM[reg][0:8], dst[8:16])
			copy(c.XMM[reg][8:16], src[8:16])
		} else { // UNPCKHPS
			copy(c.XMM[reg][0:4], dst[8:12])
			copy(c.XMM[reg][4:8], src[8:12])
			copy(c.XMM[reg][8:12], dst[12:16])
			copy(c.XMM[reg][12:16], src[12:16])
		}
		return true

	case 0x16: // MOVHPS / MOVLHPS / MOVSHDUP
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // MOVSHDUP
			src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
			copy(c.XMM[reg][0:4], src[4:8])
			copy(c.XMM[reg][4:8], src[4:8])
			copy(c.XMM[reg][8:12], src[12:16])
			copy(c.XMM[reg][12:16], src[12:16])
		} else if (modrm >> 6) == 3 { // MOVLHPS
			copy(c.XMM[reg][8:16], c.XMM[modrm&7][0:8])
		} else { // MOVHPS / MOVHPD
			addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
			binary.LittleEndian.PutUint64(c.XMM[reg][8:16], c.Read64(addr))
		}
		return true

	case 0x17: // MOVHPS / MOVHPD store
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
		c.Write64(addr, binary.LittleEndian.Uint64(c.XMM[reg][8:16]))
		return true

	case 0x28: // MOVAPS / MOVAPD (load)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		c.XMM[reg] = c.readXMMOrMem128(modrm, segOverride, hasSeg)
		return true

	case 0x29: // MOVAPS / MOVAPD (store)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		c.writeXMMOrMem128(modrm, segOverride, hasSeg, c.XMM[reg])
		return true

	case 0x2A: // CVTSI2SS / CVTSI2SD / CVTPI2PS
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		val := int32(c.readRM32(modrm, segOverride, hasSeg))
		if repF3 { // CVTSI2SS
			c.setXMMF32(reg, 0, float32(val))
		} else if repF2 { // CVTSI2SD
			c.setXMMF64(reg, 0, float64(val))
		} else {
			c.setXMMF32(reg, 0, float32(val))
		}
		return true

	case 0x2C: // CVTTSS2SI / CVTTSD2SI
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF2 {
			v := c.readXMMOrMemF64(modrm, segOverride, hasSeg)
			c.Regs[reg] = uint32(ToInt32(math.Trunc(v)))
		} else {
			v := c.readXMMOrMemF32(modrm, segOverride, hasSeg)
			c.Regs[reg] = uint32(ToInt32(math.Trunc(float64(v))))
		}
		return true

	case 0x2D: // CVTSS2SI / CVTSD2SI
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF2 {
			v := c.readXMMOrMemF64(modrm, segOverride, hasSeg)
			c.Regs[reg] = uint32(ToInt32(math.RoundToEven(v)))
		} else {
			v := c.readXMMOrMemF32(modrm, segOverride, hasSeg)
			c.Regs[reg] = uint32(ToInt32(math.RoundToEven(float64(v))))
		}
		return true

	case 0x2E, 0x2F: // UCOMISS / COMISS / UCOMISD / COMISD
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if opSize16 { // UCOMISD / COMISD
			a := c.getXMMF64(reg, 0)
			b := c.readXMMOrMemF64(modrm, segOverride, hasSeg)
			c.fpuCompareEFlags(a, b)
		} else { // UCOMISS / COMISS
			a := float64(c.getXMMF32(reg, 0))
			b := float64(c.readXMMOrMemF32(modrm, segOverride, hasSeg))
			c.fpuCompareEFlags(a, b)
		}
		return true

	case 0x50: // MOVMSKPS / MOVMSKPD
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		if opSize16 {
			m := uint32(0)
			if (c.XMM[rm][7] & 0x80) != 0 {
				m |= 1
			}
			if (c.XMM[rm][15] & 0x80) != 0 {
				m |= 2
			}
			c.Regs[reg] = m
		} else {
			m := uint32(0)
			for i := 0; i < 4; i++ {
				if (c.XMM[rm][i*4+3] & 0x80) != 0 {
					m |= (1 << i)
				}
			}
			c.Regs[reg] = m
		}
		return true

	case 0x51, 0x52, 0x53, 0x58, 0x59, 0x5C, 0x5D, 0x5E, 0x5F:
		// SQRT(51), RSQRT(52), RCP(53), ADD(58), MUL(59), SUB(5C), MIN(5D), DIV(5E), MAX(5F)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // Scalar float32 (*SS)
			a := c.getXMMF32(reg, 0)
			b := c.readXMMOrMemF32(modrm, segOverride, hasSeg)
			var r float32
			switch op2 {
			case 0x51:
				r = float32(math.Sqrt(float64(b)))
			case 0x52:
				r = float32(1.0 / math.Sqrt(float64(b)))
			case 0x53:
				r = 1.0 / b
			case 0x58:
				r = sseAdd(a, b)
			case 0x59:
				r = sseMul(a, b)
			case 0x5C:
				r = a - b
			case 0x5D:
				if a < b {
					r = a
				} else {
					r = b
				}
			case 0x5E:
				r = a / b
			case 0x5F:
				if a > b {
					r = a
				} else {
					r = b
				}
			}
			c.setXMMF32(reg, 0, r)
		} else if repF2 { // Scalar float64 (*SD)
			a := c.getXMMF64(reg, 0)
			b := c.readXMMOrMemF64(modrm, segOverride, hasSeg)
			var r float64
			switch op2 {
			case 0x51:
				r = math.Sqrt(b)
			case 0x58:
				r = sseAdd(a, b)
			case 0x59:
				r = sseMul(a, b)
			case 0x5C:
				r = a - b
			case 0x5D:
				if a < b {
					r = a
				} else {
					r = b
				}
			case 0x5E:
				r = a / b
			case 0x5F:
				if a > b {
					r = a
				} else {
					r = b
				}
			}
			c.setXMMF64(reg, 0, r)
		} else if opSize16 { // Packed float64 (*PD)
			src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
			for i := 0; i < 2; i++ {
				a := c.getXMMF64(reg, i)
				b := math.Float64frombits(binary.LittleEndian.Uint64(src[i*8 : i*8+8]))
				var r float64
				switch op2 {
				case 0x51:
					r = math.Sqrt(b)
				case 0x58:
					r = sseAdd(a, b)
				case 0x59:
					r = sseMul(a, b)
				case 0x5C:
					r = a - b
				case 0x5D:
					if a < b {
						r = a
					} else {
						r = b
					}
				case 0x5E:
					r = a / b
				case 0x5F:
					if a > b {
						r = a
					} else {
						r = b
					}
				}
				c.setXMMF64(reg, i, r)
			}
		} else { // Packed float32 (*PS)
			src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
			for i := 0; i < 4; i++ {
				a := c.getXMMF32(reg, i)
				b := math.Float32frombits(binary.LittleEndian.Uint32(src[i*4 : i*4+4]))
				var r float32
				switch op2 {
				case 0x51:
					r = float32(math.Sqrt(float64(b)))
				case 0x52:
					r = float32(1.0 / math.Sqrt(float64(b)))
				case 0x53:
					r = 1.0 / b
				case 0x58:
					r = sseAdd(a, b)
				case 0x59:
					r = sseMul(a, b)
				case 0x5C:
					r = a - b
				case 0x5D:
					if a < b {
						r = a
					} else {
						r = b
					}
				case 0x5E:
					r = a / b
				case 0x5F:
					if a > b {
						r = a
					} else {
						r = b
					}
				}
				c.setXMMF32(reg, i, r)
			}
		}
		return true

	case 0x54, 0x55, 0x56, 0x57: // ANDPS(54), ANDNPS(55), ORPS(56), XORPS(57)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		for i := 0; i < 16; i++ {
			switch op2 {
			case 0x54:
				c.XMM[reg][i] &= src[i]
			case 0x55:
				c.XMM[reg][i] = (^c.XMM[reg][i]) & src[i]
			case 0x56:
				c.XMM[reg][i] |= src[i]
			case 0x57:
				c.XMM[reg][i] ^= src[i]
			}
		}
		return true

	case 0x5A: // CVTPS2PD / CVTPD2PS / CVTSS2SD / CVTSD2SS
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // CVTSS2SD
			v := c.readXMMOrMemF32(modrm, segOverride, hasSeg)
			c.setXMMF64(reg, 0, float64(v))
		} else if repF2 { // CVTSD2SS
			v := c.readXMMOrMemF64(modrm, segOverride, hasSeg)
			c.setXMMF32(reg, 0, float32(v))
		} else if opSize16 { // CVTPD2PS
			src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
			f0 := math.Float64frombits(binary.LittleEndian.Uint64(src[0:8]))
			f1 := math.Float64frombits(binary.LittleEndian.Uint64(src[8:16]))
			clear(c.XMM[reg][:])
			c.setXMMF32(reg, 0, float32(f0))
			c.setXMMF32(reg, 1, float32(f1))
		} else { // CVTPS2PD
			var f0, f1 float32
			if (modrm >> 6) == 3 {
				f0 = c.getXMMF32(int(modrm&7), 0)
				f1 = c.getXMMF32(int(modrm&7), 1)
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				f0 = c.ReadF32(addr)
				f1 = c.ReadF32(addr + 4)
			}
			c.setXMMF64(reg, 0, float64(f0))
			c.setXMMF64(reg, 1, float64(f1))
		}
		return true

	case 0x5B: // CVTDQ2PS / CVTPS2DQ / CVTTPS2DQ
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		if repF3 { // CVTTPS2DQ
			for i := 0; i < 4; i++ {
				f := math.Float32frombits(binary.LittleEndian.Uint32(src[i*4 : i*4+4]))
				c.setXMMU32(reg, i, uint32(ToInt32(math.Trunc(float64(f)))))
			}
		} else if opSize16 { // CVTPS2DQ
			for i := 0; i < 4; i++ {
				f := math.Float32frombits(binary.LittleEndian.Uint32(src[i*4 : i*4+4]))
				c.setXMMU32(reg, i, uint32(ToInt32(math.RoundToEven(float64(f)))))
			}
		} else { // CVTDQ2PS
			for i := 0; i < 4; i++ {
				iv := int32(binary.LittleEndian.Uint32(src[i*4 : i*4+4]))
				c.setXMMF32(reg, i, float32(iv))
			}
		}
		return true

	case 0x60, 0x61, 0x62, 0x63, 0x67, 0x68, 0x69, 0x6A, 0x6B, 0x6C, 0x6D: // PUNPCK* / PACK*
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		dst := c.XMM[reg]
		var res [16]byte
		switch op2 {
		case 0x60: // PUNPCKLBW
			for i := 0; i < 8; i++ {
				res[i*2] = dst[i]
				res[i*2+1] = src[i]
			}
		case 0x61: // PUNPCKLWD
			for i := 0; i < 4; i++ {
				copy(res[i*4:i*4+2], dst[i*2:i*2+2])
				copy(res[i*4+2:i*4+4], src[i*2:i*2+2])
			}
		case 0x62: // PUNPCKLDQ
			for i := 0; i < 2; i++ {
				copy(res[i*8:i*8+4], dst[i*4:i*4+4])
				copy(res[i*8+4:i*8+8], src[i*4:i*4+4])
			}
		case 0x63: // PACKSSWB
			for i := 0; i < 8; i++ {
				v0 := int16(binary.LittleEndian.Uint16(dst[i*2 : i*2+2]))
				v1 := int16(binary.LittleEndian.Uint16(src[i*2 : i*2+2]))
				res[i] = satS16ToS8(v0)
				res[8+i] = satS16ToS8(v1)
			}
		case 0x67: // PACKUSWB
			for i := 0; i < 8; i++ {
				v0 := int16(binary.LittleEndian.Uint16(dst[i*2 : i*2+2]))
				v1 := int16(binary.LittleEndian.Uint16(src[i*2 : i*2+2]))
				res[i] = satS16ToU8(v0)
				res[8+i] = satS16ToU8(v1)
			}
		case 0x68: // PUNPCKHBW
			for i := 0; i < 8; i++ {
				res[i*2] = dst[8+i]
				res[i*2+1] = src[8+i]
			}
		case 0x69: // PUNPCKHWD
			for i := 0; i < 4; i++ {
				copy(res[i*4:i*4+2], dst[8+i*2:8+i*2+2])
				copy(res[i*4+2:i*4+4], src[8+i*2:8+i*2+2])
			}
		case 0x6A: // PUNPCKHDQ
			for i := 0; i < 2; i++ {
				copy(res[i*8:i*8+4], dst[8+i*4:8+i*4+4])
				copy(res[i*8+4:i*8+8], src[8+i*4:8+i*4+4])
			}
		case 0x6B: // PACKSSDW
			for i := 0; i < 4; i++ {
				v0 := int32(binary.LittleEndian.Uint32(dst[i*4 : i*4+4]))
				v1 := int32(binary.LittleEndian.Uint32(src[i*4 : i*4+4]))
				binary.LittleEndian.PutUint16(res[i*2:i*2+2], satS32ToS16(v0))
				binary.LittleEndian.PutUint16(res[8+i*2:8+i*2+2], satS32ToS16(v1))
			}
		case 0x6C: // PUNPCKLQDQ
			copy(res[0:8], dst[0:8])
			copy(res[8:16], src[0:8])
		case 0x6D: // PUNPCKHQDQ
			copy(res[0:8], dst[8:16])
			copy(res[8:16], src[8:16])
		}
		c.XMM[reg] = res
		return true

	case 0x6E: // MOVD xmm, r/m32
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		val := c.readRM32(modrm, segOverride, hasSeg)
		clear(c.XMM[reg][:])
		c.setXMMU32(reg, 0, val)
		return true

	case 0x6F: // MOVDQA / MOVDQU (load)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		c.XMM[reg] = c.readXMMOrMem128(modrm, segOverride, hasSeg)
		return true

	case 0x70: // PSHUFD / PSHUFLW / PSHUFHW
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		imm := c.Fetch8()
		var res [16]byte
		if opSize16 { // PSHUFD
			for i := 0; i < 4; i++ {
				sel := (imm >> (i * 2)) & 3
				copy(res[i*4:i*4+4], src[int(sel)*4:int(sel)*4+4])
			}
		} else if repF2 { // PSHUFLW
			copy(res[8:16], src[8:16])
			for i := 0; i < 4; i++ {
				sel := (imm >> (i * 2)) & 3
				copy(res[i*2:i*2+2], src[int(sel)*2:int(sel)*2+2])
			}
		} else if repF3 { // PSHUFHW
			copy(res[0:8], src[0:8])
			for i := 0; i < 4; i++ {
				sel := (imm >> (i * 2)) & 3
				copy(res[8+i*2:8+i*2+2], src[8+int(sel)*2:8+int(sel)*2+2])
			}
		}
		c.XMM[reg] = res
		return true

	case 0x71, 0x72, 0x73: // PSRLW/PSRAW/PSLLW (71), PSRLD/PSRAD/PSLLD (72), PSRLQ/PSRLDQ/PSLLQ/PSLLDQ (73)
		modrm := c.Fetch8()
		subop := (modrm >> 3) & 7
		rm := int(modrm & 7)
		imm := c.Fetch8()
		switch op2 {
		case 0x71:
			for i := 0; i < 8; i++ {
				v := binary.LittleEndian.Uint16(c.XMM[rm][i*2 : i*2+2])
				switch subop {
				case 2: // PSRLW
					v >>= imm
				case 4: // PSRAW
					v = uint16(int16(v) >> imm)
				case 6: // PSLLW
					v <<= imm
				}
				binary.LittleEndian.PutUint16(c.XMM[rm][i*2:i*2+2], v)
			}
		case 0x72:
			for i := 0; i < 4; i++ {
				v := c.getXMMU32(rm, i)
				switch subop {
				case 2: // PSRLD
					v >>= imm
				case 4: // PSRAD
					v = uint32(int32(v) >> imm)
				case 6: // PSLLD
					v <<= imm
				}
				c.setXMMU32(rm, i, v)
			}
		case 0x73:
			switch subop {
			case 2: // PSRLQ
				for i := 0; i < 2; i++ {
					c.setXMMU64(rm, i, c.getXMMU64(rm, i)>>imm)
				}
			case 3: // PSRLDQ (byte shift right)
				var res [16]byte
				if imm < 16 {
					copy(res[:16-imm], c.XMM[rm][imm:])
				}
				c.XMM[rm] = res
			case 6: // PSLLQ
				for i := 0; i < 2; i++ {
					c.setXMMU64(rm, i, c.getXMMU64(rm, i)<<imm)
				}
			case 7: // PSLLDQ (byte shift left)
				var res [16]byte
				if imm < 16 {
					copy(res[imm:], c.XMM[rm][:16-imm])
				}
				c.XMM[rm] = res
			}
		}
		return true

	case 0x74, 0x75, 0x76: // PCMPEQB(74), PCMPEQW(75), PCMPEQD(76)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		switch op2 {
		case 0x74:
			for i := 0; i < 16; i++ {
				if c.XMM[reg][i] == src[i] {
					c.XMM[reg][i] = 0xFF
				} else {
					c.XMM[reg][i] = 0
				}
			}
		case 0x75:
			for i := 0; i < 8; i++ {
				a := binary.LittleEndian.Uint16(c.XMM[reg][i*2 : i*2+2])
				b := binary.LittleEndian.Uint16(src[i*2 : i*2+2])
				v := uint16(0)
				if a == b {
					v = 0xFFFF
				}
				binary.LittleEndian.PutUint16(c.XMM[reg][i*2:i*2+2], v)
			}
		case 0x76:
			for i := 0; i < 4; i++ {
				a := c.getXMMU32(reg, i)
				b := binary.LittleEndian.Uint32(src[i*4 : i*4+4])
				if a == b {
					c.setXMMU32(reg, i, 0xFFFFFFFF)
				} else {
					c.setXMMU32(reg, i, 0)
				}
			}
		}
		return true

	case 0x77: // EMMS
		return true

	case 0x7C: // HADDPD (66) / HADDPS (F2)
		if !opSize16 && !repF2 {
			return false
		}
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		if opSize16 {
			a0, a1 := c.getXMMF64(reg, 0), c.getXMMF64(reg, 1)
			b0 := math.Float64frombits(binary.LittleEndian.Uint64(src[0:8]))
			b1 := math.Float64frombits(binary.LittleEndian.Uint64(src[8:16]))
			c.setXMMF64(reg, 0, a0+a1)
			c.setXMMF64(reg, 1, b0+b1)
			return true
		}
		a0, a1, a2, a3 := c.getXMMF32(reg, 0), c.getXMMF32(reg, 1), c.getXMMF32(reg, 2), c.getXMMF32(reg, 3)
		b0 := math.Float32frombits(binary.LittleEndian.Uint32(src[0:4]))
		b1 := math.Float32frombits(binary.LittleEndian.Uint32(src[4:8]))
		b2 := math.Float32frombits(binary.LittleEndian.Uint32(src[8:12]))
		b3 := math.Float32frombits(binary.LittleEndian.Uint32(src[12:16]))
		c.setXMMF32(reg, 0, a0+a1)
		c.setXMMF32(reg, 1, a2+a3)
		c.setXMMF32(reg, 2, b0+b1)
		c.setXMMF32(reg, 3, b2+b3)
		return true

	case 0x7E: // MOVD r/m32, xmm  OR  MOVQ xmm1, xmm2/m64 (F3 0F 7E)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // MOVQ xmm1, xmm2/m64
			var low uint64
			if (modrm >> 6) == 3 {
				low = c.getXMMU64(int(modrm&7), 0)
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				low = c.Read64(addr)
			}
			clear(c.XMM[reg][:])
			c.setXMMU64(reg, 0, low)
		} else { // MOVD r/m32, xmm
			c.writeRM32(modrm, segOverride, hasSeg, c.getXMMU32(reg, 0))
		}
		return true

	case 0x7F: // MOVDQA / MOVDQU (store)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		c.writeXMMOrMem128(modrm, segOverride, hasSeg, c.XMM[reg])
		return true

	case 0xAE: // LDMXCSR / STMXCSR / SFENCE / LFENCE / MFENCE
		modrm := c.Fetch8()
		subop := (modrm >> 3) & 7
		if (modrm >> 6) == 3 {
			return true // LFENCE / MFENCE / SFENCE
		}
		addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
		if subop == 2 { // LDMXCSR
			c.MXCSR = c.Read32(addr)
		} else if subop == 3 { // STMXCSR
			c.Write32(addr, c.MXCSR)
		}
		return true

	case 0xC2: // CMPPS / CMPSS / CMPPD / CMPSD
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if repF3 { // CMPSS
			b := c.readXMMOrMemF32(modrm, segOverride, hasSeg)
			imm := c.Fetch8()
			a := c.getXMMF32(reg, 0)
			if evalSSECompareF32(a, b, imm) {
				c.setXMMU32(reg, 0, 0xFFFFFFFF)
			} else {
				c.setXMMU32(reg, 0, 0)
			}
		} else if repF2 { // CMPSD
			b := c.readXMMOrMemF64(modrm, segOverride, hasSeg)
			imm := c.Fetch8()
			a := c.getXMMF64(reg, 0)
			if evalSSECompareF64(a, b, imm) {
				c.setXMMU64(reg, 0, 0xFFFFFFFFFFFFFFFF)
			} else {
				c.setXMMU64(reg, 0, 0)
			}
		} else if opSize16 { // CMPPD
			src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
			imm := c.Fetch8()
			for i := 0; i < 2; i++ {
				a := c.getXMMF64(reg, i)
				b := math.Float64frombits(binary.LittleEndian.Uint64(src[i*8 : i*8+8]))
				if evalSSECompareF64(a, b, imm) {
					c.setXMMU64(reg, i, 0xFFFFFFFFFFFFFFFF)
				} else {
					c.setXMMU64(reg, i, 0)
				}
			}
		} else { // CMPPS
			src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
			imm := c.Fetch8()
			for i := 0; i < 4; i++ {
				a := c.getXMMF32(reg, i)
				b := math.Float32frombits(binary.LittleEndian.Uint32(src[i*4 : i*4+4]))
				if evalSSECompareF32(a, b, imm) {
					c.setXMMU32(reg, i, 0xFFFFFFFF)
				} else {
					c.setXMMU32(reg, i, 0)
				}
			}
		}
		return true

	case 0xC4: // PINSRW
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		var v uint16
		if (modrm >> 6) == 3 {
			v = uint16(c.Regs[modrm&7])
		} else {
			addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
			v = c.Read16(addr)
		}
		imm := c.Fetch8() & 7
		binary.LittleEndian.PutUint16(c.XMM[reg][int(imm)*2:int(imm)*2+2], v)
		return true

	case 0xC5: // PEXTRW
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		imm := c.Fetch8() & 7
		c.Regs[reg] = uint32(binary.LittleEndian.Uint16(c.XMM[rm][int(imm)*2 : int(imm)*2+2]))
		return true

	case 0xC6: // SHUFPS / SHUFPD
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		dst := c.XMM[reg]
		imm := c.Fetch8()
		if opSize16 { // SHUFPD
			var res [16]byte
			s0 := int(imm & 1)
			s1 := int((imm >> 1) & 1)
			copy(res[0:8], dst[s0*8:s0*8+8])
			copy(res[8:16], src[s1*8:s1*8+8])
			c.XMM[reg] = res
		} else { // SHUFPS
			var res [16]byte
			s0 := int(imm & 3)
			s1 := int((imm >> 2) & 3)
			s2 := int((imm >> 4) & 3)
			s3 := int((imm >> 6) & 3)
			copy(res[0:4], dst[s0*4:s0*4+4])
			copy(res[4:8], dst[s1*4:s1*4+4])
			copy(res[8:12], src[s2*4:s2*4+4])
			copy(res[12:16], src[s3*4:s3*4+4])
			c.XMM[reg] = res
		}
		return true

	case 0x64, 0x65, 0x66, 0xD4, 0xF4, 0xFB, 0xFC, 0xFD, 0xFE, 0xF8, 0xF9, 0xFA, 0xD5, 0xDB, 0xDF, 0xEB, 0xEF:
		// Integer SIMD arithmetic, comparison & bitwise:
		// PCMPGTB(64), PCMPGTW(65), PCMPGTD(66), PADDQ(D4), PMULUDQ(F4), PSUBQ(FB),
		// PADDB(FC), PADDW(FD), PADDD(FE), PSUBB(F8), PSUBW(F9), PSUBD(FA),
		// PMULLW(D5), PAND(DB), PANDN(DF), POR(EB), PXOR(EF)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		switch op2 {
		case 0x64: // PCMPGTB
			for i := 0; i < 16; i++ {
				if int8(c.XMM[reg][i]) > int8(src[i]) {
					c.XMM[reg][i] = 0xFF
				} else {
					c.XMM[reg][i] = 0
				}
			}
		case 0x65: // PCMPGTW
			for i := 0; i < 8; i++ {
				a := int16(binary.LittleEndian.Uint16(c.XMM[reg][i*2 : i*2+2]))
				b := int16(binary.LittleEndian.Uint16(src[i*2 : i*2+2]))
				v := uint16(0)
				if a > b {
					v = 0xFFFF
				}
				binary.LittleEndian.PutUint16(c.XMM[reg][i*2:i*2+2], v)
			}
		case 0x66: // PCMPGTD
			for i := 0; i < 4; i++ {
				a := int32(c.getXMMU32(reg, i))
				b := int32(binary.LittleEndian.Uint32(src[i*4 : i*4+4]))
				if a > b {
					c.setXMMU32(reg, i, 0xFFFFFFFF)
				} else {
					c.setXMMU32(reg, i, 0)
				}
			}
		case 0xDB: // PAND
			for i := 0; i < 16; i++ {
				c.XMM[reg][i] &= src[i]
			}
		case 0xDF: // PANDN
			for i := 0; i < 16; i++ {
				c.XMM[reg][i] = (^c.XMM[reg][i]) & src[i]
			}
		case 0xEB: // POR
			for i := 0; i < 16; i++ {
				c.XMM[reg][i] |= src[i]
			}
		case 0xEF: // PXOR
			for i := 0; i < 16; i++ {
				c.XMM[reg][i] ^= src[i]
			}
		case 0xFC: // PADDB
			for i := 0; i < 16; i++ {
				c.XMM[reg][i] += src[i]
			}
		case 0xF8: // PSUBB
			for i := 0; i < 16; i++ {
				c.XMM[reg][i] -= src[i]
			}
		case 0xFD: // PADDW
			for i := 0; i < 8; i++ {
				a := binary.LittleEndian.Uint16(c.XMM[reg][i*2 : i*2+2])
				b := binary.LittleEndian.Uint16(src[i*2 : i*2+2])
				binary.LittleEndian.PutUint16(c.XMM[reg][i*2:i*2+2], a+b)
			}
		case 0xF9: // PSUBW
			for i := 0; i < 8; i++ {
				a := binary.LittleEndian.Uint16(c.XMM[reg][i*2 : i*2+2])
				b := binary.LittleEndian.Uint16(src[i*2 : i*2+2])
				binary.LittleEndian.PutUint16(c.XMM[reg][i*2:i*2+2], a-b)
			}
		case 0xD5: // PMULLW
			for i := 0; i < 8; i++ {
				a := int16(binary.LittleEndian.Uint16(c.XMM[reg][i*2 : i*2+2]))
				b := int16(binary.LittleEndian.Uint16(src[i*2 : i*2+2]))
				binary.LittleEndian.PutUint16(c.XMM[reg][i*2:i*2+2], uint16(a*b))
			}
		case 0xFE: // PADDD
			for i := 0; i < 4; i++ {
				a := c.getXMMU32(reg, i)
				b := binary.LittleEndian.Uint32(src[i*4 : i*4+4])
				c.setXMMU32(reg, i, a+b)
			}
		case 0xFA: // PSUBD
			for i := 0; i < 4; i++ {
				a := c.getXMMU32(reg, i)
				b := binary.LittleEndian.Uint32(src[i*4 : i*4+4])
				c.setXMMU32(reg, i, a-b)
			}
		case 0xD4: // PADDQ
			for i := 0; i < 2; i++ {
				a := c.getXMMU64(reg, i)
				b := binary.LittleEndian.Uint64(src[i*8 : i*8+8])
				c.setXMMU64(reg, i, a+b)
			}
		case 0xF4: // PMULUDQ (multiply unsigned 32-bit integers in low dwords of each 64-bit lane)
			for i := 0; i < 2; i++ {
				a := uint64(c.getXMMU32(reg, i*2))
				b := uint64(binary.LittleEndian.Uint32(src[i*8 : i*8+4]))
				c.setXMMU64(reg, i, a*b)
			}
		case 0xFB: // PSUBQ
			for i := 0; i < 2; i++ {
				a := c.getXMMU64(reg, i)
				b := binary.LittleEndian.Uint64(src[i*8 : i*8+8])
				c.setXMMU64(reg, i, a-b)
			}
		}
		return true

	case 0xD6: // MOVQ xmm2/m64, xmm1 (66 0F D6)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		low := c.getXMMU64(reg, 0)
		if (modrm >> 6) == 3 {
			rm := int(modrm & 7)
			clear(c.XMM[rm][:])
			c.setXMMU64(rm, 0, low)
		} else {
			addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
			c.Write64(addr, low)
		}
		return true

	case 0xD7: // PMOVMSKB
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		mask := uint32(0)
		for i := 0; i < 16; i++ {
			if (c.XMM[rm][i] & 0x80) != 0 {
				mask |= (1 << i)
			}
		}
		c.Regs[reg] = mask
		return true

	case 0xE6: // CVTDQ2PD (F3) / CVTTPD2DQ (66) / CVTPD2DQ (F2)
		if !repF3 && !repF2 && !opSize16 {
			return false
		}
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		src := c.readXMMOrMem128(modrm, segOverride, hasSeg)
		if repF3 { // CVTDQ2PD
			i0 := int32(binary.LittleEndian.Uint32(src[0:4]))
			i1 := int32(binary.LittleEndian.Uint32(src[4:8]))
			c.setXMMF64(reg, 0, float64(i0))
			c.setXMMF64(reg, 1, float64(i1))
		} else if opSize16 { // CVTTPD2DQ (truncates)
			f0 := math.Float64frombits(binary.LittleEndian.Uint64(src[0:8]))
			f1 := math.Float64frombits(binary.LittleEndian.Uint64(src[8:16]))
			clear(c.XMM[reg][:])
			c.setXMMU32(reg, 0, uint32(ToInt32(math.Trunc(f0))))
			c.setXMMU32(reg, 1, uint32(ToInt32(math.Trunc(f1))))
		} else { // F2: CVTPD2DQ (rounds)
			f0 := math.Float64frombits(binary.LittleEndian.Uint64(src[0:8]))
			f1 := math.Float64frombits(binary.LittleEndian.Uint64(src[8:16]))
			clear(c.XMM[reg][:])
			c.setXMMU32(reg, 0, uint32(ToInt32(math.RoundToEven(f0))))
			c.setXMMU32(reg, 1, uint32(ToInt32(math.RoundToEven(f1))))
		}
		return true
	}
	return false
}

func satS16ToS8(v int16) uint8 {
	if v > 127 {
		return 127
	}
	if v < -128 {
		return 0x80
	}
	return uint8(int8(v))
}

func satS16ToU8(v int16) uint8 {
	if v > 255 {
		return 255
	}
	if v < 0 {
		return 0
	}
	return uint8(v)
}

func satS32ToS16(v int32) uint16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return 0x8000
	}
	return uint16(int16(v))
}

func (c *CPU) DumpState() string {
	return fmt.Sprintf(
		"EIP=0x%08x EAX=0x%08x ECX=0x%08x EDX=0x%08x EBX=0x%08x ESP=0x%08x EBP=0x%08x ESI=0x%08x EDI=0x%08x FLAGS=0x%08x Steps=%d",
		c.EIP, c.Regs[0], c.Regs[1], c.Regs[2], c.Regs[3], c.Regs[4], c.Regs[5], c.Regs[6], c.Regs[7], c.Flags, c.Steps,
	)
}

// sseAdd and sseMul are SSE's addition and multiplication. When both operands are NaN, x86
// returns the first one (quieted); Go may evaluate a+b as b+a, so that case is explicit.
func sseAdd[T float32 | float64](a, b T) T {
	if a != a && b != b {
		return quietNaN(a)
	}
	return a + b
}

func sseMul[T float32 | float64](a, b T) T {
	if a != a && b != b {
		return quietNaN(a)
	}
	return a * b
}

// quietNaN sets NaN a's quiet bit, as an x86 arithmetic instruction does.
func quietNaN[T float32 | float64](a T) T {
	switch v := any(a).(type) {
	case float32:
		return T(math.Float32frombits(math.Float32bits(v) | 1<<22))
	case float64:
		return T(math.Float64frombits(math.Float64bits(v) | 1<<51))
	}
	return a
}

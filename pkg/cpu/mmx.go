package cpu

import (
	"fmt"
	"math"
)

// MMX: the 64-bit integer SIMD instructions (0F xx without a 66/F2/F3 prefix), and the
// conversions between MMX and SSE registers. Old audio code uses them (Miles' MP3 decoder and
// mixer plugins). MM registers are kept apart from the x87 stack: code using them ends with
// EMMS before touching x87 again, and nothing here reads one through the other.

// mmRM reads the r/m operand: an MM register, or 64 bits of memory.
func (c *CPU) mmRM(modrm uint8, seg uint32, hasSeg bool) uint64 {
	if modrm>>6 == 3 {
		return c.MM[modrm&7]
	}
	return c.Read64(c.calcModRMAddr(modrm, seg, hasSeg))
}

// lanes splits v into n-bit lanes; join does the reverse.
func lanes(v uint64, bitsPer uint) []uint64 {
	n := 64 / bitsPer
	out := make([]uint64, n)
	mask := uint64(1)<<bitsPer - 1
	for i := range out {
		out[i] = v >> (uint(i) * bitsPer) & mask
	}
	return out
}

func join(l []uint64, bitsPer uint) uint64 {
	var v uint64
	mask := uint64(1)<<bitsPer - 1
	for i, x := range l {
		v |= (x & mask) << (uint(i) * bitsPer)
	}
	return v
}

func signExt(x uint64, bitsPer uint) int64 { return int64(x<<(64-bitsPer)) >> (64 - bitsPer) }

func satS(v int64, bitsPer uint) uint64 {
	lo, hi := -(int64(1) << (bitsPer - 1)), int64(1)<<(bitsPer-1)-1
	return uint64(max(lo, min(hi, v)))
}

func satU(v int64, bitsPer uint) uint64 {
	return uint64(max(0, min(int64(1)<<bitsPer-1, v)))
}

// laneOp applies f to each pair of lanes.
func laneOp(a, b uint64, bitsPer uint, f func(x, y uint64) uint64) uint64 {
	la, lb := lanes(a, bitsPer), lanes(b, bitsPer)
	for i := range la {
		la[i] = f(la[i], lb[i])
	}
	return join(la, bitsPer)
}

// pack narrows the lanes of a then b (each of 2*bitsOut bits) with saturation.
func pack(a, b uint64, bitsOut uint, unsigned bool) uint64 {
	in := append(lanes(a, 2*bitsOut), lanes(b, 2*bitsOut)...)
	out := make([]uint64, len(in))
	for i, x := range in {
		if unsigned {
			out[i] = satU(signExt(x, 2*bitsOut), bitsOut)
		} else {
			out[i] = satS(signExt(x, 2*bitsOut), bitsOut)
		}
	}
	return join(out, bitsOut)
}

// unpack interleaves the low (or high) halves of a and b's lanes.
func unpack(a, b uint64, bitsPer uint, high bool) uint64 {
	la, lb := lanes(a, bitsPer), lanes(b, bitsPer)
	half := len(la) / 2
	off := 0
	if high {
		off = half
	}
	out := make([]uint64, 0, len(la))
	for i := 0; i < half; i++ {
		out = append(out, la[off+i], lb[off+i])
	}
	return join(out, bitsPer)
}

// shift shifts each lane: kind 0 logical right, 1 arithmetic right, 2 left.
func shift(v uint64, bitsPer uint, count uint64, kind int) uint64 {
	if bitsPer == 64 {
		switch {
		case count > 63 && kind != 1:
			return 0
		case kind == 0:
			return v >> count
		case kind == 2:
			return v << count
		}
	}
	return laneOp(v, 0, bitsPer, func(x, _ uint64) uint64 {
		switch kind {
		case 1:
			return uint64(signExt(x, bitsPer) >> min(count, uint64(bitsPer-1)))
		case 2:
			if count >= uint64(bitsPer) {
				return 0
			}
			return x << count
		}
		if count >= uint64(bitsPer) {
			return 0
		}
		return x >> count
	})
}

// execMMX runs 0F op2 as an MMX instruction (no operand-size or repeat prefix); false if it
// isn't one.
func (c *CPU) execMMX(op2 uint8, seg uint32, hasSeg bool) bool {
	switch op2 {
	case 0x77: // EMMS
		return true
	case 0x2A: // CVTPI2PS xmm, mm/m64
		modrm := c.Fetch8()
		v := c.mmRM(modrm, seg, hasSeg)
		reg := int(modrm>>3) & 7
		c.setXMMF32(reg, 0, float32(int32(v)))
		c.setXMMF32(reg, 1, float32(int32(v>>32)))
		return true
	case 0x2C, 0x2D: // CVTTPS2PI / CVTPS2PI mm, xmm/m64
		modrm := c.Fetch8()
		var a, b float32
		if modrm>>6 == 3 {
			a, b = c.getXMMF32(int(modrm&7), 0), c.getXMMF32(int(modrm&7), 1)
		} else {
			addr := c.calcModRMAddr(modrm, seg, hasSeg)
			a, b = c.ReadF32(addr), c.ReadF32(addr+4)
		}
		conv := math.RoundToEven
		if op2 == 0x2C {
			conv = math.Trunc
		}
		c.MM[(modrm>>3)&7] = uint64(uint32(ToInt32(conv(float64(a))))) | uint64(uint32(ToInt32(conv(float64(b)))))<<32
		return true
	}
	if !mmxOps[op2] && (op2 < 0x71 || op2 > 0x73) {
		return false
	}
	modrm := c.Fetch8()
	reg := (modrm >> 3) & 7
	switch op2 {
	case 0x6E: // MOVD mm, r/m32
		c.MM[reg] = uint64(c.readRM32(modrm, seg, hasSeg))
		return true
	case 0x7E: // MOVD r/m32, mm
		c.writeRM32(modrm, seg, hasSeg, uint32(c.MM[reg]))
		return true
	case 0x6F: // MOVQ mm, mm/m64
		c.MM[reg] = c.mmRM(modrm, seg, hasSeg)
		return true
	case 0x7F, 0xE7: // MOVQ mm/m64, mm; MOVNTQ
		if modrm>>6 == 3 {
			c.MM[modrm&7] = c.MM[reg]
		} else {
			c.Write64(c.calcModRMAddr(modrm, seg, hasSeg), c.MM[reg])
		}
		return true
	case 0x71, 0x72, 0x73: // shifts by an immediate: /2 right logical, /4 right arithmetic, /6 left
		count := uint64(c.Fetch8())
		bitsPer := map[uint8]uint{0x71: 16, 0x72: 32, 0x73: 64}[op2]
		kind := map[uint8]int{2: 0, 4: 1, 6: 2}[reg]
		if reg != 2 && reg != 4 && reg != 6 || (op2 == 0x73 && reg == 4) || modrm>>6 != 3 {
			c.Halted, c.HaltReason = true, fmt.Sprintf("unimplemented MMX shift 0F %02X /%d at EIP=0x%08x", op2, reg, c.EIP)
			return true
		}
		c.MM[modrm&7] = shift(c.MM[modrm&7], bitsPer, count, kind)
		return true
	case 0x70: // PSHUFW mm, mm/m64, imm8
		src := c.mmRM(modrm, seg, hasSeg)
		imm := c.Fetch8()
		l := lanes(src, 16)
		out := make([]uint64, 4)
		for i := range out {
			out[i] = l[(imm>>(2*uint(i)))&3]
		}
		c.MM[reg] = join(out, 16)
		return true
	case 0xC4: // PINSRW mm, r32/m16, imm8
		var v uint64
		if modrm>>6 == 3 {
			v = uint64(c.Regs[modrm&7] & 0xffff)
		} else {
			v = uint64(c.Read16(c.calcModRMAddr(modrm, seg, hasSeg)))
		}
		i := uint(c.Fetch8() & 3)
		c.MM[reg] = c.MM[reg]&^(0xffff<<(16*i)) | v<<(16*i)
		return true
	case 0xC5: // PEXTRW r32, mm, imm8
		i := uint(c.Fetch8() & 3)
		c.Regs[reg] = uint32(c.MM[modrm&7] >> (16 * i) & 0xffff)
		return true
	case 0xD7: // PMOVMSKB r32, mm
		var m uint32
		for i, b := range lanes(c.MM[modrm&7], 8) {
			m |= uint32(b>>7) << uint(i)
		}
		c.Regs[reg] = m
		return true
	case 0xF7: // MASKMOVQ mm, mm: to [EDI] the bytes whose mask byte's top bit is set
		src, mask := lanes(c.MM[reg], 8), lanes(c.MM[modrm&7], 8)
		for i := range src {
			if mask[i]&0x80 != 0 {
				c.Write8(c.Regs[RegEDI]+uint32(i), uint8(src[i]))
			}
		}
		return true
	}
	a, b := c.MM[reg], c.mmRM(modrm, seg, hasSeg)
	var r uint64
	add := func(bitsPer uint, sat int) uint64 { // sat 0 wrap, 1 signed, 2 unsigned
		return laneOp(a, b, bitsPer, func(x, y uint64) uint64 {
			switch sat {
			case 1:
				return satS(signExt(x, bitsPer)+signExt(y, bitsPer), bitsPer)
			case 2:
				return satU(int64(x+y), bitsPer)
			}
			return x + y
		})
	}
	sub := func(bitsPer uint, sat int) uint64 {
		return laneOp(a, b, bitsPer, func(x, y uint64) uint64 {
			switch sat {
			case 1:
				return satS(signExt(x, bitsPer)-signExt(y, bitsPer), bitsPer)
			case 2:
				return satU(int64(x)-int64(y), bitsPer)
			}
			return x - y
		})
	}
	cmp := func(bitsPer uint, gt bool) uint64 {
		return laneOp(a, b, bitsPer, func(x, y uint64) uint64 {
			if gt && signExt(x, bitsPer) > signExt(y, bitsPer) || !gt && x == y {
				return ^uint64(0)
			}
			return 0
		})
	}
	switch op2 {
	case 0x60, 0x61, 0x62:
		r = unpack(a, b, 8<<(op2-0x60), false)
	case 0x68, 0x69, 0x6A:
		r = unpack(a, b, 8<<(op2-0x68), true)
	case 0x63:
		r = pack(a, b, 8, false)
	case 0x6B:
		r = pack(a, b, 16, false)
	case 0x67:
		r = pack(a, b, 8, true)
	case 0x64, 0x65, 0x66:
		r = cmp(8<<(op2-0x64), true)
	case 0x74, 0x75, 0x76:
		r = cmp(8<<(op2-0x74), false)
	case 0xD1, 0xD2, 0xD3:
		r = shift(a, 8<<(op2-0xD0), b, 0)
	case 0xE1, 0xE2:
		r = shift(a, 8<<(op2-0xE0), b, 1)
	case 0xF1, 0xF2, 0xF3:
		r = shift(a, 8<<(op2-0xF0), b, 2)
	case 0xD4:
		r = a + b
	case 0xFB:
		r = a - b
	case 0xFC, 0xFD, 0xFE:
		r = add(8<<(op2-0xFC), 0)
	case 0xEC, 0xED:
		r = add(8<<(op2-0xEC), 1)
	case 0xDC, 0xDD:
		r = add(8<<(op2-0xDC), 2)
	case 0xF8, 0xF9, 0xFA:
		r = sub(8<<(op2-0xF8), 0)
	case 0xE8, 0xE9:
		r = sub(8<<(op2-0xE8), 1)
	case 0xD8, 0xD9:
		r = sub(8<<(op2-0xD8), 2)
	case 0xD5: // PMULLW
		r = laneOp(a, b, 16, func(x, y uint64) uint64 { return uint64(signExt(x, 16) * signExt(y, 16)) })
	case 0xE5: // PMULHW
		r = laneOp(a, b, 16, func(x, y uint64) uint64 { return uint64(signExt(x, 16)*signExt(y, 16)) >> 16 })
	case 0xE4: // PMULHUW
		r = laneOp(a, b, 16, func(x, y uint64) uint64 { return x * y >> 16 })
	case 0xF4: // PMULUDQ
		r = (a & 0xffffffff) * (b & 0xffffffff)
	case 0xF5: // PMADDWD
		la, lb := lanes(a, 16), lanes(b, 16)
		lo := signExt(la[0], 16)*signExt(lb[0], 16) + signExt(la[1], 16)*signExt(lb[1], 16)
		hi := signExt(la[2], 16)*signExt(lb[2], 16) + signExt(la[3], 16)*signExt(lb[3], 16)
		r = uint64(uint32(lo)) | uint64(uint32(hi))<<32
	case 0xDB:
		r = a & b
	case 0xDF:
		r = ^a & b
	case 0xEB:
		r = a | b
	case 0xEF:
		r = a ^ b
	case 0xDA:
		r = laneOp(a, b, 8, func(x, y uint64) uint64 { return min(x, y) })
	case 0xDE:
		r = laneOp(a, b, 8, func(x, y uint64) uint64 { return max(x, y) })
	case 0xEA:
		r = laneOp(a, b, 16, func(x, y uint64) uint64 { return uint64(min(signExt(x, 16), signExt(y, 16))) })
	case 0xEE:
		r = laneOp(a, b, 16, func(x, y uint64) uint64 { return uint64(max(signExt(x, 16), signExt(y, 16))) })
	case 0xE0:
		r = laneOp(a, b, 8, func(x, y uint64) uint64 { return (x + y + 1) >> 1 })
	case 0xE3:
		r = laneOp(a, b, 16, func(x, y uint64) uint64 { return (x + y + 1) >> 1 })
	case 0xF6: // PSADBW
		var s uint64
		for i, x := range lanes(a, 8) {
			y := lanes(b, 8)[i]
			s += uint64(max(int64(x)-int64(y), int64(y)-int64(x)))
		}
		r = s
	default:
		c.Halted, c.HaltReason = true, fmt.Sprintf("unimplemented MMX instruction 0F %02X at EIP=0x%08x", op2, c.EIP)
		return true
	}
	c.MM[reg] = r
	return true
}

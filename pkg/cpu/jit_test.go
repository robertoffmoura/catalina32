package cpu

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Translated code must do exactly what the interpreter does. Random programs (every form the
// decoded-instruction cache handles, register and memory operands, 8-bit high registers, fused
// compare-and-branch, calls) run in a loop, so their blocks get translated and chained part way;
// registers, flags, step counts, halts and memory must match an interpreted run.

const (
	jtCode  = 0x00100000
	jtData  = 0x00200000 // EBP
	jtArray = 0x00201000 // ESI
	jtStack = 0x00300000
	jtSub   = 0x00180000 // a subroutine: add eax, 1; ret
	jtThunk = 0x00180040 // a host function (intercept): changes EAX, EDX and memory, then returns
	jtSub2  = 0x00180080 // add eax, [esp+4]; ret 4
)

// jtGen writes random guest instructions.
type jtGen struct {
	r     *rand.Rand
	depth int // values pushed and not popped yet (the loop must leave ESP as it found it)
}

// Destination registers: not ESP, EBP, ESI (the stack and the two pointers).
var jtDst = []uint8{RegEAX, RegECX, RegEDX, RegEBX, RegEDI}

func (g *jtGen) dst() uint8 { return jtDst[g.r.Intn(len(jtDst))] }
func (g *jtGen) any() uint8 { return uint8(g.r.Intn(8)) }

// modrm returns a ModRM (and SIB/displacement) for reg and a register or memory operand. Memory
// operands are [ebp+d8], [esi+ebx*4+d8] (EBX may have become anything: out-of-range accesses are
// part of the test), [esp+d8] for loads, or an absolute address.
func (g *jtGen) modrm(reg uint8, regRM uint8, memOK bool, store bool) []byte {
	if !memOK || g.r.Intn(3) == 0 {
		return []byte{0xC0 | reg<<3 | regRM}
	}
	switch g.r.Intn(5) {
	case 0, 1:
		return []byte{0x45 | reg<<3, byte(g.r.Intn(0x60) &^ 3)} // [ebp+d8]
	case 2:
		return []byte{0x44 | reg<<3, 0x9E, byte(g.r.Intn(0x40))} // [esi+ebx*4+d8]
	case 3:
		if store {
			return []byte{0x45 | reg<<3, byte(g.r.Intn(0x60))}
		}
		return []byte{0x44 | reg<<3, 0x24, byte(g.r.Intn(4) * 4)} // [esp+d8]
	default:
		a := make([]byte, 5)
		a[0] = 0x05 | reg<<3 // [disp32]
		binary.LittleEndian.PutUint32(a[1:], jtData+uint32(g.r.Intn(0x60)))
		return a
	}
}

func (g *jtGen) imm32() []byte {
	v := g.r.Uint32()
	if g.r.Intn(2) == 0 {
		v = uint32(g.r.Intn(9)) - 4
	}
	return binary.LittleEndian.AppendUint32(nil, v)
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// inst returns one random instruction that doesn't transfer control.
func (g *jtGen) inst() []byte {
	r := g.r
	op := uint8(r.Intn(8))
	switch r.Intn(54) {
	case 0: // op r/m32, r32
		return cat([]byte{op<<3 | 1}, g.modrm(g.any(), g.dst(), true, op != 7))
	case 1: // op r32, r/m32
		return cat([]byte{op<<3 | 3}, g.modrm(g.dst(), g.any(), true, false))
	case 2: // op eax, imm32
		return cat([]byte{op<<3 | 5}, g.imm32())
	case 3: // 81 /op imm32
		return cat([]byte{0x81}, g.modrm(op, g.dst(), true, op != 7), g.imm32())
	case 4: // 83 /op imm8
		return cat([]byte{0x83}, g.modrm(op, g.dst(), true, op != 7), []byte{byte(r.Intn(256))})
	case 5: // 80 /op imm8 (8-bit, AH..BH included)
		return cat([]byte{0x80}, g.modrm(op, uint8(r.Intn(8)), true, op != 7), []byte{byte(r.Intn(256))})
	case 6: // test r/m32, r32
		return cat([]byte{0x85}, g.modrm(g.any(), g.any(), true, false))
	case 7: // test r/m8, r8
		return cat([]byte{0x84}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 8: // test r/m8, imm8
		return cat([]byte{0xF6}, g.modrm(0, uint8(r.Intn(8)), true, false), []byte{byte(r.Intn(256))})
	case 9: // test al, imm8
		return []byte{0xA8, byte(r.Intn(256))}
	case 10: // lea
		m := g.modrm(g.dst(), 0, true, false)
		if m[0]>>6 == 3 {
			m = []byte{0x45 | g.dst()<<3, byte(r.Intn(256))}
		}
		return cat([]byte{0x8D}, m)
	case 11: // inc / dec r32
		return []byte{0x40 + byte(r.Intn(2))*8 + g.dst()}
	case 12: // push r32 / pop r32
		if g.depth == 0 || r.Intn(2) == 0 {
			g.depth++
			return []byte{0x50 + g.any()}
		}
		g.depth--
		return []byte{0x58 + g.dst()}
	case 13: // movzx / movsx
		ops := []byte{0xB6, 0xB7, 0xBE, 0xBF}
		o := ops[r.Intn(4)]
		rm := g.any()
		if o == 0xB6 || o == 0xBE {
			rm = uint8(r.Intn(8)) // AL..BH
		}
		return cat([]byte{0x0F, o}, g.modrm(g.dst(), rm, true, false))
	case 14: // cmovcc
		return cat([]byte{0x0F, 0x40 | byte(r.Intn(16))}, g.modrm(g.dst(), g.any(), true, false))
	case 15: // movss / movsd load and store
		p := []byte{0xF3, 0xF2}[r.Intn(2)]
		o := byte(0x10 + r.Intn(2))
		return cat([]byte{p, 0x0F, o}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, o == 0x11))
	case 16: // addss / mulss / subss / divss
		o := []byte{0x58, 0x59, 0x5C, 0x5E}[r.Intn(4)]
		return cat([]byte{0xF3, 0x0F, o}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 17: // movaps / movups load and store
		o := []byte{0x28, 0x10, 0x29, 0x11}[r.Intn(4)]
		return cat([]byte{0x0F, o}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, o == 0x29 || o == 0x11))
	case 18: // ucomiss / comiss
		return cat([]byte{0x0F, byte(0x2E + r.Intn(2))}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 19: // xorps
		return cat([]byte{0x0F, 0x57}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 20: // nops
		if r.Intn(2) == 0 {
			return []byte{0x90}
		}
		return []byte{0x0F, 0x1F, 0x44, 0x00, 0x00}
	case 21: // mov r32, imm32
		return cat([]byte{0xB8 + g.dst()}, g.imm32())
	case 22: // shift by imm8 (counts 0..40) or 1
		if r.Intn(3) == 0 {
			return cat([]byte{0xD1}, g.modrm(op, g.dst(), true, true))
		}
		return cat([]byte{0xC1}, g.modrm(op, g.dst(), true, true), []byte{byte(r.Intn(41))})
	case 23: // shift by CL
		return cat([]byte{0xD3}, g.modrm(op, g.dst(), true, true))
	case 24: // imul r32, r/m32, imm
		if r.Intn(2) == 0 {
			return cat([]byte{0x6B}, g.modrm(g.dst(), g.any(), true, false), []byte{byte(r.Intn(256))})
		}
		return cat([]byte{0x69}, g.modrm(g.dst(), g.any(), true, false), g.imm32())
	case 25: // imul r32, r/m32
		return cat([]byte{0x0F, 0xAF}, g.modrm(g.dst(), g.any(), true, false))
	case 26: // mov r/m8, r8 / mov r8, r/m8
		if r.Intn(2) == 0 {
			return cat([]byte{0x88}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, true))
		}
		return cat([]byte{0x8A}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 27: // mov r/m8, imm8
		return cat([]byte{0xC6}, g.modrm(0, uint8(r.Intn(8)), true, true), []byte{byte(r.Intn(256))})
	case 28: // inc / dec r/m32
		return cat([]byte{0xFF}, g.modrm(uint8(r.Intn(2)), g.dst(), true, true))
	case 29: // push r/m32
		g.depth++
		return cat([]byte{0xFF}, g.modrm(6, g.any(), true, false))
	case 30: // not / neg
		return cat([]byte{0xF7}, g.modrm(uint8(2+r.Intn(2)), g.dst(), true, true))
	case 31: // setcc
		return cat([]byte{0x0F, 0x90 | byte(r.Intn(16))}, g.modrm(0, uint8(r.Intn(8)), true, true))
	case 32: // mov r/m32, r32 / mov r32, r/m32
		if r.Intn(2) == 0 {
			return cat([]byte{0x89}, g.modrm(g.any(), g.dst(), true, true))
		}
		return cat([]byte{0x8B}, g.modrm(g.dst(), g.any(), true, false))
	case 33: // mov r/m32, imm32
		return cat([]byte{0xC7}, g.modrm(0, g.dst(), true, true), g.imm32())
	case 34: // an untranslated form between translated ones: cdq
		return []byte{0x99}
	case 35: // 8-bit ALU: op r/m8, r8 / op r8, r/m8 / op al, imm8
		switch r.Intn(3) {
		case 0:
			return cat([]byte{op << 3}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, op != 7))
		case 1:
			return cat([]byte{op<<3 | 2}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
		}
		return []byte{op<<3 | 4, byte(r.Intn(256))}
	case 36: // mov r8, imm8; not / neg r/m8
		if r.Intn(2) == 0 {
			return []byte{0xB0 + byte(r.Intn(8)), byte(r.Intn(256))}
		}
		return cat([]byte{0xF6}, g.modrm(uint8(2+r.Intn(2)), uint8(r.Intn(8)), true, true))
	case 37: // 16-bit mov
		switch r.Intn(3) {
		case 0:
			return cat([]byte{0x66, 0x89}, g.modrm(g.any(), g.dst(), true, true))
		case 1:
			return cat([]byte{0x66, 0x8B}, g.modrm(g.dst(), g.any(), true, false))
		}
		return cat([]byte{0x66, 0xC7}, g.modrm(0, g.dst(), true, true), []byte{byte(r.Intn(256)), byte(r.Intn(256))})
	case 38: // 16-bit ALU and test
		switch r.Intn(5) {
		case 0:
			return cat([]byte{0x66, op<<3 | 1}, g.modrm(g.any(), g.dst(), true, op != 7))
		case 1:
			return cat([]byte{0x66, op<<3 | 3}, g.modrm(g.dst(), g.any(), true, false))
		case 2:
			return cat([]byte{0x66, 0x81}, g.modrm(op, g.dst(), true, op != 7), []byte{byte(r.Intn(256)), byte(r.Intn(256))})
		case 3:
			return cat([]byte{0x66, 0x83}, g.modrm(op, g.dst(), true, op != 7), []byte{byte(r.Intn(256))})
		}
		return cat([]byte{0x66, 0x85}, g.modrm(g.any(), g.any(), true, false))
	case 39: // sqrtss / minss / maxss, scalar double arithmetic
		if r.Intn(2) == 0 {
			o := []byte{0x51, 0x5D, 0x5F}[r.Intn(3)]
			return cat([]byte{0xF3, 0x0F, o}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
		}
		o := []byte{0x51, 0x58, 0x59, 0x5C, 0x5D, 0x5E, 0x5F}[r.Intn(7)]
		return cat([]byte{0xF2, 0x0F, o}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 40: // packed single arithmetic and logic
		o := []byte{0x51, 0x58, 0x59, 0x5C, 0x5D, 0x5E, 0x5F, 0x54, 0x55, 0x56}[r.Intn(10)]
		return cat([]byte{0x0F, o}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 41: // conversions between single and double
		return cat([]byte{[]byte{0xF3, 0xF2}[r.Intn(2)], 0x0F, 0x5A}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 42: // float to int (truncating)
		return cat([]byte{[]byte{0xF3, 0xF2}[r.Intn(2)], 0x0F, 0x2C}, g.modrm(g.dst(), uint8(r.Intn(8)), true, false))
	case 43: // int to float
		return cat([]byte{[]byte{0xF3, 0xF2}[r.Intn(2)], 0x0F, 0x2A}, g.modrm(uint8(r.Intn(8)), g.any(), true, false))
	case 44: // ucomisd / comisd
		return cat([]byte{0x66, 0x0F, byte(0x2E + r.Intn(2))}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false))
	case 45: // pshufd
		return cat([]byte{0x66, 0x0F, 0x70}, g.modrm(uint8(r.Intn(8)), uint8(r.Intn(8)), true, false), []byte{byte(r.Intn(256))})
	case 47: // lock cmpxchg8b m64 (as often equal as not: EDX:EAX is set from memory first)
		m := g.modrm(1, 0, true, true)
		if m[0]>>6 == 3 {
			m = []byte{0x4D, byte(r.Intn(0x58))}
		}
		return cat([]byte{0xF0, 0x0F, 0xC7}, m)
	case 48: // bswap
		return []byte{0x0F, 0xC8 + g.dst()}
	case 46: // movd xmm, r/m32 / movd r/m32, xmm
		if r.Intn(2) == 0 {
			return cat([]byte{0x66, 0x0F, 0x6E}, g.modrm(uint8(r.Intn(8)), g.any(), true, false))
		}
		return cat([]byte{0x66, 0x0F, 0x7E}, g.modrm(uint8(r.Intn(8)), g.dst(), true, true))
	default: // compare or test, often followed by a Jcc (fused)
		return g.compare()
	}
}

func (g *jtGen) compare() []byte {
	r := g.r
	switch r.Intn(6) {
	case 0:
		return cat([]byte{0x39}, g.modrm(g.any(), g.any(), true, false))
	case 1:
		return cat([]byte{0x3B}, g.modrm(g.any(), g.any(), true, false))
	case 2:
		return cat([]byte{0x83}, g.modrm(7, g.any(), true, false), []byte{byte(r.Intn(256))})
	case 3:
		return cat([]byte{0x3D}, g.imm32())
	case 4:
		return cat([]byte{0x85}, g.modrm(g.any(), g.any(), true, false))
	default:
		return cat([]byte{0x80}, g.modrm(7, uint8(r.Intn(8)), true, false), []byte{byte(r.Intn(256))})
	}
}

// program returns a loop: count iterations of a random body with forward branches and calls.
func (g *jtGen) program(n int) []byte {
	r := g.r
	var items [][]byte
	var skips []int // per item: -1, or a Jcc after it over this many items
	for i := 0; i < n; i++ {
		items = append(items, g.inst())
		skips = append(skips, -1)
		switch r.Intn(8) {
		case 0, 1: // a Jcc over the next 0..3 items
			skips[len(skips)-1] = r.Intn(4)
		case 2: // call the subroutine or the host function (rel32 set at layout)
			items = append(items, []byte{0xE8, byte(r.Intn(2)), 0, 0, 0})
			skips = append(skips, -1)
		case 3: // call [ebp+0x78] (the subroutine's address)
			if r.Intn(4) == 0 {
				items = append(items, []byte{0xFF, 0x55, 0x78})
				skips = append(skips, -1)
			}
		case 4: // push an argument, call a function returning with ret 4
			if r.Intn(2) == 0 {
				items = append(items, []byte{0x50 + g.any()}, []byte{0xE8, 2, 0, 0, 0})
				skips = append(skips, -1, -1)
			}
		}
	}
	for ; g.depth > 0; g.depth-- {
		items = append(items, []byte{0x58 + g.dst()})
		skips = append(skips, -1)
	}
	// Layout: each item, then its Jcc (2 bytes) if any.
	var out []byte
	for i, it := range items {
		at := len(out)
		out = append(out, it...)
		if it[0] == 0xE8 && len(it) == 5 {
			to := []uint32{jtSub, jtThunk, jtSub2}[it[1]]
			binary.LittleEndian.PutUint32(out[at+1:], to-(jtCode+uint32(at)+5))
		}
		if k := skips[i]; k >= 0 {
			skip := 0
			for j := i + 1; j <= i+k && j < len(items); j++ {
				skip += len(items[j])
				if skips[j] >= 0 {
					skip += 2
				}
			}
			if skip > 127 {
				skip = 0
			}
			out = append(out, 0x70|byte(r.Intn(16)), byte(skip))
		}
	}
	// dec dword [ebp+0x7C]; jnz top; ret
	out = append(out, 0xFF, 0x4D, 0x7C)
	out = append(out, 0x0F, 0x85)
	out = binary.LittleEndian.AppendUint32(out, uint32(-(len(out) + 4)))
	return append(out, 0xC3)
}

type jtState struct {
	regs   [8]uint32
	flags  uint32
	xmm    [8][16]byte
	data   []byte
	stack  []byte
	code   []byte
	eip    uint32
	steps  uint64
	halted string
	stats  JITStats
}

func jtRun(t *testing.T, prog []byte, seed int64, jit bool, maxSteps uint64) jtState {
	r := rand.New(rand.NewSource(seed))
	c := NewCPU()
	c.NoJIT = !jit
	copy(c.Mem[jtCode:], prog)
	copy(c.Mem[jtSub:], []byte{0x83, 0xC0, 0x01, 0xC3})              // add eax, 1; ret
	copy(c.Mem[jtSub2:], []byte{0x03, 0x44, 0x24, 0x04, 0xC2, 4, 0}) // add eax, [esp+4]; ret 4
	c.SetCodeRange(jtCode, jtSub+0x100)
	if seed%2 == 1 { // translated code hands stores to the data area to the interpreter
		c.jitSlowPages = [][2]uint32{{jtData, jtData + 0x1000}}
	}
	c.RegisterIntercept(jtThunk, "thunk", func(c *CPU) {
		c.Regs[RegEAX] = c.Regs[RegEAX]*3 + c.Regs[RegECX]
		c.Regs[RegEDX] ^= uint32(c.Steps)
		c.Write32(jtData+0x60, c.Read32(jtData+0x60)+c.Regs[RegEAX])
		c.Ret()
	})
	for i := range c.Regs {
		c.Regs[i] = r.Uint32()
		if r.Intn(3) == 0 {
			c.Regs[i] = uint32(r.Intn(9)) - 4
		}
	}
	c.Regs[RegEBX] = uint32(r.Intn(16))
	c.Regs[RegEBP] = jtData
	c.Regs[RegESI] = jtArray
	c.Regs[RegESP] = jtStack
	c.Flags = 2 | uint32(r.Intn(0x1000))&(FlagCF|FlagPF|FlagAF|FlagZF|FlagSF|FlagOF)
	for i := range c.XMM {
		for k := 0; k < 4; k++ {
			v := math.Float32bits(float32(r.NormFloat64() * 100))
			switch r.Intn(12) {
			case 0:
				v = 0x7FC00000 // NaN
			case 1:
				v = 0
			case 2:
				v = r.Uint32()
			case 3: // quiet NaN with a payload
				v = 0x7FC00000 | r.Uint32()&0x803FFFFF
			case 4: // signaling NaN
				v = 0x7F800001 | r.Uint32()&0x803FFFFF
			}
			binary.LittleEndian.PutUint32(c.XMM[i][4*k:], v)
		}
		if r.Intn(4) == 0 { // a double NaN, quiet or signaling
			v := uint64(0x7FF0000000000001) | r.Uint64()&0x800FFFFFFFFFFFFF
			if r.Intn(2) == 0 {
				v |= 1 << 51
			}
			binary.LittleEndian.PutUint64(c.XMM[i][8*r.Intn(2):], v)
		}
	}
	for i := uint32(0); i < 0x2000; i++ {
		c.Mem[jtData+i] = byte(r.Intn(256))
	}
	c.Write32(jtData+0x78, jtSub)
	c.Write32(jtData+0x7C, 6) // iterations
	c.Push32(SentinelReturnEIP)
	c.EIP = jtCode
	c.MaxSteps = maxSteps
	c.RunUntil(SentinelReturnEIP)
	st := jtState{regs: c.Regs, flags: c.Flags, xmm: c.XMM, eip: c.EIP, steps: c.Steps,
		data: append([]byte(nil), c.Mem[jtData:jtData+0x2000]...), stack: append([]byte(nil), c.Mem[jtStack-0x400:jtStack]...),
		code: append([]byte(nil), c.Mem[jtCode:jtCode+uint32(len(prog))]...)}
	if c.Halted {
		st.halted = c.HaltReason
	}
	st.stats = c.JITStats()
	c.Close()
	return st
}

func (s jtState) diff(o jtState) string {
	var d []string
	if s.regs != o.regs {
		d = append(d, fmt.Sprintf("regs %08x vs %08x", s.regs, o.regs))
	}
	if s.flags != o.flags {
		d = append(d, fmt.Sprintf("flags %#x vs %#x", s.flags, o.flags))
	}
	if s.eip != o.eip || s.steps != o.steps {
		d = append(d, fmt.Sprintf("eip %#x steps %d vs eip %#x steps %d", s.eip, s.steps, o.eip, o.steps))
	}
	if s.halted != o.halted {
		d = append(d, fmt.Sprintf("halt %q vs %q", s.halted, o.halted))
	}
	if s.xmm != o.xmm {
		d = append(d, fmt.Sprintf("xmm %x vs %x", s.xmm, o.xmm))
	}
	for _, m := range []struct {
		name string
		a, b []byte
		at   uint32
	}{{"data", s.data, o.data, jtData}, {"stack", s.stack, o.stack, jtStack - 0x400}, {"code", s.code, o.code, jtCode}} {
		for i := range m.a {
			if m.a[i] != m.b[i] {
				d = append(d, fmt.Sprintf("%s at %#x: %#x vs %#x", m.name, m.at+uint32(i), m.a[i], m.b[i]))
				break
			}
		}
	}
	return strings.Join(d, "; ")
}

func TestJITMatchesInterpreter(t *testing.T) {
	if !jitSupported || jitDisabled {
		t.Skip("no JIT on this host (or C32_NO_JIT=1; where it is opt-in, C32_JIT=1 enables it)")
	}
	defer func(v uint32) { jitThreshold = v }(jitThreshold)
	jitThreshold = 2
	n := 3000
	if testing.Short() {
		n = 300
	}
	if v, err := strconv.Atoi(os.Getenv("C32_JITN")); err == nil { // more programs
		n = v
	}
	var total JITStats
	halts := 0
	for seed := int64(0); seed < int64(n); seed++ {
		g := &jtGen{r: rand.New(rand.NewSource(seed))}
		prog := g.program(5 + g.r.Intn(30))
		a := jtRun(t, prog, seed, false, 20000)
		b := jtRun(t, prog, seed, true, 20000)
		if d := a.diff(b); d != "" {
			t.Fatalf("seed %d, program % x:\ninterpreter vs JIT: %s", seed, prog, d)
		}
		total.Translated += b.stats.Translated
		total.Entries += b.stats.Entries
		total.SlowExits += b.stats.SlowExits
		if a.halted != "" {
			halts++
		}
	}
	t.Logf("%d programs (%d halted): %d blocks translated, %d entries, %d slow exits", n, halts, total.Translated, total.Entries, total.SlowExits)
	if total.Translated < uint64(n) || total.Entries < uint64(n) {
		t.Errorf("translated code hardly ran")
	}
}

// C32_JITSEED=<n> go test ./pkg/cpu -run JITBisect -v: the first step at which seed n's program
// differs between the interpreter and the JIT (debugging TestJITMatchesInterpreter failures).
func TestJITBisect(t *testing.T) {
	v := os.Getenv("C32_JITSEED")
	if v == "" {
		t.Skip("C32_JITSEED not set")
	}
	seed, _ := strconv.ParseInt(v, 10, 64)
	defer func(v uint32) { jitThreshold = v }(jitThreshold)
	jitThreshold = 2
	g := &jtGen{r: rand.New(rand.NewSource(seed))}
	prog := g.program(5 + g.r.Intn(30))
	for n := uint64(1); n < 20000; n++ {
		a := jtRun(t, prog, seed, false, n)
		b := jtRun(t, prog, seed, true, n)
		if d := a.diff(b); d != "" && !strings.HasPrefix(d, "eip") {
			c := jtRun(t, prog, seed, false, n-1)
			t.Fatalf("first difference after %d steps (before it: eip %#x, regs %08x flags %#x):\n%s", n, c.eip, c.regs, c.flags, d)
		}
	}
}

package cpu

import (
	"encoding/binary"
	"fmt"
)

// A small x86-64 encoder for the JIT's amd64 backend (jit_amd64.go). It lives in a file without
// a build constraint so its tests run everywhere; only the backend uses it.

// Host registers.
const (
	hRAX = iota
	hRCX
	hRDX
	hRBX
	hRSP
	hRBP
	hRSI
	hRDI
	hR8
	hR9
	hR10
	hR11
	hR12
	hR13
	hR14
	hR15
)

// xop is an r/m operand: a register (reg >= 0) or memory [base + index<<scale + disp].
type xop struct {
	reg         int
	base, index int // -1: none
	scale       uint8
	disp        int32
}

func xr(r int) xop                { return xop{reg: r, base: -1, index: -1} }
func xm(base int, disp int32) xop { return xop{reg: -1, base: base, index: -1, disp: disp} }
func xmi(base, index int, scale uint8, disp int32) xop {
	return xop{reg: -1, base: base, index: index, scale: scale, disp: disp}
}

type x64 struct {
	b      []byte
	labels []int // label -> offset (-1: unbound)
	fixups []x64fixup
}

type x64fixup struct{ at, label int } // rel32 at b[at:at+4], relative to at+4

func (a *x64) emit(bs ...byte) { a.b = append(a.b, bs...) }

func (a *x64) emit32(v uint32) { a.b = binary.LittleEndian.AppendUint32(a.b, v) }

func (a *x64) emit16(v uint16) { a.b = binary.LittleEndian.AppendUint16(a.b, v) }

func (a *x64) emit64(v uint64) { a.b = binary.LittleEndian.AppendUint64(a.b, v) }

func (a *x64) newLabel() int {
	a.labels = append(a.labels, -1)
	return len(a.labels) - 1
}

func (a *x64) bind(l int) { a.labels[l] = len(a.b) }

// resolve patches every rel32 to its label.
func (a *x64) resolve() {
	for _, f := range a.fixups {
		to := a.labels[f.label]
		if to < 0 {
			panic("x64: unbound label")
		}
		binary.LittleEndian.PutUint32(a.b[f.at:], uint32(int32(to-(f.at+4))))
	}
	a.fixups = a.fixups[:0]
}

func (a *x64) rel32(l int) {
	a.fixups = append(a.fixups, x64fixup{len(a.b), l})
	a.emit32(0)
}

// Operand-size flags for inst.
const (
	xW    = 1 << iota // REX.W: 64-bit operand
	xByte             // byte operands: registers 4-7 are SPL..DIL (needs REX), never AH..BH
	xHigh             // byte operands with no REX: registers 4-7 are AH..BH (legacy high bytes)
)

// inst encodes [prefix] [REX] opcode ModRM [SIB] [disp] for register reg (or an opcode
// extension) and r/m operand rm.
func (a *x64) inst(prefix byte, flags int, opcode []byte, reg int, rm xop) {
	rex := byte(0)
	if flags&xW != 0 {
		rex |= 0x48
	}
	if reg >= 8 {
		rex |= 0x44
	}
	if rm.reg >= 8 || rm.base >= 8 {
		rex |= 0x41
	}
	if rm.index >= 8 {
		rex |= 0x42
	}
	if flags&xByte != 0 && ((reg >= 4 && reg < 8) || (rm.reg >= 4 && rm.reg < 8)) {
		rex |= 0x40
	}
	if flags&xHigh != 0 && rex != 0 {
		panic(fmt.Sprintf("x64: high byte register with a REX prefix (reg %d, rm %+v)", reg, rm))
	}
	if prefix != 0 {
		a.emit(prefix)
	}
	if rex != 0 {
		a.emit(rex)
	}
	a.emit(opcode...)
	r := byte(reg&7) << 3
	if rm.reg >= 0 {
		a.emit(0xC0 | r | byte(rm.reg&7))
		return
	}
	mod := byte(0)
	switch {
	case rm.base < 0:
		// no base: SIB with base 101 and mod 00 means disp32
	case rm.disp == 0 && rm.base&7 != 5:
	case rm.disp >= -128 && rm.disp <= 127:
		mod = 1
	default:
		mod = 2
	}
	if rm.index < 0 && rm.base >= 0 && rm.base&7 != 4 {
		a.emit(mod<<6 | r | byte(rm.base&7))
	} else {
		idx := byte(4) // none
		if rm.index >= 0 {
			if rm.index == hRSP {
				panic("x64: rsp can't be an index")
			}
			idx = byte(rm.index & 7)
		}
		base := byte(5)
		if rm.base >= 0 {
			base = byte(rm.base & 7)
		}
		a.emit(mod<<6|r|4, rm.scale<<6|idx<<3|base)
	}
	switch {
	case rm.base < 0:
		a.emit32(uint32(rm.disp))
	case mod == 1:
		a.emit(byte(rm.disp))
	case mod == 2:
		a.emit32(uint32(rm.disp))
	}
}

// Common forms.

func (a *x64) movRR32(dst, src int) {
	if dst != src {
		a.inst(0, 0, []byte{0x89}, src, xr(dst))
	}
}

func (a *x64) movRI32(dst int, imm uint32) {
	if imm == 0 {
		a.inst(0, 0, []byte{0x31}, dst, xr(dst)) // xor: shorter (changes flags)
		return
	}
	if dst >= 8 {
		a.emit(0x41)
	}
	a.emit(0xB8 + byte(dst&7))
	a.emit32(imm)
}

// movRI32NoFlags is movRI32 that leaves the flags alone.
func (a *x64) movRI32NoFlags(dst int, imm uint32) {
	if dst >= 8 {
		a.emit(0x41)
	}
	a.emit(0xB8 + byte(dst&7))
	a.emit32(imm)
}

func (a *x64) load32(dst int, m xop)      { a.inst(0, 0, []byte{0x8B}, dst, m) }
func (a *x64) store32(m xop, src int)     { a.inst(0, 0, []byte{0x89}, src, m) }
func (a *x64) load64(dst int, m xop)      { a.inst(0, xW, []byte{0x8B}, dst, m) }
func (a *x64) store64(m xop, src int)     { a.inst(0, xW, []byte{0x89}, src, m) }
func (a *x64) storeI32(m xop, imm uint32) { a.inst(0, 0, []byte{0xC7}, 0, m); a.emit32(imm) }
func (a *x64) lea32(dst int, m xop)       { a.inst(0, 0, []byte{0x8D}, dst, m) }
func (a *x64) lea64(dst int, m xop)       { a.inst(0, xW, []byte{0x8D}, dst, m) }
func (a *x64) movzx8(dst int, m xop)      { a.inst(0, xByte, []byte{0x0F, 0xB6}, dst, m) }
func (a *x64) movzx16(dst int, m xop)     { a.inst(0, 0, []byte{0x0F, 0xB7}, dst, m) }
func (a *x64) movsx8(dst int, m xop)      { a.inst(0, xByte, []byte{0x0F, 0xBE}, dst, m) }
func (a *x64) movsx16(dst int, m xop)     { a.inst(0, 0, []byte{0x0F, 0xBF}, dst, m) }
func (a *x64) store8(m xop, src int)      { a.inst(0, xByte, []byte{0x88}, src, m) }
func (a *x64) storeI8(m xop, imm uint8)   { a.inst(0, xByte, []byte{0xC6}, 0, m); a.emit(imm) }
func (a *x64) store16(m xop, src int)     { a.inst(0x66, 0, []byte{0x89}, src, m) }

// alu32 is op r/m32, reg (op: 0 add, 1 or, 2 adc, 3 sbb, 4 and, 5 sub, 6 xor, 7 cmp).
func (a *x64) alu32(op int, rm xop, reg int) { a.inst(0, 0, []byte{byte(op<<3 | 1)}, reg, rm) }

// alu32RM is op reg, r/m32.
func (a *x64) alu32RM(op int, reg int, rm xop) { a.inst(0, 0, []byte{byte(op<<3 | 3)}, reg, rm) }

// alu32I is op r/m32, imm (sign-extended imm8 when it fits).
func (a *x64) alu32I(op int, rm xop, imm uint32) {
	if int32(imm) >= -128 && int32(imm) <= 127 {
		a.inst(0, 0, []byte{0x83}, op, rm)
		a.emit(byte(imm))
		return
	}
	a.inst(0, 0, []byte{0x81}, op, rm)
	a.emit32(imm)
}

// alu64I is op r/m64, imm32 (sign-extended).
func (a *x64) alu64I(op int, rm xop, imm int32) {
	if imm >= -128 && imm <= 127 {
		a.inst(0, xW, []byte{0x83}, op, rm)
		a.emit(byte(imm))
		return
	}
	a.inst(0, xW, []byte{0x81}, op, rm)
	a.emit32(uint32(imm))
}

func (a *x64) alu8(op int, rm xop, reg int, flags int) {
	a.inst(0, flags, []byte{byte(op << 3)}, reg, rm)
}

func (a *x64) alu8I(op int, rm xop, imm uint8, flags int) {
	a.inst(0, flags, []byte{0x80}, op, rm)
	a.emit(imm)
}

func (a *x64) shiftI32(op int, rm xop, n uint8) {
	if n == 1 {
		a.inst(0, 0, []byte{0xD1}, op, rm)
		return
	}
	a.inst(0, 0, []byte{0xC1}, op, rm)
	a.emit(n)
}

func (a *x64) shiftCL32(op int, rm xop) { a.inst(0, 0, []byte{0xD3}, op, rm) }

func (a *x64) setcc(cc uint8, rm xop, flags int) { a.inst(0, flags, []byte{0x0F, 0x90 | cc}, 0, rm) }

func (a *x64) cmov(cc uint8, dst int, rm xop) { a.inst(0, 0, []byte{0x0F, 0x40 | cc}, dst, rm) }

func (a *x64) jcc(cc uint8, l int) {
	a.emit(0x0F, 0x80|cc)
	a.rel32(l)
}

func (a *x64) jmp(l int) {
	a.emit(0xE9)
	a.rel32(l)
}

// jmpAbs is a rel32 jump whose target is patched later: it returns the rel32's offset.
func (a *x64) jmpPatchable() int {
	a.emit(0xE9)
	at := len(a.b)
	a.emit32(0)
	return at
}

func (a *x64) jmpReg(r int) { a.inst(0, 0, []byte{0xFF}, 4, xr(r)) }

func (a *x64) jmpMem(m xop) { a.inst(0, 0, []byte{0xFF}, 4, m) }

func (a *x64) pushfq() { a.emit(0x9C) }
func (a *x64) popfq()  { a.emit(0x9D) }

func (a *x64) push(r int) {
	if r >= 8 {
		a.emit(0x41)
	}
	a.emit(0x50 + byte(r&7))
}

func (a *x64) pop(r int) {
	if r >= 8 {
		a.emit(0x41)
	}
	a.emit(0x58 + byte(r&7))
}

func (a *x64) ret() { a.emit(0xC3) }

// SSE: dst and src are XMM registers (reg) or r/m operands.
func (a *x64) sse(prefix byte, op byte, xmm int, rm xop) {
	a.inst(prefix, 0, []byte{0x0F, op}, xmm, rm)
}

package cpu

import (
	"fmt"
	"math/bits"
)

// A small AArch64 encoder for the JIT's arm64 backend (jit_arm64.go). Like x64asm.go it has no
// build constraint, so its test (against golang.org/x/arch's disassembler) runs everywhere.
//
// Registers are numbers 0-31; 31 is the zero register (WZR/XZR) in the forms used here, except
// as the base of a load or store, where it would be SP (never used). Methods named with a 64
// suffix (or taking sf) work on X registers, the others on W registers.

const a64ZR = 31

// Condition codes.
const (
	a64EQ = iota
	a64NE
	a64CS // HS
	a64CC // LO
	a64MI
	a64PL
	a64VS
	a64VC
	a64HI
	a64LS
	a64GE
	a64LT
	a64GT
	a64LE
	a64AL
)

// Shift types for shifted-register operands.
const (
	a64LSL = iota
	a64LSR
	a64ASR
	a64ROR
)

type a64 struct {
	w      []uint32
	labels []int // label -> instruction index (-1: unbound)
	fixups []a64fixup
}

// a64fixup is a branch at w[at] to a label: kind says which offset field it has.
type a64fixup struct{ at, label, kind int }

const (
	a64Fix26 = iota // B
	a64Fix19        // B.cond, CBZ/CBNZ
	a64Fix14        // TBZ/TBNZ
)

func (a *a64) emit(w uint32) { a.w = append(a.w, w) }

func (a *a64) newLabel() int {
	a.labels = append(a.labels, -1)
	return len(a.labels) - 1
}

func (a *a64) bind(l int) { a.labels[l] = len(a.w) }

// pc is the index of the next instruction.
func (a *a64) pc() int { return len(a.w) }

// patchBranch sets the offset field of branch instruction w (of the fixup's kind) to delta
// instructions.
func a64PatchBranch(w uint32, kind int, delta int) uint32 {
	switch kind {
	case a64Fix26:
		if delta < -(1<<25) || delta >= 1<<25 {
			panic("a64: branch out of range")
		}
		return w&^0x3FFFFFF | uint32(delta)&0x3FFFFFF
	case a64Fix19:
		if delta < -(1<<18) || delta >= 1<<18 {
			panic("a64: conditional branch out of range")
		}
		return w&^(0x7FFFF<<5) | (uint32(delta)&0x7FFFF)<<5
	default:
		if delta < -(1<<13) || delta >= 1<<13 {
			panic("a64: test branch out of range")
		}
		return w&^(0x3FFF<<5) | (uint32(delta)&0x3FFF)<<5
	}
}

// resolve patches every branch to its label.
func (a *a64) resolve() {
	for _, f := range a.fixups {
		to := a.labels[f.label]
		if to < 0 {
			panic("a64: unbound label")
		}
		a.w[f.at] = a64PatchBranch(a.w[f.at], f.kind, to-f.at)
	}
	a.fixups = a.fixups[:0]
}

func (a *a64) branchTo(w uint32, l, kind int) {
	a.fixups = append(a.fixups, a64fixup{len(a.w), l, kind})
	a.emit(w)
}

// Branches.

func (a *a64) b(l int)                 { a.branchTo(0x14000000, l, a64Fix26) }
func (a *a64) bcond(cond uint8, l int) { a.branchTo(0x54000000|uint32(cond), l, a64Fix19) }
func (a *a64) cbz(rt int, l int)       { a.branchTo(0x34000000|uint32(rt), l, a64Fix19) }
func (a *a64) cbnz(rt int, l int)      { a.branchTo(0x35000000|uint32(rt), l, a64Fix19) }
func (a *a64) tbz(rt, bit int, l int)  { a.branchTo(a64tb(0x36000000, rt, bit), l, a64Fix14) }
func (a *a64) tbnz(rt, bit int, l int) { a.branchTo(a64tb(0x37000000, rt, bit), l, a64Fix14) }
func (a *a64) br(rn int)               { a.emit(0xD61F0000 | uint32(rn)<<5) }
func (a *a64) ret()                    { a.emit(0xD65F03C0) }
func a64tb(op uint32, rt, bit int) uint32 {
	return op | uint32(bit>>5)<<31 | uint32(bit&31)<<19 | uint32(rt)
}

// Data processing.

// addI is ADD/SUB(S) with a 12-bit immediate (optionally shifted by 12): op 0 add, 1 adds, 2
// sub, 3 subs.
func (a *a64) addI(sf bool, op int, rd, rn int, imm uint32) {
	w := uint32(0x11000000) | uint32(op&2)<<29 | uint32(op&1)<<29 | a64sf(sf)
	switch {
	case imm < 1<<12:
	case imm&0xFFF == 0 && imm < 1<<24:
		w |= 1 << 22
		imm >>= 12
	default:
		panic(fmt.Sprintf("a64: add immediate %#x", imm))
	}
	a.emit(w | imm<<10 | uint32(rn)<<5 | uint32(rd))
}

func a64sf(sf bool) uint32 {
	if sf {
		return 1 << 31
	}
	return 0
}

const (
	a64Add = iota
	a64Adds
	a64Sub
	a64Subs
)

// addR is ADD/SUB(S) with a shifted register: rd = rn op (rm shift amt).
func (a *a64) addR(sf bool, op int, rd, rn, rm int, shift, amt int) {
	a.emit(0x0B000000 | a64sf(sf) | uint32(op&2)<<29 | uint32(op&1)<<29 | uint32(shift)<<22 |
		uint32(rm)<<16 | uint32(amt)<<10 | uint32(rn)<<5 | uint32(rd))
}

// addX is ADD/SUB(S) with an extended register (option: 2 uxtw, 6 sxtw).
func (a *a64) addX(sf bool, op int, rd, rn, rm int, option int, amt int) {
	a.emit(0x0B200000 | a64sf(sf) | uint32(op&2)<<29 | uint32(op&1)<<29 |
		uint32(rm)<<16 | uint32(option)<<13 | uint32(amt)<<10 | uint32(rn)<<5 | uint32(rd))
}

// Logical (shifted register) opcodes.
const (
	a64And  = 0x0A000000
	a64Bic  = 0x0A200000
	a64Orr  = 0x2A000000
	a64Orn  = 0x2A200000
	a64Eor  = 0x4A000000
	a64Eon  = 0x4A200000
	a64Ands = 0x6A000000
)

func (a *a64) logR(sf bool, op uint32, rd, rn, rm int, shift, amt int) {
	a.emit(op | a64sf(sf) | uint32(shift)<<22 | uint32(rm)<<16 | uint32(amt)<<10 | uint32(rn)<<5 | uint32(rd))
}

// Logical immediate opcodes (32-bit forms).
const (
	a64AndI  = 0x12000000
	a64OrrI  = 0x32000000
	a64EorI  = 0x52000000
	a64AndsI = 0x72000000
)

// a64BitMask encodes v as a 32-bit logical immediate (immr, imms); ok is false if it can't be.
func a64BitMask(v uint32) (immr, imms uint32, ok bool) {
	if v == 0 || v == 0xFFFFFFFF {
		return 0, 0, false
	}
	for size := uint32(2); size <= 32; size *= 2 {
		mask := uint32(1)<<size - 1
		if size == 32 {
			mask = 0xFFFFFFFF
		}
		e := v & mask
		ok := true
		for s := size; s < 32; s += size {
			if v>>s&mask != e {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		// e must be a rotated run of ones within size bits.
		ones := uint32(bits.OnesCount32(e))
		if ones == 0 || ones == size {
			continue
		}
		for r := uint32(0); r < size; r++ {
			// rotating e right by r within size bits gives the run at the bottom
			rot := (e>>r | e<<(size-r)) & mask
			if r == 0 {
				rot = e
			}
			if rot == 1<<ones-1 {
				// the element is (1<<ones-1) rotated right by immr = size-r (mod size)
				immr = (size - r) % size
				imms = (^(size*2 - 1) & 0x3F) | (ones - 1)
				return immr, imms & 0x3F, true
			}
		}
	}
	return 0, 0, false
}

// logI is a logical operation with an immediate; it panics if imm can't be encoded (callers use
// a64BitMask first when unsure).
func (a *a64) logI(op uint32, rd, rn int, imm uint32) {
	immr, imms, ok := a64BitMask(imm)
	if !ok {
		panic(fmt.Sprintf("a64: logical immediate %#x", imm))
	}
	a.emit(op | immr<<16 | imms<<10 | uint32(rn)<<5 | uint32(rd))
}

// movI puts a 32-bit constant in rd (W) without changing the flags.
func (a *a64) movI(rd int, v uint32) {
	switch {
	case v>>16 == 0:
		a.emit(0x52800000 | v<<5 | uint32(rd)) // movz
	case v&0xFFFF == 0:
		a.emit(0x52800000 | 1<<21 | (v>>16)<<5 | uint32(rd))
	case ^v>>16 == 0:
		a.emit(0x12800000 | (^v&0xFFFF)<<5 | uint32(rd)) // movn
	case ^v&0xFFFF == 0:
		a.emit(0x12800000 | 1<<21 | (^v>>16&0xFFFF)<<5 | uint32(rd))
	default:
		if _, _, ok := a64BitMask(v); ok {
			a.logI(a64OrrI, rd, a64ZR, v)
			return
		}
		a.emit(0x52800000 | (v&0xFFFF)<<5 | uint32(rd))
		a.emit(0x72800000 | 1<<21 | (v>>16)<<5 | uint32(rd)) // movk
	}
}

// movI64 puts a 64-bit constant in rd (X).
func (a *a64) movI64(rd int, v uint64) {
	a.emit(0xD2800000 | uint32(v&0xFFFF)<<5 | uint32(rd))
	for hw := 1; hw < 4; hw++ {
		if part := uint32(v >> (16 * hw) & 0xFFFF); part != 0 {
			a.emit(0xF2800000 | uint32(hw)<<21 | part<<5 | uint32(rd))
		}
	}
}

// mov copies W register rn to rd (zeroing the upper half of rd's X register).
func (a *a64) mov(rd, rn int) {
	if rd != rn {
		a.logR(false, a64Orr, rd, a64ZR, rn, 0, 0)
	}
}

func (a *a64) mov64(rd, rn int) { a.logR(true, a64Orr, rd, a64ZR, rn, 0, 0) }

// Bitfield moves (32-bit): ubfm, sbfm, bfm.
func (a *a64) ubfm(rd, rn int, immr, imms uint32) {
	a.emit(0x53000000 | immr<<16 | imms<<10 | uint32(rn)<<5 | uint32(rd))
}
func (a *a64) sbfm(rd, rn int, immr, imms uint32) {
	a.emit(0x13000000 | immr<<16 | imms<<10 | uint32(rn)<<5 | uint32(rd))
}
func (a *a64) bfm(rd, rn int, immr, imms uint32) {
	a.emit(0x33000000 | immr<<16 | imms<<10 | uint32(rn)<<5 | uint32(rd))
}

func (a *a64) lslI(rd, rn int, s uint32)      { a.ubfm(rd, rn, (32-s)&31, 31-s) }
func (a *a64) lsrI(rd, rn int, s uint32)      { a.ubfm(rd, rn, s, 31) }
func (a *a64) asrI(rd, rn int, s uint32)      { a.sbfm(rd, rn, s, 31) }
func (a *a64) ubfx(rd, rn int, lsb, w uint32) { a.ubfm(rd, rn, lsb, lsb+w-1) }
func (a *a64) bfi(rd, rn int, lsb, w uint32)  { a.bfm(rd, rn, (32-lsb)&31, w-1) }
func (a *a64) rorI(rd, rn int, s uint32) {
	a.emit(0x13800000 | uint32(rn)<<16 | s<<10 | uint32(rn)<<5 | uint32(rd)) // extr
}

// 64-bit bitfield moves.
func (a *a64) ubfm64(rd, rn int, immr, imms uint32) {
	a.emit(0xD3400000 | immr<<16 | imms<<10 | uint32(rn)<<5 | uint32(rd))
}
func (a *a64) bfm64(rd, rn int, immr, imms uint32) {
	a.emit(0xB3400000 | immr<<16 | imms<<10 | uint32(rn)<<5 | uint32(rd))
}
func (a *a64) lslI64(rd, rn int, s uint32) { a.ubfm64(rd, rn, (64-s)&63, 63-s) }
func (a *a64) lsrI64(rd, rn int, s uint32) { a.ubfm64(rd, rn, s, 63) }

// Two-source operations (32-bit): op is one of the a64*V constants.
const (
	a64Lslv = 0x1AC02000
	a64Lsrv = 0x1AC02400
	a64Asrv = 0x1AC02800
	a64Rorv = 0x1AC02C00
	a64Adc  = 0x1A000000
	a64Adcs = 0x3A000000
	a64Sbc  = 0x5A000000
	a64Sbcs = 0x7A000000
)

func (a *a64) op2(op uint32, rd, rn, rm int) {
	a.emit(op | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

// smull: Xd = Wn * Wm (signed, 64-bit product).
func (a *a64) smull(rd, rn, rm int) {
	a.emit(0x9B200000 | uint32(rm)<<16 | 31<<10 | uint32(rn)<<5 | uint32(rd))
}

func (a *a64) rev(rd, rn int) { a.emit(0x5AC00800 | uint32(rn)<<5 | uint32(rd)) }

// Conditional select (32-bit).
func (a *a64) csel(rd, rn, rm int, cond uint8) {
	a.emit(0x1A800000 | uint32(rm)<<16 | uint32(cond)<<12 | uint32(rn)<<5 | uint32(rd))
}
func (a *a64) csinc(rd, rn, rm int, cond uint8) {
	a.emit(0x1A800400 | uint32(rm)<<16 | uint32(cond)<<12 | uint32(rn)<<5 | uint32(rd))
}

// cset: rd = cond ? 1 : 0.
func (a *a64) cset(rd int, cond uint8) { a.csinc(rd, a64ZR, a64ZR, cond^1) }

// mrs/msr NZCV (X register).
func (a *a64) mrsNZCV(rt int) { a.emit(0xD53B4200 | uint32(rt)) }
func (a *a64) msrNZCV(rt int) { a.emit(0xD51B4200 | uint32(rt)) }

// Loads and stores. Sizes: 0 byte, 1 half, 2 word, 3 double word; for SIMD registers, 4 is a
// Q (128-bit) register.
const (
	a64St   = 0 // store
	a64Ld   = 1 // load, zero-extended
	a64LdS  = 3 // load, sign-extended to 32 bits (bytes and halves)
	a64LdSX = 2 // load, sign-extended to 64 bits
)

// ldstU is a load or store at [rn + off], off a multiple of the size below 4096 times it.
func (a *a64) ldstU(size int, opc int, simd bool, rt, rn int, off uint32) {
	if off&(1<<size-1) != 0 || off>>size >= 4096 {
		panic(fmt.Sprintf("a64: load/store offset %#x for size %d", off, size))
	}
	a.emit(a64ldstOp(0x39000000, size, opc, simd) | (off>>size)<<10 | uint32(rn)<<5 | uint32(rt))
}

// ldstR is a load or store at [rn + rm uxtw] (rm a W register holding a guest address).
func (a *a64) ldstR(size int, opc int, simd bool, rt, rn, rm int) {
	a.emit(a64ldstOp(0x38200800, size, opc, simd) | uint32(rm)<<16 | 2<<13 | uint32(rn)<<5 | uint32(rt))
}

// ldstX is a load or store at [rn + rm<<size] (rm an X register holding an index).
func (a *a64) ldstX(size int, opc int, simd bool, rt, rn, rm int) {
	a.emit(a64ldstOp(0x38200800, size, opc, simd) | uint32(rm)<<16 | 3<<13 | 1<<12 | uint32(rn)<<5 | uint32(rt))
}

// a64ldstOp adds the size, opc and SIMD bits to a load/store opcode (a Q register is size 0 with
// opc 1x).
func a64ldstOp(w uint32, size int, opc int, simd bool) uint32 {
	if simd {
		w |= 1 << 26
		if size == 4 {
			size, opc = 0, opc|2
		}
	}
	return w | uint32(size)<<30 | uint32(opc)<<22
}

// ldrbIdx: Wt = byte [Xn + Xm] (Xm: an index already zero-extended).
func (a *a64) ldrbIdx(rt, rn, rm int) {
	a.emit(0x38606800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rt)) // option lsl (uxtx)
}

// ldrhIdx: Wt = half [Xn + Xm<<1].
func (a *a64) ldrhIdx(rt, rn, rm int) {
	a.emit(0x78607800 | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rt)) // lsl #1
}

// stp/ldp of X registers at [rn + off] (off a multiple of 8, -512..504).
func (a *a64) stp(rt, rt2, rn int, off int32) {
	a.emit(0xA9000000 | (uint32(off/8)&0x7F)<<15 | uint32(rt2)<<10 | uint32(rn)<<5 | uint32(rt))
}
func (a *a64) ldp(rt, rt2, rn int, off int32) {
	a.emit(0xA9400000 | (uint32(off/8)&0x7F)<<15 | uint32(rt2)<<10 | uint32(rn)<<5 | uint32(rt))
}

// Floating point (scalar): ftype 0 single, 1 double.

const (
	a64Fmul = 0x1E200800
	a64Fdiv = 0x1E201800
	a64Fadd = 0x1E202800
	a64Fsub = 0x1E203800
)

func (a *a64) fop(op uint32, double bool, rd, rn, rm int) {
	a.emit(op | a64ftype(double) | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

func a64ftype(double bool) uint32 {
	if double {
		return 1 << 22
	}
	return 0
}

func (a *a64) fsqrt(double bool, rd, rn int) {
	a.emit(0x1E21C000 | a64ftype(double) | uint32(rn)<<5 | uint32(rd))
}
func (a *a64) fcmp(double bool, rn, rm int) {
	a.emit(0x1E202000 | a64ftype(double) | uint32(rm)<<16 | uint32(rn)<<5)
}
func (a *a64) fcsel(double bool, rd, rn, rm int, cond uint8) {
	a.emit(0x1E200C00 | a64ftype(double) | uint32(rm)<<16 | uint32(cond)<<12 | uint32(rn)<<5 | uint32(rd))
}
func (a *a64) fneg(rd, rn int) { a.emit(0x1E614000 | uint32(rn)<<5 | uint32(rd)) } // double
func (a *a64) fabs(rd, rn int) { a.emit(0x1E60C000 | uint32(rn)<<5 | uint32(rd)) } // double

func (a *a64) fcvtS2D(rd, rn int) { a.emit(0x1E22C000 | uint32(rn)<<5 | uint32(rd)) }
func (a *a64) fcvtD2S(rd, rn int) { a.emit(0x1E624000 | uint32(rn)<<5 | uint32(rd)) }

// fcvtzs64: Xd = trunc(Sn or Dn), saturating.
func (a *a64) fcvtzs64(double bool, rd, rn int) {
	a.emit(0x9E380000 | a64ftype(double) | uint32(rn)<<5 | uint32(rd))
}

// scvtf: Sd or Dd = float(Wn).
func (a *a64) scvtf(double bool, rd, rn int) {
	a.emit(0x1E220000 | a64ftype(double) | uint32(rn)<<5 | uint32(rd))
}

func (a *a64) fmovToW(rd, rn int)   { a.emit(0x1E260000 | uint32(rn)<<5 | uint32(rd)) } // Wd = Sn
func (a *a64) fmovFromW(rd, rn int) { a.emit(0x1E270000 | uint32(rn)<<5 | uint32(rd)) } // Sd = Wn

// Vector (128-bit) operations.
const (
	a64VFadd  = 0x4E20D400 // .4s
	a64VFsub  = 0x4EA0D400
	a64VFmul  = 0x6E20DC00
	a64VFdiv  = 0x6E20FC00
	a64VFcmeq = 0x4E20E400
	a64VFcmgt = 0x6EA0E400
	a64VFcmge = 0x6E20E400
	a64VZip1  = 0x4E803800 // .4s
	a64VAnd   = 0x4E201C00 // .16b
	a64VBic   = 0x4E601C00
	a64VOrr   = 0x4EA01C00
	a64VEor   = 0x6E201C00
	a64VBsl   = 0x6E601C00
	a64VBif   = 0x6EE01C00
)

func (a *a64) vop(op uint32, rd, rn, rm int) {
	a.emit(op | uint32(rm)<<16 | uint32(rn)<<5 | uint32(rd))
}

func (a *a64) vnot(rd, rn int) { a.emit(0x6E205800 | uint32(rn)<<5 | uint32(rd)) } // .16b

func (a *a64) vfsqrt(rd, rn int) { a.emit(0x6EA1F800 | uint32(rn)<<5 | uint32(rd)) }

// insS: Vd.s[i] = Vn.s[j].
func (a *a64) insS(rd, i, rn, j int) {
	a.emit(0x6E000400 | uint32(i<<3|4)<<16 | uint32(j<<2)<<11 | uint32(rn)<<5 | uint32(rd))
}

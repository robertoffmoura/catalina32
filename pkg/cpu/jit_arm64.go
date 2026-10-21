package cpu

import (
	"encoding/binary"
	"math"
	"unsafe"
)

// The JIT's arm64 backend. Guest registers live in host registers while translated code runs:
//
//	guest  EAX ECX EDX EBX ESP EBP ESI EDI
//	host   w19 w20 w21 w22 w23 w24 w25 w26
//
// x27 holds the CPU, w8 the guest flags (c.Flags), x7 the step budget, x9 &Mem[0], x10 the store
// table, x3 the lookup table, x5 &XMM[0], x11/x12 the flag tables (a64FlagTabs); w4 and w6 are
// constants. x0 holds effective addresses; x1, x2 and x13-x17 are scratch. Translated code never
// touches x18 (the platform register), x28 (Go's g), x29, x30 or the stack.
//
// The host has no x86 flags, so translated code computes them: an instruction setting flags runs
// as the host instruction setting NZCV the same way where there is one (ADDS, SUBS, ANDS, ...;
// 8- and 16-bit operations work on operands shifted to the top of the register). When its flags
// can be observed (an instruction reading them other than right after it, a slow exit, the
// block's end) they are converted to x86 flags and merged into w8 at once: CF, ZF, SF and OF from
// NZCV through a table (per mode: whether C is CF, its inverse, or not CF at all), PF through a
// parity table, AF from the operands. An instruction reading flags right after the one that set
// them tests NZCV directly when a host condition matches its x86 condition in that mode;
// otherwise it tests w8.

const jitSupported = true

const (
	jitCodeSize   = 112 << 20 // the code buffer (reserved); when full, all translated code is discarded. B reaches ±128 MB
	jitHotEighths = 8         // all of it: exits stay next to their block (B.cond reaches ±1 MB)
)

// Host registers.
const (
	rEA     = 0 // effective address
	rT1     = 1 // operand a, loaded values
	rT2     = 2 // results
	rTab    = 3 // &jitCode.table[0]
	rHash   = 4 // jitHashMul
	rXMM    = 5 // &c.XMM[0]
	rLim    = 6 // ArenaSize-8: loads at or past it take the interpreter
	rBudget = 7
	rFlags  = 8
	rMem    = 9
	rStores = 10
	rPar    = 11 // a64FlagTabs.par
	rNZCV   = 12 // a64FlagTabs.nzcv
	rF1     = 13 // flag computation, stubs
	rF2     = 14
	rT3     = 15 // operand b
	rT4     = 16 // checks, shifted operands
	rAddr   = 17 // CPU fields past the reach of an immediate offset
	rCPU    = 27
)

func gr(g uint8) int { return 19 + int(g) }

// Flag modes: what NZCV mean after an instruction set them.
const (
	fmNone  = -1 // nothing usable (the flags are computed otherwise)
	fmSub   = 0  // C is the inverse of CF (subtraction)
	fmAdd   = 1  // C is CF
	fmNoC   = 2  // C isn't CF (inc, dec: CF is kept)
	fmFcmp  = 3  // FCMP
	fmLogic = 4  // C and V are 0 (as fmAdd)
)

// a64FlagTabs converts host flags to x86 flags: nzcv[mode][NZCV] is CF, ZF, SF and OF (fmFcmp:
// CF, ZF and PF as UCOMISS sets them); par[b] is PF for result byte b.
var a64FlagTabs = func() (t struct {
	par  [256]uint8
	nzcv [4][16]uint16
}) {
	for i := range t.par {
		t.par[i] = uint8(parityTab[i])
	}
	for m := range t.nzcv {
		for v := 0; v < 16; v++ {
			n, z, c, ov := v&8 != 0, v&4 != 0, v&2 != 0, v&1 != 0
			var f uint32
			switch m {
			case fmFcmp: // less: N; equal: Z, C; greater: C; unordered: C, V
				f = flagIf(n || ov, FlagCF) | flagIf(z || ov, FlagZF) | flagIf(ov, FlagPF)
			default:
				f = flagIf(z, FlagZF) | flagIf(n, FlagSF) | flagIf(ov, FlagOF)
				switch m {
				case fmSub:
					f |= flagIf(!c, FlagCF)
				case fmAdd:
					f |= flagIf(c, FlagCF)
				}
			}
			t.nzcv[m][v] = uint16(f)
		}
	}
	return
}()

// a64Conds[mode][cc] is the host condition testing x86 condition cc right after an instruction
// of that mode set NZCV (-1: none does).
var a64Conds = [5][16]int8{
	fmSub:   {a64VS, a64VC, a64CC, a64CS, a64EQ, a64NE, a64LS, a64HI, a64MI, a64PL, -1, -1, a64LT, a64GE, a64LE, a64GT},
	fmAdd:   {a64VS, a64VC, a64CS, a64CC, a64EQ, a64NE, -1, -1, a64MI, a64PL, -1, -1, a64LT, a64GE, a64LE, a64GT},
	fmNoC:   {a64VS, a64VC, -1, -1, a64EQ, a64NE, -1, -1, a64MI, a64PL, -1, -1, a64LT, a64GE, a64LE, a64GT},
	fmFcmp:  {-1, -1, a64LT, a64GE, -1, -1, a64LE, a64GT, -1, -1, a64VS, a64VC, -1, -1, -1, -1},
	fmLogic: {a64VS, a64VC, a64CS, a64CC, a64EQ, a64NE, a64EQ, a64NE, a64MI, a64PL, -1, -1, a64LT, a64GE, a64LE, a64GT},
}

func a64Cond(mode int, cc uint8) (uint8, bool) {
	if mode < 0 {
		return 0, false
	}
	c := a64Conds[mode][cc&15]
	return uint8(c), c >= 0
}

// jitCall runs translated code: prologue saves the host registers Go needs back, loads the guest
// registers and jumps to target (jit_arm64.s).
func jitCall(prologue uintptr, c *CPU, target uintptr)

// jitFlushICache makes code written at [addr, addr+n) visible to instruction fetch (jit_arm64.s).
func jitFlushICache(addr, n uintptr)

// jitShared emits the prologue and epilogue at the start of the code buffer.
func jitShared(j *jitCode) int {
	g := &jitGen{}
	a := &g.a
	// prologue: x0 = c, x1 = target
	a.movI(rAddr, uint32(offSave))
	a.addR(true, a64Add, rAddr, 0, rAddr, a64LSL, 0)
	for i := 0; i < 8; i += 2 {
		a.stp(19+i, 20+i, rAddr, int32(8*i))
	}
	a.ldstU(3, a64St, false, rCPU, rAddr, 64)
	a.mov64(rCPU, 0)
	g.cpuLS(3, a64Ld, rMem, offMem)
	g.cpuLS(3, a64Ld, rStores, offStore)
	g.cpuLS(3, a64Ld, rTab, offTable)
	g.cpuLS(3, a64Ld, rBudget, offBudget)
	g.cpuLS(2, a64Ld, rFlags, offFlags)
	for r := uint8(0); r < 8; r++ {
		g.cpuLS(2, a64Ld, gr(r), offRegs+4*int32(r))
	}
	a.addI(true, a64Add, rXMM, rCPU, uint32(offXMM))
	a.movI64(rPar, uint64(uintptr(unsafe.Pointer(&a64FlagTabs.par))))
	a.movI64(rNZCV, uint64(uintptr(unsafe.Pointer(&a64FlagTabs.nzcv))))
	a.movI(rHash, jitHashMul)
	a.movI(rLim, ArenaSize-8)
	a.br(1)
	epilogue := a.pc()
	for r := uint8(0); r < 8; r++ {
		g.cpuLS(2, a64St, gr(r), offRegs+4*int32(r))
	}
	g.cpuLS(2, a64St, rFlags, offFlags)
	g.cpuLS(3, a64St, rBudget, offBudget)
	a.movI(rAddr, uint32(offSave))
	a.addR(true, a64Add, rAddr, rCPU, rAddr, a64LSL, 0)
	for i := 0; i < 8; i += 2 {
		a.ldp(19+i, 20+i, rAddr, int32(8*i))
	}
	a.ldstU(3, a64Ld, false, rCPU, rAddr, 64) // last: it addresses the CPU
	a.ret()
	base := uintptr(unsafe.Pointer(&j.buf[0]))
	jitWriteBegin()
	for i, w := range a.w {
		binary.LittleEndian.PutUint32(j.buf[4*i:], w)
	}
	jitWriteEnd(base, 4*len(a.w))
	j.prologue = base
	j.epilogue = base + uintptr(4*epilogue)
	return (4*len(a.w) + 15) &^ 15
}

// jitPatch points the B instruction at absolute address site to target.
func jitPatch(j *jitCode, site, target uintptr) {
	off := site - uintptr(unsafe.Pointer(&j.buf[0]))
	w := binary.LittleEndian.Uint32(j.buf[off:])
	jitWriteBegin()
	binary.LittleEndian.PutUint32(j.buf[off:], a64PatchBranch(w, a64Fix26, int(int64(target-site)/4)))
	jitWriteEnd(site, 4)
}

type jitGen struct {
	a  a64
	c  *CPU
	j  *jitCode
	jb *jitBlock

	slow    []int  // per instruction: label of its slow exit (-1: none yet)
	start   uint32 // guest address of the instruction being translated
	idx     int
	ends    []jitChain
	endStub int // label: exit with EIP already stored

	capture  []bool // per instruction setting flags: whether they are computed into w8
	hostFrom int    // the instruction whose flags NZCV hold (-1: none)
	mode     int    // and their mode
	native   bool   // the current instruction reads NZCV directly

	absFix    []jitAbs // branches to absolute addresses
	resume    []int    // where each instruction's code starts (instruction index)
	limitExit int      // label
	hostOK    []bool   // per instruction: NZCV hold its flags when it ends
	modes     []int    // per instruction: the mode of its flags
	natives   []bool   // per instruction: reads NZCV directly
	changed   int      // label: a self-checking block's code changed (-1: not one)
}

type jitChain struct {
	label  int    // the stub the patchable branch goes to until the target is translated
	at     int    // the patchable branch (instruction index)
	target uint32 // guest address
}

type jitAbs struct {
	at int // instruction index of a B
	to uintptr
}

// cpuLS loads or stores CPU field off (size: 2 word, 3 double word).
func (g *jitGen) cpuLS(size int, opc int, rt int, off int32) {
	o := uint32(off)
	if o>>size < 4096 {
		g.a.ldstU(size, opc, false, rt, rCPU, o)
		return
	}
	hi := o &^ 0xFFF
	g.a.addI(true, a64Add, rAddr, rCPU, hi)
	g.a.ldstU(size, opc, false, rt, rAddr, o-hi)
}

// xmm loads or stores XMM register r's low bytes (size 2: 4, 3: 8, 4: 16) from host register v
// (a SIMD register if simd).
func (g *jitGen) xmm(size int, opc int, simd bool, v int, r uint8, off uint32) {
	g.a.ldstU(size, opc, simd, v, rXMM, 16*uint32(r)+off)
}

// guest loads or stores at [x9 + w0].
func (g *jitGen) guest(size int, opc int, simd bool, v int) {
	g.a.ldstR(size, opc, simd, v, rMem, rEA)
}

func (g *jitGen) slowLabel() int {
	if g.slow[g.idx] < 0 {
		g.slow[g.idx] = g.a.newLabel()
	}
	return g.slow[g.idx]
}

// addConst: rd = rn + v (W registers; uses w16 when v doesn't fit an immediate).
func (g *jitGen) addConst(rd, rn int, v uint32) {
	a := &g.a
	switch {
	case v == 0:
		a.mov(rd, rn)
	case v < 1<<12 || v&0xFFF == 0 && v < 1<<24:
		a.addI(false, a64Add, rd, rn, v)
	case -v < 1<<12 || -v&0xFFF == 0 && -v < 1<<24:
		a.addI(false, a64Sub, rd, rn, -v)
	default:
		a.movI(rT4, v)
		a.addR(false, a64Add, rd, rn, rT4, a64LSL, 0)
	}
}

// ea computes the guest effective address of d into rd (w0 for memory operands).
func (g *jitGen) ea(d *dInst, rd int) {
	a := &g.a
	switch {
	case d.base >= 0 && d.index >= 0:
		a.addR(false, a64Add, rd, gr(uint8(d.base)), gr(uint8(d.index)), a64LSL, int(d.scale))
		g.addConst(rd, rd, d.disp)
	case d.base >= 0:
		g.addConst(rd, gr(uint8(d.base)), d.disp)
	case d.index >= 0:
		if d.scale > 0 {
			a.lslI(rd, gr(uint8(d.index)), uint32(d.scale))
			g.addConst(rd, rd, d.disp)
		} else {
			g.addConst(rd, gr(uint8(d.index)), d.disp)
		}
	default:
		a.movI(rd, d.disp)
	}
}

// chkLoad sends a load at w0 to the interpreter unless it is in range (the interpreter's fast
// path: addr < ArenaSize-8, or for 16 bytes addr+16 <= ArenaSize-8).
func (g *jitGen) chkLoad(size int) {
	g.jb.cost.Loads++
	a := &g.a
	if size == 16 {
		a.addI(false, a64Sub, rT4, rLim, 16)
		a.addR(false, a64Subs, a64ZR, rEA, rT4, a64LSL, 0)
		a.bcond(a64HI, g.slowLabel())
		return
	}
	a.addR(false, a64Subs, a64ZR, rEA, rLim, a64LSL, 0)
	a.bcond(a64CS, g.slowLabel())
}

// chkStore sends a store at register r to the interpreter if it is out of range or on (or just
// before) a page holding code: the store table marks those pages.
func (g *jitGen) chkStore(r int) {
	g.jb.cost.Stores++
	a := &g.a
	a.lsrI(rT4, r, 12)
	a.ldrbIdx(rT4, rStores, rT4)
	a.cbnz(rT4, g.slowLabel())
}

// load reads d's r/m operand of size bytes (1, 2, 4) zero-extended into w1 (a register operand:
// returns its host register instead for 4 bytes).
func (g *jitGen) loadRM(d *dInst, size int, store bool) int {
	if !d.isMem {
		switch size {
		case 1:
			g.get8(d.rm, rT1)
			return rT1
		case 2:
			g.a.ubfx(rT1, gr(d.rm), 0, 16)
			return rT1
		}
		return gr(d.rm)
	}
	g.ea(d, rEA)
	if store {
		g.chkStore(rEA)
	} else {
		g.chkLoad(size)
	}
	g.guest(a64Size(size), a64Ld, false, rT1)
	return rT1
}

// storeRM writes the low size bytes of w2 to d's r/m operand (after loadRM).
func (g *jitGen) storeRM(d *dInst, size int) {
	if d.isMem {
		g.guest(a64Size(size), a64St, false, rT2)
		return
	}
	switch size {
	case 1:
		g.put8(d.rm, rT2)
	case 2:
		g.a.bfi(gr(d.rm), rT2, 0, 16)
	default:
		g.a.mov(gr(d.rm), rT2)
	}
}

func a64Size(bytes int) int {
	switch bytes {
	case 1:
		return 0
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	}
	return 4
}

// get8 puts guest 8-bit register r (AL..BH) zero-extended into dst.
func (g *jitGen) get8(r uint8, dst int) {
	if r < 4 {
		g.a.logI(a64AndI, dst, gr(r), 0xFF)
		return
	}
	g.a.ubfx(dst, gr(r-4), 8, 8)
}

// put8 stores the low byte of src into guest 8-bit register r.
func (g *jitGen) put8(r uint8, src int) {
	if r < 4 {
		g.a.bfi(gr(r), src, 0, 8)
		return
	}
	g.a.bfi(gr(r-4), src, 8, 8)
}

// produce is called right after an instruction set NZCV (in mode): when its flags can be
// observed, mat computes the x86 flags it sets into w13 and they replace clear in w8.
func (g *jitGen) produce(mode int, clear uint32, mat func()) {
	g.hostFrom, g.mode = g.idx, mode
	g.modes[g.idx] = mode
	if !g.capture[g.idx] {
		return
	}
	g.jb.cost.FlagSaves++
	mat()
	g.a.movI(rF2, clear)
	g.a.logR(false, a64Bic, rFlags, rFlags, rF2, a64LSL, 0)
	g.a.logR(false, a64Orr, rFlags, rFlags, rF1, a64LSL, 0)
}

// nzcv puts the x86 flags NZCV stand for in mode into w13.
func (g *jitGen) nzcv(mode int) {
	a := &g.a
	a.mrsNZCV(rF1)
	a.lsrI(rF1, rF1, 28)
	tab := mode
	if mode == fmLogic {
		tab = fmAdd
	}
	if tab > 0 {
		a.logI(a64OrrI, rF1, rF1, uint32(tab)<<4)
	}
	a.ldrhIdx(rF1, rNZCV, rF1)
}

// parity adds PF for the low byte of r to w13.
func (g *jitGen) parity(r int) {
	a := &g.a
	a.logI(a64AndI, rF2, r, 0xFF)
	a.ldrbIdx(rF2, rPar, rF2)
	a.logR(false, a64Orr, rF1, rF1, rF2, a64LSL, 0)
}

// af adds AF = (a ^ b ^ res) & 0x10 to w13 (b a register, or immediate bImm if b < 0).
func (g *jitGen) af(ra, rb int, bImm uint32, res int) {
	a := &g.a
	if rb < 0 {
		a.logR(false, a64Eor, rF2, ra, res, a64LSL, 0)
		if bImm&0x10 != 0 {
			a.logI(a64EorI, rF2, rF2, 0x10)
		}
	} else {
		a.logR(false, a64Eor, rF2, ra, rb, a64LSL, 0)
		a.logR(false, a64Eor, rF2, rF2, res, a64LSL, 0)
	}
	a.logI(a64AndI, rF2, rF2, 0x10)
	a.logR(false, a64Orr, rF1, rF1, rF2, a64LSL, 0)
}

// alu32 computes guest ALU operation op (0-7: add, or, adc, sbb, and, sub, xor, cmp) of ra and
// rb (or immediate imm if rb < 0) into w2, setting the flags.
func (g *jitGen) alu32(op int, ra, rb int, imm uint32) {
	a := &g.a
	if rb < 0 && !((op == 0 || op == 5 || op == 7) && imm < 1<<12) {
		a.movI(rT3, imm)
		rb = rT3
	}
	mode := fmSub
	switch op {
	case 0:
		mode = fmAdd
		if rb < 0 {
			a.addI(false, a64Adds, rT2, ra, imm)
		} else {
			a.addR(false, a64Adds, rT2, ra, rb, a64LSL, 0)
		}
	case 5, 7:
		if rb < 0 {
			a.addI(false, a64Subs, rT2, ra, imm)
		} else {
			a.addR(false, a64Subs, rT2, ra, rb, a64LSL, 0)
		}
	case 2: // C = CF
		mode = fmAdd
		a.logI(a64AndI, rT4, rFlags, 1)
		a.addI(false, a64Subs, a64ZR, rT4, 1)
		a.op2(a64Adcs, rT2, ra, rb)
	case 3: // C = !CF
		a.logI(a64AndI, rT4, rFlags, 1)
		a.addR(false, a64Subs, a64ZR, a64ZR, rT4, a64LSL, 0)
		a.op2(a64Sbcs, rT2, ra, rb)
	case 4:
		mode = fmLogic
		a.logR(false, a64Ands, rT2, ra, rb, a64LSL, 0)
	case 1, 6:
		mode = fmLogic
		o := uint32(a64Orr)
		if op == 6 {
			o = a64Eor
		}
		a.logR(false, o, rT2, ra, rb, a64LSL, 0)
		a.logR(false, a64Ands, a64ZR, rT2, rT2, a64LSL, 0)
	}
	g.produce(mode, flagsArith, func() {
		g.nzcv(mode)
		g.parity(rT2)
		if mode != fmLogic {
			g.af(ra, rb, imm, rT2)
		}
	})
}

// aluN is alu32 for 8- or 16-bit operands (zero-extended in ra and rb); the result is zero-
// extended in w2. adc and sbb aren't done here (the caller leaves them to the interpreter).
func (g *jitGen) aluN(op int, width uint32, ra, rb int) {
	a := &g.a
	sh := 32 - width
	mode := fmSub
	switch op {
	case 0, 5, 7:
		o := a64Subs
		if op == 0 {
			o, mode = a64Adds, fmAdd
		}
		a.lslI(rT4, ra, sh)
		a.addR(false, o, rT2, rT4, rb, a64LSL, int(sh))
		a.lsrI(rT2, rT2, sh)
	default:
		mode = fmLogic
		o := uint32(a64And)
		switch op {
		case 1:
			o = a64Orr
		case 6:
			o = a64Eor
		}
		a.logR(false, o, rT2, ra, rb, a64LSL, 0)
		a.lslI(rT4, rT2, sh)
		a.logR(false, a64Ands, a64ZR, rT4, rT4, a64LSL, 0)
	}
	g.produce(mode, flagsArith, func() {
		g.nzcv(mode)
		g.parity(rT2)
		if mode != fmLogic {
			g.af(ra, rb, 0, rT2)
		}
	})
}

// condFromFlags sets NZCV from the guest flags in w8 so that x86 condition cc holds when the
// returned host condition does.
func (g *jitGen) condFromFlags(cc uint8) uint8 {
	g.jb.cost.FlagLoads++
	a := &g.a
	switch cc >> 1 & 7 {
	case 0:
		a.logI(a64AndsI, a64ZR, rFlags, FlagOF)
	case 1:
		a.logI(a64AndsI, a64ZR, rFlags, FlagCF)
	case 2:
		a.logI(a64AndsI, a64ZR, rFlags, FlagZF)
	case 3: // CF | ZF
		a.lsrI(rT4, rFlags, 6)
		a.logR(false, a64Orr, rT4, rT4, rFlags, a64LSL, 0)
		a.logI(a64AndsI, a64ZR, rT4, 1)
	case 4:
		a.logI(a64AndsI, a64ZR, rFlags, FlagSF)
	case 5:
		a.logI(a64AndsI, a64ZR, rFlags, FlagPF)
	case 6: // SF != OF
		a.logR(false, a64Eor, rT4, rFlags, rFlags, a64LSR, 4)
		a.logI(a64AndsI, a64ZR, rT4, 0x80)
	case 7: // ZF | SF != OF
		a.logR(false, a64Eor, rT4, rFlags, rFlags, a64LSR, 4)
		a.logR(false, a64Orr, rT4, rT4, rFlags, a64LSL, 1)
		a.logI(a64AndsI, a64ZR, rT4, 0x80)
	}
	if cc&1 == 0 {
		return a64NE
	}
	return a64EQ
}

// cond returns the host condition for x86 condition cc read by the current instruction: from NZCV
// when it reads them natively, otherwise from w8.
func (g *jitGen) cond(cc uint8) uint8 {
	if g.native {
		c, _ := a64Cond(g.mode, cc)
		return c
	}
	return g.condFromFlags(cc)
}

// end leaves the block for target: chained to its translation when there is one.
func (g *jitGen) end(target uint32) {
	if t := g.j.byEIP[target]; t != nil {
		g.jmpAbs(t.head)
		return
	}
	stub := g.a.newLabel()
	g.ends = append(g.ends, jitChain{label: stub, at: g.a.pc(), target: target})
	g.a.b(stub)
}

func (g *jitGen) jmpAbs(to uintptr) {
	g.absFix = append(g.absFix, jitAbs{g.a.pc(), to})
	g.a.emit(0x14000000)
}

// endReg leaves the block for the guest address in register r: through the lookup table.
func (g *jitGen) endReg(r int) {
	a := &g.a
	g.cpuLS(2, a64St, r, offEIP)
	a.emit(0x1B000000 | uint32(rHash)<<16 | 31<<10 | uint32(r)<<5 | rT4) // mul w16, r, w4
	a.lsrI(rT4, rT4, 32-jitTableBits)
	a.addR(true, a64Add, rT4, rTab, rT4, a64LSL, 4)
	a.ldp(rF1, rF2, rT4, 0)
	a.addR(false, a64Subs, a64ZR, rF1, r, a64LSL, 0)
	a.bcond(a64NE, g.endStub)
	a.br(rF2)
}

func (g *jitGen) exit(reason uint32) {
	g.a.movI(rF1, reason)
	g.cpuLS(2, a64St, rF1, offExit)
	g.jmpAbs(g.j.epilogue)
}

// jitTranslate emits translated code for jb into j's buffer.
func jitTranslate(c *CPU, j *jitCode, jb *jitBlock) bool {
	n := len(jb.insts)
	// A dry run finds the instructions that can leave for the interpreter (and whether any is
	// translated at all); then which flag results can be observed.
	dry := newJitGen(c, j, jb, nil)
	if dry.body() == 0 {
		return false
	}
	jb.cost = JITCost{}
	exits := make([]bool, n)
	for i := range exits {
		exits[i] = dry.slow[i] >= 0
	}
	g := newJitGen(c, j, jb, jitCaptures(jb.insts, exits, func(i, k int) bool {
		cc := jitFlagFx(&jb.insts[k]).cc
		_, ok := a64Cond(dry.modes[i], cc)
		return dry.hostOK[i] && ok && ccFlags(cc)&^jitFlagFx(&jb.insts[i]).value == 0
	}))
	g.body()
	a := &g.a
	jb.cost.Insts = uint64(n)
	jb.cost.Bytes = uint64(4 * a.pc())

	// exits
	a.bind(g.limitExit)
	a.addI(true, a64Add, rBudget, rBudget, uint32(jb.steps)) // not run: give the charge back
	a.movI(rF1, jb.eip)
	g.cpuLS(2, a64St, rF1, offEIP)
	g.exit(jitExitLimit)
	a.bind(g.endStub)
	g.exit(jitExitEnd)
	if g.changed >= 0 {
		a.bind(g.changed)
		a.movI(rF1, jb.eip)
		g.cpuLS(2, a64St, rF1, offEIP)
		g.exit(jitExitEnd)
	}
	slowExit := a.newLabel() // w13 = the instruction's index
	a.bind(slowExit)
	g.cpuLS(2, a64St, rF1, offEIdx)
	a.movI(rF1, jb.id)
	g.cpuLS(2, a64St, rF1, offEBlk)
	g.exit(jitExitSlow)
	for i := range jb.insts {
		if g.slow[i] >= 0 {
			jb.cost.Exits++
			a.bind(g.slow[i])
			a.movI(rF1, uint32(i))
			a.b(slowExit)
		}
	}
	// Resuming after the interpreter ran the previous instruction, NZCV are stale: an instruction
	// reading them is entered through a copy of it reading w8 instead.
	resumeAt := make([]int, n) // label, or -1: enter at the instruction's code
	for i := range resumeAt {
		resumeAt[i] = -1
		if i == 0 || g.slow[i-1] < 0 || !g.natives[i] {
			continue
		}
		resumeAt[i] = a.newLabel()
		a.bind(resumeAt[i])
		g.idx, g.native, g.start = i, false, jb.starts[i]
		g.hostFrom = -1
		g.inst(&jb.insts[i])
		if i+1 < n {
			next := a.newLabel()
			a.labels[next] = g.resume[i+1]
			a.b(next)
		}
	}
	for _, e := range g.ends {
		a.bind(e.label)
		a.movI(rF1, e.target)
		g.cpuLS(2, a64St, rF1, offEIP)
		g.exit(jitExitEnd)
	}
	a.resolve()

	size := 4 * a.pc()
	if j.coldAt-j.used < size {
		return false
	}
	at := uintptr(unsafe.Pointer(&j.buf[0])) + uintptr(j.used)
	for _, f := range g.absFix {
		a.w[f.at] = a64PatchBranch(a.w[f.at], a64Fix26, int(int64(f.to-(at+uintptr(4*f.at)))/4))
	}
	jitWriteBegin()
	for i, w := range a.w {
		binary.LittleEndian.PutUint32(j.buf[j.used+4*i:], w)
	}
	jitWriteEnd(at, size)
	jb.head = at
	jb.resume = make([]uintptr, n)
	for i, r := range g.resume {
		if resumeAt[i] >= 0 {
			r = a.labels[resumeAt[i]]
		}
		jb.resume[i] = at + uintptr(4*r)
	}
	for _, e := range g.ends {
		j.pending[e.target] = append(j.pending[e.target], at+uintptr(4*e.at))
	}
	j.used += (size + 15) &^ 15
	j.Stats.Bytes += uint64(size)
	return true
}

func newJitGen(c *CPU, j *jitCode, jb *jitBlock, capture []bool) *jitGen {
	g := &jitGen{c: c, j: j, jb: jb, capture: capture, hostFrom: -1, mode: fmNone}
	n := len(jb.insts)
	if g.capture == nil { // the dry run: compute every flag result
		g.capture = make([]bool, n)
		for i := range g.capture {
			g.capture[i] = true
		}
	}
	g.slow = make([]int, n)
	for i := range g.slow {
		g.slow[i] = -1
	}
	g.resume = make([]int, n)
	g.natives = make([]bool, n)
	g.hostOK = make([]bool, n)
	g.modes = make([]int, n)
	for i := range g.modes {
		g.modes[i] = fmNone
	}
	g.endStub = g.a.newLabel()
	g.limitExit = g.a.newLabel()
	g.changed = -1
	return g
}

// codeCheck: a self-checking block (jitBlock.code) first compares its instruction bytes with
// memory; if they changed it leaves for Go (which rebuilds the block) before charging its steps.
func (g *jitGen) codeCheck() {
	a := &g.a
	code := g.jb.code
	g.changed = a.newLabel()
	a.movI(rEA, g.jb.eip)
	for _, ch := range jitCodeChunks(len(code)) {
		off, size := ch[0], ch[1]
		r := rEA
		if off > 0 {
			a.addI(false, a64Add, rT1, rEA, uint32(off))
			r = rT1
		}
		a.ldstR(a64Size(size), a64Ld, false, rF1, rMem, r)
		switch size {
		case 8:
			a.movI64(rF2, binary.LittleEndian.Uint64(code[off:]))
		case 4:
			a.movI(rF2, binary.LittleEndian.Uint32(code[off:]))
		default:
			a.movI(rF2, uint32(code[off]))
		}
		a.addR(size == 8, a64Subs, a64ZR, rF1, rF2, a64LSL, 0)
		a.bcond(a64NE, g.changed)
	}
}

// body translates the block's instructions (the head with the step-limit check first); it
// returns how many were translated.
func (g *jitGen) body() int {
	a := &g.a
	jb := g.jb
	if len(jb.code) > 0 {
		g.codeCheck()
	}
	a.addI(true, a64Subs, rBudget, rBudget, uint32(jb.steps))
	a.bcond(a64CC, g.limitExit) // it would pass the limit
	if jitProf {
		if jb.runs == nil {
			jb.runs = new(uint64)
		}
		a.movI64(rT4, uint64(uintptr(unsafe.Pointer(jb.runs))))
		a.ldstU(3, a64Ld, false, rF1, rT4, 0)
		a.addI(true, a64Add, rF1, rF1, 1)
		a.ldstU(3, a64St, false, rF1, rT4, 0)
	}
	native := 0
	g.start = jb.eip
	for i := range jb.insts {
		d := &jb.insts[i]
		g.idx = i
		g.resume[i] = a.pc()
		f := jitFlagFx(d)
		g.native = false
		if f.reads && f.native && i > 0 && g.hostFrom == i-1 && ccFlags(f.cc)&^jitFlagFx(&jb.insts[i-1]).value == 0 {
			_, g.native = a64Cond(g.mode, f.cc)
		}
		g.natives[i] = g.native
		if !f.writes || f.dynamic {
			g.hostFrom = -1
		}
		if g.inst(d) {
			native++
		} else {
			a.b(g.slowLabel())
		}
		g.hostOK[i] = g.hostFrom == i && g.mode != fmNone
		g.start += uint32(d.len)
		if isFused(d.kind) {
			g.start += uint32(d.len2)
		}
	}
	last := &jb.insts[len(jb.insts)-1]
	if !endsBlock(last.kind) && !isFused(last.kind) {
		g.end(g.start) // the block stopped before an uncached instruction or at blockMax
	}
	return native
}

// inst translates one instruction; false if it isn't translated (the interpreter runs it). It
// returns false only before emitting anything.
func (g *jitGen) inst(d *dInst) bool {
	a := &g.a
	next := g.start + uint32(d.len)
	adcSbb := d.aux == 2 || d.aux == 3
	switch d.kind {
	case dNop:
	case dMovRImm32:
		a.movI(gr(d.reg), d.imm)
	case dMovLoad32R:
		a.mov(gr(d.reg), gr(d.rm))
	case dMovStore32R:
		a.mov(gr(d.rm), gr(d.reg))
	case dMovLoad32:
		if !d.isMem {
			a.mov(gr(d.reg), gr(d.rm))
			break
		}
		g.ea(d, rEA)
		g.chkLoad(4)
		g.guest(2, a64Ld, false, gr(d.reg))
	case dMovStore32:
		if !d.isMem {
			a.mov(gr(d.rm), gr(d.reg))
			break
		}
		g.ea(d, rEA)
		g.chkStore(rEA)
		g.guest(2, a64St, false, gr(d.reg))
	case dMovImm32:
		if !d.isMem {
			a.movI(gr(d.rm), d.imm)
			break
		}
		g.ea(d, rEA)
		g.chkStore(rEA)
		a.movI(rT1, d.imm)
		g.guest(2, a64St, false, rT1)
	case dLea:
		g.ea(d, gr(d.reg))

	case dAluRMR32, dAluRMR32R, dGrp1RM32, dGrp1RM32R:
		ra := g.loadRM(d, 4, d.aux != 7)
		rb, imm := -1, d.imm
		if d.kind == dAluRMR32 || d.kind == dAluRMR32R {
			rb = gr(d.reg)
		}
		g.alu32(int(d.aux), ra, rb, imm)
		if d.aux != 7 {
			g.storeRM(d, 4)
		}
	case dAluRRM32, dAluRRM32R:
		rb := g.loadRM(d, 4, false)
		g.alu32(int(d.aux), gr(d.reg), rb, 0)
		if d.aux != 7 {
			a.mov(gr(d.reg), rT2)
		}
	case dAluEAXImm:
		g.alu32(int(d.aux), gr(0), -1, d.imm)
		if d.aux != 7 {
			a.mov(gr(0), rT2)
		}
	case dGrp1RM8, dAlu8RMR, dAlu8RRM, dAluALImm, dGrp1RM16, dAlu16RMR, dAlu16RRM:
		if adcSbb {
			return false
		}
		size, width := 1, uint32(8)
		if d.kind == dGrp1RM16 || d.kind == dAlu16RMR || d.kind == dAlu16RRM {
			size, width = 2, 16
		}
		var ra, rb int
		switch d.kind {
		case dGrp1RM8, dGrp1RM16:
			ra = g.loadRM(d, size, d.aux != 7)
			a.movI(rT3, d.imm&(1<<width-1))
			rb = rT3
		case dAlu8RMR, dAlu16RMR:
			ra = g.loadRM(d, size, d.aux != 7)
			rb = g.reg(d.reg, size, rT3)
		case dAlu8RRM, dAlu16RRM:
			rb = g.loadRM(d, size, false)
			if rb == rT1 {
				a.mov(rT3, rT1)
				rb = rT3
			}
			ra = g.reg(d.reg, size, rT1)
		case dAluALImm:
			g.get8(0, rT1)
			ra = rT1
			a.movI(rT3, d.imm&0xFF)
			rb = rT3
		}
		g.aluN(int(d.aux), width, ra, rb)
		if d.aux == 7 {
			break
		}
		switch d.kind {
		case dAlu8RRM, dAlu16RRM:
			g.putReg(d.reg, size, rT2)
		case dAluALImm:
			g.put8(0, rT2)
		default:
			g.storeRM(d, size)
		}
	case dTestRMR32:
		ra := g.loadRM(d, 4, false)
		a.logR(false, a64Ands, rT2, ra, gr(d.reg), a64LSL, 0)
		g.produceLogic()
	case dTestRMI8, dTestRMR8, dTestALImm, dTest16:
		g.testN(d)

	case dIncR, dDecR, dIncRM32, dDecRM32:
		op := a64Adds
		if d.kind == dDecR || d.kind == dDecRM32 {
			op = a64Subs
		}
		var ra int
		if d.kind == dIncR || d.kind == dDecR {
			ra = gr(d.reg)
		} else {
			ra = g.loadRM(d, 4, true)
		}
		a.addI(false, op, rT2, ra, 1)
		g.produce(fmNoC, flagsArith&^FlagCF, func() {
			g.nzcv(fmNoC)
			g.parity(rT2)
			g.af(ra, -1, 1, rT2)
		})
		if d.kind == dIncR || d.kind == dDecR {
			a.mov(gr(d.reg), rT2)
		} else {
			g.storeRM(d, 4)
		}
	case dNot32:
		ra := g.loadRM(d, 4, true)
		a.logR(false, a64Orn, rT2, a64ZR, ra, a64LSL, 0)
		g.storeRM(d, 4)
	case dNeg32:
		ra := g.loadRM(d, 4, true)
		a.addR(false, a64Subs, rT2, a64ZR, ra, a64LSL, 0)
		g.produce(fmSub, flagsArith, func() {
			g.nzcv(fmSub)
			g.parity(rT2)
			g.af(a64ZR, ra, 0, rT2)
		})
		g.storeRM(d, 4)

	case dPushR:
		g.push(gr(d.reg))
	case dPushRM32:
		g.push(g.loadRM(d, 4, false))
	case dPopR:
		a.mov(rEA, gr(4))
		g.chkLoad(4)
		g.guest(2, a64Ld, false, rT1)
		a.addI(false, a64Add, gr(4), gr(4), 4)
		a.mov(gr(d.reg), rT1)

	case dMovzx8, dMovsx8:
		if d.isMem {
			g.ea(d, rEA)
			g.chkLoad(1)
			opc := a64Ld
			if d.kind == dMovsx8 {
				opc = a64LdS
			}
			g.guest(0, opc, false, gr(d.reg))
			break
		}
		if d.kind == dMovzx8 {
			g.get8(d.rm, gr(d.reg))
		} else if d.rm < 4 {
			a.sbfm(gr(d.reg), gr(d.rm), 0, 7)
		} else {
			a.sbfm(gr(d.reg), gr(d.rm-4), 8, 15)
		}
	case dMovzx16, dMovsx16:
		if d.isMem {
			g.ea(d, rEA)
			g.chkLoad(2)
			opc := a64Ld
			if d.kind == dMovsx16 {
				opc = a64LdS
			}
			g.guest(1, opc, false, gr(d.reg))
			break
		}
		if d.kind == dMovzx16 {
			a.ubfx(gr(d.reg), gr(d.rm), 0, 16)
		} else {
			a.sbfm(gr(d.reg), gr(d.rm), 0, 15)
		}
	case dCmov:
		src := g.loadRM(d, 4, false)
		cond := g.cond(d.aux)
		a.csel(gr(d.reg), src, gr(d.reg), cond)
	case dSetcc:
		if d.isMem {
			g.ea(d, rEA)
			g.chkStore(rEA)
		}
		a.cset(rT2, g.cond(d.aux))
		g.storeRM(d, 1)

	case dMovStore8:
		g.get8(d.reg, rT2)
		if d.isMem {
			g.ea(d, rEA)
			g.chkStore(rEA)
		}
		g.storeRM(d, 1)
	case dMovLoad8:
		g.put8(d.reg, g.loadRM(d, 1, false))
	case dMovImm8:
		if d.isMem {
			g.ea(d, rEA)
			g.chkStore(rEA)
		}
		a.movI(rT2, d.imm&0xFF)
		g.storeRM(d, 1)
	case dMovR8Imm:
		a.movI(rT2, d.imm&0xFF)
		g.put8(d.reg, rT2)
	case dNot8:
		ra := g.loadRM(d, 1, true)
		a.logI(a64EorI, rT2, ra, 0xFF)
		g.storeRM(d, 1)
	case dNeg8:
		ra := g.loadRM(d, 1, true)
		a.lslI(rT4, ra, 24)
		a.addR(false, a64Subs, rT2, a64ZR, rT4, a64LSL, 0)
		a.lsrI(rT2, rT2, 24)
		g.produce(fmSub, flagsArith, func() {
			g.nzcv(fmSub)
			g.parity(rT2)
			g.af(a64ZR, ra, 0, rT2)
		})
		g.storeRM(d, 1)

	case dMov16Store:
		if d.isMem {
			g.ea(d, rEA)
			g.chkStore(rEA)
			g.guest(1, a64St, false, gr(d.reg))
		} else {
			a.bfi(gr(d.rm), gr(d.reg), 0, 16)
		}
	case dMov16Load:
		g.putReg(d.reg, 2, g.loadRM(d, 2, false))
	case dMov16Imm:
		if d.isMem {
			g.ea(d, rEA)
			g.chkStore(rEA)
		}
		a.movI(rT2, d.imm&0xFFFF)
		g.storeRM(d, 2)

	case dShift32:
		return g.shiftImm(d)
	case dShift32CL:
		return g.shiftCL(d)
	case dImul32, dImulRRM32:
		src := g.loadRM(d, 4, false)
		if d.kind == dImul32 {
			a.movI(rT3, d.imm)
			a.smull(rT2, src, rT3)
		} else {
			a.smull(rT2, gr(d.reg), src)
		}
		a.addX(true, a64Subs, a64ZR, rT2, rT2, 6, 0) // cmp x2, w2, sxtw: NE if it overflowed
		g.produce(fmNone, FlagCF|FlagOF, func() {
			a.cset(rF1, a64NE)
			a.logR(false, a64Orr, rF1, rF1, rF1, a64LSL, 11)
		})
		a.mov(gr(d.reg), rT2)

	case dBswap:
		a.rev(gr(d.reg), gr(d.reg))
	case dCmpxchg8b: // EDX:EAX vs m64; equal: m64 = ECX:EBX, otherwise EDX:EAX = m64
		g.ea(d, rEA)
		g.chkStore(rEA)
		g.guest(3, a64Ld, false, rT1)
		a.mov(rT2, gr(0))
		a.bfm64(rT2, gr(2), 32, 31)
		a.addR(true, a64Subs, a64ZR, rT1, rT2, a64LSL, 0)
		ne, done := a.newLabel(), a.newLabel()
		a.bcond(a64NE, ne)
		a.mov(rT2, gr(3))
		a.bfm64(rT2, gr(1), 32, 31)
		g.guest(3, a64St, false, rT2)
		a.b(done)
		a.bind(ne)
		a.mov(gr(0), rT1)
		a.lsrI64(rT1, rT1, 32)
		a.mov(gr(2), rT1)
		a.bind(done)
		g.produce(fmNone, FlagZF, func() {
			a.cset(rF1, a64EQ)
			a.lslI(rF1, rF1, 6)
		})

	// Control transfers end the block.
	case dJmp:
		g.end(d.imm)
	case dJcc:
		taken := a.newLabel()
		a.bcond(g.cond(d.aux), taken)
		g.end(next)
		a.bind(taken)
		g.end(d.imm)
	case dCall:
		a.addI(false, a64Sub, rEA, gr(4), 4)
		g.chkStore(rEA)
		a.movI(rT1, next)
		g.guest(2, a64St, false, rT1)
		a.mov(gr(4), rEA)
		g.end(d.imm)
	case dRet, dRetImm:
		a.mov(rEA, gr(4))
		g.chkLoad(4)
		g.guest(2, a64Ld, false, rT1)
		if d.kind == dRet {
			a.addI(false, a64Add, gr(4), gr(4), 4)
		} else {
			g.addConst(gr(4), gr(4), 4+d.imm)
		}
		g.endReg(rT1)
	case dCallInd, dJmpInd:
		t := g.loadRM(d, 4, false)
		if t != rT1 {
			a.mov(rT1, t)
		}
		if d.kind == dCallInd {
			a.addI(false, a64Sub, rT3, gr(4), 4)
			g.chkStore(rT3)
			a.movI(rT2, next)
			a.ldstR(2, a64St, false, rT2, rMem, rT3)
			a.mov(gr(4), rT3)
		}
		g.endReg(rT1)
	case dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dCmpEAXJcc, dTestRMRJcc, dTestRMIJcc, dCmpRMI8Jcc:
		g.fused(d, next)
	default:
		return g.sse(d)
	}
	return true
}

// reg returns guest register r's low size bytes zero-extended (in tmp unless size is 4).
func (g *jitGen) reg(r uint8, size int, tmp int) int {
	switch size {
	case 1:
		g.get8(r, tmp)
	case 2:
		g.a.ubfx(tmp, gr(r), 0, 16)
	default:
		return gr(r)
	}
	return tmp
}

// putReg writes the low size bytes of src to guest register r.
func (g *jitGen) putReg(r uint8, size int, src int) {
	switch size {
	case 1:
		g.put8(r, src)
	case 2:
		g.a.bfi(gr(r), src, 0, 16)
	default:
		g.a.mov(gr(r), src)
	}
}

// produceLogic: the flags of a logical result in w2, NZCV set by ANDS.
func (g *jitGen) produceLogic() {
	g.produce(fmLogic, flagsArith, func() {
		g.nzcv(fmLogic)
		g.parity(rT2)
	})
}

// testN: test r/m8, imm8 / r/m8, r8 / al, imm8 / r/m16, r16.
func (g *jitGen) testN(d *dInst) {
	a := &g.a
	size, width := 1, uint32(8)
	if d.kind == dTest16 {
		size, width = 2, 16
	}
	var ra int
	if d.kind == dTestALImm {
		g.get8(0, rT1)
		ra = rT1
	} else {
		ra = g.loadRM(d, size, false)
	}
	rb := rT3
	if d.kind == dTestRMR8 || d.kind == dTest16 {
		rb = g.reg(d.reg, size, rT3)
	} else {
		a.movI(rT3, d.imm&0xFF)
	}
	a.logR(false, a64And, rT2, ra, rb, a64LSL, 0)
	a.lslI(rT4, rT2, 32-width)
	a.logR(false, a64Ands, a64ZR, rT4, rT4, a64LSL, 0)
	g.produceLogic()
}

// push stores register val at ESP-4.
func (g *jitGen) push(val int) {
	a := &g.a
	a.addI(false, a64Sub, rT3, gr(4), 4)
	g.chkStore(rT3)
	a.ldstR(2, a64St, false, val, rMem, rT3)
	a.mov(gr(4), rT3)
}

// fused: a compare or test and the Jcc after it.
func (g *jitGen) fused(d *dInst, next uint32) {
	a := &g.a
	switch d.kind {
	case dCmpRMRJcc:
		g.alu32(7, g.loadRM(d, 4, false), gr(d.reg), 0)
	case dCmpRRMJcc:
		g.alu32(7, gr(d.reg), g.loadRM(d, 4, false), 0)
	case dCmpRMIJcc:
		g.alu32(7, g.loadRM(d, 4, false), -1, d.imm)
	case dCmpEAXJcc:
		g.alu32(7, gr(0), -1, d.imm)
	case dTestRMRJcc:
		a.logR(false, a64Ands, rT2, g.loadRM(d, 4, false), gr(d.reg), a64LSL, 0)
		g.produceLogic()
	case dTestRMIJcc:
		g.testN(d)
	case dCmpRMI8Jcc:
		ra := g.loadRM(d, 1, false)
		a.movI(rT3, d.imm&0xFF)
		g.aluN(7, 8, ra, rT3)
	}
	cond, ok := a64Cond(g.mode, d.cond)
	if !ok { // w8 holds the flags (the block's last instruction's always do)
		cond = g.condFromFlags(d.cond)
	}
	taken := a.newLabel()
	a.bcond(cond, taken)
	g.end(next + uint32(d.len2))
	a.bind(taken)
	g.end(d.target)
}

// shiftImm: shift or rotate r/m32 by an immediate count.
func (g *jitGen) shiftImm(d *dInst) bool {
	a := &g.a
	cnt := uint32(d.imm) & 0x1F
	v := g.loadRM(d, 4, true)
	if cnt == 0 {
		return true // no change (a store of the same value to an ordinary page)
	}
	of := uint32(0)
	if cnt == 1 {
		of = FlagOF
	}
	switch d.aux {
	case 4, 6, 5, 7: // shl, sal, shr, sar
		var cfBit uint32
		switch d.aux {
		case 4, 6:
			a.lslI(rT2, v, cnt)
			cfBit = 32 - cnt
		case 5:
			a.lsrI(rT2, v, cnt)
			cfBit = cnt - 1
		case 7:
			a.asrI(rT2, v, cnt)
			cfBit = cnt - 1
		}
		a.logR(false, a64Ands, a64ZR, rT2, rT2, a64LSL, 0)
		g.produce(fmNone, flagsArith, func() {
			g.nzcv(fmLogic) // ZF, SF
			g.parity(rT2)
			a.ubfx(rT4, v, cfBit, 1)
			a.logR(false, a64Orr, rF1, rF1, rT4, a64LSL, 0)
			switch {
			case cnt != 1 || d.aux == 7:
			case d.aux == 5: // OF = the operand's sign
				a.lsrI(rF2, v, 31)
				a.logR(false, a64Orr, rF1, rF1, rF2, a64LSL, 11)
			default: // OF = CF ^ the result's sign
				a.logR(false, a64Eor, rF2, rT4, rT2, a64LSR, 31)
				a.logR(false, a64Orr, rF1, rF1, rF2, a64LSL, 11)
			}
		})
	case 0: // rol: CF = the result's bit 0; OF (count 1) = CF ^ its sign
		a.rorI(rT2, v, 32-cnt)
		g.produce(fmNone, FlagCF|of, func() {
			a.logI(a64AndI, rF1, rT2, 1)
			if cnt == 1 {
				a.logR(false, a64Eor, rF2, rF1, rT2, a64LSR, 31)
				a.logR(false, a64Orr, rF1, rF1, rF2, a64LSL, 11)
			}
		})
	case 1: // ror: CF = the result's sign
		a.rorI(rT2, v, cnt)
		g.produce(fmNone, FlagCF, func() { a.lsrI(rF1, rT2, 31) })
	case 2, 3: // rcl, rcr: a 33-bit rotation of CF:value
		n := cnt
		if d.aux == 3 {
			n = 33 - cnt
		}
		a.mov(rT3, v)
		a.logI(a64AndI, rT4, rFlags, 1)
		a.logR(true, a64Orr, rT3, rT3, rT4, a64LSL, 32)
		a.lslI64(rT4, rT3, n)
		a.lsrI64(rT3, rT3, 33-n)
		a.logR(true, a64Orr, rT4, rT4, rT3, a64LSL, 0)
		a.mov(rT2, rT4)
		g.produce(fmNone, FlagCF, func() {
			a.lsrI64(rF1, rT4, 32)
			a.logI(a64AndI, rF1, rF1, 1)
		})
	}
	g.storeRM(d, 4)
	return true
}

// shiftCL: shift or rotate r/m32 by CL (the flags it sets depend on the count: merged here).
func (g *jitGen) shiftCL(d *dInst) bool {
	if d.aux == 2 || d.aux == 3 {
		return false // rcl, rcr: the interpreter
	}
	a := &g.a
	v := g.loadRM(d, 4, true)
	done := a.newLabel()
	a.logI(a64AndI, rT3, gr(1), 0x1F)
	a.cbz(rT3, done) // count 0: nothing changes
	a.addI(false, a64Subs, a64ZR, rT3, 1)
	a.cset(rF2, a64EQ) // w14 = count == 1
	switch d.aux {
	case 4, 6, 5, 7:
		switch d.aux {
		case 4, 6:
			a.op2(a64Lslv, rT2, v, rT3)
			a.addR(false, a64Sub, rT4, a64ZR, rT3, a64LSL, 0)
			a.op2(a64Lsrv, rT4, v, rT4) // CF: the operand's bit 32-count
		case 5:
			a.op2(a64Lsrv, rT2, v, rT3)
			a.addI(false, a64Sub, rT4, rT3, 1)
			a.op2(a64Lsrv, rT4, v, rT4)
		case 7:
			a.op2(a64Asrv, rT2, v, rT3)
			a.addI(false, a64Sub, rT4, rT3, 1)
			a.op2(a64Asrv, rT4, v, rT4)
		}
		a.logI(a64AndI, rT4, rT4, 1)
		// OF (count 1 only): shl CF ^ the result's sign, shr the operand's sign, sar 0
		switch d.aux {
		case 4, 6:
			a.logR(false, a64Eor, rT3, rT4, rT2, a64LSR, 31)
		case 5:
			a.lsrI(rT3, v, 31)
		case 7:
			a.movI(rT3, 0)
		}
		a.logR(false, a64And, rT3, rT3, rF2, a64LSL, 0)
		a.logR(false, a64Ands, a64ZR, rT2, rT2, a64LSL, 0)
		g.nzcv(fmLogic)
		g.parity(rT2)
		a.logR(false, a64Orr, rF1, rF1, rT4, a64LSL, 0)
		a.logR(false, a64Orr, rF1, rF1, rT3, a64LSL, 11)
		a.movI(rF2, flagsArith)
	case 0: // rol: CF; OF for a count of 1
		a.addR(false, a64Sub, rT4, a64ZR, rT3, a64LSL, 0)
		a.op2(a64Rorv, rT2, v, rT4)
		a.logI(a64AndI, rF1, rT2, 1)
		a.logR(false, a64Eor, rT3, rF1, rT2, a64LSR, 31)
		a.logR(false, a64And, rT3, rT3, rF2, a64LSL, 0)
		a.logR(false, a64Orr, rF1, rF1, rT3, a64LSL, 11)
		a.lslI(rF2, rF2, 11) // clear: CF, and OF for a count of 1
		a.logI(a64OrrI, rF2, rF2, FlagCF)
	case 1: // ror: CF
		a.op2(a64Rorv, rT2, v, rT3)
		a.lsrI(rF1, rT2, 31)
		a.movI(rF2, FlagCF)
	}
	a.logR(false, a64Bic, rFlags, rFlags, rF2, a64LSL, 0)
	a.logR(false, a64Orr, rFlags, rFlags, rF1, a64LSL, 0)
	g.storeRM(d, 4)
	a.bind(done)
	return true
}

// sse translates the SSE forms.
func (g *jitGen) sse(d *dInst) bool {
	a := &g.a
	switch d.kind {
	case dMovssLoad, dMovsdLoad:
		size := 2
		if d.kind == dMovsdLoad {
			size = 3
		}
		if d.isMem { // the rest of the register is zeroed
			g.ea(d, rEA)
			g.chkLoad(1 << size)
			g.guest(size, a64Ld, true, 0)
			g.xmm(4, a64St, true, 0, d.reg, 0)
		} else {
			g.xmm(size, a64Ld, false, rT1, d.rm, 0)
			g.xmm(size, a64St, false, rT1, d.reg, 0)
		}
	case dMovssStore, dMovsdStore, dMovdFromX:
		size := 2
		if d.kind == dMovsdStore {
			size = 3
		}
		g.xmm(size, a64Ld, false, rT1, d.reg, 0)
		switch {
		case d.isMem:
			g.ea(d, rEA)
			g.chkStore(rEA)
			g.guest(size, a64St, false, rT1)
		case d.kind == dMovdFromX:
			a.mov(gr(d.rm), rT1)
		default:
			g.xmm(size, a64St, false, rT1, d.rm, 0)
		}
	case dMovdToX:
		src := g.loadRM(d, 4, false)
		a.mov(rT1, src) // zero-extended
		g.xmm(3, a64St, false, rT1, d.reg, 0)
		g.xmm(3, a64St, false, a64ZR, d.reg, 8)
	case dMovapsLoad:
		g.src(d, 4, 1)
		g.xmm(4, a64St, true, 1, d.reg, 0)
	case dMovapsStore:
		g.xmm(4, a64Ld, true, 0, d.reg, 0)
		if d.isMem {
			g.ea(d, rEA)
			g.chkStore(rEA)
			g.guest(4, a64St, true, 0)
		} else {
			g.xmm(4, a64St, true, 0, d.rm, 0)
		}
	case dArithSS, dArithSS2, dArithSD:
		double := d.kind == dArithSD
		size := 2
		if double {
			size = 3
		}
		g.src(d, size, 1)
		g.xmm(size, a64Ld, true, 0, d.reg, 0)
		switch d.aux {
		case 0x58, 0x59: // add, mul: a NaN first operand is the result (quieted), as on x86
			a.fcmp(double, 0, 0)
			a.fcsel(double, 1, 0, 1, a64VS)
			op := uint32(a64Fadd)
			if d.aux == 0x59 {
				op = a64Fmul
			}
			a.fop(op, double, 0, 0, 1)
		case 0x5C:
			a.fop(a64Fsub, double, 0, 0, 1)
		case 0x5E:
			a.fop(a64Fdiv, double, 0, 0, 1)
		case 0x5D: // min: a < b ? a : b
			a.fcmp(double, 0, 1)
			a.fcsel(double, 0, 0, 1, a64MI)
		case 0x5F: // max: a > b ? a : b
			a.fcmp(double, 0, 1)
			a.fcsel(double, 0, 0, 1, a64GT)
		case 0x51: // sqrt (singles as the interpreter does: through a double)
			if double {
				a.fsqrt(true, 0, 1)
			} else {
				a.fcvtS2D(1, 1)
				a.fsqrt(true, 1, 1)
				a.fcvtD2S(0, 1)
			}
		default:
			panic("jit: SSE operation")
		}
		g.xmm(size, a64St, true, 0, d.reg, 0)
	case dArithPS, dLogicPS, dXorps:
		g.src(d, 4, 1)
		g.xmm(4, a64Ld, true, 0, d.reg, 0)
		res := 0
		switch {
		case d.kind == dXorps:
			a.vop(a64VEor, 0, 0, 1)
		case d.kind == dLogicPS:
			switch d.aux {
			case 0x54:
				a.vop(a64VAnd, 0, 0, 1)
			case 0x55:
				a.vop(a64VBic, 0, 1, 0)
			case 0x56:
				a.vop(a64VOrr, 0, 0, 1)
			}
		default:
			switch d.aux {
			case 0x58, 0x59:
				a.vop(a64VFcmeq, 2, 0, 0)
				a.vop(a64VBif, 1, 0, 2) // a NaN: b = a
				op := uint32(a64VFadd)
				if d.aux == 0x59 {
					op = a64VFmul
				}
				a.vop(op, 0, 0, 1)
			case 0x5C:
				a.vop(a64VFsub, 0, 0, 1)
			case 0x5E:
				a.vop(a64VFdiv, 0, 0, 1)
			case 0x51:
				a.vfsqrt(0, 1)
			case 0x5D:
				a.vop(a64VFcmgt, 2, 1, 0) // b > a
				a.vop(a64VBsl, 2, 0, 1)
				res = 2
			case 0x5F:
				a.vop(a64VFcmgt, 2, 0, 1)
				a.vop(a64VBsl, 2, 0, 1)
				res = 2
			}
		}
		g.xmm(4, a64St, true, res, d.reg, 0)
	case dCvtSS2SD:
		g.src(d, 2, 1)
		a.fcvtS2D(0, 1)
		g.xmm(3, a64St, true, 0, d.reg, 0)
	case dCvtSD2SS:
		g.src(d, 3, 1)
		a.fcvtD2S(0, 1)
		g.xmm(2, a64St, true, 0, d.reg, 0)
	case dCvttSS2SI, dCvttSD2SI: // out of range or NaN: 0x80000000
		double := d.kind == dCvttSD2SI
		size := 2
		if double {
			size = 3
		}
		g.src(d, size, 1)
		a.fcvtzs64(double, rT1, 1)
		a.addX(true, a64Subs, a64ZR, rT1, rT1, 6, 0) // cmp x1, w1, sxtw
		a.movI(rT4, 0x80000000)
		a.csel(rT1, rT1, rT4, a64EQ)
		a.fcmp(double, 1, 1)
		a.csel(gr(d.reg), rT1, rT4, a64VC)
	case dCvtSI2SS, dCvtSI2SD:
		double := d.kind == dCvtSI2SD
		src := g.loadRM(d, 4, false)
		a.scvtf(double, 0, src)
		size := 2
		if double {
			size = 3
		}
		g.xmm(size, a64St, true, 0, d.reg, 0)
	case dUcomiss, dUcomisd:
		double := d.kind == dUcomisd
		size := 2
		if double {
			size = 3
		}
		g.src(d, size, 1)
		g.xmm(size, a64Ld, true, 0, d.reg, 0)
		a.fcmp(double, 0, 1)
		g.produce(fmFcmp, flagsArith, func() { g.nzcv(fmFcmp) })
	case dFld, dFst, dFxch, dFsign, dFArith:
		g.x87(d)
	case dPshufd:
		g.src(d, 4, 1)
		for i := 0; i < 4; i++ {
			a.insS(0, i, 1, int(d.imm>>(2*i)&3))
		}
		g.xmm(4, a64St, true, 0, d.reg, 0)
	default:
		return false
	}
	return true
}

// x87: the interpreter's registers are float64s (FPUSt), indexed from FPUTop: x17 = &FPUSt[0],
// w15 = FPUTop, w16 another index. The arithmetic is the double-precision instructions Go
// compiles the interpreter's to, with sseAdd's rule for two NaNs (see the SSE forms).

// stIdx returns the register holding ST(i)'s index.
func (g *jitGen) stIdx(i uint32) int {
	if i == 0 {
		return rT3
	}
	g.a.addI(false, a64Add, rT4, rT3, i)
	g.a.logI(a64AndI, rT4, rT4, 7)
	return rT4
}

// fpuTopMove adds delta (1 pop, -1 push) to FPUTop, in w15 and memory.
func (g *jitGen) fpuTopMove(delta int) {
	if delta > 0 {
		g.a.addI(false, a64Add, rT3, rT3, 1)
	} else {
		g.a.addI(false, a64Sub, rT3, rT3, 1)
	}
	g.a.logI(a64AndI, rT3, rT3, 7)
	g.cpuLS(2, a64St, rT3, offFPUTop)
}

func (g *jitGen) x87(d *dInst) {
	a := &g.a
	m32 := d.kind == dFld && (d.aux == fldM32 || d.aux == fldI32) || d.kind == dFst && d.aux == fstM32 || d.kind == dFArith && d.aux>>2 == fpuM32
	if d.isMem { // the access is checked before anything changes
		g.ea(d, rEA)
		if d.kind == dFst {
			g.chkStore(rEA)
		} else if m32 {
			g.chkLoad(4)
		} else {
			g.chkLoad(8)
		}
	}
	g.cpuLS(2, a64Ld, rT3, offFPUTop)
	a.addI(true, a64Add, rAddr, rCPU, uint32(offFPUSt))
	ld := func(v int, i uint32) { a.ldstX(3, a64Ld, true, v, rAddr, g.stIdx(i)) }
	st := func(v int, i uint32) { a.ldstX(3, a64St, true, v, rAddr, g.stIdx(i)) }
	mem := func(v int) { // the memory operand as a double in d<v>
		if m32 {
			g.guest(2, a64Ld, true, v)
			a.fcvtS2D(v, v)
		} else {
			g.guest(3, a64Ld, true, v)
		}
	}
	switch d.kind {
	case dFld:
		switch d.aux {
		case fldST:
			ld(0, uint32(d.rm))
		case fldM32, fldM64:
			mem(0)
		case fldI32:
			g.guest(2, a64Ld, false, rT1)
			a.scvtf(true, 0, rT1)
		default:
			v := uint64(0)
			if d.aux == fldOne {
				v = math.Float64bits(1)
			}
			a.movI64(rF1, v)
			g.fpuTopMove(-1)
			a.ldstX(3, a64St, false, rF1, rAddr, rT3)
			return
		}
		g.fpuTopMove(-1)
		st(0, 0)
	case dFst:
		ld(0, 0)
		switch d.aux {
		case fstST:
			if d.rm != 0 || d.imm == 0 {
				st(0, uint32(d.rm))
			}
		case fstM32:
			a.fcvtD2S(0, 0)
			g.guest(2, a64St, true, 0)
		case fstM64:
			g.guest(3, a64St, true, 0)
		}
		if d.imm != 0 {
			g.fpuTopMove(1)
		}
	case dFxch:
		ld(0, 0)
		ld(1, uint32(d.rm))
		st(1, 0)
		st(0, uint32(d.rm))
	case dFsign:
		ld(0, 0)
		if d.aux == 0 {
			a.fneg(0, 0)
		} else {
			a.fabs(0, 0)
		}
		st(0, 0)
	case dFArith:
		for k, sh := range []uint32{0, 4} {
			if src := d.imm >> sh & 15; src == fpuMem {
				mem(k)
			} else {
				ld(k, src)
			}
		}
		switch d.aux & 3 {
		case fpuAdd, fpuMul: // a NaN first operand is the result (quieted), as on x86
			a.fcmp(true, 0, 0)
			a.fcsel(true, 1, 0, 1, a64VS)
			op := uint32(a64Fadd)
			if d.aux&3 == fpuMul {
				op = a64Fmul
			}
			a.fop(op, true, 0, 0, 1)
		case fpuSub:
			a.fop(a64Fsub, true, 0, 0, 1)
		default:
			a.fop(a64Fdiv, true, 0, 0, 1)
		}
		st(0, uint32(d.reg))
		if d.imm>>8&1 != 0 {
			g.fpuTopMove(1)
		}
	}
}

// src loads d's XMM source operand (size: 2 single, 3 double, 4 all 16 bytes) into SIMD
// register v.
func (g *jitGen) src(d *dInst, size int, v int) {
	if d.isMem {
		g.ea(d, rEA)
		g.chkLoad(1 << size)
		g.guest(size, a64Ld, true, v)
		return
	}
	g.xmm(size, a64Ld, true, v, d.rm, 0)
}

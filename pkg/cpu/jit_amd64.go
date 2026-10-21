package cpu

import (
	"encoding/binary"
	"unsafe"
)

// The JIT's x86-64 backend. Guest registers live in host registers while translated code runs:
//
//	guest  EAX ECX EDX EBX ESP EBP ESI EDI
//	host   rax rcx rdx rbx r12 rbp rsi rdi
//
// so most instructions keep their encoding (and CL, AH..BH work as in the guest). r14 holds
// &Mem[0], r15 the CPU, r13 the store table; r8 holds effective addresses, r9-r11 are scratch.
// Guest flags stay in c.Flags: an instruction that sets flags runs natively and its flags are
// merged into c.Flags right after (with the interpreter's rules for flags the hardware leaves
// undefined); an instruction that reads flags loads them into the host's first.

const jitSupported = true

var hostReg = [8]int{hRAX, hRCX, hRDX, hRBX, hR12, hRBP, hRSI, hRDI}

// jitCall runs translated code: prologue saves the host registers Go needs back, loads the guest
// registers and jumps to target (jit_amd64.s).
func jitCall(prologue uintptr, c *CPU, target uintptr)

var (
	offRegs  = int32(unsafe.Offsetof(CPU{}.Regs))
	offEIP   = int32(unsafe.Offsetof(CPU{}.EIP))
	offFlags = int32(unsafe.Offsetof(CPU{}.Flags))
	offSteps = int32(unsafe.Offsetof(CPU{}.Steps))
	offXMM   = int32(unsafe.Offsetof(CPU{}.XMM))
	offJit   = int32(unsafe.Offsetof(CPU{}.jit))
	offMem   = offJit + int32(unsafe.Offsetof(jitState{}.mem))
	offStore = offJit + int32(unsafe.Offsetof(jitState{}.stores))
	offTable = offJit + int32(unsafe.Offsetof(jitState{}.table))
	offLimit = offJit + int32(unsafe.Offsetof(jitState{}.limit))
	offExit  = offJit + int32(unsafe.Offsetof(jitState{}.exit))
	offEIdx  = offJit + int32(unsafe.Offsetof(jitState{}.exitIdx))
	offEBlk  = offJit + int32(unsafe.Offsetof(jitState{}.exitBlk))
	offSave  = offJit + int32(unsafe.Offsetof(jitState{}.save))
)

const (
	flagsArith = FlagCF | FlagPF | FlagAF | FlagZF | FlagSF | FlagOF
	flagsLogic = flagsArith &^ FlagAF // logic ops: AF is 0 in the interpreter, undefined in hardware
)

func cpuReg(r int) xop  { return xm(hR15, offRegs+4*int32(r)) }
func xmmOp(r uint8) xop { return xm(hR15, offXMM+16*int32(r)) }

var memAt = xmi(hR14, hR8, 0, 0) // [r14 + r8]: guest memory at the effective address

// jitShared emits the prologue and epilogue at the start of the code buffer.
func jitShared(j *jitCode) int {
	var a x64
	saved := []int{hRBP, hRBX, hR12, hR13, hR14, hR15}
	// prologue: rdi = c, rsi = target
	for i, r := range saved {
		a.store64(xm(hRDI, offSave+8*int32(i)), r)
	}
	a.inst(0, xW, []byte{0x89}, hRDI, xr(hR15)) // mov r15, rdi
	a.inst(0, xW, []byte{0x89}, hRSI, xr(hR11)) // mov r11, rsi
	a.load64(hR14, xm(hR15, offMem))
	a.load64(hR13, xm(hR15, offStore))
	for g, h := range hostReg {
		a.load32(h, cpuReg(g))
	}
	a.jmpReg(hR11)
	epilogue := len(a.b)
	for g, h := range hostReg {
		a.store32(cpuReg(g), h)
	}
	for i, r := range saved { // r15 is last: it addresses the save area
		a.load64(r, xm(hR15, offSave+8*int32(i)))
	}
	a.ret()
	n := copy(j.buf, a.b)
	base := uintptr(unsafe.Pointer(&j.buf[0]))
	j.prologue = base
	j.epilogue = base + uintptr(epilogue)
	return (n + 15) &^ 15
}

// jitPatch points the rel32 at absolute address site to target.
func jitPatch(j *jitCode, site, target uintptr) {
	off := site - uintptr(unsafe.Pointer(&j.buf[0]))
	binary.LittleEndian.PutUint32(j.buf[off:], uint32(int32(target-(site+4))))
}

type jitGen struct {
	a       x64
	c       *CPU
	j       *jitCode
	jb      *jitBlock
	base    uintptr // where a.b[0] will be
	epi     uintptr // this buffer's epilogue
	slow    []int   // per instruction: label of its slow exit (-1: none yet)
	start   uint32  // guest address of the instruction being translated
	idx     int
	ends    []jitChain
	endStub int // label: exit with EIP already stored
}

type jitChain struct {
	label  int    // the jump to patch
	target uint32 // guest address
}

func h(g uint8) int { return hostReg[g] }

// jmpAbs jumps to an absolute address (in the code buffer).
func (g *jitGen) jmpAbs(to uintptr) {
	g.a.emit(0xE9)
	at := g.base + uintptr(len(g.a.b))
	g.a.emit32(uint32(int32(to - (at + 4))))
}

func (g *jitGen) slowLabel() int {
	if g.slow[g.idx] < 0 {
		g.slow[g.idx] = g.a.newLabel()
	}
	return g.slow[g.idx]
}

// ea computes the guest effective address into r8d.
func (g *jitGen) ea(d *dInst) {
	if d.base < 0 && d.index < 0 {
		g.a.movRI32NoFlags(hR8, d.disp)
		return
	}
	g.a.lea32(hR8, g.eaOp(d))
}

func (g *jitGen) eaOp(d *dInst) xop {
	b, i := -1, -1
	if d.base >= 0 {
		b = h(uint8(d.base))
	}
	if d.index >= 0 {
		i = h(uint8(d.index))
	}
	if b < 0 && i < 0 {
		panic("jit: eaOp without registers")
	}
	return xmi(b, i, d.scale, int32(d.disp))
}

// chkLoad sends a load at r8d to the interpreter unless it is in range (the interpreter's fast
// path: addr < ArenaSize-8, or for 16 bytes addr+16 <= ArenaSize-8).
func (g *jitGen) chkLoad(size int) {
	if size == 16 {
		g.a.alu32I(7, xr(hR8), ArenaSize-24)
		g.a.jcc(0x7, g.slowLabel()) // ja
		return
	}
	g.a.alu32I(7, xr(hR8), ArenaSize-8)
	g.a.jcc(0x3, g.slowLabel()) // jae
}

// chkStore sends a store at register r to the interpreter if it is out of range or on (or just
// before) a page holding code.
func (g *jitGen) chkStore(r int) {
	g.a.alu32I(7, xr(r), ArenaSize-16)
	g.a.jcc(0x7, g.slowLabel()) // ja
	g.a.movRR32(hR11, r)
	g.a.shiftI32(5, xr(hR11), 12)                          // shr r11d, 12
	g.a.inst(0, 0, []byte{0x80}, 7, xmi(hR13, hR11, 0, 0)) // cmp byte [r13+r11], 0
	g.a.emit(0)
	g.a.jcc(0x5, g.slowLabel()) // jne
}

// flags merges the host's flags into c.Flags: bits in value come from the host, the rest of
// clear become 0.
func (g *jitGen) flags(value, clear uint32) {
	g.a.pushfq()
	g.a.pop(hR11)
	g.merge(value, clear)
}

// merge is flags with the host's flags already in r11.
func (g *jitGen) merge(value, clear uint32) {
	g.a.alu32I(4, xr(hR11), value)            // and r11d, value
	g.a.alu32I(4, xm(hR15, offFlags), ^clear) // and [flags], ^clear
	g.a.alu32(1, xm(hR15, offFlags), hR11)    // or [flags], r11d
}

// loadFlags puts the guest's arithmetic flags into the host's.
func (g *jitGen) loadFlags() {
	g.a.load32(hR11, xm(hR15, offFlags))
	g.a.alu32I(4, xr(hR11), flagsArith)
	g.a.push(hR11)
	g.a.popfq()
}

func aluFlags(op uint8) (uint32, uint32) {
	if op == 1 || op == 4 || op == 6 { // or, and, xor
		return flagsLogic, flagsArith
	}
	return flagsArith, flagsArith
}

// get8 puts guest 8-bit register r (AL..BH) zero-extended into host register dst (r9..r11).
func (g *jitGen) get8(r uint8, dst int) {
	if r < 4 {
		g.a.movzx8(dst, xr(h(r)))
		return
	}
	g.a.movRR32(dst, h(r-4))
	g.a.shiftI32(5, xr(dst), 8)
	g.a.movzx8(dst, xr(dst))
}

// put8 stores the low byte of host register src (r9 or r10) into guest 8-bit register r.
func (g *jitGen) put8(r uint8, src int) {
	if r < 4 {
		g.a.store8(xr(h(r)), src)
		return
	}
	hr := h(r - 4)
	g.a.alu32I(4, xr(hr), 0xFFFF00FF)
	g.a.movzx8(hR11, xr(src))
	g.a.shiftI32(4, xr(hR11), 8)
	g.a.alu32(1, xr(hr), hR11)
}

// rm8 is the host operand for guest 8-bit r/m operand d when it's a register (legacy encoding:
// AL..BL and AH..BH, usable only in instructions without a REX prefix).
func rm8Legacy(r uint8) xop { return xr(int(r)) }

// rm32 is the host operand for d's r/m (after ea for memory forms).
func (g *jitGen) rm32(d *dInst) xop {
	if d.isMem {
		return memAt
	}
	return xr(h(d.rm))
}

// end leaves the block for target: chained to its translation when there is one.
func (g *jitGen) end(target uint32) {
	g.addSteps(g.jb.steps)
	if t := g.j.byEIP[target]; t != nil {
		g.jmpAbs(t.head)
		return
	}
	stub := g.a.newLabel()
	at := g.a.jmpPatchable()
	g.ends = append(g.ends, jitChain{label: stub, target: target})
	g.a.fixups = append(g.a.fixups, x64fixup{at, stub})
}

// endReg leaves the block for the guest address in register r: through the lookup table.
func (g *jitGen) endReg(r int) {
	g.addSteps(g.jb.steps)
	g.a.store32(xm(hR15, offEIP), r)
	g.a.movRR32(hR10, r)
	g.a.alu32I(4, xr(hR10), jitTableMask)
	g.a.shiftI32(4, xr(hR10), 4)
	g.a.inst(0, xW, []byte{0x03}, hR10, xm(hR15, offTable)) // add r10, [table]
	g.a.alu32RM(7, r, xm(hR10, 0))                          // cmp r, [r10]
	g.a.jcc(0x5, g.endStub)                                 // jne
	g.a.jmpMem(xm(hR10, 8))
}

func (g *jitGen) addSteps(n uint64) {
	if n != 0 {
		g.a.alu64I(0, xm(hR15, offSteps), int32(n))
	}
}

func (g *jitGen) exit(reason uint32) {
	g.a.storeI32(xm(hR15, offExit), reason)
	g.jmpAbs(g.epi)
}

// jitTranslate emits translated code for jb into j's buffer.
func jitTranslate(c *CPU, j *jitCode, jb *jitBlock) bool {
	g := &jitGen{c: c, j: j, jb: jb, base: uintptr(unsafe.Pointer(&j.buf[0])) + uintptr(j.used)}
	g.epi = j.epilogue
	g.slow = make([]int, len(jb.insts))
	for i := range g.slow {
		g.slow[i] = -1
	}
	g.endStub = g.a.newLabel()
	a := &g.a
	resume := make([]int, len(jb.insts))
	native := 0

	// head: the step-limit check for chained entries
	limitExit := a.newLabel()
	a.load64(hR11, xm(hR15, offSteps))
	a.alu64I(0, xr(hR11), int32(jb.steps))
	a.inst(0, xW, []byte{0x3B}, hR11, xm(hR15, offLimit)) // cmp r11, [limit]
	a.jcc(0x7, limitExit)                                 // ja

	g.start = jb.eip
	for i := range jb.insts {
		d := &jb.insts[i]
		g.idx = i
		resume[i] = len(a.b)
		if g.inst(d) {
			native++
		} else {
			a.jmp(g.slowLabel())
		}
		g.start += uint32(d.len)
		if isFused(d.kind) {
			g.start += uint32(d.len2)
		}
	}
	if native == 0 {
		return false
	}
	last := &jb.insts[len(jb.insts)-1]
	if !endsBlock(last.kind) && !isFused(last.kind) {
		g.end(g.start) // the block stopped before an uncached instruction or at blockMax
	}

	// exits
	a.bind(limitExit)
	a.storeI32(xm(hR15, offEIP), jb.eip)
	g.exit(jitExitLimit)
	a.bind(g.endStub)
	g.exit(jitExitEnd)
	eip := jb.eip
	for i := range jb.insts {
		if g.slow[i] >= 0 {
			a.bind(g.slow[i])
			a.storeI32(xm(hR15, offEIP), eip)
			g.addSteps(jb.prefix[i])
			a.storeI32(xm(hR15, offEIdx), uint32(i))
			a.storeI32(xm(hR15, offEBlk), jb.id)
			g.exit(jitExitSlow)
		}
		eip += uint32(jb.insts[i].len)
		if isFused(jb.insts[i].kind) {
			eip += uint32(jb.insts[i].len2)
		}
	}
	type site struct {
		at     int
		target uint32
	}
	var sites []site
	for _, e := range g.ends {
		a.bind(e.label)
		a.storeI32(xm(hR15, offEIP), e.target)
		g.exit(jitExitEnd)
	}
	// The patchable jumps of g.ends: their rel32 offsets, to register as pending.
	for _, f := range a.fixups {
		for _, e := range g.ends {
			if f.label == e.label {
				sites = append(sites, site{f.at, e.target})
			}
		}
	}
	a.resolve()

	if len(j.buf)-j.used < len(a.b) {
		return false
	}
	copy(j.buf[j.used:], a.b)
	jb.head = g.base
	jb.resume = make([]uintptr, len(jb.insts))
	for i, r := range resume {
		jb.resume[i] = g.base + uintptr(r)
	}
	for _, s := range sites {
		j.pending[s.target] = append(j.pending[s.target], g.base+uintptr(s.at))
	}
	j.used += (len(a.b) + 15) &^ 15
	j.Stats.Bytes += uint64(len(a.b))
	return true
}

// inst translates one instruction; false if it isn't translated (the interpreter runs it).
func (g *jitGen) inst(d *dInst) bool {
	a := &g.a
	next := g.start + uint32(d.len)
	switch d.kind {
	case dNop:
	case dMovRImm32:
		a.movRI32NoFlags(h(d.reg), d.imm)
	case dMovLoad32R:
		a.movRR32(h(d.reg), h(d.rm))
	case dMovStore32R:
		a.movRR32(h(d.rm), h(d.reg))
	case dMovLoad32:
		if !d.isMem {
			a.movRR32(h(d.reg), h(d.rm))
			break
		}
		g.ea(d)
		g.chkLoad(4)
		a.load32(h(d.reg), memAt)
	case dMovStore32:
		if !d.isMem {
			a.movRR32(h(d.rm), h(d.reg))
			break
		}
		g.ea(d)
		g.chkStore(hR8)
		a.store32(memAt, h(d.reg))
	case dMovImm32:
		if !d.isMem {
			a.movRI32NoFlags(h(d.rm), d.imm)
			break
		}
		g.ea(d)
		g.chkStore(hR8)
		a.storeI32(memAt, d.imm)
	case dLea:
		if d.base < 0 && d.index < 0 {
			a.movRI32NoFlags(h(d.reg), d.disp)
		} else {
			a.lea32(h(d.reg), g.eaOp(d))
		}

	case dAluRMR32, dAluRMR32R:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			if d.aux == 7 {
				g.chkLoad(4)
			} else {
				g.chkStore(hR8)
			}
			rm = memAt
		}
		if d.aux == 2 || d.aux == 3 {
			g.loadFlags()
		}
		a.alu32(int(d.aux), rm, h(d.reg))
		g.flags(aluFlags(d.aux))
	case dAluRRM32, dAluRRM32R:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		if d.aux == 2 || d.aux == 3 {
			g.loadFlags()
		}
		a.alu32RM(int(d.aux), h(d.reg), rm)
		g.flags(aluFlags(d.aux))
	case dAluEAXImm:
		if d.aux == 2 || d.aux == 3 {
			g.loadFlags()
		}
		a.alu32I(int(d.aux), xr(hRAX), d.imm)
		g.flags(aluFlags(d.aux))
	case dGrp1RM32, dGrp1RM32R:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			if d.aux == 7 {
				g.chkLoad(4)
			} else {
				g.chkStore(hR8)
			}
			rm = memAt
		}
		if d.aux == 2 || d.aux == 3 {
			g.loadFlags()
		}
		a.alu32I(int(d.aux), rm, d.imm)
		g.flags(aluFlags(d.aux))
	case dGrp1RM8:
		if d.isMem {
			g.ea(d)
			if d.aux == 7 {
				g.chkLoad(1)
			} else {
				g.chkStore(hR8)
			}
			if d.aux == 2 || d.aux == 3 {
				g.loadFlags()
			}
			a.alu8I(int(d.aux), memAt, uint8(d.imm), xByte)
		} else {
			if d.aux == 2 || d.aux == 3 {
				g.loadFlags()
			}
			a.alu8I(int(d.aux), rm8Legacy(d.rm), uint8(d.imm), xHigh)
		}
		g.flags(aluFlags(d.aux))
	case dTestRMR32:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.inst(0, 0, []byte{0x85}, h(d.reg), rm)
		g.flags(flagsLogic, flagsArith)
	case dTestRMI8:
		g.testRMI8(d)
		g.flags(flagsLogic, flagsArith)
	case dTestRMR8:
		g.testRMR8(d)
		g.flags(flagsLogic, flagsArith)
	case dTestALImm:
		a.emit(0xA8, uint8(d.imm))
		g.flags(flagsLogic, flagsArith)

	case dIncR, dDecR:
		op := 0
		if d.kind == dDecR {
			op = 1
		}
		a.inst(0, 0, []byte{0xFF}, op, xr(h(d.reg)))
		g.flags(flagsArith&^FlagCF, flagsArith&^FlagCF)
	case dIncRM32, dDecRM32:
		op := 0
		if d.kind == dDecRM32 {
			op = 1
		}
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			rm = memAt
		}
		a.inst(0, 0, []byte{0xFF}, op, rm)
		g.flags(flagsArith&^FlagCF, flagsArith&^FlagCF)
	case dNot32, dNeg32:
		op := 2
		if d.kind == dNeg32 {
			op = 3
		}
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			rm = memAt
		}
		a.inst(0, 0, []byte{0xF7}, op, rm)
		if d.kind == dNeg32 {
			g.flags(flagsArith, flagsArith)
		}

	case dPushR:
		g.push(func() int { return h(d.reg) })
	case dPushRM32:
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			a.load32(hR9, memAt)
		} else {
			a.movRR32(hR9, h(d.rm))
		}
		g.push(func() int { return hR9 })
	case dPopR:
		a.movRR32(hR8, hR12)
		g.chkLoad(4)
		a.load32(hR9, memAt)
		a.lea32(hR12, xm(hR12, 4))
		a.movRR32(h(d.reg), hR9)

	case dMovzx8, dMovsx8:
		op := byte(0xB6)
		if d.kind == dMovsx8 {
			op = 0xBE
		}
		if d.isMem {
			g.ea(d)
			g.chkLoad(1)
			a.inst(0, xByte, []byte{0x0F, op}, h(d.reg), memAt)
		} else if d.rm < 4 {
			a.inst(0, xByte, []byte{0x0F, op}, h(d.reg), xr(h(d.rm)))
		} else {
			g.get8(d.rm, hR9)
			a.inst(0, xByte, []byte{0x0F, op}, h(d.reg), xr(hR9))
		}
	case dMovzx16, dMovsx16:
		op := byte(0xB7)
		if d.kind == dMovsx16 {
			op = 0xBF
		}
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(2)
			rm = memAt
		}
		a.inst(0, 0, []byte{0x0F, op}, h(d.reg), rm)
	case dCmov:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		g.loadFlags()
		a.cmov(d.aux, h(d.reg), rm)
	case dSetcc:
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			g.loadFlags()
			a.setcc(d.aux, xr(hR9), xByte)
			a.store8(memAt, hR9)
		} else {
			g.loadFlags()
			a.setcc(d.aux, rm8Legacy(d.rm), xHigh)
		}

	case dMovStore8:
		if !d.isMem {
			a.inst(0, xHigh, []byte{0x88}, int(d.reg), rm8Legacy(d.rm))
			break
		}
		g.ea(d)
		g.chkStore(hR8)
		if d.reg < 4 {
			a.store8(memAt, h(d.reg))
		} else {
			g.get8(d.reg, hR9)
			a.store8(memAt, hR9)
		}
	case dMovLoad8:
		if !d.isMem {
			a.inst(0, xHigh, []byte{0x8A}, int(d.reg), rm8Legacy(d.rm))
			break
		}
		g.ea(d)
		g.chkLoad(1)
		if d.reg < 4 {
			a.inst(0, xByte, []byte{0x8A}, h(d.reg), memAt)
		} else {
			a.movzx8(hR9, memAt)
			g.put8(d.reg, hR9)
		}
	case dMovImm8:
		if !d.isMem {
			a.emit(0xB0+d.rm, uint8(d.imm))
			break
		}
		g.ea(d)
		g.chkStore(hR8)
		a.storeI8(memAt, uint8(d.imm))

	case dShift32:
		g.shiftImm(d)
	case dShift32CL:
		g.shiftCL(d)
	case dImul32:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		if int32(d.imm) >= -128 && int32(d.imm) <= 127 {
			a.inst(0, 0, []byte{0x6B}, h(d.reg), rm)
			a.emit(byte(d.imm))
		} else {
			a.inst(0, 0, []byte{0x69}, h(d.reg), rm)
			a.emit32(d.imm)
		}
		g.flags(FlagCF|FlagOF, FlagCF|FlagOF)
	case dImulRRM32:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.inst(0, 0, []byte{0x0F, 0xAF}, h(d.reg), rm)
		g.flags(FlagCF|FlagOF, FlagCF|FlagOF)

	case dMovssLoad:
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			a.sse(0xF3, 0x10, 0, memAt)     // movss xmm0, [mem]
			a.sse(0, 0x11, 0, xmmOp(d.reg)) // movups [xmm reg], xmm0
		} else {
			a.load32(hR9, xmmOp(d.rm))
			a.store32(xmmOp(d.reg), hR9)
		}
	case dMovssStore:
		a.load32(hR9, xmmOp(d.reg))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			a.store32(memAt, hR9)
		} else {
			a.store32(xmmOp(d.rm), hR9)
		}
	case dMovsdLoad:
		if d.isMem {
			g.ea(d)
			g.chkLoad(8)
			a.load64(hR9, memAt)
			a.store64(xmmOp(d.reg), hR9)
			a.inst(0, xW, []byte{0xC7}, 0, xm(hR15, offXMM+16*int32(d.reg)+8))
			a.emit32(0)
		} else {
			a.load64(hR9, xmmOp(d.rm))
			a.store64(xmmOp(d.reg), hR9)
		}
	case dMovsdStore:
		a.load64(hR9, xmmOp(d.reg))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			a.store64(memAt, hR9)
		} else {
			a.store64(xmmOp(d.rm), hR9)
		}
	case dArithSS:
		src := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			src = memAt
		}
		a.sse(0xF3, 0x10, 0, xmmOp(d.reg)) // movss xmm0, [xmm reg]
		a.sse(0xF3, d.aux, 0, src)         // addss/mulss/subss/divss xmm0, src
		a.sse(0xF3, 0x11, 0, xmmOp(d.reg)) // movss [xmm reg], xmm0
	case dMovapsLoad:
		src := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkLoad(16)
			src = memAt
		}
		a.sse(0, 0x10, 0, src)
		a.sse(0, 0x11, 0, xmmOp(d.reg))
	case dMovapsStore:
		dst := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			dst = memAt
		}
		a.sse(0, 0x10, 0, xmmOp(d.reg))
		a.sse(0, 0x11, 0, dst)
	case dUcomiss:
		src := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			src = memAt
		}
		a.sse(0xF3, 0x10, 0, xmmOp(d.reg)) // movss xmm0, [xmm reg]
		a.sse(0, 0x2E, 0, src)             // ucomiss xmm0, src
		g.flags(flagsArith, flagsArith)
	case dXorps:
		src := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkLoad(16)
			src = memAt
		}
		a.sse(0, 0x10, 1, src) // movups xmm1, src (xorps would need it aligned)
		a.sse(0, 0x10, 0, xmmOp(d.reg))
		a.sse(0, 0x57, 0, xr(1)) // xorps xmm0, xmm1
		a.sse(0, 0x11, 0, xmmOp(d.reg))

	// Control transfers end the block.
	case dJmp:
		g.end(d.imm)
	case dJcc:
		g.loadFlags()
		taken := a.newLabel()
		a.jcc(d.aux, taken)
		g.end(next)
		a.bind(taken)
		g.end(d.imm)
	case dCall:
		a.lea32(hR8, xm(hR12, -4))
		g.chkStore(hR8)
		a.storeI32(memAt, next)
		a.movRR32(hR12, hR8)
		g.end(d.imm)
	case dRet:
		a.movRR32(hR8, hR12)
		g.chkLoad(4)
		a.load32(hR9, memAt)
		a.lea32(hR12, xm(hR12, 4))
		g.endReg(hR9)
	case dCallInd, dJmpInd:
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			a.load32(hR9, memAt)
		} else {
			a.movRR32(hR9, h(d.rm))
		}
		if d.kind == dCallInd {
			a.lea32(hR10, xm(hR12, -4))
			g.chkStore(hR10)
			a.storeI32(xmi(hR14, hR10, 0, 0), next)
			a.movRR32(hR12, hR10)
		}
		g.endReg(hR9)
	case dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dCmpEAXJcc, dTestRMRJcc, dTestRMIJcc, dCmpRMI8Jcc:
		g.fused(d, next)
	default:
		return false
	}
	return true
}

// push stores the value in register val() (evaluated before ESP changes) at ESP-4.
func (g *jitGen) push(val func() int) {
	a := &g.a
	a.lea32(hR8, xm(hR12, -4))
	g.chkStore(hR8)
	a.store32(memAt, val())
	a.movRR32(hR12, hR8)
}

func (g *jitGen) testRMI8(d *dInst) {
	a := &g.a
	if d.isMem {
		g.ea(d)
		g.chkLoad(1)
		a.inst(0, xByte, []byte{0xF6}, 0, memAt)
	} else {
		a.inst(0, xHigh, []byte{0xF6}, 0, rm8Legacy(d.rm))
	}
	a.emit(uint8(d.imm))
}

func (g *jitGen) testRMR8(d *dInst) {
	a := &g.a
	if !d.isMem {
		a.inst(0, xHigh, []byte{0x84}, int(d.reg), rm8Legacy(d.rm))
		return
	}
	g.ea(d)
	g.chkLoad(1)
	r := h(d.reg)
	if d.reg >= 4 {
		g.get8(d.reg, hR9)
		r = hR9
	}
	a.inst(0, xByte, []byte{0x84}, r, memAt)
}

// fused: a compare or test and the Jcc after it.
func (g *jitGen) fused(d *dInst, next uint32) {
	a := &g.a
	value, clear := uint32(flagsArith), uint32(flagsArith)
	switch d.kind {
	case dCmpRMRJcc:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.alu32(7, rm, h(d.reg))
	case dCmpRRMJcc:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.alu32RM(7, h(d.reg), rm)
	case dCmpRMIJcc:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.alu32I(7, rm, d.imm)
	case dCmpEAXJcc:
		a.alu32I(7, xr(hRAX), d.imm)
	case dTestRMRJcc:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.inst(0, 0, []byte{0x85}, h(d.reg), rm)
		value = flagsLogic
	case dTestRMIJcc:
		g.testRMI8(d)
		value = flagsLogic
	case dCmpRMI8Jcc:
		if d.isMem {
			g.ea(d)
			g.chkLoad(1)
			a.alu8I(7, memAt, uint8(d.imm), xByte)
		} else {
			a.alu8I(7, rm8Legacy(d.rm), uint8(d.imm), xHigh)
		}
	}
	a.setcc(d.cond, xr(hR10), xByte)
	g.flags(value, clear)
	taken := a.newLabel()
	a.inst(0, xByte, []byte{0x84}, hR10, xr(hR10)) // test r10b, r10b
	a.jcc(0x5, taken)
	fall := next + uint32(d.len2)
	g.end(fall)
	a.bind(taken)
	g.end(d.target)
}

func (g *jitGen) shiftImm(d *dInst) {
	a := &g.a
	cnt := uint8(d.imm) & 0x1F
	rm := xr(h(d.rm))
	if d.isMem {
		g.ea(d)
		g.chkStore(hR8)
		rm = memAt
	}
	if cnt == 0 {
		return // no change (a store of the same value to an ordinary page)
	}
	op := int(d.aux)
	if op == 2 || op == 3 {
		g.loadFlags()
	}
	a.shiftI32(op, rm, cnt)
	of := uint32(0)
	if cnt == 1 {
		of = FlagOF
	}
	switch op {
	case 0: // rol
		g.flags(FlagCF|of, FlagCF|of)
	case 1, 2, 3: // ror, rcl, rcr
		g.flags(FlagCF, FlagCF)
	default: // shl, shr, sal, sar
		g.flags(FlagCF|FlagPF|FlagZF|FlagSF|of, flagsArith)
	}
}

func (g *jitGen) shiftCL(d *dInst) {
	a := &g.a
	rm := xr(h(d.rm))
	if d.isMem {
		g.ea(d)
		g.chkStore(hR8)
		rm = memAt
	}
	done := a.newLabel()
	a.movRR32(hR10, hRCX)
	a.alu32I(4, xr(hR10), 0x1F)
	a.jcc(0x4, done) // count 0: nothing changes
	op := int(d.aux)
	if op == 2 || op == 3 {
		g.loadFlags()
	}
	a.shiftCL32(op, rm)
	a.pushfq()
	a.pop(hR11)
	switch op {
	case 0: // rol: OF only for a count of 1
		many := a.newLabel()
		a.alu32I(7, xr(hR10), 1)
		a.jcc(0x5, many)
		g.merge(FlagCF|FlagOF, FlagCF|FlagOF)
		a.jmp(done)
		a.bind(many)
		g.merge(FlagCF, FlagCF)
	case 1, 2, 3:
		g.merge(FlagCF, FlagCF)
	default: // OF is 0 unless the count is 1
		one := a.newLabel()
		a.alu32I(7, xr(hR10), 1)
		a.jcc(0x4, one)
		a.alu32I(4, xr(hR11), ^uint32(FlagOF))
		a.bind(one)
		g.merge(FlagCF|FlagPF|FlagZF|FlagSF|FlagOF, flagsArith)
	}
	a.bind(done)
}

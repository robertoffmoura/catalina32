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
// Guest flags live in c.Flags. An instruction that sets flags runs natively; when its flags can
// be observed (by an instruction reading them, at the block's end, or at a slow exit before
// something overwrites them all) they are saved (PUSHFQ to jitState.raw, with which of them in
// jitState.pmask) and stay pending: merged into c.Flags (with the interpreter's rules for flags
// the hardware leaves undefined) when an instruction reads flags or sets only some of them, by
// Go whenever translated code returns, and simply replaced when a later instruction sets them
// all. An instruction reading flags loads c.Flags into the host's, or, right after the
// instruction that set them, uses the host's directly when they hold exactly what it reads.

const jitSupported = true

var hostReg = [8]int{hRAX, hRCX, hRDX, hRBX, hR12, hRBP, hRSI, hRDI}

// jitCall runs translated code: prologue saves the host registers Go needs back, loads the guest
// registers and jumps to target (jit_amd64.s).
func jitCall(prologue uintptr, c *CPU, target uintptr)

var (
	offRegs   = int32(unsafe.Offsetof(CPU{}.Regs))
	offEIP    = int32(unsafe.Offsetof(CPU{}.EIP))
	offFlags  = int32(unsafe.Offsetof(CPU{}.Flags))
	offSteps  = int32(unsafe.Offsetof(CPU{}.Steps))
	offXMM    = int32(unsafe.Offsetof(CPU{}.XMM))
	offJit    = int32(unsafe.Offsetof(CPU{}.jit))
	offMem    = offJit + int32(unsafe.Offsetof(jitState{}.mem))
	offStore  = offJit + int32(unsafe.Offsetof(jitState{}.stores))
	offTable  = offJit + int32(unsafe.Offsetof(jitState{}.table))
	offBudget = offJit + int32(unsafe.Offsetof(jitState{}.budget))
	offExit   = offJit + int32(unsafe.Offsetof(jitState{}.exit))
	offEIdx   = offJit + int32(unsafe.Offsetof(jitState{}.exitIdx))
	offEBlk   = offJit + int32(unsafe.Offsetof(jitState{}.exitBlk))
	offSave   = offJit + int32(unsafe.Offsetof(jitState{}.save))
	offRaw    = offJit + int32(unsafe.Offsetof(jitState{}.raw))
	offPMask  = offJit + int32(unsafe.Offsetof(jitState{}.pmask))
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
	a  x64
	c  *CPU
	j  *jitCode
	jb *jitBlock

	epi     uintptr // this buffer's epilogue
	slow    []int   // per instruction: label of its slow exit (-1: none yet)
	start   uint32  // guest address of the instruction being translated
	idx     int
	ends    []jitChain
	endStub int // label: exit with EIP already stored

	capture  []bool       // per instruction setting flags: whether they are saved (they can be observed)
	pend     jitPending   // saved flags not merged into c.Flags yet
	slowPend []jitPending // per instruction: what its slow exit must merge
	hostFrom int          // the instruction whose flags the host's flags hold (-1: none)
	native   bool         // the current instruction reads the host's flags directly

	absFix    []jitAbs // jumps to absolute addresses
	resume    []int    // where each instruction's code starts
	limitExit int      // label
	hostOK    []bool   // per instruction: the host's flags hold its flags when it ends

	entryPend []jitPending // per instruction: saved flags its code expects on entry
	natives   []bool       // per instruction: reads the host's flags directly
}

// jitPending is a flag-setting instruction's flags, saved in jitState.raw.
type jitPending struct {
	on           bool
	dyn          bool // set by an earlier block: what is pending is only known from jitState.pmask
	value, clear uint32
}

// jitFx is how an instruction uses the guest flags.
type jitFx struct {
	writes       bool   // sets flags (value from the host's, the rest of clear zeroed)
	value, clear uint32 //
	dynamic      bool   // whether it sets them depends on run-time values (shift by CL): it merges them itself
	reads        bool   // reads flags
	native       bool   // can read them from the host's flags right after the instruction that set them
	cc           uint8  // the condition it reads (native readers)
}

func jitFlagFx(d *dInst) jitFx {
	switch d.kind {
	case dAluRMR32, dAluRMR32R, dAluRRM32, dAluRRM32R, dAluEAXImm, dGrp1RM32, dGrp1RM32R, dGrp1RM8,
		dAlu8RMR, dAlu8RRM, dAluALImm, dAlu16RMR, dAlu16RRM, dGrp1RM16:
		v, c := aluFlags(d.aux)
		return jitFx{writes: true, value: v, clear: c, reads: d.aux == 2 || d.aux == 3}
	case dTestRMR32, dTestRMI8, dTestRMR8, dTestALImm, dTest16:
		return jitFx{writes: true, value: flagsLogic, clear: flagsArith}
	case dIncR, dDecR, dIncRM32, dDecRM32:
		return jitFx{writes: true, value: flagsArith &^ FlagCF, clear: flagsArith &^ FlagCF}
	case dNeg32, dNeg8, dUcomiss, dUcomisd:
		return jitFx{writes: true, value: flagsArith, clear: flagsArith}
	case dImul32, dImulRRM32:
		return jitFx{writes: true, value: FlagCF | FlagOF, clear: FlagCF | FlagOF}
	case dCmpxchg8b:
		return jitFx{writes: true, value: FlagZF, clear: FlagZF}
	case dShift32:
		cnt := uint8(d.imm) & 0x1F
		if cnt == 0 {
			return jitFx{}
		}
		v, c := shiftFlags(d.aux, cnt)
		return jitFx{writes: true, value: v, clear: c, reads: d.aux == 2 || d.aux == 3}
	case dShift32CL:
		return jitFx{writes: true, dynamic: true, reads: d.aux == 2 || d.aux == 3}
	case dJcc:
		return jitFx{reads: true, native: true, cc: d.aux}
	case dCmov, dSetcc:
		return jitFx{reads: true, native: !d.isMem, cc: d.aux}
	case dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dCmpEAXJcc, dCmpRMI8Jcc:
		return jitFx{writes: true, value: flagsArith, clear: flagsArith}
	case dTestRMRJcc, dTestRMIJcc:
		return jitFx{writes: true, value: flagsLogic, clear: flagsArith}
	}
	return jitFx{}
}

// shiftFlags is what a shift or rotate by cnt (1..31) sets: rotates only CF (and OF for a
// count of 1, rol); shifts CF, PF, ZF, SF and OF for a count of 1, zeroing AF and otherwise OF.
func shiftFlags(op uint8, cnt uint8) (uint32, uint32) {
	of := uint32(0)
	if cnt == 1 {
		of = FlagOF
	}
	switch op {
	case 0:
		return FlagCF | of, FlagCF | of
	case 1, 2, 3:
		return FlagCF, FlagCF
	}
	return FlagCF | FlagPF | FlagZF | FlagSF | of, flagsArith
}

// ccFlags is the flags condition cc tests.
func ccFlags(cc uint8) uint32 {
	return [8]uint32{FlagOF, FlagCF, FlagZF, FlagCF | FlagZF, FlagSF, FlagPF, FlagSF | FlagOF, FlagZF | FlagSF | FlagOF}[cc>>1&7]
}

// produce is called right after an instruction set the host's flags: they are saved when they
// can be observed (capture), otherwise dropped (overwritten before anyone looks).
func (g *jitGen) produce(value, clear uint32) {
	g.hostFrom = g.idx
	if !g.capture[g.idx] {
		g.pend.on = false
		return
	}
	g.jb.cost.FlagSaves++
	g.a.pushfq()
	g.a.inst(0, 0, []byte{0x8F}, 0, xm(hR15, offRaw)) // pop qword [raw]
	g.a.storeI32(xm(hR15, offPMask), value|clear<<16)
	g.pend = jitPending{on: true, value: value, clear: clear}
}

// emitMerge merges pending flags p into c.Flags (uses r9-r11, changes the host's flags).
func (g *jitGen) emitMerge(p jitPending) {
	if !p.on {
		return
	}
	a := &g.a
	if !p.dyn {
		g.jb.cost.Merges++
		a.load32(hR11, xm(hR15, offRaw))
		g.merge(p.value, p.clear)
		a.storeI32(xm(hR15, offPMask), 0)
		return
	}
	g.jb.cost.DynMerges++
	skip := a.newLabel()
	a.load32(hR10, xm(hR15, offPMask))
	a.inst(0, 0, []byte{0x85}, hR10, xr(hR10)) // test r10d, r10d
	a.jcc(0x4, skip)
	a.load32(hR11, xm(hR15, offRaw))
	a.alu32(4, xr(hR11), hR10) // and r11d, r10d: the value bits (the mask's low half)
	a.shiftI32(5, xr(hR10), 16)
	a.inst(0, 0, []byte{0xF7}, 2, xr(hR10)) // not r10d: ^clear
	a.alu32(4, xm(hR15, offFlags), hR10)
	a.alu32(1, xm(hR15, offFlags), hR11)
	a.storeI32(xm(hR15, offPMask), 0)
	a.bind(skip)
}

type jitChain struct {
	label  int    // the jump to patch
	target uint32 // guest address
}

func h(g uint8) int { return hostReg[g] }

// jmpAbs jumps to an absolute address (in the code buffer); the rel32 is set at placement.
func (g *jitGen) jmpAbs(to uintptr) {
	g.a.emit(0xE9)
	g.absFix = append(g.absFix, jitAbs{len(g.a.b), to})
	g.a.emit32(0)
}

type jitAbs struct {
	at int // rel32
	to uintptr
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
	g.jb.cost.Loads++
	if size == 16 {
		g.a.alu32I(7, xr(hR8), ArenaSize-24)
		g.a.jcc(0x7, g.slowLabel()) // ja
		return
	}
	g.a.alu32I(7, xr(hR8), ArenaSize-8)
	g.a.jcc(0x3, g.slowLabel()) // jae
}

// chkStore sends a store at register r to the interpreter if it is out of range or on (or just
// before) a page holding code: the store table marks those pages.
func (g *jitGen) chkStore(r int) {
	g.jb.cost.Stores++
	g.a.movRR32(hR11, r)
	g.a.shiftI32(5, xr(hR11), 12)                          // shr r11d, 12
	g.a.inst(0, 0, []byte{0x80}, 7, xmi(hR13, hR11, 0, 0)) // cmp byte [r13+r11], 0
	g.a.emit(0)
	g.a.jcc(0x5, g.slowLabel()) // jne
}

// merge is flags with the host's flags already in r11.
func (g *jitGen) merge(value, clear uint32) {
	g.a.alu32I(4, xr(hR11), value)            // and r11d, value
	g.a.alu32I(4, xm(hR15, offFlags), ^clear) // and [flags], ^clear
	g.a.alu32(1, xm(hR15, offFlags), hR11)    // or [flags], r11d
}

// loadFlags puts the guest's arithmetic flags into the host's.
func (g *jitGen) loadFlags() {
	g.jb.cost.FlagLoads++
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

// put8 stores the low byte of host register src (r9 or r10) into guest 8-bit register r. It
// changes the host's flags.
func (g *jitGen) put8(r uint8, src int) {
	g.hostFrom = -1
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
	g.a.store32(xm(hR15, offEIP), r)
	g.a.inst(0, 0, []byte{0x69}, hR10, xr(r)) // imul r10d, r, jitHashMul
	g.a.emit32(jitHashMul)
	g.a.shiftI32(5, xr(hR10), 32-jitTableBits)
	g.a.inst(0, xW, []byte{0xC1}, 4, xr(hR10)) // shl r10, 4
	g.a.emit(4)
	g.a.inst(0, xW, []byte{0x03}, hR10, xm(hR15, offTable)) // add r10, [table]
	g.a.alu32RM(7, r, xm(hR10, 0))                          // cmp r, [r10]
	g.a.jcc(0x5, g.endStub)                                 // jne
	g.a.jmpMem(xm(hR10, 8))
}

func (g *jitGen) exit(reason uint32) {
	g.a.storeI32(xm(hR15, offExit), reason)
	g.jmpAbs(g.epi)
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
	g := newJitGen(c, j, jb, jitCaptures(jb.insts, exits, dry.hostOK))
	g.body()
	a := &g.a
	jb.cost.Insts = uint64(n)
	for i := range exits {
		if g.slow[i] >= 0 {
			jb.cost.Exits++
		}
	}
	split := len(a.b) // the body is hot; everything after it (exits) goes to the cold region

	// exits
	a.bind(g.limitExit)
	a.alu64I(0, xm(hR15, offBudget), int32(jb.steps)) // not run: give the charge back
	a.storeI32(xm(hR15, offEIP), jb.eip)
	g.exit(jitExitLimit)
	a.bind(g.endStub)
	g.exit(jitExitEnd)
	slowExit := a.newLabel() // r11d = the instruction's index
	a.bind(slowExit)
	a.store32(xm(hR15, offEIdx), hR11)
	a.storeI32(xm(hR15, offEBlk), jb.id)
	g.exit(jitExitSlow)
	for i := range jb.insts {
		if g.slow[i] >= 0 {
			a.bind(g.slow[i])
			a.movRI32NoFlags(hR11, uint32(i))
			a.jmp(slowExit)
		}
	}
	// Resuming after the interpreter ran the previous instruction: the host's flags and the saved
	// ones are stale. Code that uses them is entered through a stub setting them from c.Flags
	// (merging those again changes nothing: bits an instruction zeroes are 0 there already).
	resumeAt := make([]int, n) // label, or -1: enter at the instruction's code
	for i := 1; i < n; i++ {
		resumeAt[i] = -1
		if g.slow[i-1] < 0 || (!(g.entryPend[i].on && !g.entryPend[i].dyn) && !g.natives[i]) {
			continue
		}
		resumeAt[i] = a.newLabel()
		a.bind(resumeAt[i])
		if g.entryPend[i].on && !g.entryPend[i].dyn {
			a.load32(hR11, xm(hR15, offFlags))
			a.store64(xm(hR15, offRaw), hR11)
		}
		if g.natives[i] {
			g.loadFlags()
		}
		code := a.newLabel()
		a.labels[code] = g.resume[i]
		a.jmp(code)
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
	// Placement: offsets below split go to the hot region, the rest to the cold one.
	hotLen, coldLen := split, len(a.b)-split
	if j.coldAt-j.used < hotLen || len(j.buf)-j.cold < coldLen {
		return false
	}
	bufAt := uintptr(unsafe.Pointer(&j.buf[0]))
	hot, cold := bufAt+uintptr(j.used), bufAt+uintptr(j.cold)
	final := func(o int) uintptr {
		if o < split {
			return hot + uintptr(o)
		}
		return cold + uintptr(o-split)
	}
	for _, f := range a.fixups {
		to := a.labels[f.label]
		binary.LittleEndian.PutUint32(a.b[f.at:], uint32(int32(final(to)-(final(f.at)+4))))
	}
	for _, f := range g.absFix {
		binary.LittleEndian.PutUint32(a.b[f.at:], uint32(int32(f.to-(final(f.at)+4))))
	}
	copy(j.buf[j.used:], a.b[:split])
	copy(j.buf[j.cold:], a.b[split:])
	jb.head = hot
	jb.resume = make([]uintptr, n)
	for i, r := range g.resume {
		if i > 0 && resumeAt[i] >= 0 {
			r = a.labels[resumeAt[i]]
		}
		jb.resume[i] = final(r)
	}
	for _, s := range sites {
		j.pending[s.target] = append(j.pending[s.target], final(s.at))
	}
	j.used += (hotLen + 15) &^ 15
	j.cold += (coldLen + 15) &^ 15
	j.Stats.Bytes += uint64(len(a.b))
	jb.cost.Bytes = uint64(hotLen)
	return true
}

func newJitGen(c *CPU, j *jitCode, jb *jitBlock, capture []bool) *jitGen {
	g := &jitGen{c: c, j: j, jb: jb, epi: j.epilogue,
		capture: capture, hostFrom: -1}
	n := len(jb.insts)
	if g.capture == nil { // the dry run: save every flag result
		g.capture = make([]bool, n)
		for i := range g.capture {
			g.capture[i] = true
		}
	}
	g.slow = make([]int, n)
	for i := range g.slow {
		g.slow[i] = -1
	}
	g.slowPend = make([]jitPending, n)
	g.resume = make([]int, n)
	g.entryPend = make([]jitPending, n)
	g.natives = make([]bool, n)
	g.hostOK = make([]bool, n)
	g.endStub = g.a.newLabel()
	g.limitExit = g.a.newLabel()
	return g
}

// jitCaptures decides, for each instruction setting flags, whether they must be saved: yes if
// anything may observe them before an instruction overwrites them all (an instruction reading
// flags other than straight from the host's, a slow exit, the block's end).
func jitCaptures(insts []dInst, exits, hostOK []bool) []bool {
	n := len(insts)
	capture := make([]bool, n)
	for i := range insts {
		f := jitFlagFx(&insts[i])
		if !f.writes || f.dynamic {
			continue
		}
		for k := i + 1; ; k++ {
			if k == n || exits[k] {
				capture[i] = true
				break
			}
			fk := jitFlagFx(&insts[k])
			if fk.reads && !(fk.native && k == i+1 && hostOK[i] && ccFlags(fk.cc)&^f.value == 0) {
				capture[i] = true
				break
			}
			if fk.writes {
				if fk.dynamic || fk.clear&f.clear != f.clear {
					capture[i] = true // merged before it (it doesn't overwrite them all)
				}
				break
			}
		}
	}
	return capture
}

// body translates the block's instructions (the head with the step-limit check first); it
// returns how many were translated.
func (g *jitGen) body() int {
	a := &g.a
	jb := g.jb
	a.alu64I(5, xm(hR15, offBudget), int32(jb.steps)) // sub [budget], steps
	a.jcc(0x2, g.limitExit)                           // jb: it would pass the limit
	if jitProf {
		if jb.runs == nil {
			jb.runs = new(uint64)
		}
		a.emit(0x49, 0xBB) // mov r11, imm64
		a.emit64(uint64(uintptr(unsafe.Pointer(jb.runs))))
		a.inst(0, xW, []byte{0xFF}, 0, xm(hR11, 0)) // inc qword [r11]
	}
	native := 0
	g.start = jb.eip
	g.pend = jitPending{on: true, dyn: true} // an earlier block may have left flags pending
	for i := range jb.insts {
		d := &jb.insts[i]
		g.idx = i
		g.resume[i] = len(a.b)
		f := jitFlagFx(d)
		g.native = f.reads && f.native && g.hostFrom == i-1 && i > 0 && ccFlags(f.cc)&^g.hostValue(i-1) == 0
		g.entryPend[i], g.natives[i] = g.pend, g.native
		pendClear := g.pend.clear
		if g.pend.dyn {
			pendClear = flagsArith // unknown: only an instruction setting them all replaces them
		}
		if g.pend.on && ((f.reads && !g.native) || (f.writes && (f.dynamic || f.clear&pendClear != pendClear))) {
			g.emitMerge(g.pend)
			g.pend.on = false
		}
		g.slowPend[i] = g.pend
		if !f.writes || f.dynamic {
			g.hostFrom = -1
		}
		if g.inst(d) {
			native++
		} else {
			a.jmp(g.slowLabel())
		}
		g.hostOK[i] = g.hostFrom == i
		if f.dynamic {
			g.pend.on = false
		}
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

// hostValue is the flags instruction i takes from the host.
func (g *jitGen) hostValue(i int) uint32 {
	return jitFlagFx(&g.jb.insts[i]).value
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
		g.produce(aluFlags(d.aux))
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
		g.produce(aluFlags(d.aux))
	case dAluEAXImm:
		if d.aux == 2 || d.aux == 3 {
			g.loadFlags()
		}
		a.alu32I(int(d.aux), xr(hRAX), d.imm)
		g.produce(aluFlags(d.aux))
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
		g.produce(aluFlags(d.aux))
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
		g.produce(aluFlags(d.aux))
	case dTestRMR32:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.inst(0, 0, []byte{0x85}, h(d.reg), rm)
		g.produce(flagsLogic, flagsArith)
	case dTestRMI8:
		g.testRMI8(d)
		g.produce(flagsLogic, flagsArith)
	case dTestRMR8:
		g.testRMR8(d)
		g.produce(flagsLogic, flagsArith)
	case dTestALImm:
		a.emit(0xA8, uint8(d.imm))
		g.produce(flagsLogic, flagsArith)

	case dIncR, dDecR:
		op := 0
		if d.kind == dDecR {
			op = 1
		}
		a.inst(0, 0, []byte{0xFF}, op, xr(h(d.reg)))
		g.produce(flagsArith&^FlagCF, flagsArith&^FlagCF)
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
		g.produce(flagsArith&^FlagCF, flagsArith&^FlagCF)
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
			g.produce(flagsArith, flagsArith)
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
		if !g.native {
			g.loadFlags()
		}
		a.cmov(d.aux, h(d.reg), rm)
	case dSetcc:
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			g.loadFlags()
			a.setcc(d.aux, xr(hR9), xByte)
			a.store8(memAt, hR9)
		} else {
			if !g.native {
				g.loadFlags()
			}
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
		g.produce(FlagCF|FlagOF, FlagCF|FlagOF)
	case dImulRRM32:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = memAt
		}
		a.inst(0, 0, []byte{0x0F, 0xAF}, h(d.reg), rm)
		g.produce(FlagCF|FlagOF, FlagCF|FlagOF)

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
		g.produce(flagsArith, flagsArith)
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
		if !g.native {
			g.loadFlags()
		}
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
		return g.inst2(d)
	}
	return true
}

// inst2 translates the forms cached for the JIT (dAlu8RMR and after).
func (g *jitGen) inst2(d *dInst) bool {
	a := &g.a
	adcSbb := d.aux == 2 || d.aux == 3
	switch d.kind {
	case dAlu8RMR: // op r/m8, r8
		if !d.isMem {
			if adcSbb {
				g.loadFlags()
			}
			a.inst(0, xHigh, []byte{d.aux << 3}, int(d.reg), rm8Legacy(d.rm))
			g.produce(aluFlags(d.aux))
			break
		}
		g.ea(d)
		if d.aux == 7 {
			g.chkLoad(1)
		} else {
			g.chkStore(hR8)
		}
		r := h(d.reg)
		if d.reg >= 4 {
			g.get8(d.reg, hR9)
			r = hR9
		}
		if adcSbb {
			g.loadFlags()
		}
		a.inst(0, xByte, []byte{d.aux << 3}, r, memAt)
		g.produce(aluFlags(d.aux))
	case dAlu8RRM: // op r8, r/m8
		if !d.isMem {
			if adcSbb {
				g.loadFlags()
			}
			a.inst(0, xHigh, []byte{d.aux<<3 | 2}, int(d.reg), rm8Legacy(d.rm))
			g.produce(aluFlags(d.aux))
			break
		}
		g.ea(d)
		g.chkLoad(1)
		if d.reg < 4 {
			if adcSbb {
				g.loadFlags()
			}
			a.inst(0, xByte, []byte{d.aux<<3 | 2}, h(d.reg), memAt)
			g.produce(aluFlags(d.aux))
			break
		}
		g.get8(d.reg, hR9)
		if adcSbb {
			g.loadFlags()
		}
		a.inst(0, xByte, []byte{d.aux<<3 | 2}, hR9, memAt)
		g.produce(aluFlags(d.aux))
		if d.aux != 7 {
			g.put8(d.reg, hR9)
		}
	case dAluALImm:
		if adcSbb {
			g.loadFlags()
		}
		a.emit(d.aux<<3|4, uint8(d.imm))
		g.produce(aluFlags(d.aux))
	case dMovR8Imm:
		a.emit(0xB0+d.reg, uint8(d.imm))
	case dNot8, dNeg8:
		op := 2
		if d.kind == dNeg8 {
			op = 3
		}
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			a.inst(0, xByte, []byte{0xF6}, op, memAt)
		} else {
			a.inst(0, xHigh, []byte{0xF6}, op, rm8Legacy(d.rm))
		}
		if d.kind == dNeg8 {
			g.produce(flagsArith, flagsArith)
		}
	case dRetImm:
		a.movRR32(hR8, hR12)
		g.chkLoad(4)
		a.load32(hR9, memAt)
		a.lea32(hR12, xm(hR12, 4+int32(d.imm)))
		g.endReg(hR9)

	case dMov16Store, dMov16Load, dAlu16RMR, dAlu16RRM, dTest16:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			if d.kind == dMov16Store || d.kind == dAlu16RMR && d.aux != 7 {
				g.chkStore(hR8)
			} else {
				g.chkLoad(2)
			}
			rm = memAt
		}
		var opc byte
		switch d.kind {
		case dMov16Store:
			opc = 0x89
		case dMov16Load:
			opc = 0x8B
		case dAlu16RMR:
			opc = d.aux<<3 | 1
		case dAlu16RRM:
			opc = d.aux<<3 | 3
		case dTest16:
			opc = 0x85
		}
		alu := d.kind == dAlu16RMR || d.kind == dAlu16RRM
		if alu && adcSbb {
			g.loadFlags()
		}
		a.inst(0x66, 0, []byte{opc}, h(d.reg), rm)
		switch {
		case alu:
			g.produce(aluFlags(d.aux))
		case d.kind == dTest16:
			g.produce(flagsLogic, flagsArith)
		}
	case dMov16Imm:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			rm = memAt
		}
		a.inst(0x66, 0, []byte{0xC7}, 0, rm)
		a.emit16(uint16(d.imm))
	case dGrp1RM16:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			if d.aux == 7 {
				g.chkLoad(2)
			} else {
				g.chkStore(hR8)
			}
			rm = memAt
		}
		if adcSbb {
			g.loadFlags()
		}
		a.inst(0x66, 0, []byte{0x81}, int(d.aux), rm)
		a.emit16(uint16(d.imm))
		g.produce(aluFlags(d.aux))

	case dArithSS2:
		src := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			src = memAt
		}
		a.sse(0xF3, 0x10, 0, xmmOp(d.reg))
		a.sse(0xF3, d.aux, 0, src)
		a.sse(0xF3, 0x11, 0, xmmOp(d.reg))
	case dArithSD:
		src := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkLoad(8)
			src = memAt
		}
		a.sse(0xF2, 0x10, 0, xmmOp(d.reg)) // movsd xmm0, [xmm reg]
		a.sse(0xF2, d.aux, 0, src)         // addsd.. xmm0, src
		a.sse(0xF2, 0x11, 0, xmmOp(d.reg)) // movsd [xmm reg], xmm0
	case dArithPS, dLogicPS:
		g.src128(d, 1)
		a.sse(0, 0x10, 0, xmmOp(d.reg))
		a.sse(0, d.aux, 0, xr(1))
		a.sse(0, 0x11, 0, xmmOp(d.reg))
	case dCvtSS2SD, dCvtSD2SS:
		src := xmmOp(d.rm)
		size, pfx, store := 4, byte(0xF3), byte(0xF2)
		if d.kind == dCvtSD2SS {
			size, pfx, store = 8, 0xF2, 0xF3
		}
		if d.isMem {
			g.ea(d)
			g.chkLoad(size)
			src = memAt
		}
		a.sse(pfx, 0x5A, 0, src)
		a.sse(store, 0x11, 0, xmmOp(d.reg)) // movsd / movss [xmm reg], xmm0
	case dCvttSS2SI, dCvttSD2SI:
		src := xmmOp(d.rm)
		size, pfx := 4, byte(0xF3)
		if d.kind == dCvttSD2SI {
			size, pfx = 8, 0xF2
		}
		if d.isMem {
			g.ea(d)
			g.chkLoad(size)
			src = memAt
		}
		a.sse(pfx, 0x2C, h(d.reg), src)
	case dCvtSI2SS, dCvtSI2SD:
		src := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			src = memAt
		}
		pfx := byte(0xF3)
		if d.kind == dCvtSI2SD {
			pfx = 0xF2
		}
		a.sse(pfx, 0x10, 0, xmmOp(d.reg))
		a.sse(pfx, 0x2A, 0, src)
		a.sse(pfx, 0x11, 0, xmmOp(d.reg))
	case dUcomisd:
		src := xmmOp(d.rm)
		if d.isMem {
			g.ea(d)
			g.chkLoad(8)
			src = memAt
		}
		a.sse(0xF2, 0x10, 0, xmmOp(d.reg))
		a.sse(0x66, 0x2E, 0, src)
		g.produce(flagsArith, flagsArith)
	case dPshufd:
		g.src128(d, 1)
		a.sse(0x66, 0x70, 0, xr(1))
		a.emit(uint8(d.imm))
		a.sse(0, 0x11, 0, xmmOp(d.reg))
	case dMovdToX:
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			a.load32(hR9, memAt)
		} else {
			a.movRR32(hR9, h(d.rm))
		}
		a.store64(xmmOp(d.reg), hR9) // zero-extended
		a.inst(0, xW, []byte{0xC7}, 0, xm(hR15, offXMM+16*int32(d.reg)+8))
		a.emit32(0)
	case dCmpxchg8b: // EDX:EAX, ECX:EBX are rdx:rax, rcx:rbx
		g.ea(d)
		g.chkStore(hR8)
		a.inst(0xF0, 0, []byte{0x0F, 0xC7}, 1, memAt)
		g.produce(FlagZF, FlagZF)
	case dBswap:
		if hr := h(d.reg); hr >= 8 {
			a.emit(0x41, 0x0F, 0xC8+byte(hr&7))
		} else {
			a.emit(0x0F, 0xC8+byte(hr))
		}
	case dMovdFromX:
		a.load32(hR9, xmmOp(d.reg))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			a.store32(memAt, hR9)
		} else {
			a.movRR32(h(d.rm), hR9)
		}
	default:
		return false
	}
	return true
}

// src128 loads d's 128-bit source operand into host XMM register x.
func (g *jitGen) src128(d *dInst, x int) {
	if d.isMem {
		g.ea(d)
		g.chkLoad(16)
		g.a.sse(0, 0x10, x, memAt)
		return
	}
	g.a.sse(0, 0x10, x, xmmOp(d.rm))
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
	g.produce(value, clear)
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
		g.produce(FlagCF|of, FlagCF|of)
	case 1, 2, 3: // ror, rcl, rcr
		g.produce(FlagCF, FlagCF)
	default: // shl, shr, sal, sar
		g.produce(FlagCF|FlagPF|FlagZF|FlagSF|of, flagsArith)
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

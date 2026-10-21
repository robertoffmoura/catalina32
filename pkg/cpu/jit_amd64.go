package cpu

import (
	"encoding/binary"
	"math"
	"unsafe"
)

// The JIT's x86-64 backend. Guest registers live in host registers while translated code runs:
//
//	guest  EAX ECX EDX EBX ESP EBP ESI EDI
//	host   rax rcx rdx rbx r12 rbp rsi rdi
//
// so most instructions keep their encoding (and CL, AH..BH work as in the guest). r14 holds
// &Mem[0], r15 the CPU, r13 the step budget (jitState.budget while Go runs); r8 holds effective
// addresses, r9-r11 are scratch. Within a block the guest's XMM registers live in xmm8-15 from
// their first use (xget); xmm0 and xmm1 are scratch.
// Guest flags live in c.Flags. An instruction that sets flags runs natively; when its flags can
// be observed before something overwrites them all (jitCaptures: by an instruction reading them,
// or after the block's end by the code its branches lead to) they are saved (PUSHFQ to
// jitState.raw, with which of them in jitState.pmask) and stay pending: merged into c.Flags
// (with the interpreter's rules for flags the hardware leaves undefined) when an instruction
// reads flags or sets only some of them, by Go whenever translated code returns, and simply
// replaced when a later instruction sets them all. An instruction reading flags loads c.Flags
// into the host's, or, right after the instruction that set them, uses the host's directly when
// they hold exactly what it reads.
// Memory accesses are checked one at a time (chkLoad, chkStore), except stack and frame accesses,
// which are checked a window at a time (jitWindows).

const jitSupported = true

const (
	jitCodeSize   = 256 << 20 // the code buffer (reserved; pages are used as code fills it); when full, all translated code is discarded
	jitHotEighths = 5         // how much of it holds blocks' bodies (hot code); their exits go in the rest
)

var hostReg = [8]int{hRAX, hRCX, hRDX, hRBX, hR12, hRBP, hRSI, hRDI}

// jitCall runs translated code: prologue saves the host registers Go needs back, loads the guest
// registers and jumps to target (jit_amd64.s).
func jitCall(prologue uintptr, c *CPU, target uintptr)

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
	a.load64(hR13, xm(hR15, offBudget))
	for g, h := range hostReg {
		a.load32(h, cpuReg(g))
	}
	a.jmpReg(hR11)
	epilogue := len(a.b)
	for g, h := range hostReg {
		a.store32(cpuReg(g), h)
	}
	a.store64(xm(hR15, offBudget), hR13)
	a.sse(0, 0x57, 15, xr(15)) // xorps xmm15, xmm15: Go's zero register (translated code uses xmm8-15)
	for i, r := range saved {  // r15 is last: it addresses the save area
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
	changed   int      // label: a self-checking block's code changed (-1: not one)

	entryPend []jitPending // per instruction: saved flags its code expects on entry
	natives   []bool       // per instruction: reads the host's flags directly

	xLoaded, xDirty     uint8   // guest XMM registers in host ones, and those changed (xget)
	xLoadedAt, xDirtyAt []uint8 // per instruction: xLoaded and xDirty when it starts

	mem      xop           // the memory operand ea computed: memAt, or a windowed one
	memWin   bool          // ea's operand is in a window: no check
	winOp    []bool        // per instruction: its r/m operand is in a window (jitWindows)
	winStack []bool        // per instruction: its push or pop is in a window
	winAt    [][]jitWindow // per instruction: the windows checked before it
	winFail  []int         // per instruction: label of its windows' failure exit (-1: none)
}

// jitPending is a flag-setting instruction's flags, saved in jitState.raw.
type jitPending struct {
	on           bool
	dyn          bool // set by an earlier block: what is pending is only known from jitState.pmask
	value, clear uint32
}

// produce is called right after an instruction set the host's flags: they are saved when they
// can be observed (capture), otherwise dropped (overwritten before anyone looks).
func (g *jitGen) produce(value, clear uint32) {
	g.hostFrom = g.idx
	if !g.capture[g.idx] {
		g.pend.on = false
		return
	}
	defer g.charge(&g.jb.cost.FlagBytes, len(g.a.b))
	g.jb.cost.FlagSaves++
	g.a.pushfq()
	g.a.inst(0, 0, []byte{0x8F}, 0, xm(hR15, offRaw)) // pop qword [raw]
	g.a.storeI32(xm(hR15, offPMask), value|clear<<16)
	g.pend = jitPending{on: true, value: value, clear: clear}
}

// charge adds the code emitted since start to *n (C32_JITPROF).
func (g *jitGen) charge(n *uint64, start int) { *n += uint64(len(g.a.b) - start) }

// emitMerge merges pending flags p into c.Flags (uses r9-r11, changes the host's flags).
func (g *jitGen) emitMerge(p jitPending) {
	if !p.on {
		return
	}
	defer g.charge(&g.jb.cost.FlagBytes, len(g.a.b))
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
	if g.winOp[g.idx] { // [esp+d] or [ebp+d] in a window: addressed directly (jitWindows)
		g.mem, g.memWin = xmi(hR14, h(uint8(d.base)), 0, int32(d.disp)), true
		return
	}
	g.mem, g.memWin = memAt, false
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
	if g.memWin {
		return
	}
	defer g.charge(&g.jb.cost.CheckBytes, len(g.a.b))
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
	if g.memWin && r == hR8 {
		return
	}
	defer g.charge(&g.jb.cost.CheckBytes, len(g.a.b))
	g.a.movRR32(hR11, r)
	g.a.shiftI32(5, xr(hR11), 12)                           // shr r11d, 12
	g.a.inst(0, xW, []byte{0x03}, hR11, xm(hR15, offStore)) // add r11, [stores]
	g.a.inst(0, 0, []byte{0x80}, 7, xm(hR11, 0))            // cmp byte [r11], 0
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
	defer g.charge(&g.jb.cost.FlagBytes, len(g.a.b))
	g.a.load32(hR11, xm(hR15, offFlags))
	g.a.alu32I(4, xr(hR11), flagsArith)
	g.a.push(hR11)
	g.a.popfq()
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
		return g.mem
	}
	return xr(h(d.rm))
}

// end leaves the block for target: chained to its translation when there is one.
func (g *jitGen) end(target uint32) {
	g.xstore(g.xDirty)
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
	g.xstore(g.xDirty)
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
	g := newJitGen(c, j, jb, jitCaptures(jb.insts, exits, c.jitLiveOut(jb.insts, jb.starts[n]), func(i, k int) bool {
		return dry.hostOK[i] && ccFlags(jitFlagFx(&jb.insts[k]).cc)&^jitFlagFx(&jb.insts[i]).value == 0
	}))
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
	a.alu64I(0, xr(hR13), int32(jb.steps)) // not run: give the charge back
	a.storeI32(xm(hR15, offEIP), jb.eip)
	g.exit(jitExitLimit)
	a.bind(g.endStub)
	g.exit(jitExitEnd)
	if g.changed >= 0 {
		a.bind(g.changed)
		a.storeI32(xm(hR15, offEIP), jb.eip)
		g.exit(jitExitEnd)
	}
	slowExit := a.newLabel() // r11d = the instruction's index
	a.bind(slowExit)
	a.store32(xm(hR15, offEIdx), hR11)
	a.storeI32(xm(hR15, offEBlk), jb.id)
	g.exit(jitExitSlow)
	for i := range jb.insts {
		if g.slow[i] >= 0 {
			a.bind(g.slow[i])
			g.xstore(g.xDirtyAt[i]) // the interpreter reads c.XMM
			a.movRI32NoFlags(hR11, uint32(i))
			a.jmp(slowExit)
		}
		if g.winFail[i] >= 0 { // a window's check failed: the interpreter runs the rest of the block
			a.bind(g.winFail[i])
			g.xstore(g.xDirtyAt[i])
			a.movRI32NoFlags(hR11, uint32(i)|jitRestOfBlock)
			a.jmp(slowExit)
		}
	}
	// Resuming after the interpreter ran the previous instruction: the host's flags and the saved
	// ones are stale. Code that uses them is entered through a stub setting them from c.Flags
	// (merging those again changes nothing: bits an instruction zeroes are 0 there already).
	resumeAt := make([]int, n) // label, or -1: enter at the instruction's code
	for i := 1; i < n; i++ {
		resumeAt[i] = -1
		if g.slow[i-1] < 0 || (!(g.entryPend[i].on && !g.entryPend[i].dyn) && !g.natives[i] && g.xLoadedAt[i] == 0) {
			continue
		}
		resumeAt[i] = a.newLabel()
		a.bind(resumeAt[i])
		g.xload(g.xLoadedAt[i]) // the interpreter may have changed c.XMM
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
	g.xLoadedAt, g.xDirtyAt = make([]uint8, n), make([]uint8, n)
	g.winOp, g.winStack, g.winAt = jitWindows(jb.insts)
	g.winFail = make([]int, n)
	for i := range g.winFail {
		g.winFail[i] = -1
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
	a.movRI32NoFlags(hR9, g.jb.eip)
	for _, ch := range jitCodeChunks(len(code)) {
		off, at := ch[0], xmi(hR14, hR9, 0, int32(ch[0]))
		switch ch[1] {
		case 8:
			a.emit(0x49, 0xBB) // mov r11, imm64
			a.emit64(binary.LittleEndian.Uint64(code[off:]))
			a.inst(0, xW, []byte{0x39}, hR11, at) // cmp [mem], r11
		case 4:
			a.alu32I(7, at, binary.LittleEndian.Uint32(code[off:]))
		default:
			a.alu8I(7, at, code[off], xByte)
		}
		a.jcc(0x5, g.changed)
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
	head := len(a.b)
	a.alu64I(5, xr(hR13), int32(jb.steps)) // sub r13 (the budget), steps
	a.jcc(0x2, g.limitExit)                // jb: it would pass the limit
	g.charge(&jb.cost.HeadBytes, head)
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
		g.xLoadedAt[i], g.xDirtyAt[i] = g.xLoaded, g.xDirty
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
		g.mem, g.memWin = memAt, false
		for _, w := range g.winAt[i] {
			g.windowCheck(w)
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
		a.load32(h(d.reg), g.mem)
	case dMovStore32:
		if !d.isMem {
			a.movRR32(h(d.rm), h(d.reg))
			break
		}
		g.ea(d)
		g.chkStore(hR8)
		a.store32(g.mem, h(d.reg))
	case dMovImm32:
		if !d.isMem {
			a.movRI32NoFlags(h(d.rm), d.imm)
			break
		}
		g.ea(d)
		g.chkStore(hR8)
		a.storeI32(g.mem, d.imm)
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
			rm = g.mem
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
			rm = g.mem
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
			rm = g.mem
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
			a.alu8I(int(d.aux), g.mem, uint8(d.imm), xByte)
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
			rm = g.mem
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
			rm = g.mem
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
			rm = g.mem
		}
		a.inst(0, 0, []byte{0xF7}, op, rm)
		if d.kind == dNeg32 {
			g.produce(flagsArith, flagsArith)
		}

	case dPushR:
		g.push(func() int { return h(d.reg) })
	case dSSE, dSSEStore:
		g.sseForm(d)
	case dPushImm:
		g.pushImm(d.imm)
	case dPushRM32:
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			a.load32(hR9, g.mem)
		} else {
			a.movRR32(hR9, h(d.rm))
		}
		g.push(func() int { return hR9 })
	case dPopR:
		g.pop(hR9, 4)
		a.movRR32(h(d.reg), hR9)

	case dMovzx8, dMovsx8:
		op := byte(0xB6)
		if d.kind == dMovsx8 {
			op = 0xBE
		}
		if d.isMem {
			g.ea(d)
			g.chkLoad(1)
			a.inst(0, xByte, []byte{0x0F, op}, h(d.reg), g.mem)
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
			rm = g.mem
		}
		a.inst(0, 0, []byte{0x0F, op}, h(d.reg), rm)
	case dCmov:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = g.mem
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
			a.store8(g.mem, hR9)
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
			a.store8(g.mem, h(d.reg))
		} else {
			g.get8(d.reg, hR9)
			a.store8(g.mem, hR9)
		}
	case dMovLoad8:
		if !d.isMem {
			a.inst(0, xHigh, []byte{0x8A}, int(d.reg), rm8Legacy(d.rm))
			break
		}
		g.ea(d)
		g.chkLoad(1)
		if d.reg < 4 {
			a.inst(0, xByte, []byte{0x8A}, h(d.reg), g.mem)
		} else {
			a.movzx8(hR9, g.mem)
			g.put8(d.reg, hR9)
		}
	case dMovImm8:
		if !d.isMem {
			a.emit(0xB0+d.rm, uint8(d.imm))
			break
		}
		g.ea(d)
		g.chkStore(hR8)
		a.storeI8(g.mem, uint8(d.imm))

	case dShift32:
		g.shiftImm(d)
	case dShift32CL:
		g.shiftCL(d)
	case dImul32:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = g.mem
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
			rm = g.mem
		}
		a.inst(0, 0, []byte{0x0F, 0xAF}, h(d.reg), rm)
		g.produce(FlagCF|FlagOF, FlagCF|FlagOF)

	// SSE: the guest's XMM registers are kept in host ones (xget and the like).
	case dMovssLoad, dMovsdLoad:
		pfx, size := byte(0xF3), 4
		if d.kind == dMovsdLoad {
			pfx, size = 0xF2, 8
		}
		if d.isMem {
			g.ea(d)
			g.chkLoad(size)
			a.sse(pfx, 0x10, g.xset(d.reg), g.mem) // movss/movsd xmm, [mem]: the rest zeroed
		} else {
			src := g.xget(d.rm)
			a.sse(pfx, 0x10, g.xmod(d.reg), xr(src)) // the low lane only
		}
	case dMovssStore, dMovsdStore:
		pfx := byte(0xF3)
		if d.kind == dMovsdStore {
			pfx = 0xF2
		}
		src := g.xget(d.reg)
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			a.sse(pfx, 0x11, src, g.mem)
		} else {
			a.sse(pfx, 0x11, src, xr(g.xmod(d.rm)))
		}
	case dArithSS, dArithSD:
		pfx, size := byte(0xF3), 4
		if d.kind == dArithSD {
			pfx, size = 0xF2, 8
		}
		src := g.xsrc(d, size)
		a.sse(pfx, d.aux, g.xmod(d.reg), src) // addss/mulss/subss/divss (sd) xmm, src
	case dMovapsLoad:
		if d.isMem {
			g.ea(d)
			g.chkLoad(16)
			a.sse(0, 0x10, g.xset(d.reg), g.mem) // movups: whatever the alignment
		} else {
			src := g.xget(d.rm)
			a.sse(0, 0x28, g.xset(d.reg), xr(src))
		}
	case dMovapsStore:
		src := g.xget(d.reg)
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			a.sse(0, 0x11, src, g.mem)
		} else {
			a.sse(0, 0x28, g.xset(d.rm), xr(src))
		}
	case dUcomiss, dUcomisd:
		pfx, size := byte(0), 4
		if d.kind == dUcomisd {
			pfx, size = 0x66, 8
		}
		src := g.xsrc(d, size)
		a.sse(pfx, 0x2E, g.xget(d.reg), src) // ucomiss / ucomisd
		g.produce(flagsArith, flagsArith)
	case dXorps:
		a.sse(0, 0x57, g.xmod(d.reg), g.xsrc(d, 16))

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
		g.pushImm(next)
		g.end(d.imm)
	case dRet:
		g.pop(hR9, 4)
		g.endReg(hR9)
	case dCallInd, dJmpInd:
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			a.load32(hR9, g.mem)
		} else {
			a.movRR32(hR9, h(d.rm))
		}
		if d.kind == dCallInd {
			if g.winStack[g.idx] {
				a.storeI32(xmi(hR14, hR12, 0, -4), next)
				a.lea32(hR12, xm(hR12, -4))
			} else {
				a.lea32(hR10, xm(hR12, -4))
				g.chkStore(hR10)
				a.storeI32(xmi(hR14, hR10, 0, 0), next)
				a.movRR32(hR12, hR10)
			}
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
		a.inst(0, xByte, []byte{d.aux << 3}, r, g.mem)
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
			a.inst(0, xByte, []byte{d.aux<<3 | 2}, h(d.reg), g.mem)
			g.produce(aluFlags(d.aux))
			break
		}
		g.get8(d.reg, hR9)
		if adcSbb {
			g.loadFlags()
		}
		a.inst(0, xByte, []byte{d.aux<<3 | 2}, hR9, g.mem)
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
			a.inst(0, xByte, []byte{0xF6}, op, g.mem)
		} else {
			a.inst(0, xHigh, []byte{0xF6}, op, rm8Legacy(d.rm))
		}
		if d.kind == dNeg8 {
			g.produce(flagsArith, flagsArith)
		}
	case dRetImm:
		g.pop(hR9, 4+int32(d.imm))
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
			rm = g.mem
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
			rm = g.mem
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
			rm = g.mem
		}
		if adcSbb {
			g.loadFlags()
		}
		a.inst(0x66, 0, []byte{0x81}, int(d.aux), rm)
		a.emit16(uint16(d.imm))
		g.produce(aluFlags(d.aux))

	case dArithSS2:
		src := g.xsrc(d, 4)
		switch d.aux {
		case 0x52: // rsqrtss as the interpreter: float32(1 / sqrt(float64(b)))
			a.sse(0xF3, 0x5A, 1, src)   // cvtss2sd xmm1, b
			a.sse(0xF2, 0x51, 1, xr(1)) // sqrtsd
			a.emit(0x49, 0xBB)          // mov r11, 1.0
			a.emit64(math.Float64bits(1))
			a.inst(0x66, xW, []byte{0x0F, 0x6E}, 0, xr(hR11)) // movq xmm0, r11
			a.sse(0xF2, 0x5E, 0, xr(1))                       // divsd
			a.sse(0xF2, 0x5A, 0, xr(0))                       // cvtsd2ss
			a.sse(0xF3, 0x10, g.xmod(d.reg), xr(0))           // movss: the low lane
		case 0x53: // rcpss as the interpreter: 1 / b in float32
			a.movRI32NoFlags(hR11, math.Float32bits(1))
			a.inst(0x66, 0, []byte{0x0F, 0x6E}, 0, xr(hR11)) // movd xmm0, r11d
			a.sse(0xF3, 0x5E, 0, src)                        // divss
			a.sse(0xF3, 0x10, g.xmod(d.reg), xr(0))
		default:
			a.sse(0xF3, d.aux, g.xmod(d.reg), src)
		}
	case dArithPS, dLogicPS:
		a.sse(0, d.aux, g.xmod(d.reg), g.xsrc(d, 16))
	case dCvtSS2SD, dCvtSD2SS:
		size, pfx := 4, byte(0xF3)
		if d.kind == dCvtSD2SS {
			size, pfx = 8, 0xF2
		}
		src := g.xsrc(d, size)
		a.sse(pfx, 0x5A, g.xmod(d.reg), src) // the low lane; the rest stays
	case dCvttSS2SI, dCvttSD2SI:
		size, pfx := 4, byte(0xF3)
		if d.kind == dCvttSD2SI {
			size, pfx = 8, 0xF2
		}
		a.sse(pfx, 0x2C, h(d.reg), g.xsrc(d, size))
	case dCvtSI2SS, dCvtSI2SD:
		src := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			src = g.mem
		}
		pfx := byte(0xF3)
		if d.kind == dCvtSI2SD {
			pfx = 0xF2
		}
		a.sse(pfx, 0x2A, g.xmod(d.reg), src) // the low lane; the rest stays
	case dPshufd:
		src := g.xsrc(d, 16)
		a.sse(0x66, 0x70, g.xset(d.reg), src)
		a.emit(uint8(d.imm))
	case dMovdToX:
		src := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			src = g.mem
		}
		a.sse(0x66, 0x6E, g.xset(d.reg), src) // movd: zero-extended
	case dCmpxchg8b: // EDX:EAX, ECX:EBX are rdx:rax, rcx:rbx
		g.ea(d)
		g.chkStore(hR8)
		a.inst(0xF0, 0, []byte{0x0F, 0xC7}, 1, g.mem)
		g.produce(FlagZF, FlagZF)
	case dBswap:
		if hr := h(d.reg); hr >= 8 {
			a.emit(0x41, 0x0F, 0xC8+byte(hr&7))
		} else {
			a.emit(0x0F, 0xC8+byte(hr))
		}
	case dFld, dFst, dFxch, dFsign, dFArith:
		g.x87(d)
	case dTestRMI32:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = g.mem
		}
		a.inst(0, 0, []byte{0xF7}, 0, rm) // test r/m32, imm32
		a.emit32(d.imm)
		g.produce(flagsLogic, flagsArith)
	case dXchg:
		if !d.isMem {
			a.inst(0, 0, []byte{0x87}, h(d.reg), xr(h(d.rm)))
			break
		}
		g.ea(d) // (without xchg's implicit lock)
		g.chkStore(hR8)
		a.load32(hR9, g.mem)
		a.store32(g.mem, h(d.reg))
		a.movRR32(h(d.reg), hR9)
	case dCmpxchg: // flags as cmp eax, r/m32; equal: r/m32 = r32, else eax = r/m32
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			rm = g.mem
		}
		a.inst(0, 0, []byte{0x0F, 0xB1}, h(d.reg), rm) // (memory: also written back when different; the page is checked)
		g.produce(flagsArith, flagsArith)
	case dMul1: // EDX:EAX = EAX * r/m32 (edx, eax are rdx, rax)
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = g.mem
		}
		a.inst(0, 0, []byte{0xF7}, int(d.aux), rm)
		g.produce(FlagCF|FlagOF, FlagCF|FlagOF)
	case dXadd:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			rm = g.mem
		}
		a.inst(0, 0, []byte{0x0F, 0xC1}, h(d.reg), rm) // (without lock)
		g.produce(flagsArith, flagsArith)
	case dBt:
		a.inst(0, 0, []byte{0x0F, 0xA3}, h(d.reg), xr(h(d.rm)))
		g.produce(FlagCF, FlagCF)
	case dUnpcklps, dCmpps, dShufps:
		src := g.xsrc(d, 16)
		dst := g.xmod(d.reg)
		switch d.kind {
		case dUnpcklps:
			a.sse(0, 0x14, dst, src)
		case dCmpps:
			a.sse(0, 0xC2, dst, src)
			a.emit(uint8(d.imm & 7))
		default:
			a.sse(0, 0xC6, dst, src)
			a.emit(uint8(d.imm))
		}
	case dMovmskps:
		a.sse(0, 0x50, h(d.reg), xr(g.xget(d.rm))) // movmskps r32, xmm
	case dMovlps:
		if d.isMem {
			g.ea(d)
			g.chkLoad(8)
			a.sse(0, 0x12, g.xmod(d.reg), g.mem) // movlps xmm, m64
		} else {
			src := g.xget(d.rm)
			a.sse(0, 0x12, g.xmod(d.reg), xr(src)) // movhlps
		}
	case dMovdFromX:
		src := g.xget(d.reg)
		if d.isMem {
			g.ea(d)
			g.chkStore(hR8)
			a.sse(0x66, 0x7E, src, g.mem) // movd
		} else {
			a.sse(0x66, 0x7E, src, xr(h(d.rm)))
		}
	default:
		return false
	}
	return true
}

// x87: the interpreter's registers are float64s (FPUSt), indexed from FPUTop, which the code
// loads into r9d; r10 and r11 index other registers. The arithmetic is SSE2's on doubles, as Go
// compiles the interpreter's (x86 returns the first operand when both are NaN, as sseAdd does).

// st returns guest register ST(i)'s memory, its index computed into idx (r9d holds FPUTop).
func (g *jitGen) st(i uint32, idx int) xop {
	if i == 0 {
		return xmi(hR15, hR9, 3, offFPUSt)
	}
	g.a.lea32(idx, xm(hR9, int32(i)))
	g.a.alu32I(4, xr(idx), 7)
	return xmi(hR15, idx, 3, offFPUSt)
}

// fpuTopMove adds delta (+1 pop, -1 push) to FPUTop, in r9d and memory.
func (g *jitGen) fpuTopMove(delta int32) {
	g.a.lea32(hR9, xm(hR9, delta))
	g.a.alu32I(4, xr(hR9), 7)
	g.a.store32(xm(hR15, offFPUTop), hR9)
}

func (g *jitGen) x87(d *dInst) {
	a := &g.a
	if d.isMem { // the access is checked before anything changes
		g.ea(d)
		size := 8
		if d.kind == dFld && (d.aux == fldM32 || d.aux == fldI32) || d.kind == dFst && d.aux == fstM32 || d.kind == dFArith && d.aux>>2 == fpuM32 {
			size = 4
		}
		if d.kind == dFst {
			g.chkStore(hR8)
		} else {
			g.chkLoad(size)
		}
	}
	a.load32(hR9, xm(hR15, offFPUTop))
	switch d.kind {
	case dFld:
		switch d.aux {
		case fldST:
			a.sse(0xF2, 0x10, 0, g.st(uint32(d.rm), hR10)) // movsd xmm0, ST(i)
		case fldM32:
			a.sse(0xF3, 0x5A, 0, g.mem) // cvtss2sd
		case fldM64:
			a.sse(0xF2, 0x10, 0, g.mem)
		case fldI32:
			a.sse(0xF2, 0x2A, 0, g.mem) // cvtsi2sd xmm0, dword
		default:
			v := uint64(0)
			if d.aux == fldOne {
				v = math.Float64bits(1)
			}
			a.emit(0x49, 0xBB) // mov r11, imm64
			a.emit64(v)
			a.inst(0x66, xW, []byte{0x0F, 0x6E}, 0, xr(hR11)) // movq xmm0, r11
		}
		g.fpuTopMove(-1)
		a.sse(0xF2, 0x11, 0, g.st(0, 0))
	case dFst:
		a.sse(0xF2, 0x10, 0, g.st(0, 0))
		switch d.aux {
		case fstST:
			if d.rm != 0 || d.imm == 0 {
				a.sse(0xF2, 0x11, 0, g.st(uint32(d.rm), hR10))
			}
		case fstM32:
			a.sse(0xF2, 0x5A, 0, xr(0)) // cvtsd2ss xmm0, xmm0
			a.sse(0xF3, 0x11, 0, g.mem)
		case fstM64:
			a.sse(0xF2, 0x11, 0, g.mem)
		}
		if d.imm != 0 {
			g.fpuTopMove(1)
		}
	case dFxch:
		other := g.st(uint32(d.rm), hR10)
		a.sse(0xF2, 0x10, 0, g.st(0, 0))
		a.sse(0xF2, 0x10, 1, other)
		a.sse(0xF2, 0x11, 1, g.st(0, 0))
		a.sse(0xF2, 0x11, 0, other)
	case dFsign:
		a.load64(hR11, g.st(0, 0))
		a.inst(0, xW, []byte{0x0F, 0xBA}, 7-int(d.aux), xr(hR11)) // btc (fchs) / btr (fabs) r11, 63
		a.emit(63)
		a.store64(g.st(0, 0), hR11)
	case dFArith:
		for k, sh := range []uint32{0, 4} {
			switch src := d.imm >> sh & 15; {
			case src != fpuMem:
				a.sse(0xF2, 0x10, k, g.st(src, hR10))
			case d.aux>>2 == fpuM32:
				a.sse(0xF3, 0x5A, k, g.mem)
			default:
				a.sse(0xF2, 0x10, k, g.mem)
			}
		}
		a.sse(0xF2, [4]byte{0x58, 0x59, 0x5C, 0x5E}[d.aux&3], 0, xr(1))
		a.sse(0xF2, 0x11, 0, g.st(uint32(d.reg), hR10))
		if d.imm>>8&1 != 0 {
			g.fpuTopMove(1)
		}
	}
}

// sseForm runs an sseForms instruction as the host's own on the host registers holding the
// guest's.
func (g *jitGen) sseForm(d *dInst) {
	a := &g.a
	f := sseForms[[2]uint8{d.cond, d.aux}]
	op := func(reg int, rm xop) {
		a.sse(d.cond, d.aux, reg, rm)
		if f.imm {
			a.emit(uint8(d.imm))
		}
	}
	if d.kind == dSSEStore {
		src := g.xget(d.reg)
		if !d.isMem {
			op(src, xr(g.xmod(d.rm)))
			return
		}
		g.ea(d)
		g.chkStore(hR8)
		if f.mem == 16 {
			a.sse(0, 0x11, src, g.mem) // movups (the 16-byte stores are all moves)
		} else {
			op(src, g.mem)
		}
		return
	}
	src := g.xsrc(d, int(f.mem))
	op(g.xmod(d.reg), src)
}

// The guest's XMM registers within a block: guest register r lives in host xmm8+r from its
// first use (xLoaded) and goes back to c.XMM when changed (xDirty) at the block's exits: its
// ends (xflush), slow exits (their stubs store what was changed before the instruction), and
// re-entries after the interpreter ran an instruction load what the code expects there.

// xget returns the host register holding guest register r, loading it first if needed.
func (g *jitGen) xget(r uint8) int {
	if g.xLoaded&(1<<r) == 0 {
		g.a.sse(0, 0x10, 8+int(r), xmmOp(r)) // movups
		g.xLoaded |= 1 << r
	}
	return 8 + int(r)
}

// xmod is xget for a register the instruction changes (it keeps part of its value).
func (g *jitGen) xmod(r uint8) int {
	x := g.xget(r)
	g.xDirty |= 1 << r
	return x
}

// xset is xget for a register the instruction replaces whole (no load).
func (g *jitGen) xset(r uint8) int {
	g.xLoaded |= 1 << r
	g.xDirty |= 1 << r
	return 8 + int(r)
}

// xsrc is d's source operand of size bytes: the host register holding its guest register, or
// its memory (a 16-byte one loaded into xmm1 first: the host's instruction may want it aligned).
func (g *jitGen) xsrc(d *dInst, size int) xop {
	if !d.isMem {
		return xr(g.xget(d.rm))
	}
	g.ea(d)
	g.chkLoad(size)
	if size == 16 {
		g.a.sse(0, 0x10, 1, g.mem)
		return xr(1)
	}
	return g.mem
}

// xstore writes the registers in mask back to c.XMM (code only: the state is unchanged).
func (g *jitGen) xstore(mask uint8) {
	for r := uint8(0); r < 8; r++ {
		if mask&(1<<r) != 0 {
			g.a.sse(0, 0x11, 8+int(r), xmmOp(r))
		}
	}
}

// xload loads the registers in mask from c.XMM.
func (g *jitGen) xload(mask uint8) {
	for r := uint8(0); r < 8; r++ {
		if mask&(1<<r) != 0 {
			g.a.sse(0, 0x10, 8+int(r), xmmOp(r))
		}
	}
}

// Memory windows. Accesses through a register and a displacement ([reg+d], pushes and pops) are
// checked a window at a time: before the first access, one check that the range of addresses
// the window's accesses reach (relative to the register then; ESP's pushes, pops and adds by
// constants tracked) lies inside guest memory and, when it stores, on pages without code; its
// accesses then go straight to [r14 + reg + d]. A failed check leaves the rest of the block to
// the interpreter. Stores are windowed only through ESP and EBP (the stack: a block storing to
// a page beside code through another register would fail its check every time). A window ends
// where its register is set otherwise, and is used only when it saves more checking than it
// costs.

type jitWindow struct {
	base   uint8 // the guest register
	lo, hi int32 // the addresses its accesses reach: base (at its check) + [lo, hi)
	store  bool
}

// jitWindows finds the windows of insts: per instruction, whether its r/m operand and its push or
// pop are in one, and the windows checked before it.
func jitWindows(insts []dInst) (op, stack []bool, at [][]jitWindow) {
	n := len(insts)
	op, stack, at = make([]bool, n), make([]bool, n), make([][]jitWindow, n)
	type access struct {
		i     int
		stack bool
	}
	type win struct {
		jitWindow
		first         int
		delta0        int32 // ESP's offset at the check
		loads, stores int
		accesses      []access
	}
	var wins []*win
	cur := [8]*win{}  // by register
	delta := int32(0) // ESP's offset from what it was at the block's start (or last unknown change)
	add := func(b uint8, i int, lo, hi int32, store, isStack bool) {
		k := b
		w := cur[k]
		if w != nil && (max(w.hi, hi)-min(w.lo, lo) > 4096) {
			w = nil
		}
		if w == nil {
			w = &win{jitWindow: jitWindow{base: b, lo: lo, hi: hi}, first: i}
			if b == RegESP {
				w.delta0 = delta
			}
			cur[k] = w
			wins = append(wins, w)
		}
		w.lo, w.hi = min(w.lo, lo), max(w.hi, hi)
		if store {
			w.store = true
			w.stores++
		} else {
			w.loads++
		}
		w.accesses = append(w.accesses, access{i, isStack})
	}
	for i := range insts {
		d := &insts[i]
		if d.isMem && d.index < 0 && d.base >= 0 && d.kind != dLea && d.kind != dNop {
			off, store := int32(d.disp), !jitLoadOnly(d)
			if d.base == RegESP {
				off += delta
			}
			if !store || d.base == RegESP || d.base == RegEBP {
				add(uint8(d.base), i, off, off+16, store, false)
			}
		}
		switch d.kind {
		case dPushR, dPushImm, dPushRM32, dCall, dCallInd:
			add(RegESP, i, delta-4, delta, true, true)
			delta -= 4
		case dPopR, dRet, dRetImm:
			add(RegESP, i, delta, delta+4, false, true)
			delta += 4
		}
		w := jitWrites(d)
		switch {
		case d.kind == dGrp1RM32R && d.rm == RegESP && (d.aux == 0 || d.aux == 5):
			if d.aux == 0 {
				delta += int32(d.imm)
			} else {
				delta -= int32(d.imm)
			}
		case d.kind == dLea && d.reg == RegESP && d.base == RegESP && d.index < 0:
			delta += int32(d.disp)
		case d.kind == dPopR && d.reg != RegESP, d.kind == dPushR, d.kind == dPushImm, d.kind == dPushRM32,
			d.kind == dCall, d.kind == dCallInd, d.kind == dRet, d.kind == dRetImm:
		case w&(1<<RegESP) != 0:
			cur[RegESP], delta = nil, 0
		}
		for r := uint8(0); r < 8; r++ {
			if r != RegESP && w&(1<<r) != 0 {
				cur[r] = nil
			}
		}
	}
	for _, w := range wins {
		// A check costs about 5 instructions (15 when it stores); each access it covers saves 3
		// (a load) or 6 (a store).
		cost := 5
		if w.store {
			cost = 15
		}
		if 3*w.loads+6*w.stores <= cost {
			continue
		}
		jw := w.jitWindow
		jw.lo -= w.delta0
		jw.hi -= w.delta0
		at[w.first] = append(at[w.first], jw)
		for _, a := range w.accesses {
			if a.stack {
				stack[a.i] = true
			} else {
				op[a.i] = true
			}
		}
	}
	return op, stack, at
}

// jitLoadOnly says whether d only reads its memory operand.
func jitLoadOnly(d *dInst) bool {
	switch d.kind {
	case dAluRMR32, dGrp1RM32, dGrp1RM8, dAlu8RMR, dAlu16RMR, dGrp1RM16:
		return d.aux == 7 // cmp
	case dMovLoad32, dAluRRM32, dTestRMR32, dTestRMI8, dTestRMR8, dTest16, dTestRMI32, dMovzx8, dMovzx16, dMovsx8,
		dMovsx16, dCmov, dImul32, dImulRRM32, dMovssLoad, dMovsdLoad, dArithSS, dArithSS2, dArithSD, dArithPS,
		dLogicPS, dMovapsLoad, dUcomiss, dUcomisd, dXorps, dCvtSS2SD, dCvtSD2SS, dCvttSS2SI, dCvttSD2SI, dCvtSI2SS,
		dCvtSI2SD, dPshufd, dMovdToX, dUnpcklps, dCmpps, dShufps, dMovlps, dSSE, dFld, dFArith, dPushRM32,
		dCallInd, dJmpInd, dMovLoad8, dMov16Load, dAlu16RRM, dAlu8RRM, dMul1,
		dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dTestRMRJcc, dTestRMIJcc, dCmpRMI8Jcc:
		return true
	}
	return false
}

// windowCheck emits w's check (r10, r11 and the host's flags change).
func (g *jitGen) windowCheck(w jitWindow) {
	a := &g.a
	if g.winFail[g.idx] < 0 {
		g.winFail[g.idx] = a.newLabel()
	}
	fail := g.winFail[g.idx]
	g.jb.cost.Loads++
	defer g.charge(&g.jb.cost.CheckBytes, len(a.b))
	a.movRR32(hR11, h(w.base))                             // r11 = base, zero-extended
	a.alu64I(0, xr(hR11), w.lo)                            // add r11, lo: negative if it goes below 0
	a.movRI32NoFlags(hR10, ArenaSize-16-uint32(w.hi-w.lo)) // (the accesses' sizes are in hi already)
	a.inst(0, xW, []byte{0x39}, hR10, xr(hR11))            // cmp r11, r10
	a.jcc(0x3, fail)                                       // jae
	if !w.store {
		return
	}
	for _, off := range []int32{0, w.hi - w.lo - 1} { // its first and last pages (it spans at most two)
		a.inst(0, xW, []byte{0x8D}, hR10, xm(hR11, off)) // lea r10, [r11+off]
		a.inst(0, xW, []byte{0xC1}, 5, xr(hR10))         // shr r10, 12
		a.emit(12)
		a.inst(0, xW, []byte{0x03}, hR10, xm(hR15, offStore)) // add r10, [stores]
		a.inst(0, 0, []byte{0x80}, 7, xm(hR10, 0))            // cmp byte [r10], 0
		a.emit(0)
		a.jcc(0x5, fail) // jne
	}
}

// push stores the value in register val() (evaluated before ESP changes) at ESP-4.

func (g *jitGen) push(val func() int) {
	a := &g.a
	if g.winStack[g.idx] {
		a.store32(xmi(hR14, hR12, 0, -4), val())
		a.lea32(hR12, xm(hR12, -4))
		return
	}
	a.lea32(hR8, xm(hR12, -4))
	g.memWin = false
	g.chkStore(hR8)
	a.store32(memAt, val())
	a.movRR32(hR12, hR8)
}

// pushImm pushes v.
func (g *jitGen) pushImm(v uint32) {
	a := &g.a
	if g.winStack[g.idx] {
		a.storeI32(xmi(hR14, hR12, 0, -4), v)
		a.lea32(hR12, xm(hR12, -4))
		return
	}
	a.lea32(hR8, xm(hR12, -4))
	g.memWin = false
	g.chkStore(hR8)
	a.storeI32(memAt, v)
	a.movRR32(hR12, hR8)
}

// pop loads [ESP] into r and adds release to ESP.
func (g *jitGen) pop(r int, release int32) {
	a := &g.a
	if g.winStack[g.idx] {
		a.load32(r, xmi(hR14, hR12, 0, 0))
	} else {
		a.movRR32(hR8, hR12)
		g.memWin = false
		g.chkLoad(4)
		a.load32(r, memAt)
	}
	a.lea32(hR12, xm(hR12, release))
}

func (g *jitGen) testRMI8(d *dInst) {
	a := &g.a
	if d.isMem {
		g.ea(d)
		g.chkLoad(1)
		a.inst(0, xByte, []byte{0xF6}, 0, g.mem)
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
	a.inst(0, xByte, []byte{0x84}, r, g.mem)
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
			rm = g.mem
		}
		a.alu32(7, rm, h(d.reg))
	case dCmpRRMJcc:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = g.mem
		}
		a.alu32RM(7, h(d.reg), rm)
	case dCmpRMIJcc:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = g.mem
		}
		a.alu32I(7, rm, d.imm)
	case dCmpEAXJcc:
		a.alu32I(7, xr(hRAX), d.imm)
	case dTestRMRJcc:
		rm := xr(h(d.rm))
		if d.isMem {
			g.ea(d)
			g.chkLoad(4)
			rm = g.mem
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
			a.alu8I(7, g.mem, uint8(d.imm), xByte)
		} else {
			a.alu8I(7, rm8Legacy(d.rm), uint8(d.imm), xHigh)
		}
	}
	g.produce(value, clear) // (saving them leaves the host's flags as they are)
	taken := a.newLabel()
	a.jcc(d.cond, taken)
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
		rm = g.mem
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
		rm = g.mem
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

package cpu

// Decoded-instruction cache.
//
// Step() normally re-decodes every instruction (prefixes, opcode switch, ModRM/SIB/displacement)
// each time it runs. The cache decodes the most frequently executed instruction forms once into a
// dInst and executes them through a compact switch. Execution reuses the interpreter's own semantic
// helpers (aluOp32, UpdateFlags*, Read32/Write32, EvalCond, Ret, setXMMF32, ...), so cached and
// uncached execution are identical; forms that are not cached (and any instruction with segment or
// operand-size prefixes) are marked dSlow once and always take the full decoder.
//
// Only instructions inside [CodeLo, CodeHi) are cached. Guest writes into that range flush the
// cache (see the Write* slow paths); host code that patches guest code must call FlushCodeCache.

const (
	dcacheBits = 18
	dcacheSize = 1 << dcacheBits
	dcacheMask = dcacheSize - 1
	dNoTag     = 0xFFFFFFFF
)

const (
	dSlow       uint8 = iota
	dMovStore32       // 89: r/m32 <- r32
	dMovLoad32        // 8B: r32 <- r/m32
	dMovImm32         // C7 /0: r/m32 <- imm32
	dAluRMR32         // 01,09,..,39: r/m32 op= r32
	dAluRRM32         // 03,0B,..,3B: r32 op= r/m32
	dAluEAXImm        // 05,0D,..,3D: EAX op= imm32
	dGrp1RM32         // 81 / 83: r/m32 op= imm (pre-sign-extended)
	dGrp1RM8          // 80: r/m8 op= imm8
	dTestRMR32        // 85
	dTestRMI8         // F6 /0
	dLea              // 8D
	dIncR             // 40+r
	dDecR             // 48+r
	dPushR            // 50+r
	dPopR             // 58+r
	dJcc              // 7x / 0F 8x (imm = absolute target)
	dJmp              // EB / E9 (imm = absolute target)
	dCall             // E8 (imm = absolute target)
	dRet              // C3
	dMovzx8           // 0F B6
	dMovzx16          // 0F B7
	dMovsx8           // 0F BE
	dMovsx16          // 0F BF
	dCmov             // 0F 4x
	dMovssLoad        // F3 0F 10
	dMovssStore       // F3 0F 11
	dMovsdLoad        // F2 0F 10
	dMovsdStore       // F2 0F 11
	dArithSS          // F3 0F 58/59/5C/5E (aux = op2)
	dMovapsLoad       // 0F 28 / 0F 10 (no prefix)
	dMovapsStore
	dUcomiss   // 0F 2E / 0F 2F (no prefix)
	dXorps     // 0F 57 (no prefix)
	dNop       // 90, and [66|2E]* 0F 1F /0 padding
	dMovRImm32 // B8+r
	dShift32   // C1 / D1 (imm = count)
	dShift32CL // D3
	dImul32    // 69 / 6B (imm = sign-extended immediate)
	dMovStore8 // 88
	dMovLoad8  // 8A
	dMovImm8   // C6 /0
	dTestRMR8  // 84
	dTestALImm // A8
	dIncRM32   // FF /0
	dDecRM32   // FF /1
	dCallInd   // FF /2
	dJmpInd    // FF /4
	dPushRM32  // FF /6

	// Register-operand variants chosen at decode time (the generic kinds above handle memory forms).
	dMovLoad32R  // 8B r32, r32
	dMovStore32R // 89 r32, r32
	dAluRMR32R   // 01..39 r32, r32
	dAluRRM32R   // 03..3B r32, r32
	dGrp1RM32R   // 81 / 83 r32, imm

	// Fused compare + Jcc pairs (blocks only). The compare sets flags exactly as on its own; the
	// pair counts as two steps. target/cond/len2 describe the Jcc.
	dCmpRMRJcc  // 39 + Jcc
	dCmpRRMJcc  // 3B + Jcc
	dCmpRMIJcc  // 81/83 /7 + Jcc
	dCmpEAXJcc  // 3D + Jcc
	dTestRMRJcc // 85 + Jcc
	dTestRMIJcc // F6 /0 + Jcc
	dCmpRMI8Jcc // 80 /7 + Jcc

	dImulRRM32 // 0F AF: r32 *= r/m32
	dNot32     // F7 /2
	dNeg32     // F7 /3
	dSetcc     // 0F 9x: r/m8 = condition (aux)

	// Forms below run through the full decoder when interpreted (execBlock rewinds and calls
	// stepFull, so they behave exactly as uncached): they are cached so blocks don't end at
	// them and the JIT can translate them.
	dAlu8RMR    // 00,08,..,38: r/m8 op= r8
	dAlu8RRM    // 02,0A,..,3A: r8 op= r/m8
	dAluALImm   // 04,0C,..,3C: AL op= imm8
	dMovR8Imm   // B0+r
	dNot8       // F6 /2
	dNeg8       // F6 /3
	dRetImm     // C2 iw (imm = bytes released)
	dMov16Store // 66 89
	dMov16Load  // 66 8B
	dMov16Imm   // 66 C7 /0
	dAlu16RMR   // 66 01..39
	dAlu16RRM   // 66 03..3B
	dGrp1RM16   // 66 81 / 66 83 (imm sign-extended to 16 bits)
	dTest16     // 66 85
	dArithSS2   // F3 0F 51/5D/5F: sqrtss, minss, maxss (aux = op2)
	dArithSD    // F2 0F 51/58/59/5C/5D/5E/5F (aux = op2)
	dArithPS    // 0F 51/58/59/5C/5D/5E/5F (aux = op2)
	dLogicPS    // 0F 54/55/56: andps, andnps, orps (aux = op2)
	dCvtSS2SD   // F3 0F 5A
	dCvtSD2SS   // F2 0F 5A
	dCvttSS2SI  // F3 0F 2C
	dCvttSD2SI  // F2 0F 2C
	dCvtSI2SS   // F3 0F 2A
	dCvtSI2SD   // F2 0F 2A
	dUcomisd    // 66 0F 2E / 2F
	dPshufd     // 66 0F 70 (imm = order)
	dMovdToX    // 66 0F 6E: xmm = r/m32
	dMovdFromX  // 66 0F 7E: r/m32 = xmm
)

type dInst struct {
	tag   uint32
	imm   uint32 // immediate, or absolute branch target
	disp  uint32
	kind  uint8
	len   uint8
	reg   uint8 // ModRM reg field, or register encoded in the opcode
	rm    uint8 // ModRM rm register (register form)
	aux   uint8 // ALU op / condition code / SSE op2
	isMem bool
	base  int8 // -1: none
	index int8 // -1: none
	scale uint8

	// Fused Jcc (dCmp*Jcc / dTest*Jcc).
	cond   uint8
	len2   uint8
	target uint32
}

func newDCache() []dInst {
	d := make([]dInst, dcacheSize)
	for i := range d {
		d[i].tag = dNoTag
	}
	return d
}

// Straight-line blocks: a run of cached instructions starting at tag that ends after the first
// control transfer (or before an uncached instruction, an intercepted address, or blockMax).
// Blocks hold copies of their instructions so collisions in the instruction cache cannot alter them.
const (
	blockBits = 18
	blockMask = 1<<blockBits - 1
	blockMax  = 64
)

type dBlock struct {
	tag   uint32
	steps int // guest instructions in the block (a fused pair counts as two)
	insts []dInst

	hits   uint32    // runs, until it is translated (jit.go)
	jit    *jitBlock // its translation, valid while jitGen == c.jitGen
	jitGen uint32
}

// FlushCodeCache discards all decoded instructions and blocks (call after modifying guest code
// from the host).
func (c *CPU) FlushCodeCache() {
	for i := range c.dcache {
		c.dcache[i].tag = dNoTag
	}
	for i := range c.blocks {
		c.blocks[i].tag = dNoTag
	}
	c.CodeCacheFlushes++
	c.jitReset()
}

func endsBlock(kind uint8) bool {
	switch kind {
	case dJcc, dJmp, dCall, dRet, dCallInd, dJmpInd, dRetImm:
		return true
	}
	return false
}

// fusedJcc returns the fused kind for compare-like instruction d followed by a Jcc, or dSlow.
func fusedJcc(d *dInst) uint8 {
	switch {
	case (d.kind == dAluRMR32 || d.kind == dAluRMR32R) && d.aux == 7:
		return dCmpRMRJcc
	case (d.kind == dAluRRM32 || d.kind == dAluRRM32R) && d.aux == 7:
		return dCmpRRMJcc
	case (d.kind == dGrp1RM32 || d.kind == dGrp1RM32R) && d.aux == 7:
		return dCmpRMIJcc
	case d.kind == dAluEAXImm && d.aux == 7:
		return dCmpEAXJcc
	case d.kind == dTestRMR32:
		return dTestRMRJcc
	case d.kind == dTestRMI8:
		return dTestRMIJcc
	case d.kind == dGrp1RM8 && d.aux == 7:
		return dCmpRMI8Jcc
	}
	return dSlow
}

func (c *CPU) buildBlock(b *dBlock, eip uint32) {
	b.tag = eip
	b.steps = 0
	b.insts = b.insts[:0]
	b.hits, b.jit = 0, nil
	defer func() { b.steps += len(b.insts) }()
	span := c.CodeHi - c.CodeLo
	for len(b.insts) < blockMax && eip-c.CodeLo < span && c.icept[eip>>6]&(1<<(eip&63)) == 0 {
		d := &c.dcache[eip&dcacheMask]
		if d.tag != eip {
			c.decodeCached(d, eip)
		}
		if d.kind == dSlow {
			return
		}
		if n := len(b.insts); d.kind == dJcc && n > 0 {
			if fk := fusedJcc(&b.insts[n-1]); fk != dSlow {
				p := &b.insts[n-1]
				p.kind, p.cond, p.len2, p.target = fk, d.aux, d.len, d.imm
				b.steps++ // the Jcc folded into p
				return
			}
		}
		b.insts = append(b.insts, *d)
		if endsBlock(d.kind) {
			return
		}
		eip += uint32(d.len)
	}
}

// SetCodeRange enables the decoded-instruction cache for guest code in [lo, hi).
func (c *CPU) SetCodeRange(lo, hi uint32) {
	c.CodeLo, c.CodeHi = lo, hi
	if c.jitc != nil {
		c.jitc.stale = true
	}
	if c.watchFn == nil {
		c.writeSpan = ArenaSize - 7 - hi
	}
	if c.dcache == nil {
		c.dcache = newDCache()
		c.blocks = make([]dBlock, 1<<blockBits)
	}
	c.FlushCodeCache()
}

// decodeModRM decodes the ModRM (and SIB/displacement) at m[p:], filling the addressing fields of
// d. It returns the number of bytes consumed. Mirrors calcModRMAddr.
func decodeModRM(m []byte, p uint32, d *dInst) uint32 {
	modrm := m[p]
	n := uint32(1)
	mod := modrm >> 6
	rm := modrm & 7
	d.reg = (modrm >> 3) & 7
	d.rm = rm
	d.base, d.index, d.scale, d.disp = -1, -1, 0, 0
	if mod == 3 {
		d.isMem = false
		return n
	}
	d.isMem = true
	if rm == 4 {
		sib := m[p+n]
		n++
		d.scale = (sib >> 6) & 3
		if idx := (sib >> 3) & 7; idx != 4 {
			d.index = int8(idx)
		}
		if base := sib & 7; base == 5 && mod == 0 {
			d.disp = le32(m, p+n)
			n += 4
		} else {
			d.base = int8(base)
		}
	} else if rm == 5 && mod == 0 {
		d.disp = le32(m, p+n)
		n += 4
	} else {
		d.base = int8(rm)
	}
	switch mod {
	case 1:
		d.disp += uint32(int32(int8(m[p+n])))
		n++
	case 2:
		d.disp += le32(m, p+n)
		n += 4
	}
	return n
}

func le32(m []byte, p uint32) uint32 {
	return uint32(m[p]) | uint32(m[p+1])<<8 | uint32(m[p+2])<<16 | uint32(m[p+3])<<24
}

// decodeCached decodes the instruction at eip into d (d.kind = dSlow if the form is not cached).
func (c *CPU) decodeCached(d *dInst, eip uint32) {
	c.dcacheUsed = true
	m := c.Mem
	d.tag = eip
	d.kind = dSlow
	d.isMem = false
	p := eip
	if q := p; m[q] == 0x66 || m[q] == 0x2E || m[q] == 0x0F {
		for m[q] == 0x66 || m[q] == 0x2E {
			q++
		}
		if m[q] == 0x0F && m[q+1] == 0x1F && (m[q+2]>>3)&7 == 0 {
			n := decodeModRM(m, q+2, d)
			d.kind, d.len = dNop, uint8(q+2+n-eip)
			return
		}
	}
	pfx := uint8(0)
	if b := m[p]; b == 0xF3 || b == 0xF2 {
		pfx = b
		p++
	}
	op := m[p]
	p++
	set := func(kind uint8, end uint32) {
		d.kind = kind
		d.len = uint8(end - eip)
		if !d.isMem {
			switch kind {
			case dMovLoad32:
				d.kind = dMovLoad32R
			case dMovStore32:
				d.kind = dMovStore32R
			case dAluRMR32:
				d.kind = dAluRMR32R
			case dAluRRM32:
				d.kind = dAluRRM32R
			case dGrp1RM32:
				d.kind = dGrp1RM32R
			}
		}
	}

	if pfx != 0 {
		// Only scalar SSE forms take a prefix.
		if op != 0x0F {
			return
		}
		op2 := m[p]
		p++
		switch op2 {
		case 0x10, 0x11:
			n := decodeModRM(m, p, d)
			k := dMovssLoad
			if pfx == 0xF3 && op2 == 0x11 {
				k = dMovssStore
			} else if pfx == 0xF2 && op2 == 0x10 {
				k = dMovsdLoad
			} else if pfx == 0xF2 {
				k = dMovsdStore
			}
			set(k, p+n)
		case 0x58, 0x59, 0x5C, 0x5E, 0x51, 0x5D, 0x5F:
			n := decodeModRM(m, p, d)
			d.aux = op2
			switch {
			case pfx == 0xF2:
				set(dArithSD, p+n)
			case op2 == 0x51 || op2 == 0x5D || op2 == 0x5F:
				set(dArithSS2, p+n)
			default:
				set(dArithSS, p+n)
			}
		case 0x5A, 0x2C, 0x2A:
			n := decodeModRM(m, p, d)
			k := map[uint8][2]uint8{0x5A: {dCvtSS2SD, dCvtSD2SS}, 0x2C: {dCvttSS2SI, dCvttSD2SI}, 0x2A: {dCvtSI2SS, dCvtSI2SD}}[op2]
			if pfx == 0xF3 {
				set(k[0], p+n)
			} else {
				set(k[1], p+n)
			}
		}
		return
	}
	if op == 0x66 {
		c.decode66(d, eip, p, set)
		return
	}

	if op < 0x40 && op&7 < 6 {
		switch op & 7 {
		case 0:
			n := decodeModRM(m, p, d)
			d.aux = (op >> 3) & 7
			set(dAlu8RMR, p+n)
		case 2:
			n := decodeModRM(m, p, d)
			d.aux = (op >> 3) & 7
			set(dAlu8RRM, p+n)
		case 4:
			d.aux = (op >> 3) & 7
			d.imm = uint32(m[p])
			set(dAluALImm, p+1)
		case 1:
			n := decodeModRM(m, p, d)
			d.aux = (op >> 3) & 7
			set(dAluRMR32, p+n)
		case 3:
			n := decodeModRM(m, p, d)
			d.aux = (op >> 3) & 7
			set(dAluRRM32, p+n)
		case 5:
			d.aux = (op >> 3) & 7
			d.imm = le32(m, p)
			set(dAluEAXImm, p+4)
		}
		return
	}

	switch {
	case op >= 0x40 && op <= 0x47:
		d.reg = op & 7
		set(dIncR, p)
	case op >= 0x48 && op <= 0x4F:
		d.reg = op & 7
		set(dDecR, p)
	case op >= 0x50 && op <= 0x57:
		d.reg = op & 7
		set(dPushR, p)
	case op >= 0x58 && op <= 0x5F:
		d.reg = op & 7
		set(dPopR, p)
	case op >= 0x70 && op <= 0x7F:
		d.aux = op & 0x0F
		d.imm = p + 1 + uint32(int32(int8(m[p])))
		set(dJcc, p+1)
	case op == 0x80:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		d.imm = uint32(m[p+n])
		set(dGrp1RM8, p+n+1)
	case op == 0x81:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		d.imm = le32(m, p+n)
		set(dGrp1RM32, p+n+4)
	case op == 0x83:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		d.imm = uint32(int32(int8(m[p+n])))
		set(dGrp1RM32, p+n+1)
	case op == 0x85:
		n := decodeModRM(m, p, d)
		set(dTestRMR32, p+n)
	case op == 0x84:
		set(dTestRMR8, p+decodeModRM(m, p, d))
	case op == 0x88:
		set(dMovStore8, p+decodeModRM(m, p, d))
	case op == 0x8A:
		set(dMovLoad8, p+decodeModRM(m, p, d))
	case op == 0x90:
		set(dNop, p)
	case op == 0xA8:
		d.imm = uint32(m[p])
		set(dTestALImm, p+1)
	case op >= 0xB0 && op <= 0xB7:
		d.reg = op & 7
		d.imm = uint32(m[p])
		set(dMovR8Imm, p+1)
	case op == 0xC2:
		d.imm = uint32(m[p]) | uint32(m[p+1])<<8
		set(dRetImm, p+2)
	case op >= 0xB8 && op <= 0xBF:
		d.reg = op & 7
		d.imm = le32(m, p)
		set(dMovRImm32, p+4)
	case op == 0xC1:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		d.imm = uint32(m[p+n])
		set(dShift32, p+n+1)
	case op == 0xD1:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		d.imm = 1
		set(dShift32, p+n)
	case op == 0xD3:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		set(dShift32CL, p+n)
	case op == 0x69:
		n := decodeModRM(m, p, d)
		d.imm = le32(m, p+n)
		set(dImul32, p+n+4)
	case op == 0x6B:
		n := decodeModRM(m, p, d)
		d.imm = uint32(int32(int8(m[p+n])))
		set(dImul32, p+n+1)
	case op == 0xC6:
		if n := decodeModRM(m, p, d); d.reg == 0 {
			d.imm = uint32(m[p+n])
			set(dMovImm8, p+n+1)
		}
	case op == 0xFF:
		n := decodeModRM(m, p, d)
		switch d.reg {
		case 0:
			set(dIncRM32, p+n)
		case 1:
			set(dDecRM32, p+n)
		case 2:
			set(dCallInd, p+n)
		case 4:
			set(dJmpInd, p+n)
		case 6:
			set(dPushRM32, p+n)
		}
	case op == 0x89:
		n := decodeModRM(m, p, d)
		set(dMovStore32, p+n)
	case op == 0x8B:
		n := decodeModRM(m, p, d)
		set(dMovLoad32, p+n)
	case op == 0x8D:
		if n := decodeModRM(m, p, d); d.isMem {
			set(dLea, p+n)
		}
	case op == 0xC3:
		set(dRet, p)
	case op == 0xC7:
		if n := decodeModRM(m, p, d); d.reg == 0 {
			d.imm = le32(m, p+n)
			set(dMovImm32, p+n+4)
		}
	case op == 0xE8:
		rel := le32(m, p)
		d.imm = p + 4 + rel
		d.aux = 0
		if rel != 0 {
			d.aux = 1 // records a call frame when TraceCalls is on (PIC "call $+5" does not)
		}
		set(dCall, p+4)
	case op == 0xE9:
		d.imm = p + 4 + le32(m, p)
		set(dJmp, p+4)
	case op == 0xEB:
		d.imm = p + 1 + uint32(int32(int8(m[p])))
		set(dJmp, p+1)
	case op == 0xF7:
		if n := decodeModRM(m, p, d); d.reg == 2 {
			set(dNot32, p+n)
		} else if d.reg == 3 {
			set(dNeg32, p+n)
		}
	case op == 0xF6:
		if n := decodeModRM(m, p, d); d.reg == 0 || d.reg == 1 {
			d.imm = uint32(m[p+n])
			set(dTestRMI8, p+n+1)
		} else if d.reg == 2 {
			set(dNot8, p+n)
		} else if d.reg == 3 {
			set(dNeg8, p+n)
		}
	case op == 0x0F:
		op2 := m[p]
		p++
		switch {
		case op2 >= 0x80 && op2 <= 0x8F:
			d.aux = op2 & 0x0F
			d.imm = p + 4 + le32(m, p)
			set(dJcc, p+4)
		case op2 >= 0x40 && op2 <= 0x4F:
			n := decodeModRM(m, p, d)
			d.aux = op2 & 0x0F
			set(dCmov, p+n)
		case op2 >= 0x90 && op2 <= 0x9F:
			n := decodeModRM(m, p, d)
			d.aux = op2 & 0x0F
			set(dSetcc, p+n)
		case op2 == 0xAF:
			set(dImulRRM32, p+decodeModRM(m, p, d))
		case op2 == 0xB6:
			set(dMovzx8, p+decodeModRM(m, p, d))
		case op2 == 0xB7:
			set(dMovzx16, p+decodeModRM(m, p, d))
		case op2 == 0xBE:
			set(dMovsx8, p+decodeModRM(m, p, d))
		case op2 == 0xBF:
			set(dMovsx16, p+decodeModRM(m, p, d))
		case op2 == 0x10 || op2 == 0x28:
			n := decodeModRM(m, p, d)
			set(dMovapsLoad, p+n)
		case op2 == 0x11 || op2 == 0x29:
			n := decodeModRM(m, p, d)
			set(dMovapsStore, p+n)
		case op2 == 0x2E || op2 == 0x2F:
			n := decodeModRM(m, p, d)
			set(dUcomiss, p+n)
		case op2 == 0x57:
			n := decodeModRM(m, p, d)
			set(dXorps, p+n)
		case op2 == 0x51 || op2 == 0x58 || op2 == 0x59 || op2 == 0x5C || op2 == 0x5D || op2 == 0x5E || op2 == 0x5F:
			n := decodeModRM(m, p, d)
			d.aux = op2
			set(dArithPS, p+n)
		case op2 >= 0x54 && op2 <= 0x56:
			n := decodeModRM(m, p, d)
			d.aux = op2
			set(dLogicPS, p+n)
		}
	}
}

// decode66 decodes the 16-bit and 66-prefixed SSE forms (op at m[p]).
func (c *CPU) decode66(d *dInst, eip, p uint32, set func(uint8, uint32)) {
	m := c.Mem
	op := m[p]
	p++
	switch {
	case op < 0x40 && op&7 == 1:
		n := decodeModRM(m, p, d)
		d.aux = (op >> 3) & 7
		set(dAlu16RMR, p+n)
	case op < 0x40 && op&7 == 3:
		n := decodeModRM(m, p, d)
		d.aux = (op >> 3) & 7
		set(dAlu16RRM, p+n)
	case op == 0x81:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		d.imm = uint32(m[p+n]) | uint32(m[p+n+1])<<8
		set(dGrp1RM16, p+n+2)
	case op == 0x83:
		n := decodeModRM(m, p, d)
		d.aux = d.reg
		d.imm = uint32(uint16(int16(int8(m[p+n]))))
		set(dGrp1RM16, p+n+1)
	case op == 0x85:
		set(dTest16, p+decodeModRM(m, p, d))
	case op == 0x89:
		set(dMov16Store, p+decodeModRM(m, p, d))
	case op == 0x8B:
		set(dMov16Load, p+decodeModRM(m, p, d))
	case op == 0xC7:
		if n := decodeModRM(m, p, d); d.reg == 0 {
			d.imm = uint32(m[p+n]) | uint32(m[p+n+1])<<8
			set(dMov16Imm, p+n+2)
		}
	case op == 0x90:
		set(dNop, p)
	case op == 0x0F:
		op2 := m[p]
		p++
		switch op2 {
		case 0x2E, 0x2F:
			set(dUcomisd, p+decodeModRM(m, p, d))
		case 0x70:
			n := decodeModRM(m, p, d)
			d.imm = uint32(m[p+n])
			set(dPshufd, p+n+1)
		case 0x6E:
			set(dMovdToX, p+decodeModRM(m, p, d))
		case 0x7E:
			set(dMovdFromX, p+decodeModRM(m, p, d))
		}
	}
}

func (c *CPU) ea(d *dInst) uint32 {
	a := d.disp
	if d.base >= 0 {
		a += c.Regs[d.base]
	}
	if d.index >= 0 {
		a += c.Regs[d.index] << d.scale
	}
	return a
}

// execBlock runs decoded instructions in order, stopping early if one halts the CPU. c.EIP is
// advanced before each instruction executes, matching the full decoder (which has fetched the
// whole instruction first). Keeping the loop inside the switch function avoids a call per
// instruction.
func (c *CPU) execBlock(insts []dInst) {
	for i := range insts {
		d := &insts[i]
		c.Steps++
		start := c.EIP
		c.EIP = start + uint32(d.len)
		switch d.kind {
		case dMovLoad32:
			if d.isMem {
				c.Regs[d.reg] = c.Read32(c.ea(d))
			} else {
				c.Regs[d.reg] = c.Regs[d.rm]
			}
		case dMovStore32:
			if d.isMem {
				c.Write32(c.ea(d), c.Regs[d.reg])
			} else {
				c.Regs[d.rm] = c.Regs[d.reg]
			}
		case dMovImm32:
			if d.isMem {
				c.Write32(c.ea(d), d.imm)
			} else {
				c.Regs[d.rm] = d.imm
			}
		case dAluRMR32:
			if d.isMem {
				addr := c.ea(d)
				if res, wb := c.aluOp32(int(d.aux), c.Read32(addr), c.Regs[d.reg]); wb {
					c.Write32(addr, res)
				}
			} else if res, wb := c.aluOp32(int(d.aux), c.Regs[d.rm], c.Regs[d.reg]); wb {
				c.Regs[d.rm] = res
			}
		case dAluRRM32:
			var b uint32
			if d.isMem {
				b = c.Read32(c.ea(d))
			} else {
				b = c.Regs[d.rm]
			}
			if res, wb := c.aluOp32(int(d.aux), c.Regs[d.reg], b); wb {
				c.Regs[d.reg] = res
			}
		case dAluEAXImm:
			if res, wb := c.aluOp32(int(d.aux), c.Regs[RegEAX], d.imm); wb {
				c.Regs[RegEAX] = res
			}
		case dGrp1RM32:
			if d.isMem {
				addr := c.ea(d)
				if res, wb := c.aluOp32(int(d.aux), c.Read32(addr), d.imm); wb {
					c.Write32(addr, res)
				}
			} else if res, wb := c.aluOp32(int(d.aux), c.Regs[d.rm], d.imm); wb {
				c.Regs[d.rm] = res
			}
		case dGrp1RM8:
			if d.isMem {
				addr := c.ea(d)
				if res, wb := c.aluOp8(int(d.aux), c.Read8(addr), uint8(d.imm)); wb {
					c.Write8(addr, res)
				}
			} else if res, wb := c.aluOp8(int(d.aux), c.GetReg8(int(d.rm)), uint8(d.imm)); wb {
				c.SetReg8(int(d.rm), res)
			}
		case dTestRMR32:
			var a uint32
			if d.isMem {
				a = c.Read32(c.ea(d))
			} else {
				a = c.Regs[d.rm]
			}
			c.UpdateFlagsLogic32(a & c.Regs[d.reg])
		case dTestRMI8:
			var a uint8
			if d.isMem {
				a = c.Read8(c.ea(d))
			} else {
				a = c.GetReg8(int(d.rm))
			}
			c.UpdateFlagsLogic8(a & uint8(d.imm))
		case dLea:
			c.Regs[d.reg] = c.ea(d)
		case dIncR:
			cf := c.Flags & FlagCF
			v := c.Regs[d.reg]
			c.UpdateFlagsAdd32(v, 1, v+1)
			c.Regs[d.reg] = v + 1
			c.Flags = c.Flags&^FlagCF | cf
		case dDecR:
			cf := c.Flags & FlagCF
			v := c.Regs[d.reg]
			c.UpdateFlagsSub32(v, 1, v-1)
			c.Regs[d.reg] = v - 1
			c.Flags = c.Flags&^FlagCF | cf
		case dPushR:
			c.Push32(c.Regs[d.reg])
		case dPopR:
			c.Regs[d.reg] = c.Pop32()
		case dJcc:
			if c.EvalCond(d.aux) {
				c.EIP = d.imm
			}
		case dJmp:
			c.EIP = d.imm
		case dCall:
			c.Push32(c.EIP)
			if c.TraceCalls && d.aux != 0 {
				c.CallStack = append(c.CallStack, CallFrame{CallerEIP: start, TargetEIP: d.imm, ESP: c.Regs[RegESP]})
				c.VisitedFuncs[d.imm]++
			}
			c.EIP = d.imm
		case dRet:
			c.Ret()
		case dMovzx8, dMovsx8:
			var v uint8
			if d.isMem {
				v = c.Read8(c.ea(d))
			} else {
				v = c.GetReg8(int(d.rm))
			}
			if d.kind == dMovzx8 {
				c.Regs[d.reg] = uint32(v)
			} else {
				c.Regs[d.reg] = uint32(int32(int8(v)))
			}
		case dMovzx16, dMovsx16:
			var v uint16
			if d.isMem {
				v = c.Read16(c.ea(d))
			} else {
				v = c.GetReg16(int(d.rm))
			}
			if d.kind == dMovzx16 {
				c.Regs[d.reg] = uint32(v)
			} else {
				c.Regs[d.reg] = uint32(int32(int16(v)))
			}
		case dCmov:
			var v uint32
			if d.isMem {
				v = c.Read32(c.ea(d))
			} else {
				v = c.Regs[d.rm]
			}
			if c.EvalCond(d.aux) {
				c.Regs[d.reg] = v
			}
		case dMovssLoad:
			if d.isMem {
				addr := c.ea(d)
				clear(c.XMM[d.reg][:])
				c.setXMMF32(int(d.reg), 0, c.ReadF32(addr))
			} else {
				c.setXMMF32(int(d.reg), 0, c.getXMMF32(int(d.rm), 0))
			}
		case dMovssStore:
			if d.isMem {
				c.WriteF32(c.ea(d), c.getXMMF32(int(d.reg), 0))
			} else {
				c.setXMMF32(int(d.rm), 0, c.getXMMF32(int(d.reg), 0))
			}
		case dMovsdLoad:
			if d.isMem {
				addr := c.ea(d)
				clear(c.XMM[d.reg][:])
				c.setXMMF64(int(d.reg), 0, c.ReadF64(addr))
			} else {
				c.setXMMF64(int(d.reg), 0, c.getXMMF64(int(d.rm), 0))
			}
		case dMovsdStore:
			if d.isMem {
				c.WriteF64(c.ea(d), c.getXMMF64(int(d.reg), 0))
			} else {
				c.setXMMF64(int(d.rm), 0, c.getXMMF64(int(d.reg), 0))
			}
		case dArithSS:
			a := c.getXMMF32(int(d.reg), 0)
			var b float32
			if d.isMem {
				b = c.ReadF32(c.ea(d))
			} else {
				b = c.getXMMF32(int(d.rm), 0)
			}
			var r float32
			switch d.aux {
			case 0x58:
				r = sseAdd(a, b)
			case 0x59:
				r = sseMul(a, b)
			case 0x5C:
				r = a - b
			case 0x5E:
				r = a / b
			}
			c.setXMMF32(int(d.reg), 0, r)
		case dMovapsLoad:
			if d.isMem {
				c.XMM[d.reg] = c.Read128(c.ea(d))
			} else {
				c.XMM[d.reg] = c.XMM[d.rm]
			}
		case dMovapsStore:
			if d.isMem {
				addr := c.ea(d)
				c.Write128(addr, c.XMM[d.reg])
			} else {
				c.XMM[d.rm] = c.XMM[d.reg]
			}
		case dUcomiss:
			a := float64(c.getXMMF32(int(d.reg), 0))
			var b float64
			if d.isMem {
				b = float64(c.ReadF32(c.ea(d)))
			} else {
				b = float64(c.getXMMF32(int(d.rm), 0))
			}
			c.fpuCompareEFlags(a, b)
		case dMovLoad32R:
			c.Regs[d.reg] = c.Regs[d.rm]
		case dMovStore32R:
			c.Regs[d.rm] = c.Regs[d.reg]
		case dAluRMR32R:
			if res, wb := c.aluOp32(int(d.aux), c.Regs[d.rm], c.Regs[d.reg]); wb {
				c.Regs[d.rm] = res
			}
		case dAluRRM32R:
			if res, wb := c.aluOp32(int(d.aux), c.Regs[d.reg], c.Regs[d.rm]); wb {
				c.Regs[d.reg] = res
			}
		case dGrp1RM32R:
			if res, wb := c.aluOp32(int(d.aux), c.Regs[d.rm], d.imm); wb {
				c.Regs[d.rm] = res
			}
		case dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dCmpEAXJcc:
			var a, b uint32
			switch d.kind {
			case dCmpRMRJcc:
				a, b = c.rm32(d), c.Regs[d.reg]
			case dCmpRRMJcc:
				a, b = c.Regs[d.reg], c.rm32(d)
			case dCmpRMIJcc:
				a, b = c.rm32(d), d.imm
			default:
				a, b = c.Regs[RegEAX], d.imm
			}
			c.UpdateFlagsSub32(a, b, a-b) // = aluOp32(CMP)
			if c.Halted {
				return
			}
			c.Steps++
			c.EIP += uint32(d.len2)
			if c.EvalCond(d.cond) {
				c.EIP = d.target
			}
		case dTestRMRJcc, dTestRMIJcc, dCmpRMI8Jcc:
			switch d.kind {
			case dTestRMRJcc:
				c.UpdateFlagsLogic32(c.rm32(d) & c.Regs[d.reg])
			case dTestRMIJcc:
				c.UpdateFlagsLogic8(c.rm8(d) & uint8(d.imm))
			default:
				a, b := c.rm8(d), uint8(d.imm)
				c.UpdateFlagsSub8(a, b, a-b) // = aluOp8(CMP)
			}
			if c.Halted {
				return
			}
			c.Steps++
			c.EIP += uint32(d.len2)
			if c.EvalCond(d.cond) {
				c.EIP = d.target
			}
		case dNop:
		case dMovRImm32:
			c.Regs[d.reg] = d.imm
		case dShift32, dShift32CL:
			cnt := uint8(d.imm)
			if d.kind == dShift32CL {
				cnt = c.GetReg8(RegECX)
			}
			if d.isMem {
				addr := c.ea(d)
				c.Write32(addr, c.shiftOp32(int(d.aux), c.Read32(addr), cnt))
			} else {
				c.Regs[d.rm] = c.shiftOp32(int(d.aux), c.Regs[d.rm], cnt)
			}
		case dImulRRM32:
			prod := int64(int32(c.Regs[d.reg])) * int64(int32(c.rm32(d)))
			res := uint32(prod)
			c.Regs[d.reg] = res
			c.SetFlag(FlagCF|FlagOF, prod != int64(int32(res)))
		case dNot32, dNeg32:
			var addr, val uint32
			if d.isMem {
				addr = c.ea(d)
				val = c.Read32(addr)
			} else {
				val = c.Regs[d.rm]
			}
			res := ^val
			if d.kind == dNeg32 {
				res = -val
				c.UpdateFlagsSub32(0, val, res)
				c.SetFlag(FlagCF, val != 0)
			}
			if d.isMem {
				c.Write32(addr, res)
			} else {
				c.Regs[d.rm] = res
			}
		case dSetcc:
			v := uint8(0)
			if c.EvalCond(d.aux) {
				v = 1
			}
			if d.isMem {
				c.Write8(c.ea(d), v)
			} else {
				c.SetReg8(int(d.rm), v)
			}
		case dImul32:
			src := int64(int32(c.rm32(d)))
			prod := src * int64(int32(d.imm))
			res := uint32(prod)
			c.Regs[d.reg] = res
			c.SetFlag(FlagCF|FlagOF, prod != int64(int32(res)))
		case dMovStore8:
			if d.isMem {
				c.Write8(c.ea(d), c.GetReg8(int(d.reg)))
			} else {
				c.SetReg8(int(d.rm), c.GetReg8(int(d.reg)))
			}
		case dMovLoad8:
			c.SetReg8(int(d.reg), c.rm8(d))
		case dMovImm8:
			if d.isMem {
				c.Write8(c.ea(d), uint8(d.imm))
			} else {
				c.SetReg8(int(d.rm), uint8(d.imm))
			}
		case dTestRMR8:
			c.UpdateFlagsLogic8(c.rm8(d) & c.GetReg8(int(d.reg)))
		case dTestALImm:
			c.UpdateFlagsLogic8(c.GetReg8(RegEAX) & uint8(d.imm))
		case dIncRM32, dDecRM32:
			cf := c.Flags & FlagCF
			var addr, v uint32
			if d.isMem {
				addr = c.ea(d)
				v = c.Read32(addr)
			} else {
				v = c.Regs[d.rm]
			}
			var res uint32
			if d.kind == dIncRM32 {
				res = v + 1
				c.UpdateFlagsAdd32(v, 1, res)
			} else {
				res = v - 1
				c.UpdateFlagsSub32(v, 1, res)
			}
			if d.isMem {
				c.Write32(addr, res)
			} else {
				c.Regs[d.rm] = res
			}
			c.Flags = c.Flags&^FlagCF | cf
		case dCallInd:
			target := c.rm32(d)
			c.Push32(c.EIP)
			if c.TraceCalls {
				c.CallStack = append(c.CallStack, CallFrame{CallerEIP: start, TargetEIP: target, ESP: c.Regs[RegESP]})
				c.VisitedFuncs[target]++
			}
			c.EIP = target
		case dJmpInd:
			c.EIP = c.rm32(d)
		case dPushRM32:
			c.Push32(c.rm32(d))
		case dXorps:
			var src [16]byte
			if d.isMem {
				src = c.Read128(c.ea(d))
			} else {
				src = c.XMM[d.rm]
			}
			for i := range src {
				c.XMM[d.reg][i] ^= src[i]
			}
		default: // forms the full decoder runs (exactly as when uncached)
			c.EIP = start
			c.Steps--
			c.stepFull()
			if c.EIP != start+uint32(d.len) && !endsBlock(d.kind) {
				return // the code changed under the block (or the instruction did something else)
			}
		}
		if c.Halted {
			return
		}
	}
}

func (c *CPU) rm32(d *dInst) uint32 {
	if d.isMem {
		return c.Read32(c.ea(d))
	}
	return c.Regs[d.rm]
}

func (c *CPU) rm8(d *dInst) uint8 {
	if d.isMem {
		return c.Read8(c.ea(d))
	}
	return c.GetReg8(int(d.rm))
}

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
	dUcomiss // 0F 2E / 0F 2F (no prefix)
	dXorps   // 0F 57 (no prefix)
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
}

func newDCache() []dInst {
	d := make([]dInst, dcacheSize)
	for i := range d {
		d[i].tag = dNoTag
	}
	return d
}

// FlushCodeCache discards all decoded instructions (call after modifying guest code from the host).
func (c *CPU) FlushCodeCache() {
	for i := range c.dcache {
		c.dcache[i].tag = dNoTag
	}
	c.CodeCacheFlushes++
}

// SetCodeRange enables the decoded-instruction cache for guest code in [lo, hi).
func (c *CPU) SetCodeRange(lo, hi uint32) {
	c.CodeLo, c.CodeHi = lo, hi
	c.writeSpan = ArenaSize - 8 - hi
	if c.dcache == nil {
		c.dcache = newDCache()
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
	m := c.Mem
	d.tag = eip
	d.kind = dSlow
	p := eip
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
		case 0x58, 0x59, 0x5C, 0x5E:
			if pfx == 0xF3 {
				n := decodeModRM(m, p, d)
				d.aux = op2
				set(dArithSS, p+n)
			}
		}
		return
	}

	if op < 0x40 && op&7 < 6 {
		switch op & 7 {
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
	case op == 0xF6:
		if n := decodeModRM(m, p, d); d.reg == 0 || d.reg == 1 {
			d.imm = uint32(m[p+n])
			set(dTestRMI8, p+n+1)
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

// execCached runs a decoded instruction. c.EIP is advanced first, matching the full decoder (which
// has fetched the whole instruction before executing it).
func (c *CPU) execCached(d *dInst) {
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
			r = a + b
		case 0x59:
			r = a * b
		case 0x5C:
			r = a - b
		case 0x5E:
			r = a / b
		}
		c.setXMMF32(int(d.reg), 0, r)
	case dMovapsLoad:
		if d.isMem {
			addr := c.ea(d)
			copy(c.XMM[d.reg][:], c.Mem[addr:addr+16])
		} else {
			c.XMM[d.reg] = c.XMM[d.rm]
		}
	case dMovapsStore:
		if d.isMem {
			addr := c.ea(d)
			c.storeCode128(addr, c.XMM[d.reg])
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
	case dXorps:
		var src [16]byte
		if d.isMem {
			addr := c.ea(d)
			copy(src[:], c.Mem[addr:addr+16])
		} else {
			src = c.XMM[d.rm]
		}
		for i := range src {
			c.XMM[d.reg][i] ^= src[i]
		}
	}
}

// storeCode128 writes 16 bytes like writeXMMOrMem128, flushing the cache if it lands in guest code.
func (c *CPU) storeCode128(addr uint32, v [16]byte) {
	copy(c.Mem[addr:addr+16], v[:])
	if addr < c.CodeHi && addr+16 > c.CodeLo {
		c.FlushCodeCache()
	}
}

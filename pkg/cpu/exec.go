package cpu

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

func (c *CPU) Fetch8() uint8 {
	v := c.Mem[c.EIP]
	c.EIP++
	return v
}

func (c *CPU) Fetch16() uint16 {
	v := binary.LittleEndian.Uint16(c.Mem[c.EIP : c.EIP+2])
	c.EIP += 2
	return v
}

func (c *CPU) Fetch32() uint32 {
	v := binary.LittleEndian.Uint32(c.Mem[c.EIP : c.EIP+4])
	c.EIP += 4
	return v
}

func (c *CPU) calcModRMAddr(modrm uint8, segOverride uint32, hasSeg bool) uint32 {
	mod := (modrm >> 6) & 3
	rm := int(modrm & 7)
	var addr uint32

	if rm == 4 { // SIB byte follows
		sib := c.Fetch8()
		scale := uint32(1 << ((sib >> 6) & 3))
		index := int((sib >> 3) & 7)
		base := int(sib & 7)

		var baseVal uint32
		if base == 5 && mod == 0 {
			baseVal = c.Fetch32()
		} else {
			baseVal = c.Regs[base]
		}

		var indexVal uint32
		if index != 4 {
			indexVal = c.Regs[index] * scale
		}
		addr = baseVal + indexVal
	} else if rm == 5 && mod == 0 {
		addr = c.Fetch32()
	} else {
		addr = c.Regs[rm]
	}

	if mod == 1 {
		disp8 := int32(int8(c.Fetch8()))
		addr = uint32(int32(addr) + disp8)
	} else if mod == 2 {
		disp32 := int32(c.Fetch32())
		addr = uint32(int32(addr) + disp32)
	}

	if hasSeg {
		addr += segOverride
	}
	return addr
}

func (c *CPU) readRM8(modrm uint8, segOverride uint32, hasSeg bool) (uint8, uint32, bool) {
	if (modrm >> 6) == 3 {
		return c.GetReg8(int(modrm & 7)), 0, true
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	return c.Read8(addr), addr, false
}

func (c *CPU) writeRM8(modrm uint8, addr uint32, isReg bool, val uint8) {
	if isReg {
		c.SetReg8(int(modrm&7), val)
	} else {
		c.Write8(addr, val)
	}
}

func (c *CPU) readRM16(modrm uint8, segOverride uint32, hasSeg bool) uint16 {
	if (modrm >> 6) == 3 {
		return c.GetReg16(int(modrm & 7))
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	return c.Read16(addr)
}

func (c *CPU) readWriteRM16(modrm uint8, segOverride uint32, hasSeg bool) (uint16, uint32, bool) {
	if (modrm >> 6) == 3 {
		return c.GetReg16(int(modrm & 7)), 0, true
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	return c.Read16(addr), addr, false
}

func (c *CPU) commitRM16(modrm uint8, addr uint32, isReg bool, val uint16) {
	if isReg {
		c.SetReg16(int(modrm&7), val)
	} else {
		c.Write16(addr, val)
	}
}

func (c *CPU) readRM32(modrm uint8, segOverride uint32, hasSeg bool) uint32 {
	if (modrm >> 6) == 3 {
		return c.Regs[modrm&7]
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	return c.Read32(addr)
}

func (c *CPU) writeRM32(modrm uint8, segOverride uint32, hasSeg bool, val uint32) {
	if (modrm >> 6) == 3 {
		c.Regs[modrm&7] = val
		return
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	c.Write32(addr, val)
}

func (c *CPU) readWriteRM32(modrm uint8, segOverride uint32, hasSeg bool) (uint32, uint32, bool) {
	if (modrm >> 6) == 3 {
		return c.Regs[modrm&7], 0, true
	}
	addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
	return c.Read32(addr), addr, false
}

func (c *CPU) commitRM32(modrm uint8, addr uint32, isReg bool, val uint32) {
	if isReg {
		c.Regs[modrm&7] = val
	} else {
		c.Write32(addr, val)
	}
}

func (c *CPU) aluOp8(op int, a, b uint8) (uint8, bool) {
	switch op {
	case 0: // ADD
		res := a + b
		c.UpdateFlagsAdd8(a, b, res)
		return res, true
	case 1: // OR
		res := a | b
		c.UpdateFlagsLogic8(res)
		return res, true
	case 2: // ADC
		cf := uint8(0)
		if c.GetFlag(FlagCF) {
			cf = 1
		}
		res := a + b + cf
		c.SetFlag(FlagCF, uint16(a)+uint16(b)+uint16(cf) > 0xFF)
		c.SetFlag(FlagZF, res == 0)
		c.SetFlag(FlagSF, (res&0x80) != 0)
		c.SetFlag(FlagPF, parity8(res))
		c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
		c.SetFlag(FlagOF, ((^(a ^ b) & (a ^ res)) & 0x80) != 0)
		return res, true
	case 3: // SBB
		cf := uint8(0)
		if c.GetFlag(FlagCF) {
			cf = 1
		}
		res := a - b - cf
		c.SetFlag(FlagCF, uint16(a) < uint16(b)+uint16(cf))
		c.SetFlag(FlagZF, res == 0)
		c.SetFlag(FlagSF, (res&0x80) != 0)
		c.SetFlag(FlagPF, parity8(res))
		c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
		c.SetFlag(FlagOF, (((a ^ b) & (a ^ res)) & 0x80) != 0)
		return res, true
	case 4: // AND
		res := a & b
		c.UpdateFlagsLogic8(res)
		return res, true
	case 5: // SUB
		res := a - b
		c.UpdateFlagsSub8(a, b, res)
		return res, true
	case 6: // XOR
		res := a ^ b
		c.UpdateFlagsLogic8(res)
		return res, true
	case 7: // CMP
		res := a - b
		c.UpdateFlagsSub8(a, b, res)
		return a, false
	}
	return a, false
}

func (c *CPU) aluOp16(op int, a, b uint16) (uint16, bool) {
	switch op {
	case 0: // ADD
		res := a + b
		c.UpdateFlagsAdd16(a, b, res)
		return res, true
	case 1: // OR
		res := a | b
		c.UpdateFlagsLogic16(res)
		return res, true
	case 2: // ADC
		cf := uint32(0)
		if c.GetFlag(FlagCF) {
			cf = 1
		}
		full := uint32(a) + uint32(b) + cf
		res := uint16(full)
		c.SetFlag(FlagCF, full > 0xFFFF)
		c.SetFlag(FlagZF, res == 0)
		c.SetFlag(FlagSF, (res&0x8000) != 0)
		c.SetFlag(FlagPF, parity8(uint8(res)))
		c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
		c.SetFlag(FlagOF, ((^(a ^ b) & (a ^ res)) & 0x8000) != 0)
		return res, true
	case 3: // SBB
		cf := uint32(0)
		if c.GetFlag(FlagCF) {
			cf = 1
		}
		res := uint16(uint32(a) - uint32(b) - cf)
		c.SetFlag(FlagCF, uint32(a) < uint32(b)+cf)
		c.SetFlag(FlagZF, res == 0)
		c.SetFlag(FlagSF, (res&0x8000) != 0)
		c.SetFlag(FlagPF, parity8(uint8(res)))
		c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
		c.SetFlag(FlagOF, (((a ^ b) & (a ^ res)) & 0x8000) != 0)
		return res, true
	case 4: // AND
		res := a & b
		c.UpdateFlagsLogic16(res)
		return res, true
	case 5: // SUB
		res := a - b
		c.UpdateFlagsSub16(a, b, res)
		return res, true
	case 6: // XOR
		res := a ^ b
		c.UpdateFlagsLogic16(res)
		return res, true
	case 7: // CMP
		res := a - b
		c.UpdateFlagsSub16(a, b, res)
		return a, false
	}
	return a, false
}

func (c *CPU) aluOp32(op int, a, b uint32) (uint32, bool) {
	switch op {
	case 0: // ADD
		res := a + b
		c.UpdateFlagsAdd32(a, b, res)
		return res, true
	case 1: // OR
		res := a | b
		c.UpdateFlagsLogic32(res)
		return res, true
	case 2: // ADC
		cf := uint64(0)
		if c.GetFlag(FlagCF) {
			cf = 1
		}
		full := uint64(a) + uint64(b) + cf
		res := uint32(full)
		c.SetFlag(FlagCF, full > 0xFFFFFFFF)
		c.SetFlag(FlagZF, res == 0)
		c.SetFlag(FlagSF, (res&0x80000000) != 0)
		c.SetFlag(FlagPF, parity8(uint8(res)))
		c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
		c.SetFlag(FlagOF, ((^(a ^ b) & (a ^ res)) & 0x80000000) != 0)
		return res, true
	case 3: // SBB
		cf := uint64(0)
		if c.GetFlag(FlagCF) {
			cf = 1
		}
		res := uint32(uint64(a) - uint64(b) - cf)
		c.SetFlag(FlagCF, uint64(a) < uint64(b)+cf)
		c.SetFlag(FlagZF, res == 0)
		c.SetFlag(FlagSF, (res&0x80000000) != 0)
		c.SetFlag(FlagPF, parity8(uint8(res)))
		c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
		c.SetFlag(FlagOF, (((a ^ b) & (a ^ res)) & 0x80000000) != 0)
		return res, true
	case 4: // AND
		res := a & b
		c.UpdateFlagsLogic32(res)
		return res, true
	case 5: // SUB
		res := a - b
		c.UpdateFlagsSub32(a, b, res)
		return res, true
	case 6: // XOR
		res := a ^ b
		c.UpdateFlagsLogic32(res)
		return res, true
	case 7: // CMP
		res := a - b
		c.UpdateFlagsSub32(a, b, res)
		return a, false
	}
	return a, false
}

func (c *CPU) shiftOp8(op int, val uint8, count uint8) uint8 {
	cnt := count & 0x1F
	if cnt == 0 {
		return val
	}
	switch op {
	case 0: // ROL
		res := bits.RotateLeft8(val, int(cnt))
		c.SetFlag(FlagCF, (res&1) != 0)
		return res
	case 1: // ROR
		res := bits.RotateLeft8(val, -int(cnt))
		c.SetFlag(FlagCF, (res&0x80) != 0)
		return res
	case 2: // RCL
		res := val
		for i := uint8(0); i < cnt; i++ {
			cf := uint8(0)
			if c.GetFlag(FlagCF) {
				cf = 1
			}
			newCF := (res & 0x80) != 0
			res = (res << 1) | cf
			c.SetFlag(FlagCF, newCF)
		}
		return res
	case 3: // RCR
		res := val
		for i := uint8(0); i < cnt; i++ {
			cf := uint8(0)
			if c.GetFlag(FlagCF) {
				cf = 0x80
			}
			newCF := (res & 1) != 0
			res = (res >> 1) | cf
			c.SetFlag(FlagCF, newCF)
		}
		return res
	case 4, 6: // SHL / SAL
		var cf bool
		var res uint8
		if cnt <= 8 {
			cf = ((val << (cnt - 1)) & 0x80) != 0
			res = val << cnt
		}
		c.UpdateFlagsLogic8(res)
		c.SetFlag(FlagCF, cf)
		return res
	case 5: // SHR
		var cf bool
		var res uint8
		if cnt <= 8 {
			cf = ((val >> (cnt - 1)) & 1) != 0
			res = val >> cnt
		}
		c.UpdateFlagsLogic8(res)
		c.SetFlag(FlagCF, cf)
		return res
	case 7: // SAR
		sval := int8(val)
		var cf bool
		var res uint8
		if cnt >= 8 {
			cf = sval < 0
			res = uint8(sval >> 7)
		} else {
			cf = ((sval >> (cnt - 1)) & 1) != 0
			res = uint8(sval >> cnt)
		}
		c.UpdateFlagsLogic8(res)
		c.SetFlag(FlagCF, cf)
		return res
	}
	return val
}

func (c *CPU) shiftOp16(op int, val uint16, count uint8) uint16 {
	cnt := count & 0x1F
	if cnt == 0 {
		return val
	}
	switch op {
	case 0: // ROL
		res := bits.RotateLeft16(val, int(cnt))
		c.SetFlag(FlagCF, (res&1) != 0)
		return res
	case 1: // ROR
		res := bits.RotateLeft16(val, -int(cnt))
		c.SetFlag(FlagCF, (res&0x8000) != 0)
		return res
	case 4, 6: // SHL / SAL
		var cf bool
		var res uint16
		if cnt <= 16 {
			cf = ((val << (cnt - 1)) & 0x8000) != 0
			res = val << cnt
		}
		c.UpdateFlagsLogic16(res)
		c.SetFlag(FlagCF, cf)
		return res
	case 5: // SHR
		var cf bool
		var res uint16
		if cnt <= 16 {
			cf = ((val >> (cnt - 1)) & 1) != 0
			res = val >> cnt
		}
		c.UpdateFlagsLogic16(res)
		c.SetFlag(FlagCF, cf)
		return res
	case 7: // SAR
		sval := int16(val)
		var cf bool
		var res uint16
		if cnt >= 16 {
			cf = sval < 0
			res = uint16(sval >> 15)
		} else {
			cf = ((sval >> (cnt - 1)) & 1) != 0
			res = uint16(sval >> cnt)
		}
		c.UpdateFlagsLogic16(res)
		c.SetFlag(FlagCF, cf)
		return res
	}
	return val
}

func (c *CPU) shiftOp32(op int, val uint32, count uint8) uint32 {
	cnt := count & 0x1F
	if cnt == 0 {
		return val
	}
	switch op {
	case 0: // ROL
		res := bits.RotateLeft32(val, int(cnt))
		c.SetFlag(FlagCF, (res&1) != 0)
		if cnt == 1 {
			c.SetFlag(FlagOF, ((res>>31)&1 != 0) != c.GetFlag(FlagCF))
		}
		return res
	case 1: // ROR
		res := bits.RotateLeft32(val, -int(cnt))
		c.SetFlag(FlagCF, (res&0x80000000) != 0)
		return res
	case 2: // RCL
		res := val
		for i := uint8(0); i < cnt; i++ {
			cf := uint32(0)
			if c.GetFlag(FlagCF) {
				cf = 1
			}
			newCF := (res & 0x80000000) != 0
			res = (res << 1) | cf
			c.SetFlag(FlagCF, newCF)
		}
		return res
	case 3: // RCR
		res := val
		for i := uint8(0); i < cnt; i++ {
			cf := uint32(0)
			if c.GetFlag(FlagCF) {
				cf = 0x80000000
			}
			newCF := (res & 1) != 0
			res = (res >> 1) | cf
			c.SetFlag(FlagCF, newCF)
		}
		return res
	case 4, 6: // SHL / SAL
		cf := ((val << (cnt - 1)) & 0x80000000) != 0
		res := val << cnt
		c.UpdateFlagsLogic32(res)
		c.SetFlag(FlagCF, cf)
		if cnt == 1 {
			c.SetFlag(FlagOF, ((res&0x80000000) != 0) != cf)
		}
		return res
	case 5: // SHR
		cf := ((val >> (cnt - 1)) & 1) != 0
		res := val >> cnt
		c.UpdateFlagsLogic32(res)
		c.SetFlag(FlagCF, cf)
		if cnt == 1 {
			c.SetFlag(FlagOF, (val&0x80000000) != 0)
		}
		return res
	case 7: // SAR
		sval := int32(val)
		cf := ((sval >> (cnt - 1)) & 1) != 0
		res := uint32(sval >> cnt)
		c.UpdateFlagsLogic32(res)
		c.SetFlag(FlagCF, cf)
		if cnt == 1 {
			c.SetFlag(FlagOF, false)
		}
		return res
	}
	return val
}

func (c *CPU) Step() {
	if c.Halted {
		return
	}

	// Check if current EIP is intercepted by a host thunk or lifted function
	if c.EIP >= ArenaSize || c.icept[c.EIP>>6]&(1<<(c.EIP&63)) != 0 {
		if thunk, ok := c.Intercepts[c.EIP]; ok {
			c.Steps++
			thunk(c)
			return
		}
	}

	if c.EIP >= c.CodeLo && c.EIP < c.CodeHi {
		d := &c.dcache[c.EIP&dcacheMask]
		if d.tag != c.EIP {
			c.decodeCached(d, c.EIP)
		}
		if d.kind != dSlow {
			c.Steps++
			c.execCached(d)
			return
		}
	}

	startEIP := c.EIP
	c.Steps++

	opSize16 := false
	repF3 := false
	repF2 := false
	var segOverride uint32
	hasSeg := false

	var op uint8
	for {
		op = c.Fetch8()
		switch op {
		case 0x66:
			opSize16 = true
			continue
		case 0xF3:
			repF3 = true
			continue
		case 0xF2:
			repF2 = true
			continue
		case 0xF0: // LOCK
			continue
		case 0x26, 0x2E, 0x36, 0x3E: // ES/CS/SS/DS segment override (flat 0 base)
			continue
		case 0x64: // FS override
			segOverride = c.FSBase
			hasSeg = true
			continue
		case 0x65: // GS override
			segOverride = c.GSBase
			hasSeg = true
			continue
		}
		break
	}

	// Standard 8/16/32 ALU family 0x00..0x3D (excluding 0x06,0x07,0x0E,0x0F,0x16,0x17,0x1E,0x1F,0x26,0x27,0x2E,0x2F,0x36,0x37,0x3E,0x3F)
	if op < 0x40 && (op&7) < 6 {
		aluOp := int((op >> 3) & 7)
		dir := op & 7
		switch dir {
		case 0: // r/m8, r8
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			a, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
			b := c.GetReg8(reg)
			if res, wb := c.aluOp8(aluOp, a, b); wb {
				c.writeRM8(modrm, addr, isReg, res)
			}
		case 1: // r/m32(16), r32(16)
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			if opSize16 {
				a, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
				b := c.GetReg16(reg)
				if res, wb := c.aluOp16(aluOp, a, b); wb {
					c.commitRM16(modrm, addr, isReg, res)
				}
			} else {
				a, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
				b := c.Regs[reg]
				if res, wb := c.aluOp32(aluOp, a, b); wb {
					c.commitRM32(modrm, addr, isReg, res)
				}
			}
		case 2: // r8, r/m8
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			a := c.GetReg8(reg)
			b, _, _ := c.readRM8(modrm, segOverride, hasSeg)
			if res, wb := c.aluOp8(aluOp, a, b); wb {
				c.SetReg8(reg, res)
			}
		case 3: // r32(16), r/m32(16)
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			if opSize16 {
				a := c.GetReg16(reg)
				b := c.readRM16(modrm, segOverride, hasSeg)
				if res, wb := c.aluOp16(aluOp, a, b); wb {
					c.SetReg16(reg, res)
				}
			} else {
				a := c.Regs[reg]
				b := c.readRM32(modrm, segOverride, hasSeg)
				if res, wb := c.aluOp32(aluOp, a, b); wb {
					c.Regs[reg] = res
				}
			}
		case 4: // AL, imm8
			imm := c.Fetch8()
			a := c.GetReg8(RegEAX)
			if res, wb := c.aluOp8(aluOp, a, imm); wb {
				c.SetReg8(RegEAX, res)
			}
		case 5: // EAX(AX), imm32(16)
			if opSize16 {
				imm := c.Fetch16()
				a := c.GetReg16(RegEAX)
				if res, wb := c.aluOp16(aluOp, a, imm); wb {
					c.SetReg16(RegEAX, res)
				}
			} else {
				imm := c.Fetch32()
				a := c.Regs[RegEAX]
				if res, wb := c.aluOp32(aluOp, a, imm); wb {
					c.Regs[RegEAX] = res
				}
			}
		}
		return
	}

	switch op {
	case 0x0F: // Two-byte opcodes
		op2 := c.Fetch8()
		switch {
		case op2 >= 0x18 && op2 <= 0x1F: // Hintable NOP / multi-byte NOP (0F 1F /0)
			modrm := c.Fetch8()
			if (modrm >> 6) != 3 {
				_ = c.calcModRMAddr(modrm, segOverride, hasSeg)
			}
			return

		case op2 == 0x31: // RDTSC
			tsc := c.Steps * 10
			c.Regs[RegEAX] = uint32(tsc)
			c.Regs[RegEDX] = uint32(tsc >> 32)
			return

		case op2 >= 0x40 && op2 <= 0x4F: // CMOVcc
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			if opSize16 {
				val := c.readRM16(modrm, segOverride, hasSeg)
				if c.EvalCond(op2 & 0x0F) {
					c.SetReg16(reg, val)
				}
			} else {
				val := c.readRM32(modrm, segOverride, hasSeg)
				if c.EvalCond(op2 & 0x0F) {
					c.Regs[reg] = val
				}
			}
			return

		case op2 >= 0x80 && op2 <= 0x8F: // Jcc rel32
			rel := int32(c.Fetch32())
			if c.EvalCond(op2 & 0x0F) {
				c.EIP = uint32(int32(c.EIP) + rel)
			}
			return

		case op2 >= 0x90 && op2 <= 0x9F: // SETcc r/m8
			modrm := c.Fetch8()
			_, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
			val := uint8(0)
			if c.EvalCond(op2 & 0x0F) {
				val = 1
			}
			c.writeRM8(modrm, addr, isReg, val)
			return

		case op2 == 0xA2: // CPUID
			leaf := c.Regs[RegEAX]
			switch leaf {
			case 0:
				c.Regs[RegEAX] = 1
				c.Regs[RegEBX] = 0x756e6547 // "Genu"
				c.Regs[RegEDX] = 0x49656e69 // "ineI"
				c.Regs[RegECX] = 0x6c65746e // "ntel"
			case 1:
				c.Regs[RegEAX] = 0x000006F6
				c.Regs[RegEBX] = 0x00010800
				c.Regs[RegECX] = 0x00000201
				c.Regs[RegEDX] = 0x078BFBFF // FPU, MMX, SSE, SSE2
			default:
				c.Regs[RegEAX] = 0
				c.Regs[RegEBX] = 0
				c.Regs[RegECX] = 0
				c.Regs[RegEDX] = 0
			}
			return

		case op2 == 0xA3, op2 == 0xAB, op2 == 0xB3, op2 == 0xBB: // BT(A3), BTS(AB), BTR(B3), BTC(BB) r/m32, r32
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			bitIdx := int32(c.Regs[reg])
			if (modrm >> 6) == 3 {
				rm := int(modrm & 7)
				b := uint32(bitIdx) & 31
				c.SetFlag(FlagCF, ((c.Regs[rm]>>b)&1) != 0)
				switch op2 {
				case 0xAB:
					c.Regs[rm] |= (1 << b)
				case 0xB3:
					c.Regs[rm] &^= (1 << b)
				case 0xBB:
					c.Regs[rm] ^= (1 << b)
				}
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				byteAddr := uint32(int32(addr) + (bitIdx >> 3))
				b := uint8(bitIdx & 7)
				v := c.Read8(byteAddr)
				c.SetFlag(FlagCF, ((v>>b)&1) != 0)
				switch op2 {
				case 0xAB:
					c.Write8(byteAddr, v|(1<<b))
				case 0xB3:
					c.Write8(byteAddr, v&^(1<<b))
				case 0xBB:
					c.Write8(byteAddr, v^(1<<b))
				}
			}
			return

		case op2 == 0xA4 || op2 == 0xA5: // SHLD
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			dst, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			var cnt uint8
			if op2 == 0xA4 {
				cnt = c.Fetch8() & 31
			} else {
				cnt = c.GetReg8(RegECX) & 31
			}
			if cnt > 0 {
				src := c.Regs[reg]
				cf := ((dst << (cnt - 1)) & 0x80000000) != 0
				res := (dst << cnt) | (src >> (32 - cnt))
				c.UpdateFlagsLogic32(res)
				c.SetFlag(FlagCF, cf)
				c.commitRM32(modrm, addr, isReg, res)
			}
			return

		case op2 == 0xAC || op2 == 0xAD: // SHRD
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			dst, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			var cnt uint8
			if op2 == 0xAC {
				cnt = c.Fetch8() & 31
			} else {
				cnt = c.GetReg8(RegECX) & 31
			}
			if cnt > 0 {
				src := c.Regs[reg]
				cf := ((dst >> (cnt - 1)) & 1) != 0
				res := (dst >> cnt) | (src << (32 - cnt))
				c.UpdateFlagsLogic32(res)
				c.SetFlag(FlagCF, cf)
				c.commitRM32(modrm, addr, isReg, res)
			}
			return

		case op2 == 0xAF: // IMUL r32(16), r/m32(16)
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			if opSize16 {
				a := int32(int16(c.GetReg16(reg)))
				b := int32(int16(c.readRM16(modrm, segOverride, hasSeg)))
				prod := a * b
				res := uint16(prod)
				c.SetReg16(reg, res)
				ovf := prod != int32(int16(res))
				c.SetFlag(FlagCF|FlagOF, ovf)
			} else {
				a := int64(int32(c.Regs[reg]))
				b := int64(int32(c.readRM32(modrm, segOverride, hasSeg)))
				prod := a * b
				res := uint32(prod)
				c.Regs[reg] = res
				ovf := prod != int64(int32(res))
				c.SetFlag(FlagCF|FlagOF, ovf)
			}
			return

		case op2 == 0xB0: // CMPXCHG r/m8, r8
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			dst, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
			al := c.GetReg8(RegEAX)
			c.UpdateFlagsSub8(al, dst, al-dst)
			if al == dst {
				c.writeRM8(modrm, addr, isReg, c.GetReg8(reg))
			} else {
				c.SetReg8(RegEAX, dst)
			}
			return

		case op2 == 0xB1: // CMPXCHG r/m32, r32
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			dst, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			eax := c.Regs[RegEAX]
			c.UpdateFlagsSub32(eax, dst, eax-dst)
			if eax == dst {
				c.commitRM32(modrm, addr, isReg, c.Regs[reg])
			} else {
				c.Regs[RegEAX] = dst
			}
			return

		case op2 == 0xB6: // MOVZX r32(16), r/m8
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			val, _, _ := c.readRM8(modrm, segOverride, hasSeg)
			if opSize16 {
				c.SetReg16(reg, uint16(val))
			} else {
				c.Regs[reg] = uint32(val)
			}
			return

		case op2 == 0xB7: // MOVZX r32, r/m16
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			val := c.readRM16(modrm, segOverride, hasSeg)
			c.Regs[reg] = uint32(val)
			return

		case op2 == 0xBA: // Group 8: BT/BTS/BTR/BTC r/m32, imm8
			modrm := c.Fetch8()
			subop := (modrm >> 3) & 7
			dst, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			imm := c.Fetch8() & 31
			c.SetFlag(FlagCF, ((dst>>imm)&1) != 0)
			switch subop {
			case 5: // BTS
				c.commitRM32(modrm, addr, isReg, dst|(1<<imm))
			case 6: // BTR
				c.commitRM32(modrm, addr, isReg, dst&^(1<<imm))
			case 7: // BTC
				c.commitRM32(modrm, addr, isReg, dst^(1<<imm))
			}
			return

		case op2 == 0xBC: // BSF
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			val := c.readRM32(modrm, segOverride, hasSeg)
			if val == 0 {
				c.SetFlag(FlagZF, true)
			} else {
				c.SetFlag(FlagZF, false)
				c.Regs[reg] = uint32(bits.TrailingZeros32(val))
			}
			return

		case op2 == 0xBD: // BSR
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			val := c.readRM32(modrm, segOverride, hasSeg)
			if val == 0 {
				c.SetFlag(FlagZF, true)
			} else {
				c.SetFlag(FlagZF, false)
				c.Regs[reg] = uint32(31 - bits.LeadingZeros32(val))
			}
			return

		case op2 == 0xBE: // MOVSX r32(16), r/m8
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			val, _, _ := c.readRM8(modrm, segOverride, hasSeg)
			if opSize16 {
				c.SetReg16(reg, uint16(int16(int8(val))))
			} else {
				c.Regs[reg] = uint32(int32(int8(val)))
			}
			return

		case op2 == 0xBF: // MOVSX r32, r/m16
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			val := c.readRM16(modrm, segOverride, hasSeg)
			c.Regs[reg] = uint32(int32(int16(val)))
			return

		case op2 == 0xC0: // XADD r/m8, r8
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			dst, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
			src := c.GetReg8(reg)
			res := dst + src
			c.UpdateFlagsAdd8(dst, src, res)
			c.SetReg8(reg, dst)
			c.writeRM8(modrm, addr, isReg, res)
			return

		case op2 == 0xC1: // XADD r/m32, r32
			modrm := c.Fetch8()
			reg := int((modrm >> 3) & 7)
			dst, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			src := c.Regs[reg]
			res := dst + src
			c.UpdateFlagsAdd32(dst, src, res)
			c.Regs[reg] = dst
			c.commitRM32(modrm, addr, isReg, res)
			return

		case op2 == 0xC7: // CMPXCHG8B m64
			modrm := c.Fetch8()
			addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
			mem64 := c.Read64(addr)
			edxEax := (uint64(c.Regs[RegEDX]) << 32) | uint64(c.Regs[RegEAX])
			if edxEax == mem64 {
				c.SetFlag(FlagZF, true)
				ecxEbx := (uint64(c.Regs[RegECX]) << 32) | uint64(c.Regs[RegEBX])
				c.Write64(addr, ecxEbx)
			} else {
				c.SetFlag(FlagZF, false)
				c.Regs[RegEAX] = uint32(mem64)
				c.Regs[RegEDX] = uint32(mem64 >> 32)
			}
			return

		case op2 >= 0xC8 && op2 <= 0xCF: // BSWAP r32
			reg := int(op2 & 7)
			c.Regs[reg] = bits.ReverseBytes32(c.Regs[reg])
			return
		}

		if c.execSSE(op2, opSize16, repF3, repF2, segOverride, hasSeg) {
			return
		}
		c.Halted = true
		c.HaltReason = fmt.Sprintf("unimplemented 0F opcode 0x0F 0x%02x at EIP=0x%08x", op2, startEIP)
		return

	case 0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47: // INC r32(16)
		reg := int(op & 7)
		cf := c.GetFlag(FlagCF)
		if opSize16 {
			v := c.GetReg16(reg)
			res := v + 1
			c.UpdateFlagsAdd16(v, 1, res)
			c.SetReg16(reg, res)
		} else {
			v := c.Regs[reg]
			res := v + 1
			c.UpdateFlagsAdd32(v, 1, res)
			c.Regs[reg] = res
		}
		c.SetFlag(FlagCF, cf)
		return

	case 0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0x4E, 0x4F: // DEC r32(16)
		reg := int(op & 7)
		cf := c.GetFlag(FlagCF)
		if opSize16 {
			v := c.GetReg16(reg)
			res := v - 1
			c.UpdateFlagsSub16(v, 1, res)
			c.SetReg16(reg, res)
		} else {
			v := c.Regs[reg]
			res := v - 1
			c.UpdateFlagsSub32(v, 1, res)
			c.Regs[reg] = res
		}
		c.SetFlag(FlagCF, cf)
		return

	case 0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57: // PUSH r32(16)
		reg := int(op & 7)
		if opSize16 {
			c.Push16(c.GetReg16(reg))
		} else {
			c.Push32(c.Regs[reg])
		}
		return

	case 0x58, 0x59, 0x5A, 0x5B, 0x5C, 0x5D, 0x5E, 0x5F: // POP r32(16)
		reg := int(op & 7)
		if opSize16 {
			c.SetReg16(reg, c.Pop16())
		} else {
			c.Regs[reg] = c.Pop32()
		}
		return

	case 0x60: // PUSHAD
		tempESP := c.Regs[RegESP]
		for i := 0; i < 8; i++ {
			if i == RegESP {
				c.Push32(tempESP)
			} else {
				c.Push32(c.Regs[i])
			}
		}
		return

	case 0x61: // POPAD
		for i := 7; i >= 0; i-- {
			v := c.Pop32()
			if i != RegESP {
				c.Regs[i] = v
			}
		}
		return

	case 0x68: // PUSH imm32(16)
		if opSize16 {
			c.Push16(c.Fetch16())
		} else {
			c.Push32(c.Fetch32())
		}
		return

	case 0x69: // IMUL r32(16), r/m32(16), imm32(16)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if opSize16 {
			src := int32(int16(c.readRM16(modrm, segOverride, hasSeg)))
			imm := int32(int16(c.Fetch16()))
			prod := src * imm
			res := uint16(prod)
			c.SetReg16(reg, res)
			c.SetFlag(FlagCF|FlagOF, prod != int32(int16(res)))
		} else {
			src := int64(int32(c.readRM32(modrm, segOverride, hasSeg)))
			imm := int64(int32(c.Fetch32()))
			prod := src * imm
			res := uint32(prod)
			c.Regs[reg] = res
			c.SetFlag(FlagCF|FlagOF, prod != int64(int32(res)))
		}
		return

	case 0x6A: // PUSH imm8 (sign-extended)
		imm := int32(int8(c.Fetch8()))
		if opSize16 {
			c.Push16(uint16(imm))
		} else {
			c.Push32(uint32(imm))
		}
		return

	case 0x6B: // IMUL r32(16), r/m32(16), imm8
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if opSize16 {
			src := int32(int16(c.readRM16(modrm, segOverride, hasSeg)))
			imm := int32(int8(c.Fetch8()))
			prod := src * imm
			res := uint16(prod)
			c.SetReg16(reg, res)
			c.SetFlag(FlagCF|FlagOF, prod != int32(int16(res)))
		} else {
			src := int64(int32(c.readRM32(modrm, segOverride, hasSeg)))
			imm := int64(int8(c.Fetch8()))
			prod := src * imm
			res := uint32(prod)
			c.Regs[reg] = res
			c.SetFlag(FlagCF|FlagOF, prod != int64(int32(res)))
		}
		return

	case 0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77,
		0x78, 0x79, 0x7A, 0x7B, 0x7C, 0x7D, 0x7E, 0x7F: // Jcc rel8
		rel := int32(int8(c.Fetch8()))
		if c.EvalCond(op & 0x0F) {
			c.EIP = uint32(int32(c.EIP) + rel)
		}
		return

	case 0x80, 0x82: // Grp1 r/m8, imm8
		modrm := c.Fetch8()
		aluOp := int((modrm >> 3) & 7)
		a, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
		imm := c.Fetch8()
		if res, wb := c.aluOp8(aluOp, a, imm); wb {
			c.writeRM8(modrm, addr, isReg, res)
		}
		return

	case 0x81: // Grp1 r/m32(16), imm32(16)
		modrm := c.Fetch8()
		aluOp := int((modrm >> 3) & 7)
		if opSize16 {
			a, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
			imm := c.Fetch16()
			if res, wb := c.aluOp16(aluOp, a, imm); wb {
				c.commitRM16(modrm, addr, isReg, res)
			}
		} else {
			a, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			imm := c.Fetch32()
			if res, wb := c.aluOp32(aluOp, a, imm); wb {
				c.commitRM32(modrm, addr, isReg, res)
			}
		}
		return

	case 0x83: // Grp1 r/m32(16), imm8 (sign-extended)
		modrm := c.Fetch8()
		aluOp := int((modrm >> 3) & 7)
		if opSize16 {
			a, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
			imm := uint16(int16(int8(c.Fetch8())))
			if res, wb := c.aluOp16(aluOp, a, imm); wb {
				c.commitRM16(modrm, addr, isReg, res)
			}
		} else {
			a, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			imm := uint32(int32(int8(c.Fetch8())))
			if res, wb := c.aluOp32(aluOp, a, imm); wb {
				c.commitRM32(modrm, addr, isReg, res)
			}
		}
		return

	case 0x84: // TEST r/m8, r8
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		a, _, _ := c.readRM8(modrm, segOverride, hasSeg)
		b := c.GetReg8(reg)
		c.UpdateFlagsLogic8(a & b)
		return

	case 0x85: // TEST r/m32(16), r32(16)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if opSize16 {
			a := c.readRM16(modrm, segOverride, hasSeg)
			b := c.GetReg16(reg)
			c.UpdateFlagsLogic16(a & b)
		} else {
			a := c.readRM32(modrm, segOverride, hasSeg)
			b := c.Regs[reg]
			c.UpdateFlagsLogic32(a & b)
		}
		return

	case 0x86: // XCHG r/m8, r8
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		a, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
		b := c.GetReg8(reg)
		c.writeRM8(modrm, addr, isReg, b)
		c.SetReg8(reg, a)
		return

	case 0x87: // XCHG r/m32(16), r32(16)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if opSize16 {
			a, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
			b := c.GetReg16(reg)
			c.commitRM16(modrm, addr, isReg, b)
			c.SetReg16(reg, a)
		} else {
			a, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			b := c.Regs[reg]
			c.commitRM32(modrm, addr, isReg, b)
			c.Regs[reg] = a
		}
		return

	case 0x88: // MOV r/m8, r8
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		val := c.GetReg8(reg)
		if (modrm >> 6) == 3 {
			c.SetReg8(int(modrm&7), val)
		} else {
			addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
			c.Write8(addr, val)
		}
		return

	case 0x89: // MOV r/m32(16), r32(16)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if opSize16 {
			val := c.GetReg16(reg)
			if (modrm >> 6) == 3 {
				c.SetReg16(int(modrm&7), val)
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				c.Write16(addr, val)
			}
		} else {
			c.writeRM32(modrm, segOverride, hasSeg, c.Regs[reg])
		}
		return

	case 0x8A: // MOV r8, r/m8
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		val, _, _ := c.readRM8(modrm, segOverride, hasSeg)
		c.SetReg8(reg, val)
		return

	case 0x8B: // MOV r32(16), r/m32(16)
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		if opSize16 {
			c.SetReg16(reg, c.readRM16(modrm, segOverride, hasSeg))
		} else {
			c.Regs[reg] = c.readRM32(modrm, segOverride, hasSeg)
		}
		return

	case 0x8D: // LEA r32(16), m
		modrm := c.Fetch8()
		reg := int((modrm >> 3) & 7)
		addr := c.calcModRMAddr(modrm, segOverride, false)
		if opSize16 {
			c.SetReg16(reg, uint16(addr))
		} else {
			c.Regs[reg] = addr
		}
		return

	case 0x8F: // POP r/m32(16)
		modrm := c.Fetch8()
		if opSize16 {
			val := c.Pop16()
			_, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
			c.commitRM16(modrm, addr, isReg, val)
		} else {
			val := c.Pop32()
			c.writeRM32(modrm, segOverride, hasSeg, val)
		}
		return

	case 0x90: // NOP / PAUSE
		return

	case 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97: // XCHG EAX, r32(16)
		reg := int(op & 7)
		if opSize16 {
			a := c.GetReg16(RegEAX)
			b := c.GetReg16(reg)
			c.SetReg16(RegEAX, b)
			c.SetReg16(reg, a)
		} else {
			c.Regs[RegEAX], c.Regs[reg] = c.Regs[reg], c.Regs[RegEAX]
		}
		return

	case 0x98: // CBW / CWDE
		if opSize16 {
			c.SetReg16(RegEAX, uint16(int16(int8(c.GetReg8(RegEAX)))))
		} else {
			c.Regs[RegEAX] = uint32(int32(int16(c.GetReg16(RegEAX))))
		}
		return

	case 0x99: // CWD / CDQ
		if opSize16 {
			if int16(c.GetReg16(RegEAX)) < 0 {
				c.SetReg16(RegEDX, 0xFFFF)
			} else {
				c.SetReg16(RegEDX, 0)
			}
		} else {
			if int32(c.Regs[RegEAX]) < 0 {
				c.Regs[RegEDX] = 0xFFFFFFFF
			} else {
				c.Regs[RegEDX] = 0
			}
		}
		return

	case 0x9C: // PUSHFD
		c.Push32(c.Flags)
		return

	case 0x9D: // POPFD
		c.Flags = c.Pop32() | 2
		return

	case 0x9E: // SAHF
		ah := c.GetReg8(4) // AH
		mask := uint32(FlagCF | FlagPF | FlagAF | FlagZF | FlagSF)
		c.Flags = (c.Flags &^ mask) | (uint32(ah) & mask) | 2
		return

	case 0x9F: // LAHF
		c.SetReg8(4, uint8(c.Flags)|2)
		return

	case 0xA0: // MOV AL, moffs8
		addr := c.Fetch32()
		if hasSeg {
			addr += segOverride
		}
		c.SetReg8(RegEAX, c.Read8(addr))
		return

	case 0xA1: // MOV EAX(AX), moffs32(16)
		addr := c.Fetch32()
		if hasSeg {
			addr += segOverride
		}
		if opSize16 {
			c.SetReg16(RegEAX, c.Read16(addr))
		} else {
			c.Regs[RegEAX] = c.Read32(addr)
		}
		return

	case 0xA2: // MOV moffs8, AL
		addr := c.Fetch32()
		if hasSeg {
			addr += segOverride
		}
		c.Write8(addr, c.GetReg8(RegEAX))
		return

	case 0xA3: // MOV moffs32(16), EAX(AX)
		addr := c.Fetch32()
		if hasSeg {
			addr += segOverride
		}
		if opSize16 {
			c.Write16(addr, c.GetReg16(RegEAX))
		} else {
			c.Write32(addr, c.Regs[RegEAX])
		}
		return

	case 0xA4, 0xA5, 0xA6, 0xA7, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF: // String instructions
		c.execStringOp(op, opSize16, repF3, repF2)
		return

	case 0xA8: // TEST AL, imm8
		imm := c.Fetch8()
		c.UpdateFlagsLogic8(c.GetReg8(RegEAX) & imm)
		return

	case 0xA9: // TEST EAX(AX), imm32(16)
		if opSize16 {
			imm := c.Fetch16()
			c.UpdateFlagsLogic16(c.GetReg16(RegEAX) & imm)
		} else {
			imm := c.Fetch32()
			c.UpdateFlagsLogic32(c.Regs[RegEAX] & imm)
		}
		return

	case 0xB0, 0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7: // MOV r8, imm8
		c.SetReg8(int(op&7), c.Fetch8())
		return

	case 0xB8, 0xB9, 0xBA, 0xBB, 0xBC, 0xBD, 0xBE, 0xBF: // MOV r32(16), imm32(16)
		reg := int(op & 7)
		if opSize16 {
			c.SetReg16(reg, c.Fetch16())
		} else {
			c.Regs[reg] = c.Fetch32()
		}
		return

	case 0xC0, 0xD0, 0xD2: // Grp2 shift r/m8
		modrm := c.Fetch8()
		subop := int((modrm >> 3) & 7)
		dst, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
		var cnt uint8
		if op == 0xC0 {
			cnt = c.Fetch8()
		} else if op == 0xD0 {
			cnt = 1
		} else {
			cnt = c.GetReg8(RegECX)
		}
		res := c.shiftOp8(subop, dst, cnt)
		c.writeRM8(modrm, addr, isReg, res)
		return

	case 0xC1, 0xD1, 0xD3: // Grp2 shift r/m32(16)
		modrm := c.Fetch8()
		subop := int((modrm >> 3) & 7)
		if opSize16 {
			dst, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
			var cnt uint8
			if op == 0xC1 {
				cnt = c.Fetch8()
			} else if op == 0xD1 {
				cnt = 1
			} else {
				cnt = c.GetReg8(RegECX)
			}
			res := c.shiftOp16(subop, dst, cnt)
			c.commitRM16(modrm, addr, isReg, res)
		} else {
			dst, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			var cnt uint8
			if op == 0xC1 {
				cnt = c.Fetch8()
			} else if op == 0xD1 {
				cnt = 1
			} else {
				cnt = c.GetReg8(RegECX)
			}
			res := c.shiftOp32(subop, dst, cnt)
			c.commitRM32(modrm, addr, isReg, res)
		}
		return

	case 0xC2: // RET imm16
		popBytes := uint32(c.Fetch16())
		c.RetN(popBytes)
		return

	case 0xC3: // RET
		c.Ret()
		return

	case 0xC6: // MOV r/m8, imm8
		modrm := c.Fetch8()
		if (modrm >> 6) == 3 {
			c.SetReg8(int(modrm&7), c.Fetch8())
		} else {
			addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
			c.Write8(addr, c.Fetch8())
		}
		return

	case 0xC7: // MOV r/m32(16), imm32(16)
		modrm := c.Fetch8()
		if opSize16 {
			if (modrm >> 6) == 3 {
				c.SetReg16(int(modrm&7), c.Fetch16())
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				c.Write16(addr, c.Fetch16())
			}
		} else {
			if (modrm >> 6) == 3 {
				c.Regs[modrm&7] = c.Fetch32()
			} else {
				addr := c.calcModRMAddr(modrm, segOverride, hasSeg)
				c.Write32(addr, c.Fetch32())
			}
		}
		return

	case 0xC8: // ENTER imm16, imm8
		allocSize := uint32(c.Fetch16())
		_ = c.Fetch8()
		c.Push32(c.Regs[RegEBP])
		c.Regs[RegEBP] = c.Regs[RegESP]
		c.Regs[RegESP] -= allocSize
		return

	case 0xC9: // LEAVE
		c.Regs[RegESP] = c.Regs[RegEBP]
		c.Regs[RegEBP] = c.Pop32()
		return

	case 0xCC: // INT3
		c.Halted = true
		c.HaltReason = fmt.Sprintf("INT3 breakpoint at EIP=0x%08x", startEIP)
		return

	case 0xD8, 0xD9, 0xDA, 0xDB, 0xDC, 0xDD, 0xDE, 0xDF: // x87 FPU
		c.execFPU(op, segOverride, hasSeg)
		return

	case 0xE0, 0xE1, 0xE2, 0xE3: // LOOPNE(E0), LOOPE(E1), LOOP(E2), JECXZ(E3)
		rel := int32(int8(c.Fetch8()))
		if op == 0xE3 {
			if c.Regs[RegECX] == 0 {
				c.EIP = uint32(int32(c.EIP) + rel)
			}
			return
		}
		c.Regs[RegECX]--
		take := c.Regs[RegECX] != 0
		if op == 0xE0 {
			take = take && !c.GetFlag(FlagZF)
		} else if op == 0xE1 {
			take = take && c.GetFlag(FlagZF)
		}
		if take {
			c.EIP = uint32(int32(c.EIP) + rel)
		}
		return

	case 0xE8: // CALL rel32
		rel := int32(c.Fetch32())
		target := uint32(int32(c.EIP) + rel)
		c.Push32(c.EIP)
		if c.TraceCalls && rel != 0 { // Skip recording PIC call $+5
			c.CallStack = append(c.CallStack, CallFrame{
				CallerEIP: startEIP,
				TargetEIP: target,
				ESP:       c.Regs[RegESP],
			})
			c.VisitedFuncs[target]++
		}
		c.EIP = target
		return

	case 0xE9: // JMP rel32
		rel := int32(c.Fetch32())
		c.EIP = uint32(int32(c.EIP) + rel)
		return

	case 0xEB: // JMP rel8
		rel := int32(int8(c.Fetch8()))
		c.EIP = uint32(int32(c.EIP) + rel)
		return

	case 0xF5: // CMC
		c.SetFlag(FlagCF, !c.GetFlag(FlagCF))
		return

	case 0xF6: // Grp3 r/m8
		modrm := c.Fetch8()
		subop := int((modrm >> 3) & 7)
		val, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
		switch subop {
		case 0, 1: // TEST r/m8, imm8
			imm := c.Fetch8()
			c.UpdateFlagsLogic8(val & imm)
		case 2: // NOT r/m8
			c.writeRM8(modrm, addr, isReg, ^val)
		case 3: // NEG r/m8
			res := -val
			c.UpdateFlagsSub8(0, val, res)
			c.SetFlag(FlagCF, val != 0)
			c.writeRM8(modrm, addr, isReg, res)
		case 4: // MUL r/m8 (AX = AL * r/m8)
			al := uint16(c.GetReg8(RegEAX))
			prod := al * uint16(val)
			c.SetReg16(RegEAX, prod)
			c.SetFlag(FlagCF|FlagOF, (prod&0xFF00) != 0)
		case 5: // IMUL r/m8
			al := int16(int8(c.GetReg8(RegEAX)))
			prod := al * int16(int8(val))
			c.SetReg16(RegEAX, uint16(prod))
			c.SetFlag(FlagCF|FlagOF, prod != int16(int8(prod)))
		case 6: // DIV r/m8
			ax := c.GetReg16(RegEAX)
			q := ax / uint16(val)
			r := ax % uint16(val)
			c.SetReg8(RegEAX, uint8(q))
			c.SetReg8(4, uint8(r)) // AH
		case 7: // IDIV r/m8
			ax := int16(c.GetReg16(RegEAX))
			d := int16(int8(val))
			q := ax / d
			r := ax % d
			c.SetReg8(RegEAX, uint8(int8(q)))
			c.SetReg8(4, uint8(int8(r))) // AH
		}
		return

	case 0xF7: // Grp3 r/m32(16)
		modrm := c.Fetch8()
		subop := int((modrm >> 3) & 7)
		if opSize16 {
			val, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
			switch subop {
			case 0, 1: // TEST r/m16, imm16
				imm := c.Fetch16()
				c.UpdateFlagsLogic16(val & imm)
			case 2: // NOT r/m16
				c.commitRM16(modrm, addr, isReg, ^val)
			case 3: // NEG r/m16
				res := -val
				c.UpdateFlagsSub16(0, val, res)
				c.SetFlag(FlagCF, val != 0)
				c.commitRM16(modrm, addr, isReg, res)
			case 4: // MUL r/m16
				prod := uint32(c.GetReg16(RegEAX)) * uint32(val)
				c.SetReg16(RegEAX, uint16(prod))
				c.SetReg16(RegEDX, uint16(prod>>16))
				c.SetFlag(FlagCF|FlagOF, (prod>>16) != 0)
			case 5: // IMUL r/m16
				prod := int32(int16(c.GetReg16(RegEAX))) * int32(int16(val))
				c.SetReg16(RegEAX, uint16(prod))
				c.SetReg16(RegEDX, uint16(uint32(prod)>>16))
				c.SetFlag(FlagCF|FlagOF, prod != int32(int16(prod)))
			case 6: // DIV r/m16
				num := (uint32(c.GetReg16(RegEDX)) << 16) | uint32(c.GetReg16(RegEAX))
				q := num / uint32(val)
				r := num % uint32(val)
				c.SetReg16(RegEAX, uint16(q))
				c.SetReg16(RegEDX, uint16(r))
			case 7: // IDIV r/m16
				num := int32((uint32(c.GetReg16(RegEDX)) << 16) | uint32(c.GetReg16(RegEAX)))
				d := int32(int16(val))
				q := num / d
				r := num % d
				c.SetReg16(RegEAX, uint16(int16(q)))
				c.SetReg16(RegEDX, uint16(int16(r)))
			}
		} else {
			val, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
			switch subop {
			case 0, 1: // TEST r/m32, imm32
				imm := c.Fetch32()
				c.UpdateFlagsLogic32(val & imm)
			case 2: // NOT r/m32
				c.commitRM32(modrm, addr, isReg, ^val)
			case 3: // NEG r/m32
				res := -val
				c.UpdateFlagsSub32(0, val, res)
				c.SetFlag(FlagCF, val != 0)
				c.commitRM32(modrm, addr, isReg, res)
			case 4: // MUL r/m32
				hi, lo := bits.Mul32(c.Regs[RegEAX], val)
				c.Regs[RegEAX] = lo
				c.Regs[RegEDX] = hi
				c.SetFlag(FlagCF|FlagOF, hi != 0)
			case 5: // IMUL r/m32
				prod := int64(int32(c.Regs[RegEAX])) * int64(int32(val))
				c.Regs[RegEAX] = uint32(prod)
				c.Regs[RegEDX] = uint32(uint64(prod) >> 32)
				c.SetFlag(FlagCF|FlagOF, prod != int64(int32(prod)))
			case 6: // DIV r/m32
				if val == 0 {
					fmt.Printf("[CPU-WARN] DIV32 by zero at EIP=0x%08x\n", c.EIP)
					c.Regs[RegEAX] = 0
					c.Regs[RegEDX] = 0
				} else {
					q, r := bits.Div32(c.Regs[RegEDX], c.Regs[RegEAX], val)
					c.Regs[RegEAX] = q
					c.Regs[RegEDX] = r
				}
			case 7: // IDIV r/m32
				d := int64(int32(val))
				if d == 0 {
					fmt.Printf("[CPU-WARN] IDIV32 by zero at EIP=0x%08x\n", c.EIP)
					c.Regs[RegEAX] = 0
					c.Regs[RegEDX] = 0
				} else {
					num := int64((uint64(c.Regs[RegEDX]) << 32) | uint64(c.Regs[RegEAX]))
					q := num / d
					r := num % d
					c.Regs[RegEAX] = uint32(int32(q))
					c.Regs[RegEDX] = uint32(int32(r))
				}
			}
		}
		return

	case 0xF8: // CLC
		c.SetFlag(FlagCF, false)
		return

	case 0xF9: // STC
		c.SetFlag(FlagCF, true)
		return

	case 0xFC: // CLD
		c.SetFlag(FlagDF, false)
		return

	case 0xFD: // STD
		c.SetFlag(FlagDF, true)
		return

	case 0xFE: // Grp4 INC/DEC r/m8
		modrm := c.Fetch8()
		subop := (modrm >> 3) & 7
		val, addr, isReg := c.readRM8(modrm, segOverride, hasSeg)
		cf := c.GetFlag(FlagCF)
		if subop == 0 {
			res := val + 1
			c.UpdateFlagsAdd8(val, 1, res)
			c.writeRM8(modrm, addr, isReg, res)
		} else if subop == 1 {
			res := val - 1
			c.UpdateFlagsSub8(val, 1, res)
			c.writeRM8(modrm, addr, isReg, res)
		}
		c.SetFlag(FlagCF, cf)
		return

	case 0xFF: // Grp5 INC/DEC/CALL/JMP/PUSH r/m32(16)
		modrm := c.Fetch8()
		subop := (modrm >> 3) & 7
		switch subop {
		case 0: // INC r/m32(16)
			cf := c.GetFlag(FlagCF)
			if opSize16 {
				v, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
				res := v + 1
				c.UpdateFlagsAdd16(v, 1, res)
				c.commitRM16(modrm, addr, isReg, res)
			} else {
				v, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
				res := v + 1
				c.UpdateFlagsAdd32(v, 1, res)
				c.commitRM32(modrm, addr, isReg, res)
			}
			c.SetFlag(FlagCF, cf)
		case 1: // DEC r/m32(16)
			cf := c.GetFlag(FlagCF)
			if opSize16 {
				v, addr, isReg := c.readWriteRM16(modrm, segOverride, hasSeg)
				res := v - 1
				c.UpdateFlagsSub16(v, 1, res)
				c.commitRM16(modrm, addr, isReg, res)
			} else {
				v, addr, isReg := c.readWriteRM32(modrm, segOverride, hasSeg)
				res := v - 1
				c.UpdateFlagsSub32(v, 1, res)
				c.commitRM32(modrm, addr, isReg, res)
			}
			c.SetFlag(FlagCF, cf)
		case 2: // CALL r/m32
			target := c.readRM32(modrm, segOverride, hasSeg)
			c.Push32(c.EIP)
			if c.TraceCalls {
				c.CallStack = append(c.CallStack, CallFrame{
					CallerEIP: startEIP,
					TargetEIP: target,
					ESP:       c.Regs[RegESP],
				})
				c.VisitedFuncs[target]++
			}
			c.EIP = target
		case 4: // JMP r/m32
			target := c.readRM32(modrm, segOverride, hasSeg)
			c.EIP = target
		case 6: // PUSH r/m32(16)
			if opSize16 {
				val := c.readRM16(modrm, segOverride, hasSeg)
				c.Push16(val)
			} else {
				val := c.readRM32(modrm, segOverride, hasSeg)
				c.Push32(val)
			}
		}
		return
	}

	c.Halted = true
	c.HaltReason = fmt.Sprintf("unimplemented opcode 0x%02x at EIP=0x%08x", op, startEIP)
}

func (c *CPU) execStringOp(op uint8, opSize16 bool, repF3 bool, repF2 bool) {
	step := int32(1)
	if (op & 1) != 0 {
		if opSize16 {
			step = 2
		} else {
			step = 4
		}
	}
	if c.GetFlag(FlagDF) {
		step = -step
	}

	doOne := func() {
		switch op {
		case 0xA4: // MOVSB
			c.Write8(c.Regs[RegEDI], c.Read8(c.Regs[RegESI]))
			c.Regs[RegESI] = uint32(int32(c.Regs[RegESI]) + step)
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		case 0xA5: // MOVSD / MOVSW
			if opSize16 {
				c.Write16(c.Regs[RegEDI], c.Read16(c.Regs[RegESI]))
			} else {
				c.Write32(c.Regs[RegEDI], c.Read32(c.Regs[RegESI]))
			}
			c.Regs[RegESI] = uint32(int32(c.Regs[RegESI]) + step)
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		case 0xA6: // CMPSB
			a := c.Read8(c.Regs[RegESI])
			b := c.Read8(c.Regs[RegEDI])
			c.UpdateFlagsSub8(a, b, a-b)
			c.Regs[RegESI] = uint32(int32(c.Regs[RegESI]) + step)
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		case 0xA7: // CMPSD / CMPSW
			if opSize16 {
				a := c.Read16(c.Regs[RegESI])
				b := c.Read16(c.Regs[RegEDI])
				c.UpdateFlagsSub16(a, b, a-b)
			} else {
				a := c.Read32(c.Regs[RegESI])
				b := c.Read32(c.Regs[RegEDI])
				c.UpdateFlagsSub32(a, b, a-b)
			}
			c.Regs[RegESI] = uint32(int32(c.Regs[RegESI]) + step)
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		case 0xAA: // STOSB
			c.Write8(c.Regs[RegEDI], c.GetReg8(RegEAX))
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		case 0xAB: // STOSD / STOSW
			if opSize16 {
				c.Write16(c.Regs[RegEDI], c.GetReg16(RegEAX))
			} else {
				c.Write32(c.Regs[RegEDI], c.Regs[RegEAX])
			}
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		case 0xAC: // LODSB
			c.SetReg8(RegEAX, c.Read8(c.Regs[RegESI]))
			c.Regs[RegESI] = uint32(int32(c.Regs[RegESI]) + step)
		case 0xAD: // LODSD / LODSW
			if opSize16 {
				c.SetReg16(RegEAX, c.Read16(c.Regs[RegESI]))
			} else {
				c.Regs[RegEAX] = c.Read32(c.Regs[RegESI])
			}
			c.Regs[RegESI] = uint32(int32(c.Regs[RegESI]) + step)
		case 0xAE: // SCASB
			a := c.GetReg8(RegEAX)
			b := c.Read8(c.Regs[RegEDI])
			c.UpdateFlagsSub8(a, b, a-b)
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		case 0xAF: // SCASD / SCASW
			if opSize16 {
				a := c.GetReg16(RegEAX)
				b := c.Read16(c.Regs[RegEDI])
				c.UpdateFlagsSub16(a, b, a-b)
			} else {
				a := c.Regs[RegEAX]
				b := c.Read32(c.Regs[RegEDI])
				c.UpdateFlagsSub32(a, b, a-b)
			}
			c.Regs[RegEDI] = uint32(int32(c.Regs[RegEDI]) + step)
		}
	}

	if !repF3 && !repF2 {
		doOne()
		return
	}

	isScanOrCmp := op == 0xA6 || op == 0xA7 || op == 0xAE || op == 0xAF
	for c.Regs[RegECX] != 0 {
		doOne()
		c.Regs[RegECX]--
		if isScanOrCmp {
			if repF3 && !c.GetFlag(FlagZF) {
				break
			}
			if repF2 && c.GetFlag(FlagZF) {
				break
			}
		}
	}
}

func (c *CPU) RunUntil(stopEIP uint32) error {
	for !c.Halted && c.EIP != stopEIP {
		if c.MaxSteps > 0 && c.Steps >= c.MaxSteps {
			c.Halted = true
			c.HaltReason = fmt.Sprintf("reached MaxSteps (%d) at EIP=0x%08x", c.MaxSteps, c.EIP)
			break
		}
		if c.Preempt != nil && c.NestDepth == 0 && c.Steps >= c.nextPreempt {
			c.nextPreempt = c.Steps + c.PreemptInterval
			c.Preempt(c)
			if c.EIP == stopEIP {
				break
			}
		}
		if c.ProfEvery > 0 && c.Steps%c.ProfEvery == 0 && c.Steps >= c.ProfFrom {
			c.sampleProfile()
		}
		c.Step()
	}
	if c.Halted {
		return fmt.Errorf("%s", c.HaltReason)
	}
	return nil
}

func (c *CPU) CallGuest(fnAddr uint32, args ...uint32) (uint32, error) {
	savedESP := c.Regs[RegESP]
	savedEIP := c.EIP

	// Align stack to 16 bytes for arguments
	argBytes := uint32(len(args) * 4)
	c.Regs[RegESP] = (c.Regs[RegESP] - argBytes) & ^uint32(15)
	for i, a := range args {
		c.Write32(c.Regs[RegESP]+uint32(i*4), a)
	}
	c.Push32(SentinelReturnEIP)
	c.EIP = fnAddr

	c.NestDepth++
	err := c.RunUntil(SentinelReturnEIP)
	c.NestDepth--
	c.Regs[RegESP] = savedESP
	c.EIP = savedEIP
	return c.Regs[RegEAX], err
}

func (c *CPU) CallGuestNested(fnAddr uint32, args ...uint32) (uint32, error) {
	savedRegs := c.Regs
	savedFlags := c.Flags
	savedXMM := c.XMM
	savedEIP := c.EIP
	stopEIP := uint32(0x1FFFF000 - (c.NestDepth+2)*4)

	argBytes := uint32(len(args) * 4)
	c.Regs[RegESP] = (c.Regs[RegESP] - 64 - argBytes) & ^uint32(15)
	for i, a := range args {
		c.Write32(c.Regs[RegESP]+uint32(i*4), a)
	}
	c.Push32(stopEIP)
	c.EIP = fnAddr

	c.NestDepth++
	err := c.RunUntil(stopEIP)
	c.NestDepth--
	if err != nil {
		return 0, err
	}
	retVal := c.Regs[RegEAX]

	c.Regs = savedRegs
	c.Flags = savedFlags
	c.XMM = savedXMM
	c.EIP = savedEIP
	return retVal, nil
}


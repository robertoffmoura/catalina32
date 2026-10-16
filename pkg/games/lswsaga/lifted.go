package lswsaga

import (
	"encoding/binary"
	"math"
	"os"

	"catalina32/pkg/cpu"
)

// registerLifted registers statically lifted 64-bit Go translations of hot NuEngine inner-loop
// functions in this binary (LZHUF archive decompression, string helpers, SSE matrix math, and
// buffer copy/search routines).
func registerLifted(c *cpu.CPU) {
	c.RegisterIntercept(0x0027ce00, "lifted::NuLZHUF_DecodeC_0027ce00", liftedNuLZHUFDecodeC)
	c.RegisterIntercept(0x0027d9d0, "lifted::NuLZHUF_DecodeP_0027d9d0", liftedNuLZHUFDecodeP)
	c.RegisterIntercept(0x00270500, "lifted::NuStrLen_00270500", liftedNuStrLen)
	c.RegisterIntercept(0x002738a0, "lifted::NuStrICmp_002738a0", liftedNuStrICmp)
	c.RegisterIntercept(0x0026ee60, "lifted::NuStrNICmp_0026ee60", liftedNuStrNICmp)
	c.RegisterIntercept(0x00275250, "lifted::NuFile_ReadU16_00275250", liftedNuFileReadU16)
	c.RegisterIntercept(0x0020f130, "lifted::NuStrSliceEq_0020f130", liftedNuStrSliceEq)
	if os.Getenv("C32_REAL_HLSL") != "1" { // C32_REAL_HLSL=1 runs the engine's own HLSL generator
		c.RegisterIntercept(0x002aee50, "lifted::NuShaderGenerateHLSL_002aee50", liftedNuShaderGenerateHLSL)
	}
	c.RegisterIntercept(0x002767c6, "lifted::NuTxtDispatchLoop_002767c6", liftedNuTxtDispatchLoop)
	c.RegisterIntercept(0x00275c10, "lifted::NuTxtReadToken_00275c10", liftedNuTxtReadToken)
	c.RegisterIntercept(0x00275510, "lifted::NuTxtReadLine_00275510", liftedNuTxtReadLine)
	c.RegisterIntercept(0x001fb350, "lifted::FindCharIdByName_001fb350", liftedFindCharIdByName)
	c.RegisterIntercept(0x002a52e0, "lifted::SoundSearchLoop_002a52e0", liftedSoundSearchLoop)
	c.RegisterIntercept(0x00277e70, "lifted::NuStrNCmp_00277e70", liftedNuStrNCmp)
	c.RegisterIntercept(0x0026eca0, "lifted::NuStrCpy_0026eca0", liftedNuStrCpy)
	c.RegisterIntercept(0x001fd6f0, "lifted::NuGetSceneByIdx_001fd6f0", liftedNuGetSceneByIdx)
	c.RegisterIntercept(0x0002cb10, "lifted::NuFindSpecialObject_0002cb10", liftedNuFindSpecialObject)
	c.RegisterIntercept(0x001052a0, "lifted::NuMtxMul_001052a0", liftedNuMtxMul)
	c.RegisterIntercept(0x002e2dc0, "lifted::NuBufReadF32_002e2dc0", liftedNuBufReadF32)
	c.RegisterIntercept(0x0006e050, "lifted::NuInitMtlItem_0006e050", liftedNuInitMtlItem)
	c.RegisterIntercept(0x0006e090, "lifted::NuNoOp_0006e090", liftedNuNoOp)
	c.RegisterIntercept(0x0006e0a0, "lifted::NuNoOp_0006e0a0", liftedNuNoOp)
	c.RegisterIntercept(0x0002dd60, "lifted::NuHasRenderData_0002dd60", liftedNuHasRenderData)
	c.RegisterIntercept(0x00102c40, "lifted::NuVecMtxTransform_00102c40", liftedNuVecMtxTransform)
	c.RegisterIntercept(0x00102dd0, "lifted::NuVecMtxRotate_00102dd0", liftedNuVecMtxRotate)
	c.RegisterIntercept(0x0016ff70, "lifted::NuSqrt_0016ff70", liftedNuSqrt)
}

func liftedNuMtxMul(c *cpu.CPU) {
	pOut := c.Arg(0)
	pM1 := c.Arg(1)
	pM2 := c.Arg(2)
	var m1, m2, out [16]float32
	for i := 0; i < 16; i++ {
		m1[i] = c.ReadF32(pM1 + uint32(i*4))
		m2[i] = c.ReadF32(pM2 + uint32(i*4))
	}
	for r := 0; r < 3; r++ {
		for col := 0; col < 3; col++ {
			out[r*4+col] = m1[r*4+0]*m2[0*4+col] +
				m1[r*4+1]*m2[1*4+col] +
				m1[r*4+2]*m2[2*4+col]
		}
		out[r*4+3] = 0
	}
	for col := 0; col < 3; col++ {
		out[12+col] = m1[12+0]*m2[0*4+col] +
			m1[12+1]*m2[1*4+col] +
			m1[12+2]*m2[2*4+col] +
			m2[12+col]
	}
	out[15] = 1.0
	for i := 0; i < 16; i++ {
		c.WriteF32(pOut+uint32(i*4), out[i])
	}
	c.Ret()
}

func liftedNuBufReadF32(c *cpu.CPU) {
	ebx := c.Read32(0x02bb2114)
	if ebx+4 < 0x1000 {
		c.Write32(0x02bb2114, ebx+4)
		var f float32
		if c.Read32(0x02bb2118) != 0 {
			u := binary.BigEndian.Uint32(c.Mem[0x02bb1108+ebx : 0x02bb1108+ebx+4])
			f = math.Float32frombits(u)
		} else {
			f = c.ReadF32(0x02bb1108 + ebx)
		}
		c.FPUPush(float64(f))
		c.Ret()
		return
	}
	c.Push32(c.Regs[cpu.RegEBP])
	c.EIP = 0x002e2dc1
}

func liftedNuStrCpy(c *cpu.CPU) {
	dst := c.Arg(0)
	src := c.Arg(1)
	var n uint32
	if src != 0 {
		for {
			b := c.Mem[src+n]
			if b == 0 {
				break
			}
			c.Mem[dst+n] = b
			n++
		}
	}
	c.Mem[dst+n] = 0
	c.Regs[cpu.RegEAX] = n
	c.Ret()
}

func liftedNuGetSceneByIdx(c *cpu.CPU) {
	idx := int32(c.Arg(0))
	if idx >= 0 && idx < int32(c.Read32(0x02b61634)) {
		arr := c.Read32(0x02c3fa78)
		c.Regs[cpu.RegEAX] = c.Read32(arr + uint32(idx)*4)
	} else {
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func liftedNuFindSpecialObject(c *cpu.CPU) {
	edi := c.Arg(0)
	outPtr := c.Arg(1)
	namePtr := c.Arg(2)
	if edi == 0 || namePtr == 0 {
		if outPtr != 0 {
			c.Write32(outPtr, 0)
			c.Write32(outPtr+4, 0)
			c.Write32(outPtr+8, 0)
		}
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	sub := c.Read32(edi + 0x110)
	if sub != 0 {
		count := int32(c.Read32(sub + 0x6c))
		esi := c.Read32(sub + 0x70)
		for i := int32(0); i < count; i++ {
			s := c.Read32(esi + 0xb4)
			if s != 0 && guestStrICmpEq(c.Mem, namePtr, s) {
				c.Write32(outPtr, edi)
				c.Write32(outPtr+4, 0)
				c.Write32(outPtr+8, esi)
				c.Regs[cpu.RegEAX] = 1
				c.Ret()
				return
			}
			esi += 0xd0
		}
	} else {
		count := int32(c.Read32(edi + 0x24))
		esi := c.Read32(edi + 0x28)
		for i := int32(0); i < count; i++ {
			s := c.Read32(esi + 0x44)
			if s != 0 && guestStrICmpEq(c.Mem, namePtr, s) {
				c.Write32(outPtr, edi)
				c.Write32(outPtr+4, esi)
				c.Write32(outPtr+8, 0)
				c.Regs[cpu.RegEAX] = 1
				c.Ret()
				return
			}
			esi += 76
		}
	}
	c.Write32(outPtr, 0)
	c.Write32(outPtr+4, 0)
	c.Write32(outPtr+8, 0)
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func liftedFindCharIdByName(c *cpu.CPU) {
	esi := c.Arg(0)
	eax := c.Read32(0x02b615e0)
	if eax == 0 {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		c.Ret()
		return
	}
	ecx := c.Read32(0x02c3fa64)
	count := int32(c.Read32(eax + 8))
	if ecx != 0 && count > 0 && esi != 0 {
		for i := int32(0); i < count; i++ {
			entryName := c.Read32(ecx + uint32(i*8))
			if entryName != 0 && guestStrICmpEq(c.Mem, entryName, esi) {
				c.Regs[cpu.RegEAX] = uint32(i)
				c.Ret()
				return
			}
		}
	}
	edi := c.Read32(0x02c3fa68)
	if edi != 0 && esi != 0 {
		for {
			aliasName := c.Read32(edi)
			if aliasName == 0 {
				break
			}
			if guestStrICmpEq(c.Mem, aliasName, esi) {
				c.Regs[cpu.RegEAX] = c.Read32(edi + 4)
				c.Ret()
				return
			}
			edi += 8
		}
	}
	c.Regs[cpu.RegEAX] = 0xFFFFFFFF
	c.Ret()
}

func liftedSoundSearchLoop(c *cpu.CPU) {
	esi := c.Regs[cpu.RegESI]
	count := int32(c.Read32(esi + 0x2a3a01f))
	base := c.Read32(esi + 0x28f064b)
	parser := c.Read32(c.Regs[cpu.RegEBP] + 8)
	target := c.Read32(parser + 0x510)
	var edi uint32
	for ebx := int32(0); ebx < count; ebx++ {
		namePtr := c.Read32(base + edi)
		if namePtr != 0 && target != 0 && guestStrICmpEq(c.Mem, namePtr, target) {
			c.Regs[cpu.RegEBX] = uint32(ebx)
			c.Regs[cpu.RegEDI] = edi
			c.Regs[cpu.RegEAX] = 0
			c.EIP = 0x002a5406
			return
		}
		edi += 28
	}
	c.Regs[cpu.RegEBX] = uint32(count)
	c.Regs[cpu.RegEDI] = edi
	c.EIP = 0x002a5460
}

func liftedNuStrNCmp(c *cpu.CPU) {
	s1 := c.Arg(0)
	if s1 == 0 {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		c.Ret()
		return
	}
	s2 := c.Arg(1)
	if s2 == 0 {
		c.Regs[cpu.RegEAX] = 1
		c.Ret()
		return
	}
	eax := c.Arg(2)
	if eax == 0 {
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	if eax == 0xFFFFFFFF {
		eax = 0
		for c.Mem[s1+eax] != 0 {
			eax++
		}
	} else if eax == 0xFFFFFFFE {
		eax = 0
		for c.Mem[s2+eax] != 0 {
			eax++
		}
	}
	edi := uint32(1) - eax
	for {
		cl := int8(c.Mem[s1])
		bl := int8(c.Mem[s2])
		if cl > bl {
			c.Regs[cpu.RegEAX] = 1
			c.Ret()
			return
		}
		if cl < bl {
			c.Regs[cpu.RegEAX] = 0xFFFFFFFF
			c.Ret()
			return
		}
		if edi == 0 || cl == 0 {
			c.Regs[cpu.RegEAX] = 0
			c.Ret()
			return
		}
		s1++
		s2++
		edi++
		if bl == 0 {
			c.Regs[cpu.RegEAX] = 0
			c.Ret()
			return
		}
	}
}

func liftedNuTxtReadLine(c *cpu.CPU) {
	edi := c.Arg(0)
	if c.Mem[edi+0x578] != 0 {
		c.EIP = 0x00275370
		return
	}
	pos := int32(c.Read32(edi + 0x524))
	end := int32(c.Read32(edi + 0x52c))
	if pos > end {
		c.Push32(c.Regs[cpu.RegEBP])
		c.EIP = 0x00275511
		return
	}
	buf := c.Read32(edi)
	base := c.Read32(edi + 0x528)
	lineBuf := c.Read32(edi + 0x50c)
	lineMask := c.Read32(edi+0x514) - 1
	lineNo := c.Read32(edi+0x51c) + 1

	var temp [4096]byte
	var ebx uint32
	var hasNonSpace uint32
	var inQuote uint32

	for {
		if pos > end {
			c.Push32(c.Regs[cpu.RegEBP])
			c.EIP = 0x00275511
			return
		}
		al := c.Mem[buf+uint32(pos)-base]
		pos++
		esi := int32(int8(al))
		edx := uint32(1)
		if esi <= 31 {
			if esi <= 9 {
				if esi == 0 {
					break
				}
				if esi == 9 {
					edx = hasNonSpace
				}
				temp[ebx&lineMask] = al
				ebx++
				hasNonSpace = edx
				continue
			}
			if esi == 10 {
				if ebx != 0 && hasNonSpace != 0 {
					break
				}
				lineNo++
				hasNonSpace = 0
				continue
			}
			if esi == 13 {
				if pos > end {
					c.Push32(c.Regs[cpu.RegEBP])
					c.EIP = 0x00275511
					return
				}
				pos++
				if ebx != 0 && hasNonSpace != 0 {
					break
				}
				lineNo++
				hasNonSpace = 0
				continue
			}
			temp[ebx&lineMask] = al
			ebx++
			hasNonSpace = 1
			continue
		}
		if esi == 32 {
			edx = hasNonSpace
			temp[ebx&lineMask] = al
			ebx++
			hasNonSpace = edx
			continue
		}
		if esi == 34 {
			temp[ebx&lineMask] = al
			inQuote = 1 - inQuote
			ebx++
			hasNonSpace = 1
			continue
		}
		if esi == 59 && inQuote == 0 {
			for {
				if pos > end {
					c.Push32(c.Regs[cpu.RegEBP])
					c.EIP = 0x00275511
					return
				}
				dl := int8(c.Mem[buf+uint32(pos)-base])
				pos++
				if dl == 0 || dl == 10 {
					break
				}
				if dl == 13 {
					if pos > end {
						c.Push32(c.Regs[cpu.RegEBP])
						c.EIP = 0x00275511
						return
					}
					pos++
					break
				}
			}
			if ebx != 0 && hasNonSpace != 0 {
				break
			}
			lineNo++
			ebx = 0
			hasNonSpace = 0
			continue
		}
		temp[ebx&lineMask] = al
		ebx++
		hasNonSpace = 1
	}

	c.Write32(edi+0x520, 0)
	c.Write32(edi+0x51c, lineNo)
	c.Write32(edi+0x524, uint32(pos))
	copy(c.Mem[lineBuf:lineBuf+ebx], temp[:ebx])
	c.Mem[lineBuf+ebx] = 0
	c.Regs[cpu.RegEAX] = ebx
	c.Ret()
}

func cstrContains(mem []byte, ptr uint32, ch uint8) bool {
	if ptr == 0 {
		return false
	}
	for {
		b := mem[ptr]
		if b == 0 {
			return false
		}
		if b == ch {
			return true
		}
		ptr++
	}
}

func liftedNuTxtReadToken(c *cpu.CPU) {
	ecx := c.Arg(0)
	if c.Mem[ecx+0x578] != 0 {
		c.EIP = 0x00275990
		return
	}
	edi := c.Read32(ecx + 0x520)
	c.Write32(0x02cc70ec, edi)
	var outLen uint32
	var inQuote uint32
	var hadQuote uint32

	for {
		lineBuf := c.Read32(ecx + 0x50c)
		lineMask := c.Read32(ecx+0x514) - 1
		if c.Mem[lineBuf+(edi&lineMask)] == 0 {
			break
		}
		bl := c.Mem[lineBuf+edi]
		if inQuote == 0 {
			if cstrContains(c.Mem, c.Read32(ecx+0x580), bl) {
				if outLen == 0 {
					tokBuf := c.Read32(ecx + 0x510)
					c.Mem[tokBuf] = bl
					c.Write32(ecx+0x520, edi+1)
					outLen = 1
				}
				tokBuf := c.Read32(ecx + 0x510)
				tokMask := c.Read32(ecx+0x518) - 1
				c.Mem[tokBuf+(outLen&tokMask)] = 0
				c.Regs[cpu.RegEAX] = outLen
				c.Ret()
				return
			}
			if cstrContains(c.Mem, c.Read32(ecx+0x57c), bl) {
				bl = 0x20
			}
		}
		sbl := int8(bl)
		if (sbl == 9 || sbl == 32 || sbl == 44) && inQuote == 0 {
			if outLen != 0 {
				tokBuf := c.Read32(ecx + 0x510)
				tokMask := c.Read32(ecx+0x518) - 1
				c.Mem[tokBuf+(outLen&tokMask)] = 0
				c.Regs[cpu.RegEAX] = outLen
				c.Ret()
				return
			}
			inQuote = 0
			outLen = 0
		} else if bl == 34 {
			hadQuote = 1
			inQuote = 1 - inQuote
		} else {
			tokBuf := c.Read32(ecx + 0x510)
			tokMask := c.Read32(ecx+0x518) - 1
			c.Mem[tokBuf+(outLen&tokMask)] = bl
			outLen++
			edi = c.Read32(ecx + 0x520)
		}
		edi++
		c.Write32(ecx+0x520, edi)
		if (inQuote|outLen) == 0 && hadQuote != 0 {
			break
		}
	}
	tokBuf := c.Read32(ecx + 0x510)
	tokMask := c.Read32(ecx+0x518) - 1
	c.Mem[tokBuf+(outLen&tokMask)] = 0
	c.Regs[cpu.RegEAX] = outLen
	c.Ret()
}

func guestStrICmpEq(mem []byte, a, b uint32) bool {
	for {
		ca := mem[a]
		if ca-'a' < 26 {
			ca -= 0x20
		}
		cb := mem[b]
		if cb-'a' < 26 {
			cb -= 0x20
		}
		if ca != cb {
			return false
		}
		if ca == 0 {
			return true
		}
		a++
		b++
	}
}

func liftedNuTxtDispatchLoop(c *cpu.CPU) {
	ebp := c.Regs[cpu.RegEBP]
	ecx := c.Read32(ebp + 8)
	tokenAddr := ebp - 0x50
	table1 := c.Read32(ebp - 0x60)
	if table1 != 0 {
		for idx := uint32(0); ; idx++ {
			namePtr := c.Read32(table1 + idx*8)
			if namePtr == 0 {
				break
			}
			if guestStrICmpEq(c.Mem, namePtr, tokenAddr) {
				c.Write32(ebp-0x60, table1)
				c.Write32(ebp-0x5c, idx)
				c.EIP = 0x002768c4
				return
			}
		}
	}
	eax := c.Read32(ebp - 0x64)
	table2 := c.Read32(ecx + eax*4 + 0x550)
	if table2 != 0 {
		for idx := uint32(0); ; idx++ {
			namePtr := c.Read32(table2 + idx*8)
			if namePtr == 0 {
				break
			}
			if guestStrICmpEq(c.Mem, namePtr, tokenAddr) {
				c.Write32(ebp-0x60, table2)
				c.Write32(ebp-0x5c, idx)
				c.EIP = 0x002768c4
				return
			}
		}
	}
	c.Regs[cpu.RegEDI] = c.Read32(ebp - 0x58)
	c.Regs[cpu.RegECX] = ecx
	c.EIP = 0x002768ae
}

func liftedNuStrNICmp(c *cpu.CPU) {
	s1 := c.Arg(0)
	if s1 == 0 {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		c.Ret()
		return
	}
	s2 := c.Arg(1)
	if s2 == 0 {
		c.Regs[cpu.RegEAX] = 1
		c.Ret()
		return
	}
	eax := c.Arg(2)
	if eax == 0 {
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	if eax == 0xFFFFFFFF {
		eax = 0
		for c.Mem[s1+eax] != 0 {
			eax++
		}
	} else if eax == 0xFFFFFFFE {
		eax = 0
		for c.Mem[s2+eax] != 0 {
			eax++
		}
	}
	esi := uint32(1) - eax
	for {
		bl := c.Mem[s1]
		if bl-'a' < 26 {
			bl -= 0x20
		}
		bh := c.Mem[s2]
		if bh-'a' < 26 {
			bh -= 0x20
		}
		if int8(bl) > int8(bh) {
			c.Regs[cpu.RegEAX] = 1
			c.Ret()
			return
		}
		if int8(bl) < int8(bh) {
			c.Regs[cpu.RegEAX] = 0xFFFFFFFF
			c.Ret()
			return
		}
		if esi == 0 || bl == 0 {
			c.Regs[cpu.RegEAX] = 0
			c.Ret()
			return
		}
		s1++
		s2++
		esi++
		if bh == 0 {
			c.Regs[cpu.RegEAX] = 0
			c.Ret()
			return
		}
	}
}

func liftedNuShaderGenerateHLSL(c *cpu.CPU) {
	outVS := c.Arg(2)
	outPS := c.Arg(3)
	const stubHLSL = "void main(){}\x00"
	for _, vec := range [2]uint32{outVS, outPS} {
		if vec == 0 {
			continue
		}
		buf := c.Read32(vec)
		capSz := c.Read32(vec + 8)
		if buf == 0 || capSz < 32 {
			buf = c.Malloc(64)
			c.Write32(vec, buf)
			c.Write32(vec+8, 64)
		}
		copy(c.Mem[buf:], stubHLSL)
		c.Write32(vec+4, uint32(len(stubHLSL)))
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func liftedNuStrLen(c *cpu.CPU) {
	s := c.Arg(0)
	var n uint32
	for c.Mem[s+n] != 0 {
		n++
	}
	c.Regs[cpu.RegEAX] = n
	c.Ret()
}

func liftedNuStrICmp(c *cpu.CPU) {
	s1 := c.Arg(0)
	if s1 == 0 {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		c.Ret()
		return
	}
	s2 := c.Arg(1)
	if s2 == 0 {
		c.Regs[cpu.RegEAX] = 1
		c.Ret()
		return
	}
	for {
		bl := c.Mem[s1]
		if bl-'a' < 26 {
			bl -= 0x20
		}
		bh := c.Mem[s2]
		if bh-'a' < 26 {
			bh -= 0x20
		}
		ibs := int8(bl)
		ibh := int8(bh)
		if ibs > ibh {
			c.Regs[cpu.RegEAX] = 1
			c.Ret()
			return
		}
		if ibs < ibh {
			c.Regs[cpu.RegEAX] = 0xFFFFFFFF
			c.Ret()
			return
		}
		if bl == 0 {
			c.Regs[cpu.RegEAX] = 0
			c.Ret()
			return
		}
		s1++
		s2++
		if bh == 0 {
			c.Regs[cpu.RegEAX] = 0
			c.Ret()
			return
		}
	}
}

func liftedNuFileReadU16(c *cpu.CPU) {
	esi := c.Arg(0)
	pos := int32(c.Read32(esi + 0x524))
	end := int32(c.Read32(esi + 0x52c))
	if pos < end {
		buf := c.Read32(esi)
		base := c.Read32(esi + 0x528)
		idx := uint32(pos) - base
		val := uint32(binary.LittleEndian.Uint16(c.Mem[buf+idx : buf+idx+2]))
		c.Write32(esi+0x524, uint32(pos+2))
		c.Regs[cpu.RegEAX] = val
		c.Ret()
		return
	}
	c.Push32(c.Regs[cpu.RegEBP])
	c.EIP = 0x00275251
}

func liftedNuStrSliceEq(c *cpu.CPU) {
	edx := c.Arg(0)
	eax := c.Arg(1)
	slen := int32(c.Read32(edx + 4))
	if slen > 0 {
		sptr := c.Read32(edx)
		for i := int32(0); i < slen; i++ {
			if c.Mem[eax+uint32(i)] != c.Mem[sptr+uint32(i)] {
				c.Regs[cpu.RegEAX] = 0
				c.Ret()
				return
			}
		}
		eax += uint32(slen)
	}
	if c.Mem[eax] == 0 {
		c.Regs[cpu.RegEAX] = 1
	} else {
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func lzhufFillBits(c *cpu.CPU, bitbuf uint32, nbits uint32) uint32 {
	var edi uint32
	if nbits < 32 {
		edi = bitbuf << nbits
	}
	c.Write32(0x02b940d4, edi)
	avail := c.Read32(0x02cccf90)
	for avail < nbits {
		nbits -= avail
		if int32(nbits) <= 31 {
			edi |= c.Read32(0x02cccf94) << nbits
			c.Write32(0x02b940d4, edi)
		}
		rem := c.Read32(0x02b940dc)
		var nextByte uint32
		if rem != 0 {
			c.Write32(0x02b940dc, rem-1)
			ptr := c.Read32(0x02b940cc)
			c.Write32(0x02b940cc, ptr+1)
			nextByte = uint32(c.Mem[ptr])
		}
		c.Write32(0x02cccf94, nextByte)
		c.Write32(0x02cccf90, 8)
		avail = 8
	}
	avail -= nbits
	c.Write32(0x02cccf90, avail)
	if avail <= 31 {
		edi |= c.Read32(0x02cccf94) >> avail
		c.Write32(0x02b940d4, edi)
	}
	return edi
}

// Statically lifted 64-bit translation of NuEngine's LZHUF literal/length decoder (0x0027ce00)
func liftedNuLZHUFDecodeC(c *cpu.CPU) {
	retAddr := c.Read32(c.Regs[cpu.RegESP])
	if retAddr == 0x002736cb {
		esi := c.Regs[cpu.RegESI]
		limit := c.Read32(c.Regs[cpu.RegEBP] - 0x18)
		for {
			blockRem := c.Read32(0x02ccefa0)
			if blockRem == 0 {
				c.Regs[cpu.RegESI] = esi
				c.Push32(c.Regs[cpu.RegEBP])
				c.EIP = 0x0027ce01
				return
			}
			c.Write32(0x02ccefa0, blockRem-1)
			bitbuf := c.Read32(0x02b940d4)
			sym := uint32(binary.LittleEndian.Uint16(c.Mem[0x02ccefa4+(bitbuf>>20)*2:]))
			if sym >= 0x1fe {
				mask := uint32(0x00080000)
				for sym > 0x1fd {
					if (bitbuf & mask) != 0 {
						sym = uint32(binary.LittleEndian.Uint16(c.Mem[0x02b948d6+sym*2:]))
					} else {
						sym = uint32(binary.LittleEndian.Uint16(c.Mem[0x02b940e0+sym*2:]))
					}
					mask >>= 1
				}
			}
			nbits := uint32(c.Mem[0x02cd0fa4+sym])
			lzhufFillBits(c, bitbuf, nbits)
			if sym > 0xff {
				c.Regs[cpu.RegESI] = esi
				c.Regs[cpu.RegEAX] = sym
				c.Ret()
				return
			}
			c.Mem[0x02cccf9d+esi] = uint8(sym)
			esi++
			if esi == limit {
				c.Regs[cpu.RegESI] = esi
				c.Regs[cpu.RegEAX] = sym
				c.Ret()
				c.EIP = 0x0027375a
				return
			}
		}
	}

	blockRem := c.Read32(0x02ccefa0)
	if blockRem == 0 {
		c.Push32(c.Regs[cpu.RegEBP])
		c.EIP = 0x0027ce01
		return
	}
	c.Write32(0x02ccefa0, blockRem-1)
	bitbuf := c.Read32(0x02b940d4)
	sym := uint32(binary.LittleEndian.Uint16(c.Mem[0x02ccefa4+(bitbuf>>20)*2:]))
	if sym >= 0x1fe {
		mask := uint32(0x00080000)
		for sym > 0x1fd {
			if (bitbuf & mask) != 0 {
				sym = uint32(binary.LittleEndian.Uint16(c.Mem[0x02b948d6+sym*2:]))
			} else {
				sym = uint32(binary.LittleEndian.Uint16(c.Mem[0x02b940e0+sym*2:]))
			}
			mask >>= 1
		}
	}
	nbits := uint32(c.Mem[0x02cd0fa4+sym])
	lzhufFillBits(c, bitbuf, nbits)
	c.Regs[cpu.RegEAX] = sym
	c.Ret()
}

// Statically lifted 64-bit translation of NuEngine's LZHUF distance decoder (0x0027d9d0)
func liftedNuLZHUFDecodeP(c *cpu.CPU) {
	bitbuf := c.Read32(0x02b940d4)
	pt := uint32(binary.LittleEndian.Uint16(c.Mem[0x02cd11a2+(bitbuf>>24)*2:]))
	if pt >= 0x0e {
		mask := uint32(0x00800000)
		for pt > 0x0d {
			if (bitbuf & mask) != 0 {
				pt = uint32(binary.LittleEndian.Uint16(c.Mem[0x02b948d6+pt*2:]))
			} else {
				pt = uint32(binary.LittleEndian.Uint16(c.Mem[0x02b940e0+pt*2:]))
			}
			mask >>= 1
		}
	}
	nbits := uint32(c.Mem[0x02cd13a2+pt])
	bitbuf = lzhufFillBits(c, bitbuf, nbits)
	var dist uint32
	if pt != 0 {
		eb := pt - 1
		base := uint32(1) << eb
		var extra uint32
		if eb != 0 {
			extra = bitbuf >> (33 - pt)
			lzhufFillBits(c, bitbuf, eb)
		}
		dist = base + extra
	}

	if c.Read32(c.Regs[cpu.RegESP]) == 0x00273700 {
		esi := c.Regs[cpu.RegESI]
		limit := c.Read32(c.Regs[cpu.RegEBP] - 0x10)
		ecx := (esi + 0x1fff - dist) & 0x1fff
		c.Write32(0x02cd23f8, ecx)
		eax := int32(c.Read32(0x02cd23f4))
		for eax > 0 {
			eax--
			bl := c.Mem[0x02cccf9d+ecx]
			c.Mem[0x02cccf9d+esi] = bl
			ecx = (ecx + 1) & 0x1fff
			c.Write32(0x02cd23f8, ecx)
			esi++
			if esi == limit {
				c.Write32(0x02cd23f4, uint32(eax))
				c.Regs[cpu.RegESI] = esi
				c.Regs[cpu.RegEAX] = uint32(eax)
				c.Ret()
				c.EIP = 0x0027375a
				return
			}
		}
		c.Write32(0x02cd23f4, 0xFFFFFFFF)
		c.Regs[cpu.RegESI] = esi
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		c.Ret()
		c.EIP = 0x002736c6
		return
	}

	c.Regs[cpu.RegEAX] = dist
	c.Ret()
}

func liftedNuInitMtlItem(c *cpu.CPU) {
	p := c.Arg(0)
	c.Write32(p+0x8, 0)
	c.Write32(p+0x14, 0xffffffff)
	c.Write32(p+0x20, 0xffffffff)
	c.Write16(p+0x24, 0xffff)
	c.Write16(p+0x2c, 0xffff)
	c.Write16(p+0x2e, 0xffff)
	c.Write16(p+0x1e, 0xffff)
	c.Ret()
}

func liftedNuNoOp(c *cpu.CPU) {
	c.Ret()
}

func liftedNuHasRenderData(c *cpu.CPU) {
	p := c.Arg(0)
	if p != 0 && (c.Read32(p+4) != 0 || c.Read32(p+8) != 0) {
		c.Regs[cpu.RegEAX] = 1
	} else {
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func liftedNuVecMtxTransform(c *cpu.CPU) {
	out := c.Arg(0)
	in := c.Arg(1)
	mtx := c.Arg(2)
	ix, iy, iz := c.ReadF32(in), c.ReadF32(in+4), c.ReadF32(in+8)
	ox := ix*c.ReadF32(mtx+0x00) + iy*c.ReadF32(mtx+0x10) + iz*c.ReadF32(mtx+0x20) + c.ReadF32(mtx+0x30)
	oy := ix*c.ReadF32(mtx+0x04) + iy*c.ReadF32(mtx+0x14) + iz*c.ReadF32(mtx+0x24) + c.ReadF32(mtx+0x34)
	oz := ix*c.ReadF32(mtx+0x08) + iy*c.ReadF32(mtx+0x18) + iz*c.ReadF32(mtx+0x28) + c.ReadF32(mtx+0x38)
	c.WriteF32(out, ox)
	c.WriteF32(out+4, oy)
	c.WriteF32(out+8, oz)
	c.Ret()
}

func liftedNuVecMtxRotate(c *cpu.CPU) {
	out := c.Arg(0)
	in := c.Arg(1)
	mtx := c.Arg(2)
	ix, iy, iz := c.ReadF32(in), c.ReadF32(in+4), c.ReadF32(in+8)
	ox := ix*c.ReadF32(mtx+0x00) + iy*c.ReadF32(mtx+0x10) + iz*c.ReadF32(mtx+0x20)
	oy := ix*c.ReadF32(mtx+0x04) + iy*c.ReadF32(mtx+0x14) + iz*c.ReadF32(mtx+0x24)
	oz := ix*c.ReadF32(mtx+0x08) + iy*c.ReadF32(mtx+0x18) + iz*c.ReadF32(mtx+0x28)
	c.WriteF32(out, ox)
	c.WriteF32(out+4, oy)
	c.WriteF32(out+8, oz)
	c.Ret()
}

func liftedNuSqrt(c *cpu.CPU) {
	x := c.ReadF32(c.Regs[cpu.RegESP] + 4)
	if x <= 0 {
		c.FPUPush(0)
	} else {
		c.FPUPush(math.Sqrt(float64(x)))
	}
	c.Ret()
}

package crt

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"strings"

	"catalina32/pkg/cpu"
	"catalina32/pkg/macho32"
	"catalina32/pkg/profile"
)

const (
	StackGuardGuestAddr = 0x03000100
	ErrnoGuestAddr      = 0x03000110
	NlTrampolineBase    = 0x0e000000
)

func RegisterCRT(c *CPUWrapper, img *macho32.Image, prof *profile.Profile) {
	// Set up ___stack_chk_guard -> 0x03000100 -> 0xDEADBEEF
	c.Write32(StackGuardGuestAddr, 0xDEADBEEF)
	for nlAddr, symName := range img.NlPtrByAddr {
		switch symName {
		case "___stack_chk_guard":
			c.Write32(nlAddr, StackGuardGuestAddr)
		case "_malloc":
			tramp := uint32(NlTrampolineBase + 0x00)
			c.Write32(nlAddr, tramp)
			c.RegisterIntercept(tramp, "_malloc", thunkMalloc)
		case "_free":
			tramp := uint32(NlTrampolineBase + 0x04)
			c.Write32(nlAddr, tramp)
			c.RegisterIntercept(tramp, "_free", thunkFree)
		case "_realloc":
			tramp := uint32(NlTrampolineBase + 0x08)
			c.Write32(nlAddr, tramp)
			c.RegisterIntercept(tramp, "_realloc", thunkRealloc)
		}
	}

	// Register all external __symbol_stub entries
	stubLo, stubHi := symbolStubRange(img)
	for stubAddr, symName := range img.StubsByAddr {
		// An i386 stub is `jmp *[lazy_ptr]` (FF 25 disp32). If the lazy pointer already holds an
		// address below the stub section, the symbol is a weak C++ template defined in this binary:
		// leave the stub alone so it jumps to the real code.
		if c.Read8(stubAddr) == 0xFF && c.Read8(stubAddr+1) == 0x25 {
			laVal := c.Read32(c.Read32(stubAddr + 2))
			if laVal != 0 && laVal < stubLo && stubLo < stubHi {
				continue
			}
		}
		if fn := lookupCRTThunk(symName); fn != nil {
			c.RegisterIntercept(stubAddr, symName, fn)
		} else {
			nameCopy := symName
			addrCopy := stubAddr
			c.RegisterIntercept(stubAddr, symName, func(vm *cpu.CPU) {
				vm.UnmappedCalls[addrCopy]++
				vm.ReportOnce("crt:"+nameCopy, "[CRT-STUB] %s @ 0x%08x is not implemented: returns 0", nameCopy, addrCopy)
				vm.Regs[cpu.RegEAX] = 0
				vm.Ret()
			})
		}
	}

	// Native fast-paths for hot statically linked libogg / libvorbis bitstream & Huffman functions
	prof.Bind(c, "ogg:oggpack_look", thunkOggpackLook)
	prof.Bind(c, "ogg:oggpack_adv", thunkOggpackAdv)
	prof.Bind(c, "ogg:oggpack_read", thunkOggpackRead)
	prof.Bind(c, "vorbis:decode_packed_entry_number", thunkVorbisDecodePackedEntryNumber)
	prof.Bind(c, "vorbis:vorbis_book_decodev_set", thunkVorbisBookDecodevSet)
	prof.Bind(c, "vorbis:vorbis_book_decodevv_add", thunkVorbisBookDecodevvAdd)
	if prof.Bind(c, "vorbis:mdct_backward", thunkMdctBackward) {
		mdct.addr, _ = prof.Addr("vorbis:mdct_backward")
	}
}

// symbolStubRange returns the address range of the S_SYMBOL_STUBS (__symbol_stub) section.
func symbolStubRange(img *macho32.Image) (lo, hi uint32) {
	for _, sec := range img.Sections {
		if sec.Flags&0xFF == 0x8 {
			return sec.Addr, sec.Addr + sec.Size
		}
	}
	return 0, 0
}

type CPUWrapper = cpu.CPU

func lookupCRTThunk(sym string) cpu.ThunkFunc {
	switch sym {
	case "_malloc", "__Znwm", "__Znam":
		return thunkMalloc
	case "_free", "__ZdlPv", "__ZdaPv":
		return thunkFree
	case "_calloc":
		return thunkCalloc
	case "_realloc":
		return thunkRealloc
	case "___bzero", "_bzero":
		return thunkBzero
	case "_memset":
		return thunkMemset
	case "_memset_pattern16":
		return thunkMemsetPattern16
	case "_memcpy", "_memmove":
		return thunkMemcpy
	case "_memcmp":
		return thunkMemcmp
	case "_memchr":
		return thunkMemchr
	case "_strlen":
		return thunkStrlen
	case "_strcpy":
		return thunkStrcpy
	case "_strncpy":
		return thunkStrncpy
	case "_strcat":
		return thunkStrcat
	case "_strncat":
		return thunkStrncat
	case "_strcmp":
		return thunkStrcmp
	case "_strncmp":
		return thunkStrncmp
	case "_strcasecmp":
		return thunkStrcasecmp
	case "_strncasecmp":
		return thunkStrncasecmp
	case "_strstr":
		return thunkStrstr
	case "_strchr":
		return thunkStrchr
	case "_strrchr":
		return thunkStrrchr
	case "_strdup":
		return thunkStrdup
	case "_sprintf", "___sprintf_chk":
		return thunkSprintf
	case "_snprintf", "___snprintf_chk":
		return thunkSnprintf
	case "_vsprintf", "___vsprintf_chk":
		return thunkVsprintf
	case "_vsnprintf", "___vsnprintf_chk":
		return thunkVsnprintf
	case "_printf":
		return thunkPrintf
	case "_sscanf":
		return thunkSscanf
	case "_atoi", "_atol":
		return thunkAtoi
	case "_atof":
		return thunkAtof
	case "_strtol":
		return thunkStrtol
	case "_strtoul":
		return thunkStrtoul
	case "_qsort":
		return thunkQsort
	case "_rand", "_random":
		return thunkRand
	case "_srand", "_srandom":
		return thunkSrand
	case "_sinf":
		return thunkUnaryMathF32(math.Sin)
	case "_cosf":
		return thunkUnaryMathF32(math.Cos)
	case "_tanf":
		return thunkUnaryMathF32(math.Tan)
	case "_asinf":
		return thunkUnaryMathF32(math.Asin)
	case "_acosf":
		return thunkUnaryMathF32(math.Acos)
	case "_atanf":
		return thunkUnaryMathF32(math.Atan)
	case "_sqrtf":
		return thunkUnaryMathF32(math.Sqrt)
	case "_ceilf":
		return thunkUnaryMathF32(math.Ceil)
	case "_floorf":
		return thunkUnaryMathF32(math.Floor)
	case "_logf":
		return thunkUnaryMathF32(math.Log)
	case "_log10f":
		return thunkUnaryMathF32(math.Log10)
	case "_expf":
		return thunkUnaryMathF32(math.Exp)
	case "_rintf", "_nearbyintf":
		return thunkUnaryMathF32(math.RoundToEven)
	case "_roundf":
		return thunkUnaryMathF32(math.Round)
	case "_truncf":
		return thunkUnaryMathF32(math.Trunc)
	case "_fabsf":
		return thunkUnaryMathF32(math.Abs)
	case "_atan2f":
		return thunkBinaryMathF32(math.Atan2)
	case "_powf":
		return thunkBinaryMathF32(math.Pow)
	case "_fmodf":
		return thunkBinaryMathF32(math.Mod)
	case "_pow":
		return thunkPowF64
	case "_atan2":
		return thunkBinaryMathF64(math.Atan2)
	case "_fmod":
		return thunkBinaryMathF64(math.Mod)
	case "_sin":
		return thunkUnaryMathF64(math.Sin)
	case "_cos":
		return thunkUnaryMathF64(math.Cos)
	case "_tan":
		return thunkUnaryMathF64(math.Tan)
	case "_asin":
		return thunkUnaryMathF64(math.Asin)
	case "_acos":
		return thunkUnaryMathF64(math.Acos)
	case "_atan":
		return thunkUnaryMathF64(math.Atan)
	case "_sqrt":
		return thunkUnaryMathF64(math.Sqrt)
	case "_floor":
		return thunkUnaryMathF64(math.Floor)
	case "_ceil":
		return thunkUnaryMathF64(math.Ceil)
	case "_rint", "_nearbyint":
		return thunkUnaryMathF64(math.RoundToEven)
	case "_round":
		return thunkUnaryMathF64(math.Round)
	case "_trunc":
		return thunkUnaryMathF64(math.Trunc)
	case "_fabs":
		return thunkUnaryMathF64(math.Abs)
	case "_log":
		return thunkUnaryMathF64(math.Log)
	case "_log10":
		return thunkUnaryMathF64(math.Log10)
	case "_exp":
		return thunkUnaryMathF64(math.Exp)
	case "_frexp":
		return thunkFrexpF64
	case "_frexpf":
		return thunkFrexpF32
	case "_ldexp":
		return thunkLdexpF64
	case "_ldexpf":
		return thunkLdexpF32
	case "___isfinited":
		return thunkIsFiniteD
	case "___isnand":
		return thunkIsNanD
	case "___fixunssfdi":
		return thunkFixUnsSfDi
	case "___divdi3":
		return thunkDivDi3
	case "___udivdi3":
		return thunkUdivDi3
	case "___moddi3":
		return thunkModDi3
	case "___umoddi3":
		return thunkUmodDi3
	case "_Fix2X":
		return thunkFix2X
	case "_X2Fix":
		return thunkX2Fix
	case "___cxa_atexit":
		return thunkCxaAtexit
	case "___cxa_guard_acquire":
		return thunkCxaGuardAcquire
	case "___cxa_guard_release", "___cxa_guard_abort":
		return thunkCxaGuardRelease
	case "___stack_chk_fail":
		return thunkStackChkFail
	case "___error":
		return thunkError
	case "__ZNSt3__112basic_stringIcNS_11char_traitsIcEENS_9allocatorIcEEE6__initEPKcm":
		return thunkBasicStringInit
	case "__ZNSt3__112basic_stringIcNS_11char_traitsIcEENS_9allocatorIcEEED1Ev",
		"__ZNSt3__112basic_stringIcNS_11char_traitsIcEENS_9allocatorIcEEED2Ev":
		return thunkBasicStringDtor
	}
	return nil
}

func thunkMalloc(c *cpu.CPU) {
	sz := c.Arg(0)
	c.Regs[cpu.RegEAX] = c.Malloc(sz)
	c.Ret()
}

func thunkFree(c *cpu.CPU) {
	c.Free(c.Arg(0))
	c.Ret()
}

func thunkCalloc(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = c.Calloc(c.Arg(0), c.Arg(1))
	c.Ret()
}

func thunkRealloc(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = c.Realloc(c.Arg(0), c.Arg(1))
	c.Ret()
}

// Guest C printf/sprintf formatter reading from 32-bit stack / va_list
func formatGuestC(c *cpu.CPU, format string, vaPtr uint32) string {
	var out strings.Builder
	curVA := vaPtr
	nextU32 := func() uint32 {
		v := c.Read32(curVA)
		curVA += 4
		return v
	}
	nextU64 := func() uint64 {
		v := c.Read64(curVA)
		curVA += 8
		return v
	}
	nextF64 := func() float64 {
		v := c.ReadF64(curVA)
		curVA += 8
		return v
	}

	i := 0
	for i < len(format) {
		if format[i] != '%' {
			out.WriteByte(format[i])
			i++
			continue
		}
		i++
		if i >= len(format) {
			break
		}
		if format[i] == '%' {
			out.WriteByte('%')
			i++
			continue
		}

		// Parse flags, width, precision, length
		var spec strings.Builder
		spec.WriteByte('%')
		for i < len(format) && strings.ContainsRune("-+ #0", rune(format[i])) {
			spec.WriteByte(format[i])
			i++
		}
		if i < len(format) && format[i] == '*' {
			w := int32(nextU32())
			spec.WriteString(strconv.Itoa(int(w)))
			i++
		} else {
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				spec.WriteByte(format[i])
				i++
			}
		}
		if i < len(format) && format[i] == '.' {
			spec.WriteByte('.')
			i++
			if i < len(format) && format[i] == '*' {
				p := int32(nextU32())
				spec.WriteString(strconv.Itoa(int(p)))
				i++
			} else {
				for i < len(format) && format[i] >= '0' && format[i] <= '9' {
					spec.WriteByte(format[i])
					i++
				}
			}
		}
		isLongLong := false
		for i < len(format) && strings.ContainsRune("hljztL", rune(format[i])) {
			if format[i] == 'l' && i+1 < len(format) && format[i+1] == 'l' {
				isLongLong = true
				i += 2
				break
			}
			i++
		}
		if i >= len(format) {
			break
		}
		conv := format[i]
		i++

		switch conv {
		case 'd', 'i':
			spec.WriteByte('d')
			if isLongLong {
				out.WriteString(fmt.Sprintf(spec.String(), int64(nextU64())))
			} else {
				out.WriteString(fmt.Sprintf(spec.String(), int32(nextU32())))
			}
		case 'u':
			spec.WriteByte('d')
			if isLongLong {
				out.WriteString(fmt.Sprintf(spec.String(), nextU64()))
			} else {
				out.WriteString(fmt.Sprintf(spec.String(), nextU32()))
			}
		case 'x', 'X', 'o':
			spec.WriteByte(conv)
			if isLongLong {
				out.WriteString(fmt.Sprintf(spec.String(), nextU64()))
			} else {
				out.WriteString(fmt.Sprintf(spec.String(), nextU32()))
			}
		case 'p':
			out.WriteString(fmt.Sprintf("0x%08x", nextU32()))
		case 'c':
			ch := byte(nextU32())
			out.WriteByte(ch)
		case 's':
			spec.WriteByte('s')
			strPtr := nextU32()
			var s string
			if strPtr == 0 {
				s = "(null)"
			} else {
				s = c.ReadCString(strPtr)
			}
			out.WriteString(fmt.Sprintf(spec.String(), s))
		case 'f', 'F', 'e', 'E', 'g', 'G':
			spec.WriteByte(conv)
			out.WriteString(fmt.Sprintf(spec.String(), nextF64()))
		case 'n':
			ptr := nextU32()
			if ptr != 0 {
				c.Write32(ptr, uint32(out.Len()))
			}
		default:
			out.WriteByte(conv)
		}
	}
	return out.String()
}

func thunkSprintf(c *cpu.CPU) {
	dst := c.Arg(0)
	fmtAddr := c.Arg(1)
	vaPtr := c.Regs[cpu.RegESP] + 12
	res := formatGuestC(c, c.ReadCString(fmtAddr), vaPtr)
	c.WriteCString(dst, res)
	c.Regs[cpu.RegEAX] = uint32(len(res))
	c.Ret()
}

func thunkSnprintf(c *cpu.CPU) {
	dst := c.Arg(0)
	maxSz := int(c.Arg(1))
	fmtAddr := c.Arg(2)
	vaPtr := c.Regs[cpu.RegESP] + 16
	res := formatGuestC(c, c.ReadCString(fmtAddr), vaPtr)
	fullLen := len(res)
	if maxSz > 0 && dst != 0 {
		if len(res) >= maxSz {
			res = res[:maxSz-1]
		}
		c.WriteCString(dst, res)
	}
	c.Regs[cpu.RegEAX] = uint32(fullLen)
	c.Ret()
}

func thunkVsprintf(c *cpu.CPU) {
	dst := c.Arg(0)
	fmtAddr := c.Arg(1)
	vaPtr := c.Arg(2)
	res := formatGuestC(c, c.ReadCString(fmtAddr), vaPtr)
	c.WriteCString(dst, res)
	c.Regs[cpu.RegEAX] = uint32(len(res))
	c.Ret()
}

func thunkVsnprintf(c *cpu.CPU) {
	dst := c.Arg(0)
	maxSz := int(c.Arg(1))
	fmtAddr := c.Arg(2)
	vaPtr := c.Arg(3)
	res := formatGuestC(c, c.ReadCString(fmtAddr), vaPtr)
	fullLen := len(res)
	if maxSz > 0 && dst != 0 {
		if len(res) >= maxSz {
			res = res[:maxSz-1]
		}
		c.WriteCString(dst, res)
	}
	c.Regs[cpu.RegEAX] = uint32(fullLen)
	c.Ret()
}

func thunkPrintf(c *cpu.CPU) {
	fmtAddr := c.Arg(0)
	vaPtr := c.Regs[cpu.RegESP] + 8
	res := formatGuestC(c, c.ReadCString(fmtAddr), vaPtr)
	fmt.Printf("[GUEST PRINTF] %s", res)
	c.Regs[cpu.RegEAX] = uint32(len(res))
	c.Ret()
}

func thunkQsort(c *cpu.CPU) {
	base := c.Arg(0)
	nmemb := int(c.Arg(1))
	size := int(c.Arg(2))
	compar := c.Arg(3)
	c.Ret()

	if nmemb <= 1 || size <= 0 || compar == 0 {
		return
	}

	if uint64(nmemb)*uint64(size) >= 1<<32 {
		return
	}
	arr := c.WBytes(base, uint32(nmemb*size))
	if arr == nil {
		return
	}
	items := make([][]byte, nmemb)
	for i := range items {
		items[i] = append([]byte(nil), arr[i*size:(i+1)*size]...)
	}

	tmpA := c.Malloc(uint32(size))
	tmpB := c.Malloc(uint32(size))
	defer func() {
		c.Free(tmpA)
		c.Free(tmpB)
	}()

	sort.Slice(items, func(i, j int) bool {
		copy(c.WBytes(tmpA, uint32(size)), items[i])
		copy(c.WBytes(tmpB, uint32(size)), items[j])
		res, err := c.CallGuest(compar, tmpA, tmpB)
		if err != nil {
			return false
		}
		return int32(res) < 0
	})

	for i, it := range items {
		copy(arr[i*size:], it)
	}
}

var rngState uint32 = 1

func thunkRand(c *cpu.CPU) {
	rngState = rngState*1103515245 + 12345
	c.Regs[cpu.RegEAX] = (rngState >> 16) & 0x7FFF
	c.Ret()
}

func thunkSrand(c *cpu.CPU) {
	rngState = c.Arg(0)
	c.Ret()
}

func thunkUnaryMathF32(fn func(float64) float64) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		x := float64(c.ArgF32(0))
		c.FPUPush(fn(x))
		c.Ret()
	}
}

func thunkBinaryMathF32(fn func(float64, float64) float64) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		x := float64(c.ArgF32(0))
		y := float64(c.ArgF32(1))
		c.FPUPush(fn(x, y))
		c.Ret()
	}
}

func thunkUnaryMathF64(fn func(float64) float64) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
		c.FPUPush(fn(x))
		c.Ret()
	}
}

func thunkPowF64(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	y := c.ReadF64(c.Regs[cpu.RegESP] + 12)
	c.FPUPush(math.Pow(x, y))
	c.Ret()
}

func thunkBinaryMathF64(fn func(float64, float64) float64) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
		y := c.ReadF64(c.Regs[cpu.RegESP] + 12)
		c.FPUPush(fn(x, y))
		c.Ret()
	}
}

func thunkFrexpF64(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	expPtr := c.Read32(c.Regs[cpu.RegESP] + 12)
	frac, exp := math.Frexp(x)
	if expPtr != 0 {
		c.Write32(expPtr, uint32(int32(exp)))
	}
	c.FPUPush(frac)
	c.Ret()
}

func thunkFrexpF32(c *cpu.CPU) {
	x := float64(c.ArgF32(0))
	expPtr := c.Arg(1)
	frac, exp := math.Frexp(x)
	if expPtr != 0 {
		c.Write32(expPtr, uint32(int32(exp)))
	}
	c.FPUPush(frac)
	c.Ret()
}

func thunkLdexpF64(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	exp := int(int32(c.Read32(c.Regs[cpu.RegESP] + 12)))
	c.FPUPush(math.Ldexp(x, exp))
	c.Ret()
}

func thunkLdexpF32(c *cpu.CPU) {
	x := float64(c.ArgF32(0))
	exp := int(int32(c.Arg(1)))
	c.FPUPush(math.Ldexp(x, exp))
	c.Ret()
}

func thunkIsFiniteD(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	if !math.IsNaN(x) && !math.IsInf(x, 0) {
		c.Regs[cpu.RegEAX] = 1
	} else {
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func thunkIsNanD(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	if math.IsNaN(x) {
		c.Regs[cpu.RegEAX] = 1
	} else {
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func thunkFixUnsSfDi(c *cpu.CPU) {
	u := fixUns64(float64(c.ArgF32(0)))
	c.Regs[cpu.RegEAX] = uint32(u)
	c.Regs[cpu.RegEDX] = uint32(u >> 32)
	c.Ret()
}

func thunkDivDi3(c *cpu.CPU) {
	a := int64((uint64(c.Arg(1)) << 32) | uint64(c.Arg(0)))
	b := int64((uint64(c.Arg(3)) << 32) | uint64(c.Arg(2)))
	var q int64
	if b != 0 {
		q = a / b
	}
	c.Regs[cpu.RegEAX] = uint32(uint64(q))
	c.Regs[cpu.RegEDX] = uint32(uint64(q) >> 32)
	c.Ret()
}

func thunkUdivDi3(c *cpu.CPU) {
	a := (uint64(c.Arg(1)) << 32) | uint64(c.Arg(0))
	b := (uint64(c.Arg(3)) << 32) | uint64(c.Arg(2))
	var q uint64
	if b != 0 {
		q = a / b
	}
	c.Regs[cpu.RegEAX] = uint32(q)
	c.Regs[cpu.RegEDX] = uint32(q >> 32)
	c.Ret()
}

func thunkModDi3(c *cpu.CPU) {
	a := int64((uint64(c.Arg(1)) << 32) | uint64(c.Arg(0)))
	b := int64((uint64(c.Arg(3)) << 32) | uint64(c.Arg(2)))
	var r int64
	if b != 0 {
		r = a % b
	}
	c.Regs[cpu.RegEAX] = uint32(uint64(r))
	c.Regs[cpu.RegEDX] = uint32(uint64(r) >> 32)
	c.Ret()
}

func thunkUmodDi3(c *cpu.CPU) {
	a := (uint64(c.Arg(1)) << 32) | uint64(c.Arg(0))
	b := (uint64(c.Arg(3)) << 32) | uint64(c.Arg(2))
	var r uint64
	if b != 0 {
		r = a % b
	}
	c.Regs[cpu.RegEAX] = uint32(r)
	c.Regs[cpu.RegEDX] = uint32(r >> 32)
	c.Ret()
}

func thunkFix2X(c *cpu.CPU) {
	fx := int32(c.Arg(0))
	c.FPUPush(float64(fx) / 65536.0)
	c.Ret()
}

func thunkX2Fix(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	c.Regs[cpu.RegEAX] = uint32(x2Fix(x))
	c.Ret()
}

// fixUns64 converts like compiler-rt's __fixunssfdi: negative and NaN give 0, too large saturates
// (Go leaves these cases to the host, and amd64 and arm64 differ).
func fixUns64(f float64) uint64 {
	switch {
	case !(f > 0):
		return 0
	case f >= 1<<64:
		return math.MaxUint64
	}
	return uint64(f)
}

// x2Fix converts to 16.16 fixed point like the Mac Toolbox X2Fix, which saturates.
func x2Fix(x float64) int32 {
	v := math.Trunc(x * 65536)
	switch {
	case v >= math.MaxInt32:
		return math.MaxInt32
	case v <= math.MinInt32:
		return math.MinInt32
	case v != v:
		return math.MinInt32
	}
	return int32(v)
}

func thunkCxaAtexit(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func thunkCxaGuardAcquire(c *cpu.CPU) {
	guard := c.Arg(0)
	if c.Read8(guard) == 0 {
		c.Regs[cpu.RegEAX] = 1
	} else {
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func thunkCxaGuardRelease(c *cpu.CPU) {
	guard := c.Arg(0)
	c.Write8(guard, 1)
	c.Ret()
}

func thunkStackChkFail(c *cpu.CPU) {
	c.Halted = true
	c.HaltReason = fmt.Sprintf("___stack_chk_fail triggered at ESP=0x%08x", c.Regs[cpu.RegESP])
}

func thunkError(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = ErrnoGuestAddr
	c.Ret()
}

// libc++ 32-bit std::__1::basic_string<char>::__init(const char* s, size_t sz)
func thunkBasicStringInit(c *cpu.CPU) {
	thisPtr := c.Arg(0)
	strPtr := c.Arg(1)
	sz := c.Arg(2)
	if sz <= 10 {
		clear(c.WBytes(thisPtr, 12))
		c.Write8(thisPtr, uint8(sz<<1))
		if sz > 0 && strPtr != 0 {
			copy(c.WBytes(thisPtr+1, sz), c.Bytes(strPtr, sz))
		}
		c.Write8(thisPtr+1+sz, 0)
	} else {
		capAligned := ((sz + 16) & ^uint32(15))
		buf := c.Malloc(capAligned)
		if strPtr != 0 {
			copy(c.WBytes(buf, sz), c.Bytes(strPtr, sz))
		}
		c.Write8(buf+sz, 0)
		c.Write32(thisPtr, capAligned|1)
		c.Write32(thisPtr+4, sz)
		c.Write32(thisPtr+8, buf)
	}
	c.Ret()
}

// libc++ 32-bit std::__1::basic_string<char>::~basic_string()
func thunkBasicStringDtor(c *cpu.CPU) {
	thisPtr := c.Arg(0)
	if (c.Read8(thisPtr) & 1) != 0 {
		buf := c.Read32(thisPtr + 8)
		c.Free(buf)
	}
	c.Ret()
}

func oggpackLook(c *cpu.CPU, b uint32, nbits int32) int32 {
	if nbits < 0 || nbits > 32 {
		return -1
	}
	endbyte := int32(c.Read32(b))
	endbit := int32(c.Read32(b + 4))
	storage := int32(c.Read32(b + 16))
	if endbyte+4 >= storage {
		if endbyte*8+endbit+nbits > storage*8 {
			return -1
		}
	}
	ptr := c.Read32(b + 12)
	if ptr+8 <= uint32(len(c.Mem)) {
		w := binary.LittleEndian.Uint64(c.Mem[ptr : ptr+8])
		mask := (uint64(1) << uint(nbits)) - 1
		return int32((w >> uint(endbit)) & mask)
	}
	return -1
}

func oggpackAdv(c *cpu.CPU, b uint32, nbits int32) {
	total := int32(c.Read32(b+4)) + nbits
	bytes := total / 8
	c.Write32(b+12, c.Read32(b+12)+uint32(bytes))
	c.Write32(b, c.Read32(b)+uint32(bytes))
	c.Write32(b+4, uint32(total&7))
}

func thunkOggpackLook(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = uint32(oggpackLook(c, c.Arg(0), int32(c.Arg(1))))
	c.Ret()
}

func thunkOggpackAdv(c *cpu.CPU) {
	oggpackAdv(c, c.Arg(0), int32(c.Arg(1)))
	c.Ret()
}

func thunkOggpackRead(c *cpu.CPU) {
	b := c.Arg(0)
	nbits := int32(c.Arg(1))
	ret := oggpackLook(c, b, nbits)
	oggpackAdv(c, b, nbits)
	c.Regs[cpu.RegEAX] = uint32(ret)
	c.Ret()
}

func vorbisDecodePackedEntryNumber(c *cpu.CPU, book uint32, b uint32) int32 {
	firstTabN := int32(c.Read32(book + 0x24))
	maxLen := int32(c.Read32(book + 0x28))
	lok := oggpackLook(c, b, firstTabN)
	var lo, hi int32
	if lok >= 0 {
		firstTable := c.Read32(book + 0x20)
		entry := int32(c.Read32(firstTable + uint32(lok)*4))
		if entry >= 0 {
			codelengths := c.Read32(book + 0x1c)
			l := int32(int8(c.Read8(codelengths + uint32(entry) - 1)))
			oggpackAdv(c, b, l)
			return entry - 1
		}
		u := uint32(entry)
		lo = int32((u >> 15) & 0x7fff)
		hi = int32(c.Read32(book+0x8)) - int32(u&0x7fff)
	} else {
		lo = 0
		hi = int32(c.Read32(book + 0x8))
	}
	read := maxLen
	lok = oggpackLook(c, b, read)
	for lok < 0 && read > 1 {
		read--
		lok = oggpackLook(c, b, read)
	}
	if lok < 0 {
		return -1
	}
	rev := bits.Reverse32(uint32(lok))
	codelist := c.Read32(book + 0x14)
	for hi-lo > 1 {
		p := (hi - lo) >> 1
		if c.Read32(codelist+uint32(lo+p)*4) <= rev {
			lo += p
		} else {
			hi -= p
		}
	}
	codelengths := c.Read32(book + 0x1c)
	l := int32(int8(c.Read8(codelengths + uint32(lo))))
	if l <= read {
		oggpackAdv(c, b, l)
		return lo
	}
	oggpackAdv(c, b, read)
	return -1
}

// decode_packed_entry_number (fastcall: ECX = book, EDX = b)
func thunkVorbisDecodePackedEntryNumber(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = uint32(vorbisDecodePackedEntryNumber(c, c.Regs[cpu.RegECX], c.Regs[cpu.RegEDX]))
	c.Ret()
}

// vorbis_book_decodev_set(codebook *book, float *a, oggpack_buffer *b, int n)
func thunkVorbisBookDecodevSet(c *cpu.CPU) {
	book := c.Arg(0)
	a := c.Arg(1)
	b := c.Arg(2)
	n := int32(c.Arg(3))
	dim := int32(c.Read32(book))
	valuelist := c.Read32(book + 0x10)
	for i := int32(0); i < n; {
		entry := vorbisDecodePackedEntryNumber(c, book, b)
		if entry == -1 {
			c.Regs[cpu.RegEAX] = 0xffffffff
			c.Ret()
			return
		}
		if dim > 0 {
			t := valuelist + uint32(entry*dim)*4
			for j := int32(0); j < dim; j++ {
				c.Write32(a+uint32(i)*4, c.Read32(t+uint32(j)*4))
				i++
			}
		} else {
			i++
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

// vorbis_book_decodevv_add(codebook *book, float **a, long offset, int ch, oggpack_buffer *b, int n)
func thunkVorbisBookDecodevvAdd(c *cpu.CPU) {
	book := c.Arg(0)
	a := c.Arg(1)
	offset := int32(c.Arg(2))
	ch := int32(c.Arg(3))
	b := c.Arg(4)
	n := int32(c.Arg(5))
	if ch <= 0 {
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	i := offset / ch
	end := (offset + n) / ch
	dim := int32(c.Read32(book))
	valuelist := c.Read32(book + 0x10)
	chptr := int32(0)
	for i < end {
		entry := vorbisDecodePackedEntryNumber(c, book, b)
		if entry == -1 {
			c.Regs[cpu.RegEAX] = 0xffffffff
			c.Ret()
			return
		}
		if dim > 0 {
			t := valuelist + uint32(entry*dim)*4
			for j := int32(0); j < dim; j++ {
				chBuf := c.Read32(a + uint32(chptr)*4)
				addr := chBuf + uint32(i)*4
				c.WriteF32(addr, c.ReadF32(addr)+c.ReadF32(t+uint32(j)*4))
				chptr++
				if chptr == ch {
					chptr = 0
					i++
				}
			}
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

package crt

import (
	"bytes"
	"math"
	"strconv"

	"catalina32/pkg/cpu"
)

// libc memory and string functions. They work on guest memory in place (no Go strings), and
// go through cpu.Bytes/WBytes/CBytes so a bad pointer halts the guest instead of crashing.

func thunkBzero(c *cpu.CPU) {
	clear(c.WBytes(c.Arg(0), c.Arg(1)))
	c.Ret()
}

func thunkMemset(c *cpu.CPU) {
	dst := c.Arg(0)
	val := uint8(c.Arg(1))
	b := c.WBytes(dst, c.Arg(2))
	if val == 0 {
		clear(b)
	} else if len(b) > 0 {
		b[0] = val
		for i := 1; i < len(b); i *= 2 { // double the filled prefix
			copy(b[i:], b[:i])
		}
	}
	c.Regs[cpu.RegEAX] = dst
	c.Ret()
}

func thunkMemsetPattern16(c *cpu.CPU) {
	b := c.WBytes(c.Arg(0), c.Arg(2))
	if p := c.Bytes(c.Arg(1), 16); p != nil {
		for off := 0; off < len(b); off += 16 {
			copy(b[off:], p)
		}
	}
	c.Ret()
}

func thunkMemcpy(c *cpu.CPU) { // also memmove: copy handles overlap
	dst, src, n := c.Arg(0), c.Arg(1), c.Arg(2)
	if n > 0 {
		copy(c.WBytes(dst, n), c.Bytes(src, n))
	}
	c.Regs[cpu.RegEAX] = dst
	c.Ret()
}

func thunkMemcmp(c *cpu.CPU) {
	n := c.Arg(2)
	c.Regs[cpu.RegEAX] = uint32(int32(bytes.Compare(c.Bytes(c.Arg(0), n), c.Bytes(c.Arg(1), n))))
	c.Ret()
}

func thunkMemchr(c *cpu.CPU) {
	s := c.Arg(0)
	c.Regs[cpu.RegEAX] = 0
	if i := bytes.IndexByte(c.Bytes(s, c.Arg(2)), byte(c.Arg(1))); i >= 0 {
		c.Regs[cpu.RegEAX] = s + uint32(i)
	}
	c.Ret()
}

func thunkStrlen(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = uint32(len(c.CBytes(c.Arg(0))))
	c.Ret()
}

// putCString writes src plus a terminator at dst.
func putCString(c *cpu.CPU, dst uint32, src []byte) {
	if b := c.WBytes(dst, uint32(len(src))+1); b != nil {
		copy(b, src)
		b[len(src)] = 0
	}
}

func thunkStrcpy(c *cpu.CPU) {
	dst := c.Arg(0)
	putCString(c, dst, c.CBytes(c.Arg(1)))
	c.Regs[cpu.RegEAX] = dst
	c.Ret()
}

func thunkStrncpy(c *cpu.CPU) {
	dst := c.Arg(0)
	src := c.CBytes(c.Arg(1))
	b := c.WBytes(dst, c.Arg(2))
	n := copy(b, src)
	clear(b[n:])
	c.Regs[cpu.RegEAX] = dst
	c.Ret()
}

func thunkStrcat(c *cpu.CPU) {
	dst := c.Arg(0)
	putCString(c, dst+uint32(len(c.CBytes(dst))), c.CBytes(c.Arg(1)))
	c.Regs[cpu.RegEAX] = dst
	c.Ret()
}

func thunkStrncat(c *cpu.CPU) {
	dst := c.Arg(0)
	src := c.CBytes(c.Arg(1))
	if n := int(c.Arg(2)); len(src) > n {
		src = src[:n]
	}
	putCString(c, dst+uint32(len(c.CBytes(dst))), src)
	c.Regs[cpu.RegEAX] = dst
	c.Ret()
}

func prefix(b []byte, n uint32) []byte {
	if uint32(len(b)) > n {
		return b[:n]
	}
	return b
}

func thunkStrcmp(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = uint32(int32(bytes.Compare(c.CBytes(c.Arg(0)), c.CBytes(c.Arg(1)))))
	c.Ret()
}

func thunkStrncmp(c *cpu.CPU) {
	n := c.Arg(2)
	c.Regs[cpu.RegEAX] = uint32(int32(bytes.Compare(prefix(c.CBytes(c.Arg(0)), n), prefix(c.CBytes(c.Arg(1)), n))))
	c.Ret()
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// compareFold compares like strcasecmp in the C locale (ASCII case folding), returning -1, 0 or 1.
func compareFold(a, b []byte) int32 {
	for i := 0; i < len(a) && i < len(b); i++ {
		if x, y := lowerASCII(a[i]), lowerASCII(b[i]); x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func thunkStrcasecmp(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = uint32(compareFold(c.CBytes(c.Arg(0)), c.CBytes(c.Arg(1))))
	c.Ret()
}

func thunkStrncasecmp(c *cpu.CPU) {
	n := c.Arg(2)
	c.Regs[cpu.RegEAX] = uint32(compareFold(prefix(c.CBytes(c.Arg(0)), n), prefix(c.CBytes(c.Arg(1)), n)))
	c.Ret()
}

func thunkStrstr(c *cpu.CPU) {
	h := c.Arg(0)
	c.Regs[cpu.RegEAX] = 0
	if i := bytes.Index(c.CBytes(h), c.CBytes(c.Arg(1))); i >= 0 {
		c.Regs[cpu.RegEAX] = h + uint32(i)
	}
	c.Ret()
}

func thunkStrchr(c *cpu.CPU) {
	s := c.Arg(0)
	b := c.CBytes(s)
	c.Regs[cpu.RegEAX] = 0
	if ch := byte(c.Arg(1)); ch == 0 {
		c.Regs[cpu.RegEAX] = s + uint32(len(b))
	} else if i := bytes.IndexByte(b, ch); i >= 0 {
		c.Regs[cpu.RegEAX] = s + uint32(i)
	}
	c.Ret()
}

func thunkStrrchr(c *cpu.CPU) {
	s := c.Arg(0)
	b := c.CBytes(s)
	c.Regs[cpu.RegEAX] = 0
	if ch := byte(c.Arg(1)); ch == 0 {
		c.Regs[cpu.RegEAX] = s + uint32(len(b))
	} else if i := bytes.LastIndexByte(b, ch); i >= 0 {
		c.Regs[cpu.RegEAX] = s + uint32(i)
	}
	c.Ret()
}

func thunkStrdup(c *cpu.CPU) {
	src := c.CBytes(c.Arg(0))
	ptr := c.Malloc(uint32(len(src)) + 1)
	putCString(c, ptr, src)
	c.Regs[cpu.RegEAX] = ptr
	c.Ret()
}

func isSpace(b byte) bool {
	return b == ' ' || b >= '\t' && b <= '\r'
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

func digitVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'z':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'Z':
		return int(b-'A') + 10
	}
	return 99
}

// parseInt parses an integer prefix of b like strtol/strtoul: optional whitespace and sign, a
// 0x prefix for base 16 (or base 0), octal for a leading 0 in base 0. It returns the magnitude
// (saturated at 1<<64-1), the sign, and the index after the last digit (0 if no digits).
func parseInt(b []byte, base int) (mag uint64, neg, overflow bool, end int) {
	i := skipSpace(b, 0)
	if i < len(b) && (b[i] == '+' || b[i] == '-') {
		neg = b[i] == '-'
		i++
	}
	if (base == 0 || base == 16) && i+2 < len(b) && b[i] == '0' && b[i+1]|0x20 == 'x' && digitVal(b[i+2]) < 16 {
		i += 2
		base = 16
	} else if base == 0 {
		base = 10
		if i < len(b) && b[i] == '0' {
			base = 8
		}
	}
	if base < 2 || base > 36 {
		return 0, false, false, 0
	}
	start := i
	for ; i < len(b); i++ {
		d := digitVal(b[i])
		if d >= base {
			break
		}
		if mag > (math.MaxUint64-uint64(d))/uint64(base) {
			overflow = true
			mag = math.MaxUint64
		} else if !overflow {
			mag = mag*uint64(base) + uint64(d)
		}
	}
	if i == start {
		return 0, false, false, 0
	}
	return mag, neg, overflow, i
}

// strtol32 is strtol for a 32-bit long: out-of-range values saturate.
func strtol32(b []byte, base int) (int32, int) {
	mag, neg, _, end := parseInt(b, base)
	switch {
	case neg && mag > 1<<31:
		return math.MinInt32, end
	case neg:
		return int32(-int64(mag)), end
	case mag > math.MaxInt32:
		return math.MaxInt32, end
	}
	return int32(mag), end
}

// strtoul32 is strtoul for a 32-bit unsigned long: a minus sign negates, out of range saturates.
func strtoul32(b []byte, base int) (uint32, int) {
	mag, neg, _, end := parseInt(b, base)
	if mag > math.MaxUint32 {
		return math.MaxUint32, end
	}
	if neg {
		return -uint32(mag), end
	}
	return uint32(mag), end
}

func setEnd(c *cpu.CPU, s, endPtr uint32, end int) {
	if endPtr != 0 {
		c.Write32(endPtr, s+uint32(end)) // no digits: end == 0, the original pointer
	}
}

func thunkStrtol(c *cpu.CPU) {
	s := c.Arg(0)
	v, end := strtol32(c.CBytes(s), int(c.Arg(2)))
	setEnd(c, s, c.Arg(1), end)
	c.Regs[cpu.RegEAX] = uint32(v)
	c.Ret()
}

func thunkStrtoul(c *cpu.CPU) {
	s := c.Arg(0)
	v, end := strtoul32(c.CBytes(s), int(c.Arg(2)))
	setEnd(c, s, c.Arg(1), end)
	c.Regs[cpu.RegEAX] = v
	c.Ret()
}

func thunkAtoi(c *cpu.CPU) { // also atol
	v, _ := strtol32(c.CBytes(c.Arg(0)), 10)
	c.Regs[cpu.RegEAX] = uint32(v)
	c.Ret()
}

// parseFloat parses the longest floating-point prefix of b like strtod (decimal and hex forms,
// inf, nan), returning the value and the index after it (0 if none).
func parseFloat(b []byte) (float64, int) {
	i := skipSpace(b, 0)
	start := i
	if i < len(b) && (b[i] == '+' || b[i] == '-') {
		i++
	}
	for _, word := range []string{"infinity", "inf", "nan"} {
		if len(b)-i >= len(word) && compareFold(b[i:i+len(word)], []byte(word)) == 0 {
			v, _ := strconv.ParseFloat(string(b[start:i+len(word)]), 64)
			return v, i + len(word)
		}
	}
	hex := i+1 < len(b) && b[i] == '0' && (b[i+1]|0x20) == 'x'
	digitsOK := func(d byte) bool { return d >= '0' && d <= '9' }
	if hex {
		i += 2
		digitsOK = func(d byte) bool { return digitVal(d) < 16 }
	}
	mant := i
	for i < len(b) && digitsOK(b[i]) {
		i++
	}
	if i < len(b) && b[i] == '.' {
		i++
		for i < len(b) && digitsOK(b[i]) {
			i++
		}
	}
	if i == mant || i == mant+1 && b[mant] == '.' {
		if hex { // "0x" with no hex digits: just the 0
			v, _ := strconv.ParseFloat(string(b[start:mant-1]), 64)
			return v, mant - 1
		}
		return 0, 0
	}
	if exp := byte('e'); i < len(b) && (b[i]|0x20 == exp && !hex || b[i]|0x20 == 'p' && hex) {
		j := i + 1
		if j < len(b) && (b[j] == '+' || b[j] == '-') {
			j++
		}
		if j < len(b) && b[j] >= '0' && b[j] <= '9' {
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			i = j
		}
	}
	text := string(b[start:i])
	if hex && !bytes.ContainsAny(b[mant:i], "pP") {
		text += "p0" // Go requires the exponent in hex floats
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil && v == 0 {
		return 0, i
	}
	return v, i
}

func thunkAtof(c *cpu.CPU) {
	v, _ := parseFloat(c.CBytes(c.Arg(0)))
	c.FPUPush(v)
	c.Ret()
}

func thunkSscanf(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = uint32(scanGuest(c, c.CBytes(c.Arg(0)), c.CBytes(c.Arg(1)), c.Regs[cpu.RegESP]+12))
	c.Ret()
}

// scanGuest implements sscanf: conversions d i u o x X f e g E G a s c [ n p, with '*'
// suppression, widths and the hh/h/l/ll/L/j/z/t length modifiers. It returns the number of
// assignments, or -1 if the input ended before the first conversion.
func scanGuest(c *cpu.CPU, in, format []byte, va uint32) int32 {
	pos, assigned, convs := 0, int32(0), 0
	nextPtr := func() uint32 {
		p := c.Read32(va)
		va += 4
		return p
	}
	for f := 0; f < len(format); f++ {
		ch := format[f]
		if isSpace(ch) {
			pos = skipSpace(in, pos)
			continue
		}
		if ch != '%' || f+1 < len(format) && format[f+1] == '%' {
			if ch == '%' {
				f++
				pos = skipSpace(in, pos)
			}
			if pos >= len(in) {
				if convs == 0 {
					return -1
				}
				break
			}
			if in[pos] != ch {
				break
			}
			pos++
			continue
		}
		f++
		suppress := f < len(format) && format[f] == '*'
		if suppress {
			f++
		}
		width := 0
		for f < len(format) && format[f] >= '0' && format[f] <= '9' {
			width = width*10 + int(format[f]-'0')
			f++
		}
		lmod := ""
		for f < len(format) && bytes.IndexByte([]byte("hlLqjzt"), format[f]) >= 0 {
			lmod += string(format[f])
			f++
		}
		if f >= len(format) {
			break
		}
		size := 4 // bytes stored by an integer conversion
		switch lmod {
		case "hh":
			size = 1
		case "h":
			size = 2
		case "ll", "L", "q", "j":
			size = 8
		}
		double := lmod != "" && lmod[0] == 'l' || lmod == "L"
		conv := format[f]
		if conv != 'c' && conv != '[' && conv != 'n' {
			pos = skipSpace(in, pos)
		}
		if pos >= len(in) && conv != 'n' {
			if convs == 0 {
				return -1
			}
			break
		}
		field := in[pos:]
		if width > 0 && width < len(field) {
			field = field[:width]
		}
		convs++
		switch conv {
		case 'd', 'i', 'u', 'o', 'x', 'X', 'p':
			base := map[byte]int{'d': 10, 'i': 0, 'u': 10, 'o': 8, 'x': 16, 'X': 16, 'p': 16}[conv]
			mag, neg, _, end := parseInt(field, base)
			if end == 0 {
				return assigned
			}
			pos += end
			v := mag
			if neg {
				v = -mag
			}
			if !suppress {
				p := nextPtr()
				switch size {
				case 1:
					c.Write8(p, uint8(v))
				case 2:
					c.Write16(p, uint16(v))
				case 8:
					c.Write64(p, v)
				default:
					c.Write32(p, uint32(v))
				}
				assigned++
			}
		case 'f', 'e', 'g', 'E', 'G', 'a', 'A', 'F':
			v, end := parseFloat(field)
			if end == 0 {
				return assigned
			}
			pos += end
			if !suppress {
				p := nextPtr()
				if double {
					c.WriteF64(p, v)
				} else {
					c.WriteF32(p, float32(v))
				}
				assigned++
			}
		case 's':
			end := 0
			for end < len(field) && !isSpace(field[end]) {
				end++
			}
			pos += end
			if !suppress {
				putCString(c, nextPtr(), field[:end])
				assigned++
			}
		case 'c':
			n := max(width, 1)
			if n > len(field) {
				return assigned
			}
			pos += n
			if !suppress {
				copy(c.WBytes(nextPtr(), uint32(n)), field[:n])
				assigned++
			}
		case '[':
			f++
			negate := f < len(format) && format[f] == '^'
			if negate {
				f++
			}
			var set [256]bool
			for first := true; f < len(format) && (format[f] != ']' || first); f++ {
				if f+2 < len(format) && format[f+1] == '-' && format[f+2] != ']' {
					for x := int(format[f]); x <= int(format[f+2]); x++ {
						set[x] = true
					}
					f += 2
				} else {
					set[format[f]] = true
				}
				first = false
			}
			end := 0
			for end < len(field) && set[field[end]] != negate {
				end++
			}
			if end == 0 {
				return assigned
			}
			pos += end
			if !suppress {
				putCString(c, nextPtr(), field[:end])
				assigned++
			}
		case 'n':
			convs--
			if !suppress {
				c.Write32(nextPtr(), uint32(pos))
			}
		default:
			return assigned
		}
	}
	return assigned
}

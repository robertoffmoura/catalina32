package source

import (
	"path/filepath"
	"strings"

	"catalina32/pkg/cpu"
	"catalina32/pkg/crt"
)

// system is the linker's resolver for everything outside the game: it gives each system symbol
// one guest address, a host function (an intercept) or a data block.
func (m *Machine) system(lib, sym string) uint32 {
	if a, ok := m.sys[sym]; ok {
		return a
	}
	if size, init := dataSymbol(sym); size > 0 {
		a := m.data(size)
		if init != nil {
			init(m, a)
		}
		m.sys[sym], m.sysName[a] = a, sym
		return a
	}
	a := m.thunkNext
	m.thunkNext += 4
	if m.thunkNext > thunkHi {
		panic("source: out of host function addresses")
	}
	m.C.Write8(a, 0xc3) // ret, should anything look at the code
	fn := m.shim(sym)
	if fn == nil && isGL(lib, sym) {
		fn = m.glFunc(sym)
	}
	if fn == nil {
		fn = m.unimplemented(filepath.Base(lib), sym)
	}
	m.C.RegisterIntercept(a, sym, fn)
	m.sys[sym], m.sysName[a] = a, sym
	return a
}

// data allocates n zeroed bytes of system library data.
func (m *Machine) data(n uint32) uint32 {
	a := (m.dataNext + 15) &^ 15
	m.dataNext = a + n
	if m.dataNext > dataHi {
		panic("source: out of system data space")
	}
	return a
}

// shim finds the host implementation of a system function.
func (m *Machine) shim(sym string) cpu.ThunkFunc {
	if fn := m.shims[sym]; fn != nil {
		return fn
	}
	base := sym
	if i := strings.IndexByte(sym, '$'); i > 0 { // _fopen$UNIX2003, _stat$INODE64...
		base = sym[:i]
		if fn := m.shims[base]; fn != nil {
			return fn
		}
	}
	return crt.Lookup(base)
}

// unimplemented returns a stub that records the call and returns 0.
func (m *Machine) unimplemented(lib, sym string) cpu.ThunkFunc {
	m.unimplLib[sym] = lib
	return func(c *cpu.CPU) {
		m.Unimpl[sym]++
		if m.Unimpl[sym] == 1 {
			m.logf("[unimpl] %s (%s) called from %s", sym, lib, m.where(c.Read32(c.Regs[cpu.RegESP])))
		}
		c.Regs[cpu.RegEAX], c.Regs[cpu.RegEDX] = 0, 0
		c.Ret()
	}
}

// dataSymbol says whether a system symbol is data, its size and how to fill it.
func dataSymbol(sym string) (uint32, func(m *Machine, a uint32)) {
	switch sym {
	case "___stack_chk_guard":
		return 4, func(m *Machine, a uint32) { m.C.Write32(a, 0xdeadbeef) }
	case "___stdinp", "___stdoutp", "___stderrp":
		return 4, func(m *Machine, a uint32) { m.C.Write32(a, m.stdFile(sym)) }
	case "__DefaultRuneLocale":
		return runeLocaleSize, initRuneLocale
	case "_environ", "_NXArgc", "_NXArgv":
		return 4, nil
	case "_kCFAllocatorDefault", "___CFConstantStringClassReference", "_kCFTypeDictionaryKeyCallBacks",
		"_kCFTypeDictionaryValueCallBacks", "_kCFTypeArrayCallBacks", "_kCFBooleanTrue", "_kCFBooleanFalse":
		return 32, nil
	case "_mach_task_self_":
		return 4, func(m *Machine, a uint32) { m.C.Write32(a, 0x103) }
	}
	switch {
	case strings.HasPrefix(sym, "__ZTV"): // vtables of the C++ runtime's classes (type_info kinds, exceptions)
		return 64, initRuntimeVtable
	case strings.HasPrefix(sym, "__ZTI"): // type_info objects
		return 16, initRuntimeTypeinfo(sym)
	case strings.HasPrefix(sym, "__ZTS"): // type names
		return uint32(len(sym)) + 1, func(m *Machine, a uint32) { m.C.WriteCString(a, sym[5:]) }
	case strings.HasSuffix(sym, "_S_empty_rep_storageE"): // COW std::string's shared empty string
		return 16, nil
	case strings.HasPrefix(sym, "__ZSt4cout"), strings.HasPrefix(sym, "__ZSt4cerr"), strings.HasPrefix(sym, "__ZSt4clog"):
		return 256, nil
	case strings.HasPrefix(sym, "_kCF"), strings.HasPrefix(sym, "_kAX"), strings.HasPrefix(sym, "_kIO"):
		return 32, nil
	}
	return 0, nil
}

// _RuneLocale: magic[8], encoding[32], sgetrune, sputrune, invalid_rune, runetype[256],
// maplower[256], mapupper[256], then range tables we leave empty.
const (
	runeLocaleSize = 3200
	runetypeOff    = 52
	maplowerOff    = runetypeOff + 1024
	mapupperOff    = maplowerOff + 1024
)

// ctype classes (<runetype.h>).
const (
	ctA   = 0x100      // alpha
	ctC   = 0x200      // control
	ctD   = 0x400      // digit
	ctG   = 0x800      // graph
	ctL   = 0x1000     // lower
	ctP   = 0x2000     // punct
	ctS   = 0x4000     // space
	ctU   = 0x8000     // upper
	ctX   = 0x10000    // hex digit
	ctB   = 0x20000    // blank
	ctR   = 0x40000    // print
	ctI   = 0x80000    // ideogram
	ctT   = 0x100000   // special
	ctQ   = 0x200000   // phonogram
	ctSW1 = 0x40000000 // one column wide
)

func ctype(r int) uint32 {
	var t uint32
	switch {
	case r < 0x20 || r == 0x7f:
		t |= ctC
	default:
		if r < 0x80 {
			t |= ctR | ctSW1
		}
	}
	if r == ' ' || r == '\t' {
		t |= ctB
	}
	if r == ' ' || (r >= '\t' && r <= '\r') {
		t |= ctS
	}
	switch {
	case r >= 'a' && r <= 'z':
		t |= ctA | ctL | ctG
	case r >= 'A' && r <= 'Z':
		t |= ctA | ctU | ctG
	case r >= '0' && r <= '9':
		t |= ctD | ctG | uint32(r-'0') // the digit's value in the low byte
	case r > 0x20 && r < 0x7f:
		t |= ctP | ctG
	}
	if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
		t |= ctX
	}
	return t
}

func initRuneLocale(m *Machine, a uint32) {
	c := m.C
	copy(c.WBytes(a, 8), "RuneMagA")
	copy(c.WBytes(a+8, 32), "NONE")
	c.Write32(a+48, 0xfffd)
	for r := 0; r < 256; r++ {
		c.Write32(a+runetypeOff+uint32(4*r), ctype(r))
		lo, up := r, r
		if r >= 'A' && r <= 'Z' {
			lo = r + 32
		}
		if r >= 'a' && r <= 'z' {
			up = r - 32
		}
		c.Write32(a+maplowerOff+uint32(4*r), uint32(lo))
		c.Write32(a+mapupperOff+uint32(4*r), uint32(up))
	}
}

// The runtime's own classes only need vtables that exist (type_info objects point at vtable+8);
// their slots are filled lazily by the C++ runtime shims.
func initRuntimeVtable(m *Machine, a uint32) {}

func initRuntimeTypeinfo(sym string) func(m *Machine, a uint32) {
	return func(m *Machine, a uint32) {
		name := m.data(uint32(len(sym)))
		m.C.WriteCString(name, sym[5:])
		vt := "__ZTVN10__cxxabiv117__class_type_infoE"
		if sym == "__ZTIi" || sym == "__ZTIj" || sym == "__ZTIc" || sym == "__ZTIf" {
			vt = "__ZTVN10__cxxabiv123__fundamental_type_infoE"
		}
		m.C.Write32(a, m.system("", vt)+8)
		m.C.Write32(a+4, name)
	}
}

// hostFunc gives a host function a guest address (for vtables the host fills).
func (m *Machine) hostFunc(name string, fn cpu.ThunkFunc) uint32 {
	a := m.thunkNext
	m.thunkNext += 4
	if m.thunkNext > thunkHi {
		panic("source: out of host function addresses")
	}
	m.C.Write8(a, 0xc3)
	m.C.RegisterIntercept(a, name, fn)
	m.sysName[a] = name
	return a
}

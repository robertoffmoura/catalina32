package source

import (
	"bytes"
	"strings"

	"catalina32/pkg/cpu"
)

// The C++ runtime: GCC's libstdc++ (the copy-on-write std::string ABI, red-black tree and list
// helpers), and the Itanium C++ ABI's dynamic_cast and exceptions.

type cxxState struct {
	vtables map[uint32]string // type_info vtable (+8) -> kind
}

// COW std::basic_string: the object holds a pointer to the characters, which follow a _Rep
// header { length, capacity, refcount } (refcount 0: one owner, -1: unshareable).
type cowString struct {
	m     *Machine
	width uint32 // 1 (char) or 4 (wchar_t)
	empty uint32 // the shared empty _Rep
}

func (m *Machine) cow(width uint32) *cowString {
	name := "__ZNSs4_Rep20_S_empty_rep_storageE"
	if width == 4 {
		name = "__ZNSbIwSt11char_traitsIwESaIwEE4_Rep20_S_empty_rep_storageE"
	}
	return &cowString{m: m, width: width, empty: m.system("", name)}
}

func (s *cowString) get(obj uint32) []byte {
	c := s.m.C
	p := c.Read32(obj)
	n := c.Read32(p - 12)
	return append([]byte(nil), c.Bytes(p, n*s.width)...)
}

// rep allocates a rep holding data (length in characters = len/width) and returns its characters.
func (s *cowString) rep(data []byte, capChars uint32) uint32 {
	c := s.m.C
	n := uint32(len(data)) / s.width
	if n == 0 && capChars == 0 {
		return s.empty + 12
	}
	capChars = max(capChars, n)
	r := c.Malloc(12 + (capChars+1)*s.width)
	c.Write32(r, n)
	c.Write32(r+4, capChars)
	c.Write32(r+8, 0)
	copy(c.WBytes(r+12, uint32(len(data))), data)
	clear(c.WBytes(r+12+n*s.width, s.width))
	return r + 12
}

func (s *cowString) dispose(p uint32) {
	c := s.m.C
	r := p - 12
	if r == s.empty || p == 0 {
		return
	}
	rc := int32(c.Read32(r + 8))
	if rc <= 0 {
		c.Free(r)
		return
	}
	c.Write32(r+8, uint32(rc-1))
}

func (s *cowString) set(obj uint32, data []byte) {
	old := s.m.C.Read32(obj)
	s.m.C.Write32(obj, s.rep(data, 0))
	s.dispose(old)
}

// leak makes the string's rep unshared and marks it unshareable (non-const access to characters).
func (s *cowString) leak(obj uint32) uint32 {
	c := s.m.C
	p := c.Read32(obj)
	r := p - 12
	if r == s.empty {
		return p
	}
	if int32(c.Read32(r+8)) > 0 {
		s.set(obj, s.get(obj))
		p = c.Read32(obj)
		r = p - 12
	}
	if r != s.empty {
		c.Write32(r+8, 0xffffffff)
	}
	return p
}

func (s *cowString) cstr(a uint32) []byte { // a NUL-terminated string of this width
	c := s.m.C
	var out []byte
	for {
		ch := c.Bytes(a, s.width)
		if bytes.Equal(ch, make([]byte, s.width)) {
			return out
		}
		out = append(out, ch...)
		a += s.width
	}
}

func clampPos(pos, n uint32, size uint32) (uint32, uint32) {
	if pos > size {
		pos = size
	}
	if n > size-pos {
		n = size - pos
	}
	return pos, n
}

func cmpBytes(a, b []byte) uint32 {
	return uint32(int32(bytes.Compare(a, b)))
}

func (m *Machine) registerCxxShims() {
	s, w := m.cow(1), m.cow(4)
	m.cxx.vtables = map[uint32]string{}
	for _, kind := range []string{"17__class_type_info", "20__si_class_type_info", "21__vmi_class_type_info",
		"23__fundamental_type_info", "19__pointer_type_info", "16__enum_type_info"} {
		m.cxx.vtables[m.system("", "__ZTVN10__cxxabiv1"+kind+"E")+8] = kind[2:]
	}
	npos := uint32(0xffffffff)
	str := map[string]cpu.ThunkFunc{
		"__ZN9__gnu_cxx18__exchange_and_addEPVii": func(c *cpu.CPU) {
			old := c.Read32(c.Arg(0))
			c.Write32(c.Arg(0), old+c.Arg(1))
			ret(c, old)
		},
		"__ZN9__gnu_cxx12__atomic_addEPVii": func(c *cpu.CPU) {
			c.Write32(c.Arg(0), c.Read32(c.Arg(0))+c.Arg(1))
			c.Ret()
		},
		"__ZNKSs4dataEv":     func(c *cpu.CPU) { ret(c, c.Read32(c.Arg(0))) },
		"__ZNKSs5c_strEv":    func(c *cpu.CPU) { ret(c, c.Read32(c.Arg(0))) },
		"__ZNKSs4sizeEv":     func(c *cpu.CPU) { ret(c, c.Read32(c.Read32(c.Arg(0))-12)) },
		"__ZNKSs6lengthEv":   func(c *cpu.CPU) { ret(c, c.Read32(c.Read32(c.Arg(0))-12)) },
		"__ZNKSs8capacityEv": func(c *cpu.CPU) { ret(c, c.Read32(c.Read32(c.Arg(0))-8)) },
		"__ZNKSs5emptyEv": func(c *cpu.CPU) {
			if c.Read32(c.Read32(c.Arg(0))-12) == 0 {
				ret(c, 1)
				return
			}
			ret(c, 0)
		},
		"__ZNKSs7_M_iendEv": func(c *cpu.CPU) { p := c.Read32(c.Arg(0)); ret(c, p+c.Read32(p-12)) },
		"__ZNKSsixEm":       func(c *cpu.CPU) { ret(c, c.Read32(c.Arg(0))+c.Arg(1)) },
		"__ZNSsixEm":        func(c *cpu.CPU) { ret(c, s.leak(c.Arg(0))+c.Arg(1)) },
		"__ZNSs5beginEv":    func(c *cpu.CPU) { ret(c, s.leak(c.Arg(0))) },
		"__ZNSs3endEv":      func(c *cpu.CPU) { p := s.leak(c.Arg(0)); ret(c, p+c.Read32(p-12)) },
		"__ZNKSs12find_last_ofEcm": func(c *cpu.CPU) {
			d, ch, pos := s.get(c.Arg(0)), byte(c.Arg(1)), c.Arg(2)
			if len(d) == 0 {
				ret(c, npos)
				return
			}
			if pos >= uint32(len(d)) {
				pos = uint32(len(d)) - 1
			}
			ret(c, uint32(bytes.LastIndexByte(d[:pos+1], ch)))
		},
		"__ZNKSs13find_first_ofEcm": func(c *cpu.CPU) {
			d, ch, pos := s.get(c.Arg(0)), byte(c.Arg(1)), c.Arg(2)
			if pos >= uint32(len(d)) {
				ret(c, npos)
				return
			}
			i := bytes.IndexByte(d[pos:], ch)
			if i < 0 {
				ret(c, npos)
				return
			}
			ret(c, pos+uint32(i))
		},
		"__ZNKSs4findEPKcmm": func(c *cpu.CPU) {
			d, needle, pos := s.get(c.Arg(0)), c.Bytes(c.Arg(1), c.Arg(3)), c.Arg(2)
			if pos > uint32(len(d)) {
				ret(c, npos)
				return
			}
			i := bytes.Index(d[pos:], needle)
			if i < 0 {
				ret(c, npos)
				return
			}
			ret(c, pos+uint32(i))
		},
		"__ZNKSs6substrEmm": func(c *cpu.CPU) { // sret, this, pos, n
			out, d := c.Arg(0), s.get(c.Arg(1))
			pos, n := clampPos(c.Arg(2), c.Arg(3), uint32(len(d)))
			c.Write32(out, s.rep(d[pos:pos+n], 0))
			c.Regs[cpu.RegEAX] = out
			c.RetN(4)
		},
		"__ZNKSs7compareEPKc":  func(c *cpu.CPU) { ret(c, cmpBytes(s.get(c.Arg(0)), s.cstr(c.Arg(1)))) },
		"__ZNKSs7compareERKSs": func(c *cpu.CPU) { ret(c, cmpBytes(s.get(c.Arg(0)), s.get(c.Arg(1)))) },
		"__ZNKSs7compareEmmRKSs": func(c *cpu.CPU) {
			d := s.get(c.Arg(0))
			pos, n := clampPos(c.Arg(1), c.Arg(2), uint32(len(d)))
			ret(c, cmpBytes(d[pos:pos+n], s.get(c.Arg(3))))
		},
		"__ZNSaIcEC1Ev": retVoid, "__ZNSaIcED1Ev": retVoid, "__ZNSaIcEC2Ev": retVoid, "__ZNSaIcED2Ev": retVoid,
		"__ZNSs12_Alloc_hiderC1EPcRKSaIcE": func(c *cpu.CPU) { c.Write32(c.Arg(0), c.Arg(1)); c.Ret() },
		"__ZNSs12_S_empty_repEv":           func(c *cpu.CPU) { ret(c, s.empty) },
		"__ZNSs13_S_copy_charsEPcS_S_": func(c *cpu.CPU) {
			copy(c.WBytes(c.Arg(0), c.Arg(2)-c.Arg(1)), c.Bytes(c.Arg(1), c.Arg(2)-c.Arg(1)))
			c.Ret()
		},
		"__ZNSs4_Rep10_M_destroyERKSaIcE": func(c *cpu.CPU) { c.Free(c.Arg(0)); c.Ret() },
		"__ZNSs4_Rep10_M_refdataEv":       func(c *cpu.CPU) { ret(c, c.Arg(0)+12) },
		"__ZNSs4_Rep26_M_set_length_and_sharableEm": func(c *cpu.CPU) {
			r, n := c.Arg(0), c.Arg(1)
			if r != s.empty {
				c.Write32(r+8, 0)
				c.Write32(r, n)
				c.Write8(r+12+n, 0)
			}
			c.Ret()
		},
		"__ZNSs4_Rep9_S_createEmmRKSaIcE": func(c *cpu.CPU) {
			capacity := max(c.Arg(0), 1)
			if old := c.Arg(1); capacity > old && capacity < 2*old {
				capacity = 2 * old
			}
			r := c.Malloc(12 + capacity + 1)
			c.Write32(r, 0)
			c.Write32(r+4, capacity)
			c.Write32(r+8, 0)
			ret(c, r)
		},
		"__ZNSs5clearEv": func(c *cpu.CPU) { s.set(c.Arg(0), nil); c.Ret() },
		"__ZNSs5eraseEmm": func(c *cpu.CPU) {
			d := s.get(c.Arg(0))
			pos, n := clampPos(c.Arg(1), c.Arg(2), uint32(len(d)))
			s.set(c.Arg(0), append(d[:pos:pos], d[pos+n:]...))
			ret(c, c.Arg(0))
		},
		"__ZNSs6appendEPKc": func(c *cpu.CPU) { s.set(c.Arg(0), append(s.get(c.Arg(0)), s.cstr(c.Arg(1))...)); ret(c, c.Arg(0)) },
		"__ZNSs6appendEPKcm": func(c *cpu.CPU) {
			s.set(c.Arg(0), append(s.get(c.Arg(0)), c.Bytes(c.Arg(1), c.Arg(2))...))
			ret(c, c.Arg(0))
		},
		"__ZNSs6appendERKSs": func(c *cpu.CPU) { s.set(c.Arg(0), append(s.get(c.Arg(0)), s.get(c.Arg(1))...)); ret(c, c.Arg(0)) },
		"__ZNSs6appendERKSsmm": func(c *cpu.CPU) {
			src := s.get(c.Arg(1))
			pos, n := clampPos(c.Arg(2), c.Arg(3), uint32(len(src)))
			s.set(c.Arg(0), append(s.get(c.Arg(0)), src[pos:pos+n]...))
			ret(c, c.Arg(0))
		},
		"__ZNSs6appendEmc": func(c *cpu.CPU) {
			s.set(c.Arg(0), append(s.get(c.Arg(0)), bytes.Repeat([]byte{byte(c.Arg(2))}, int(c.Arg(1)))...))
			ret(c, c.Arg(0))
		},
		"__ZNSs6assignEPKc": func(c *cpu.CPU) { s.set(c.Arg(0), s.cstr(c.Arg(1))); ret(c, c.Arg(0)) },
		"__ZNSs6assignEPKcm": func(c *cpu.CPU) {
			s.set(c.Arg(0), append([]byte(nil), c.Bytes(c.Arg(1), c.Arg(2))...))
			ret(c, c.Arg(0))
		},
		"__ZNSs6assignERKSs": func(c *cpu.CPU) { s.set(c.Arg(0), s.get(c.Arg(1))); ret(c, c.Arg(0)) },
		"__ZNSsaSEPKc":       func(c *cpu.CPU) { s.set(c.Arg(0), s.cstr(c.Arg(1))); ret(c, c.Arg(0)) },
		"__ZNSsaSERKSs":      func(c *cpu.CPU) { s.set(c.Arg(0), s.get(c.Arg(1))); ret(c, c.Arg(0)) },
		"__ZNSspLEPKc":       func(c *cpu.CPU) { s.set(c.Arg(0), append(s.get(c.Arg(0)), s.cstr(c.Arg(1))...)); ret(c, c.Arg(0)) },
		"__ZNSspLERKSs":      func(c *cpu.CPU) { s.set(c.Arg(0), append(s.get(c.Arg(0)), s.get(c.Arg(1))...)); ret(c, c.Arg(0)) },
		"__ZNSspLEc":         func(c *cpu.CPU) { s.set(c.Arg(0), append(s.get(c.Arg(0)), byte(c.Arg(1)))); ret(c, c.Arg(0)) },
		"__ZNSs9push_backEc": func(c *cpu.CPU) { s.set(c.Arg(0), append(s.get(c.Arg(0)), byte(c.Arg(1)))); c.Ret() },
		"__ZNSs6resizeEm": func(c *cpu.CPU) {
			d, n := s.get(c.Arg(0)), int(c.Arg(1))
			if n <= len(d) {
				d = d[:n]
			} else {
				d = append(d, make([]byte, n-len(d))...)
			}
			s.set(c.Arg(0), d)
			c.Ret()
		},
		"__ZNSs7reserveEm": func(c *cpu.CPU) {
			obj := c.Arg(0)
			d := s.get(obj)
			old := c.Read32(obj)
			c.Write32(obj, s.rep(d, c.Arg(1)))
			s.dispose(old)
			c.Ret()
		},
		"__ZNSs9_M_mutateEmmm": func(c *cpu.CPU) { // replace len1 characters at pos by len2 (to be written by the caller)
			d := s.get(c.Arg(0))
			pos, len1, len2 := c.Arg(1), c.Arg(2), c.Arg(3)
			pos, len1 = clampPos(pos, len1, uint32(len(d)))
			nd := append(append(append([]byte(nil), d[:pos]...), make([]byte, len2)...), d[pos+len1:]...)
			s.set(c.Arg(0), nd)
			c.Ret()
		},
		"__ZNSs7replaceEN9__gnu_cxx17__normal_iteratorIPcSsEES2_PKcS4_": func(c *cpu.CPU) {
			obj := c.Arg(0)
			p := c.Read32(obj)
			d := s.get(obj)
			i1, i2 := c.Arg(1)-p, c.Arg(2)-p
			src := append([]byte(nil), c.Bytes(c.Arg(3), c.Arg(4)-c.Arg(3))...)
			nd := append(append(append([]byte(nil), d[:i1]...), src...), d[i2:]...)
			s.set(obj, nd)
			ret(c, obj)
		},
		"__ZNSsC1EPKcRKSaIcE":  func(c *cpu.CPU) { c.Write32(c.Arg(0), s.rep(s.cstr(c.Arg(1)), 0)); c.Ret() },
		"__ZNSsC2EPKcRKSaIcE":  func(c *cpu.CPU) { c.Write32(c.Arg(0), s.rep(s.cstr(c.Arg(1)), 0)); c.Ret() },
		"__ZNSsC1EPKcmRKSaIcE": func(c *cpu.CPU) { c.Write32(c.Arg(0), s.rep(c.Bytes(c.Arg(1), c.Arg(2)), 0)); c.Ret() },
		"__ZNSsC1ERKSs":        func(c *cpu.CPU) { c.Write32(c.Arg(0), s.rep(s.get(c.Arg(1)), 0)); c.Ret() },
		"__ZNSsC2ERKSs":        func(c *cpu.CPU) { c.Write32(c.Arg(0), s.rep(s.get(c.Arg(1)), 0)); c.Ret() },
		"__ZNSsC1Ev":           func(c *cpu.CPU) { c.Write32(c.Arg(0), s.empty+12); c.Ret() },
		"__ZNSsD1Ev":           func(c *cpu.CPU) { s.dispose(c.Read32(c.Arg(0))); c.Ret() },
		"__ZNSsD2Ev":           func(c *cpu.CPU) { s.dispose(c.Read32(c.Arg(0))); c.Ret() },

		// std::wstring (wchar_t is 4 bytes).
		"__ZNSbIwSt11char_traitsIwESaIwEE4_Rep10_M_destroyERKS1_": func(c *cpu.CPU) { c.Free(c.Arg(0)); c.Ret() },
		"__ZNSbIwSt11char_traitsIwESaIwEE6assignERKS2_":           func(c *cpu.CPU) { w.set(c.Arg(0), w.get(c.Arg(1))); ret(c, c.Arg(0)) },
		"__ZNSbIwSt11char_traitsIwESaIwEEC1EPKwRKS1_":             func(c *cpu.CPU) { c.Write32(c.Arg(0), w.rep(w.cstr(c.Arg(1)), 0)); c.Ret() },
		"__ZNSbIwSt11char_traitsIwESaIwEEC1EPKwmRKS1_": func(c *cpu.CPU) {
			c.Write32(c.Arg(0), w.rep(c.Bytes(c.Arg(1), 4*c.Arg(2)), 0))
			c.Ret()
		},
		"__ZNSbIwSt11char_traitsIwESaIwEEC1ERKS2_": func(c *cpu.CPU) { c.Write32(c.Arg(0), w.rep(w.get(c.Arg(1)), 0)); c.Ret() },

		// Containers.
		"__ZNSt15_List_node_base4hookEPS_": func(c *cpu.CPU) {
			this, pos := c.Arg(0), c.Arg(1)
			prev := c.Read32(pos + 4)
			c.Write32(this, pos)
			c.Write32(this+4, prev)
			c.Write32(prev, this)
			c.Write32(pos+4, this)
			c.Ret()
		},
		"__ZNSt15_List_node_base6unhookEv": func(c *cpu.CPU) {
			this := c.Arg(0)
			next, prev := c.Read32(this), c.Read32(this+4)
			c.Write32(prev, next)
			c.Write32(next+4, prev)
			c.Ret()
		},
		"__ZSt18_Rb_tree_incrementPSt18_Rb_tree_node_base":  func(c *cpu.CPU) { ret(c, rbIncrement(c, c.Arg(0))) },
		"__ZSt18_Rb_tree_incrementPKSt18_Rb_tree_node_base": func(c *cpu.CPU) { ret(c, rbIncrement(c, c.Arg(0))) },
		"__ZSt18_Rb_tree_decrementPSt18_Rb_tree_node_base":  func(c *cpu.CPU) { ret(c, rbDecrement(c, c.Arg(0))) },
		"__ZSt18_Rb_tree_decrementPKSt18_Rb_tree_node_base": func(c *cpu.CPU) { ret(c, rbDecrement(c, c.Arg(0))) },
		"__ZSt29_Rb_tree_insert_and_rebalancebPSt18_Rb_tree_node_baseS0_RS_": func(c *cpu.CPU) {
			rbInsertAndRebalance(c, c.Arg(0)&0xff != 0, c.Arg(1), c.Arg(2), c.Arg(3))
			c.Ret()
		},
		"__ZSt28_Rb_tree_rebalance_for_erasePSt18_Rb_tree_node_baseRS_": func(c *cpu.CPU) {
			ret(c, rbRebalanceForErase(c, c.Arg(0), c.Arg(1)))
		},

		// Runtime support.
		"__ZNSt8ios_base4InitC1Ev": retVoid, "__ZNSt8ios_base4InitD1Ev": retVoid,
		"__ZNSt9bad_allocD1Ev": retVoid, "__ZNSt9bad_allocD0Ev": retVoid, "__ZNSt9exceptionD2Ev": retVoid,
		"__ZNSt9exceptionD1Ev": retVoid, "__ZSt15set_new_handlerPFvvE": ret0, "__ZSt18uncaught_exceptionv": ret0,
		"__ZSt17__throw_bad_allocv":      m.cxxFatal("std::bad_alloc"),
		"__ZSt19__throw_logic_errorPKc":  m.cxxFatal("std::logic_error"),
		"__ZSt20__throw_length_errorPKc": m.cxxFatal("std::length_error"),
		"__ZSt20__throw_out_of_rangePKc": m.cxxFatal("std::out_of_range"),
		"__ZSt9terminatev":               m.cxxFatal("std::terminate"),
		"___cxa_pure_virtual":            m.cxxFatal("pure virtual call"),
		"___cxa_call_unexpected":         m.cxxFatal("unexpected exception"),
		"___cxa_allocate_exception":      func(c *cpu.CPU) { ret(c, c.Malloc(c.Arg(0)+64)+64) },
		"___cxa_free_exception":          func(c *cpu.CPU) { c.Ret() },
		"___cxa_throw":                   m.cxaThrow,
		"___cxa_rethrow":                 m.cxxFatal("rethrow"),
		"___cxa_begin_catch":             func(c *cpu.CPU) { ret(c, c.Arg(0)) },
		"___cxa_end_catch":               retVoid,
		"___cxa_get_exception_ptr":       func(c *cpu.CPU) { ret(c, c.Arg(0)) },
		"__Unwind_Resume":                m.cxxFatal("_Unwind_Resume (exception unwinding)"),
		"__Unwind_Resume_or_Rethrow":     m.cxxFatal("_Unwind_Resume_or_Rethrow"),
		"___dynamic_cast":                m.dynamicCast,
		"__ZNKSt9type_info4nameEv":       func(c *cpu.CPU) { ret(c, c.Read32(c.Arg(0)+4)) },
	}
	for k, v := range str {
		m.shims[k] = v
	}
}

func (m *Machine) cxxFatal(what string) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		msg := ""
		if strings.HasPrefix(what, "std::") && c.Arg(0) != 0 && c.Arg(0) < cpu.ArenaSize {
			msg = ": " + c.ReadCString(c.Arg(0))
		}
		m.logf("[c++] %s%s; backtrace:\n  %s", what, msg, strings.Join(m.Backtrace(24), "\n  "))
		c.Halted = true
		c.HaltReason = "C++ runtime: " + what
	}
}

func (m *Machine) cxaThrow(c *cpu.CPU) {
	ti := c.Arg(1)
	name := ""
	if ti != 0 {
		name = c.ReadCString(c.Read32(ti + 4))
	}
	m.logf("[c++] exception thrown (type %s); backtrace:\n  %s", name, strings.Join(m.Backtrace(24), "\n  "))
	c.Halted = true
	c.HaltReason = "C++ exception (unwinding not implemented): " + name
}

// typeName returns a type_info's name (a leading '*' marks a name not to be merged).
func (m *Machine) typeName(ti uint32) string {
	return strings.TrimPrefix(m.C.ReadCString(m.C.Read32(ti+4)), "*")
}

// findBase looks for a base of type dst (by name: each image has its own type_info objects) in
// the object of type ti at address obj; it returns the base's address.
func (m *Machine) findBase(ti, obj uint32, dst string, depth int) (uint32, bool) {
	c := m.C
	if depth > 32 {
		return 0, false
	}
	if m.typeName(ti) == dst {
		return obj, true
	}
	switch m.cxx.vtables[c.Read32(ti)] {
	case "__si_class_type_info":
		return m.findBase(c.Read32(ti+8), obj, dst, depth+1)
	case "__vmi_class_type_info":
		n := c.Read32(ti + 12)
		for i := uint32(0); i < n && i < 64; i++ {
			base, flags := c.Read32(ti+16+8*i), int32(c.Read32(ti+20+8*i))
			off := flags >> 8
			if flags&1 != 0 { // virtual base: its offset is in the vtable
				off = int32(c.Read32(uint32(int32(c.Read32(obj)) + off)))
			}
			if flags&2 == 0 { // not public
				continue
			}
			if a, ok := m.findBase(base, uint32(int32(obj)+off), dst, depth+1); ok {
				return a, true
			}
		}
	}
	return 0, false
}

// __dynamic_cast(src, src_type, dst_type, src2dst): find the complete object, then dst in it.
func (m *Machine) dynamicCast(c *cpu.CPU) {
	src, dst := c.Arg(0), c.Arg(2)
	if src == 0 {
		ret(c, 0)
		return
	}
	vt := c.Read32(src)
	whole := uint32(int32(src) + int32(c.Read32(vt-8)))
	wholeType := c.Read32(vt - 4)
	if a, ok := m.findBase(wholeType, whole, m.typeName(dst), 0); ok {
		ret(c, a)
		return
	}
	ret(c, 0)
}

// Red-black tree helpers (libstdc++'s tree.cc). Node: color (0 red, 1 black), parent, left, right.

const (
	rbRed   = 0
	rbBlack = 1
)

func color(c *cpu.CPU, x uint32) uint32  { return c.Read32(x) }
func parent(c *cpu.CPU, x uint32) uint32 { return c.Read32(x + 4) }
func left(c *cpu.CPU, x uint32) uint32   { return c.Read32(x + 8) }
func right(c *cpu.CPU, x uint32) uint32  { return c.Read32(x + 12) }
func setColor(c *cpu.CPU, x, v uint32)   { c.Write32(x, v) }
func setParent(c *cpu.CPU, x, v uint32)  { c.Write32(x+4, v) }
func setLeft(c *cpu.CPU, x, v uint32)    { c.Write32(x+8, v) }
func setRight(c *cpu.CPU, x, v uint32)   { c.Write32(x+12, v) }

func rbIncrement(c *cpu.CPU, x uint32) uint32 {
	if right(c, x) != 0 {
		x = right(c, x)
		for left(c, x) != 0 {
			x = left(c, x)
		}
		return x
	}
	y := parent(c, x)
	for x == right(c, y) {
		x = y
		y = parent(c, y)
	}
	if right(c, x) != y {
		x = y
	}
	return x
}

func rbDecrement(c *cpu.CPU, x uint32) uint32 {
	if color(c, x) == rbRed && parent(c, parent(c, x)) == x {
		return right(c, x) // x is the header: the rightmost node
	}
	if left(c, x) != 0 {
		y := left(c, x)
		for right(c, y) != 0 {
			y = right(c, y)
		}
		return y
	}
	y := parent(c, x)
	for x == left(c, y) {
		x = y
		y = parent(c, y)
	}
	return y
}

func rbRotateLeft(c *cpu.CPU, x, rootp uint32) {
	y := right(c, x)
	setRight(c, x, left(c, y))
	if left(c, y) != 0 {
		setParent(c, left(c, y), x)
	}
	setParent(c, y, parent(c, x))
	switch {
	case x == c.Read32(rootp):
		c.Write32(rootp, y)
	case x == left(c, parent(c, x)):
		setLeft(c, parent(c, x), y)
	default:
		setRight(c, parent(c, x), y)
	}
	setLeft(c, y, x)
	setParent(c, x, y)
}

func rbRotateRight(c *cpu.CPU, x, rootp uint32) {
	y := left(c, x)
	setLeft(c, x, right(c, y))
	if right(c, y) != 0 {
		setParent(c, right(c, y), x)
	}
	setParent(c, y, parent(c, x))
	switch {
	case x == c.Read32(rootp):
		c.Write32(rootp, y)
	case x == right(c, parent(c, x)):
		setRight(c, parent(c, x), y)
	default:
		setLeft(c, parent(c, x), y)
	}
	setRight(c, y, x)
	setParent(c, x, y)
}

// rbInsertAndRebalance(insert_left, x, p, header).
func rbInsertAndRebalance(c *cpu.CPU, insertLeft bool, x, p, header uint32) {
	rootp := header + 4
	setParent(c, x, p)
	setLeft(c, x, 0)
	setRight(c, x, 0)
	setColor(c, x, rbRed)
	if insertLeft {
		setLeft(c, p, x) // also makes leftmost = x when p == header
		if p == header {
			setParent(c, header, x)
			setRight(c, header, x)
		} else if p == left(c, header) {
			setLeft(c, header, x)
		}
	} else {
		setRight(c, p, x)
		if p == right(c, header) {
			setRight(c, header, x)
		}
	}
	for x != c.Read32(rootp) && color(c, parent(c, x)) == rbRed {
		xpp := parent(c, parent(c, x))
		if parent(c, x) == left(c, xpp) {
			y := right(c, xpp)
			if y != 0 && color(c, y) == rbRed {
				setColor(c, parent(c, x), rbBlack)
				setColor(c, y, rbBlack)
				setColor(c, xpp, rbRed)
				x = xpp
			} else {
				if x == right(c, parent(c, x)) {
					x = parent(c, x)
					rbRotateLeft(c, x, rootp)
				}
				setColor(c, parent(c, x), rbBlack)
				setColor(c, xpp, rbRed)
				rbRotateRight(c, xpp, rootp)
			}
		} else {
			y := left(c, xpp)
			if y != 0 && color(c, y) == rbRed {
				setColor(c, parent(c, x), rbBlack)
				setColor(c, y, rbBlack)
				setColor(c, xpp, rbRed)
				x = xpp
			} else {
				if x == left(c, parent(c, x)) {
					x = parent(c, x)
					rbRotateRight(c, x, rootp)
				}
				setColor(c, parent(c, x), rbBlack)
				setColor(c, xpp, rbRed)
				rbRotateLeft(c, xpp, rootp)
			}
		}
	}
	setColor(c, c.Read32(rootp), rbBlack)
}

func rbRebalanceForErase(c *cpu.CPU, z, header uint32) uint32 {
	rootp, leftmostp, rightmostp := header+4, header+8, header+12
	y := z
	var x, xParent uint32
	if left(c, y) == 0 {
		x = right(c, y)
	} else if right(c, y) == 0 {
		x = left(c, y)
	} else {
		y = right(c, y)
		for left(c, y) != 0 {
			y = left(c, y)
		}
		x = right(c, y)
	}
	if y != z {
		setParent(c, left(c, z), y)
		setLeft(c, y, left(c, z))
		if y != right(c, z) {
			xParent = parent(c, y)
			if x != 0 {
				setParent(c, x, parent(c, y))
			}
			setLeft(c, parent(c, y), x)
			setRight(c, y, right(c, z))
			setParent(c, right(c, z), y)
		} else {
			xParent = y
		}
		switch {
		case c.Read32(rootp) == z:
			c.Write32(rootp, y)
		case left(c, parent(c, z)) == z:
			setLeft(c, parent(c, z), y)
		default:
			setRight(c, parent(c, z), y)
		}
		setParent(c, y, parent(c, z))
		yc, zc := color(c, y), color(c, z)
		setColor(c, y, zc)
		setColor(c, z, yc)
		y = z
	} else {
		xParent = parent(c, y)
		if x != 0 {
			setParent(c, x, parent(c, y))
		}
		switch {
		case c.Read32(rootp) == z:
			c.Write32(rootp, x)
		case left(c, parent(c, z)) == z:
			setLeft(c, parent(c, z), x)
		default:
			setRight(c, parent(c, z), x)
		}
		if c.Read32(leftmostp) == z {
			if right(c, z) == 0 {
				c.Write32(leftmostp, parent(c, z))
			} else {
				m := x
				for left(c, m) != 0 {
					m = left(c, m)
				}
				c.Write32(leftmostp, m)
			}
		}
		if c.Read32(rightmostp) == z {
			if left(c, z) == 0 {
				c.Write32(rightmostp, parent(c, z))
			} else {
				m := x
				for right(c, m) != 0 {
					m = right(c, m)
				}
				c.Write32(rightmostp, m)
			}
		}
	}
	if color(c, y) != rbRed {
		for x != c.Read32(rootp) && (x == 0 || color(c, x) == rbBlack) {
			if x == left(c, xParent) {
				w := right(c, xParent)
				if color(c, w) == rbRed {
					setColor(c, w, rbBlack)
					setColor(c, xParent, rbRed)
					rbRotateLeft(c, xParent, rootp)
					w = right(c, xParent)
				}
				if (left(c, w) == 0 || color(c, left(c, w)) == rbBlack) && (right(c, w) == 0 || color(c, right(c, w)) == rbBlack) {
					setColor(c, w, rbRed)
					x = xParent
					xParent = parent(c, xParent)
				} else {
					if right(c, w) == 0 || color(c, right(c, w)) == rbBlack {
						setColor(c, left(c, w), rbBlack)
						setColor(c, w, rbRed)
						rbRotateRight(c, w, rootp)
						w = right(c, xParent)
					}
					setColor(c, w, color(c, xParent))
					setColor(c, xParent, rbBlack)
					if right(c, w) != 0 {
						setColor(c, right(c, w), rbBlack)
					}
					rbRotateLeft(c, xParent, rootp)
					break
				}
			} else {
				w := left(c, xParent)
				if color(c, w) == rbRed {
					setColor(c, w, rbBlack)
					setColor(c, xParent, rbRed)
					rbRotateRight(c, xParent, rootp)
					w = left(c, xParent)
				}
				if (right(c, w) == 0 || color(c, right(c, w)) == rbBlack) && (left(c, w) == 0 || color(c, left(c, w)) == rbBlack) {
					setColor(c, w, rbRed)
					x = xParent
					xParent = parent(c, xParent)
				} else {
					if left(c, w) == 0 || color(c, left(c, w)) == rbBlack {
						setColor(c, right(c, w), rbBlack)
						setColor(c, w, rbRed)
						rbRotateLeft(c, w, rootp)
						w = left(c, xParent)
					}
					setColor(c, w, color(c, xParent))
					setColor(c, xParent, rbBlack)
					if left(c, w) != 0 {
						setColor(c, left(c, w), rbBlack)
					}
					rbRotateRight(c, xParent, rootp)
					break
				}
			}
		}
		if x != 0 {
			setColor(c, x, rbBlack)
		}
	}
	return y
}

package source

import (
	"path/filepath"

	"catalina32/pkg/cpu"
	"catalina32/pkg/dyld"
)

// dlopen/dlsym over the linker. A handle is the image's mach header address; libraries we
// don't load (Skip) get a handle whose symbols are host stubs.

const (
	rtldNext    = 0xffffffff
	rtldDefault = 0xfffffffe
	rtldSelf    = 0xfffffffd
	rtldMain    = 0xfffffffb
)

type skippedLib struct{ name string }

func (m *Machine) handleOf(img *dyld.Image) uint32 {
	lo, _ := img.Span()
	return lo + img.Slide
}

func (m *Machine) imageOf(h uint32) *dyld.Image {
	for _, img := range m.L.Images {
		if m.handleOf(img) == h {
			return img
		}
	}
	return nil
}

func (m *Machine) dlopen(c *cpu.CPU) {
	name := ""
	if c.Arg(0) != 0 {
		name = c.ReadCString(c.Arg(0))
	}
	if name == "" {
		ret(c, m.handleOf(m.L.Main))
		return
	}
	if m.Skip[filepath.Base(name)] {
		key := "dl:" + filepath.Base(name)
		h, ok := m.sys[key]
		if !ok {
			h = m.data(16)
			m.sys[key] = h
			m.sysName[h] = filepath.Base(name)
		}
		m.logf("[dlopen] %s: provided by the host", name)
		ret(c, h)
		return
	}
	img, err := m.L.Open(name, nil)
	if err != nil {
		m.dlerr = err.Error()
		m.logf("[dlopen] %s: %v", name, err)
		ret(c, 0)
		return
	}
	h := m.handleOf(img)
	m.logf("[dlopen] %s", img.Name())
	ret(c, h)
	if err := m.RunInits(); err != nil {
		m.logf("[dlopen] %s: %v", img.Name(), err)
	}
	c.Regs[cpu.RegEAX] = h
}

func (m *Machine) dlsym(c *cpu.CPU) {
	h, name := c.Arg(0), "_"+c.ReadCString(c.Arg(1))
	if lib, ok := m.sysName[h]; ok && m.Skip[lib] {
		ret(c, m.system(lib, name))
		return
	}
	var img *dyld.Image
	switch h {
	case rtldDefault, rtldNext, rtldSelf:
	case rtldMain:
		img = m.L.Main
	default:
		if img = m.imageOf(h); img == nil {
			m.dlerr = "invalid handle"
			ret(c, 0)
			return
		}
	}
	a, ok := m.L.Symbol(img, name)
	if !ok {
		m.dlerr = "symbol not found: " + name
		m.logf("[dlsym] %s: not found", name)
		ret(c, 0)
		return
	}
	ret(c, a)
}

func (m *Machine) dlerror(c *cpu.CPU) {
	if m.dlerr == "" {
		ret(c, 0)
		return
	}
	a, ok := m.sys["dlerror"]
	if !ok {
		a = m.data(256)
		m.sys["dlerror"] = a
	}
	s := m.dlerr
	if len(s) > 255 {
		s = s[:255]
	}
	c.WriteCString(a, s)
	m.dlerr = ""
	ret(c, a)
}

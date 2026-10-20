package source

import (
	"catalina32/pkg/cpu"
)

// Small parts of Apple frameworks the game touches early: CoreFoundation strings, the
// Objective-C runtime's selectors.

func (m *Machine) registerFrameworkShims() {
	for k, v := range map[string]cpu.ThunkFunc{
		"_CFStringCreateWithCString": m.cfString,
		"_CFStringGetCString":        m.cfGetCString,
		"_CFStringGetLength":         func(c *cpu.CPU) { ret(c, uint32(len(m.cfStrings[c.Arg(0)]))) },
		"_CFRelease":                 retVoid,
		"_CFRetain":                  func(c *cpu.CPU) { ret(c, c.Arg(0)) },
		"_sel_registerName":          m.selRegister,
		"_sel_getUid":                m.selRegister,
		"_objc_msgSend": func(c *cpu.CPU) { // no Objective-C classes here: messages to nil answer 0
			sel := c.Arg(1)
			m.C.ReportOnce("objc:"+c.ReadCString(sel), "[objc] -%s from %s: answered 0", c.ReadCString(sel), m.where(c.Read32(c.Regs[cpu.RegESP])))
			c.Regs[cpu.RegEDX] = 0
			ret(c, 0)
		},
		// One display, ID 1, driven by renderer mask bit 0.
		"_CGOpenGLDisplayMaskToDisplayID": func(c *cpu.CPU) {
			if c.Arg(0)&1 != 0 {
				ret(c, 1)
				return
			}
			ret(c, 0)
		},
		"_CGDisplayIsActive":   retN(1),
		"_Gestalt":             m.gestalt,
		"_CGMainDisplayID":     retN(1),
		"_CGDisplayPixelsWide": retN(desktopW),
		"_CGDisplayPixelsHigh": retN(desktopH),
	} {
		m.shims[k] = v
	}
}

func (m *Machine) cfString(c *cpu.CPU) {
	s := c.ReadCString(c.Arg(1))
	p := c.Malloc(16)
	if m.cfStrings == nil {
		m.cfStrings = map[uint32]string{}
	}
	m.cfStrings[p] = s
	ret(c, p)
}

func (m *Machine) cfGetCString(c *cpu.CPU) {
	s, ok := m.cfStrings[c.Arg(0)]
	if !ok || uint32(len(s))+1 > c.Arg(2) {
		ret(c, 0)
		return
	}
	c.WriteCString(c.Arg(1), s)
	ret(c, 1)
}

func (m *Machine) selRegister(c *cpu.CPU) {
	name := c.ReadCString(c.Arg(0))
	key := "sel:" + name
	a, ok := m.sys[key]
	if !ok {
		a = m.data(uint32(len(name)) + 1)
		c.WriteCString(a, name)
		m.sys[key] = a
	}
	ret(c, a)
}

// Gestalt answers the system version as Mojave, the last macOS that ran 32-bit apps.
func (m *Machine) gestalt(c *cpu.CPU) {
	v, ok := map[uint32]uint32{
		0x73797376: 0x1099, // 'sysv' (capped at 10.9.9 since 10.10)
		0x73797331: 10,     // 'sys1'
		0x73797332: 14,     // 'sys2'
		0x73797333: 6,      // 'sys3'
	}[c.Arg(0)]
	if !ok {
		ret(c, 0xffffea51) // gestaltUndefSelectorErr
		return
	}
	c.Write32(c.Arg(1), v)
	ret(c, 0)
}

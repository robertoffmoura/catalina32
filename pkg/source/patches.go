package source

import (
	"catalina32/pkg/cpu"
	"catalina32/pkg/dyld"
)

// Game fixes (experimental; per-game code belongs in pkg/games once Source games have a family).
//
// Portal's Mac build of Nov 25 2024 (the anniversary update, made after 32-bit Macs were gone)
// can't create fonts: CFontManager was compiled against a newer font interface (IVGuiFontImpl,
// methods at vtable offsets up to 0x48) than the only Mac font class (COSXFont: 12 methods, the
// old layout, never instantiated), and CreateOrFindWin32Font compiled down to "append a NULL,
// return NULL" — the next lookup then calls a method on that NULL. Until fonts are provided by
// the host, the replacement fails cleanly: no font, no NULL left behind (text doesn't show).

func (m *Machine) applyPatches(img *dyld.Image) {
	if img.Name() != "vguimatsurface.dylib" {
		return
	}
	find, ok := m.localSymbol(img, "__ZN12CFontManager21CreateOrFindWin32FontEPKciiiii")
	if !ok {
		return
	}
	m.logf("[patch] vguimatsurface: CreateOrFindWin32Font fails cleanly (this build can't make fonts)")
	m.C.RegisterIntercept(find, "CreateOrFindWin32Font (patched)", func(c *cpu.CPU) {
		m.C.ReportOnce("font:"+c.ReadCString(c.Arg(1)), "[patch] no font %q", c.ReadCString(c.Arg(1)))
		ret(c, 0)
	})
}

// localSymbol finds any of names (local or exported) in img.
func (m *Machine) localSymbol(img *dyld.Image, names ...string) (uint32, bool) {
	var at uint32
	img.ForEachSymbol(func(name string, addr uint32) {
		for _, n := range names {
			if name == n && at == 0 {
				at = addr
			}
		}
	})
	return at, at != 0
}

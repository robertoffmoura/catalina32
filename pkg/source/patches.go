package source

import (
	"bytes"
	"strings"

	"catalina32/pkg/cpu"
	"catalina32/pkg/dyld"
)

// Game fixes (experimental; per-game code belongs in pkg/games once Source games have a family).
//
// Fonts. vguimatsurface draws text with font objects it gets from
// CFontManager::CreateOrFindWin32Font. On the Mac they were ATSUI fonts (COSXFont), and ATSUI
// only ever existed for 32-bit apps; and in Portal's Mac build of Nov 25 2024 (the anniversary
// update, made after 32-bit Macs were gone) that function can't make a font at all: CFontManager
// was compiled against a newer font interface (IVGuiFontImpl) than COSXFont implements, and the
// function compiled down to "append a NULL, return NULL". It is replaced: fonts come from the
// host (hostfont.go), with the IVGuiFontImpl layout. (Builds with the old layout, which call
// IsEqualTo at vtable offset 8 instead of 0x10, are left alone for now.)

func (m *Machine) applyPatches(img *dyld.Image) {
	if img.Name() != "vguimatsurface.dylib" {
		return
	}
	find, ok := m.localSymbol(img, "__ZN12CFontManager21CreateOrFindWin32FontEPKciiiii")
	if !ok {
		return
	}
	if !bytes.Contains(m.C.Bytes(find, 0x80), []byte{0xff, 0x51, 0x10}) {
		m.logf("[patch] vguimatsurface: an older font interface; fonts not replaced")
		return
	}
	m.logf("[patch] vguimatsurface: fonts drawn by catalina32")
	m.C.RegisterIntercept(find, "CreateOrFindWin32Font (host fonts)", func(c *cpu.CPU) {
		name := c.ReadCString(c.Arg(1))
		tall, weight, blur, scan, flags := int32(c.Arg(2)), int32(c.Arg(3)), int32(c.Arg(4)), int32(c.Arg(5)), int32(c.Arg(6))
		for _, f := range m.hostFonts.fonts {
			if strings.EqualFold(f.name, name) && f.tall == tall && f.weight == weight && f.blur == blur && f.scanlines == scan && f.flags == flags {
				ret(c, f.obj)
				return
			}
		}
		ret(c, m.newHostFont(name, tall, weight, blur, scan, flags).obj)
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

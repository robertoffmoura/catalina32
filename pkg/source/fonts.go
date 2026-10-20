package source

import (
	"math"

	"catalina32/pkg/cpu"
)

// Fonts: ATSUI (Apple's 32-bit-only text API) and the CoreGraphics bitmap contexts vgui draws
// glyphs into. For now fonts exist with plausible metrics and blank glyphs; real glyphs come
// from rasterising the Mac's system fonts in Go (docs/PORTAL.md).

type fontState struct {
	next      uint32
	styleSize map[uint32]float64 // ATSUStyle -> point size
	ctxData   map[uint32]uint32  // CGContext -> pixels
}

func (m *Machine) registerFontShims() {
	f := &m.fonts
	f.next, f.styleSize, f.ctxData = 0x100, map[uint32]float64{}, map[uint32]uint32{}
	handle := func() uint32 { f.next++; return f.next }
	for k, v := range map[string]cpu.ThunkFunc{
		"_FSPathMakeRef":                    ret0,
		"_ATSFontActivateFromFileReference": ret0,
		"_ATSFontFindFromName":              func(c *cpu.CPU) { ret(c, handle()) },
		"_ATSFontFindFromPostScriptName":    func(c *cpu.CPU) { ret(c, handle()) },
		"_ATSFontGetHorizontalMetrics": func(c *cpu.CPU) { // (font, options, ATSFontMetrics*), in ems
			p := c.Arg(2)
			c.Write32(p, 0)
			for i, v := range []float32{0.9, -0.21, 0, 0.5, 1, 0, 0, 0.1, 0.1, 0.7, 0.5, 0, -0.1, 0.05} {
				c.WriteF32(p+4+uint32(4*i), v)
			}
			ret(c, 0)
		},
		"_ATSUCreateStyle": func(c *cpu.CPU) {
			h := handle()
			f.styleSize[h] = 12
			c.Write32(c.Arg(0), h)
			ret(c, 0)
		},
		"_ATSUSetAttributes": func(c *cpu.CPU) { // (style, count, tags[], sizes[], values[])
			style, n, tags, vals := c.Arg(0), c.Arg(1), c.Arg(2), c.Arg(4)
			for i := uint32(0); i < n; i++ {
				if c.Read32(tags+4*i) == 0x73697a65 { // 'size': Fixed 16.16
					f.styleSize[style] = float64(int32(c.Read32(c.Read32(vals+4*i)))) / 65536
				}
			}
			ret(c, 0)
		},
		"_ATSUDisposeStyle":                    ret0,
		"_ATSUSetTransientFontMatching":        ret0,
		"_ATSUSetLayoutControls":               ret0,
		"_ATSUDisposeTextLayout":               ret0,
		"_ATSUDrawText":                        ret0,
		"_ATSUDirectReleaseLayoutDataArrayPtr": ret0,
		"_ATSUCreateTextLayoutWithTextPtr": func(c *cpu.CPU) { // (text, offset, length, total, runs, runLengths, styles, &layout)
			c.Write32(c.Arg(7), handle())
			ret(c, 0)
		},
		"_ATSUDirectGetLayoutDataArrayPtrFromTextLayout": func(c *cpu.CPU) { // (layout, offset, selector, &array, &count)
			a := c.Malloc(64)
			clear(c.WBytes(a, 64))
			c.Write32(c.Arg(3), a)
			c.Write32(c.Arg(4), 2)
			ret(c, 0)
		},
		"_ATSUGlyphGetScreenMetrics": func(c *cpu.CPU) { // (style, n, glyphs, delta, aa, aaSwitch, metrics[])
			size := f.styleSize[c.Arg(0)]
			for i := uint32(0); i < c.Arg(1); i++ {
				p := c.Arg(6) + 40*i
				w := float32(math.Ceil(size * 0.55))
				c.WriteF32(p, w)   // deviceAdvance.x
				c.WriteF32(p+4, 0) // .y
				c.WriteF32(p+8, 0) // topLeft.x
				c.WriteF32(p+12, float32(size*0.75))
				c.Write32(p+16, uint32(size)) // height
				c.Write32(p+20, uint32(w))    // width
				c.WriteF32(p+24, 0)
				c.WriteF32(p+28, 0)
				c.WriteF32(p+32, 0)
				c.WriteF32(p+36, 0)
			}
			ret(c, 0)
		},
		"_CGColorSpaceCreateDeviceRGB": func(c *cpu.CPU) { ret(c, handle()) },
		"_CGColorSpaceRelease":         retVoid,
		"_CGBitmapContextCreate": func(c *cpu.CPU) { // (data, w, h, bpc, bytesPerRow, space, info)
			h := handle()
			data := c.Arg(0)
			if data == 0 {
				data = c.Malloc(max(c.Arg(4)*c.Arg(2), 16))
			}
			f.ctxData[h] = data
			ret(c, h)
		},
		"_CGBitmapContextGetData": func(c *cpu.CPU) { ret(c, f.ctxData[c.Arg(0)]) },
		"_CGContextRelease":       retVoid, "_CGContextFlush": retVoid, "_CGContextClearRect": retVoid,
		"_CGContextSetAllowsAntialiasing": retVoid, "_CGContextSetShouldAntialias": retVoid,
		"_CGContextSetLineWidth": retVoid, "_CGContextSetRGBStrokeColor": retVoid, "_CGContextSetTextDrawingMode": retVoid,
	} {
		m.shims[k] = v
	}
}

package source

import (
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"catalina32/pkg/cpu"
)

// Host fonts: vgui's fonts drawn by catalina32. The game asks for a font by name (Verdana,
// Tahoma, its own HalfLife2...); the face comes from the fonts the game registered
// (AddCustomFontFile), its resource folders, or the Mac's font folders, and glyphs are
// rasterised in Go, with vgui's effects (outline, drop shadow, blur, scanlines).
//
// The font objects implement the font interface vguimatsurface calls (IVGuiFontImpl in the
// 2024 builds) through a vtable of host functions; patches.go hands them out.

// vgui font flags (ISurface::EFontFlags).
const (
	fontItalic     = 0x001
	fontUnderline  = 0x002
	fontAntialias  = 0x010
	fontBlur       = 0x020
	fontDropShadow = 0x080
	fontOutline    = 0x200
)

type hostFont struct {
	obj                                   uint32 // the guest object
	name, family                          string
	nameAddr, familyAddr                  uint32
	tall, weight, blur, scanlines, flags  int32
	face                                  font.Face
	ascent, height, maxWidth, pad, shadow int
	glyphs                                map[rune]*hostGlyph
}

type hostGlyph struct {
	a, b, c int     // ABC widths (with the effects' room)
	adv     float64 // advance
	mask    *image.Alpha
	x0, y0  int // where the mask goes in the cell
}

type fontFace struct {
	path   string
	index  int
	family string // lower case
	style  string // lower case subfamily
}

type fontRegistry struct {
	custom  []string // font files the game registered
	scanned bool
	faces   []fontFace
	fonts   []*hostFont
	byObj   map[uint32]*hostFont
	vtable  uint32
	parsed  map[string]*sfnt.Font
}

// fontDirs are the Mac's font folders.
func (m *Machine) fontDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{"/System/Library/Fonts", "/System/Library/Fonts/Supplemental", "/Library/Fonts",
		filepath.Join(home, "Library", "Fonts")}
}

// gameFontDirs are the game's resource folders (custom fonts, and the Linux build's
// substitutes for fonts a system may lack).
func (m *Machine) gameFontDirs() []string {
	var dirs []string
	for _, d := range m.modDirs() {
		dirs = append(dirs, filepath.Join(m.Dir, d, "resource"), filepath.Join(m.Dir, d, "resource", "linux_fonts"))
	}
	return dirs
}

// Substitutes for fonts a Mac may not have (the Linux build's choices, in the game folder).
var fontSubstitutes = map[string][]string{
	"verdana":         {"dejavu sans"},
	"tahoma":          {"verdana", "dejavu sans"},
	"trebuchet ms":    {"verdana", "dejavu sans"},
	"arial":           {"helvetica", "liberation sans"},
	"courier new":     {"courier", "liberation mono"},
	"lucida console":  {"monaco", "menlo", "liberation mono"},
	"times new roman": {"times", "dejavu serif"},
	"helvetica":       {"arial", "liberation sans"},
	"courier":         {"courier new", "liberation mono"},
}

func (r *fontRegistry) scan(m *Machine) {
	if r.scanned {
		return
	}
	r.scanned = true
	files := append([]string(nil), r.custom...)
	for _, dir := range append(m.fontDirs(), m.gameFontDirs()...) {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			switch strings.ToLower(filepath.Ext(e.Name())) {
			case ".ttf", ".otf", ".ttc", ".otc":
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	var buf sfnt.Buffer
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		coll, err := sfnt.ParseCollection(data)
		if err != nil {
			continue
		}
		for i := 0; i < coll.NumFonts(); i++ {
			ft, err := coll.Font(i)
			if err != nil {
				continue
			}
			fam, _ := ft.Name(&buf, sfnt.NameIDTypographicFamily)
			if fam == "" {
				fam, _ = ft.Name(&buf, sfnt.NameIDFamily)
			}
			style, _ := ft.Name(&buf, sfnt.NameIDTypographicSubfamily)
			if style == "" {
				style, _ = ft.Name(&buf, sfnt.NameIDSubfamily)
			}
			full, _ := ft.Name(&buf, sfnt.NameIDFull)
			r.faces = append(r.faces, fontFace{f, i, strings.ToLower(fam), strings.ToLower(style)})
			if full != "" && !strings.EqualFold(full, fam) {
				r.faces = append(r.faces, fontFace{f, i, strings.ToLower(full), "regular"})
			}
		}
	}
}

// find returns the face for a family, preferring the style closest to bold/italic.
func (r *fontRegistry) find(family string, bold, italic bool) (fontFace, bool) {
	family = strings.ToLower(strings.TrimSpace(family))
	best, bestScore := fontFace{}, -1
	for _, f := range r.faces {
		if f.family != family {
			continue
		}
		isBold := strings.Contains(f.style, "bold") || strings.Contains(f.style, "black") || strings.Contains(f.style, "heavy")
		isItalic := strings.Contains(f.style, "italic") || strings.Contains(f.style, "oblique")
		score := 0
		if isBold == bold {
			score += 2
		}
		if isItalic == italic {
			score += 4
		}
		if f.style == "regular" || f.style == "book" || f.style == "roman" {
			score++
		}
		if score > bestScore {
			best, bestScore = f, score
		}
	}
	return best, bestScore >= 0
}

func (r *fontRegistry) load(f fontFace) *sfnt.Font {
	key := f.path + "#" + string(rune('0'+f.index))
	if ft, ok := r.parsed[key]; ok {
		return ft
	}
	var ft *sfnt.Font
	if data, err := os.ReadFile(f.path); err == nil {
		if coll, err := sfnt.ParseCollection(data); err == nil {
			ft, _ = coll.Font(f.index)
		}
	}
	r.parsed[key] = ft
	return ft
}

var goRegular *sfnt.Font

// face returns the face for a vgui font request.
func (m *Machine) fontFace(name string, bold, italic bool) (*sfnt.Font, string) {
	r := &m.hostFonts
	r.scan(m)
	if f, ok := r.find(name, bold, italic); ok { // a family, or a face's full name ("Verdana Bold")
		if ft := r.load(f); ft != nil {
			return ft, name
		}
	}
	// "Verdana Bold Italic": the family Verdana, bold and italic.
	words := strings.Fields(name)
	for len(words) > 1 {
		w := strings.ToLower(words[len(words)-1])
		if w == "bold" || w == "black" || w == "heavy" {
			bold = true
		} else if w == "italic" || w == "oblique" {
			italic = true
		} else if w != "regular" && w != "medium" && w != "light" && w != "roman" {
			break
		}
		words = words[:len(words)-1]
	}
	family := strings.Join(words, " ")
	try := append([]string{family}, fontSubstitutes[strings.ToLower(family)]...)
	try = append(try, "dejavu sans")
	for _, n := range try {
		if f, ok := r.find(n, bold, italic); ok {
			if ft := r.load(f); ft != nil {
				return ft, n
			}
		}
	}
	if goRegular == nil {
		goRegular, _ = sfnt.Parse(goregular.TTF)
	}
	return goRegular, "Go"
}

// newHostFont makes the font object for a request.
func (m *Machine) newHostFont(name string, tall, weight, blur, scanlines, flags int32) *hostFont {
	c := m.C
	r := &m.hostFonts
	if r.vtable == 0 {
		m.fontVtable()
	}
	ft, used := m.fontFace(name, weight >= 600, flags&fontItalic != 0)
	hf := &hostFont{name: name, family: name, tall: tall, weight: weight, blur: blur, scanlines: scanlines,
		flags: flags, glyphs: map[rune]*hostGlyph{}}
	if used != name && !strings.EqualFold(used, name) {
		m.logf("[font] %q: using %s", name, used)
	}
	// Size the face so that its cell (ascent + descent) is the requested height, as on Windows.
	size := math.Max(float64(tall), 1)
	if f, err := opentype.NewFace(ft, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone}); err == nil {
		mt := f.Metrics()
		if cell := float64(mt.Ascent+mt.Descent) / 64; cell > 0 {
			size *= float64(tall) / cell
		}
		f.Close()
	}
	hf.face, _ = opentype.NewFace(ft, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if hf.face == nil {
		hf.face, _ = opentype.NewFace(goRegular, &opentype.FaceOptions{Size: size, DPI: 72})
	}
	mt := hf.face.Metrics()
	if flags&fontOutline != 0 {
		hf.pad = 1
	}
	if flags&fontBlur != 0 || blur > 0 {
		hf.pad += int(blur)
	}
	if flags&fontDropShadow != 0 {
		hf.shadow = 1
	}
	hf.ascent = mt.Ascent.Ceil() + hf.pad
	hf.height = mt.Ascent.Ceil() + mt.Descent.Ceil() + 2*hf.pad + hf.shadow
	for _, ch := range "WMm@" {
		g := m.glyph(hf, ch)
		hf.maxWidth = max(hf.maxWidth, g.a+g.b+g.c)
	}
	hf.obj = c.Malloc(16)
	c.Write32(hf.obj, r.vtable)
	c.Write32(hf.obj+4, uint32(len(r.fonts)))
	hf.nameAddr = m.data(uint32(len(name)) + 1)
	c.WriteCString(hf.nameAddr, name)
	hf.familyAddr = hf.nameAddr
	r.fonts = append(r.fonts, hf)
	r.byObj[hf.obj] = hf
	return hf
}

// glyph rasterises (once) a character.
func (m *Machine) glyph(hf *hostFont, ch rune) *hostGlyph {
	if g, ok := hf.glyphs[ch]; ok {
		return g
	}
	r := ch
	if _, ok := hf.face.GlyphAdvance(r); !ok && ch < 0x100 {
		r = 0xF000 + ch // symbol fonts (Marlett) keep their glyphs in the private area
	}
	g := &hostGlyph{}
	bounds, adv, ok := hf.face.GlyphBounds(r)
	g.adv = float64(adv) / 64
	if ok && !unicode.IsSpace(ch) && bounds.Max.X > bounds.Min.X {
		g.a = bounds.Min.X.Floor()
		g.b = bounds.Max.X.Ceil() - g.a
		dr, mask, mp, _, ok2 := hf.face.Glyph(fixed.Point26_6{X: fixed.I(-g.a), Y: fixed.I(hf.ascent - hf.pad)}, r)
		if ok2 {
			a := image.NewAlpha(image.Rect(0, 0, g.b, hf.height))
			for y := dr.Min.Y; y < dr.Max.Y; y++ {
				for x := dr.Min.X; x < dr.Max.X; x++ {
					if x >= 0 && x < g.b && y >= 0 && y < hf.height {
						_, _, _, al := mask.At(mp.X+x-dr.Min.X, mp.Y+y-dr.Min.Y).RGBA()
						a.Pix[y*a.Stride+x] = uint8(al >> 8)
					}
				}
			}
			g.mask = a
		}
	}
	g.a -= hf.pad
	if g.b > 0 || hf.pad > 0 {
		g.b += 2*hf.pad + hf.shadow
	}
	g.c = int(math.Round(g.adv)) - g.a - g.b
	hf.glyphs[ch] = g
	return g
}

// rgba draws a character into a wide x tall RGBA cell: white, coverage in alpha, then the effects.
func (m *Machine) rgba(hf *hostFont, ch rune, wide, tall int) []byte {
	out := make([]byte, wide*tall*4)
	g := m.glyph(hf, ch)
	alpha := make([]float64, wide*tall)
	if g.mask != nil {
		for y := 0; y < g.mask.Rect.Dy() && y+hf.pad < tall; y++ {
			for x := 0; x < g.mask.Rect.Dx() && x+hf.pad < wide; x++ {
				v := float64(g.mask.Pix[y*g.mask.Stride+x]) / 255
				if hf.flags&fontAntialias == 0 && hf.tall > 0 {
					v = math.Round(v)
				}
				alpha[(y)*wide+x+hf.pad] = v
			}
		}
	}
	if hf.flags&fontUnderline != 0 {
		y := hf.ascent + 1
		for x := 0; y < tall && x < wide; x++ {
			alpha[y*wide+x] = 1
		}
	}
	white := make([]float64, len(alpha)) // 1 where the glyph is, 0 for effect-only pixels (black)
	copy(white, alpha)
	if hf.blur > 0 {
		alpha = blurAlpha(alpha, wide, tall, int(hf.blur))
		copy(white, alpha)
	}
	if hf.flags&fontOutline != 0 {
		out2 := make([]float64, len(alpha))
		for y := 0; y < tall; y++ {
			for x := 0; x < wide; x++ {
				best := alpha[y*wide+x]
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if xx, yy := x+dx, y+dy; xx >= 0 && yy >= 0 && xx < wide && yy < tall {
							best = math.Max(best, alpha[yy*wide+xx])
						}
					}
				}
				out2[y*wide+x] = best
			}
		}
		alpha = out2
	}
	if hf.shadow > 0 {
		for y := tall - 1; y >= 1; y-- {
			for x := wide - 1; x >= 1; x-- {
				if s := alpha[(y-1)*wide+x-1]; s > alpha[y*wide+x] {
					alpha[y*wide+x] = s
				}
			}
		}
	}
	for i, a := range alpha {
		w := white[i]
		if a > 0 {
			w = math.Min(w/a, 1) // the glyph's share of this pixel; the rest is outline or shadow
		}
		v := uint8(255 * w)
		if hf.scanlines > 1 && (i/wide)%int(hf.scanlines) != 0 {
			a *= 0.7
		}
		out[4*i], out[4*i+1], out[4*i+2], out[4*i+3] = v, v, v, uint8(math.Round(255*a))
	}
	return out
}

func blurAlpha(a []float64, w, h, r int) []float64 {
	k := make([]float64, 2*r+1)
	sigma := float64(r) / 2
	sum := 0.0
	for i := range k {
		x := float64(i - r)
		k[i] = math.Exp(-x * x / (2 * sigma * sigma))
		sum += k[i]
	}
	for i := range k {
		k[i] /= sum
	}
	tmp := make([]float64, len(a))
	out := make([]float64, len(a))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s := 0.0
			for i, kv := range k {
				if xx := x + i - r; xx >= 0 && xx < w {
					s += a[y*w+xx] * kv
				}
			}
			tmp[y*w+x] = s
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s := 0.0
			for i, kv := range k {
				if yy := y + i - r; yy >= 0 && yy < h {
					s += tmp[yy*w+x] * kv
				}
			}
			out[y*w+x] = math.Min(s*1.5, 1)
		}
	}
	return out
}

// fontVtable builds the vtable of the host font objects (offsets in comments: see patches.go).
func (m *Machine) fontVtable() {
	r := &m.hostFonts
	self := func(c *cpu.CPU) *hostFont { return r.byObj[c.Arg(0)] }
	slots := []cpu.ThunkFunc{
		retVoid, // 0x00 destructor
		retVoid, // 0x04 deleting destructor (the fonts live as long as the game)
		retN(1), // 0x08 Create (fonts are made by CreateOrFindWin32Font)
		func(c *cpu.CPU) { // 0x0C GetCharRGBA(ch, wide, tall, rgba)
			hf := self(c)
			wide, tall, dst := int(int32(c.Arg(2))), int(int32(c.Arg(3))), c.Arg(4)
			if hf != nil && wide > 0 && tall > 0 && wide*tall < 1<<22 {
				copy(c.WBytes(dst, uint32(wide*tall*4)), m.rgba(hf, rune(c.Arg(1)), wide, tall))
			}
			c.Ret()
		},
		func(c *cpu.CPU) { // 0x10 IsEqualTo(name, tall, weight, blur, scanlines, flags)
			hf := self(c)
			eq := hf != nil && strings.EqualFold(c.ReadCString(c.Arg(1)), hf.name) && int32(c.Arg(2)) == hf.tall &&
				int32(c.Arg(3)) == hf.weight && int32(c.Arg(4)) == hf.blur && int32(c.Arg(5)) == hf.scanlines && int32(c.Arg(6)) == hf.flags
			ret(c, b2u(eq))
		},
		retN(1), // 0x14 IsValid
		func(c *cpu.CPU) { // 0x18 GetCharABCWidths(ch, &a, &b, &c, ...)
			if hf := self(c); hf != nil {
				g := m.glyph(hf, rune(c.Arg(1)))
				c.Write32(c.Arg(2), uint32(int32(g.a)))
				c.Write32(c.Arg(3), uint32(int32(g.b)))
				c.Write32(c.Arg(4), uint32(int32(g.c)))
			}
			c.Ret()
		},
		ret0, // 0x1C
		func(c *cpu.CPU) { ret(c, uint32(self(c).height)) },                // 0x20 GetHeight
		func(c *cpu.CPU) { ret(c, uint32(self(c).tall)) },                  // 0x24 GetHeightRequested
		func(c *cpu.CPU) { ret(c, uint32(self(c).ascent)) },                // 0x28 GetAscent
		func(c *cpu.CPU) { ret(c, uint32(self(c).maxWidth)) },              // 0x2C GetMaxCharWidth
		func(c *cpu.CPU) { ret(c, uint32(self(c).flags)) },                 // 0x30 GetFlags
		func(c *cpu.CPU) { ret(c, b2u(self(c).flags&fontUnderline != 0)) }, // 0x34 GetUnderlined
		func(c *cpu.CPU) { ret(c, self(c).nameAddr) },                      // 0x38 GetName
		func(c *cpu.CPU) { ret(c, self(c).familyAddr) },                    // 0x3C GetFamilyName
		func(c *cpu.CPU) { ret(c, uint32(self(c).weight)) },                // 0x40
		func(c *cpu.CPU) { // 0x44 GetKernedCharWidth(ch, before, after, &wide, &a, &c)
			hf := self(c)
			g := m.glyph(hf, rune(c.Arg(1)))
			wide := float64(g.a + g.b + g.c)
			if after := rune(c.Arg(3)); after != 0 {
				if k := hf.face.Kern(rune(c.Arg(1)), after); k != 0 {
					wide += float64(k) / 64
				}
			}
			c.WriteF32(c.Arg(4), float32(wide))
			c.WriteF32(c.Arg(5), float32(g.a))
			c.WriteF32(c.Arg(6), float32(g.c))
			c.Ret()
		},
		retN(1), // 0x48 the font covers every character (the amalgam takes it for the whole range)
	}
	r.vtable = m.data(uint32(4 * len(slots)))
	for i, fn := range slots {
		m.C.Write32(r.vtable+uint32(4*i), m.hostFunc(fmt.Sprintf("<font method %#x>", 4*i), fn))
	}
}

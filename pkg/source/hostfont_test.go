package source

import (
	"image"
	imgcolor "image/color"
	"image/png"
	"os"
	"testing"
)

// C32_SOURCE_DIR=... C32_FONT_PNG=out.png: draws sample text with the host fonts, as vgui would
// (glyph cells from GetCharRGBA placed by their ABC widths), white on grey.
func TestHostFontSample(t *testing.T) {
	dir, out := os.Getenv("C32_SOURCE_DIR"), os.Getenv("C32_FONT_PNG")
	if dir == "" || out == "" {
		t.Skip("C32_SOURCE_DIR and C32_FONT_PNG not set")
	}
	m, err := New(dir, nil, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.hostFonts.custom = append(m.hostFonts.custom, dir+"/hl2/resource/halflife2.ttf", dir+"/hl2/resource/marlett.ttf")
	type sample struct {
		name                      string
		tall, weight, blur, flags int32
		text                      string
	}
	samples := []sample{
		{"Verdana", 14, 400, 0, fontAntialias, "Verdana 14: New Game, Load Game, Options, Quit"},
		{"Tahoma", 24, 700, 0, fontAntialias | fontDropShadow, "Tahoma 24 bold, drop shadow: PORTAL"},
		{"Trebuchet MS", 32, 900, 0, fontAntialias | fontOutline, "Trebuchet 32 outline: Aperture"},
		{"Verdana", 28, 900, 4, fontAntialias | fontBlur, "Verdana 28 blur 4: GLOW"},
		{"HalfLife2", 40, 0, 0, fontAntialias, "abcdefghijklmnop"},
		{"Marlett", 20, 0, 0, fontAntialias, "abcdu468"},
	}
	img := image.NewRGBA(image.Rect(0, 0, 900, 320))
	for i := range img.Pix {
		img.Pix[i] = 60
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	y := 6
	for _, s := range samples {
		hf := m.newHostFont(s.name, s.tall, s.weight, s.blur, 0, s.flags)
		x := 6
		for _, ch := range s.text {
			g := m.glyph(hf, ch)
			w, h := g.b+2, hf.height+2
			if g.b > 0 {
				cell := m.rgba(hf, ch, w, h)
				for yy := 0; yy < h; yy++ {
					for xx := 0; xx < w; xx++ {
						p := cell[4*(yy*w+xx):]
						a := float64(p[3]) / 255
						px, py := x+g.a+xx, y+yy
						if !image.Pt(px, py).In(img.Rect) {
							continue
						}
						o := img.RGBAAt(px, py)
						mix := func(dst uint8, src uint8) uint8 { return uint8(float64(dst)*(1-a) + float64(src)*a) }
						img.SetRGBA(px, py, imgcolor.RGBA{mix(o.R, p[0]), mix(o.G, p[1]), mix(o.B, p[2]), 255})
					}
				}
			}
			x += g.a + g.b + g.c
		}
		y += hf.height + 8
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, img)
}

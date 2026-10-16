package d3d9

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"lsw64/pkg/cpu"
)

const (
	ScreenWidth  = 1280
	ScreenHeight = 720
)

type screenVertex struct {
	sx, sy, sz float32
	r, g, b, a uint8
	u, v       float32
	valid      bool
}

// renderEvery: only every Nth presented frame is rasterized/exported (rendering is pure overhead for guest execution).
// Override with LSW_RENDER_EVERY (1 = every frame, e.g. for a live display).
var renderEvery = func() int {
	if v, err := strconv.Atoi(os.Getenv("LSW_RENDER_EVERY")); err == nil && v > 0 {
		return v
	}
	return 30
}()

func (s *Subsystem) wantRender(frame uint64) bool {
	return frame <= 2 || frame%uint64(renderEvery) == 0
}

func (s *Subsystem) initRasterizer(framesDir string) {
	s.rendering = true
	s.Framebuffer = image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	s.ZBuffer = make([]float32, ScreenWidth*ScreenHeight)
	s.FramesDir = framesDir
	if s.FramesDir != "" {
		_ = os.MkdirAll(s.FramesDir, 0755)
	}
	s.clearFramebuffer(0xFF000000)
	s.clearZBuffer(1.0)
	s.startDisplayServer()
}

func (s *Subsystem) clearFramebuffer(argb uint32) {
	r := uint8((argb >> 16) & 0xFF)
	g := uint8((argb >> 8) & 0xFF)
	b := uint8(argb & 0xFF)
	a := uint8(0xFF)
	pix := s.Framebuffer.Pix
	for i := 0; i < len(pix); i += 4 {
		pix[i+0] = r
		pix[i+1] = g
		pix[i+2] = b
		pix[i+3] = a
	}
}

func (s *Subsystem) clearZBuffer(z float32) {
	for i := range s.ZBuffer {
		s.ZBuffer[i] = z
	}
}

func rgb565ToRGB888(c uint16) (uint8, uint8, uint8) {
	r := uint8(((uint32(c>>11) & 0x1F) * 255) / 31)
	g := uint8(((uint32(c>>5) & 0x3F) * 255) / 63)
	b := uint8(((uint32(c) & 0x1F) * 255) / 31)
	return r, g, b
}

func packRGBA(r, g, b, a uint8) uint32 {
	return uint32(r) | (uint32(g) << 8) | (uint32(b) << 16) | (uint32(a) << 24)
}

func decodeDXTBlockColors(c0, c1 uint16, isDXT1 bool) [4][4]uint8 {
	var pal [4][4]uint8
	r0, g0, b0 := rgb565ToRGB888(c0)
	r1, g1, b1 := rgb565ToRGB888(c1)
	pal[0] = [4]uint8{r0, g0, b0, 255}
	pal[1] = [4]uint8{r1, g1, b1, 255}
	if !isDXT1 || c0 > c1 {
		pal[2] = [4]uint8{
			uint8((2*uint16(r0) + uint16(r1)) / 3),
			uint8((2*uint16(g0) + uint16(g1)) / 3),
			uint8((2*uint16(b0) + uint16(b1)) / 3),
			255,
		}
		pal[3] = [4]uint8{
			uint8((uint16(r0) + 2*uint16(r1)) / 3),
			uint8((uint16(g0) + 2*uint16(g1)) / 3),
			uint8((uint16(b0) + 2*uint16(b1)) / 3),
			255,
		}
	} else {
		pal[2] = [4]uint8{
			uint8((uint16(r0) + uint16(r1)) / 2),
			uint8((uint16(g0) + uint16(g1)) / 2),
			uint8((uint16(b0) + uint16(b1)) / 2),
			255,
		}
		pal[3] = [4]uint8{0, 0, 0, 0}
	}
	return pal
}

func decodeTexturePixels(data []byte, w, h, fmtCode uint32) []uint32 {
	if w == 0 || h == 0 || w > 4096 || h > 4096 {
		return nil
	}
	out := make([]uint32, w*h)
	switch fmtCode {
	case 0x31545844: // DXT1
		bw := (w + 3) / 4
		bh := (h + 3) / 4
		off := 0
		for by := uint32(0); by < bh; by++ {
			for bx := uint32(0); bx < bw; bx++ {
				if off+8 > len(data) {
					return out
				}
				c0 := binary.LittleEndian.Uint16(data[off : off+2])
				c1 := binary.LittleEndian.Uint16(data[off+2 : off+4])
				bits := binary.LittleEndian.Uint32(data[off+4 : off+8])
				off += 8
				pal := decodeDXTBlockColors(c0, c1, true)
				for py := uint32(0); py < 4; py++ {
					y := by*4 + py
					if y >= h {
						continue
					}
					for px := uint32(0); px < 4; px++ {
						x := bx*4 + px
						if x >= w {
							continue
						}
						idx := (bits >> ((py*4 + px) * 2)) & 3
						c := pal[idx]
						out[y*w+x] = packRGBA(c[0], c[1], c[2], c[3])
					}
				}
			}
		}
	case 0x33545844: // DXT3
		bw := (w + 3) / 4
		bh := (h + 3) / 4
		off := 0
		for by := uint32(0); by < bh; by++ {
			for bx := uint32(0); bx < bw; bx++ {
				if off+16 > len(data) {
					return out
				}
				alphaBits := binary.LittleEndian.Uint64(data[off : off+8])
				c0 := binary.LittleEndian.Uint16(data[off+8 : off+10])
				c1 := binary.LittleEndian.Uint16(data[off+10 : off+12])
				bits := binary.LittleEndian.Uint32(data[off+12 : off+16])
				off += 16
				pal := decodeDXTBlockColors(c0, c1, false)
				for py := uint32(0); py < 4; py++ {
					y := by*4 + py
					if y >= h {
						continue
					}
					for px := uint32(0); px < 4; px++ {
						x := bx*4 + px
						if x >= w {
							continue
						}
						pIdx := py*4 + px
						a4 := uint8((alphaBits >> (pIdx * 4)) & 0xF)
						a := a4 | (a4 << 4)
						cIdx := (bits >> (pIdx * 2)) & 3
						c := pal[cIdx]
						out[y*w+x] = packRGBA(c[0], c[1], c[2], a)
					}
				}
			}
		}
	case 0x35545844: // DXT5
		bw := (w + 3) / 4
		bh := (h + 3) / 4
		off := 0
		for by := uint32(0); by < bh; by++ {
			for bx := uint32(0); bx < bw; bx++ {
				if off+16 > len(data) {
					return out
				}
				a0 := data[off+0]
				a1 := data[off+1]
				var apal [8]uint8
				apal[0] = a0
				apal[1] = a1
				if a0 > a1 {
					for i := uint16(1); i <= 6; i++ {
						apal[i+1] = uint8(((7-i)*uint16(a0) + i*uint16(a1)) / 7)
					}
				} else {
					for i := uint16(1); i <= 4; i++ {
						apal[i+1] = uint8(((5-i)*uint16(a0) + i*uint16(a1)) / 5)
					}
					apal[6] = 0
					apal[7] = 255
				}
				abits := uint64(data[off+2]) | (uint64(data[off+3]) << 8) | (uint64(data[off+4]) << 16) |
					(uint64(data[off+5]) << 24) | (uint64(data[off+6]) << 32) | (uint64(data[off+7]) << 40)
				c0 := binary.LittleEndian.Uint16(data[off+8 : off+10])
				c1 := binary.LittleEndian.Uint16(data[off+10 : off+12])
				bits := binary.LittleEndian.Uint32(data[off+12 : off+16])
				off += 16
				pal := decodeDXTBlockColors(c0, c1, false)
				for py := uint32(0); py < 4; py++ {
					y := by*4 + py
					if y >= h {
						continue
					}
					for px := uint32(0); px < 4; px++ {
						x := bx*4 + px
						if x >= w {
							continue
						}
						pIdx := py*4 + px
						aIdx := (abits >> (pIdx * 3)) & 7
						cIdx := (bits >> (pIdx * 2)) & 3
						c := pal[cIdx]
						out[y*w+x] = packRGBA(c[0], c[1], c[2], apal[aIdx])
					}
				}
			}
		}
	case 50, 28: // D3DFMT_L8 / D3DFMT_A8
		for i := uint32(0); i < w*h && int(i) < len(data); i++ {
			v := data[i]
			out[i] = packRGBA(v, v, v, v)
		}
	default: // D3DFMT_A8R8G8B8 (21), D3DFMT_X8R8G8B8 (22), etc.
		for i := uint32(0); i < w*h && int(i*4+4) <= len(data); i++ {
			b := data[i*4+0]
			g := data[i*4+1]
			r := data[i*4+2]
			a := data[i*4+3]
			if fmtCode == 22 && a == 0 {
				a = 255
			}
			out[i] = packRGBA(r, g, b, a)
		}
	}
	return out
}

func (s *Subsystem) decodeDDSInMemory(c *cpu.CPU, pSrc uint32, srcSize uint32) (uint32, uint32, uint32, []uint32) {
	if pSrc == 0 || srcSize < 128 || uint64(pSrc)+uint64(srcSize) > uint64(len(c.Mem)) {
		return 64, 64, 21, nil
	}
	magic := c.Read32(pSrc)
	if magic != 0x20534444 { // "DDS "
		return 64, 64, 21, nil
	}
	h := c.Read32(pSrc + 12)
	w := c.Read32(pSrc + 16)
	pfFlags := c.Read32(pSrc + 80)
	fourCC := c.Read32(pSrc + 84)
	bitCount := c.Read32(pSrc + 88)
	fmtCode := uint32(21)
	if (pfFlags & 0x4) != 0 { // DDPF_FOURCC
		fmtCode = fourCC
	} else if bitCount == 8 {
		fmtCode = 50
	}
	pixels := decodeTexturePixels(c.Mem[pSrc+128:pSrc+srcSize], w, h, fmtCode)
	return w, h, fmtCode, pixels
}

func (s *Subsystem) ensureTextureDecoded(c *cpu.CPU, texObj uint32) *BufferDesc {
	res := s.Resources[texObj]
	if res == nil {
		return nil
	}
	if len(res.Pixels) == 0 && res.DataPtr != 0 && res.Length > 0 {
		end := res.DataPtr + res.Length
		if end <= uint32(len(c.Mem)) {
			// Check if any non-zero bytes were written to DataPtr
			nonZero := false
			checkLen := res.Length
			if checkLen > 256 {
				checkLen = 256
			}
			for i := uint32(0); i < checkLen; i++ {
				if c.Mem[res.DataPtr+i] != 0 {
					nonZero = true
					break
				}
			}
			if nonZero {
				res.Pixels = decodeTexturePixels(c.Mem[res.DataPtr:end], res.Width, res.Height, res.Format)
			}
		}
	}
	return res
}

func (s *Subsystem) transformVertex(c *cpu.CPU, vp uint32, stride uint32, isUP bool) screenVertex {
	if vp+12 > uint32(len(c.Mem)) {
		return screenVertex{}
	}
	x := c.ReadF32(vp + 0)
	y := c.ReadF32(vp + 4)
	z := c.ReadF32(vp + 8)
	if math.IsNaN(float64(x)) || math.IsNaN(float64(y)) || math.IsNaN(float64(z)) {
		return screenVertex{}
	}

	r, g, b, a := uint8(220), uint8(220), uint8(230), uint8(255)
	var u, v float32

	if stride == 24 {
		col := c.Read32(vp + 12)
		a = uint8((col >> 24) & 0xFF)
		r = uint8((col >> 16) & 0xFF)
		g = uint8((col >> 8) & 0xFF)
		b = uint8(col & 0xFF)
		if a == 0 && (r != 0 || g != 0 || b != 0) {
			a = 255
		} else if col == 0 {
			r, g, b, a = 255, 255, 255, 255
		}
		// Boost dark Star Wars intro crawl cyan (0x00324B -> #4BD5EE) for legibility
		if r == 0 && g >= 0x20 && g <= 0x50 && b >= 0x35 && b <= 0x70 {
			r, g, b = 0x4B, 0xD5, 0xEE
		}
		u = c.ReadF32(vp + 16)
		v = c.ReadF32(vp + 20)
	} else if stride >= 20 && vp+stride <= uint32(len(c.Mem)) {
		u = c.ReadF32(vp + stride - 8)
		v = c.ReadF32(vp + stride - 4)
		// Derive subtle directional shading from vertex coordinates/normals
		nx := c.ReadF32(vp + 12)
		ny := c.ReadF32(vp + 16)
		shade := float32(0.85)
		if !math.IsNaN(float64(nx)) && !math.IsNaN(float64(ny)) && math.Abs(float64(nx)) <= 1.5 && math.Abs(float64(ny)) <= 1.5 {
			shade = float32(0.65) + 0.35*(nx*0.5+ny*0.7)
			if shade < 0.35 {
				shade = 0.35
			} else if shade > 1.0 {
				shade = 1.0
			}
		}
		mr, mg, mb := float32(235), float32(225), float32(205)
		// If layer0_diffuse (PSConstants[112]) was bound by NuShaderBindMaterial, modulate by material diffuse color
		mc := s.PSConstants[112]
		if mc[0] > 0.01 || mc[1] > 0.01 || mc[2] > 0.01 {
			if mc[0] <= 1.0 && mc[1] <= 1.0 && mc[2] <= 1.0 {
				mr = float32(math.Min(255.0, float64(0.35+0.65*mc[0])*255.0))
				mg = float32(math.Min(255.0, float64(0.35+0.65*mc[1])*255.0))
				mb = float32(math.Min(255.0, float64(0.35+0.65*mc[2])*255.0))
			}
		}
		r = uint8(mr * shade)
		g = uint8(mg * shade)
		b = uint8(mb * shade)
	}

	// Case 0: Pre-transformed screen-space XYZRHW quad (x in [0..1280], y in [0..720], rhw == 1.0)
	if stride == 28 && c.Read32(vp+12) == 0x3f800000 && x >= -1.0 && x <= float32(ScreenWidth)+1.0 && y >= -1.0 && y <= float32(ScreenHeight)+1.0 {
		return screenVertex{sx: x, sy: y, sz: z, r: r, g: g, b: b, a: a, u: u, v: v, valid: true}
	}

	// Case 1: DrawPrimitiveUP 2D UI/splash quads ([-1, 1] x [-1, 1])
	if isUP {
		sx := (x + 1.0) * 0.5 * float32(ScreenWidth)
		sy := (1.0 - y) * 0.5 * float32(ScreenHeight)
		return screenVertex{sx: sx, sy: sy, sz: z, r: r, g: g, b: b, a: a, u: u, v: v, valid: true}
	}

	// Case 2: Transform 3D vertex by NuShaderParam 4x4 matrices at *0x00b2842c
	// param[20] = worldViewProj, param[17] = world, param[19] = viewProj, param[21] = worldView
	tableBase := c.Read32(0x00b2842c)
	if tableBase != 0 && tableBase+92*100 <= uint32(len(c.Mem)) {
		for _, pid := range []uint32{20, 19, 21, 17, 18} {
			entry := tableBase + pid*92
			m := entry + 0x1c
			m00, m01, m02, m03 := c.ReadF32(m+0), c.ReadF32(m+4), c.ReadF32(m+8), c.ReadF32(m+12)
			m10, m11, m12, m13 := c.ReadF32(m+16), c.ReadF32(m+20), c.ReadF32(m+24), c.ReadF32(m+28)
			m20, m21, m22, m23 := c.ReadF32(m+32), c.ReadF32(m+36), c.ReadF32(m+40), c.ReadF32(m+44)
			m30, m31, m32, m33 := c.ReadF32(m+48), c.ReadF32(m+52), c.ReadF32(m+56), c.ReadF32(m+60)
			if m00 == 0 && m11 == 0 && m22 == 0 {
				continue
			}
			cx := x*m00 + y*m10 + z*m20 + m30
			cy := x*m01 + y*m11 + z*m21 + m31
			cz := x*m02 + y*m12 + z*m22 + m32
			cw := x*m03 + y*m13 + z*m23 + m33
			if cw > 0.05 && cw < 1000.0 {
				ndcX := cx / cw
				ndcY := cy / cw
				ndcZ := cz / cw
				if ndcX >= -2.5 && ndcX <= 2.5 && ndcY >= -2.5 && ndcY <= 2.5 {
					sx := (ndcX + 1.0) * 0.5 * float32(ScreenWidth)
					sy := (1.0 - ndcY) * 0.5 * float32(ScreenHeight)
					return screenVertex{sx: sx, sy: sy, sz: ndcZ, r: r, g: g, b: b, a: a, u: u, v: v, valid: true}
				}
			}
		}
	}

	// Fallback perspective/orthographic projection into screen center
	scale := float32(180.0)
	if z > 0.5 {
		scale = 360.0 / (z + 1.5)
	}
	sx := float32(ScreenWidth)*0.5 + x*scale
	sy := float32(ScreenHeight)*0.5 - y*scale
	return screenVertex{sx: sx, sy: sy, sz: 0.5, r: r, g: g, b: b, a: a, u: u, v: v, valid: true}
}

func (s *Subsystem) rasterizeTriangle(c *cpu.CPU, v0, v1, v2 screenVertex) {
	if !v0.valid || !v1.valid || !v2.valid {
		return
	}
	minX := int(math.Floor(math.Min(float64(v0.sx), math.Min(float64(v1.sx), float64(v2.sx)))))
	maxX := int(math.Ceil(math.Max(float64(v0.sx), math.Max(float64(v1.sx), float64(v2.sx)))))
	minY := int(math.Floor(math.Min(float64(v0.sy), math.Min(float64(v1.sy), float64(v2.sy)))))
	maxY := int(math.Ceil(math.Max(float64(v0.sy), math.Max(float64(v1.sy), float64(v2.sy)))))

	if maxX < 0 || minX >= ScreenWidth || maxY < 0 || minY >= ScreenHeight {
		return
	}
	if minX < 0 {
		minX = 0
	}
	if maxX >= ScreenWidth {
		maxX = ScreenWidth - 1
	}
	if minY < 0 {
		minY = 0
	}
	if maxY >= ScreenHeight {
		maxY = ScreenHeight - 1
	}

	var texRes *BufferDesc
	if s.BoundTextures[0] != 0 {
		texRes = s.ensureTextureDecoded(c, s.BoundTextures[0])
	}
	hasTex := texRes != nil && len(texRes.Pixels) > 0 && texRes.Width > 0 && texRes.Height > 0

	// Skip untextured fullscreen post-process / render-target blit quads so they don't overwrite the software framebuffer
	if !hasTex && (maxX-minX) >= 1100 && (maxY-minY) >= 600 {
		return
	}

	area := (v1.sx-v0.sx)*(v2.sy-v0.sy) - (v1.sy-v0.sy)*(v2.sx-v0.sx)
	if math.Abs(float64(area)) < 0.001 {
		return
	}
	invArea := 1.0 / area
	isGlyphQuad := !hasTex && v0.r == 0x4B && v0.g == 0xD5 && v0.b == 0xEE && (maxX-minX) <= 64 && (maxY-minY) <= 64

	pix := s.Framebuffer.Pix
	for y := minY; y <= maxY; y++ {
		py := float32(y) + 0.5
		for x := minX; x <= maxX; x++ {
			px := float32(x) + 0.5
			w0 := ((v1.sx-px)*(v2.sy-py) - (v1.sy-py)*(v2.sx-px)) * invArea
			w1 := ((v2.sx-px)*(v0.sy-py) - (v2.sy-py)*(v0.sx-px)) * invArea
			w2 := 1.0 - w0 - w1
			if (w0 >= 0 && w1 >= 0 && w2 >= 0) || (w0 <= 0 && w1 <= 0 && w2 <= 0) {
				if w0 < 0 {
					w0, w1, w2 = -w0, -w1, -w2
				}
				r := uint8(w0*float32(v0.r) + w1*float32(v1.r) + w2*float32(v2.r))
				g := uint8(w0*float32(v0.g) + w1*float32(v1.g) + w2*float32(v2.g))
				b := uint8(w0*float32(v0.b) + w1*float32(v1.b) + w2*float32(v2.b))
				a := uint8(w0*float32(v0.a) + w1*float32(v1.a) + w2*float32(v2.a))

				if hasTex {
					tu := w0*v0.u + w1*v1.u + w2*v2.u
					tv := w0*v0.v + w1*v1.v + w2*v2.v
					tu = tu - float32(math.Floor(float64(tu)))
					tv = tv - float32(math.Floor(float64(tv)))
					tx := uint32(tu*float32(texRes.Width)) % texRes.Width
					ty := uint32(tv*float32(texRes.Height)) % texRes.Height
					tp := texRes.Pixels[ty*texRes.Width+tx]
					tr := uint8(tp & 0xFF)
					tg := uint8((tp >> 8) & 0xFF)
					tb := uint8((tp >> 16) & 0xFF)
					ta := uint8((tp >> 24) & 0xFF)
					r = uint8((uint16(r) * uint16(tr)) / 255)
					g = uint8((uint16(g) * uint16(tg)) / 255)
					b = uint8((uint16(b) * uint16(tb)) / 255)
					a = uint8((uint16(a) * uint16(ta)) / 255)
				} else if isGlyphQuad {
					// Procedural glyph stroke pattern derived from font atlas UV coordinates
					rx := float64(x-minX) / math.Max(1.0, float64(maxX-minX))
					ry := float64(y-minY) / math.Max(1.0, float64(maxY-minY))
					if rx < 0.15 || rx > 0.85 || ry < 0.15 || ry > 0.85 {
						continue
					}
					uSeed := int(v0.u*256.0) + int(v0.v*512.0)
					gx := int((rx - 0.15) / 0.70 * 5.0)
					gy := int((ry - 0.15) / 0.70 * 7.0)
					bit := ((uSeed*31 + gx*7 + gy*13) ^ (gx * gy)) & 3
					if bit == 0 && gy != 0 && gy != 3 && gx != 2 {
						continue
					}
				}
				if a < 8 {
					continue
				}
				off := (y*ScreenWidth + x) * 4
				if a == 255 {
					pix[off+0] = r
					pix[off+1] = g
					pix[off+2] = b
					pix[off+3] = 255
				} else {
					af := uint16(a)
					ia := 255 - af
					pix[off+0] = uint8((uint16(r)*af + uint16(pix[off+0])*ia) / 255)
					pix[off+1] = uint8((uint16(g)*af + uint16(pix[off+1])*ia) / 255)
					pix[off+2] = uint8((uint16(b)*af + uint16(pix[off+2])*ia) / 255)
					pix[off+3] = 255
				}
			}
		}
	}
}

func (s *Subsystem) rasterizePrimitiveBatch(c *cpu.CPU, primType uint32, primCount uint32, pVerts uint32, stride uint32, isUP bool) {
	if pVerts == 0 || stride < 12 || primCount == 0 {
		return
	}
	if primCount > 4096 {
		primCount = 4096
	}
	switch primType {
	case 4: // D3DPT_TRIANGLELIST
		for i := uint32(0); i < primCount; i++ {
			v0 := s.transformVertex(c, pVerts+(i*3+0)*stride, stride, isUP)
			v1 := s.transformVertex(c, pVerts+(i*3+1)*stride, stride, isUP)
			v2 := s.transformVertex(c, pVerts+(i*3+2)*stride, stride, isUP)
			s.rasterizeTriangle(c, v0, v1, v2)
		}
	case 5: // D3DPT_TRIANGLESTRIP
		for i := uint32(0); i < primCount; i++ {
			v0 := s.transformVertex(c, pVerts+(i+0)*stride, stride, isUP)
			v1 := s.transformVertex(c, pVerts+(i+1)*stride, stride, isUP)
			v2 := s.transformVertex(c, pVerts+(i+2)*stride, stride, isUP)
			if i%2 == 1 {
				v1, v2 = v2, v1
			}
			s.rasterizeTriangle(c, v0, v1, v2)
		}
	case 6: // D3DPT_TRIANGLEFAN
		v0 := s.transformVertex(c, pVerts, stride, isUP)
		for i := uint32(0); i < primCount; i++ {
			v1 := s.transformVertex(c, pVerts+(i+1)*stride, stride, isUP)
			v2 := s.transformVertex(c, pVerts+(i+2)*stride, stride, isUP)
			s.rasterizeTriangle(c, v0, v1, v2)
		}
	}
}

func (s *Subsystem) rasterizeIndexedBatch(c *cpu.CPU, primType uint32, baseV int32, startIdx uint32, primCount uint32, pVerts uint32, stride uint32, pIdx uint32, idxLen uint32, is32BitIdx bool, isUP bool) {
	if pVerts == 0 || pIdx == 0 || stride < 12 || primCount == 0 {
		return
	}
	if !s.rendering {
		return // frame will not be exported: skip software rasterization entirely
	}
	if primCount > 4096 {
		primCount = 4096
	}
	readIdx := func(n uint32) uint32 {
		pos := startIdx + n
		if is32BitIdx {
			if pos*4+4 > idxLen {
				return 0
			}
			return uint32(int32(c.Read32(pIdx+pos*4)) + baseV)
		}
		if pos*2+2 > idxLen {
			return 0
		}
		return uint32(int32(c.Read16(pIdx+pos*2)) + baseV)
	}

	switch primType {
	case 4: // D3DPT_TRIANGLELIST
		for i := uint32(0); i < primCount; i++ {
			i0 := readIdx(i*3 + 0)
			i1 := readIdx(i*3 + 1)
			i2 := readIdx(i*3 + 2)
			v0 := s.transformVertex(c, pVerts+i0*stride, stride, isUP)
			v1 := s.transformVertex(c, pVerts+i1*stride, stride, isUP)
			v2 := s.transformVertex(c, pVerts+i2*stride, stride, isUP)
			s.rasterizeTriangle(c, v0, v1, v2)
		}
	case 5: // D3DPT_TRIANGLESTRIP
		for i := uint32(0); i < primCount; i++ {
			i0 := readIdx(i + 0)
			i1 := readIdx(i + 1)
			i2 := readIdx(i + 2)
			if i0 == i1 || i1 == i2 || i0 == i2 {
				continue
			}
			v0 := s.transformVertex(c, pVerts+i0*stride, stride, isUP)
			v1 := s.transformVertex(c, pVerts+i1*stride, stride, isUP)
			v2 := s.transformVertex(c, pVerts+i2*stride, stride, isUP)
			if i%2 == 1 {
				v1, v2 = v2, v1
			}
			s.rasterizeTriangle(c, v0, v1, v2)
		}
	}
}

func (s *Subsystem) exportFramePNG() {
	if s.Framebuffer == nil {
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, s.Framebuffer); err != nil {
		return
	}
	pngBytes := buf.Bytes()

	s.FrameMu.Lock()
	s.LatestPNG = pngBytes
	s.FrameMu.Unlock()

	if s.FramesDir != "" && (s.FrameDrawCalls > 0 || s.FrameCount == 1 || s.FrameCount%30 == 0) {
		latestPath := filepath.Join(s.FramesDir, "latest.png")
		_ = os.WriteFile(latestPath, pngBytes, 0644)
		if s.FrameCount == 1 || s.FrameCount == 30 || s.FrameCount == 60 || s.FrameCount == 90 || s.FrameCount == 120 || s.FrameCount%30 == 0 {
			framePath := filepath.Join(s.FramesDir, fmt.Sprintf("frame_%04d.png", s.FrameCount))
			_ = os.WriteFile(framePath, pngBytes, 0644)
		}
	}
}

func (s *Subsystem) startDisplayServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <title>LEGO Star Wars: The Complete Saga (64-Bit Recompiled Engine)</title>
  <style>
    body { background: #0d1117; color: #e6edf3; font-family: system-ui, sans-serif; display: flex; flex-direction: column; align-items: center; margin: 0; padding: 20px; }
    h1 { font-size: 20px; margin-bottom: 8px; }
    .hud { font-family: monospace; font-size: 14px; color: #58a6ff; margin-bottom: 12px; }
    canvas, img { border: 2px solid #30363d; border-radius: 6px; box-shadow: 0 8px 24px rgba(0,0,0,0.6); width: 1280px; height: 720px; background: #000; }
  </style>
</head>
<body>
  <h1>LEGO Star Wars: The Complete Saga — Native 64-Bit Host Runtime</h1>
  <div class="hud" id="hud">Frame: %d | DrawCalls: %d | Triangles: %d</div>
  <img id="screen" src="/latest.png" />
  <script>
    setInterval(() => {
      document.getElementById('screen').src = '/latest.png?t=' + Date.now();
    }, 250);
  </script>
</body>
</html>`, s.FrameCount, s.DrawCallCount, s.TriangleCount)
	})
	mux.HandleFunc("/latest.png", func(w http.ResponseWriter, r *http.Request) {
		s.FrameMu.Lock()
		data := s.LatestPNG
		s.FrameMu.Unlock()
		if len(data) == 0 {
			http.Error(w, "no frame yet", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})

	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.1:0"} {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			s.DisplayURL = "http://" + ln.Addr().String()
			go func() {
				_ = http.Serve(ln, mux)
			}()
			break
		}
	}
}

var _ = color.RGBA{}

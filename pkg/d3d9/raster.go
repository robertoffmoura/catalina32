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

var liveMode = os.Getenv("LSW_LIVE") == "1"
var fastPNG = png.Encoder{CompressionLevel: png.BestSpeed}

func (s *Subsystem) wantRender(frame uint64) bool {
	if s.CantinaDIPLogged > 0 {
		if liveMode {
			return frame%2 == 0 || frame%uint64(renderEvery) == 0
		}
		return frame%5 == 0 || frame%uint64(renderEvery) == 0
	}
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
		vd := s.VertexDecls[s.CurrentVertexDecl]
		if vd != nil && !isUP && vd.HasTex0 {
			if vd.Tex0Type == 3 && uint32(vd.Tex0Offset)+16 <= stride {
				// D3DDECLTYPE_FLOAT4: .xy is normal/surface UV, .zw is layer0 diffuse/atlas UV
				u = c.ReadF32(vp + uint32(vd.Tex0Offset) + 8)
				v = c.ReadF32(vp + uint32(vd.Tex0Offset) + 12)
				if math.Abs(float64(u)) < 0.001 && math.Abs(float64(v+1.0)) < 0.001 {
					u = -1000.0
					v = -1000.0
				}
			} else if uint32(vd.Tex0Offset)+8 <= stride {
				u = c.ReadF32(vp + uint32(vd.Tex0Offset))
				v = c.ReadF32(vp + uint32(vd.Tex0Offset) + 4)
			}
		} else {
			switch stride {
			case 32, 40, 48:
				u = c.ReadF32(vp + 24)
				v = c.ReadF32(vp + 28)
			case 36, 44, 52:
				u = c.ReadF32(vp + 28)
				v = c.ReadF32(vp + 32)
			case 56, 64:
				u = c.ReadF32(vp + 32)
				v = c.ReadF32(vp + 36)
			default:
				u = c.ReadF32(vp + stride - 8)
				v = c.ReadF32(vp + stride - 4)
			}
		}
		// Decode packed UBYTE4N normal at vp+12 for directional 3D lighting
		nx := float32(int(c.Mem[vp+12])-128) / 127.0
		ny := float32(int(c.Mem[vp+13])-128) / 127.0
		nz := float32(int(c.Mem[vp+14])-128) / 127.0
		// Apply skeletal animation bone matrices (NuUploadSkinMatrices, 0x00070110) for skinned character meshes
		bwOff, biOff := uint32(36), uint32(40)
		isSkinned := stride == 44
		if vd != nil && !isUP && vd.HasBlendWeight && vd.HasBlendIndices {
			bwOff = uint32(vd.BlendWOffset)
			biOff = uint32(vd.BlendIOffset)
			isSkinned = true
		}
		if isSkinned && s.NumSkinMatrices > 0 && vp+bwOff+4 <= uint32(len(c.Mem)) && vp+biOff+4 <= uint32(len(c.Mem)) {
			var bx, by, bz, bnx, bny, bnz, totalW float32
			for k := uint32(0); k < 3; k++ {
				wb := c.Mem[vp+bwOff+k]
				bi := uint32(c.Mem[vp+biOff+k])
				if wb > 0 && bi < s.NumSkinMatrices {
					wf := float32(wb) / 255.0
					m := s.SkinMatrices[bi]
					bx += wf * (x*m[0] + y*m[4] + z*m[8] + m[12])
					by += wf * (x*m[1] + y*m[5] + z*m[9] + m[13])
					bz += wf * (x*m[2] + y*m[6] + z*m[10] + m[14])
					bnx += wf * (nx*m[0] + ny*m[4] + nz*m[8])
					bny += wf * (nx*m[1] + ny*m[5] + nz*m[9])
					bnz += wf * (nx*m[2] + ny*m[6] + nz*m[10])
					totalW += wf
				}
			}
			if totalW > 0.01 {
				x, y, z = bx, by, bz
				nx, ny, nz = bnx, bny, bnz
			}
		}
		dot := nx*0.45 + ny*0.70 - nz*0.45
		shade := float32(0.60) + 0.40*dot
		if shade < 0.32 {
			shade = 0.32
		} else if shade > 1.0 {
			shade = 1.0
		}
		mr, mg, mb := float32(235), float32(222), float32(198)
		// Check material colors bound by NuShaderBindMaterial:
		// PSConstants[112] = layer0_diffuse ([material+0xc8])
		// VSConstants[114] = surface diffuse ([material+0x120])
		// PSConstants[113] = ambientColor ([material+0x11c])
		mc := s.PSConstants[112]
		if mc[0] <= 0.01 && mc[1] <= 0.01 && mc[2] <= 0.01 {
			mc = s.VSConstants[114]
		}
		if mc[0] <= 0.01 && mc[1] <= 0.01 && mc[2] <= 0.01 {
			mc = s.PSConstants[113]
		}
		if mc[0] > 0.01 || mc[1] > 0.01 || mc[2] > 0.01 {
			if mc[0] <= 1.0 && mc[1] <= 1.0 && mc[2] <= 1.0 {
				// If c112 is pure white (1,1,1) on an untextured FLOAT4 surface mesh, keep warm Cantina sandstone tint
				if !(u < -900.0 && mc[0] > 0.98 && mc[1] > 0.98 && mc[2] > 0.98) {
					mr = float32(math.Min(255.0, float64(0.20+0.80*mc[0])*255.0))
					mg = float32(math.Min(255.0, float64(0.20+0.80*mc[1])*255.0))
					mb = float32(math.Min(255.0, float64(0.20+0.80*mc[2])*255.0))
				}
			}
		}
		if vd != nil && !isUP && vd.HasColor0 && uint32(vd.Color0Offset)+4 <= stride {
			col0 := c.Read32(vp + uint32(vd.Color0Offset))
			cr := float32((col0>>16)&0xFF) / 127.0
			cg := float32((col0>>8)&0xFF) / 127.0
			cb := float32(col0&0xFF) / 127.0
			if cr > 1.0 {
				cr = 1.0
			}
			if cg > 1.0 {
				cg = 1.0
			}
			if cb > 1.0 {
				cb = 1.0
			}
			if cr > 0.02 || cg > 0.02 || cb > 0.02 {
				mr *= 0.55 + 0.45*cr
				mg *= 0.55 + 0.45*cg
				mb *= 0.55 + 0.45*cb
			}
		}
		r = uint8(mr * shade)
		g = uint8(mg * shade)
		b = uint8(mb * shade)
	}

	// Case 0: Pre-transformed screen-space XYZRHW quad (x in [0..1280], y in [0..720], rhw == 1.0)
	if stride == 28 && c.Read32(vp+12) == 0x3f800000 && x >= -1.0 && x <= float32(ScreenWidth)+1.0 && y >= -1.0 && y <= float32(ScreenHeight)+1.0 {
		return screenVertex{sx: x, sy: y, sz: 0.0, r: r, g: g, b: b, a: a, u: u, v: v, valid: true}
	}

	// Case 1: DrawPrimitiveUP 2D UI/splash quads ([-1, 1] x [-1, 1]), excluding 3D in-world DrawPrimitiveUP (shadows & 3D Cantina text)
	is3DUP := isUP && (math.Abs(float64(x)) > 2.0 || math.Abs(float64(y)) > 2.0 || math.Abs(float64(z)) > 0.01 ||
		(math.Abs(float64(s.VSConstants[17][1]-1.0)) < 0.05 && (math.Abs(float64(s.VSConstants[16][3])) > 1.0 || math.Abs(float64(s.VSConstants[18][3])) > 1.0)))
	if isUP && !is3DUP {
		sx := (x + 1.0) * 0.5 * float32(ScreenWidth)
		sy := (1.0 - y) * 0.5 * float32(ScreenHeight)
		return screenVertex{sx: sx, sy: sy, sz: 0.0, r: r, g: g, b: b, a: a, u: u, v: v, valid: true}
	}

	// Case 2: Transform 3D vertex using D3D9 VSConstants (c16..c18 = world, c0..c3 = viewProj, transposed dp4 layout)
	vp0, vp1, vp2, vp3 := s.VSConstants[0], s.VSConstants[1], s.VSConstants[2], s.VSConstants[3]
	if vp0[0] != 0 || vp0[2] != 0 || vp1[1] != 0 || vp2[0] != 0 {
		wx, wy, wz := x, y, z
		w0, w1, w2 := s.VSConstants[16], s.VSConstants[17], s.VSConstants[18]
		if w0[0] != 0 || w0[1] != 0 || w0[2] != 0 || w1[0] != 0 || w1[1] != 0 || w1[2] != 0 {
			wx = w0[0]*x + w0[1]*y + w0[2]*z + w0[3]
			wy = w1[0]*x + w1[1]*y + w1[2]*z + w1[3]
			wz = w2[0]*x + w2[1]*y + w2[2]*z + w2[3]
		}
		cx := vp0[0]*wx + vp0[1]*wy + vp0[2]*wz + vp0[3]
		cy := vp1[0]*wx + vp1[1]*wy + vp1[2]*wz + vp1[3]
		cz := vp2[0]*wx + vp2[1]*wy + vp2[2]*wz + vp2[3]
		cw := vp3[0]*wx + vp3[1]*wy + vp3[2]*wz + vp3[3]
		if cw <= 0.08 || cw > 1000.0 {
			return screenVertex{}
		}
		ndcX, ndcY, ndcZ := cx/cw, cy/cw, cz/cw
		if ndcX < -4.0 || ndcX > 4.0 || ndcY < -4.0 || ndcY > 4.0 {
			return screenVertex{}
		}
		sx := (ndcX + 1.0) * 0.5 * float32(ScreenWidth)
		sy := (1.0 - ndcY) * 0.5 * float32(ScreenHeight)
		return screenVertex{sx: sx, sy: sy, sz: ndcZ, r: r, g: g, b: b, a: a, u: u, v: v, valid: true}
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
	is3D := v0.sz > 0.001 || v1.sz > 0.001 || v2.sz > 0.001
	if is3D && (maxY-minY) > 660 && (maxX-minX) > 660 {
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

	// Skip fullscreen post-process / fade / pillarbox quads so they don't overwrite the software framebuffer
	if !hasTex && ((maxX-minX) >= 1100 && (maxY-minY) >= 600 || (!is3D && v0.r < 16 && v0.g < 16 && v0.b < 16 && ((maxX-minX) >= 24 || (maxY-minY) >= 24))) {
		return
	}

	area := (v1.sx-v0.sx)*(v2.sy-v0.sy) - (v1.sy-v0.sy)*(v2.sx-v0.sx)
	if math.Abs(float64(area)) < 0.001 {
		return
	}
	invArea := 1.0 / area
	isGlyphQuad := !hasTex && v0.r == 0x4B && v0.g == 0xD5 && v0.b == 0xEE && (maxX-minX) <= 64 && (maxY-minY) <= 64
	alphaEnabled := s.RenderStates[27] != 0 || s.RenderStates[15] != 0

	pix := s.Framebuffer.Pix
	zbuf := s.ZBuffer
	for y := minY; y <= maxY; y++ {
		py := float32(y) + 0.5
		rowIdx := y * ScreenWidth
		for x := minX; x <= maxX; x++ {
			px := float32(x) + 0.5
			w0 := ((v1.sx-px)*(v2.sy-py) - (v1.sy-py)*(v2.sx-px)) * invArea
			w1 := ((v2.sx-px)*(v0.sy-py) - (v2.sy-py)*(v0.sx-px)) * invArea
			w2 := 1.0 - w0 - w1
			if (w0 >= 0 && w1 >= 0 && w2 >= 0) || (w0 <= 0 && w1 <= 0 && w2 <= 0) {
				if w0 < 0 {
					w0, w1, w2 = -w0, -w1, -w2
				}
				z := w0*v0.sz + w1*v1.sz + w2*v2.sz
				zIdx := rowIdx + x
				if is3D && z > zbuf[zIdx] {
					continue
				}
				r := uint8(w0*float32(v0.r) + w1*float32(v1.r) + w2*float32(v2.r))
				g := uint8(w0*float32(v0.g) + w1*float32(v1.g) + w2*float32(v2.g))
				b := uint8(w0*float32(v0.b) + w1*float32(v1.b) + w2*float32(v2.b))
				a := uint8(w0*float32(v0.a) + w1*float32(v1.a) + w2*float32(v2.a))

				if hasTex && v0.u > -900.0 {
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
					if tr == 0 && tg == 0 && tb == 0 && ta == 0 && texRes.Format != 0x31545844 {
						continue
					}
					r = uint8((uint16(r) * uint16(tr)) / 255)
					g = uint8((uint16(g) * uint16(tg)) / 255)
					b = uint8((uint16(b) * uint16(tb)) / 255)
					if !is3D || alphaEnabled {
						a = uint8((uint16(a) * uint16(ta)) / 255)
					} else {
						a = 255
					}
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
				if is3D && a >= 240 {
					zbuf[zIdx] = z
				}
				off := zIdx * 4
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
	if pVerts == 0 || stride < 12 || primCount == 0 || !s.rendering {
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
	if err := fastPNG.Encode(&buf, s.Framebuffer); err != nil {
		return
	}
	pngBytes := buf.Bytes()

	s.FrameMu.Lock()
	s.LatestPNG = pngBytes
	s.FrameMu.Unlock()

	if liveMode {
		if s.FramesDir != "" && s.FrameCount%60 == 0 {
			latestPath := filepath.Join(s.FramesDir, "latest.png")
			_ = os.WriteFile(latestPath, pngBytes, 0644)
		}
		return
	}

	if s.FramesDir != "" && (s.FrameDrawCalls > 0 || s.FrameCount == 1 || s.FrameCount%30 == 0) {
		latestPath := filepath.Join(s.FramesDir, "latest.png")
		_ = os.WriteFile(latestPath, pngBytes, 0644)
		if s.CubeTexCount >= 8 && s.FrameDrawCalls > 50 {
			cantinaPath := filepath.Join(s.FramesDir, "cantina_latest.png")
			_ = os.WriteFile(cantinaPath, pngBytes, 0644)
			framePath := filepath.Join(s.FramesDir, fmt.Sprintf("frame_%04d.png", s.FrameCount))
			_ = os.WriteFile(framePath, pngBytes, 0644)
		} else if s.FrameCount == 1 || s.FrameCount == 30 || s.FrameCount == 60 || s.FrameCount == 90 || s.FrameCount == 120 || s.FrameCount%30 == 0 {
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
    body { background: #0d1117; color: #e6edf3; font-family: system-ui, sans-serif; display: flex; flex-direction: column; align-items: center; margin: 0; padding: 12px; user-select: none; }
    h1 { font-size: 16px; margin: 0 0 4px 0; }
    .hud { font-family: monospace; font-size: 13px; color: #58a6ff; margin-bottom: 8px; }
    .controls { display: flex; gap: 6px; flex-wrap: wrap; justify-content: center; margin-bottom: 8px; }
    button { background: #21262d; color: #c9d1d9; border: 1px solid #30363d; border-radius: 6px; padding: 5px 10px; font-family: monospace; font-size: 12px; cursor: pointer; }
    button:hover { background: #30363d; border-color: #58a6ff; }
    button:active { background: #1f6feb; color: #fff; }
    canvas, img { border: 2px solid #30363d; border-radius: 6px; box-shadow: 0 8px 24px rgba(0,0,0,0.6); max-width: 100%%; width: 1120px; height: auto; aspect-ratio: 16/9; background: #000; }
    .hint { font-size: 12px; color: #8b949e; margin-top: 6px; font-family: monospace; }
  </style>
</head>
<body>
  <h1>LEGO Star Wars: The Complete Saga — Native 64-Bit Host Runtime</h1>
  <div class="hud" id="hud">Frame: %d | DrawCalls: %d | Triangles: %d</div>
  <div class="controls">
    <button onclick="tapKey(0x1C)">Enter / Start</button>
    <button onclick="tapKey(0x3C)">F2 (P2 Start)</button>
    <button onclick="tapKey(0x01)">Esc (Pause)</button>
    <button onclick="tapKey(0xC8)">&#9650; Up (W)</button>
    <button onclick="tapKey(0xD0)">&#9660; Down (S)</button>
    <button onclick="tapKey(0xCB)">&#9664; Left (A)</button>
    <button onclick="tapKey(0xCD)">&#9654; Right (D)</button>
    <button onclick="tapKey(0x16)">U (Jump)</button>
    <button onclick="tapKey(0x23)">H (Attack)</button>
    <button onclick="tapKey(0x24)">J (Special)</button>
    <button onclick="tapKey(0x25)">K (Switch)</button>
    <button onclick="cycleAutoKeys()" id="autokeys-btn">AutoKeys: %s</button>
  </div>
  <img id="screen" src="/latest.png" />
  <div class="hint">Keyboard focus active: Arrow keys / WASD / Enter / Space / Esc / F2 / U H J K are forwarded directly to DirectInput8.</div>
  <script>
    const keyMap = {
      'Enter': 0x1C, 'Escape': 0x01, 'Space': 0x39, 'F2': 0x3C,
      'ArrowUp': 0xC8, 'ArrowDown': 0xD0, 'ArrowLeft': 0xCB, 'ArrowRight': 0xCD,
      'KeyW': 0x11, 'KeyS': 0x1F, 'KeyA': 0x1E, 'KeyD': 0x20,
      'KeyU': 0x16, 'KeyH': 0x23, 'KeyJ': 0x24, 'KeyK': 0x25
    };
    let autoMode = %q;
    function tapKey(dik) {
      fetch('/key?dik=' + dik + '&action=tap', {method: 'POST'});
    }
    function cycleAutoKeys() {
      const order = ['boot', 'off', 'loop'];
      autoMode = order[(order.indexOf(autoMode) + 1) %% order.length];
      fetch('/autokeys?mode=' + autoMode, {method: 'POST'});
    }
    window.addEventListener('keydown', (e) => {
      if (keyMap[e.code] !== undefined) {
        e.preventDefault();
        fetch('/key?dik=' + keyMap[e.code] + '&action=down', {method: 'POST'});
      }
    });
    window.addEventListener('keyup', (e) => {
      if (keyMap[e.code] !== undefined) {
        e.preventDefault();
        fetch('/key?dik=' + keyMap[e.code] + '&action=up', {method: 'POST'});
      }
    });
    const screenImg = document.getElementById('screen');
    let frameBusy = false;
    function pumpFrame() {
      if (frameBusy) return;
      frameBusy = true;
      const next = new Image();
      next.onload = () => {
        screenImg.src = next.src;
        frameBusy = false;
        setTimeout(pumpFrame, 30);
      };
      next.onerror = () => {
        frameBusy = false;
        setTimeout(pumpFrame, 80);
      };
      next.src = '/latest.png?t=' + performance.now();
    }
    pumpFrame();
    let lastFrame = 0, lastTime = performance.now(), fps = 0;
    setInterval(async () => {
      try {
        const res = await fetch('/status.json');
        const st = await res.json();
        const now = performance.now();
        if (lastFrame > 0 && st.frame >= lastFrame) {
          fps = ((st.frame - lastFrame) * 1000 / Math.max(1, now - lastTime)).toFixed(1);
        }
        lastFrame = st.frame;
        lastTime = now;
        document.getElementById('hud').textContent =
          'Frame: ' + st.frame + ' (' + fps + ' FPS) | DrawCalls: ' + st.drawCalls +
          ' | Triangles: ' + st.triangles + ' | Steps: ' + (st.steps / 1e6).toFixed(1) + 'M';
        autoMode = st.autoKeys;
        document.getElementById('autokeys-btn').textContent = 'AutoKeys: ' + st.autoKeys;
      } catch (e) {}
    }, 250);
  </script>
</body>
</html>`, s.FrameCount, s.DrawCallCount, s.TriangleCount, s.AutoKeysMode, s.AutoKeysMode)
	})
	mux.HandleFunc("/status.json", func(w http.ResponseWriter, r *http.Request) {
		steps := uint64(0)
		if s.VM != nil {
			steps = s.VM.Steps
		}
		s.KeyMu.Lock()
		ak := s.AutoKeysMode
		s.KeyMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprintf(w, `{"frame":%d,"drawCalls":%d,"triangles":%d,"clears":%d,"steps":%d,"autoKeys":%q}`,
			s.FrameCount, s.DrawCallCount, s.TriangleCount, s.ClearCount, steps, ak)
	})
	mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) {
		dik, err := strconv.ParseUint(r.URL.Query().Get("dik"), 0, 8)
		if err != nil {
			http.Error(w, "bad dik", http.StatusBadRequest)
			return
		}
		action := r.URL.Query().Get("action")
		s.KeyMu.Lock()
		switch action {
		case "down":
			s.KeyHoldUntil[dik] = s.FrameCount + 60
		case "up":
			s.KeyHoldUntil[dik] = 0
		default: // "tap"
			s.KeyHoldUntil[dik] = s.FrameCount + 10
		}
		s.KeyMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/autokeys", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if mode == "boot" || mode == "off" || mode == "loop" {
			s.KeyMu.Lock()
			s.AutoKeysMode = mode
			s.KeyMu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
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

	addrs := []string{"127.0.0.1:8080", "127.0.0.1:0"}
	if p := os.Getenv("LSW_PORT"); p != "" {
		addrs = []string{"127.0.0.1:" + p, "127.0.0.1:0"}
	}
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			s.DisplayURL = "http://" + ln.Addr().String()
			fmt.Printf("[Display] Live viewer listening at %s\n", s.DisplayURL)
			go func() {
				_ = http.Serve(ln, mux)
			}()
			break
		}
	}
}

var _ = color.RGBA{}

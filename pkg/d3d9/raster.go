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
	"sync"
	"time"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3dsm"
)

const (
	ScreenWidth    = 1280
	ScreenHeight   = 720
	ShmHeaderSize  = 288
	ShmAudioOffset = ShmHeaderSize + ScreenWidth*ScreenHeight*4
	ShmAudioFrames = 16384
	// Game controllers written by the viewer: a u32 ready marker ("C32P" once its controller scan
	// is done), then ShmPads records of ShmPadSize bytes each:
	// u32 connected, u16 XInput buttons, u8 left trigger, u8 right trigger, s16 LX, LY, RX, RY.
	ShmPadOffset = ShmAudioOffset + 8 + ShmAudioFrames*4
	ShmPadReady  = 0x50323343 // "C32P" little-endian
	ShmPads      = 4
	// ShmHotplugPads slots are announced to the game even when empty (players 1 and 2).
	ShmHotplugPads = 2
	ShmPadSize     = 16
	ShmTotalSize   = ShmPadOffset + 4 + ShmPads*ShmPadSize
)

type screenVertex struct {
	sx, sy, sz     float32
	r, g, b, a     uint8
	u, v           float32
	u1, v1, blend1 float32
	valid          bool
	// Clip-space position (only meaningful when clip == true, i.e. a vertex transformed by the
	// world/viewProj constants). Used to clip triangles against the near plane before projection.
	cx, cy, cz, cw float32
	clip           bool
	invW           float32 // 1/w for perspective-correct texturing (1 for pre-transformed / 2D vertices)
}

// texSnap is the texture state a queued triangle samples. Texture updates replace a resource's
// Pixels slice rather than writing into it, so the snapshot stays valid while the frame is drawn
// on the render goroutine.
type texSnap struct {
	Pixels                []uint32
	Width, Height, Format uint32
}

func snapTex(res *BufferDesc) texSnap {
	if res == nil {
		return texSnap{}
	}
	return texSnap{Pixels: res.Pixels, Width: res.Width, Height: res.Height, Format: res.Format}
}

// queuedTri is one render operation: a screen-space triangle, or (clearOp) a framebuffer/Z clear.
type queuedTri struct {
	v0, v1, v2   screenVertex
	tex0, tex1   texSnap
	clearOp      bool
	rtOp         bool   // switch the target of the following operations
	rtPix        []byte // rtOp: target colour buffer (nil: back buffer)
	rtSkip       bool   // rtOp: unsupported target, drop the following operations
	copyOp       bool   // StretchRect: copy copySrc (nil: back buffer) to copyDst
	copySrc      []byte
	copyDst      []byte
	draw         *drawCmd  // real-shader draw, expanded into triangles on the render goroutine
	pdraw        *progDraw // bytecode-shader draw, expanded into prog triangles on the render goroutine
	prog         *progTri  // bytecode-shader triangle (rasterized with its pixel shader)
	clearFlags   uint32
	clearColor   uint32
	minX, maxX   int
	minY, maxY   int
	invArea      float32
	alphaEnabled bool
	additive     bool
	x2           bool
	id           int32 // draw call within the frame (C32_DRAW_LOG)
}

// nearW is the minimum clip-space w accepted before perspective divide. Triangles crossing it are clipped.
const nearW = 0.05

// renderEvery: only every Nth presented frame is rasterized/exported (rendering is pure overhead for guest execution).
// Override with C32_RENDER_EVERY (1 = every frame, e.g. for a live display).
var renderEvery = func() int {
	if v, err := strconv.Atoi(os.Getenv("C32_RENDER_EVERY")); err == nil && v > 0 {
		return v
	}
	return 30
}()

var liveMode = os.Getenv("C32_LIVE") == "1"
var fastPNG = png.Encoder{CompressionLevel: png.BestSpeed}

func (s *Subsystem) wantRender(frame uint64) bool {
	if drawLogFrames[frame] {
		return true
	}
	if s.ShmFile != nil && s.FrameDrawCalls > 0 && s.FrameDrawCalls < 80 {
		return true
	}
	if s.CantinaDIPLogged > 0 || (liveMode && s.TitleFirstFrame > 0) {
		if liveMode {
			return true
		}
		return frame%5 == 0 || frame%uint64(renderEvery) == 0
	}
	return frame <= 2 || frame%uint64(renderEvery) == 0
}

func (s *Subsystem) initRasterizer(framesDir string) {
	s.rendering = true
	s.Framebuffer = image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	s.LatestRGBA = make([]byte, ScreenWidth*ScreenHeight*4)
	s.ZBuffer = make([]float32, ScreenWidth*ScreenHeight)
	s.FramesDir = framesDir
	if s.FramesDir != "" {
		_ = os.MkdirAll(s.FramesDir, 0755)
	}
	if shmPath := os.Getenv("C32_SHM_FILE"); shmPath != "" {
		if f, err := os.OpenFile(shmPath, os.O_RDWR|os.O_CREATE, 0666); err == nil {
			_ = f.Truncate(ShmTotalSize)
			_, _ = f.WriteAt(make([]byte, 4+ShmPads*ShmPadSize), ShmPadOffset) // no stale controllers
			var modeByte [1]byte
			switch s.AutoKeysMode {
			case "off":
				modeByte[0] = 1
			case "boot":
				modeByte[0] = 2
			case "loop":
				modeByte[0] = 3
			}
			_, _ = f.WriteAt(modeByte[:], 32)
			s.ShmFile = f
		}
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

// clearRows applies a Clear to framebuffer/Z rows y0..y1.
func (s *Subsystem) clearRows(target []byte, argb, flags uint32, y0, y1 int) {
	if flags&1 != 0 {
		r, g, b := uint8(argb>>16), uint8(argb>>8), uint8(argb)
		pix := target[y0*ScreenWidth*4 : (y1+1)*ScreenWidth*4]
		for i := 0; i < len(pix); i += 4 {
			pix[i+0], pix[i+1], pix[i+2], pix[i+3] = r, g, b, 0xFF
		}
	}
	if flags&2 != 0 {
		z := s.ZBuffer[y0*ScreenWidth : (y1+1)*ScreenWidth]
		for i := range z {
			z[i] = 1.0
		}
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
	if res == nil || res.IsRT {
		return res
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
	var u, v, u1, v1, blend1 float32

	if stride == 24 {
		col := c.Read32(vp + 12)
		a = uint8((col >> 24) & 0xFF)
		r = uint8((col >> 16) & 0xFF)
		g = uint8((col >> 8) & 0xFF)
		b = uint8(col & 0xFF)
		// The engine's shaders scale vertex colours by COLOR_FACTOR = 2 after texturing (0x80 = 1.0):
		// the rasterizer applies it per pixel for these draws (triState.x2).
		u = c.ReadF32(vp + 16)
		v = c.ReadF32(vp + 20)
	} else if stride >= 20 && vp+stride <= uint32(len(c.Mem)) {
		vd := s.CurrentVD
		if vd != nil && !isUP && vd.HasTex0 && uint32(vd.Tex0Offset)+8 <= stride {
			// For both FLOAT2 and FLOAT4 (where .xy is primary tiled diffuse UV and .zw is lightmap or layer 1 UV), read .xy
			u = c.ReadF32(vp + uint32(vd.Tex0Offset))
			v = c.ReadF32(vp + uint32(vd.Tex0Offset) + 4)
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
		// Multi-layer blending (layer1_sampler at s.BoundTextures[1]):
		// When vd.HasTex1 is true and TEXCOORD0 is FLOAT4 (uvSet01), LAYER1_UVSET is uvSet1 = TEXCOORD0.zw; otherwise uvSet0 = TEXCOORD0.xy.
		if s.BoundTextures[1] != 0 {
			if vd != nil && !isUP && vd.HasTex1 && vd.Tex0Type == 3 && uint32(vd.Tex0Offset)+16 <= stride {
				u1 = c.ReadF32(vp + uint32(vd.Tex0Offset) + 8)
				v1 = c.ReadF32(vp + uint32(vd.Tex0Offset) + 12)
			} else {
				u1, v1 = u, v
			}
			opac1 := float32(1.0)
			if a1 := s.PSConstants[115][3]; a1 > 0.001 && a1 <= 1.0 {
				opac1 *= a1
			}
			if kg := s.PSConstants[116][1]; kg > 0.001 && kg <= 1.0 {
				opac1 *= kg
			}
			if vd != nil && !isUP && vd.HasColor1 && uint32(vd.Color1Offset)+4 <= stride {
				col1 := c.Read32(vp + uint32(vd.Color1Offset))
				// LAYER1_COLORSET.r: check both D3DCOLOR (.r at bits 16..23) and RGBA (.r at bits 0..7)
				rByte := (col1 >> 16) & 0xFF
				if b0 := col1 & 0xFF; b0 > rByte {
					rByte = b0
				}
				blend1 = (float32(rByte) / 255.0) * opac1
			} else {
				blend1 = opac1
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
				untexturedFloat4 := s.BoundTextures[0] == 0 && vd != nil && vd.Tex0Type == 3
				if !(untexturedFloat4 && mc[0] > 0.98 && mc[1] > 0.98 && mc[2] > 0.98) {
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
		return screenVertex{sx: x, sy: y, sz: 0.0, r: r, g: g, b: b, a: a, u: u, v: v, u1: u1, v1: v1, blend1: blend1, valid: true}
	}

	// Case 1: DrawPrimitiveUP 2D UI/splash quads ([-1, 1] x [-1, 1]); in-world UP draws (shadows, 3D text, the intro crawl) are decided per draw by classifyUPBatch
	if isUP && !s.up3D {
		sx := (x + 1.0) * 0.5 * float32(ScreenWidth)
		sy := (1.0 - y) * 0.5 * float32(ScreenHeight)
		return screenVertex{sx: sx, sy: sy, sz: 0.0, r: r, g: g, b: b, a: a, u: u, v: v, u1: u1, v1: v1, blend1: blend1, valid: true}
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
		if cw != cw || cx != cx || cy != cy || cz != cz { // NaN
			return screenVertex{}
		}
		sv := screenVertex{r: r, g: g, b: b, a: a, u: u, v: v, u1: u1, v1: v1, blend1: blend1, valid: true, cx: cx, cy: cy, cz: cz, cw: cw, clip: true}
		if cw > nearW {
			projectClip(&sv)
		}
		return sv
	}

	// Fallback perspective/orthographic projection into screen center
	scale := float32(180.0)
	if z > 0.5 {
		scale = 360.0 / (z + 1.5)
	}
	sx := float32(ScreenWidth)*0.5 + x*scale
	sy := float32(ScreenHeight)*0.5 - y*scale
	return screenVertex{sx: sx, sy: sy, sz: 0.5, r: r, g: g, b: b, a: a, u: u, v: v, u1: u1, v1: v1, blend1: blend1, valid: true, invW: 1}
}

func sat2x(v uint8) uint8 { return uint8(min(int(v)*2, 255)) }

// classifyUPBatch decides whether a DrawPrimitiveUP-style batch is in-world 3D rather than a 2D overlay
// in [-1, 1] coordinates (s.up3D). The decision is per draw: deciding per vertex tears triangles that
// straddle the boundary (the intro crawl drew both flat and tilted). 3D: depth testing on, or any
// vertex outside the 2D range. (Vertex shader constants are no evidence: overlays leave the previous
// 3D draw's world matrix in place.)
func (s *Subsystem) classifyUPBatch(c *cpu.CPU, vertex func(k int) uint32, n int) {
	s.up3D = s.renderState(rsZEnable) != 0
	for k := 0; k < n && !s.up3D; k++ {
		vp := vertex(k)
		if uint64(vp)+12 > uint64(len(c.Mem)) {
			continue
		}
		x, y, z := c.ReadF32(vp), c.ReadF32(vp+4), c.ReadF32(vp+8)
		s.up3D = math.Abs(float64(x)) > 2.0 || math.Abs(float64(y)) > 2.0 || math.Abs(float64(z)) > 0.01
	}
}

// projectClip performs the perspective divide + viewport transform for a clip-space vertex with w > nearW.
func projectClip(sv *screenVertex) {
	iw := 1.0 / sv.cw
	sv.sx = (sv.cx*iw + 1.0) * 0.5 * float32(ScreenWidth)
	sv.sy = (1.0 - sv.cy*iw) * 0.5 * float32(ScreenHeight)
	sv.sz = sv.cz * iw
	sv.invW = iw
}

func lerpU8(a, b uint8, t float32) uint8 {
	return uint8(float32(a) + (float32(b)-float32(a))*t + 0.5)
}

// lerpClip interpolates two clip-space vertices (all attributes are linear in clip space).
func lerpClip(a, b *screenVertex, t float32) screenVertex {
	return screenVertex{
		cx: a.cx + (b.cx-a.cx)*t, cy: a.cy + (b.cy-a.cy)*t, cz: a.cz + (b.cz-a.cz)*t, cw: a.cw + (b.cw-a.cw)*t,
		r: lerpU8(a.r, b.r, t), g: lerpU8(a.g, b.g, t), b: lerpU8(a.b, b.b, t), a: lerpU8(a.a, b.a, t),
		u: a.u + (b.u-a.u)*t, v: a.v + (b.v-a.v)*t,
		u1: a.u1 + (b.u1-a.u1)*t, v1: a.v1 + (b.v1-a.v1)*t, blend1: a.blend1 + (b.blend1-a.blend1)*t,
		valid: true, clip: true,
	}
}

// rasterizeTriangle clips 3D triangles against the w = nearW plane (Sutherland–Hodgman, one plane) and
// rasterizes the resulting polygon as a fan. Triangles fully in front of the plane go straight through.
// primVertexCount is the number of vertices (indices) a primitive batch consumes.
func primVertexCount(primType, primCount uint32) int {
	switch primType {
	case 4:
		return int(primCount) * 3
	case 5, 6:
		return int(primCount) + 2
	}
	return 0
}

// triState is the per-draw state a triangle captures when queued.
type triState struct {
	tex0, tex1      texSnap
	alpha, additive bool
	x2              bool // colour = 2 * vertex colour * texture, saturated (stride-24 position/colour/uv vertices)
	id              int32
}

func (s *Subsystem) curTriState(texRes, texRes1 *BufferDesc) triState {
	return triState{tex0: snapTex(texRes), tex1: snapTex(texRes1),
		alpha:    s.RenderStates[27] != 0 || s.RenderStates[15] != 0,
		additive: s.RenderStates[27] != 0 && s.RenderStates[20] == 2, x2: s.colorX2, id: s.drawSeq}
}

func (s *Subsystem) rasterizeTriangle(c *cpu.CPU, v0, v1, v2 screenVertex, texRes, texRes1 *BufferDesc) {
	st := s.curTriState(texRes, texRes1)
	emitTriangle(&s.frameTris, v0, v1, v2, &st)
}

// emitTriangle clips a triangle against the near plane and appends the resulting screen
// triangles to dst.
func emitTriangle(dst *[]queuedTri, v0, v1, v2 screenVertex, st *triState) {
	if !v0.valid || !v1.valid || !v2.valid {
		return
	}
	if !(v0.clip && v1.clip && v2.clip) || (v0.cw > nearW && v1.cw > nearW && v2.cw > nearW) {
		if !v0.clip && v0.invW == 0 {
			v0.invW = 1
		}
		if !v1.clip && v1.invW == 0 {
			v1.invW = 1
		}
		if !v2.clip && v2.invW == 0 {
			v2.invW = 1
		}
		emitScreenTriangle(dst, v0, v1, v2, st)
		return
	}
	if v0.cw <= nearW && v1.cw <= nearW && v2.cw <= nearW {
		return // entirely behind the near plane
	}
	in := [3]*screenVertex{&v0, &v1, &v2}
	var out [4]screenVertex
	n := 0
	for i := 0; i < 3; i++ {
		a, b := in[i], in[(i+1)%3]
		da, db := a.cw-nearW, b.cw-nearW
		if da > 0 {
			out[n] = *a
			n++
		}
		if (da > 0) != (db > 0) {
			out[n] = lerpClip(a, b, da/(da-db))
			n++
		}
	}
	for i := 0; i < n; i++ {
		if out[i].cw <= 0 {
			out[i].cw = nearW
		}
		projectClip(&out[i])
	}
	for i := 1; i+1 < n; i++ {
		emitScreenTriangle(dst, out[0], out[i], out[i+1], st)
	}
}

func emitScreenTriangle(dst *[]queuedTri, v0, v1, v2 screenVertex, st *triState) {
	minXf, maxXf := v0.sx, v0.sx
	if v1.sx < minXf {
		minXf = v1.sx
	} else if v1.sx > maxXf {
		maxXf = v1.sx
	}
	if v2.sx < minXf {
		minXf = v2.sx
	} else if v2.sx > maxXf {
		maxXf = v2.sx
	}
	minYf, maxYf := v0.sy, v0.sy
	if v1.sy < minYf {
		minYf = v1.sy
	} else if v1.sy > maxYf {
		maxYf = v1.sy
	}
	if v2.sy < minYf {
		minYf = v2.sy
	} else if v2.sy > maxYf {
		maxYf = v2.sy
	}
	minX := int(math.Floor(float64(minXf)))
	maxX := int(math.Ceil(float64(maxXf)))
	minY := int(math.Floor(float64(minYf)))
	maxY := int(math.Ceil(float64(maxYf)))

	if maxX < 0 || minX >= ScreenWidth || maxY < 0 || minY >= ScreenHeight {
		return
	}
	is3D := v0.sz > 0.001 || v1.sz > 0.001 || v2.sz > 0.001
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

	hasTex := len(st.tex0.Pixels) > 0 && st.tex0.Width > 0 && st.tex0.Height > 0

	// Skip fullscreen post-process / fade / pillarbox quads so they don't overwrite the software framebuffer
	if !hasTex && ((maxX-minX) >= 1100 && (maxY-minY) >= 600 || (!is3D && v0.r < 16 && v0.g < 16 && v0.b < 16 && ((maxX-minX) >= 24 || (maxY-minY) >= 24))) {
		return
	}

	area := (v1.sx-v0.sx)*(v2.sy-v0.sy) - (v1.sy-v0.sy)*(v2.sx-v0.sx)
	if area > -0.001 && area < 0.001 {
		return
	}

	*dst = append(*dst, queuedTri{
		v0:           v0,
		v1:           v1,
		v2:           v2,
		tex0:         st.tex0,
		tex1:         st.tex1,
		minX:         minX,
		maxX:         maxX,
		minY:         minY,
		maxY:         maxY,
		invArea:      1.0 / area,
		alphaEnabled: st.alpha,
		additive:     st.additive,
		x2:           st.x2,
		id:           st.id,
	})
}

// frameInfo holds the counters exportFrame needs, captured when the frame is submitted.
type frameInfo struct {
	frame, drawCalls, triangles, steps uint64
	frameDrawCalls                     uint64
	cubeTexCount                       uint64
}

// queueClear records a Clear (D3DCLEAR_TARGET = 1, D3DCLEAR_ZBUFFER = 2) in draw order.
func (s *Subsystem) queueClear(color, flags uint32) {
	s.frameTris = append(s.frameTris, queuedTri{clearOp: true, clearFlags: flags, clearColor: color,
		minY: 0, maxY: ScreenHeight - 1})
}

// submitFrame hands the frame's queued operations to the render goroutine, which draws and exports
// them while the emulator runs the next frame. At most one frame is in flight.
func (s *Subsystem) submitFrame() {
	t := time.Now()
	s.WaitRender()
	s.renderWait += time.Since(t)
	info := frameInfo{frame: s.FrameCount, drawCalls: s.DrawCallCount, triangles: s.TriangleCount,
		frameDrawCalls: s.FrameDrawCalls, cubeTexCount: s.CubeTexCount}
	if s.VM != nil {
		info.steps = s.VM.Steps
	}
	ops := s.frameTris
	s.frameTris = s.spareTris[:0]
	s.renderWG.Add(1)
	var ids []int32
	if drawLogFrames[info.frame] {
		ids = make([]int32, ScreenWidth*ScreenHeight)
		for i := range ids {
			ids[i] = -1
		}
	}
	go func() {
		defer s.renderWG.Done()
		s.drawOps(s.expandProg(s.expandDraws(ops)), ids)
		if ids != nil {
			reportDrawFootprints(info.frame, ids, s.ZBuffer)
		}
		s.exportFrame(info)
		s.spareTris = ops
	}()
	if syncRender {
		s.WaitRender()
	}
}

// syncRender (C32_SYNC_RENDER=1) draws each frame before the emulator continues, for A/B testing.
var syncRender = os.Getenv("C32_SYNC_RENDER") == "1"

// dropFrameKeepClears discards a frame that is not drawn, keeping its clears so they still reach the
// framebuffer (they apply before the next submitted frame's operations).
func (s *Subsystem) dropFrameKeepClears() {
	kept := s.frameTris[:0]
	for _, op := range s.frameTris {
		if op.clearOp || op.rtOp || op.copyOp {
			kept = append(kept, op)
		}
	}
	s.frameTris = kept
}

// reportFPS prints frames and emulated instructions per second every 2 seconds (live mode only;
// headless runs use a virtual clock, so wall-clock rates there are meaningless).
func (s *Subsystem) reportFPS(c *cpu.CPU) {
	now := time.Now()
	if s.fpsTime.IsZero() {
		s.fpsTime, s.fpsFrame, s.fpsStep = now, s.FrameCount, c.Steps
		return
	}
	if dt := now.Sub(s.fpsTime).Seconds(); dt >= 2 {
		fmt.Printf("[catalina32] %.1f FPS, %.1fM x86 instructions/s, %.0f%% waiting for the renderer (frame %d)\n",
			float64(s.FrameCount-s.fpsFrame)/dt, float64(c.Steps-s.fpsStep)/dt/1e6,
			100*s.renderWait.Seconds()/dt, s.FrameCount)
		s.fpsTime, s.fpsFrame, s.fpsStep, s.renderWait = now, s.FrameCount, c.Steps, 0
	}
}

// WaitRender blocks until the in-flight frame (if any) has been drawn and exported.
func (s *Subsystem) WaitRender() {
	s.renderWG.Wait()
}

func (s *Subsystem) drawOps(ops []queuedTri, ids []int32) {
	if len(ops) == 0 || s.Framebuffer == nil {
		return
	}
	start := 0
	for i := range ops {
		switch {
		case ops[i].rtOp:
			s.drawSegment(ops[start:i], ids)
			s.drawPix, s.drawSkip = ops[i].rtPix, ops[i].rtSkip
			start = i + 1
		case ops[i].copyOp:
			s.drawSegment(ops[start:i], ids)
			src := ops[i].copySrc
			if src == nil {
				src = s.Framebuffer.Pix
			}
			copy(ops[i].copyDst, src)
			start = i + 1
		}
	}
	s.drawSegment(ops[start:], ids)
}

// drawSegment rasterizes operations into the current target in parallel horizontal bands.
func (s *Subsystem) drawSegment(ops []queuedTri, ids []int32) {
	if len(ops) == 0 || s.drawSkip {
		return
	}
	target := s.Framebuffer.Pix
	if s.drawPix != nil {
		target, ids = s.drawPix, nil
	}
	const numBands = 8
	const bandH = ScreenHeight / numBands
	var wg sync.WaitGroup
	wg.Add(numBands)
	for b := 0; b < numBands; b++ {
		bMinY := b * bandH
		bMaxY := (b+1)*bandH - 1
		if b == numBands-1 {
			bMaxY = ScreenHeight - 1
		}
		go func(yLo, yHi int) {
			defer wg.Done()
			s.rasterizeBand(ops, ids, target, yLo, yHi)
		}(bMinY, bMaxY)
	}
	wg.Wait()
}

func (s *Subsystem) rasterizeBand(ops []queuedTri, ids []int32, pix []byte, bandMinY, bandMaxY int) {
	zbuf := s.ZBuffer
	var pm d3dsm.Machine
	for i := range ops {
		qt := &ops[i]
		if qt.clearOp {
			s.clearRows(pix, qt.clearColor, qt.clearFlags, bandMinY, bandMaxY)
			continue
		}
		if qt.maxY < bandMinY || qt.minY > bandMaxY {
			continue
		}
		yStart := qt.minY
		if yStart < bandMinY {
			yStart = bandMinY
		}
		yEnd := qt.maxY
		if yEnd > bandMaxY {
			yEnd = bandMaxY
		}
		if qt.prog != nil {
			s.rasterizeProg(qt, &pm, ids, pix, yStart, yEnd)
			continue
		}

		v0, v1, v2 := qt.v0, qt.v1, qt.v2
		minX, maxX := qt.minX, qt.maxX
		texRes, texRes1 := &qt.tex0, &qt.tex1

		is3D := v0.sz > 0.001 || v1.sz > 0.001 || v2.sz > 0.001
		hasTex := len(texRes.Pixels) > 0 && texRes.Width > 0 && texRes.Height > 0
		hasTex1 := len(texRes1.Pixels) > 0 && texRes1.Width > 0 && texRes1.Height > 0

		te, teOK := setupEdges(v0.sx, v0.sy, v1.sx, v1.sy, v2.sx, v2.sy)
		if !teOK {
			continue
		}

		isGlyphQuad := !hasTex && v0.r == 0x4B && v0.g == 0xD5 && v0.b == 0xEE && (maxX-minX) <= 64 && (qt.maxY-qt.minY) <= 64
		alphaEnabled := qt.alphaEnabled
		sampleTex := hasTex && v0.u > -900.0
		sampleTex1 := hasTex1 && v0.u1 > -900.0 && (v0.blend1 > 0.01 || v1.blend1 > 0.01 || v2.blend1 > 0.01)

		var texW, texH, wMask, hMask uint32
		var texWf, texHf float32
		var isPow2, isDXT1 bool
		var texPixels []uint32
		if sampleTex {
			texW = texRes.Width
			texH = texRes.Height
			texWf = float32(texW)
			texHf = float32(texH)
			isPow2 = (texW&(texW-1)) == 0 && (texH&(texH-1)) == 0
			wMask = texW - 1
			hMask = texH - 1
			isDXT1 = texRes.Format == 0x31545844
			texPixels = texRes.Pixels
		}

		var tex1W, tex1H, w1Mask, h1Mask uint32
		var tex1Wf, tex1Hf float32
		var isPow2_1 bool
		var tex1Pixels []uint32
		if sampleTex1 {
			tex1W = texRes1.Width
			tex1H = texRes1.Height
			tex1Wf = float32(tex1W)
			tex1Hf = float32(tex1H)
			isPow2_1 = (tex1W&(tex1W-1)) == 0 && (tex1H&(tex1H-1)) == 0
			w1Mask = tex1W - 1
			h1Mask = tex1H - 1
			tex1Pixels = texRes1.Pixels
		}

		v0r, v1r, v2r := float32(v0.r), float32(v1.r), float32(v2.r)
		perspTex := (sampleTex || sampleTex1) && v0.invW > 0 && v1.invW > 0 && v2.invW > 0 &&
			(v0.invW != v1.invW || v1.invW != v2.invW)
		u0w, u1w, u2w := v0.u*v0.invW, v1.u*v1.invW, v2.u*v2.invW
		t0w, t1w, t2w := v0.v*v0.invW, v1.v*v1.invW, v2.v*v2.invW
		l1u0w, l1u1w, l1u2w := v0.u1*v0.invW, v1.u1*v1.invW, v2.u1*v2.invW
		l1t0w, l1t1w, l1t2w := v0.v1*v0.invW, v1.v1*v1.invW, v2.v1*v2.invW
		v0g, v1g, v2g := float32(v0.g), float32(v1.g), float32(v2.g)
		v0b, v1b, v2b := float32(v0.b), float32(v1.b), float32(v2.b)
		v0a, v1a, v2a := float32(v0.a), float32(v1.a), float32(v2.a)

		for y := yStart; y <= yEnd; y++ {
			rowIdx := y * ScreenWidth
			e, step := te.start(minX, y)
			for x := minX; x <= maxX; x, e[0], e[1], e[2] = x+1, e[0]+step[0], e[1]+step[1], e[2]+step[2] {
				if te.inside(&e) {
					w0 := float32(e[0]) * te.inv
					w1 := float32(e[1]) * te.inv
					w2 := 1.0 - w0 - w1
					z := w0*v0.sz + w1*v1.sz + w2*v2.sz
					zIdx := rowIdx + x
					if !is3D || z <= zbuf[zIdx] {
						r := uint8(w0*v0r + w1*v1r + w2*v2r)
						g := uint8(w0*v0g + w1*v1g + w2*v2g)
						b := uint8(w0*v0b + w1*v1b + w2*v2b)
						a := uint8(w0*v0a + w1*v1a + w2*v2a)
						skip := false

						if sampleTex {
							var tu, tv, iw float32
							if perspTex {
								iw = w0*v0.invW + w1*v1.invW + w2*v2.invW
								tu = (w0*u0w + w1*u1w + w2*u2w) / iw
								tv = (w0*t0w + w1*t1w + w2*t2w) / iw
							} else {
								tu = w0*v0.u + w1*v1.u + w2*v2.u
								tv = w0*v0.v + w1*v1.v + w2*v2.v
							}
							var tx, ty uint32
							if isPow2 {
								ui := int32(tu * texWf)
								if tu < 0 {
									ui--
								}
								vi := int32(tv * texHf)
								if tv < 0 {
									vi--
								}
								tx = uint32(ui) & wMask
								ty = uint32(vi) & hMask
							} else {
								tu = tu - float32(math.Floor(float64(tu)))
								tv = tv - float32(math.Floor(float64(tv)))
								tx = uint32(tu*texWf) % texW
								ty = uint32(tv*texHf) % texH
							}
							tp := texPixels[ty*texW+tx]
							if tp == 0 && !isDXT1 && !sampleTex1 {
								skip = true
							} else {
								tr := uint8(tp & 0xFF)
								tg := uint8((tp >> 8) & 0xFF)
								tb := uint8((tp >> 16) & 0xFF)
								ta := uint8((tp >> 24) & 0xFF)
								if sampleTex1 {
									b1 := w0*v0.blend1 + w1*v1.blend1 + w2*v2.blend1
									if b1 > 0.01 {
										var tu1, tv1 float32
										if perspTex {
											tu1 = (w0*l1u0w + w1*l1u1w + w2*l1u2w) / iw
											tv1 = (w0*l1t0w + w1*l1t1w + w2*l1t2w) / iw
										} else {
											tu1 = w0*v0.u1 + w1*v1.u1 + w2*v2.u1
											tv1 = w0*v0.v1 + w1*v1.v1 + w2*v2.v1
										}
										var tx1, ty1 uint32
										if isPow2_1 {
											ui1 := int32(tu1 * tex1Wf)
											if tu1 < 0 {
												ui1--
											}
											vi1 := int32(tv1 * tex1Hf)
											if tv1 < 0 {
												vi1--
											}
											tx1 = uint32(ui1) & w1Mask
											ty1 = uint32(vi1) & h1Mask
										} else {
											tu1 = tu1 - float32(math.Floor(float64(tu1)))
											tv1 = tv1 - float32(math.Floor(float64(tv1)))
											tx1 = uint32(tu1*tex1Wf) % tex1W
											ty1 = uint32(tv1*tex1Hf) % tex1H
										}
										tp1 := tex1Pixels[ty1*tex1W+tx1]
										tr1 := uint8(tp1 & 0xFF)
										tg1 := uint8((tp1 >> 8) & 0xFF)
										tb1 := uint8((tp1 >> 16) & 0xFF)
										ta1 := uint8((tp1 >> 24) & 0xFF)
										a1 := uint16(b1 * float32(ta1))
										if a1 > 255 {
											a1 = 255
										}
										ia1 := 255 - a1
										tr = uint8((uint16(tr)*ia1 + uint16(tr1)*a1) / 255)
										tg = uint8((uint16(tg)*ia1 + uint16(tg1)*a1) / 255)
										tb = uint8((uint16(tb)*ia1 + uint16(tb1)*a1) / 255)
										ta = 255
									}
								}
								r = uint8((uint16(r) * uint16(tr)) / 255)
								g = uint8((uint16(g) * uint16(tg)) / 255)
								b = uint8((uint16(b) * uint16(tb)) / 255)
								if !is3D || alphaEnabled {
									a = uint8((uint16(a) * uint16(ta)) / 255)
								} else {
									a = 255
								}
							}
						} else if isGlyphQuad {
							rx := float64(x-minX) / math.Max(1.0, float64(maxX-minX))
							ry := float64(y-qt.minY) / math.Max(1.0, float64(qt.maxY-qt.minY))
							if rx < 0.15 || rx > 0.85 || ry < 0.15 || ry > 0.85 {
								skip = true
							} else {
								uSeed := int(v0.u*256.0) + int(v0.v*512.0)
								gx := int((rx - 0.15) / 0.70 * 5.0)
								gy := int((ry - 0.15) / 0.70 * 7.0)
								bit := ((uSeed*31 + gx*7 + gy*13) ^ (gx * gy)) & 3
								if bit == 0 && gy != 0 && gy != 3 && gx != 2 {
									skip = true
								}
							}
						}
						if qt.x2 {
							r, g, b, a = sat2x(r), sat2x(g), sat2x(b), sat2x(a)
						}
						if !skip && a >= 8 {
							if is3D && a >= 240 && !qt.additive {
								zbuf[zIdx] = z
							}
							off := zIdx * 4
							if ids != nil {
								ids[zIdx] = qt.id
							}
							if qt.additive {
								af := uint16(a)
								rAdd := uint16(pix[off+0]) + (uint16(r)*af)/255
								gAdd := uint16(pix[off+1]) + (uint16(g)*af)/255
								bAdd := uint16(pix[off+2]) + (uint16(b)*af)/255
								if rAdd > 255 {
									rAdd = 255
								}
								if gAdd > 255 {
									gAdd = 255
								}
								if bAdd > 255 {
									bAdd = 255
								}
								pix[off+0] = uint8(rAdd)
								pix[off+1] = uint8(gAdd)
								pix[off+2] = uint8(bAdd)
								pix[off+3] = 255
							} else if a == 255 {
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
		}
	}
}

func (s *Subsystem) rasterizePrimitiveBatch(c *cpu.CPU, primType uint32, primCount uint32, pVerts uint32, stride uint32, isUP bool) {
	if pVerts == 0 || stride < 12 || primCount == 0 || !s.rendering {
		return
	}
	if s.drawLogging() {
		defer s.logEmitted(c, len(s.frameTris), pVerts, stride, isUP)
	}
	s.fbDirty = true
	if primCount > 1<<20 {
		primCount = 1 << 20
	}
	var texRes, texRes1 *BufferDesc
	if s.BoundTextures[0] != 0 {
		texRes = s.ensureTextureDecoded(c, s.BoundTextures[0])
	}
	if s.BoundTextures[1] != 0 {
		texRes1 = s.ensureTextureDecoded(c, s.BoundTextures[1])
	}
	if s.progShading() || s.realShading() {
		base0, first := pVerts, uint32(0)
		if !isUP && stride != 0 {
			if res := s.Resources[s.StreamSources[0]]; res != nil && pVerts >= res.DataPtr+s.StreamOffsets[0] {
				base0 = res.DataPtr + s.StreamOffsets[0]
				first = (pVerts - base0) / stride
			}
		}
		order := make([]uint32, primVertexCount(primType, primCount))
		for k := range order {
			order[k] = first + uint32(k)
		}
		if s.progShading() {
			s.queueProgDraw(c, primType, order, base0, stride, isUP)
		} else {
			s.queueRealDraw(c, primType, primCount, order, base0, stride, isUP, s.realTexture(c))
		}
		return
	}
	if isUP {
		s.classifyUPBatch(c, func(k int) uint32 { return pVerts + uint32(k)*stride }, primVertexCount(primType, primCount))
	}
	s.colorX2 = stride == 24
	switch primType {
	case 4: // D3DPT_TRIANGLELIST
		for i := uint32(0); i < primCount; i++ {
			v0 := s.transformVertex(c, pVerts+(i*3+0)*stride, stride, isUP)
			v1 := s.transformVertex(c, pVerts+(i*3+1)*stride, stride, isUP)
			v2 := s.transformVertex(c, pVerts+(i*3+2)*stride, stride, isUP)
			s.rasterizeTriangle(c, v0, v1, v2, texRes, texRes1)
		}
	case 5: // D3DPT_TRIANGLESTRIP
		v0 := s.transformVertex(c, pVerts, stride, isUP)
		v1 := s.transformVertex(c, pVerts+stride, stride, isUP)
		for i := uint32(0); i < primCount; i++ {
			v2 := s.transformVertex(c, pVerts+(i+2)*stride, stride, isUP)
			if i&1 == 1 {
				s.rasterizeTriangle(c, v0, v2, v1, texRes, texRes1)
			} else {
				s.rasterizeTriangle(c, v0, v1, v2, texRes, texRes1)
			}
			v0, v1 = v1, v2
		}
	case 6: // D3DPT_TRIANGLEFAN
		v0 := s.transformVertex(c, pVerts, stride, isUP)
		v1 := s.transformVertex(c, pVerts+stride, stride, isUP)
		for i := uint32(0); i < primCount; i++ {
			v2 := s.transformVertex(c, pVerts+(i+2)*stride, stride, isUP)
			s.rasterizeTriangle(c, v0, v1, v2, texRes, texRes1)
			v1 = v2
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
	if s.drawLogging() {
		defer s.logEmitted(c, len(s.frameTris), pVerts, stride, isUP)
	}
	s.fbDirty = true
	if primCount > 1<<20 {
		primCount = 1 << 20
	}
	var texRes, texRes1 *BufferDesc
	if s.BoundTextures[0] != 0 {
		texRes = s.ensureTextureDecoded(c, s.BoundTextures[0])
	}
	if s.BoundTextures[1] != 0 {
		texRes1 = s.ensureTextureDecoded(c, s.BoundTextures[1])
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

	if s.progShading() || s.realShading() {
		order := make([]uint32, primVertexCount(primType, primCount))
		for k := range order {
			order[k] = readIdx(uint32(k))
		}
		if s.progShading() {
			s.queueProgDraw(c, primType, order, pVerts, stride, isUP)
		} else {
			s.queueRealDraw(c, primType, primCount, order, pVerts, stride, isUP, s.realTexture(c))
		}
		return
	}
	if isUP {
		s.classifyUPBatch(c, func(k int) uint32 { return pVerts + readIdx(uint32(k))*stride }, primVertexCount(primType, primCount))
	}
	s.colorX2 = stride == 24
	var vCache [256]struct {
		idx uint32
		v   screenVertex
	}
	for k := range vCache {
		vCache[k].idx = 0xFFFFFFFF
	}
	getVert := func(idx uint32) screenVertex {
		slot := &vCache[idx&255]
		if slot.idx == idx {
			return slot.v
		}
		v := s.transformVertex(c, pVerts+idx*stride, stride, isUP)
		slot.idx = idx
		slot.v = v
		return v
	}

	switch primType {
	case 4: // D3DPT_TRIANGLELIST
		for i := uint32(0); i < primCount; i++ {
			i0 := readIdx(i*3 + 0)
			i1 := readIdx(i*3 + 1)
			i2 := readIdx(i*3 + 2)
			v0 := getVert(i0)
			v1 := getVert(i1)
			v2 := getVert(i2)
			s.rasterizeTriangle(c, v0, v1, v2, texRes, texRes1)
		}
	case 5: // D3DPT_TRIANGLESTRIP
		i0 := readIdx(0)
		i1 := readIdx(1)
		v0 := getVert(i0)
		v1 := getVert(i1)
		for i := uint32(0); i < primCount; i++ {
			i2 := readIdx(i + 2)
			v2 := getVert(i2)
			if i0 != i1 && i1 != i2 && i0 != i2 {
				if i&1 == 1 {
					s.rasterizeTriangle(c, v0, v2, v1, texRes, texRes1)
				} else {
					s.rasterizeTriangle(c, v0, v1, v2, texRes, texRes1)
				}
			}
			i0, i1 = i1, i2
			v0, v1 = v1, v2
		}
	}
}

func (s *Subsystem) exportFrame(fi frameInfo) {
	if s.Framebuffer == nil {
		return
	}

	s.FrameMu.Lock()
	copy(s.LatestRGBA, s.Framebuffer.Pix)
	s.LatestPNG = nil
	s.FrameMu.Unlock()

	if s.ShmFile != nil {
		var hdr [32]byte
		steps := fi.steps
		binary.LittleEndian.PutUint64(hdr[0:8], fi.frame)
		binary.LittleEndian.PutUint64(hdr[8:16], fi.drawCalls)
		binary.LittleEndian.PutUint64(hdr[16:24], fi.triangles)
		binary.LittleEndian.PutUint64(hdr[24:32], steps)
		_, _ = s.ShmFile.WriteAt(s.Framebuffer.Pix, ShmHeaderSize)
		_, _ = s.ShmFile.WriteAt(hdr[:], 0)
	}

	if liveMode {
		if s.ShmFile != nil {
			return
		}
		if s.FramesDir != "" && fi.frame%60 == 0 {
			var buf bytes.Buffer
			if err := fastPNG.Encode(&buf, s.Framebuffer); err == nil {
				latestPath := filepath.Join(s.FramesDir, "latest.png")
				_ = os.WriteFile(latestPath, buf.Bytes(), 0644)
			}
		}
		return
	}

	if s.FramesDir != "" && (fi.frameDrawCalls > 0 || fi.frame == 1 || fi.frame%30 == 0) {
		var buf bytes.Buffer
		if err := fastPNG.Encode(&buf, s.Framebuffer); err != nil {
			return
		}
		pngBytes := buf.Bytes()
		s.FrameMu.Lock()
		s.LatestPNG = pngBytes
		s.FrameMu.Unlock()

		latestPath := filepath.Join(s.FramesDir, "latest.png")
		_ = os.WriteFile(latestPath, pngBytes, 0644)
		if fi.cubeTexCount >= 8 && fi.frameDrawCalls > 50 {
			cantinaPath := filepath.Join(s.FramesDir, "cantina_latest.png")
			_ = os.WriteFile(cantinaPath, pngBytes, 0644)
			framePath := filepath.Join(s.FramesDir, fmt.Sprintf("frame_%04d.png", fi.frame))
			_ = os.WriteFile(framePath, pngBytes, 0644)
		} else if fi.frame == 1 || fi.frame == 30 || fi.frame == 60 || fi.frame == 90 || fi.frame == 120 || fi.frame%30 == 0 {
			framePath := filepath.Join(s.FramesDir, fmt.Sprintf("frame_%04d.png", fi.frame))
			_ = os.WriteFile(framePath, pngBytes, 0644)
		}
	}
}

func (s *Subsystem) mixAudioFrame(c *cpu.CPU) {
	const maxOutFrames = 4096
	outFrames := 735 // ~16.67ms at 44.1 kHz
	now := time.Now()
	if s.ShmFile != nil && s.AutoKeysMode == "off" && !s.LastAudioTime.IsZero() {
		dt := now.Sub(s.LastAudioTime).Seconds()
		if dt < 0.005 {
			dt = 0.005
		} else if dt > 0.090 {
			dt = 0.090
		}
		outFrames = int(dt * 44100.0)
		if outFrames < 220 {
			outFrames = 220
		} else if outFrames > maxOutFrames {
			outFrames = maxOutFrames
		}
	}
	s.LastAudioTime = now

	var mixL [maxOutFrames]int32
	var mixR [maxOutFrames]int32
	anyPlaying := false
	memLen := uint32(len(c.Mem))

	for _, ab := range s.AudioBuffers {
		if ab == nil || !ab.Playing || ab.Length == 0 {
			continue
		}
		ch := uint32(ab.Channels)
		if ch == 0 || ch > 2 {
			ch = 2
		}
		bps := uint32(ab.BitsPerSample / 8)
		if bps == 0 || bps > 2 {
			bps = 2
		}
		frameBytes := ch * bps
		freq := ab.Frequency
		if freq < 4000 || freq > 96000 {
			freq = ab.SampleRate
		}
		if freq < 4000 || freq > 96000 {
			freq = 44100
		}

		if s.ShmFile != nil && ab.DataPtr != 0 && ab.DataPtr+ab.Length <= memLen && ab.Volume > -5000 {
			gain := float32(1.0)
			if ab.Volume < 0 {
				gain = float32(math.Pow(10.0, float64(ab.Volume)/2000.0))
			}
			for i := 0; i < outFrames; i++ {
				srcIdx := uint32(uint64(i) * uint64(freq) / 44100)
				off := ab.PlayCursor + srcIdx*frameBytes
				if off+frameBytes > ab.Length {
					if ab.Looping {
						off %= ab.Length
						off -= off % frameBytes
					} else {
						break
					}
				}
				p := ab.DataPtr + off
				if p+frameBytes > memLen {
					break
				}
				var l, r int16
				if bps == 2 {
					l = int16(binary.LittleEndian.Uint16(c.Mem[p : p+2]))
					if ch == 2 {
						r = int16(binary.LittleEndian.Uint16(c.Mem[p+2 : p+4]))
					} else {
						r = l
					}
				} else {
					l = (int16(c.Mem[p]) - 128) << 8
					if ch == 2 {
						r = (int16(c.Mem[p+1]) - 128) << 8
					} else {
						r = l
					}
				}
				if l != 0 || r != 0 {
					anyPlaying = true
				}
				mixL[i] += int32(float32(l) * gain)
				mixR[i] += int32(float32(r) * gain)
			}
		}

		advFrames := uint32(uint64(outFrames) * uint64(freq) / 44100)
		advBytes := advFrames * frameBytes
		if ab.PlayCursor+advBytes >= ab.Length {
			if ab.Looping {
				ab.PlayCursor = (ab.PlayCursor + advBytes) % ab.Length
				ab.PlayCursor -= ab.PlayCursor % frameBytes
			} else {
				ab.PlayCursor = 0
				ab.Playing = false
			}
		} else {
			ab.PlayCursor += advBytes
		}
	}

	if s.ShmFile != nil && anyPlaying {
		var pcm [maxOutFrames * 4]byte
		for i := 0; i < outFrames; i++ {
			l, r := mixL[i], mixR[i]
			if l > 32767 {
				l = 32767
			} else if l < -32768 {
				l = -32768
			}
			if r > 32767 {
				r = 32767
			} else if r < -32768 {
				r = -32768
			}
			binary.LittleEndian.PutUint16(pcm[i*4:i*4+2], uint16(int16(l)))
			binary.LittleEndian.PutUint16(pcm[i*4+2:i*4+4], uint16(int16(r)))
		}
		ringPos := s.AudioWriteSamples % ShmAudioFrames
		uOut := uint64(outFrames)
		firstFrames := uOut
		if ringPos+firstFrames > ShmAudioFrames {
			firstFrames = ShmAudioFrames - ringPos
		}
		_, _ = s.ShmFile.WriteAt(pcm[:firstFrames*4], int64(ShmAudioOffset+8+ringPos*4))
		if firstFrames < uOut {
			_, _ = s.ShmFile.WriteAt(pcm[firstFrames*4:uOut*4], int64(ShmAudioOffset+8))
		}
		s.AudioWriteSamples += uOut
		var idxBuf [8]byte
		binary.LittleEndian.PutUint64(idxBuf[:], s.AudioWriteSamples)
		_, _ = s.ShmFile.WriteAt(idxBuf[:], int64(ShmAudioOffset))
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
		if len(data) == 0 && len(s.LatestRGBA) == ScreenWidth*ScreenHeight*4 {
			img := &image.RGBA{
				Pix:    s.LatestRGBA,
				Stride: ScreenWidth * 4,
				Rect:   image.Rect(0, 0, ScreenWidth, ScreenHeight),
			}
			var buf bytes.Buffer
			if err := fastPNG.Encode(&buf, img); err == nil {
				data = buf.Bytes()
				s.LatestPNG = data
			}
		}
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
	if p := os.Getenv("C32_PORT"); p != "" {
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

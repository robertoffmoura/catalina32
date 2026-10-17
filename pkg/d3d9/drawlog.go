package d3d9

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"catalina32/pkg/cpu"
)

// Draw inspection (C32_DRAW_LOG=900,1200): for each listed frame (numbers as in "Frame #N" and
// frame_NNNN.png), every draw call is printed with its pipeline state, the frame is always
// rendered, and after rasterization each draw's visible footprint (pixels it wrote last, and
// their bounding box) is printed. Used to find which draw produced a wrong-looking region.

var drawLogFrames = func() map[uint64]bool {
	m := map[uint64]bool{}
	for _, f := range strings.Split(os.Getenv("C32_DRAW_LOG"), ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64); err == nil {
			m[n] = true
		}
	}
	return m
}()

// drawLogging reports whether the frame being recorded is inspected.
func (s *Subsystem) drawLogging() bool {
	return len(drawLogFrames) > 0 && drawLogFrames[s.FrameCount+1]
}

// logDraw prints one draw call's state. vp is the address of the first vertex of stream 0.
func (s *Subsystem) logDraw(c *cpu.CPU, kind string, primType, primCount, vp, stride uint32) {
	if !s.drawLogging() {
		return
	}
	rs := func(i int) uint32 { return s.renderState(i) }
	tex := func(st int) string {
		t := s.BoundTextures[st]
		if t == 0 {
			return "-"
		}
		s.dumpTexture(c, t)
		if r := s.Resources[t]; r != nil {
			return fmt.Sprintf("%x(%dx%d f%d)", t, r.Width, r.Height, r.Format)
		}
		return fmt.Sprintf("%x(?)", t)
	}
	var v0 string
	if vp != 0 && uint64(vp)+16 <= uint64(len(c.Mem)) {
		v0 = fmt.Sprintf("(%.3g %.3g %.3g | %08x)", c.ReadF32(vp), c.ReadF32(vp+4), c.ReadF32(vp+8), c.Read32(vp+12))
	}
	fmt.Printf("[DRAW f%d #%d] rt=%x %s prim=%d n=%d stride=%d vs=%x ps=%x decl=%x tex0=%s tex1=%s tex2=%s"+
		" z=%d/%d/%d blend=%d %d,%d op%d atest=%d ref=%d fn%d cull=%d cw=%x v0=%s\n",
		s.FrameCount+1, s.drawSeq, s.curRT, kind, primType, primCount, stride, s.curVSObj, s.curPSObj, s.CurrentVertexDecl,
		tex(0), tex(1), tex(2),
		rs(rsZEnable), rs(rsZWriteEnable), rs(rsZFunc), rs(rsAlphaBlendEnable), rs(rsSrcBlend), rs(rsDestBlend), rs(rsBlendOp),
		rs(rsAlphaTestEnable), rs(rsAlphaRef), rs(rsAlphaFunc), rs(rsCullMode), rs(rsColorWriteEnable), v0)
}

// reportDrawFootprints prints, per draw, how many pixels of the final image it wrote and where.
func reportDrawFootprints(frame uint64, ids []int32) {
	type fp struct {
		n                      int
		minX, minY, maxX, maxY int
	}
	m := map[int32]*fp{}
	for i, id := range ids {
		if id < 0 {
			continue
		}
		x, y := i%ScreenWidth, i/ScreenWidth
		f := m[id]
		if f == nil {
			f = &fp{minX: x, minY: y, maxX: x, maxY: y}
			m[id] = f
		}
		f.n++
		f.minX, f.minY, f.maxX, f.maxY = min(f.minX, x), min(f.minY, y), max(f.maxX, x), max(f.maxY, y)
	}
	keys := make([]int32, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		f := m[k]
		fmt.Printf("[DRAWPX f%d #%d] %d px in (%d,%d)-(%d,%d)\n", frame, k, f.n, f.minX, f.minY, f.maxX, f.maxY)
	}
}

// logEmitted prints how many triangles a draw queued (default renderer; real-shader draws queue one
// deferred command) and the first vertex as the default renderer transforms it.
func (s *Subsystem) logEmitted(c *cpu.CPU, before int, vp, stride uint32, isUP bool) {
	n := len(s.frameTris) - before
	v := s.transformVertex(c, vp, stride, isUP)
	fmt.Printf("[DRAWTRI f%d #%d] queued=%d v0: screen=(%.1f %.1f z%.3g) clip=(%.3g %.3g %.3g w%.3g) rgba=%d,%d,%d,%d uv=(%.3g %.3g) valid=%v\n",
		s.FrameCount+1, s.drawSeq, n, v.sx, v.sy, v.sz, v.cx, v.cy, v.cz, v.cw, v.r, v.g, v.b, v.a, v.u, v.v, v.valid)
}

// dumpTexture writes a texture used by an inspected draw to <frames dir>/tex_<object>.png (once).
func (s *Subsystem) dumpTexture(c *cpu.CPU, t uint32) {
	if s.FramesDir == "" || s.dumpedTex[t] {
		return
	}
	if s.dumpedTex == nil {
		s.dumpedTex = map[uint32]bool{}
	}
	s.dumpedTex[t] = true
	r := s.ensureTextureDecoded(c, t)
	if r == nil || len(r.Pixels) < int(r.Width*r.Height) || r.Width == 0 {
		return
	}
	im := image.NewNRGBA(image.Rect(0, 0, int(r.Width), int(r.Height)))
	for i, p := range r.Pixels[:r.Width*r.Height] {
		im.Pix[4*i], im.Pix[4*i+1], im.Pix[4*i+2], im.Pix[4*i+3] = uint8(p), uint8(p>>8), uint8(p>>16), uint8(p>>24)
	}
	if f, err := os.Create(filepath.Join(s.FramesDir, fmt.Sprintf("tex_%x.png", t))); err == nil {
		_ = fastPNG.Encode(f, im)
		f.Close()
	}
}

// logShaderCreate prints each shader object the game creates (with C32_DRAW_LOG set): the object,
// its function token stream's version token and length, and whether it came from D3DXCompileShader.
func (s *Subsystem) logShaderCreate(c *cpu.CPU, kind string, obj, fn uint32) {
	if len(drawLogFrames) == 0 || fn == 0 {
		return
	}
	n := 0
	for p := fn; uint64(p)+4 <= uint64(len(c.Mem)) && n < 1<<16; p += 4 {
		n++
		if c.Read32(p) == 0x0000FFFF { // D3DSIO_END
			break
		}
	}
	fmt.Printf("[SHADER] %s obj=%x fn=%x version=%08x tokens=%d compiled=%v\n", kind, obj, fn, c.Read32(fn), n, s.shaderBlobs[fn] != nil)
}

// loggedDeviceCalls are the IDirect3DDevice9 methods printed for inspected frames (surface copies,
// viewports and render target changes that the rasterizer may need to model).
var loggedDeviceCalls = map[int]string{30: "UpdateSurface", 31: "UpdateTexture", 32: "GetRenderTargetData",
	34: "StretchRect", 35: "ColorFill", 39: "SetDepthStencilSurface", 43: "Clear", 47: "SetViewport", 75: "SetScissorRect"}

// noteOrigin records how a texture or surface was created (only with C32_DRAW_LOG).
func (s *Subsystem) noteOrigin(obj uint32, format string, args ...any) {
	if len(drawLogFrames) == 0 {
		return
	}
	if s.origin == nil {
		s.origin = map[uint32]string{}
	}
	s.origin[obj] = fmt.Sprintf(format, args...)
}

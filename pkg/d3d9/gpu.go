package d3d9

// GPU renderer, emulator side: instead of drawing frames in software, the device's frames are
// recorded as a command stream in shared memory (C32_GPU_SHM), which the macOS viewer replays
// with Metal. Shaders reach it as MSL (pkg/hlsl and pkg/d3dsm translate them), textures as
// RGBA8 images, draws with their vertex data, indices, constants, texture bindings and render
// state as the game set them (D3D values; the viewer maps them to Metal).
//
// Shared memory (little-endian):
//
//	header (gpuHeaderSize bytes):
//	  0  u32 magic "C32G"          4  u32 version (gpuVersion)
//	  8  u32 slot size             12 u32 slot count (gpuSlots)
//	  16 u32 viewer ready (written by the viewer: 1 while it consumes frames)
//	  20 u32 viewer errors (shader compilation and pipeline failures, for the log)
//	  32 + 16*k: slot k: u64 frame number, u32 byte length, u32 state
//	             (0 free, 1 written by the emulator, 2 being drawn by the viewer)
//	slots: gpuHeaderSize + k*slot size
//
// A slot holds one frame: a sequence of commands, each u32 opcode, u32 payload length (a
// multiple of 4), payload. The emulator fills a free slot, publishes its length and frame
// number and sets the state to 1 (release); the viewer draws the oldest written slot and
// frees it. Commands (payload fields in order):
//
//	gpuLibrary  u32 id, u32 stage (0 vertex, 1 fragment), u32 byte length, MSL source (padded)
//	gpuTexture  u32 id, u32 width, u32 height, u32 format, data: 1 = RGBA8 (red first),
//	            2/3/4 = BC1/BC2/BC3 (the game's DXT1/DXT3/DXT5 blocks, width and height multiples of 4);
//	            format | 0x100 (gpuCubeFlag): a cube texture, its six faces (+X -X +Y -Y +Z -Z) one after another;
//	            format | 0x200 (gpuMipsFlag): D3D gives the texture a mip chain, which the viewer generates
//	gpuTexFree  u32 id
//	gpuTarget   u32 id (0 = back buffer), u32 width, u32 height: draw into this target from now
//	            on (created on first use; render targets are also textures with the same id)
//	gpuClear    u32 flags (1 colour, 2 depth), u32 colour (ARGB), f32 depth, i32 x0, y0, x1, y1
//	            (inclusive)
//	gpuCopy     u32 source id, u32 destination id, u32 destination width, height (whole
//	            targets, scaled; the destination becomes a render target if it is not one)
//	gpuDraw     see encodeDraw
//	gpuFrameEnd (no payload): present the back buffer

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3dsm"
	"catalina32/pkg/hlsl"
	"catalina32/pkg/msl"
)

const (
	gpuMagic      = 0x47323343 // "C32G"
	gpuVersion    = 4          // 2: gpuDraw gained a per-draw viewport; 3: a scissor rectangle; 4: fog
	gpuHeaderSize = 4096
	gpuSlots      = 2
	gpuSlotSize   = 48 << 20

	gpuLibrary  = 1
	gpuTexture  = 2
	gpuTexFree  = 3
	gpuTarget   = 4
	gpuClear    = 5
	gpuCopy     = 6
	gpuDraw     = 7
	gpuFrameEnd = 8

	gpuDefaultAttr = 0xFFFFFFFF // attribute stream for elements the vertex data lacks: (0, 0, 0, 1)
)

// D3D render and sampler states the GPU path reads beyond state.go's.
const (
	rsSrcBlendAlpha  = 207
	rsDestBlendAlpha = 208
	rsBlendOpAlpha   = 209
	sampAddressW     = 3
	sampMinFilter    = 6
	sampMipFilter    = 7
)

// gpuState is a draw's fixed-function state, as D3D values.
type gpuState struct {
	blend                         bool
	src, dst, op, srcA, dstA, opA uint32
	blendFactor, colorWrite       uint32
	zEnable, zWrite               bool
	zFunc, cull                   uint32
	alphaFunc                     uint32 // D3DCMPFUNC; 8 (always) when alpha testing is off
	alphaRef                      float32
}

// gpuTexRef is the texture bound to a unit for one draw.
type gpuTexRef struct {
	unit, kind                            uint32 // kind: msl.TextureKind the shader declares
	desc                                  *BufferDesc
	pixels                                []uint32 // the image at draw time (nil for render targets)
	raw                                   []byte   // its compressed data (DXT formats), if kept
	format                                uint32   // D3DFORMAT
	width, height                         uint32
	addrU, addrV, addrW, magF, minF, mipF uint32
}

// gpuDrawCmd is a recorded draw for the GPU path.
type gpuDrawCmd struct {
	vs, ps       msl.Shader
	vsKey, psKey any // shader identities for library caching (psKey nil: fixed function)
	elems        []VertexElement
	streams      [16]streamSnap
	indices      []uint32  // relative to the first vertex of the snapshots
	prim         uint32    // D3DPRIMITIVETYPE (fans become lists)
	viewport     [4]uint32 // SetViewport: x, y, width, height (the target's full size by default)
	scissor      [5]uint32 // scissor test: enabled, x, y, width, height
	fog          [5]uint32 // fixed-function fog: mode (msl.Fog*), start, end, density (float bits), colour
	state        gpuState
	vsC, psC     [][4]float32
	tex          []gpuTexRef
	fixedTex     bool // fixed fragment function: texture 0 is bound
}

type gpuTexEntry struct {
	id       uint32
	uploaded unsafe.Pointer // first pixel of the image last sent (nil: never)
}

// gpuEncoder writes frames into the viewer's shared memory.
type gpuEncoder struct {
	shm      []byte
	buf      []byte
	nextID   uint32
	libs     map[string]uint32
	textures map[*BufferDesc]*gpuTexEntry
	mu       sync.Mutex // frees, which the emulation thread queues
	frees    []uint32
	seq      uint64
	logged   map[string]bool
	full     bool        // the current frame ran out of slot space
	cur      *BufferDesc // current render target (persists across frames, like the device's)
	lastErr  uint32
}

// newGPUEncoder maps the shared memory named by C32_GPU_SHM (nil if unset or unusable).
func newGPUEncoder() *gpuEncoder {
	path := os.Getenv("C32_GPU_SHM")
	if path == "" {
		return nil
	}
	_ = os.Remove(path) // a new file: a viewer still mapping the previous run's notices and remaps
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		fmt.Printf("[GPU] cannot open %s: %v\n", path, err)
		return nil
	}
	defer f.Close()
	size := gpuHeaderSize + gpuSlots*gpuSlotSize
	if err := f.Truncate(int64(size)); err != nil {
		fmt.Printf("[GPU] cannot size %s: %v\n", path, err)
		return nil
	}
	m, err := mapShm(f, size)
	if err != nil {
		fmt.Printf("[GPU] cannot map %s: %v\n", path, err)
		return nil
	}
	clear(m[:gpuHeaderSize])
	le := binary.LittleEndian
	le.PutUint32(m[4:], gpuVersion)
	le.PutUint32(m[8:], gpuSlotSize)
	le.PutUint32(m[12:], gpuSlots)
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&m[0])), gpuMagic)
	fmt.Printf("[GPU] command stream at %s (the viewer draws frames with Metal once it is ready)\n", path)
	return &gpuEncoder{shm: m, nextID: 1, libs: map[string]uint32{}, textures: map[*BufferDesc]*gpuTexEntry{}, logged: map[string]bool{}}
}

func (g *gpuEncoder) word(off int) *uint32 { return (*uint32)(unsafe.Pointer(&g.shm[off])) }

// ready reports whether the viewer is consuming frames.
// resend forgets what the viewer was sent, so that libraries and images are sent again (their
// old ids stay unused).
func (g *gpuEncoder) resend() {
	clear(g.libs)
	for _, e := range g.textures {
		e.uploaded = nil
	}
}

func (g *gpuEncoder) ready() bool { return g != nil && atomic.LoadUint32(g.word(16)) == 1 }

func (g *gpuEncoder) logOnce(key, format string, args ...any) {
	if !g.logged[key] {
		g.logged[key] = true
		fmt.Printf("[GPU] "+format+"\n", args...)
	}
}

// forget releases a destroyed resource's GPU texture.
func (g *gpuEncoder) forget(desc *BufferDesc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e := g.textures[desc]; e != nil {
		g.frees = append(g.frees, e.id)
		delete(g.textures, desc)
	}
}

// ---------------------------------------------------------------------------------------------
// Recording (emulation thread)

// hlslModule translates a compiled HLSL shader to MSL once (nil if it cannot be).
func (s *Subsystem) hlslModule(cs *compiledShader) msl.Shader {
	if cs.mod == nil && cs.modErr == nil {
		var m *hlsl.MSLModule
		if m, cs.modErr = hlsl.TranslateMSL(cs.file, cs.prog); cs.modErr == nil {
			cs.mod = m
		} else {
			fmt.Printf("[GPU] %s: %v\n", cs.prog.Entry, cs.modErr)
		}
	}
	if cs.mod == nil {
		return nil
	}
	return cs.mod
}

func (s *Subsystem) gpuState() gpuState {
	rs := s.renderState
	st := gpuState{
		blend: rs(rsAlphaBlendEnable) != 0, src: rs(rsSrcBlend), dst: rs(rsDestBlend), op: rs(rsBlendOp),
		blendFactor: rs(rsBlendFactor), colorWrite: rs(rsColorWriteEnable),
		zEnable: rs(rsZEnable) != 0, zWrite: rs(rsZWriteEnable) != 0, zFunc: rs(rsZFunc), cull: rs(rsCullMode),
		alphaFunc: 8,
	}
	st.srcA, st.dstA, st.opA = st.src, st.dst, st.op
	if rs(rsSeparateAlphaBlendEnab) != 0 {
		st.srcA, st.dstA, st.opA = rs(rsSrcBlendAlpha), rs(rsDestBlendAlpha), rs(rsBlendOpAlpha)
		if st.srcA == 0 {
			st.srcA = 2 // ONE (the D3D default)
		}
		if st.dstA == 0 {
			st.dstA = 1 // ZERO
		}
		if st.opA == 0 {
			st.opA = 1 // ADD
		}
	}
	if rs(rsAlphaTestEnable) != 0 {
		st.alphaFunc, st.alphaRef = rs(rsAlphaFunc), float32(rs(rsAlphaRef)&0xFF)/255
	}
	return st
}

// gpuFog is a draw's fixed-function fog: D3D9 applies it after pixel shaders older than
// ps_3_0 (and the fixed fragment function) while D3DRS_FOGENABLE is on, per pixel from depth
// with D3DRS_FOGTABLEMODE's curve, else from the vertex shader's fog output.
func (s *Subsystem) gpuFog(psModel int) [5]uint32 {
	if s.renderState(rsFogEnable) == 0 || psModel >= 3 {
		return [5]uint32{msl.FogOff}
	}
	mode := uint32(msl.FogVertex)
	switch s.renderState(rsFogTableMode) {
	case 1: // D3DFOG_EXP
		mode = msl.FogExp
	case 2: // D3DFOG_EXP2
		mode = msl.FogExp2
	case 3: // D3DFOG_LINEAR
		mode = msl.FogLinear
	}
	return [5]uint32{mode, s.renderState(rsFogStart), s.renderState(rsFogEnd), s.renderState(rsFogDensity), s.renderState(rsFogColor)}
}

// gpuSampler returns a sampler state with D3D's defaults (wrap, point, no mipmaps).
func (s *Subsystem) gpuSampler(st, ty int) uint32 {
	if s.sampSet[st][ty] {
		return s.samplerStates[st][ty]
	}
	switch ty {
	case sampAddressU, sampAddressV, sampAddressW, sampMagFilter, sampMinFilter:
		return 1
	}
	return 0
}

// queueGPUDraw records a shader draw for the GPU path (arguments as for queueRealDraw).
func (s *Subsystem) queueGPUDraw(c *cpu.CPU, primType uint32, order []uint32, base0, stride0 uint32, isUP bool) {
	if len(order) == 0 {
		return
	}
	minI, maxI := order[0], order[0]
	for _, i := range order {
		minI, maxI = min(minI, i), max(maxI, i)
	}
	if maxI-minI > 1<<20 {
		return
	}
	d := &gpuDrawCmd{elems: s.CurrentVD.Elements, prim: primType, state: s.gpuState(),
		viewport: [4]uint32{s.viewportX, s.viewportY, s.viewportW, s.viewportH}}
	if s.renderState(rsScissorTestEnable) != 0 {
		sc := s.scissor
		d.scissor = [5]uint32{1, sc[0], sc[1], sc[2] - min(sc[0], sc[2]), sc[3] - min(sc[1], sc[3])}
	}
	psModel := 0 // pixel shader model (0: the fixed fragment function)
	switch {
	case s.progShading():
		d.vs, d.vsKey = s.curVSProg, s.curVSProg
		d.vsC = make([][4]float32, max(s.curVSProg.MaxConst, 1))
		copy(d.vsC, s.VSConstants[:])
		s.curVSProg.ApplyDefs(d.vsC)
		if ps := s.curPSProg; ps != nil {
			d.ps, d.psKey = ps, ps
			psModel = ps.Shader.Major
			d.psC = make([][4]float32, max(ps.MaxConst, 1))
			copy(d.psC, s.PSConstants[:])
			if ps.Shader.Major < 2 { // ps_1_x constants are clamped to [-1, 1]
				for i := range d.psC {
					for k := range d.psC[i] {
						d.psC[i][k] = min(max(d.psC[i][k], -1), 1)
					}
				}
			}
			ps.ApplyDefs(d.psC)
		}
	case s.realShading():
		cs := s.curVS
		if d.vs = s.hlslModule(cs); d.vs == nil {
			return
		}
		d.vsKey = cs
		if cs.maxReg < 0 {
			cs.maxReg = maxFloatReg(cs.prog)
		}
		d.vsC = snapRegs(&s.VSConstants, cs.maxReg)
		if ps := s.curPS; ps != nil {
			if d.ps = s.hlslModule(ps); d.ps == nil {
				return
			}
			d.psKey = ps
			psModel = ps.model
			if ps.maxReg < 0 {
				ps.maxReg = maxFloatReg(ps.prog)
			}
			d.psC = snapRegs(&s.PSConstants, ps.maxReg)
		}
	default:
		return
	}
	d.fog = s.gpuFog(psModel)
	// Every unit a function declares gets a binding (desc nil: nothing bound; the viewer binds
	// a white placeholder of the declared kind).
	units := map[int]msl.TextureKind{}
	for _, sh := range []msl.Shader{d.vs, d.ps} {
		if sh != nil {
			for _, t := range sh.Textures() {
				units[t.Unit] = t.Kind
			}
		}
	}
	if d.ps == nil { // fixed fragment function: COLOR0 x texture 0
		units[0] = msl.Tex2D
	}
	for u := 0; u < 16; u++ {
		kind, ok := units[u]
		if !ok {
			continue
		}
		ref := gpuTexRef{unit: uint32(u), kind: uint32(kind),
			addrU: s.gpuSampler(u, sampAddressU), addrV: s.gpuSampler(u, sampAddressV), addrW: s.gpuSampler(u, sampAddressW),
			magF: s.gpuSampler(u, sampMagFilter), minF: s.gpuSampler(u, sampMinFilter), mipF: s.gpuSampler(u, sampMipFilter)}
		if s.BoundTextures[u] != 0 {
			if res := s.ensureTextureDecoded(c, s.BoundTextures[u]); res != nil && (res.IsRT || len(res.Pixels) > 0) {
				ref.desc, ref.width, ref.height, ref.format = res, res.Width, res.Height, res.Format
				if !res.IsRT {
					ref.pixels, ref.raw = res.Pixels, res.Raw
				}
			}
		}
		d.tex = append(d.tex, ref)
		if u == 0 && d.ps == nil && ref.desc != nil {
			d.fixedTex = true
		}
	}
	n := maxI - minI + 1
	d.streams = s.snapStreams(c, d.elems, minI, n, base0, stride0, isUP)
	d.indices = make([]uint32, 0, len(order))
	if primType == 6 { // TRIANGLEFAN -> TRIANGLELIST
		for k := 1; k+1 < len(order); k++ {
			d.indices = append(d.indices, order[0]-minI, order[k]-minI, order[k+1]-minI)
		}
		d.prim = 4
	} else {
		for _, i := range order {
			d.indices = append(d.indices, i-minI)
		}
	}
	s.frameTris = append(s.frameTris, queuedTri{gpu: d})
}

// ---------------------------------------------------------------------------------------------
// Encoding (render goroutine)

func (g *gpuEncoder) u32(v uint32)  { g.buf = binary.LittleEndian.AppendUint32(g.buf, v) }
func (g *gpuEncoder) f32(v float32) { g.u32(math.Float32bits(v)) }
func (g *gpuEncoder) b32(v bool) {
	if v {
		g.u32(1)
	} else {
		g.u32(0)
	}
}

// pad aligns the buffer to 4 bytes.
func (g *gpuEncoder) pad() {
	for len(g.buf)%4 != 0 {
		g.buf = append(g.buf, 0)
	}
}

// begin starts a command and returns the offset of its length field (end fills it in).
func (g *gpuEncoder) begin(op uint32) int {
	g.u32(op)
	g.u32(0)
	return len(g.buf) - 4
}

func (g *gpuEncoder) end(lenOff int) {
	g.pad()
	binary.LittleEndian.PutUint32(g.buf[lenOff:], uint32(len(g.buf)-lenOff-4))
}

// room reports whether n more bytes fit in a slot (with space kept for the frame end).
func (g *gpuEncoder) room(n int) bool { return len(g.buf)+n+64 <= gpuSlotSize }

// library returns the id of a vertex/fragment function, sending its source the first time.
func (g *gpuEncoder) library(key string, stage uint32, source func() string) uint32 {
	if id, ok := g.libs[key]; ok {
		return id
	}
	src := source()
	if !g.room(len(src) + 16) {
		return 0
	}
	id := g.nextID
	g.nextID++
	g.libs[key] = id
	l := g.begin(gpuLibrary)
	g.u32(id)
	g.u32(stage)
	g.u32(uint32(len(src)))
	g.buf = append(g.buf, src...)
	g.end(l)
	return id
}

// texture returns the id of a texture, sending its pixels when they are new or changed (and
// fit in this frame; otherwise a later frame sends them and the viewer draws a placeholder).
func (g *gpuEncoder) texture(desc *BufferDesc, pixels []uint32, raw []byte, format, w, h uint32) uint32 {
	g.mu.Lock()
	e := g.textures[desc]
	if e == nil {
		e = &gpuTexEntry{id: g.nextID}
		g.nextID++
		g.textures[desc] = e
	}
	g.mu.Unlock()
	faces := 1
	if desc != nil && desc.Cube {
		faces = 6
	}
	if len(pixels) == 0 || len(pixels) < int(w*h)*faces {
		return e.id
	}
	if p := unsafe.Pointer(&pixels[0]); p != e.uploaded {
		kind, data := uint32(1), unsafe.Slice((*byte)(unsafe.Pointer(&pixels[0])), int(w*h*4)*faces)
		if bc := bcFormat(format); bc != 0 && raw != nil && w%4 == 0 && h%4 == 0 {
			kind, data = bc, raw
		}
		if faces == 6 {
			kind |= gpuCubeFlag
		} else if desc != nil && desc.Mips && max(w, h) > 1 {
			kind |= gpuMipsFlag
		}
		if !g.room(len(data) + 32) {
			g.full = true
			return e.id
		}
		l := g.begin(gpuTexture)
		g.u32(e.id)
		g.u32(w)
		g.u32(h)
		g.u32(kind)
		g.buf = append(g.buf, data...)
		g.end(l)
		e.uploaded = p
	}
	return e.id
}

// Flags of a gpuTexture format word: the data is the six faces of a cube texture; the texture
// has a mip chain (the stream carries level 0 and the viewer generates the rest).
const (
	gpuCubeFlag = 0x100
	gpuMipsFlag = 0x200
)

// bcFormat is the stream's compressed texture format for a D3DFORMAT (0: none).
func bcFormat(format uint32) uint32 {
	switch format {
	case 0x31545844: // DXT1
		return 2
	case 0x32545844, 0x33545844: // DXT2, DXT3
		return 3
	case 0x34545844, 0x35545844: // DXT4, DXT5
		return 4
	}
	return 0
}

// targetID is the id of a render target (0 = back buffer).
func (g *gpuEncoder) targetID(desc *BufferDesc) uint32 {
	if desc == nil {
		return 0
	}
	return g.texture(desc, nil, nil, 0, desc.Width, desc.Height)
}

// attrClasses lists, for each vertex shader attribute, the element feeding it and the data class.
func gpuAttributes(vs msl.Shader, elems []VertexElement, streams *[16]streamSnap) (matched []int, classes []msl.AttrClass) {
	for _, a := range vs.Attributes() {
		k := -1
		for i, e := range elems {
			if int(e.Usage) < len(usageSemantic) && usageSemantic[e.Usage] == a.Semantic && int(e.UIdx) == a.Index &&
				e.Stream < 16 && streams[e.Stream].data != nil {
				k = i
				break
			}
		}
		matched = append(matched, k)
		cls := msl.AttrFloat
		if k >= 0 {
			switch elems[k].Type {
			case 5: // UBYTE4
				cls = msl.AttrUint
			case 6, 7: // SHORT2, SHORT4
				cls = msl.AttrInt
			}
		}
		classes = append(classes, cls)
	}
	return matched, classes
}

func varyingKey(v []msl.Varying) string {
	var b strings.Builder
	for _, x := range v {
		fmt.Fprintf(&b, "%s:%d:%v,", x.Name, x.Cols, x.Int)
	}
	return b.String()
}

// encodeDraw writes a draw. Payload:
//
//	u32 vertex library, fragment library, primitive type (D3DPRIMITIVETYPE), index count,
//	    attribute count, stream count, texture count, vertex constant count, fragment constant
//	    count
//	viewport: u32 x, y, width, height (SetViewport; the target's full size by default)
//	scissor: u32 enabled, x, y, width, height (SetScissorRect, when D3DRS_SCISSORTESTENABLE is on)
//	u32 fixed-texture flag
//	state: u32 blend enable, src, dst, op, src alpha, dst alpha, op alpha (D3DBLEND/D3DBLENDOP),
//	    blend factor (ARGB), colour write mask (D3DCOLORWRITEENABLE), z enable, z write, z func
//	    (D3DCMPFUNC), cull (D3DCULL), alpha func, f32 alpha reference
//	fog: u32 mode (msl.FogOff, FogVertex, FogExp, FogExp2, FogLinear), f32 start, end, density,
//	    u32 colour (ARGB)
//	attributes: u32 attribute index, stream (gpuDefaultAttr: none), offset, D3DDECLTYPE
//	streams: u32 stream, stride, byte length, bytes (padded); bound at buffer FirstVertexBuffer+stream
//	textures: u32 unit, texture id (0: none bound), kind (msl.TextureKind), address U, V, W
//	    (D3DTEXTUREADDRESS), mag, min, mip filter (D3DTEXTUREFILTERTYPE)
//	indices: u32 each
//	constants: vertex, then fragment, float4 each
func (g *gpuEncoder) encodeDraw(d *gpuDrawCmd) {
	matched, classes := gpuAttributes(d.vs, d.elems, &d.streams)
	vsLib := g.library(fmt.Sprintf("v%p/%v", d.vsKey, classes), 0, func() string { return d.vs.VertexMSL(classes) })
	vary := d.vs.Varyings()
	var psLib uint32
	if d.ps != nil {
		psLib = g.library(fmt.Sprintf("f%p/%s", d.psKey, varyingKey(vary)), 1, func() string { return d.ps.FragmentMSL(vary) })
	} else {
		psLib = g.library("fixed/"+varyingKey(vary), 1, func() string { return d3dsm.FixedFragmentMSL(vary) })
	}
	if vsLib == 0 || psLib == 0 {
		g.full = true
		return
	}
	var texIDs []uint32
	for _, t := range d.tex {
		switch {
		case t.desc == nil:
			texIDs = append(texIDs, 0)
		case t.pixels == nil:
			texIDs = append(texIDs, g.targetID(t.desc))
		default:
			texIDs = append(texIDs, g.texture(t.desc, t.pixels, t.raw, t.format, t.width, t.height))
		}
	}
	nStreams := 0
	size := 256 + len(matched)*16 + len(d.tex)*36 + len(d.indices)*4 + (len(d.vsC)+len(d.psC))*16
	for _, s := range d.streams {
		if s.data != nil {
			nStreams++
			size += len(s.data) + 16
		}
	}
	if !g.room(size) {
		g.full = true
		return
	}
	l := g.begin(gpuDraw)
	for _, v := range []uint32{vsLib, psLib, d.prim, uint32(len(d.indices)), uint32(len(matched)), uint32(nStreams),
		uint32(len(d.tex)), uint32(len(d.vsC)), uint32(len(d.psC))} {
		g.u32(v)
	}
	for _, v := range d.viewport {
		g.u32(v)
	}
	for _, v := range d.scissor {
		g.u32(v)
	}
	g.b32(d.fixedTex)
	st := &d.state
	g.b32(st.blend)
	for _, v := range []uint32{st.src, st.dst, st.op, st.srcA, st.dstA, st.opA, st.blendFactor, st.colorWrite} {
		g.u32(v)
	}
	g.b32(st.zEnable)
	g.b32(st.zWrite)
	g.u32(st.zFunc)
	g.u32(st.cull)
	g.u32(st.alphaFunc)
	g.f32(st.alphaRef)
	for _, v := range d.fog {
		g.u32(v)
	}
	for i, k := range matched {
		g.u32(uint32(i))
		if k < 0 {
			g.u32(gpuDefaultAttr)
			g.u32(0)
			g.u32(3) // FLOAT4
			continue
		}
		e := d.elems[k]
		g.u32(uint32(e.Stream))
		g.u32(uint32(e.Offset))
		g.u32(uint32(e.Type))
	}
	for i, s := range d.streams {
		if s.data == nil {
			continue
		}
		g.u32(uint32(i))
		g.u32(s.stride)
		g.u32(uint32(len(s.data)))
		g.buf = append(g.buf, s.data...)
		g.pad()
	}
	for i, t := range d.tex {
		for _, v := range []uint32{t.unit, texIDs[i], t.kind, t.addrU, t.addrV, t.addrW, t.magF, t.minF, t.mipF} {
			g.u32(v)
		}
	}
	for _, i := range d.indices {
		g.u32(i)
	}
	for _, c := range append(d.vsC, d.psC...) {
		for _, f := range c {
			g.f32(f)
		}
	}
	g.end(l)
}

// encodeFrame encodes a frame's operations and hands them to the viewer. It reports false if
// the viewer is not consuming frames (the caller then draws in software).
func (g *gpuEncoder) encodeFrame(ops []queuedTri, frame uint64) bool {
	if !g.ready() {
		return false
	}
	g.buf, g.full = g.buf[:0], false
	g.mu.Lock()
	frees := g.frees
	g.frees = nil
	if g.cur != nil && g.textures[g.cur] == nil { // the current target was destroyed
		g.cur = nil
	}
	g.mu.Unlock()
	for _, id := range frees {
		l := g.begin(gpuTexFree)
		g.u32(id)
		g.end(l)
	}
	target := func(desc *BufferDesc) {
		g.cur = desc
		l := g.begin(gpuTarget)
		w, h := uint32(ScreenWidth), uint32(ScreenHeight)
		if desc != nil {
			w, h = desc.Width, desc.Height
		}
		g.u32(g.targetID(desc))
		g.u32(w)
		g.u32(h)
		g.end(l)
	}
	target(g.cur)
	skipped := 0
	for i := range ops {
		op := &ops[i]
		switch {
		case op.gpu != nil:
			g.encodeDraw(op.gpu)
		case op.clearOp:
			l := g.begin(gpuClear)
			g.u32(op.clearFlags)
			g.u32(op.clearColor)
			g.f32(op.clearZ)
			for _, v := range []int{op.minX, op.minY, op.maxX, op.maxY} {
				g.u32(uint32(int32(v)))
			}
			g.end(l)
		case op.rtOp:
			target(op.rtDesc)
		case op.copyOp:
			l := g.begin(gpuCopy)
			g.u32(g.targetID(op.copySrcDesc))
			g.u32(g.targetID(op.copyDstDesc))
			w, h := uint32(ScreenWidth), uint32(ScreenHeight)
			if d := op.copyDstDesc; d != nil {
				w, h = d.Width, d.Height
			}
			g.u32(w)
			g.u32(h)
			g.end(l)
		default:
			skipped++
		}
	}
	g.begin(gpuFrameEnd)
	if skipped > 0 {
		g.logOnce("skipped", "%d operations of frame %d have no GPU form (drawn without shaders): not drawn", skipped, frame)
	}
	if g.full {
		g.logOnce("full", "frame %d did not fit in %d MB; the rest follows in later frames", frame, gpuSlotSize>>20)
	}
	return g.publish(frame)
}

// publish copies the frame into a free slot, waiting for the viewer to free one. The viewer
// can take seconds over a frame that needs many new shaders compiled, so only a viewer that
// has quit or has not taken a frame for 30 seconds loses the frame; everything the lost frame
// sent is then sent again.
func (g *gpuEncoder) publish(frame uint64) bool {
	k := int(g.seq % gpuSlots)
	state := g.word(32 + 16*k + 12)
	start := time.Now()
	for atomic.LoadUint32(state) != 0 {
		if waited := time.Since(start); waited > 30*time.Second || !g.ready() {
			g.logOnce("stalled", "the viewer stopped taking frames (frame %d lost)", frame)
			g.resend()
			return false
		}
		time.Sleep(200 * time.Microsecond)
	}
	if waited := time.Since(start); waited > 250*time.Millisecond {
		fmt.Printf("[GPU] waited %v for the viewer before frame %d (shader compiles?)\n", waited.Round(time.Millisecond), frame)
	}
	if n := atomic.LoadUint32(g.word(20)); n != g.lastErr {
		fmt.Printf("[GPU] the viewer reported %d shader/pipeline errors (see its log)\n", n)
		g.lastErr = n
	}
	off := gpuHeaderSize + k*gpuSlotSize
	copy(g.shm[off:], g.buf)
	binary.LittleEndian.PutUint64(g.shm[32+16*k:], frame)
	binary.LittleEndian.PutUint32(g.shm[32+16*k+8:], uint32(len(g.buf)))
	atomic.StoreUint32(state, 1)
	g.seq++
	return true
}

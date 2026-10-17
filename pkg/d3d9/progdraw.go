package d3d9

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3dsm"
)

// Programmable pipeline for draws whose vertex shader is D3D9 shader bytecode (CreateVertexShader
// with a vs_1_1..vs_2_0 token stream, as games ship for effects: sprites, particles, glows). Such
// draws are exact in both renderers: the vertex shader runs per vertex, the pixel shader (or, if
// the bound pixel shader is not bytecode, texture stage 0 modulated by oD0) runs per pixel, and the
// output merger follows the device's render states (blend factors and op, alpha test, depth test
// and write, culling, colour write mask).

// Varying slots carried from the vertex shader to the pixel shader.
const (
	varyD0 = 0 // oD0 -> v0
	varyD1 = 1 // oD1 -> v1
	varyT0 = 2 // oT0..oT7 -> t0..t7
	nVary  = 10
)

// outState is the output-merger state a draw captures.
type outState struct {
	blend          bool
	src, dst, op   uint32
	alphaTest      bool
	alphaRef       float32
	alphaFunc      uint32
	zTest, zWrite  bool
	zFunc          uint32
	cull           uint32
	colorWrite     uint32
	blendFactor    [4]float32
	linear         [8]bool    // bilinear magnification per sampler stage
	clampU, clampV [8]bool    // D3DTADDRESS_CLAMP per sampler stage
	tex            [8]texSnap // textures per sampler stage
}

type progDraw struct {
	vs, ps     *d3dsm.Program
	vsC, psC   []d3dsm.Vec
	elems      []VertexElement
	streams    [16]streamSnap
	order      []uint32
	primType   uint32
	out        outState
	id         int32
	verts      []progVertex
	need       []bool
	varyUsed   uint16 // varying slots the pixel stage reads
	fixedPixel bool   // no bytecode pixel shader: tex0(oT0) * oD0
}

type progVertex struct {
	cx, cy, cz, cw  float32
	sx, sy, sz, inW float32
	vary            [nVary]d3dsm.Vec
	valid           bool
}

// progTri is one triangle of a programmable draw, in screen space.
type progTri struct {
	d *progDraw
	v [3]progVertex
}

func (s *Subsystem) captureOutState() outState {
	rs := s.renderState
	o := outState{
		blend: rs(rsAlphaBlendEnable) != 0, src: rs(rsSrcBlend), dst: rs(rsDestBlend), op: rs(rsBlendOp),
		alphaTest: rs(rsAlphaTestEnable) != 0, alphaRef: float32(rs(rsAlphaRef)&0xFF) / 255, alphaFunc: rs(rsAlphaFunc),
		zTest: rs(rsZEnable) != 0, zWrite: rs(rsZWriteEnable) != 0, zFunc: rs(rsZFunc),
		cull: rs(rsCullMode), colorWrite: rs(rsColorWriteEnable),
	}
	bf := rs(rsBlendFactor)
	o.blendFactor = [4]float32{float32((bf>>16)&0xFF) / 255, float32((bf>>8)&0xFF) / 255, float32(bf&0xFF) / 255, float32(bf>>24) / 255}
	for st := 0; st < 8; st++ {
		o.linear[st] = s.samplerState(st, sampMagFilter) >= 2
		o.clampU[st] = s.samplerState(st, sampAddressU) == 3
		o.clampV[st] = s.samplerState(st, sampAddressV) == 3
	}
	return o
}

// progShading reports whether the current draw goes through the bytecode pipeline.
func (s *Subsystem) progShading() bool {
	return s.curVSProg != nil && s.CurrentVD != nil && len(s.CurrentVD.Elements) > 0
}

// queueProgDraw records a draw for the bytecode pipeline (arguments as for queueRealDraw).
func (s *Subsystem) queueProgDraw(c *cpu.CPU, primType uint32, order []uint32, base0, stride0 uint32, isUP bool) {
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
	d := &progDraw{vs: s.curVSProg, ps: s.curPSProg, elems: s.CurrentVD.Elements, primType: primType,
		out: s.captureOutState(), id: s.drawSeq}
	d.vsC = make([]d3dsm.Vec, max(d.vs.MaxConst, 1))
	copy(d.vsC, s.VSConstants[:])
	d.vs.ApplyDefs(d.vsC)
	samplers := uint32(1)
	if d.ps != nil {
		d.psC = make([]d3dsm.Vec, max(d.ps.MaxConst, 1))
		copy(d.psC, s.PSConstants[:])
		if d.ps.Shader.Major < 2 {
			for i := range d.psC { // ps_1_x constants are clamped to [-1, 1]
				for k := range d.psC[i] {
					d.psC[i][k] = min(max(d.psC[i][k], -1), 1)
				}
			}
		}
		d.ps.ApplyDefs(d.psC)
		samplers = d.ps.Samplers
		d.varyUsed = uint16(d.ps.ReadsV&3) | uint16(d.ps.ReadsT&0xFF)<<varyT0
	} else {
		d.fixedPixel = true
		d.varyUsed = 1<<varyD0 | 1<<varyT0
	}
	for st := 0; st < 8; st++ {
		if samplers&(1<<st) != 0 && s.BoundTextures[st] != 0 {
			d.out.tex[st] = snapTex(s.ensureTextureDecoded(c, s.BoundTextures[st]))
		}
	}
	n := maxI - minI + 1
	for _, e := range d.elems {
		st := e.Stream
		if st > 15 || d.streams[st].data != nil {
			continue
		}
		var addr, stride uint32
		switch {
		case st == 0:
			addr, stride = base0, stride0
		case isUP:
			continue
		default:
			res := s.Resources[s.StreamSources[st]]
			if res == nil {
				continue
			}
			addr, stride = res.DataPtr+s.StreamOffsets[st], s.StreamStrides[st]
		}
		lo := uint64(addr) + uint64(minI)*uint64(stride)
		hi := lo + uint64(n)*uint64(stride) + 16
		if hi > uint64(len(c.Mem)) {
			continue
		}
		d.streams[st] = streamSnap{data: append([]byte(nil), c.Mem[lo:hi]...), stride: stride}
	}
	d.order = make([]uint32, len(order))
	d.need = make([]bool, n)
	for k, i := range order {
		d.order[k] = i - minI
		d.need[i-minI] = true
	}
	d.verts = make([]progVertex, n)
	s.fbDirty = true
	s.frameTris = append(s.frameTris, queuedTri{pdraw: d})
}

// shadeProg runs the vertex shader for every referenced vertex of d.
func shadeProg(d *progDraw, m *d3dsm.Machine) {
	m.C = d.vsC
	for i := range d.verts {
		if !d.need[i] {
			continue
		}
		for r := range m.V {
			m.V[r] = d3dsm.Vec{0, 0, 0, 1}
		}
		for _, e := range d.elems {
			if e.Stream > 15 {
				continue
			}
			sn := &d.streams[e.Stream]
			off := uint32(i)*sn.stride + uint32(e.Offset)
			if sn.data == nil || int(off)+16 > len(sn.data) {
				continue
			}
			for _, in := range d.vs.Inputs {
				if in.Usage == int(e.Usage) && in.UsageIdx == int(e.UIdx) {
					m.V[in.Reg&15] = decodeElementBytes(sn.data[off:], e.Type)
				}
			}
		}
		m.OPos, m.OD, m.OT = d3dsm.Vec{}, [2]d3dsm.Vec{{1, 1, 1, 1}, {0, 0, 0, 0}}, [8]d3dsm.Vec{}
		d.vs.Run(m)
		v := &d.verts[i]
		v.cx, v.cy, v.cz, v.cw = m.OPos[0], m.OPos[1], m.OPos[2], m.OPos[3]
		if v.cx != v.cx || v.cy != v.cy || v.cz != v.cz || v.cw != v.cw {
			continue
		}
		for k := 0; k < 2; k++ { // colour outputs are clamped to [0, 1]
			for j := 0; j < 4; j++ {
				v.vary[varyD0+k][j] = min(max(m.OD[k][j], 0), 1)
			}
		}
		for k := 0; k < 8; k++ {
			v.vary[varyT0+k] = m.OT[k]
		}
		v.valid = true
		if v.cw > nearW {
			projectProg(v)
		}
	}
}

func projectProg(v *progVertex) {
	iw := 1 / v.cw
	v.sx = (v.cx*iw + 1) * 0.5 * float32(ScreenWidth)
	v.sy = (1 - v.cy*iw) * 0.5 * float32(ScreenHeight)
	v.sz = v.cz * iw
	v.inW = iw
}

func lerpProg(a, b *progVertex, t float32) progVertex {
	r := progVertex{cx: a.cx + (b.cx-a.cx)*t, cy: a.cy + (b.cy-a.cy)*t, cz: a.cz + (b.cz-a.cz)*t, cw: a.cw + (b.cw-a.cw)*t, valid: true}
	for k := range r.vary {
		for j := 0; j < 4; j++ {
			r.vary[k][j] = a.vary[k][j] + (b.vary[k][j]-a.vary[k][j])*t
		}
	}
	return r
}

// emitProg clips a triangle against the near plane and queues the pieces.
func emitProg(dst *[]queuedTri, d *progDraw, v0, v1, v2 *progVertex) {
	if !v0.valid || !v1.valid || !v2.valid {
		return
	}
	if v0.cw > nearW && v1.cw > nearW && v2.cw > nearW {
		emitProgScreen(dst, d, v0, v1, v2)
		return
	}
	if v0.cw <= nearW && v1.cw <= nearW && v2.cw <= nearW {
		return
	}
	in := [3]*progVertex{v0, v1, v2}
	var out [4]progVertex
	n := 0
	for i := 0; i < 3; i++ {
		a, b := in[i], in[(i+1)%3]
		da, db := a.cw-nearW, b.cw-nearW
		if da > 0 {
			out[n] = *a
			n++
		}
		if (da > 0) != (db > 0) {
			out[n] = lerpProg(a, b, da/(da-db))
			n++
		}
	}
	for i := 0; i < n; i++ {
		projectProg(&out[i])
	}
	for i := 1; i+1 < n; i++ {
		emitProgScreen(dst, d, &out[0], &out[i], &out[i+1])
	}
}

func emitProgScreen(dst *[]queuedTri, d *progDraw, v0, v1, v2 *progVertex) {
	area := (v1.sx-v0.sx)*(v2.sy-v0.sy) - (v1.sy-v0.sy)*(v2.sx-v0.sx)
	if area > -1e-6 && area < 1e-6 {
		return
	}
	// Screen y points down: clockwise triangles have positive area.
	switch d.out.cull {
	case 2: // D3DCULL_CW
		if area > 0 {
			return
		}
	case 3: // D3DCULL_CCW
		if area < 0 {
			return
		}
	}
	minX := int(math.Floor(float64(min(v0.sx, v1.sx, v2.sx))))
	maxX := int(math.Ceil(float64(max(v0.sx, v1.sx, v2.sx))))
	minY := int(math.Floor(float64(min(v0.sy, v1.sy, v2.sy))))
	maxY := int(math.Ceil(float64(max(v0.sy, v1.sy, v2.sy))))
	if maxX < 0 || minX >= ScreenWidth || maxY < 0 || minY >= ScreenHeight {
		return
	}
	*dst = append(*dst, queuedTri{prog: &progTri{d: d, v: [3]progVertex{*v0, *v1, *v2}},
		minX: max(minX, 0), maxX: min(maxX, ScreenWidth-1), minY: max(minY, 0), maxY: min(maxY, ScreenHeight-1),
		invArea: 1 / area, id: d.id})
}

// expandProg shades the vertices of every programmable draw in ops (in parallel) and replaces each
// draw by its triangles.
func (s *Subsystem) expandProg(ops []queuedTri) []queuedTri {
	var draws []*progDraw
	for i := range ops {
		if ops[i].pdraw != nil {
			draws = append(draws, ops[i].pdraw)
		}
	}
	if len(draws) == 0 {
		return ops
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := &d3dsm.Machine{}
			for {
				k := int(next.Add(1) - 1)
				if k >= len(draws) {
					return
				}
				shadeProg(draws[k], m)
			}
		}()
	}
	wg.Wait()
	out := s.expandedProg[:0]
	for i := range ops {
		d := ops[i].pdraw
		if d == nil {
			out = append(out, ops[i])
			continue
		}
		v, o := d.verts, d.order
		switch d.primType {
		case 4:
			for k := 0; k+2 < len(o); k += 3 {
				emitProg(&out, d, &v[o[k]], &v[o[k+1]], &v[o[k+2]])
			}
		case 5:
			for k := 0; k+2 < len(o); k++ {
				i0, i1, i2 := o[k], o[k+1], o[k+2]
				if i0 == i1 || i1 == i2 || i0 == i2 {
					continue
				}
				if k&1 == 1 {
					emitProg(&out, d, &v[i0], &v[i2], &v[i1])
				} else {
					emitProg(&out, d, &v[i0], &v[i1], &v[i2])
				}
			}
		case 6:
			for k := 1; k+1 < len(o); k++ {
				emitProg(&out, d, &v[o[0]], &v[o[k]], &v[o[k+1]])
			}
		}
	}
	s.expandedProg = out
	return out
}

// sampleTex reads texture stage st at uv (wrap or clamp addressing, point or bilinear).
func sampleTex(o *outState, st int, uv d3dsm.Vec) d3dsm.Vec {
	t := &o.tex[st]
	if len(t.Pixels) == 0 || t.Width == 0 || t.Height == 0 {
		return d3dsm.Vec{1, 1, 1, 1}
	}
	w, h := int(t.Width), int(t.Height)
	addr := func(i, n int, clamp bool) int {
		if clamp {
			return min(max(i, 0), n-1)
		}
		i %= n
		if i < 0 {
			i += n
		}
		return i
	}
	texel := func(x, y int) d3dsm.Vec {
		p := t.Pixels[addr(y, h, o.clampV[st])*w+addr(x, w, o.clampU[st])]
		return d3dsm.Vec{float32(p&0xFF) / 255, float32((p>>8)&0xFF) / 255, float32((p>>16)&0xFF) / 255, float32(p>>24) / 255}
	}
	fu, fv := uv[0]*float32(w), uv[1]*float32(h)
	if !o.linear[st] {
		return texel(int(math.Floor(float64(fu))), int(math.Floor(float64(fv))))
	}
	fu, fv = fu-0.5, fv-0.5
	x0, y0 := math.Floor(float64(fu)), math.Floor(float64(fv))
	ax, ay := fu-float32(x0), fv-float32(y0)
	a, b := texel(int(x0), int(y0)), texel(int(x0)+1, int(y0))
	c, e := texel(int(x0), int(y0)+1), texel(int(x0)+1, int(y0)+1)
	var r d3dsm.Vec
	for k := 0; k < 4; k++ {
		top := a[k] + (b[k]-a[k])*ax
		bot := c[k] + (e[k]-c[k])*ax
		r[k] = top + (bot-top)*ay
	}
	return r
}

func cmpFunc(f uint32, a, b float32) bool {
	switch f {
	case 1:
		return false
	case 2:
		return a < b
	case 3:
		return a == b
	case 4:
		return a <= b
	case 5:
		return a > b
	case 6:
		return a != b
	case 7:
		return a >= b
	}
	return true
}

// blendFactor returns a D3DBLEND factor.
func blendFactor(f uint32, src, dst d3dsm.Vec, bf [4]float32) d3dsm.Vec {
	switch f {
	case 1:
		return d3dsm.Vec{}
	case 2:
		return d3dsm.Vec{1, 1, 1, 1}
	case 3:
		return src
	case 4:
		return d3dsm.Vec{1 - src[0], 1 - src[1], 1 - src[2], 1 - src[3]}
	case 5:
		a := src[3]
		return d3dsm.Vec{a, a, a, a}
	case 6:
		a := 1 - src[3]
		return d3dsm.Vec{a, a, a, a}
	case 7:
		a := dst[3]
		return d3dsm.Vec{a, a, a, a}
	case 8:
		a := 1 - dst[3]
		return d3dsm.Vec{a, a, a, a}
	case 9:
		return dst
	case 10:
		return d3dsm.Vec{1 - dst[0], 1 - dst[1], 1 - dst[2], 1 - dst[3]}
	case 11:
		a := min(src[3], 1-dst[3])
		return d3dsm.Vec{a, a, a, 1}
	case 14:
		return bf
	case 15:
		return d3dsm.Vec{1 - bf[0], 1 - bf[1], 1 - bf[2], 1 - bf[3]}
	}
	return d3dsm.Vec{1, 1, 1, 1}
}

// rasterizeProg draws one programmable triangle's rows within the band.
func (s *Subsystem) rasterizeProg(qt *queuedTri, m *d3dsm.Machine, ids []int32, pix []byte, yStart, yEnd int) {
	pt := qt.prog
	d := pt.d
	o := &d.out
	v0, v1, v2 := &pt.v[0], &pt.v[1], &pt.v[2]
	zbuf := s.ZBuffer
	m.C = d.psC
	m.Sample = func(st int, uv d3dsm.Vec) d3dsm.Vec { return sampleTex(o, st&7, uv) }
	// Perspective-correct varyings: interpolate vary/w and 1/w.
	var pw [3][nVary]d3dsm.Vec
	used := d.varyUsed
	for i, v := range []*progVertex{v0, v1, v2} {
		for k := 0; k < nVary; k++ {
			if used&(1<<k) == 0 {
				continue
			}
			for j := 0; j < 4; j++ {
				pw[i][k][j] = v.vary[k][j] * v.inW
			}
		}
	}
	te, ok := setupEdges(v0.sx, v0.sy, v1.sx, v1.sy, v2.sx, v2.sy)
	if !ok {
		return
	}
	for y := yStart; y <= yEnd; y++ {
		row := y * ScreenWidth
		e, step := te.start(qt.minX, y)
		for x := qt.minX; x <= qt.maxX; x, e[0], e[1], e[2] = x+1, e[0]+step[0], e[1]+step[1], e[2]+step[2] {
			if !te.inside(&e) {
				continue
			}
			w0 := float32(e[0]) * te.inv
			w1 := float32(e[1]) * te.inv
			w2 := 1 - w0 - w1
			idx := row + x
			z := w0*v0.sz + w1*v1.sz + w2*v2.sz
			if o.zTest && !cmpFunc(o.zFunc, z, zbuf[idx]) {
				continue
			}
			iw := 1 / (w0*v0.inW + w1*v1.inW + w2*v2.inW)
			var vary [nVary]d3dsm.Vec
			for k := 0; k < nVary; k++ {
				if used&(1<<k) == 0 {
					continue
				}
				for j := 0; j < 4; j++ {
					vary[k][j] = (w0*pw[0][k][j] + w1*pw[1][k][j] + w2*pw[2][k][j]) * iw
				}
			}
			var col d3dsm.Vec
			if d.fixedPixel {
				col = vary[varyD0]
				if len(o.tex[0].Pixels) > 0 {
					t := sampleTex(o, 0, vary[varyT0])
					col = d3dsm.Vec{col[0] * t[0], col[1] * t[1], col[2] * t[2], col[3] * t[3]}
				}
			} else {
				m.V[0], m.V[1] = vary[varyD0], vary[varyD1]
				copy(m.T[:], vary[varyT0:])
				m.Kill = false
				d.ps.Run(m)
				if m.Kill {
					continue
				}
				col = m.OC[0]
			}
			for j := 0; j < 4; j++ {
				col[j] = min(max(col[j], 0), 1)
			}
			if o.alphaTest && !cmpFunc(o.alphaFunc, float32(math.Round(float64(col[3]*255)))/255, o.alphaRef) {
				continue
			}
			off := idx * 4
			if o.blend {
				dst := d3dsm.Vec{float32(pix[off]) / 255, float32(pix[off+1]) / 255, float32(pix[off+2]) / 255, 1}
				sf := blendFactor(o.src, col, dst, o.blendFactor)
				df := blendFactor(o.dst, col, dst, o.blendFactor)
				for j := 0; j < 3; j++ {
					a, b := col[j]*sf[j], dst[j]*df[j]
					switch o.op {
					case 2:
						col[j] = a - b
					case 3:
						col[j] = b - a
					case 4:
						col[j] = min(col[j], dst[j])
					case 5:
						col[j] = max(col[j], dst[j])
					default:
						col[j] = a + b
					}
					col[j] = min(max(col[j], 0), 1)
				}
			}
			for j := 0; j < 3; j++ {
				if o.colorWrite&(1<<j) != 0 {
					pix[off+j] = uint8(col[j]*255 + 0.5)
				}
			}
			pix[off+3] = 255
			if o.zTest && o.zWrite {
				zbuf[idx] = z
			}
			if ids != nil {
				ids[idx] = d.id
			}
		}
	}
}

// createProg decodes a shader object's token stream (unless it came from D3DXCompileShader) for the
// bytecode pipeline.
func (s *Subsystem) createProg(c *cpu.CPU, obj, fn uint32) {
	if fn == 0 || s.shaderBlobs[fn] != nil || s.stubBlobs[fn] {
		return
	}
	var tok []uint32
	for p := fn; uint64(p)+4 <= uint64(len(c.Mem)) && len(tok) < 1<<16; p += 4 {
		t := c.Read32(p)
		tok = append(tok, t)
		if t == 0x0000FFFF {
			break
		}
	}
	sh, err := d3dsm.Decode(tok)
	if err != nil {
		return
	}
	if p, err := d3dsm.Compile(sh); err == nil {
		s.progObjs[obj] = p
	} else if s.TraceLog {
		fmt.Printf("[D3D9] shader %x: %v\n", obj, err)
	}
}

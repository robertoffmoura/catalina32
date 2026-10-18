package d3d9

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"

	"catalina32/pkg/cpu"
	"catalina32/pkg/hlsl"
)

// Deferred shading for the real shader pipeline: a draw call is recorded on the emulation thread
// (shaders, constant snapshots, vertex data, indices, blend/texture state) and shaded on the
// render goroutine, where all draws of a frame run in parallel and are then assembled into
// triangles in submission order.

type streamSnap struct {
	data   []byte
	stride uint32
}

type drawCmd struct {
	vs, ps         *compiledShader
	vsRegs, psRegs [][4]float32
	elems          []VertexElement
	streams        [16]streamSnap
	order          []uint32 // vertex indices in primitive order, relative to minIndex
	primType       uint32
	primCount      uint32
	tri            triState
	verts          []screenVertex // shaded vertices by relative index
	colors         [][4]float32   // unclamped shader colour per vertex
	over           bool           // some colour exceeds 1: rasterize with the 2x rule (see expandDraws)
	need           []bool
}

// maxFloatReg is one past the highest float constant register a program reads.
func maxFloatReg(p *hlsl.Program) int {
	n := 0
	for _, k := range p.Constants {
		if k.RegisterSet == hlsl.RegFloat4 {
			n = max(n, k.RegisterIndex+k.RegisterCount)
		}
	}
	return n
}

func snapRegs(regs *[256][4]float32, n int) [][4]float32 {
	out := make([][4]float32, min(n, 256))
	copy(out, regs[:len(out)])
	return out
}

// queueRealDraw records a draw for deferred shading. order holds absolute vertex indices in
// primitive order; vertex data for stream 0 starts at base0 (the address of vertex 0).
func (s *Subsystem) queueRealDraw(c *cpu.CPU, primType, primCount uint32, order []uint32, base0, stride0 uint32, isUP bool, texRes *BufferDesc) {
	if len(order) == 0 {
		return
	}
	minI, maxI := order[0], order[0]
	for _, i := range order {
		minI, maxI = min(minI, i), max(maxI, i)
	}
	if maxI-minI > 1<<20 {
		return // corrupt index data
	}
	cmd := &drawCmd{vs: s.curVS, ps: s.curPS, elems: s.CurrentVD.Elements, primType: primType, primCount: primCount,
		tri: s.curTriState(texRes, nil)}
	if cmd.vs.maxReg < 0 {
		cmd.vs.maxReg = maxFloatReg(cmd.vs.prog)
	}
	cmd.vsRegs = snapRegs(&s.VSConstants, cmd.vs.maxReg)
	if cmd.ps != nil {
		if cmd.ps.maxReg < 0 {
			cmd.ps.maxReg = maxFloatReg(cmd.ps.prog)
		}
		cmd.psRegs = snapRegs(&s.PSConstants, cmd.ps.maxReg)
	}
	n := maxI - minI + 1
	for _, e := range cmd.elems {
		st := e.Stream
		if st > 15 || cmd.streams[st].data != nil {
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
		hi := lo + uint64(n)*uint64(stride) + 16 // room for the widest element read
		if hi > uint64(len(c.Mem)) {
			continue
		}
		cmd.streams[st] = streamSnap{data: append([]byte(nil), c.Mem[lo:hi]...), stride: stride}
	}
	cmd.order = make([]uint32, len(order))
	for k, i := range order {
		cmd.order[k] = i - minI
	}
	cmd.verts = make([]screenVertex, n)
	cmd.colors = make([][4]float32, n)
	cmd.need = make([]bool, n)
	for _, i := range cmd.order {
		cmd.need[i] = true
	}
	s.fbDirty = true
	s.frameTris = append(s.frameTris, queuedTri{draw: cmd})
}

// decodeElementBytes reads one vertex element (D3DDECLTYPE) as up to four floats.
func decodeElementBytes(b []byte, typ uint8) [4]float32 {
	v := [4]float32{0, 0, 0, 1}
	le := binary.LittleEndian
	switch typ {
	case 0, 1, 2, 3: // FLOAT1..4
		for i := 0; i <= int(typ); i++ {
			v[i] = math.Float32frombits(le.Uint32(b[4*i:]))
		}
	case 4: // D3DCOLOR (BGRA in memory) -> rgba
		v = [4]float32{float32(b[2]) / 255, float32(b[1]) / 255, float32(b[0]) / 255, float32(b[3]) / 255}
	case 5: // UBYTE4
		v = [4]float32{float32(b[0]), float32(b[1]), float32(b[2]), float32(b[3])}
	case 8: // UBYTE4N
		v = [4]float32{float32(b[0]) / 255, float32(b[1]) / 255, float32(b[2]) / 255, float32(b[3]) / 255}
	case 6, 7: // SHORT2, SHORT4
		for i := 0; i < 2+2*int(typ-6); i++ {
			v[i] = float32(int16(le.Uint16(b[2*i:])))
		}
	case 9, 10: // SHORT2N, SHORT4N
		for i := 0; i < 2+2*int(typ-9); i++ {
			v[i] = max(float32(int16(le.Uint16(b[2*i:])))/32767, -1)
		}
	case 11, 12: // USHORT2N, USHORT4N
		for i := 0; i < 2+2*int(typ-11); i++ {
			v[i] = float32(le.Uint16(b[2*i:])) / 65535
		}
	case 13, 14: // UDEC3, DEC3N
		w := le.Uint32(b)
		for i := uint32(0); i < 3; i++ {
			x := (w >> (10 * i)) & 0x3FF
			if typ == 13 {
				v[i] = float32(x)
			} else {
				v[i] = float32(int32(x<<22)>>22) / 511
			}
		}
	case 15, 16: // FLOAT16_2, FLOAT16_4
		for i := 0; i < 2+2*int(typ-15); i++ {
			v[i] = halfToFloat(le.Uint16(b[2*i:]))
		}
	}
	return v
}

// shadeWorker owns one evaluation State per program.
type shadeWorker struct {
	states map[*compiledShader]*hlsl.State
}

func (w *shadeWorker) state(cs *compiledShader) *hlsl.State {
	st := w.states[cs]
	if st == nil {
		st = cs.prog.NewState()
		w.states[cs] = st
	}
	return st
}

// shade runs the draw's vertex shader (and, per vertex, its pixel shader) over the vertices the
// primitives use, hlsl.Lanes vertices at a time.
func (w *shadeWorker) shade(cmd *drawCmd) {
	vs, ps := cmd.vs, cmd.ps
	vst := w.state(vs)
	vst.LoadConstants(cmd.vsRegs)
	var pst *hlsl.State
	if ps != nil {
		pst = w.state(ps)
		pst.LoadConstants(cmd.psRegs)
	}
	// Each vertex element feeds the vertex shader inputs with its semantic.
	type binding struct {
		e     VertexElement
		slots []*hlsl.Varying
	}
	var binds []binding
	for _, e := range cmd.elems {
		if int(e.Usage) >= len(usageSemantic) || e.Stream > 15 || cmd.streams[e.Stream].data == nil {
			continue
		}
		b := binding{e: e}
		for _, in := range vs.prog.Inputs {
			if in.Semantic == usageSemantic[e.Usage] && in.Index == int(e.UIdx) {
				b.slots = append(b.slots, in)
			}
		}
		if len(b.slots) > 0 {
			binds = append(binds, b)
		}
	}
	// Pixel shader inputs come from the vertex shader outputs with the same semantic.
	type link struct{ from, to, n int }
	var links []link
	if ps != nil {
		for _, in := range ps.prog.Inputs {
			if out := vs.prog.Output(in.Semantic, in.Index); out != nil {
				links = append(links, link{out.Offset, in.Offset, min(in.Components, out.Components)})
			}
		}
	}
	pos := vs.prog.Output("POSITION", 0)
	var lanes [hlsl.Lanes]int // vertex index per lane
	n := 0
	flush := func() {
		vst.Run(n)
		if ps != nil {
			for _, k := range links {
				copy(pst.M[k.to:k.to+k.n], vst.M[k.from:k.from+k.n])
			}
			pst.Run(n)
		}
		for l, i := range lanes[:n] {
			cmd.verts[i], cmd.colors[i] = finishVertex(vs, ps, vst, pst, pos, l)
			for _, c := range cmd.colors[i] {
				if c > 1.001 {
					cmd.over = true
				}
			}
		}
		n = 0
	}
	for i := range cmd.verts {
		if !cmd.need[i] {
			continue
		}
		for _, b := range binds {
			sn := &cmd.streams[b.e.Stream]
			off := uint32(i)*sn.stride + uint32(b.e.Offset)
			if int(off)+16 > len(sn.data) {
				continue
			}
			v := decodeElementBytes(sn.data[off:], b.e.Type)
			for _, in := range b.slots {
				for k := 0; k < min(in.Components, 4); k++ {
					vst.M[in.Offset+k][n] = v[k]
				}
			}
		}
		lanes[n] = i
		n++
		if n == hlsl.Lanes {
			flush()
		}
	}
	if n > 0 {
		flush()
	}
}

// finishVertex turns lane l's vertex shader outputs (and pixel shader result) into a clip-space
// screen vertex.
func finishVertex(vs, ps *compiledShader, vst, pst *hlsl.State, pos *hlsl.Varying, l int) (screenVertex, [4]float32) {
	raw := [4]float32{1, 1, 1, 1}
	if pos == nil || pos.Components < 4 {
		return screenVertex{}, raw
	}
	m := vst.M
	sv := screenVertex{cx: m[pos.Offset][l], cy: m[pos.Offset+1][l], cz: m[pos.Offset+2][l], cw: m[pos.Offset+3][l],
		clip: true, valid: true, r: 255, g: 255, b: 255, a: 255}
	if sv.cx != sv.cx || sv.cy != sv.cy || sv.cz != sv.cz || sv.cw != sv.cw {
		return screenVertex{}, raw
	}
	var color *hlsl.Varying
	cm := m
	if ps != nil {
		if tc, n, ok := pst.TexCoord(ps.texUnit, l); ok && n >= 2 {
			sv.u, sv.v = tc[0], tc[1]
		}
		color, cm = ps.prog.Output("COLOR", 0), pst.M
	} else {
		color = vs.prog.Output("COLOR", 0)
		if out := vs.prog.Output("TEXCOORD", 0); out != nil && out.Components >= 2 {
			sv.u, sv.v = m[out.Offset][l], m[out.Offset+1][l]
		}
	}
	if color != nil && color.Components >= 4 {
		for k := range raw {
			raw[k] = cm[color.Offset+k][l]
		}
		sv.r, sv.g, sv.b, sv.a = clampByte(raw[0]), clampByte(raw[1]), clampByte(raw[2]), clampByte(raw[3])
	}
	if sv.cw > nearW {
		projectClip(&sv)
	}
	return sv, raw
}

// expandDraws shades every recorded draw in ops (in parallel) and returns the operation list
// with each draw replaced by its triangles, in submission order.
func (s *Subsystem) expandDraws(ops []queuedTri) []queuedTri {
	var cmds []*drawCmd
	for i := range ops {
		if ops[i].draw != nil {
			cmds = append(cmds, ops[i].draw)
		}
	}
	if len(cmds) == 0 {
		return ops
	}
	if len(s.shadeWorkers) == 0 {
		for i := 0; i < renderWorkers; i++ {
			s.shadeWorkers = append(s.shadeWorkers, &shadeWorker{states: map[*compiledShader]*hlsl.State{}})
		}
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for _, w := range s.shadeWorkers {
		wg.Add(1)
		go func(w *shadeWorker) {
			defer wg.Done()
			for {
				k := int(next.Add(1) - 1)
				if k >= len(cmds) {
					return
				}
				w.shade(cmds[k])
			}
		}(w)
	}
	wg.Wait()

	out := s.expanded[:0]
	for i := range ops {
		cmd := ops[i].draw
		if cmd == nil {
			out = append(out, ops[i])
			continue
		}
		v, o := cmd.verts, cmd.order
		if cmd.over {
			// The pixel shader's output exceeds 1 before the texture multiply (e.g. the engine's
			// 2 x vertex colour): keep half the colour and double after texturing, per pixel.
			for i := range v {
				c := cmd.colors[i]
				v[i].r, v[i].g, v[i].b, v[i].a = clampByte(c[0]/2), clampByte(c[1]/2), clampByte(c[2]/2), clampByte(c[3]/2)
			}
			cmd.tri.x2 = true
		}
		switch cmd.primType {
		case 4: // TRIANGLELIST
			for k := 0; k+2 < len(o); k += 3 {
				emitTriangle(&out, v[o[k]], v[o[k+1]], v[o[k+2]], &cmd.tri)
			}
		case 5: // TRIANGLESTRIP (degenerate triangles skipped)
			for k := 0; k+2 < len(o); k++ {
				i0, i1, i2 := o[k], o[k+1], o[k+2]
				if i0 == i1 || i1 == i2 || i0 == i2 {
					continue
				}
				if k&1 == 1 {
					emitTriangle(&out, v[i0], v[i2], v[i1], &cmd.tri)
				} else {
					emitTriangle(&out, v[i0], v[i1], v[i2], &cmd.tri)
				}
			}
		case 6: // TRIANGLEFAN
			for k := 1; k+1 < len(o); k++ {
				emitTriangle(&out, v[o[0]], v[o[k]], v[o[k+1]], &cmd.tri)
			}
		}
	}
	s.expanded = out
	return out
}

package d3dsm

import "math"

// Lanes is the number of invocations a WideMachine runs together.
const Lanes = 32

// Lane holds one register component for every lane.
type Lane = [Lanes]float32

// WideMachine runs a pixel shader for up to Lanes pixels at once: registers are stored per
// component across lanes, so each instruction is decoded once per batch and executes as tight
// loops. Results are identical to Machine (the same float operations per lane). Only the
// components an instruction writes are computed.
type WideMachine struct {
	C    []Vec // float constants
	F    [fileLen][4]Lane
	Kill [Lanes]bool
	N    int // active lanes
	// Sample returns the texel of sampler stage s at texture coordinate uv.
	Sample func(s int, uv Vec) Vec
	// SampleLanes, if set, samples stage s at the coordinates uv[0], uv[1] of lanes 0..n-1 into out.
	SampleLanes func(s int, uv *[4]Lane, n int, out *[4]Lane)

	tmp [3][4]Lane // source operands that need a copy (constants, modifiers)
	res [4]Lane
}

// Wide reports whether RunWide can execute the program (pixel shaders without relative
// addressing and with the common instructions; others run per pixel on a Machine).
func (p *Program) Wide() bool { return p.wide }

// InputComps is the mask of components of input register F[reg] (FileV+i or FileT+i) that RunWide
// reads; other components need not be set.
func (p *Program) InputComps(reg int) uint8 {
	if !p.wide {
		return 15
	}
	return p.inComps[reg]
}

// wideOps are the instructions RunWide implements.
var wideOps = map[int]bool{OpMov: true, OpAdd: true, OpSub: true, OpMul: true, OpMad: true, OpMin: true,
	OpMax: true, OpSlt: true, OpSge: true, OpLrp: true, OpCmp: true, OpCnd: true, OpFrc: true, OpAbs: true,
	OpDp3: true, OpDp4: true, OpDp2Add: true, OpRcp: true, OpRsq: true, OpExp: true, OpLog: true,
	OpPow: true, OpNrm: true, OpCrs: true, OpTexCoord: true, OpTex: true, OpTexKill: true}

// checkWide decides whether RunWide can run the program and, if so, which source components each
// instruction needs (p.need) and which input components the program reads (p.inComps).
func (p *Program) checkWide() {
	if !p.Shader.Pixel {
		return
	}
	for i := range p.code {
		in := &p.code[i]
		if !wideOps[in.op] || in.dst.a0 {
			return
		}
		for _, s := range in.src {
			if s.rel {
				return
			}
		}
	}
	p.wide = true
	p.need = make([][3]uint8, len(p.code))
	for i := range p.code {
		in := &p.code[i]
		nd := &p.need[i]
		m := in.dst.mask
		switch in.op {
		case OpCnd:
			*nd = [3]uint8{m, m, m}
			if in.cnd1 {
				nd[0] = 8
			}
		case OpDp3, OpCrs:
			*nd = [3]uint8{7, 7}
		case OpDp4:
			*nd = [3]uint8{15, 15}
		case OpDp2Add:
			*nd = [3]uint8{3, 3, 8}
		case OpRcp, OpRsq, OpExp, OpLog, OpPow:
			*nd = [3]uint8{8, 8}
		case OpNrm:
			nd[0] = 7 | m&8
		case OpTex:
			if p.Shader.Major >= 2 {
				nd[0] = 3 // samplers read u, v
				if in.proj {
					nd[0] = 15
				}
			} else {
				p.inComps[FileT+in.n] = 15
			}
		case OpTexCoord:
			p.inComps[FileT+in.n] = 15
		case OpTexKill:
			p.inComps[in.src[0].idx] |= 1<<in.n - 1
		default: // per-component instructions
			*nd = [3]uint8{m, m, m}
		}
		for k, s := range in.src {
			if s.konst || nd[k] == 0 {
				continue
			}
			if s.mod == 9 || s.mod == 10 || s.mod > 12 { // whole-vector modifiers
				nd[k] = 15
			}
			for c := 0; c < 4; c++ {
				if nd[k]&(1<<c) != 0 {
					p.inComps[s.idx] |= 1 << s.sw[c]
				}
			}
		}
	}
}

// operand returns the component lanes of a source, after swizzle and modifier; only the
// components in need are valid.
func (m *WideMachine) operand(o *operand, slot int, need uint8) (r [4]*Lane) {
	n := m.N
	if !o.konst && o.mod == 0 {
		f := &m.F[o.idx]
		return [4]*Lane{&f[o.sw[0]], &f[o.sw[1]], &f[o.sw[2]], &f[o.sw[3]]}
	}
	t := &m.tmp[slot]
	r = [4]*Lane{&t[0], &t[1], &t[2], &t[3]}
	if o.konst {
		v := zeroVec
		if o.idx < len(m.C) {
			v = m.C[o.idx]
		}
		v = Vec{v[o.sw[0]], v[o.sw[1]], v[o.sw[2]], v[o.sw[3]]}
		if o.mod != 0 {
			v = applyMod(v, o.mod)
		}
		for c := 0; c < 4; c++ {
			if need&(1<<c) != 0 {
				tc := t[c][:n]
				for i := range tc {
					tc[i] = v[c]
				}
			}
		}
		return r
	}
	f := &m.F[o.idx]
	if o.mod == 9 || o.mod == 10 || o.mod > 12 { // whole-vector modifiers
		for i := 0; i < n; i++ {
			v := applyMod(Vec{f[o.sw[0]][i], f[o.sw[1]][i], f[o.sw[2]][i], f[o.sw[3]][i]}, o.mod)
			t[0][i], t[1][i], t[2][i], t[3][i] = v[0], v[1], v[2], v[3]
		}
		return r
	}
	for c := 0; c < 4; c++ {
		if need&(1<<c) == 0 {
			continue
		}
		tc := t[c][:n]
		sc := f[o.sw[c]][:len(tc)]
		switch o.mod {
		case 1:
			for i := range tc {
				tc[i] = -sc[i]
			}
		case 2:
			for i := range tc {
				tc[i] = sc[i] - 0.5
			}
		case 3:
			for i := range tc {
				tc[i] = 0.5 - sc[i]
			}
		case 4:
			for i := range tc {
				tc[i] = 2*sc[i] - 1
			}
		case 5:
			for i := range tc {
				tc[i] = 1 - 2*sc[i]
			}
		case 6:
			for i := range tc {
				tc[i] = 1 - sc[i]
			}
		case 7:
			for i := range tc {
				tc[i] = 2 * sc[i]
			}
		case 8:
			for i := range tc {
				tc[i] = -2 * sc[i]
			}
		case 11:
			for i := range tc {
				tc[i] = abs(sc[i])
			}
		case 12:
			for i := range tc {
				tc[i] = -abs(sc[i])
			}
		}
	}
	return r
}

// store writes the result through the destination's mask, shift and saturate: component c
// comes from m.res[c], or from m.res[0] for scalar results.
func (m *WideMachine) store(d *dest, scalar bool) {
	n := m.N
	f := &m.F[d.idx]
	for c := 0; c < 4; c++ {
		if d.mask&(1<<c) == 0 {
			continue
		}
		src := &m.res[c]
		if scalar {
			src = &m.res[0]
		}
		dst := f[c][:n]
		r := src[:len(dst)]
		switch {
		case d.scale != 1 && d.sat:
			for i := range dst {
				dst[i] = min(max(r[i]*d.scale, 0), 1)
			}
		case d.scale != 1:
			for i := range dst {
				dst[i] = r[i] * d.scale
			}
		case d.sat:
			for i := range dst {
				dst[i] = min(max(r[i], 0), 1)
			}
		default:
			copy(dst, r)
		}
	}
}

// RunWide executes the program for lanes 0..m.N-1 (Wide must be true).
func (p *Program) RunWide(m *WideMachine) {
	n := m.N
	for k := range p.code {
		in := &p.code[k]
		d := &in.dst
		nd := &p.need[k]
		mask := d.mask
		res := &m.res
		scalar := false
		switch in.op {
		case OpMov:
			a := m.operand(&in.src[0], 0, nd[0])
			for c := 0; c < 4; c++ {
				if mask&(1<<c) != 0 {
					copy(res[c][:n], a[c][:n])
				}
			}
		case OpAdd, OpSub, OpMul, OpMin, OpMax, OpSlt, OpSge:
			a, b := m.operand(&in.src[0], 0, nd[0]), m.operand(&in.src[1], 1, nd[1])
			for c := 0; c < 4; c++ {
				if mask&(1<<c) == 0 {
					continue
				}
				rc := res[c][:n]
				ac, bc := a[c][:len(rc)], b[c][:len(rc)]
				switch in.op {
				case OpAdd:
					for i := range rc {
						rc[i] = ac[i] + bc[i]
					}
				case OpSub:
					for i := range rc {
						rc[i] = ac[i] - bc[i]
					}
				case OpMul:
					for i := range rc {
						rc[i] = ac[i] * bc[i]
					}
				case OpMin:
					for i := range rc {
						rc[i] = min(ac[i], bc[i])
					}
				case OpMax:
					for i := range rc {
						rc[i] = max(ac[i], bc[i])
					}
				case OpSlt:
					for i := range rc {
						rc[i] = b2f(ac[i] < bc[i])
					}
				case OpSge:
					for i := range rc {
						rc[i] = b2f(ac[i] >= bc[i])
					}
				}
			}
		case OpMad, OpLrp, OpCmp:
			a, b, cc := m.operand(&in.src[0], 0, nd[0]), m.operand(&in.src[1], 1, nd[1]), m.operand(&in.src[2], 2, nd[2])
			for c := 0; c < 4; c++ {
				if mask&(1<<c) == 0 {
					continue
				}
				rc := res[c][:n]
				ac, bc, xc := a[c][:len(rc)], b[c][:len(rc)], cc[c][:len(rc)]
				switch in.op {
				case OpMad:
					for i := range rc {
						rc[i] = ac[i]*bc[i] + xc[i]
					}
				case OpLrp:
					for i := range rc {
						rc[i] = ac[i]*(bc[i]-xc[i]) + xc[i]
					}
				case OpCmp:
					for i := range rc {
						if ac[i] >= 0 {
							rc[i] = bc[i]
						} else {
							rc[i] = xc[i]
						}
					}
				}
			}
		case OpCnd:
			a, b, cc := m.operand(&in.src[0], 0, nd[0]), m.operand(&in.src[1], 1, nd[1]), m.operand(&in.src[2], 2, nd[2])
			for c := 0; c < 4; c++ {
				if mask&(1<<c) == 0 {
					continue
				}
				sel := a[c]
				if in.cnd1 {
					sel = a[3]
				}
				rc := res[c][:n]
				sc, bc, xc := sel[:len(rc)], b[c][:len(rc)], cc[c][:len(rc)]
				for i := range rc {
					if sc[i] > 0.5 {
						rc[i] = bc[i]
					} else {
						rc[i] = xc[i]
					}
				}
			}
		case OpFrc, OpAbs:
			a := m.operand(&in.src[0], 0, nd[0])
			for c := 0; c < 4; c++ {
				if mask&(1<<c) == 0 {
					continue
				}
				rc := res[c][:n]
				ac := a[c][:len(rc)]
				for i := range rc {
					if in.op == OpFrc {
						rc[i] = ac[i] - float32(math.Floor(float64(ac[i])))
					} else {
						rc[i] = abs(ac[i])
					}
				}
			}
		case OpDp3, OpDp4:
			a, b := m.operand(&in.src[0], 0, nd[0]), m.operand(&in.src[1], 1, nd[1])
			r := &res[0]
			for i := 0; i < n; i++ {
				r[i] = a[0][i]*b[0][i] + a[1][i]*b[1][i] + a[2][i]*b[2][i]
			}
			if in.op == OpDp4 {
				for i := 0; i < n; i++ {
					r[i] += a[3][i] * b[3][i]
				}
			}
			scalar = true
		case OpDp2Add:
			a, b, cc := m.operand(&in.src[0], 0, nd[0]), m.operand(&in.src[1], 1, nd[1]), m.operand(&in.src[2], 2, nd[2])
			r := &res[0]
			for i := 0; i < n; i++ {
				r[i] = a[0][i]*b[0][i] + a[1][i]*b[1][i] + cc[3][i]
			}
			scalar = true
		case OpRcp, OpRsq, OpExp, OpLog:
			x, r := m.operand(&in.src[0], 0, nd[0])[3], &res[0]
			for i := 0; i < n; i++ {
				switch in.op {
				case OpRcp:
					r[i] = rcp(x[i])
				case OpRsq:
					r[i] = rcp(float32(math.Sqrt(math.Abs(float64(x[i])))))
				case OpExp:
					r[i] = float32(math.Exp2(float64(x[i])))
				case OpLog:
					if x[i] == 0 {
						r[i] = float32(math.Inf(-1))
					} else {
						r[i] = float32(math.Log2(math.Abs(float64(x[i]))))
					}
				}
			}
			scalar = true
		case OpPow:
			a, b := m.operand(&in.src[0], 0, nd[0])[3], m.operand(&in.src[1], 1, nd[1])[3]
			r := &res[0]
			for i := 0; i < n; i++ {
				r[i] = float32(math.Pow(math.Abs(float64(a[i])), float64(b[i])))
			}
			scalar = true
		case OpNrm:
			a := m.operand(&in.src[0], 0, nd[0])
			for i := 0; i < n; i++ {
				x, y, z := a[0][i], a[1][i], a[2][i]
				l := rcp(float32(math.Sqrt(float64(x*x + y*y + z*z))))
				res[0][i], res[1][i], res[2][i], res[3][i] = x*l, y*l, z*l, a[3][i]*l
			}
		case OpCrs:
			a, b := m.operand(&in.src[0], 0, nd[0]), m.operand(&in.src[1], 1, nd[1])
			for i := 0; i < n; i++ {
				ax, ay, az := a[0][i], a[1][i], a[2][i]
				bx, by, bz := b[0][i], b[1][i], b[2][i]
				res[0][i], res[1][i], res[2][i], res[3][i] = ay*bz-az*by, az*bx-ax*bz, ax*by-ay*bx, 0
			}
		case OpTexCoord:
			t := &m.F[FileT+in.n]
			for i := 0; i < n; i++ {
				t[0][i], t[1][i], t[2][i], t[3][i] = min(max(t[0][i], 0), 1), min(max(t[1][i], 0), 1), min(max(t[2][i], 0), 1), 1
			}
			continue
		case OpTex:
			if p.Shader.Major < 2 {
				t := &m.F[FileT+in.n]
				for i := 0; i < n; i++ {
					v := m.Sample(in.n, Vec{t[0][i], t[1][i], t[2][i], t[3][i]})
					t[0][i], t[1][i], t[2][i], t[3][i] = v[0], v[1], v[2], v[3]
				}
				continue
			}
			a := m.operand(&in.src[0], 0, nd[0])
			if m.SampleLanes != nil && !in.proj {
				uv := &m.tmp[1]
				copy(uv[0][:n], a[0][:n])
				copy(uv[1][:n], a[1][:n])
				m.SampleLanes(in.n, uv, n, res)
				break
			}
			for i := 0; i < n; i++ {
				x, y, z, w := a[0][i], a[1][i], a[2][i], a[3][i]
				if in.proj && w != 0 {
					x, y, z, w = x/w, y/w, z/w, 1
				}
				v := m.Sample(in.n, Vec{x, y, z, w})
				res[0][i], res[1][i], res[2][i], res[3][i] = v[0], v[1], v[2], v[3]
			}
		case OpTexKill:
			t := &m.F[in.src[0].idx]
			for c := 0; c < in.n; c++ {
				for i := 0; i < n; i++ {
					if t[c][i] < 0 {
						m.Kill[i] = true
					}
				}
			}
			continue
		}
		m.store(d, scalar)
	}
	if p.ps1 {
		m.F[FileOC] = m.F[FileR]
	}
}

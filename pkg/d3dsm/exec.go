package d3dsm

import (
	"fmt"
	"math"
)

// Vec is one four-component register.
type Vec = [4]float32

// Machine holds the register state of one shader invocation. Callers fill the inputs (V for
// vertex shaders; V = colours and T = texture coordinates for pixel shaders), run a Program,
// and read the outputs.
type Machine struct {
	C    []Vec // float constants (the draw's snapshot, with the shader's def values applied)
	R    [32]Vec
	V    [16]Vec
	T    [8]Vec // ps: texture registers / texture coordinate inputs
	A0   [4]int32
	OPos Vec
	OD   [2]Vec // vs colour outputs
	OT   [8]Vec // vs texture coordinate outputs
	OFog Vec
	OPts Vec
	OC   [4]Vec // ps colour outputs (ps_1_x: r0 is copied to OC[0])
	Kill bool
	// Sample returns the texel of sampler stage s at texture coordinate uv.
	Sample func(s int, uv Vec) Vec
}

// Program is a shader ready to execute: one closure per instruction.
type Program struct {
	Shader *Shader
	steps  []func(m *Machine)
	defs   []def
	// Inputs lists the declared vertex shader inputs (v# with usage) or pixel shader inputs.
	Inputs []Decl
	// MaxConst is one past the highest float constant register read.
	MaxConst int
	// Samplers is a bit mask of the sampler stages read.
	Samplers uint32
	// ReadsV and ReadsT are bit masks of the input (v#) and texture (t#) registers read: for a
	// pixel shader, the interpolated colours and texture coordinates it needs.
	ReadsV, ReadsT uint32
}

// Decl is a dcl'd input register.
type Decl struct {
	Reg, Usage, UsageIdx int
}

type def struct {
	reg int
	v   Vec
}

// ApplyDefs writes the shader's def constants into a constant snapshot.
func (p *Program) ApplyDefs(c []Vec) {
	for _, d := range p.defs {
		if d.reg < len(c) {
			c[d.reg] = d.v
		}
	}
}

// Run executes the program on m.
func (p *Program) Run(m *Machine) {
	for _, f := range p.steps {
		f(m)
	}
	if p.Shader.Pixel && p.Shader.Major < 2 {
		m.OC[0] = m.R[0]
	}
}

// Compile prepares a decoded shader for execution.
func Compile(sh *Shader) (*Program, error) {
	p := &Program{Shader: sh}
	for i := range sh.Insts {
		in := &sh.Insts[i]
		switch in.Op {
		case OpDcl:
			if in.Dst.Type == RegInput || (sh.Pixel && in.Dst.Type == RegAddrTex) {
				p.Inputs = append(p.Inputs, Decl{in.Dst.Num, in.Usage, in.UsageIdx})
			}
			continue
		case OpDef:
			p.defs = append(p.defs, def{in.Dst.Num, Vec{f32(in.Imm[0]), f32(in.Imm[1]), f32(in.Imm[2]), f32(in.Imm[3])}})
			p.MaxConst = max(p.MaxConst, in.Dst.Num+1)
			continue
		case OpDefI, OpDefB, OpNop:
			continue
		}
		for _, s := range in.Src {
			if s.Type == RegConst {
				n := s.Num + 1
				if s.Relative {
					n = 256
				}
				if in.Op >= OpM4x4 && in.Op <= OpM3x2 {
					n += 3
				}
				p.MaxConst = max(p.MaxConst, min(n, 256))
			}
			if s.Type == RegSampler {
				p.Samplers |= 1 << s.Num
			}
			if s.Type == RegInput {
				p.ReadsV |= 1 << (s.Num & 31)
			}
			if s.Type == RegAddrTex && sh.Pixel {
				p.ReadsT |= 1 << (s.Num & 7)
			}
		}
		if sh.Pixel && sh.Major < 2 && (in.Op == OpTex || in.Op == OpTexCoord) {
			p.ReadsT |= 1 << (in.Dst.Num & 7)
			if in.Op == OpTex {
				p.Samplers |= 1 << in.Dst.Num
			}
		}
		f, err := p.compileInst(in)
		if err != nil {
			return nil, err
		}
		p.steps = append(p.steps, f)
	}
	return p, nil
}

// reg returns a pointer to a register.
func (p *Program) reg(m *Machine, t, n int) *Vec {
	switch t {
	case RegTemp:
		return &m.R[n&31]
	case RegInput:
		return &m.V[n&15]
	case RegConst:
		if n < len(m.C) {
			return &m.C[n]
		}
		return &zeroVec
	case RegAddrTex:
		if p.Shader.Pixel {
			return &m.T[n&7]
		}
		return &scratch // a0 is written through mova/mov handling
	case RegRastOut:
		switch n {
		case 0:
			return &m.OPos
		case 1:
			return &m.OFog
		}
		return &m.OPts
	case RegAttrOut:
		return &m.OD[n&1]
	case RegTexCrdOut:
		return &m.OT[n&7]
	case RegColorOut:
		return &m.OC[n&3]
	}
	return &scratch
}

var zeroVec, scratch Vec

func (p *Program) fetch(s Param) func(m *Machine) Vec {
	sw := s.Swizzle
	mod := s.SrcMod
	rel := s.Relative && s.Type == RegConst
	t, n, rc := s.Type, s.Num, s.RelComp
	return func(m *Machine) Vec {
		var r *Vec
		if rel {
			i := n + int(m.A0[rc])
			if i >= 0 && i < len(m.C) {
				r = &m.C[i]
			} else {
				r = &zeroVec
			}
		} else {
			r = p.reg(m, t, n)
		}
		v := Vec{r[sw[0]], r[sw[1]], r[sw[2]], r[sw[3]]}
		switch mod {
		case 0:
		case 1:
			v = Vec{-v[0], -v[1], -v[2], -v[3]}
		case 2:
			v = Vec{v[0] - 0.5, v[1] - 0.5, v[2] - 0.5, v[3] - 0.5}
		case 3:
			v = Vec{0.5 - v[0], 0.5 - v[1], 0.5 - v[2], 0.5 - v[3]}
		case 4:
			v = Vec{2*v[0] - 1, 2*v[1] - 1, 2*v[2] - 1, 2*v[3] - 1}
		case 5:
			v = Vec{1 - 2*v[0], 1 - 2*v[1], 1 - 2*v[2], 1 - 2*v[3]}
		case 6:
			v = Vec{1 - v[0], 1 - v[1], 1 - v[2], 1 - v[3]}
		case 7:
			v = Vec{2 * v[0], 2 * v[1], 2 * v[2], 2 * v[3]}
		case 8:
			v = Vec{-2 * v[0], -2 * v[1], -2 * v[2], -2 * v[3]}
		case 9: // dz: divide by z
			v = Vec{v[0] / v[2], v[1] / v[2], v[2], v[3]}
		case 10: // dw
			v = Vec{v[0] / v[3], v[1] / v[3], v[2], v[3]}
		case 11:
			v = Vec{abs(v[0]), abs(v[1]), abs(v[2]), abs(v[3])}
		case 12:
			v = Vec{-abs(v[0]), -abs(v[1]), -abs(v[2]), -abs(v[3])}
		}
		return v
	}
}

func abs(f float32) float32 { return float32(math.Abs(float64(f))) }

// store writes a result through the destination's mask, shift and saturate.
func (p *Program) store(d Param) func(m *Machine, v Vec) {
	mask, sat := d.Mask, d.Saturate
	scale := float32(math.Ldexp(1, d.Shift))
	t, n := d.Type, d.Num
	if t == RegAddrTex && !p.Shader.Pixel { // a0
		floor := p.Shader.Major < 2
		return func(m *Machine, v Vec) {
			for i := 0; i < 4; i++ {
				if mask&(1<<i) != 0 {
					if floor {
						m.A0[i] = int32(math.Floor(float64(v[i])))
					} else {
						m.A0[i] = int32(math.Floor(float64(v[i]) + 0.5))
					}
				}
			}
		}
	}
	return func(m *Machine, v Vec) {
		r := p.reg(m, t, n)
		for i := 0; i < 4; i++ {
			if mask&(1<<i) == 0 {
				continue
			}
			x := v[i] * scale
			if sat {
				x = min(max(x, 0), 1)
			}
			r[i] = x
		}
	}
}

func dot3(a, b Vec) float32 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func dot4(a, b Vec) float32 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] + a[3]*b[3] }
func splat(f float32) Vec   { return Vec{f, f, f, f} }

// Scalar operands use component 3 of the swizzled source: the replicated component, or w when
// no swizzle is given (the D3D rule).

func rcp(f float32) float32 {
	if f == 0 {
		return float32(math.Inf(1))
	}
	return 1 / f
}

func (p *Program) compileInst(in *Inst) (func(m *Machine), error) {
	src := make([]func(m *Machine) Vec, len(in.Src))
	for i, s := range in.Src {
		src[i] = p.fetch(s)
	}
	st := p.store(in.Dst)
	comp := func(f func(a, b, c float32) float32) func(m *Machine) {
		return func(m *Machine) {
			var a, b, c Vec
			a = src[0](m)
			if len(src) > 1 {
				b = src[1](m)
			}
			if len(src) > 2 {
				c = src[2](m)
			}
			st(m, Vec{f(a[0], b[0], c[0]), f(a[1], b[1], c[1]), f(a[2], b[2], c[2]), f(a[3], b[3], c[3])})
		}
	}
	scalar := func(f func(x float32) float32) func(m *Machine) {
		return func(m *Machine) { st(m, splat(f(src[0](m)[3]))) }
	}
	matrix := func(cols, rows int) func(m *Machine) {
		// mAxB dst, src0, src1: dst.i = dot(src0, src1 + i) for i < rows
		base := in.Src[1]
		rowsF := make([]func(m *Machine) Vec, rows)
		for i := 0; i < rows; i++ {
			b := base
			b.Num += i
			rowsF[i] = p.fetch(b)
		}
		return func(m *Machine) {
			a := src[0](m)
			var r Vec
			for i := 0; i < rows; i++ {
				b := rowsF[i](m)
				if cols == 4 {
					r[i] = dot4(a, b)
				} else {
					r[i] = dot3(a, b)
				}
			}
			st(m, r)
		}
	}
	sh := p.Shader
	switch in.Op {
	case OpMov, OpMova:
		return func(m *Machine) { st(m, src[0](m)) }, nil
	case OpAdd:
		return comp(func(a, b, _ float32) float32 { return a + b }), nil
	case OpSub:
		return comp(func(a, b, _ float32) float32 { return a - b }), nil
	case OpMul:
		return comp(func(a, b, _ float32) float32 { return a * b }), nil
	case OpMad:
		return comp(func(a, b, c float32) float32 { return a*b + c }), nil
	case OpMin:
		return comp(func(a, b, _ float32) float32 { return min(a, b) }), nil
	case OpMax:
		return comp(func(a, b, _ float32) float32 { return max(a, b) }), nil
	case OpSlt:
		return comp(func(a, b, _ float32) float32 { return b2f(a < b) }), nil
	case OpSge:
		return comp(func(a, b, _ float32) float32 { return b2f(a >= b) }), nil
	case OpLrp:
		return comp(func(a, b, c float32) float32 { return a*(b-c) + c }), nil
	case OpCmp:
		return comp(func(a, b, c float32) float32 {
			if a >= 0 {
				return b
			}
			return c
		}), nil
	case OpCnd:
		if sh.Pixel && sh.Major == 1 && sh.Minor < 4 {
			return func(m *Machine) {
				a, b, c := src[0](m), src[1](m), src[2](m)
				if a[3] > 0.5 {
					st(m, b)
				} else {
					st(m, c)
				}
			}, nil
		}
		return comp(func(a, b, c float32) float32 {
			if a > 0.5 {
				return b
			}
			return c
		}), nil
	case OpFrc:
		return comp(func(a, _, _ float32) float32 { return a - float32(math.Floor(float64(a))) }), nil
	case OpAbs:
		return comp(func(a, _, _ float32) float32 { return abs(a) }), nil
	case OpDp3:
		return func(m *Machine) { st(m, splat(dot3(src[0](m), src[1](m)))) }, nil
	case OpDp4:
		return func(m *Machine) { st(m, splat(dot4(src[0](m), src[1](m)))) }, nil
	case OpDp2Add:
		return func(m *Machine) {
			a, b, c := src[0](m), src[1](m), src[2](m)
			st(m, splat(a[0]*b[0]+a[1]*b[1]+c[3]))
		}, nil
	case OpRcp:
		return scalar(rcp), nil
	case OpRsq:
		return scalar(func(x float32) float32 { return rcp(float32(math.Sqrt(math.Abs(float64(x))))) }), nil
	case OpExp:
		return scalar(func(x float32) float32 { return float32(math.Exp2(float64(x))) }), nil
	case OpLog:
		return scalar(func(x float32) float32 {
			if x == 0 {
				return float32(math.Inf(-1))
			}
			return float32(math.Log2(math.Abs(float64(x))))
		}), nil
	case OpExpp:
		return func(m *Machine) {
			x := src[0](m)[3]
			fl := float32(math.Floor(float64(x)))
			st(m, Vec{float32(math.Exp2(float64(fl))), x - fl, float32(math.Exp2(float64(x))), 1})
		}, nil
	case OpLogp:
		return func(m *Machine) {
			x := abs(src[0](m)[3])
			if x == 0 {
				st(m, splat(float32(math.Inf(-1))))
				return
			}
			e, mant := math.Frexp(float64(x))
			st(m, Vec{float32(e - 1), float32(mant * 2), float32(math.Log2(float64(x))), 1})
		}, nil
	case OpPow:
		return func(m *Machine) {
			a, b := src[0](m), src[1](m)
			st(m, splat(float32(math.Pow(math.Abs(float64(a[3])), float64(b[3])))))
		}, nil
	case OpLit:
		return func(m *Machine) {
			a := src[0](m)
			r := Vec{1, 0, 0, 1}
			if a[0] > 0 {
				r[1] = a[0]
				if a[1] > 0 {
					pw := min(max(a[3], -128), 128)
					r[2] = float32(math.Pow(float64(a[1]), float64(pw)))
				}
			}
			st(m, r)
		}, nil
	case OpDst:
		return func(m *Machine) {
			a, b := src[0](m), src[1](m)
			st(m, Vec{1, a[1] * b[1], a[2], b[3]})
		}, nil
	case OpNrm:
		return func(m *Machine) {
			a := src[0](m)
			l := rcp(float32(math.Sqrt(float64(dot3(a, a)))))
			st(m, Vec{a[0] * l, a[1] * l, a[2] * l, a[3] * l})
		}, nil
	case OpCrs:
		return func(m *Machine) {
			a, b := src[0](m), src[1](m)
			st(m, Vec{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0], 0})
		}, nil
	case OpSinCos:
		return func(m *Machine) {
			x := float64(src[0](m)[3])
			st(m, Vec{float32(math.Cos(x)), float32(math.Sin(x)), 0, 0})
		}, nil
	case OpM4x4:
		return matrix(4, 4), nil
	case OpM4x3:
		return matrix(4, 3), nil
	case OpM3x4:
		return matrix(3, 4), nil
	case OpM3x3:
		return matrix(3, 3), nil
	case OpM3x2:
		return matrix(3, 2), nil
	case OpTexCoord:
		if sh.Major == 1 && sh.Minor < 4 {
			n := in.Dst.Num & 7
			return func(m *Machine) {
				t := m.T[n]
				m.T[n] = Vec{min(max(t[0], 0), 1), min(max(t[1], 0), 1), min(max(t[2], 0), 1), 1}
			}, nil
		}
	case OpTex:
		switch {
		case sh.Major >= 2: // texld dst, coord, sampler
			proj := in.Ctrl&1 != 0
			stage := in.Src[1].Num
			return func(m *Machine) {
				uv := src[0](m)
				if proj && uv[3] != 0 {
					uv = Vec{uv[0] / uv[3], uv[1] / uv[3], uv[2] / uv[3], 1}
				}
				st(m, m.Sample(stage, uv))
			}, nil
		case sh.Minor < 4: // tex t#: sample stage # at texture coordinate t#
			n := in.Dst.Num & 7
			return func(m *Machine) { m.T[n] = m.Sample(n, m.T[n]) }, nil
		}
	case OpTexKill:
		r := in.Dst
		return func(m *Machine) {
			v := p.reg(m, r.Type, r.Num)
			n := 4
			if sh.Major < 2 {
				n = 3 // ps_1_x: x, y, z of the texture coordinate
			}
			for i := 0; i < n; i++ {
				if v[i] < 0 {
					m.Kill = true
				}
			}
		}, nil
	}
	return nil, fmt.Errorf("d3dsm: unsupported instruction %s", opNames[in.Op])
}

func b2f(b bool) float32 {
	if b {
		return 1
	}
	return 0
}

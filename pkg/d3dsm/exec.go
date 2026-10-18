package d3dsm

import (
	"fmt"
	"math"
)

// Vec is one four-component register.
type Vec = [4]float32

// Register file layout: every register a program reads or writes, except constants, lives in
// Machine.F at these offsets.
const (
	FileR       = 0  // r0..r31
	FileV       = 32 // v0..v15 (vs inputs; ps colours)
	FileT       = 48 // t0..t7 (ps texture coordinates / texture registers)
	FileOPos    = 56
	FileOFog    = 57
	FileOPts    = 58
	FileOD      = 59 // oD0, oD1
	FileOT      = 61 // oT0..oT7
	FileOC      = 69 // oC0..oC3 (ps_1_x: r0 is copied to oC0)
	fileScratch = 73 // writes to registers without a home (a0 is handled separately)
	fileLen     = 74
)

// Machine holds the register state of one shader invocation. Callers fill the inputs (F[FileV:]
// for vertex shaders; colours F[FileV:] and texture coordinates F[FileT:] for pixel shaders), run
// a Program, and read the outputs.
type Machine struct {
	C    []Vec // float constants (the draw's snapshot, with the shader's def values applied)
	F    [fileLen]Vec
	A0   [4]int32
	Kill bool
	// Sample returns the texel of sampler stage s at texture coordinate uv.
	Sample func(s int, uv Vec) Vec
}

// operand is a source register resolved at compile time.
type operand struct {
	idx     int  // index into F, or into C for constants
	konst   bool // constant register
	rel     bool // constant indexed by a0
	relComp uint8
	sw      [4]uint8
	mod     uint8 // D3DSPSM_*
}

// dest is a destination register resolved at compile time.
type dest struct {
	idx   int
	mask  uint8
	sat   bool
	scale float32
	a0    bool // vs address register
	floor bool // a0 conversion: floor (vs_1_x) rather than round
}

// inst is one executable instruction.
type inst struct {
	op   int
	src  [3]operand
	dst  dest
	n    int  // matrix rows; texture stage or register for tex/texcoord; components for texkill
	cols int  // matrix columns
	proj bool // texldp
	cnd1 bool // ps_1_0..1_3 cnd: select on src0.w
}

// Program is a shader ready to execute.
type Program struct {
	Shader *Shader
	code   []inst
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
	ps1            bool // ps_1_x: r0 is the colour output
	wide           bool // RunWide supports it
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

// Compile prepares a decoded shader for execution.
func Compile(sh *Shader) (*Program, error) {
	p := &Program{Shader: sh, ps1: sh.Pixel && sh.Major < 2}
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
		c, err := p.compileInst(in)
		if err != nil {
			return nil, err
		}
		p.code = append(p.code, c)
	}
	p.checkWide()
	return p, nil
}

// regIndex is the F index of a non-constant register.
func (p *Program) regIndex(t, n int) int {
	switch t {
	case RegTemp:
		return FileR + n&31
	case RegInput:
		return FileV + n&15
	case RegAddrTex:
		if p.Shader.Pixel {
			return FileT + n&7
		}
	case RegRastOut:
		switch n {
		case 0:
			return FileOPos
		case 1:
			return FileOFog
		}
		return FileOPts
	case RegAttrOut:
		return FileOD + n&1
	case RegTexCrdOut:
		return FileOT + n&7
	case RegColorOut:
		return FileOC + n&3
	}
	return fileScratch
}

func (p *Program) operand(s Param) operand {
	o := operand{sw: s.Swizzle, mod: uint8(s.SrcMod)}
	if s.Type == RegConst {
		o.konst, o.idx = true, s.Num
		o.rel, o.relComp = s.Relative, s.RelComp
		return o
	}
	o.idx = p.regIndex(s.Type, s.Num)
	return o
}

func (p *Program) dest(d Param) dest {
	r := dest{idx: p.regIndex(d.Type, d.Num), mask: d.Mask, sat: d.Saturate, scale: float32(math.Ldexp(1, d.Shift))}
	if d.Type == RegAddrTex && !p.Shader.Pixel {
		r.a0, r.floor = true, p.Shader.Major < 2
	}
	return r
}

var zeroVec Vec

// fetch reads a source operand with its swizzle and modifier applied. Components travel as
// separate values (in registers) rather than as a Vec, which the CPU would store and reload.
func (m *Machine) fetch(o *operand) (x, y, z, w float32) {
	var r *Vec
	switch {
	case !o.konst:
		r = &m.F[o.idx]
	case o.rel:
		r = &zeroVec
		if i := o.idx + int(m.A0[o.relComp]); i >= 0 && i < len(m.C) {
			r = &m.C[i]
		}
	case o.idx < len(m.C):
		r = &m.C[o.idx]
	default:
		r = &zeroVec
	}
	x, y, z, w = r[o.sw[0]], r[o.sw[1]], r[o.sw[2]], r[o.sw[3]]
	if o.mod != 0 {
		v := applyMod(Vec{x, y, z, w}, o.mod)
		x, y, z, w = v[0], v[1], v[2], v[3]
	}
	return
}

// fetchVec is fetch for the less common instructions that work on whole vectors.
func (m *Machine) fetchVec(o *operand) Vec {
	x, y, z, w := m.fetch(o)
	return Vec{x, y, z, w}
}

func applyMod(v Vec, mod uint8) Vec {
	switch mod {
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

func abs(f float32) float32 { return float32(math.Abs(float64(f))) }

// store writes a result through the destination's mask, shift and saturate.
func (m *Machine) store(d *dest, x, y, z, w float32) {
	if d.a0 {
		v := Vec{x, y, z, w}
		for i := 0; i < 4; i++ {
			if d.mask&(1<<i) != 0 {
				if d.floor {
					m.A0[i] = int32(math.Floor(float64(v[i])))
				} else {
					m.A0[i] = int32(math.Floor(float64(v[i]) + 0.5))
				}
			}
		}
		return
	}
	if d.scale != 1 {
		x, y, z, w = x*d.scale, y*d.scale, z*d.scale, w*d.scale
	}
	if d.sat {
		x, y, z, w = min(max(x, 0), 1), min(max(y, 0), 1), min(max(z, 0), 1), min(max(w, 0), 1)
	}
	r := &m.F[d.idx]
	if d.mask&1 != 0 {
		r[0] = x
	}
	if d.mask&2 != 0 {
		r[1] = y
	}
	if d.mask&4 != 0 {
		r[2] = z
	}
	if d.mask&8 != 0 {
		r[3] = w
	}
}

// storeVec is store for a whole vector.
func (m *Machine) storeVec(d *dest, v Vec) { m.store(d, v[0], v[1], v[2], v[3]) }

func dot3(a, b Vec) float32 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func dot4(a, b Vec) float32 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] + a[3]*b[3] }

// Scalar operands use component 3 of the swizzled source: the replicated component, or w when
// no swizzle is given (the D3D rule).

func rcp(f float32) float32 {
	if f == 0 {
		return float32(math.Inf(1))
	}
	return 1 / f
}

func b2f(b bool) float32 {
	if b {
		return 1
	}
	return 0
}

// compileInst resolves an instruction's operands.
func (p *Program) compileInst(in *Inst) (inst, error) {
	c := inst{op: in.Op, dst: p.dest(in.Dst)}
	for i, s := range in.Src {
		if i < len(c.src) {
			c.src[i] = p.operand(s)
		}
	}
	sh := p.Shader
	switch in.Op {
	case OpMov, OpMova, OpAdd, OpSub, OpMul, OpMad, OpMin, OpMax, OpSlt, OpSge, OpLrp, OpCmp, OpFrc,
		OpAbs, OpDp3, OpDp4, OpDp2Add, OpRcp, OpRsq, OpExp, OpLog, OpExpp, OpLogp, OpPow, OpLit, OpDst,
		OpNrm, OpCrs, OpSinCos:
		return c, nil
	case OpCnd:
		c.cnd1 = sh.Pixel && sh.Major == 1 && sh.Minor < 4
		return c, nil
	case OpM4x4, OpM4x3, OpM3x4, OpM3x3, OpM3x2:
		c.cols = 4
		if in.Op >= OpM3x4 {
			c.cols = 3
		}
		c.n = map[int]int{OpM4x4: 4, OpM4x3: 3, OpM3x4: 4, OpM3x3: 3, OpM3x2: 2}[in.Op]
		return c, nil
	case OpTexCoord:
		if sh.Major == 1 && sh.Minor < 4 {
			c.n = in.Dst.Num & 7
			return c, nil
		}
	case OpTex:
		switch {
		case sh.Major >= 2: // texld dst, coord, sampler
			c.proj, c.n = in.Ctrl&1 != 0, in.Src[1].Num
			return c, nil
		case sh.Minor < 4: // tex t#: sample stage # at texture coordinate t#
			c.n = in.Dst.Num & 7
			return c, nil
		}
	case OpTexKill:
		c.src[0] = operand{idx: c.dst.idx, sw: [4]uint8{0, 1, 2, 3}}
		c.n = 4
		if sh.Major < 2 {
			c.n = 3 // ps_1_x: x, y, z of the texture coordinate
		}
		return c, nil
	}
	return c, fmt.Errorf("d3dsm: unsupported instruction %s", opNames[in.Op])
}

// Run executes the program on m.
func (p *Program) Run(m *Machine) {
	for i := range p.code {
		in := &p.code[i]
		d := &in.dst
		switch in.op {
		case OpMov, OpMova:
			x, y, z, w := m.fetch(&in.src[0])
			m.store(d, x, y, z, w)
		case OpAdd:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			m.store(d, ax+bx, ay+by, az+bz, aw+bw)
		case OpSub:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			m.store(d, ax-bx, ay-by, az-bz, aw-bw)
		case OpMul:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			m.store(d, ax*bx, ay*by, az*bz, aw*bw)
		case OpMad:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			cx, cy, cz, cw := m.fetch(&in.src[2])
			m.store(d, ax*bx+cx, ay*by+cy, az*bz+cz, aw*bw+cw)
		case OpMin:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			m.store(d, min(ax, bx), min(ay, by), min(az, bz), min(aw, bw))
		case OpMax:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			m.store(d, max(ax, bx), max(ay, by), max(az, bz), max(aw, bw))
		case OpSlt:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			m.store(d, b2f(ax < bx), b2f(ay < by), b2f(az < bz), b2f(aw < bw))
		case OpSge:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			m.store(d, b2f(ax >= bx), b2f(ay >= by), b2f(az >= bz), b2f(aw >= bw))
		case OpLrp:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			cx, cy, cz, cw := m.fetch(&in.src[2])
			m.store(d, ax*(bx-cx)+cx, ay*(by-cy)+cy, az*(bz-cz)+cz, aw*(bw-cw)+cw)
		case OpCmp:
			a, b, c := m.fetchVec(&in.src[0]), m.fetchVec(&in.src[1]), m.fetchVec(&in.src[2])
			for k := 0; k < 4; k++ {
				if !(a[k] >= 0) {
					b[k] = c[k]
				}
			}
			m.storeVec(d, b)
		case OpCnd:
			a, b, c := m.fetchVec(&in.src[0]), m.fetchVec(&in.src[1]), m.fetchVec(&in.src[2])
			if in.cnd1 {
				if !(a[3] > 0.5) {
					b = c
				}
			} else {
				for k := 0; k < 4; k++ {
					if !(a[k] > 0.5) {
						b[k] = c[k]
					}
				}
			}
			m.storeVec(d, b)
		case OpFrc:
			a := m.fetchVec(&in.src[0])
			for k := 0; k < 4; k++ {
				a[k] -= float32(math.Floor(float64(a[k])))
			}
			m.storeVec(d, a)
		case OpAbs:
			ax, ay, az, aw := m.fetch(&in.src[0])
			m.store(d, abs(ax), abs(ay), abs(az), abs(aw))
		case OpDp3:
			ax, ay, az, _ := m.fetch(&in.src[0])
			bx, by, bz, _ := m.fetch(&in.src[1])
			r := ax*bx + ay*by + az*bz
			m.store(d, r, r, r, r)
		case OpDp4:
			ax, ay, az, aw := m.fetch(&in.src[0])
			bx, by, bz, bw := m.fetch(&in.src[1])
			r := ax*bx + ay*by + az*bz + aw*bw
			m.store(d, r, r, r, r)
		case OpDp2Add:
			ax, ay, _, _ := m.fetch(&in.src[0])
			bx, by, _, _ := m.fetch(&in.src[1])
			_, _, _, cw := m.fetch(&in.src[2])
			r := ax*bx + ay*by + cw
			m.store(d, r, r, r, r)
		case OpRcp:
			_, _, _, x := m.fetch(&in.src[0])
			r := rcp(x)
			m.store(d, r, r, r, r)
		case OpRsq:
			_, _, _, x := m.fetch(&in.src[0])
			r := rcp(float32(math.Sqrt(math.Abs(float64(x)))))
			m.store(d, r, r, r, r)
		case OpExp:
			_, _, _, x := m.fetch(&in.src[0])
			r := float32(math.Exp2(float64(x)))
			m.store(d, r, r, r, r)
		case OpLog:
			_, _, _, x := m.fetch(&in.src[0])
			r := float32(math.Inf(-1))
			if x != 0 {
				r = float32(math.Log2(math.Abs(float64(x))))
			}
			m.store(d, r, r, r, r)
		case OpExpp:
			_, _, _, x := m.fetch(&in.src[0])
			fl := float32(math.Floor(float64(x)))
			m.store(d, float32(math.Exp2(float64(fl))), x-fl, float32(math.Exp2(float64(x))), 1)
		case OpLogp:
			_, _, _, x := m.fetch(&in.src[0])
			x = abs(x)
			if x == 0 {
				r := float32(math.Inf(-1))
				m.store(d, r, r, r, r)
				break
			}
			e, mant := math.Frexp(float64(x))
			m.store(d, float32(e-1), float32(mant*2), float32(math.Log2(float64(x))), 1)
		case OpPow:
			_, _, _, a := m.fetch(&in.src[0])
			_, _, _, b := m.fetch(&in.src[1])
			r := float32(math.Pow(math.Abs(float64(a)), float64(b)))
			m.store(d, r, r, r, r)
		case OpLit:
			a := m.fetchVec(&in.src[0])
			r := Vec{1, 0, 0, 1}
			if a[0] > 0 {
				r[1] = a[0]
				if a[1] > 0 {
					pw := min(max(a[3], -128), 128)
					r[2] = float32(math.Pow(float64(a[1]), float64(pw)))
				}
			}
			m.storeVec(d, r)
		case OpDst:
			_, ay, az, _ := m.fetch(&in.src[0])
			_, by, _, bw := m.fetch(&in.src[1])
			m.store(d, 1, ay*by, az, bw)
		case OpNrm:
			ax, ay, az, aw := m.fetch(&in.src[0])
			l := rcp(float32(math.Sqrt(float64(ax*ax + ay*ay + az*az))))
			m.store(d, ax*l, ay*l, az*l, aw*l)
		case OpCrs:
			ax, ay, az, _ := m.fetch(&in.src[0])
			bx, by, bz, _ := m.fetch(&in.src[1])
			m.store(d, ay*bz-az*by, az*bx-ax*bz, ax*by-ay*bx, 0)
		case OpSinCos:
			_, _, _, x := m.fetch(&in.src[0])
			m.store(d, float32(math.Cos(float64(x))), float32(math.Sin(float64(x))), 0, 0)
		case OpM4x4, OpM4x3, OpM3x4, OpM3x3, OpM3x2:
			// mAxB dst, src0, src1: dst.i = dot(src0, src1 + i) for i < rows
			a := m.fetchVec(&in.src[0])
			row := in.src[1]
			var r Vec
			for i := 0; i < in.n; i++ {
				b := m.fetchVec(&row)
				if in.cols == 4 {
					r[i] = dot4(a, b)
				} else {
					r[i] = dot3(a, b)
				}
				row.idx++
			}
			m.storeVec(d, r)
		case OpTexCoord:
			t := &m.F[FileT+in.n]
			t[0], t[1], t[2], t[3] = min(max(t[0], 0), 1), min(max(t[1], 0), 1), min(max(t[2], 0), 1), 1
		case OpTex:
			if p.Shader.Major >= 2 {
				x, y, z, w := m.fetch(&in.src[0])
				if in.proj && w != 0 {
					x, y, z, w = x/w, y/w, z/w, 1
				}
				m.storeVec(d, m.Sample(in.n, Vec{x, y, z, w}))
			} else {
				m.F[FileT+in.n] = m.Sample(in.n, m.F[FileT+in.n])
			}
		case OpTexKill:
			v := &m.F[in.src[0].idx]
			for k := 0; k < in.n; k++ {
				if v[k] < 0 {
					m.Kill = true
				}
			}
		}
	}
	if p.ps1 {
		m.F[FileOC] = m.F[FileR]
	}
}

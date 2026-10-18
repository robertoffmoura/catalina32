package d3dsm

// Translation of shader bytecode to Metal Shading Language, for the GPU renderer. Each
// instruction becomes MSL statements on float4 variables named after the registers, with the
// interpreter's semantics (Run in exec.go): same operand swizzles and modifiers, destination
// masks, shifts and saturation, the scalar rules (component 3 of the swizzled source), and the
// special cases (rcp(0) = +inf, rsq/log/pow of |x|, log(0) = -inf, ...).
//
// Varyings use the names HLSL semantics give them: oD#/v# are COLOR#, oT#/t# are TEXCOORD#,
// oFog is FOG0. A vertex function's colour outputs are clamped to [0, 1] (as D3D9 does for
// vs_1_x/vs_2_x), and oD0 starts white, as in the software renderer.

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"catalina32/pkg/msl"
)

// usageSemantic is the HLSL semantic name of a D3DDECLUSAGE.
var usageSemantic = [...]string{"POSITION", "BLENDWEIGHT", "BLENDINDICES", "NORMAL", "PSIZE", "TEXCOORD",
	"TANGENT", "BINORMAL", "TESSFACTOR", "POSITIONT", "COLOR", "FOG", "DEPTH", "SAMPLE"}

var _ msl.Shader = (*Program)(nil)

// Attributes lists the dcl'd vertex inputs (vertex shaders).
func (p *Program) Attributes() []msl.Attribute {
	var a []msl.Attribute
	for _, d := range p.Inputs {
		sem := "TEXCOORD"
		if d.Usage >= 0 && d.Usage < len(usageSemantic) {
			sem = usageSemantic[d.Usage]
		}
		a = append(a, msl.Attribute{Semantic: sem, Index: d.UsageIdx})
	}
	return a
}

// written reports which F registers some instruction writes.
func (p *Program) written() map[int]bool {
	w := map[int]bool{}
	for _, in := range p.code {
		if !in.dst.a0 && in.op != OpTexKill {
			w[in.dst.idx] = true
		}
	}
	return w
}

// Varyings lists the vertex outputs: COLOR0 always (it defaults to white), the other colours,
// texture coordinates and fog when written.
func (p *Program) Varyings() []msl.Varying {
	w := p.written()
	v := []msl.Varying{{Name: "COLOR0", Cols: 4}}
	if w[FileOD+1] {
		v = append(v, msl.Varying{Name: "COLOR1", Cols: 4})
	}
	for i := 0; i < 8; i++ {
		if w[FileOT+i] {
			v = append(v, msl.Varying{Name: fmt.Sprintf("TEXCOORD%d", i), Cols: 4})
		}
	}
	if w[FileOFog] {
		v = append(v, msl.Varying{Name: "FOG0", Cols: 4})
	}
	return v
}

// Textures lists the sampler stages read, with the texture type their dcl gives (2D otherwise).
func (p *Program) Textures() []msl.Texture {
	kinds := map[int]msl.TextureKind{}
	for _, in := range p.Shader.Insts {
		if in.Op == OpDcl && in.Dst.Type == RegSampler {
			switch in.Usage {
			case 3:
				kinds[in.Dst.Num] = msl.TexCube
			case 4:
				kinds[in.Dst.Num] = msl.Tex3D
			}
		}
	}
	var t []msl.Texture
	for s := 0; s < 16; s++ {
		if p.Samplers&(1<<s) != 0 {
			t = append(t, msl.Texture{Unit: s, Kind: kinds[s]})
		}
	}
	return t
}

// regName is the MSL variable holding F register idx.
func regName(idx int) string {
	switch {
	case idx < FileV:
		return fmt.Sprintf("R%d", idx-FileR)
	case idx < FileT:
		return fmt.Sprintf("V%d", idx-FileV)
	case idx < FileOPos:
		return fmt.Sprintf("T%d", idx-FileT)
	case idx == FileOPos:
		return "OPOS"
	case idx == FileOFog:
		return "OFOG"
	case idx == FileOPts:
		return "OPTS"
	case idx < FileOT:
		return fmt.Sprintf("OD%d", idx-FileOD)
	case idx < FileOC:
		return fmt.Sprintf("OT%d", idx-FileOT)
	case idx < fileScratch:
		return fmt.Sprintf("OC%d", idx-FileOC)
	}
	return "SCR"
}

func mslFloat(f float32) string {
	switch {
	case f != f:
		return "NAN"
	case math.IsInf(float64(f), 1):
		return "INFINITY"
	case math.IsInf(float64(f), -1):
		return "(-INFINITY)"
	}
	s := strconv.FormatFloat(float64(f), 'g', -1, 32)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s + "f"
}

func vecLit(v Vec) string {
	return fmt.Sprintf("float4(%s, %s, %s, %s)", mslFloat(v[0]), mslFloat(v[1]), mslFloat(v[2]), mslFloat(v[3]))
}

type mslWriter struct {
	p    *Program
	b    strings.Builder
	defs map[int]Vec
	tex  map[int]msl.Texture
}

// reg spells a constant or F register (constants defined by def are inlined).
func (w *mslWriter) reg(konst bool, idx int) string {
	if !konst {
		return regName(idx)
	}
	if v, ok := w.defs[idx]; ok {
		return vecLit(v)
	}
	if idx < 0 || idx >= 256 {
		return "float4(0)"
	}
	return fmt.Sprintf("C[%d]", idx)
}

// src spells a source operand with its swizzle and modifier.
func (w *mslWriter) src(o operand) string {
	var r string
	switch {
	case o.konst && o.rel:
		r = fmt.Sprintf("h_crel(C, %d + A0.%c)", o.idx, "xyzw"[o.relComp&3])
	default:
		r = w.reg(o.konst, o.idx)
	}
	if o.sw != [4]uint8{0, 1, 2, 3} {
		var s [4]byte
		for i, c := range o.sw {
			s[i] = "xyzw"[c&3]
		}
		r = "(" + r + ")." + string(s[:])
	}
	switch o.mod {
	case 1:
		return "(-" + r + ")"
	case 2:
		return "(" + r + " - 0.5f)"
	case 3:
		return "(0.5f - " + r + ")"
	case 4:
		return "(2.0f * " + r + " - 1.0f)"
	case 5:
		return "(1.0f - 2.0f * " + r + ")"
	case 6:
		return "(1.0f - " + r + ")"
	case 7:
		return "(2.0f * " + r + ")"
	case 8:
		return "(-2.0f * " + r + ")"
	case 9:
		return "h_divz(" + r + ")"
	case 10:
		return "h_divw(" + r + ")"
	case 11:
		return "abs(" + r + ")"
	case 12:
		return "(-abs(" + r + "))"
	}
	return r
}

// store writes value (a float4 expression) through the destination.
func (w *mslWriter) store(d dest, value string) {
	b := &w.b
	b.WriteString("\t{ float4 v_ = " + value + ";")
	if d.a0 {
		for i := 0; i < 4; i++ {
			if d.mask&(1<<i) != 0 {
				c := "xyzw"[i]
				if d.floor {
					fmt.Fprintf(b, " A0.%c = h_toA0(floor(v_.%c));", c, c)
				} else {
					fmt.Fprintf(b, " A0.%c = h_toA0(floor(v_.%c + 0.5f));", c, c)
				}
			}
		}
		b.WriteString(" }\n")
		return
	}
	if d.scale != 1 {
		b.WriteString(" v_ *= " + mslFloat(d.scale) + ";")
	}
	if d.sat {
		b.WriteString(" v_ = saturate(v_);")
	}
	if d.mask == 15 {
		fmt.Fprintf(b, " %s = v_;", regName(d.idx))
	} else if d.mask != 0 {
		var m []byte
		for i := 0; i < 4; i++ {
			if d.mask&(1<<i) != 0 {
				m = append(m, "xyzw"[i])
			}
		}
		fmt.Fprintf(b, " %s.%s = v_.%s;", regName(d.idx), m, m)
	}
	b.WriteString(" }\n")
}

// body translates the program's instructions.
func (w *mslWriter) body() error {
	p := w.p
	for i := range p.code {
		in := &p.code[i]
		a := func(k int) string { return w.src(in.src[k]) }
		scalar := func(k int) string { return "(" + a(k) + ").w" }
		switch in.op {
		case OpMov, OpMova:
			w.store(in.dst, a(0))
		case OpAdd:
			w.store(in.dst, a(0)+" + "+a(1))
		case OpSub:
			w.store(in.dst, a(0)+" - "+a(1))
		case OpMul:
			w.store(in.dst, a(0)+" * "+a(1))
		case OpMad:
			w.store(in.dst, a(0)+" * "+a(1)+" + "+a(2))
		case OpMin:
			w.store(in.dst, "min("+a(0)+", "+a(1)+")")
		case OpMax:
			w.store(in.dst, "max("+a(0)+", "+a(1)+")")
		case OpSlt:
			w.store(in.dst, "select(float4(0.0f), float4(1.0f), "+a(0)+" < "+a(1)+")")
		case OpSge:
			w.store(in.dst, "select(float4(0.0f), float4(1.0f), "+a(0)+" >= "+a(1)+")")
		case OpLrp:
			w.store(in.dst, "h_lrp("+a(0)+", "+a(1)+", "+a(2)+")")
		case OpCmp:
			w.store(in.dst, "select("+a(2)+", "+a(1)+", "+a(0)+" >= 0)")
		case OpCnd:
			if in.cnd1 {
				w.store(in.dst, "(("+a(0)+").w > 0.5f ? "+a(1)+" : "+a(2)+")")
			} else {
				w.store(in.dst, "select("+a(2)+", "+a(1)+", "+a(0)+" > 0.5f)")
			}
		case OpFrc:
			w.store(in.dst, "h_frc("+a(0)+")")
		case OpAbs:
			w.store(in.dst, "abs("+a(0)+")")
		case OpDp3:
			w.store(in.dst, "float4(h_dot3("+a(0)+", "+a(1)+"))")
		case OpDp4:
			w.store(in.dst, "float4(h_dot4("+a(0)+", "+a(1)+"))")
		case OpDp2Add:
			w.store(in.dst, "float4(h_dp2add("+a(0)+", "+a(1)+", "+a(2)+"))")
		case OpRcp:
			w.store(in.dst, "float4(h_rcp("+scalar(0)+"))")
		case OpRsq:
			w.store(in.dst, "float4(h_rcp(sqrt(abs("+scalar(0)+"))))")
		case OpExp:
			w.store(in.dst, "float4(exp2("+scalar(0)+"))")
		case OpLog:
			w.store(in.dst, "float4(h_log("+scalar(0)+"))")
		case OpExpp:
			w.store(in.dst, "h_expp("+scalar(0)+")")
		case OpLogp:
			w.store(in.dst, "h_logp("+scalar(0)+")")
		case OpPow:
			w.store(in.dst, "float4(pow(abs("+scalar(0)+"), "+scalar(1)+"))")
		case OpLit:
			w.store(in.dst, "h_lit("+a(0)+")")
		case OpDst:
			w.store(in.dst, "h_dst("+a(0)+", "+a(1)+")")
		case OpNrm:
			w.store(in.dst, "h_nrm("+a(0)+")")
		case OpCrs:
			w.store(in.dst, "h_crs("+a(0)+", "+a(1)+")")
		case OpSinCos:
			w.store(in.dst, "h_sincos("+scalar(0)+")")
		case OpM4x4, OpM4x3, OpM3x4, OpM3x3, OpM3x2:
			comps := make([]string, 4)
			for k := range comps {
				comps[k] = "0.0f"
			}
			row := in.src[1]
			for k := 0; k < in.n; k++ {
				fn := "h_dot4"
				if in.cols == 3 {
					fn = "h_dot3"
				}
				comps[k] = fmt.Sprintf("%s(%s, %s)", fn, a(0), w.src(row))
				row.idx++
			}
			w.store(in.dst, "float4("+strings.Join(comps, ", ")+")")
		case OpTexCoord:
			fmt.Fprintf(&w.b, "\tT%d = float4(saturate(T%d.xyz), 1.0f);\n", in.n, in.n)
		case OpTex:
			t := w.tex[in.n]
			coord := ".xy"
			if t.Kind != msl.Tex2D {
				coord = ".xyz"
			}
			if p.Shader.Major >= 2 {
				c := a(0)
				if in.proj {
					c = "h_proj(" + c + ")"
				}
				w.store(in.dst, fmt.Sprintf("t%d.sample(s%d, (%s)%s)", in.n, in.n, c, coord))
			} else {
				fmt.Fprintf(&w.b, "\tT%d = t%d.sample(s%d, T%d%s);\n", in.n, in.n, in.n, in.n, coord)
			}
		case OpTexKill:
			r := regName(in.src[0].idx)
			conds := make([]string, in.n)
			for k := range conds {
				conds[k] = fmt.Sprintf("%s.%c < 0", r, "xyzw"[k])
			}
			fmt.Fprintf(&w.b, "\tif (%s) discard_fragment();\n", strings.Join(conds, " || "))
		default:
			return fmt.Errorf("d3dsm: no MSL for %s", opNames[in.op])
		}
	}
	return nil
}

// declarations declares every register variable (zero; oD0 white).
func declarations(b *strings.Builder) {
	b.WriteString("\tint4 A0 = int4(0);\n\tfloat4 OPOS = float4(0), OFOG = float4(0), OPTS = float4(0), SCR = float4(0);\n")
	b.WriteString("\tfloat4 OD0 = float4(1.0f), OD1 = float4(0);\n")
	for _, set := range []struct {
		prefix string
		n      int
	}{{"R", 32}, {"V", 16}, {"T", 8}, {"OT", 8}, {"OC", 4}} {
		names := make([]string, set.n)
		for i := range names {
			names[i] = fmt.Sprintf("%s%d = float4(0)", set.prefix, i)
		}
		fmt.Fprintf(b, "\tfloat4 %s;\n", strings.Join(names, ", "))
	}
	// Silence unused-variable warnings for registers the program never touches.
	b.WriteString("\t(void)A0; (void)OPOS; (void)OFOG; (void)OPTS; (void)SCR; (void)OD1;\n")
}

func (p *Program) writer() *mslWriter {
	w := &mslWriter{p: p, defs: map[int]Vec{}, tex: map[int]msl.Texture{}}
	for _, d := range p.defs {
		w.defs[d.reg] = d.v
	}
	for _, t := range p.Textures() {
		w.tex[t.Unit] = t
	}
	return w
}

func texParams(ts []msl.Texture) string {
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, ", %s t%d [[texture(%d)]], sampler s%d [[sampler(%d)]]", t.TypeName(), t.Unit, t.Unit, t.Unit, t.Unit)
	}
	return b.String()
}

// VertexMSL builds the vertex function (see msl.Shader). Attribute i feeds the dcl'd input
// register p.Inputs[i].
func (p *Program) VertexMSL(attrs []msl.AttrClass) string {
	w := p.writer()
	b := &w.b
	b.WriteString(bytecodePrelude)
	b.WriteString("struct VIn {\n")
	for i := range p.Inputs {
		ty := "float4"
		if i < len(attrs) {
			ty = msl.AttrType(attrs[i])
		}
		fmt.Fprintf(b, "\t%s a%d [[attribute(%d)]];\n", ty, i, i)
	}
	if len(p.Inputs) == 0 {
		b.WriteString("\tfloat4 unused_ [[attribute(0)]];\n")
	}
	b.WriteString("};\nstruct VOut {\n\tfloat4 pos [[position]];\n")
	vary := p.Varyings()
	for _, v := range vary {
		fmt.Fprintf(b, "\tfloat4 %s [[user(%s)]];\n", v.Name, v.Name)
	}
	b.WriteString("};\n")
	fmt.Fprintf(b, "vertex VOut vs_main(VIn vin [[stage_in]], constant float4* C [[buffer(%d)]], constant float4* S [[buffer(%d)]]) {\n", msl.ConstantsBuffer, msl.StateBuffer)
	declarations(b)
	for i, d := range p.Inputs {
		v := fmt.Sprintf("vin.a%d", i)
		if i < len(attrs) && attrs[i] != msl.AttrFloat {
			v = "float4(" + v + ")"
		}
		fmt.Fprintf(b, "\tV%d = %s;\n", d.Reg&15, v)
	}
	if err := w.body(); err != nil {
		return "#error " + err.Error() + "\n"
	}
	b.WriteString("\tVOut o;\n\to.pos = OPOS;\n")
	for _, v := range vary {
		src := map[string]string{"COLOR0": "saturate(OD0)", "COLOR1": "saturate(OD1)", "FOG0": "OFOG"}[v.Name]
		if src == "" {
			src = "OT" + strings.TrimPrefix(v.Name, "TEXCOORD")
		}
		fmt.Fprintf(b, "\to.%s = %s;\n", v.Name, src)
	}
	b.WriteString("\to.pos.xy += S[0].xy * o.pos.w; // D3D9 pixel centres\n\treturn o;\n}\n")
	return b.String()
}

// FragmentMSL builds the fragment function (see msl.Shader).
func (p *Program) FragmentMSL(provided []msl.Varying) string {
	w := p.writer()
	b := &w.b
	b.WriteString(bytecodePrelude)
	have := map[string]msl.Varying{}
	for _, v := range provided {
		have[v.Name] = v
	}
	b.WriteString("struct PIn {\n\tfloat4 pos [[position]];\n")
	type input struct {
		name, reg string
	}
	var ins []input
	for i := 0; i < 2; i++ {
		if p.ReadsV&(1<<i) != 0 {
			ins = append(ins, input{fmt.Sprintf("COLOR%d", i), fmt.Sprintf("V%d", i)})
		}
	}
	for i := 0; i < 8; i++ {
		if p.ReadsT&(1<<i) != 0 {
			ins = append(ins, input{fmt.Sprintf("TEXCOORD%d", i), fmt.Sprintf("T%d", i)})
		}
	}
	var assigns []string
	for _, in := range ins {
		v, ok := have[in.name]
		if !ok {
			continue
		}
		fmt.Fprintf(b, "\t%s %s [[user(%s)]];\n", v.TypeName(), in.name, in.name)
		assigns = append(assigns, fmt.Sprintf("\t%s = %s;\n", in.reg, widenVarying("pin."+in.name, v)))
	}
	b.WriteString("};\nstruct POut {\n\tfloat4 c0 [[color(0)]];\n")
	wr := p.written()
	for i := 1; i < 4; i++ {
		if wr[FileOC+i] {
			fmt.Fprintf(b, "\tfloat4 c%d [[color(%d)]];\n", i, i)
		}
	}
	b.WriteString("};\n")
	fmt.Fprintf(b, "fragment POut ps_main(PIn pin [[stage_in]], constant float4* C [[buffer(%d)]], constant float4* S [[buffer(%d)]]%s) {\n",
		msl.ConstantsBuffer, msl.StateBuffer, texParams(p.Textures()))
	declarations(b)
	for _, a := range assigns {
		b.WriteString(a)
	}
	if err := w.body(); err != nil {
		return "#error " + err.Error() + "\n"
	}
	if p.ps1 {
		b.WriteString("\tOC0 = R0;\n")
	}
	b.WriteString("\tPOut o;\n\to.c0 = OC0;\n")
	for i := 1; i < 4; i++ {
		if wr[FileOC+i] {
			fmt.Fprintf(b, "\to.c%d = OC%d;\n", i, i)
		}
	}
	b.WriteString(alphaTest)
	b.WriteString("\treturn o;\n}\n")
	return b.String()
}

// FixedFragmentMSL is the fragment function for bytecode vertex shaders drawn without a pixel
// shader: the fixed pipeline's default stage 0, COLOR0 times the texture at TEXCOORD0 (S[0].z
// non-zero when a texture is bound).
func FixedFragmentMSL(provided []msl.Varying) string {
	var b strings.Builder
	b.WriteString(bytecodePrelude)
	have := map[string]msl.Varying{}
	for _, v := range provided {
		have[v.Name] = v
	}
	b.WriteString("struct PIn {\n\tfloat4 pos [[position]];\n")
	col, uv := "float4(0)", "float4(0)"
	if v, ok := have["COLOR0"]; ok {
		fmt.Fprintf(&b, "\t%s COLOR0 [[user(COLOR0)]];\n", v.TypeName())
		col = widenVarying("pin.COLOR0", v)
	}
	if v, ok := have["TEXCOORD0"]; ok {
		fmt.Fprintf(&b, "\t%s TEXCOORD0 [[user(TEXCOORD0)]];\n", v.TypeName())
		uv = widenVarying("pin.TEXCOORD0", v)
	}
	b.WriteString("};\nstruct POut {\n\tfloat4 c0 [[color(0)]];\n};\n")
	fmt.Fprintf(&b, "fragment POut ps_main(PIn pin [[stage_in]], constant float4* C [[buffer(%d)]], constant float4* S [[buffer(%d)]], texture2d<float> t0 [[texture(0)]], sampler s0 [[sampler(0)]]) {\n",
		msl.ConstantsBuffer, msl.StateBuffer)
	fmt.Fprintf(&b, "\tPOut o;\n\to.c0 = %s;\n\tif (S[0].z != 0) o.c0 *= t0.sample(s0, (%s).xy);\n", col, uv)
	b.WriteString(alphaTest)
	b.WriteString("\treturn o;\n}\n")
	return b.String()
}

// widenVarying makes a float4 of a varying (missing components 0, w 1).
func widenVarying(x string, v msl.Varying) string {
	if v.Int {
		x = "float" + strings.TrimPrefix(v.TypeName(), "int") + "(" + x + ")"
		if v.Cols == 1 {
			x = "float(" + strings.TrimPrefix(x, "float") + ")"
		}
	}
	switch v.Cols {
	case 1:
		return "float4(" + x + ", 0.0f, 0.0f, 1.0f)"
	case 2:
		return "float4(" + x + ", 0.0f, 1.0f)"
	case 3:
		return "float4(" + x + ", 1.0f)"
	}
	return x
}

// alphaTest is D3D's alpha test on colour 0 (as in the HLSL translation).
const alphaTest = `	{
		int fn = int(S[0].x);
		float a = rint(saturate(o.c0.a) * 255.0f) / 255.0f, ref = S[0].y;
		bool pass = fn == 8 || (fn == 2 && a < ref) || (fn == 3 && a == ref) || (fn == 4 && a <= ref) ||
			(fn == 5 && a > ref) || (fn == 6 && a != ref) || (fn == 7 && a >= ref);
		if (!pass) discard_fragment();
	}
`

// BytecodePrelude holds the helpers generated bytecode functions use (tests swap it for C++).
const BytecodePrelude = bytecodePrelude

const bytecodePrelude = `#include <metal_stdlib>
using namespace metal;

inline float h_rcp(float x) { return x == 0 ? INFINITY : 1.0f / x; }
inline float h_dot3(float4 a, float4 b) { return a.x * b.x + a.y * b.y + a.z * b.z; }
inline float h_dot4(float4 a, float4 b) { return a.x * b.x + a.y * b.y + a.z * b.z + a.w * b.w; }
inline float h_dp2add(float4 a, float4 b, float4 c) { return a.x * b.x + a.y * b.y + c.w; }
inline float4 h_lrp(float4 a, float4 b, float4 c) { return a * (b - c) + c; }
inline float4 h_frc(float4 a) { return a - floor(a); }
inline float h_log(float x) { return x != 0 ? log2(abs(x)) : -INFINITY; }
inline float4 h_expp(float x) { float f = floor(x); return float4(exp2(f), x - f, exp2(x), 1.0f); }
inline float4 h_logp(float x) {
	x = abs(x);
	if (x == 0) return float4(-INFINITY);
	int e;
	float m = frexp(x, e);
	return float4(float(e - 1), m * 2.0f, log2(x), 1.0f);
}
inline float4 h_lit(float4 a) {
	float4 r = float4(1.0f, 0.0f, 0.0f, 1.0f);
	if (a.x > 0) {
		r.y = a.x;
		if (a.y > 0) r.z = pow(a.y, min(max(a.w, -128.0f), 128.0f));
	}
	return r;
}
inline float4 h_dst(float4 a, float4 b) { return float4(1.0f, a.y * b.y, a.z, b.w); }
inline float4 h_nrm(float4 a) { float l = h_rcp(sqrt(a.x * a.x + a.y * a.y + a.z * a.z)); return a * l; }
inline float4 h_crs(float4 a, float4 b) { return float4(a.y * b.z - a.z * b.y, a.z * b.x - a.x * b.z, a.x * b.y - a.y * b.x, 0.0f); }
inline float4 h_sincos(float x) { return float4(cos(x), sin(x), 0.0f, 0.0f); }
inline float4 h_divz(float4 v) { return float4(v.x / v.z, v.y / v.z, v.z, v.w); }
inline float4 h_divw(float4 v) { return float4(v.x / v.w, v.y / v.w, v.z, v.w); }
inline float4 h_proj(float4 v) { return v.w != 0 ? float4(v.xyz / v.w, 1.0f) : v; }
inline float4 h_crel(constant float4* C, int i) { return i >= 0 && i < 256 ? C[i] : float4(0); }
inline int h_toA0(float f) { return f >= -2147483648.0f && f < 2147483648.0f ? int(f) : 0; }
// end of prelude
`

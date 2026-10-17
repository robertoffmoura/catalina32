// Package d3dsm decodes and executes Direct3D 9 shader bytecode (vs_1_1 .. vs_3_0, ps_1_1 ..
// ps_3_0): the token streams games pass to IDirect3DDevice9::CreateVertexShader/CreatePixelShader.
package d3dsm

import (
	"fmt"
	"math"
	"strings"
)

// Register types (D3DSHADER_PARAM_REGISTER_TYPE).
const (
	RegTemp      = 0
	RegInput     = 1
	RegConst     = 2
	RegAddrTex   = 3 // vs: address register a0; ps: texture register t#
	RegRastOut   = 4 // oPos (0), oFog (1), oPts (2)
	RegAttrOut   = 5 // oD#
	RegTexCrdOut = 6 // oT# (vs_3_0: o#)
	RegConstInt  = 7
	RegColorOut  = 8
	RegDepthOut  = 9
	RegSampler   = 10
	RegConstBool = 14
	RegLoop      = 15
	RegMisc      = 17
	RegLabel     = 18
	RegPredicate = 19
)

// Opcodes (D3DSHADER_INSTRUCTION_OPCODE_TYPE).
const (
	OpNop      = 0
	OpMov      = 1
	OpAdd      = 2
	OpSub      = 3
	OpMad      = 4
	OpMul      = 5
	OpRcp      = 6
	OpRsq      = 7
	OpDp3      = 8
	OpDp4      = 9
	OpMin      = 10
	OpMax      = 11
	OpSlt      = 12
	OpSge      = 13
	OpExp      = 14
	OpLog      = 15
	OpLit      = 16
	OpDst      = 17
	OpLrp      = 18
	OpFrc      = 19
	OpM4x4     = 20
	OpM4x3     = 21
	OpM3x4     = 22
	OpM3x3     = 23
	OpM3x2     = 24
	OpDcl      = 31
	OpPow      = 32
	OpCrs      = 33
	OpSgn      = 34
	OpAbs      = 35
	OpNrm      = 36
	OpSinCos   = 37
	OpMova     = 46
	OpDefB     = 47
	OpDefI     = 48
	OpTexCoord = 64
	OpTexKill  = 65
	OpTex      = 66
	OpExpp     = 78
	OpLogp     = 79
	OpCnd      = 80
	OpDef      = 81
	OpCmp      = 88
	OpDp2Add   = 90
	OpComment  = 0xFFFE
	OpEnd      = 0xFFFF
)

var opNames = map[int]string{OpNop: "nop", OpMov: "mov", OpAdd: "add", OpSub: "sub", OpMad: "mad", OpMul: "mul",
	OpRcp: "rcp", OpRsq: "rsq", OpDp3: "dp3", OpDp4: "dp4", OpMin: "min", OpMax: "max", OpSlt: "slt", OpSge: "sge",
	OpExp: "exp", OpLog: "log", OpLit: "lit", OpDst: "dst", OpLrp: "lrp", OpFrc: "frc", OpM4x4: "m4x4", OpM4x3: "m4x3",
	OpM3x4: "m3x4", OpM3x3: "m3x3", OpM3x2: "m3x2", OpDcl: "dcl", OpPow: "pow", OpCrs: "crs", OpSgn: "sgn", OpAbs: "abs",
	OpNrm: "nrm", OpSinCos: "sincos", OpMova: "mova", OpDefB: "defb", OpDefI: "defi", OpTexCoord: "texcoord",
	OpTexKill: "texkill", OpTex: "tex", OpExpp: "expp", OpLogp: "logp", OpCnd: "cnd", OpDef: "def", OpCmp: "cmp",
	OpDp2Add: "dp2add"}

// srcCount is the number of source operands per opcode (all listed opcodes have one destination,
// except texkill which has only a destination-like operand).
var srcCount = map[int]int{OpNop: 0, OpMov: 1, OpAdd: 2, OpSub: 2, OpMad: 3, OpMul: 2, OpRcp: 1, OpRsq: 1, OpDp3: 2,
	OpDp4: 2, OpMin: 2, OpMax: 2, OpSlt: 2, OpSge: 2, OpExp: 1, OpLog: 1, OpLit: 1, OpDst: 2, OpLrp: 3, OpFrc: 1,
	OpM4x4: 2, OpM4x3: 2, OpM3x4: 2, OpM3x3: 2, OpM3x2: 2, OpPow: 2, OpCrs: 2, OpSgn: 3, OpAbs: 1, OpNrm: 1,
	OpMova: 1, OpTexCoord: 0, OpTexKill: 0, OpExpp: 1, OpLogp: 1, OpCnd: 3, OpCmp: 3, OpDp2Add: 3}

// Param is a decoded register operand.
type Param struct {
	Type, Num int
	Mask      uint8    // destination write mask (bit 0 = x)
	Swizzle   [4]uint8 // source component selects
	SrcMod    int      // D3DSPSM_* (source modifier)
	Saturate  bool     // destination _sat
	Shift     int      // destination shift scale: +1 = x2, -1 = d2, ...
	Relative  bool     // source uses relative addressing (a0.x / aL)
	RelComp   uint8    // component of the address register
}

// Inst is one decoded instruction.
type Inst struct {
	Op       int
	Ctrl     int // opcode-specific control bits (texld project/bias, comparison)
	CoIssue  bool
	Dst      Param
	HasDst   bool
	Src      []Param
	Usage    int // dcl: D3DDECLUSAGE (or sampler texture type)
	UsageIdx int
	Imm      [4]uint32 // def/defi/defb
}

// Shader is a decoded token stream.
type Shader struct {
	Pixel        bool
	Major, Minor int
	Insts        []Inst
}

func regType(t uint32) int { return int((t>>28)&7) | int((t>>8)&0x18) }

func decodeDst(t uint32) Param {
	p := Param{Type: regType(t), Num: int(t & 0x7FF), Mask: uint8((t >> 16) & 0xF), Saturate: (t>>20)&1 != 0}
	if sh := int((t >> 24) & 0xF); sh != 0 {
		if sh >= 8 {
			sh -= 16
		}
		p.Shift = sh
	}
	return p
}

func decodeSrc(t uint32) Param {
	p := Param{Type: regType(t), Num: int(t & 0x7FF), SrcMod: int((t >> 24) & 0xF), Relative: (t>>13)&1 != 0}
	sw := (t >> 16) & 0xFF
	for i := range p.Swizzle {
		p.Swizzle[i] = uint8((sw >> (2 * i)) & 3)
	}
	return p
}

// Decode parses a shader token stream (up to and including the end token).
func Decode(tok []uint32) (*Shader, error) {
	if len(tok) == 0 {
		return nil, fmt.Errorf("empty shader")
	}
	v := tok[0]
	sh := &Shader{Major: int((v >> 8) & 0xFF), Minor: int(v & 0xFF)}
	switch v >> 16 {
	case 0xFFFE:
	case 0xFFFF:
		sh.Pixel = true
	default:
		return nil, fmt.Errorf("bad version token %08x", v)
	}
	for i := 1; i < len(tok); {
		t := tok[i]
		op := int(t & 0xFFFF)
		if op == OpEnd {
			return sh, nil
		}
		if op == OpComment {
			i += 1 + int((t>>16)&0x7FFF)
			continue
		}
		in := Inst{Op: op, Ctrl: int((t >> 16) & 0xFF), CoIssue: (t>>30)&1 != 0}
		i++
		args := tok[i:]
		need := func(n int) error {
			if len(args) < n {
				return fmt.Errorf("truncated instruction %d", op)
			}
			return nil
		}
		// SM2+ stores the instruction length; SM1 needs the operand table.
		length := -1
		if sh.Major >= 2 {
			length = int((t >> 24) & 0xF)
		}
		switch op {
		case OpDcl:
			if err := need(2); err != nil {
				return nil, err
			}
			in.Usage, in.UsageIdx = int(args[0]&0x1F), int((args[0]>>16)&0xF)
			in.Dst, in.HasDst = decodeDst(args[1]), true
			if in.Dst.Type == RegSampler {
				in.Usage = int((args[0] >> 27) & 0xF)
			}
			i += 2
		case OpDef, OpDefI:
			if err := need(5); err != nil {
				return nil, err
			}
			in.Dst, in.HasDst = decodeDst(args[0]), true
			copy(in.Imm[:], args[1:5])
			i += 5
		case OpDefB:
			if err := need(2); err != nil {
				return nil, err
			}
			in.Dst, in.HasDst = decodeDst(args[0]), true
			in.Imm[0] = args[1]
			i += 2
		default:
			n, ok := srcCount[op]
			hasDst := true
			if op == OpTex {
				// ps_1_1-1_3: tex t#; ps_1_4: texld r#, t#; ps_2_0+: texld dst, coord, sampler
				switch {
				case sh.Major >= 2:
					n = 2
				case sh.Minor == 4:
					n = 1
				default:
					n = 0
				}
				ok = true
			}
			if op == OpSinCos {
				ok, n = true, 1
				if !sh.Pixel && sh.Major == 2 {
					n = 3
				}
			}
			if !ok && length < 0 {
				return nil, fmt.Errorf("unsupported opcode %d in shader model %d", op, sh.Major)
			}
			if length >= 0 {
				// Trust the encoded length (handles relative-addressing extra tokens).
				if err := need(length); err != nil {
					return nil, err
				}
				ops := args[:length]
				if len(ops) > 0 && hasDst {
					in.Dst, in.HasDst = decodeDst(ops[0]), true
					ops = ops[1:]
				}
				for k := 0; k < len(ops); k++ {
					p := decodeSrc(ops[k])
					if p.Relative && k+1 < len(ops) {
						k++
						p.RelComp = uint8((ops[k] >> 16) & 3)
					}
					in.Src = append(in.Src, p)
				}
				i += length
				break
			}
			if err := need(1 + n); err != nil {
				return nil, err
			}
			in.Dst, in.HasDst = decodeDst(args[0]), true
			for k := 0; k < n; k++ {
				in.Src = append(in.Src, decodeSrc(args[1+k]))
			}
			i += 1 + n
		}
		sh.Insts = append(sh.Insts, in)
	}
	return nil, fmt.Errorf("no end token")
}

var regPrefix = map[int]string{RegTemp: "r", RegInput: "v", RegConst: "c", RegRastOut: "oRast", RegAttrOut: "oD",
	RegTexCrdOut: "oT", RegConstInt: "i", RegColorOut: "oC", RegDepthOut: "oDepth", RegSampler: "s", RegConstBool: "b",
	RegLoop: "aL", RegMisc: "vMisc", RegPredicate: "p"}

func (sh *Shader) regName(t, n int) string {
	if t == RegAddrTex {
		if sh.Pixel {
			return fmt.Sprintf("t%d", n)
		}
		return fmt.Sprintf("a%d", n)
	}
	if t == RegRastOut {
		return [...]string{"oPos", "oFog", "oPts", "oRast?"}[min(n, 3)]
	}
	return fmt.Sprintf("%s%d", regPrefix[t], n)
}

// String disassembles the shader.
func (sh *Shader) String() string {
	var b strings.Builder
	kind := "vs"
	if sh.Pixel {
		kind = "ps"
	}
	fmt.Fprintf(&b, "%s_%d_%d\n", kind, sh.Major, sh.Minor)
	for _, in := range sh.Insts {
		name := opNames[in.Op]
		if name == "" {
			name = fmt.Sprintf("op%d", in.Op)
		}
		if in.Op == OpTex && sh.Major >= 2 {
			name = "texld"
		}
		if in.CoIssue {
			name = "+" + name
		}
		if in.Dst.Saturate {
			name += "_sat"
		}
		switch in.Dst.Shift {
		case 1:
			name += "_x2"
		case 2:
			name += "_x4"
		case -1:
			name += "_d2"
		}
		var ops []string
		if in.Op == OpDcl {
			name = fmt.Sprintf("dcl_u%d_%d", in.Usage, in.UsageIdx)
		}
		if in.HasDst {
			d := sh.regName(in.Dst.Type, in.Dst.Num)
			if in.Dst.Mask != 0xF && in.Dst.Mask != 0 {
				d += "." + maskStr(in.Dst.Mask)
			}
			ops = append(ops, d)
		}
		if in.Op == OpDef {
			ops = append(ops, fmt.Sprintf("%g, %g, %g, %g", f32(in.Imm[0]), f32(in.Imm[1]), f32(in.Imm[2]), f32(in.Imm[3])))
		}
		for _, s := range in.Src {
			r := sh.regName(s.Type, s.Num)
			if s.Relative {
				r = fmt.Sprintf("%s[a0.%c + %d]", regPrefix[s.Type], "xyzw"[s.RelComp], s.Num)
			}
			if sw := swzStr(s.Swizzle); sw != "" {
				r += "." + sw
			}
			ops = append(ops, srcModStr(s.SrcMod, r))
		}
		fmt.Fprintf(&b, "  %s %s\n", name, strings.Join(ops, ", "))
	}
	return b.String()
}

func maskStr(m uint8) string {
	s := ""
	for i := 0; i < 4; i++ {
		if m&(1<<i) != 0 {
			s += string("xyzw"[i])
		}
	}
	return s
}

func swzStr(sw [4]uint8) string {
	if sw == [4]uint8{0, 1, 2, 3} {
		return ""
	}
	s := ""
	for _, c := range sw {
		s += string("xyzw"[c])
	}
	return s
}

func srcModStr(m int, r string) string {
	switch m {
	case 1:
		return "-" + r
	case 2:
		return r + "_bias"
	case 3:
		return "-" + r + "_bias"
	case 4:
		return r + "_bx2"
	case 5:
		return "-" + r + "_bx2"
	case 6:
		return "1-" + r
	case 7:
		return r + "_x2"
	case 8:
		return "-" + r + "_x2"
	case 9:
		return r + "_dz"
	case 10:
		return r + "_dw"
	case 11:
		return "|" + r + "|"
	case 12:
		return "-|" + r + "|"
	}
	return r
}

func f32(u uint32) float32 { return math.Float32frombits(u) }

package d3d9

import (
	"crypto/sha1"
	"fmt"
	"math"
	"os"

	"catalina32/pkg/cpu"
	"catalina32/pkg/hlsl"
)

// Real shader pipeline (C32_REAL_SHADERS=1).
//
// D3DXCompileShader compiles the game's HLSL with pkg/hlsl and returns a real ID3DXConstantTable,
// so the game discovers constant registers and sampler units through the standard D3DX API and
// uploads its own constants. Draw calls then run the bound vertex shader per vertex. The pixel
// shader is evaluated per vertex with textures reading as white, and the rasterizer multiplies
// that colour by the per-pixel sample of the shader's first texture lookup (Gouraud-style: exact
// for shaders whose output is linear in that texture, approximate otherwise).

// RealShadersEnabled reports whether C32_REAL_SHADERS=1 is set.
func RealShadersEnabled() bool { return os.Getenv("C32_REAL_SHADERS") == "1" }

type compiledShader struct {
	prog    *hlsl.Program
	ps      bool
	state   *hlsl.State
	handles map[uint32]*hlsl.Constant // D3DXHANDLE (guest pointer to the name) -> constant
	byName  map[string]uint32
	ordered []uint32 // handles in table order
	texUnit int      // sampler register of the first texture lookup (-1: none)
}

// VertexElement is one D3DVERTEXELEMENT9.
type VertexElement struct {
	Stream, Offset    uint16
	Type, Usage, UIdx uint8
}

// D3DDECLUSAGE -> HLSL semantic name.
var usageSemantic = [...]string{"POSITION", "BLENDWEIGHT", "BLENDINDICES", "NORMAL", "PSIZE", "TEXCOORD",
	"TANGENT", "BINORMAL", "TESSFACTOR", "POSITIONT", "COLOR", "FOG", "DEPTH", "SAMPLE"}

func (s *Subsystem) compileHLSL(c *cpu.CPU, src []byte, entry, profile string) (*compiledShader, error) {
	key := fmt.Sprintf("%x/%s/%s", sha1.Sum(src), entry, profile)
	if cs, ok := s.shaderCache[key]; ok {
		return cs, nil
	}
	file, err := hlsl.Parse(string(src))
	if err != nil {
		return nil, err
	}
	prog, err := hlsl.Compile(file, entry)
	if err != nil {
		return nil, err
	}
	cs := &compiledShader{prog: prog, ps: len(profile) > 1 && profile[0] == 'p', handles: map[uint32]*hlsl.Constant{},
		byName: map[string]uint32{}, texUnit: -1}
	cs.state = prog.NewState()
	for _, k := range prog.Constants {
		h := c.Malloc(uint32(len(k.Name) + 1))
		c.WriteCString(h, k.Name)
		cs.handles[h] = k
		cs.byName[k.Name] = h
		cs.ordered = append(cs.ordered, h)
	}
	if len(prog.TexSamplers) > 0 {
		cs.texUnit = prog.TexSamplers[0].RegisterIndex
	}
	s.shaderCache[key] = cs
	return cs, nil
}

// compileShaderReal implements D3DXCompileShader. It returns false if compilation failed (the
// caller then falls back to the placeholder behaviour so the game keeps running).
func (s *Subsystem) compileShaderReal(c *cpu.CPU) bool {
	src := c.Mem[c.Arg(0) : c.Arg(0)+c.Arg(1)]
	entry, profile := c.ReadCString(c.Arg(4)), c.ReadCString(c.Arg(5))
	ppShader, ppErr, ppConst := c.Arg(7), c.Arg(8), c.Arg(9)
	cs, err := s.compileHLSL(c, src, entry, profile)
	if err != nil {
		s.shaderErrors++
		if s.shaderErrors <= 10 {
			fmt.Printf("[HLSL] %s/%s: %v\n", entry, profile, err)
		}
		return false
	}
	if ppShader != 0 {
		buf := s.allocD3DXBuffer(c, 16)
		data := s.Resources[buf].DataPtr
		version := uint32(0xFFFE0300) // vs_3_0
		if cs.ps {
			version = 0xFFFF0300 // ps_3_0
		}
		c.Write32(data, version)
		c.Write32(data+4, 0x0000FFFF)
		s.shaderBlobs[data] = cs
		c.Write32(ppShader, buf)
	}
	if ppErr != 0 {
		c.Write32(ppErr, 0)
	}
	if ppConst != 0 {
		tbl := s.allocCOMObject(c, s.vtConstTable, 16)
		s.constTables[tbl] = cs
		c.Write32(ppConst, tbl)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
	return true
}

// D3DXPARAMETER_CLASS / D3DXPARAMETER_TYPE for a constant.
func constClassType(k *hlsl.Constant) (class, typ, rows, cols, elems uint32) {
	t := k.Type
	elems = 1
	if t.IsArray() {
		elems = uint32(t.ArrayLen)
		t = t.Elem
	}
	switch t.Base {
	case hlsl.BSampler:
		typ = 10
		switch t.Sampler {
		case "sampler1D":
			typ = 11
		case "sampler2D", "texture", "texture2D":
			typ = 12
		case "sampler3D":
			typ = 13
		case "samplerCUBE", "textureCUBE":
			typ = 14
		}
		return 4, typ, 1, 1, elems // D3DXPC_OBJECT
	case hlsl.BStruct:
		return 5, 0, 1, 1, elems
	case hlsl.BBool:
		typ = 1
	case hlsl.BInt:
		typ = 2
	default:
		typ = 3
	}
	rows, cols = uint32(t.Rows), uint32(t.Cols)
	switch {
	case t.Rows > 1:
		class = 3 // D3DXPC_MATRIX_COLUMNS
	case t.Cols > 1:
		class = 1 // D3DXPC_VECTOR
	}
	return class, typ, rows, cols, elems
}

// handleID3DXConstantTable implements the ID3DXConstantTable methods the game uses for discovery.
// The Set* methods are accepted but ignored (the game uploads through SetVertexShaderConstantF).
func (s *Subsystem) handleID3DXConstantTable(c *cpu.CPU, winSlot int) {
	cs := s.constTables[c.Arg(0)]
	c.Regs[cpu.RegEAX] = 0
	if cs == nil {
		c.Ret()
		return
	}
	switch winSlot {
	case 3: // GetBufferPointer
		c.Regs[cpu.RegEAX] = 0
	case 4: // GetBufferSize
		c.Regs[cpu.RegEAX] = 0
	case 5: // GetDesc(this, pDesc)
		if p := c.Arg(1); p != 0 {
			c.Write32(p, 0)
			version := uint32(0xFFFE0300)
			if cs.ps {
				version = 0xFFFF0300
			}
			c.Write32(p+4, version)
			c.Write32(p+8, uint32(len(cs.ordered)))
		}
	case 6: // GetConstantDesc(this, hConstant, pDesc, pCount)
		k := cs.handles[c.Arg(1)]
		pDesc, pCount := c.Arg(2), c.Arg(3)
		if k == nil {
			c.Regs[cpu.RegEAX] = 0x8876086C // D3DERR_INVALIDCALL
			break
		}
		class, typ, rows, cols, elems := constClassType(k)
		if pDesc != 0 {
			c.Write32(pDesc+0, c.Arg(1)) // Name: the handle is the name string
			c.Write32(pDesc+4, uint32(k.RegisterSet))
			c.Write32(pDesc+8, uint32(k.RegisterIndex))
			c.Write32(pDesc+12, uint32(k.RegisterCount))
			c.Write32(pDesc+16, class)
			c.Write32(pDesc+20, typ)
			c.Write32(pDesc+24, rows)
			c.Write32(pDesc+28, cols)
			c.Write32(pDesc+32, elems)
			c.Write32(pDesc+36, 0)
			c.Write32(pDesc+40, uint32(k.Type.Size()*4))
			c.Write32(pDesc+44, 0)
		}
		if pCount != 0 {
			c.Write32(pCount, 1)
		}
	case 7: // GetSamplerIndex(this, hConstant)
		if k := cs.handles[c.Arg(1)]; k != nil && k.RegisterSet == hlsl.RegSampler {
			c.Regs[cpu.RegEAX] = uint32(k.RegisterIndex)
		} else {
			c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		}
	case 8: // GetConstant(this, hParent, Index)
		if i := c.Arg(2); c.Arg(1) == 0 && int(i) < len(cs.ordered) {
			c.Regs[cpu.RegEAX] = cs.ordered[i]
		}
	case 9: // GetConstantByName(this, hParent, pName)
		if c.Arg(1) == 0 {
			c.Regs[cpu.RegEAX] = cs.byName[c.ReadCString(c.Arg(2))]
		}
	case 10: // GetConstantElement(this, hConstant, Index): elements share the array's handle
		if cs.handles[c.Arg(1)] != nil {
			c.Regs[cpu.RegEAX] = c.Arg(1)
		}
	default: // SetDefaults and Set*: accepted
		if !s.loggedConstTableSet {
			s.loggedConstTableSet = true
			fmt.Printf("[HLSL] ID3DXConstantTable method %d used (ignored)\n", winSlot)
		}
	}
	c.Ret()
}

// decodeElement reads one vertex element as up to four floats (missing components 0,0,0,1).
func decodeElement(c *cpu.CPU, addr uint32, typ uint8) [4]float32 {
	v := [4]float32{0, 0, 0, 1}
	b := func(i uint32) uint8 { return c.Mem[addr+i] }
	s16 := func(i uint32) int16 { return int16(c.Read16(addr + 2*i)) }
	switch typ {
	case 0, 1, 2, 3: // FLOAT1..4
		for i := uint32(0); i <= uint32(typ); i++ {
			v[i] = c.ReadF32(addr + 4*i)
		}
	case 4: // D3DCOLOR (BGRA in memory) -> rgba
		v = [4]float32{float32(b(2)) / 255, float32(b(1)) / 255, float32(b(0)) / 255, float32(b(3)) / 255}
	case 5: // UBYTE4
		v = [4]float32{float32(b(0)), float32(b(1)), float32(b(2)), float32(b(3))}
	case 8: // UBYTE4N
		v = [4]float32{float32(b(0)) / 255, float32(b(1)) / 255, float32(b(2)) / 255, float32(b(3)) / 255}
	case 6, 7: // SHORT2, SHORT4
		n := uint32(2 + 2*(typ-6))
		for i := uint32(0); i < n; i++ {
			v[i] = float32(s16(i))
		}
	case 9, 10: // SHORT2N, SHORT4N
		n := uint32(2 + 2*(typ-9))
		for i := uint32(0); i < n; i++ {
			v[i] = max(float32(s16(i))/32767, -1)
		}
	case 11, 12: // USHORT2N, USHORT4N
		n := uint32(2 + 2*(typ-11))
		for i := uint32(0); i < n; i++ {
			v[i] = float32(uint16(s16(i))) / 65535
		}
	case 13, 14: // UDEC3, DEC3N
		w := c.Read32(addr)
		for i := uint32(0); i < 3; i++ {
			x := (w >> (10 * i)) & 0x3FF
			if typ == 13 {
				v[i] = float32(x)
			} else {
				v[i] = float32(int32(x<<22)>>22) / 511
			}
		}
	case 15, 16: // FLOAT16_2, FLOAT16_4
		n := uint32(2 + 2*(typ-15))
		for i := uint32(0); i < n; i++ {
			v[i] = halfToFloat(c.Read16(addr + 2*i))
		}
	}
	return v
}

func halfToFloat(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1F
	mant := uint32(h) & 0x3FF
	switch {
	case exp == 0 && mant == 0:
		return math.Float32frombits(sign)
	case exp == 0: // subnormal
		f := float32(mant) / 1024 / 16384
		if sign != 0 {
			return -f
		}
		return f
	case exp == 31:
		return math.Float32frombits(sign | 0x7F800000 | mant<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | mant<<13)
}

func clampByte(f float32) uint8 {
	switch {
	case f != f || f <= 0:
		return 0
	case f >= 1:
		return 255
	}
	return uint8(f*255 + 0.5)
}

// shadeVertex runs the bound vertex (and per-vertex pixel) shader for the vertex whose stream-0
// data is at vp.
func (s *Subsystem) shadeVertex(c *cpu.CPU, vp, stride uint32, isUP bool) screenVertex {
	vs, vd := s.curVS, s.CurrentVD
	if s.shaderConstsDirty {
		vs.state.LoadConstants(s.VSConstants[:])
		if s.curPS != nil {
			s.curPS.state.LoadConstants(s.PSConstants[:])
		}
		s.shaderConstsDirty = false
	}
	st := vs.state
	var idx uint32
	if !isUP && stride != 0 {
		if res := s.Resources[s.StreamSources[0]]; res != nil {
			idx = (vp - (res.DataPtr + s.StreamOffsets[0])) / stride
		}
	}
	for _, e := range vd.Elements {
		if int(e.Usage) >= len(usageSemantic) {
			continue
		}
		sem, si := usageSemantic[e.Usage], int(e.UIdx)
		addr := vp + uint32(e.Offset)
		if e.Stream != 0 {
			res := s.Resources[s.StreamSources[e.Stream]]
			if res == nil || isUP {
				continue
			}
			addr = res.DataPtr + s.StreamOffsets[e.Stream] + idx*s.StreamStrides[e.Stream] + uint32(e.Offset)
		}
		if addr+16 > uint32(len(c.Mem)) {
			continue
		}
		v := decodeElement(c, addr, e.Type)
		// Several input fields may share a semantic (e.g. half4 uvSet01 and half2 uvSet0 both on
		// TEXCOORD0): each reads the same register.
		for _, in := range vs.prog.Inputs {
			if in.Semantic == sem && in.Index == si {
				copy(st.M[in.Offset:in.Offset+min(in.Components, 4)], v[:])
			}
		}
	}
	st.Run()
	pos := vs.prog.Output("POSITION", 0)
	if pos == nil || pos.Components < 4 {
		return screenVertex{}
	}
	p := st.M[pos.Offset : pos.Offset+4]
	sv := screenVertex{cx: p[0], cy: p[1], cz: p[2], cw: p[3], clip: true, valid: true, r: 255, g: 255, b: 255, a: 255}
	if sv.cx != sv.cx || sv.cy != sv.cy || sv.cz != sv.cz || sv.cw != sv.cw {
		return screenVertex{}
	}

	var color []float32
	var uv [2]float32
	if ps := s.curPS; ps != nil {
		pst := ps.state
		for _, in := range ps.prog.Inputs {
			if out := vs.prog.Output(in.Semantic, in.Index); out != nil {
				n := min(in.Components, out.Components)
				copy(pst.M[in.Offset:in.Offset+n], st.M[out.Offset:out.Offset+n])
			}
		}
		pst.Run()
		if tc, ok := pst.TexCoord(ps.texUnit); ok && len(tc) >= 2 {
			uv[0], uv[1] = tc[0], tc[1]
		}
		if out := ps.prog.Output("COLOR", 0); out != nil && out.Components >= 4 {
			color = pst.M[out.Offset : out.Offset+4]
		}
	} else {
		if out := vs.prog.Output("COLOR", 0); out != nil && out.Components >= 4 {
			color = st.M[out.Offset : out.Offset+4]
		}
		if out := vs.prog.Output("TEXCOORD", 0); out != nil && out.Components >= 2 {
			uv[0], uv[1] = st.M[out.Offset], st.M[out.Offset+1]
		}
	}
	if color != nil {
		sv.r, sv.g, sv.b, sv.a = clampByte(color[0]), clampByte(color[1]), clampByte(color[2]), clampByte(color[3])
	}
	sv.u, sv.v = uv[0], uv[1]
	if sv.cw > nearW {
		projectClip(&sv)
	}
	return sv
}

// realTexture returns the texture sampled by the bound pixel shader's first lookup.
func (s *Subsystem) realTexture(c *cpu.CPU) *BufferDesc {
	if ps := s.curPS; ps != nil && ps.texUnit >= 0 && ps.texUnit < len(s.BoundTextures) {
		if t := s.BoundTextures[ps.texUnit]; t != 0 {
			return s.ensureTextureDecoded(c, t)
		}
	}
	return nil
}

// realShading reports whether the current draw goes through the real shader pipeline.
func (s *Subsystem) realShading() bool {
	return s.RealShaders && s.curVS != nil && s.CurrentVD != nil && len(s.CurrentVD.Elements) > 0
}

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
	handles map[uint32]*hlsl.Constant // D3DXHANDLE (guest pointer to the name) -> constant
	byName  map[string]uint32
	ordered []uint32 // handles in table order
	texUnit int      // sampler register of the first texture lookup (-1: none)
	maxReg  int      // one past the highest float constant register read (-1: not computed)
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
		byName: map[string]uint32{}, texUnit: -1, maxReg: -1}
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

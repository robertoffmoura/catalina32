// Package msl holds the types the shader translators (pkg/hlsl for HLSL source, pkg/d3dsm for
// shader bytecode) use to describe the interface of the Metal functions they generate, so a
// vertex function from either can be paired with a fragment function from either.
package msl

// Attribute is a vertex function input: the D3D vertex element it reads, by semantic.
type Attribute struct {
	Semantic string // upper-case D3D usage name: "POSITION", "COLOR", "TEXCOORD", ...
	Index    int
}

// AttrClass is how the vertex data of an attribute reaches the shader.
type AttrClass uint8

const (
	AttrFloat AttrClass = iota // float, normalized or half formats
	AttrInt                    // non-normalized signed integer formats (SHORT2, SHORT4)
	AttrUint                   // non-normalized unsigned integer formats (UBYTE4)
)

// AttrType is the MSL type a vertex function declares for an attribute of class c.
func AttrType(c AttrClass) string {
	switch c {
	case AttrInt:
		return "int4"
	case AttrUint:
		return "uint4"
	}
	return "float4"
}

// Varying is a vertex function output read by fragment functions. Name is the Metal user
// attribute linking the two ("COLOR0", "TEXCOORD3", ...).
type Varying struct {
	Name string
	Cols int  // 1..4
	Int  bool // int vector (else float)
}

// TypeName is the MSL type of the varying.
func (v Varying) TypeName() string {
	base := "float"
	if v.Int {
		base = "int"
	}
	if v.Cols == 1 {
		return base
	}
	return base + string(rune('0'+v.Cols))
}

// TextureKind is the dimension of a texture unit's binding.
type TextureKind uint8

const (
	Tex2D TextureKind = iota
	TexCube
	Tex3D
)

// Texture is a texture/sampler unit a shader reads.
type Texture struct {
	Unit int
	Kind TextureKind
}

// TypeName is the MSL texture type of the unit.
func (t Texture) TypeName() string {
	switch t.Kind {
	case TexCube:
		return "texturecube<float>"
	case Tex3D:
		return "texture3d<float>"
	}
	return "texture2d<float>"
}

// Shader is a translated vertex or fragment program.
type Shader interface {
	// Attributes lists the vertex inputs (vertex shaders).
	Attributes() []Attribute
	// Varyings lists the outputs a fragment function can read (vertex shaders).
	Varyings() []Varying
	// Textures lists the texture units read.
	Textures() []Texture
	// VertexMSL builds the complete source of a vertex function "vs_main" for vertex data of
	// the given classes (by attribute index).
	VertexMSL(attrs []AttrClass) string
	// FragmentMSL builds the complete source of a fragment function "ps_main" whose inputs come
	// from a vertex function with the given varyings (inputs it does not provide read as zero).
	FragmentMSL(provided []Varying) string
}

// Buffer and register conventions shared by every generated function:
//
//	buffer(0): float constant registers (float4[256])
//	buffer(1): renderer state: S[0].xy = D3D9 half-pixel offset in clip units (vertex);
//	           S[0].x = alpha test function (D3DCMPFUNC, 8 = always), S[0].y = reference (fragment)
//	buffer(2+i): vertex stream i
//	texture(n)/sampler(n): sampler register n
const (
	ConstantsBuffer   = 0
	StateBuffer       = 1
	FirstVertexBuffer = 2
)

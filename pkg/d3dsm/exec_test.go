package d3dsm

import (
	"math"
	"testing"
)

// tokens for:
//
//	vs_1_1
//	def c4, 1, 0, 0, 2
//	dcl_position v0
//	dcl_color v1
//	dp4 oPos.x, v0, c0 ... (m4x4 oPos, v0, c0)
//	mul oD0, v1, c4.wwww       (2x colour)
//	rcp oT0.x, c4.w            (scalar, w component)
func TestVS(t *testing.T) {
	f := func(x float32) uint32 { return mathFloat32bits(x) }
	tok := []uint32{
		0xFFFE0101,
		0x00000051, 0xA00F0004, f(1), f(0), f(0), f(2), // def c4
		0x0000001F, 0x80000000, 0x900F0000, // dcl_position v0
		0x0000001F, 0x8000000A, 0x900F0001, // dcl_color v1
		0x00000014, 0xC00F0000, 0x90E40000, 0xA0E40000, // m4x4 oPos, v0, c0
		0x00000005, 0xD00F0000, 0x90E40001, 0xA0FF0004, // mul oD0, v1, c4.wwww
		0x00000006, 0xE0010000, 0xA0E40004, // rcp oT0.x, c4 (no swizzle: w)
		0x0000FFFF,
	}
	sh, err := Decode(tok)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(sh)
	if err != nil {
		t.Fatal(err)
	}
	m := &Machine{C: make([]Vec, 256)}
	m.C[0], m.C[1], m.C[2], m.C[3] = Vec{2, 0, 0, 0}, Vec{0, 3, 0, 0}, Vec{0, 0, 4, 0}, Vec{0, 0, 0, 1}
	p.ApplyDefs(m.C)
	m.F[FileV+0] = Vec{1, 1, 1, 1}
	m.F[FileV+1] = Vec{0.25, 0.5, 0.75, 0.5}
	p.Run(m)
	if m.F[FileOPos] != (Vec{2, 3, 4, 1}) {
		t.Errorf("oPos = %v", m.F[FileOPos])
	}
	if m.F[FileOD] != (Vec{0.5, 1, 1.5, 1}) {
		t.Errorf("oD0 = %v", m.F[FileOD])
	}
	if m.F[FileOT][0] != 0.5 {
		t.Errorf("oT0.x = %v", m.F[FileOT][0])
	}
	if len(p.Inputs) != 2 || p.Inputs[1].Usage != 10 {
		t.Errorf("inputs = %v", p.Inputs)
	}
}

// ps_1_1: tex t0; mul_x2 r0, t0, v0
func TestPS11(t *testing.T) {
	tok := []uint32{
		0xFFFF0101,
		0x00000042, 0xB00F0000, // tex t0
		0x00000005, 0x810F0000, 0xB0E40000, 0x90E40000, // mul_x2 r0, t0, v0
		0x0000FFFF,
	}
	sh, err := Decode(tok)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(sh)
	if err != nil {
		t.Fatal(err)
	}
	m := &Machine{C: make([]Vec, 8), Sample: func(s int, uv Vec) Vec { return Vec{uv[0], uv[1], 1, 0.5} }}
	m.F[FileT+0] = Vec{0.25, 0.5, 0, 0}
	m.F[FileV+0] = Vec{1, 1, 0.5, 1}
	p.Run(m)
	if m.F[FileOC] != (Vec{0.5, 1, 1, 1}) {
		t.Errorf("oC0 = %v", m.F[FileOC])
	}
}

func mathFloat32bits(x float32) uint32 { return math.Float32bits(x) }

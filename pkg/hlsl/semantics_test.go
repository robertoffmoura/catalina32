package hlsl

import (
	"math"
	"testing"
)

const testShader = `
#define SCALE 2.0
struct VIn  { float4 pos: POSITION; half4 col: COLOR0; float2 uv: TEXCOORD0; };
struct VOut { float4 pos: POSITION; half4 col: COLOR0; float3 n: TEXCOORD1_centroid; };
shared uniform float4x4 viewProj: SEMANTIC_X: register(c0);
uniform float4x4 bones[2];
uniform half4 tint;
uniform float3x3 unusedByEntry;
static const half k = 0.5;
VOut vout;

float3 twice(float3 v) { return v * SCALE; }
float4 twice(float4 v) { return v * 3.0; }
void split(in float4 v, out float2 a, out float b) { a = v.zw; b = v.x; }

VOut main(VIn vin) {
	float4 p = mul(vin.pos, bones[int(vin.uv.x)]);
	vout.pos = mul(p, viewProj);
	vout.col = vin.col.bgra * tint;
	vout.col.a = k;
	float2 a; float b;
	split(float4(1, 2, 3, 4), a, b);
	vout.n = twice(float3(a, b));
	vout.n.x += mul(float3(1, 0, 0), (float3x3)viewProj).x - 1.0;
	if (vin.uv.y > 0.5) { vout.n.z = -1; } else { vout.n.z = twice(float4(1,1,1,1)).w; }
	return vout;
}
float4 other() { return mul(float4(1,0,0,0), (float4x4)unusedByEntry._m00); }
`

func near(a, b float32) bool { return math.Abs(float64(a-b)) < 1e-5 }

func TestSemantics(t *testing.T) {
	f, err := Parse(testShader)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(f, "main")
	if err != nil {
		t.Fatal(err)
	}
	// Constants: only reachable uniforms; explicit register kept; others allocated around it.
	got := map[string][2]int{}
	for _, k := range p.Constants {
		got[k.Name] = [2]int{k.RegisterIndex, k.RegisterCount}
	}
	if len(got) != 3 || got["viewProj"] != [2]int{0, 4} || got["bones"] != [2]int{4, 8} || got["tint"] != [2]int{12, 1} {
		t.Fatalf("constant table = %v", got)
	}

	regs := make([][4]float32, 16)
	// viewProj = translation by (10,20,30) as a row-vector matrix: M[3] = (10,20,30,1).
	// Column-major packing: register c holds column c, i.e. M[r][c] in regs[c][r].
	vp := [4][4]float32{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {10, 20, 30, 1}}
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			regs[c][r] = vp[r][c]
		}
	}
	// bones[1] = uniform scale 2 (w kept 1); bones[0] = identity.
	for i := 0; i < 4; i++ {
		regs[4+i][i] = 1
		regs[8+i][i] = 2
	}
	regs[11][3] = 1
	regs[12] = [4]float32{1, 0.5, 0.25, 1} // tint

	s := p.NewState()
	s.LoadConstants(regs)
	// Lane 0 takes the if branch, lane 1 the else branch, in the same run.
	in := func(lane int, sem string, idx int, v ...float32) {
		vy := p.Input(sem, idx)
		if vy == nil {
			t.Fatalf("no input %s%d", sem, idx)
		}
		for i, f := range v {
			s.M[vy.Offset+i][lane] = f
		}
	}
	for lane := 0; lane < 2; lane++ {
		in(lane, "POSITION", 0, 1, 2, 3, 1)
		in(lane, "COLOR", 0, 0.1, 0.2, 0.3, 0.4)
	}
	in(0, "TEXCOORD", 0, 1.7, 0.9) // bone index int(1.7) = 1; uv.y > 0.5
	in(1, "TEXCOORD", 0, 0, 0.1)
	s.Run(2)
	out := func(lane int, sem string, idx int) []float32 {
		vy := p.Output(sem, idx)
		if vy == nil {
			t.Fatalf("no output %s%d", sem, idx)
		}
		var r []float32
		for i := 0; i < vy.Components; i++ {
			r = append(r, s.M[vy.Offset+i][lane])
		}
		return r
	}
	want := map[string][]float32{
		"POSITION": {2 + 10, 4 + 20, 6 + 30, 1},           // scaled by bone 1, then translated
		"COLOR":    {0.3 * 1, 0.2 * 0.5, 0.1 * 0.25, 0.5}, // .bgra * tint, a = k
		"TEXCOORD": {3*2 + 1 - 1, 4 * 2, -1},              // twice(float3(3,4,1)) ; x += M[0][0]-1 ; z = -1
	}
	for sem, w := range want {
		idx := 0
		if sem == "TEXCOORD" {
			idx = 1
		}
		g := out(0, sem, idx)
		for i := range w {
			if !near(g[i], w[i]) {
				t.Errorf("%s = %v, want %v", sem, g, w)
				break
			}
		}
	}
	// Else branch (lane 1, bone 0 = identity) and float4 overload.
	if g := out(1, "TEXCOORD", 1); !near(g[2], 3) {
		t.Errorf("else branch: n.z = %v, want 3", g[2])
	}
	if g := out(1, "POSITION", 0); !near(g[0], 1+10) {
		t.Errorf("lane 1 position = %v, want x = 11", g)
	}
}

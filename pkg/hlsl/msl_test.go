package hlsl

// Checks TranslateMSL by running the generated MSL: it is compiled as C++ against
// testdata/metal_stub.h (a stand-in for the Metal standard library) with clang, run on random
// constants and inputs, and compared with the closure evaluator on the same data. The game's
// own shaders are covered when HLSL_TEST_DIR points at a directory of dumped .hlsl files
// (NNNN_vs_3_0_entry.hlsl / NNNN_ps_3_0_entry.hlsl, from C32_DUMP_SHADERS).

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"catalina32/pkg/msl"
)

const mslVectors = 8 // random input sets per shader

// harnessSample is the texture function shared by the C++ harness and the evaluator.
func harnessSample(unit int, c []float32, cube bool) [4]float32 {
	fr := func(x float32) float32 { return x - float32(math.Floor(float64(x))) }
	if cube {
		return [4]float32{fr(c[0] * 1.3), fr(c[1] * 0.7), fr(c[2] * 2.1), 1}
	}
	u := float32(unit)
	return [4]float32{fr(c[0]*1.37 + u*0.1), fr(c[1] * 2.11), fr((c[0] + c[1]) * 0.73), 0.5 + 0.1*u}
}

const harnessSampleCpp = `
extern "C" void harness_sample(int unit, const float* c, int n, float* out) {
	auto fr = [](float x) { return x - __builtin_floorf(x); };
	if (unit & 0x100) {
		out[0] = fr(c[0] * 1.3f); out[1] = fr(c[1] * 0.7f); out[2] = fr(c[2] * 2.1f); out[3] = 1;
		return;
	}
	float u = float(unit);
	out[0] = fr(c[0] * 1.37f + u * 0.1f); out[1] = fr(c[1] * 2.11f); out[2] = fr((c[0] + c[1]) * 0.73f); out[3] = 0.5f + 0.1f * u;
}
bool metal::harness_discarded = false;
`

type mslCase struct {
	name   string
	src    string
	vsName string // vertex entry (always compiled: the fragment stage takes its varyings from it)
	psName string // fragment entry ("" for a vertex-only test)
	vertex bool   // test the vertex stage (else the fragment stage)
}

func TestMSLMatchesEvaluator(t *testing.T) {
	if _, err := exec.LookPath("clang++"); err != nil {
		t.Skip("clang++ not installed")
	}
	cases := []mslCase{
		{"inline-vs", mslTestShader, "vsMain", "psMain", true},
		{"inline-ps", mslTestShader, "vsMain", "psMain", false},
	}
	if dir := os.Getenv("HLSL_TEST_DIR"); dir != "" {
		files, _ := filepath.Glob(filepath.Join(dir, "*.hlsl"))
		sort.Strings(files)
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			base := filepath.Base(f)
			parts := strings.SplitN(strings.TrimSuffix(base, ".hlsl"), "_", 5)
			if len(parts) < 5 {
				continue
			}
			cases = append(cases, mslCase{base, string(src), "vertexProgram", "fragmentProgram", parts[1] == "vs"})
		}
	}
	work, err := os.MkdirTemp("", "msltest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	stub, _ := filepath.Abs("testdata")

	var mu sync.Mutex
	var failures []string
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, tc := range cases {
		wg.Add(1)
		go func(i int, tc mslCase) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := runMSLCase(tc, filepath.Join(work, fmt.Sprintf("c%d", i)), stub); err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s (%s): %v", tc.name, map[bool]string{true: "vertex", false: "fragment"}[tc.vertex], err))
				mu.Unlock()
			}
		}(i, tc)
	}
	wg.Wait()
	sort.Strings(failures)
	for i, f := range failures {
		if i < 10 {
			t.Error(f)
		}
	}
	if len(failures) > 0 {
		t.Errorf("%d of %d shaders failed", len(failures), len(cases))
	}
	t.Logf("%d shaders checked", len(cases))
}

func compileEntry(src, entry string) (*File, *Program, *MSLModule, error) {
	file, err := Parse(src)
	if err != nil {
		return nil, nil, nil, err
	}
	prog, err := Compile(file, entry)
	if err != nil {
		return nil, nil, nil, err
	}
	mod, err := TranslateMSL(file, prog)
	return file, prog, mod, err
}

var _ msl.Shader = (*MSLModule)(nil)

func runMSLCase(tc mslCase, dir, stub string) error {
	_, vprog, vmod, err := compileEntry(tc.src, tc.vsName)
	if err != nil {
		return fmt.Errorf("vertex: %v", err)
	}
	prog, mod := vprog, vmod
	if !tc.vertex {
		if _, prog, mod, err = compileEntry(tc.src, tc.psName); err != nil {
			return fmt.Errorf("fragment: %v", err)
		}
	}
	if len(prog.Inputs) != len(mod.Inputs) || len(prog.Outputs) != len(mod.Outputs) {
		return fmt.Errorf("varying lists differ: evaluator %d/%d, msl %d/%d", len(prog.Inputs), len(prog.Outputs), len(mod.Inputs), len(mod.Outputs))
	}
	r := rand.New(rand.NewSource(int64(len(tc.src))))
	regs := make([][4]float32, 256)
	for i := range regs {
		for j := range regs[i] {
			regs[i][j] = float32(r.Float64()*4 - 2)
		}
	}
	pos := [4]float32{100.5, 200.5, 0.25, 1}

	// Fragment inputs present in the vertex outputs (others read as zero).
	provided := map[string]*Type{}
	for _, v := range vmod.Varyings() {
		provided[v.Name] = varyingType(v)
	}
	inputs := make([][][4]float32, mslVectors)
	for k := range inputs {
		inputs[k] = make([][4]float32, len(mod.Inputs))
		for i, l := range mod.Inputs {
			for j := 0; j < 4; j++ {
				v := float32(r.Float64()*4 - 2)
				if l.Type.Base != BFloat {
					v = float32(r.Intn(5) - 2)
				}
				inputs[k][i][j] = v
			}
		}
	}

	// Expected values from the evaluator.
	prog.Sampler = harnessSample
	st := prog.NewState()
	st.LoadConstants(regs)
	var want [][]float32
	for k := 0; k < mslVectors; k++ {
		for i, l := range mod.Inputs {
			v := prog.Inputs[i]
			var val [4]float32
			switch {
			case tc.vertex:
				val = inputs[k][i]
			case l.Semantic == "POSITION" || l.Semantic == "VPOS":
				val = pos
			case provided[varyingName(l)] != nil:
				copy(val[:], widenVals(inputs[k][i], provided[varyingName(l)].Cols))
			}
			for c := 0; c < v.Components && c < 4; c++ {
				x := val[c]
				switch v.Type.Base {
				case BInt:
					x = float32(math.Trunc(float64(x)))
				case BBool:
					x = b2f(x != 0)
				}
				st.M[v.Offset+c][0] = x
			}
		}
		st.Run(1)
		var out []float32
		for _, v := range prog.Outputs {
			var val [4]float32
			for c := 0; c < v.Components; c++ {
				val[c] = st.M[v.Offset+c][0]
			}
			out = append(out, widenVals(val, v.Components)...)
		}
		want = append(want, out)
	}

	// The same through the generated MSL.
	var src string
	if tc.vertex {
		src = mod.VertexMSL(nil)
	} else {
		src = mod.FragmentMSL(vmod.Varyings())
	}
	if !strings.Contains(src, mslPrelude) {
		return fmt.Errorf("prelude not found in generated source")
	}
	src = strings.Replace(src, mslPrelude, "", 1)
	var h strings.Builder
	h.WriteString("#include \"metal_stub.h\"\n")
	h.WriteString(src)
	h.WriteString(harnessSampleCpp)
	h.WriteString("static float4 Cv[256];\nstatic float4 Sv[4];\n")
	fmt.Fprintf(&h, "static const float REGS[256][4] = {%s};\n", floatRows(regs))
	h.WriteString("int main() {\n\tfor (int i = 0; i < 256; i++) Cv[i] = float4(REGS[i][0], REGS[i][1], REGS[i][2], REGS[i][3]);\n")
	h.WriteString("\tSv[0] = float4(0.0f, 0.0f, 0.0f, 0.0f);\n\tSv[0].x = tc_vertex ? 0.0f : 8.0f;\n")
	texArgs := ""
	for _, tx := range mod.Textures() {
		texArgs += fmt.Sprintf(", %s{%d}, sampler{}", tx.TypeName(), tx.Unit)
	}
	for k := 0; k < mslVectors; k++ {
		h.WriteString("\t{\n")
		if tc.vertex {
			h.WriteString("\t\tVIn vin;\n")
			for i := range mod.Inputs {
				v := inputs[k][i]
				fmt.Fprintf(&h, "\t\tvin.a%d = float4(%s);\n", i, floatList(v[:]))
			}
			fmt.Fprintf(&h, "\t\tVOut o = vs_main(vin, Cv, Sv%s);\n", texArgs)
			h.WriteString("\t\tprintf(\"%.9g %.9g %.9g %.9g \", o.pos.x, o.pos.y, o.pos.z, o.pos.w);\n")
			for _, l := range mod.Outputs {
				if l.Semantic == "POSITION" && l.Index == 0 {
					continue
				}
				printWidened(&h, "o."+varyingName(l), l.Type)
			}
		} else {
			fmt.Fprintf(&h, "\t\tPIn pin;\n\t\tpin.pos = float4(%s);\n", floatList(pos[:]))
			set := map[string]bool{}
			for i, l := range mod.Inputs {
				n := varyingName(l)
				vt := provided[n]
				if vt == nil || set[n] || l.Semantic == "POSITION" {
					continue
				}
				set[n] = true
				v := inputs[k][i]
				if vt.Cols == 1 {
					fmt.Fprintf(&h, "\t\tpin.%s = %s;\n", n, floatLit(float64(v[0])))
				} else {
					fmt.Fprintf(&h, "\t\tpin.%s = float%d(%s);\n", n, vt.Cols, floatList(v[:vt.Cols]))
				}
			}
			h.WriteString("\t\tharness_discarded = false;\n")
			fmt.Fprintf(&h, "\t\tPOut o = ps_main(pin, Cv, Sv%s);\n", texArgs)
			for _, l := range mod.Outputs {
				if l.Semantic == "COLOR" {
					fmt.Fprintf(&h, "\t\tprintf(\"%%.9g %%.9g %%.9g %%.9g \", o.c%d.x, o.c%d.y, o.c%d.z, o.c%d.w);\n", l.Index, l.Index, l.Index, l.Index)
				}
			}
		}
		h.WriteString("\t\tprintf(\"\\n\");\n\t}\n")
	}
	h.WriteString("\treturn 0;\n}\n")
	cpp := strings.ReplaceAll(h.String(), "tc_vertex", strconv.FormatBool(tc.vertex))

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	srcPath, bin := filepath.Join(dir, "s.cpp"), filepath.Join(dir, "s")
	if err := os.WriteFile(srcPath, []byte(cpp), 0644); err != nil {
		return err
	}
	if out, err := exec.Command("clang++", "-std=c++20", "-O1", "-w", "-ferror-limit=5", "-I", stub, "-o", bin, srcPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clang: %v\n%s(source kept in %s)", err, firstLines(string(out), 12), srcPath)
	}
	out, err := exec.Command(bin).Output()
	if err != nil {
		return fmt.Errorf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != mslVectors {
		return fmt.Errorf("harness printed %d lines", len(lines))
	}
	for k, line := range lines {
		got := strings.Fields(line)
		w := want[k]
		if !tc.vertex {
			w = nil
			for i, v := range prog.Outputs {
				if mod.Outputs[i].Semantic == "COLOR" {
					w = append(w, want[k][4*i:4*i+4]...)
				}
				_ = v
			}
		} else {
			// The vertex harness prints the position first, then the other outputs in order.
			var p, rest []float32
			for i := range prog.Outputs {
				l := mod.Outputs[i]
				if l.Semantic == "POSITION" && l.Index == 0 {
					p = want[k][4*i : 4*i+4]
				} else {
					rest = append(rest, want[k][4*i:4*i+4]...)
				}
			}
			if p == nil {
				p = []float32{0, 0, 0, 1}
			}
			w = append(append([]float32{}, p...), rest...)
		}
		if len(got) != len(w) {
			return fmt.Errorf("vector %d: %d values, want %d", k, len(got), len(w))
		}
		for i := range got {
			g, err := strconv.ParseFloat(strings.TrimPrefix(got[i], "-"), 32) // also "nan", "-nan"
			if strings.HasPrefix(got[i], "-") {
				g = -g
			}
			if err != nil {
				return fmt.Errorf("vector %d: bad output %q", k, got[i])
			}
			if !closeEnough(float32(g), w[i]) {
				return fmt.Errorf("vector %d value %d: msl %v, evaluator %v (source in %s)", k, i, g, w[i], srcPath)
			}
		}
	}
	return nil
}

func closeEnough(a, b float32) bool {
	if a != a || b != b {
		return (a != a) == (b != b)
	}
	if math.IsInf(float64(a), 0) || math.IsInf(float64(b), 0) {
		return a == b || math.Abs(float64(a)) > 1e30 && math.Abs(float64(b)) > 1e30
	}
	d := math.Abs(float64(a - b))
	return d <= 1e-4 || d <= 1e-4*math.Max(math.Abs(float64(a)), math.Abs(float64(b)))
}

// widenVals keeps the first n components of v and fills the rest like widen() (0, 0, 1).
func widenVals(v [4]float32, n int) []float32 {
	out := []float32{0, 0, 0, 1}
	copy(out, v[:n])
	return out
}

func printWidened(h *strings.Builder, x string, t *Type) {
	w := widen(x, t)
	fmt.Fprintf(h, "\t\t{ float4 v = %s; printf(\"%%.9g %%.9g %%.9g %%.9g \", v.x, v.y, v.z, v.w); }\n", w)
}

func floatList(v []float32) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = floatLit(float64(x))
	}
	return strings.Join(s, ", ")
}

func floatRows(rows [][4]float32) string {
	s := make([]string, len(rows))
	for i, r := range rows {
		s[i] = "{" + floatList(r[:]) + "}"
	}
	return strings.Join(s, ", ")
}

func firstLines(s string, n int) string {
	l := strings.SplitN(s, "\n", n+1)
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, "\n") + "\n"
}

// mslTestShader exercises the translation rules on a small program of its own.
const mslTestShader = `
struct VIn { float4 pos : POSITION; float3 nrm : NORMAL; half4 col : COLOR0; float2 uv : TEXCOORD0; };
struct VOut { float4 pos : POSITION; half4 col : COLOR0; float2 uv : TEXCOORD0; float3 n : TEXCOORD1; float s : TEXCOORD2; };
uniform float4x4 wvp;
uniform float4x3 bones[2];
uniform float3 lightDir;
uniform float4 tint : register(c40);
uniform sampler2D tex0;
static float k = 0.5;
float4 g_scratch;

void shade(float3 n, out float3 diff, inout float amount) {
	diff = saturate(dot(n, -lightDir)) * float3(1, 0.5, 0.25);
	amount += length(n) * 0.1;
}

float2 swap(float2 v) { return v.yx; }

VOut vsMain(VIn input) {
	VOut o;
	g_scratch = 0; // the evaluator keeps globals between invocations; a GPU does not
	float4 p = mul(input.pos, wvp);
	float3 b = mul(input.pos, bones[1]);
	float3x3 m = (float3x3)wvp;
	o.pos = p + float4(b * 0.01, 0);
	o.n = normalize(mul(m, input.nrm));
	float3 d;
	float a = 0;
	shade(o.n, d, a);
	o.col = input.col * float4(d, 1) * k;
	o.uv = swap(input.uv) * 2 + tint.xy;
	o.s = a > 0.5 ? a : -a;
	g_scratch.xy = o.uv;
	shade(o.n, d, g_scratch.z);
	o.s += g_scratch.z + (all(o.n) ? 1 : 0);
	if (input.pos.x > 0) {
		o.col.rg = frac(o.col.gr * 3);
	} else
		o.col = lerp(o.col, tint, 0.25);
	float arr[3] = { 1, 2, 3 };
	int idx = (int)(input.uv.x * 3);
	o.s += arr[idx] + fmod(input.uv.y * 7, 2) + pow(abs(input.uv.x), 1.5);
	return o;
}

struct POut { float4 color : COLOR0; };

POut psMain(VOut input) {
	POut o;
	float4 t = tex2D(tex0, input.uv);
	float3 n = normalize(input.n);
	o.color = t * input.col + float4(n * 0.1, input.s);
	o.color.a = step(0.3, t.a) * max(input.s, 0.1);
	o.color.rgb = lerp(o.color.rgb, tint.rgb, smoothstep(0, 1, t.r));
	return o;
}
`

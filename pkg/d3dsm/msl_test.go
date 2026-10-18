package d3dsm

// Checks the MSL translation of bytecode against the interpreter: every shader of the game
// executable named by C32_BIN_PATH is translated, compiled as C++ against
// ../hlsl/testdata/metal_stub.h with clang, run on random registers and inputs, and compared
// with Run on the same data.

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"catalina32/pkg/macho32"
	"catalina32/pkg/msl"
)

// The game's shaders, by address (C32_DRAW_LOG / createProg).
var gameShaders = []uint32{0x8e2028, 0x8e20d0, 0x8e217c, 0x8e23d8, 0x8e25d0, 0x8e26c0, 0x8e276c, 0x8e290c,
	0x8e2b14, 0x8e2c0c, 0x8e2ebc, 0x8e3044, 0x8e3448, 0x8e5818, 0x8e5ae8, 0x8e63aa, 0x8e64da, 0x8e658a,
	0x8e66ee, 0x8e6b16, 0x8e6f1e, 0x8e708a, 0x8e71ba, 0x8e7266}

const vectors = 8

func harnessSample(unit int, c Vec, kind msl.TextureKind) Vec {
	fr := func(x float32) float32 { return x - float32(math.Floor(float64(x))) }
	if kind != msl.Tex2D {
		return Vec{fr(c[0] * 1.3), fr(c[1] * 0.7), fr(c[2] * 2.1), 1}
	}
	u := float32(unit)
	return Vec{fr(c[0]*1.37 + u*0.1), fr(c[1] * 2.11), fr((c[0] + c[1]) * 0.73), 0.5 + 0.1*u}
}

const harnessCpp = `
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

func loadShaders(t *testing.T) map[uint32]*Program {
	path := os.Getenv("C32_BIN_PATH")
	if path == "" {
		t.Skip("C32_BIN_PATH not set (the game's shaders are needed)")
	}
	img, err := macho32.LoadImage(path)
	if err != nil {
		t.Skip(err)
	}
	mem := make([]byte, 0x04000000)
	if err := img.MapIntoArena(mem); err != nil {
		t.Fatal(err)
	}
	out := map[uint32]*Program{}
	for _, a := range gameShaders {
		var tok []uint32
		for p := a; len(tok) < 1<<16; p += 4 {
			v := binary.LittleEndian.Uint32(mem[p:])
			tok = append(tok, v)
			if v == 0x0000FFFF {
				break
			}
		}
		sh, err := Decode(tok)
		if err != nil {
			t.Fatalf("%#x: %v", a, err)
		}
		p, err := Compile(sh)
		if err != nil {
			t.Fatalf("%#x: %v", a, err)
		}
		out[a] = p
	}
	return out
}

func TestMSLMatchesInterpreter(t *testing.T) {
	if _, err := exec.LookPath("clang++"); err != nil {
		t.Skip("clang++ not installed")
	}
	progs := loadShaders(t)
	stub, _ := filepath.Abs("../hlsl/testdata")
	work := t.TempDir()
	for _, a := range gameShaders {
		p := progs[a]
		t.Run(fmt.Sprintf("%#x", a), func(t *testing.T) {
			if err := checkShader(p, filepath.Join(work, fmt.Sprintf("s%x", a)), stub, int64(a)); err != nil {
				t.Error(err)
			}
		})
	}
}

func lit(f float32) string {
	switch {
	case f != f:
		return "NAN"
	case math.IsInf(float64(f), 0):
		return mslFloat(f)
	}
	return mslFloat(f)
}

func vlit(v Vec) string {
	return fmt.Sprintf("float4(%s, %s, %s, %s)", lit(v[0]), lit(v[1]), lit(v[2]), lit(v[3]))
}

func checkShader(p *Program, dir, stub string, seed int64) error {
	r := rand.New(rand.NewSource(seed))
	regs := make([]Vec, 256)
	for i := range regs {
		for k := range regs[i] {
			regs[i][k] = float32(r.Float64()*4 - 2)
			if p.ps1 {
				regs[i][k] = float32(r.Float64()*2 - 1) // the host clamps ps_1_x constants
			}
		}
	}
	consts := append([]Vec(nil), regs...)
	p.ApplyDefs(consts)
	vertex := !p.Shader.Pixel
	kinds := map[int]msl.TextureKind{}
	for _, tx := range p.Textures() {
		kinds[tx.Unit] = tx.Kind
	}
	// Pixel shaders read every colour and texture coordinate from a full set of varyings.
	all := []msl.Varying{{Name: "COLOR0", Cols: 4}, {Name: "COLOR1", Cols: 4}}
	for i := 0; i < 8; i++ {
		all = append(all, msl.Varying{Name: fmt.Sprintf("TEXCOORD%d", i), Cols: 4})
	}

	var want [][]float32
	ins := make([][]Vec, vectors)
	for k := range ins {
		n := len(p.Inputs)
		if !vertex {
			n = 10 // v0, v1, t0..t7
		}
		ins[k] = make([]Vec, n)
		for i := range ins[k] {
			for c := range ins[k][i] {
				ins[k][i][c] = float32(r.Float64()*4 - 2)
				if !vertex && i < 2 {
					ins[k][i][c] = float32(r.Float64()) // colours arrive clamped
				}
			}
		}
		m := &Machine{C: consts, Sample: func(s int, uv Vec) Vec { return harnessSample(s, uv, kinds[s]) }}
		var out []float32
		if vertex {
			for i, d := range p.Inputs {
				m.F[FileV+d.Reg&15] = ins[k][i]
			}
			m.F[FileOD] = Vec{1, 1, 1, 1}
			p.Run(m)
			out = append(out, m.F[FileOPos][:]...)
			for _, v := range p.Varyings() {
				var x Vec
				switch v.Name {
				case "COLOR0", "COLOR1":
					x = m.F[FileOD+int(v.Name[5]-'0')]
					for c := range x {
						x[c] = min(max(x[c], 0), 1)
					}
				case "FOG0":
					x = m.F[FileOFog]
				default:
					n, _ := strconv.Atoi(strings.TrimPrefix(v.Name, "TEXCOORD"))
					x = m.F[FileOT+n]
				}
				out = append(out, x[:]...)
			}
		} else {
			for i := 0; i < 2; i++ {
				if p.ReadsV&(1<<i) != 0 {
					m.F[FileV+i] = ins[k][i]
				}
			}
			for i := 0; i < 8; i++ {
				if p.ReadsT&(1<<i) != 0 {
					m.F[FileT+i] = ins[k][2+i]
				}
			}
			p.Run(m)
			if p.ps1 {
				m.F[FileOC] = m.F[FileR]
			}
			out = append(out, m.F[FileOC][:]...)
			out = append(out, b2f(m.Kill))
		}
		want = append(want, out)
	}

	var src string
	if vertex {
		src = p.VertexMSL(nil)
	} else {
		src = p.FragmentMSL(all)
	}
	src = strings.Replace(src, "#include <metal_stdlib>\nusing namespace metal;\n", "", 1)
	var h strings.Builder
	h.WriteString("#include \"metal_stub.h\"\n" + src + harnessCpp)
	fmt.Fprintf(&h, "static float4 Cv[256];\nstatic float4 Sv[4];\nint main() {\n")
	for i, v := range regs {
		fmt.Fprintf(&h, "\tCv[%d] = %s;\n", i, vlit(v))
	}
	h.WriteString("\tSv[0] = float4(0.0f, 0.0f, 0.0f, 0.0f);\n")
	if !vertex {
		h.WriteString("\tSv[0].x = 8.0f;\n")
	}
	texArgs := ""
	for _, tx := range p.Textures() {
		texArgs += fmt.Sprintf(", %s{%d}, sampler{}", tx.TypeName(), tx.Unit)
	}
	for k := 0; k < vectors; k++ {
		h.WriteString("\t{\n")
		if vertex {
			h.WriteString("\t\tVIn vin;\n")
			for i := range p.Inputs {
				fmt.Fprintf(&h, "\t\tvin.a%d = %s;\n", i, vlit(ins[k][i]))
			}
			h.WriteString("\t\tVOut o = vs_main(vin, Cv, Sv);\n\t\tprintf(\"%.9g %.9g %.9g %.9g \", o.pos.x, o.pos.y, o.pos.z, o.pos.w);\n")
			for _, v := range p.Varyings() {
				fmt.Fprintf(&h, "\t\tprintf(\"%%.9g %%.9g %%.9g %%.9g \", o.%s.x, o.%s.y, o.%s.z, o.%s.w);\n", v.Name, v.Name, v.Name, v.Name)
			}
		} else {
			h.WriteString("\t\tPIn pin;\n\t\tpin.pos = float4(10.5f, 20.5f, 0.5f, 1.0f);\n")
			for i := 0; i < 2; i++ {
				if p.ReadsV&(1<<i) != 0 {
					fmt.Fprintf(&h, "\t\tpin.COLOR%d = %s;\n", i, vlit(ins[k][i]))
				}
			}
			for i := 0; i < 8; i++ {
				if p.ReadsT&(1<<i) != 0 {
					fmt.Fprintf(&h, "\t\tpin.TEXCOORD%d = %s;\n", i, vlit(ins[k][2+i]))
				}
			}
			fmt.Fprintf(&h, "\t\tharness_discarded = false;\n\t\tPOut o = ps_main(pin, Cv, Sv%s);\n", texArgs)
			h.WriteString("\t\tprintf(\"%.9g %.9g %.9g %.9g %d \", o.c0.x, o.c0.y, o.c0.z, o.c0.w, harness_discarded ? 1 : 0);\n")
		}
		h.WriteString("\t\tprintf(\"\\n\");\n\t}\n")
	}
	h.WriteString("\treturn 0;\n}\n")

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	srcPath, bin := filepath.Join(dir, "s.cpp"), filepath.Join(dir, "s")
	if err := os.WriteFile(srcPath, []byte(h.String()), 0644); err != nil {
		return err
	}
	if out, err := exec.Command("clang++", "-std=c++20", "-O1", "-w", "-ferror-limit=5", "-I", stub, "-o", bin, srcPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clang: %v\n%s", err, out)
	}
	out, err := exec.Command(bin).Output()
	if err != nil {
		return fmt.Errorf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != vectors {
		return fmt.Errorf("harness printed %d lines", len(lines))
	}
	for k, line := range lines {
		got := strings.Fields(line)
		w := want[k]
		if !vertex && w[4] == 1 { // killed: the colour does not matter
			if got[4] != "1" {
				return fmt.Errorf("vector %d: interpreter kills the pixel, msl does not", k)
			}
			continue
		}
		if len(got) != len(w) {
			return fmt.Errorf("vector %d: %d values, want %d", k, len(got), len(w))
		}
		for i := range got {
			g, err := strconv.ParseFloat(strings.TrimPrefix(got[i], "-"), 32)
			if err != nil {
				return fmt.Errorf("vector %d: bad output %q", k, got[i])
			}
			if strings.HasPrefix(got[i], "-") {
				g = -g
			}
			if !close32(float32(g), w[i]) {
				return fmt.Errorf("vector %d value %d: msl %v, interpreter %v (%s)", k, i, g, w[i], srcPath)
			}
		}
	}
	return nil
}

func close32(a, b float32) bool {
	if a != a || b != b {
		return (a != a) == (b != b)
	}
	if math.IsInf(float64(a), 0) || math.IsInf(float64(b), 0) {
		return a == b
	}
	d := math.Abs(float64(a - b))
	return d <= 1e-4 || d <= 1e-4*math.Max(math.Abs(float64(a)), math.Abs(float64(b)))
}

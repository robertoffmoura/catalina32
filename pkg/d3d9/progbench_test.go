package d3d9

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"syscall"
	"testing"

	"catalina32/pkg/d3dsm"
	"catalina32/pkg/macho32"
)

// loadGameShader reads a shader token stream from the game executable named by C32_BIN_PATH (the
// benchmark needs real shipped shaders; none are vendored).
func loadGameShader(tb testing.TB, addr uint32) *d3dsm.Program {
	return shaderAt(tb, gameMemory(tb), addr)
}

// gameMemory maps the game executable named by C32_BIN_PATH.
func gameMemory(tb testing.TB) []byte {
	path := os.Getenv("C32_BIN_PATH")
	if path == "" {
		tb.Skip("C32_BIN_PATH not set")
	}
	img, err := macho32.LoadImage(path)
	if err != nil {
		tb.Skip(err)
	}
	mem := make([]byte, 0x04000000)
	if err := img.MapIntoArena(mem); err != nil {
		tb.Fatal(err)
	}
	return mem
}

func shaderAt(tb testing.TB, mem []byte, addr uint32) *d3dsm.Program {
	p, err := decodeShaderAt(mem, addr)
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

func decodeShaderAt(mem []byte, addr uint32) (*d3dsm.Program, error) {
	var tok []uint32
	for p := addr; len(tok) < 1<<16; p += 4 {
		t := binary.LittleEndian.Uint32(mem[p:])
		tok = append(tok, t)
		if t == 0x0000FFFF {
			break
		}
	}
	sh, err := d3dsm.Decode(tok)
	if err != nil {
		return nil, err
	}
	return d3dsm.Compile(sh)
}

// BenchmarkPostProcessComposite rasterizes LEGO Star Wars' full-screen post-process composite
// (ps_2_0 at 0x8e2ebc: two bilinear texture reads, three ALU instructions) over a 1280x720 target.
func BenchmarkPostProcessComposite(b *testing.B) {
	ps := loadGameShader(b, 0x008e2ebc)
	r := rand.New(rand.NewSource(1))
	tex := func(w, h int) texSnap {
		px := make([]uint32, w*h)
		for i := range px {
			px[i] = r.Uint32()
		}
		return texSnap{Pixels: px, Width: uint32(w), Height: uint32(h)}
	}
	d := &progDraw{ps: ps, psC: make([]d3dsm.Vec, max(ps.MaxConst, 1)), varyUsed: uint16(ps.ReadsV&3) | uint16(ps.ReadsT&0xFF)<<varyT0}
	ps.ApplyDefs(d.psC)
	d.out.colorWrite = 0xF
	d.out.tex[0], d.out.tex[3] = tex(ScreenWidth, ScreenHeight), tex(512, 256)
	for st := range d.out.linear {
		d.out.linear[st], d.out.clampU[st], d.out.clampV[st] = true, true, true
	}
	corner := func(x, y float32) progVertex {
		v := progVertex{sx: x * ScreenWidth, sy: y * ScreenHeight, inW: 1, valid: true}
		v.vary[varyT0+0] = d3dsm.Vec{x, y, 0, 1}
		v.vary[varyT0+3] = d3dsm.Vec{x, y, 0, 1}
		v.vary[varyT0+2] = d3dsm.Vec{0.2, 0.7, 0, 1}
		return v
	}
	a, bb, c, e := corner(0, 0), corner(1, 0), corner(0, 1), corner(1, 1)
	tris := []queuedTri{
		{prog: &progTri{d: d, v: [3]progVertex{a, bb, c}}, minX: 0, maxX: ScreenWidth - 1, minY: 0, maxY: ScreenHeight - 1},
		{prog: &progTri{d: d, v: [3]progVertex{bb, e, c}}, minX: 0, maxX: ScreenWidth - 1, minY: 0, maxY: ScreenHeight - 1},
	}
	s := &Subsystem{ZBuffer: make([]float32, ScreenWidth*ScreenHeight)}
	pix := make([]byte, ScreenWidth*ScreenHeight*4)
	sh := new(progShader)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for k := range tris {
			s.rasterizeProg(&tris[k], sh, nil, pix, 0, ScreenHeight-1)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*ScreenWidth*ScreenHeight), "ns/pixel")
}

func BenchmarkCompositeShaderRun(b *testing.B) {
	ps := loadGameShader(b, 0x008e2ebc)
	m := &d3dsm.Machine{C: make([]d3dsm.Vec, max(ps.MaxConst, 1))}
	ps.ApplyDefs(m.C)
	m.Sample = func(s int, uv d3dsm.Vec) d3dsm.Vec { return uv }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ps.Run(m)
	}
}

// BenchmarkFogLayer rasterizes one of the title screen's 16 full-screen fog layers: perspective
// textured (128x128), additive, depth tested, vertex alpha 82.
func BenchmarkFogLayer(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	px := make([]uint32, 128*128)
	for i := range px {
		px[i] = r.Uint32()
	}
	tex := texSnap{Pixels: px, Width: 128, Height: 128}
	vert := func(x, y, z, w, u, v float32) screenVertex {
		return screenVertex{sx: x, sy: y, sz: z, r: 200, g: 210, b: 230, a: 82, u: u, v: v, valid: true, invW: 1 / w}
	}
	a, bb := vert(-200, 0, 0.9, 8, 0, 0), vert(1480, 0, 0.9, 8, 4, 0)
	c, e := vert(-600, 720, 0.5, 1, 0, 3), vert(1880, 720, 0.5, 1, 4, 3)
	st := triState{tex0: tex, alpha: true, additive: true}
	var ops []queuedTri
	emitScreenTriangle(&ops, a, bb, c, &st)
	emitScreenTriangle(&ops, bb, e, c, &st)
	s := &Subsystem{ZBuffer: make([]float32, ScreenWidth*ScreenHeight)}
	for i := range s.ZBuffer {
		s.ZBuffer[i] = 1
	}
	pix := make([]byte, ScreenWidth*ScreenHeight*4)
	list := []int32{0, 1}
	sh := new(progShader)
	cpu0 := cpuTime()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.rasterizeBand(ops, list, nil, pix, 0, ScreenHeight-1, sh)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*ScreenWidth*ScreenHeight), "ns/pixel")
	b.ReportMetric(float64(cpuTime()-cpu0)/float64(b.N*ScreenWidth*ScreenHeight), "cpu-ns/pixel")
}

// cpuTime is the process's CPU time in nanoseconds (wall time is unreliable on throttled VMs).
func cpuTime() int64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(0, &ru)
	return ru.Utime.Nano() + ru.Stime.Nano()
}

// TestWideMatchesScalar runs every pixel shader embedded in the game (the bytecode block around
// 0x8e2000) through both interpreters on random inputs: RunWide must give Run's results.
func TestWideMatchesScalar(t *testing.T) {
	mem := gameMemory(t)
	r := rand.New(rand.NewSource(1))
	tested := 0
	for addr := uint32(0x008e0000); addr < 0x008f0000; addr += 4 {
		v := binary.LittleEndian.Uint32(mem[addr:])
		if v != 0xFFFF0101 && v != 0xFFFF0104 && v != 0xFFFF0200 {
			continue
		}
		ps, err := decodeShaderAt(mem, addr)
		if err != nil || !ps.Wide() { // not a shader (data that looks like a version token)
			continue
		}
		tested++
		c := make([]d3dsm.Vec, max(ps.MaxConst, 1))
		for i := range c {
			c[i] = d3dsm.Vec{r.Float32()*2 - 1, r.Float32()*2 - 1, r.Float32()*2 - 1, r.Float32()*2 - 1}
		}
		ps.ApplyDefs(c)
		sample := func(s int, uv d3dsm.Vec) d3dsm.Vec {
			return d3dsm.Vec{uv[0] * 0.5, uv[1]*0.25 + float32(s)*0.1, uv[0] * uv[1], 0.75}
		}
		wm := &d3dsm.WideMachine{C: c, Sample: sample, N: d3dsm.Lanes}
		var in [d3dsm.Lanes][16]d3dsm.Vec
		for l := range in {
			for k := range in[l] {
				in[l][k] = d3dsm.Vec{r.Float32(), r.Float32(), r.Float32(), r.Float32()}
				reg := d3dsm.FileV + k
				if k >= 2 {
					reg = d3dsm.FileT + k - 2
				}
				if k < 10 {
					for j := 0; j < 4; j++ {
						wm.F[reg][j][l] = in[l][k][j]
						if ps.InputComps(reg)&(1<<j) == 0 {
							wm.F[reg][j][l] = float32(math.NaN()) // unread: must not matter
						}
					}
				}
			}
		}
		ps.RunWide(wm)
		for l := 0; l < d3dsm.Lanes; l++ {
			m := &d3dsm.Machine{C: c, Sample: sample}
			m.F[d3dsm.FileV], m.F[d3dsm.FileV+1] = in[l][0], in[l][1]
			for k := 0; k < 8; k++ {
				m.F[d3dsm.FileT+k] = in[l][k+2]
			}
			ps.Run(m)
			want := m.F[d3dsm.FileOC]
			got := d3dsm.Vec{wm.F[d3dsm.FileOC][0][l], wm.F[d3dsm.FileOC][1][l], wm.F[d3dsm.FileOC][2][l], wm.F[d3dsm.FileOC][3][l]}
			for j := 0; j < 4; j++ {
				if d := got[j] - want[j]; d > 1e-5 || d < -1e-5 || m.Kill != wm.Kill[l] {
					t.Fatalf("shader %#x lane %d: wide %v kill %v, scalar %v kill %v", addr, l, got, wm.Kill[l], want, m.Kill)
				}
			}
		}
	}
	if tested == 0 {
		t.Fatal("no pixel shaders found")
	}
	t.Logf("%d pixel shaders agree", tested)
}

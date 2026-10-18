package lswsaga

// The hand-lifted functions against the game's own code: each case runs the original guest
// function in the interpreter and the lifted Go version on the same random inputs, and compares
// the return value, the FPU stack, callee-saved registers and the memory they write. Needs the
// game executable (C32_BIN_PATH); skipped without it.

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"testing"

	"catalina32/pkg/cpu"
	"catalina32/pkg/macho32"
)

const (
	liftData  = 0x1E800000 // test inputs and outputs (above the guest heap)
	liftStack = 0x1FF00000
)

func loadGame(t *testing.T) *macho32.Image {
	path := os.Getenv("C32_BIN_PATH")
	if path == "" {
		t.Skip("C32_BIN_PATH not set (the game executable is needed)")
	}
	img, err := macho32.LoadImage(path)
	if err != nil {
		t.Skip("cannot load the game executable:", err)
	}
	return img
}

func newGuest(t *testing.T, img *macho32.Image) *cpu.CPU {
	c := cpu.NewCPU()
	if err := img.MapIntoArena(c.Mem); err != nil {
		t.Fatal(err)
	}
	c.Regs[cpu.RegESP] = liftStack
	return c
}

// liftCase prepares inputs in guest memory (the same on both CPUs) and returns the arguments.
type liftCase struct {
	name  string
	addr  uint32
	fn    cpu.ThunkFunc
	setup func(r *rand.Rand, c *cpu.CPU) []uint32
	fpu   bool // returns a value on the x87 stack
}

func randString(r *rand.Rand, maxLen int) string {
	const alpha = "abcXYZ_09.-/\\ AbC"
	b := make([]byte, r.Intn(maxLen+1))
	for i := range b {
		b[i] = alpha[r.Intn(len(alpha))]
	}
	return string(b)
}

// similar returns s with random case changes and, sometimes, a changed tail.
func similar(r *rand.Rand, s string) string {
	b := []byte(s)
	for i := range b {
		if r.Intn(3) == 0 {
			b[i] ^= 0x20 * byte(r.Intn(2))
		}
	}
	if r.Intn(3) == 0 {
		return string(b) + randString(r, 4)
	}
	if r.Intn(4) == 0 && len(b) > 0 {
		return string(b[:r.Intn(len(b))])
	}
	return string(b)
}

func putFloats(r *rand.Rand, c *cpu.CPU, addr uint32, n int) {
	for i := 0; i < n; i++ {
		c.WriteF32(addr+uint32(4*i), float32(r.NormFloat64()*10))
	}
}

func liftCases() []liftCase {
	twoStrings := func(r *rand.Rand, c *cpu.CPU) (uint32, uint32) {
		a := randString(r, 12)
		b := similar(r, a)
		if r.Intn(4) == 0 {
			b = randString(r, 12)
		}
		c.WriteCString(liftData, a)
		c.WriteCString(liftData+0x100, b)
		return liftData, liftData + 0x100
	}
	return []liftCase{
		{"NuStrLen", 0x00270500, liftedNuStrLen, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			c.WriteCString(liftData, randString(r, 40))
			return []uint32{liftData}
		}, false},
		{"NuStrICmp", 0x002738a0, liftedNuStrICmp, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			a, b := twoStrings(r, c)
			return []uint32{a, b}
		}, false},
		{"NuStrNICmp", 0x0026ee60, liftedNuStrNICmp, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			a, b := twoStrings(r, c)
			n := []uint32{0, 1, 3, 8, 20, 0xFFFFFFFF, 0xFFFFFFFE}[r.Intn(7)]
			return []uint32{a, b, n}
		}, false},
		{"NuStrNCmp", 0x00277e70, liftedNuStrNCmp, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			a, b := twoStrings(r, c)
			return []uint32{a, b, uint32(r.Intn(16))}
		}, false},
		{"NuStrCpy", 0x0026eca0, liftedNuStrCpy, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			c.WriteCString(liftData+0x100, randString(r, 40))
			return []uint32{liftData, liftData + 0x100}
		}, false},
		{"NuMtxMul", 0x001052a0, liftedNuMtxMul, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			putFloats(r, c, liftData+0x100, 16)
			putFloats(r, c, liftData+0x200, 16)
			return []uint32{liftData, liftData + 0x100, liftData + 0x200}
		}, false},
		{"NuVecMtxTransform", 0x00102c40, liftedNuVecMtxTransform, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			putFloats(r, c, liftData+0x100, 3)
			putFloats(r, c, liftData+0x200, 16)
			return []uint32{liftData, liftData + 0x100, liftData + 0x200}
		}, false},
		{"NuVecMtxRotate", 0x00102dd0, liftedNuVecMtxRotate, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			putFloats(r, c, liftData+0x100, 3)
			putFloats(r, c, liftData+0x200, 16)
			return []uint32{liftData, liftData + 0x100, liftData + 0x200}
		}, false},
		{"NuSqrt", 0x0016ff70, liftedNuSqrt, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			x := float32(r.NormFloat64() * 100)
			if r.Intn(5) == 0 {
				x = 0
			}
			return []uint32{math.Float32bits(x)}
		}, true},
		{"NuStrSliceEq", 0x0020f130, liftedNuStrSliceEq, func(r *rand.Rand, c *cpu.CPU) []uint32 {
			a, b := twoStrings(r, c)
			n := uint32(len(c.ReadCString(a)))
			if r.Intn(3) == 0 && n > 0 {
				n = uint32(r.Intn(int(n)))
			}
			c.Write32(liftData+0x200, a)
			c.Write32(liftData+0x204, n)
			return []uint32{liftData + 0x200, b}
		}, false},
	}
}

func liftState(c *cpu.CPU, ret uint32, fpu bool) string {
	s := fmt.Sprintf("eax=%#x ebx=%#x esi=%#x edi=%#x ebp=%#x esp=%#x halted=%v", ret, c.Regs[cpu.RegEBX], c.Regs[cpu.RegESI],
		c.Regs[cpu.RegEDI], c.Regs[cpu.RegEBP], c.Regs[cpu.RegESP], c.Halted)
	if fpu {
		s += fmt.Sprintf(" st0=%#x top=%d", math.Float64bits(c.FPUGet(0)), c.FPUTop)
	}
	return s
}

func TestLiftedMatchGuest(t *testing.T) {
	img := loadGame(t)
	orig, lifted := newGuest(t, img), newGuest(t, img)
	for _, lc := range liftCases() {
		lifted.RegisterIntercept(lc.addr, "lifted::"+lc.name, lc.fn)
	}
	for _, lc := range liftCases() {
		t.Run(lc.name, func(t *testing.T) {
			if _, ok := orig.Intercepts[lc.addr]; ok {
				t.Fatalf("0x%08x is intercepted in the reference CPU", lc.addr)
			}
			r1, r2 := rand.New(rand.NewSource(1)), rand.New(rand.NewSource(1))
			for i := 0; i < 2000; i++ {
				var states [2]string
				var mem [2][]byte
				for k, c := range []*cpu.CPU{orig, lifted} {
					r := r1
					if k == 1 {
						r = r2
					}
					clear(c.Mem[liftData : liftData+0x400])
					args := lc.setup(r, c)
					c.Regs[cpu.RegEBX], c.Regs[cpu.RegESI], c.Regs[cpu.RegEDI], c.Regs[cpu.RegEBP] = 0x1111, 0x2222, 0x3333, 0x4444
					c.Regs[cpu.RegEAX], c.Regs[cpu.RegECX], c.Regs[cpu.RegEDX] = 0x5555, 0x6666, 0x7777
					c.FPUTop = 0
					c.Halted = false
					ret, err := c.CallGuest(lc.addr, args...)
					if err != nil {
						t.Fatalf("call %d: %v", i, err)
					}
					states[k] = liftState(c, ret, lc.fpu)
					mem[k] = append([]byte(nil), c.Mem[liftData:liftData+0x400]...)
				}
				if states[0] != states[1] || !bytes.Equal(mem[0], mem[1]) {
					t.Fatalf("input %d differs:\n  guest  %s\n  lifted %s\n  memory equal: %v",
						i, states[0], states[1], bytes.Equal(mem[0], mem[1]))
				}
			}
		})
	}
}

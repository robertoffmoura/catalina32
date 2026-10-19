package crt

// The native NuLZHUF_Decompress against the game's own: both decode the same inputs (real
// compressed blocks captured with C32_DUMP_LZHUF=<dir> and given as LZHUF_TEST_DIR, the same
// blocks damaged, and random bytes) and must leave the same return value, registers, output
// and decoder state. Needs a game executable that has the decoder (C32_BIN_PATH). By default it
// takes every 100th block and damages the first 100 (under a minute); LZHUF_TEST_ALL=1 takes
// all of them (a session's 3,724 blocks: about 15 minutes, nearly all of it the emulated decoder).

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"catalina32/pkg/cpu"
	"catalina32/pkg/lh5"
	"catalina32/pkg/macho32"
	"catalina32/pkg/profile"
)

const lzhufStack = 0x1FF00000

// newLZHUFGuest maps the game into a fresh CPU with the C runtime shims; native picks whether
// the decoder is replaced.
func newLZHUFGuest(t *testing.T, native bool) (*cpu.CPU, lzhufGlobals) {
	path := os.Getenv("C32_BIN_PATH")
	if path == "" {
		t.Skip("C32_BIN_PATH not set (the game executable is needed)")
	}
	img, err := macho32.LoadImage(path)
	if err != nil {
		t.Skip("cannot load the game executable:", err)
	}
	c := cpu.NewCPU()
	if err := img.MapIntoArena(c.Mem); err != nil {
		t.Fatal(err)
	}
	c.Regs[cpu.RegESP] = lzhufStack
	prof := &profile.Profile{}
	prof.Attach(img, c.Mem)
	g, ok := findLZHUF(c, prof)
	if !ok {
		t.Skip("this executable has no NuEngine LZHUF decoder")
	}
	RegisterCRT(c, img, prof)
	if !native {
		delete(c.Intercepts, g.fn)
	}
	return c, g
}

// The addresses read out of LEGO Star Wars: The Complete Saga's code must be the ones found by
// hand when the decoder was first replaced.
func TestLZHUFGlobalsLSWSaga(t *testing.T) {
	_, g := newLZHUFGuest(t, true)
	if g.fn != 0x002734f0 {
		t.Skip("not LEGO Star Wars: The Complete Saga")
	}
	want := lzhufGlobals{fn: 0x002734f0, inPtr: 0x02b940cc, outPtr: 0x02b940d0, bitBuf: 0x02b940d4,
		outLeft: 0x02b940d8, inLeft: 0x02b940dc, left: 0x02b940e0, right: 0x02b948d6, bitCount: 0x02cccf90,
		subBitBuf: 0x02cccf94, disabled: 0x02cccf9c, text: 0x02cccf9d, blockSize: 0x02ccefa0,
		cTable: 0x02ccefa4, cLen: 0x02cd0fa4, ptTable: 0x02cd11a2, ptLen: 0x02cd13a2, j: 0x02cd23f4, i: 0x02cd23f8}
	if g != want {
		t.Fatalf("globals read from the code:\n  got  %+v\n  want %+v", g, want)
	}
}

type lzhufInput struct {
	name    string
	src     []byte
	dstSize uint32
}

func lzhufInputs(t *testing.T) []lzhufInput {
	var in []lzhufInput
	all := os.Getenv("LZHUF_TEST_ALL") == "1"
	if dir := os.Getenv("LZHUF_TEST_DIR"); dir != "" {
		names, _ := filepath.Glob(filepath.Join(dir, "*.lh5"))
		sort.Strings(names)
		for k, name := range names {
			if !all && k%100 != 0 {
				continue
			}
			_, size, _ := strings.Cut(strings.TrimSuffix(filepath.Base(name), ".lh5"), "_")
			n, err := strconv.ParseUint(size, 10, 32)
			b, err2 := os.ReadFile(name)
			if err != nil || err2 != nil {
				t.Fatalf("%s: not a C32_DUMP_LZHUF file", name)
			}
			in = append(in, lzhufInput{filepath.Base(name), b, uint32(n)})
		}
	}
	if len(in) == 0 {
		t.Log("LZHUF_TEST_DIR not set or empty: random input only")
	}
	r := rand.New(rand.NewSource(1))
	damaged := len(in)
	if !all {
		damaged = min(damaged, 100)
	}
	for i := 0; i < damaged; i++ {
		b := append([]byte(nil), in[i].src...)
		for k := 0; k < 1+r.Intn(4); k++ {
			b[r.Intn(len(b))] ^= byte(1 << r.Intn(8))
		}
		in = append(in, lzhufInput{in[i].name + " damaged", b, in[i].dstSize})
	}
	for i := 0; i < 200; i++ {
		b := make([]byte, r.Intn(2000))
		r.Read(b)
		in = append(in, lzhufInput{"random " + strconv.Itoa(i), b, uint32(r.Intn(20000))})
	}
	return in
}

func TestLZHUFMatchesGuest(t *testing.T) {
	orig, g := newLZHUFGuest(t, false)
	native, _ := newLZHUFGuest(t, true)
	l := native.Intercepts[g.fn]
	if l == nil {
		t.Fatal("the native decoder is not installed (C32_NO_NATIVE_LZHUF set?)")
	}

	const src, dst = 0x1E000000, 0x1E400000
	state := func(c *cpu.CPU) []byte { // the decoder's globals (two areas in this game)
		return append(append([]byte(nil), c.Mem[g.inPtr:g.right+2*(2*lh5.NC-1)]...), c.Mem[g.bitCount:g.i+4]...)
	}
	var spent [2]time.Duration
	hung := 0
	for _, in := range lzhufInputs(t) {
		if uint64(dst)+uint64(in.dstSize) > lzhufStack-0x100000 {
			continue
		}
		var states [2]string
		var out, glob [2][]byte
		for k, c := range []*cpu.CPU{orig, native} {
			copy(c.Mem[src:], in.src)
			clear(c.Mem[dst : dst+in.dstSize])
			// On corrupt input the original reads stack memory it never wrote (make_table
			// indexes its local arrays out of bounds): give both CPUs the same leftovers.
			clear(c.Mem[lzhufStack-0x10000 : lzhufStack])
			c.Regs[cpu.RegEBX], c.Regs[cpu.RegESI], c.Regs[cpu.RegEDI], c.Regs[cpu.RegEBP] = 0x1111, 0x2222, 0x3333, 0x4444
			c.Halted = false
			c.MaxSteps = c.Steps + 100_000_000 // damaged input can loop forever in the original
			start := time.Now()
			ret, err := c.CallGuest(g.fn, src, dst, uint32(len(in.src)), in.dstSize)
			if !strings.Contains(in.name, " ") { // a real block, not damaged or random
				spent[k] += time.Since(start)
			}
			if err != nil && !c.Halted {
				t.Fatalf("%s: %v", in.name, err)
			}
			states[k] = fmt.Sprintf("eax=%#x ebx=%#x esi=%#x edi=%#x ebp=%#x esp=%#x halted=%v", ret,
				c.Regs[cpu.RegEBX], c.Regs[cpu.RegESI], c.Regs[cpu.RegEDI], c.Regs[cpu.RegEBP], c.Regs[cpu.RegESP], c.Halted)
			out[k] = append([]byte(nil), c.Mem[dst:dst+in.dstSize]...)
			glob[k] = state(c)
		}
		if orig.Halted && native.Halted {
			// The original loops forever on this input (the native decoder declined it too):
			// nothing to compare. Both stopped at the step limit, at different points; carry on
			// from the reference CPU's memory.
			hung++
			copy(native.Mem[g.inPtr:g.i+4], orig.Mem[g.inPtr:g.i+4])
			continue
		}
		if !bytes.Equal(out[1], out[0]) || states[0] != states[1] || !bytes.Equal(glob[0], glob[1]) {
			t.Fatalf("%s differs:\n  guest  %s\n  native %s\n  output equal: %v, decoder state equal: %v",
				in.name, states[0], states[1], bytes.Equal(out[0], out[1]), bytes.Equal(glob[0], glob[1]))
		}
	}
	t.Logf("%d inputs on which the original never returns; the real blocks took %v with the game's decoder (emulated), %v natively",
		hung, spent[0], spent[1])
}

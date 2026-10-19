package lswsaga

// The native NuLZHUF_Decompress against the game's own: both decode the same inputs (real
// compressed blocks captured with C32_DUMP_LZHUF=<dir> and given as LZHUF_TEST_DIR, the same
// blocks damaged, and random bytes) and must leave the same return value, registers, output
// and decoder state. Needs the game executable (C32_BIN_PATH). By default it takes every 50th
// block and damages the first 100; LZHUF_TEST_ALL=1 takes all of them (a
// session's worth, 3,724 blocks, is about 15 minutes, nearly all of it the emulated decoder).

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
	"catalina32/pkg/crt"
)

type lzhufInput struct {
	name    string
	src     []byte
	dstSize uint32
}

func lzhufInputs(t *testing.T) []lzhufInput {
	var in []lzhufInput
	if dir := os.Getenv("LZHUF_TEST_DIR"); dir != "" {
		names, _ := filepath.Glob(filepath.Join(dir, "*.lh5"))
		sort.Strings(names)
		all := os.Getenv("LZHUF_TEST_ALL") == "1"
		for k, name := range names {
			if !all && k%50 != 0 {
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
		t.Log("LZHUF_TEST_DIR not set or empty: random and damaged inputs only")
	}
	r := rand.New(rand.NewSource(1))
	damaged := len(in)
	if os.Getenv("LZHUF_TEST_ALL") != "1" {
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
	img := loadGame(t)
	orig, native := newGuest(t, img), newGuest(t, img)
	for _, c := range []*cpu.CPU{orig, native} {
		Profile.Attach(img, c.Mem)
		crt.RegisterCRT(c, img, Profile)
	}
	// Only the native decoder: input it declines runs the same guest code on both CPUs. (The
	// lifted DecodeC/DecodeP the game falls back to can loop forever on corrupt input, inside
	// one step, where the step limit cannot stop them.)
	native.RegisterIntercept(lzhufDecompress, "native::NuLZHUF_Decompress", nativeNuLZHUFDecompress)

	const src, dst = 0x1E000000, 0x1E400000
	state := func(c *cpu.CPU) []byte { // the decoder's globals
		return append(append([]byte(nil), c.Mem[lzhufInPtr:lzhufRight+2*1019]...), c.Mem[lzhufBitCount:lzhufI+4]...)
	}
	var spent [2]time.Duration
	nativeBefore, hung := lzhufNative, 0
	for _, in := range lzhufInputs(t) {
		if uint64(dst)+uint64(in.dstSize) > liftStack-0x100000 {
			continue
		}
		var states [2]string
		var out, glob [2][]byte
		for k, c := range []*cpu.CPU{orig, native} {
			copy(c.Mem[src:], in.src)
			clear(c.Mem[dst : dst+in.dstSize])
			// On corrupt input the original reads stack memory it never wrote (make_table
			// indexes its local arrays out of bounds): give both CPUs the same leftovers.
			clear(c.Mem[liftStack-0x10000 : liftStack])
			c.Regs[cpu.RegEBX], c.Regs[cpu.RegESI], c.Regs[cpu.RegEDI], c.Regs[cpu.RegEBP] = 0x1111, 0x2222, 0x3333, 0x4444
			c.Halted = false
			c.MaxSteps = c.Steps + 100_000_000 // damaged input can loop forever in the original
			start := time.Now()
			ret, err := c.CallGuest(lzhufDecompress, src, dst, uint32(len(in.src)), in.dstSize)
			if !strings.Contains(in.name, " ") { // a real block, not damaged or random
				spent[k] += time.Since(start)
			}
			if err != nil && !c.Halted {
				t.Fatalf("%s: %v", in.name, err)
			}
			states[k] = liftState(c, ret, false)
			out[k] = append([]byte(nil), c.Mem[dst:dst+in.dstSize]...)
			glob[k] = state(c)
		}
		if orig.Halted && native.Halted {
			// The original loops forever on this input (the native decoder declined it too):
			// nothing to compare. Both stopped at the step limit, at different points; carry on
			// from the reference CPU's memory.
			hung++
			copy(native.Mem[0x02b00000:0x02d00000], orig.Mem[0x02b00000:0x02d00000])
			continue
		}
		if states[0] != states[1] || !bytes.Equal(out[0], out[1]) || !bytes.Equal(glob[0], glob[1]) {
			t.Fatalf("%s differs:\n  guest  %s\n  native %s\n  output equal: %v, decoder state: %s",
				in.name, states[0], states[1], bytes.Equal(out[0], out[1]), firstDiff(glob[0], glob[1]))
		}
	}
	t.Logf("%d calls decoded natively, %d inputs on which the original never returns; the real blocks took %v with the game's decoder (emulated), %v natively",
		lzhufNative-nativeBefore, hung, spent[0], spent[1])
}

// firstDiff names the first decoder-state byte that differs (see state in TestLZHUFMatchesGuest).
func firstDiff(a, b []byte) string {
	for i := range a {
		if a[i] != b[i] {
			addr := uint32(lzhufInPtr + i)
			if n := uint32(lzhufRight + 2*1019 - lzhufInPtr); uint32(i) >= n {
				addr = lzhufBitCount + uint32(i) - n
			}
			n := 0
			for j := i; j < len(a); j++ {
				if a[j] != b[j] {
					n++
				}
			}
			return fmt.Sprintf("first difference at 0x%08x: guest %#02x, native %#02x (%d bytes differ)", addr, a[i], b[i], n)
		}
	}
	return "equal"
}

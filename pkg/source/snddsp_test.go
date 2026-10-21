package source

// The native sound effects against Portal's own code: engine.dylib's code is placed in a bare CPU,
// random delay lines, filters, reverbs and diffusors are built in its memory, and every function
// runs both ways from the same memory; the return value and the whole test heap must come out
// the same. Needs the game (C32_SOURCE_DIR).

import (
	"bytes"
	"debug/macho"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"catalina32/pkg/cpu"
)

const (
	dspTestBase  = 0x10000000 // engine.dylib's code, at its own addresses above this
	dspTestHeap  = 0x30000000
	dspTestSize  = 0x100000
	dspTestStack = 0x1FF00000
	dspTestRet   = 0x2FFF0000 // where the native calls "return" to
)

type dspTestGuest struct {
	c     *cpu.CPU
	d     *sndDSP
	funcs map[string]*sndDSPFunc
	next  uint32
	r     *rand.Rand
}

func newDSPTestGuest(t *testing.T) *dspTestGuest {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set (Portal is needed)")
	}
	f, err := macho.Open(filepath.Join(dir, "bin", "engine.dylib"))
	if err != nil {
		t.Skip("no engine.dylib:", err)
	}
	defer f.Close()
	text := f.Section("__text")
	code, err := text.Data()
	if err != nil {
		t.Fatal(err)
	}
	g := &dspTestGuest{c: cpu.NewCPU(), funcs: map[string]*sndDSPFunc{}, r: rand.New(rand.NewSource(1))}
	copy(g.c.Mem[dspTestBase+uint32(text.Addr):], code)
	g.d = &sndDSP{m: &Machine{}, checked: map[string]int{}, bad: map[string]int{}, declined: map[string]int{}}
	for i := range sndDSPFuncs {
		fn := sndDSPFuncs[i]
		for _, s := range f.Symtab.Syms {
			if s.Name == fn.name {
				fn.addr = dspTestBase + uint32(s.Value)
			}
		}
		if fn.addr == 0 {
			t.Fatalf("engine.dylib has no %s", fn.name)
		}
		g.funcs[fn.name[5:strings.Index(fn.name, "P")]] = &fn // "DLY_GetNext", "DLY_GetNextN", ...
	}
	return g
}

func (g *dspTestGuest) alloc(n uint32) uint32 {
	a := dspTestHeap + g.next
	g.next += (n + 15) &^ 15
	return a
}

// q12 picks a gain: usually a sensible one, sometimes anything (the products must wrap the same).
func (g *dspTestGuest) q12() uint32 {
	if g.r.Intn(8) == 0 {
		return g.r.Uint32()
	}
	return uint32(int32(g.r.Intn(8192) - 4096))
}

func (g *dspTestGuest) sample() uint32 {
	switch g.r.Intn(6) {
	case 0:
		return 0
	case 1:
		return g.r.Uint32()
	}
	return uint32(int32(g.r.Intn(65536) - 32768))
}

func (g *dspTestGuest) flt(chain bool) uint32 {
	c := g.c
	a := g.alloc(fltSize)
	zero := g.r.Intn(3) == 0 // state at rest: the shortcut that leaves it alone
	for _, off := range []uint32{fltB0, fltB1, fltA1} {
		c.Write32(a+off, g.q12())
	}
	if !zero {
		c.Write32(a+fltW0, g.sample())
		c.Write32(a+fltW1, g.sample())
	}
	if chain {
		n := g.r.Intn(5)
		c.Write32(a+fltCount, uint32(n))
		for k := 0; k < 3; k++ {
			c.Write32(a+fltNext+uint32(4*k), g.flt(false))
		}
	}
	return a
}

func (g *dspTestGuest) dly(typ int) uint32 {
	c := g.c
	a := g.alloc(dlySize)
	d := uint32(g.r.Intn(40))
	if g.r.Intn(10) == 0 {
		d = 0
	}
	w := g.alloc((d + 1) * 4)
	for k := uint32(0); k <= d; k++ {
		c.Write32(w+4*k, g.sample())
	}
	c.Write32(a+dlyType, uint32(typ))
	c.Write32(a+dlyD, d)
	c.Write32(a+dlyT, uint32(g.r.Intn(int(d)+1)))
	for k := uint32(0); k < 3; k++ {
		c.Write32(a+dlyT1+4*k, uint32(g.r.Intn(int(d)+1)))
	}
	c.Write32(a+dlyW, w)
	c.Write32(a+dlyP, w+4*uint32(g.r.Intn(int(d)+1)))
	c.Write32(a+dlyA, g.q12())
	c.Write32(a+dlyB, g.q12())
	c.Write32(a+dlyFlt, g.flt(false))
	return a
}

func (g *dspTestGuest) samples(n int) uint32 {
	a := g.alloc(uint32(n) * 8)
	for k := 0; k < 2*n; k++ {
		g.c.Write32(a+uint32(4*k), g.sample())
	}
	return a
}

// both runs fn from the same memory as the guest and natively, and compares.
func (g *dspTestGuest) both(t *testing.T, fn *sndDSPFunc, args ...uint32) {
	t.Helper()
	c := g.c
	heap := c.Mem[dspTestHeap : dspTestHeap+dspTestSize]
	before := append([]byte(nil), heap...)
	c.Regs[cpu.RegESP] = dspTestStack
	want, err := c.CallGuestNested(fn.addr, args...)
	if err != nil {
		t.Fatalf("%s: the guest code: %v", fn.name, err)
	}
	guest := append([]byte(nil), heap...)
	copy(heap, before)

	esp := uint32(dspTestStack - 0x100)
	for i := len(args) - 1; i >= 0; i-- {
		esp -= 4
		c.Write32(esp, args[i])
	}
	esp -= 4
	c.Write32(esp, dspTestRet)
	c.Regs[cpu.RegESP] = esp
	c.Regs[cpu.RegEAX] = 0xdeadbeef
	declined := g.d.declined[fn.name]
	g.d.call(c, fn)
	if g.d.declined[fn.name] != declined {
		t.Fatalf("%s(%#x): left to the guest", fn.name, args)
	}
	if c.EIP != dspTestRet || c.Regs[cpu.RegESP] != esp+4 {
		t.Fatalf("%s: returned to %#x with ESP %#x", fn.name, c.EIP, c.Regs[cpu.RegESP])
	}
	if fn.value && c.Regs[cpu.RegEAX] != want {
		t.Fatalf("%s(%#x): returned %#x, the guest %#x", fn.name, args, c.Regs[cpu.RegEAX], want)
	}
	if !bytes.Equal(heap, guest) {
		for k := range heap {
			if heap[k] != guest[k] {
				t.Fatalf("%s(%#x): memory differs first at %#x: %#02x, the guest %#02x", fn.name, args, dspTestHeap+k, heap[k], guest[k])
			}
		}
	}
}

func TestSndDSPAgainstEngine(t *testing.T) {
	g := newDSPTestGuest(t)
	c := g.c
	for round := 0; round < 3000; round++ {
		g.next = 0
		clear(c.Mem[dspTestHeap : dspTestHeap+dspTestSize])
		typ := g.r.Intn(9) // 0, 7, 8: the plain feedback delay
		n := g.r.Intn(70)
		op := uint32(g.r.Intn(4))
		switch round % 6 {
		case 0:
			g.both(t, g.funcs["DLY_GetNext"], g.dly(typ), g.sample())
		case 1:
			g.both(t, g.funcs["DLY_GetNextN"], g.dly(typ), g.samples(n), uint32(n), op)
		case 2:
			g.both(t, g.funcs["FLT_GetNext"], g.flt(true), g.sample())
		case 3:
			g.both(t, g.funcs["FLT_GetNextN"], g.flt(true), g.samples(n), uint32(n), op)
		case 4:
			rva := g.alloc(rvaSize)
			m := g.r.Intn(13)
			c.Write32(rva+rvaM, uint32(m))
			for k := 0; k < m; k++ {
				c.Write32(rva+rvaDly+uint32(4*k), g.dly(g.r.Intn(9)))
			}
			c.Write32(rva+rvaPar, uint32(g.r.Intn(2)))
			if g.r.Intn(4) != 0 {
				c.Write32(rva+rvaFlt, g.flt(true))
			}
			g.both(t, g.funcs["RVA_GetNextN"], rva, g.samples(n), uint32(n), op)
		case 5:
			k := g.r.Intn(9)
			dfr := g.alloc(dfrDly + uint32(4*k))
			c.Write32(dfr+dfrN, uint32(k))
			for i := 0; i < k; i++ {
				c.Write32(dfr+dfrDly+uint32(4*i), g.dly(g.r.Intn(9)))
			}
			g.both(t, g.funcs["DFR_GetNextN"], dfr, g.samples(n), uint32(n), op)
		}
	}
}

package source

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"catalina32/pkg/cpu"
	"catalina32/pkg/dyld"
)

// Source's sound effects (snd_dsp.cpp) in Go.
//
// The engine runs every mixed sample through the room's DSP preset: chains of delay lines,
// reverbs (RVA: up to 12 delay lines in parallel and a low-pass), diffusors (DFR: all-pass delay
// lines in series) and filters, all in 32-bit fixed point. The guest code calls DLY_GetNext once
// per sample for every delay line, so in a large room the reverb was about a fifth of all the
// instructions the game ran (Portal's escape_02). The functions below do the same arithmetic
// natively, a buffer at a time.
//
// They are bound by symbol and only when the function's bytes are the ones they were written
// from (sndDSPFuncs: a build compiled differently keeps the original code). Each native function
// reads the guest's structures, checks them (pointers inside the arena, delay positions inside
// their buffers, no two structures or buffers overlapping) and otherwise leaves the call to the
// guest code; it then runs with the structures' fields in Go variables and writes the results
// back. C32_DSPCHECK=1 runs the guest code too on every call and compares everything both wrote
// (see verify); C32_NATIVE_DSP=0 turns the native versions off.

// sndDSPFunc is a native function: the symbol, size and SHA-256 prefix of the code it was written
// from (Portal's engine.dylib of Nov 2024), its argument count and whether it returns a value.
type sndDSPFunc struct {
	name  string
	size  uint32
	hash  string
	args  int
	value bool
	load  func(d *sndDSP, c *cpu.CPU) func() uint32
	addr  uint32
}

var sndDSPFuncs = []sndDSPFunc{
	{"__Z11DLY_GetNextP5dly_ti", 944, "3ff246c150e222c2", 2, true, (*sndDSP).dlyGetNext, 0},
	{"__Z12DLY_GetNextNP5dly_tP21portable_samplepair_tii", 144, "08b5983033a7b781", 4, false, (*sndDSP).dlyGetNextN, 0},
	{"__Z11FLT_GetNextP5flt_ti", 560, "57ede6c036834edf", 2, true, (*sndDSP).fltGetNext, 0},
	{"__Z12FLT_GetNextNP5flt_tP21portable_samplepair_tii", 144, "d0bc66fc13fe141b", 4, false, (*sndDSP).fltGetNextN, 0},
	{"__Z12RVA_GetNextNP5rva_tP21portable_samplepair_tii", 624, "bff1d383fe68ff46", 4, false, (*sndDSP).rvaGetNextN, 0},
	{"__Z12DFR_GetNextNP5dfr_tP21portable_samplepair_tii", 624, "2a651ba0d200e589", 4, false, (*sndDSP).dfrGetNextN, 0},
}

// Structure layouts, from the code.
const (
	dlyType = 0x04 // 1 all-pass, 2 low-pass in the loop, 3 plain, 4 low-pass out, 5 multi-tap + low-pass, 6 multi-tap; else feedback
	dlyD    = 0x08 // length - 1
	dlyT    = 0x0c // tap
	dlyT1   = 0x18 // multi-tap taps
	dlyP    = 0x34 // write position
	dlyW    = 0x38 // buffer
	dlyA    = 0x3c // feedback gain (Q12)
	dlyB    = 0x40 // output gain (Q12)
	dlyFlt  = 0x44 // low-pass (types 2, 4, 5)
	dlySize = 0x48

	fltB0    = 0x04 // one-pole coefficients (Q12)
	fltB1    = 0x08
	fltA1    = 0x3c
	fltW0    = 0x6c // state
	fltW1    = 0x70
	fltCount = 0xa8 // 1-3: this filter and 1-3 more in series (fltNext)
	fltNext  = 0xac
	fltSize  = 0xb8

	rvaM     = 0x04 // number of delay lines
	rvaPar   = 0x08 // parallel: no low-pass after the sum
	rvaFlt   = 0x0c
	rvaDly   = 0x10 // dly_t *[12]
	rvaModDy = 0x70 // byte: modulated delay lines (mdy_t, left to the guest)
	rvaSize  = 0x74

	dfrN   = 0x04
	dfrDly = 0x48 // dly_t *[n]
)

// sampleOp says which channel of a portable_samplepair_t buffer a processor reads and writes.
const (
	opLeft          = 0
	opRight         = 1
	opLeftDuplicate = 2 // left in, both out
)

type sndDSP struct {
	m        *Machine
	addrs    []uint32 // bound functions
	check    bool
	guest    bool // check mode: the guest code is running (every call goes to it)
	checked  map[string]int
	bad      map[string]int
	declined map[string]int

	// per call
	regions []dspRegion
	dlys    []dlyState
	flts    []fltState
}

type dspRegion struct{ lo, hi uint32 }

type dlyState struct {
	addr uint32
	typ  int32
	d    int32
	t    [4]int32
	i    int32 // write position: (p - w) / 4
	w    uint32
	a, b int32
	buf  []byte
	flt  int // index into flts, -1 none
}

type fltState struct {
	addr               uint32
	b0, b1, a1, w0, w1 int32
}

var le = binary.LittleEndian

// bindSndDSP replaces engine.dylib's DSP functions when they are the ones the Go code follows.
func (m *Machine) bindSndDSP(img *dyld.Image) {
	if img.Name() != "engine.dylib" || os.Getenv("C32_NATIVE_DSP") == "0" {
		return
	}
	d := &sndDSP{m: m, check: os.Getenv("C32_DSPCHECK") == "1",
		checked: map[string]int{}, bad: map[string]int{}, declined: map[string]int{}}
	var other []string
	for _, f := range sndDSPFuncs {
		addr, ok := m.localSymbol(img, f.name)
		if !ok {
			continue
		}
		code := m.C.Bytes(addr, f.size)
		h := sha256.Sum256(code)
		if code == nil || hex.EncodeToString(h[:8]) != f.hash {
			other = append(other, f.name)
			continue
		}
		f.addr = addr
		fp := &f
		d.addrs = append(d.addrs, addr)
		m.C.RegisterIntercept(addr, "engine.dylib!"+f.name+" (native)", func(c *cpu.CPU) { d.call(c, fp) })
	}
	if len(d.addrs) > 0 {
		m.logf("[dsp] sound effects in Go: %d functions", len(d.addrs))
		m.sndDSP = d
	}
	if len(other) > 0 {
		m.logf("[dsp] different code, left to the guest: %v", other)
	}
}

// report summarises check mode and declined calls (for tests).
func (d *sndDSP) report() string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("checked %v, mismatched %v, left to the guest %v", d.checked, d.bad, d.declined)
}

// --- loading and checking the guest's structures ---

func (d *sndDSP) reset() {
	d.regions = d.regions[:0]
	d.dlys = d.dlys[:0]
	d.flts = d.flts[:0]
}

func inArena(addr, n uint32) bool {
	return addr >= 0x1000 && uint64(addr)+uint64(n) <= cpu.ArenaSize
}

// region records a structure or buffer the call reads or writes; disjoint checks them all.
func (d *sndDSP) region(addr, n uint32) bool {
	if !inArena(addr, n) {
		return false
	}
	d.regions = append(d.regions, dspRegion{addr, addr + n})
	return true
}

// disjoint says whether no two recorded regions overlap (so fields can be kept in Go variables:
// no write the guest code makes can change what another of its reads sees).
func (d *sndDSP) disjoint() bool {
	r := d.regions
	sort.Slice(r, func(i, j int) bool { return r[i].lo < r[j].lo })
	for i := 1; i < len(r); i++ {
		if r[i].lo < r[i-1].hi {
			return false
		}
	}
	return true
}

func (d *sndDSP) loadFlt(c *cpu.CPU, addr uint32) int {
	if !d.region(addr, fltSize) {
		return -1
	}
	d.flts = append(d.flts, fltState{addr: addr,
		b0: int32(c.Read32(addr + fltB0)), b1: int32(c.Read32(addr + fltB1)), a1: int32(c.Read32(addr + fltA1)),
		w0: int32(c.Read32(addr + fltW0)), w1: int32(c.Read32(addr + fltW1))})
	return len(d.flts) - 1
}

// loadFltChain loads a filter and the ones FLT_GetNext runs after it; it returns the first and
// one past the last index into flts.
func (d *sndDSP) loadFltChain(c *cpu.CPU, addr uint32) (int, int, bool) {
	if !inArena(addr, fltSize) {
		return 0, 0, false
	}
	n := 0
	switch int32(c.Read32(addr + fltCount)) {
	case 1:
		n = 1
	case 2:
		n = 2
	case 3:
		n = 3
	}
	first := d.loadFlt(c, addr)
	if first < 0 {
		return 0, 0, false
	}
	for k := 0; k < n; k++ {
		if d.loadFlt(c, c.Read32(addr+fltNext+uint32(4*k))) < 0 {
			return 0, 0, false
		}
	}
	return first, len(d.flts), true
}

// loadDly loads a delay line; allpass is DFR's inlined all-pass, which ignores the type.
func (d *sndDSP) loadDly(c *cpu.CPU, addr uint32, allpass bool) bool {
	if !d.region(addr, dlySize) {
		return false
	}
	s := dlyState{addr: addr, typ: int32(c.Read32(addr + dlyType)), d: int32(c.Read32(addr + dlyD)),
		w: c.Read32(addr + dlyW), a: int32(c.Read32(addr + dlyA)), b: int32(c.Read32(addr + dlyB)), flt: -1}
	if allpass {
		s.typ = 1
	}
	taps := 1
	if s.typ == 5 || s.typ == 6 {
		taps = 4
	}
	s.t[0] = int32(c.Read32(addr + dlyT))
	for k := 1; k < taps; k++ {
		s.t[k] = int32(c.Read32(addr + dlyT1 + uint32(4*(k-1))))
	}
	if s.d < 0 || s.d >= 1<<26 {
		return false
	}
	size := uint32(s.d+1) * 4
	p := c.Read32(addr + dlyP)
	if p < s.w || p-s.w >= size || (p-s.w)&3 != 0 {
		return false
	}
	for k := 0; k < taps; k++ {
		if s.t[k] < 0 || s.t[k] > s.d {
			return false
		}
	}
	if !d.region(s.w, size) {
		return false
	}
	s.i = int32((p - s.w) / 4)
	s.buf = c.WBytes(s.w, size)
	if s.typ == 2 || s.typ == 4 || s.typ == 5 {
		if s.flt = d.loadFlt(c, c.Read32(addr+dlyFlt)); s.flt < 0 {
			return false
		}
	}
	d.dlys = append(d.dlys, s)
	return true
}

// store writes the delay lines' positions and the filters' states back.
func (d *sndDSP) store(c *cpu.CPU) {
	for i := range d.dlys {
		s := &d.dlys[i]
		c.Write32(s.addr+dlyP, s.w+uint32(s.i)*4)
	}
	for i := range d.flts {
		f := &d.flts[i]
		c.Write32(f.addr+fltW0, uint32(f.w0))
		c.Write32(f.addr+fltW1, uint32(f.w1))
	}
}

// samples checks a portable_samplepair_t buffer of n samples and returns it.
func (d *sndDSP) samples(c *cpu.CPU, addr uint32, n int32) ([]byte, bool) {
	if n < 0 || n >= 1<<24 || !d.region(addr, uint32(n)*8) {
		return nil, false
	}
	return c.WBytes(addr, uint32(n)*8), true
}

// --- the arithmetic (each line follows the guest code; Q12 products wrap as 32-bit imul does) ---

func (f *fltState) next(x int32) int32 {
	if x|f.w0|f.w1 == 0 {
		return 0
	}
	w := x - f.a1*f.w1>>12
	y := (f.b0*w + f.w1*f.b1) >> 12
	f.w0, f.w1 = w, w
	return y
}

func (d *sndDSP) fltChain(first, end int, x int32) int32 {
	for k := first; k < end; k++ {
		x = d.flts[k].next(x)
	}
	return x
}

func (s *dlyState) tap(t int32) int32 {
	j := s.i + t
	if j > s.d {
		j -= s.d + 1
	}
	return int32(le.Uint32(s.buf[4*j:]))
}

func (s *dlyState) put(v int32) {
	le.PutUint32(s.buf[4*s.i:], uint32(v))
	if s.i == 0 {
		s.i = s.d
	} else {
		s.i--
	}
}

func (d *sndDSP) dlyNext(s *dlyState, x int32) int32 {
	switch s.typ {
	case 1: // all-pass
		y := s.tap(s.t[0])
		v := s.a*y>>12 + x
		out := -s.a*v>>12 + y
		s.put(v)
		return out * s.b >> 12
	case 2:
		v := d.flts[s.flt].next(s.tap(s.t[0])) + x
		s.put(v)
		return v * s.b >> 12
	case 3:
		y := s.tap(s.t[0])
		s.put(x)
		return y
	case 4:
		f := d.flts[s.flt].next(s.tap(s.t[0]))
		s.put(x)
		return f * s.b >> 12
	case 5, 6:
		y0, y1, y2, y3 := s.tap(s.t[0]), s.tap(s.t[1]), s.tap(s.t[2]), s.tap(s.t[3])
		if s.typ == 5 {
			s.put(d.flts[s.flt].next(y3) + x)
		} else {
			s.put(s.a*y3>>12 + x)
		}
		return (y0 + x + y1 + y2 + y3) * s.b >> 12
	default:
		v := s.a*s.tap(s.t[0])>>12 + x
		s.put(v)
		return v * s.b >> 12
	}
}

// eachSample runs fn over a sample buffer the way the *_GetNextN functions do for op.
func eachSample(buf []byte, op int32, fn func(x int32) int32) {
	for k := 0; k < len(buf); k += 8 {
		switch op {
		case opLeftDuplicate:
			y := fn(int32(le.Uint32(buf[k:])))
			le.PutUint32(buf[k+4:], uint32(y))
			le.PutUint32(buf[k:], uint32(y))
		case opRight:
			le.PutUint32(buf[k+4:], uint32(fn(int32(le.Uint32(buf[k+4:])))))
		default:
			le.PutUint32(buf[k:], uint32(fn(int32(le.Uint32(buf[k:])))))
		}
	}
}

// --- the functions ---
//
// Each loads and checks the call's structures and returns what runs it (nil: the call is left to
// the guest). The run computes, writes the results back and returns the function's value.

// int DLY_GetNext(dly_t *pdly, int x)
func (d *sndDSP) dlyGetNext(c *cpu.CPU) func() uint32 {
	if !d.loadDly(c, c.Arg(0), false) || !d.disjoint() {
		return nil
	}
	x := int32(c.Arg(1))
	return func() uint32 { return uint32(d.dlyNext(&d.dlys[0], x)) }
}

// void DLY_GetNextN(dly_t *pdly, portable_samplepair_t *pbuffer, int SampleCount, int op)
func (d *sndDSP) dlyGetNextN(c *cpu.CPU) func() uint32 {
	if !d.loadDly(c, c.Arg(0), false) {
		return nil
	}
	buf, ok := d.samples(c, c.Arg(1), int32(c.Arg(2)))
	if !ok || !d.disjoint() {
		return nil
	}
	op := int32(c.Arg(3))
	return func() uint32 {
		s := &d.dlys[0]
		eachSample(buf, op, func(x int32) int32 { return d.dlyNext(s, x) })
		return 0
	}
}

// int FLT_GetNext(flt_t *pflt, int x)
func (d *sndDSP) fltGetNext(c *cpu.CPU) func() uint32 {
	first, end, ok := d.loadFltChain(c, c.Arg(0))
	if !ok || !d.disjoint() {
		return nil
	}
	x := int32(c.Arg(1))
	return func() uint32 { return uint32(d.fltChain(first, end, x)) }
}

// void FLT_GetNextN(flt_t *pflt, portable_samplepair_t *pbuffer, int SampleCount, int op)
func (d *sndDSP) fltGetNextN(c *cpu.CPU) func() uint32 {
	first, end, ok := d.loadFltChain(c, c.Arg(0))
	if !ok {
		return nil
	}
	buf, ok := d.samples(c, c.Arg(1), int32(c.Arg(2)))
	if !ok || !d.disjoint() {
		return nil
	}
	op := int32(c.Arg(3))
	return func() uint32 {
		eachSample(buf, op, func(x int32) int32 { return d.fltChain(first, end, x) })
		return 0
	}
}

// void RVA_GetNextN(rva_t *prva, portable_samplepair_t *pbuffer, int SampleCount, int op):
// the sum of the delay lines, low-passed unless parallel. Modulated delay lines are left to
// the guest.
func (d *sndDSP) rvaGetNextN(c *cpu.CPU) func() uint32 {
	rva := c.Arg(0)
	if !d.region(rva, rvaSize) || c.Read8(rva+rvaModDy) != 0 {
		return nil
	}
	m := int32(c.Read32(rva + rvaM))
	if m > 12 {
		return nil
	}
	for k := int32(0); k < m; k++ {
		if !d.loadDly(c, c.Read32(rva+rvaDly+uint32(4*k)), false) {
			return nil
		}
	}
	first, end := 0, 0
	if c.Read32(rva+rvaPar) == 0 {
		if pf := c.Read32(rva + rvaFlt); pf != 0 {
			var ok bool
			if first, end, ok = d.loadFltChain(c, pf); !ok {
				return nil
			}
		}
	}
	buf, ok := d.samples(c, c.Arg(1), int32(c.Arg(2)))
	if !ok || !d.disjoint() {
		return nil
	}
	op := int32(c.Arg(3))
	return func() uint32 {
		dlys := d.dlys
		eachSample(buf, op, func(x int32) int32 {
			var y int32
			for k := range dlys {
				y += d.dlyNext(&dlys[k], x)
			}
			return d.fltChain(first, end, y)
		})
		return 0
	}
}

// void DFR_GetNextN(dfr_t *pdfr, portable_samplepair_t *pbuffer, int SampleCount, int op):
// all-pass delay lines in series.
func (d *sndDSP) dfrGetNextN(c *cpu.CPU) func() uint32 {
	dfr := c.Arg(0)
	if !inArena(dfr, dfrDly) {
		return nil
	}
	n := int32(c.Read32(dfr + dfrN))
	if n > 64 || !d.region(dfr, dfrDly+uint32(max(n, 0))*4) {
		return nil
	}
	for k := int32(0); k < n; k++ {
		if !d.loadDly(c, c.Read32(dfr+dfrDly+uint32(4*k)), true) {
			return nil
		}
	}
	buf, ok := d.samples(c, c.Arg(1), int32(c.Arg(2)))
	if !ok || !d.disjoint() {
		return nil
	}
	op := int32(c.Arg(3))
	return func() uint32 {
		dlys := d.dlys
		eachSample(buf, op, func(x int32) int32 {
			for k := range dlys {
				x = d.dlyNext(&dlys[k], x)
			}
			return x
		})
		return 0
	}
}

// call runs one intercepted call natively, or leaves it to the guest code.
func (d *sndDSP) call(c *cpu.CPU, f *sndDSPFunc) {
	if d.guest {
		c.StepRaw()
		return
	}
	d.reset()
	run := f.load(d, c)
	if run == nil {
		d.declined[f.name]++
		c.StepRaw() // the guest code, from its first instruction
		return
	}
	if d.check {
		d.verify(c, f, run)
		return
	}
	v := run()
	d.store(c)
	if f.value {
		ret(c, v)
	} else {
		c.Ret()
	}
}

// --- check mode ---

// verify runs a call both ways from the same memory: the guest code first (with every native
// DSP function passing calls to the guest code, so its nested calls are the guest's too), then the native one, and compares
// the return value and every structure and buffer the call reads or writes.
func (d *sndDSP) verify(c *cpu.CPU, f *sndDSPFunc, run func() uint32) {
	regions := append([]dspRegion(nil), d.regions...)
	snap := func() [][]byte {
		var out [][]byte
		for _, r := range regions {
			out = append(out, append([]byte(nil), c.Bytes(r.lo, r.hi-r.lo)...))
		}
		return out
	}
	before := snap()
	args := make([]uint32, f.args)
	for i := range args {
		args[i] = c.Arg(i)
	}
	d.guest = true // (not SetInterceptEnabled: that flushes the code cache)
	want, err := c.CallGuestNested(f.addr, args...)
	d.guest = false
	guest := snap()
	for i, r := range regions {
		copy(c.WBytes(r.lo, r.hi-r.lo), before[i])
	}
	got := run()
	d.store(c)
	native := snap()
	diff := ""
	if err != nil {
		diff = err.Error()
	} else if f.value && got != want {
		diff = fmt.Sprintf("returned %#x, the guest %#x", got, want)
	}
	for i, r := range regions {
		if diff != "" {
			break
		}
		for k := range native[i] {
			if native[i][k] != guest[i][k] {
				diff = fmt.Sprintf("byte %#x of %#x-%#x: %#02x, the guest %#02x", r.lo+uint32(k), r.lo, r.hi, native[i][k], guest[i][k])
				break
			}
		}
	}
	if diff == "" {
		d.checked[f.name]++
	} else {
		if d.bad[f.name] < 5 {
			d.m.logf("[dsp] MISMATCH %s(%#x): %s", f.name, args, diff)
		}
		d.bad[f.name]++
		for i, r := range regions {
			copy(c.WBytes(r.lo, r.hi-r.lo), guest[i]) // carry on with the guest's results
		}
		got = want
	}
	if f.value {
		ret(c, got)
	} else {
		c.Ret()
	}
}

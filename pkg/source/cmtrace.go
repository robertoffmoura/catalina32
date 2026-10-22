package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"

	"catalina32/pkg/cpu"
	"catalina32/pkg/dyld"
)

// Source's collision traces against the world (cmodel.cpp) in Go.
//
// Every box trace through the map (player movement, physics, the turrets' and lasers' line of
// sight) walks the collision BSP in CM_RecursiveHullCheckImpl, splitting the ray at each node
// plane, and clips it against the brushes of every leaf it reaches (CM_TraceToLeaf,
// CM_ClipBoxToBrush, IntersectRayWithBoxBrush). In GLaDOS's chamber in escape_02 that was about
// a third of the code the game ran. The functions below do the same arithmetic natively, from the
// first call of a trace's walk (box traces only: the point-trace instantiation is left as it is).
//
// They are bound by symbol and only when all four functions' bytes, and the constants they read,
// are the ones they were written from (Portal's engine.dylib of Nov 2024). They read and write the
// guest's memory where the guest code does, in the same order, with the same single- and
// double-precision steps: the result is bit for bit the guest's. Leaves with displacements are
// handed to the guest's CM_TraceToLeaf whole; so is every trace while VProf is enabled.
// C32_TRACECHECK=1 runs the guest code too on every trace and compares (see verify);
// C32_NATIVE_TRACE=0 turns the native version off.

type cmTraceFunc struct {
	name string
	file uint32 // address in the file, for the constants' addresses below
	size uint32
	hash string
}

var cmTraceFuncs = [...]cmTraceFunc{
	{"__ZL25CM_RecursiveHullCheckImplILb0EEvP11TraceInfo_tiffRK6VectorS4_", 0x102870, 896, "9ecc772374ee993f"},
	{"__Z14CM_TraceToLeafILb0EEvP11TraceInfo_tiff", 0x1055a0, 1664, "a74cec90759e868b"},
	{"__Z17CM_ClipBoxToBrushILb0EEvP11TraceInfo_tPK8cbrush_t", 0x105c20, 880, "339e432503ad1b81"},
	{"__Z24IntersectRayWithBoxBrushP11TraceInfo_tPK8cbrush_tP11cboxbrush_t", 0x101750, 960, "f58f5950aaba24ef"},
	// the point-trace (ray) instantiations
	{"__ZL25CM_RecursiveHullCheckImplILb1EEvP11TraceInfo_tiffRK6VectorS4_", 0x102520, 848, "7618c83ac651f2c1"},
	{"__Z14CM_TraceToLeafILb1EEvP11TraceInfo_tiff", 0x104f80, 1568, "8e63cb2c11abcdad"},
	{"__Z17CM_ClipBoxToBrushILb1EEvP11TraceInfo_tPK8cbrush_t", 0x105f90, 752, "e395f0f68e4b1e42"},
}

// The constants the code reads (file addresses and bytes), checked when binding.
var cmTraceConsts = []struct {
	file uint32
	hex  string
}{
	{0x3f3f60, "ffffff7fffffff7fffffff7fffffff7f"}, // |x| mask
	{0x3f3f70, "00000080000000800000008000000080"}, // sign mask
	{0x3f39e4, "0000803f"},                         // 1.0f
	{0x3f4040, "000000bd"},                         // -1/32 (DIST_EPSILON)
	{0x3f78d0, "000000000000a03f000000000000a0bf"}, // {1/32, -1/32} (double)
	{0x3f78e0, "000000000000a0bf000000000000a03f"}, // {-1/32, 1/32}
	{0x3f78f4, "804fc3c7000000000000a03f"},         // -99999.0f (never updated), 1/32 (double)
	{0x3f7890, "000000bd000000bd000000bd000000bd"}, // -1/32 x4
	{0x3f78a0, "0000003d0000003d0000003d0000003d"}, // 1/32 x4
	{0x3f78b0, "000000000100000002000000ffffffff"}, // faces 0-2
	{0x3f78c0, "030000000400000005000000ffffffff"}, // faces 3-5
	{0x3f7900, "010000000200000004000000"},         // sign bits per axis
	// Four_Zeros, Four_Ones, Four_FLT_MAX, Four_Negative_FLT_MAX, g_SIMD_clear_wmask, vec3_origin
	// (through the pointers below)
	{0x3fdfc0, "00000000000000000000000000000000"},
	{0x3fdfd0, "0000803f0000803f0000803f0000803f"},
	{0x3fdff0, "ffff7f7fffff7f7fffff7f7fffff7f7f"},
	{0x3fe000, "ffff7fffffff7fffffff7fffffff7fff"},
	{0x3fe010, "ffffffffffffffffffffffff00000000"},
	{0x3fdfa8, "000000000000000000000000"},
}

// Pointers the code loads its SIMD constants through (non-lazy pointers into its own constants:
// checked at the first trace, once dyld has rebased them).
var cmTraceSlots = [][2]uint32{
	{0x4cb048, 0x3fdff0}, {0x4cb04c, 0x3fe000}, {0x4cb050, 0x3fdfd0}, {0x4cb054, 0x3fdfc0},
	{0x4cb058, 0x3fe010}, {0x4cb060, 0x3fdfa8},
}

const (
	cmNullSurface = 0x53d210 // CCollisionBSPData::nullsurface
	cmVProfSlot   = 0x4cb01c // &g_VProfCurrentProfile
	cmVProfOn     = 0x100c   // its m_enabled
	cmMaxDepth    = 1 << 14  // deeper than any map's tree: the data is broken
)

// TraceInfo_t (0x178 bytes), CCollisionBSPData and the structures they point to, from the code.
const (
	tiStart        = 0x00 // Vector m_start, m_end
	tiEnd          = 0x0c
	tiExtents      = 0x30
	tiDelta        = 0x3c
	tiInvDelta     = 0x48
	tiPlane        = 0x6c  // trace result: plane normal, dist (+0xc), type, signbits (+0x10, +0x11)
	tiFraction     = 0x80  // float
	tiContents     = 0x84  // int
	tiAllSolid     = 0x8a  // bool
	tiStartSolid   = 0x8b  // bool
	tiLeftSolid    = 0x8c  // float fractionleftsolid
	tiSurface      = 0x90  // csurface_t (8 bytes)
	tiMask         = 0xfc  // contents mask
	tiIsRay        = 0x100 // bool
	tiBSP          = 0x104
	tiDispHit      = 0x114
	tiDepth        = 0x11c // check-count depth
	tiCheckCount   = 0x120 // int[2]
	tiBrushCounter = 0x128 // CUtlVector<int>[2] (stride 0x14): memory at +0, size at +0xc
	tiSize         = 0x178

	bspNodes      = 0x00  // cnode_t * (12 bytes: plane, children[2])
	bspBrushSides = 0x68  // cbrushside_t * (8 bytes: plane, u16 surface, ...)
	bspBoxBrushes = 0x70  // cboxbrush_t * (0x30 bytes: mins, maxs, u16 surface[6] at 0x20)
	bspLeafs      = 0x88  // cleaf_t * (16 bytes: u16 firstbrush at 8, numbrushes at 0xa, numdisps at 0xe)
	bspLeafBrush  = 0x98  // u16 *
	bspBrushes    = 0xa8  // cbrush_t * (8 bytes: contents, u16 numsides, u16 firstside)
	bspSurfaces   = 0x220 // csurface_t * (8 bytes: flags u16 at 6)

	contentsOpaque      = 0x80
	contentsIgnoreNoDrw = 0x2000
	surfNoDraw          = 0x80
	cmEpsilon           = 0.03125 // DIST_EPSILON
	cmNeverUpdated      = -99999
)

type cmTrace struct {
	m     *Machine
	c     *cpu.CPU
	slide uint32
	hull  [2]uint32 // CM_RecursiveHullCheckImpl<false>, <true>
	leafF [2]uint32 // CM_TraceToLeaf<false>, <true>
	point bool      // this trace is a point trace (the <true> instantiation)
	slots int8      // 0: not checked yet, 1: as expected, -1: not (every trace goes to the guest)
	mem   []byte    // the CPU's memory and read limit, for this trace (r32 and the like)
	lim   uint32
	check bool
	guest bool // check mode: the guest code is running

	traces, declined, leaves, checked, bad int
}

// bindCMTrace replaces engine.dylib's box-trace walk when it is the one the Go code follows.
func (m *Machine) bindCMTrace(img *dyld.Image) {
	if img.Name() != "engine.dylib" || os.Getenv("C32_NATIVE_TRACE") == "0" {
		return
	}
	var addrs [len(cmTraceFuncs)]uint32
	for i, f := range cmTraceFuncs {
		addr, ok := m.localSymbol(img, f.name)
		if !ok {
			return
		}
		code := m.C.Bytes(addr, f.size)
		h := sha256.Sum256(code)
		if code == nil || hex.EncodeToString(h[:8]) != f.hash || addr-f.file != addrs[0]-cmTraceFuncs[0].file && i > 0 {
			m.logf("[trace] different code (%s), collision traces left to the guest", f.name)
			return
		}
		addrs[i] = addr
	}
	slide := addrs[0] - cmTraceFuncs[0].file
	for _, k := range cmTraceConsts {
		want, _ := hex.DecodeString(k.hex)
		if got := m.C.Bytes(slide+k.file, uint32(len(want))); !bytes.Equal(got, want) {
			m.logf("[trace] different constants at %#x, collision traces left to the guest", k.file)
			return
		}
	}
	t := &cmTrace{m: m, c: m.C, slide: slide, hull: [2]uint32{addrs[0], addrs[4]}, leafF: [2]uint32{addrs[1], addrs[5]},
		check: os.Getenv("C32_TRACECHECK") == "1"}
	m.C.RegisterIntercept(t.hull[0], "engine.dylib!"+cmTraceFuncs[0].name+" (native)", func(c *cpu.CPU) { t.call(c, false) })
	m.C.RegisterIntercept(t.hull[1], "engine.dylib!"+cmTraceFuncs[4].name+" (native)", func(c *cpu.CPU) { t.call(c, true) })
	m.cmTrace = t
	m.logf("[trace] collision traces in Go")
}

// report summarises the traces run (for tests).
func (t *cmTrace) report() string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("%d traces, %d left to the guest, %d leaves with displacements; checked %d, mismatched %d",
		t.traces, t.declined, t.leaves, t.checked, t.bad)
}

// --- memory ---

// The walk reads guest memory a lot: below the CPU's read limit (taken at each trace) straight
// from Mem, else through Read32 and the like (out of range: the guest halts, as its code would).

func (t *cmTrace) r32(a uint32) uint32 {
	if a < t.lim {
		return le.Uint32(t.mem[a:])
	}
	return t.c.Read32(a)
}

func (t *cmTrace) r16(a uint32) uint16 {
	if a < t.lim {
		return le.Uint16(t.mem[a:])
	}
	return t.c.Read16(a)
}

func (t *cmTrace) r8(a uint32) uint8 {
	if a < t.lim {
		return t.mem[a]
	}
	return t.c.Read8(a)
}

func (t *cmTrace) r64(a uint32) uint64 {
	if a < t.lim {
		return le.Uint64(t.mem[a:])
	}
	return t.c.Read64(a)
}

func (t *cmTrace) f32(a uint32) float32       { return math.Float32frombits(t.r32(a)) }
func (t *cmTrace) putF32(a uint32, v float32) { t.c.Write32(a, math.Float32bits(v)) }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func absf(x float32) float32 { return math.Float32frombits(math.Float32bits(x) &^ (1 << 31)) }
func negf(x float32) float32 { return math.Float32frombits(math.Float32bits(x) ^ 1<<31) }

// maxss and minss are the SSE instructions: the second operand unless the first is
// greater (smaller); so a NaN or two zeros give the second.
func maxss(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

func minss(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

// vec is a Vector argument. The guest code passes pointers and reads through them every time it
// needs a component, so one at a guest address is read there each time; the split points the
// walk makes itself live in its stack frames, which nothing else writes (a value here).
type vec struct {
	addr uint32
	v    [3]float32
}

func (t *cmTrace) at(p *vec, i uint32) float32 {
	if p.addr != 0 {
		return t.f32(p.addr + 4*i)
	}
	return p.v[i]
}

// --- the functions ---
//
// Products are rounded with float32()/float64() before the sums they feed, so that no target
// fuses them into a multiply-add (the guest code rounds each step).

// call is the intercept on CM_RecursiveHullCheckImpl<IS_POINT>(TraceInfo_t *ecx, int num edx,
// float p1f xmm0, float p2f xmm1, const Vector &p1, const Vector &p2).
func (t *cmTrace) call(c *cpu.CPU, point bool) {
	if t.guest {
		c.StepRaw()
		return
	}
	if !t.ready() {
		t.declined++
		c.StepRaw()
		return
	}
	t.traces++
	t.point = point
	if t.check {
		t.verify()
		return
	}
	t.run()
	c.Ret()
}

// ready checks what the native walk relies on beyond the code: the constants' pointers, and
// VProf being off (with it on, the guest code times the leaves).
func (t *cmTrace) ready() bool {
	c := t.c
	if t.slots == 0 {
		t.slots = 1
		for _, s := range cmTraceSlots {
			if c.Read32(t.slide+s[0]) != t.slide+s[1] {
				t.slots = -1
				t.m.logf("[trace] constant pointer at %#x is %#x: collision traces left to the guest", s[0], c.Read32(t.slide+s[0]))
			}
		}
	}
	if t.slots < 0 {
		return false
	}
	prof := c.Read32(t.slide + cmVProfSlot)
	return prof != 0 && c.Read32(prof+cmVProfOn) == 0
}

func (t *cmTrace) run() {
	c := t.c
	t.mem, t.lim = c.Mem, c.ReadLimit()
	esp := c.Regs[cpu.RegESP]
	p1 := vec{addr: c.Read32(esp + 4)}
	p2 := vec{addr: c.Read32(esp + 8)}
	t.recurse(c.Regs[cpu.RegECX], int32(c.Regs[cpu.RegEDX]),
		math.Float32frombits(le.Uint32(c.XMM[0][:])), math.Float32frombits(le.Uint32(c.XMM[1][:])), &p1, &p2, 0)
}

// recurse is CM_RecursiveHullCheckImpl: down the tree while the box stays on one side of the
// node planes, then the near side and the far side of the split. A point trace (point) has no
// box beside slanted planes (its offset is 0 there; beside axial ones it still reads the extents).
func (t *cmTrace) recurse(ti uint32, num int32, p1f, p2f float32, p1, p2 *vec, depth int) {
	c := t.c
	if p1f >= t.f32(ti+tiFraction) {
		return
	}
	if num < 0 {
		t.leaf(ti, ^num, p1f, p2f)
		return
	}
	if depth > cmMaxDepth {
		c.Halted = true
		c.HaltReason = fmt.Sprintf("collision trace deeper than %d nodes (ti %#x)", cmMaxDepth, ti)
		return
	}
	nodes := t.r32(t.r32(ti+tiBSP) + bspNodes)
	var node uint32
	var d1, d2, t1, t2, offset float32
	for {
		if c.Halted {
			return
		}
		node = nodes + uint32(num)*12
		plane := t.r32(node)
		typ := uint32(t.r8(plane + 0x10))
		dist := t.f32(plane + 0xc)
		if typ <= 2 { // axial
			offset = t.f32(ti + tiExtents + typ*4)
			d1, d2 = t.at(p1, typ), t.at(p2, typ)
		} else {
			n0, n1, n2 := t.f32(plane), t.f32(plane+4), t.f32(plane+8)
			d1 = float32(t.at(p1, 2)*n2) + (float32(t.at(p1, 1)*n1) + float32(t.at(p1, 0)*n0))
			d2 = float32(t.at(p2, 2)*n2) + (float32(t.at(p2, 1)*n1) + float32(t.at(p2, 0)*n0))
			offset = 0
			if !t.point {
				offset = absf(float32(n2*t.f32(ti+tiExtents+8))) +
					(absf(float32(n1*t.f32(ti+tiExtents+4))) + absf(float32(n0*t.f32(ti+tiExtents))))
			}
		}
		t1, t2 = d1-dist, d2-dist
		var child uint32
		if t1 > offset && t2 > offset {
			child = node + 4
		} else if neg := negf(offset); !(t1 >= neg) && !(t2 >= neg) {
			child = node + 8
		} else {
			break
		}
		if num = int32(t.r32(child)); num < 0 {
			t.leaf(ti, ^num, p1f, p2f)
			return
		}
	}

	// The box straddles the plane: split, a little past it on each side (in double precision).
	var frac, frac2 float32
	side := uint32(0)
	if !(t1 >= t2) {
		idist := float64(1 / (d1 - d2))
		frac = float32(idist * (float64(t1-offset) - cmEpsilon))
		frac2 = float32(idist * (float64(offset+t1) + cmEpsilon))
		side = 1
	} else if t1 > t2 {
		idist := float64(1 / (d1 - d2))
		frac = float32(idist * (float64(offset+t1) + cmEpsilon))
		frac2 = float32(idist * (float64(t1-offset) - cmEpsilon))
	} else {
		frac, frac2 = 1, 0
	}
	span := p2f - p1f

	f := minss(maxss(frac, 0), 1)
	mid := vec{}
	for i := uint32(0); i < 3; i++ {
		a := t.at(p1, i)
		mid.v[i] = float32((t.at(p2, i)-a)*f) + a
	}
	t.recurse(ti, int32(t.r32(node+4+side*4)), p1f, float32(span*f)+p1f, p1, &mid, depth+1)

	f = minss(maxss(frac2, 0), 1)
	midf := float32(span*f) + p1f
	for i := uint32(0); i < 3; i++ {
		a := t.at(p1, i)
		mid.v[i] = float32((t.at(p2, i)-a)*f) + a
	}
	t.recurse(ti, int32(t.r32(node+4+(side^1)*4)), midf, p2f, &mid, p2, depth+1)
}

// leaf is CM_TraceToLeaf: clip against each brush of the leaf not seen yet in this trace (both
// instantiations are the same but for the clipping they call).
func (t *cmTrace) leaf(ti uint32, ndx int32, f1, f2 float32) {
	c := t.c
	bsp := t.r32(ti + tiBSP)
	lf := t.r32(bsp+bspLeafs) + uint32(ndx)<<4
	if t.r16(lf+0xe) != 0 {
		t.guestLeaf(ti, ndx, f1, f2)
		return
	}
	n := uint32(t.r16(lf + 0xa))
	if n == 0 {
		return
	}
	first := uint32(t.r16(lf + 8))
	end := int32(first + n)
	depth := t.r32(ti + tiDepth)
	counters := t.r32(ti + depth*0x14 + tiBrushCounter)
	count := t.r32(ti + depth*4 + tiCheckCount)
	for i := int32(first); i < end; i++ {
		depth = t.r32(ti + tiDepth)
		bn := uint32(t.r16(t.r32(bsp+bspLeafBrush) + uint32(i)*2))
		if int32(t.r32(ti+depth*0x14+tiBrushCounter+0xc)) <= int32(bn) || counters == 0 {
			continue
		}
		seen := counters + bn*4
		if t.r32(seen) == count {
			continue
		}
		brushes := t.r32(bsp + bspBrushes)
		b := brushes + bn*8
		c.Write32(seen, count)
		mask := t.r32(ti + tiMask)
		contents := t.r32(b) & mask
		if contents == 0 {
			continue
		}
		if contents == contentsOpaque && mask&contentsIgnoreNoDrw != 0 && t.noDraw(ti, brushes+bn*8) {
			continue
		}
		t.clip(ti, b)
		if fr := t.f32(ti + tiFraction); fr == 0 || fr != fr {
			return
		}
	}
}

// guestLeaf runs the guest's CM_TraceToLeaf (a leaf with displacements).
func (t *cmTrace) guestLeaf(ti uint32, ndx int32, f1, f2 float32) {
	t.leaves++
	t.guest = true
	_, err := t.c.CallGuestNested(t.leafF[b2i(t.point)], ti, uint32(ndx), math.Float32bits(f1), math.Float32bits(f2))
	t.guest = false
	t.mem, t.lim = t.c.Mem, t.c.ReadLimit()
	if err != nil && !t.c.Halted {
		t.c.Halted = true
		t.c.HaltReason = "collision trace: " + err.Error()
	}
}

// surface returns the csurface_t a side's surface index names.
func (t *cmTrace) surface(bsp uint32, idx uint16) uint32 {
	if idx == 0xffff {
		return t.slide + cmNullSurface
	}
	return t.r32(bsp+bspSurfaces) + uint32(idx)*8
}

// noDraw says whether an opaque brush has a nodraw side (such brushes are skipped when the mask
// says CONTENTS_IGNORE_NODRAW_OPAQUE).
func (t *cmTrace) noDraw(ti, b uint32) bool {
	ns := uint32(t.r16(b + 4))
	if ns == 0 {
		return false
	}
	bsp := t.r32(ti + tiBSP)
	fs := uint32(t.r16(b + 6))
	if ns == 0xffff {
		sides := t.r32(bsp+bspBoxBrushes) + fs*0x30 + 0x20
		for k := uint32(0); k <= 5; k++ {
			if t.r8(t.surface(bsp, t.r16(sides+k*2))+6)&surfNoDraw != 0 {
				return true
			}
		}
		return false
	}
	sides := t.r32(bsp+bspBrushSides) + fs*8 + 4
	for k := uint32(0); int32(k) < int32(ns); k++ {
		if t.r8(t.surface(bsp, t.r16(sides+k*8))+6)&surfNoDraw != 0 {
			return true
		}
	}
	return false
}

// clip is CM_ClipBoxToBrush<false>: where the swept box enters and leaves the brush's planes.
func (t *cmTrace) clip(ti, b uint32) {
	c := t.c
	ns := uint32(t.r16(b + 4))
	if ns == 0 {
		return
	}
	if ns == 0xffff {
		t.box(ti, b, t.r32(t.r32(ti+tiBSP)+bspBoxBrushes)+uint32(t.r16(b+6))*0x30)
		return
	}
	contents := t.r32(b)
	first := uint32(t.r16(b + 6))
	e0, e1, e2 := t.f32(ti+tiExtents), t.f32(ti+tiExtents+4), t.f32(ti+tiExtents+8)
	s0, s1, s2 := t.f32(ti+tiStart), t.f32(ti+tiStart+4), t.f32(ti+tiStart+8)
	x0, x1, x2 := t.f32(ti+tiEnd), t.f32(ti+tiEnd+4), t.f32(ti+tiEnd+8)
	bsp := t.r32(ti + tiBSP)
	sides := t.r32(bsp + bspBrushSides)
	enter, leave := float32(cmNeverUpdated), float32(1)
	startOut, getOut := false, false
	lead := uint32(0)
	for s, end := sides+first*8, sides+(first+ns)*8; s < end; s += 8 {
		if t.point && t.r16(s+6) != 0 {
			continue // a point trace skips the bevel planes
		}
		plane := t.r32(s)
		n0, n1, n2 := t.f32(plane), t.f32(plane+4), t.f32(plane+8)
		dist := t.f32(plane + 0xc)
		if !t.point {
			dist = (absf(float32(n2*e2)) + (absf(float32(n1*e1)) + absf(float32(n0*e0)))) + dist
		}
		ds := float32(n2*s2) + (float32(n1*s1) + float32(n0*s0))
		de := float32(n2*x2) + (float32(n1*x1) + float32(n0*x0))
		d1, d2 := ds-dist, de-dist
		out := getOut
		if d1 > 0 {
			startOut = true
			if d2 > 0 {
				return // in front of this plane all the way
			}
		} else {
			out = true
			if !(d2 > 0) {
				continue // behind it all the way
			}
		}
		if d1 > d2 { // entering
			f := maxss(d1-cmEpsilon, 0) / (ds - de)
			if f > enter {
				enter, lead = f, s
			}
		} else { // leaving
			f := float32((float64(d1) + cmEpsilon) / float64(ds-de))
			if !(f >= leave) {
				leave = f
			}
		}
		getOut = out
	}

	// The start is inside the brush; for a point trace, also when it enters the brush before
	// getting out of the one it started in.
	if !startOut || t.point && t.f32(ti+tiLeftSolid)-enter > 0 {
		c.Write8(ti+tiStartSolid, 1)
		c.Write32(ti+tiContents, contents)
		if !getOut {
			c.Write8(ti+tiAllSolid, 1)
			c.Write32(ti+tiFraction, 0)
			t.putF32(ti+tiLeftSolid, 1)
			return
		}
		if leave == 1 || leave != leave || !(leave > t.f32(ti+tiLeftSolid)) {
			return
		}
		t.putF32(ti+tiLeftSolid, leave)
		if !(leave >= t.f32(ti+tiFraction)) {
			return
		}
		t.putF32(ti+tiFraction, 1)
		c.Write64(ti+tiSurface, t.r64(t.slide+cmNullSurface))
		return
	}
	if enter >= leave || !(enter > cmNeverUpdated) || enter >= t.f32(ti+tiFraction) {
		return
	}
	t.putF32(ti+tiFraction, maxss(0, enter))
	c.Write32(ti+tiDispHit, 0)
	plane := t.r32(lead)
	c.Write32(ti+tiPlane, t.r32(plane))
	c.Write32(ti+tiPlane+4, t.r32(plane+4))
	c.Write32(ti+tiPlane+8, t.r32(plane+8))
	c.Write64(ti+tiPlane+0xc, t.r64(plane+0xc))
	c.Write64(ti+tiSurface, t.r64(t.surface(bsp, t.r16(lead+4))))
	c.Write32(ti+tiContents, contents)
}

// box is IntersectRayWithBoxBrush (the same for both instantiations): the slab test against an axis-aligned brush, in SSE on four
// lanes in the guest code. Only lanes 0-2 reach a result (the fourth is masked off or never
// read), so it runs on three; per lane each step is the SSE instruction's.
func (t *cmTrace) box(ti, brush, bx uint32) {
	c := t.c
	var lo1, hi1, inv, lo2, hi2 [3]float32
	var below, above, sel [3]bool
	early := false
	for i := uint32(0); i < 3; i++ {
		s, ext, delta := t.f32(ti+tiStart+4*i), t.f32(ti+tiExtents+4*i), t.f32(ti+tiDelta+4*i)
		mins := t.f32(bx+4*i) - s - ext
		maxs := t.f32(bx+0x10+4*i) - s + ext
		lo1[i], hi1[i] = mins, maxs
		below[i] = 0 < mins // the start is outside on this axis
		above[i] = maxs < 0
		enters, leaves := delta < mins, maxs < delta
		if below[i] && enters || above[i] && leaves {
			early = true // outside and moving away
		}
		sel[i] = enters != below[i] || leaves != above[i] // the ray crosses this slab
		inv[i] = t.f32(ti + tiInvDelta + 4*i)
	}
	if early {
		return
	}
	// First without the epsilon: does the ray touch the box at all?
	var tlo, thi [3]float32
	for i := range tlo {
		a, b := float32(lo1[i]*inv[i]), float32(hi1[i]*inv[i])
		tlo[i], thi[i] = float32(-math.MaxFloat32), float32(math.MaxFloat32)
		if sel[i] {
			tlo[i], thi[i] = minss(a, b), maxss(b, a)
		}
	}
	tmax := minss(minss(thi[2], minss(thi[1], thi[0])), 1)
	tmin := maxss(maxss(tlo[2], maxss(tlo[1], tlo[0])), 0)
	if tmax < tmin {
		return
	}
	var a2, b2 [3]float32
	for i := range lo2 {
		a2[i], b2[i] = float32((lo1[i]-cmEpsilon)*inv[i]), float32((hi1[i]+cmEpsilon)*inv[i])
		lo2[i], hi2[i] = float32(-math.MaxFloat32), float32(math.MaxFloat32)
		if sel[i] {
			lo2[i], hi2[i] = minss(a2[i], b2[i]), maxss(a2[i], b2[i])
		}
	}
	tmax = minss(minss(hi2[2], minss(hi2[1], hi2[0])), 1)
	m01 := maxss(lo2[0], lo2[1])
	tmin = maxss(maxss(m01, lo2[2]), 0)
	if tmax < tmin {
		return
	}
	outside := below[0] || below[1] || below[2] || above[0] || above[1] || above[2]
	if !outside || t.r8(ti+tiIsRay) != 0 && !(tmin >= t.f32(ti+tiLeftSolid)) {
		c.Write8(ti+tiStartSolid, 1)
		c.Write32(ti+tiContents, t.r32(brush))
		if tmax >= 1 {
			c.Write8(ti+tiAllSolid, 1)
			c.Write32(ti+tiFraction, 0)
			return
		}
		if !(tmax > t.f32(ti+tiLeftSolid)) {
			return
		}
		t.putF32(ti+tiLeftSolid, tmax)
		if tmax >= t.f32(ti+tiFraction) {
			t.putF32(ti+tiFraction, 1)
			c.Write64(ti+tiSurface, t.r64(t.slide+cmNullSurface))
		}
		return
	}
	if tmin >= t.f32(ti+tiFraction) {
		return
	}
	// The face it enters through: of the axis with the latest entry, the near one.
	var face [3]uint32
	for i := range face {
		face[i] = uint32(i) + 3
		if a2[i] <= b2[i] {
			face[i] = uint32(i)
		}
	}
	f := face[1]
	if lo2[1] < lo2[0] {
		f = face[0]
	}
	if !(lo2[2] < m01) {
		f = face[2]
	}
	c.Write32(ti+tiDispHit, 0)
	t.putF32(ti+tiFraction, tmin)
	origin := t.r32(t.slide + 0x4cb060) // vec3_origin
	c.Write32(ti+tiPlane, t.r32(origin))
	c.Write32(ti+tiPlane+4, t.r32(origin+4))
	c.Write32(ti+tiPlane+8, t.r32(origin+8))
	c.Write64(ti+tiSurface, t.r64(t.surface(t.r32(ti+tiBSP), t.r16(bx+0x20+f*2))))
	if f >= 3 {
		c.Write32(ti+tiPlane+0xc, t.r32(bx+f*4+4)) // maxs[f-3]
		t.putF32(ti+0x60+f*4, 1)
		f -= 3
		c.Write8(ti+tiPlane+0x11, 0)
	} else {
		c.Write32(ti+tiPlane+0xc, t.r32(bx+f*4)^1<<31) // -mins[f]
		t.putF32(ti+tiPlane+f*4, -1)
		c.Write8(ti+tiPlane+0x11, t.r8(t.slide+0x3f7900+f*4))
	}
	c.Write8(ti+tiPlane+0x10, uint8(f))
	c.Write32(ti+tiContents, t.r32(brush))
}

// --- check mode ---

// verify runs a trace both ways from the same memory: the guest code first (every nested call
// the guest's too), then the native one; it compares the TraceInfo and both check-count arrays
// (everything either writes) and carries on with the guest's results.
func (t *cmTrace) verify() {
	c := t.c
	ti := c.Regs[cpu.RegECX]
	regions := []dspRegion{{ti, ti + tiSize}}
	for k := uint32(0); k < 4; k++ { // brush and displacement counters, both depths
		v := ti + tiBrushCounter + k*0x14
		if base, n := c.Read32(v), c.Read32(v+0xc); base != 0 && n < 1<<22 {
			regions = append(regions, dspRegion{base, base + 4*n})
		}
	}
	snap := func() [][]byte {
		var out [][]byte
		for _, r := range regions {
			out = append(out, append([]byte(nil), c.Bytes(r.lo, r.hi-r.lo)...))
		}
		return out
	}
	before := snap()
	regs, xmm, eip := c.Regs, c.XMM, c.EIP
	// The guest's walk, as a call from here: the same registers and arguments, one frame lower.
	esp := regs[cpu.RegESP]
	args := []uint32{c.Read32(esp + 4), c.Read32(esp + 8)}
	t.guest = true
	err := t.callHull(args)
	t.guest = false
	c.Regs, c.XMM, c.EIP = regs, xmm, eip
	guest := snap()
	for i, r := range regions {
		copy(c.WBytes(r.lo, r.hi-r.lo), before[i])
	}
	t.run()
	native := snap()
	diff := ""
	if err != nil {
		diff = err.Error()
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
		t.checked++
	} else {
		if t.bad < 5 {
			t.m.logf("[trace] MISMATCH ti %#x num %d: %s", ti, int32(regs[cpu.RegEDX]), diff)
		}
		t.bad++
		for i, r := range regions {
			copy(c.WBytes(r.lo, r.hi-r.lo), guest[i])
		}
	}
	c.Ret()
}

// callHull runs the guest's CM_RecursiveHullCheckImpl with the current ecx, edx, xmm0 and xmm1
// and the given stack arguments (CallGuestNested passes only stack arguments; the registers
// it leaves as they are).
func (t *cmTrace) callHull(args []uint32) error {
	_, err := t.c.CallGuestNested(t.hull[b2i(t.point)], args...)
	return err
}

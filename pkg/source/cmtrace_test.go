package source

// The native collision traces against Portal's own code: engine.dylib is placed in a bare CPU,
// random collision BSPs (node planes axial and not, leaves sharing brushes, plain and box
// brushes, nodraw sides) and random box and ray traces are built in its memory, and every trace
// runs both ways from the same memory; the whole test heap must come out the same. Needs the
// game (C32_SOURCE_DIR).

import (
	"bytes"
	"debug/macho"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"catalina32/pkg/cpu"
)

const (
	cmTestBase  = 0x10000000 // engine.dylib, at its own addresses above this
	cmTestHeap  = 0x30000000
	cmTestSize  = 0x40000
	cmTestStack = 0x1FF00000
	cmTestRet   = 0x2FFF0000
)

type cmTestGuest struct {
	c      *cpu.CPU
	t      *cmTrace
	r      *rand.Rand
	next   uint32
	bounds [][2][3]float32 // the brushes' boxes (for slanted ones, a box inside), for aiming traces
}

func newCMTestGuest(t *testing.T) *cmTestGuest {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set (Portal is needed)")
	}
	f, err := macho.Open(filepath.Join(dir, "bin", "engine.dylib"))
	if err != nil {
		t.Skip("no engine.dylib:", err)
	}
	defer f.Close()
	c := cpu.NewCPU()
	for _, s := range f.Sections {
		if s.Flags&0xff == 1 { // zero fill
			continue
		}
		b, err := s.Data()
		if err != nil {
			t.Fatal(err)
		}
		copy(c.Mem[cmTestBase+uint32(s.Addr):], b)
	}
	var addrs [len(cmTraceFuncs)]uint32
	for i, fn := range cmTraceFuncs {
		for _, s := range f.Symtab.Syms {
			if s.Name == fn.name {
				addrs[i] = cmTestBase + uint32(s.Value)
			}
		}
		if addrs[i] != cmTestBase+fn.file {
			t.Fatalf("engine.dylib's %s is at %#x, not %#x", fn.name, addrs[i]-cmTestBase, fn.file)
		}
	}
	for _, s := range cmTraceSlots { // what dyld does: rebase the constants' pointers
		c.Write32(cmTestBase+s[0], cmTestBase+s[1])
	}
	m := &Machine{C: c}
	g := &cmTestGuest{c: c, r: rand.New(rand.NewSource(1)),
		t: &cmTrace{m: m, c: c, slide: cmTestBase, hull: [2]uint32{addrs[0], addrs[4]}, leafF: [2]uint32{addrs[1], addrs[5]}}}
	return g
}

func (g *cmTestGuest) alloc(n uint32) uint32 {
	a := cmTestHeap + g.next
	g.next += (n + 15) &^ 15
	if g.next > cmTestSize {
		panic("test heap full")
	}
	return a
}

func (g *cmTestGuest) f32(a uint32, v float32) { g.c.Write32(a, math.Float32bits(v)) }

// coord picks a coordinate: mostly on a coarse grid (so planes and box sides are hit exactly,
// and start and end often sit on them), sometimes anything.
func (g *cmTestGuest) coord() float32 {
	if g.r.Intn(3) == 0 {
		return float32(g.r.Float64()*400 - 200)
	}
	return float32(g.r.Intn(25)*16 - 192)
}

// plane allocates a cplane_t: axial (type 0-2, the normal along +axis) or not (type 3-5).
func (g *cmTestGuest) plane(axialOK bool) uint32 {
	p := g.alloc(20)
	var n [3]float32
	typ := 3 + g.r.Intn(3)
	if axialOK && g.r.Intn(2) == 0 {
		typ = g.r.Intn(3)
		n[typ] = 1
	} else {
		var l float64
		for l < 1e-3 {
			l = 0
			for i := range n {
				n[i] = float32(g.r.NormFloat64())
				if g.r.Intn(4) == 0 {
					n[i] = 0
				}
				l += float64(n[i] * n[i])
			}
		}
		for i := range n {
			n[i] = float32(float64(n[i]) / math.Sqrt(l))
		}
	}
	for i := range n {
		g.f32(p+uint32(4*i), n[i])
	}
	g.f32(p+0xc, g.coord())
	g.c.Write8(p+0x10, uint8(typ))
	g.c.Write8(p+0x11, uint8(g.r.Intn(8)))
	return p
}

// world builds a collision BSP and returns it with its brush count, root (a node or ^leaf) and
// leaf count.
func (g *cmTestGuest) world() (bsp uint32, nbrushes int, root int32, nleaves int) {
	c := g.c
	bsp = g.alloc(0x230)
	nsurf := 6
	surfs := g.alloc(uint32(nsurf) * 8)
	for k := 0; k < nsurf; k++ {
		flags := uint16(g.r.Intn(0x100))
		if g.r.Intn(3) != 0 {
			flags &^= surfNoDraw
		}
		c.Write32(surfs+uint32(8*k), g.r.Uint32())
		c.Write16(surfs+uint32(8*k)+4, uint16(g.r.Intn(100)))
		c.Write16(surfs+uint32(8*k)+6, flags)
	}
	surface := func() uint16 {
		if g.r.Intn(5) == 0 {
			return 0xffff // the null surface
		}
		return uint16(g.r.Intn(nsurf))
	}

	// Brushes: plain ones (sides around a box plus some slanted, or all slanted around a
	// point), box brushes, empty ones.
	g.bounds = g.bounds[:0]
	nbrushes = 1 + g.r.Intn(12)
	brushes := g.alloc(uint32(nbrushes) * 8)
	boxes := g.alloc(uint32(nbrushes) * 0x30)
	var sides []uint32 // plane, surface pairs
	contents := []uint32{1, 1, 1, contentsOpaque, contentsOpaque, 0x81, 8, 0x4000000, 0}
	for b := 0; b < nbrushes; b++ {
		at := brushes + uint32(8*b)
		c.Write32(at, contents[g.r.Intn(len(contents))])
		switch k := g.r.Intn(10); {
		case k < 4: // a box brush
			c.Write16(at+4, 0xffff)
			c.Write16(at+6, uint16(b))
			bx := boxes + uint32(0x30*b)
			var bb [2][3]float32
			for i := uint32(0); i < 3; i++ {
				lo, hi := g.coord(), g.coord()
				if lo > hi {
					lo, hi = hi, lo
				}
				g.f32(bx+4*i, lo)
				g.f32(bx+0x10+4*i, hi)
				bb[0][i], bb[1][i] = lo, hi
			}
			g.bounds = append(g.bounds, bb)
			c.Write32(bx+0xc, g.r.Uint32()) // padding the SIMD code loads too
			c.Write32(bx+0x1c, g.r.Uint32())
			for i := uint32(0); i < 6; i++ {
				c.Write16(bx+0x20+2*i, surface())
			}
		case k == 4: // no sides
			c.Write16(at+6, uint16(len(sides)/2))
		case k < 7: // slanted planes around a point
			c.Write16(at+6, uint16(len(sides)/2))
			var ctr [3]float32
			for i := range ctr {
				ctr[i] = g.coord()
			}
			r := float32(16 + g.r.Intn(80))
			n := 6 + g.r.Intn(4)
			for k := 0; k < n; k++ {
				p := g.plane(false)
				d := float32(0)
				for i := uint32(0); i < 3; i++ {
					d += math.Float32frombits(c.Read32(p+4*i)) * ctr[i]
				}
				g.f32(p+0xc, d+r*float32(0.6+0.4*g.r.Float64()))
				sides = append(sides, p, uint32(surface()))
			}
			c.Write16(at+4, uint16(n))
			h := r / 2 // inside: every plane is at least 0.6r away
			g.bounds = append(g.bounds, [2][3]float32{{ctr[0] - h, ctr[1] - h, ctr[2] - h}, {ctr[0] + h, ctr[1] + h, ctr[2] + h}})
		default:
			c.Write16(at+6, uint16(len(sides)/2))
			n := 0
			var bb [2][3]float32
			for i := 0; i < 3; i++ { // around a box
				lo, hi := g.coord(), g.coord()
				if lo > hi {
					lo, hi = hi, lo
				}
				bb[0][i], bb[1][i] = lo, hi
				for s, d := range []float32{-lo, hi} {
					p := g.alloc(20)
					g.f32(p+uint32(4*i), float32(2*s-1))
					g.f32(p+0xc, d)
					c.Write8(p+0x10, uint8(i))
					sides = append(sides, p, uint32(surface()))
					n++
				}
			}
			for extra := g.r.Intn(3); extra > 0; extra-- {
				sides = append(sides, g.plane(false), uint32(surface()))
				n++
			}
			c.Write16(at+4, uint16(n))
			g.bounds = append(g.bounds, bb)
		}
	}
	bsides := g.alloc(uint32(len(sides)/2*8 + 8))
	for k := 0; k < len(sides); k += 2 {
		c.Write32(bsides+uint32(4*k), sides[k])
		c.Write16(bsides+uint32(4*k)+4, uint16(sides[k+1]))
		c.Write16(bsides+uint32(4*k)+6, uint16(g.r.Intn(2)))
	}

	// The tree: node planes, leaves with a few of the brushes each (shared between leaves).
	var nodes [][3]uint32
	var leaves [][2]uint16
	var leafBrushes []uint16
	var build func(depth int) int32
	build = func(depth int) int32 {
		if depth == 0 || g.r.Intn(4) == 0 {
			first := len(leafBrushes)
			for k := g.r.Intn(4); k > 0; k-- {
				leafBrushes = append(leafBrushes, uint16(g.r.Intn(nbrushes)))
			}
			leaves = append(leaves, [2]uint16{uint16(first), uint16(len(leafBrushes) - first)})
			return ^int32(len(leaves) - 1)
		}
		i := len(nodes)
		nodes = append(nodes, [3]uint32{g.plane(true)})
		front, back := build(depth-1), build(depth-1)
		nodes[i][1], nodes[i][2] = uint32(front), uint32(back)
		return int32(i)
	}
	root = build(1 + g.r.Intn(7))
	nodeMem := g.alloc(uint32(len(nodes)*12 + 12))
	for k, n := range nodes {
		for j := range n {
			c.Write32(nodeMem+uint32(12*k+4*j), n[j])
		}
	}
	leafMem := g.alloc(uint32(len(leaves)*16 + 16))
	for k, l := range leaves {
		c.Write32(leafMem+uint32(16*k), g.r.Uint32()) // contents, cluster, area: not read
		c.Write16(leafMem+uint32(16*k)+8, l[0])
		c.Write16(leafMem+uint32(16*k)+0xa, l[1])
		c.Write16(leafMem+uint32(16*k)+0xc, uint16(g.r.Intn(9)))
		c.Write16(leafMem+uint32(16*k)+0xe, 0) // no displacements (those go to the guest's code)
	}
	lbMem := g.alloc(uint32(len(leafBrushes)*2 + 2))
	for k, b := range leafBrushes {
		c.Write16(lbMem+uint32(2*k), b)
	}
	c.Write32(bsp+bspNodes, nodeMem)
	c.Write32(bsp+bspBrushSides, bsides)
	c.Write32(bsp+bspBoxBrushes, boxes)
	c.Write32(bsp+bspLeafs, leafMem)
	c.Write32(bsp+bspLeafBrush, lbMem)
	c.Write32(bsp+bspBrushes, brushes)
	c.Write32(bsp+bspSurfaces, surfs)
	return bsp, nbrushes, root, len(leaves)
}

// traceInfo builds a TraceInfo_t for a trace through bsp, as CM_BoxTrace fills it in (for a point
// trace, mostly without extents).
func (g *cmTestGuest) traceInfo(bsp uint32, nbrushes int, point bool) uint32 {
	c := g.c
	ti := g.alloc(tiSize)
	for k := uint32(0); k < tiSize; k += 4 { // what the trace doesn't set: anything
		c.Write32(ti+k, g.r.Uint32())
	}
	var start, end, ext [3]float32
	if point && g.r.Intn(10) != 0 {
		// no extents
	} else if g.r.Intn(2) == 0 { // a box, else a ray
		for i := range ext {
			ext[i] = float32(g.r.Intn(5) * 8)
			if g.r.Intn(2) == 0 {
				ext[i] = float32(g.r.Float64() * 40)
			}
		}
	}
	for i := range start {
		start[i] = g.coord()
		switch g.r.Intn(4) {
		case 0:
			end[i] = start[i] // not moving on this axis
		default:
			end[i] = g.coord()
		}
	}
	switch mode := g.r.Intn(8); {
	case mode == 0:
		end = start
	case mode < 3 && len(g.bounds) > 0: // starting inside a brush
		b := g.bounds[g.r.Intn(len(g.bounds))]
		for i := range start {
			start[i] = b[0][i] + (b[1][i]-b[0][i])*float32(g.r.Intn(5))/4
		}
	case mode < 5 && len(g.bounds) > 0: // at a brush's corner, diagonally: entry times tie
		b := g.bounds[g.r.Intn(len(g.bounds))]
		k, d := float32(g.r.Intn(4)*8), float32(g.r.Intn(8)*16+16)
		for i := range start {
			switch g.r.Intn(3) {
			case 0:
				start[i] = b[0][i] - ext[i] - k
				end[i] = start[i] + d
			case 1:
				start[i] = b[1][i] + ext[i] + k
				end[i] = start[i] - d
			default:
				start[i] = (b[0][i] + b[1][i]) / 2
				end[i] = start[i]
			}
		}
	}
	isRay := ext == [3]float32{}
	for i := uint32(0); i < 3; i++ {
		d := end[i] - start[i]
		inv := float32(math.MaxFloat32)
		if d != 0 {
			inv = 1 / d
		}
		g.f32(ti+tiStart+4*i, start[i])
		g.f32(ti+tiEnd+4*i, end[i])
		g.f32(ti+0x18+4*i, -ext[i])
		g.f32(ti+0x24+4*i, ext[i])
		g.f32(ti+tiExtents+4*i, ext[i])
		g.f32(ti+tiDelta+4*i, d)
		g.f32(ti+tiInvDelta+4*i, inv)
	}
	for k := uint32(0x54); k < 0x80; k += 4 {
		c.Write32(ti+k, 0)
	}
	g.f32(ti+tiFraction, 1)
	if g.r.Intn(6) == 0 {
		g.f32(ti+tiFraction, float32(g.r.Float64())) // a nearer hit found already
	}
	c.Write32(ti+tiContents, 0)
	c.Write8(ti+tiAllSolid, 0)
	c.Write8(ti+tiStartSolid, 0)
	g.f32(ti+tiLeftSolid, 0)
	masks := []uint32{0xffffffff, 1, 0x81, 0x2081, 0x200b, 0x4000001, 0x2000 | contentsOpaque}
	c.Write32(ti+tiMask, masks[g.r.Intn(len(masks))])
	c.Write8(ti+tiIsRay, b2u8(isRay))
	c.Write8(ti+tiIsRay+1, b2u8(end != start))
	c.Write32(ti+tiBSP, bsp)
	depth := uint32(g.r.Intn(2))
	c.Write32(ti+tiDepth, depth)
	count := g.r.Uint32()
	c.Write32(ti+tiCheckCount+4*depth, count)
	n := nbrushes
	if g.r.Intn(5) == 0 {
		n = g.r.Intn(nbrushes + 1) // too short: those brushes are skipped
	}
	counters := g.alloc(uint32(4*nbrushes + 4))
	for k := 0; k < nbrushes; k++ {
		v := g.r.Uint32()
		if g.r.Intn(5) == 0 {
			v = count // seen already in this trace
		}
		c.Write32(counters+uint32(4*k), v)
	}
	v := ti + tiBrushCounter + 0x14*depth
	if g.r.Intn(20) == 0 {
		counters = 0
	}
	c.Write32(v, counters)
	c.Write32(v+0xc, uint32(n))
	return ti
}

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// run runs a trace natively, as the intercept would.
func (g *cmTestGuest) run(ti uint32, num int32, p1f, p2f float32, point bool) {
	c := g.c
	g.t.point = point
	c.Regs[cpu.RegECX] = ti
	c.Regs[cpu.RegEDX] = uint32(num)
	c.XMM[0], c.XMM[1] = [16]byte{}, [16]byte{}
	le.PutUint32(c.XMM[0][:], math.Float32bits(p1f))
	le.PutUint32(c.XMM[1][:], math.Float32bits(p2f))
	esp := uint32(cmTestStack - 0x100)
	c.Write32(esp-4, ti+tiEnd)
	c.Write32(esp-8, ti+tiStart)
	c.Write32(esp-12, cmTestRet)
	c.Regs[cpu.RegESP] = esp - 12
	g.t.run()
}

// both runs a trace from node num of ti's tree between fractions p1f and p2f, the guest's way
// and natively from the same memory, and compares the heap.
func (g *cmTestGuest) both(t *testing.T, round int, ti uint32, num int32, p1f, p2f float32, point bool) {
	t.Helper()
	c := g.c
	heap := c.Mem[cmTestHeap : cmTestHeap+cmTestSize]
	before := append([]byte(nil), heap...)
	set := func() {
		c.Regs[cpu.RegECX] = ti
		c.Regs[cpu.RegEDX] = uint32(num)
		c.XMM[0], c.XMM[1] = [16]byte{}, [16]byte{}
		le.PutUint32(c.XMM[0][:], math.Float32bits(p1f))
		le.PutUint32(c.XMM[1][:], math.Float32bits(p2f))
	}
	set()
	c.Regs[cpu.RegESP] = cmTestStack
	if _, err := c.CallGuestNested(g.t.hull[b2i(point)], ti+tiStart, ti+tiEnd); err != nil {
		t.Fatalf("round %d: the guest code: %v", round, err)
	}
	guest := append([]byte(nil), heap...)
	copy(heap, before)

	g.run(ti, num, p1f, p2f, point)
	if c.Halted {
		t.Fatalf("round %d: halted: %s", round, c.HaltReason)
	}
	if !bytes.Equal(heap, guest) {
		for k := range heap {
			if heap[k] != guest[k] {
				off := uint32(k) + cmTestHeap
				what := ""
				if off >= ti && off < ti+tiSize {
					what = fmt.Sprintf(" (TraceInfo+%#x)", off-ti)
				}
				t.Fatalf("round %d (point %v): memory differs first at %#x%s: %#02x, the guest %#02x", round, point, off, what, heap[k], guest[k])
			}
		}
	}
}

func TestCMTraceAgainstEngine(t *testing.T) {
	g := newCMTestGuest(t)
	c := g.c
	prof := uint32(cmTestHeap + cmTestSize) // g_VProfCurrentProfile, off
	c.Write32(cmTestBase+cmVProfSlot, prof)
	if !g.t.ready() {
		t.Fatal("not ready: the constants' pointers or VProf")
	}
	hits, solid, out := 0, 0, 0
	for round := 0; round < 40000; round++ {
		g.next = 0
		clear(c.Mem[cmTestHeap : cmTestHeap+cmTestSize])
		bsp, nb, root, nleaves := g.world()
		point := round%2 == 1
		ti := g.traceInfo(bsp, nb, point)
		p1f, p2f := float32(0), float32(1)
		if g.r.Intn(5) == 0 {
			p1f, p2f = float32(g.r.Float64()), float32(g.r.Float64())
		}
		num := root
		if g.r.Intn(10) == 0 {
			num = ^int32(g.r.Intn(nleaves)) // straight to a leaf
		}
		if g.r.Intn(4) == 0 {
			// Starting in a brush, the trace records where it gets out (fractionleftsolid); a
			// hit found already exactly there is the tie between "<" and "<=".
			heap := c.Mem[cmTestHeap : cmTestHeap+cmTestSize]
			before := append([]byte(nil), heap...)
			g.run(ti, num, p1f, p2f, point)
			left := c.Read32(ti + tiLeftSolid)
			copy(heap, before)
			if f := math.Float32frombits(left); f > 0 && f < 1 {
				c.Write32(ti+tiFraction, left)
			}
		}
		g.both(t, round, ti, num, p1f, p2f, point)
		if math.Float32frombits(c.Read32(ti+tiFraction)) < 1 {
			hits++
		}
		if c.Read8(ti+tiStartSolid) != 0 {
			solid++
			if f := math.Float32frombits(c.Read32(ti + tiLeftSolid)); f > 0 && f < 1 {
				out++
			}
		}
	}
	t.Logf("%d traces hit something, %d started in a brush, %d of them got out", hits, solid, out)
	if hits < 4000 || solid < 600 {
		t.Errorf("too few hits (%d) or starts in a brush (%d) to test much", hits, solid)
	}
}

package d3d9

import (
	"math/rand"
	"testing"
)

// TestSpanMatchesInside checks that span covers exactly the pixels inside reports, row by row.
func TestSpanMatchesInside(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	coord := func(n float32) float32 {
		switch r.Intn(4) {
		case 0:
			return float32(r.Intn(int(n))) // on pixel corners
		case 1:
			return float32(r.Intn(int(n))) + 0.5 // through pixel centres
		default:
			return r.Float32()*(n*3) - n // anywhere, often off screen
		}
	}
	for k := 0; k < 20000; k++ {
		ax, ay, bx, by, cx, cy := coord(64), coord(48), coord(64), coord(48), coord(64), coord(48)
		te, ok := setupEdges(ax, ay, bx, by, cx, cy)
		if !ok {
			continue
		}
		x0, x1 := r.Intn(20), 20+r.Intn(44)
		for y := 0; y < 48; y++ {
			e, step := te.start(x0, y)
			want := map[int]bool{}
			f := e
			for x := x0; x <= x1; x, f[0], f[1], f[2] = x+1, f[0]+step[0], f[1]+step[1], f[2]+step[2] {
				if te.inside(&f) {
					want[x] = true
				}
			}
			first, last, ok := te.span(&e, step, x0, x1)
			got := 0
			if ok {
				g, _ := te.start(first, y)
				if g != e {
					t.Fatalf("tri %d row %d: stepped values %v, want %v", k, y, e, g)
				}
				for x := first; x <= last; x++ {
					if !want[x] {
						t.Fatalf("tri %d row %d: span [%d,%d] covers %d, which inside rejects", k, y, first, last, x)
					}
					got++
				}
			}
			if got != len(want) {
				t.Fatalf("tri %d row %d: span covers %d pixels, inside %d", k, y, got, len(want))
			}
		}
	}
}

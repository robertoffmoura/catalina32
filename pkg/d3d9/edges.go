package d3d9

// triEdges are a screen triangle's three edge functions in 28.4 fixed point, evaluated at pixel
// centres. Coverage uses a tie-breaking rule that is antisymmetric in the edge direction, so two
// triangles sharing an edge cover each pixel on it exactly once (no cracks, no double blending).
// e0 is the edge opposite v0, so e0/area is v0's barycentric weight.
type triEdges struct {
	dx, dy [3]int64 // change per pixel step in x and per row
	adj    [3]int64 // 0 if the edge owns pixels exactly on it, else -1
	inv    float32  // 1 / (e0 + e1 + e2)
	x0, y0 [3]int64 // edge start points (fixed point)
	x1, y1 [3]int64 // edge end points
	sign   int64
}

const fixShift = 4 // 1/16 pixel

func toFix(f float32) int64 {
	if f >= 0 {
		return int64(f*(1<<fixShift) + 0.5)
	}
	return -int64(-f*(1<<fixShift) + 0.5)
}

// setupEdges prepares the edge functions; ok is false for degenerate or absurdly large triangles.
func setupEdges(ax, ay, bx, by, cx, cy float32) (t triEdges, ok bool) {
	const lim = 1 << 26
	for _, f := range [...]float32{ax, ay, bx, by, cx, cy} {
		if f != f || f > lim || f < -lim {
			return t, false
		}
	}
	X := [3]int64{toFix(ax), toFix(bx), toFix(cx)}
	Y := [3]int64{toFix(ay), toFix(by), toFix(cy)}
	// Edge i runs from vertex i+1 to vertex i+2.
	for i := 0; i < 3; i++ {
		a, b := (i+1)%3, (i+2)%3
		t.x0[i], t.y0[i], t.x1[i], t.y1[i] = X[a], Y[a], X[b], Y[b]
		t.dx[i] = (Y[a] - Y[b]) << fixShift
		t.dy[i] = (X[b] - X[a]) << fixShift
	}
	// The edge functions sum to twice the signed area everywhere; at vertex 0 only edge 0 is non-zero.
	area := t.at(0, X[0], Y[0])
	if area == 0 {
		return t, false
	}
	t.sign = 1
	if area < 0 {
		t.sign = -1
		area = -area
	}
	for i := 0; i < 3; i++ {
		dx, dy := t.dx[i]*t.sign, t.dy[i]*t.sign
		if dx > 0 || (dx == 0 && dy < 0) {
			t.adj[i] = 0
		} else {
			t.adj[i] = -1
		}
	}
	t.inv = 1 / float32(area)
	return t, true
}

func (t *triEdges) at(i int, px, py int64) int64 {
	return (t.x0[i]-px)*(t.y1[i]-py) - (t.y0[i]-py)*(t.x1[i]-px)
}

// start returns the oriented edge values at the centre of pixel (x, y) and the per-pixel x steps.
func (t *triEdges) start(x, y int) (e, step [3]int64) {
	px := int64(x)<<fixShift + 1<<(fixShift-1)
	py := int64(y)<<fixShift + 1<<(fixShift-1)
	for i := 0; i < 3; i++ {
		e[i] = t.at(i, px, py) * t.sign
		step[i] = t.dx[i] * t.sign
	}
	return
}

// inside reports whether the oriented edge values cover the pixel.
func (t *triEdges) inside(e *[3]int64) bool {
	return e[0]+t.adj[0] >= 0 && e[1]+t.adj[1] >= 0 && e[2]+t.adj[2] >= 0
}

// span returns the first and last x in [x0, x1] covered on the row whose oriented edge values at
// x0 are e (from start), stepping the values to x = first. ok is false if the row is not covered.
// It is exact: the pixels in [first, last] are those inside reports covered.
func (t *triEdges) span(e *[3]int64, step [3]int64, x0, x1 int) (first, last int, ok bool) {
	lo, hi := int64(0), int64(x1-x0)
	for i := 0; i < 3; i++ {
		v, s := e[i]+t.adj[i], step[i]
		switch {
		case s > 0:
			if v < 0 {
				lo = max(lo, (-v+s-1)/s)
			}
		case s < 0:
			if v < 0 {
				return 0, 0, false
			}
			hi = min(hi, v/-s)
		default:
			if v < 0 {
				return 0, 0, false
			}
		}
	}
	if lo > hi {
		return 0, 0, false
	}
	for i := 0; i < 3; i++ {
		e[i] += lo * step[i]
	}
	return x0 + int(lo), x0 + int(hi), true
}

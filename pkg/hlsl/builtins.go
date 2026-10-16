package hlsl

import (
	"math"
)

func (c *compiler) call(e *CallExpr) *cval {
	if e.Ctor != nil {
		return c.construct(e)
	}
	if cands := c.funcs[e.Name]; len(cands) > 0 {
		return c.userCall(e, cands)
	}
	return c.intrinsic(e)
}

// conversionCost ranks how well an argument type matches a parameter type (-1 = impossible).
func conversionCost(from, to *Type) int {
	switch {
	case sameShape(from, to):
		return 0
	case from.Base == BStruct || to.Base == BStruct || from.Base == BSampler || to.Base == BSampler || from.IsArray() || to.IsArray():
		if from.Base == to.Base && from.Struct == to.Struct && from.Sampler == to.Sampler && from.ArrayLen == to.ArrayLen {
			return 0
		}
		return -1
	case from.Rows == to.Rows && from.Cols == to.Cols:
		return 1 // base type conversion
	case from.IsScalar():
		return 2 // broadcast
	case from.Rows == 1 && to.Rows == 1 && from.Cols > to.Cols:
		return 3 // vector truncation
	case from.Rows >= to.Rows && from.Cols >= to.Cols && to.Rows > 1:
		return 3 // matrix truncation
	}
	return -1
}

func (c *compiler) userCall(e *CallExpr, cands []*FuncDecl) *cval {
	args := make([]*cval, len(e.Args))
	for i, a := range e.Args {
		args[i] = c.expr(a)
	}
	var best *FuncDecl
	bestCost := -1
	for _, fd := range cands {
		if len(fd.Params) != len(args) {
			continue
		}
		cost := 0
		for i, prm := range fd.Params {
			k := conversionCost(args[i].t, prm.Type)
			if prm.Out && !sameShape(args[i].t, prm.Type) {
				k = -1
			}
			if k < 0 {
				cost = -1
				break
			}
			cost += k
		}
		if cost >= 0 && (best == nil || cost < bestCost) {
			best, bestCost = fd, cost
		}
	}
	if best == nil {
		failf(e.Line, "no matching overload for %s with %d arguments", e.Name, len(args))
	}
	cf := c.function(best)
	type inArg struct {
		v    *cval
		off  int
		size int
	}
	var ins []inArg
	type outArg struct {
		st  func(m []float32, src int)
		off int
	}
	var outs []outArg
	for i, prm := range best.Params {
		if prm.In {
			v := c.convertTo(args[i], prm.Type, e.Line)
			ins = append(ins, inArg{v, cf.paramOff[i], prm.Type.Size()})
		}
		if prm.Out {
			if args[i].lv == nil {
				failf(e.Line, "argument %d of %s must be assignable (out parameter)", i+1, e.Name)
			}
			outs = append(outs, outArg{storeFn(args[i].lv, prm.Type.Size()), cf.paramOff[i]})
		}
	}
	body := cf.body
	ret := best.Ret
	if ret.Base == BVoid {
		return &cval{t: tyVoid, off: 0, eval: func(m []float32) {
			for _, a := range ins {
				a.v.eval(m)
			}
			for _, a := range ins {
				copy(m[a.off:a.off+a.size], m[a.v.off:a.v.off+a.size])
			}
			body(m)
			for _, o := range outs {
				o.st(m, o.off)
			}
		}}
	}
	size := ret.Size()
	tmp := c.temp(ret)
	retOff := cf.retOff
	return &cval{t: ret, off: tmp, eval: func(m []float32) {
		for _, a := range ins {
			a.v.eval(m)
		}
		for _, a := range ins {
			copy(m[a.off:a.off+a.size], m[a.v.off:a.v.off+a.size])
		}
		body(m)
		for _, o := range outs {
			o.st(m, o.off)
		}
		copy(m[tmp:tmp+size], m[retOff:retOff+size])
	}}
}

func (c *compiler) construct(e *CallExpr) *cval {
	t := e.Ctor
	if len(e.Args) == 1 {
		return c.convertTo(c.expr(e.Args[0]), t, e.Line)
	}
	var parts []*cval
	total := 0
	for _, a := range e.Args {
		v := c.expr(a)
		if !v.t.IsNumeric() {
			failf(e.Line, "constructor argument of type %s", v.t)
		}
		parts = append(parts, v)
		total += v.t.Size()
	}
	if total != t.Size() {
		failf(e.Line, "%s constructor needs %d components, got %d", t, t.Size(), total)
	}
	tmp := c.temp(t)
	conv := t.Base == BInt
	return &cval{t: t, off: tmp, eval: func(m []float32) {
		o := tmp
		for _, p := range parts {
			p.eval(m)
			n := p.t.Size()
			copy(m[o:o+n], m[p.off:p.off+n])
			if conv {
				for i := o; i < o+n; i++ {
					m[i] = float32(math.Trunc(float64(m[i])))
				}
			}
			o += n
		}
	}}
}

// floatShape converts all args to a common float shape (broadcasting scalars).
func (c *compiler) floatShape(args []*cval, line int) (*Type, []*cval) {
	rows, cols := args[0].t.Rows, args[0].t.Cols
	shape := args[0].t
	for _, a := range args[1:] {
		rows, cols = binaryShape(shape, a.t, line)
		shape = &Type{Base: BFloat, Rows: rows, Cols: cols}
	}
	t := &Type{Base: BFloat, Rows: rows, Cols: cols}
	out := make([]*cval, len(args))
	for i, a := range args {
		out[i] = c.convertTo(a, t, line)
	}
	return t, out
}

func f32(x float64) float32 { return float32(x) }

var unaryFns = map[string]func(float32) float32{
	"abs":   func(x float32) float32 { return f32(math.Abs(float64(x))) },
	"sqrt":  func(x float32) float32 { return f32(math.Sqrt(float64(x))) },
	"rsqrt": func(x float32) float32 { return f32(1 / math.Sqrt(float64(x))) },
	"exp":   func(x float32) float32 { return f32(math.Exp(float64(x))) },
	"exp2":  func(x float32) float32 { return f32(math.Exp2(float64(x))) },
	"log":   func(x float32) float32 { return f32(math.Log(float64(x))) },
	"log2":  func(x float32) float32 { return f32(math.Log2(float64(x))) },
	"log10": func(x float32) float32 { return f32(math.Log10(float64(x))) },
	"floor": func(x float32) float32 { return f32(math.Floor(float64(x))) },
	"ceil":  func(x float32) float32 { return f32(math.Ceil(float64(x))) },
	"round": func(x float32) float32 { return f32(math.RoundToEven(float64(x))) },
	"trunc": func(x float32) float32 { return f32(math.Trunc(float64(x))) },
	"frac":  func(x float32) float32 { return x - f32(math.Floor(float64(x))) },
	"sin":   func(x float32) float32 { return f32(math.Sin(float64(x))) },
	"cos":   func(x float32) float32 { return f32(math.Cos(float64(x))) },
	"tan":   func(x float32) float32 { return f32(math.Tan(float64(x))) },
	"asin":  func(x float32) float32 { return f32(math.Asin(float64(x))) },
	"acos":  func(x float32) float32 { return f32(math.Acos(float64(x))) },
	"atan":  func(x float32) float32 { return f32(math.Atan(float64(x))) },
	"sign": func(x float32) float32 {
		switch {
		case x > 0:
			return 1
		case x < 0:
			return -1
		}
		return 0
	},
	"saturate": func(x float32) float32 {
		switch {
		case x < 0:
			return 0
		case x > 1:
			return 1
		}
		return x // NaN stays NaN in hardware; keep it simple
	},
}

var binaryFns = map[string]func(a, b float32) float32{
	"max":   func(a, b float32) float32 { return max(a, b) },
	"min":   func(a, b float32) float32 { return min(a, b) },
	"pow":   func(a, b float32) float32 { return f32(math.Pow(float64(a), float64(b))) },
	"step":  func(a, b float32) float32 { return b2f(b >= a) },
	"fmod":  func(a, b float32) float32 { return f32(math.Mod(float64(a), float64(b))) },
	"atan2": func(a, b float32) float32 { return f32(math.Atan2(float64(a), float64(b))) },
}

func (c *compiler) intrinsic(e *CallExpr) *cval {
	args := make([]*cval, len(e.Args))
	for i, a := range e.Args {
		args[i] = c.expr(a)
	}
	need := func(n int) {
		if len(args) != n {
			failf(e.Line, "%s takes %d arguments, got %d", e.Name, n, len(args))
		}
	}
	if f, ok := unaryFns[e.Name]; ok {
		need(1)
		t, a := c.floatShape(args, e.Line)
		n, tmp, ae, ao := t.Size(), c.temp(t), a[0].eval, a[0].off
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			ae(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = f(m[ao+i])
			}
		}}
	}
	if f, ok := binaryFns[e.Name]; ok {
		need(2)
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(t)
		xe, ye, xo, yo := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = f(m[xo+i], m[yo+i])
			}
		}}
	}
	switch e.Name {
	case "lerp", "clamp", "smoothstep":
		need(3)
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(t)
		ae, be, ce := a[0].eval, a[1].eval, a[2].eval
		ao, bo, co := a[0].off, a[1].off, a[2].off
		var f func(x, y, z float32) float32
		switch e.Name {
		case "lerp":
			f = func(x, y, s float32) float32 { return x + s*(y-x) }
		case "clamp":
			f = func(x, lo, hi float32) float32 { return min(max(x, lo), hi) }
		default:
			f = func(lo, hi, x float32) float32 {
				t := min(max((x-lo)/(hi-lo), 0), 1)
				return t * t * (3 - 2*t)
			}
		}
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			ae(m)
			be(m)
			ce(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = f(m[ao+i], m[bo+i], m[co+i])
			}
		}}
	case "dot":
		need(2)
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(tyFloat)
		xe, ye, xo, yo := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: tyFloat, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			var s float32
			for i := 0; i < n; i++ {
				s += m[xo+i] * m[yo+i]
			}
			m[tmp] = s
		}}
	case "length", "normalize":
		need(1)
		t, a := c.floatShape(args, e.Line)
		n, ae, ao := t.Size(), a[0].eval, a[0].off
		if e.Name == "length" {
			tmp := c.temp(tyFloat)
			return &cval{t: tyFloat, off: tmp, eval: func(m []float32) {
				ae(m)
				var s float32
				for i := 0; i < n; i++ {
					s += m[ao+i] * m[ao+i]
				}
				m[tmp] = f32(math.Sqrt(float64(s)))
			}}
		}
		tmp := c.temp(t)
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			ae(m)
			var s float32
			for i := 0; i < n; i++ {
				s += m[ao+i] * m[ao+i]
			}
			inv := float32(0) // normalize(0) yields 0 instead of NaN
			if s > 0 {
				inv = f32(1 / math.Sqrt(float64(s)))
			}
			for i := 0; i < n; i++ {
				m[tmp+i] = m[ao+i] * inv
			}
		}}
	case "distance":
		need(2)
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(tyFloat)
		xe, ye, xo, yo := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: tyFloat, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			var s float32
			for i := 0; i < n; i++ {
				d := m[xo+i] - m[yo+i]
				s += d * d
			}
			m[tmp] = f32(math.Sqrt(float64(s)))
		}}
	case "reflect":
		need(2) // i - 2*dot(n,i)*n
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(t)
		ie, ne, io, no := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			ie(m)
			ne(m)
			var d float32
			for i := 0; i < n; i++ {
				d += m[io+i] * m[no+i]
			}
			for i := 0; i < n; i++ {
				m[tmp+i] = m[io+i] - 2*d*m[no+i]
			}
		}}
	case "cross":
		need(2)
		v3 := vectorType(BFloat, 3)
		a, b := c.convertTo(args[0], v3, e.Line), c.convertTo(args[1], v3, e.Line)
		tmp := c.temp(v3)
		ae, be, ao, bo := a.eval, b.eval, a.off, b.off
		return &cval{t: v3, off: tmp, eval: func(m []float32) {
			ae(m)
			be(m)
			x0, y0, z0 := m[ao], m[ao+1], m[ao+2]
			x1, y1, z1 := m[bo], m[bo+1], m[bo+2]
			m[tmp], m[tmp+1], m[tmp+2] = y0*z1-z0*y1, z0*x1-x0*z1, x0*y1-y0*x1
		}}
	case "any", "all":
		need(1)
		n, tmp, ae, ao := args[0].t.Size(), c.temp(tyBool), args[0].eval, args[0].off
		all := e.Name == "all"
		return &cval{t: tyBool, off: tmp, eval: func(m []float32) {
			ae(m)
			r := all
			for i := 0; i < n; i++ {
				if all {
					r = r && m[ao+i] != 0
				} else {
					r = r || m[ao+i] != 0
				}
			}
			m[tmp] = b2f(r)
		}}
	case "mul":
		need(2)
		return c.mul(args[0], args[1], e.Line)
	case "tex2D", "texCUBE", "tex2Dlod", "tex2Dbias", "tex2Dproj", "tex2Dgrad", "texCUBElod", "texCUBEbias":
		if len(args) < 2 {
			failf(e.Line, "%s needs a sampler and coordinates", e.Name)
		}
		smp, coord := args[0], args[1]
		if smp.t.Base != BSampler {
			failf(e.Line, "%s: first argument must be a sampler", e.Name)
		}
		if k := smp.konst; k != nil {
			seen := false
			for _, x := range c.prog.TexSamplers {
				seen = seen || x == k
			}
			if !seen {
				c.prog.TexSamplers = append(c.prog.TexSamplers, k)
			}
		}
		t := vectorType(BFloat, 4)
		tmp := c.temp(t)
		se, ce, so, co, cn := smp.eval, coord.eval, smp.off, coord.off, coord.t.Size()
		flag := c.alloc(1)
		c.prog.TexCalls = append(c.prog.TexCalls, &TexCall{unitOff: so, coordOff: co, coordN: cn, flagOff: flag})
		cube := e.Name[3] == 'C'
		proj := e.Name == "tex2Dproj"
		prog := c.prog
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			se(m)
			ce(m)
			m[flag] = 1
			var r [4]float32
			if prog.Sampler != nil {
				coords := m[co : co+cn]
				if proj && cn == 4 && coords[3] != 0 {
					p := [4]float32{coords[0] / coords[3], coords[1] / coords[3], coords[2], 1}
					coords = p[:]
				}
				r = prog.Sampler(int(m[so]), coords, cube)
			} else {
				r = [4]float32{1, 1, 1, 1}
			}
			copy(m[tmp:tmp+4], r[:])
		}}
	}
	failf(e.Line, "unknown function %q", e.Name)
	return nil
}

// mul implements HLSL mul() for scalar/vector/matrix combinations (row-vector convention for
// vector*matrix, column vector for matrix*vector). Matrices are stored row-major in slots.
func (c *compiler) mul(x, y *cval, line int) *cval {
	switch {
	case x.t.IsScalar() || y.t.IsScalar():
		return c.arith("*", x, y, line)
	case x.t.IsVector() && y.t.IsVector():
		n := min(x.t.Cols, y.t.Cols)
		tmp := c.temp(tyFloat)
		xe, ye, xo, yo := x.eval, y.eval, x.off, y.off
		return &cval{t: tyFloat, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			var s float32
			for i := 0; i < n; i++ {
				s += m[xo+i] * m[yo+i]
			}
			m[tmp] = s
		}}
	case x.t.IsVector() && y.t.IsMatrix():
		// r[j] = sum_i v[i] * M[i][j]
		rows, cols := y.t.Rows, y.t.Cols
		if x.t.Cols != rows {
			failf(line, "mul(%s, %s): dimension mismatch", x.t, y.t)
		}
		t := vectorType(BFloat, cols)
		tmp := c.temp(t)
		xe, ye, xo, yo := x.eval, y.eval, x.off, y.off
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			for j := 0; j < cols; j++ {
				var s float32
				for i := 0; i < rows; i++ {
					s += m[xo+i] * m[yo+i*cols+j]
				}
				m[tmp+j] = s
			}
		}}
	case x.t.IsMatrix() && y.t.IsVector():
		rows, cols := x.t.Rows, x.t.Cols
		if y.t.Cols != cols {
			failf(line, "mul(%s, %s): dimension mismatch", x.t, y.t)
		}
		t := vectorType(BFloat, rows)
		tmp := c.temp(t)
		xe, ye, xo, yo := x.eval, y.eval, x.off, y.off
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			for i := 0; i < rows; i++ {
				var s float32
				for j := 0; j < cols; j++ {
					s += m[xo+i*cols+j] * m[yo+j]
				}
				m[tmp+i] = s
			}
		}}
	case x.t.IsMatrix() && y.t.IsMatrix():
		r, k, cc := x.t.Rows, x.t.Cols, y.t.Cols
		if y.t.Rows != k {
			failf(line, "mul(%s, %s): dimension mismatch", x.t, y.t)
		}
		t := &Type{Base: BFloat, Rows: r, Cols: cc}
		tmp := c.temp(t)
		xe, ye, xo, yo := x.eval, y.eval, x.off, y.off
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			for i := 0; i < r; i++ {
				for j := 0; j < cc; j++ {
					var s float32
					for q := 0; q < k; q++ {
						s += m[xo+i*k+q] * m[yo+q*cc+j]
					}
					m[tmp+i*cc+j] = s
				}
			}
		}}
	}
	failf(line, "mul(%s, %s) is not supported", x.t, y.t)
	return nil
}

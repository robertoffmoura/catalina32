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
	if c.cur != nil && cf.impure {
		c.cur.impure = true
	}
	if len(best.Params) == 0 && best.Ret.Base != BVoid && !cf.impure {
		// Depends only on uniforms and literals: evaluate once per constant load.
		if !cf.memo {
			cf.memo = true
			c.prog.pre = append(c.prog.pre, cf.body)
		}
		return &cval{t: best.Ret, off: cf.retOff, eval: noop} // written only by Program.pre
	}
	type inArg struct {
		v    *cval
		off  int
		size int
	}
	var ins []inArg
	type outArg struct {
		st  func(x *ctx, src int)
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
	call := func(x *ctx) {
		for _, a := range ins {
			a.v.eval(x)
		}
		for _, a := range ins {
			copy(x.m[a.off:a.off+a.size], x.m[a.v.off:a.v.off+a.size])
		}
		body(x)
		for _, o := range outs {
			o.st(x, o.off)
		}
	}
	if ret.Base == BVoid {
		return &cval{t: tyVoid, off: 0, eval: call}
	}
	size := ret.Size()
	tmp := c.temp(ret)
	retOff := cf.retOff
	return &cval{t: ret, off: tmp, eval: func(x *ctx) {
		call(x)
		copy(x.m[tmp:tmp+size], x.m[retOff:retOff+size])
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
	return &cval{t: t, off: tmp, eval: func(x *ctx) {
		o := tmp
		for _, p := range parts {
			p.eval(x)
			n := p.t.Size()
			copy(x.m[o:o+n], x.m[p.off:p.off+n])
			if conv {
				for i := o; i < o+n; i++ {
					d := &x.m[i]
					for l := range d {
						d[l] = float32(math.Trunc(float64(d[l])))
					}
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

// laneOp1 computes d = f(a) in every lane.
type laneOp1 func(d, a *Lane)

func perLane1(f func(float32) float32) laneOp1 {
	return func(d, a *Lane) {
		for l := range d {
			d[l] = f(a[l])
		}
	}
}

func perLane2(f func(a, b float32) float32) laneOp2 {
	return func(d, a, b *Lane) {
		for l := range d {
			d[l] = f(a[l], b[l])
		}
	}
}

var unaryFns = map[string]laneOp1{
	"abs": func(d, a *Lane) {
		for l := range d {
			d[l] = f32(math.Abs(float64(a[l])))
		}
	},
	"sqrt": func(d, a *Lane) {
		for l := range d {
			d[l] = f32(math.Sqrt(float64(a[l])))
		}
	},
	"rsqrt": func(d, a *Lane) {
		for l := range d {
			d[l] = f32(1 / math.Sqrt(float64(a[l])))
		}
	},
	"exp":   perLane1(func(x float32) float32 { return f32(math.Exp(float64(x))) }),
	"exp2":  perLane1(func(x float32) float32 { return f32(math.Exp2(float64(x))) }),
	"log":   perLane1(func(x float32) float32 { return f32(math.Log(float64(x))) }),
	"log2":  perLane1(func(x float32) float32 { return f32(math.Log2(float64(x))) }),
	"log10": perLane1(func(x float32) float32 { return f32(math.Log10(float64(x))) }),
	"floor": func(d, a *Lane) {
		for l := range d {
			d[l] = f32(math.Floor(float64(a[l])))
		}
	},
	"ceil":  perLane1(func(x float32) float32 { return f32(math.Ceil(float64(x))) }),
	"round": perLane1(func(x float32) float32 { return f32(math.RoundToEven(float64(x))) }),
	"trunc": perLane1(func(x float32) float32 { return f32(math.Trunc(float64(x))) }),
	"frac": func(d, a *Lane) {
		for l := range d {
			d[l] = a[l] - f32(math.Floor(float64(a[l])))
		}
	},
	"sin":  perLane1(func(x float32) float32 { return f32(math.Sin(float64(x))) }),
	"cos":  perLane1(func(x float32) float32 { return f32(math.Cos(float64(x))) }),
	"tan":  perLane1(func(x float32) float32 { return f32(math.Tan(float64(x))) }),
	"asin": perLane1(func(x float32) float32 { return f32(math.Asin(float64(x))) }),
	"acos": perLane1(func(x float32) float32 { return f32(math.Acos(float64(x))) }),
	"atan": perLane1(func(x float32) float32 { return f32(math.Atan(float64(x))) }),
	"sign": perLane1(func(x float32) float32 {
		switch {
		case x > 0:
			return 1
		case x < 0:
			return -1
		}
		return 0
	}),
	"saturate": func(d, a *Lane) {
		for l, x := range a {
			switch {
			case x < 0:
				x = 0
			case x > 1:
				x = 1
			}
			d[l] = x // NaN stays NaN in hardware; keep it simple
		}
	},
}

var binaryFns = map[string]laneOp2{
	"max": func(d, a, b *Lane) {
		for l := range d {
			d[l] = max(a[l], b[l])
		}
	},
	"min": func(d, a, b *Lane) {
		for l := range d {
			d[l] = min(a[l], b[l])
		}
	},
	"pow": perLane2(func(a, b float32) float32 { return f32(math.Pow(float64(a), float64(b))) }),
	"step": func(d, a, b *Lane) {
		for l := range d {
			d[l] = b2f(b[l] >= a[l])
		}
	},
	"fmod":  perLane2(func(a, b float32) float32 { return f32(math.Mod(float64(a), float64(b))) }),
	"atan2": perLane2(func(a, b float32) float32 { return f32(math.Atan2(float64(a), float64(b))) }),
}

// dotLanes is sum over i < n of m[a+i]*m[b+i], accumulated in component order.
func dotLanes(d *Lane, m []Lane, a, b, n int) {
	var s Lane
	for i := 0; i < n; i++ {
		x, y := &m[a+i], &m[b+i]
		for l := range s {
			s[l] += x[l] * y[l]
		}
	}
	*d = s
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
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			ae(x)
			for i := 0; i < n; i++ {
				f(&x.m[tmp+i], &x.m[ao+i])
			}
		}}
	}
	if f, ok := binaryFns[e.Name]; ok {
		need(2)
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(t)
		xe, ye, xo, yo := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x)
			for i := 0; i < n; i++ {
				f(&x.m[tmp+i], &x.m[xo+i], &x.m[yo+i])
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
		var f func(d, a, b, c *Lane)
		switch e.Name {
		case "lerp":
			f = func(d, x, y, s *Lane) {
				for l := range d {
					d[l] = x[l] + s[l]*(y[l]-x[l])
				}
			}
		case "clamp":
			f = func(d, x, lo, hi *Lane) {
				for l := range d {
					d[l] = min(max(x[l], lo[l]), hi[l])
				}
			}
		default:
			f = func(d, lo, hi, x *Lane) {
				for l := range d {
					t := min(max((x[l]-lo[l])/(hi[l]-lo[l]), 0), 1)
					d[l] = t * t * (3 - 2*t)
				}
			}
		}
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			ae(x)
			be(x)
			ce(x)
			for i := 0; i < n; i++ {
				f(&x.m[tmp+i], &x.m[ao+i], &x.m[bo+i], &x.m[co+i])
			}
		}}
	case "dot":
		need(2)
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(tyFloat)
		xe, ye, xo, yo := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: tyFloat, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x)
			dotLanes(&x.m[tmp], x.m, xo, yo, n)
		}}
	case "length", "normalize":
		need(1)
		t, a := c.floatShape(args, e.Line)
		n, ae, ao := t.Size(), a[0].eval, a[0].off
		if e.Name == "length" {
			tmp := c.temp(tyFloat)
			return &cval{t: tyFloat, off: tmp, eval: func(x *ctx) {
				ae(x)
				d := &x.m[tmp]
				dotLanes(d, x.m, ao, ao, n)
				for l := range d {
					d[l] = f32(math.Sqrt(float64(d[l])))
				}
			}}
		}
		tmp := c.temp(t)
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			ae(x)
			var inv Lane
			dotLanes(&inv, x.m, ao, ao, n)
			for l, s := range inv {
				inv[l] = 0 // normalize(0) yields 0 instead of NaN
				if s > 0 {
					inv[l] = f32(1 / math.Sqrt(float64(s)))
				}
			}
			for i := 0; i < n; i++ {
				d, v := &x.m[tmp+i], &x.m[ao+i]
				for l := range d {
					d[l] = v[l] * inv[l]
				}
			}
		}}
	case "distance":
		need(2)
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(tyFloat)
		xe, ye, xo, yo := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: tyFloat, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x)
			var s Lane
			for i := 0; i < n; i++ {
				p, q := &x.m[xo+i], &x.m[yo+i]
				for l := range s {
					d := p[l] - q[l]
					s[l] += d * d
				}
			}
			d := &x.m[tmp]
			for l := range d {
				d[l] = f32(math.Sqrt(float64(s[l])))
			}
		}}
	case "reflect":
		need(2) // i - 2*dot(n,i)*n
		t, a := c.floatShape(args, e.Line)
		n, tmp := t.Size(), c.temp(t)
		ie, ne, io, no := a[0].eval, a[1].eval, a[0].off, a[1].off
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			ie(x)
			ne(x)
			var dt Lane
			dotLanes(&dt, x.m, io, no, n)
			for i := 0; i < n; i++ {
				d, iv, nv := &x.m[tmp+i], &x.m[io+i], &x.m[no+i]
				for l := range d {
					d[l] = iv[l] - 2*dt[l]*nv[l]
				}
			}
		}}
	case "cross":
		need(2)
		v3 := vectorType(BFloat, 3)
		a, b := c.convertTo(args[0], v3, e.Line), c.convertTo(args[1], v3, e.Line)
		tmp := c.temp(v3)
		ae, be, ao, bo := a.eval, b.eval, a.off, b.off
		return &cval{t: v3, off: tmp, eval: func(x *ctx) {
			ae(x)
			be(x)
			m := x.m
			for l := 0; l < Lanes; l++ {
				x0, y0, z0 := m[ao][l], m[ao+1][l], m[ao+2][l]
				x1, y1, z1 := m[bo][l], m[bo+1][l], m[bo+2][l]
				m[tmp][l], m[tmp+1][l], m[tmp+2][l] = y0*z1-z0*y1, z0*x1-x0*z1, x0*y1-y0*x1
			}
		}}
	case "any", "all":
		need(1)
		n, tmp, ae, ao := args[0].t.Size(), c.temp(tyBool), args[0].eval, args[0].off
		all := e.Name == "all"
		return &cval{t: tyBool, off: tmp, eval: func(x *ctx) {
			ae(x)
			d := &x.m[tmp]
			for l := range d {
				r := all
				for i := 0; i < n; i++ {
					if all {
						r = r && x.m[ao+i][l] != 0
					} else {
						r = r || x.m[ao+i][l] != 0
					}
				}
				d[l] = b2f(r)
			}
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
		if c.cur != nil {
			c.cur.impure = true
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
		for i := 0; i < 4; i++ {
			c.prog.lits = append(c.prog.lits, literal{tmp + i, 1}) // white without a Sampler
		}
		se, ce, so, co, cn := smp.eval, coord.eval, smp.off, coord.off, coord.t.Size()
		flag := c.alloc(1)
		c.prog.TexCalls = append(c.prog.TexCalls, &TexCall{unitOff: so, coordOff: co, coordN: cn, flagOff: flag})
		cube := e.Name[3] == 'C'
		proj := e.Name == "tex2Dproj"
		prog := c.prog
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			se(x)
			ce(x)
			m := x.m
			f := &m[flag]
			for l := range f {
				if x.mask>>l&1 != 0 {
					f[l] = 1
				}
			}
			if prog.Sampler == nil {
				return
			}
			for l := 0; l < Lanes; l++ {
				var p [4]float32
				for i := 0; i < cn && i < 4; i++ {
					p[i] = m[co+i][l]
				}
				coords := p[:cn]
				if proj && cn == 4 && p[3] != 0 {
					coords = []float32{p[0] / p[3], p[1] / p[3], p[2], 1}
				}
				r := prog.Sampler(int(m[so][l]), coords, cube)
				for i := 0; i < 4; i++ {
					m[tmp+i][l] = r[i]
				}
			}
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
		return &cval{t: tyFloat, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x)
			dotLanes(&x.m[tmp], x.m, xo, yo, n)
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
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x)
			m := x.m
			for j := 0; j < cols; j++ {
				var s Lane
				for i := 0; i < rows; i++ {
					v, a := &m[xo+i], &m[yo+i*cols+j]
					for l := range s {
						s[l] += v[l] * a[l]
					}
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
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x)
			for i := 0; i < rows; i++ {
				dotLanes(&x.m[tmp+i], x.m, xo+i*cols, yo, cols)
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
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x)
			m := x.m
			for i := 0; i < r; i++ {
				for j := 0; j < cc; j++ {
					var s Lane
					for q := 0; q < k; q++ {
						a, b := &m[xo+i*k+q], &m[yo+q*cc+j]
						for l := range s {
							s[l] += a[l] * b[l]
						}
					}
					m[tmp+i*cc+j] = s
				}
			}
		}}
	}
	failf(line, "mul(%s, %s) is not supported", x.t, y.t)
	return nil
}

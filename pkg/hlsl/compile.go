package hlsl

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Program is one compiled entry point (vertex or pixel program).
type Program struct {
	Entry     string
	Constants []*Constant // uniforms reachable from the entry point, sorted by register
	Inputs    []*Varying  // entry parameters (flattened struct fields) by semantic
	Outputs   []*Varying  // return value (flattened struct fields) by semantic
	MemSize   int         // float slots per invocation (see NewState)

	run    evalFn
	retOff int
	inits  []evalFn // static/global initialisers, run at the start of each Run
	pre    []evalFn // uniform-only function bodies, run after each constant load
	lits   []literal
	consts []*Constant
	// Sampler, if set, performs texture lookups (otherwise they return white). It is shared by
	// all States of the program, so it must not be set while States run concurrently.
	Sampler func(unit int, coord []float32, cube bool) [4]float32
	// TexSamplers lists the sampler uniforms passed to texture lookups, in code order.
	TexSamplers []*Constant
	// TexCalls lists every texture lookup site in code order (see State.TexCoord).
	TexCalls []*TexCall
}

// Register sets, as in D3DXREGISTER_SET.
const (
	RegBool    = 0
	RegInt4    = 1
	RegFloat4  = 2
	RegSampler = 3
)

// Constant describes a uniform as reported by ID3DXConstantTable.
type Constant struct {
	Name          string
	Type          *Type
	RegisterSet   int
	RegisterIndex int
	RegisterCount int
	Explicit      bool // register(...) given in source
	off           int  // evaluator slot
}

// literal is a slot holding a constant, filled once per State (no operation writes it).
type literal struct {
	off int
	v   float32
}

// TexCall is one texture lookup site: its sampler unit, coordinate and executed-flag slots.
type TexCall struct {
	unitOff, coordOff, coordN, flagOff int
}

// Varying is one input or output element of the entry point.
type Varying struct {
	Name       string // struct field (or parameter) name
	Semantic   string // upper-case, without index, e.g. "TEXCOORD"
	Index      int    // semantic index (TEXCOORD2 -> 2)
	Type       *Type
	Offset     int // evaluator slot
	Components int
}

// Compile type-checks file and compiles entry for the given profile ("vs_3_0", "ps_3_0", ...).
func Compile(file *File, entry string) (prog *Program, err error) {
	defer func() {
		if r := recover(); r != nil {
			ce, ok := r.(compileError)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("%s", ce.msg)
		}
	}()
	c := &compiler{file: file, funcs: map[string][]*FuncDecl{}, globals: map[string]*gvar{}, compiled: map[*FuncDecl]*cfunc{}}
	for _, f := range file.Funcs {
		c.funcs[f.Name] = append(c.funcs[f.Name], f)
	}
	for _, g := range file.Globals {
		c.globals[g.Name] = &gvar{decl: g}
	}
	ents := c.funcs[entry]
	if len(ents) != 1 {
		return nil, fmt.Errorf("entry point %q: found %d definitions", entry, len(ents))
	}
	ef := ents[0]
	prog = &Program{Entry: entry}
	c.prog = prog

	// Entry parameters are inputs; the return value (and out parameters) are outputs.
	fn := c.function(ef)
	params := fn.paramOff
	for i, prm := range ef.Params {
		if prm.In {
			c.varyings(&prog.Inputs, prm.Name, prm.Semantic, prm.Type, params[i])
		}
	}
	prog.retOff = fn.retOff
	if ef.Ret.Base != BVoid {
		c.varyings(&prog.Outputs, "", ef.Semantic, ef.Ret, fn.retOff)
	}
	for i, prm := range ef.Params {
		if prm.Out {
			c.varyings(&prog.Outputs, prm.Name, prm.Semantic, prm.Type, params[i])
		}
	}
	body := fn.body
	prog.run = body
	c.assignRegisters()
	prog.MemSize = c.mem
	return prog, nil
}

type compileError struct{ msg string }

func failf(line int, format string, args ...any) {
	panic(compileError{fmt.Sprintf("line %d: ", line) + fmt.Sprintf(format, args...)})
}

type gvar struct {
	decl     *VarDecl
	off      int
	alloced  bool
	constant *Constant
}

type cfunc struct {
	decl     *FuncDecl
	paramOff []int
	retOff   int
	body     evalFn
	locals   []map[string]*local
	impure   bool // reads/writes non-uniform globals, samples textures, or calls impure code
	memo     bool // pure, parameterless: body runs once per constant load (Program.pre)
}

type local struct {
	t   *Type
	off int
}

type compiler struct {
	file     *File
	prog     *Program
	funcs    map[string][]*FuncDecl
	globals  map[string]*gvar
	compiled map[*FuncDecl]*cfunc
	mem      int
	cur      *cfunc // function being compiled
	scopes   []map[string]*local
	inProg   map[*FuncDecl]bool
}

func (c *compiler) alloc(n int) int {
	off := c.mem
	c.mem += n
	return off
}

// varyings flattens an entry parameter/return type into semantic-tagged elements.
func (c *compiler) varyings(list *[]*Varying, name, sem string, t *Type, off int) {
	if t.Base == BStruct && !t.IsArray() {
		o := off
		for _, f := range t.Struct.Fields {
			c.varyings(list, f.Name, f.Semantic, f.Type, o)
			o += f.Type.Size()
		}
		return
	}
	if t.IsArray() {
		for i := 0; i < t.ArrayLen; i++ {
			s, idx := splitSemantic(sem)
			c.varyings(list, fmt.Sprintf("%s[%d]", name, i), fmt.Sprintf("%s%d", s, idx+i), t.Elem, off+i*t.Elem.Size())
		}
		return
	}
	s, idx := splitSemantic(sem)
	*list = append(*list, &Varying{Name: name, Semantic: s, Index: idx, Type: t, Offset: off, Components: t.Size()})
}

// splitSemantic turns "TEXCOORD2_centroid" into ("TEXCOORD", 2).
func splitSemantic(sem string) (string, int) {
	sem = strings.ToUpper(sem)
	if i := strings.IndexByte(sem, '_'); i > 0 {
		sem = sem[:i]
	}
	j := len(sem)
	for j > 0 && sem[j-1] >= '0' && sem[j-1] <= '9' {
		j--
	}
	idx := 0
	fmt.Sscan(sem[j:], &idx)
	return sem[:j], idx
}

// global resolves a global variable, allocating it (and its constant-table entry) on first use.
func (c *compiler) global(name string, line int) *gvar {
	g := c.globals[name]
	if g == nil {
		return nil
	}
	if !g.alloced {
		g.alloced = true
		g.off = c.alloc(g.decl.Type.Size())
		d := g.decl
		if d.Uniform || d.Type.Base == BSampler || (d.Type.IsArray() && d.Type.Elem.Base == BSampler) {
			k := &Constant{Name: d.Name, Type: d.Type, off: g.off, RegisterSet: RegFloat4}
			if d.Type.Base == BSampler {
				k.RegisterSet = RegSampler
			}
			k.RegisterCount = registerCount(d.Type)
			if cls, idx, ok := registerIndex(d.Register); ok {
				k.Explicit = true
				k.RegisterIndex = idx
				if cls == 's' {
					k.RegisterSet = RegSampler
				}
			}
			g.constant = k
			c.prog.consts = append(c.prog.consts, k)
		} else if d.Init != nil {
			init := c.convertTo(c.expr(d.Init), d.Type, line)
			off := g.off
			size := d.Type.Size()
			c.prog.inits = append(c.prog.inits, func(x *ctx) {
				init.eval(x)
				copy(x.m[off:off+size], x.m[init.off:init.off+size])
			})
		}
	}
	return g
}

// registerCount is the number of constant registers a uniform occupies (column-major packing).
func registerCount(t *Type) int {
	switch {
	case t.IsArray():
		return t.ArrayLen * registerCount(t.Elem)
	case t.Base == BSampler:
		return 1
	case t.Base == BStruct:
		n := 0
		for _, f := range t.Struct.Fields {
			n += registerCount(f.Type)
		}
		return n
	case t.Rows > 1:
		return t.Cols
	}
	return 1
}

// assignRegisters gives every reachable uniform without register(...) a free register range,
// in declaration order, avoiding all explicitly bound ranges of the file.
func (c *compiler) assignRegisters() {
	used := map[int]map[int]bool{RegFloat4: {}, RegSampler: {}}
	for _, g := range c.file.Globals {
		if cls, idx, ok := registerIndex(g.Register); ok {
			set := RegFloat4
			if cls == 's' {
				set = RegSampler
			}
			for i := 0; i < registerCount(g.Type); i++ {
				used[set][idx+i] = true
			}
		}
	}
	order := map[string]int{}
	for i, g := range c.file.Globals {
		order[g.Name] = i
	}
	ks := c.prog.consts
	sort.SliceStable(ks, func(i, j int) bool { return order[ks[i].Name] < order[ks[j].Name] })
	for _, k := range ks {
		if k.Explicit {
			continue
		}
		start := 0
		for {
			free := true
			for i := 0; i < k.RegisterCount; i++ {
				if used[k.RegisterSet][start+i] {
					free = false
					start += i + 1
					break
				}
			}
			if free {
				break
			}
		}
		k.RegisterIndex = start
		for i := 0; i < k.RegisterCount; i++ {
			used[k.RegisterSet][start+i] = true
		}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].RegisterSet != ks[j].RegisterSet {
			return ks[i].RegisterSet > ks[j].RegisterSet // float constants first, then samplers
		}
		return ks[i].RegisterIndex < ks[j].RegisterIndex
	})
	c.prog.Constants = ks
}

// ---------------------------------------------------------------------------------------------
// Functions

func (c *compiler) function(fd *FuncDecl) *cfunc {
	if cf := c.compiled[fd]; cf != nil {
		if cf.body == nil {
			failf(fd.Line, "recursive call to %s", fd.Name)
		}
		return cf
	}
	cf := &cfunc{decl: fd}
	c.compiled[fd] = cf
	cf.paramOff = make([]int, len(fd.Params))
	scope := map[string]*local{}
	for i, prm := range fd.Params {
		cf.paramOff[i] = c.alloc(prm.Type.Size())
		scope[prm.Name] = &local{t: prm.Type, off: cf.paramOff[i]}
	}
	if fd.Ret.Base != BVoid {
		cf.retOff = c.alloc(fd.Ret.Size())
	}
	savedCur, savedScopes := c.cur, c.scopes
	c.cur, c.scopes = cf, []map[string]*local{scope}
	body := c.block(fd.Body)
	c.cur, c.scopes = savedCur, savedScopes
	cf.body = func(x *ctx) {
		if x.mask == 0 {
			return
		}
		mask, retd := x.mask, x.retd
		x.retd = 0
		body(x)
		x.mask, x.retd = mask, retd
	}
	return cf
}

// A block stops once none of its lanes is still executing (they all returned).
func (c *compiler) block(b *BlockStmt) evalFn {
	c.scopes = append(c.scopes, map[string]*local{})
	var list []evalFn
	for _, s := range b.List {
		list = append(list, c.stmt(s))
	}
	c.scopes = c.scopes[:len(c.scopes)-1]
	return func(x *ctx) {
		for _, s := range list {
			s(x)
			if x.mask == 0 {
				return
			}
		}
	}
}

func (c *compiler) stmt(s Stmt) evalFn {
	switch s := s.(type) {
	case *BlockStmt:
		return c.block(s)
	case *DeclStmt:
		var fns []evalFn
		for _, v := range s.Vars {
			off := c.alloc(v.Type.Size())
			if v.Init != nil {
				var init *cval
				if il, ok := v.Init.(*InitListExpr); ok {
					init = c.initList(il, v.Type)
				} else {
					init = c.convertTo(c.expr(v.Init), v.Type, s.Line)
				}
				size := v.Type.Size()
				fns = append(fns, func(x *ctx) {
					init.eval(x)
					storeMasked(x.m, off, init.off, size, x.mask)
				})
			}
			c.scopes[len(c.scopes)-1][v.Name] = &local{t: v.Type, off: off}
		}
		return func(x *ctx) {
			for _, f := range fns {
				f(x)
			}
		}
	case *ExprStmt:
		return c.expr(s.X).eval
	case *IfStmt:
		cond := c.convertTo(c.expr(s.Cond), tyBool, s.Line)
		then := c.stmt(s.Then)
		var els evalFn
		if s.Else != nil {
			els = c.stmt(s.Else)
		}
		co := cond.off
		return func(x *ctx) {
			cond.eval(x)
			t := laneBits(&x.m[co])
			mask := x.mask
			if tm := mask & t; tm != 0 {
				x.mask = tm
				then(x)
			}
			if em := mask &^ t; em != 0 && els != nil {
				x.mask = em
				els(x)
			}
			x.mask = mask &^ x.retd
		}
	case *ReturnStmt:
		ret := c.cur.decl.Ret
		if s.X == nil {
			return func(x *ctx) {
				x.retd |= x.mask
				x.mask = 0
			}
		}
		v := c.convertTo(c.expr(s.X), ret, s.Line)
		off, size := c.cur.retOff, ret.Size()
		return func(x *ctx) {
			v.eval(x)
			storeMasked(x.m, off, v.off, size, x.mask)
			x.retd |= x.mask
			x.mask = 0
		}
	}
	failf(s.stmtLine(), "unsupported statement %T", s)
	return nil
}

// ---------------------------------------------------------------------------------------------
// Expressions

// cval is a compiled expression: after eval(x), its value is in slots off : off+t.Size()
// (contiguous) of every lane. Expressions compute all lanes; only stores are masked.
// Lvalues additionally carry store information.
type cval struct {
	t     *Type
	eval  evalFn // never nil; no-op for values already in storage
	off   int
	lv    *lvalue
	konst *Constant // set when the value is a uniform read directly
}

// lvalue describes an assignable location: slot base, plus (when dyn is set) a per-lane offset
// that dyn computes into slot dynOff (dynamic array indices), and an optional component map
// (swizzle) relative to that.
type lvalue struct {
	base   int
	dyn    evalFn
	dynOff int
	comps  []int
}

func (c *compiler) temp(t *Type) int { return c.alloc(t.Size()) }

// literal allocates a slot holding the constant v.
func (c *compiler) literal(t *Type, v float32) *cval {
	off := c.temp(t)
	c.prog.lits = append(c.prog.lits, literal{off, v})
	return &cval{t: t, off: off, eval: noop}
}

func (c *compiler) lookup(name string, line int) *cval {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if l := c.scopes[i][name]; l != nil {
			return &cval{t: l.t, eval: noop, off: l.off, lv: &lvalue{base: l.off}}
		}
	}
	if g := c.global(name, line); g != nil {
		v := &cval{t: g.decl.Type, eval: noop, off: g.off, konst: g.constant}
		if g.constant == nil {
			v.lv = &lvalue{base: g.off}
			if c.cur != nil {
				c.cur.impure = true
			}
		}
		return v
	}
	failf(line, "undefined identifier %q", name)
	return nil
}

func (c *compiler) expr(e Expr) *cval {
	switch e := e.(type) {
	case *NumberExpr:
		if e.IsInt {
			return c.literal(tyInt, float32(e.Value))
		}
		return c.literal(tyFloat, float32(e.Value))
	case *BoolExpr:
		return c.literal(tyBool, b2f(e.Value))
	case *IdentExpr:
		return c.lookup(e.Name, e.Line)
	case *MemberExpr:
		return c.member(e)
	case *IndexExpr:
		return c.index(e)
	case *UnaryExpr:
		return c.unary(e)
	case *BinaryExpr:
		return c.binaryExpr(e)
	case *AssignExpr:
		return c.assign(e)
	case *CastExpr:
		return c.cast(c.expr(e.X), e.To, e.Line)
	case *CallExpr:
		return c.call(e)
	case *CondExpr:
		return c.condExpr(e)
	case *InitListExpr:
		failf(e.Line, "initializer list outside a declaration")
	}
	failf(e.line(), "unsupported expression %T", e)
	return nil
}

var swizzleIndex = map[byte]int{'x': 0, 'y': 1, 'z': 2, 'w': 3, 'r': 0, 'g': 1, 'b': 2, 'a': 3}

func (c *compiler) member(e *MemberExpr) *cval {
	x := c.expr(e.X)
	if x.t.Base == BStruct && !x.t.IsArray() {
		off := 0
		var f *Field
		for _, ff := range x.t.Struct.Fields {
			if ff.Name == e.Name {
				f = ff
				break
			}
			off += ff.Type.Size()
		}
		if f == nil {
			failf(e.Line, "%s has no field %q", x.t, e.Name)
		}
		return c.subvalue(x, f.Type, off, nil)
	}
	if !x.t.IsNumeric() || x.t.IsMatrix() {
		failf(e.Line, "cannot select %q from %s", e.Name, x.t)
	}
	if len(e.Name) > 4 {
		failf(e.Line, "bad swizzle %q", e.Name)
	}
	comps := make([]int, len(e.Name))
	for i := 0; i < len(e.Name); i++ {
		k, ok := swizzleIndex[e.Name[i]]
		if !ok || k >= x.t.Cols {
			failf(e.Line, "bad swizzle %q on %s", e.Name, x.t)
		}
		comps[i] = k
	}
	t := vectorType(x.t.Base, len(comps))
	if len(comps) == 1 {
		t = scalarType(x.t.Base)
	}
	return c.subvalue(x, t, 0, comps)
}

// subvalue selects part of x: either a contiguous field at offset off, or components comps.
func (c *compiler) subvalue(x *cval, t *Type, off int, comps []int) *cval {
	size := t.Size()
	// Fully static location (variable or field of one): no copy needed for contiguous parts.
	if x.lv != nil && x.lv.dyn == nil && x.lv.comps == nil && comps == nil {
		base := x.lv.base + off
		return &cval{t: t, eval: x.eval, off: base, lv: &lvalue{base: base}}
	}
	if x.lv == nil && comps == nil {
		// constant/uniform or temporary: contiguous slice of x
		base := x.off + off
		return &cval{t: t, eval: x.eval, off: base}
	}
	// Copy into a temporary.
	tmp := c.temp(t)
	var lv *lvalue
	if x.lv != nil {
		lv = &lvalue{base: x.lv.base + off, dyn: x.lv.dyn, dynOff: x.lv.dynOff}
		if x.lv.comps != nil {
			// swizzle of a swizzle
			if comps == nil {
				failf(0, "field access through a swizzle")
			}
			cc := make([]int, len(comps))
			for i, k := range comps {
				cc[i] = x.lv.comps[k]
			}
			lv.comps = cc
		} else {
			lv.comps = comps
		}
	}
	src := x.off + off
	ev := x.eval
	if comps == nil {
		return &cval{t: t, off: tmp, lv: lv, eval: func(x *ctx) {
			ev(x)
			copy(x.m[tmp:tmp+size], x.m[src:src+size])
		}}
	}
	return &cval{t: t, off: tmp, lv: lv, eval: func(x *ctx) {
		ev(x)
		for i, k := range comps {
			x.m[tmp+i] = x.m[src+k]
		}
	}}
}

func (c *compiler) index(e *IndexExpr) *cval {
	x := c.expr(e.X)
	idx := c.convertTo(c.expr(e.Index), tyInt, e.Line)
	var elem *Type
	var stride, n int
	switch {
	case x.t.IsArray():
		elem, stride, n = x.t.Elem, x.t.Elem.Size(), x.t.ArrayLen
	case x.t.IsMatrix():
		elem, stride, n = vectorType(x.t.Base, x.t.Cols), x.t.Cols, x.t.Rows
	case x.t.IsVector():
		elem, stride, n = scalarType(x.t.Base), 1, x.t.Cols
	default:
		failf(e.Line, "cannot index %s", x.t)
	}
	size := elem.Size()
	tmp := c.temp(elem)
	at := c.alloc(1) // per-lane slot offset of the element (clamped index * stride)
	xev, iev, xoff, ioff := x.eval, idx.eval, x.off, idx.off
	offsets := func(x *ctx) {
		iv, d := &x.m[ioff], &x.m[at]
		for l := range d {
			i := int(iv[l])
			if i < 0 {
				i = 0
			} else if i >= n {
				i = n - 1
			}
			d[l] = float32(i * stride)
		}
	}
	var lv *lvalue
	if x.lv != nil && x.lv.comps == nil {
		xd, xdOff := x.lv.dyn, x.lv.dynOff
		dynOff := c.alloc(1)
		lv = &lvalue{base: x.lv.base, dynOff: dynOff, dyn: func(x *ctx) {
			iev(x)
			offsets(x)
			d, a := &x.m[dynOff], &x.m[at]
			if xd == nil {
				*d = *a
				return
			}
			xd(x)
			p := &x.m[xdOff]
			for l := range d {
				d[l] = p[l] + a[l]
			}
		}}
	}
	return &cval{t: elem, off: tmp, lv: lv, eval: func(x *ctx) {
		xev(x)
		iev(x)
		offsets(x)
		m := x.m
		a := &m[at]
		for l := range a {
			src := xoff + int(a[l])
			for k := 0; k < size; k++ {
				m[tmp+k][l] = m[src+k][l]
			}
		}
	}}
}

// storeFn returns a function writing the value at slots src.. (type of size slots, already
// converted) to lvalue lv, in the executing lanes.
func storeFn(lv *lvalue, size int) func(x *ctx, src int) {
	base, dyn, dynOff, comps := lv.base, lv.dyn, lv.dynOff, lv.comps
	if dyn == nil {
		if comps == nil {
			return func(x *ctx, src int) { storeMasked(x.m, base, src, size, x.mask) }
		}
		return func(x *ctx, src int) {
			for i, k := range comps {
				storeMasked(x.m, base+k, src+i, 1, x.mask)
			}
		}
	}
	if comps == nil {
		comps = make([]int, size)
		for i := range comps {
			comps[i] = i
		}
	}
	return func(x *ctx, src int) {
		dyn(x)
		m := x.m
		d := &m[dynOff]
		for l := range d {
			if x.mask>>l&1 == 0 {
				continue
			}
			b := base + int(d[l])
			for i, k := range comps {
				m[b+k][l] = m[src+i][l]
			}
		}
	}
}

func (c *compiler) assign(e *AssignExpr) *cval {
	lhs := c.expr(e.LHS)
	if lhs.lv == nil {
		failf(e.Line, "cannot assign to this expression")
	}
	if len(lhs.lv.comps) > 0 {
		seen := map[int]bool{}
		for _, k := range lhs.lv.comps {
			if seen[k] {
				failf(e.Line, "repeated component in assignment target")
			}
			seen[k] = true
		}
	}
	var rhs *cval
	if e.Op == "=" {
		rhs = c.convertTo(c.expr(e.RHS), lhs.t, e.Line)
	} else {
		op := e.Op[:1]
		rhs = c.convertTo(c.arith(op, lhs, c.expr(e.RHS), e.Line), lhs.t, e.Line)
	}
	st := storeFn(lhs.lv, lhs.t.Size())
	rev, roff := rhs.eval, rhs.off
	return &cval{t: lhs.t, off: roff, eval: func(x *ctx) {
		rev(x)
		st(x, roff)
	}}
}

func (c *compiler) unary(e *UnaryExpr) *cval {
	x := c.expr(e.X)
	switch e.Op {
	case "+":
		return x
	case "-":
		if !x.t.IsNumeric() {
			failf(e.Line, "cannot negate %s", x.t)
		}
		t := x.t
		if t.Base == BBool {
			t = &Type{Base: BInt, Rows: t.Rows, Cols: t.Cols}
		}
		n := t.Size()
		tmp := c.temp(t)
		xe, xo := x.eval, x.off
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			xe(x)
			for i := 0; i < n; i++ {
				d, a := &x.m[tmp+i], &x.m[xo+i]
				for l := range d {
					d[l] = -a[l]
				}
			}
		}}
	case "!":
		t := &Type{Base: BBool, Rows: x.t.Rows, Cols: x.t.Cols}
		n := t.Size()
		tmp := c.temp(t)
		xe, xo := x.eval, x.off
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			xe(x)
			for i := 0; i < n; i++ {
				d, a := &x.m[tmp+i], &x.m[xo+i]
				for l := range d {
					d[l] = b2f(a[l] == 0)
				}
			}
		}}
	}
	failf(e.Line, "unsupported unary operator %s", e.Op)
	return nil
}

func b2f(b bool) float32 {
	if b {
		return 1
	}
	return 0
}

func (c *compiler) binaryExpr(e *BinaryExpr) *cval {
	switch e.Op {
	case "&&", "||":
		x := c.convertTo(c.expr(e.X), tyBool, e.Line)
		y := c.convertTo(c.expr(e.Y), tyBool, e.Line)
		tmp := c.temp(tyBool)
		xe, ye, xo, yo := x.eval, y.eval, x.off, y.off
		and := e.Op == "&&"
		return &cval{t: tyBool, off: tmp, eval: func(x *ctx) {
			xe(x)
			ye(x) // HLSL evaluates both operands
			d, a, b := &x.m[tmp], &x.m[xo], &x.m[yo]
			for l := range d {
				if and {
					d[l] = b2f(a[l] != 0 && b[l] != 0)
				} else {
					d[l] = b2f(a[l] != 0 || b[l] != 0)
				}
			}
		}}
	case "+", "-", "*", "/", "%", "<", ">", "<=", ">=", "==", "!=":
		return c.arith(e.Op, c.expr(e.X), c.expr(e.Y), e.Line)
	}
	failf(e.Line, "unsupported operator %s", e.Op)
	return nil
}

// binaryShape returns the common shape of x and y: scalars broadcast, vectors truncate to the
// shorter length, matrices must match.
func binaryShape(x, y *Type, line int) (rows, cols int) {
	switch {
	case x.IsScalar():
		return y.Rows, y.Cols
	case y.IsScalar():
		return x.Rows, x.Cols
	case x.Rows == 1 && y.Rows == 1:
		return 1, min(x.Cols, y.Cols)
	case x.Rows == y.Rows && x.Cols == y.Cols:
		return x.Rows, x.Cols
	case x.Rows > 1 && y.Rows > 1:
		return min(x.Rows, y.Rows), min(x.Cols, y.Cols)
	}
	failf(line, "incompatible operands %s and %s", x, y)
	return 0, 0
}

func promote(a, b Base) Base {
	if a == BFloat || b == BFloat {
		return BFloat
	}
	return BInt
}

// laneOp2 computes d = f(a, b) in every lane.
type laneOp2 func(d, a, b *Lane)

func arithOp(op string, isInt bool) laneOp2 {
	switch op {
	case "+":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = a[l] + b[l]
			}
		}
	case "-":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = a[l] - b[l]
			}
		}
	case "*":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = a[l] * b[l]
			}
		}
	case "/":
		if isInt {
			return func(d, a, b *Lane) {
				for l := range d {
					if b[l] == 0 {
						d[l] = 0
					} else {
						d[l] = float32(int32(a[l]) / int32(b[l]))
					}
				}
			}
		}
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = a[l] / b[l]
			}
		}
	case "%":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = float32(math.Mod(float64(a[l]), float64(b[l])))
			}
		}
	case "<":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = b2f(a[l] < b[l])
			}
		}
	case ">":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = b2f(a[l] > b[l])
			}
		}
	case "<=":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = b2f(a[l] <= b[l])
			}
		}
	case ">=":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = b2f(a[l] >= b[l])
			}
		}
	case "==":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = b2f(a[l] == b[l])
			}
		}
	case "!=":
		return func(d, a, b *Lane) {
			for l := range d {
				d[l] = b2f(a[l] != b[l])
			}
		}
	}
	return nil
}

func (c *compiler) arith(op string, x, y *cval, line int) *cval {
	if !x.t.IsNumeric() || !y.t.IsNumeric() {
		failf(line, "operator %s needs numeric operands, got %s and %s", op, x.t, y.t)
	}
	rows, cols := binaryShape(x.t, y.t, line)
	base := promote(x.t.Base, y.t.Base)
	shape := &Type{Base: base, Rows: rows, Cols: cols}
	x = c.convertTo(x, shape, line)
	y = c.convertTo(y, shape, line)
	rt := shape
	switch op {
	case "<", ">", "<=", ">=", "==", "!=":
		rt = &Type{Base: BBool, Rows: rows, Cols: cols}
	}
	n := shape.Size()
	tmp := c.temp(rt)
	xe, ye, xo, yo := x.eval, y.eval, x.off, y.off
	f := arithOp(op, base == BInt)
	return &cval{t: rt, off: tmp, eval: func(x *ctx) {
		xe(x)
		ye(x)
		m := x.m
		for i := 0; i < n; i++ {
			f(&m[tmp+i], &m[xo+i], &m[yo+i])
		}
	}}
}

// condExpr evaluates each side for the executing lanes that select it.
func (c *compiler) condExpr(e *CondExpr) *cval {
	cond := c.convertTo(c.expr(e.Cond), tyBool, e.Line)
	x, y := c.expr(e.X), c.expr(e.Y)
	rows, cols := binaryShape(x.t, y.t, e.Line)
	t := &Type{Base: promote(x.t.Base, y.t.Base), Rows: rows, Cols: cols}
	x, y = c.convertTo(x, t, e.Line), c.convertTo(y, t, e.Line)
	n := t.Size()
	tmp := c.temp(t)
	ce, xe, ye, co, xo, yo := cond.eval, x.eval, y.eval, cond.off, x.off, y.off
	return &cval{t: t, off: tmp, eval: func(x *ctx) {
		ce(x)
		sel := laneBits(&x.m[co])
		mask := x.mask
		if mask&sel != 0 {
			x.mask = mask & sel
			xe(x)
		}
		if mask&^sel != 0 {
			x.mask = mask &^ sel
			ye(x)
		}
		x.mask = mask
		m := x.m
		for i := 0; i < n; i++ {
			d, a, b := &m[tmp+i], &m[xo+i], &m[yo+i]
			for l := range d {
				if sel>>l&1 != 0 {
					d[l] = a[l]
				} else {
					d[l] = b[l]
				}
			}
		}
	}}
}

// ---------------------------------------------------------------------------------------------
// Conversions

func sameShape(a, b *Type) bool {
	return a.Base == b.Base && a.Rows == b.Rows && a.Cols == b.Cols && a.ArrayLen == b.ArrayLen &&
		a.Struct == b.Struct && (a.Elem == nil) == (b.Elem == nil) && (a.Elem == nil || sameShape(a.Elem, b.Elem))
}

// convertTo applies HLSL implicit conversion rules: identity, scalar broadcast, vector/matrix
// truncation, numeric base conversion (float->int truncates toward zero, ->bool tests != 0).
func (c *compiler) convertTo(x *cval, t *Type, line int) *cval {
	if sameShape(x.t, t) {
		return x
	}
	if !x.t.IsNumeric() || !t.IsNumeric() {
		if x.t.Base == BStruct && t.Base == BStruct && x.t.Struct == t.Struct && !x.t.IsArray() && !t.IsArray() {
			return x
		}
		if x.t.Base == BSampler && t.Base == BSampler {
			return x
		}
		failf(line, "cannot convert %s to %s", x.t, t)
	}
	n := t.Size()
	var idx []int // source component for each destination component
	switch {
	case x.t.IsScalar():
		idx = make([]int, n)
	case t.IsScalar():
		idx = []int{0}
	case x.t.Rows == 1 && t.Rows == 1 && x.t.Cols >= t.Cols:
		idx = make([]int, n)
		for i := range idx {
			idx[i] = i
		}
	case x.t.Rows >= t.Rows && x.t.Cols >= t.Cols && t.Rows > 1:
		idx = make([]int, 0, n)
		for r := 0; r < t.Rows; r++ {
			for cc := 0; cc < t.Cols; cc++ {
				idx = append(idx, r*x.t.Cols+cc)
			}
		}
	case x.t.Rows > 1 && t.Rows == 1 && x.t.Rows*x.t.Cols >= t.Cols:
		idx = make([]int, n) // matrix -> vector: first components in row order
		for i := range idx {
			idx[i] = i
		}
	default:
		failf(line, "cannot convert %s to %s", x.t, t)
	}
	tmp := c.temp(t)
	xe, xo := x.eval, x.off
	var conv func(d, a *Lane)
	switch {
	case t.Base == BInt && x.t.Base == BFloat:
		conv = func(d, a *Lane) {
			for l := range d {
				d[l] = float32(math.Trunc(float64(a[l])))
			}
		}
	case t.Base == BBool:
		conv = func(d, a *Lane) {
			for l := range d {
				d[l] = b2f(a[l] != 0)
			}
		}
	}
	if conv == nil {
		return &cval{t: t, off: tmp, eval: func(x *ctx) {
			xe(x)
			for i, k := range idx {
				x.m[tmp+i] = x.m[xo+k]
			}
		}}
	}
	return &cval{t: t, off: tmp, eval: func(x *ctx) {
		xe(x)
		for i, k := range idx {
			conv(&x.m[tmp+i], &x.m[xo+k])
		}
	}}
}

// cast handles explicit casts, which additionally allow (float3x3)float4x4-style truncation and
// struct identity.
func (c *compiler) cast(x *cval, t *Type, line int) *cval {
	return c.convertTo(x, t, line)
}

func (c *compiler) initList(il *InitListExpr, t *Type) *cval {
	// Flatten scalars in order.
	var items []*cval
	var flatten func(e Expr)
	flatten = func(e Expr) {
		if l, ok := e.(*InitListExpr); ok {
			for _, it := range l.Items {
				flatten(it)
			}
			return
		}
		items = append(items, c.expr(e))
	}
	flatten(il)
	tmp := c.temp(t)
	n := t.Size()
	type part struct {
		eval     evalFn
		off, len int
	}
	var parts []part
	total := 0
	for _, it := range items {
		if !it.t.IsNumeric() {
			failf(il.Line, "initializer element of type %s", it.t)
		}
		parts = append(parts, part{it.eval, it.off, it.t.Size()})
		total += it.t.Size()
	}
	if total != n {
		failf(il.Line, "initializer has %d components, %s needs %d", total, t, n)
	}
	return &cval{t: t, off: tmp, eval: func(x *ctx) {
		o := tmp
		for _, p := range parts {
			p.eval(x)
			copy(x.m[o:o+p.len], x.m[p.off:p.off+p.len])
			o += p.len
		}
	}}
}

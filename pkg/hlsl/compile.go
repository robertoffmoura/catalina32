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

	run      func(m []float32)
	retOff   int
	inits    []func(m []float32) // static/global initialisers, run once per State
	consts   []*Constant
	Sampler  func(unit int, coord []float32, cube bool) [4]float32 // texture lookups (optional)
	uniformT []func(m []float32)                                   // per-batch: copy registers into uniform slots (set by Load*)
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
	body     func(m []float32)
	locals   []map[string]*local
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
			c.prog.inits = append(c.prog.inits, func(m []float32) {
				init.eval(m)
				copy(m[off:off+size], m[init.off:init.off+size])
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
	cf.body = func(m []float32) { body(m) }
	return cf
}

// Statement execution returns true when a return statement ran.
type stmtFn func(m []float32) bool

func (c *compiler) block(b *BlockStmt) func(m []float32) {
	c.scopes = append(c.scopes, map[string]*local{})
	var list []stmtFn
	for _, s := range b.List {
		list = append(list, c.stmt(s))
	}
	c.scopes = c.scopes[:len(c.scopes)-1]
	return func(m []float32) {
		for _, s := range list {
			if s(m) {
				return
			}
		}
	}
}

func (c *compiler) blockStmt(b *BlockStmt) stmtFn {
	c.scopes = append(c.scopes, map[string]*local{})
	var list []stmtFn
	for _, s := range b.List {
		list = append(list, c.stmt(s))
	}
	c.scopes = c.scopes[:len(c.scopes)-1]
	return func(m []float32) bool {
		for _, s := range list {
			if s(m) {
				return true
			}
		}
		return false
	}
}

func (c *compiler) stmt(s Stmt) stmtFn {
	switch s := s.(type) {
	case *BlockStmt:
		return c.blockStmt(s)
	case *DeclStmt:
		var fns []func(m []float32)
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
				fns = append(fns, func(m []float32) {
					init.eval(m)
					copy(m[off:off+size], m[init.off:init.off+size])
				})
			}
			c.scopes[len(c.scopes)-1][v.Name] = &local{t: v.Type, off: off}
		}
		return func(m []float32) bool {
			for _, f := range fns {
				f(m)
			}
			return false
		}
	case *ExprStmt:
		e := c.expr(s.X)
		return func(m []float32) bool { e.eval(m); return false }
	case *IfStmt:
		cond := c.convertTo(c.expr(s.Cond), tyBool, s.Line)
		then := c.stmt(s.Then)
		var els stmtFn
		if s.Else != nil {
			els = c.stmt(s.Else)
		}
		co := cond.off
		return func(m []float32) bool {
			cond.eval(m)
			if m[co] != 0 {
				return then(m)
			} else if els != nil {
				return els(m)
			}
			return false
		}
	case *ReturnStmt:
		ret := c.cur.decl.Ret
		if s.X == nil {
			return func(m []float32) bool { return true }
		}
		v := c.convertTo(c.expr(s.X), ret, s.Line)
		off, size := c.cur.retOff, ret.Size()
		return func(m []float32) bool {
			v.eval(m)
			copy(m[off:off+size], m[v.off:v.off+size])
			return true
		}
	}
	failf(s.stmtLine(), "unsupported statement %T", s)
	return nil
}

// ---------------------------------------------------------------------------------------------
// Expressions

// cval is a compiled expression: after eval(m), its value is in m[off : off+t.Size()]
// (contiguous). Lvalues additionally carry store information.
type cval struct {
	t    *Type
	eval func(m []float32) // never nil; no-op for values already in storage
	off  int
	lv   *lvalue
}

// lvalue describes an assignable location: base slot (static or dynamic) plus an optional
// component map (swizzle) relative to the base.
type lvalue struct {
	base  int
	dyn   func(m []float32) int // runtime base (dynamic array index); nil = static
	comps []int
}

func noop(m []float32) {}

func (c *compiler) temp(t *Type) int { return c.alloc(t.Size()) }

func (c *compiler) lookup(name string, line int) *cval {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if l := c.scopes[i][name]; l != nil {
			return &cval{t: l.t, eval: noop, off: l.off, lv: &lvalue{base: l.off}}
		}
	}
	if g := c.global(name, line); g != nil {
		v := &cval{t: g.decl.Type, eval: noop, off: g.off}
		if g.constant == nil {
			v.lv = &lvalue{base: g.off}
		}
		return v
	}
	failf(line, "undefined identifier %q", name)
	return nil
}

func (c *compiler) expr(e Expr) *cval {
	switch e := e.(type) {
	case *NumberExpr:
		t := tyFloat
		if e.IsInt {
			t = tyInt
		}
		off := c.temp(t)
		v := float32(e.Value)
		return &cval{t: t, off: off, eval: func(m []float32) { m[off] = v }}
	case *BoolExpr:
		off := c.temp(tyBool)
		v := float32(0)
		if e.Value {
			v = 1
		}
		return &cval{t: tyBool, off: off, eval: func(m []float32) { m[off] = v }}
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
		lv = &lvalue{base: x.lv.base + off, dyn: x.lv.dyn}
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
		return &cval{t: t, off: tmp, lv: lv, eval: func(m []float32) {
			ev(m)
			copy(m[tmp:tmp+size], m[src:src+size])
		}}
	}
	return &cval{t: t, off: tmp, lv: lv, eval: func(m []float32) {
		ev(m)
		for i, k := range comps {
			m[tmp+i] = m[src+k]
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
	xev, iev, xoff, ioff := x.eval, idx.eval, x.off, idx.off
	at := func(m []float32) int {
		i := int(m[ioff])
		if i < 0 {
			i = 0
		} else if i >= n {
			i = n - 1
		}
		return i * stride
	}
	var lv *lvalue
	if x.lv != nil && x.lv.comps == nil {
		base, xd := x.lv.base, x.lv.dyn
		lv = &lvalue{dyn: func(m []float32) int {
			iev(m)
			b := base
			if xd != nil {
				b = xd(m)
			}
			return b + at(m)
		}}
	}
	return &cval{t: elem, off: tmp, lv: lv, eval: func(m []float32) {
		xev(m)
		iev(m)
		src := xoff + at(m)
		copy(m[tmp:tmp+size], m[src:src+size])
	}}
}

// store writes the value at m[src:] (type t, already converted) to lvalue lv.
func storeFn(lv *lvalue, size int) func(m []float32, src int) {
	if lv.comps == nil {
		if lv.dyn == nil {
			base := lv.base
			return func(m []float32, src int) { copy(m[base:base+size], m[src:src+size]) }
		}
		dyn := lv.dyn
		return func(m []float32, src int) {
			b := dyn(m)
			copy(m[b:b+size], m[src:src+size])
		}
	}
	comps, base, dyn := lv.comps, lv.base, lv.dyn
	return func(m []float32, src int) {
		b := base
		if dyn != nil {
			b = dyn(m)
		}
		for i, k := range comps {
			m[b+k] = m[src+i]
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
	return &cval{t: lhs.t, off: roff, eval: func(m []float32) {
		rev(m)
		st(m, roff)
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
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			xe(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = -m[xo+i]
			}
		}}
	case "!":
		t := &Type{Base: BBool, Rows: x.t.Rows, Cols: x.t.Cols}
		n := t.Size()
		tmp := c.temp(t)
		xe, xo := x.eval, x.off
		return &cval{t: t, off: tmp, eval: func(m []float32) {
			xe(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = b2f(m[xo+i] == 0)
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
		return &cval{t: tyBool, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m) // HLSL evaluates both operands
			if and {
				m[tmp] = b2f(m[xo] != 0 && m[yo] != 0)
			} else {
				m[tmp] = b2f(m[xo] != 0 || m[yo] != 0)
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
	cmp := false
	switch op {
	case "<", ">", "<=", ">=", "==", "!=":
		rt = &Type{Base: BBool, Rows: rows, Cols: cols}
		cmp = true
	}
	n := shape.Size()
	tmp := c.temp(rt)
	xe, ye, xo, yo := x.eval, y.eval, x.off, y.off
	isInt := base == BInt
	var f func(a, b float32) float32
	switch op {
	case "+":
		f = func(a, b float32) float32 { return a + b }
	case "-":
		f = func(a, b float32) float32 { return a - b }
	case "*":
		f = func(a, b float32) float32 { return a * b }
	case "/":
		if isInt {
			f = func(a, b float32) float32 {
				if b == 0 {
					return 0
				}
				return float32(int32(a) / int32(b))
			}
		} else {
			f = func(a, b float32) float32 { return a / b }
		}
	case "%":
		f = func(a, b float32) float32 { return float32(math.Mod(float64(a), float64(b))) }
	case "<":
		f = func(a, b float32) float32 { return b2f(a < b) }
	case ">":
		f = func(a, b float32) float32 { return b2f(a > b) }
	case "<=":
		f = func(a, b float32) float32 { return b2f(a <= b) }
	case ">=":
		f = func(a, b float32) float32 { return b2f(a >= b) }
	case "==":
		f = func(a, b float32) float32 { return b2f(a == b) }
	case "!=":
		f = func(a, b float32) float32 { return b2f(a != b) }
	}
	_ = cmp
	// Specialise the common float ops to avoid an indirect call per component.
	switch op {
	case "+":
		return &cval{t: rt, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = m[xo+i] + m[yo+i]
			}
		}}
	case "-":
		return &cval{t: rt, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = m[xo+i] - m[yo+i]
			}
		}}
	case "*":
		return &cval{t: rt, off: tmp, eval: func(m []float32) {
			xe(m)
			ye(m)
			for i := 0; i < n; i++ {
				m[tmp+i] = m[xo+i] * m[yo+i]
			}
		}}
	}
	return &cval{t: rt, off: tmp, eval: func(m []float32) {
		xe(m)
		ye(m)
		for i := 0; i < n; i++ {
			m[tmp+i] = f(m[xo+i], m[yo+i])
		}
	}}
}

func (c *compiler) condExpr(e *CondExpr) *cval {
	cond := c.convertTo(c.expr(e.Cond), tyBool, e.Line)
	x, y := c.expr(e.X), c.expr(e.Y)
	rows, cols := binaryShape(x.t, y.t, e.Line)
	t := &Type{Base: promote(x.t.Base, y.t.Base), Rows: rows, Cols: cols}
	x, y = c.convertTo(x, t, e.Line), c.convertTo(y, t, e.Line)
	n := t.Size()
	tmp := c.temp(t)
	ce, xe, ye, co, xo, yo := cond.eval, x.eval, y.eval, cond.off, x.off, y.off
	return &cval{t: t, off: tmp, eval: func(m []float32) {
		ce(m)
		if m[co] != 0 {
			xe(m)
			copy(m[tmp:tmp+n], m[xo:xo+n])
		} else {
			ye(m)
			copy(m[tmp:tmp+n], m[yo:yo+n])
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
	var idx []int // source component for each destination component (-1 = broadcast slot 0)
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
	conv := func(v float32) float32 { return v }
	switch t.Base {
	case BInt:
		if x.t.Base == BFloat {
			conv = func(v float32) float32 { return float32(math.Trunc(float64(v))) }
		}
	case BBool:
		conv = func(v float32) float32 { return b2f(v != 0) }
	}
	return &cval{t: t, off: tmp, eval: func(m []float32) {
		xe(m)
		for i, k := range idx {
			m[tmp+i] = conv(m[xo+k])
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
		eval     func(m []float32)
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
	return &cval{t: t, off: tmp, eval: func(m []float32) {
		o := tmp
		for _, p := range parts {
			p.eval(m)
			copy(m[o:o+p.len], m[p.off:p.off+p.len])
			o += p.len
		}
	}}
}

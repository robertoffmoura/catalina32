package hlsl

// Translation to Metal Shading Language, for the GPU renderer.
//
// A Program (one entry point of a parsed file) becomes an MSLModule: the core (structs, a
// per-invocation struct holding every global, and the user functions reachable from the
// entry), plus what is needed to build the stage function around it. The stage function
// depends on the pipeline: a vertex function needs the class of each vertex attribute (float
// or integer data), and a fragment function takes its inputs from what the paired vertex
// function outputs. VertexMSL and FragmentMSL build complete compilable sources.
//
// Semantics follow the closure evaluator in compile.go, which the tests compare against:
//   - HLSL matrices are row-major values; an HLSL RxC matrix is stored as the MSL matrix
//     floatRxC (R columns of C components, one per HLSL row), so m[i] is row i in both and
//     mul() maps to MSL products with the operands swapped.
//   - Uniforms are unpacked from the float constant registers with D3DX's column-major
//     packing (element [r][c] in register reg+c, component r).
//   - Implicit conversions, array index clamping, normalize(0) = 0 and the per-component
//     operators of the evaluator are reproduced explicitly.
//   - Globals that are not uniforms live in the per-invocation struct G, zero-initialised, with
//     static initialisers run at entry; every user function takes it (and the textures) by
//     reference.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"catalina32/pkg/msl"
)

// MSLLeaf is one scalar/vector element of an entry point's inputs or outputs.
type MSLLeaf struct {
	Path     string // MSL access path relative to the parameter/return variable
	Semantic string // upper-case, e.g. "TEXCOORD"
	Index    int
	Type     *Type // leaf type (scalar or vector)
	param    int   // entry parameter index, or -1 for the return value
}

// MSLModule is a translated entry point.
type MSLModule struct {
	Vertex   bool
	Core     string    // prelude, structs, G, user functions
	Inputs   []MSLLeaf // entry inputs (vertex attributes / fragment varyings)
	Outputs  []MSLLeaf // entry outputs (varyings / colours)
	texUnits []msl.Texture
	entry    *FuncDecl
	fnName   string
	loads    string // statements filling g's uniforms from the register array C
	inits    string // global initialisers
}

// TranslateMSL translates entry (already compiled into prog) of file.
func TranslateMSL(file *File, prog *Program) (mod *MSLModule, err error) {
	defer func() {
		if r := recover(); r != nil {
			ce, ok := r.(compileError)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("msl: %s", ce.msg)
		}
	}()
	g := &mslGen{file: file, prog: prog, funcs: map[string][]*FuncDecl{}, globals: map[string]*VarDecl{},
		emitted: map[*FuncDecl]bool{}, structsUsed: map[*StructDef]bool{}, samplers: map[string]msl.Texture{}}
	for _, f := range file.Funcs {
		g.funcs[f.Name] = append(g.funcs[f.Name], f)
	}
	for _, v := range file.Globals {
		g.globals[v.Name] = v
	}
	for _, k := range prog.Constants {
		if k.RegisterSet == RegSampler {
			tex := msl.Texture{Unit: k.RegisterIndex}
			switch sm := strings.ToLower(k.Type.Sampler); {
			case strings.Contains(sm, "cube"):
				tex.Kind = msl.TexCube
			case strings.Contains(sm, "3d"):
				tex.Kind = msl.Tex3D
			}
			g.samplers[k.Name] = tex
			g.textures = append(g.textures, tex)
		}
	}
	sort.Slice(g.textures, func(i, j int) bool { return g.textures[i].Unit < g.textures[j].Unit })
	ents := g.funcs[prog.Entry]
	if len(ents) != 1 {
		return nil, fmt.Errorf("msl: entry %q not found", prog.Entry)
	}
	ef := ents[0]
	mod = &MSLModule{entry: ef, texUnits: g.textures}
	mod.fnName = g.function(ef)
	for i, prm := range ef.Params {
		if prm.In {
			flattenLeaves(&mod.Inputs, "", prm.Semantic, prm.Type, i)
		}
	}
	if ef.Ret.Base != BVoid {
		flattenLeaves(&mod.Outputs, "", ef.Semantic, ef.Ret, -1)
	}
	for i, prm := range ef.Params {
		if prm.Out {
			flattenLeaves(&mod.Outputs, "", prm.Semantic, prm.Type, i)
		}
	}
	for _, l := range append(append([]MSLLeaf(nil), mod.Inputs...), mod.Outputs...) {
		if !l.Type.IsNumeric() || l.Type.IsMatrix() {
			return nil, fmt.Errorf("msl: varying %s%d of type %s", l.Semantic, l.Index, l.Type)
		}
	}
	mod.loads = g.uniformLoads()
	mod.inits = g.globalInits()
	mod.Core = g.core()
	return mod, nil
}

// flattenLeaves lists the scalar/vector elements of an entry parameter or return type.
func flattenLeaves(list *[]MSLLeaf, path, sem string, t *Type, param int) {
	switch {
	case t.IsArray():
		s, idx := splitSemantic(sem)
		for i := 0; i < t.ArrayLen; i++ {
			flattenLeaves(list, fmt.Sprintf("%s[%d]", path, i), fmt.Sprintf("%s%d", s, idx+i), t.Elem, param)
		}
	case t.Base == BStruct:
		for _, f := range t.Struct.Fields {
			flattenLeaves(list, path+"."+fieldName(f.Name), f.Semantic, f.Type, param)
		}
	default:
		s, idx := splitSemantic(sem)
		*list = append(*list, MSLLeaf{Path: path, Semantic: s, Index: idx, Type: t, param: param})
	}
}

type mslGen struct {
	file        *File
	prog        *Program
	funcs       map[string][]*FuncDecl
	globals     map[string]*VarDecl
	usedGlobals []*VarDecl // non-sampler globals in first-use order
	globalSeen  map[string]bool
	samplers    map[string]msl.Texture
	textures    []msl.Texture
	emitted     map[*FuncDecl]bool
	structsUsed map[*StructDef]bool
	structOrder []*StructDef
	fnText      []string // function definitions, callees first
	scopes      []map[string]*mvar
	cur         *FuncDecl
	tmp         int
	pre, post   []string // statements around the statement being emitted (out-parameter temporaries)
}

type mvar struct {
	t    *Type
	name string
}

// mv is a translated expression.
type mv struct {
	code        string
	t           *Type
	lvalue      bool         // assignable
	addressable bool         // can bind to a reference (no swizzle or vector component in the path)
	sampler     *msl.Texture // sampler globals
}

// Identifier spelling: every user name gets a prefix, so none collides with MSL keywords,
// types or functions.
func localName(n string) string  { return "l_" + n }
func globalName(n string) string { return "g_" + n }
func fieldName(n string) string  { return "m_" + n }
func structName(n string) string { return "st_" + n }
func funcName(n string) string   { return "fn_" + n }

func (g *mslGen) useStruct(s *StructDef) {
	if g.structsUsed[s] {
		return
	}
	g.structsUsed[s] = true
	for _, f := range s.Fields {
		g.useType(f.Type)
	}
	g.structOrder = append(g.structOrder, s)
}

func (g *mslGen) useType(t *Type) {
	for t.IsArray() {
		t = t.Elem
	}
	if t.Base == BStruct {
		g.useStruct(t.Struct)
	}
}

// mslType spells a non-array type.
func (g *mslGen) mslType(t *Type, line int) string {
	switch t.Base {
	case BVoid:
		return "void"
	case BStruct:
		g.useStruct(t.Struct)
		return structName(t.Struct.Name)
	case BFloat, BInt, BBool:
		name := map[Base]string{BFloat: "float", BInt: "int", BBool: "bool"}[t.Base]
		switch {
		case t.Rows == 1 && t.Cols == 1:
			return name
		case t.Rows == 1:
			return name + strconv.Itoa(t.Cols)
		case t.Base != BFloat:
			failf(line, "msl: %s matrices are not supported", name)
		}
		return fmt.Sprintf("float%dx%d", t.Rows, t.Cols)
	}
	failf(line, "msl: type %s cannot be used here", t)
	return ""
}

// decl spells a declaration of name with type t (arrays in C style).
func (g *mslGen) decl(t *Type, name string, line int) string {
	dims := ""
	for t.IsArray() {
		dims += fmt.Sprintf("[%d]", t.ArrayLen)
		t = t.Elem
	}
	return g.mslType(t, line) + " " + name + dims
}

// zero is a zero value of t usable as an initializer.
func (g *mslGen) zero(t *Type, line int) string {
	if t.IsArray() || t.Base == BStruct {
		return "{}"
	}
	if t.IsMatrix() {
		return g.mslType(t, line) + "(0.0f)"
	}
	return g.mslType(t, line) + "(0)"
}

// texParams/texArgs are the texture and sampler parameters every user function takes.
func (g *mslGen) texParams() string {
	var b strings.Builder
	for _, t := range g.textures {
		fmt.Fprintf(&b, ", %s t%d, sampler s%d", t.TypeName(), t.Unit, t.Unit)
	}
	return b.String()
}

func (g *mslGen) texArgs() string {
	var b strings.Builder
	for _, t := range g.textures {
		fmt.Fprintf(&b, ", t%d, s%d", t.Unit, t.Unit)
	}
	return b.String()
}

// function translates fd (once) and returns its MSL name.
func (g *mslGen) function(fd *FuncDecl) string {
	name := funcName(fd.Name)
	if g.emitted[fd] {
		return name
	}
	g.emitted[fd] = true
	savedCur, savedScopes, savedPre, savedPost := g.cur, g.scopes, g.pre, g.post
	g.cur, g.scopes, g.pre, g.post = fd, []map[string]*mvar{{}}, nil, nil
	var sig strings.Builder
	fmt.Fprintf(&sig, "%s %s(thread G& g%s", g.mslType(fd.Ret, fd.Line), name, g.texParams())
	for _, prm := range fd.Params {
		if prm.Type.IsArray() {
			failf(fd.Line, "msl: array parameter %s", prm.Name)
		}
		ln := localName(prm.Name)
		if prm.Out {
			fmt.Fprintf(&sig, ", thread %s& %s", g.mslType(prm.Type, fd.Line), ln)
		} else {
			fmt.Fprintf(&sig, ", %s %s", g.mslType(prm.Type, fd.Line), ln)
		}
		g.scopes[0][prm.Name] = &mvar{t: prm.Type, name: ln}
	}
	sig.WriteString(")")
	var body strings.Builder
	g.block(&body, fd.Body, 1)
	if fd.Ret.Base != BVoid {
		// HLSL functions that fall off the end return an unspecified value: return zero.
		fmt.Fprintf(&body, "\treturn %s;\n", g.zeroValue(fd.Ret, fd.Line))
	}
	g.fnText = append(g.fnText, sig.String()+" {\n"+body.String()+"}\n")
	g.cur, g.scopes, g.pre, g.post = savedCur, savedScopes, savedPre, savedPost
	return name
}

// zeroValue is a zero expression of t (for return statements).
func (g *mslGen) zeroValue(t *Type, line int) string {
	if t.IsArray() || t.Base == BStruct {
		return g.mslType(t, line) + "{}"
	}
	return g.zero(t, line)
}

func indent(n int) string { return strings.Repeat("\t", n) }

func (g *mslGen) block(b *strings.Builder, blk *BlockStmt, depth int) {
	g.scopes = append(g.scopes, map[string]*mvar{})
	for _, s := range blk.List {
		g.stmt(b, s, depth)
	}
	g.scopes = g.scopes[:len(g.scopes)-1]
}

// stmt translates one statement; out-parameter temporaries of calls inside it are declared
// before it and written back after it.
func (g *mslGen) stmt(b *strings.Builder, s Stmt, depth int) {
	in := indent(depth)
	var text strings.Builder
	g.pre, g.post = nil, nil
	switch s := s.(type) {
	case *BlockStmt:
		text.WriteString(in + "{\n")
		g.block(&text, s, depth+1)
		text.WriteString(in + "}\n")
	case *DeclStmt:
		for _, v := range s.Vars {
			name := localName(v.Name)
			init := g.zero(v.Type, s.Line)
			if v.Init != nil {
				if il, ok := v.Init.(*InitListExpr); ok {
					init = g.initList(il, v.Type)
				} else {
					init = g.conv(g.expr(v.Init), v.Type, s.Line)
				}
			}
			if len(g.pre) > 0 || len(g.post) > 0 {
				// Out-parameter temporaries need a block; the variable is declared before it so
				// it stays in scope.
				fmt.Fprintf(&text, "%s%s = %s;\n%s{\n", in, g.decl(v.Type, name, s.Line), g.zero(v.Type, s.Line), in)
				for _, p := range g.pre {
					text.WriteString(in + "\t" + p + "\n")
				}
				fmt.Fprintf(&text, "%s\t%s = %s;\n", in, name, init)
				for _, p := range g.post {
					text.WriteString(in + "\t" + p + "\n")
				}
				text.WriteString(in + "}\n")
				g.pre, g.post = nil, nil
			} else {
				fmt.Fprintf(&text, "%s%s = %s;\n", in, g.decl(v.Type, name, s.Line), init)
			}
			// Declared after the initializer is translated: "float x = x;" refers to the outer x.
			g.scopes[len(g.scopes)-1][v.Name] = &mvar{t: v.Type, name: name}
		}
	case *ExprStmt:
		fmt.Fprintf(&text, "%s%s;\n", in, g.expr(s.X).code)
	case *IfStmt:
		cond := g.conv(g.expr(s.Cond), tyBool, s.Line)
		pre, post := g.pre, g.post
		g.pre, g.post = nil, nil
		fmt.Fprintf(&text, "%sif (%s) {\n", in, cond)
		g.stmt(&text, s.Then, depth+1)
		if s.Else != nil {
			text.WriteString(in + "} else {\n")
			g.stmt(&text, s.Else, depth+1)
		}
		text.WriteString(in + "}\n")
		g.pre, g.post = pre, post
	case *ReturnStmt:
		if s.X == nil {
			text.WriteString(in + "return;\n")
		} else {
			v := g.conv(g.expr(s.X), g.cur.Ret, s.Line)
			if len(g.post) > 0 { // write-backs must happen before returning
				t := g.newTemp()
				fmt.Fprintf(&text, "%s%s = %s;\n", in, g.decl(g.cur.Ret, t, s.Line), v)
				for _, p := range g.post {
					text.WriteString(in + p + "\n")
				}
				g.post = nil
				v = t
			}
			fmt.Fprintf(&text, "%sreturn %s;\n", in, v)
		}
	default:
		failf(s.stmtLine(), "msl: unsupported statement %T", s)
	}
	if len(g.pre) > 0 || len(g.post) > 0 {
		b.WriteString(in + "{\n")
		for _, p := range g.pre {
			b.WriteString(in + "\t" + p + "\n")
		}
		b.WriteString(text.String())
		for _, p := range g.post {
			b.WriteString(in + "\t" + p + "\n")
		}
		b.WriteString(in + "}\n")
	} else {
		b.WriteString(text.String())
	}
	g.pre, g.post = nil, nil
}

func (g *mslGen) newTemp() string {
	g.tmp++
	return fmt.Sprintf("tmp%d", g.tmp)
}

// ---------------------------------------------------------------------------------------------
// Expressions

func floatLit(v float64) string {
	s := strconv.FormatFloat(float64(float32(v)), 'g', -1, 32)
	if !strings.ContainsAny(s, ".eEn") { // "n": NaN/Inf do not occur in source
		s += ".0"
	}
	return s + "f"
}

func (g *mslGen) lookup(name string, line int) mv {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		if v := g.scopes[i][name]; v != nil {
			return mv{code: v.name, t: v.t, lvalue: true, addressable: true}
		}
	}
	if tex, ok := g.samplers[name]; ok {
		t := tex
		return mv{code: fmt.Sprintf("t%d", tex.Unit), t: &Type{Base: BSampler}, sampler: &t}
	}
	if d := g.globals[name]; d != nil {
		if d.Type.Base == BSampler || (d.Type.IsArray() && d.Type.Elem.Base == BSampler) {
			failf(line, "msl: sampler %s is not bound to a register", name)
		}
		if g.globalSeen == nil {
			g.globalSeen = map[string]bool{}
		}
		if !g.globalSeen[name] {
			g.globalSeen[name] = true
			g.usedGlobals = append(g.usedGlobals, d)
			g.useType(d.Type)
			if d.Init != nil && !d.Uniform {
				g.conv(g.expr(d.Init), d.Type, line) // reach what the initializer uses
			}
		}
		return mv{code: "g." + globalName(name), t: d.Type, lvalue: true, addressable: true}
	}
	failf(line, "undefined identifier %q", name)
	return mv{}
}

func (g *mslGen) expr(e Expr) mv {
	switch e := e.(type) {
	case *NumberExpr:
		if e.IsInt {
			return mv{code: strconv.FormatInt(int64(e.Value), 10), t: tyInt}
		}
		return mv{code: floatLit(e.Value), t: tyFloat}
	case *BoolExpr:
		return mv{code: strconv.FormatBool(e.Value), t: tyBool}
	case *IdentExpr:
		return g.lookup(e.Name, e.Line)
	case *MemberExpr:
		return g.member(e)
	case *IndexExpr:
		return g.index(e)
	case *UnaryExpr:
		return g.unary(e)
	case *BinaryExpr:
		switch e.Op {
		case "&&", "||":
			// The evaluator converts both operands to scalar bools and evaluates both.
			x := g.conv(g.expr(e.X), tyBool, e.Line)
			y := g.conv(g.expr(e.Y), tyBool, e.Line)
			return mv{code: "(" + x + " " + e.Op + " " + y + ")", t: tyBool}
		}
		return g.arith(e.Op, g.expr(e.X), g.expr(e.Y), e.Line)
	case *AssignExpr:
		return g.assign(e)
	case *CastExpr:
		x := g.expr(e.X)
		return mv{code: g.conv(x, e.To, e.Line), t: e.To}
	case *CallExpr:
		return g.call(e)
	case *CondExpr:
		cond := g.conv(g.expr(e.Cond), tyBool, e.Line)
		x, y := g.expr(e.X), g.expr(e.Y)
		rows, cols := binaryShape(x.t, y.t, e.Line)
		t := &Type{Base: promote(x.t.Base, y.t.Base), Rows: rows, Cols: cols}
		return mv{code: fmt.Sprintf("(%s ? %s : %s)", cond, g.conv(x, t, e.Line), g.conv(y, t, e.Line)), t: t}
	case *InitListExpr:
		failf(e.Line, "initializer list outside a declaration")
	}
	failf(e.line(), "msl: unsupported expression %T", e)
	return mv{}
}

var swizzleName = map[byte]byte{'x': 'x', 'y': 'y', 'z': 'z', 'w': 'w', 'r': 'x', 'g': 'y', 'b': 'z', 'a': 'w'}

func (g *mslGen) member(e *MemberExpr) mv {
	x := g.expr(e.X)
	if x.t.Base == BStruct && !x.t.IsArray() {
		f := x.t.Struct.field(e.Name)
		if f == nil {
			failf(e.Line, "%s has no field %q", x.t, e.Name)
		}
		return mv{code: x.code + "." + fieldName(f.Name), t: f.Type, lvalue: x.lvalue, addressable: x.addressable}
	}
	if !x.t.IsNumeric() || x.t.IsMatrix() || len(e.Name) > 4 {
		failf(e.Line, "cannot select %q from %s", e.Name, x.t)
	}
	sw := make([]byte, len(e.Name))
	for i := 0; i < len(e.Name); i++ {
		c, ok := swizzleName[e.Name[i]]
		if !ok || int(swizzleIndex[c]) >= x.t.Cols {
			failf(e.Line, "bad swizzle %q on %s", e.Name, x.t)
		}
		sw[i] = c
	}
	n := len(sw)
	t := vectorType(x.t.Base, n)
	if n == 1 {
		t = scalarType(x.t.Base)
	}
	if x.t.IsScalar() {
		if n == 1 {
			return x
		}
		return mv{code: g.mslType(t, e.Line) + "(" + x.code + ")", t: t}
	}
	if n == x.t.Cols && string(sw) == "xyzw"[:n] {
		return x
	}
	return mv{code: "(" + x.code + ")." + string(sw), t: t, lvalue: x.lvalue}
}

// clampIndex spells an index into n elements, clamped like the evaluator (a literal in range
// is used as is).
func clampIndex(idx string, n int) string {
	if v, err := strconv.Atoi(idx); err == nil && v >= 0 && v < n {
		return idx
	}
	return fmt.Sprintf("clamp(%s, 0, %d)", idx, n-1)
}

func (g *mslGen) index(e *IndexExpr) mv {
	x := g.expr(e.X)
	idx := g.conv(g.expr(e.Index), tyInt, e.Line)
	switch {
	case x.t.IsArray():
		return mv{code: fmt.Sprintf("%s[%s]", x.code, clampIndex(idx, x.t.ArrayLen)), t: x.t.Elem, lvalue: x.lvalue, addressable: x.addressable}
	case x.t.IsMatrix():
		return mv{code: fmt.Sprintf("%s[%s]", x.code, clampIndex(idx, x.t.Rows)), t: vectorType(x.t.Base, x.t.Cols), lvalue: x.lvalue, addressable: x.addressable}
	case x.t.IsVector():
		return mv{code: fmt.Sprintf("%s[%s]", x.code, clampIndex(idx, x.t.Cols)), t: scalarType(x.t.Base), lvalue: x.lvalue}
	}
	failf(e.Line, "cannot index %s", x.t)
	return mv{}
}

func (g *mslGen) unary(e *UnaryExpr) mv {
	x := g.expr(e.X)
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
			return mv{code: "(-" + g.conv(x, t, e.Line) + ")", t: t}
		}
		return mv{code: "(-" + x.code + ")", t: t}
	case "!":
		t := &Type{Base: BBool, Rows: x.t.Rows, Cols: x.t.Cols}
		if t.IsMatrix() {
			failf(e.Line, "msl: ! on a matrix")
		}
		if x.t.Base == BBool {
			return mv{code: "(!" + x.code + ")", t: t}
		}
		return mv{code: "(" + x.code + " == 0)", t: t}
	}
	failf(e.Line, "unsupported unary operator %s", e.Op)
	return mv{}
}

func (g *mslGen) arith(op string, x, y mv, line int) mv {
	if !x.t.IsNumeric() || !y.t.IsNumeric() {
		failf(line, "operator %s needs numeric operands, got %s and %s", op, x.t, y.t)
	}
	rows, cols := binaryShape(x.t, y.t, line)
	base := promote(x.t.Base, y.t.Base)
	shape := &Type{Base: base, Rows: rows, Cols: cols}
	a, b := g.conv(x, shape, line), g.conv(y, shape, line)
	rt := shape
	switch op {
	case "<", ">", "<=", ">=", "==", "!=":
		rt = &Type{Base: BBool, Rows: rows, Cols: cols}
		if shape.IsMatrix() {
			failf(line, "msl: matrix comparison")
		}
	}
	switch {
	case op == "%" && base == BFloat:
		return mv{code: fmt.Sprintf("fmod(%s, %s)", a, b), t: rt}
	case shape.IsMatrix() && (op == "*" || op == "/"):
		// HLSL * and / on matrices are per component.
		fn := map[string]string{"*": "h_cwmul", "/": "h_cwdiv"}[op]
		return mv{code: fmt.Sprintf("%s(%s, %s)", fn, a, b), t: rt}
	}
	return mv{code: "(" + a + " " + op + " " + b + ")", t: rt}
}

func (g *mslGen) assign(e *AssignExpr) mv {
	lhs := g.expr(e.LHS)
	if !lhs.lvalue {
		failf(e.Line, "cannot assign to this expression")
	}
	var rhs string
	if e.Op == "=" {
		rhs = g.conv(g.expr(e.RHS), lhs.t, e.Line)
	} else {
		rhs = g.conv(g.arith(e.Op[:1], lhs, g.expr(e.RHS), e.Line), lhs.t, e.Line)
	}
	return mv{code: "(" + lhs.code + " = " + rhs + ")", t: lhs.t}
}

// ---------------------------------------------------------------------------------------------
// Conversions (convertTo in compile.go)

// comp spells component k (row-major order) of x, which has type t.
func comp(x string, t *Type, k int) string {
	switch {
	case t.IsScalar():
		return x
	case t.IsMatrix():
		return fmt.Sprintf("(%s)[%d][%d]", x, k/t.Cols, k%t.Cols)
	}
	return fmt.Sprintf("(%s)[%d]", x, k)
}

// conv converts x to t with HLSL's implicit conversion rules.
func (g *mslGen) conv(x mv, t *Type, line int) string {
	if sameShape(x.t, t) {
		return x.code
	}
	if !x.t.IsNumeric() || !t.IsNumeric() {
		if x.t.Base == BStruct && t.Base == BStruct && x.t.Struct == t.Struct && !x.t.IsArray() && !t.IsArray() {
			return x.code
		}
		failf(line, "cannot convert %s to %s", x.t, t)
	}
	ts := g.mslType(t, line)
	if x.t.Base == BBool && t.Base != BBool && x.t.IsVector() && t.Rows == 1 && x.t.Cols >= t.Cols {
		// bool vectors become 0/1 vectors through select (also well defined where comparisons
		// produce masks).
		v := x.code
		if x.t.Cols > t.Cols {
			v = "(" + v + ")." + "xyzw"[:t.Cols]
		}
		if t.Cols == 1 {
			return fmt.Sprintf("(%s ? %s(1) : %s(0))", v, ts, ts)
		}
		return fmt.Sprintf("select(%s(0), %s(1), %s)", ts, ts, v)
	}
	switch {
	case x.t.IsScalar() && t.IsMatrix():
		col := g.mslType(vectorType(t.Base, t.Cols), line)
		cols := make([]string, t.Rows)
		for i := range cols {
			cols[i] = col + "(" + x.code + ")"
		}
		return ts + "(" + strings.Join(cols, ", ") + ")"
	case x.t.IsScalar():
		return ts + "(" + x.code + ")"
	case t.IsScalar():
		return ts + "(" + comp(x.code, x.t, 0) + ")"
	case x.t.Rows == 1 && t.Rows == 1 && x.t.Cols >= t.Cols:
		v := x.code
		if x.t.Cols > t.Cols {
			v = "(" + x.code + ")." + "xyzw"[:t.Cols]
		}
		if x.t.Base == t.Base {
			return v
		}
		return ts + "(" + v + ")"
	case x.t.Rows >= t.Rows && x.t.Cols >= t.Cols && t.Rows > 1:
		col := g.mslType(vectorType(t.Base, t.Cols), line)
		rows := make([]string, t.Rows)
		for r := range rows {
			v := fmt.Sprintf("(%s)[%d]", x.code, r)
			if x.t.Cols > t.Cols {
				v += "." + "xyzw"[:t.Cols]
			}
			rows[r] = col + "(" + v + ")"
		}
		return ts + "(" + strings.Join(rows, ", ") + ")"
	case x.t.Rows > 1 && t.Rows == 1 && x.t.Rows*x.t.Cols >= t.Cols:
		cs := make([]string, t.Cols)
		for i := range cs {
			cs[i] = comp(x.code, x.t, i)
		}
		return ts + "(" + strings.Join(cs, ", ") + ")"
	}
	failf(line, "cannot convert %s to %s", x.t, t)
	return ""
}

// initList translates a brace initializer for t from its flattened scalar components.
func (g *mslGen) initList(il *InitListExpr, t *Type) string {
	var items []mv
	var flatten func(e Expr)
	flatten = func(e Expr) {
		if l, ok := e.(*InitListExpr); ok {
			for _, it := range l.Items {
				flatten(it)
			}
			return
		}
		items = append(items, g.expr(e))
	}
	flatten(il)
	var comps []string
	for _, it := range items {
		if !it.t.IsNumeric() {
			failf(il.Line, "initializer element of type %s", it.t)
		}
		for k := 0; k < it.t.Size(); k++ {
			comps = append(comps, comp(it.code, it.t, k))
		}
	}
	if len(comps) != t.Size() {
		failf(il.Line, "initializer has %d components, %s needs %d", len(comps), t, t.Size())
	}
	i := 0
	return g.fromComps(t, comps, &i, il.Line)
}

// fromComps builds a value of t from scalar components (row-major), for initializers.
func (g *mslGen) fromComps(t *Type, comps []string, i *int, line int) string {
	switch {
	case t.IsArray():
		parts := make([]string, t.ArrayLen)
		for k := range parts {
			parts[k] = g.fromComps(t.Elem, comps, i, line)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case t.Base == BStruct:
		parts := make([]string, len(t.Struct.Fields))
		for k, f := range t.Struct.Fields {
			parts[k] = g.fromComps(f.Type, comps, i, line)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	n := t.Size()
	scalar := g.mslType(scalarType(t.Base), line)
	parts := make([]string, n)
	for k := range parts {
		parts[k] = scalar + "(" + comps[*i+k] + ")"
	}
	*i += n
	if n == 1 {
		return parts[0]
	}
	return g.mslType(t, line) + "(" + strings.Join(parts, ", ") + ")"
}

// ---------------------------------------------------------------------------------------------
// Calls

func (g *mslGen) call(e *CallExpr) mv {
	if e.Ctor != nil {
		return g.construct(e)
	}
	if cands := g.funcs[e.Name]; len(cands) > 0 {
		return g.userCall(e, cands)
	}
	return g.intrinsic(e)
}

func (g *mslGen) construct(e *CallExpr) mv {
	t := e.Ctor
	if len(e.Args) == 1 {
		return mv{code: g.conv(g.expr(e.Args[0]), t, e.Line), t: t}
	}
	var parts []mv
	total := 0
	for _, a := range e.Args {
		v := g.expr(a)
		if !v.t.IsNumeric() {
			failf(e.Line, "constructor argument of type %s", v.t)
		}
		parts = append(parts, v)
		total += v.t.Size()
	}
	if total != t.Size() {
		failf(e.Line, "%s constructor needs %d components, got %d", t, t.Size(), total)
	}
	ts := g.mslType(t, e.Line)
	// Each argument keeps its shape when it fits (scalars, and vectors in vector constructors
	// or whole rows of a matrix); anything else is split into components.
	var args []string
	for _, p := range parts {
		fits := p.t.IsScalar() || (p.t.IsVector() && (t.IsVector() || p.t.Cols == t.Cols))
		if !fits {
			for k := 0; k < p.t.Size(); k++ {
				args = append(args, g.mslType(scalarType(t.Base), e.Line)+"("+comp(p.code, p.t, k)+")")
			}
			continue
		}
		code := p.code
		if p.t.Base != t.Base {
			code = g.mslType(&Type{Base: t.Base, Rows: 1, Cols: p.t.Cols}, e.Line) + "(" + code + ")"
		}
		args = append(args, code)
	}
	if t.IsMatrix() {
		// A matrix takes either all scalars or one vector per row (an MSL column).
		allScalar := true
		for _, p := range parts {
			allScalar = allScalar && p.t.IsScalar()
		}
		allRows := true
		for _, p := range parts {
			allRows = allRows && p.t.IsVector() && p.t.Cols == t.Cols
		}
		if !allScalar && !allRows {
			args = args[:0]
			for _, p := range parts {
				for k := 0; k < p.t.Size(); k++ {
					args = append(args, "float("+comp(p.code, p.t, k)+")")
				}
			}
		}
	}
	return mv{code: ts + "(" + strings.Join(args, ", ") + ")", t: t}
}

func (g *mslGen) userCall(e *CallExpr, cands []*FuncDecl) mv {
	args := make([]mv, len(e.Args))
	for i, a := range e.Args {
		args[i] = g.expr(a)
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
	name := g.function(best)
	var list []string
	for i, prm := range best.Params {
		switch {
		case prm.Out && !args[i].lvalue:
			failf(e.Line, "argument %d of %s must be assignable (out parameter)", i+1, e.Name)
		case prm.Out && !args[i].addressable:
			// A swizzle or vector component cannot bind to a reference: go through a temporary.
			t := g.newTemp()
			init := g.zero(prm.Type, e.Line)
			if prm.In {
				init = args[i].code
			}
			g.pre = append(g.pre, fmt.Sprintf("%s = %s;", g.decl(prm.Type, t, e.Line), init))
			g.post = append(g.post, fmt.Sprintf("%s = %s;", args[i].code, t))
			list = append(list, t)
		case prm.Out:
			list = append(list, args[i].code)
		default:
			list = append(list, g.conv(args[i], prm.Type, e.Line))
		}
	}
	code := name + "(g" + g.texArgs()
	if len(list) > 0 {
		code += ", " + strings.Join(list, ", ")
	}
	return mv{code: code + ")", t: best.Ret}
}

// floatShape converts args to their common float shape (as compile.go).
func (g *mslGen) floatShape(args []mv, line int) (*Type, []string) {
	rows, cols := args[0].t.Rows, args[0].t.Cols
	shape := args[0].t
	for _, a := range args[1:] {
		rows, cols = binaryShape(shape, a.t, line)
		shape = &Type{Base: BFloat, Rows: rows, Cols: cols}
	}
	t := &Type{Base: BFloat, Rows: rows, Cols: cols}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = g.conv(a, t, line)
	}
	return t, out
}

// Intrinsics that apply per component, by HLSL name: the MSL function.
var mslUnary = map[string]string{
	"abs": "abs", "sqrt": "sqrt", "rsqrt": "rsqrt", "exp": "exp", "exp2": "exp2", "log": "log",
	"log2": "log2", "log10": "log10", "floor": "floor", "ceil": "ceil", "round": "rint", "trunc": "trunc",
	"frac": "fract", "sin": "sin", "cos": "cos", "tan": "tan", "asin": "asin", "acos": "acos",
	"atan": "atan", "sign": "sign", "saturate": "saturate",
}

var mslBinary = map[string]string{"max": "max", "min": "min", "pow": "h_pow", "step": "step", "fmod": "fmod", "atan2": "atan2"}

func (g *mslGen) intrinsic(e *CallExpr) mv {
	args := make([]mv, len(e.Args))
	for i, a := range e.Args {
		args[i] = g.expr(a)
	}
	need := func(n int) {
		if len(args) != n {
			failf(e.Line, "%s takes %d arguments, got %d", e.Name, n, len(args))
		}
	}
	perComponent := func(fn string, t *Type, a []string) mv {
		if t.IsMatrix() {
			fn = "h_m_" + fn // matrix forms are in the prelude
		}
		return mv{code: fn + "(" + strings.Join(a, ", ") + ")", t: t}
	}
	if fn, ok := mslUnary[e.Name]; ok {
		need(1)
		t, a := g.floatShape(args, e.Line)
		return perComponent(fn, t, a)
	}
	if fn, ok := mslBinary[e.Name]; ok {
		need(2)
		t, a := g.floatShape(args, e.Line)
		return perComponent(fn, t, a)
	}
	switch e.Name {
	case "lerp", "clamp", "smoothstep":
		need(3)
		t, a := g.floatShape(args, e.Line)
		fn := map[string]string{"lerp": "h_lerp", "clamp": "h_clamp", "smoothstep": "h_smoothstep"}[e.Name]
		return perComponent(fn, t, a)
	case "dot":
		need(2)
		t, a := g.floatShape(args, e.Line)
		if t.IsScalar() {
			return mv{code: "(" + a[0] + " * " + a[1] + ")", t: tyFloat}
		}
		return mv{code: fmt.Sprintf("h_dot(%s, %s)", a[0], a[1]), t: tyFloat}
	case "length":
		need(1)
		t, a := g.floatShape(args, e.Line)
		if t.IsScalar() {
			return mv{code: "abs(" + a[0] + ")", t: tyFloat}
		}
		return mv{code: "sqrt(h_dot(" + a[0] + ", " + a[0] + "))", t: tyFloat}
	case "normalize":
		need(1)
		t, a := g.floatShape(args, e.Line)
		return mv{code: "h_normalize(" + a[0] + ")", t: t}
	case "distance":
		need(2)
		t, a := g.floatShape(args, e.Line)
		if t.IsScalar() {
			return mv{code: "abs(" + a[0] + " - " + a[1] + ")", t: tyFloat}
		}
		return mv{code: fmt.Sprintf("h_distance(%s, %s)", a[0], a[1]), t: tyFloat}
	case "reflect":
		need(2)
		t, a := g.floatShape(args, e.Line)
		return mv{code: fmt.Sprintf("h_reflect(%s, %s)", a[0], a[1]), t: t}
	case "cross":
		need(2)
		v3 := vectorType(BFloat, 3)
		return mv{code: fmt.Sprintf("cross(%s, %s)", g.conv(args[0], v3, e.Line), g.conv(args[1], v3, e.Line)), t: v3}
	case "any", "all":
		need(1)
		x := args[0]
		if x.t.IsScalar() {
			return mv{code: "(" + g.conv(x, tyBool, e.Line) + ")", t: tyBool}
		}
		if x.t.IsMatrix() {
			failf(e.Line, "msl: %s of a matrix", e.Name)
		}
		bt := &Type{Base: BBool, Rows: 1, Cols: x.t.Cols}
		return mv{code: e.Name + "(" + g.conv(x, bt, e.Line) + ")", t: tyBool}
	case "mul":
		need(2)
		return g.mul(args[0], args[1], e.Line)
	case "tex2D", "texCUBE", "tex2Dlod", "tex2Dbias", "tex2Dproj", "tex2Dgrad", "texCUBElod", "texCUBEbias":
		if len(args) < 2 {
			failf(e.Line, "%s needs a sampler and coordinates", e.Name)
		}
		smp := args[0].sampler
		if smp == nil {
			failf(e.Line, "%s: first argument must be a sampler", e.Name)
		}
		v4 := vectorType(BFloat, 4)
		dim := 2
		if smp.Kind != msl.Tex2D {
			dim = 3
		}
		coordT := vectorType(BFloat, dim)
		tex, sam := fmt.Sprintf("t%d", smp.Unit), fmt.Sprintf("s%d", smp.Unit)
		var code string
		switch e.Name {
		case "tex2D", "texCUBE":
			code = fmt.Sprintf("%s.sample(%s, %s)", tex, sam, g.conv(args[1], coordT, e.Line))
		case "tex2Dlod", "texCUBElod", "tex2Dbias", "texCUBEbias":
			c4 := g.conv(args[1], v4, e.Line)
			opt := "level"
			if strings.HasSuffix(e.Name, "bias") {
				opt = "bias"
			}
			code = fmt.Sprintf("h_sample_%s(%s, %s, %s)", opt, tex, sam, c4)
		case "tex2Dproj":
			code = fmt.Sprintf("h_sample_proj(%s, %s, %s)", tex, sam, g.conv(args[1], v4, e.Line))
		case "tex2Dgrad":
			need(4)
			v2 := vectorType(BFloat, 2)
			code = fmt.Sprintf("%s.sample(%s, %s, gradient2d(%s, %s))", tex, sam, g.conv(args[1], v2, e.Line),
				g.conv(args[2], v2, e.Line), g.conv(args[3], v2, e.Line))
		}
		return mv{code: code, t: v4}
	}
	failf(e.Line, "unknown function %q", e.Name)
	return mv{}
}

// mul translates HLSL mul(); see the matrix storage note at the top of the file.
func (g *mslGen) mul(x, y mv, line int) mv {
	toFloat := func(v mv) string {
		if v.t.Base == BFloat {
			return v.code
		}
		return g.conv(v, &Type{Base: BFloat, Rows: v.t.Rows, Cols: v.t.Cols}, line)
	}
	switch {
	case x.t.IsScalar() || y.t.IsScalar():
		return g.arith("*", x, y, line)
	case x.t.IsVector() && y.t.IsVector():
		n := min(x.t.Cols, y.t.Cols)
		vt := vectorType(BFloat, n)
		return mv{code: fmt.Sprintf("h_dot(%s, %s)", g.conv(x, vt, line), g.conv(y, vt, line)), t: tyFloat}
	case x.t.IsVector() && y.t.IsMatrix():
		if x.t.Cols != y.t.Rows {
			failf(line, "mul(%s, %s): dimension mismatch", x.t, y.t)
		}
		return mv{code: fmt.Sprintf("(%s * %s)", toFloat(y), toFloat(x)), t: vectorType(BFloat, y.t.Cols)}
	case x.t.IsMatrix() && y.t.IsVector():
		if y.t.Cols != x.t.Cols {
			failf(line, "mul(%s, %s): dimension mismatch", x.t, y.t)
		}
		return mv{code: fmt.Sprintf("(%s * %s)", toFloat(y), toFloat(x)), t: vectorType(BFloat, x.t.Rows)}
	case x.t.IsMatrix() && y.t.IsMatrix():
		if y.t.Rows != x.t.Cols {
			failf(line, "mul(%s, %s): dimension mismatch", x.t, y.t)
		}
		return mv{code: fmt.Sprintf("(%s * %s)", toFloat(y), toFloat(x)), t: &Type{Base: BFloat, Rows: x.t.Rows, Cols: y.t.Cols}}
	}
	failf(line, "mul(%s, %s) is not supported", x.t, y.t)
	return mv{}
}

// ---------------------------------------------------------------------------------------------
// Module assembly

// uniformLoads fills g's uniforms from the float constant registers C (appendLoads in runtime.go).
func (g *mslGen) uniformLoads() string {
	var b strings.Builder
	var load func(path string, t *Type, reg int)
	load = func(path string, t *Type, reg int) {
		switch {
		case t.IsArray():
			for i := 0; i < t.ArrayLen; i++ {
				load(fmt.Sprintf("%s[%d]", path, i), t.Elem, reg+i*registerCount(t.Elem))
			}
		case t.Base == BStruct:
			for _, f := range t.Struct.Fields {
				load(path+"."+fieldName(f.Name), f.Type, reg)
				reg += registerCount(f.Type)
			}
		case t.Rows > 1:
			col := g.mslType(vectorType(BFloat, t.Cols), 0)
			for r := 0; r < t.Rows; r++ {
				cs := make([]string, t.Cols)
				for c := range cs {
					cs[c] = fmt.Sprintf("C[%d][%d]", reg+c, r)
				}
				fmt.Fprintf(&b, "\t%s[%d] = %s(%s);\n", path, r, col, strings.Join(cs, ", "))
			}
		default:
			v := fmt.Sprintf("C[%d]", reg)
			if t.Cols < 4 {
				v += "." + "xyzw"[:t.Cols]
			}
			if t.Base != BFloat {
				v = g.mslType(t, 0) + "(" + v + ")"
			}
			fmt.Fprintf(&b, "\t%s = %s;\n", path, v)
		}
	}
	for _, k := range g.prog.Constants {
		if k.RegisterSet != RegFloat4 || !g.globalSeen[k.Name] {
			continue
		}
		load("g."+globalName(k.Name), k.Type, k.RegisterIndex)
	}
	return b.String()
}

// globalInits runs static initialisers of the globals in use, in declaration order.
func (g *mslGen) globalInits() string {
	var b strings.Builder
	for _, d := range g.file.Globals {
		if !g.globalSeen[d.Name] || d.Init == nil || d.Uniform {
			continue
		}
		g.pre, g.post = nil, nil
		var v string
		if il, ok := d.Init.(*InitListExpr); ok {
			if d.Type.IsArray() || d.Type.Base == BStruct {
				// Aggregates are assigned element by element through a typed local.
				t := g.newTemp()
				fmt.Fprintf(&b, "\t{ %s = %s; h_copy(g.%s, %s); }\n", g.decl(d.Type, t, d.Line), g.initList(il, d.Type), globalName(d.Name), t)
				continue
			}
			v = g.initList(il, d.Type)
		} else {
			v = g.conv(g.expr(d.Init), d.Type, d.Line)
		}
		fmt.Fprintf(&b, "\tg.%s = %s;\n", globalName(d.Name), v)
	}
	return b.String()
}

// core assembles the prelude, structs, G and the functions.
func (g *mslGen) core() string {
	var b strings.Builder
	b.WriteString(mslPrelude)
	for _, s := range g.structOrder {
		fmt.Fprintf(&b, "struct %s {\n", structName(s.Name))
		for _, f := range s.Fields {
			fmt.Fprintf(&b, "\t%s;\n", g.decl(f.Type, fieldName(f.Name), 0))
		}
		b.WriteString("};\n")
	}
	b.WriteString("struct G {\n")
	for _, d := range g.usedGlobals {
		fmt.Fprintf(&b, "\t%s;\n", g.decl(d.Type, globalName(d.Name), d.Line))
	}
	b.WriteString("\tint unused_;\n};\n")
	for _, f := range g.fnText {
		b.WriteString(f)
	}
	return b.String()
}

// mslPrelude holds helpers for HLSL semantics MSL lacks or spells differently.
const mslPrelude = `#include <metal_stdlib>
using namespace metal;

template <typename T> inline T h_lerp(T a, T b, T s) { return a + s * (b - a); }
template <typename T> inline T h_clamp(T x, T lo, T hi) { return min(max(x, lo), hi); }
template <typename T> inline T h_smoothstep(T lo, T hi, T x) {
	T t = min(max((x - lo) / (hi - lo), T(0)), T(1));
	return t * t * (T(3) - T(2) * t);
}
inline float h_pow(float a, float b) { return pow(a, b); }
template <int N> inline vec<float, N> h_pow(vec<float, N> a, vec<float, N> b) { return pow(a, b); }
// Dot products and lengths accumulate in component order, like the evaluator.
template <int N> inline float h_dot(vec<float, N> a, vec<float, N> b) {
	float s = 0;
	for (int i = 0; i < N; i++) s += a[i] * b[i];
	return s;
}
template <int N> inline vec<float, N> h_normalize(vec<float, N> v) {
	float s = h_dot(v, v);
	return s > 0 ? v * rsqrt(s) : vec<float, N>(0);
}
inline float h_normalize(float v) { return v > 0 ? 1.0f : (v < 0 ? -1.0f : 0.0f); }
template <int N> inline float h_distance(vec<float, N> a, vec<float, N> b) { vec<float, N> d = a - b; return sqrt(h_dot(d, d)); }
template <int N> inline vec<float, N> h_reflect(vec<float, N> i, vec<float, N> n) { return i - 2 * h_dot(i, n) * n; }
inline float h_reflect(float i, float n) { return i - 2 * i * n * n; }
template <int C, int R> inline matrix<float, C, R> h_cwmul(matrix<float, C, R> a, matrix<float, C, R> b) {
	matrix<float, C, R> r;
	for (int i = 0; i < C; i++) r[i] = a[i] * b[i];
	return r;
}
template <int C, int R> inline matrix<float, C, R> h_cwdiv(matrix<float, C, R> a, matrix<float, C, R> b) {
	matrix<float, C, R> r;
	for (int i = 0; i < C; i++) r[i] = a[i] / b[i];
	return r;
}
inline float4 h_sample_level(texture2d<float> t, sampler s, float4 c) { return t.sample(s, c.xy, level(c.w)); }
inline float4 h_sample_bias(texture2d<float> t, sampler s, float4 c) { return t.sample(s, c.xy, bias(c.w)); }
inline float4 h_sample_level(texturecube<float> t, sampler s, float4 c) { return t.sample(s, c.xyz, level(c.w)); }
inline float4 h_sample_bias(texturecube<float> t, sampler s, float4 c) { return t.sample(s, c.xyz, bias(c.w)); }
inline float4 h_sample_proj(texture2d<float> t, sampler s, float4 c) { return t.sample(s, c.w != 0 ? c.xy / c.w : c.xy); }
template <typename T, int N> inline void h_copy(thread T (&d)[N], thread T (&s)[N]) { for (int i = 0; i < N; i++) d[i] = s[i]; }
template <typename T> inline void h_copy(thread T& d, thread T& s) { d = s; }
// end of prelude
`

// ---------------------------------------------------------------------------------------------
// Stage functions

// VertexMSL builds a complete vertex function source. attrs gives the data class of each input
// (by index in Inputs; missing entries are float). Buffer 0 holds the float constant registers,
// buffer 1 the renderer's state (S[0].xy: half-pixel offset in clip units); vertex streams
// start at buffer 2.
func (m *MSLModule) VertexMSL(attrs []msl.AttrClass) string {
	var b strings.Builder
	b.WriteString(m.Core)
	b.WriteString("struct VIn {\n")
	for i := range m.Inputs {
		ty := "float4"
		if i < len(attrs) {
			ty = msl.AttrType(attrs[i])
		}
		fmt.Fprintf(&b, "\t%s a%d [[attribute(%d)]];\n", ty, i, i)
	}
	if len(m.Inputs) == 0 {
		b.WriteString("\tfloat4 unused_ [[attribute(0)]];\n")
	}
	b.WriteString("};\nstruct VOut {\n\tfloat4 pos [[position]];\n")
	for _, l := range m.Outputs {
		if l.Semantic == "POSITION" && l.Index == 0 {
			continue
		}
		fmt.Fprintf(&b, "\t%s %s [[user(%s)]];\n", vecDecl(l.Type), varyingName(l), varyingName(l))
	}
	b.WriteString("};\n")
	fmt.Fprintf(&b, "vertex VOut vs_main(VIn vin [[stage_in]], constant float4* C [[buffer(0)]], constant float4* S [[buffer(1)]]%s) {\n", m.stageTexParams())
	m.entryCall(&b, func(l MSLLeaf, i int) string {
		v := fmt.Sprintf("vin.a%d", i)
		if i < len(attrs) && attrs[i] != msl.AttrFloat {
			v = "float4(" + v + ")"
		}
		return leafConv(v, l.Type)
	})
	b.WriteString("\tVOut o;\n\to.pos = float4(0, 0, 0, 1);\n")
	for _, l := range m.Outputs {
		src := m.outputRef(l)
		if l.Semantic == "POSITION" && l.Index == 0 {
			fmt.Fprintf(&b, "\to.pos = %s;\n", widen(src, l.Type))
			continue
		}
		fmt.Fprintf(&b, "\to.%s = %s;\n", varyingName(l), src)
	}
	b.WriteString("\to.pos.xy += S[0].xy * o.pos.w; // D3D9 pixel centres\n\treturn o;\n}\n")
	return b.String()
}

// FragmentMSL builds a complete fragment function source taking its varyings from a vertex
// function providing the given ones (inputs it does not provide read as zero). Buffer 0 holds
// the float constant registers, buffer 1 the renderer's state (S[0].x: alpha test function,
// S[0].y: reference).
func (m *MSLModule) FragmentMSL(vsOut []msl.Varying) string {
	var b strings.Builder
	b.WriteString(m.Core)
	provided := map[string]msl.Varying{}
	for _, v := range vsOut {
		provided[v.Name] = v
	}
	b.WriteString("struct PIn {\n\tfloat4 pos [[position]];\n")
	used := map[string]bool{}
	for _, l := range m.Inputs {
		n := varyingName(l)
		if vl, ok := provided[n]; ok && !used[n] && !(l.Semantic == "POSITION" && l.Index == 0) {
			used[n] = true
			fmt.Fprintf(&b, "\t%s %s [[user(%s)]];\n", vl.TypeName(), n, n)
		}
	}
	fog := "1.0f" // vertex fog factor for the fixed-function fog stage
	if v, ok := provided[msl.FogVarying]; ok {
		if !used[msl.FogVarying] {
			used[msl.FogVarying] = true
			fmt.Fprintf(&b, "\t%s %s [[user(%s)]];\n", v.TypeName(), msl.FogVarying, msl.FogVarying)
		}
		fog = widen("pin."+msl.FogVarying, varyingType(v)) + ".x"
	}
	b.WriteString("};\nstruct POut {\n")
	colors := 0
	for _, l := range m.Outputs {
		if l.Semantic == "COLOR" {
			fmt.Fprintf(&b, "\tfloat4 c%d [[color(%d)]];\n", l.Index, l.Index)
			colors++
		}
	}
	if colors == 0 {
		b.WriteString("\tfloat4 c0 [[color(0)]];\n")
	}
	b.WriteString("};\n")
	fmt.Fprintf(&b, "fragment POut ps_main(PIn pin [[stage_in]], constant float4* C [[buffer(0)]], constant float4* S [[buffer(1)]]%s) {\n", m.stageTexParams())
	m.entryCall(&b, func(l MSLLeaf, i int) string {
		n := varyingName(l)
		switch {
		case l.Semantic == "POSITION" || l.Semantic == "VPOS":
			return leafConv("pin.pos", l.Type)
		case used[n]:
			return leafConv(widen("pin."+n, varyingType(provided[n])), l.Type)
		}
		return "" // not provided: stays zero
	})
	b.WriteString("\tPOut o;\n")
	if colors == 0 {
		b.WriteString("\to.c0 = float4(0);\n")
	}
	for _, l := range m.Outputs {
		if l.Semantic == "COLOR" {
			fmt.Fprintf(&b, "\to.c%d = %s;\n", l.Index, widen(m.outputRef(l), l.Type))
		}
	}
	b.WriteString(msl.FogStage(fog))
	b.WriteString(alphaTestMSL)
	b.WriteString("\treturn o;\n}\n")
	return b.String()
}

// alphaTestMSL implements D3D's alpha test on colour 0, comparing its 8-bit alpha (as the
// software renderer does) with S[0].y using the D3DCMPFUNC in S[0].x (8 = always).
const alphaTestMSL = `	{
		int fn = int(S[0].x);
		float a = rint(saturate(o.c0.a) * 255.0f) / 255.0f, ref = S[0].y;
		bool pass = fn == 8 || (fn == 2 && a < ref) || (fn == 3 && a == ref) || (fn == 4 && a <= ref) ||
			(fn == 5 && a > ref) || (fn == 6 && a != ref) || (fn == 7 && a >= ref);
		if (!pass) discard_fragment();
	}
`

func (m *MSLModule) stageTexParams() string {
	var b strings.Builder
	for _, t := range m.texUnits {
		fmt.Fprintf(&b, ", %s t%d [[texture(%d)]], sampler s%d [[sampler(%d)]]", t.TypeName(), t.Unit, t.Unit, t.Unit, t.Unit)
	}
	return b.String()
}

func (m *MSLModule) texArgs() string {
	var b strings.Builder
	for _, t := range m.texUnits {
		fmt.Fprintf(&b, ", t%d, s%d", t.Unit, t.Unit)
	}
	return b.String()
}

// entryCall declares g, loads uniforms, runs initialisers, fills the entry parameters from the
// stage inputs (input returns the value for an input leaf, or "" to leave it zero) and calls
// the entry; its return value is in "r".
func (m *MSLModule) entryCall(b *strings.Builder, input func(l MSLLeaf, i int) string) {
	b.WriteString("\tG g = {};\n")
	b.WriteString(strings.ReplaceAll(m.loads, "\t", "\t"))
	b.WriteString(m.inits)
	gen := &mslGen{structsUsed: map[*StructDef]bool{}} // only for spelling types
	var args []string
	for i, prm := range m.entry.Params {
		name := fmt.Sprintf("p%d", i)
		fmt.Fprintf(b, "\t%s = %s;\n", gen.decl(prm.Type, name, 0), gen.zero(prm.Type, 0))
		args = append(args, name)
	}
	for i, l := range m.Inputs {
		if v := input(l, i); v != "" {
			fmt.Fprintf(b, "\tp%d%s = %s;\n", l.param, l.Path, v)
		}
	}
	call := m.fnName + "(g" + m.texArgs()
	if len(args) > 0 {
		call += ", " + strings.Join(args, ", ")
	}
	call += ")"
	if m.entry.Ret.Base != BVoid {
		fmt.Fprintf(b, "\t%s = %s;\n", gen.decl(m.entry.Ret, "r", 0), call)
	} else {
		fmt.Fprintf(b, "\t%s;\n", call)
	}
}

func (m *MSLModule) outputRef(l MSLLeaf) string {
	if l.param < 0 {
		return "r" + l.Path
	}
	return fmt.Sprintf("p%d%s", l.param, l.Path)
}

// Attributes lists the vertex inputs (msl.Shader).
func (m *MSLModule) Attributes() []msl.Attribute {
	var a []msl.Attribute
	for _, l := range m.Inputs {
		a = append(a, msl.Attribute{Semantic: l.Semantic, Index: l.Index})
	}
	return a
}

// Varyings lists the outputs other than the position (msl.Shader).
func (m *MSLModule) Varyings() []msl.Varying {
	var v []msl.Varying
	for _, l := range m.Outputs {
		if l.Semantic == "POSITION" && l.Index == 0 {
			continue
		}
		v = append(v, msl.Varying{Name: varyingName(l), Cols: l.Type.Cols, Int: l.Type.Base == BInt})
	}
	return v
}

// Textures lists the texture units read (msl.Shader).
func (m *MSLModule) Textures() []msl.Texture { return m.texUnits }

func varyingType(v msl.Varying) *Type {
	if v.Int {
		return vectorType(BInt, v.Cols)
	}
	return vectorType(BFloat, v.Cols)
}

// varyingName is the user attribute linking a vertex output to a fragment input.
func varyingName(l MSLLeaf) string { return fmt.Sprintf("%s%d", l.Semantic, l.Index) }

func vecDecl(t *Type) string {
	if t.Base == BInt {
		if t.Cols == 1 {
			return "int"
		}
		return fmt.Sprintf("int%d", t.Cols)
	}
	if t.Cols == 1 {
		return "float"
	}
	return fmt.Sprintf("float%d", t.Cols)
}

// widen makes a float4 from a scalar/vector expression of type t (missing components 0, w 1).
func widen(x string, t *Type) string {
	v := x
	if t.Base != BFloat {
		v = fmt.Sprintf("float%d(%s)", t.Cols, x)
		if t.Cols == 1 {
			v = "float(" + x + ")"
		}
	}
	switch t.Cols {
	case 1:
		return "float4(" + v + ", 0, 0, 1)"
	case 2:
		return "float4(" + v + ", 0, 1)"
	case 3:
		return "float4(" + v + ", 1)"
	}
	return v
}

// leafConv converts a float4 stage value to a leaf type.
func leafConv(x string, t *Type) string {
	v := x
	if t.Cols < 4 {
		v = "(" + x + ")." + "xyzw"[:t.Cols]
	}
	switch t.Base {
	case BInt:
		return vecDecl(t) + "(" + v + ")"
	case BBool:
		if t.Cols == 1 {
			return "bool(" + v + ")"
		}
		return fmt.Sprintf("bool%d(%s)", t.Cols, v)
	}
	return v
}

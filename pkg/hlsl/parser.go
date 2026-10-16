package hlsl

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse preprocesses and parses HLSL source.
func Parse(src string) (*File, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	if toks, err = preprocess(toks); err != nil {
		return nil, err
	}
	p := &parser{toks: toks, file: &File{Structs: map[string]*StructDef{}}}
	if err := p.parseFile(); err != nil {
		return nil, err
	}
	return p.file, nil
}

type parser struct {
	toks []token
	pos  int
	file *File
}

type parseError struct{ msg string }

func (p *parser) fail(format string, args ...any) {
	panic(parseError{fmt.Sprintf("line %d: ", p.peek().line) + fmt.Sprintf(format, args...)})
}

func (p *parser) peek() token        { return p.toks[p.pos] }
func (p *parser) peekAt(n int) token { return p.toks[min(p.pos+n, len(p.toks)-1)] }
func (p *parser) next() token        { t := p.toks[p.pos]; p.pos = min(p.pos+1, len(p.toks)-1); return t }
func (p *parser) is(text string) bool {
	t := p.peek()
	return (t.kind == tPunct || t.kind == tIdent) && t.text == text
}
func (p *parser) accept(text string) bool {
	if p.is(text) {
		p.next()
		return true
	}
	return false
}
func (p *parser) expect(text string) token {
	if !p.is(text) {
		p.fail("expected %q, found %s", text, p.peek())
	}
	return p.next()
}
func (p *parser) ident() string {
	t := p.peek()
	if t.kind != tIdent {
		p.fail("expected identifier, found %s", t)
	}
	p.next()
	return t.text
}

func (p *parser) parseFile() (err error) {
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(parseError)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("%s", pe.msg)
		}
	}()
	for p.peek().kind != tEOF {
		p.topLevel()
	}
	return nil
}

func (p *parser) topLevel() {
	if p.accept(";") {
		return
	}
	if p.is("struct") {
		p.structDef()
		p.expect(";")
		return
	}
	if p.is("technique") || p.is("technique10") {
		p.next()
		for !p.is("{") {
			p.next()
		}
		p.skipBraces()
		return
	}
	q := p.qualifiers()
	typ := p.typeName()
	name := p.ident()
	if p.is("(") {
		p.funcDecl(typ, name)
		return
	}
	p.declarators(typ, name, q, func(v *VarDecl) { p.file.Globals = append(p.file.Globals, v) })
	p.expect(";")
}

func (p *parser) skipBraces() {
	depth := 0
	for {
		t := p.next()
		if t.kind == tEOF {
			p.fail("unbalanced braces")
		}
		if t.kind == tPunct && t.text == "{" {
			depth++
		} else if t.kind == tPunct && t.text == "}" {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

type quals struct{ uniform, shared, static, constQ, in, out bool }

func (p *parser) qualifiers() quals {
	var q quals
	for {
		switch {
		case p.accept("uniform"):
			q.uniform = true
		case p.accept("shared"):
			q.shared = true
		case p.accept("static"):
			q.static = true
		case p.accept("const"):
			q.constQ = true
		case p.accept("in"):
			q.in = true
		case p.accept("out"):
			q.out = true
		case p.accept("inout"):
			q.in, q.out = true, true
		case p.accept("inline"), p.accept("extern"), p.accept("volatile"), p.accept("precise"),
			p.accept("row_major"), p.accept("column_major"), p.accept("centroid"), p.accept("linear"),
			p.accept("nointerpolation"):
		default:
			return q
		}
	}
}

func (p *parser) structDef() *StructDef {
	p.expect("struct")
	sd := &StructDef{Name: p.ident()}
	p.file.Structs[sd.Name] = sd
	p.expect("{")
	for !p.accept("}") {
		p.qualifiers()
		typ := p.typeName()
		for {
			f := &Field{Name: p.ident(), Type: typ}
			f.Type = p.arraySuffix(f.Type)
			for p.accept(":") {
				f.Semantic = p.ident()
			}
			sd.Fields = append(sd.Fields, f)
			if !p.accept(",") {
				break
			}
		}
		p.expect(";")
	}
	return sd
}

// builtinType parses names like float, half3, float4x4, int2, bool, sampler2D, string, void.
func builtinType(name string) *Type {
	switch name {
	case "void":
		return tyVoid
	case "string":
		return &Type{Base: BString}
	case "sampler", "sampler1D", "sampler2D", "sampler3D", "samplerCUBE", "texture", "texture2D", "textureCUBE":
		return &Type{Base: BSampler, Sampler: name}
	case "vector":
		return vectorType(BFloat, 4)
	case "matrix":
		return &Type{Base: BFloat, Rows: 4, Cols: 4}
	}
	var base Base
	var rest string
	for _, pfx := range []struct {
		s string
		b Base
	}{{"float", BFloat}, {"half", BFloat}, {"double", BFloat}, {"min16float", BFloat}, {"uint", BInt}, {"int", BInt}, {"dword", BInt}, {"bool", BBool}} {
		if strings.HasPrefix(name, pfx.s) {
			base, rest = pfx.b, name[len(pfx.s):]
			break
		}
	}
	if base == BVoid {
		return nil
	}
	switch {
	case rest == "":
		return scalarType(base)
	case len(rest) == 1 && rest[0] >= '1' && rest[0] <= '4':
		return vectorType(base, int(rest[0]-'0'))
	case len(rest) == 3 && rest[1] == 'x' && rest[0] >= '1' && rest[0] <= '4' && rest[2] >= '1' && rest[2] <= '4':
		r, c := int(rest[0]-'0'), int(rest[2]-'0')
		if r == 1 {
			return vectorType(base, c)
		}
		return &Type{Base: base, Rows: r, Cols: c}
	}
	return nil
}

func (p *parser) isTypeName(t token) bool {
	if t.kind != tIdent {
		return false
	}
	if builtinType(t.text) != nil {
		return true
	}
	_, ok := p.file.Structs[t.text]
	return ok
}

func (p *parser) typeName() *Type {
	t := p.peek()
	if t.kind == tIdent {
		if bt := builtinType(t.text); bt != nil {
			p.next()
			return bt
		}
		if sd, ok := p.file.Structs[t.text]; ok {
			p.next()
			return &Type{Base: BStruct, Struct: sd}
		}
		if t.text == "struct" {
			sd := p.structDef()
			return &Type{Base: BStruct, Struct: sd}
		}
	}
	p.fail("expected a type, found %s", t)
	return nil
}

func (p *parser) arraySuffix(t *Type) *Type {
	if !p.accept("[") {
		return t
	}
	n := p.peek()
	if n.kind != tNumber || !n.isInt || n.num <= 0 {
		p.fail("array size must be a positive integer constant, found %s", n)
	}
	p.next()
	p.expect("]")
	return &Type{ArrayLen: int(n.num), Elem: t, Base: t.Base}
}

func (p *parser) declarators(typ *Type, firstName string, q quals, add func(*VarDecl)) {
	name := firstName
	for {
		line := p.peek().line
		v := &VarDecl{Name: name, Type: p.arraySuffix(typ), Uniform: q.uniform, Shared: q.shared,
			Static: q.static, Const: q.constQ, Line: line}
		for p.accept(":") {
			if p.is("register") {
				p.next()
				p.expect("(")
				v.Register = strings.ToLower(p.ident())
				p.expect(")")
			} else {
				v.Semantic = p.ident()
			}
		}
		if p.accept("=") {
			if p.peek().kind == tString {
				p.next() // string annotations/metadata are ignored
			} else if p.is("{") {
				v.Init = p.initList()
			} else {
				v.Init = p.assignExpr()
			}
		}
		add(v)
		if !p.accept(",") {
			return
		}
		name = p.ident()
	}
}

func (p *parser) initList() Expr {
	line := p.expect("{").line
	il := &InitListExpr{Line: line}
	for !p.accept("}") {
		if p.is("{") {
			il.Items = append(il.Items, p.initList())
		} else {
			il.Items = append(il.Items, p.assignExpr())
		}
		if !p.accept(",") {
			p.expect("}")
			break
		}
	}
	return il
}

func (p *parser) funcDecl(ret *Type, name string) {
	line := p.expect("(").line
	fd := &FuncDecl{Name: name, Ret: ret, Line: line}
	if p.is("void") && p.peekAt(1).text == ")" {
		p.next()
	}
	for !p.accept(")") {
		q := p.qualifiers()
		pt := p.typeName()
		prm := &Param{Name: p.ident(), Type: pt, In: q.in || !q.out, Out: q.out}
		prm.Type = p.arraySuffix(prm.Type)
		for p.accept(":") {
			prm.Semantic = p.ident()
		}
		if p.accept("=") {
			p.fail("default parameter values are not supported")
		}
		fd.Params = append(fd.Params, prm)
		if !p.accept(",") {
			p.expect(")")
			break
		}
	}
	if p.accept(":") {
		fd.Semantic = p.ident()
	}
	if p.accept(";") {
		return // prototype
	}
	fd.Body = p.block()
	p.file.Funcs = append(p.file.Funcs, fd)
}

func (p *parser) block() *BlockStmt {
	b := &BlockStmt{Line: p.expect("{").line}
	for !p.accept("}") {
		if p.peek().kind == tEOF {
			p.fail("unterminated block")
		}
		b.List = append(b.List, p.stmt())
	}
	return b
}

func (p *parser) stmt() Stmt {
	t := p.peek()
	switch {
	case p.is("{"):
		return p.block()
	case p.accept(";"):
		return &BlockStmt{Line: t.line}
	case p.is("if"):
		p.next()
		p.expect("(")
		s := &IfStmt{Cond: p.expr(), Line: t.line}
		p.expect(")")
		s.Then = p.stmt()
		if p.accept("else") {
			s.Else = p.stmt()
		}
		return s
	case p.is("return"):
		p.next()
		s := &ReturnStmt{Line: t.line}
		if !p.is(";") {
			s.X = p.expr()
		}
		p.expect(";")
		return s
	case p.is("for") || p.is("while") || p.is("do") || p.is("switch") || p.is("break") || p.is("continue"):
		p.fail("%q statements are not supported", t.text)
	case p.is("discard"):
		p.fail("discard is not supported")
	}
	if p.is("const") || p.is("static") || (p.isTypeName(t) && p.peekAt(1).kind == tIdent) {
		q := p.qualifiers()
		typ := p.typeName()
		ds := &DeclStmt{Line: t.line}
		p.declarators(typ, p.ident(), q, func(v *VarDecl) { ds.Vars = append(ds.Vars, v) })
		p.expect(";")
		return ds
	}
	s := &ExprStmt{X: p.expr(), Line: t.line}
	p.expect(";")
	return s
}

func (p *parser) expr() Expr {
	e := p.assignExpr()
	if p.is(",") {
		p.fail("comma expressions are not supported")
	}
	return e
}

var assignOps = map[string]bool{"=": true, "+=": true, "-=": true, "*=": true, "/=": true}

func (p *parser) assignExpr() Expr {
	lhs := p.condExpr()
	if t := p.peek(); t.kind == tPunct && assignOps[t.text] {
		p.next()
		return &AssignExpr{Op: t.text, LHS: lhs, RHS: p.assignExpr(), Line: t.line}
	}
	return lhs
}

func (p *parser) condExpr() Expr {
	c := p.binary(0)
	if t := p.peek(); p.accept("?") {
		x := p.assignExpr()
		p.expect(":")
		return &CondExpr{Cond: c, X: x, Y: p.condExpr(), Line: t.line}
	}
	return c
}

var binPrec = map[string]int{
	"||": 1, "&&": 2, "|": 3, "^": 4, "&": 5,
	"==": 6, "!=": 6, "<": 7, ">": 7, "<=": 7, ">=": 7,
	"<<": 8, ">>": 8, "+": 9, "-": 9, "*": 10, "/": 10, "%": 10,
}

func (p *parser) binary(minPrec int) Expr {
	x := p.unary()
	for {
		t := p.peek()
		prec, ok := binPrec[t.text]
		if t.kind != tPunct || !ok || prec <= minPrec {
			return x
		}
		p.next()
		y := p.binary(prec)
		x = &BinaryExpr{Op: t.text, X: x, Y: y, Line: t.line}
	}
}

func (p *parser) unary() Expr {
	t := p.peek()
	if t.kind == tPunct {
		switch t.text {
		case "-", "+", "!", "~":
			p.next()
			return &UnaryExpr{Op: t.text, X: p.unary(), Line: t.line}
		case "++", "--":
			p.fail("prefix %s is not supported", t.text)
		case "(":
			if nt := p.peekAt(1); p.isTypeName(nt) && p.peekAt(2).text == ")" {
				p.next()
				to := p.typeName()
				p.expect(")")
				return &CastExpr{To: to, X: p.unary(), Line: t.line}
			}
		}
	}
	return p.postfix(p.primary())
}

func (p *parser) postfix(x Expr) Expr {
	for {
		t := p.peek()
		switch {
		case p.accept("."):
			x = &MemberExpr{X: x, Name: p.ident(), Line: t.line}
		case p.accept("["):
			idx := p.expr()
			p.expect("]")
			x = &IndexExpr{X: x, Index: idx, Line: t.line}
		case p.is("++") || p.is("--"):
			p.fail("postfix %s is not supported", t.text)
		default:
			return x
		}
	}
}

func (p *parser) args() []Expr {
	p.expect("(")
	var args []Expr
	for !p.accept(")") {
		args = append(args, p.assignExpr())
		if !p.accept(",") {
			p.expect(")")
			break
		}
	}
	return args
}

func (p *parser) primary() Expr {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.next()
		return &NumberExpr{Value: t.num, IsInt: t.isInt, Line: t.line}
	case tIdent:
		if t.text == "true" || t.text == "false" {
			p.next()
			return &BoolExpr{Value: t.text == "true", Line: t.line}
		}
		if bt := builtinType(t.text); bt != nil && p.peekAt(1).text == "(" {
			p.next()
			return &CallExpr{Name: t.text, Ctor: bt, Args: p.args(), Line: t.line}
		}
		p.next()
		if p.is("(") {
			return &CallExpr{Name: t.text, Args: p.args(), Line: t.line}
		}
		return &IdentExpr{Name: t.text, Line: t.line}
	case tPunct:
		if t.text == "(" {
			p.next()
			e := p.expr()
			p.expect(")")
			return e
		}
	case tString:
		p.fail("string expressions are not supported")
	}
	p.fail("unexpected %s", t)
	return nil
}

// registerIndex parses "c16" / "s0" into (class, index).
func registerIndex(r string) (byte, int, bool) {
	if len(r) < 2 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(r[1:])
	return r[0], n, err == nil
}

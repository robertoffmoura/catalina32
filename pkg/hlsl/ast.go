package hlsl

import "fmt"

// Base type of a value.
type Base uint8

const (
	BVoid Base = iota
	BFloat
	BInt
	BBool
	BStruct
	BSampler
	BString
)

// Type describes a value: a scalar (Rows=Cols=1), vector (Rows=1, Cols=N), matrix (Rows x Cols),
// struct, sampler or string, optionally an array (ArrayLen > 0; Elem is the element type).
type Type struct {
	Base       Base
	Rows, Cols int
	Struct     *StructDef
	Sampler    string // "sampler2D", "samplerCUBE", ...
	ArrayLen   int
	Elem       *Type
}

func (t *Type) IsArray() bool { return t.ArrayLen > 0 }
func (t *Type) IsNumeric() bool {
	return !t.IsArray() && (t.Base == BFloat || t.Base == BInt || t.Base == BBool)
}
func (t *Type) IsScalar() bool { return t.IsNumeric() && t.Rows == 1 && t.Cols == 1 }
func (t *Type) IsVector() bool { return t.IsNumeric() && t.Rows == 1 && t.Cols > 1 }
func (t *Type) IsMatrix() bool { return t.IsNumeric() && t.Rows > 1 }

// Size is the number of float slots a value occupies in the evaluator.
func (t *Type) Size() int {
	switch {
	case t.IsArray():
		return t.ArrayLen * t.Elem.Size()
	case t.Base == BStruct:
		n := 0
		for _, f := range t.Struct.Fields {
			n += f.Type.Size()
		}
		return n
	case t.Base == BSampler:
		return 1 // sampler register index
	case t.Base == BString, t.Base == BVoid:
		return 0
	}
	return t.Rows * t.Cols
}

func (t *Type) String() string {
	if t.IsArray() {
		return fmt.Sprintf("%s[%d]", t.Elem, t.ArrayLen)
	}
	switch t.Base {
	case BVoid:
		return "void"
	case BStruct:
		return t.Struct.Name
	case BSampler:
		return t.Sampler
	case BString:
		return "string"
	}
	name := map[Base]string{BFloat: "float", BInt: "int", BBool: "bool"}[t.Base]
	switch {
	case t.Rows == 1 && t.Cols == 1:
		return name
	case t.Rows == 1:
		return fmt.Sprintf("%s%d", name, t.Cols)
	}
	return fmt.Sprintf("%s%dx%d", name, t.Rows, t.Cols)
}

func scalarType(b Base) *Type        { return &Type{Base: b, Rows: 1, Cols: 1} }
func vectorType(b Base, n int) *Type { return &Type{Base: b, Rows: 1, Cols: n} }

var (
	tyVoid  = &Type{Base: BVoid}
	tyFloat = scalarType(BFloat)
	tyInt   = scalarType(BInt)
	tyBool  = scalarType(BBool)
)

type StructDef struct {
	Name   string
	Fields []*Field
}

type Field struct {
	Name     string
	Type     *Type
	Semantic string
	offset   int
}

func (s *StructDef) field(name string) *Field {
	for _, f := range s.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// Expressions.
type Expr interface{ line() int }

type (
	IdentExpr struct {
		Name string
		Line int
	}
	NumberExpr struct {
		Value float64
		IsInt bool
		Line  int
	}
	BoolExpr struct {
		Value bool
		Line  int
	}
	// CallExpr is a function call, intrinsic, or constructor (when Ctor != nil).
	CallExpr struct {
		Name string
		Ctor *Type
		Args []Expr
		Line int
	}
	CastExpr struct {
		To   *Type
		X    Expr
		Line int
	}
	MemberExpr struct {
		X    Expr
		Name string
		Line int
	}
	IndexExpr struct {
		X, Index Expr
		Line     int
	}
	UnaryExpr struct {
		Op   string
		X    Expr
		Line int
	}
	BinaryExpr struct {
		Op   string
		X, Y Expr
		Line int
	}
	CondExpr struct {
		Cond, X, Y Expr
		Line       int
	}
	AssignExpr struct {
		Op   string // "=", "+=", ...
		LHS  Expr
		RHS  Expr
		Line int
	}
	// InitListExpr is a brace initializer { a, b, ... }.
	InitListExpr struct {
		Items []Expr
		Line  int
	}
)

func (e *IdentExpr) line() int    { return e.Line }
func (e *NumberExpr) line() int   { return e.Line }
func (e *BoolExpr) line() int     { return e.Line }
func (e *CallExpr) line() int     { return e.Line }
func (e *CastExpr) line() int     { return e.Line }
func (e *MemberExpr) line() int   { return e.Line }
func (e *IndexExpr) line() int    { return e.Line }
func (e *UnaryExpr) line() int    { return e.Line }
func (e *BinaryExpr) line() int   { return e.Line }
func (e *CondExpr) line() int     { return e.Line }
func (e *AssignExpr) line() int   { return e.Line }
func (e *InitListExpr) line() int { return e.Line }

// Statements.
type Stmt interface{ stmtLine() int }

type (
	BlockStmt struct {
		List []Stmt
		Line int
	}
	DeclStmt struct {
		Vars []*VarDecl
		Line int
	}
	ExprStmt struct {
		X    Expr
		Line int
	}
	IfStmt struct {
		Cond Expr
		Then Stmt
		Else Stmt
		Line int
	}
	ReturnStmt struct {
		X    Expr
		Line int
	}
)

func (s *BlockStmt) stmtLine() int  { return s.Line }
func (s *DeclStmt) stmtLine() int   { return s.Line }
func (s *ExprStmt) stmtLine() int   { return s.Line }
func (s *IfStmt) stmtLine() int     { return s.Line }
func (s *ReturnStmt) stmtLine() int { return s.Line }

// VarDecl is a global or local variable.
type VarDecl struct {
	Name     string
	Type     *Type
	Uniform  bool
	Shared   bool
	Static   bool
	Const    bool
	Semantic string
	Register string // "c16", "s0", ... ("" = unassigned)
	Init     Expr
	Line     int
}

type Param struct {
	Name     string
	Type     *Type
	Out, In  bool
	Semantic string
}

type FuncDecl struct {
	Name     string
	Ret      *Type
	Params   []*Param
	Body     *BlockStmt // nil for prototypes
	Semantic string
	Line     int
}

// File is a parsed translation unit.
type File struct {
	Structs map[string]*StructDef
	Globals []*VarDecl
	Funcs   []*FuncDecl // definitions only, in source order
}

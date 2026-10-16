// Package hlsl compiles the subset of HLSL (Shader Model 3) used by D3D9 games that generate their
// shaders at runtime and hand the source to D3DXCompileShader, and evaluates the result on the CPU.
//
// Supported: object-like #define, structs, global/uniform declarations with semantics and
// register(cN/sN) bindings, functions with in/out/inout parameters and overloading, local
// declarations, if/else, return, assignment operators, swizzles, constructors, casts, arrays,
// and the intrinsics listed in builtins.go. Not supported (rejected by the parser): loops,
// function-like macros, conditional compilation.
package hlsl

import (
	"fmt"
	"strings"
)

type tokKind uint8

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tString
	tPunct
	tDirective // '#' at the start of a line; text holds the directive line after '#'
)

type token struct {
	kind tokKind
	text string
	line int
	// number value (tNumber)
	num   float64
	isInt bool
}

func (t token) String() string {
	if t.kind == tEOF {
		return "end of input"
	}
	return fmt.Sprintf("%q (line %d)", t.text, t.line)
}

var punct3 = []string{"<<=", ">>="}
var punct2 = []string{"+=", "-=", "*=", "/=", "%=", "==", "!=", "<=", ">=", "&&", "||", "++", "--", "<<", ">>", "::"}

// lex splits src into tokens. NUL bytes and CR are treated as whitespace (generated sources
// often carry trailing NULs and CRLF line endings).
func lex(src string) ([]token, error) {
	var toks []token
	line := 1
	lineStart := true
	i := 0
	for i < len(src) {
		ch := src[i]
		switch {
		case ch == '\n':
			line++
			lineStart = true
			i++
			continue
		case ch == ' ' || ch == '\t' || ch == '\r' || ch == 0 || ch == '\f' || ch == '\v':
			i++
			continue
		case ch == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		case ch == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated comment", line)
			}
			line += strings.Count(src[i:i+2+end], "\n")
			i += end + 4
			continue
		case ch == '#' && lineStart:
			j := i + 1
			for j < len(src) && src[j] != '\n' {
				if src[j] == '\\' && j+1 < len(src) && src[j+1] == '\n' {
					return nil, fmt.Errorf("line %d: line continuations in directives are not supported", line)
				}
				j++
			}
			text := src[i+1 : j]
			if k := strings.Index(text, "//"); k >= 0 {
				text = text[:k]
			}
			toks = append(toks, token{kind: tDirective, text: strings.TrimSpace(text), line: line})
			i = j
			continue
		}
		lineStart = false
		switch {
		case isIdentStart(ch):
			j := i + 1
			for j < len(src) && isIdentChar(src[j]) {
				j++
			}
			toks = append(toks, token{kind: tIdent, text: src[i:j], line: line})
			i = j
		case isDigit(ch) || (ch == '.' && i+1 < len(src) && isDigit(src[i+1])):
			t, n, err := lexNumber(src[i:], line)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i += n
		case ch == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' && src[j] != '\n' {
				j++
			}
			if j >= len(src) || src[j] != '"' {
				return nil, fmt.Errorf("line %d: unterminated string", line)
			}
			toks = append(toks, token{kind: tString, text: src[i+1 : j], line: line})
			i = j + 1
		default:
			n := 1
			for _, p := range punct3 {
				if strings.HasPrefix(src[i:], p) {
					n = 3
				}
			}
			if n == 1 {
				for _, p := range punct2 {
					if strings.HasPrefix(src[i:], p) {
						n = 2
					}
				}
			}
			if n == 1 && !strings.ContainsRune("+-*/%=<>!&|^~(){}[],;:.?", rune(ch)) {
				return nil, fmt.Errorf("line %d: unexpected character %q", line, ch)
			}
			toks = append(toks, token{kind: tPunct, text: src[i : i+n], line: line})
			i += n
		}
	}
	toks = append(toks, token{kind: tEOF, line: line})
	return toks, nil
}

func lexNumber(s string, line int) (token, int, error) {
	i := 0
	isInt := true
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		i = 2
		var v uint64
		for i < len(s) && isHex(s[i]) {
			v = v*16 + uint64(hexVal(s[i]))
			i++
		}
		for i < len(s) && (s[i] == 'u' || s[i] == 'U' || s[i] == 'l' || s[i] == 'L') {
			i++
		}
		return token{kind: tNumber, text: s[:i], line: line, num: float64(v), isInt: true}, i, nil
	}
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i < len(s) && s[i] == '.' {
		isInt = false
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			isInt = false
			i = j
			for i < len(s) && isDigit(s[i]) {
				i++
			}
		}
	}
	text := s[:i]
	var v float64
	if _, err := fmt.Sscan(text, &v); err != nil {
		return token{}, 0, fmt.Errorf("line %d: bad number %q", line, text)
	}
	for i < len(s) && strings.IndexByte("fFhHuUlL", s[i]) >= 0 {
		if s[i] == 'f' || s[i] == 'F' || s[i] == 'h' || s[i] == 'H' {
			isInt = false
		}
		i++
	}
	return token{kind: tNumber, text: s[:i], line: line, num: v, isInt: isInt}, i, nil
}

func isIdentStart(c byte) bool { return c == '_' || (c|0x20) >= 'a' && (c|0x20) <= 'z' }
func isIdentChar(c byte) bool  { return isIdentStart(c) || isDigit(c) }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isHex(c byte) bool        { return isDigit(c) || (c|0x20) >= 'a' && (c|0x20) <= 'f' }
func hexVal(c byte) int {
	if isDigit(c) {
		return int(c - '0')
	}
	return int(c|0x20-'a') + 10
}

// preprocess expands object-like #define macros in order of appearance and drops other
// directives (#pragma). Macros may be (re)defined anywhere, including inside declarations.
func preprocess(toks []token) ([]token, error) {
	macros := map[string][]token{}
	var out []token
	var expand func(t token, active map[string]bool, depth int) error
	expand = func(t token, active map[string]bool, depth int) error {
		if t.kind != tIdent {
			out = append(out, t)
			return nil
		}
		body, ok := macros[t.text]
		if !ok || active[t.text] {
			out = append(out, t)
			return nil
		}
		if depth > 64 {
			return fmt.Errorf("line %d: macro expansion too deep at %q", t.line, t.text)
		}
		active[t.text] = true
		for _, b := range body {
			b.line = t.line
			if err := expand(b, active, depth+1); err != nil {
				return err
			}
		}
		delete(active, t.text)
		return nil
	}
	for _, t := range toks {
		if t.kind != tDirective {
			if err := expand(t, map[string]bool{}, 0); err != nil {
				return nil, err
			}
			continue
		}
		fields := strings.Fields(t.text)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "define":
			if len(fields) < 2 {
				return nil, fmt.Errorf("line %d: #define without a name", t.line)
			}
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t.text[len("define"):]), fields[1]))
			if strings.HasPrefix(t.text[strings.Index(t.text, fields[1])+len(fields[1]):], "(") {
				return nil, fmt.Errorf("line %d: function-like macro %q is not supported", t.line, fields[1])
			}
			body, err := lex(rest)
			if err != nil {
				return nil, fmt.Errorf("line %d: in #define %s: %v", t.line, fields[1], err)
			}
			macros[fields[1]] = body[:len(body)-1] // drop EOF
		case "undef":
			if len(fields) >= 2 {
				delete(macros, fields[1])
			}
		case "pragma", "line":
		default:
			return nil, fmt.Errorf("line %d: unsupported directive #%s", t.line, fields[0])
		}
	}
	return out, nil
}

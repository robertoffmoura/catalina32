package source

import (
	"fmt"
	"strconv"
	"strings"
)

// GLCall is one parsed GLSpec entry.
type GLCall struct {
	Name           string
	Custom, Viewer bool // flags E and V
	Args           []GLArg
}

// GLArg is one argument of a GLCall.
type GLArg struct {
	Kind byte // 'i' 'l' 'f' 'd' 's' 'p', 'o' (object), 'g' (names to create), 'x' (names to delete), '*' (data)
	Obj  byte // for o, g, x: the object kind letter (T B F R Q P H)
	Mul  int  // for '*': bytes per unit
	Of   int  // for '*': the argument counting the units, or -1 (Mul bytes)
}

// ParseGLSpec parses GLSpec (panicking on a malformed entry: it is a constant).
func ParseGLSpec() []GLCall {
	var out []GLCall
	for _, line := range GLSpec {
		head, args, ok := strings.Cut(line, ":")
		if !ok {
			panic("glspec: no colon: " + line)
		}
		f := strings.Fields(head)
		call := GLCall{Name: f[0]}
		if len(f) > 1 {
			call.Custom = strings.Contains(f[1], "E")
			call.Viewer = strings.Contains(f[1], "V")
		}
		for _, a := range strings.Fields(args) {
			switch {
			case len(a) == 1 && strings.Contains("ilfdsp", a):
				call.Args = append(call.Args, GLArg{Kind: a[0]})
			case len(a) == 1 && strings.Contains("TBFRQPH", a):
				call.Args = append(call.Args, GLArg{Kind: 'o', Obj: a[0]})
			case len(a) == 2 && (a[0] == 'g' || a[0] == 'x') && strings.Contains("TBFRQP", a[1:]):
				call.Args = append(call.Args, GLArg{Kind: a[0], Obj: a[1]})
			case a[0] == '*':
				mul, of, hasOf := strings.Cut(a[1:], "@")
				n, err := strconv.Atoi(mul)
				if err != nil {
					panic("glspec: bad size in " + line)
				}
				arg := GLArg{Kind: '*', Mul: n, Of: -1}
				if hasOf {
					if arg.Of, err = strconv.Atoi(of); err != nil {
						panic("glspec: bad size in " + line)
					}
				}
				call.Args = append(call.Args, arg)
			default:
				panic(fmt.Sprintf("glspec: bad argument %q in %s", a, line))
			}
		}
		out = append(out, call)
	}
	return out
}

package main

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"

	"catalina32/pkg/source"
)

// C32_GLXML=<Khronos gl.xml> go test ./cmd/glgen: every call the viewer makes with the
// stream's arguments as they are (those without a hand-written handler, flag V) must have the
// GL function's parameters: the same count, and integers, floats, doubles and pointers in the
// same places. (The registry: https://github.com/KhronosGroup/OpenGL-Registry, xml/gl.xml.)
func TestSpecMatchesRegistry(t *testing.T) {
	path := os.Getenv("C32_GLXML")
	if path == "" {
		t.Skip("C32_GLXML not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Commands []struct {
			Proto struct {
				Name string `xml:"name"`
			} `xml:"proto"`
			Params []struct {
				Inner string `xml:",innerxml"`
				Ptype string `xml:"ptype"`
			} `xml:"param"`
		} `xml:"commands>command"`
	}
	if err := xml.Unmarshal(data, &reg); err != nil {
		t.Fatal(err)
	}
	params := map[string][]string{} // name -> parameter classes
	for _, c := range reg.Commands {
		var cl []string
		for _, p := range c.Params {
			switch {
			case strings.Contains(p.Inner, "*"):
				cl = append(cl, "ptr")
			case p.Ptype == "GLfloat" || p.Ptype == "GLclampf":
				cl = append(cl, "f")
			case p.Ptype == "GLdouble" || p.Ptype == "GLclampd":
				cl = append(cl, "d")
			case strings.Contains(p.Ptype, "intptr") || strings.Contains(p.Ptype, "sizeiptr"):
				cl = append(cl, "l")
			case p.Ptype == "GLhandleARB":
				cl = append(cl, "H")
			default:
				cl = append(cl, "i")
			}
		}
		params[c.Proto.Name] = cl
	}
	for _, c := range source.ParseGLSpec() {
		if c.Viewer || strings.HasPrefix(c.Name, "c32") {
			continue
		}
		want, ok := params[c.Name]
		if !ok {
			t.Errorf("%s: not in the registry", c.Name)
			continue
		}
		var have []string
		for _, a := range c.Args {
			switch a.Kind {
			case 's', '*', 'p', 'g', 'x':
				have = append(have, "ptr")
			case 'f', 'd', 'l':
				have = append(have, string(a.Kind))
			case 'o':
				if a.Obj == 'H' {
					have = append(have, "H")
				} else {
					have = append(have, "i")
				}
			default:
				have = append(have, "i")
			}
		}
		// GLSL objects: GL 2.0 functions take GLuint, the ARB ones GLhandleARB; both are handles here.
		norm := func(s []string) string { return strings.ReplaceAll(strings.Join(s, " "), "H", "i") }
		if norm(have) != norm(want) {
			t.Errorf("%s: the stream has (%s), GL has (%s): add a handler (flag V) or fix the entry",
				c.Name, strings.Join(have, " "), strings.Join(want, " "))
		}
	}
}

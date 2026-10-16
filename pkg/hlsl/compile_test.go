package hlsl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileDumpedShaders(t *testing.T) {
	for _, f := range shaderFiles(t) {
		src, _ := os.ReadFile(f)
		file, err := Parse(string(src))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
		entry := "vertexProgram"
		if strings.Contains(filepath.Base(f), "ps_") {
			entry = "fragmentProgram"
		}
		p, err := Compile(file, entry)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if len(p.Outputs) == 0 {
			t.Errorf("%s: no outputs", filepath.Base(f))
		}
		s := p.NewState()
		s.LoadConstants(make([][4]float32, 256))
		s.Run()
	}
}

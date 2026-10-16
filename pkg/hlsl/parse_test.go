package hlsl

import (
	"os"
	"path/filepath"
	"testing"
)

// shaderFiles returns the HLSL sources in $HLSL_TEST_DIR (shaders dumped from a game with
// C32_DUMP_SHADERS; they are game content and are not part of the repository).
func shaderFiles(t *testing.T) []string {
	dir := os.Getenv("HLSL_TEST_DIR")
	if dir == "" {
		t.Skip("set HLSL_TEST_DIR to a directory of dumped .hlsl files")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.hlsl"))
	if len(files) == 0 {
		t.Fatalf("no .hlsl files in %s", dir)
	}
	return files
}

func TestParseDumpedShaders(t *testing.T) {
	for _, f := range shaderFiles(t) {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := Parse(string(src))
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if len(file.Funcs) == 0 {
			t.Errorf("%s: no functions parsed", filepath.Base(f))
		}
	}
}

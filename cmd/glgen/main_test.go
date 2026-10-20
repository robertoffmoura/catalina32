package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The committed decoder must match the spec (run `go generate ./pkg/source` after changing it).
func TestGeneratedHeaderUpToDate(t *testing.T) {
	have, err := os.ReadFile(filepath.Join("..", "..", "viewer", "C32GLCalls.h"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, Generate()) {
		t.Fatal("viewer/C32GLCalls.h is out of date: run go generate ./pkg/source")
	}
}

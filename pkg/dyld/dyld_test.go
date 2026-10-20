package dyld

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestLEB(t *testing.T) {
	p := 0
	if v := uleb([]byte{0xe5, 0x8e, 0x26}, &p); v != 624485 || p != 3 {
		t.Fatalf("uleb = %d (%d bytes)", v, p)
	}
	p = 0
	if v := sleb([]byte{0xc0, 0xbb, 0x78}, &p); v != -123456 {
		t.Fatalf("sleb = %d", v)
	}
}

// With a Source game folder (C32_SOURCE_DIR, the one holding hl2_osx), link the executable,
// then open every module the way the game does, and report what the system has to provide.
func TestLinkSourceGame(t *testing.T) {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set")
	}
	mem := make([]byte, 1<<30)
	systemSyms := map[string]map[string]bool{}
	next := uint32(0xf0000000)
	l := &Linker{
		Mem: mem, Lo: 0x00100000, Hi: 0x3f000000,
		Search: []string{filepath.Join(dir, "bin")},
		System: func(lib, sym string) uint32 {
			if systemSyms[lib] == nil {
				systemSyms[lib] = map[string]bool{}
			}
			systemSyms[lib][sym] = true
			next += 16
			return next
		},
		Skip: func(path string) bool { return filepath.Base(path) == "libcef.dylib" },
	}
	if _, err := l.LoadExecutable(filepath.Join(dir, "hl2_osx")); err != nil {
		t.Fatal(err)
	}
	var mods []string
	for _, sub := range []string{"bin", filepath.Join("portal", "bin")} {
		names, _ := filepath.Glob(filepath.Join(dir, sub, "*.dylib"))
		mods = append(mods, names...)
	}
	for _, m := range mods {
		if filepath.Base(m) == "libcef.dylib" {
			continue
		}
		if _, err := l.Open(m, nil); err != nil {
			t.Errorf("open %s: %v", filepath.Base(m), err)
		}
	}
	t.Logf("%d images linked, %.1f MB of guest memory (%#x-%#x), %d initialisers",
		len(l.Images), float64(l.next-l.Lo)/(1<<20), l.Lo, l.next, len(l.Inits))
	// Imports a dependency doesn't export are bound lazily by the real dyld, so they only fail
	// when called (sourcevr imports VR_* from an older libsteam_api than the one shipped).
	for _, line := range l.UnresolvedReport() {
		t.Logf("missing guest symbols: %s", line)
	}
	var libs []string
	total := 0
	for lib, syms := range systemSyms {
		libs = append(libs, lib)
		total += len(syms)
	}
	sort.Slice(libs, func(i, j int) bool { return len(systemSyms[libs[i]]) > len(systemSyms[libs[j]]) })
	t.Logf("%d system symbols from %d libraries:", total, len(libs))
	for _, lib := range libs {
		t.Logf("  %4d %s", len(systemSyms[lib]), lib)
	}
	// Spot checks: CreateInterface resolves in engine.dylib; stubs point at their targets.
	eng := l.byPath[filepath.Clean(filepath.Join(dir, "bin", "engine.dylib"))]
	if eng == nil {
		t.Fatal("engine.dylib not loaded")
	}
	ci, ok := l.Symbol(eng, "_CreateInterface")
	if !ok || l.ImageAt(ci) != eng {
		t.Fatalf("engine CreateInterface = %#x %v", ci, ok)
	}
	if lp, n, ok := eng.Section("__DATA", "__la_symbol_ptr"); ok {
		unbound := 0
		for off := uint32(0); off < n; off += 4 {
			v := binary.LittleEndian.Uint32(mem[lp+off:])
			if img := l.ImageAt(v); img == eng && strings.Contains(sectionOf(eng, v-eng.Slide), "__stub_helper") {
				unbound++
			}
		}
		if unbound > 0 {
			t.Errorf("engine: %d lazy pointers still point at the stub helper", unbound)
		}
	}
}

func sectionOf(img *Image, a uint32) string {
	for _, s := range img.Segs {
		for _, sc := range s.Sections {
			if a >= sc.Addr && a < sc.Addr+sc.Size {
				return sc.Name
			}
		}
	}
	return ""
}

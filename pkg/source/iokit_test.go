package source

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"catalina32/pkg/cpu"
)

func TestPCIVendor(t *testing.T) {
	for s, want := range map[string]uint32{
		"ATI Technologies Inc.": 0x1002, "AMD": 0x1002, "NVIDIA Corporation": 0x10de, "Intel Inc.": 0x8086,
		"Intel Open Source Technology Center": 0x8086, "Apple": 0, "Mesa": 0, "": 0,
	} {
		if got := pciVendor(s); got != want {
			t.Errorf("pciVendor(%q) = %#x, want %#x", s, got, want)
		}
	}
}

// C32_SOURCE_DIR=<Portal> go test ./pkg/source -run RendererClipMode -v: on a window whose GPU
// is AMD's (glcheck answers as one), the launcher's renderer table sees an ATI part and turns
// off "native clip vertex" mode, so ToGL adjusts portal clip planes for AMD's drivers itself.
// (Without the GPU's vendor it kept that mode, and AMD Macs drew portals with walls and floors
// cut away.)
func TestRendererClipMode(t *testing.T) {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set")
	}
	glPath := filepath.Join(t.TempDir(), "gl.shm")
	t.Setenv("C32_GL_SHM", glPath)
	stop, checked := make(chan struct{}), make(chan struct{})
	go func() {
		if _, err := RunGLCheck(glPath, stop); err != nil {
			t.Errorf("glcheck: %v", err)
		}
		close(checked)
	}()
	m, err := New(dir, []string{"-game", "portal"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type info struct{ ati, nativeClip byte }
	var seen []info
	m.Around(regexp.MustCompile(`^__ZN15GLMRendererInfoC2EP21GLMRendererInfoFields$`), func(_ string, c *cpu.CPU) func(*cpu.CPU) {
		this := c.Arg(0)
		return func(c *cpu.CPU) { // GLMRendererInfoFields: m_ati at 0xd0, m_hasNativeClipVertexMode at 0xe2
			seen = append(seen, info{c.Read8(this + 0xd0), c.Read8(this + 0xe2)})
		}
	})
	m.Run(1_000_000_000)
	close(stop)
	<-checked
	t.Logf("renderers: %+v", seen)
	if len(seen) == 0 {
		t.Fatal("no renderer was described")
	}
	for _, r := range seen {
		if r.ati != 1 || r.nativeClip != 0 {
			t.Errorf("renderer %+v: want an ATI part without native clip vertex mode", r)
		}
	}
}

package source

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"catalina32/pkg/cpu"
)

// C32_SOURCE_DIR=<folder with hl2_osx> [C32_STEPS=n] go test ./pkg/source -run Boot -v
// boots the game headless and reports how far it got and what it called that isn't there.
func TestBoot(t *testing.T) {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set")
	}
	steps := uint64(200_000_000)
	if v, err := strconv.ParseUint(os.Getenv("C32_STEPS"), 10, 64); err == nil {
		steps = v
	}
	logPath := filepath.Join(t.TempDir(), "boot.log")
	if p := os.Getenv("C32_LOG"); p != "" {
		logPath = p
	}
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	args := []string{"-game", "portal"}
	if a := os.Getenv("C32_ARGS"); a != "" {
		args = append(args, strings.Fields(a)...)
	}
	overlay := t.TempDir()
	if p := os.Getenv("C32_OVERLAY"); p != "" {
		overlay = p
	}
	m, err := New(dir, args, overlay, log)
	if err != nil {
		t.Fatal(err)
	}
	if re := os.Getenv("C32_PROBE"); re != "" {
		m.Probe(regexp.MustCompile(re), 2)
	}
	if os.Getenv("C32_WATCH_NULL") != "" { // writes to page zero: NULL-pointer stores the Mac would fault on
		n := 0
		m.C.SetWatch(0, 0x1000, func(c *cpu.CPU, addr uint32, size int, write bool) {
			if n++; write && n <= 1000 {
				t.Logf("page-zero %v of %d bytes at %#x by %s", map[bool]string{true: "write", false: "read"}[write], size, addr,
					strings.Join(m.Backtrace(12), " < "))
			}
		})
	}
	err = m.Run(steps)
	t.Logf("stopped after %d instructions: %v", m.C.Steps, err)
	t.Logf("registers: %08x eip %08x", m.C.Regs, m.C.EIP)
	t.Logf("backtrace:\n  %s", strings.Join(m.Backtrace(16), "\n  "))
	t.Logf("%d images, %d initialisers run, %d threads, %d frames presented, heap %d MB in use (peak %d MB), log in %s",
		len(m.L.Images), m.initsRun, len(m.threads.threads), m.sdl.frames, m.C.HeapInUse()>>20, m.C.HeapPeak()>>20, logPath)
	for _, line := range m.Report() {
		t.Logf("unimplemented: %s", line)
	}
}

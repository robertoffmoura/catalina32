package source

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	var check *GLCheck
	checked := make(chan struct{})
	stopCheck := make(chan struct{})
	if os.Getenv("C32_GLCHECK") != "" { // the OpenGL stream, consumed by the Go stand-in for the viewer
		path := filepath.Join(t.TempDir(), "gl.shm")
		t.Setenv("C32_GL_SHM", path)
		go func() {
			var err error
			if check, err = RunGLCheck(path, stopCheck); err != nil {
				t.Errorf("glcheck: %v", err)
			}
			close(checked)
		}()
	} else {
		close(checked)
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
	if os.Getenv("C32_TRACECALLS") != "" {
		m.C.TraceCalls = true
	}
	if v := os.Getenv("C32_CMDS"); v != "" { // console commands typed in at instruction counts: "steps:cmd;cmd|steps:..."
		type timed struct {
			at   uint64
			cmds string
		}
		var queue []timed
		for _, part := range strings.Split(v, "|") {
			at, cmds, _ := strings.Cut(part, ":")
			n, err := strconv.ParseUint(at, 10, 64)
			if err != nil {
				t.Fatalf("C32_CMDS: %q: %v", part, err)
			}
			queue = append(queue, timed{n, cmds})
		}
		m.OnPoll = func(c *cpu.CPU) {
			for len(queue) > 0 && c.Steps >= queue[0].at {
				addText := m.symbolAddr("__Z12Cbuf_AddTextPKc")
				s := c.Malloc(uint32(len(queue[0].cmds) + 2))
				c.WriteCString(s, queue[0].cmds+"\n")
				c.CallGuest(addText, s)
				t.Logf("at %d (frame %d): %s", c.Steps, m.sdl.frames, queue[0].cmds)
				queue = queue[1:]
			}
		}
	}
	err = m.Run(steps)
	if m.C.TraceCalls {
		for _, f := range m.C.CallStack {
			t.Logf("call %s -> %s", m.where(f.CallerEIP), m.where(f.TargetEIP))
		}
	}
	close(stopCheck)
	<-checked
	if check != nil {
		t.Logf("glcheck: %d commands, %.1f MB, %d frames, %d questions; errors: %q",
			check.Commands, float64(check.Bytes)/(1<<20), check.Frames, check.Questions, check.Errors)
		if len(check.Errors) > 0 {
			t.Error("the OpenGL stream has errors")
		}
	}
	t.Logf("stopped after %d instructions: %v", m.C.Steps, err)
	t.Logf("registers: %08x eip %08x", m.C.Regs, m.C.EIP)
	t.Logf("backtrace:\n  %s", strings.Join(m.Backtrace(16), "\n  "))
	t.Logf("%d images, %d initialisers run, %d threads, %d frames presented, heap %d MB in use (peak %d MB), log in %s",
		len(m.L.Images), m.initsRun, len(m.threads.threads), m.sdl.frames, m.C.HeapInUse()>>20, m.C.HeapPeak()>>20, logPath)
	if os.Getenv("C32_GLSTATS") != "" {
		type kv struct {
			k string
			v uint64
		}
		var l []kv
		for k, v := range m.gl.calls {
			l = append(l, kv{k, v})
		}
		sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v })
		for _, e := range l {
			t.Logf("gl %10d %s", e.v, e.k)
		}
	}
	for _, line := range m.Report() {
		t.Logf("unimplemented: %s", line)
	}
}

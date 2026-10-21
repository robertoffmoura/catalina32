package source

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	if v := os.Getenv("C32_CMDS"); v != "" {
		m.OnPoll = consoleAt(t, m, v)
	}
	if v := os.Getenv("C32_JITSTATS"); v != "" { // a number: count end exits from that instruction on
		from, _ := strconv.ParseUint(v, 10, 64)
		if from <= 1 {
			m.C.Run0JITStats()
		} else {
			prev := m.OnPoll
			m.OnPoll = func(c *cpu.CPU) {
				if prev != nil {
					prev(c)
				}
				if from != 0 && c.Steps >= from {
					from = 0
					c.Run0JITStats()
				}
			}
		}
	}
	if os.Getenv("C32_SLOWOPS") != "" {
		m.C.SlowOps = map[uint32]uint64{}
		defer func() {
			type kv struct {
				op uint32
				n  uint64
			}
			var top []kv
			for op, n := range m.C.SlowOps {
				top = append(top, kv{op, n})
			}
			sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
			for i := 0; i < len(top) && i < 100; i++ {
				t.Logf("  slow op %06x: %d", top[i].op, top[i].n)
			}
		}()
	}
	if p := os.Getenv("C32_FRAMESTATS"); p != "" { // a CSV line per frame: what it took
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fmt.Fprintln(f, "frame,steps,instructions,full_decodes,host_ms,cpu_ms,host_calls,gl_commands,gl_kb,jit_slow,jit_end,jit_resets")
		var last struct {
			steps, full, calls, cmds, bytes, slow, end uint64
			at                                         time.Time
			cpu                                        time.Duration
		}
		last.at, last.cpu = time.Now(), processCPU()
		frame := 0
		m.OnFrame = func(c *cpu.CPU) {
			frame++
			now, st := time.Now(), c.JITStats()
			cpuNow := processCPU()
			fmt.Fprintf(f, "%d,%d,%d,%d,%.2f,%.2f,%d,%d,%.1f,%d,%d,%d\n", frame, c.Steps, c.Steps-last.steps, c.FullDecodes-last.full,
				float64(now.Sub(last.at).Microseconds())/1000, float64((cpuNow-last.cpu).Microseconds())/1000, c.HostCalls-last.calls, m.glr.sent-last.cmds,
				float64(m.glr.w-last.bytes)/1024, st.SlowExits-last.slow, st.EndExits-last.end, st.Resets)
			last.steps, last.full, last.calls, last.cmds, last.bytes, last.slow, last.end, last.at, last.cpu = c.Steps, c.FullDecodes, c.HostCalls, m.glr.sent, m.glr.w, st.SlowExits, st.EndExits, now, cpuNow
		}
	}
	if p := os.Getenv("C32_CPUPROF"); p != "" { // the host's CPU profile from instruction C32_CPUPROF_AFTER on
		after, _ := strconv.ParseUint(os.Getenv("C32_CPUPROF_AFTER"), 10, 64)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		started := false
		start := func() {
			if !started && m.C.Steps >= after {
				started = true
				if err := pprof.StartCPUProfile(f); err != nil {
					t.Fatal(err)
				}
				t.Logf("CPU profile from instruction %d", m.C.Steps)
			}
		}
		if prev := m.OnFrame; prev != nil {
			m.OnFrame = func(c *cpu.CPU) { prev(c); start() }
		} else {
			m.OnFrame = func(c *cpu.CPU) { start() }
		}
		defer func() {
			if started {
				pprof.StopCPUProfile()
			}
		}()
	}
	err = m.Run(steps)
	if v := os.Getenv("C32_JITDUMP"); v != "" { // eip,eip...: write those blocks' host and guest code to the temporary folder
		for _, a := range strings.Split(v, ",") {
			eip, _ := strconv.ParseUint(a, 0, 32)
			host, guest := m.C.JITCode(uint32(eip))
			os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("jit_%x.host", eip)), host, 0o644)
			os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("jit_%x.guest", eip)), guest, 0o644)
			t.Logf("jit dump %#x: %d host bytes for %d guest bytes", eip, len(host), len(guest))
		}
	}
	if os.Getenv("C32_JITPROF") == "1" {
		total, runs, top := m.C.JITProfile()
		t.Logf("jit profile: %d block runs; per run: %.1f insts, %.0f bytes, %.2f flag saves, %.2f flag loads, %.2f merges, %.2f dynamic merges, %.2f loads, %.2f stores",
			runs, float64(total.Insts)/float64(runs), float64(total.Bytes)/float64(runs), float64(total.FlagSaves)/float64(runs),
			float64(total.FlagLoads)/float64(runs), float64(total.Merges)/float64(runs), float64(total.DynMerges)/float64(runs),
			float64(total.Loads)/float64(runs), float64(total.Stores)/float64(runs))
		for _, l := range top {
			t.Logf("  %s", l)
		}
	}
	if os.Getenv("C32_JITSTATS") != "" {
		st := m.C.JITStats()
		t.Logf("jit: %d translated (%d KB), %d entries, %d slow, %d end, %d limit exits, %d resets",
			st.Translated, st.Bytes>>10, st.Entries, st.SlowExits, st.EndExits, st.LimitExits, st.Resets)
		type kv struct {
			eip uint32
			n   uint64
		}
		var top []kv
		for e, n := range st.EndAt {
			top = append(top, kv{e, n})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
		kinds := map[string]uint64{}
		for _, e := range top {
			kinds[m.C.JITTargetKind(e.eip)] += e.n
		}
		t.Logf("  end exits by what runs there: %v", kinds)
		for i := 0; i < len(top) && i < 25; i++ {
			t.Logf("  end exit to %#x (%s, %s): %d", top[i].eip, m.where(top[i].eip), m.C.JITTargetKind(top[i].eip), top[i].n)
		}
	}
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

// consoleAt returns an OnPoll hook that types console commands in at instruction counts, as
// spec says: "steps:cmd;cmd|steps:...".
func consoleAt(t *testing.T, m *Machine, spec string) func(c *cpu.CPU) {
	type timed struct {
		at   uint64
		cmds string
	}
	var queue []timed
	for _, part := range strings.Split(spec, "|") {
		at, cmds, _ := strings.Cut(part, ":")
		n, err := strconv.ParseUint(at, 10, 64)
		if err != nil {
			t.Fatalf("console commands: %q: %v", part, err)
		}
		queue = append(queue, timed{n, cmds})
	}
	return func(c *cpu.CPU) {
		for len(queue) > 0 && c.Steps >= queue[0].at {
			s := c.Malloc(uint32(len(queue[0].cmds) + 2))
			c.WriteCString(s, queue[0].cmds+"\n")
			c.CallGuest(m.symbolAddr("__Z12Cbuf_AddTextPKc"), s)
			t.Logf("at %d (frame %d): %s", c.Steps, m.sdl.frames, queue[0].cmds)
			queue = queue[1:]
		}
	}
}

// processCPU is the CPU time the process has used (user and system): steadier than the wall
// clock when other work shares the machine.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

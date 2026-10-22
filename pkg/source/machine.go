// Package source runs Valve's Mac Source engine games (hl2_osx and its dylibs: Portal, Half-Life
// 2...). It links the game's images into the guest address space (pkg/dyld) and provides the
// system libraries they import from the host: the C library, POSIX threads on green threads, the
// C++ runtime, dlopen. Experimental: see docs/PORTAL.md.
package source

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/image/font/sfnt"

	"catalina32/pkg/cpu"
	"catalina32/pkg/dyld"
)

// Guest memory layout. Code (the images) stays below the heap, so the interpreter's fast store
// path (above the code range) covers the whole heap.
const (
	imageLo  = 0x00100000 // dylibs, slid one after another
	imageHi  = 0x0d000000
	thunkLo  = 0x0e000000 // one address per host function
	thunkHi  = 0x0e800000
	dataLo   = 0x0e800000 // data the system libraries export (stdio FILEs, ctype tables...)
	dataHi   = 0x0f000000
	heapLo   = 0x10000000 // (cpu's sentinel return addresses lie just below)
	heapHi   = 0xbf000000
	stackTop = 0xbfff0000 // the main thread's stack, 16 MB below
)

// Machine is one running Source game.
type Machine struct {
	C       *cpu.CPU
	L       *dyld.Linker
	Dir     string    // game root (hl2.sh's folder), the working directory
	Args    []string  // command line after the program name ("-game", "portal")
	Overlay string    // files the game writes go below this folder (the game folder is never written)
	Log     io.Writer // diagnostics
	// Skip names guest libraries not to load: calls into them go to host stubs.
	Skip map[string]bool

	sys       map[string]uint32 // symbol -> host function or data address
	sysName   map[uint32]string
	shims     map[string]cpu.ThunkFunc
	thunkNext uint32
	dataNext  uint32
	initsRun  int
	Unimpl    map[string]int // unimplemented functions called, and how often
	unimplLib map[string]string

	argc                     uint32
	argv, envp, apple, pvars uint32
	env                      map[string]string
	cwd                      string
	dlerr                    string

	files              map[uint32]*gfile // FILE* -> open file
	fds                map[int32]*gfile  // file descriptor -> open file
	nextFD             int32
	dirs               map[uint32]*gdir
	threads            *sched
	mutexes            map[uint32]*gmutex
	conds              map[uint32]*gcond
	tlsKeys            uint32
	onces              map[uint32]bool
	strtokAt           uint32
	atexit             []uint32
	cxx                cxxState
	stopped            bool
	skew               uint64    // microseconds the clock jumped while every thread slept
	errno              uint32    // errno's address
	glr                *glRemote // the OpenGL stream (C32_GL_SHM), nil without one
	glOn               bool      // the viewer is drawing: GL calls go to the stream
	Live               bool      // wall-clock time (a player is watching), else time from the instruction count
	start              time.Time
	input              inputState
	pads               padInput
	hostFonts          fontRegistry
	OnPoll             func(c *cpu.CPU) // tests: called when the game polls for events (to feed it input)
	OnFrame            func(c *cpu.CPU) // diagnostics: called when the game presents a frame
	cfStrings          map[uint32]string
	cfData             map[uint32][]byte            // CFData objects -> their bytes
	folded             map[string]string            // fold's cache (libc.go)
	dirNames           map[string]map[string]string // fold's directory listings: lower-case name -> name
	ovMissing          map[string]bool              // overlay paths known not to exist (libc.go)
	deleted            map[string]bool              // game files the game deleted, lower case (libc.go); nil: not loaded
	sdl                sdlState
	gl                 nullGL
	fonts              fontState
	audio              audioState
	al                 openAL
	alBufferDataStatic uint32   // the extension alGetProcAddress hands out
	sndDSP             *sndDSP  // the sound effects in Go (snddsp.go), nil when not bound
	cmTrace            *cmTrace // collision traces in Go (cmtrace.go), nil when not bound
}

// New links the game in dir (the folder holding hl2_osx). With dir "", it only sets up the
// system libraries (for tests).
func New(dir string, args []string, overlay string, log io.Writer) (*Machine, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	m := &Machine{
		C: cpu.NewCPU(), Dir: abs, Args: args, Overlay: overlay, Log: log, cwd: abs,
		Skip: map[string]bool{
			"libcef.dylib":        true, // Chromium: HTML panels are optional
			"libSDL2-2.0.0.dylib": true, // windows and input: provided natively
			"libsteam_api.dylib":  true, // talks to the Steam client
		},
		sys: map[string]uint32{}, sysName: map[uint32]string{}, thunkNext: thunkLo, dataNext: dataLo,
		Unimpl: map[string]int{}, unimplLib: map[string]string{},
		env: map[string]string{"HOME": filepath.Join(overlay, "home"), "USER": "player",
			"DYLD_LIBRARY_PATH": filepath.Join(abs, "bin"), "SteamAppId": readTrim(filepath.Join(abs, "steam_appid.txt"))},
		files: map[uint32]*gfile{}, fds: map[int32]*gfile{}, nextFD: 3, dirs: map[uint32]*gdir{},
		mutexes: map[uint32]*gmutex{}, conds: map[uint32]*gcond{}, onces: map[uint32]bool{},
	}
	m.start = time.Now()
	m.hostFonts = fontRegistry{byObj: map[uint32]*hostFont{}, parsed: map[string]*sfnt.Font{}}
	m.glr = newGLRemote(m)
	m.openFramebufferShm()
	m.Live = os.Getenv("C32_LIVE") == "1"
	m.C.SetHeap(heapLo, heapHi)
	m.registerShims()
	m.errno = m.data(4)
	m.C.FSBase, m.C.GSBase = m.data(4096), m.data(4096)
	m.C.SetCodeRange(0x1000, imageHi)
	m.L = &dyld.Linker{
		Mem: m.C.Mem, Lo: imageLo, Hi: imageHi,
		Search: []string{filepath.Join(abs, "bin")},
		System: m.system,
		Skip:   func(p string) bool { return m.Skip[filepath.Base(p)] },
		Mapped: m.mapped,
	}
	m.Watch(regexp.MustCompile(`^__Z22Host_ReadConfigurationv$`), func(_ string, c *cpu.CPU) { m.announcePads(c) })
	m.initStdio()
	m.initThreads()
	if dir == "" { // the system libraries alone (tests)
		return m, nil
	}
	if _, err := m.L.LoadExecutable(filepath.Join(abs, "hl2_osx")); err != nil {
		return nil, err
	}
	m.setupProcess()
	return m, nil
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func (m *Machine) logf(format string, args ...any) {
	if m.Log != nil {
		fmt.Fprintf(m.Log, format+"\n", args...)
	}
}

// mapped marks a freshly linked image's code pages, so data stores beside them stay fast.
func (m *Machine) mapped(img *dyld.Image) {
	for _, s := range img.Segs {
		if s.InitPro&4 != 0 && s.Name != "__PAGEZERO" {
			m.C.MarkTextPages(s.VMAddr+img.Slide, s.VMAddr+img.Slide+s.VMSize)
		}
	}
	lo, hi := img.Span()
	m.logf("[dyld] %s at %#08x-%#08x", img.Name(), lo+img.Slide, hi+img.Slide)
	m.applyPatches(img)
	m.bindSndDSP(img)
	m.bindCMTrace(img)
}

// setupProcess builds the initial stack (argc, argv, envp, apple) and the ProgramVars.
func (m *Machine) setupProcess() {
	c := m.C
	args := append([]string{filepath.Join(m.Dir, "hl2_osx")}, m.Args...)
	var envs []string
	for k, v := range m.env {
		envs = append(envs, k+"="+v)
	}
	sort.Strings(envs)
	str := func(s string) uint32 {
		p := c.Malloc(uint32(len(s) + 1))
		c.WriteCString(p, s)
		return p
	}
	vec := func(list []string) uint32 {
		p := c.Malloc(uint32(4 * (len(list) + 1)))
		for i, s := range list {
			c.Write32(p+uint32(4*i), str(s))
		}
		c.Write32(p+uint32(4*len(list)), 0)
		return p
	}
	m.argc, m.argv, m.envp = uint32(len(args)), vec(args), vec(envs)
	m.apple = vec([]string{"executable_path=" + args[0]})
	// struct ProgramVars { mh, NXArgcPtr, NXArgvPtr, environPtr, __prognamePtr }
	cells := m.data(16)
	c.Write32(cells, m.argc)
	c.Write32(cells+4, m.argv)
	c.Write32(cells+8, m.envp)
	c.Write32(cells+12, str("hl2_osx"))
	m.pvars = m.data(20)
	c.Write32(m.pvars, 0x1000)
	for i := uint32(0); i < 4; i++ {
		c.Write32(m.pvars+4+4*i, cells+4*i)
	}
	if a, ok := m.sys["_environ"]; ok {
		c.Write32(a, m.envp)
	}

	// The process stack, as the kernel leaves it for start: argc, argv[], 0, envp[], 0, apple[], 0.
	words := []uint32{m.argc}
	for i := uint32(0); i <= m.argc; i++ {
		words = append(words, c.Read32(m.argv+4*i))
	}
	for i := uint32(0); ; i++ {
		v := c.Read32(m.envp + 4*i)
		words = append(words, v)
		if v == 0 {
			break
		}
	}
	words = append(words, c.Read32(m.apple), 0)
	sp := (uint32(stackTop) - 64 - uint32(4*len(words))) &^ 15
	for i, w := range words {
		c.Write32(sp+uint32(4*i), w)
	}
	c.Regs[cpu.RegESP], c.Regs[cpu.RegEBP] = sp, 0
	c.EIP = m.L.Main.Entry
}

// RunInits runs the initialisers of images loaded since the last call (dependencies first).
func (m *Machine) RunInits() error {
	for m.initsRun < len(m.L.Inits) {
		fn := m.L.Inits[m.initsRun]
		m.initsRun++
		if _, err := m.C.CallGuest(fn, m.argc, m.argv, m.envp, m.apple, m.pvars); err != nil {
			return fmt.Errorf("initialiser %#x (%s): %w", fn, m.where(fn), err)
		}
	}
	return nil
}

// Run runs the game from its entry point until it exits, halts or reaches maxSteps (0: no limit).
func (m *Machine) Run(maxSteps uint64) error {
	m.C.MaxSteps = maxSteps
	if err := m.RunInits(); err != nil {
		return err
	}
	return m.C.RunUntil(cpu.ExitReturnEIP)
}

// where names the image and offset of a guest address.
func (m *Machine) where(a uint32) string {
	if name, ok := m.sysName[a]; ok {
		return name
	}
	if img := m.L.ImageAt(a); img != nil {
		lo, _ := img.Span()
		if sym, off := img.SymbolAt(a); sym != "" {
			return fmt.Sprintf("%s!%s+%#x", img.Name(), sym, off)
		}
		return fmt.Sprintf("%s+%#x", img.Name(), a-(lo+img.Slide))
	}
	return fmt.Sprintf("%#08x", a)
}

// Backtrace walks the guest's EBP chain from the current state.
func (m *Machine) Backtrace(max int) []string {
	c := m.C
	out := []string{m.where(c.EIP)}
	if ret := c.Read32(c.Regs[cpu.RegESP]); m.L.ImageAt(ret) != nil {
		out = append(out, "(ret) "+m.where(ret))
	}
	ebp := c.Regs[cpu.RegEBP]
	for i := 0; i < max && ebp != 0 && ebp < cpu.ArenaSize-8; i++ {
		ret := c.Read32(ebp + 4)
		if ret == 0 {
			break
		}
		out = append(out, m.where(ret))
		next := c.Read32(ebp)
		if next <= ebp {
			break
		}
		ebp = next
	}
	return out
}

// Report summarises the unimplemented system functions the game called.
func (m *Machine) Report() []string {
	var names []string
	for n := range m.Unimpl {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return m.Unimpl[names[i]] > m.Unimpl[names[j]] })
	var out []string
	for _, n := range names {
		out = append(out, fmt.Sprintf("%8d %s (%s)", m.Unimpl[n], n, m.unimplLib[n]))
	}
	return out
}

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"time"

	"catalina32/pkg/cpu"
	"catalina32/pkg/crt"
	"catalina32/pkg/d3d9"
	"catalina32/pkg/games"
	"catalina32/pkg/macho32"
	"catalina32/pkg/win32"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// appParent returns the directory containing the .app bundle the executable is in (or the
// executable's own directory).
func appParent(bin string) string {
	for d := filepath.Dir(bin); d != filepath.Dir(d); d = filepath.Dir(d) {
		if strings.HasSuffix(d, ".app") {
			return filepath.Dir(d)
		}
	}
	return filepath.Dir(bin)
}

func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(runCommand(os.Args[1], os.Args[2:]))
	}
	// The engine alone, on an executable (the development flow: run_on_mac.sh, scripts/).
	gameFlag := flag.String("game", "", "path to the game's i386 Mach-O executable (default: $C32_BIN_PATH)")
	flag.Usage = usage
	flag.Parse()
	binPath := os.Getenv("C32_BIN_PATH")
	if *gameFlag != "" {
		binPath = *gameFlag
	}
	if binPath == "" {
		usage()
		os.Exit(2)
	}
	engineMain(binPath)
}

// engineMain runs a game's executable: folders and saves come from C32_DATA_ROOT and
// C32_SAVE_ROOT, or default from the profile.
func engineMain(binPath string) {
	img, err := macho32.LoadImage(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load Mach-O image: %v\n", err)
		os.Exit(1)
	}
	game, err := games.Detect(img.Raw, os.Getenv("C32_PROFILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	prof := game.Profile
	// Defaults: game data in the profile's folder next to the .app, saves under the user's
	// application support directory.
	dataRoot := envOrDefault("C32_DATA_ROOT", filepath.Join(appParent(binPath), prof.DataDir))
	saveRoot := os.Getenv("C32_SAVE_ROOT")
	if saveRoot == "" {
		dir, _ := os.UserConfigDir()
		name := prof.SaveDir
		if name == "" {
			name = game.ID
		}
		saveRoot = filepath.Join(dir, "catalina32", name, "SaveData")
	}
	var modInits []uint32
	for _, fn := range img.ModInitFuncs {
		if fn != 0 && (prof.ModInitLimit == 0 || fn < prof.ModInitLimit) {
			modInits = append(modInits, fn)
		}
	}
	fmt.Printf("[catalina32] Loaded 32-bit Mach-O (%d segments, %d sections, %d symbols, %d FDEs, %d game static initializers) -> %s\n",
		len(img.Segments), len(img.Sections), len(img.Symbols), len(img.FDEs), len(modInits), prof.Name)

	vm := cpu.NewCPU()
	if err := img.MapIntoArena(vm.Mem); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to map segments: %v\n", err)
		os.Exit(1)
	}

	prof.Attach(img, vm.Mem)
	for _, sec := range img.Sections {
		if sec.SegName == "__TEXT" && sec.SectName == "__text" && os.Getenv("C32_NO_DCACHE") == "" {
			vm.SetCodeRange(sec.Addr, sec.Addr+sec.Size-16) // leave room so decoding never runs off the end
		}
	}
	entry, ok := prof.Addr(prof.EntryPoint)
	if !ok {
		fmt.Fprintf(os.Stderr, "profile %q does not locate entry point %q\n", prof.Name, prof.EntryPoint)
		os.Exit(1)
	}

	crt.RegisterCRT(vm, img, prof)
	w32 := win32.NewWin32Env(vm, dataRoot, saveRoot)
	if prof.ModulePath != "" {
		w32.ModulePath = prof.ModulePath
	}
	w32.RegisterBoundaryThunks(vm, prof)
	d3d := d3d9.NewSubsystem(vm)
	w32.OnCursor = d3d.SetWin32Cursor // the viewer hides the pointer when the game doesn't want it
	d3d.RegisterIndirectXThunks(vm, prof)
	game.Install(vm, d3d, prof)
	fmt.Printf("[catalina32] %s\n", prof.Report())

	// 1. Execute all 18 NuEngine static initializers
	fmt.Printf("[catalina32] Running %d game C++ static initializers (__mod_init_func)...\n", len(modInits))
	for i, fnAddr := range modInits {
		_, err := vm.CallGuest(fnAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [Init %2d] 0x%08x FAILED: %v\n  State: %s\n", i+1, fnAddr, err, vm.DumpState())
			os.Exit(1)
		}
	}
	fmt.Printf("[catalina32] All %d game C++ static initializers completed (%d instructions).\n", len(modInits), vm.Steps)

	// 2. Call the entry point: WinMain(hInstance=0x00001000, hPrevInstance=0, lpCmdLine=w32.CmdLineAddr, nCmdShow=1)
	fmt.Printf("[catalina32] Launching %s(0x00001000, 0, 0x%08x, 1) @ 0x%08x...\n", prof.EntryPoint, w32.CmdLineAddr, entry)
	// Headless runs stop after a fixed budget (C32_MAX_STEPS overrides it; 0 = no limit); a live
	// session runs until the game quits.
	vm.MaxSteps = 1_100_000_000
	if os.Getenv("C32_LIVE") == "1" {
		vm.MaxSteps = 0
	}
	vm.TraceCalls = os.Getenv("C32_TRACE_CALLS") == "1"
	if v := os.Getenv("C32_GUEST_PROF"); v != "" { // value = first step to sample from
		from, _ := strconv.ParseUint(v, 10, 64)
		vm.TraceCalls = true
		vm.ProfEvery, vm.ProfFrom = 997, from
		vm.ProfSelf, vm.ProfIncl = map[uint32]uint64{}, map[uint32]uint64{}
	}
	if pf := os.Getenv("C32_CPUPROF"); pf != "" {
		if f, err := os.Create(pf); err == nil {
			// C32_CPUPROF_AFTER=<seconds> starts the profile later (e.g. once a level is running);
			// C32_CPUPROF_FRAMES=<first>-<last> profiles only those frames (one scene).
			var first, last uint64
			if _, err := fmt.Sscanf(os.Getenv("C32_CPUPROF_FRAMES"), "%d-%d", &first, &last); err == nil {
				d3d.OnPresent = func(frame uint64) {
					switch frame {
					case first:
						_ = pprof.StartCPUProfile(f)
					case last:
						d3d.WaitRender()
						pprof.StopCPUProfile()
						fmt.Printf("[catalina32] CPU profile of frames %d-%d written\n", first, last)
					}
				}
			} else if d, err := time.ParseDuration(os.Getenv("C32_CPUPROF_AFTER") + "s"); err == nil && d > 0 {
				time.AfterFunc(d, func() { _ = pprof.StartCPUProfile(f) })
			} else {
				_ = pprof.StartCPUProfile(f)
			}
			defer pprof.StopCPUProfile()
		}
	}
	if v := os.Getenv("C32_MAX_STEPS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			vm.MaxSteps = n
		}
	}
	// C32_WATCH=lo-hi (guest addresses, e.g. 0x2c317e8-0x2c319ea) logs the first access to that range from each EIP.
	if w := os.Getenv("C32_WATCH"); w != "" {
		l, h, _ := strings.Cut(w, "-")
		lo, err1 := strconv.ParseUint(l, 0, 32)
		hi, err2 := strconv.ParseUint(h, 0, 32)
		if err1 == nil && err2 == nil {
			seen := map[uint32]bool{}
			vm.SetWatch(uint32(lo), uint32(hi), func(c *cpu.CPU, addr uint32, size int, write bool) {
				if seen[c.EIP] {
					return
				}
				seen[c.EIP] = true
				fmt.Printf("[WATCH] write=%v size=%d addr=0x%08x EIP=0x%08x steps=%d\n", write, size, addr, c.EIP, c.Steps)
			})
		}
	}
	vm.NestDepth = -1 // WinMain is the top-level thread; keep depth 0 inside it so the scheduler may preempt
	ret, err := vm.CallGuest(entry, 0x00001000, 0, w32.CmdLineAddr, 1)
	pprof.StopCPUProfile()
	d3d.WaitRender() // let the last submitted frame finish drawing/exporting
	dumpGuestProfile(vm)
	d3d.GameEnded()
	if vm.Exited {
		fmt.Printf("[catalina32] %s after %d instructions\n", vm.HaltReason, vm.Steps)
		os.Exit(int(vm.ExitCode))
	}
	if err != nil {
		fmt.Printf("[catalina32] WinMain stopped: %v\n  State: %s\n", err, vm.DumpState())
		fmt.Printf("  CallStack depth=%d:\n", len(vm.CallStack))
		start := 0
		if len(vm.CallStack) > 12 {
			start = len(vm.CallStack) - 12
		}
		for i := start; i < len(vm.CallStack); i++ {
			f := vm.CallStack[i]
			fmt.Printf("    [%2d] caller=0x%08x -> target=0x%08x (ESP=0x%08x)\n", i, f.CallerEIP, f.TargetEIP, f.ESP)
		}
		type kv struct {
			addr  uint32
			count uint64
		}
		var top []kv
		for k, v := range vm.VisitedFuncs {
			top = append(top, kv{k, v})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].count > top[j].count })
		fmt.Printf("  Top Visited Functions:\n")
		for i := 0; i < len(top) && i < 15; i++ {
			fmt.Printf("    0x%08x : %d calls (%s)\n", top[i].addr, top[i].count, vm.InterceptTag[top[i].addr])
		}
		os.Exit(1)
	}
	fmt.Printf("[catalina32] WinMain returned EAX=0x%08x after %d instructions!\n", ret, vm.Steps)
}

// dumpGuestProfile prints the guest functions that consumed the most sampled x86 steps.
func dumpGuestProfile(vm *cpu.CPU) {
	if vm.ProfSamples == 0 {
		return
	}
	type kv struct {
		addr  uint32
		count uint64
	}
	n := 40
	if v, err := strconv.Atoi(os.Getenv("C32_PROF_TOP")); err == nil && v > 0 {
		n = v
	}
	report := func(title string, m map[uint32]uint64) {
		var top []kv
		for k, v := range m {
			top = append(top, kv{k, v})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].count > top[j].count })
		fmt.Printf("[PROF] %s (%d samples, 1 per %d steps):\n", title, vm.ProfSamples, vm.ProfEvery)
		for i := 0; i < len(top) && i < n; i++ {
			fmt.Printf("[PROF]   0x%08x %6.2f%%  %s\n", top[i].addr, 100*float64(top[i].count)/float64(vm.ProfSamples), vm.InterceptTag[top[i].addr])
		}
	}
	report("self", vm.ProfSelf)
	report("inclusive", vm.ProfIncl)
}

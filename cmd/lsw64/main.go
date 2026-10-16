package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"strconv"

	"lsw64/pkg/cpu"
	"lsw64/pkg/crt"
	"lsw64/pkg/d3d9"
	"lsw64/pkg/games"
	"lsw64/pkg/macho32"
	"lsw64/pkg/win32"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	binPath := envOrDefault("LSW_BIN_PATH", "/workspace/Lego Star Wars Saga/LEGO Star Wars Saga.app/Contents/MacOS/LEGO Star Wars Saga")
	dataRoot := envOrDefault("LSW_DATA_ROOT", "/workspace/Lego Star Wars Saga/LEGOStarWarsSagaData")
	saveRoot := envOrDefault("LSW_SAVE_ROOT", "/workspace/Lego Star Wars Saga/SaveData")

	img, err := macho32.LoadImage(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load Mach-O image: %v\n", err)
		os.Exit(1)
	}
	game, err := games.Detect(img.Raw, os.Getenv("LSW_PROFILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	prof := game.Profile
	var modInits []uint32
	for _, fn := range img.ModInitFuncs {
		if fn != 0 && (prof.ModInitLimit == 0 || fn < prof.ModInitLimit) {
			modInits = append(modInits, fn)
		}
	}
	fmt.Printf("[lsw64] Loaded 32-bit Mach-O (%d segments, %d sections, %d symbols, %d FDEs, %d game static initializers) -> %s\n",
		len(img.Segments), len(img.Sections), len(img.Symbols), len(img.FDEs), len(modInits), prof.Name)

	vm := cpu.NewCPU()
	if err := img.MapIntoArena(vm.Mem); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to map segments: %v\n", err)
		os.Exit(1)
	}

	prof.Attach(img, vm.Mem)
	for _, sec := range img.Sections {
		if sec.SegName == "__TEXT" && sec.SectName == "__text" && os.Getenv("LSW_NO_DCACHE") == "" {
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
	w32.RegisterBoundaryThunks(vm, prof)
	d3d := d3d9.NewSubsystem(vm)
	d3d.RegisterIndirectXThunks(vm, prof)
	game.Install(vm, d3d, prof)
	fmt.Printf("[lsw64] %s\n", prof.Report())

	// 1. Execute all 18 NuEngine static initializers
	fmt.Printf("[lsw64] Running %d game C++ static initializers (__mod_init_func)...\n", len(modInits))
	for i, fnAddr := range modInits {
		_, err := vm.CallGuest(fnAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [Init %2d] 0x%08x FAILED: %v\n  State: %s\n", i+1, fnAddr, err, vm.DumpState())
			os.Exit(1)
		}
	}
	fmt.Printf("[lsw64] All %d game C++ static initializers completed (%d instructions).\n", len(modInits), vm.Steps)

	// 2. Call the entry point: WinMain(hInstance=0x00001000, hPrevInstance=0, lpCmdLine=w32.CmdLineAddr, nCmdShow=1)
	fmt.Printf("[lsw64] Launching %s(0x00001000, 0, 0x%08x, 1) @ 0x%08x...\n", prof.EntryPoint, w32.CmdLineAddr, entry)
	vm.MaxSteps = 1_100_000_000
	vm.TraceCalls = os.Getenv("LSW_TRACE_CALLS") == "1"
	if v := os.Getenv("LSW_GUEST_PROF"); v != "" { // value = first step to sample from
		from, _ := strconv.ParseUint(v, 10, 64)
		vm.TraceCalls = true
		vm.ProfEvery, vm.ProfFrom = 997, from
		vm.ProfSelf, vm.ProfIncl = map[uint32]uint64{}, map[uint32]uint64{}
	}
	if pf := os.Getenv("LSW_CPUPROF"); pf != "" {
		if f, err := os.Create(pf); err == nil {
			_ = pprof.StartCPUProfile(f)
			defer pprof.StopCPUProfile()
		}
	}
	if v := os.Getenv("LSW_MAX_STEPS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			vm.MaxSteps = n
		}
	}
	if os.Getenv("LSW_WATCH_KEYS") != "" {
		seen := map[uint32]bool{}
		vm.WatchLo, vm.WatchHi = 0x02C317E8, 0x02C319EA
		vm.WatchFn = func(c *cpu.CPU, addr uint32, size int, write bool) {
			if seen[c.EIP] {
				return
			}
			seen[c.EIP] = true
			fmt.Printf("[WATCH] write=%v size=%d addr=0x%08x EIP=0x%08x steps=%d\n", write, size, addr, c.EIP, c.Steps)
		}
	}
	vm.NestDepth = -1 // WinMain is the top-level thread; keep depth 0 inside it so the scheduler may preempt
	ret, err := vm.CallGuest(entry, 0x00001000, 0, w32.CmdLineAddr, 1)
	pprof.StopCPUProfile()
	dumpGuestProfile(vm)
	if err != nil {
		fmt.Printf("[lsw64] WinMain stopped: %v\n  State: %s\n", err, vm.DumpState())
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
	fmt.Printf("[lsw64] WinMain returned EAX=0x%08x after %d instructions!\n", ret, vm.Steps)
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
	report := func(title string, m map[uint32]uint64) {
		var top []kv
		for k, v := range m {
			top = append(top, kv{k, v})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].count > top[j].count })
		fmt.Printf("[PROF] %s (%d samples, 1 per %d steps):\n", title, vm.ProfSamples, vm.ProfEvery)
		for i := 0; i < len(top) && i < 40; i++ {
			fmt.Printf("[PROF]   0x%08x %6.2f%%  %s\n", top[i].addr, 100*float64(top[i].count)/float64(vm.ProfSamples), vm.InterceptTag[top[i].addr])
		}
	}
	report("self", vm.ProfSelf)
	report("inclusive", vm.ProfIncl)
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"

	"lsw64/pkg/cpu"
	"lsw64/pkg/crt"
	"lsw64/pkg/d3d9"
	"lsw64/pkg/lifted"
	"lsw64/pkg/macho32"
	"lsw64/pkg/win32"
)

type BoundaryCatalog struct {
	BoundaryFunctions []struct {
		Address string `json:"address"`
	} `json:"boundary_functions"`
}

func loadBoundaryAddrs(path string) []uint32 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cat BoundaryCatalog
	if err := json.Unmarshal(b, &cat); err != nil {
		return nil
	}
	var out []uint32
	for _, fn := range cat.BoundaryFunctions {
		if v, err := strconv.ParseUint(fn.Address, 0, 32); err == nil {
			out = append(out, uint32(v))
		}
	}
	return out
}

func main() {
	binPath := "/workspace/Lego Star Wars Saga/LEGO Star Wars Saga.app/Contents/MacOS/LEGO Star Wars Saga"
	dataRoot := "/workspace/Lego Star Wars Saga/LEGOStarWarsSagaData"
	saveRoot := "/workspace/Lego Star Wars Saga/SaveData"

	img, err := macho32.LoadImage(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load Mach-O image: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[lsw64] Loaded 32-bit Mach-O (%d segments, %d sections, %d symbols, %d FDEs, %d NuEngine static initializers)\n",
		len(img.Segments), len(img.Sections), len(img.Symbols), len(img.FDEs), len(img.NuModInits))

	vm := cpu.NewCPU()
	if err := img.MapIntoArena(vm.Mem); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to map segments: %v\n", err)
		os.Exit(1)
	}

	crt.RegisterCRT(vm, img)
	w32 := win32.NewWin32Env(vm, dataRoot, saveRoot)
	boundaryAddrs := loadBoundaryAddrs("/workspace/lego_star_wars_193_boundary_catalog.json")
	w32.RegisterBoundaryThunks(vm, boundaryAddrs)
	d3d := d3d9.NewSubsystem(vm)
	d3d.RegisterIndirectXThunks(vm)
	lifted.RegisterLiftedFunctions(vm)

	// 1. Execute all 18 NuEngine static initializers
	fmt.Printf("[lsw64] Running %d NuEngine C++ static initializers (__mod_init_func)...\n", len(img.NuModInits))
	for i, fnAddr := range img.NuModInits {
		_, err := vm.CallGuest(fnAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [Init %2d] 0x%08x FAILED: %v\n  State: %s\n", i+1, fnAddr, err, vm.DumpState())
			os.Exit(1)
		}
	}
	fmt.Printf("[lsw64] All %d NuEngine C++ static initializers completed (%d instructions).\n", len(img.NuModInits), vm.Steps)

	// 2. Call WinMain(hInstance=0x00001000, hPrevInstance=0, lpCmdLine=w32.CmdLineAddr, nCmdShow=1) at 0x00209d10
	fmt.Printf("[lsw64] Launching WinMain(0x00001000, 0, 0x%08x, 1) @ 0x00209d10...\n", w32.CmdLineAddr)
	vm.MaxSteps = 1_100_000_000
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
	ret, err := vm.CallGuest(0x00209d10, 0x00001000, 0, w32.CmdLineAddr, 1)
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

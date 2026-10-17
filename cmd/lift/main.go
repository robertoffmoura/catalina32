// Command lift disassembles a range of a game executable (i386 Mach-O VAs) to stdout, to help
// write native replacements for hot guest functions. It never modifies the repository.
//
//	go run ./cmd/lift -start 0x0007ab10 -end 0x0007af80 [-game path] [-bytes]
//
// Start at a function boundary: the disassembly is a linear sweep.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/arch/x86/x86asm"

	"catalina32/pkg/macho32"
)

func main() {
	startStr := flag.String("start", "0x00270500", "start VA")
	endStr := flag.String("end", "0x002705b0", "end VA (exclusive)")
	game := flag.String("game", "/workspace/Lego Star Wars Saga/LEGO Star Wars Saga.app/Contents/MacOS/LEGO Star Wars Saga", "i386 Mach-O executable")
	showBytes := flag.Bool("bytes", false, "print instruction bytes")
	flag.Parse()

	s, err1 := strconv.ParseUint(*startStr, 0, 32)
	e, err2 := strconv.ParseUint(*endStr, 0, 32)
	if err1 != nil || err2 != nil || e <= s {
		fmt.Fprintln(os.Stderr, "bad -start/-end")
		os.Exit(2)
	}
	img, err := macho32.LoadImage(*game)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	mem := make([]byte, 0x04000000)
	if err := img.MapIntoArena(mem); err != nil {
		fmt.Fprintf(os.Stderr, "map: %v\n", err)
		os.Exit(1)
	}
	symbol := func(addr uint64) (string, uint64) {
		if name, ok := img.SymbolsByAddr[uint32(addr)]; ok {
			return name, addr
		}
		if name, ok := img.StubsByAddr[uint32(addr)]; ok {
			return name, addr
		}
		return "", 0
	}
	for pc := uint32(s); pc < uint32(e); {
		inst, err := x86asm.Decode(mem[pc:min(int(pc)+16, len(mem))], 32)
		if err != nil || inst.Len == 0 {
			fmt.Printf("  0x%08x: db 0x%02x\n", pc, mem[pc])
			pc++
			continue
		}
		text := x86asm.IntelSyntax(inst, uint64(pc), symbol)
		if *showBytes {
			var b strings.Builder
			for _, x := range mem[pc : pc+uint32(inst.Len)] {
				fmt.Fprintf(&b, "%02x ", x)
			}
			fmt.Printf("  0x%08x: %-30s %s\n", pc, b.String(), text)
		} else {
			fmt.Printf("  0x%08x: %s\n", pc, text)
		}
		pc += uint32(inst.Len)
	}
}

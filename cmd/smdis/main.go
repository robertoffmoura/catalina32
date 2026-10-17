// Command smdis disassembles Direct3D 9 shader bytecode embedded in an i386 Mach-O executable.
//
//	go run ./cmd/smdis -game <exe> 0x008e6b16 0x008e708a ...
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"

	"catalina32/pkg/d3dsm"
	"catalina32/pkg/macho32"
)

func main() {
	game := flag.String("game", "", "i386 Mach-O executable")
	flag.Parse()
	img, err := macho32.LoadImage(*game)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mem := make([]byte, 0x04000000)
	if err := img.MapIntoArena(mem); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, a := range flag.Args() {
		addr, err := strconv.ParseUint(a, 0, 32)
		if err != nil {
			continue
		}
		var tok []uint32
		for p := uint32(addr); int(p)+4 <= len(mem) && len(tok) < 1<<16; p += 4 {
			t := binary.LittleEndian.Uint32(mem[p:])
			tok = append(tok, t)
			if t == 0x0000FFFF {
				break
			}
		}
		sh, err := d3dsm.Decode(tok)
		fmt.Printf("; 0x%08x (%d tokens)\n", addr, len(tok))
		if err != nil {
			fmt.Printf("; error: %v\n\n", err)
			continue
		}
		fmt.Println(sh)
		if _, err := d3dsm.Compile(sh); err != nil {
			fmt.Printf("; compile error: %v\n\n", err)
		}
	}
}

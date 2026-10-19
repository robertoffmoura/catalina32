// Command sigmake prints a pkg/profile signature for a function of a game executable: its bytes
// as hex, with "??" over the operands that move between builds (PIC-relative displacements,
// calls out of the function). With -list it also prints each such operand's offset and target,
// for code that reads addresses out of a matched function.
//
//	go run ./cmd/sigmake -start 0x002734f0 -end 0x002737a0 [-game path] [-list]
//
// -end is exclusive: the byte after the function's last instruction.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/arch/x86/x86asm"

	"catalina32/pkg/macho32"
)

func main() {
	startStr := flag.String("start", "", "function start VA")
	endStr := flag.String("end", "", "end VA (exclusive)")
	game := flag.String("game", os.Getenv("C32_BIN_PATH"), "i386 Mach-O executable (default: $C32_BIN_PATH)")
	list := flag.Bool("list", false, "list the wildcarded operands")
	flag.Parse()
	s, err1 := strconv.ParseUint(*startStr, 0, 32)
	e, err2 := strconv.ParseUint(*endStr, 0, 32)
	if err1 != nil || err2 != nil || e <= s {
		fmt.Fprintln(os.Stderr, "sigmake: -start and -end are required (end > start)")
		os.Exit(2)
	}
	img, err := macho32.LoadImage(*game)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sigmake:", err)
		os.Exit(1)
	}
	mem := make([]byte, 1<<30)
	if err := img.MapIntoArena(mem); err != nil {
		fmt.Fprintln(os.Stderr, "sigmake:", err)
		os.Exit(1)
	}
	start := uint32(s)
	code := mem[s:e]
	wild := make([]bool, len(code))
	var pic uint32 // the address a `call next; pop reg` sequence leaves in reg
	for off := 0; off < len(code); {
		inst, err := x86asm.Decode(code[off:], 32)
		if err != nil {
			off++
			continue
		}
		pc := start + uint32(off)
		if rel, ok := inst.Args[0].(x86asm.Rel); ok && inst.Op == x86asm.CALL {
			t := pc + uint32(inst.Len) + uint32(int32(rel))
			if t == pc+uint32(inst.Len) {
				pic = t
			} else if t < start || t >= uint32(e) {
				for k := 1; k <= 4; k++ {
					wild[off+inst.Len-k] = true
				}
				if *list {
					fmt.Printf("call  +%#05x (rel32 at +%#05x) -> %#08x\n", off, off+inst.Len-4, t)
				}
			}
		}
		for _, a := range inst.Args {
			m, ok := a.(x86asm.Mem)
			if !ok || m.Base == 0 || (m.Disp < 0x10000 && m.Disp > -0x10000) {
				continue
			}
			var d [4]byte
			binary.LittleEndian.PutUint32(d[:], uint32(m.Disp))
			i := strings.Index(string(code[off:off+inst.Len]), string(d[:]))
			if i < 0 {
				continue
			}
			for k := 0; k < 4; k++ {
				wild[off+i+k] = true
			}
			if *list {
				fmt.Printf("disp  +%#05x (disp32 at +%#05x) %-40v -> %#08x (PIC base %#08x)\n", off, off+i, inst, pic+uint32(m.Disp), pic)
			}
		}
		off += inst.Len
	}
	var sb strings.Builder
	for i, x := range code {
		if i > 0 {
			sb.WriteByte(' ')
		}
		if wild[i] {
			sb.WriteString("??")
		} else {
			fmt.Fprintf(&sb, "%02x", x)
		}
	}
	fmt.Println(sb.String())
}

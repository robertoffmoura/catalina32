package main

// Differential test of the x86 implementations: random straight-line snippets run through the
// full interpreter (stepFull), the decoded-instruction cache (dcache blocks) and code generated
// by this compiler (genFunc), and the final registers, flags, XMM registers, memory and step
// counts must agree. The compiled side is built and run as a separate program.

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"catalina32/pkg/cpu"
)

const (
	diffCode     = 0x00100000 // snippet i lives at diffCode + i*0x1000
	diffData     = 0x00400000 // 4 KB of random data; EBX points at its middle
	diffStack    = 0x00500000
	diffSentinel = 0x00000100 // return address of every snippet
	diffSnippets = 400
)

var dest32 = []byte{0, 1, 2, 5, 6, 7} // registers a snippet may write (not EBX, the data base, or ESP)
var dest8 = []byte{0, 1, 2, 4, 5, 6}  // AL CL DL AH CH DH

// genSnippet returns random instructions ending in RET.
func genSnippet(r *rand.Rand) []byte {
	var code []byte
	pick := func(l []byte) byte { return l[r.Intn(len(l))] }
	modrm := func(reg byte, rmDest bool) []byte { // register or [ebx+disp8] operand
		if r.Intn(3) == 0 {
			return []byte{0x40 | reg<<3 | 3, byte(r.Intn(256) &^ 3)}
		}
		rm := byte(r.Intn(8))
		if rmDest {
			rm = pick(dest32)
		}
		return []byte{0xC0 | reg<<3 | rm}
	}
	modrm8 := func(reg byte, rmDest bool) []byte {
		if r.Intn(3) == 0 {
			return []byte{0x40 | reg<<3 | 3, byte(r.Intn(256))}
		}
		rm := byte(r.Intn(8))
		if rmDest {
			rm = pick(dest8)
		}
		return []byte{0xC0 | reg<<3 | rm}
	}
	xm := func(reg byte) []byte { // xmm register or [ebx+disp8]
		if r.Intn(3) == 0 {
			return []byte{0x40 | reg<<3 | 3, byte(r.Intn(256) &^ 15)}
		}
		return []byte{0xC0 | reg<<3 | byte(r.Intn(8))}
	}
	cat := func(parts ...[]byte) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return b
	}
	imm32 := func() []byte { v := r.Uint32(); return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	one := func() []byte {
		switch r.Intn(30) {
		case 0: // ALU r/m32, r32
			return cat([]byte{pick([]byte{0x01, 0x09, 0x11, 0x19, 0x21, 0x29, 0x31, 0x39})}, modrm(byte(r.Intn(8)), true))
		case 1: // ALU r32, r/m32
			return cat([]byte{pick([]byte{0x03, 0x0B, 0x13, 0x1B, 0x23, 0x2B, 0x33, 0x3B})}, modrm(pick(dest32), false))
		case 2: // ALU r/m8, r8 and r8, r/m8
			if r.Intn(2) == 0 {
				return cat([]byte{pick([]byte{0x00, 0x08, 0x10, 0x18, 0x20, 0x28, 0x30, 0x38})}, modrm8(byte(r.Intn(8)), true))
			}
			return cat([]byte{pick([]byte{0x02, 0x0A, 0x12, 0x1A, 0x22, 0x2A, 0x32, 0x3A})}, modrm8(pick(dest8), false))
		case 3: // ALU r/m32, imm8 / imm32
			if r.Intn(2) == 0 {
				return cat([]byte{0x83}, modrm(byte(r.Intn(8)), true), []byte{byte(r.Intn(256))})
			}
			return cat([]byte{0x81}, modrm(byte(r.Intn(8)), true), imm32())
		case 4: // ALU r/m8, imm8
			return cat([]byte{0x80}, modrm8(byte(r.Intn(8)), true), []byte{byte(r.Intn(256))})
		case 5: // 16-bit ALU
			return cat([]byte{0x66, pick([]byte{0x01, 0x03, 0x11, 0x19, 0x21, 0x29, 0x31, 0x39})}, []byte{0xC0 | pick(dest32)<<3 | pick(dest32)})
		case 6: // MOV forms
			switch r.Intn(5) {
			case 0:
				return cat([]byte{0x89}, modrm(byte(r.Intn(8)), true))
			case 1:
				return cat([]byte{0x8B}, modrm(pick(dest32), false))
			case 2:
				return cat([]byte{0x88}, modrm8(byte(r.Intn(8)), true))
			case 3:
				return cat([]byte{0x8A}, modrm8(pick(dest8), false))
			}
			return cat([]byte{0xB8 + pick(dest32)}, imm32())
		case 7: // TEST
			if r.Intn(2) == 0 {
				return cat([]byte{0x85}, modrm(byte(r.Intn(8)), false))
			}
			return cat([]byte{0x84}, modrm8(byte(r.Intn(8)), false))
		case 8: // INC/DEC
			if r.Intn(2) == 0 {
				return []byte{0x40 + pick([]byte{0, 8}) + pick(dest32)}
			}
			return cat([]byte{0xFF}, modrm(byte(r.Intn(2)), true))
		case 9: // NOT/NEG/MUL/IMUL r/m32
			return cat([]byte{0xF7}, modrm(byte(2+r.Intn(4)), true))
		case 10: // IMUL r32, r/m32 (, imm)
			switch r.Intn(3) {
			case 0:
				return cat([]byte{0x0F, 0xAF}, modrm(pick(dest32), false))
			case 1:
				return cat([]byte{0x6B}, modrm(pick(dest32), false), []byte{byte(r.Intn(256))})
			}
			return cat([]byte{0x69}, modrm(pick(dest32), false), imm32())
		case 11: // shifts and rotates, 32-bit
			switch r.Intn(3) {
			case 0:
				return cat([]byte{0xC1}, modrm(byte(r.Intn(8)), true), []byte{byte(r.Intn(40))})
			case 1:
				return cat([]byte{0xD1}, modrm(byte(r.Intn(8)), true))
			}
			return cat([]byte{0xD3}, modrm(byte(r.Intn(8)), true))
		case 12: // shifts and rotates, 8- and 16-bit
			if r.Intn(2) == 0 {
				return cat([]byte{0xC0}, modrm8(byte(r.Intn(8)), true), []byte{byte(r.Intn(20))})
			}
			return []byte{0x66, 0xC1, 0xC0 | byte(r.Intn(8))<<3 | pick(dest32), byte(r.Intn(20))}
		case 13: // MOVZX/MOVSX
			op := pick([]byte{0xB6, 0xB7, 0xBE, 0xBF})
			if op == 0xB6 || op == 0xBE {
				return cat([]byte{0x0F, op}, modrm8(pick(dest32), false))
			}
			return cat([]byte{0x0F, op}, modrm(pick(dest32), false))
		case 14: // LEA r32, [ebx + idx*scale + disp8]
			idx := pick([]byte{0, 1, 2, 3, 5, 6, 7})
			return []byte{0x8D, 0x44 | pick(dest32)<<3, byte(r.Intn(4))<<6 | idx<<3 | 3, byte(r.Intn(256))}
		case 15: // SETcc
			return cat([]byte{0x0F, 0x90 + byte(r.Intn(16))}, modrm8(0, true))
		case 16: // CMOVcc
			return cat([]byte{0x0F, 0x40 + byte(r.Intn(16))}, modrm(pick(dest32), false))
		case 17: // BSWAP, XCHG, CDQ, CWDE, LAHF/SAHF
			switch r.Intn(5) {
			case 0:
				return []byte{0x0F, 0xC8 + pick(dest32)}
			case 1:
				return []byte{0x87, 0xC0 | pick(dest32)<<3 | pick(dest32)}
			case 2:
				return []byte{0x99}
			case 3:
				return []byte{0x98}
			}
			return []byte{pick([]byte{0x9E, 0x9F})}
		case 18: // scalar SSE arithmetic and moves
			op := pick([]byte{0x10, 0x58, 0x59, 0x5C, 0x5E, 0x5D, 0x5F})
			return cat([]byte{pick([]byte{0xF3, 0xF2}), 0x0F, op}, xm(byte(r.Intn(8))))
		case 19: // MOVSS/MOVSD store
			return []byte{pick([]byte{0xF3, 0xF2}), 0x0F, 0x11, 0x40 | byte(r.Intn(8))<<3 | 3, byte(r.Intn(256) &^ 7)}
		case 20: // CVTSI2SS, CVTTSS2SI, CVTSS2SI
			switch r.Intn(3) {
			case 0:
				return cat([]byte{0xF3, 0x0F, 0x2A}, modrm(byte(r.Intn(8)), false))
			case 1:
				return cat([]byte{0xF3, 0x0F, 0x2C}, []byte{0xC0 | pick(dest32)<<3 | byte(r.Intn(8))})
			}
			return cat([]byte{0xF3, 0x0F, 0x2D}, []byte{0xC0 | pick(dest32)<<3 | byte(r.Intn(8))})
		case 21: // UCOMISS/COMISS, XORPS, MOVAPS, ANDPS
			switch r.Intn(4) {
			case 0:
				return cat([]byte{0x0F, pick([]byte{0x2E, 0x2F})}, xm(byte(r.Intn(8))))
			case 1:
				return []byte{0x0F, 0x57, 0xC0 | byte(r.Intn(8))<<3 | byte(r.Intn(8))}
			case 2:
				return []byte{0x0F, 0x28, 0xC0 | byte(r.Intn(8))<<3 | byte(r.Intn(8))}
			}
			return []byte{0x0F, 0x54, 0xC0 | byte(r.Intn(8))<<3 | byte(r.Intn(8))}
		case 22: // balanced PUSH/POP
			return []byte{0x50 + byte(r.Intn(8)), 0x58 + pick(dest32)}
		case 23: // REP MOVSD/STOSD/MOVSB/STOSB over the data area
			n := byte(r.Intn(20))
			return []byte{0xB9, n, 0, 0, 0, 0x8D, 0x7B, byte(r.Intn(64)), 0x8D, 0x73, byte(r.Intn(96)), 0xF3, pick([]byte{0xA4, 0xA5, 0xAA, 0xAB})}
		case 24: // 8-bit INC/DEC (FE /0 /1), NOT/NEG (F6 /2 /3)
			if r.Intn(2) == 0 {
				return cat([]byte{0xFE}, modrm8(byte(r.Intn(2)), true))
			}
			return cat([]byte{0xF6}, modrm8(byte(2+r.Intn(2)), true))
		case 25: // MOV r/m32, imm32
			return cat([]byte{0xC7}, modrm(0, true), imm32())
		case 26: // 16-bit moves and immediates
			if r.Intn(2) == 0 {
				return []byte{0x66, 0xB8 + pick(dest32), byte(r.Intn(256)), byte(r.Intn(256))}
			}
			return []byte{0x66, 0x89, 0x40 | byte(r.Intn(8))<<3 | 3, byte(r.Intn(256) &^ 1)}
		case 27: // BT/BTS/BTR, SHLD/SHRD
			if r.Intn(2) == 0 {
				return []byte{0x0F, pick([]byte{0xA3, 0xAB, 0xB3}), 0xC0 | byte(r.Intn(8))<<3 | pick(dest32)}
			}
			return []byte{0x0F, pick([]byte{0xA4, 0xAC}), 0xC0 | byte(r.Intn(8))<<3 | pick(dest32), byte(r.Intn(32))}
		case 28: // CMP + Jcc over the next instruction (fused pairs and branches)
			return nil
		}
		return []byte{0x90}
	}
	n := 4 + r.Intn(24)
	for i := 0; i < n; i++ {
		ins := one()
		if ins == nil {
			next := one()
			for next == nil {
				next = one()
			}
			ins = cat([]byte{0x39, 0xC0 | byte(r.Intn(8))<<3 | byte(r.Intn(8)), 0x70 + byte(r.Intn(16)), byte(len(next))}, next)
		}
		code = append(code, ins...)
	}
	return append(code, 0xC3)
}

// diffInit sets snippet i's initial state from a small PRNG (the same code runs in the
// generated program, see diffInitSrc).
func diffInit(c *cpu.CPU, i int) {
	x := uint32(i)*2654435761 + 12345
	next := func() uint32 { x ^= x << 13; x ^= x >> 17; x ^= x << 5; return x }
	for k := range c.Regs {
		c.Regs[k] = next()
	}
	c.Regs[3] = diffData + 0x800
	c.Regs[4] = diffStack - 4
	c.Write32(diffStack-4, diffSentinel)
	c.Flags = next()&0x8D5 | 2
	for k := range c.XMM {
		for j := 0; j < 16; j += 4 {
			v := next()
			if v&3 == 0 { // some small, exactly representable floats
				v = uint32(int32(v>>20)-2048) & 0x8000FFFF
				v = 0x3F800000 | v&0x007F0000 | v&0x80000000
			}
			c.XMM[k][j], c.XMM[k][j+1], c.XMM[k][j+2], c.XMM[k][j+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
	}
	for a := uint32(diffData); a < diffData+0x1000; a += 4 {
		c.Write32(a, next())
	}
	c.EIP = diffCode + uint32(i)*0x1000
	c.Steps = 0
}

const diffInitSrc = `
func diffInit(c *cpu.CPU, i int) {
	x := uint32(i)*2654435761 + 12345
	next := func() uint32 { x ^= x << 13; x ^= x >> 17; x ^= x << 5; return x }
	for k := range c.Regs {
		c.Regs[k] = next()
	}
	c.Regs[3] = diffData + 0x800
	c.Regs[4] = diffStack - 4
	c.Write32(diffStack-4, diffSentinel)
	c.Flags = next()&0x8D5 | 2
	for k := range c.XMM {
		for j := 0; j < 16; j += 4 {
			v := next()
			if v&3 == 0 {
				v = uint32(int32(v>>20)-2048) & 0x8000FFFF
				v = 0x3F800000 | v&0x007F0000 | v&0x80000000
			}
			c.XMM[k][j], c.XMM[k][j+1], c.XMM[k][j+2], c.XMM[k][j+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
	}
	for a := uint32(diffData); a < diffData+0x1000; a += 4 {
		c.Write32(a, next())
	}
	c.EIP = diffCode + uint32(i)*0x1000
	c.Steps = 0
}

func diffState(c *cpu.CPU, i int) string {
	h := fnv.New64a()
	h.Write(c.Mem[diffData : diffData+0x1000])
	h.Write(c.Mem[diffStack-64 : diffStack])
	return fmt.Sprintf("%d eip=%x regs=%x flags=%x xmm=%x mem=%x steps=%d halted=%v", i, c.EIP, c.Regs, c.Flags, c.XMM, h.Sum64(), c.Steps, c.Halted)
}
`

func diffState(c *cpu.CPU, i int) string {
	h := fnv.New64a()
	h.Write(c.Mem[diffData : diffData+0x1000])
	h.Write(c.Mem[diffStack-64 : diffStack])
	return fmt.Sprintf("%d eip=%x regs=%x flags=%x xmm=%x mem=%x steps=%d halted=%v", i, c.EIP, c.Regs, c.Flags, c.XMM, h.Sum64(), c.Steps, c.Halted)
}

func runSnippets(snips [][]byte, dcache bool) []string {
	c := cpu.NewCPU()
	for i, s := range snips {
		copy(c.Mem[diffCode+i*0x1000:], s)
	}
	if dcache {
		c.SetCodeRange(diffCode, diffCode+uint32(len(snips))*0x1000)
	}
	var out []string
	for i := range snips {
		diffInit(c, i)
		c.Halted = false
		_ = c.RunUntil(diffSentinel)
		out = append(out, diffState(c, i))
	}
	return out
}

func TestImplementationsAgree(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var snips [][]byte
	for i := 0; i < diffSnippets; i++ {
		snips = append(snips, genSnippet(r))
	}
	interp := runSnippets(snips, false)
	cached := runSnippets(snips, true)
	for i, st := range interp {
		if !strings.Contains(st, fmt.Sprintf(" eip=%x ", diffSentinel)) || strings.HasSuffix(st, "halted=true") {
			t.Fatalf("snippet %d (% x) did not run to its return: %s", i, snips[i], st)
		}
	}
	bad := 0
	for i := range snips {
		if interp[i] != cached[i] && bad < 5 {
			bad++
			t.Errorf("decoded-instruction cache differs on snippet %d (% x):\n  interp %s\n  cached %s", i, snips[i], interp[i], cached[i])
		}
	}

	if testing.Short() {
		t.Skip("compiled code: skipped in -short mode")
	}
	compiled := runCompiled(t, snips)
	for i := range snips {
		if i < len(compiled) && interp[i] != compiled[i] && bad < 10 {
			bad++
			t.Errorf("compiled code differs on snippet %d (% x):\n  interp   %s\n  compiled %s", i, snips[i], interp[i], compiled[i])
		}
	}
	if len(compiled) != len(snips) {
		t.Fatalf("compiled program reported %d of %d snippets", len(compiled), len(snips))
	}
}

// runCompiled translates every snippet with genFunc, builds a program that runs them like
// runSnippets (with the translations registered as intercepts) and returns its states.
func runCompiled(t *testing.T, snips [][]byte) []string {
	mem := make([]byte, diffCode+len(snips)*0x1000+16)
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n\t\"fmt\"\n\t\"hash/fnv\"\n\t\"math\"\n\n\t\"catalina32/pkg/cpu\"\n)\n\nvar _ = math.Trunc\n\n")
	fmt.Fprintf(&b, "const (\n\tdiffCode = %#x\n\tdiffData = %#x\n\tdiffStack = %#x\n\tdiffSentinel = %#x\n)\n", diffCode, diffData, diffStack, diffSentinel)
	b.WriteString(diffInitSrc)
	var regs strings.Builder
	native, total := 0, 0
	for i, s := range snips {
		addr := uint32(diffCode + i*0x1000)
		copy(mem[addr:], s)
		name := fmt.Sprintf("aot_%08x", addr)
		entries, nat, tot := genFunc(&b, name, mem, addr, addr+uint32(len(s)))
		native += nat
		total += tot
		fmt.Fprintf(&regs, "\tcopy(c.Mem[%#x:], []byte{%s})\n", addr, byteList(s))
		for _, e := range entries {
			fmt.Fprintf(&regs, "\tc.RegisterIntercept(%#x, %q, %s)\n", e, name, name)
		}
	}
	fmt.Fprintf(&b, "\nfunc main() {\n\tc := cpu.NewCPU()\n%s\tfor i := 0; i < %d; i++ {\n\t\tdiffInit(c, i)\n\t\tc.Halted = false\n\t\t_ = c.RunUntil(diffSentinel)\n\t\tfmt.Println(diffState(c, i))\n\t}\n}\n", regs.String(), len(snips))

	t.Logf("%d snippets, %d/%d instructions compiled natively", len(snips), native, total)
	_ = os.MkdirAll("testdata", 0755) // inside the module, so the program can import catalina32/pkg/cpu
	dir, err := os.MkdirTemp("testdata", "aotdiff")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "run", "./"+dir).CombinedOutput()
	if err != nil {
		t.Fatalf("compiled program: %v\n%s", err, firstLines(string(out), 30))
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func byteList(b []byte) string {
	var s []string
	for _, x := range b {
		s = append(s, fmt.Sprintf("%#x", x))
	}
	return strings.Join(s, ", ")
}

func firstLines(s string, n int) string {
	l := strings.SplitN(s, "\n", n+1)
	return strings.Join(l[:min(n, len(l))], "\n")
}

package crt

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"catalina32/pkg/cpu"
	"catalina32/pkg/lh5"
	"catalina32/pkg/profile"
)

// NuEngine's LZHUF_Decompress, int (src, dst, srcSize, dstSize): its LHA -lh5- decoder (ar002),
// which games stream level data through while the player moves between areas. Emulated, it held
// the frame rate near zero for seconds at a time; natively it takes milliseconds. The routines
// are found by signature (signatures_nulzhuf.go), and the addresses of the decoder's globals
// are read out of the matched code. The native version loads those globals, decodes with
// pkg/lh5 and stores them back as the game's code leaves them, so the two are
// indistinguishable (TestLZHUFMatchesGuest); input the original would mis-decode is left to it.
//
//	C32_NO_NATIVE_LZHUF=1  keep the game's own decoder (A/B tests)
//	C32_DUMP_LZHUF=<dir>   save every compressed block (NNNNN_<size>.lh5) for the test

// lzhufGlobals are the decoder's globals in one binary.
type lzhufGlobals struct {
	fn                                                     uint32 // the decompress routine
	inPtr, outPtr, bitBuf, outLeft, inLeft, left, right    uint32
	bitCount, subBitBuf, disabled, text, blockSize, cTable uint32
	cLen, ptTable, ptLen, j, i                             uint32
}

// findLZHUF locates the decoder in the attached binary, or reports false.
func findLZHUF(c *cpu.CPU, prof *profile.Profile) (lzhufGlobals, bool) {
	var g lzhufGlobals
	addr := map[string]uint32{}
	for _, name := range []string{"decompress", "decode_c", "read_pt_len", "decode_p", "make_table"} {
		a, ok := prof.Addr("nu:lzhuf_" + name)
		if !ok {
			return g, false
		}
		addr[name] = a
	}
	// The routines must call each other as the decoder does (call rel32 targets).
	callTarget := func(fn, rel uint32) uint32 { return fn + rel + 4 + c.Read32(fn+rel) }
	if callTarget(addr["decompress"], 0x1d7) != addr["decode_c"] ||
		callTarget(addr["decompress"], 0x20c) != addr["decode_p"] ||
		callTarget(addr["decode_c"], 0xe0) != addr["read_pt_len"] ||
		callTarget(addr["decode_c"], 0x4b1) != addr["make_table"] {
		return g, false
	}
	// Each routine starts `push ebp; mov ebp, esp; ...; call next; pop reg`, so its globals are
	// addressed relative to the routine's start + 0xe; disp reads a displacement at offset off.
	disp := func(fn string, off uint32) uint32 {
		return addr[fn] + 0xe + c.Read32(addr[fn]+off)
	}
	g = lzhufGlobals{
		fn:        addr["decompress"],
		disabled:  disp("decompress", 0x13),
		inPtr:     disp("decompress", 0x33),
		outPtr:    disp("decompress", 0x39),
		outLeft:   disp("decompress", 0x3f),
		inLeft:    disp("decompress", 0x45),
		subBitBuf: disp("decompress", 0x4b),
		bitCount:  disp("decompress", 0x55),
		bitBuf:    disp("decompress", 0x5f),
		blockSize: disp("decompress", 0xff),
		j:         disp("decompress", 0x109),
		text:      disp("decompress", 0x11f),
		i:         disp("decompress", 0x17b),
		cTable:    disp("decode_c", 0x48f),
		cLen:      disp("decode_c", 0x499),
		ptTable:   disp("decode_p", 0x1e),
		right:     disp("decode_p", 0x34),
		left:      disp("decode_p", 0x3a),
		ptLen:     disp("decode_p", 0x50),
	}
	return g, true
}

// bindLZHUF replaces the decoder with the native one when the binary has it.
func bindLZHUF(c *cpu.CPU, prof *profile.Profile) {
	g, ok := findLZHUF(c, prof)
	if !ok {
		return
	}
	l := &lzhuf{g: g, dumpDir: os.Getenv("C32_DUMP_LZHUF"), off: os.Getenv("C32_NO_NATIVE_LZHUF") == "1"}
	if l.off && l.dumpDir == "" {
		return
	}
	c.RegisterIntercept(g.fn, "native::NuLZHUF_Decompress", l.decompress)
}

type lzhuf struct {
	g       lzhufGlobals
	dumpDir string
	off     bool
	dumped  int
	native  int // calls decoded natively
	s       lh5.State
}

func inArena(addr, n uint32) bool { return uint64(addr)+uint64(n) <= cpu.ArenaSize }

func loadU16s(dst []uint16, b []byte) {
	for i := range dst {
		dst[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
}

func storeU16s(b []byte, src []uint16) {
	for i, v := range src {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
}

func (l *lzhuf) decompress(c *cpu.CPU) {
	src, dst, srcSize, dstSize := c.Arg(0), c.Arg(1), c.Arg(2), c.Arg(3)
	if c.Read8(l.g.disabled) == 0 && inArena(src, srcSize) && inArena(dst, dstSize) {
		if l.dumpDir != "" {
			l.dumped++
			name := filepath.Join(l.dumpDir, fmt.Sprintf("%05d_%d.lh5", l.dumped, dstSize))
			_ = os.WriteFile(name, c.Mem[src:src+srcSize], 0o644)
		}
		if !l.off && l.decode(c, src, dst, srcSize, dstSize) {
			l.native++
			c.Regs[cpu.RegEAX] = dstSize
			c.Ret()
			return
		}
	}
	// The original: execute its first instruction (push ebp) and continue after it.
	c.Push32(c.Regs[cpu.RegEBP])
	c.EIP = l.g.fn + 1
}

// decode decodes natively, or returns false having changed nothing.
func (l *lzhuf) decode(c *cpu.CPU, src, dst, srcSize, dstSize uint32) bool {
	g, s, m := &l.g, &l.s, c.Mem
	loadU16s(s.CTable[:], m[g.cTable:])
	copy(s.CLen[:], m[g.cLen:])
	loadU16s(s.PTTable[:], m[g.ptTable:])
	copy(s.PTLen[:], m[g.ptLen:])
	loadU16s(s.Left[:], m[g.left:])
	loadU16s(s.Right[:], m[g.right:])
	copy(s.Text[:], m[g.text:])
	s.I = c.Read32(g.i)

	s.In = m[src : src+srcSize]
	s.InitBits()
	s.BlockSize, s.J = 0, 0
	out := make([]byte, dstSize)
	for off := 0; off < len(out); {
		n := min(len(out)-off, lh5.DicSize)
		if s.Decode(n) != nil {
			return false
		}
		copy(out[off:], s.Text[:n])
		off += n
	}

	copy(c.WBytes(dst, dstSize), out)
	consumed := srcSize - uint32(len(s.In))
	c.Write32(g.inPtr, src+consumed)
	c.Write32(g.inLeft, srcSize-consumed)
	c.Write32(g.outPtr, dst+dstSize)
	c.Write32(g.outLeft, 0)
	c.Write32(g.bitBuf, s.BitBuf)
	c.Write32(g.subBitBuf, s.SubBitBuf)
	c.Write32(g.bitCount, s.BitCount)
	c.Write32(g.blockSize, s.BlockSize)
	c.Write32(g.j, uint32(s.J))
	c.Write32(g.i, s.I)
	storeU16s(m[g.cTable:], s.CTable[:])
	copy(m[g.cLen:], s.CLen[:])
	storeU16s(m[g.ptTable:], s.PTTable[:])
	copy(m[g.ptLen:], s.PTLen[:])
	storeU16s(m[g.left:], s.Left[:])
	storeU16s(m[g.right:], s.Right[:])
	copy(m[g.text:], s.Text[:])
	return true
}

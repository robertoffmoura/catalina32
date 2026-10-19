package lswsaga

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"catalina32/pkg/cpu"
	"catalina32/pkg/lh5"
)

// NuLZHUF_Decompress (0x002734f0), int (src, dst, srcSize, dstSize): the engine's LHA -lh5-
// decoder (ar002), which the game runs on level data it streams in while the player moves
// between areas. Emulated, it held the frame rate near zero for seconds at a time; natively it
// takes milliseconds. The decoder keeps its state in globals; the native version loads them,
// decodes with pkg/lh5 and stores them back as the original leaves them, so the two are
// indistinguishable (TestLZHUFMatchesGuest). Input the original would mis-decode is left to it.
const (
	lzhufDecompress = 0x002734f0

	lzhufInPtr     = 0x02b940cc
	lzhufOutPtr    = 0x02b940d0
	lzhufBitBuf    = 0x02b940d4
	lzhufOutLeft   = 0x02b940d8
	lzhufInLeft    = 0x02b940dc
	lzhufLeft      = 0x02b940e0 // u16 [2*NC-1]
	lzhufRight     = 0x02b948d6 // u16 [2*NC-1]
	lzhufBitCount  = 0x02cccf90
	lzhufSubBitBuf = 0x02cccf94
	lzhufDisabled  = 0x02cccf9c // nonzero: decompress returns 0 and does nothing
	lzhufText      = 0x02cccf9d // the 8 KB window
	lzhufBlockSize = 0x02ccefa0
	lzhufCTable    = 0x02ccefa4 // u16 [4096]
	lzhufCLen      = 0x02cd0fa4 // u8 [NC]
	lzhufPTTable   = 0x02cd11a2 // u16 [256]
	lzhufPTLen     = 0x02cd13a2 // u8 [NPT]
	lzhufJ         = 0x02cd23f4
	lzhufI         = 0x02cd23f8
)

var (
	lzhufDumpDir  = os.Getenv("C32_DUMP_LZHUF")             // save each compressed input here (tests)
	lzhufNoNative = os.Getenv("C32_NO_NATIVE_LZHUF") == "1" // run the game's own decoder (A/B tests)
	lzhufDumped   int
	lzhufNative   int // calls decoded natively
)

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

func nativeNuLZHUFDecompress(c *cpu.CPU) {
	src, dst, srcSize, dstSize := c.Arg(0), c.Arg(1), c.Arg(2), c.Arg(3)
	if c.Read8(lzhufDisabled) == 0 && inArena(src, srcSize) && inArena(dst, dstSize) {
		if lzhufDumpDir != "" {
			lzhufDumped++
			name := filepath.Join(lzhufDumpDir, fmt.Sprintf("%05d_%d.lh5", lzhufDumped, dstSize))
			_ = os.WriteFile(name, c.Mem[src:src+srcSize], 0o644)
		}
		if !lzhufNoNative && lzhufDecodeNative(c, src, dst, srcSize, dstSize) {
			lzhufNative++
			c.Regs[cpu.RegEAX] = dstSize
			c.Ret()
			return
		}
	}
	// The original: execute its first instruction (push ebp) and continue after it.
	c.Push32(c.Regs[cpu.RegEBP])
	c.EIP = lzhufDecompress + 1
}

var lzhufState lh5.State // reused: the emulation goroutine is the only caller

// lzhufDecodeNative decodes natively, or returns false having changed nothing.
func lzhufDecodeNative(c *cpu.CPU, src, dst, srcSize, dstSize uint32) bool {
	s := &lzhufState
	m := c.Mem
	loadU16s(s.CTable[:], m[lzhufCTable:])
	copy(s.CLen[:], m[lzhufCLen:])
	loadU16s(s.PTTable[:], m[lzhufPTTable:])
	copy(s.PTLen[:], m[lzhufPTLen:])
	loadU16s(s.Left[:], m[lzhufLeft:])
	loadU16s(s.Right[:], m[lzhufRight:])
	copy(s.Text[:], m[lzhufText:])
	s.I = c.Read32(lzhufI)

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
	c.Write32(lzhufInPtr, src+consumed)
	c.Write32(lzhufInLeft, srcSize-consumed)
	c.Write32(lzhufOutPtr, dst+dstSize)
	c.Write32(lzhufOutLeft, 0)
	c.Write32(lzhufBitBuf, s.BitBuf)
	c.Write32(lzhufSubBitBuf, s.SubBitBuf)
	c.Write32(lzhufBitCount, s.BitCount)
	c.Write32(lzhufBlockSize, s.BlockSize)
	c.Write32(lzhufJ, uint32(s.J))
	c.Write32(lzhufI, s.I)
	storeU16s(m[lzhufCTable:], s.CTable[:])
	copy(m[lzhufCLen:], s.CLen[:])
	storeU16s(m[lzhufPTTable:], s.PTTable[:])
	copy(m[lzhufPTLen:], s.PTLen[:])
	storeU16s(m[lzhufLeft:], s.Left[:])
	storeU16s(m[lzhufRight:], s.Right[:])
	copy(m[lzhufText:], s.Text[:])
	return true
}

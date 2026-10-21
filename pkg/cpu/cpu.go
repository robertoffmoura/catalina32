package cpu

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

const (
	// ArenaSize is the guest address space: 3 GB, reserved up front but only backed by memory
	// where the guest touches it (newArena). Games set their own heap range (SetHeap).
	ArenaSize     = 0xC0000000
	HeapStartAddr = 0x04000000 // the default heap range (LEGO Star Wars' layout)
	HeapEndAddr   = 0x1e000000
	// Host-reserved guest region (D3D vtable + trampoline arenas live here); the heap must never allocate inside it.
	ReservedLo        = 0x0f000000
	ReservedHi        = 0x0f400000
	StackTopAddr      = 0x1fef0000
	SentinelReturnEIP = 0x0ffffff0
	ExitReturnEIP     = 0x0ffffff4
)

// Register indices
const (
	RegEAX = 0
	RegECX = 1
	RegEDX = 2
	RegEBX = 3
	RegESP = 4
	RegEBP = 5
	RegESI = 6
	RegEDI = 7
)

var RegNames32 = [8]string{"eax", "ecx", "edx", "ebx", "esp", "ebp", "esi", "edi"}
var RegNames16 = [8]string{"ax", "cx", "dx", "bx", "sp", "bp", "si", "di"}
var RegNames8 = [8]string{"al", "cl", "dl", "bl", "ah", "ch", "dh", "bh"}

// EFLAGS bits
const (
	FlagCF = 1 << 0
	FlagPF = 1 << 2
	FlagAF = 1 << 4
	FlagZF = 1 << 6
	FlagSF = 1 << 7
	FlagDF = 1 << 10
	FlagOF = 1 << 11
)

type ThunkFunc func(c *CPU)

type CallFrame struct {
	CallerEIP uint32
	TargetEIP uint32
	ESP       uint32
}

type CPU struct {
	Mem   []byte
	Regs  [8]uint32
	EIP   uint32
	Flags uint32

	// Segment bases (for TLS / GS / FS)
	FSBase uint32
	GSBase uint32

	// x87 FPU stack (8 registers, TOP pointer)
	FPUSt  [8]float64
	FPUTop int
	FPUCW  uint16
	FPUSW  uint16

	// SSE XMM0..XMM7 registers (128-bit = 16 bytes each)
	XMM   [8][16]byte
	MM    [8]uint64 // MMX registers (mmx.go)
	MXCSR uint32

	// Heap allocator state
	HeapPtr     uint32
	HeapStart   uint32 // the heap range: [HeapStart, HeapEnd), minus ReservedLo..ReservedHi
	HeapEnd     uint32
	AllocSize   map[uint32]uint32
	FreeBlocks  map[uint32][]uint32 // size class -> free list
	freeExtents []extent            // free blocks above smallBlock, by address
	heapInUse   uint64

	// Intercept table (stubs, MacDoze, IndirectX, COM vtables, Lifted functions)
	Intercepts map[uint32]ThunkFunc
	thunkCache [1024]struct {
		eip uint32
		fn  ThunkFunc
	} // direct-mapped cache in front of Intercepts (cleared when intercepts change)
	icept        []uint64 // bitmap over guest addresses: fast pre-check before the Intercepts map lookup
	InterceptTag map[uint32]string

	// Execution telemetry & debugging
	Steps         uint64
	MaxSteps      uint64
	Halted        bool
	HaltReason    string
	Exited        bool   // the game ended the process itself (ExitProcess): Halted, and not an error
	ExitCode      uint32 // its exit code
	TraceCalls    bool
	CallStack     []CallFrame
	UnmappedCalls map[uint32]uint64
	VisitedFuncs  map[uint32]uint64

	// Decoded-instruction cache (dcache.go); active once SetCodeRange is called.
	dcache           []dInst
	blocks           []dBlock
	CodeLo, CodeHi   uint32
	CodeCacheFlushes uint64
	dcacheUsed       bool // something was decoded; only then do intercept changes need a flush
	// With several images in the code range, their data lies between their code: textPages marks
	// the 4 KB pages holding code, and only stores to those flush (nil: the whole range is code).
	textPages []uint64
	// The inlined fast paths: loads with addr < readLimit and stores with addr-CodeHi < writeSpan
	// are in bounds, above guest code (stores) and not watched. SetWatch zeroes both.
	readLimit uint32
	writeSpan uint32

	// Guest sampling profiler (enabled when ProfEvery > 0; needs TraceCalls for function attribution).
	// Every ProfEvery steps past ProfFrom, the innermost call frame is charged in ProfSelf and every
	// distinct frame on the call stack in ProfIncl.
	ProfEvery   uint64
	ProfFrom    uint64
	ProfSamples uint64
	ProfSelf    map[uint32]uint64
	ProfIncl    map[uint32]uint64
	profSeen    map[uint32]bool
	reported    map[string]bool // ReportOnce keys

	// Green-thread scheduling support. NestDepth counts host-initiated nested guest calls
	// (CallGuest/CallGuestNested); thread switches are only legal at depth 0.
	NestDepth       int
	Preempt         func(c *CPU)
	PreemptInterval uint64
	nextPreempt     uint64

	// Debug: optional watchpoint over [watchLo, watchHi) (SetWatch).
	watchLo, watchHi uint32
	watchFn          func(c *CPU, addr uint32, size int, write bool)

	// JIT (jit.go): hot blocks translated to host code. NoJIT keeps this CPU interpreted.
	NoJIT  bool
	jit    jitState
	jitc   *jitCode
	jitGen uint32 // bumped when translations are discarded (dBlock.jitGen must match)
}

// SetWatch calls fn for every guest load and store that touches [lo, hi). Accesses then all take
// the slow path, so this is for debugging only.
func (c *CPU) SetWatch(lo, hi uint32, fn func(c *CPU, addr uint32, size int, write bool)) {
	c.watchLo, c.watchHi, c.watchFn = lo, hi, fn
	c.readLimit, c.writeSpan = 0, 0
}

func (c *CPU) SetPreemptInterval(interval uint64) {
	c.PreemptInterval = interval
	c.nextPreempt = c.Steps + interval
}

func NewCPU() *CPU {
	c := &CPU{
		Mem:           newArena(ArenaSize),
		Flags:         0x00000002,
		FPUCW:         0x037F,
		MXCSR:         0x1F80,
		HeapPtr:       HeapStartAddr,
		HeapStart:     HeapStartAddr,
		HeapEnd:       HeapEndAddr,
		AllocSize:     make(map[uint32]uint32),
		FreeBlocks:    make(map[uint32][]uint32),
		Intercepts:    make(map[uint32]ThunkFunc),
		InterceptTag:  make(map[uint32]string),
		UnmappedCalls: make(map[uint32]uint64),
		VisitedFuncs:  make(map[uint32]uint64),
		icept:         newBitmap(ArenaSize / 64),
		readLimit:     ArenaSize - 8,
		writeSpan:     ArenaSize - 7,
		FSBase:        0x03004000,
		GSBase:        0x03008000,
	}
	c.Regs[RegESP] = StackTopAddr
	c.Regs[RegEBP] = StackTopAddr
	return c
}

func (c *CPU) RegisterIntercept(addr uint32, name string, fn ThunkFunc) {
	c.Intercepts[addr] = fn
	c.thunkCache = [len(c.thunkCache)]struct {
		eip uint32
		fn  ThunkFunc
	}{}
	if c.icept == nil {
		c.icept = newBitmap(ArenaSize / 64)
	}
	if addr < ArenaSize {
		c.icept[addr>>6] |= 1 << (addr & 63)
	}
	if addr >= c.CodeLo && addr < c.CodeHi && c.dcacheUsed {
		c.FlushCodeCache() // blocks are built assuming no intercept inside them
	}
	c.InterceptTag[addr] = name
}

// SetInterceptEnabled toggles an already registered intercept, e.g. so a host thunk can run the
// original guest code it replaces (differential verification).
func (c *CPU) SetInterceptEnabled(addr uint32, on bool) {
	if addr >= ArenaSize {
		return
	}
	if on {
		c.icept[addr>>6] |= 1 << (addr & 63)
	} else {
		c.icept[addr>>6] &^= 1 << (addr & 63)
	}
	if addr >= c.CodeLo && addr < c.CodeHi && c.dcacheUsed {
		c.FlushCodeCache()
	}
}

// Memory accessors
func (c *CPU) checkAddr(addr uint32, sz uint32) bool {
	if uint64(addr)+uint64(sz) > ArenaSize {
		c.Halted = true
		c.HaltReason = fmt.Sprintf("OOB memory access addr=0x%08x at EIP=0x%08x", addr, c.EIP)
		return false
	}
	return true
}

// Each accessor has an inlinable fast path (one compare) for in-bounds accesses with no watchpoint
// armed; everything else goes through access, which halts on OOB and reports to the watchpoint.

// access checks a slow-path access and reports it to the watchpoint; false means out of range.
func (c *CPU) access(addr, size uint32, write bool) bool {
	if !c.checkAddr(addr, size) {
		return false
	}
	if write {
		c.noteCodeWrite(addr, size)
	}
	if c.watchFn != nil && addr+size > c.watchLo && addr < c.watchHi {
		c.watchFn(c, addr, int(size), write)
	}
	return true
}

func (c *CPU) Read8(addr uint32) uint8 {
	if addr < c.readLimit {
		return c.Mem[addr]
	}
	if !c.access(addr, 1, false) {
		return 0
	}
	return c.Mem[addr]
}

func (c *CPU) Read16(addr uint32) uint16 {
	if addr < c.readLimit {
		return binary.LittleEndian.Uint16(c.Mem[addr : addr+2])
	}
	return c.read16Slow(addr)
}

func (c *CPU) read16Slow(addr uint32) uint16 {
	if !c.access(addr, 2, false) {
		return 0
	}
	return binary.LittleEndian.Uint16(c.Mem[addr : addr+2])
}

func (c *CPU) Read32(addr uint32) uint32 {
	if addr < c.readLimit {
		return binary.LittleEndian.Uint32(c.Mem[addr : addr+4])
	}
	return c.read32Slow(addr)
}

func (c *CPU) read32Slow(addr uint32) uint32 {
	if !c.access(addr, 4, false) {
		return 0
	}
	return binary.LittleEndian.Uint32(c.Mem[addr : addr+4])
}

func (c *CPU) Read64(addr uint32) uint64 {
	if addr < c.readLimit {
		return binary.LittleEndian.Uint64(c.Mem[addr : addr+8])
	}
	return c.read64Slow(addr)
}

func (c *CPU) read64Slow(addr uint32) uint64 {
	if !c.access(addr, 8, false) {
		return 0
	}
	return binary.LittleEndian.Uint64(c.Mem[addr : addr+8])
}

// Read128 and Write128 move 16 bytes (SSE loads and stores).
func (c *CPU) Read128(addr uint32) (v [16]byte) {
	if uint64(addr)+16 <= uint64(c.readLimit) {
		copy(v[:], c.Mem[addr:addr+16])
	} else if c.access(addr, 16, false) {
		copy(v[:], c.Mem[addr:addr+16])
	}
	return v
}

func (c *CPU) Write128(addr uint32, v [16]byte) {
	if addr-c.CodeHi < c.writeSpan && addr <= ArenaSize-16 {
		copy(c.Mem[addr:addr+16], v[:])
	} else if c.access(addr, 16, true) {
		copy(c.Mem[addr:addr+16], v[:])
	}
}

func (c *CPU) ReadF32(addr uint32) float32 {
	return math.Float32frombits(c.Read32(addr))
}

func (c *CPU) ReadF64(addr uint32) float64 {
	return math.Float64frombits(c.Read64(addr))
}

func (c *CPU) Write8(addr uint32, val uint8) {
	if addr-c.CodeHi < c.writeSpan {
		c.Mem[addr] = val
		return
	}
	c.write8Slow(addr, val)
}

func (c *CPU) write8Slow(addr uint32, val uint8) {
	if c.access(addr, 1, true) {
		c.Mem[addr] = val
	}
}

func (c *CPU) Write16(addr uint32, val uint16) {
	if addr-c.CodeHi < c.writeSpan {
		binary.LittleEndian.PutUint16(c.Mem[addr:addr+2], val)
		return
	}
	c.write16Slow(addr, val)
}

func (c *CPU) write16Slow(addr uint32, val uint16) {
	if c.access(addr, 2, true) {
		binary.LittleEndian.PutUint16(c.Mem[addr:addr+2], val)
	}
}

func (c *CPU) Write32(addr uint32, val uint32) {
	if addr-c.CodeHi < c.writeSpan {
		binary.LittleEndian.PutUint32(c.Mem[addr:addr+4], val)
		return
	}
	c.write32Slow(addr, val)
}

func (c *CPU) write32Slow(addr uint32, val uint32) {
	if c.access(addr, 4, true) {
		binary.LittleEndian.PutUint32(c.Mem[addr:addr+4], val)
	}
}

func (c *CPU) Write64(addr uint32, val uint64) {
	if addr-c.CodeHi < c.writeSpan {
		binary.LittleEndian.PutUint64(c.Mem[addr:addr+8], val)
		return
	}
	c.write64Slow(addr, val)
}

func (c *CPU) write64Slow(addr uint32, val uint64) {
	if c.access(addr, 8, true) {
		binary.LittleEndian.PutUint64(c.Mem[addr:addr+8], val)
	}
}

// noteCodeWrite flushes the decoded-instruction cache when a guest store touches cached code.
func (c *CPU) noteCodeWrite(addr, size uint32) {
	if addr < c.CodeHi && addr+size > c.CodeLo && c.dcache != nil &&
		(c.textPages == nil || c.isTextPage(addr) || c.isTextPage(addr+size-1)) {
		c.FlushCodeCache()
	}
}

// MarkTextPages records that [lo, hi) holds code (see textPages).
func (c *CPU) MarkTextPages(lo, hi uint32) {
	if c.jitc != nil {
		c.jitc.stale = true
	}
	if c.textPages == nil {
		c.textPages = make([]uint64, ArenaSize>>12/64)
	}
	for p := lo >> 12; p < (hi+0xfff)>>12 && p < ArenaSize>>12; p++ {
		c.textPages[p>>6] |= 1 << (p & 63)
	}
}

func (c *CPU) isTextPage(addr uint32) bool {
	p := addr >> 12
	return p < ArenaSize>>12 && c.textPages[p>>6]&(1<<(p&63)) != 0
}

func (c *CPU) WriteF32(addr uint32, val float32) {
	c.Write32(addr, math.Float32bits(val))
}

func (c *CPU) WriteF64(addr uint32, val float64) {
	c.Write64(addr, math.Float64bits(val))
}

// Host shims access guest memory in bulk through Bytes, WBytes and CBytes rather than slicing
// c.Mem: an out-of-range range halts the guest (instead of a Go panic), and writes that touch
// guest code flush the decoded-instruction cache.

// Bytes returns guest memory [addr, addr+n) for reading, or nil (halting the guest) when the
// range does not fit.
func (c *CPU) Bytes(addr, n uint32) []byte {
	if uint64(addr)+uint64(n) > ArenaSize {
		c.checkAddr(addr, n)
		return nil
	}
	return c.Mem[addr : addr+n : addr+n]
}

// WBytes is Bytes for a range the caller is about to write.
func (c *CPU) WBytes(addr, n uint32) []byte {
	b := c.Bytes(addr, n)
	if b != nil {
		c.noteCodeWrite(addr, n)
	}
	return b
}

// CBytes returns the NUL-terminated string at addr without the terminator (nil for a null or
// out-of-range pointer). The slice aliases guest memory.
func (c *CPU) CBytes(addr uint32) []byte {
	if addr == 0 || addr >= ArenaSize {
		return nil
	}
	m := c.Mem[addr:ArenaSize]
	if n := bytes.IndexByte(m, 0); n >= 0 {
		return m[:n:n]
	}
	return m
}

// ReportOnce prints a message the first time key is seen (gaps in the shims: unimplemented
// functions, deadlocks), with the guest EIP of that first occurrence.
func (c *CPU) ReportOnce(key, format string, args ...any) {
	if c.reported[key] {
		return
	}
	if c.reported == nil {
		c.reported = map[string]bool{}
	}
	c.reported[key] = true
	fmt.Printf(format+" (first at EIP=0x%08x)\n", append(args, c.EIP)...)
}

// PatchCode overwrites guest code (host-side binary patches), invalidating decoded instructions.
// Patch before compiled functions are registered: CodeHash then sees the patched bytes.
func (c *CPU) PatchCode(addr uint32, b []byte) {
	copy(c.WBytes(addr, uint32(len(b))), b)
}

// CodeHash returns the FNV-1a hash of guest memory [lo, hi): compiled functions compare it with
// the bytes they were compiled from before they are installed.
func (c *CPU) CodeHash(lo, hi uint32) uint64 {
	h := uint64(14695981039346656037)
	for _, b := range c.Bytes(lo, hi-lo) {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return h
}

func (c *CPU) ReadCString(addr uint32) string {
	return string(c.CBytes(addr))
}

func (c *CPU) WriteCString(addr uint32, s string) {
	if b := c.WBytes(addr, uint32(len(s))+1); b != nil {
		copy(b, s)
		b[len(s)] = 0
	}
}

// 8-bit & 16-bit register accessors
func (c *CPU) GetReg8(idx int) uint8 {
	if idx < 4 {
		return uint8(c.Regs[idx])
	}
	return uint8(c.Regs[idx-4] >> 8)
}

func (c *CPU) SetReg8(idx int, val uint8) {
	if idx < 4 {
		c.Regs[idx] = (c.Regs[idx] & 0xFFFFFF00) | uint32(val)
	} else {
		c.Regs[idx-4] = (c.Regs[idx-4] & 0xFFFF00FF) | (uint32(val) << 8)
	}
}

func (c *CPU) GetReg16(idx int) uint16 {
	return uint16(c.Regs[idx])
}

func (c *CPU) SetReg16(idx int, val uint16) {
	c.Regs[idx] = (c.Regs[idx] & 0xFFFF0000) | uint32(val)
}

// Stack helpers
func (c *CPU) Push32(val uint32) {
	c.Regs[RegESP] -= 4
	c.Write32(c.Regs[RegESP], val)
}

func (c *CPU) Pop32() uint32 {
	val := c.Read32(c.Regs[RegESP])
	c.Regs[RegESP] += 4
	return val
}

func (c *CPU) Push16(val uint16) {
	c.Regs[RegESP] -= 2
	c.Write16(c.Regs[RegESP], val)
}

func (c *CPU) Pop16() uint16 {
	val := c.Read16(c.Regs[RegESP])
	c.Regs[RegESP] += 2
	return val
}

// Calling convention helpers for Thunks
func (c *CPU) Arg(i int) uint32 {
	return c.Read32(c.Regs[RegESP] + uint32((i+1)*4))
}

func (c *CPU) ArgF32(i int) float32 {
	return math.Float32frombits(c.Arg(i))
}

func (c *CPU) sampleProfile() {
	c.ProfSamples++
	if len(c.CallStack) == 0 {
		c.ProfSelf[0]++
		return
	}
	c.ProfSelf[c.CallStack[len(c.CallStack)-1].TargetEIP]++
	if c.profSeen == nil {
		c.profSeen = map[uint32]bool{}
	}
	clear(c.profSeen)
	for _, f := range c.CallStack { // each function once per sample, however deep the recursion
		if !c.profSeen[f.TargetEIP] {
			c.profSeen[f.TargetEIP] = true
			c.ProfIncl[f.TargetEIP]++
		}
	}
}

func (c *CPU) Ret() {
	c.EIP = c.Pop32()
	if c.TraceCalls && len(c.CallStack) > 0 {
		c.CallStack = c.CallStack[:len(c.CallStack)-1]
	}
}

func (c *CPU) RetN(popBytes uint32) {
	c.EIP = c.Pop32()
	c.Regs[RegESP] += popBytes
	if c.TraceCalls && len(c.CallStack) > 0 {
		c.CallStack = c.CallStack[:len(c.CallStack)-1]
	}
}

// Guest heap allocator. Blocks are 16-byte aligned. Freed blocks up to 64 KB go to exact-size
// free lists; larger ones go to an address-ordered list of free extents (coalesced with their
// neighbours), which large requests search first-fit before the bump pointer, and every request
// falls back to when the bump region is used up.

// smallBlock is the largest block size kept on the exact-size free lists.
const smallBlock = 65536

type extent struct{ addr, size uint32 }

// HeapInUse returns the bytes currently allocated.
func (c *CPU) HeapInUse() uint64 { return c.heapInUse }

// HeapPeak returns the bump pointer's distance from the heap start (the high-water mark).
func (c *CPU) HeapPeak() uint32 { return c.HeapPtr - c.HeapStart }

// SetHeap moves the (still empty) heap to [lo, hi).
func (c *CPU) SetHeap(lo, hi uint32) {
	c.HeapStart, c.HeapEnd, c.HeapPtr = lo, hi, lo
}

func (c *CPU) Malloc(size uint32) uint32 {
	if size == 0 {
		size = 16
	}
	if size > c.HeapEnd {
		return c.heapExhausted(size)
	}
	aligned := (size + 15) & ^uint32(15)
	ptr := uint32(0)
	if list := c.FreeBlocks[aligned]; len(list) > 0 {
		ptr = list[len(list)-1]
		c.FreeBlocks[aligned] = list[:len(list)-1]
	} else if aligned > smallBlock {
		ptr = c.takeExtent(aligned)
	}
	if ptr == 0 {
		if c.HeapPtr < ReservedHi && c.HeapPtr+aligned > ReservedLo {
			c.HeapPtr = ReservedHi // skip the host-reserved region
		}
		if uint64(c.HeapPtr)+uint64(aligned) <= uint64(c.HeapEnd) {
			ptr = c.HeapPtr
			c.HeapPtr += aligned
		} else if ptr = c.takeExtent(aligned); ptr == 0 {
			return c.heapExhausted(size)
		}
	}
	c.AllocSize[ptr] = aligned
	c.heapInUse += uint64(aligned)
	return ptr
}

// heapExhausted stops the guest (the alternative is handing out memory that is in use) and
// returns NULL.
func (c *CPU) heapExhausted(size uint32) uint32 {
	c.Halted = true
	c.HaltReason = fmt.Sprintf("guest heap exhausted (request %d bytes, %d MB in use) at EIP=0x%08x",
		size, c.heapInUse>>20, c.EIP)
	return 0
}

// takeExtent carves size bytes from the first free extent large enough, or returns 0.
func (c *CPU) takeExtent(size uint32) uint32 {
	for i, e := range c.freeExtents {
		if e.size >= size {
			if e.size == size {
				c.freeExtents = append(c.freeExtents[:i], c.freeExtents[i+1:]...)
			} else {
				c.freeExtents[i] = extent{e.addr + size, e.size - size}
			}
			return e.addr
		}
	}
	return 0
}

// putExtent returns a block to the extent list, merging it with adjacent free extents.
func (c *CPU) putExtent(addr, size uint32) {
	l := c.freeExtents
	i := sort.Search(len(l), func(i int) bool { return l[i].addr > addr })
	if i > 0 && l[i-1].addr+l[i-1].size == addr {
		l[i-1].size += size
		if i < len(l) && addr+size == l[i].addr {
			l[i-1].size += l[i].size
			l = append(l[:i], l[i+1:]...)
		}
	} else if i < len(l) && addr+size == l[i].addr {
		l[i] = extent{addr, size + l[i].size}
	} else {
		l = append(l, extent{})
		copy(l[i+1:], l[i:])
		l[i] = extent{addr, size}
	}
	c.freeExtents = l
}

func (c *CPU) Calloc(num, size uint32) uint32 {
	total := uint64(num) * uint64(size)
	if total > uint64(c.HeapEnd) {
		return c.heapExhausted(uint32(min(total, 1<<32-1)))
	}
	ptr := c.Malloc(uint32(total))
	if ptr != 0 {
		clear(c.Mem[ptr : ptr+uint32(total)])
	}
	return ptr
}

func (c *CPU) Free(ptr uint32) {
	if ptr == 0 {
		return
	}
	if sz, ok := c.AllocSize[ptr]; ok {
		delete(c.AllocSize, ptr)
		c.heapInUse -= uint64(sz)
		if sz <= smallBlock {
			c.FreeBlocks[sz] = append(c.FreeBlocks[sz], ptr)
		} else {
			c.putExtent(ptr, sz)
		}
	}
}

func (c *CPU) Realloc(ptr, newSize uint32) uint32 {
	if ptr == 0 {
		return c.Malloc(newSize)
	}
	if newSize == 0 {
		c.Free(ptr)
		return 0
	}
	oldSize, known := c.AllocSize[ptr]
	if newSize <= oldSize {
		return ptr
	}
	newPtr := c.Malloc(newSize)
	if newPtr == 0 {
		return 0
	}
	if !known {
		// Not from this heap (static data, or a pointer the game made up): its size is unknown,
		// so keep as much as fits instead of losing the contents.
		c.ReportOnce(fmt.Sprintf("realloc:%x", ptr), "[heap] realloc of 0x%08x, which this heap did not allocate: copying %d bytes", ptr, newSize)
		oldSize = 0
		if ptr < ArenaSize {
			oldSize = min(newSize, ArenaSize-ptr)
		}
	}
	copy(c.WBytes(newPtr, oldSize), c.Bytes(ptr, oldSize))
	c.Free(ptr)
	return newPtr
}

// x87 FPU Stack Helpers
func (c *CPU) FPUPush(v float64) {
	c.FPUTop = (c.FPUTop - 1) & 7
	c.FPUSt[c.FPUTop] = v
}

func (c *CPU) FPUPop() float64 {
	v := c.FPUSt[c.FPUTop]
	c.FPUTop = (c.FPUTop + 1) & 7
	return v
}

func (c *CPU) FPUGet(i int) float64 {
	return c.FPUSt[(c.FPUTop+i)&7]
}

func (c *CPU) FPUSet(i int, v float64) {
	c.FPUSt[(c.FPUTop+i)&7] = v
}

// Flag Helpers
func parity8(v uint8) bool {
	v ^= v >> 4
	v ^= v >> 2
	v ^= v >> 1
	return (v & 1) == 0
}

func (c *CPU) SetFlag(mask uint32, cond bool) {
	if cond {
		c.Flags |= mask
	} else {
		c.Flags &^= mask
	}
}

func (c *CPU) GetFlag(mask uint32) bool {
	return (c.Flags & mask) != 0
}

// parityTab[v] is FlagPF when v has an even number of set bits.
var parityTab = func() (t [256]uint32) {
	for i := range t {
		if parity8(uint8(i)) {
			t[i] = FlagPF
		}
	}
	return
}()

const arithFlags = FlagCF | FlagPF | FlagAF | FlagZF | FlagSF | FlagOF

func flagIf(cond bool, mask uint32) uint32 {
	if cond {
		return mask
	}
	return 0
}

// The UpdateFlags* helpers rebuild all six arithmetic flags in one store; SF and OF are moved into
// place with shifts (SF = bit 7, OF = bit 11) instead of per-flag read-modify-writes.

func (c *CPU) UpdateFlagsLogic32(res uint32) {
	c.Flags = c.Flags&^arithFlags | flagIf(res == 0, FlagZF) | (res>>24)&FlagSF | parityTab[uint8(res)]
}

func (c *CPU) UpdateFlagsLogic16(res uint16) {
	r := uint32(res)
	c.Flags = c.Flags&^arithFlags | flagIf(r == 0, FlagZF) | (r>>8)&FlagSF | parityTab[uint8(r)]
}

func (c *CPU) UpdateFlagsLogic8(res uint8) {
	r := uint32(res)
	c.Flags = c.Flags&^arithFlags | flagIf(r == 0, FlagZF) | r&FlagSF | parityTab[r]
}

func (c *CPU) UpdateFlagsAdd32(a, b, res uint32) {
	c.Flags = c.Flags&^arithFlags | flagIf(res < a, FlagCF) | flagIf(res == 0, FlagZF) | (res>>24)&FlagSF |
		parityTab[uint8(res)] | (a^b^res)&FlagAF | ((^(a^b)&(a^res))>>20)&FlagOF
}

func (c *CPU) UpdateFlagsSub32(a, b, res uint32) {
	c.Flags = c.Flags&^arithFlags | flagIf(a < b, FlagCF) | flagIf(res == 0, FlagZF) | (res>>24)&FlagSF |
		parityTab[uint8(res)] | (a^b^res)&FlagAF | (((a^b)&(a^res))>>20)&FlagOF
}

func (c *CPU) UpdateFlagsAdd16(a, b, res uint16) {
	A, B, R := uint32(a), uint32(b), uint32(res)
	c.Flags = c.Flags&^arithFlags | flagIf(res < a, FlagCF) | flagIf(R == 0, FlagZF) | (R>>8)&FlagSF |
		parityTab[uint8(R)] | (A^B^R)&FlagAF | ((^(A^B)&(A^R))>>4)&FlagOF
}

func (c *CPU) UpdateFlagsSub16(a, b, res uint16) {
	A, B, R := uint32(a), uint32(b), uint32(res)
	c.Flags = c.Flags&^arithFlags | flagIf(a < b, FlagCF) | flagIf(R == 0, FlagZF) | (R>>8)&FlagSF |
		parityTab[uint8(R)] | (A^B^R)&FlagAF | (((A^B)&(A^R))>>4)&FlagOF
}

func (c *CPU) UpdateFlagsAdd8(a, b, res uint8) {
	A, B, R := uint32(a), uint32(b), uint32(res)
	c.Flags = c.Flags&^arithFlags | flagIf(res < a, FlagCF) | flagIf(R == 0, FlagZF) | R&FlagSF |
		parityTab[R] | (A^B^R)&FlagAF | ((^(A^B)&(A^R))<<4)&FlagOF
}

func (c *CPU) UpdateFlagsSub8(a, b, res uint8) {
	A, B, R := uint32(a), uint32(b), uint32(res)
	c.Flags = c.Flags&^arithFlags | flagIf(a < b, FlagCF) | flagIf(R == 0, FlagZF) | R&FlagSF |
		parityTab[R] | (A^B^R)&FlagAF | (((A^B)&(A^R))<<4)&FlagOF
}

func (c *CPU) EvalCond(cc uint8) bool {
	switch cc & 0x0F {
	case 0x0: // JO
		return c.GetFlag(FlagOF)
	case 0x1: // JNO
		return !c.GetFlag(FlagOF)
	case 0x2: // JB / JC
		return c.GetFlag(FlagCF)
	case 0x3: // JAE / JNC
		return !c.GetFlag(FlagCF)
	case 0x4: // JE / JZ
		return c.GetFlag(FlagZF)
	case 0x5: // JNE / JNZ
		return !c.GetFlag(FlagZF)
	case 0x6: // JBE / JNA
		return c.GetFlag(FlagCF) || c.GetFlag(FlagZF)
	case 0x7: // JA / JNBE
		return !c.GetFlag(FlagCF) && !c.GetFlag(FlagZF)
	case 0x8: // JS
		return c.GetFlag(FlagSF)
	case 0x9: // JNS
		return !c.GetFlag(FlagSF)
	case 0xA: // JP / JPE
		return c.GetFlag(FlagPF)
	case 0xB: // JNP / JPO
		return !c.GetFlag(FlagPF)
	case 0xC: // JL / JNGE
		return c.GetFlag(FlagSF) != c.GetFlag(FlagOF)
	case 0xD: // JGE / JNL
		return c.GetFlag(FlagSF) == c.GetFlag(FlagOF)
	case 0xE: // JLE / JNG
		return c.GetFlag(FlagZF) || (c.GetFlag(FlagSF) != c.GetFlag(FlagOF))
	case 0xF: // JG / JNLE
		return !c.GetFlag(FlagZF) && (c.GetFlag(FlagSF) == c.GetFlag(FlagOF))
	}
	return false
}

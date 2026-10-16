package cpu

import (
	"encoding/binary"
	"fmt"
	"math"
)

const (
	ArenaSize     = 0x20000000 // 512 MB 32-bit guest address space
	HeapStartAddr = 0x04000000
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
	MXCSR uint32

	// Heap allocator state
	HeapPtr    uint32
	AllocSize  map[uint32]uint32
	FreeBlocks map[uint32][]uint32 // size class -> free list

	// Intercept table (stubs, MacDoze, IndirectX, COM vtables, Lifted functions)
	Intercepts   map[uint32]ThunkFunc
	icept        []uint64 // bitmap over guest addresses: fast pre-check before the Intercepts map lookup
	InterceptTag map[uint32]string

	// Execution telemetry & debugging
	Steps         uint64
	MaxSteps      uint64
	Halted        bool
	HaltReason    string
	TraceCalls    bool
	CallStack     []CallFrame
	UnmappedCalls map[uint32]uint64
	VisitedFuncs  map[uint32]uint64

	// Guest sampling profiler (enabled when ProfEvery > 0; needs TraceCalls for function attribution).
	// Every ProfEvery steps past ProfFrom, the innermost call frame is charged in ProfSelf and every
	// distinct frame on the call stack in ProfIncl.
	ProfEvery   uint64
	ProfFrom    uint64
	ProfSamples uint64
	ProfSelf    map[uint32]uint64
	ProfIncl    map[uint32]uint64

	// Green-thread scheduling support. NestDepth counts host-initiated nested guest calls
	// (CallGuest/CallGuestNested); thread switches are only legal at depth 0.
	NestDepth       int
	Preempt         func(c *CPU)
	PreemptInterval uint64
	nextPreempt     uint64

	// Debug: optional read watchpoint over [WatchLo, WatchHi).
	WatchLo, WatchHi uint32
	WatchFn          func(c *CPU, addr uint32, size int, write bool)
}

func (c *CPU) SetPreemptInterval(interval uint64) {
	c.PreemptInterval = interval
	c.nextPreempt = c.Steps + interval
}

func NewCPU() *CPU {
	c := &CPU{
		Mem:           make([]byte, ArenaSize),
		Flags:         0x00000002,
		FPUCW:         0x037F,
		MXCSR:         0x1F80,
		HeapPtr:       HeapStartAddr,
		AllocSize:     make(map[uint32]uint32),
		FreeBlocks:    make(map[uint32][]uint32),
		Intercepts:    make(map[uint32]ThunkFunc),
		InterceptTag:  make(map[uint32]string),
		UnmappedCalls: make(map[uint32]uint64),
		VisitedFuncs:  make(map[uint32]uint64),
		icept:         make([]uint64, ArenaSize/64),
		FSBase:        0x03004000,
		GSBase:        0x03008000,
	}
	c.Regs[RegESP] = StackTopAddr
	c.Regs[RegEBP] = StackTopAddr
	return c
}

func (c *CPU) RegisterIntercept(addr uint32, name string, fn ThunkFunc) {
	c.Intercepts[addr] = fn
	if c.icept == nil {
		c.icept = make([]uint64, ArenaSize/64)
	}
	if addr < ArenaSize {
		c.icept[addr>>6] |= 1 << (addr & 63)
	}
	c.InterceptTag[addr] = name
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

func (c *CPU) Read8(addr uint32) uint8 {
	if !c.checkAddr(addr, 1) {
		return 0
	}
	if c.WatchFn != nil && addr >= c.WatchLo && addr < c.WatchHi {
		c.WatchFn(c, addr, 1, false)
	}
	return c.Mem[addr]
}

func (c *CPU) Read16(addr uint32) uint16 {
	if !c.checkAddr(addr, 2) {
		return 0
	}
	return binary.LittleEndian.Uint16(c.Mem[addr : addr+2])
}

func (c *CPU) Read32(addr uint32) uint32 {
	if !c.checkAddr(addr, 4) {
		return 0
	}
	if c.WatchFn != nil && addr+4 > c.WatchLo && addr < c.WatchHi {
		c.WatchFn(c, addr, 4, false)
	}
	return binary.LittleEndian.Uint32(c.Mem[addr : addr+4])
}

func (c *CPU) Read64(addr uint32) uint64 {
	if !c.checkAddr(addr, 8) {
		return 0
	}
	return binary.LittleEndian.Uint64(c.Mem[addr : addr+8])
}

func (c *CPU) ReadF32(addr uint32) float32 {
	return math.Float32frombits(c.Read32(addr))
}

func (c *CPU) ReadF64(addr uint32) float64 {
	return math.Float64frombits(c.Read64(addr))
}

func (c *CPU) Write8(addr uint32, val uint8) {
	if !c.checkAddr(addr, 1) {
		return
	}
	if c.WatchFn != nil && addr >= c.WatchLo && addr < c.WatchHi {
		c.WatchFn(c, addr, 1, true)
	}
	c.Mem[addr] = val
}

func (c *CPU) Write16(addr uint32, val uint16) {
	if !c.checkAddr(addr, 2) {
		return
	}
	binary.LittleEndian.PutUint16(c.Mem[addr:addr+2], val)
}

func (c *CPU) Write32(addr uint32, val uint32) {
	if !c.checkAddr(addr, 4) {
		return
	}
	binary.LittleEndian.PutUint32(c.Mem[addr:addr+4], val)
}

func (c *CPU) Write64(addr uint32, val uint64) {
	if !c.checkAddr(addr, 8) {
		return
	}
	binary.LittleEndian.PutUint64(c.Mem[addr:addr+8], val)
}

func (c *CPU) WriteF32(addr uint32, val float32) {
	c.Write32(addr, math.Float32bits(val))
}

func (c *CPU) WriteF64(addr uint32, val float64) {
	c.Write64(addr, math.Float64bits(val))
}

func (c *CPU) ReadCString(addr uint32) string {
	if addr == 0 || int(addr) >= len(c.Mem) {
		return ""
	}
	end := int(addr)
	for end < len(c.Mem) && c.Mem[end] != 0 {
		end++
	}
	return string(c.Mem[addr:end])
}

func (c *CPU) WriteCString(addr uint32, s string) {
	copy(c.Mem[addr:], s)
	c.Mem[int(addr)+len(s)] = 0
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
	for i, f := range c.CallStack {
		dup := false
		for _, g := range c.CallStack[i+1:] {
			if g.TargetEIP == f.TargetEIP {
				dup = true
				break
			}
		}
		if !dup {
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

// Guest Heap Allocator
func (c *CPU) Malloc(size uint32) uint32 {
	if size == 0 {
		size = 16
	}
	aligned := (size + 15) & ^uint32(15)
	if list := c.FreeBlocks[aligned]; len(list) > 0 {
		ptr := list[len(list)-1]
		c.FreeBlocks[aligned] = list[:len(list)-1]
		c.AllocSize[ptr] = aligned
		return ptr
	}
	if c.HeapPtr < ReservedHi && c.HeapPtr+aligned > ReservedLo {
		c.HeapPtr = ReservedHi // skip the host-reserved region
	}
	if c.HeapPtr+aligned > HeapEndAddr {
		panic(fmt.Sprintf("guest heap exhausted (HeapPtr=0x%08x, req=%d)", c.HeapPtr, aligned))
	}
	ptr := c.HeapPtr
	c.HeapPtr += aligned
	c.AllocSize[ptr] = aligned
	return ptr
}

func (c *CPU) Calloc(num, size uint32) uint32 {
	total := num * size
	ptr := c.Malloc(total)
	clear(c.Mem[ptr : ptr+total])
	return ptr
}

func (c *CPU) Free(ptr uint32) {
	if ptr == 0 {
		return
	}
	if sz, ok := c.AllocSize[ptr]; ok {
		delete(c.AllocSize, ptr)
		if sz <= 65536 {
			c.FreeBlocks[sz] = append(c.FreeBlocks[sz], ptr)
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
	oldSize := c.AllocSize[ptr]
	if newSize <= oldSize {
		return ptr
	}
	newPtr := c.Malloc(newSize)
	if oldSize > 0 {
		copy(c.Mem[newPtr:newPtr+oldSize], c.Mem[ptr:ptr+oldSize])
	}
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

func (c *CPU) UpdateFlagsLogic32(res uint32) {
	c.SetFlag(FlagCF|FlagOF|FlagAF, false)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x80000000) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
}

func (c *CPU) UpdateFlagsLogic16(res uint16) {
	c.SetFlag(FlagCF|FlagOF|FlagAF, false)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x8000) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
}

func (c *CPU) UpdateFlagsLogic8(res uint8) {
	c.SetFlag(FlagCF|FlagOF|FlagAF, false)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x80) != 0)
	c.SetFlag(FlagPF, parity8(res))
}

func (c *CPU) UpdateFlagsAdd32(a, b, res uint32) {
	c.SetFlag(FlagCF, res < a)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x80000000) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
	c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
	c.SetFlag(FlagOF, ((^(a^b)&(a^res))&0x80000000) != 0)
}

func (c *CPU) UpdateFlagsSub32(a, b, res uint32) {
	c.SetFlag(FlagCF, a < b)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x80000000) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
	c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
	c.SetFlag(FlagOF, (((a^b)&(a^res))&0x80000000) != 0)
}

func (c *CPU) UpdateFlagsAdd16(a, b, res uint16) {
	c.SetFlag(FlagCF, res < a)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x8000) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
	c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
	c.SetFlag(FlagOF, ((^(a^b)&(a^res))&0x8000) != 0)
}

func (c *CPU) UpdateFlagsSub16(a, b, res uint16) {
	c.SetFlag(FlagCF, a < b)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x8000) != 0)
	c.SetFlag(FlagPF, parity8(uint8(res)))
	c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
	c.SetFlag(FlagOF, (((a^b)&(a^res))&0x8000) != 0)
}

func (c *CPU) UpdateFlagsAdd8(a, b, res uint8) {
	c.SetFlag(FlagCF, res < a)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x80) != 0)
	c.SetFlag(FlagPF, parity8(res))
	c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
	c.SetFlag(FlagOF, ((^(a^b)&(a^res))&0x80) != 0)
}

func (c *CPU) UpdateFlagsSub8(a, b, res uint8) {
	c.SetFlag(FlagCF, a < b)
	c.SetFlag(FlagZF, res == 0)
	c.SetFlag(FlagSF, (res&0x80) != 0)
	c.SetFlag(FlagPF, parity8(res))
	c.SetFlag(FlagAF, ((a^b^res)&0x10) != 0)
	c.SetFlag(FlagOF, (((a^b)&(a^res))&0x80) != 0)
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

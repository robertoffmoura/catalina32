package cpu

import (
	"fmt"
	"os"
	"sort"
	"unsafe"
)

// JIT: hot blocks of the decoded-instruction cache translated to host machine code at run time
// (docs/JIT.md). runCached counts how often each block runs; at jitThreshold it is translated
// (jitTranslate, per host architecture) and from then on runs as native code.
//
// Translated code does exactly what execBlock does, so cached, translated and interpreted
// execution can't be told apart:
//   - Steps advance by the same counts (a block's total, or the instructions before an exit),
//     so headless runs stay deterministic.
//   - Flags are written back to c.Flags as the interpreter computes them, including where the
//     hardware leaves them undefined.
//   - Any instruction that needs more than the fast paths (an out-of-range access, a store into
//     a page holding code, a form not translated) leaves translated code before it changes
//     anything. The interpreter runs that one instruction, then translated code resumes at the
//     next one (or, if the instruction flushed the code cache, the interpreter finishes the
//     block, as execBlock would with its stale copy).
//
// Translated code never calls Go and never faults; Go regains control at every exit. Flushing
// the code cache (FlushCodeCache) discards all translated code.

// jitThreshold is how many runs of a block it takes to translate it (a variable for tests).
var jitThreshold uint32 = 32

const (
	jitSlice     = 4_000_000 // most steps per entry into translated code (chained blocks): Go gets control back that often
	jitTableBits = 18        // the table translated code looks indirect targets up in (hashed)
	jitTableMask = 1<<jitTableBits - 1
)

// Exit reasons, set by translated code in jitState.exit.
const (
	jitExitEnd   = iota // a block (or chain of blocks) ended: EIP and Steps are set
	jitExitSlow         // instruction exitIdx of block exitBlk needs the interpreter (Go sets EIP and Steps)
	jitExitLimit        // the next block would cross the step limit: EIP is its start
)

// jitDisabled switches translation off (C32_NO_JIT=1): for A/B comparisons, and a way out. Where
// the JIT is opt-in (jitOptIn: hosts it hasn't been verified on), C32_JIT=1 switches it on.
var jitDisabled = os.Getenv("C32_NO_JIT") == "1" || jitOptIn && os.Getenv("C32_JIT") != "1"

// jitState is what translated code reads and writes besides the CPU's registers.
type jitState struct {
	mem     uintptr // &Mem[0]
	stores  uintptr // &jitCode.stores[0]
	table   uintptr // &jitCode.table[0]
	limit   uint64  // Steps may not pass it (set by Go before entering translated code)
	budget  uint64  // limit minus the steps charged: each block charges its steps at its head
	exit    uint32
	exitIdx uint32
	exitBlk uint32
	_       uint32
	raw     uint64     // host flags saved by translated code, not yet merged into c.Flags (x86-64 backend)
	pmask   uint32     // which of raw are pending: value | clear<<16 (0: none); Go merges on every exit
	save    [10]uint64 // host registers kept across translated code (the backend's prologue)
}

// Offsets in the CPU of what translated code uses.
var (
	offRegs   = int32(unsafe.Offsetof(CPU{}.Regs))
	offEIP    = int32(unsafe.Offsetof(CPU{}.EIP))
	offFlags  = int32(unsafe.Offsetof(CPU{}.Flags))
	offSteps  = int32(unsafe.Offsetof(CPU{}.Steps))
	offXMM    = int32(unsafe.Offsetof(CPU{}.XMM))
	offFPUSt  = int32(unsafe.Offsetof(CPU{}.FPUSt))
	offFPUTop = int32(unsafe.Offsetof(CPU{}.FPUTop))
	offJit    = int32(unsafe.Offsetof(CPU{}.jit))
	offMem    = offJit + int32(unsafe.Offsetof(jitState{}.mem))
	offStore  = offJit + int32(unsafe.Offsetof(jitState{}.stores))
	offTable  = offJit + int32(unsafe.Offsetof(jitState{}.table))
	offBudget = offJit + int32(unsafe.Offsetof(jitState{}.budget))
	offExit   = offJit + int32(unsafe.Offsetof(jitState{}.exit))
	offEIdx   = offJit + int32(unsafe.Offsetof(jitState{}.exitIdx))
	offEBlk   = offJit + int32(unsafe.Offsetof(jitState{}.exitBlk))
	offSave   = offJit + int32(unsafe.Offsetof(jitState{}.save))
	offRaw    = offJit + int32(unsafe.Offsetof(jitState{}.raw))
	offPMask  = offJit + int32(unsafe.Offsetof(jitState{}.pmask))
)

// jitTableEntry maps a guest address to translated code (for indirect branches and returns).
type jitTableEntry struct {
	eip  uint32
	_    uint32
	code uintptr
}

// jitBlock is one translated block.
type jitBlock struct {
	id     uint32
	eip    uint32
	insts  []dInst   // the decoded instructions it was translated from (the interpreter's view)
	prefix []uint64  // steps before instruction i (a fused compare and branch counts two)
	starts []uint32  // guest address of instruction i
	resume []uintptr // where translated code for instruction i starts (0: never resumed there)
	head   uintptr   // entry with the step-limit check (chaining)
	steps  uint64    // all its steps
	code   []byte    // a self-checking block's instruction bytes (dBlock.code): its head compares them with memory

	// Diagnostics (C32_JITPROF=1): how often the block was entered at its head, and what its
	// code does per run.
	runs *uint64
	cost JITCost
}

// JITCost counts what a translated block's code does in one run.
type JITCost struct {
	Insts, Bytes                    uint64
	FlagSaves, FlagLoads, Merges    uint64 // PUSHFQ, POPFQ (flags into the host), merges into c.Flags
	DynMerges, Loads, Stores, Exits uint64 // merges of another block's flags; checked accesses; possible slow exits
}

// jitProf turns on per-block run counts (C32_JITPROF=1).
var jitProf = os.Getenv("C32_JITPROF") == "1"

// JITProfile returns, weighted by how often each block ran, the total of what translated code
// did (C32_JITPROF=1), and the most expensive blocks.
func (c *CPU) JITProfile() (total JITCost, runs uint64, top []string) {
	if c.jitc == nil {
		return
	}
	type bc struct {
		jb *jitBlock
		w  uint64
	}
	var all []bc
	for _, jb := range c.jitc.blocks {
		if jb.runs == nil {
			continue
		}
		n := *jb.runs
		runs += n
		k := jb.cost
		total.Insts += n * k.Insts
		total.Bytes += n * k.Bytes
		total.FlagSaves += n * k.FlagSaves
		total.FlagLoads += n * k.FlagLoads
		total.Merges += n * k.Merges
		total.DynMerges += n * k.DynMerges
		total.Loads += n * k.Loads
		total.Stores += n * k.Stores
		total.Exits += n * k.Exits
		all = append(all, bc{jb, n * k.Bytes})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].w > all[j].w })
	for i := 0; i < len(all) && i < 10; i++ {
		jb := all[i].jb
		top = append(top, fmt.Sprintf("%#x: %d runs x %d insts, %+v", jb.eip, *jb.runs, len(jb.insts), jb.cost))
	}
	return
}

// jitCode is one CPU's translated code.
type jitCode struct {
	buf      []byte // executable: shared code, then hot code (blocks' bodies), then cold code (their exits)
	used     int    // hot code ends here
	cold     int    // cold code ends here
	coldAt   int    // where cold code starts
	fixed    int    // bytes of shared code at the start (prologue, epilogue), kept across resets
	prologue uintptr
	epilogue uintptr
	blocks   []*jitBlock
	byEIP    map[uint32]*jitBlock
	pending  map[uint32][]uintptr // jumps waiting for a block at that address (rel32 sites)
	stores   []byte               // per 4 KB page: nonzero = stores there must take the interpreter
	table    []jitTableEntry
	failed   map[uint32]bool // blocks that start with an instruction translated code can't run
	stale    bool            // stores must be rebuilt (code range or text pages changed)

	Stats JITStats
}

// JITStats counts what the JIT did (diagnostics, tests).
type JITStats struct {
	Translated uint64            // blocks translated
	Bytes      uint64            // host code written
	Entries    uint64            // entries into translated code from Go
	SlowExits  uint64            // instructions handed to the interpreter
	EndExits   uint64            // returns to Go at a block end (target not translated, or a lookup miss)
	LimitExits uint64            // returns to Go at the step limit
	Resets     uint64            // times all translations were discarded
	EndAt      map[uint32]uint64 // where EndExits went (when non-nil: diagnostics)
}

// JITStats returns the JIT's counters (zero if it never ran).
func (c *CPU) JITStats() JITStats {
	if c.jitc == nil {
		return JITStats{}
	}
	return c.jitc.Stats
}

// jitUsable says whether translated code may run now: not where the interpreter's per-
// instruction hooks are wanted (call tracing, profiling, watchpoints).
func (c *CPU) jitUsable() bool {
	return jitSupported && !jitDisabled && !c.NoJIT && !c.TraceCalls && c.watchFn == nil && c.ProfEvery == 0
}

// jitReset discards all translated code (the code cache was flushed or the buffer is full).
func (c *CPU) jitReset() {
	j := c.jitc
	if j == nil {
		return
	}
	c.jitGen++ // blocks in c.blocks drop their translations
	if len(j.blocks) > 0 {
		j.Stats.Resets++
	}
	j.used, j.cold = j.fixed, j.coldAt
	j.blocks = j.blocks[:0]
	clear(j.byEIP)
	clear(j.pending)
	clear(j.failed)
	for i := range j.table {
		j.table[i] = jitTableEntry{eip: dNoTag}
	}
}

// jitInit sets up the code buffer and shared code on first use; false if the host can't run
// translated code (no executable memory).
func (c *CPU) jitInit() bool {
	if c.jitc != nil {
		return c.jitc.buf != nil
	}
	j := &jitCode{byEIP: map[uint32]*jitBlock{}, pending: map[uint32][]uintptr{}, failed: map[uint32]bool{}, stale: true}
	c.jitc = j
	buf, err := mapExecutable(jitCodeSize)
	if err != nil {
		c.ReportOnce("jit-mmap", "[jit] no executable memory (%v): running interpreted", err)
		return false
	}
	j.buf = buf
	j.table = make([]jitTableEntry, 1<<jitTableBits)
	for i := range j.table {
		j.table[i] = jitTableEntry{eip: dNoTag}
	}
	j.stores = make([]byte, 1<<20) // every page of the 32-bit address space
	j.used = jitShared(j)
	j.fixed = j.used
	j.coldAt = len(j.buf) * jitHotEighths / 8
	j.cold = j.coldAt
	c.jit.mem = uintptr(unsafe.Pointer(&c.Mem[0]))
	c.jit.stores = uintptr(unsafe.Pointer(&j.stores[0]))
	c.jit.table = uintptr(unsafe.Pointer(&j.table[0]))
	return true
}

// jitStores rebuilds the per-page store table: a store may be done by translated code unless it
// touches a page holding code in [CodeLo, CodeHi) (the interpreter then flushes the code cache)
// or isn't inside guest memory. A store starting on the page before such a page may reach into
// it, so that page is marked too and only the first page of a store needs checking (stores are
// at most 16 bytes).
func (c *CPU) jitStores() {
	j := c.jitc
	j.stale = false
	clear(j.stores)
	for p := uint32(ArenaSize>>12) - 1; p < uint32(len(j.stores)); p++ {
		j.stores[p] = 1
	}
	if c.CodeHi <= c.CodeLo {
		return
	}
	for _, r := range c.jitSlowPages {
		for p := r[0] >> 12; p < r[1]>>12; p++ {
			j.stores[p] = 1
		}
	}
	for p := c.CodeLo >> 12; p <= (c.CodeHi-1)>>12 && p < uint32(len(j.stores)); p++ {
		if c.textPages == nil || c.textPages[p>>6]&(1<<(p&63)) != 0 {
			j.stores[p] = 1
			if p > 0 {
				j.stores[p-1] = 1
			}
		}
	}
}

// jitCompile translates block b (hot), or returns nil.
func (c *CPU) jitCompile(b *dBlock) *jitBlock {
	if !c.jitInit() {
		return nil
	}
	j := c.jitc
	if j.stale {
		c.jitStores()
	}
	if j.failed[b.tag] {
		return nil
	}
	if j.coldAt-j.used < 1<<16 || j.coldAt < len(j.buf) && len(j.buf)-j.cold < 1<<16 {
		c.jitReset() // full: start again
	}
	jb := &jitBlock{id: uint32(len(j.blocks)), eip: b.tag, insts: append([]dInst(nil), b.insts...), steps: uint64(b.steps),
		code: append([]byte(nil), b.code...)}
	jb.prefix = make([]uint64, len(jb.insts)+1)
	jb.starts = make([]uint32, len(jb.insts)+1)
	jb.starts[0] = jb.eip
	for i, d := range jb.insts {
		n, l := uint64(1), uint32(d.len)
		if isFused(d.kind) {
			n, l = 2, l+uint32(d.len2)
		}
		jb.prefix[i+1] = jb.prefix[i] + n
		jb.starts[i+1] = jb.starts[i] + l
	}
	if !jitTranslate(c, j, jb) {
		j.failed[b.tag] = true
		return nil
	}
	j.blocks = append(j.blocks, jb)
	j.byEIP[jb.eip] = jb
	j.Stats.Translated++
	e := &j.table[jitHash(jb.eip)]
	e.eip, e.code = jb.eip, jb.head
	for _, site := range j.pending[jb.eip] {
		jitPatch(j, site, jb.head)
	}
	delete(j.pending, jb.eip)
	return jb
}

// jitCodeChunks splits a self-checking block's n code bytes into the loads its head compares:
// (offset, size) with sizes 8, 4 or 1, the last ones overlapping rather than reading past the end.
func jitCodeChunks(n int) (chunks [][2]int) {
	switch {
	case n >= 8:
		for off := 0; off+8 <= n; off += 8 {
			chunks = append(chunks, [2]int{off, 8})
		}
		if n%8 != 0 {
			chunks = append(chunks, [2]int{n - 8, 8})
		}
	case n >= 4:
		chunks = append(chunks, [2]int{0, 4}, [2]int{n - 4, 4})
	default:
		for off := 0; off < n; off++ {
			chunks = append(chunks, [2]int{off, 1})
		}
	}
	return chunks
}

func isFused(k uint8) bool {
	switch k {
	case dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dCmpEAXJcc, dTestRMRJcc, dTestRMIJcc, dCmpRMI8Jcc:
		return true
	}
	return false
}

// jitRun runs translated block jb (whose steps fit before limit) and whatever it chains to, until
// translated code exits; slow instructions run in the interpreter, after which translated code
// resumes. Calls to host functions (intercepts) are made here too, and translated code entered
// again after them, as long as RunUntil wouldn't have done anything else in between (limit,
// stopEIP, a halt).
func (c *CPU) jitRun(jb *jitBlock, stopEIP uint32, limit uint64) {
	j := c.jitc
	entry, idx := jb.head, 0
	for {
		if j.stale {
			c.jitStores()
		}
		c.jit.limit = min(limit, c.Steps+jitSlice)
		// Translated code charges each block's steps at its head; entering part way, the rest of
		// this block is charged here.
		c.jit.budget = c.jit.limit - c.Steps
		if idx > 0 {
			c.jit.budget -= jb.steps - jb.prefix[idx]
		}
		jitCall(j.prologue, c, entry)
		j.Stats.Entries++
		c.Steps = c.jit.limit - c.jit.budget
		if m := c.jit.pmask; m != 0 { // flags translated code left pending
			c.Flags = c.Flags&^(m>>16) | uint32(c.jit.raw)&m&0xFFFF
			c.jit.pmask = 0
		}
		switch c.jit.exit {
		case jitExitEnd:
			j.Stats.EndExits++
			if j.Stats.EndAt != nil {
				j.Stats.EndAt[c.EIP]++
			}
			if jb = c.jitNext(stopEIP, &limit); jb == nil {
				return
			}
			entry, idx = jb.head, 0
		case jitExitLimit:
			j.Stats.LimitExits++
			return
		case jitExitSlow:
			j.Stats.SlowExits++
			jb = j.blocks[c.jit.exitBlk]
			i := int(c.jit.exitIdx)
			c.EIP = jb.starts[i]
			c.Steps -= jb.steps - jb.prefix[i] // charged, not run
			flushes := c.CodeCacheFlushes
			c.execBlock(jb.insts[i : i+1])
			if c.Halted {
				return
			}
			if i+1 == len(jb.insts) {
				if jb = c.jitNext(stopEIP, &limit); jb == nil {
					return
				}
				entry, idx = jb.head, 0
				continue
			}
			if c.CodeCacheFlushes != flushes { // the instruction changed code: finish as execBlock would
				c.execBlock(jb.insts[i+1:])
				return
			}
			idx = i + 1
			entry = jb.resume[idx]
		}
	}
}

// jitNext runs host functions at EIP (as RunUntil's Step would) until EIP is a translated block
// that fits before the limit, which it returns; nil when RunUntil or runCached must take over.
func (c *CPU) jitNext(stopEIP uint32, limit *uint64) *jitBlock {
	for {
		eip := c.EIP
		if c.Halted || eip == stopEIP || c.Steps >= *limit {
			return nil
		}
		if fn := c.interceptAt(eip); fn != nil {
			c.Steps++
			c.HostCalls++
			fn(c)
			*limit = c.nextEventStep() // what RunUntil would use next
			continue
		}
		b := &c.blocks[eip&blockMask]
		if b.tag != eip || b.jit == nil || b.jitGen != c.jitGen || uint64(b.steps) > *limit-c.Steps || b.code != nil && !c.codeSame(b) {
			return nil
		}
		if e := &c.jitc.table[jitHash(eip)]; e.eip != eip { // another block took its place: take it back
			e.eip, e.code = eip, b.jit.head
		}
		return b.jit
	}
}

// JITTargetKind says what runs at eip (diagnostics for JITStats.EndAt): "host" (an intercept),
// "translated", "cached" (a block not translated), "uncached" (the full decoder's), or "none"
// (no block there now).
func (c *CPU) JITTargetKind(eip uint32) string {
	switch {
	case eip < ArenaSize && c.icept != nil && c.icept[eip>>6]&(1<<(eip&63)) != 0:
		return "host"
	case c.blocks == nil || c.blocks[eip&blockMask].tag != eip:
		return "none"
	}
	b := &c.blocks[eip&blockMask]
	switch {
	case len(b.insts) == 0:
		return "uncached"
	case b.jit != nil && b.jitGen == c.jitGen:
		return "translated"
	}
	return "cached"
}

// Run0JITStats starts counting where translated code returns to Go (diagnostics).
func (c *CPU) Run0JITStats() {
	if c.jitInit() {
		c.jitc.Stats.EndAt = map[uint32]uint64{}
	}
}

// jitHash is the lookup table's index for guest address eip (Fibonacci hashing; translated code
// computes the same).
func jitHash(eip uint32) uint32 { return eip * jitHashMul >> (32 - jitTableBits) }

const jitHashMul = 0x9E3779B1

// JITCode returns the host code translated for the block at eip and the guest instructions
// it came from (diagnostics; nil if there is none).
func (c *CPU) JITCode(eip uint32) ([]byte, []byte) {
	if c.jitc == nil {
		return nil, nil
	}
	jb := c.jitc.byEIP[eip]
	if jb == nil {
		return nil, nil
	}
	base := uintptr(unsafe.Pointer(&c.jitc.buf[0]))
	end := len(c.jitc.buf)
	for _, o := range c.jitc.blocks { // the next block starts where this one's code ends
		if o.head > jb.head && int(o.head-base) < end {
			end = int(o.head - base)
		}
	}
	end = min(end, c.jitc.used)
	return c.jitc.buf[jb.head-base : end], c.Mem[jb.eip:jb.starts[len(jb.insts)]]
}

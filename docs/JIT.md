# JIT: design for a future session

Status: **not started**. This is the design agreed with the user, written so a future session
can implement it. Read `CONTEXT.md` (the CPU tiers) and `docs/ARCHITECTURE.md` (what "generic"
means here) first.

## Why

Players run Catalina32.app, which never contains the AOT code (it is a translation of the
game's code, compiled on the developer's machine, and can't be distributed). So in the product
every game instruction runs on the interpreter and its decoded-block cache. Measured on LEGO Star
Wars: in heavy scenes the interpreter takes most of the host time even though AOT functions run
83% of the instructions; without AOT it is the whole cost. The user notices it as choppiness in
release builds. Interpreter tweaks have run out (the dispatch loop is near what plain Go
allows; precomputed addresses and batched step counts measured as noise).

A JIT translates hot guest code into host machine code at run time, on the player's machine,
from their own copy. It is fully generic: it works on x86 instructions, needs no profiles,
signatures or per-game data, and distributes nothing derived from any game.

## Where it fits (read these first)

- `pkg/cpu/exec.go`: `RunUntil` (the run loop), `nextEventStep` (when step limit, preemption or
  the profiler need a check), `runCached` (runs cached blocks until then), `Step` (one
  instruction, including intercepts), `stepFull` (the full decoder).
- `pkg/cpu/dcache.go`: the decoded-instruction cache (`dInst`, about 60 `d*` kinds including
  fused compare+branch), blocks (`dBlock`, up to `blockMax` instructions, ending at control
  transfers), `buildBlock`, `execBlock`, `FlushCodeCache`, `noteCodeWrite` (writes into the code
  range flush the cache: self-modifying code and patches).
- `pkg/cpu/cpu.go`: guest memory (`Mem`, `ArenaSize`), the inlined access fast paths
  (`readLimit`, `writeSpan`: in bounds, above code for stores, not watched), flags
  (`UpdateFlags*`, `EvalCond`), x87 helpers (`FPUPush` ...).
- Intercepts: `RegisterIntercept`, the `icept` bitmap; a block never contains an intercepted
  address. Thunks (Go functions) run at block boundaries only.
- `cmd/c32aot` and `pkg/games/*/aot_gen.go`: the AOT translator. Its handling of instruction
  forms, flags and fallbacks (`StepRaw` for untranslated instructions) is the closest existing
  model for a translator, and its tests (random snippets, interpreter vs compiled) are the
  model for the JIT's.
- Determinism: headless runs are exactly reproducible; the clock is derived from `Steps`
  (instruction count) and green-thread preemption happens at step counts (`Preempt`,
  `nextPreempt`). The JIT must count exactly as the interpreter does.

## Design

### Translation unit and lifecycle

1. **Hot blocks.** Count executions per block in `runCached` (or per block start address); at a
   threshold (start around 50–200) translate the block. Cold code stays interpreted.
2. **Block = the same block `buildBlock` makes** (same boundaries: control transfers,
   intercepts, uncached forms, `blockMax`). Same step count (`b.steps`), so determinism holds
   trivially. Later: superblocks / traces across unconditional jumps.
3. **Entry check.** `runCached` already refuses a block that would cross the next event
   (`b.steps > limit-c.Steps`); keep that: a translated block runs whole or not at all.
4. **Exit.** Translated code returns to the dispatcher with the next EIP in the CPU struct and
   an exit reason (normal, needs-slow-memory-path, unsupported-instruction-at, halted).
5. **Chaining (phase 2).** A block exit to a known, translated, non-intercepted target jumps
   straight to it (patched jump), with the step budget checked at block entry (a counter in a
   register, compared against `limit`).
6. **Invalidation.** `FlushCodeCache` (and `noteCodeWrite`) must drop translated blocks too
   (unlink chains, free or recycle the code buffer). Intercepts installed later
   (`RegisterIntercept`) also flush, as they do for the dcache.
7. **Fallback.** An instruction the translator doesn't handle ends the block before it; the
   interpreter executes it, exactly as `runCached` stops at `dSlow` today.

### Generated code conventions (x86-64 host)

- **Guest registers in host registers** for the duration of a block: 8 guest GPRs map to 8 host
  registers (e.g. r8–r15); loaded at entry, stored at exit (phase 2: keep them across chained
  blocks). One host register holds the guest memory base (`&Mem[0]`), one the CPU struct.
- **Memory access** = `base + guest address` with the same guarantees as the interpreter's fast
  paths: loads need `addr < readLimit`, stores `addr-CodeHi < writeSpan` (above code, in
  bounds, not watched); anything else exits to the slow path (the interpreter re-executes that
  instruction: so translate per instruction with an exit point before each memory access that
  can fail, or make the slow path a helper exit that resumes after it). Generated code must
  never fault.
- **Flags.** On an x86-64 host, x86-32 arithmetic sets the host flags identically: compute them
  natively and store EFLAGS at exit only when a later block can read them; within a block, skip
  flag materialisation for flags that are overwritten before being read (liveness over the
  block). Watch the quirks the interpreter already encodes: INC/DEC preserve CF, shifts by 0
  leave flags alone, partial-register writes (AH etc.).
- **No host stack use** in generated code (no calls, no pushes beyond a fixed scratch area in
  the CPU struct). Anything needing Go (x87, rare SSE, helpers) is an exit, not a call.
- **x87/SSE.** Start with fallback; then translate the scalar SSE forms the dcache already
  caches (`movss`, `addss`, `mulss`, `ucomiss`...) natively. x87 stays in the interpreter (its
  semantics there are float64-based; the JIT must match the interpreter, not real x87).

### Running generated code from Go

- **Executable memory:** `mmap` a code buffer.
  - Intel Macs and Linux: read-write-execute memory is allowed for an app without the hardened
    runtime (Catalina32 is signed ad hoc, not notarised, so it has none).
  - Apple Silicon: memory is never writable and executable at once. Use `MAP_JIT` and toggle
    with `pthread_jit_write_protect_np` per thread, then `sys_icache_invalidate`. Works without
    entitlements while the app isn't notarised (with the hardened runtime it needs
    `com.apple.security.cs.allow-jit`).
- **Calling it:** a small Go assembly trampoline (`.s` file) that loads the pinned registers
  and jumps in; generated code returns to it. Because generated code uses no stack and never
  calls back, it can run on the goroutine's stack. Async preemption signals (SIGURG) landing in
  generated code are harmless (Go sees a non-Go PC and skips preemption), as is SIGPROF.
- **Alternative** (the backlog's long-standing idea): the CPU core in Rust or C behind the same
  interface, via cgo. Makes stack/signal questions simpler, cross-compilation harder (the app
  is built per architecture on a Mac with clang anyway). Prefer pure Go + assembly first.
- **Switch:** `C32_NO_JIT=1` disables it (A/B tests, and a way out for players).

### Backends

1. **x86-64 first.** It covers Intel Macs (the user's), and the Linux dev container can run
   and test it (unlike Metal). Guest x86-32 → host x86-64 is close to 1:1.
2. **arm64 later.** Same front end; flags must be emulated (lazy flags: record op and operands,
   materialise on read), W^X toggling, memory ordering is not a concern (single guest core).

## Testing and measuring

- **Differential tests:** random instruction sequences run by the interpreter and by the JIT
  from the same state, comparing registers, flags, memory and EIP (reuse `cmd/c32aot`'s
  generator and harness). Include faulting/edge cases (slow-path addresses, writes into code).
- **Whole-game determinism:** headless routes (`scripts/runroute.sh`, `scripts/gpuroute.sh`)
  must give identical `Steps=` lines and identical gpucheck output with and without the JIT.
- **Speed:** paired side-by-side headless runs to the same instruction count (the method in
  `docs/DEBUGGING.md`), interpreter vs JIT, GPU path, **without** `aot_gen.go`. Keep runs
  short (the user asked for quick iterations: under a few minutes).
- **On the Mac:** the user plays with a `build-app.sh` build (no AOT) and compares.

## Phases

1. **Infrastructure:** code buffer, trampoline, dispatcher hook in `runCached`, invalidation,
   `C32_NO_JIT`. Translate a handful of forms (mov, alu reg/imm, lea, jcc/jmp, call/ret, push/
   pop). Differential tests green. Measure.
2. **Coverage:** every `dInst` kind the dcache caches, fused compare+branch, scalar SSE.
3. **Speed:** chaining, registers kept across blocks, flag liveness, hot-path inline memory
   access.
4. **arm64 backend.**

Acceptance at each phase: differential tests and determinism checks pass; measurable speedup
over the interpreter in the A/B; no change in what the game does.

## Pitfalls already known

- Green threads switch on step counts (`Preempt`): a block must not run past `nextPreempt`.
- Code may be patched at run time (`pkg/games/lswsaga` patches `NuTexRead`; games may
  self-modify): invalidation must be exact.
- Intercepts can be registered after code was translated (lazy `Bind`): flush.
- `TraceCalls` / the guest profiler rely on per-call hooks in `execBlock` (`dCall`): disable the
  JIT while they are on, or emit the hook.
- AOT and JIT together (developer builds): AOT functions are intercepts, so blocks stop at them;
  nothing special needed, but measure the JIT without AOT.

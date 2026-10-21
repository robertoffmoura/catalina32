# The JIT

Status (October 2026, branch `jit`): **phases 1–4 done.**

- **x86-64 backend:** Intel Macs and Linux. On by default.
- **arm64 backend:** tested under QEMU user-mode on Linux only. On Apple Silicon it is
  **off unless `C32_JIT=1`** until someone verifies it on Apple hardware (the platform glue
  there, `MAP_JIT` and write protection, can't be tested here).

Read `CONTEXT.md` (the CPU tiers) and `docs/ARCHITECTURE.md` (what "generic" means here) first.

## Why

Players run Catalina32.app, which never contains AOT code. AOT code is a translation of the
game's code, compiled on the developer's machine, and can't be distributed. So in the product
every game instruction ran on the interpreter and its decoded-block cache: choppy heavy scenes
in LEGO Star Wars, a few frames per second in Portal.

A JIT translates hot guest code into host machine code at run time, on the player's machine,
from their own copy. It is fully generic: it works on x86 instructions, needs no profiles,
signatures or per-game data, and distributes nothing derived from any game.

## Results

Measured on the Linux dev container, x86-64 backend, against the interpreter with
`C32_NO_JIT=1`.

| Run | Interpreter | JIT | Same result |
|---|---|---|---|
| LEGO, `gpuroute.sh` `episode1`, 1.1 billion instructions, GPU path, no AOT | 17.0 s | 6.5–7.8 s | identical `Steps=` lines and final state |
| Portal `TestBoot`, 12 billion instructions | 182 s | ~50 s | byte-identical OpenGL stream (`C32_GLTRACE`) |
| Portal gameplay | ~75M instructions/s | ~280M instructions/s | |

**Portal, looking into a room against facing a wall** (`testchmb_a_15`, the same spot,
`TestBoot` with `C32_FRAMESTATS`; host milliseconds per frame on the container):

| | Room view | Wall |
|---|---|---|
| Before self-checking blocks | 177 ms | 140 ms |
| With them and a bigger code buffer | 146 ms | 100 ms |
| Guest instructions per frame | 19.8 M | 16.5 M |
| Of which the full decoder runs | 1.57 M, now 0.75 M | 1.61 M, now 0.75 M |
| GL commands per frame | 3,645 | 1,730 |

- The full decoder had run Miles's MP3 decoder (`mssmp3.asi`), which Miles loads into heap
  memory itself: outside the code range, so neither cached nor translated. Self-checking
  blocks (below) now cover it.
- **What is left:** its x87 and MMX instructions, which the cache doesn't decode (about 0.33 M
  per frame), and Go round trips for host calls (about 40,000 a frame).
- **What a larger view adds:** more translated game code, and more host calls and returns to
  Go.

**arm64:** QEMU emulates the host, so speed there says little about real hardware. The
arm64 backend's numbers are in "Testing".

## How it works

Generic part: `pkg/cpu/jit.go` and `jit_flags.go`. The two backends: `jit_amd64.go` (encoder
`x64asm.go`) and `jit_arm64.go` (encoder `a64asm.go`).

### Lifecycle

- **What is translated.** `runCached` counts runs of each decoded block (`dBlock.hits`). At
  `jitThreshold` (32) it translates the block, the same block `buildBlock` made: same
  boundaries, same step count. A block whose first instruction can't be translated is
  remembered (`failed`) and stays interpreted.
- **Code outside the code range** (code the game loaded or made in its own memory, like Miles's
  plug-ins) runs in **self-checking blocks**: stores there don't flush anything (they stay on
  the fast path), so such a block keeps its instruction bytes (`dBlock.code`) and compares
  them with memory before it runs, in Go and at the head of its translated code. If they
  changed, the block is rebuilt. A block that changes its own bytes while running keeps its
  old instructions to its end. `TestJITSelfCheckingCode` patches code in a loop.
- **Running.** `jitRun` enters translated code through `jitCall`, an assembly trampoline that
  calls the backend's prologue. The prologue loads the guest registers into host registers and
  jumps to the block.
- **The step limit.** Each block charges its steps at its head against `jitState.budget`. If
  the block wouldn't fit before the limit, it exits without running. The limit is the next
  event: preemption, the step limit, or at most `jitSlice` (4M) steps. So Steps advance
  exactly as in the interpreter, and green-thread switches happen at the same instruction.
- **Chaining.**
  - A direct jump or call to a translated block jumps straight to it.
  - A direct jump to a block not translated yet goes through a patchable jump. It is patched
    when the target is translated (`pending`).
  - Indirect jumps, calls and returns look the target up in a hashed table (2^18 entries;
    translated code computes the same Fibonacci hash).
- **Exits.**
  - `End`: the target isn't translated. `jitNext` runs host functions (intercepts) in place
    and enters translated code again if it can.
  - `Slow`: an instruction needs the interpreter. Go runs that one instruction with
    `execBlock`, then translated code resumes at the next one. If that instruction flushed
    the code cache, the interpreter finishes the block, as `execBlock` would with its stale
    copy.
  - `Limit`: the next block wouldn't fit before the limit.
- **Invalidation.**
  - `FlushCodeCache` (self-modifying code, patches, intercepts registered later) discards all
    translated code (`jitReset`).
  - A full code buffer does the same: 256 MB reserved on x86-64; 112 MB on arm64, where a B
    instruction reaches ±128 MB.
  - `SetCodeRange` and `MarkTextPages` mark the store table stale.
- **Off** with `C32_NO_JIT=1` (or `CPU.NoJIT`). It is also off while the interpreter's
  per-instruction hooks are wanted: call tracing, watchpoints, the guest profiler.

### Exactness

Translated code must be indistinguishable from the interpreter.

- **Memory.** Only the interpreter's fast paths are inlined:
  - loads need `addr < ArenaSize-8` (16-byte loads: `addr+16 <= ArenaSize-8`);
  - stores look up a per-page store table. It marks pages holding code, the page before each
    (a store may straddle into it), and pages past guest memory.
  
  Anything else is a slow exit *before* the instruction changes anything. Generated code never
  faults.
- **Flags.** `c.Flags` gets exactly what the interpreter computes, including bits the
  hardware leaves undefined:
  - logic ops zero AF;
  - INC and DEC keep CF;
  - IMUL sets only CF and OF;
  - shifts set OF only for a count of 1;
  - ROR, RCL and RCR set only CF;
  - SSE `UCOMISS` clears OF, SF and AF.
  
  **Flag liveness** (`jitCaptures`, shared by both backends): a flag result is kept only if
  something may observe it before an instruction overwrites all of it. Observers are an
  instruction reading flags other than straight from the host's, a slow exit, and the block's
  end.
- **Floating point.** Translated SSE matches the interpreter's Go code on the same host, NaN
  payloads included:
  - ADD and MUL with two NaN operands return the first one, quieted (`sseAdd`, `sseMul`);
  - truncating conversions out of range give 0x80000000 (`ToInt32`);
  - MINSS and MAXSS return the second operand unless strictly less (greater).
- **Forms.** Every `dInst` kind the decoded-instruction cache has is translated, including the
  fused compare+branch kinds, 8- and 16-bit ALU forms, and scalar and packed SSE. x87 isn't
  cached, so it stays in the interpreter. Untranslated forms are slow exits.

### x86-64 backend

- **Registers.**
  - Guest registers EAX..EDI live in rax, rcx, rdx, rbx, r12, rbp, rsi, rdi, so most
    instructions keep their encoding, including AH..BH.
  - r15 holds the CPU, r14 guest memory, r13 the store table.
  - r8 holds effective addresses; r9–r11 are scratch.
- **Flags** come from the host instruction itself:
  - When they must be kept: `PUSHFQ` into `jitState.raw`, with which bits in `pmask`.
  - They stay **pending** across blocks. They are merged into `c.Flags` only when something
    reads them or sets just some of them, and by Go on every exit.
  - A JCC, CMOV or SETCC right after the instruction that set the flags reads the host's
    flags directly.
- **Code placement.** Block bodies go in a hot region; exit stubs go in a cold one.

### arm64 backend

- **Registers.**
  - Guest registers live in w19–w26.
  - x27 holds the CPU, w8 the guest flags, x7 the step budget.
  - x9 holds guest memory, x10 the store table, x3 the lookup table, x5 `&XMM[0]`, and x11
    and x12 the flag tables.
  - x0 holds effective addresses; x1, x2 and x13–x17 are scratch.
  - Translated code never touches x18 (the platform register), x28 (Go's `g`, which signal
    handlers rely on), x29, x30 or the stack.
- **Flags are computed.** An instruction runs as the host instruction that sets NZCV the same
  way: ADDS, SUBS, ADCS, SBCS, ANDS, or TST after ORR/EOR. 8- and 16-bit operations work on
  operands shifted to the top of the register. When the flags must be kept they go into w8 at
  once:
  - CF, ZF, SF and OF come from NZCV through a table (`a64FlagTabs`), per mode: subtraction
    inverts C; INC and DEC have no CF; FCMP has its own table.
  - PF comes from a parity table.
  - AF is `(a^b^result) & 0x10`.
  - A reader right after the instruction that set the flags tests NZCV directly when a host
    condition matches its x86 condition in that mode (`a64Conds`); otherwise it tests w8.
  - Resuming after a slow exit, such a reader is entered through a copy of it that tests w8.
- **Left to the interpreter:** ADC and SBB on 8 and 16 bits, and RCL and RCR by CL. They are
  rare.
- **Executable memory.**
  - Linux: read-write-execute memory, then the caches are synchronised after writing
    (`jitFlushICache`: DC CVAU, IC IVAU, DSB, ISB).
  - macOS: `MAP_JIT`, with `pthread_jit_write_protect_np` toggled around writes on a locked
    thread, then `sys_icache_invalidate`. These are called through libSystem trampolines, as
    `golang.org/x/sys/unix` does, because the app is built without cgo.
  - Off by default there (`jitOptIn`) until verified.

## Testing

- **`TestJITMatchesInterpreter`** (`pkg/cpu/jit_test.go`) runs 3000 random programs through
  the interpreter and through the JIT, from the same state, and compares registers, flags,
  XMM, memory, EIP, steps and halts. Set `C32_JITN` for more programs.
  - The programs cover every form, with forward Jccs, calls to a guest subroutine, to a host
    function, through memory, and to `ret 4`, in a 6-iteration loop.
  - On odd seeds, stores to the data area are forced through the interpreter.
  - XMM registers start with quiet and signaling NaNs, single and double.
  - **`TestJITBisect`** (`C32_JITSEED=n`) finds the first step where seed n differs.
- **`TestA64Encodings`** checks the arm64 encoder against `golang.org/x/arch`'s disassembler.
- **arm64 on an x86 machine**, with QEMU user-mode (`qemu-user` from the distribution):
  ```sh
  GOARCH=arm64 go test -c -o /tmp/cpu_arm64.test ./pkg/cpu
  qemu-aarch64 /tmp/cpu_arm64.test -test.run 'JITMatches|A64' -test.v
  ```
  A wrapper script (`exec qemu-aarch64 <arm64 catalina32> "$@"`) runs whole games the same
  way, for example through `scripts/gpuroute.sh`.
- **Mutation checks** (done by hand, October 2026): deliberately broken arm64 code was caught
  by the full test:
  - CF not inverted after subtraction;
  - AF missing;
  - a wrong host condition for JLE;
  - the NaN rule for ADD and MUL (scalar and packed);
  - the NaN case of CVTTSS2SI.
- **Whole games:**
  - LEGO: `scripts/gpuroute.sh` with and without `C32_NO_JIT=1` must give identical `Steps=`
    lines.
  - Portal: `TestBoot` with `C32_GLCHECK` and `C32_GLTRACE` must give identical GL streams.
- **Speed:** the same runs side by side, without `aot_gen.go`.

**arm64 under QEMU** (October 2026):

- **LEGO**, `gpuroute.sh` `episode1`, 1.1 billion instructions, with both runs at once on the
  container:
  - Interpreter and JIT give the same 27 `Steps=` lines, final state and gpucheck summary as
    the x86-64 interpreter.
  - Times: 4 min 25 s interpreted, 1 min 40 s with the JIT. QEMU's own costs dominate both,
    so this says little about Apple hardware.
- **Portal**: `TestBoot` with `+map testchmb_a_00`, 4 billion instructions. The arm64 JIT
  gives a GL stream byte-identical to the x86-64 interpreter's (9.6 MB of commands).

## Diagnostics

| Variable | Effect |
|---|---|
| `C32_NO_JIT=1` | no JIT (A/B comparisons; a way out for players) |
| `C32_JIT=1` | the JIT where it is opt-in (Apple Silicon) |
| `C32_JITPROF=1` | per-block run counts and code costs; `TestBoot` prints the totals (flag saves, loads, stores, bytes per block run) and the costliest blocks |
| `C32_JITSTATS=1` | `TestBoot`: translations, entries, exits by kind, and where translated code most often returns to Go |
| `C32_JITDUMP=eip,...` | `TestBoot`: write those blocks' host and guest code to the temporary folder (disassemble with `objdump -D -b binary -mi386:x86-64`) |
| `C32_SLOWOPS=1` | `TestBoot`: the most frequent instructions the interpreter's full decoder runs (forms not cached, so not translated) |
| `C32_JITN`, `C32_JITSEED` | `TestJITMatchesInterpreter`: number of programs; `TestJITBisect`: the seed to bisect |

## Ideas not done

- **x87 (and MMX) in the cache and the JIT.** Miles's MP3 decoder is heavy in x87: `fld`,
  `fxch`, `fmul`, `faddp`, `fstp`. The interpreter's x87 is plain `float64` arithmetic on eight
  registers, which both backends can do with double-precision instructions.
- **Host calls without leaving translated code** (the main cost left in Portal). It would
  need calling Go from generated code, or translating the hottest host functions.
- **Superblocks or traces** across unconditional jumps and calls.
- **arm64:** keep flags pending across blocks, as x86-64 does. Today every kept result is
  computed at once, and a compare at a block's end is always kept.
- **Apple Silicon:** verify, then set `jitOptIn` to false in `jitmem_darwin_arm64.go`.

## Pitfalls

- Green threads switch on step counts (`Preempt`): a block must not run past `nextPreempt`.
- Code may be patched at run time (`pkg/games/lswsaga` patches `NuTexRead`; games may
  self-modify): invalidation must be exact.
- Intercepts can be registered after code was translated (lazy `Bind`): flush.
- `TraceCalls` and the guest profiler rely on per-call hooks in `execBlock` (`dCall`): the JIT
  is off while they are on.
- AOT and JIT together (developer builds): AOT functions are intercepts, so blocks stop at
  them; measure the JIT without AOT.
- Two `Watch`/`Around` hooks on the same symbol: the second is silently skipped.
- Go may evaluate `a+b` as `b+a`. Where the operand order shows (NaN payloads), the
  interpreter must say which it means (`sseAdd`), and the backends must do the same.

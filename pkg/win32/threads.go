package win32

import (
	"fmt"

	"catalina32/pkg/cpu"
)

// Green-thread scheduler.
//
// Guest threads are multiplexed onto the single emulated CPU. A switch happens when the running thread
// calls Sleep / blocks in WaitForSingleObject / blocks on a contended critical section, or when the
// preemption hook fires (every PreemptInterval steps). Switches only happen at nesting depth 0 so that
// host-initiated nested guest calls (CallGuestNested, qsort callbacks, ...) are never interleaved.

const (
	threadExitEIP     = 0x1FFFD000
	threadStackSize   = 512 * 1024
	waitTimeout       = 258
	waitInfinite      = 0xFFFFFFFF
	createSuspended   = 0x4
	preemptIntervalSt = 20000
)

type guestThread struct {
	handle    uint32
	ctx       cpu.Context
	suspended bool
	done      bool
	blocked   func() bool // non-nil while waiting; returns true while still blocked
}

type guestEvent struct {
	manual   bool
	signaled bool
}

type guestTimer struct {
	id         uint32
	delayMs    uint32
	proc       uint32
	user       uint32
	periodic   bool
	lastFireMs uint32
}

type scheduler struct {
	threads  []*guestThread // threads[0] is the main thread
	cur      *guestThread
	byHandle map[uint32]*guestThread
	events   map[uint32]*guestEvent
	timers   map[uint32]*guestTimer
}

func (env *Win32Env) initScheduler(c *cpu.CPU) {
	main := &guestThread{handle: env.CurrentThreadId}
	env.sched = &scheduler{
		threads:  []*guestThread{main},
		cur:      main,
		byHandle: map[uint32]*guestThread{main.handle: main},
		events:   make(map[uint32]*guestEvent),
		timers:   make(map[uint32]*guestTimer),
	}
	c.RegisterIntercept(threadExitEIP, "ThreadExit", env.thunkThreadExit)
	c.Preempt = func(vm *cpu.CPU) { env.yield(vm) }
	c.PreemptInterval = preemptIntervalSt
}

func (t *guestThread) runnable() bool {
	if t.done || t.suspended {
		return false
	}
	return t.blocked == nil || !t.blocked()
}

// pickNext returns the next runnable thread after the current one (round robin), or nil.
func (env *Win32Env) pickNext() *guestThread {
	s := env.sched
	n := len(s.threads)
	start := 0
	for i, t := range s.threads {
		if t == s.cur {
			start = i
			break
		}
	}
	for k := 1; k < n; k++ {
		t := s.threads[(start+k)%n]
		if t.runnable() {
			return t
		}
	}
	return nil
}

// switchTo saves the running thread (unless it has exited) and resumes next.
func (env *Win32Env) switchTo(c *cpu.CPU, next *guestThread) {
	s := env.sched
	if next == s.cur {
		return
	}
	if !s.cur.done {
		s.cur.ctx = c.SaveContext()
	}
	next.blocked = nil
	c.LoadContext(&next.ctx)
	s.cur = next
	env.CurrentThreadId = next.handle
	if next == s.threads[0] {
		c.SetPreemptInterval(preemptIntervalSt)
	} else {
		c.SetPreemptInterval(400000)
	}
}

// yield gives other runnable threads a chance to run. No-op inside nested host-initiated guest calls.
func (env *Win32Env) yield(c *cpu.CPU) {
	if env.sched == nil || c.NestDepth != 0 || len(env.sched.threads) < 2 {
		return
	}
	if next := env.pickNext(); next != nil {
		env.switchTo(c, next)
	}
}

// blockAndSwitch parks the current thread at the current EIP (the thunk will be re-executed on resume)
// until stillBlocked returns false. Returns false if no other thread can run (caller must not block).
func (env *Win32Env) blockAndSwitch(c *cpu.CPU, stillBlocked func() bool) bool {
	if env.sched == nil || c.NestDepth != 0 {
		return false
	}
	next := env.pickNext()
	if next == nil {
		return false
	}
	env.sched.cur.blocked = stillBlocked
	env.switchTo(c, next)
	return true
}

func (env *Win32Env) thunkThreadExit(c *cpu.CPU) {
	s := env.sched
	s.cur.done = true
	next := env.pickNext()
	if next == nil {
		next = s.threads[0] // main thread always exists; resume it even if its wait is unsatisfied
	}
	if env.TraceLog {
		fmt.Printf("[Win32] thread 0x%x exited -> resuming 0x%x\n", s.cur.handle, next.handle)
	}
	env.switchTo(c, next)
}

func (env *Win32Env) thunkCreateThread(c *cpu.CPU) {
	// lpThreadAttributes(0), dwStackSize(1), lpStartAddress(2), lpParameter(3), dwCreationFlags(4), lpThreadId(5)
	start := c.Arg(2)
	param := c.Arg(3)
	flags := c.Arg(4)
	tidPtr := c.Arg(5)
	h := env.NextHandle
	env.NextHandle++
	if tidPtr != 0 {
		c.Write32(tidPtr, h)
	}

	stack := c.Malloc(threadStackSize)
	top := (stack + threadStackSize) &^ 15
	esp := top - 20 // esp+4 is 16-byte aligned, as after a call
	c.Write32(esp, threadExitEIP)
	c.Write32(esp+4, param)

	t := &guestThread{handle: h, suspended: flags&createSuspended != 0}
	t.ctx = cpu.Context{
		EIP:       start,
		Flags:     0x202,
		FSBase:    c.FSBase,
		GSBase:    c.GSBase,
		FPUCW:     0x037F,
		MXCSR:     0x1F80,
		CallStack: make([]cpu.CallFrame, 0, 64),
	}
	t.ctx.Regs[cpu.RegESP] = esp
	env.sched.threads = append(env.sched.threads, t)
	env.sched.byHandle[h] = t

	if env.TraceLog {
		fmt.Printf("[Win32] CreateThread(start=0x%08x, param=0x%08x, suspended=%v) -> handle=0x%x\n", start, param, t.suspended, h)
	}
	c.Regs[cpu.RegEAX] = h
	c.Ret()
}

func (env *Win32Env) thunkResumeThread(c *cpu.CPU) {
	h := c.Arg(0)
	if t, ok := env.sched.byHandle[h]; ok {
		t.suspended = false
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkSleep(c *cpu.CPU) {
	c.Ret()
	env.yield(c)
}

func (env *Win32Env) thunkCreateEventA(c *cpu.CPU) {
	// CreateEventA(lpAttributes, bManualReset, bInitialState, lpName)
	h := env.NextHandle
	env.NextHandle++
	env.sched.events[h] = &guestEvent{manual: c.Arg(1) != 0, signaled: c.Arg(2) != 0}
	c.Regs[cpu.RegEAX] = h
	c.Ret()
}

// isSignaled reports whether waiting on h would succeed right now. known=false means h is not a handle we model.
func (env *Win32Env) isSignaled(h uint32) (signaled, known bool) {
	if t, ok := env.sched.byHandle[h]; ok {
		return t.done, true
	}
	if ev, ok := env.sched.events[h]; ok {
		return ev.signaled, true
	}
	return true, false
}

func (env *Win32Env) consume(h uint32) {
	if ev, ok := env.sched.events[h]; ok && !ev.manual {
		ev.signaled = false
	}
}

func (env *Win32Env) thunkWaitForSingleObject(c *cpu.CPU) {
	h := c.Arg(0)
	timeout := c.Arg(1)
	sig, known := env.isSignaled(h)
	if !known || sig {
		if known {
			env.consume(h)
		}
		c.Regs[cpu.RegEAX] = 0 // WAIT_OBJECT_0
		c.Ret()
		return
	}
	if timeout == 0 {
		c.Regs[cpu.RegEAX] = waitTimeout
		c.Ret()
		return
	}
	if timeout == waitInfinite {
		still := func() bool { s, _ := env.isSignaled(h); return !s }
		if env.blockAndSwitch(c, still) {
			return // thunk re-executes when this thread is resumed
		}
		// Nothing else can run: avoid hanging forever (legacy behaviour).
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	// Finite timeout: no real clock, so yield once and report a timeout.
	c.Regs[cpu.RegEAX] = waitTimeout
	c.Ret()
	env.yield(c)
}

func (env *Win32Env) thunkEnterCriticalSection(c *cpu.CPU) {
	cs := c.Arg(0)
	if cs == 0 {
		c.Ret()
		return
	}
	me := env.sched.cur.handle
	owner := c.Read32(cs + 8)
	if owner != 0 && owner != me {
		still := func() bool { o := c.Read32(cs + 8); return o != 0 && o != me }
		if env.blockAndSwitch(c, still) {
			return // re-executes on resume
		}
		// Deadlock fallback: take the lock.
	}
	c.Write32(cs+8, me)
	c.Write32(cs+4, c.Read32(cs+4)+1)
	c.Ret()
}

func (env *Win32Env) thunkLeaveCriticalSection(c *cpu.CPU) {
	cs := c.Arg(0)
	if cs != 0 {
		v := c.Read32(cs + 4)
		if v > 0 {
			v--
			c.Write32(cs+4, v)
		}
		if v == 0 {
			c.Write32(cs+8, 0)
		}
	}
	c.Ret()
}

// thunkResetEvent: 0x00623c30 takes exactly one argument (the event handle) and is called by the worker loop
// at 0x001e7a67 right after its WaitForSingleObject(h, INFINITE) returns.
func (env *Win32Env) thunkResetEvent(c *cpu.CPU) {
	if ev, ok := env.sched.events[c.Arg(0)]; ok {
		ev.signaled = false
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

// thunkTimeSetEvent: 0x00624310 timeSetEvent(uDelay, uResolution, lpTimeProc, dwUser, fuEvent)
func (env *Win32Env) thunkTimeSetEvent(c *cpu.CPU) {
	delay := c.Arg(0)
	if delay < 450 {
		delay = 450
	}
	proc := c.Arg(2)
	user := c.Arg(3)
	fuEvent := c.Arg(4)
	id := env.NextHandle
	env.NextHandle++
	env.sched.timers[id] = &guestTimer{
		id:         id,
		delayMs:    delay,
		proc:       proc,
		user:       user,
		periodic:   (fuEvent & 1) != 0,
		lastFireMs: env.currentMs(c),
	}
	c.Regs[cpu.RegEAX] = id
	c.Ret()
}

// thunkTimeKillEvent: 0x006245f0 timeKillEvent(uTimerID)
func (env *Win32Env) thunkTimeKillEvent(c *cpu.CPU) {
	delete(env.sched.timers, c.Arg(0))
	c.Regs[cpu.RegEAX] = 0 // TIMERR_NOERROR
	c.Ret()
}

// fireTimers invokes any elapsed multimedia timers on the main thread when at NestDepth 0.
func (env *Win32Env) fireTimers(c *cpu.CPU) {
	if env.sched == nil || c.NestDepth != 0 || len(env.sched.timers) == 0 {
		return
	}
	now := env.currentMs(c)
	fired := false
	for id, t := range env.sched.timers {
		if now-t.lastFireMs >= t.delayMs {
			t.lastFireMs = now
			if !t.periodic {
				delete(env.sched.timers, id)
			}
			if t.proc != 0 {
				_, _ = c.CallGuestNested(t.proc, t.id, 0, t.user, 0, 0)
				fired = true
			}
		}
	}
	if fired {
		env.yield(c)
	}
}


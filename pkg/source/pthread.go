package source

import (
	"fmt"
	"strings"
	"time"

	"catalina32/pkg/cpu"
)

// POSIX threads on green threads: guest threads share the one emulated CPU and switch when one
// blocks (mutex, condition variable, join, sleep) or after a time slice (preemption). A blocked
// thread is parked at the call into the host function, which runs again when it resumes; a
// sleeping thread is parked after the call. When every thread waits, the clock jumps to the
// first timeout.

const (
	defaultStack      = 512 << 10
	timeSlice         = 200_000 // instructions
	eBUSY             = 16
	eDEADLK           = 11
	eSRCH             = 3
	mutexRecursiveSig = 0x32AAABA2 // PTHREAD_RECURSIVE_MUTEX_INITIALIZER
)

type gthread struct {
	id         uint32 // pthread_t (a small guest block)
	ctx        cpu.Context
	done       bool
	suspended  bool
	retval     uint32
	blocked    func() bool // non-nil while parked: true while it must stay parked
	wakeAt     uint64      // parked with a timeout (microseconds), or 0
	tls        map[uint32]uint32
	wait       *condWait // inside pthread_cond_wait
	mpDeadline uint64    // inside MPWaitOnSemaphore with a timeout
	name       string
}

type condWait struct {
	cond     uint32
	signaled bool
	deadline uint64 // 0: none
	timedOut bool
}

type sched struct {
	threads []*gthread
	cur     *gthread
	byID    map[uint32]*gthread
	exitEIP uint32
}

type gmutex struct {
	owner     *gthread
	count     int
	recursive bool
}

type gcond struct {
	waiters []*gthread
}

func (m *Machine) initThreads() {
	main := &gthread{id: m.data(16), tls: map[uint32]uint32{}, name: "main"}
	m.threads = &sched{threads: []*gthread{main}, cur: main, byID: map[uint32]*gthread{main.id: main}}
	m.threads.exitEIP = m.system("", "<thread exit>")
	m.C.RegisterIntercept(m.threads.exitEIP, "<thread exit>", m.threadExit)
	m.C.Preempt = func(c *cpu.CPU) {
		if len(m.threads.threads) > 1 {
			m.schedule(c)
		}
	}
	m.C.SetPreemptInterval(timeSlice)
}

func (m *Machine) registerThreadShims() {
	for k, v := range map[string]cpu.ThunkFunc{
		"_pthread_create": m.pthreadCreate(false), "_pthread_create_suspended_np": m.pthreadCreate(true),
		"_pthread_self": m.pthreadSelf, "_pthread_mach_thread_np": m.pthreadSelfArg, "_pthread_from_mach_thread_np": m.pthreadSelfArg,
		"_pthread_equal": m.pthreadEqual, "_pthread_join": m.pthreadJoin, "_pthread_detach": ret0,
		"_pthread_kill": m.pthreadKill, "_pthread_exit": m.pthreadExit, "_pthread_setname_np": m.pthreadSetname,
		"_pthread_attr_init": m.attrInit, "_pthread_attr_destroy": ret0, "_pthread_attr_setstacksize": m.attrSetStack,
		"_pthread_attr_getstacksize": m.attrGetStack, "_pthread_attr_setdetachstate": ret0, "_pthread_attr_setschedparam": ret0,
		"_pthread_attr_getschedparam": m.getSchedParam(1), "_pthread_attr_setschedpolicy": ret0,
		"_pthread_getschedparam": m.getSchedParam(2), "_pthread_setschedparam": ret0,
		"_sched_get_priority_max": retN(47), "_sched_get_priority_min": retN(15), "_sched_yield": m.yieldShim,
		"_pthread_yield_np": m.yieldShim, "_thread_policy_set": ret0, "_thread_policy_get": ret0,
		"_thread_suspend": m.threadSuspend(true), "_thread_resume": m.threadSuspend(false),
		"_pthread_key_create": m.keyCreate, "_pthread_key_delete": ret0,
		"_pthread_getspecific": m.getSpecific, "_pthread_setspecific": m.setSpecific, "_pthread_once": m.once,
		"_pthread_mutexattr_init": m.mutexattrInit, "_pthread_mutexattr_destroy": ret0, "_pthread_mutexattr_settype": m.mutexattrSettype,
		"_pthread_mutex_init": m.mutexInit, "_pthread_mutex_destroy": m.mutexDestroy, "_pthread_mutex_lock": m.mutexLock,
		"_pthread_mutex_trylock": m.mutexTrylock, "_pthread_mutex_unlock": m.mutexUnlock,
		"_pthread_cond_init": m.condInit, "_pthread_cond_destroy": m.condDestroy, "_pthread_cond_signal": m.condSignal(false),
		"_pthread_cond_broadcast": m.condSignal(true), "_pthread_cond_wait": m.condWait(untimed), "_pthread_cond_timedwait": m.condWait(absolute),
		"_pthread_cond_timedwait_relative_np": m.condWait(relative),
		"_semaphore_create":                   m.enosys,
	} {
		m.shims[k] = v
	}
}

func (m *Machine) runnable(t *gthread) bool {
	if t.done || t.suspended {
		return false
	}
	if t.blocked == nil || !t.blocked() {
		return true
	}
	return t.wakeAt != 0 && m.micros() >= t.wakeAt
}

// schedule switches to the next runnable thread (round robin, the current one last). With none
// runnable it moves the clock to the earliest timeout; with no timeout either, the game is
// deadlocked and stops.
func (m *Machine) schedule(c *cpu.CPU) bool {
	s := m.threads
	if c.NestDepth != 0 {
		return false
	}
	for {
		n := len(s.threads)
		start := 0
		for i, t := range s.threads {
			if t == s.cur {
				start = i
			}
		}
		for k := 1; k <= n; k++ {
			t := s.threads[(start+k)%n]
			if m.runnable(t) {
				m.switchTo(c, t)
				return true
			}
		}
		earliest := uint64(0)
		for _, t := range s.threads {
			if !t.done && !t.suspended && t.wakeAt != 0 && (earliest == 0 || t.wakeAt < earliest) {
				earliest = t.wakeAt
			}
		}
		if earliest == 0 {
			var where []string
			for _, t := range s.threads {
				if !t.done {
					eip := t.ctx.EIP
					if t == s.cur {
						eip = c.EIP
					}
					where = append(where, fmt.Sprintf("%s(%#x) at %s", t.name, t.id, m.where(eip)))
				}
			}
			c.Halted = true
			c.HaltReason = "deadlock: every thread waits: " + strings.Join(where, "; ")
			return false
		}
		if wait := earliest - m.micros(); m.Live && wait < 1e6 {
			time.Sleep(time.Duration(wait) * time.Microsecond) // every thread waits: really wait
		} else {
			m.skew += earliest - m.micros()
		}
	}
}

func (m *Machine) switchTo(c *cpu.CPU, t *gthread) {
	s := m.threads
	t.blocked, t.wakeAt = nil, 0
	if t == s.cur {
		return
	}
	if !s.cur.done {
		s.cur.ctx = c.SaveContext()
	}
	c.LoadContext(&t.ctx)
	s.cur = t
}

// park blocks the current thread until stillBlocked returns false (or the clock reaches wake,
// if not 0) and runs another. False when no switch is possible (inside a host-initiated call).
func (m *Machine) park(c *cpu.CPU, stillBlocked func() bool, wake uint64) bool {
	if c.NestDepth != 0 {
		return false
	}
	t := m.threads.cur
	t.blocked, t.wakeAt = stillBlocked, wake
	m.schedule(c)
	return true
}

// sleepFor parks the current thread (already returned from the call) for us microseconds.
func (m *Machine) sleepFor(us uint64) {
	c := m.C
	if us == 0 {
		m.schedule(c)
		return
	}
	wake := m.micros() + us
	if !m.park(c, func() bool { return m.micros() < wake }, wake) {
		m.skew += us
	}
}

func (m *Machine) yieldShim(c *cpu.CPU) {
	ret(c, 0)
	m.schedule(c)
}

func (m *Machine) pthreadCreate(suspended bool) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		out, attr, start, arg := c.Arg(0), c.Arg(1), c.Arg(2), c.Arg(3)
		size := uint32(defaultStack)
		if attr != 0 && c.Read32(attr+8) != 0 {
			size = c.Read32(attr + 8)
		}
		stack := c.Malloc(size)
		if stack == 0 {
			ret(c, 35)
			return
		}
		esp := (stack+size)&^15 - 16 + 4 // as after a call: the argument 16-byte aligned
		esp -= 4
		c.Write32(esp, m.threads.exitEIP)
		c.Write32(esp+4, arg)
		t := &gthread{id: m.data(16), tls: map[uint32]uint32{}, suspended: suspended,
			name: fmt.Sprintf("thread%d", len(m.threads.threads))}
		t.ctx = cpu.Context{EIP: start, Flags: 0x202, FSBase: c.FSBase, GSBase: c.GSBase, FPUCW: 0x037F, MXCSR: 0x1F80}
		t.ctx.Regs[cpu.RegESP] = esp
		m.threads.threads = append(m.threads.threads, t)
		m.threads.byID[t.id] = t
		c.Write32(out, t.id)
		m.logf("[thread] %s created: %s(%#x), stack %d KB", t.name, m.where(start), arg, size>>10)
		ret(c, 0)
	}
}

func (m *Machine) threadExit(c *cpu.CPU) {
	t := m.threads.cur
	t.done, t.retval = true, c.Regs[cpu.RegEAX]
	m.schedule(c)
}

func (m *Machine) pthreadExit(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = c.Arg(0)
	m.threadExit(c)
}

func (m *Machine) pthreadSelf(c *cpu.CPU)    { ret(c, m.threads.cur.id) }
func (m *Machine) pthreadSelfArg(c *cpu.CPU) { ret(c, c.Arg(0)) }

func (m *Machine) pthreadEqual(c *cpu.CPU) {
	if c.Arg(0) == c.Arg(1) {
		ret(c, 1)
		return
	}
	ret(c, 0)
}

func (m *Machine) pthreadSetname(c *cpu.CPU) {
	m.threads.cur.name = c.ReadCString(c.Arg(0))
	ret(c, 0)
}

func (m *Machine) pthreadJoin(c *cpu.CPU) {
	t := m.threads.byID[c.Arg(0)]
	if t == nil {
		ret(c, eSRCH)
		return
	}
	if !t.done {
		if !m.park(c, func() bool { return !t.done }, 0) {
			ret(c, eDEADLK)
		}
		return // runs again once t has finished
	}
	if c.Arg(1) != 0 {
		c.Write32(c.Arg(1), t.retval)
	}
	ret(c, 0)
}

func (m *Machine) pthreadKill(c *cpu.CPU) {
	if t := m.threads.byID[c.Arg(0)]; t == nil || t.done {
		ret(c, eSRCH)
		return
	}
	ret(c, 0)
}

func (m *Machine) threadSuspend(suspend bool) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		t := m.threads.byID[c.Arg(0)]
		if t == nil {
			ret(c, 4)
			return
		}
		t.suspended = suspend
		ret(c, 0)
		if suspend && t == m.threads.cur {
			m.schedule(c)
		}
	}
}

// pthread_attr_t: sig, then (ours) the stack size at +8.
func (m *Machine) attrInit(c *cpu.CPU) {
	clear(c.WBytes(c.Arg(0), 40))
	c.Write32(c.Arg(0), 0x54484441)
	ret(c, 0)
}

func (m *Machine) attrSetStack(c *cpu.CPU) {
	c.Write32(c.Arg(0)+8, c.Arg(1))
	ret(c, 0)
}

func (m *Machine) attrGetStack(c *cpu.CPU) {
	v := c.Read32(c.Arg(0) + 8)
	if v == 0 {
		v = defaultStack
	}
	c.Write32(c.Arg(1), v)
	ret(c, 0)
}

func (m *Machine) getSchedParam(paramArg int) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		if paramArg == 2 {
			c.Write32(c.Arg(1), 1) // SCHED_OTHER
		}
		c.Write32(c.Arg(paramArg), 31)
		ret(c, 0)
	}
}

func (m *Machine) keyCreate(c *cpu.CPU) {
	m.tlsKeys++
	c.Write32(c.Arg(0), m.tlsKeys)
	ret(c, 0)
}

func (m *Machine) getSpecific(c *cpu.CPU) { ret(c, m.threads.cur.tls[c.Arg(0)]) }

func (m *Machine) setSpecific(c *cpu.CPU) {
	m.threads.cur.tls[c.Arg(0)] = c.Arg(1)
	ret(c, 0)
}

func (m *Machine) once(c *cpu.CPU) {
	p, fn := c.Arg(0), c.Arg(1)
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
	if m.onces[p] {
		return
	}
	m.onces[p] = true
	eax := c.Regs[cpu.RegEAX]
	c.CallGuest(fn)
	c.Regs[cpu.RegEAX] = eax
}

// pthread_mutexattr_t: sig, then (ours) the type at +4.
func (m *Machine) mutexattrInit(c *cpu.CPU) {
	c.Write32(c.Arg(0), 0x4d545841)
	c.Write32(c.Arg(0)+4, 0)
	ret(c, 0)
}

func (m *Machine) mutexattrSettype(c *cpu.CPU) {
	c.Write32(c.Arg(0)+4, c.Arg(1))
	ret(c, 0)
}

func (m *Machine) mutex(p uint32) *gmutex {
	mu := m.mutexes[p]
	if mu == nil {
		mu = &gmutex{recursive: m.C.Read32(p) == mutexRecursiveSig}
		m.mutexes[p] = mu
	}
	return mu
}

func (m *Machine) mutexInit(c *cpu.CPU) {
	p, attr := c.Arg(0), c.Arg(1)
	m.mutexes[p] = &gmutex{recursive: attr != 0 && c.Read32(attr+4) == 2}
	c.Write32(p, 0x4d555458)
	ret(c, 0)
}

func (m *Machine) mutexDestroy(c *cpu.CPU) {
	delete(m.mutexes, c.Arg(0))
	ret(c, 0)
}

func (m *Machine) tryLock(mu *gmutex) bool {
	t := m.threads.cur
	switch {
	case mu.owner == nil:
		mu.owner, mu.count = t, 1
	case mu.owner == t && mu.recursive:
		mu.count++
	default:
		return false
	}
	return true
}

func (m *Machine) mutexLock(c *cpu.CPU) {
	mu := m.mutex(c.Arg(0))
	if m.tryLock(mu) {
		ret(c, 0)
		return
	}
	if mu.owner == m.threads.cur {
		m.logf("[thread] %s locks a mutex it holds (not recursive) at %s", m.threads.cur.name, m.where(c.Read32(c.Regs[cpu.RegESP])))
		ret(c, eDEADLK)
		return
	}
	if !m.park(c, func() bool { return mu.owner != nil && !mu.owner.done }, 0) {
		ret(c, eDEADLK)
	}
	if mu.owner != nil && mu.owner.done {
		mu.owner = nil
	}
}

func (m *Machine) mutexTrylock(c *cpu.CPU) {
	if m.tryLock(m.mutex(c.Arg(0))) {
		ret(c, 0)
		return
	}
	ret(c, eBUSY)
}

func (m *Machine) mutexUnlock(c *cpu.CPU) {
	mu := m.mutex(c.Arg(0))
	if mu.owner == m.threads.cur {
		mu.count--
		if mu.count <= 0 {
			mu.owner, mu.count = nil, 0
		}
	}
	ret(c, 0)
}

func (m *Machine) cond(p uint32) *gcond {
	cv := m.conds[p]
	if cv == nil {
		cv = &gcond{}
		m.conds[p] = cv
	}
	return cv
}

func (m *Machine) condInit(c *cpu.CPU) {
	m.conds[c.Arg(0)] = &gcond{}
	ret(c, 0)
}

func (m *Machine) condDestroy(c *cpu.CPU) {
	delete(m.conds, c.Arg(0))
	ret(c, 0)
}

func (m *Machine) condSignal(all bool) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		cv := m.cond(c.Arg(0))
		for len(cv.waiters) > 0 {
			t := cv.waiters[0]
			cv.waiters = cv.waiters[1:]
			if t.wait != nil && !t.wait.signaled && !t.wait.timedOut {
				t.wait.signaled = true
				if !all {
					break
				}
			}
		}
		ret(c, 0)
	}
}

// condWait: the first call releases the mutex and parks; the call runs again when the thread is
// signalled or times out, and then takes the mutex back (parking again while it is held).
const (
	untimed = iota
	absolute
	relative
)

func (m *Machine) condWait(mode int) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		t := m.threads.cur
		cp, mp := c.Arg(0), c.Arg(1)
		mu := m.mutex(mp)
		if t.wait == nil || t.wait.cond != cp {
			w := &condWait{cond: cp}
			if mode != untimed {
				ts := c.Arg(2)
				us := uint64(c.Read32(ts))*1e6 + uint64(c.Read32(ts+4))/1000
				if mode == relative {
					w.deadline = m.micros() + us
				} else if base := uint64(epoch.UnixMicro()); us > base {
					w.deadline = us - base
				} else {
					w.deadline = 1
				}
			}
			t.wait = w
			mu.owner, mu.count = nil, 0
			cv := m.cond(cp)
			cv.waiters = append(cv.waiters, t)
			if !m.park(c, func() bool { return !w.signaled && (w.deadline == 0 || m.micros() < w.deadline) }, w.deadline) {
				t.wait = nil
				mu.owner, mu.count = t, 1
				ret(c, eDEADLK)
			}
			return
		}
		w := t.wait
		if !w.signaled {
			w.timedOut = true
		}
		if !m.tryLock(mu) {
			m.park(c, func() bool { return mu.owner != nil && !mu.owner.done }, 0)
			return
		}
		t.wait = nil
		if w.timedOut {
			ret(c, eTIMEDOUT)
			return
		}
		ret(c, 0)
	}
}

package source

import (
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"catalina32/pkg/cpu"
	"catalina32/pkg/crt"
)

func ret(c *cpu.CPU, v uint32) {
	c.Regs[cpu.RegEAX] = v
	c.Ret()
}

func ret64(c *cpu.CPU, v uint64) {
	c.Regs[cpu.RegEAX], c.Regs[cpu.RegEDX] = uint32(v), uint32(v>>32)
	c.Ret()
}

func retF(c *cpu.CPU, v float64) {
	c.FPUPush(v)
	c.Ret()
}

func neg1() uint32 { return 0xffffffff }

// errno values (Darwin).
const (
	eNOENT    = 2
	eBADF     = 9
	eACCES    = 13
	eEXIST    = 17
	eNOTDIR   = 20
	eINVAL    = 22
	eAGAIN    = 35
	eTIMEDOUT = 60
	eNOSYS    = 78
)

func (m *Machine) setErrno(v uint32) { m.C.Write32(m.errno, v) }

func errnoOf(err error) uint32 {
	switch {
	case os.IsNotExist(err):
		return eNOENT
	case os.IsExist(err):
		return eEXIST
	case os.IsPermission(err):
		return eACCES
	}
	return eINVAL
}

// Clock: guest time is derived from the instruction count, so runs are reproducible.
const stepsPerMicrosecond = 50

var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func (m *Machine) micros() uint64 {
	if m.Live {
		return uint64(time.Since(m.start).Microseconds()) + m.skew
	}
	return m.C.Steps/stepsPerMicrosecond + m.skew
}

func (m *Machine) now() time.Time {
	return epoch.Add(time.Duration(m.micros()) * time.Microsecond)
}

// hostPath maps a guest path to the host: relative paths are from the working directory; files
// opened for writing (and everything created) live in the overlay; reads prefer the overlay.
// Mac file systems ignore case, so a path that doesn't exist is looked up ignoring case.
func (m *Machine) hostPath(p string, write bool) string {
	if p == "/dev/null" || p == "/dev/zero" || p == "/dev/urandom" || p == "/dev/random" {
		return p
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.cwd, p)
	}
	p = filepath.Clean(p)
	ov := filepath.Join(m.Overlay, p)
	if write {
		clear(m.ovMissing) // the game may be about to create files there
		os.MkdirAll(filepath.Dir(ov), 0o755)
		if _, err := os.Stat(ov); err != nil {
			if src := m.fold(p); src != "" { // copy-on-write: start from the game's file
				if b, err := os.ReadFile(src); err == nil {
					os.WriteFile(ov, b, 0o644)
				}
			}
		}
		return ov
	}
	if m.ovExists(ov) {
		return ov
	}
	if q := m.fold(p); q != "" {
		return q
	}
	return p
}

// Source games look for many files that aren't there (every search path, every frame), and
// each miss used to cost directory reads; these caches answer them. The game's folder doesn't
// change while it runs; the overlay only gains files through hostPath's write mode, which
// forgets what the overlay was known not to have.

// fold is foldCase, with its results and the directory listings it reads cached.
func (m *Machine) fold(p string) string {
	if q, ok := m.folded[p]; ok {
		return q
	}
	if m.folded == nil {
		m.folded = map[string]string{}
		m.dirNames = map[string]map[string]string{}
	}
	q := ""
	if _, err := os.Lstat(p); err == nil {
		q = p
	} else if dir, base := filepath.Split(p); base != "" && filepath.Clean(dir) != p {
		if real := m.fold(filepath.Clean(dir)); real != "" {
			names, ok := m.dirNames[real]
			if !ok {
				names = map[string]string{}
				if ents, err := os.ReadDir(real); err == nil {
					for _, e := range ents {
						names[strings.ToLower(e.Name())] = e.Name()
					}
				}
				m.dirNames[real] = names
			}
			if n, ok := names[strings.ToLower(base)]; ok {
				q = filepath.Join(real, n)
			}
		}
	}
	m.folded[p] = q
	return q
}

// ovExists says whether the overlay has ov (misses are remembered until the game writes).
func (m *Machine) ovExists(ov string) bool {
	if m.ovMissing[ov] {
		return false
	}
	if _, err := os.Stat(ov); err == nil {
		return true
	}
	if m.ovMissing == nil {
		m.ovMissing = map[string]bool{}
	}
	m.ovMissing[ov] = true
	return false
}

// foldCase returns the existing path matching p ignoring case, or "".
func foldCase(p string) string {
	if _, err := os.Lstat(p); err == nil {
		return p
	}
	dir, base := filepath.Split(p)
	dir = filepath.Clean(dir)
	if dir == p || base == "" {
		return ""
	}
	real := foldCase(dir)
	if real == "" {
		return ""
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if strings.EqualFold(e.Name(), base) {
			return filepath.Join(real, e.Name())
		}
	}
	return ""
}

func (m *Machine) registerShims() {
	c := m.C
	_ = c
	m.shims = map[string]cpu.ThunkFunc{
		// Process.
		"___error": func(c *cpu.CPU) { ret(c, m.errno) },
		"_exit":    m.exit, "__exit": m.exit, "_abort": m.abort, "___assert_rtn": m.assertRtn,
		"_atexit": m.atexitShim, "___cxa_atexit": m.cxaAtexit,
		"_getenv": m.getenv, "_setenv": m.setenv, "_unsetenv": ret0,
		"_getpid": retN(4242), "_getuid": retN(501), "_geteuid": retN(501), "_getppid": retN(1),
		"__NSGetExecutablePath": m.getExecutablePath,
		"__NSGetArgc":           m.nsGetArgc, "__NSGetArgv": m.nsGetArgv, "__NSGetEnviron": m.nsGetEnviron,
		"_signal": ret0, "_sigaction": ret0, "_sigprocmask": ret0, "_raise": m.abort, "_kill": ret0,
		"_setlocale": m.setlocale, "_isatty": ret0, "_system": retN(neg1()), "_getrlimit": m.getrlimit, "_setrlimit": ret0,
		"_getrusage": m.getrusage, "_getloadavg": retN(neg1()), "_sysctl": m.sysctl, "_sysctlbyname": m.sysctlbyname,
		"_gethostname": m.gethostname, "_getpwuid": ret0, "_ctermid": ret0, "_tcgetattr": retN(neg1()), "_tcsetattr": retN(neg1()),
		"_setjmp": m.setjmp, "__setjmp": m.setjmp, "_longjmp": m.longjmp, "__longjmp": m.longjmp,
		"_dlopen": m.dlopen, "_dlsym": m.dlsym, "_dlclose": ret0, "_dlerror": m.dlerror, "_dladdr": ret0,
		"_mach_absolute_time": m.machAbsoluteTime, "_mach_timebase_info": m.machTimebaseInfo,
		"_mach_task_self": retN(0x103), "_mach_thread_self": m.pthreadSelf, "_mach_port_deallocate": ret0,
		"_host_page_size": m.hostPageSize, "_vm_page_size": retN(4096),
		"_OSAtomicCompareAndSwap32": m.casN(4), "_OSAtomicCompareAndSwap32Barrier": m.casN(4),
		"_OSAtomicCompareAndSwapPtr": m.casN(4), "_OSAtomicCompareAndSwapPtrBarrier": m.casN(4),
		"_OSAtomicCompareAndSwapInt": m.casN(4), "_OSAtomicCompareAndSwapIntBarrier": m.casN(4),
		"_OSAtomicCompareAndSwap64": m.casN(8), "_OSAtomicCompareAndSwap64Barrier": m.casN(8),
		"_OSAtomicAdd32": m.atomicAdd, "_OSAtomicAdd32Barrier": m.atomicAdd,
		"_OSAtomicIncrement32": m.atomicInc(1), "_OSAtomicIncrement32Barrier": m.atomicInc(1),
		"_OSAtomicDecrement32": m.atomicInc(-1), "_OSAtomicDecrement32Barrier": m.atomicInc(-1),
		"_OSMemoryBarrier": retVoid,

		// Memory.
		"_malloc_size": m.mallocSize, "_valloc": m.valloc, "_posix_memalign": m.posixMemalign,
		"_mmap": m.mmap, "_munmap": ret0, "_mprotect": ret0, "_madvise": ret0, "_mstats": m.mstats,
		"___memcpy_chk": m.memcpyChk, "___memmove_chk": m.memcpyChk, "___memset_chk": m.memsetChk,
		"___strcpy_chk": m.strcpyChk, "___strcat_chk": m.strcatChk, "___strncpy_chk": m.strncpyChk,

		// Strings and characters.
		"___maskrune": m.maskrune, "___tolower": m.tolower, "___toupper": m.toupper,
		"_tolower": m.tolower, "_toupper": m.toupper,
		"_strtok": m.strtok, "_strpbrk": m.strpbrk, "_strspn": m.strspn, "_strcspn": m.strcspn,
		"_strerror": m.strerror, "_strtod": m.strtod, "_strtof": m.strtod, "_atoll": m.strtoll(false, true),
		"_strtoll": m.strtoll(false, false), "_strtoull": m.strtoll(true, false), "_strnlen": m.strnlen,
		"_strlcpy": m.strlcpy, "_strlcat": m.strlcat, "_stpcpy": m.stpcpy,
		"_mbstowcs": m.mbstowcs, "_wcstombs": m.wcstombs, "_wcslen": m.wcslen, "_wcscpy": m.wcscpy,
		"_wcsncpy": m.wcsncpy, "_wcscat": m.wcscat, "_wcsncat": m.wcsncat, "_wcscmp": m.wcscmp, "_wcscoll": m.wcscmp,
		"_wcsncmp": m.wcsncmp, "_wcschr": m.wcschr, "_wcsstr": m.wcsstr, "_wcstol": m.wcstol, "_wcstoll": m.wcstol,
		"_wcstod": m.wcstod, "_swprintf": m.swprintf, "_vswprintf": m.vswprintf, "_swscanf": m.swscanf,
		"_vfprintf": m.vfprintf, "_fprintf": m.fprintf, "_puts": m.puts, "_putchar": m.putchar,

		// Maths not in crt.
		"_modf": m.modf, "_finite": m.finite, "___isnanf": m.isnanf, "_fmaxf": m.fminmaxf(math.Max), "_fminf": m.fminmaxf(math.Min),
		"___fixunsdfdi": m.fixunsdfdi, "_lround": m.lround, "_lroundf": m.lroundf, "_sqrtf": nil,
		"_cbrt": m.unaryF64(math.Cbrt), "_exp2": m.unaryF64(math.Exp2), "_log2": m.unaryF64(math.Log2),
		"_hypot": m.binaryF64(math.Hypot), "_sinh": m.unaryF64(math.Sinh), "_cosh": m.unaryF64(math.Cosh), "_tanh": m.unaryF64(math.Tanh),

		// Time.
		"_time": m.timeShim, "_gettimeofday": m.gettimeofday, "_clock": m.clock, "_difftime": m.difftime,
		"_localtime": m.localtime(false), "_localtime_r": m.localtime(true), "_gmtime": m.localtime(false),
		"_gmtime_r": m.localtime(true), "_mktime": m.mktime, "_timegm": m.mktime, "_tzset": retVoid,
		"_strftime": m.strftime, "_asctime": m.asctime, "_ctime": m.ctime, "_ctime_r": m.ctime,
		"_usleep": m.usleep, "_sleep": m.sleep, "_nanosleep": m.nanosleep,

		// Files.
		"_fopen": m.fopen, "_fdopen": m.fdopen, "_freopen": ret0, "_fclose": m.fclose, "_fread": m.fread,
		"_fwrite": m.fwrite, "_fseek": m.fseek, "_fseeko": m.fseeko, "_ftell": m.ftell, "_ftello": m.ftello,
		"_rewind": m.rewind, "_fflush": ret0, "_feof": m.feof, "_ferror": ret0, "_clearerr": retVoid,
		"_fgetc": m.fgetc, "_getc": m.fgetc, "_fgets": m.fgets, "_fputc": m.fputc, "_putc": m.fputc,
		"_fputs": m.fputs, "_ungetc": m.ungetc, "_fileno": m.fileno, "_setbuf": retVoid, "_setvbuf": ret0,
		"_fscanf": m.fscanf,
		"_open":   m.open, "_close": m.close, "_read": m.read, "_write": m.write, "_lseek": m.lseek,
		"_dup": m.dup, "_fcntl": ret0, "_flock": ret0, "_fsync": ret0, "_ioctl": retN(neg1()),
		"_stat": m.stat(false), "_lstat": m.stat(false), "_fstat": m.stat(true), "_statfs64": m.statfs,
		"_access": m.access, "_chdir": m.chdir, "_getcwd": m.getcwd, "_mkdir": m.mkdir, "_rmdir": m.unlink,
		"_unlink": m.unlink, "_remove": m.unlink, "_rename": m.rename, "_chmod": ret0, "_fchmod": ret0,
		"_futimes": ret0, "_utime": ret0, "_copyfile": retN(neg1()), "_realpath": m.realpath,
		"_scandir": m.scandir, "_alphasort": ret0, "_opendir": m.opendir, "_readdir": m.readdir, "_closedir": m.closedir,
		"_tmpnam": ret0, "_popen": ret0, "_pclose": retN(neg1()), "_shm_open": retN(neg1()),
		"_select": m.selectShim,

		// Network: none (the game runs offline).
		"_socket": m.enosys, "_gethostbyname": ret0, "_inet_addr": retN(neg1()),
	}
	for k, v := range m.shims {
		if v == nil {
			delete(m.shims, k)
		}
	}
	m.registerThreadShims()
	m.registerCxxShims()
	m.registerFrameworkShims()
	m.registerIOKitShims()
	m.registerSDLShims()
	m.registerPadShims()
	m.registerFontShims()
	m.registerSteamShims()
	m.registerAudioShims()
	m.registerOpenAL()
	m.registerNSModuleShims()
}

func ret0(c *cpu.CPU)    { ret(c, 0) }
func retVoid(c *cpu.CPU) { c.Ret() }
func retN(v uint32) cpu.ThunkFunc {
	return func(c *cpu.CPU) { ret(c, v) }
}

func (m *Machine) enosys(c *cpu.CPU) {
	m.setErrno(eNOSYS)
	ret(c, neg1())
}

// --- Process ---

func (m *Machine) exit(c *cpu.CPU) {
	code := c.Arg(0)
	m.logf("[process] exit(%d) from %s", int32(code), m.where(c.Read32(c.Regs[cpu.RegESP])))
	c.ExitCode = code
	if m.glr != nil {
		m.glr.ended()
	}
	m.publishGameState(true)
	c.Halted = true
	c.HaltReason = "exit(" + strconv.Itoa(int(int32(code))) + ")"
	m.stopped = true
}

func (m *Machine) abort(c *cpu.CPU) {
	m.logf("[process] abort; backtrace:\n  %s", strings.Join(m.Backtrace(24), "\n  "))
	c.Halted = true
	c.HaltReason = "abort()"
}

func (m *Machine) assertRtn(c *cpu.CPU) {
	m.logf("[process] assertion failed: %s (%s:%d in %s)", c.ReadCString(c.Arg(3)), c.ReadCString(c.Arg(1)), c.Arg(2), c.ReadCString(c.Arg(0)))
	m.abort(c)
}

func (m *Machine) atexitShim(c *cpu.CPU) {
	m.atexit = append(m.atexit, c.Arg(0))
	ret(c, 0)
}

func (m *Machine) cxaAtexit(c *cpu.CPU) { ret(c, 0) } // destructors at exit: not needed

func (m *Machine) getenv(c *cpu.CPU) {
	name := c.ReadCString(c.Arg(0))
	v, ok := m.env[name]
	if !ok {
		ret(c, 0)
		return
	}
	key := "env:" + name
	a, ok := m.sys[key]
	if !ok {
		a = c.Malloc(uint32(len(v)) + 1)
		c.WriteCString(a, v)
		m.sys[key] = a
	}
	ret(c, a)
}

func (m *Machine) setenv(c *cpu.CPU) {
	name, v := c.ReadCString(c.Arg(0)), c.ReadCString(c.Arg(1))
	if _, ok := m.env[name]; !ok || c.Arg(2) != 0 {
		m.env[name] = v
		delete(m.sys, "env:"+name)
	}
	ret(c, 0)
}

func (m *Machine) getExecutablePath(c *cpu.CPU) {
	buf, sizep := c.Arg(0), c.Arg(1)
	p := filepath.Join(m.Dir, "hl2_osx")
	if c.Read32(sizep) < uint32(len(p))+1 {
		c.Write32(sizep, uint32(len(p))+1)
		ret(c, neg1())
		return
	}
	c.WriteCString(buf, p)
	ret(c, 0)
}

func (m *Machine) nsGetArgc(c *cpu.CPU)    { ret(c, c.Read32(m.pvars+4)) }
func (m *Machine) nsGetArgv(c *cpu.CPU)    { ret(c, c.Read32(m.pvars+8)) }
func (m *Machine) nsGetEnviron(c *cpu.CPU) { ret(c, c.Read32(m.pvars+12)) }

func (m *Machine) setlocale(c *cpu.CPU) {
	a, ok := m.sys["locale:C"]
	if !ok {
		a = m.data(2)
		c.WriteCString(a, "C")
		m.sys["locale:C"] = a
	}
	ret(c, a)
}

func (m *Machine) getrlimit(c *cpu.CPU) {
	c.Write64(c.Arg(1), 2048)
	c.Write64(c.Arg(1)+8, 2048)
	ret(c, 0)
}

func (m *Machine) getrusage(c *cpu.CPU) {
	clear(c.WBytes(c.Arg(1), 72))
	us := m.micros()
	c.Write32(c.Arg(1), uint32(us/1e6))
	c.Write32(c.Arg(1)+4, uint32(us%1e6))
	ret(c, 0)
}

// sysctl answers the hardware questions engines ask.
func (m *Machine) sysctlValue(name string) (uint64, int, bool) {
	switch name {
	case "hw.ncpu", "hw.activecpu", "hw.physicalcpu", "hw.logicalcpu", "hw.availcpu":
		return 1, 4, true // one emulated CPU
	case "hw.memsize":
		return 4 << 30, 8, true
	case "hw.physmem", "hw.usermem":
		return 1 << 30, 4, true
	case "hw.cpufrequency", "hw.cpufrequency_max":
		return 2_400_000_000, 8, true
	case "hw.busfrequency":
		return 100_000_000, 8, true
	case "hw.cachelinesize":
		return 64, 4, true
	case "hw.l2cachesize":
		return 256 << 10, 4, true
	case "hw.l3cachesize":
		return 6 << 20, 4, true
	case "hw.pagesize":
		return 4096, 4, true
	case "hw.optional.sse", "hw.optional.sse2", "hw.optional.sse3", "hw.optional.mmx":
		return 1, 4, true
	case "hw.optional.supplementalsse3", "hw.optional.sse4_1", "hw.optional.sse4_2", "hw.optional.avx1_0", "hw.optional.x86_64":
		return 0, 4, true
	case "kern.maxfilesperproc":
		return 2048, 4, true
	}
	return 0, 0, false
}

var sysctlStrings = map[string]string{
	"kern.osrelease":           "19.6.0",
	"kern.ostype":              "Darwin",
	"kern.osversion":           "19H2",
	"hw.model":                 "MacBookPro11,5",
	"hw.machine":               "i386",
	"machdep.cpu.brand_string": "Intel(R) Core(TM) i7 CPU",
	"machdep.cpu.vendor":       "GenuineIntel",
}

func (m *Machine) sysctlOut(c *cpu.CPU, name string, oldp, oldlenp uint32) bool {
	if v, size, ok := m.sysctlValue(name); ok {
		if oldp != 0 && oldlenp != 0 {
			if size == 8 && c.Read32(oldlenp) >= 8 {
				c.Write64(oldp, v)
			} else {
				size = 4
				c.Write32(oldp, uint32(v))
			}
		}
		if oldlenp != 0 {
			c.Write32(oldlenp, uint32(size))
		}
		return true
	}
	if s, ok := sysctlStrings[name]; ok {
		if oldp != 0 && oldlenp != 0 {
			n := min(uint32(len(s))+1, c.Read32(oldlenp))
			copy(c.WBytes(oldp, n), s+"\x00")
		}
		if oldlenp != 0 {
			c.Write32(oldlenp, uint32(len(s))+1)
		}
		return true
	}
	m.logf("[sysctl] %s: unknown", name)
	return false
}

func (m *Machine) sysctlbyname(c *cpu.CPU) {
	if m.sysctlOut(c, c.ReadCString(c.Arg(0)), c.Arg(1), c.Arg(2)) {
		ret(c, 0)
		return
	}
	m.setErrno(2)
	ret(c, neg1())
}

func (m *Machine) sysctl(c *cpu.CPU) {
	mib, n := c.Arg(0), c.Arg(1)
	var name string
	if n >= 2 {
		a, b := c.Read32(mib), c.Read32(mib+4)
		name = map[[2]uint32]string{
			{6, 3}: "hw.ncpu", {6, 5}: "hw.physmem", {6, 24}: "hw.memsize", {6, 15}: "hw.cpufrequency",
			{6, 7}: "hw.pagesize", {6, 1}: "hw.machine", {6, 2}: "hw.model", {1, 2}: "kern.osrelease",
			{1, 1}: "kern.ostype", {1, 25}: "kern.osrelease",
		}[[2]uint32{a, b}]
		if name == "" {
			m.logf("[sysctl] mib %d.%d: unknown", a, b)
		}
	}
	if name != "" && m.sysctlOut(c, name, c.Arg(2), c.Arg(3)) {
		ret(c, 0)
		return
	}
	m.setErrno(2)
	ret(c, neg1())
}

func (m *Machine) gethostname(c *cpu.CPU) {
	copy(c.WBytes(c.Arg(0), min(c.Arg(1), 8)), "player\x00")
	ret(c, 0)
}

// jmp_buf: ebx, esi, edi, ebp, esp (after the call returns), eip (the return address).
func (m *Machine) setjmp(c *cpu.CPU) {
	buf := c.Arg(0)
	for i, r := range []int{cpu.RegEBX, cpu.RegESI, cpu.RegEDI, cpu.RegEBP} {
		c.Write32(buf+uint32(4*i), c.Regs[r])
	}
	c.Write32(buf+16, c.Regs[cpu.RegESP]+4)
	c.Write32(buf+20, c.Read32(c.Regs[cpu.RegESP]))
	ret(c, 0)
}

func (m *Machine) longjmp(c *cpu.CPU) {
	buf, v := c.Arg(0), c.Arg(1)
	if v == 0 {
		v = 1
	}
	for i, r := range []int{cpu.RegEBX, cpu.RegESI, cpu.RegEDI, cpu.RegEBP} {
		c.Regs[r] = c.Read32(buf + uint32(4*i))
	}
	c.Regs[cpu.RegESP] = c.Read32(buf + 16)
	c.EIP = c.Read32(buf + 20)
	c.Regs[cpu.RegEAX] = v
}

func (m *Machine) machAbsoluteTime(c *cpu.CPU) { ret64(c, m.micros()*1000) }

func (m *Machine) machTimebaseInfo(c *cpu.CPU) {
	c.Write32(c.Arg(0), 1)
	c.Write32(c.Arg(0)+4, 1)
	ret(c, 0)
}

func (m *Machine) hostPageSize(c *cpu.CPU) {
	c.Write32(c.Arg(1), 4096)
	ret(c, 0)
}

// OSAtomicCompareAndSwap*(old, new, ptr); the 64-bit form takes two-word values.
func (m *Machine) casN(size int) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		if size == 8 {
			old := uint64(c.Arg(0)) | uint64(c.Arg(1))<<32
			nv := uint64(c.Arg(2)) | uint64(c.Arg(3))<<32
			p := c.Arg(4)
			if c.Read64(p) == old {
				c.Write64(p, nv)
				ret(c, 1)
				return
			}
			ret(c, 0)
			return
		}
		old, nv, p := c.Arg(0), c.Arg(1), c.Arg(2)
		if c.Read32(p) == old {
			c.Write32(p, nv)
			ret(c, 1)
			return
		}
		ret(c, 0)
	}
}

func (m *Machine) atomicAdd(c *cpu.CPU) {
	v := c.Read32(c.Arg(1)) + c.Arg(0)
	c.Write32(c.Arg(1), v)
	ret(c, v)
}

func (m *Machine) atomicInc(d int32) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		v := c.Read32(c.Arg(0)) + uint32(d)
		c.Write32(c.Arg(0), v)
		ret(c, v)
	}
}

// --- Memory ---

func (m *Machine) mallocSize(c *cpu.CPU) { ret(c, c.AllocSize[c.Arg(0)]) }

// aligned returns memory aligned to align (a power of two) from the guest heap; the block's
// start is remembered so free() of the aligned pointer is a no-op rather than corruption.
func (m *Machine) aligned(c *cpu.CPU, size, align uint32) uint32 {
	if align <= 16 {
		return c.Malloc(size)
	}
	p := c.Malloc(size + align)
	if p == 0 {
		return 0
	}
	return (p + align - 1) &^ (align - 1)
}

func (m *Machine) valloc(c *cpu.CPU) { ret(c, m.aligned(c, c.Arg(0), 4096)) }

func (m *Machine) posixMemalign(c *cpu.CPU) {
	p := m.aligned(c, c.Arg(2), c.Arg(1))
	c.Write32(c.Arg(0), p)
	if p == 0 {
		ret(c, 12)
		return
	}
	ret(c, 0)
}

func (m *Machine) mmap(c *cpu.CPU) {
	length, flags, fd := c.Arg(1), c.Arg(3), int32(c.Arg(4))
	if flags&0x1000 == 0 && fd >= 0 { // a file mapping: read it into memory
		f := m.fds[fd]
		p := m.aligned(c, length, 4096)
		if f == nil || p == 0 {
			ret(c, neg1())
			return
		}
		off := int64(c.Arg(5)) | int64(c.Arg(6))<<32
		f.f.ReadAt(c.WBytes(p, length), off)
		ret(c, p)
		return
	}
	p := m.aligned(c, length, 4096)
	if p == 0 {
		ret(c, neg1())
		return
	}
	clear(c.WBytes(p, length))
	ret(c, p)
}

func (m *Machine) mstats(c *cpu.CPU) {
	// struct mstats { bytes_total, chunks_used, bytes_used, chunks_free, bytes_free } (returned in memory)
	p := c.Arg(0)
	c.Write32(p, uint32(c.HeapPeak()))
	c.Write32(p+4, uint32(len(c.AllocSize)))
	c.Write32(p+8, uint32(c.HeapInUse()))
	c.Write32(p+12, 0)
	c.Write32(p+16, 0)
	c.Regs[cpu.RegEAX] = p
	c.RetN(4) // struct return: the callee pops the hidden pointer
}

func (m *Machine) memcpyChk(c *cpu.CPU) {
	dst, src, n := c.Arg(0), c.Arg(1), c.Arg(2)
	copy(c.WBytes(dst, n), c.Bytes(src, n))
	ret(c, dst)
}

func (m *Machine) memsetChk(c *cpu.CPU) {
	dst, v, n := c.Arg(0), byte(c.Arg(1)), c.Arg(2)
	b := c.WBytes(dst, n)
	for i := range b {
		b[i] = v
	}
	ret(c, dst)
}

func (m *Machine) strcpyChk(c *cpu.CPU) {
	dst := c.Arg(0)
	c.WriteCString(dst, c.ReadCString(c.Arg(1)))
	ret(c, dst)
}

func (m *Machine) strcatChk(c *cpu.CPU) {
	dst := c.Arg(0)
	d := c.ReadCString(dst)
	c.WriteCString(dst+uint32(len(d)), c.ReadCString(c.Arg(1)))
	ret(c, dst)
}

func (m *Machine) strncpyChk(c *cpu.CPU) {
	dst, src, n := c.Arg(0), c.Arg(1), c.Arg(2)
	s := c.ReadCString(src)
	b := c.WBytes(dst, n)
	clear(b)
	copy(b, s)
	ret(c, dst)
}

// --- Strings ---

func (m *Machine) ctypeOf(r uint32) uint32 {
	if r < 256 {
		return ctype(int(r))
	}
	return 0
}

func (m *Machine) maskrune(c *cpu.CPU) { ret(c, m.ctypeOf(c.Arg(0))&c.Arg(1)) }

func (m *Machine) tolower(c *cpu.CPU) {
	r := c.Arg(0)
	if r >= 'A' && r <= 'Z' {
		r += 32
	}
	ret(c, r)
}

func (m *Machine) toupper(c *cpu.CPU) {
	r := c.Arg(0)
	if r >= 'a' && r <= 'z' {
		r -= 32
	}
	ret(c, r)
}

func (m *Machine) strtok(c *cpu.CPU) {
	s, delim := c.Arg(0), c.ReadCString(c.Arg(1))
	if s == 0 {
		s = m.strtokAt
	}
	if s == 0 {
		ret(c, 0)
		return
	}
	for c.Read8(s) != 0 && strings.IndexByte(delim, c.Read8(s)) >= 0 {
		s++
	}
	if c.Read8(s) == 0 {
		m.strtokAt = 0
		ret(c, 0)
		return
	}
	e := s
	for c.Read8(e) != 0 && strings.IndexByte(delim, c.Read8(e)) < 0 {
		e++
	}
	if c.Read8(e) != 0 {
		c.Write8(e, 0)
		m.strtokAt = e + 1
	} else {
		m.strtokAt = 0
	}
	ret(c, s)
}

func (m *Machine) strpbrk(c *cpu.CPU) {
	s, set := c.Arg(0), c.ReadCString(c.Arg(1))
	for ; c.Read8(s) != 0; s++ {
		if strings.IndexByte(set, c.Read8(s)) >= 0 {
			ret(c, s)
			return
		}
	}
	ret(c, 0)
}

func (m *Machine) strspn(c *cpu.CPU) {
	s, set := c.ReadCString(c.Arg(0)), c.ReadCString(c.Arg(1))
	n := 0
	for n < len(s) && strings.IndexByte(set, s[n]) >= 0 {
		n++
	}
	ret(c, uint32(n))
}

func (m *Machine) strcspn(c *cpu.CPU) {
	s, set := c.ReadCString(c.Arg(0)), c.ReadCString(c.Arg(1))
	n := 0
	for n < len(s) && strings.IndexByte(set, s[n]) < 0 {
		n++
	}
	ret(c, uint32(n))
}

func (m *Machine) strerror(c *cpu.CPU) {
	a, ok := m.sys["strerror"]
	if !ok {
		a = m.data(64)
		m.sys["strerror"] = a
	}
	c.WriteCString(a, "error "+strconv.Itoa(int(c.Arg(0))))
	ret(c, a)
}

// parseFloatPrefix parses the longest float at the start of s (after spaces), like strtod.
func parseFloatPrefix(s string) (float64, int) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] >= '\t' && s[i] <= '\r') {
		i++
	}
	best, bestN := 0.0, 0
	for j := len(s); j > i; j-- {
		if v, err := strconv.ParseFloat(s[i:j], 64); err == nil {
			return v, j
		} else if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return v, j
		}
		if j-i > 64 { // numbers are short: skip ahead
			j = i + 65
		}
	}
	return best, bestN
}

func (m *Machine) strtod(c *cpu.CPU) {
	s := c.ReadCString(c.Arg(0))
	v, n := parseFloatPrefix(s)
	if c.Arg(1) != 0 {
		c.Write32(c.Arg(1), c.Arg(0)+uint32(n))
	}
	retF(c, v)
}

func (m *Machine) strtoll(unsigned, atoll bool) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		s := c.ReadCString(c.Arg(0))
		base := 10
		if !atoll {
			base = int(int32(c.Arg(2)))
		}
		v, n := parseIntPrefix(s, base)
		if !atoll && c.Arg(1) != 0 {
			c.Write32(c.Arg(1), c.Arg(0)+uint32(n))
		}
		_ = unsigned
		ret64(c, uint64(v))
	}
}

// parseIntPrefix parses an integer at the start of s like strtoll (base 0: auto).
func parseIntPrefix(s string, base int) (int64, int) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] >= '\t' && s[i] <= '\r') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	if (base == 0 || base == 16) && i+1 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') {
		i += 2
		base = 16
	} else if base == 0 && i < len(s) && s[i] == '0' {
		base = 8
	} else if base == 0 {
		base = 10
	}
	start := i
	var v uint64
	for i < len(s) {
		d := digitVal(s[i])
		if d < 0 || d >= base {
			break
		}
		v = v*uint64(base) + uint64(d)
		i++
	}
	if i == start {
		return 0, 0
	}
	if neg {
		return -int64(v), i
	}
	return int64(v), i
}

func digitVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'z':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'Z':
		return int(b-'A') + 10
	}
	return -1
}

func (m *Machine) strnlen(c *cpu.CPU) {
	p, n := c.Arg(0), c.Arg(1)
	i := uint32(0)
	for i < n && c.Read8(p+i) != 0 {
		i++
	}
	ret(c, i)
}

func (m *Machine) strlcpy(c *cpu.CPU) {
	dst, s, n := c.Arg(0), c.ReadCString(c.Arg(1)), c.Arg(2)
	if n > 0 {
		k := min(uint32(len(s)), n-1)
		copy(c.WBytes(dst, k), s)
		c.Write8(dst+k, 0)
	}
	ret(c, uint32(len(s)))
}

func (m *Machine) strlcat(c *cpu.CPU) {
	dst, s, n := c.Arg(0), c.ReadCString(c.Arg(1)), c.Arg(2)
	d := uint32(len(c.ReadCString(dst)))
	if d < n {
		k := min(uint32(len(s)), n-d-1)
		copy(c.WBytes(dst+d, k), s)
		c.Write8(dst+d+k, 0)
	}
	ret(c, d+uint32(len(s)))
}

func (m *Machine) stpcpy(c *cpu.CPU) {
	s := c.ReadCString(c.Arg(1))
	c.WriteCString(c.Arg(0), s)
	ret(c, c.Arg(0)+uint32(len(s)))
}

// Wide strings: wchar_t is 32 bits on the Mac.

func (m *Machine) readW(p uint32) []rune {
	var r []rune
	for p != 0 {
		v := m.C.Read32(p)
		if v == 0 {
			break
		}
		r = append(r, rune(v))
		p += 4
	}
	return r
}

func (m *Machine) writeW(p uint32, r []rune, max uint32) uint32 {
	n := uint32(len(r))
	if max > 0 && n > max-1 {
		n = max - 1
	}
	for i := uint32(0); i < n; i++ {
		m.C.Write32(p+4*i, uint32(r[i]))
	}
	m.C.Write32(p+4*n, 0)
	return n
}

func (m *Machine) mbstowcs(c *cpu.CPU) {
	s := []rune(c.ReadCString(c.Arg(1)))
	if c.Arg(0) == 0 {
		ret(c, uint32(len(s)))
		return
	}
	n := min(uint32(len(s)), c.Arg(2))
	for i := uint32(0); i < n; i++ {
		c.Write32(c.Arg(0)+4*i, uint32(s[i]))
	}
	if n < c.Arg(2) {
		c.Write32(c.Arg(0)+4*n, 0)
	}
	ret(c, n)
}

func (m *Machine) wcstombs(c *cpu.CPU) {
	s := string(m.readW(c.Arg(1)))
	if c.Arg(0) == 0 {
		ret(c, uint32(len(s)))
		return
	}
	n := min(uint32(len(s)), c.Arg(2))
	copy(c.WBytes(c.Arg(0), n), s)
	if n < c.Arg(2) {
		c.Write8(c.Arg(0)+n, 0)
	}
	ret(c, n)
}

func (m *Machine) wcslen(c *cpu.CPU) { ret(c, uint32(len(m.readW(c.Arg(0))))) }

func (m *Machine) wcscpy(c *cpu.CPU) {
	m.writeW(c.Arg(0), m.readW(c.Arg(1)), 0)
	ret(c, c.Arg(0))
}

func (m *Machine) wcsncpy(c *cpu.CPU) {
	r, n := m.readW(c.Arg(1)), c.Arg(2)
	for i := uint32(0); i < n; i++ {
		v := uint32(0)
		if int(i) < len(r) {
			v = uint32(r[i])
		}
		c.Write32(c.Arg(0)+4*i, v)
	}
	ret(c, c.Arg(0))
}

func (m *Machine) wcscat(c *cpu.CPU) {
	d := m.readW(c.Arg(0))
	m.writeW(c.Arg(0)+uint32(4*len(d)), m.readW(c.Arg(1)), 0)
	ret(c, c.Arg(0))
}

func (m *Machine) wcsncat(c *cpu.CPU) {
	d, s := m.readW(c.Arg(0)), m.readW(c.Arg(1))
	if uint32(len(s)) > c.Arg(2) {
		s = s[:c.Arg(2)]
	}
	m.writeW(c.Arg(0)+uint32(4*len(d)), s, 0)
	ret(c, c.Arg(0))
}

func (m *Machine) wcscmp(c *cpu.CPU) {
	ret(c, uint32(int32(strings.Compare(string(m.readW(c.Arg(0))), string(m.readW(c.Arg(1)))))))
}

func (m *Machine) wcsncmp(c *cpu.CPU) {
	a, b, n := m.readW(c.Arg(0)), m.readW(c.Arg(1)), int(c.Arg(2))
	if len(a) > n {
		a = a[:n]
	}
	if len(b) > n {
		b = b[:n]
	}
	ret(c, uint32(int32(strings.Compare(string(a), string(b)))))
}

func (m *Machine) wcschr(c *cpu.CPU) {
	p, ch := c.Arg(0), c.Arg(1)
	for {
		v := c.Read32(p)
		if v == ch {
			ret(c, p)
			return
		}
		if v == 0 {
			ret(c, 0)
			return
		}
		p += 4
	}
}

func (m *Machine) wcsstr(c *cpu.CPU) {
	h, n := m.readW(c.Arg(0)), m.readW(c.Arg(1))
	i := strings.Index(string(h), string(n))
	if i < 0 {
		ret(c, 0)
		return
	}
	ret(c, c.Arg(0)+4*uint32(len([]rune(string(h)[:i]))))
}

func (m *Machine) wcstol(c *cpu.CPU) {
	s := string(m.readW(c.Arg(0)))
	v, n := parseIntPrefix(s, int(int32(c.Arg(2))))
	if c.Arg(1) != 0 {
		c.Write32(c.Arg(1), c.Arg(0)+4*uint32(len([]rune(s[:n]))))
	}
	ret64(c, uint64(v))
}

func (m *Machine) wcstod(c *cpu.CPU) {
	s := string(m.readW(c.Arg(0)))
	v, n := parseFloatPrefix(s)
	if c.Arg(1) != 0 {
		c.Write32(c.Arg(1), c.Arg(0)+4*uint32(len([]rune(s[:n]))))
	}
	retF(c, v)
}

// formatW formats a wide format string: converted to narrow, with %s/%ls taking wide strings
// (Source's convention on the Mac: %s in a wide format is a wide string).
func (m *Machine) formatW(c *cpu.CPU, format []rune, va uint32) []rune {
	f := string(format)
	f = strings.ReplaceAll(f, "%ls", "%S")
	var out strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			out.WriteByte(f[i])
			continue
		}
		j := i + 1
		for j < len(f) && strings.IndexByte("-+ #0123456789.lhqjzt", f[j]) >= 0 {
			j++
		}
		if j >= len(f) {
			break
		}
		spec := f[i : j+1]
		switch f[j] {
		case 's', 'S':
			out.WriteString(string(m.readW(c.Read32(va))))
			va += 4
		case 'c', 'C':
			out.WriteRune(rune(c.Read32(va)))
			va += 4
		default:
			// One argument through the narrow formatter.
			tmp := c.Malloc(16)
			n := uint32(4)
			if strings.ContainsAny(f[j:j+1], "eEfgGaA") || strings.Contains(spec, "ll") || strings.Contains(spec, "q") {
				n = 8
			}
			if f[j] == '%' {
				n = 0
			}
			copy(c.WBytes(tmp, n), c.Bytes(va, n))
			out.WriteString(crt.Format(c, spec, tmp))
			c.Free(tmp)
			va += n
		}
		i = j
	}
	return []rune(out.String())
}

func (m *Machine) swprintf(c *cpu.CPU) {
	r := m.formatW(c, m.readW(c.Arg(2)), c.Regs[cpu.RegESP]+16)
	ret(c, m.writeW(c.Arg(0), r, c.Arg(1)))
}

func (m *Machine) vswprintf(c *cpu.CPU) {
	r := m.formatW(c, m.readW(c.Arg(2)), c.Arg(3))
	ret(c, m.writeW(c.Arg(0), r, c.Arg(1)))
}

func (m *Machine) swscanf(c *cpu.CPU) {
	in, format := m.readW(c.Arg(0)), m.readW(c.Arg(1))
	ret(c, uint32(m.scan(c, in, format, c.Regs[cpu.RegESP]+12, true)))
}

// scan is scanf over runes: whitespace, literals and %d %i %u %x %o %f %e %g %s %c %n %% (with
// widths, '*' and the h/l/ll sizes); values go to the pointers in the guest varargs at va.
// It returns the number of values stored.
func (m *Machine) scan(c *cpu.CPU, in, f []rune, va uint32, wide bool) int {
	pos, n := 0, 0
	space := func() {
		for pos < len(in) && unicode.IsSpace(in[pos]) {
			pos++
		}
	}
	for i := 0; i < len(f); i++ {
		switch {
		case unicode.IsSpace(f[i]):
			space()
			continue
		case f[i] != '%':
			if pos >= len(in) || in[pos] != f[i] {
				return n
			}
			pos++
			continue
		}
		i++
		if i < len(f) && f[i] == '%' {
			if pos >= len(in) || in[pos] != '%' {
				return n
			}
			pos++
			continue
		}
		skip := i < len(f) && f[i] == '*'
		if skip {
			i++
		}
		width := 0
		for i < len(f) && f[i] >= '0' && f[i] <= '9' {
			width = width*10 + int(f[i]-'0')
			i++
		}
		size := ""
		for i < len(f) && strings.ContainsRune("hlLqjzt", f[i]) {
			size += string(f[i])
			i++
		}
		if i >= len(f) {
			return n
		}
		conv := f[i]
		if conv != 'c' && conv != 'n' {
			space()
		}
		end := len(in)
		if width > 0 && pos+width < end {
			end = pos + width
		}
		dst := func() uint32 { a := c.Read32(va); va += 4; return a }
		switch conv {
		case 'd', 'i', 'u', 'x', 'X', 'o':
			base := map[rune]int{'d': 10, 'u': 10, 'i': 0, 'x': 16, 'X': 16, 'o': 8}[conv]
			v, used := parseIntPrefix(string(in[pos:end]), base)
			if used == 0 {
				return n
			}
			pos += len([]rune(string(in[pos:end])[:used]))
			if !skip {
				a := dst()
				switch size {
				case "hh":
					c.Write8(a, uint8(v))
				case "h":
					c.Write16(a, uint16(v))
				case "ll", "q", "j":
					c.Write64(a, uint64(v))
				default:
					c.Write32(a, uint32(v))
				}
				n++
			}
		case 'f', 'e', 'g', 'E', 'G', 'a':
			v, used := parseFloatPrefix(string(in[pos:end]))
			if used == 0 {
				return n
			}
			pos += len([]rune(string(in[pos:end])[:used]))
			if !skip {
				if a := dst(); size == "l" || size == "L" {
					c.WriteF64(a, v)
				} else {
					c.WriteF32(a, float32(v))
				}
				n++
			}
		case 's', 'S':
			start := pos
			for pos < end && !unicode.IsSpace(in[pos]) {
				pos++
			}
			if pos == start {
				return n
			}
			if !skip {
				a := dst()
				if (wide && size != "h") || size == "l" || conv == 'S' {
					m.writeW(a, in[start:pos], 0)
				} else {
					c.WriteCString(a, string(in[start:pos]))
				}
				n++
			}
		case 'c':
			k := max(width, 1)
			if pos+k > len(in) {
				return n
			}
			if !skip {
				a := dst()
				for j := 0; j < k; j++ {
					if (wide && size != "h") || size == "l" {
						c.Write32(a+uint32(4*j), uint32(in[pos+j]))
					} else {
						c.Write8(a+uint32(j), uint8(in[pos+j]))
					}
				}
				n++
			}
			pos += k
		case 'n':
			if !skip {
				c.Write32(dst(), uint32(pos))
			}
		default:
			return n
		}
	}
	return n
}

// --- Maths ---

func (m *Machine) modf(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	ip, frac := math.Modf(x)
	c.WriteF64(c.Arg(2), ip)
	retF(c, frac)
}

func (m *Machine) finite(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	if math.IsInf(x, 0) || math.IsNaN(x) {
		ret(c, 0)
		return
	}
	ret(c, 1)
}

func (m *Machine) isnanf(c *cpu.CPU) {
	if math.IsNaN(float64(c.ArgF32(0))) {
		ret(c, 1)
		return
	}
	ret(c, 0)
}

func (m *Machine) fminmaxf(fn func(a, b float64) float64) cpu.ThunkFunc {
	return func(c *cpu.CPU) { retF(c, float64(float32(fn(float64(c.ArgF32(0)), float64(c.ArgF32(1)))))) }
}

func (m *Machine) unaryF64(fn func(float64) float64) cpu.ThunkFunc {
	return func(c *cpu.CPU) { retF(c, fn(c.ReadF64(c.Regs[cpu.RegESP]+4))) }
}

func (m *Machine) binaryF64(fn func(a, b float64) float64) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		retF(c, fn(c.ReadF64(c.Regs[cpu.RegESP]+4), c.ReadF64(c.Regs[cpu.RegESP]+12)))
	}
}

func (m *Machine) fixunsdfdi(c *cpu.CPU) {
	x := c.ReadF64(c.Regs[cpu.RegESP] + 4)
	switch {
	case x <= 0 || math.IsNaN(x):
		ret64(c, 0)
	case x >= 1<<64:
		ret64(c, math.MaxUint64)
	default:
		ret64(c, uint64(x))
	}
}

func (m *Machine) lround(c *cpu.CPU) {
	ret(c, uint32(int32(math.Round(c.ReadF64(c.Regs[cpu.RegESP]+4)))))
}
func (m *Machine) lroundf(c *cpu.CPU) { ret(c, uint32(int32(math.Round(float64(c.ArgF32(0)))))) }

// --- Time ---

func (m *Machine) timeShim(c *cpu.CPU) {
	t := uint32(m.now().Unix())
	if c.Arg(0) != 0 {
		c.Write32(c.Arg(0), t)
	}
	ret(c, t)
}

func (m *Machine) gettimeofday(c *cpu.CPU) {
	if p := c.Arg(0); p != 0 {
		t := m.now()
		c.Write32(p, uint32(t.Unix()))
		c.Write32(p+4, uint32(t.Nanosecond()/1000))
	}
	ret(c, 0)
}

func (m *Machine) clock(c *cpu.CPU) { ret(c, uint32(m.micros())) } // CLOCKS_PER_SEC = 1000000

func (m *Machine) difftime(c *cpu.CPU) { retF(c, float64(int32(c.Arg(0))-int32(c.Arg(1)))) }

// struct tm: sec min hour mday mon year wday yday isdst gmtoff zone (44 bytes)
func (m *Machine) writeTM(p uint32, t time.Time) {
	c := m.C
	for i, v := range []int{t.Second(), t.Minute(), t.Hour(), t.Day(), int(t.Month()) - 1, t.Year() - 1900, int(t.Weekday()), t.YearDay() - 1, 0, 0} {
		c.Write32(p+uint32(4*i), uint32(int32(v)))
	}
	c.Write32(p+40, 0)
}

func (m *Machine) readTM(p uint32) time.Time {
	c := m.C
	g := func(i int) int { return int(int32(c.Read32(p + uint32(4*i)))) }
	return time.Date(g(5)+1900, time.Month(g(4)+1), g(3), g(2), g(1), g(0), 0, time.UTC)
}

func (m *Machine) localtime(reentrant bool) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		t := time.Unix(int64(int32(c.Read32(c.Arg(0)))), 0).UTC()
		p := c.Arg(1)
		if !reentrant {
			a, ok := m.sys["tm"]
			if !ok {
				a = m.data(44)
				m.sys["tm"] = a
			}
			p = a
		}
		m.writeTM(p, t)
		ret(c, p)
	}
}

func (m *Machine) mktime(c *cpu.CPU) { ret(c, uint32(m.readTM(c.Arg(0)).Unix())) }

func (m *Machine) strftime(c *cpu.CPU) {
	buf, max, f, t := c.Arg(0), c.Arg(1), c.ReadCString(c.Arg(2)), m.readTM(c.Arg(3))
	r := strings.NewReplacer("%Y", t.Format("2006"), "%m", t.Format("01"), "%d", t.Format("02"),
		"%H", t.Format("15"), "%M", t.Format("04"), "%S", t.Format("05"), "%y", t.Format("06"),
		"%b", t.Format("Jan"), "%a", t.Format("Mon"), "%B", t.Format("January"), "%A", t.Format("Monday"),
		"%p", t.Format("PM"), "%I", t.Format("03"), "%j", strconv.Itoa(t.YearDay()), "%%", "%",
		"%c", t.Format("Mon Jan  2 15:04:05 2006"), "%x", t.Format("01/02/06"), "%X", t.Format("15:04:05"))
	s := r.Replace(f)
	if uint32(len(s))+1 > max {
		ret(c, 0)
		return
	}
	c.WriteCString(buf, s)
	ret(c, uint32(len(s)))
}

func (m *Machine) asctimeString(t time.Time) uint32 {
	a, ok := m.sys["asctime"]
	if !ok {
		a = m.data(32)
		m.sys["asctime"] = a
	}
	m.C.WriteCString(a, t.Format("Mon Jan  2 15:04:05 2006")+"\n")
	return a
}

func (m *Machine) asctime(c *cpu.CPU) { ret(c, m.asctimeString(m.readTM(c.Arg(0)))) }

func (m *Machine) ctime(c *cpu.CPU) {
	ret(c, m.asctimeString(time.Unix(int64(int32(c.Read32(c.Arg(0)))), 0).UTC()))
}

func (m *Machine) usleep(c *cpu.CPU) {
	us := c.Arg(0)
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
	m.sleepFor(uint64(us))
}

func (m *Machine) sleep(c *cpu.CPU) {
	s := c.Arg(0)
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
	m.sleepFor(uint64(s) * 1e6)
}

func (m *Machine) nanosleep(c *cpu.CPU) {
	p := c.Arg(0)
	us := uint64(c.Read32(p))*1e6 + uint64(c.Read32(p+4))/1000
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
	m.sleepFor(us)
}

// --- Files ---

type gfile struct {
	f      *os.File
	path   string
	eof    bool
	unget  int
	fd     int32
	stream uint32 // FILE* if opened through stdio
	std    io.Writer
	dirBuf []byte
}

const fileSize = 88 // sizeof(FILE) on i386

func (m *Machine) initStdio() {}

// stdFile returns the FILE* of stdin, stdout or stderr.
func (m *Machine) stdFile(sym string) uint32 {
	p := m.data(fileSize)
	g := &gfile{fd: map[string]int32{"___stdinp": 0, "___stdoutp": 1, "___stderrp": 2}[sym], stream: p, unget: -1}
	if g.fd > 0 {
		g.std = os.Stderr
		if m.Log != nil {
			g.std = m.Log
		}
	}
	m.files[p] = g
	m.fds[g.fd] = g
	m.C.Write16(p+12, uint16(g.fd)) // _file
	return p
}

func openFlags(mode string) (int, bool) {
	plus := strings.Contains(mode, "+")
	switch mode[0] {
	case 'w':
		if plus {
			return os.O_RDWR | os.O_CREATE | os.O_TRUNC, true
		}
		return os.O_WRONLY | os.O_CREATE | os.O_TRUNC, true
	case 'a':
		if plus {
			return os.O_RDWR | os.O_CREATE | os.O_APPEND, true
		}
		return os.O_WRONLY | os.O_CREATE | os.O_APPEND, true
	}
	if plus {
		return os.O_RDWR, true
	}
	return os.O_RDONLY, false
}

func (m *Machine) openFile(path string, flags int, write bool) (*gfile, error) {
	hp := m.hostPath(path, write)
	f, err := os.OpenFile(hp, flags, 0o644)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.IsDir() && write {
		f.Close()
		return nil, os.ErrPermission
	}
	g := &gfile{f: f, path: hp, unget: -1, fd: m.nextFD}
	m.nextFD++
	m.fds[g.fd] = g
	return g, nil
}

func (m *Machine) fopen(c *cpu.CPU) {
	path, mode := c.ReadCString(c.Arg(0)), c.ReadCString(c.Arg(1))
	if mode == "" {
		ret(c, 0)
		return
	}
	flags, write := openFlags(mode)
	g, err := m.openFile(path, flags, write)
	if err != nil {
		m.setErrno(errnoOf(err))
		ret(c, 0)
		return
	}
	g.stream = c.Malloc(fileSize)
	clear(c.WBytes(g.stream, fileSize))
	c.Write16(g.stream+12, uint16(g.fd))
	m.files[g.stream] = g
	ret(c, g.stream)
}

func (m *Machine) fdopen(c *cpu.CPU) {
	g := m.fds[int32(c.Arg(0))]
	if g == nil {
		ret(c, 0)
		return
	}
	if g.stream == 0 {
		g.stream = c.Malloc(fileSize)
		clear(c.WBytes(g.stream, fileSize))
		c.Write16(g.stream+12, uint16(g.fd))
		m.files[g.stream] = g
	}
	ret(c, g.stream)
}

func (m *Machine) fclose(c *cpu.CPU) {
	p := c.Arg(0)
	g := m.files[p]
	if g == nil {
		ret(c, neg1())
		return
	}
	if g.f != nil {
		g.f.Close()
		delete(m.fds, g.fd)
		delete(m.files, p)
		c.Free(p)
	}
	ret(c, 0)
}

func (g *gfile) readInto(b []byte) int {
	n := 0
	if g.unget >= 0 && len(b) > 0 {
		b[0] = byte(g.unget)
		g.unget = -1
		n = 1
	}
	if g.f == nil {
		return n
	}
	k, err := io.ReadFull(g.f, b[n:])
	if err != nil {
		g.eof = true
	}
	return n + k
}

func (g *gfile) write(b []byte) int {
	if g.std != nil {
		g.std.Write(b)
		return len(b)
	}
	if g.f == nil {
		return 0
	}
	n, _ := g.f.Write(b)
	return n
}

func (m *Machine) fread(c *cpu.CPU) {
	buf, size, n, g := c.Arg(0), c.Arg(1), c.Arg(2), m.files[c.Arg(3)]
	if g == nil || size == 0 {
		ret(c, 0)
		return
	}
	total := uint64(size) * uint64(n)
	if total > 1<<31 {
		ret(c, 0)
		return
	}
	got := g.readInto(c.WBytes(buf, uint32(total)))
	ret(c, uint32(got)/size)
}

func (m *Machine) fwrite(c *cpu.CPU) {
	buf, size, n, g := c.Arg(0), c.Arg(1), c.Arg(2), m.files[c.Arg(3)]
	if g == nil || size == 0 {
		ret(c, 0)
		return
	}
	w := g.write(c.Bytes(buf, size*n))
	ret(c, uint32(w)/size)
}

func (m *Machine) seek(g *gfile, off int64, whence int) (int64, bool) {
	if g == nil || g.f == nil {
		return 0, false
	}
	g.unget, g.eof = -1, false
	pos, err := g.f.Seek(off, whence)
	return pos, err == nil
}

func (m *Machine) fseek(c *cpu.CPU) {
	if _, ok := m.seek(m.files[c.Arg(0)], int64(int32(c.Arg(1))), int(c.Arg(2))); ok {
		ret(c, 0)
		return
	}
	ret(c, neg1())
}

func (m *Machine) fseeko(c *cpu.CPU) {
	off := int64(uint64(c.Arg(1)) | uint64(c.Arg(2))<<32)
	if _, ok := m.seek(m.files[c.Arg(0)], off, int(c.Arg(3))); ok {
		ret(c, 0)
		return
	}
	ret(c, neg1())
}

func (m *Machine) tell(g *gfile) int64 {
	if g == nil || g.f == nil {
		return -1
	}
	pos, _ := g.f.Seek(0, io.SeekCurrent)
	if g.unget >= 0 {
		pos--
	}
	return pos
}

func (m *Machine) ftell(c *cpu.CPU)  { ret(c, uint32(m.tell(m.files[c.Arg(0)]))) }
func (m *Machine) ftello(c *cpu.CPU) { ret64(c, uint64(m.tell(m.files[c.Arg(0)]))) }

func (m *Machine) rewind(c *cpu.CPU) {
	m.seek(m.files[c.Arg(0)], 0, 0)
	c.Ret()
}

func (m *Machine) feof(c *cpu.CPU) {
	if g := m.files[c.Arg(0)]; g != nil && g.eof {
		ret(c, 1)
		return
	}
	ret(c, 0)
}

func (m *Machine) fgetc(c *cpu.CPU) {
	g := m.files[c.Arg(0)]
	var b [1]byte
	if g == nil || g.readInto(b[:]) == 0 {
		ret(c, neg1())
		return
	}
	ret(c, uint32(b[0]))
}

func (m *Machine) ungetc(c *cpu.CPU) {
	if g := m.files[c.Arg(1)]; g != nil && int32(c.Arg(0)) >= 0 {
		g.unget, g.eof = int(c.Arg(0)&0xff), false
		ret(c, c.Arg(0)&0xff)
		return
	}
	ret(c, neg1())
}

func (m *Machine) fgets(c *cpu.CPU) {
	buf, n, g := c.Arg(0), int(int32(c.Arg(1))), m.files[c.Arg(2)]
	if g == nil || n <= 0 {
		ret(c, 0)
		return
	}
	var line []byte
	var b [1]byte
	for len(line) < n-1 {
		if g.readInto(b[:]) == 0 {
			break
		}
		line = append(line, b[0])
		if b[0] == '\n' {
			break
		}
	}
	if len(line) == 0 {
		ret(c, 0)
		return
	}
	copy(c.WBytes(buf, uint32(len(line))), line)
	c.Write8(buf+uint32(len(line)), 0)
	ret(c, buf)
}

func (m *Machine) fputc(c *cpu.CPU) {
	if g := m.files[c.Arg(1)]; g != nil {
		g.write([]byte{byte(c.Arg(0))})
	}
	ret(c, c.Arg(0)&0xff)
}

func (m *Machine) fputs(c *cpu.CPU) {
	if g := m.files[c.Arg(1)]; g != nil {
		g.write([]byte(c.ReadCString(c.Arg(0))))
	}
	ret(c, 0)
}

func (m *Machine) puts(c *cpu.CPU) {
	m.logf("[stdout] %s", c.ReadCString(c.Arg(0)))
	ret(c, 1)
}

func (m *Machine) putchar(c *cpu.CPU) {
	if m.Log != nil {
		m.Log.Write([]byte{byte(c.Arg(0))})
	}
	ret(c, c.Arg(0))
}

func (m *Machine) fprintf(c *cpu.CPU) {
	s := crt.Format(c, c.ReadCString(c.Arg(1)), c.Regs[cpu.RegESP]+12)
	if g := m.files[c.Arg(0)]; g != nil {
		g.write([]byte(s))
	}
	ret(c, uint32(len(s)))
}

func (m *Machine) vfprintf(c *cpu.CPU) {
	s := crt.Format(c, c.ReadCString(c.Arg(1)), c.Arg(2))
	if g := m.files[c.Arg(0)]; g != nil {
		g.write([]byte(s))
	}
	ret(c, uint32(len(s)))
}

func (m *Machine) fscanf(c *cpu.CPU) {
	m.logf("[libc] fscanf(%q): not implemented", c.ReadCString(c.Arg(1)))
	ret(c, neg1())
}

func (m *Machine) fileno(c *cpu.CPU) {
	if g := m.files[c.Arg(0)]; g != nil {
		ret(c, uint32(g.fd))
		return
	}
	ret(c, neg1())
}

// open(2) flags (Darwin).
func posixFlags(f uint32) (int, bool) {
	var fl int
	switch f & 3 {
	case 0:
		fl = os.O_RDONLY
	case 1:
		fl = os.O_WRONLY
	default:
		fl = os.O_RDWR
	}
	if f&0x8 != 0 {
		fl |= os.O_APPEND
	}
	if f&0x200 != 0 {
		fl |= os.O_CREATE
	}
	if f&0x400 != 0 {
		fl |= os.O_TRUNC
	}
	if f&0x800 != 0 {
		fl |= os.O_EXCL
	}
	return fl, f&3 != 0 || f&0x200 != 0
}

func (m *Machine) open(c *cpu.CPU) {
	fl, write := posixFlags(c.Arg(1))
	g, err := m.openFile(c.ReadCString(c.Arg(0)), fl, write)
	if err != nil {
		m.setErrno(errnoOf(err))
		ret(c, neg1())
		return
	}
	ret(c, uint32(g.fd))
}

func (m *Machine) close(c *cpu.CPU) {
	fd := int32(c.Arg(0))
	if g := m.fds[fd]; g != nil && g.f != nil {
		if g.stream == 0 {
			g.f.Close()
		}
		delete(m.fds, fd)
		ret(c, 0)
		return
	}
	ret(c, neg1())
}

func (m *Machine) read(c *cpu.CPU) {
	g := m.fds[int32(c.Arg(0))]
	if g == nil {
		m.setErrno(eBADF)
		ret(c, neg1())
		return
	}
	ret(c, uint32(g.readInto(c.WBytes(c.Arg(1), c.Arg(2)))))
}

func (m *Machine) write(c *cpu.CPU) {
	g := m.fds[int32(c.Arg(0))]
	if g == nil {
		m.setErrno(eBADF)
		ret(c, neg1())
		return
	}
	ret(c, uint32(g.write(c.Bytes(c.Arg(1), c.Arg(2)))))
}

func (m *Machine) lseek(c *cpu.CPU) {
	off := int64(uint64(c.Arg(1)) | uint64(c.Arg(2))<<32)
	pos, ok := m.seek(m.fds[int32(c.Arg(0))], off, int(c.Arg(3)))
	if !ok {
		ret64(c, math.MaxUint64)
		return
	}
	ret64(c, uint64(pos))
}

func (m *Machine) dup(c *cpu.CPU) {
	g := m.fds[int32(c.Arg(0))]
	if g == nil {
		ret(c, neg1())
		return
	}
	h := *g
	h.fd, h.stream = m.nextFD, 0
	m.nextFD++
	m.fds[h.fd] = &h
	ret(c, uint32(h.fd))
}

// writeStat fills a struct stat (64-bit inode layout, 108 bytes).
func (m *Machine) writeStat(p uint32, st os.FileInfo) {
	c := m.C
	clear(c.WBytes(p, 108))
	mode := uint16(st.Mode().Perm())
	if st.IsDir() {
		mode |= 0o040000
	} else {
		mode |= 0o100000
	}
	c.Write32(p, 0x1000004)
	c.Write16(p+4, mode)
	c.Write16(p+6, 1)
	c.Write64(p+8, uint64(len(st.Name()))*7919+uint64(st.Size())) // any stable inode number
	c.Write32(p+16, 501)
	c.Write32(p+20, 20)
	mt := uint32(st.ModTime().Unix())
	for _, off := range []uint32{28, 36, 44, 52} {
		c.Write32(p+off, mt)
	}
	c.Write64(p+60, uint64(st.Size()))
	c.Write64(p+68, uint64(st.Size()+511)/512)
	c.Write32(p+76, 4096)
}

func (m *Machine) stat(byFD bool) cpu.ThunkFunc {
	return func(c *cpu.CPU) {
		var st os.FileInfo
		var err error
		if byFD {
			g := m.fds[int32(c.Arg(0))]
			if g == nil || g.f == nil {
				m.setErrno(eBADF)
				ret(c, neg1())
				return
			}
			st, err = g.f.Stat()
		} else {
			st, err = os.Stat(m.hostPath(c.ReadCString(c.Arg(0)), false))
		}
		if err != nil {
			m.setErrno(errnoOf(err))
			ret(c, neg1())
			return
		}
		m.writeStat(c.Arg(1), st)
		ret(c, 0)
	}
}

func (m *Machine) statfs(c *cpu.CPU) {
	// struct statfs64: bsize, iosize, blocks(8), bfree(8), bavail(8), files(8), ffree(8)...
	p := c.Arg(1)
	clear(c.WBytes(p, 2168))
	c.Write32(p, 4096)
	c.Write32(p+4, 4096)
	c.Write64(p+8, 1<<26)
	c.Write64(p+16, 1<<25)
	c.Write64(p+24, 1<<25)
	ret(c, 0)
}

func (m *Machine) access(c *cpu.CPU) {
	if _, err := os.Stat(m.hostPath(c.ReadCString(c.Arg(0)), false)); err != nil {
		m.setErrno(errnoOf(err))
		ret(c, neg1())
		return
	}
	ret(c, 0)
}

func (m *Machine) chdir(c *cpu.CPU) {
	p := c.ReadCString(c.Arg(0))
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.cwd, p)
	}
	if st, err := os.Stat(m.hostPath(p, false)); err != nil || !st.IsDir() {
		m.setErrno(eNOENT)
		ret(c, neg1())
		return
	}
	m.cwd = filepath.Clean(p)
	ret(c, 0)
}

func (m *Machine) getcwd(c *cpu.CPU) {
	buf, n := c.Arg(0), c.Arg(1)
	if buf == 0 {
		buf = c.Malloc(uint32(len(m.cwd)) + 1)
	} else if uint32(len(m.cwd))+1 > n {
		m.setErrno(34)
		ret(c, 0)
		return
	}
	c.WriteCString(buf, m.cwd)
	ret(c, buf)
}

func (m *Machine) mkdir(c *cpu.CPU) {
	hp := m.hostPath(c.ReadCString(c.Arg(0)), true)
	if err := os.Mkdir(hp, 0o755); err != nil {
		m.setErrno(errnoOf(err))
		ret(c, neg1())
		return
	}
	ret(c, 0)
}

func (m *Machine) unlink(c *cpu.CPU) {
	p := c.ReadCString(c.Arg(0))
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.cwd, p)
	}
	// Only files the game wrote (in the overlay) are removed; the game's own files stay.
	if err := os.Remove(filepath.Join(m.Overlay, filepath.Clean(p))); err != nil {
		m.setErrno(eNOENT)
		ret(c, neg1())
		return
	}
	ret(c, 0)
}

func (m *Machine) rename(c *cpu.CPU) {
	from := m.hostPath(c.ReadCString(c.Arg(0)), false)
	to := m.hostPath(c.ReadCString(c.Arg(1)), true)
	if !strings.HasPrefix(from, m.Overlay) {
		b, err := os.ReadFile(from)
		if err == nil {
			err = os.WriteFile(to, b, 0o644)
		}
		if err != nil {
			ret(c, neg1())
			return
		}
		ret(c, 0)
		return
	}
	if err := os.Rename(from, to); err != nil {
		ret(c, neg1())
		return
	}
	ret(c, 0)
}

func (m *Machine) realpath(c *cpu.CPU) {
	p := c.ReadCString(c.Arg(0))
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.cwd, p)
	}
	p = filepath.Clean(p)
	buf := c.Arg(1)
	if buf == 0 {
		buf = c.Malloc(1024)
	}
	c.WriteCString(buf, p)
	ret(c, buf)
}

type gdir struct {
	names []string
	types []byte
	i     int
	ent   uint32
}

// listDir lists a guest directory: what the game wrote (the overlay) merged with the game's own
// files, the overlay's first and winning on names that differ only in case (Mac file systems
// ignore case). ok is false when neither is a directory.
func (m *Machine) listDir(p string) (names []string, types []byte, ok bool) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.cwd, p)
	}
	p = filepath.Clean(p)
	game := m.fold(p)
	if game == "" {
		game = p
	}
	seen := map[string]bool{}
	for _, hp := range []string{filepath.Join(m.Overlay, p), game} {
		if st, err := os.Stat(hp); err != nil || !st.IsDir() {
			continue
		}
		ok = true
		ents, _ := os.ReadDir(hp)
		for _, e := range ents {
			if k := strings.ToLower(e.Name()); !seen[k] {
				seen[k] = true
				names = append(names, e.Name())
				t := byte(8) // DT_REG
				if e.IsDir() {
					t = 4 // DT_DIR
				}
				types = append(types, t)
			}
		}
	}
	return names, types, ok
}

func (m *Machine) opendir(c *cpu.CPU) {
	names, types, ok := m.listDir(c.ReadCString(c.Arg(0)))
	if !ok {
		m.setErrno(eNOENT)
		ret(c, 0)
		return
	}
	d := &gdir{names: append([]string{".", ".."}, names...), types: append([]byte{4, 4}, types...)}
	h := c.Malloc(16)
	d.ent = c.Malloc(8 + 256)
	m.dirs[h] = d
	ret(c, h)
}

// readdir returns the old struct dirent: ino u32, reclen u16, type u8, namlen u8, name[256].
func (m *Machine) readdir(c *cpu.CPU) {
	d := m.dirs[c.Arg(0)]
	if d == nil || d.i >= len(d.names) {
		ret(c, 0)
		return
	}
	name := d.names[d.i]
	if len(name) > 255 {
		name = name[:255]
	}
	c.Write32(d.ent, uint32(d.i+1))
	c.Write16(d.ent+4, 264)
	c.Write8(d.ent+6, d.types[d.i])
	c.Write8(d.ent+7, uint8(len(name)))
	c.WriteCString(d.ent+8, name)
	d.i++
	ret(c, d.ent)
}

func (m *Machine) closedir(c *cpu.CPU) {
	if d := m.dirs[c.Arg(0)]; d != nil {
		c.Free(d.ent)
		c.Free(c.Arg(0))
		delete(m.dirs, c.Arg(0))
	}
	ret(c, 0)
}

func (m *Machine) selectShim(c *cpu.CPU) {
	// Only used as a portable sleep (no descriptors).
	tv := c.Arg(4)
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
	if tv != 0 {
		m.sleepFor(uint64(c.Read32(tv))*1e6 + uint64(c.Read32(tv+4)))
	}
}

var _ = binary.LittleEndian

// scandir(dir, &list, select, compar) with the 64-bit-inode struct dirent: ino u64, seekoff u64,
// reclen u16, namlen u16, type u8, name[1024].
func (m *Machine) scandir(c *cpu.CPU) {
	dir, out, sel, cmp := c.Arg(0), c.Arg(1), c.Arg(2), c.Arg(3)
	c.Ret()
	found, types, ok := m.listDir(c.ReadCString(dir))
	if !ok {
		m.setErrno(eNOENT)
		c.Regs[cpu.RegEAX] = neg1()
		return
	}
	names := append([]string{".", ".."}, found...)
	types = append([]byte{4, 4}, types...)
	var ents []uint32
	for i, n := range names {
		e := c.Malloc(1048)
		clear(c.WBytes(e, 1048))
		c.Write64(e, uint64(i+1))
		c.Write16(e+16, 1048)
		c.Write16(e+18, uint16(len(n)))
		c.Write8(e+20, types[i])
		c.WriteCString(e+21, n)
		if sel != 0 {
			if keep, _ := c.CallGuest(sel, e); keep == 0 {
				c.Free(e)
				continue
			}
		}
		ents = append(ents, e)
	}
	if cmp != 0 && m.sysName[cmp] == "" { // a guest comparison: sort with it
		sort.SliceStable(ents, func(i, j int) bool {
			a, b := c.Malloc(8), c.Malloc(8)
			c.Write32(a, ents[i])
			c.Write32(b, ents[j])
			r, _ := c.CallGuest(cmp, a, b)
			c.Free(a)
			c.Free(b)
			return int32(r) < 0
		})
	} else if cmp != 0 {
		sort.SliceStable(ents, func(i, j int) bool { return c.ReadCString(ents[i]+21) < c.ReadCString(ents[j]+21) })
	}
	list := c.Malloc(uint32(4*len(ents) + 4))
	for i, e := range ents {
		c.Write32(list+uint32(4*i), e)
	}
	c.Write32(out, list)
	c.Regs[cpu.RegEAX] = uint32(len(ents))
}

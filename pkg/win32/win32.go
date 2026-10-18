package win32

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"catalina32/pkg/cpu"
	"catalina32/pkg/profile"
)

const (
	InvalidHandleValue = 0xFFFFFFFF
	HandleBaseWin32    = 0x00001000
	HandleBaseStdio    = 0x03002000
)

type OpenFile struct {
	HostPath string
	File     *os.File
	Size     int64
}

type Win32Env struct {
	DataRoot        string
	SaveRoot        string
	NextHandle      uint32
	Handles         map[uint32]*OpenFile
	StdioMap        map[uint32]*OpenFile // guest FILE* address -> OpenFile
	StartTime       time.Time
	LastError       uint32
	CmdLineAddr     uint32
	ModulePath      string
	CurrentThreadId uint32
	PerfCounter     uint64
	TraceLog        bool
	LiveClock       bool
	dirCache        map[string][]string // DataRoot directory listings (ResolvePath)
	prof            *profile.Profile
	sched           *scheduler
	WndProc         uint32
	peekCount       uint64
	// InputScript, when set, returns a window message (msg, wParam, lParam) to inject on the given PeekMessageA call.
	InputScript func(n uint64) (msg, wParam, lParam uint32, ok bool)
}

func NewWin32Env(c *cpu.CPU, dataRoot, saveRoot string) *Win32Env {
	_ = os.MkdirAll(saveRoot, 0755)
	cmdAddr := c.Malloc(256)
	c.WriteCString(cmdAddr, "")
	ak := os.Getenv("C32_AUTO_KEYS")
	liveClock := (os.Getenv("C32_SHM_FILE") != "" || os.Getenv("C32_LIVE") == "1") && (ak == "" || ak == "off")
	return &Win32Env{
		DataRoot:        dataRoot,
		SaveRoot:        saveRoot,
		NextHandle:      HandleBaseWin32,
		Handles:         make(map[uint32]*OpenFile),
		StdioMap:        make(map[uint32]*OpenFile),
		StartTime:       time.Now(),
		CmdLineAddr:     cmdAddr,
		ModulePath:      "C:\\LEGOStarWarsSaga\\LEGOStarWarsSaga.exe",
		CurrentThreadId: 1,
		PerfCounter:     1000000,
		TraceLog:        os.Getenv("C32_LIVE") != "1",
		LiveClock:       liveClock,
	}
}

// ResolvePath maps a guest path to a host path. "SaveData\..." lives under SaveRoot. Everything
// else is game data, matched case-insensitively inside DataRoot, which is never written: writes go
// to a copy under SaveRoot/DataOverlay (see openGuestFile), and reads prefer that copy once it exists.
func (env *Win32Env) ResolvePath(guestPath string, forWrite bool) string {
	clean := strings.ReplaceAll(guestPath, "\\", "/")
	clean = strings.TrimSpace(clean)
	if idx := strings.IndexByte(clean, ':'); idx == 1 {
		clean = clean[2:]
	}
	clean = strings.TrimLeft(clean, "/")

	if strings.HasPrefix(clean, "SaveData/") {
		return filepath.Join(env.SaveRoot, strings.TrimPrefix(clean, "SaveData/"))
	}
	if strings.HasPrefix(clean, strings.TrimLeft(env.SaveRoot, "/")) {
		return "/" + clean
	}
	overlay := filepath.Join(env.SaveRoot, "DataOverlay", filepath.FromSlash(strings.ToLower(clean)))
	if forWrite {
		return overlay
	}
	if _, err := os.Stat(overlay); err == nil {
		return overlay
	}
	return env.findData(clean)
}

// findData walks DataRoot case-insensitively. DataRoot is read-only, so directory listings are
// cached.
func (env *Win32Env) findData(clean string) string {
	parts := strings.Split(clean, "/")
	cur := env.DataRoot
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		names, ok := env.dirCache[cur]
		if !ok {
			entries, err := os.ReadDir(cur)
			if err != nil {
				break
			}
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if env.dirCache == nil {
				env.dirCache = map[string][]string{}
			}
			env.dirCache[cur] = names
		}
		matched := ""
		for _, n := range names {
			if strings.EqualFold(n, part) {
				matched = n
				break
			}
		}
		if matched == "" {
			// vfs-config.xml rule: .bik -> .mov
			if i == len(parts)-1 && strings.HasSuffix(strings.ToLower(part), ".bik") {
				movPart := part[:len(part)-4] + ".mov"
				for _, n := range names {
					if strings.EqualFold(n, movPart) {
						return filepath.Join(cur, n)
					}
				}
			}
			break
		}
		cur = filepath.Join(cur, matched)
		if i == len(parts)-1 {
			return cur
		}
	}
	return filepath.Join(env.DataRoot, clean)
}

// openGuestFile opens a guest path with os.OpenFile flags. A write to game data first copies the
// original into the overlay (unless the open truncates it), so DataRoot is never modified.
func (env *Win32Env) openGuestFile(guestPath string, flag int) (*os.File, string, error) {
	if flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		hostPath := env.ResolvePath(guestPath, false)
		f, err := os.Open(hostPath)
		return f, hostPath, err
	}
	readPath := env.ResolvePath(guestPath, false)
	hostPath := env.ResolvePath(guestPath, true)
	if flag&os.O_EXCL != 0 {
		if _, err := os.Stat(readPath); err == nil {
			return nil, hostPath, os.ErrExist
		}
	}
	_ = os.MkdirAll(filepath.Dir(hostPath), 0755)
	if readPath != hostPath && flag&os.O_TRUNC == 0 {
		if err := copyFile(readPath, hostPath); err != nil && !os.IsNotExist(err) {
			return nil, hostPath, err
		}
	}
	f, err := os.OpenFile(hostPath, flag, 0644)
	return f, hostPath, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if st, err := in.Stat(); err != nil || st.IsDir() {
		return os.ErrNotExist
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RegisterBoundaryThunks binds the MacDoze (Win32) shims to the functions the game profile names,
// and turns the profile's unmapped platform boundary functions into logged "return 1" stubs.
func (env *Win32Env) RegisterBoundaryThunks(c *cpu.CPU, prof *profile.Profile) {
	env.prof = prof
	env.initScheduler(c)
	for _, addr := range prof.UnmappedStubs {
		if prof.Named(addr) {
			continue // bound to a real shim by name
		}
		fnAddr := addr
		c.RegisterIntercept(fnAddr, fmt.Sprintf("boundary_0x%08x", fnAddr), func(vm *cpu.CPU) {
			vm.UnmappedCalls[fnAddr]++
			caller := uint32(0)
			if len(vm.CallStack) > 0 {
				caller = vm.CallStack[len(vm.CallStack)-1].CallerEIP
			}
			if env.TraceLog && vm.UnmappedCalls[fnAddr] <= 3 {
				fmt.Printf("[BOUNDARY-UNMAPPED] 0x%08x called from 0x%08x (arg0=0x%08x arg1=0x%08x arg2=0x%08x)\n",
					fnAddr, caller, vm.Arg(0), vm.Arg(1), vm.Arg(2))
			} else {
				vm.ReportOnce(fmt.Sprintf("boundary:%x", fnAddr), "[BOUNDARY-UNMAPPED] 0x%08x has no shim: returns 1", fnAddr)
			}
			vm.Regs[cpu.RegEAX] = 1
			vm.Ret()
		})
	}

	// Register concrete MacDoze Win32 API implementations
	// Critical Sections & Synchronization
	prof.Bind(c, "InitializeCriticalSection", env.thunkInitializeCriticalSection)
	prof.Bind(c, "EnterCriticalSection", env.thunkEnterCriticalSection)
	prof.Bind(c, "LeaveCriticalSection", env.thunkLeaveCriticalSection)
	prof.Bind(c, "CreateEventA", env.thunkCreateEventA)
	prof.Bind(c, "SetEvent", env.thunkSetEvent)
	prof.Bind(c, "WaitForSingleObject", env.thunkWaitForSingleObject)
	prof.Bind(c, "ResetEvent", env.thunkResetEvent)
	prof.Bind(c, "CloseHandle", env.thunkCloseHandle)
	prof.Bind(c, "CreateThread_2", env.thunkCreateThread)
	prof.Bind(c, "ResumeThread_2", env.thunkResumeThread)
	prof.Bind(c, "Sleep", env.thunkSleep)
	prof.Bind(c, "GetCurrentThread", env.thunkGetCurrentThread)
	prof.Bind(c, "GetCurrentThread_2", env.thunkGetCurrentThread)
	prof.Bind(c, "SetThreadPriority", env.thunkSetThreadPriority)
	prof.Bind(c, "GetCurrentThreadId", env.thunkGetCurrentThreadId)
	prof.Bind(c, "SetThreadIdealProcessor", env.thunkReturnOne)

	// System / Process / Startup
	prof.Bind(c, "SystemParametersInfoA", env.thunkSystemParametersInfoA)
	prof.Bind(c, "SetErrorMode", env.thunkSetErrorMode)
	prof.Bind(c, "CoInitializeEx", env.thunkCoInitializeEx)
	prof.Bind(c, "CoUninitialize", env.thunkReturnZero)
	prof.Bind(c, "GetModuleHandleA", env.thunkGetModuleHandleA)
	prof.Bind(c, "GetModuleFileNameA", env.thunkGetModuleFileNameA)
	prof.Bind(c, "GetCommandLineA", env.thunkGetCommandLineA)
	prof.Bind(c, "SetLastError", env.thunkSetLastError)
	prof.Bind(c, "GetLastError", env.thunkGetLastError)
	prof.Bind(c, "IsDebuggerPresent_OrOne", env.thunkReturnOne)

	// High-Resolution & Multimedia Timers
	prof.Bind(c, "QueryPerformanceFrequency", env.thunkQueryPerformanceFrequency)
	prof.Bind(c, "QueryPerformanceCounter", env.thunkQueryPerformanceCounter)
	prof.Bind(c, "timeGetTime_OrDouble", env.thunkTimeGetTime)
	prof.Bind(c, "GetTickCount", env.thunkTimeGetTime)
	prof.Bind(c, "timeSetEvent", env.thunkTimeSetEvent)
	prof.Bind(c, "timeKillEvent", env.thunkTimeKillEvent)

	// Win32 File I/O (CreateFileA, ReadFile, WriteFile, SetFilePointerEx, GetFileSize, etc.)
	prof.Bind(c, "CreateFileA", env.thunkCreateFileA)
	prof.Bind(c, "ReadFile", env.thunkReadFile)
	prof.Bind(c, "WriteFile", env.thunkWriteFile)
	prof.Bind(c, "SetFilePointerEx", env.thunkSetFilePointerEx)
	prof.Bind(c, "GetFileSize", env.thunkGetFileSize)
	prof.Bind(c, "CreateDirectoryA", env.thunkCreateDirectoryA)
	prof.Bind(c, "GetFileAttributesA", env.thunkGetFileAttributesA)
	prof.Bind(c, "GetDiskFreeSpaceExA", env.thunkGetDiskFreeSpaceExA)
	prof.Bind(c, "SHGetFolderPathA", env.thunkSHGetFolderPathA)
	prof.Bind(c, "ShowCursor", env.thunkReturnOne)

	// MacdozeFile (stdio wrappers)
	prof.Bind(c, "MacdozeFile::fopen", env.thunkFopen)
	prof.Bind(c, "MacdozeFile::fread", env.thunkFread)
	prof.Bind(c, "MacdozeFile::fwrite", env.thunkFwrite)
	prof.Bind(c, "MacdozeFile::fseek", env.thunkFseek)
	prof.Bind(c, "MacdozeFile::ftell", env.thunkFtell)
	prof.Bind(c, "MacdozeFile::fclose", env.thunkFclose)
	prof.Bind(c, "MacdozeFile::fputc", env.thunkFputc)
	prof.Bind(c, "MacdozeFile::fflush", env.thunkReturnZero)
	prof.Bind(c, "MacdozeFile::fprintf", env.thunkReturnZero)

	// Registry & Window Message Pump
	prof.Bind(c, "RegOpenKeyExA", env.thunkRegOpenKeyExA)
	prof.Bind(c, "RegQueryValueExA", env.thunkRegQueryValueExA)
	prof.Bind(c, "RegCloseKey", env.thunkReturnZero)
	prof.Bind(c, "MacdozeReg::ReadDword", env.thunkRegReadDword)
	prof.Bind(c, "MacdozeReg::WriteDword", env.thunkReturnOne)
	prof.Bind(c, "MacdozeReg::Flush", env.thunkReturnOne)
	prof.Bind(c, "MacdozeReg::ValueExists", env.thunkReturnZero)
	prof.Bind(c, "FeralRegReadDwordIfPresent", env.thunkReturnZero)
	prof.Bind(c, "FeralRegReadBoolIfPresent", env.thunkReturnZero)
	prof.Bind(c, "SendMessageA", env.thunkReturnOne)
	prof.Bind(c, "PeekMessageA", env.thunkPeekMessageA)
	prof.Bind(c, "TranslateMessage", env.thunkReturnZero)
	prof.Bind(c, "DispatchMessageA", env.thunkReturnZero)
	prof.Bind(c, "MacDoze::SetFocus", env.thunkReturnOne)
	prof.Bind(c, "FeralApp::GetInstance", env.thunkReturnOne)
	prof.Bind(c, "FeralApp::NotifyState", env.thunkReturnOne)
	prof.Bind(c, "LoadIconA", env.thunkReturnOne)
	prof.Bind(c, "LoadCursorA", env.thunkReturnOne)
	prof.Bind(c, "RegisterClassExA", env.thunkRegisterClassExA)
	prof.Bind(c, "CreateWindowExA", env.thunkReturnOne)
	prof.Bind(c, "ShowWindow", env.thunkShowWindow)
	prof.Bind(c, "UpdateWindow", env.thunkReturnOne)
	prof.Bind(c, "IndirectX::SetCurrentShaderHint", env.thunkReturnZero)
}

func (env *Win32Env) thunkShowWindow(c *cpu.CPU) {
	// Emulate the window state WndProc would record for WM_ACTIVATE (wParam=1) and WM_SIZE (1280x720).
	// TODO: deliver real WM_ACTIVATE/WM_SIZE messages to the registered WndProc instead.
	if a := env.prof.Global("MacDoze.WindowActive"); a != 0 {
		c.Write32(a, 1)
	}
	if a := env.prof.Global("MacDoze.WindowWidth"); a != 0 {
		c.Write32(a, 1280)
	}
	if a := env.prof.Global("MacDoze.WindowHeight"); a != 0 {
		c.Write32(a, 720)
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkReturnZero(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (env *Win32Env) thunkReturnOne(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkInitializeCriticalSection(c *cpu.CPU) {
	cs := c.Arg(0)
	if cs != 0 {
		clear(c.WBytes(cs, 24))
	}
	c.Ret()
}

func (env *Win32Env) thunkSetEvent(c *cpu.CPU) {
	hEvent := c.Arg(0)
	if ev, ok := env.sched.events[hEvent]; ok {
		ev.signaled = true
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
	env.yield(c)
}

func (env *Win32Env) thunkCloseHandle(c *cpu.CPU) {
	h := c.Arg(0)
	if of, ok := env.Handles[h]; ok {
		_ = of.File.Close()
		delete(env.Handles, h)
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkGetCurrentThread(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0xFFFFFFFF
	c.Ret()
}

func (env *Win32Env) thunkGetCurrentThreadId(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = env.CurrentThreadId
	c.Ret()
}

func (env *Win32Env) thunkSetThreadPriority(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkSystemParametersInfoA(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkSetErrorMode(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 2
	c.Ret()
}

func (env *Win32Env) thunkCoInitializeEx(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0 // S_OK
	c.Ret()
}

func (env *Win32Env) thunkGetModuleHandleA(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0x00001000
	c.Ret()
}

func (env *Win32Env) thunkGetModuleFileNameA(c *cpu.CPU) {
	buf := c.Arg(1)
	sz := int(c.Arg(2))
	s := env.ModulePath
	if len(s) >= sz && sz > 0 {
		s = s[:sz-1]
	}
	if buf != 0 && sz > 0 {
		c.WriteCString(buf, s)
	}
	c.Regs[cpu.RegEAX] = uint32(len(s))
	c.Ret()
}

func (env *Win32Env) thunkGetCommandLineA(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = env.CmdLineAddr
	c.Ret()
}

func (env *Win32Env) thunkSetLastError(c *cpu.CPU) {
	env.LastError = c.Arg(0)
	if a := env.prof.Global("MacDoze.LastError"); a != 0 {
		c.Write32(a, env.LastError)
	}
	c.Ret()
}

func (env *Win32Env) thunkGetLastError(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = env.LastError
	c.Ret()
}

func (env *Win32Env) currentMs(c *cpu.CPU) uint32 {
	if env.LiveClock {
		return uint32(time.Since(env.StartTime).Milliseconds())
	}
	return uint32(c.Steps / 50000)
}

func (env *Win32Env) thunkQueryPerformanceCounter(c *cpu.CPU) {
	ptr := c.Arg(0)
	var ticks uint64
	if env.LiveClock {
		ticks = uint64(time.Since(env.StartTime).Nanoseconds())
	} else {
		// Increment deterministically so game frame delta is smooth (~50-60 FPS) in headless runs
		env.PerfCounter += 20000000
		ticks = env.PerfCounter + c.Steps*20
	}
	if ptr != 0 {
		c.Write64(ptr, ticks)
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkQueryPerformanceFrequency(c *cpu.CPU) {
	ptr := c.Arg(0)
	if ptr != 0 {
		c.Write64(ptr, 1000000000) // 1 GHz
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkTimeGetTime(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = env.currentMs(c)
	c.Ret()
}

func (env *Win32Env) thunkCreateFileA(c *cpu.CPU) {
	namePtr := c.Arg(0)
	access := c.Arg(1)
	creation := c.Arg(4)
	guestPath := c.ReadCString(namePtr)
	flag := os.O_RDONLY
	switch write := access&0x40000000 != 0; {
	case write && access&0x80000000 != 0:
		flag = os.O_RDWR
	case write:
		flag = os.O_WRONLY
	}
	switch creation {
	case 1: // CREATE_NEW
		flag |= os.O_CREATE | os.O_EXCL
	case 2: // CREATE_ALWAYS
		flag |= os.O_CREATE | os.O_TRUNC
	case 4: // OPEN_ALWAYS
		flag |= os.O_CREATE
	case 5: // TRUNCATE_EXISTING
		flag |= os.O_TRUNC
	}
	if flag&(os.O_CREATE|os.O_TRUNC) != 0 && flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		flag |= os.O_RDWR // creating or truncating needs write access on the host
	}
	existed := false
	if _, err := os.Stat(env.ResolvePath(guestPath, false)); err == nil {
		existed = true
	}
	f, hostPath, err := env.openGuestFile(guestPath, flag)
	if err != nil {
		if env.TraceLog {
			fmt.Printf("[Win32] CreateFileA(%q -> %q) -> NOT FOUND\n", guestPath, hostPath)
		}
		env.LastError = 2 // ERROR_FILE_NOT_FOUND
		if os.IsExist(err) {
			env.LastError = 80 // ERROR_FILE_EXISTS
		}
		c.Regs[cpu.RegEAX] = InvalidHandleValue
		c.Ret()
		return
	}
	if existed && (creation == 2 || creation == 4) {
		env.LastError = 183 // ERROR_ALREADY_EXISTS
	}
	st, _ := f.Stat()
	sz := int64(0)
	if st != nil {
		sz = st.Size()
	}
	h := env.NextHandle
	env.NextHandle++
	env.Handles[h] = &OpenFile{
		HostPath: hostPath,
		File:     f,
		Size:     sz,
	}
	if env.TraceLog {
		fmt.Printf("[Win32] CreateFileA(%q -> %q, size=%d MB) -> handle=0x%x\n",
			guestPath, filepath.Base(hostPath), sz/(1024*1024), h)
	}
	c.Regs[cpu.RegEAX] = h
	c.Ret()
}

func (env *Win32Env) thunkReadFile(c *cpu.CPU) {
	h := c.Arg(0)
	buf := c.Arg(1)
	toRead := c.Arg(2)
	readPtr := c.Arg(3)

	of, ok := env.Handles[h]
	if !ok {
		if readPtr != 0 {
			c.Write32(readPtr, 0)
		}
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	n, err := io.ReadFull(of.File, c.WBytes(buf, toRead))
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	if err != nil && err != io.EOF {
		if readPtr != 0 {
			c.Write32(readPtr, 0)
		}
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	if readPtr != 0 {
		c.Write32(readPtr, uint32(n))
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkWriteFile(c *cpu.CPU) {
	h := c.Arg(0)
	buf := c.Arg(1)
	toWrite := c.Arg(2)
	writtenPtr := c.Arg(3)

	of, ok := env.Handles[h]
	if !ok {
		if writtenPtr != 0 {
			c.Write32(writtenPtr, 0)
		}
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	n, err := of.File.Write(c.Bytes(buf, toWrite))
	if err != nil {
		if writtenPtr != 0 {
			c.Write32(writtenPtr, 0)
		}
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	if writtenPtr != 0 {
		c.Write32(writtenPtr, uint32(n))
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkSetFilePointerEx(c *cpu.CPU) {
	h := c.Arg(0)
	distLow := c.Arg(1)
	distHigh := c.Arg(2)
	newPosPtr := c.Arg(3)
	method := c.Arg(4)

	of, ok := env.Handles[h]
	if !ok {
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	dist := int64((uint64(distHigh) << 32) | uint64(distLow))
	whence := io.SeekStart
	if method == 1 {
		whence = io.SeekCurrent
	} else if method == 2 {
		whence = io.SeekEnd
	}
	pos, err := of.File.Seek(dist, whence)
	if err != nil {
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	if newPosPtr != 0 {
		c.Write64(newPosPtr, uint64(pos))
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkGetFileSize(c *cpu.CPU) {
	h := c.Arg(0)
	highPtr := c.Arg(1)
	of, ok := env.Handles[h]
	if !ok {
		c.Regs[cpu.RegEAX] = InvalidHandleValue
		c.Ret()
		return
	}
	size := of.Size
	if st, err := of.File.Stat(); err == nil {
		size = st.Size() // the guest may have written to it since it was opened
	}
	if highPtr != 0 {
		c.Write32(highPtr, uint32(uint64(size)>>32))
	}
	c.Regs[cpu.RegEAX] = uint32(size)
	c.Ret()
}

func (env *Win32Env) thunkCreateDirectoryA(c *cpu.CPU) {
	path := c.ReadCString(c.Arg(0))
	hostPath := env.ResolvePath(path, true)
	_ = os.MkdirAll(hostPath, 0755)
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkGetFileAttributesA(c *cpu.CPU) {
	path := c.ReadCString(c.Arg(0))
	hostPath := env.ResolvePath(path, false)
	st, err := os.Stat(hostPath)
	if err != nil {
		c.Regs[cpu.RegEAX] = InvalidHandleValue
	} else if st.IsDir() {
		c.Regs[cpu.RegEAX] = 0x10 // FILE_ATTRIBUTE_DIRECTORY
	} else {
		c.Regs[cpu.RegEAX] = 0x80 // FILE_ATTRIBUTE_NORMAL
	}
	c.Ret()
}

func (env *Win32Env) thunkGetDiskFreeSpaceExA(c *cpu.CPU) {
	freeAvail := c.Arg(1)
	totalBytes := c.Arg(2)
	totalFree := c.Arg(3)
	const tenGB = uint64(10 * 1024 * 1024 * 1024)
	if freeAvail != 0 {
		c.Write64(freeAvail, tenGB)
	}
	if totalBytes != 0 {
		c.Write64(totalBytes, tenGB*10)
	}
	if totalFree != 0 {
		c.Write64(totalFree, tenGB)
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

func (env *Win32Env) thunkSHGetFolderPathA(c *cpu.CPU) {
	pszPath := c.Arg(4)
	if pszPath != 0 {
		c.WriteCString(pszPath, "C:\\SaveData")
	}
	c.Regs[cpu.RegEAX] = 0 // S_OK
	c.Ret()
}

// fopenFlags maps a C fopen mode ("r", "w+b", "ab", ...) to os.OpenFile flags.
func fopenFlags(mode string) int {
	plus := strings.Contains(mode, "+")
	var flag int
	switch {
	case strings.HasPrefix(mode, "w"):
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	case strings.HasPrefix(mode, "a"):
		flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	default:
		flag = os.O_RDONLY
	}
	if plus {
		flag = flag&^os.O_WRONLY | os.O_RDWR
	}
	return flag
}

func (env *Win32Env) thunkFopen(c *cpu.CPU) {
	path := c.ReadCString(c.Arg(0))
	mode := c.ReadCString(c.Arg(1))
	f, hostPath, err := env.openGuestFile(path, fopenFlags(mode))
	if err != nil {
		if env.TraceLog {
			fmt.Printf("[stdio] fopen(%q, %q) -> NULL\n", path, mode)
		}
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	st, _ := f.Stat()
	sz := int64(0)
	if st != nil {
		sz = st.Size()
	}
	guestFilePtr := c.Malloc(80)
	env.StdioMap[guestFilePtr] = &OpenFile{
		HostPath: hostPath,
		File:     f,
		Size:     sz,
	}
	if env.TraceLog {
		fmt.Printf("[stdio] fopen(%q -> %q, %q) -> 0x%08x\n", path, filepath.Base(hostPath), mode, guestFilePtr)
	}
	c.Regs[cpu.RegEAX] = guestFilePtr
	c.Ret()
}

func (env *Win32Env) thunkFread(c *cpu.CPU) {
	ptr := c.Arg(0)
	size := c.Arg(1)
	nmemb := c.Arg(2)
	stream := c.Arg(3)

	of, ok := env.StdioMap[stream]
	if !ok || size == 0 || nmemb == 0 {
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	total := uint64(size) * uint64(nmemb)
	if total >= 1<<32 {
		total = 0
	}
	n, _ := io.ReadFull(of.File, c.WBytes(ptr, uint32(total)))
	c.Regs[cpu.RegEAX] = uint32(n) / size
	c.Ret()
}

func (env *Win32Env) thunkFwrite(c *cpu.CPU) {
	ptr := c.Arg(0)
	size := c.Arg(1)
	nmemb := c.Arg(2)
	stream := c.Arg(3)

	of, ok := env.StdioMap[stream]
	if !ok || size == 0 || nmemb == 0 {
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		return
	}
	total := uint64(size) * uint64(nmemb)
	if total >= 1<<32 {
		total = 0
	}
	n, _ := of.File.Write(c.Bytes(ptr, uint32(total)))
	c.Regs[cpu.RegEAX] = uint32(n) / size
	c.Ret()
}

func (env *Win32Env) thunkFseek(c *cpu.CPU) {
	stream := c.Arg(0)
	offset := int64(int32(c.Arg(1)))
	whence := int(c.Arg(2))

	of, ok := env.StdioMap[stream]
	if !ok {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		c.Ret()
		return
	}
	_, err := of.File.Seek(offset, whence)
	if err != nil {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
	} else {
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (env *Win32Env) thunkFtell(c *cpu.CPU) {
	stream := c.Arg(0)
	of, ok := env.StdioMap[stream]
	if !ok {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
		c.Ret()
		return
	}
	pos, err := of.File.Seek(0, io.SeekCurrent)
	if err != nil {
		c.Regs[cpu.RegEAX] = 0xFFFFFFFF
	} else {
		c.Regs[cpu.RegEAX] = uint32(pos)
	}
	c.Ret()
}

func (env *Win32Env) thunkFclose(c *cpu.CPU) {
	stream := c.Arg(0)
	if of, ok := env.StdioMap[stream]; ok {
		_ = of.File.Close()
		delete(env.StdioMap, stream)
		c.Free(stream)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (env *Win32Env) thunkFputc(c *cpu.CPU) {
	ch := byte(c.Arg(0))
	stream := c.Arg(1)
	if of, ok := env.StdioMap[stream]; ok {
		_, _ = of.File.Write([]byte{ch})
	}
	c.Regs[cpu.RegEAX] = uint32(ch)
	c.Ret()
}

func (env *Win32Env) thunkRegOpenKeyExA(c *cpu.CPU) {
	phkResult := c.Arg(4)
	if phkResult != 0 {
		c.Write32(phkResult, 0x12345678)
	}
	c.Regs[cpu.RegEAX] = 0 // ERROR_SUCCESS
	c.Ret()
}

func (env *Win32Env) thunkRegQueryValueExA(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 2 // ERROR_FILE_NOT_FOUND
	c.Ret()
}

func (env *Win32Env) thunkRegReadDword(c *cpu.CPU) {
	valName := c.ReadCString(c.Arg(2))
	var val uint32
	switch valName {
	case "ScreenW", "WindowWidth":
		val = 1280
	case "ScreenH", "WindowHeight":
		val = 720
	case "ScreenR":
		val = 60 << 16 // Fixed-point 16.16 for _Fix2X -> 60.0 Hz
	case "Windowed":
		val = 1
	default:
		val = 0
	}
	if env.TraceLog {
		fmt.Printf("[Win32] MacdozeReg::ReadDword(%q) -> %d (0x%x)\n", valName, val, val)
	}
	c.Regs[cpu.RegEAX] = val
	c.Ret()
}

// thunkRegisterClassExA records the window procedure so synthetic input messages can be dispatched to it.
func (env *Win32Env) thunkRegisterClassExA(c *cpu.CPU) {
	wc := c.Arg(0)
	if wc != 0 {
		env.WndProc = c.Read32(wc + 8)
		if env.TraceLog {
			fmt.Printf("[Win32] RegisterClassExA: cbSize=%d style=0x%x lpfnWndProc=0x%08x\n", c.Read32(wc), c.Read32(wc+4), env.WndProc)
		}
	}
	c.Regs[cpu.RegEAX] = 1
	c.Ret()
}

// thunkPeekMessageA delivers scripted/host input as window messages straight to the game's WndProc.
func (env *Win32Env) thunkPeekMessageA(c *cpu.CPU) {
	env.peekCount++
	if env.WndProc != 0 && env.InputScript != nil {
		if msg, wp, lp, ok := env.InputScript(env.peekCount); ok {
			if env.TraceLog {
				fmt.Printf("[Win32] inject msg=0x%x wParam=0x%x lParam=0x%08x (peek #%d)\n", msg, wp, lp, env.peekCount)
			}
			_, _ = c.CallGuestNested(env.WndProc, 0x1000, msg, wp, lp)
		}
	}
	c.Regs[cpu.RegEAX] = 0 // no message left in the queue
	c.Ret()
	env.fireTimers(c)
}

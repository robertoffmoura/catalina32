package win32

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lsw64/pkg/cpu"
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
		TraceLog:        true,
	}
}

// Case-insensitive path resolver against DataRoot and SaveRoot
func (env *Win32Env) ResolvePath(guestPath string, forWrite bool) string {
	clean := strings.ReplaceAll(guestPath, "\\", "/")
	clean = strings.TrimSpace(clean)
	if idx := strings.IndexByte(clean, ':'); idx == 1 {
		clean = clean[2:]
	}
	clean = strings.TrimLeft(clean, "/")

	// If under SaveRoot prefix
	if strings.HasPrefix(clean, "SaveData/") {
		rel := strings.TrimPrefix(clean, "SaveData/")
		return filepath.Join(env.SaveRoot, rel)
	}
	if strings.HasPrefix(clean, strings.TrimLeft(env.SaveRoot, "/")) {
		return "/" + clean
	}

	// Walk case-insensitively inside DataRoot
	parts := strings.Split(clean, "/")
	cur := env.DataRoot
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		entries, err := os.ReadDir(cur)
		if err != nil {
			break
		}
		matched := ""
		for _, e := range entries {
			if strings.EqualFold(e.Name(), part) {
				matched = e.Name()
				break
			}
		}
		if matched != "" {
			cur = filepath.Join(cur, matched)
			if i == len(parts)-1 {
				return cur
			}
		} else {
			// Check vfs-config.xml rule: .bik -> .mov
			if i == len(parts)-1 && strings.HasSuffix(strings.ToLower(part), ".bik") {
				movPart := part[:len(part)-4] + ".mov"
				for _, e := range entries {
					if strings.EqualFold(e.Name(), movPart) {
						return filepath.Join(cur, e.Name())
					}
				}
			}
			break
		}
	}

	if forWrite {
		return filepath.Join(env.SaveRoot, filepath.Base(clean))
	}
	return filepath.Join(env.DataRoot, clean)
}

func (env *Win32Env) RegisterBoundaryThunks(c *cpu.CPU, boundaryAddrs []uint32) {
	env.initScheduler(c)
	// Register default boundary tracer for boundary functions >= 0x00500000 first
	// (pure Feral C++/NString helpers in 0x00390000..0x00420000 execute natively on our i386 CPU)
	for _, addr := range boundaryAddrs {
		if addr < 0x00500000 {
			continue
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
			}
			vm.Regs[cpu.RegEAX] = 1
			vm.Ret()
		})
	}

	// Register concrete MacDoze Win32 API implementations
	// Critical Sections & Synchronization
	c.RegisterIntercept(0x006411d0, "InitializeCriticalSection", env.thunkInitializeCriticalSection)
	c.RegisterIntercept(0x006412d0, "EnterCriticalSection", env.thunkEnterCriticalSection)
	c.RegisterIntercept(0x00641340, "LeaveCriticalSection", env.thunkLeaveCriticalSection)
	c.RegisterIntercept(0x00623f40, "CreateEventA", env.thunkCreateEventA)
	c.RegisterIntercept(0x00623fb0, "SetEvent", env.thunkSetEvent)
	c.RegisterIntercept(0x00623b90, "WaitForSingleObject", env.thunkWaitForSingleObject)
	c.RegisterIntercept(0x00623c30, "ResetEvent", env.thunkResetEvent)
	c.RegisterIntercept(0x00620660, "CloseHandle", env.thunkCloseHandle)
	c.RegisterIntercept(0x006245f0, "CreateThread", env.thunkCreateThread)
	c.RegisterIntercept(0x00623ac0, "CreateThread_2", env.thunkCreateThread)
	c.RegisterIntercept(0x00623b70, "ResumeThread_2", env.thunkResumeThread)
	c.RegisterIntercept(0x00623c60, "Sleep", env.thunkSleep)
	c.RegisterIntercept(0x00623d80, "GetCurrentThread", env.thunkGetCurrentThread)
	c.RegisterIntercept(0x00623d90, "GetCurrentThread_2", env.thunkGetCurrentThread)
	c.RegisterIntercept(0x00623d60, "SetThreadPriority", env.thunkSetThreadPriority)
	c.RegisterIntercept(0x00623d70, "GetCurrentThreadId", env.thunkGetCurrentThreadId)
	c.RegisterIntercept(0x00623cc0, "SetThreadIdealProcessor", env.thunkReturnOne)

	// System / Process / Startup
	c.RegisterIntercept(0x00620db0, "SystemParametersInfoA", env.thunkSystemParametersInfoA)
	c.RegisterIntercept(0x00620f90, "SetErrorMode", env.thunkSetErrorMode)
	c.RegisterIntercept(0x0061f990, "CoInitializeEx", env.thunkCoInitializeEx)
	c.RegisterIntercept(0x0061f9a0, "CoUninitialize", env.thunkReturnZero)
	c.RegisterIntercept(0x0060d6f0, "GetModuleHandleA", env.thunkGetModuleHandleA)
	c.RegisterIntercept(0x0060d600, "GetModuleFileNameA", env.thunkGetModuleFileNameA)
	c.RegisterIntercept(0x006428a0, "GetCommandLineA", env.thunkGetCommandLineA)
	c.RegisterIntercept(0x00624cb0, "SetLastError", env.thunkSetLastError)
	c.RegisterIntercept(0x00624cd0, "GetLastError", env.thunkGetLastError)
	c.RegisterIntercept(0x00620da0, "NoOp_620da0", env.thunkReturnZero)
	c.RegisterIntercept(0x00620810, "IsDebuggerPresent_OrOne", env.thunkReturnOne)

	// High-Resolution Timers
	c.RegisterIntercept(0x00624700, "QueryPerformanceFrequency", env.thunkQueryPerformanceFrequency)
	c.RegisterIntercept(0x00624720, "QueryPerformanceCounter", env.thunkQueryPerformanceCounter)
	c.RegisterIntercept(0x006242d0, "timeGetTime_OrDouble", env.thunkTimeGetTime)
	c.RegisterIntercept(0x00624290, "GetTickCount", env.thunkTimeGetTime)

	// Win32 File I/O (CreateFileA, ReadFile, WriteFile, SetFilePointerEx, etc.)
	c.RegisterIntercept(0x0060a600, "CreateFileA", env.thunkCreateFileA)
	c.RegisterIntercept(0x0060ae10, "ReadFile", env.thunkReadFile)
	c.RegisterIntercept(0x0060b2d0, "WriteFile", env.thunkWriteFile)
	c.RegisterIntercept(0x0060b8e0, "SetFilePointerEx", env.thunkSetFilePointerEx)
	c.RegisterIntercept(0x0060b8a0, "FlushFileBuffers", env.thunkReturnOne)
	c.RegisterIntercept(0x0060b790, "CreateDirectoryA", env.thunkCreateDirectoryA)
	c.RegisterIntercept(0x0060bb30, "GetFileAttributesA", env.thunkGetFileAttributesA)
	c.RegisterIntercept(0x0060bd50, "GetDiskFreeSpaceExA", env.thunkGetDiskFreeSpaceExA)
	c.RegisterIntercept(0x0060f610, "SHGetFolderPathA", env.thunkSHGetFolderPathA)
	c.RegisterIntercept(0x0060f850, "ShowCursor", env.thunkReturnOne)

	// MacdozeFile (stdio wrappers)
	c.RegisterIntercept(0x00612ce0, "MacdozeFile::fopen", env.thunkFopen)
	c.RegisterIntercept(0x006137e0, "MacdozeFile::fread", env.thunkFread)
	c.RegisterIntercept(0x00613a90, "MacdozeFile::fwrite", env.thunkFwrite)
	c.RegisterIntercept(0x00613b60, "MacdozeFile::fseek", env.thunkFseek)
	c.RegisterIntercept(0x00613c10, "MacdozeFile::ftell", env.thunkFtell)
	c.RegisterIntercept(0x00613420, "MacdozeFile::fclose", env.thunkFclose)
	c.RegisterIntercept(0x006139e0, "MacdozeFile::fputc", env.thunkFputc)
	c.RegisterIntercept(0x006137d0, "MacdozeFile::fflush", env.thunkReturnZero)
	c.RegisterIntercept(0x00613c60, "MacdozeFile::fprintf", env.thunkReturnZero)

	// Registry & Window Message Pump
	c.RegisterIntercept(0x00610fe0, "RegOpenKeyExA", env.thunkRegOpenKeyExA)
	c.RegisterIntercept(0x006110b0, "RegQueryValueExA", env.thunkRegQueryValueExA)
	c.RegisterIntercept(0x006108d0, "RegCloseKey", env.thunkReturnZero)
	c.RegisterIntercept(0x00611340, "MacdozeReg::ReadDword", env.thunkRegReadDword)
	c.RegisterIntercept(0x006114b0, "MacdozeReg::WriteDword", env.thunkReturnOne)
	c.RegisterIntercept(0x00612290, "MacdozeReg::Flush", env.thunkReturnOne)
	c.RegisterIntercept(0x00610690, "MacdozeReg::ValueExists", env.thunkReturnZero)
	c.RegisterIntercept(0x0016fc60, "FeralRegReadDwordIfPresent", env.thunkReturnZero)
	c.RegisterIntercept(0x0016fd10, "FeralRegReadBoolIfPresent", env.thunkReturnZero)
	c.RegisterIntercept(0x006353a0, "SendMessageA", env.thunkReturnOne)
	c.RegisterIntercept(0x00635570, "PeekMessageA", env.thunkPeekMessageA)
	c.RegisterIntercept(0x00635f90, "TranslateMessage", env.thunkReturnZero)
	c.RegisterIntercept(0x006367f0, "DispatchMessageA", env.thunkReturnZero)
	c.RegisterIntercept(0x00619c00, "MacDoze::SetFocus", env.thunkReturnOne)
	c.RegisterIntercept(0x00832350, "FeralApp::GetInstance", env.thunkReturnOne)
	c.RegisterIntercept(0x0082cdf0, "FeralApp::NotifyState", env.thunkReturnOne)
	c.RegisterIntercept(0x0061aa90, "LoadIconA", env.thunkReturnOne)
	c.RegisterIntercept(0x00619a80, "LoadCursorA", env.thunkReturnOne)
	c.RegisterIntercept(0x0063e590, "RegisterClassExA", env.thunkRegisterClassExA)
	c.RegisterIntercept(0x0063e8c0, "CreateWindowExA", env.thunkReturnOne)
	c.RegisterIntercept(0x0063ea60, "ShowWindow", env.thunkShowWindow)
	c.RegisterIntercept(0x0063eb40, "UpdateWindow", env.thunkReturnOne)
	c.RegisterIntercept(0x006bd5b0, "IndirectX::SetCurrentShaderHint", env.thunkReturnZero)
}

func (env *Win32Env) thunkShowWindow(c *cpu.CPU) {
	// Emulate WndProc WM_ACTIVATE (wParam=1) and WM_SIZE (1280x720)
	c.Write32(0x00b67c20, 1)
	c.Write32(0x00b67c38, 1280)
	c.Write32(0x00b67c3c, 720)
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
		clear(c.Mem[cs : cs+24])
	}
	c.Ret()
}

func (env *Win32Env) thunkSetEvent(c *cpu.CPU) {
	hEvent := c.Arg(0)
	c.Ret()
	if ev, ok := env.sched.events[hEvent]; ok {
		ev.signaled = true
	}

	// Check if this is NuEngine's 'BGWorkToDo' event (stored at 0x02c3d45c)
	bgEvent := c.Read32(0x02c3d45c)
	if hEvent != 0 && hEvent == bgEvent {
		poolPtr := c.Read32(0x02c3d440)
		for poolPtr != 0 {
			jobPtr, err := c.CallGuestNested(0x0007c870, poolPtr, 0)
			if err != nil || jobPtr == 0 {
				break
			}
			c.Write32(0x02c3d43c, jobPtr)
			c.Write32(jobPtr+8, c.Read32(jobPtr+8)|1)

			savedTid := env.CurrentThreadId
			bgTid := c.Read32(0x02b5c70c)
			if bgTid != 0 {
				env.CurrentThreadId = bgTid
			}
			fn1 := c.Read32(jobPtr)
			fn2 := c.Read32(jobPtr + 4)
			if env.TraceLog {
				fmt.Printf("[BGWorker] Executing async job 0x%08x (fn1=0x%08x, fn2=0x%08x)...\n", jobPtr, fn1, fn2)
			}
			if fn1 != 0 {
				_, _ = c.CallGuestNested(fn1, jobPtr)
			}
			if fn2 != 0 {
				_, _ = c.CallGuestNested(fn2, jobPtr)
			}
			env.CurrentThreadId = savedTid

			_, _ = c.CallGuestNested(0x0007c7b0, jobPtr)
			c.Write32(0x02c3d43c, 0)
		}
	}
	c.Regs[cpu.RegEAX] = 1
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
	c.Write32(0x00b9fdac, env.LastError)
	c.Ret()
}

func (env *Win32Env) thunkGetLastError(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = env.LastError
	c.Ret()
}

func (env *Win32Env) thunkQueryPerformanceCounter(c *cpu.CPU) {
	ptr := c.Arg(0)
	// Increment deterministically so game frame delta is smooth (~50-60 FPS)
	env.PerfCounter += 20000000
	ticks := env.PerfCounter + c.Steps*20
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
	ms := uint32(c.Steps / 50000)
	c.Regs[cpu.RegEAX] = ms
	c.Ret()
}

func (env *Win32Env) thunkCreateFileA(c *cpu.CPU) {
	namePtr := c.Arg(0)
	access := c.Arg(1)
	creation := c.Arg(4)
	guestPath := c.ReadCString(namePtr)
	forWrite := (access&0x40000000) != 0 || creation == 2 || creation == 4
	hostPath := env.ResolvePath(guestPath, forWrite)

	var f *os.File
	var err error
	if forWrite {
		_ = os.MkdirAll(filepath.Dir(hostPath), 0755)
		f, err = os.OpenFile(hostPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	} else {
		f, err = os.Open(hostPath)
	}
	if err != nil {
		if env.TraceLog {
			fmt.Printf("[Win32] CreateFileA(%q -> %q) -> NOT FOUND\n", guestPath, hostPath)
		}
		env.LastError = 2 // ERROR_FILE_NOT_FOUND
		c.Regs[cpu.RegEAX] = InvalidHandleValue
		c.Ret()
		return
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
	n, err := of.File.Read(c.Mem[buf : buf+toRead])
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
	n, err := of.File.Write(c.Mem[buf : buf+toWrite])
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

func (env *Win32Env) thunkFopen(c *cpu.CPU) {
	path := c.ReadCString(c.Arg(0))
	mode := c.ReadCString(c.Arg(1))
	forWrite := strings.ContainsAny(mode, "wa+")
	hostPath := env.ResolvePath(path, forWrite)

	var f *os.File
	var err error
	if forWrite {
		_ = os.MkdirAll(filepath.Dir(hostPath), 0755)
		f, err = os.OpenFile(hostPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	} else {
		f, err = os.Open(hostPath)
	}
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
	total := size * nmemb
	n, _ := of.File.Read(c.Mem[ptr : ptr+total])
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
	total := size * nmemb
	n, _ := of.File.Write(c.Mem[ptr : ptr+total])
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
	if env.WndProc != 0 {
		script := env.InputScript
		if script == nil {
			script = defaultInputScript
		}
		if msg, wp, lp, ok := script(env.peekCount); ok {
			if env.TraceLog {
				fmt.Printf("[Win32] inject msg=0x%x wParam=0x%x lParam=0x%08x (peek #%d)\n", msg, wp, lp, env.peekCount)
			}
			_, _ = c.CallGuestNested(env.WndProc, 0x1000, msg, wp, lp)
		}
	}
	c.Regs[cpu.RegEAX] = 0 // no message left in the queue
	c.Ret()
}

// defaultInputScript taps Enter (scancode 0x1C) periodically so menus can be driven without a real keyboard.
func defaultInputScript(n uint64) (msg, wParam, lParam uint32, ok bool) {
	const period = 120
	if n < 200 {
		return 0, 0, 0, false
	}
	switch (n - 200) % period {
	case 100:
		return 0x100, 0x0D, 0x1C << 16, true // WM_KEYDOWN VK_RETURN
	case 106:
		return 0x101, 0x0D, 0xC0000000 | 0x1C<<16, true // WM_KEYUP VK_RETURN
	}
	return 0, 0, 0, false
}

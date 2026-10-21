package cpu

import (
	"runtime"
	"syscall"
	_ "unsafe" // go:linkname
)

// Apple Silicon: memory is never writable and executable at once. The code buffer is mapped with
// MAP_JIT; a thread writes it after pthread_jit_write_protect_np(0) (which makes it unexecutable
// for that thread only), then protects it again and invalidates the instruction cache. Without the
// hardened runtime (Catalina32 is signed ad hoc) no entitlement is needed. The libSystem calls are
// made as golang.org/x/sys/unix makes them (the app is built without cgo).
//
// Not verified on Apple hardware yet, so off unless C32_JIT=1 (jitOptIn).

const jitOptIn = true

const mapJIT = 0x800

func mapExecutable(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC, syscall.MAP_ANON|syscall.MAP_PRIVATE|mapJIT)
}

func jitWriteBegin() {
	runtime.LockOSThread()
	syscall_syscall(libc_pthread_jit_write_protect_np_trampoline_addr, 0, 0, 0)
}

func jitWriteEnd(addr uintptr, n int) {
	syscall_syscall(libc_pthread_jit_write_protect_np_trampoline_addr, 1, 0, 0)
	syscall_syscall(libc_sys_icache_invalidate_trampoline_addr, addr, uintptr(n), 0)
	runtime.UnlockOSThread()
}

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

var libc_pthread_jit_write_protect_np_trampoline_addr uintptr

//go:cgo_import_dynamic libc_pthread_jit_write_protect_np pthread_jit_write_protect_np "/usr/lib/libSystem.B.dylib"

var libc_sys_icache_invalidate_trampoline_addr uintptr

//go:cgo_import_dynamic libc_sys_icache_invalidate sys_icache_invalidate "/usr/lib/libSystem.B.dylib"

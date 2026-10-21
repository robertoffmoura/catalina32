//go:build arm64 && !darwin

package cpu

// Writing translated code: the instruction cache must be told (jitFlushICache).

func jitWriteBegin() {}

func jitWriteEnd(addr uintptr, n int) { jitFlushICache(addr, uintptr(n)) }

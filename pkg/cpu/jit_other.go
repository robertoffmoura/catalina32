//go:build !amd64 && !arm64

package cpu

// No JIT backend for this architecture: blocks run in the interpreter.

const jitSupported = false

func jitShared(j *jitCode) int                           { return 0 }
func jitTranslate(c *CPU, j *jitCode, jb *jitBlock) bool { return false }
func jitPatch(j *jitCode, site, target uintptr)          {}
func jitCall(prologue uintptr, c *CPU, target uintptr)   {}

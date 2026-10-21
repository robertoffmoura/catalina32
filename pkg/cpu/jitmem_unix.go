//go:build unix

package cpu

import "syscall"

// mapExecutable reserves memory translated code can be written to and run from.
func mapExecutable(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC, syscall.MAP_ANON|syscall.MAP_PRIVATE)
}

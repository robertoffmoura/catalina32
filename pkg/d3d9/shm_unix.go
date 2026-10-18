//go:build unix

package d3d9

import (
	"os"
	"syscall"
)

// mapShm maps the live viewer's shared-memory file.
func mapShm(f *os.File, size int) ([]byte, error) {
	return syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}

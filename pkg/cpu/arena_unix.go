//go:build unix

package cpu

import (
	"syscall"
	"unsafe"
)

// newArena reserves size bytes of zeroed memory outside the Go heap: pages are only backed when
// touched, the garbage collector never scans or counts them, and the address never moves (so it
// can be handed to the host).
func newArena(size uint64) []byte {
	m, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE|syscall.MAP_NORESERVE)
	if err != nil {
		panic("cpu: cannot reserve the guest address space: " + err.Error())
	}
	return m
}

// newBitmap is newArena for n 64-bit words.
func newBitmap(n uint64) []uint64 {
	b := newArena(n * 8)
	return unsafe.Slice((*uint64)(unsafe.Pointer(&b[0])), n)
}

// freeArena returns memory from newArena or newBitmap.
func freeArena(b []byte) {
	if len(b) > 0 {
		syscall.Munmap(b)
	}
}

func freeBitmap(b []uint64) {
	if len(b) > 0 {
		freeArena(unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), len(b)*8))
	}
}

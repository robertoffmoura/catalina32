//go:build !unix

package cpu

func newArena(size uint64) []byte { return make([]byte, size) }
func newBitmap(n uint64) []uint64 { return make([]uint64, n) }

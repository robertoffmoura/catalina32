package cpu

// Float-to-integer conversions with x86 semantics. Go leaves NaN and out-of-range conversions
// to the host (amd64 and arm64 differ), so every guest-visible conversion goes through these:
// x87 FIST/FISTP/FISTTP and SSE CVT(T)S*2SI produce the "integer indefinite" value (the most
// negative integer) for NaN and for results that do not fit. x is already rounded/truncated.

// ToInt16 converts x like a 16-bit x87 integer store.
func ToInt16(x float64) int16 {
	if x >= -1<<15 && x < 1<<15 {
		return int16(x)
	}
	return -1 << 15
}

// ToInt32 converts x like a 32-bit x87 integer store or SSE conversion to a 32-bit register.
func ToInt32(x float64) int32 {
	if x >= -1<<31 && x < 1<<31 {
		return int32(x)
	}
	return -1 << 31
}

// ToInt64 converts x like a 64-bit x87 integer store.
func ToInt64(x float64) int64 {
	if x >= -1<<63 && x < 1<<63 {
		return int64(x)
	}
	return -1 << 63
}

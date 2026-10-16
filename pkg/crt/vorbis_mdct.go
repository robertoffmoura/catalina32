package crt

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"

	"lsw64/pkg/cpu"
)

// Native port of libvorbis mdct.c (mdct_backward + butterflies + bitreverse), the inverse MDCT
// that dominates Ogg music decoding (~12% of all guest steps while music plays).
//
// Guest layout (statically linked libvorbis, i386):
//   mdct_lookup { int n; int log2n; float *trig; int *bitrev; float scale; }
//   void mdct_backward(mdct_lookup *init, float *in, float *out)   // cdecl, called with in == out
//
// The function is located by the "vorbis:mdct_backward" signature (pkg/profile).

const (
	cPI3_8 = float32(.38268343236508977175)
	cPI2_8 = float32(.70710678118654752441)
	cPI1_8 = float32(.92387953251128675613)
)

// mdctTables caches the decoded trig/bitrev tables per lookup; they are fixed once mdct_init has run.
type mdctTables struct {
	trigPtr, bitrevPtr uint32
	n, log2n           int
	trig               []float32
	bitrev             []int32
}

type mdctState struct {
	tables map[uint32]*mdctTables
	addr   uint32
	buf    []float32
	verify bool

	verifiedCalls uint64
	seenN         map[int]bool
}

var mdct = &mdctState{tables: map[uint32]*mdctTables{}, seenN: map[int]bool{}, verify: os.Getenv("LSW_VERIFY_MDCT") == "1"}

func guestF32s(c *cpu.CPU, addr uint32, dst []float32) {
	m := c.Mem[addr : addr+uint32(len(dst))*4]
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(m[i*4:]))
	}
}

func putGuestF32s(c *cpu.CPU, addr uint32, src []float32) {
	m := c.Mem[addr : addr+uint32(len(src))*4]
	for i, v := range src {
		binary.LittleEndian.PutUint32(m[i*4:], math.Float32bits(v))
	}
}

func (s *mdctState) lookup(c *cpu.CPU, init uint32) *mdctTables {
	n := int(int32(c.Read32(init)))
	log2n := int(int32(c.Read32(init + 4)))
	trigPtr, bitrevPtr := c.Read32(init+8), c.Read32(init+12)
	t := s.tables[init]
	if t != nil && t.n == n && t.log2n == log2n && t.trigPtr == trigPtr && t.bitrevPtr == bitrevPtr {
		return t
	}
	t = &mdctTables{trigPtr: trigPtr, bitrevPtr: bitrevPtr, n: n, log2n: log2n,
		trig: make([]float32, n+n/4), bitrev: make([]int32, n/4)}
	guestF32s(c, trigPtr, t.trig)
	for i := range t.bitrev {
		t.bitrev[i] = int32(c.Read32(bitrevPtr + uint32(i)*4))
	}
	s.tables[init] = t
	return t
}

func thunkMdctBackward(c *cpu.CPU) {
	init, in, out := c.Arg(0), c.Arg(1), c.Arg(2)
	t := mdct.lookup(c, init)
	n := t.n
	if n < 64 || n&(n-1) != 0 || uint64(in)+uint64(n)*4 > uint64(len(c.Mem)) || uint64(out)+uint64(n)*4 > uint64(len(c.Mem)) {
		c.Halted = true
		c.HaltReason = fmt.Sprintf("mdct_backward: unexpected n=%d in=0x%08x out=0x%08x", n, in, out)
		return
	}
	if cap(mdct.buf) < n+n/2 {
		mdct.buf = make([]float32, n+n/2)
	}
	src := mdct.buf[n : n+n/2]
	dst := mdct.buf[:n]
	guestF32s(c, in, src)

	if mdct.verify {
		// Run the original x86 code on the same input and compare.
		c.SetInterceptEnabled(mdct.addr, false)
		_, err := c.CallGuestNested(mdct.addr, init, in, out)
		c.SetInterceptEnabled(mdct.addr, true)
		mdctBackward(t, src, dst)
		ref := make([]float32, n)
		guestF32s(c, out, ref)
		var maxDiff, maxMag float64
		for i := range ref {
			maxDiff = math.Max(maxDiff, math.Abs(float64(ref[i]-dst[i])))
			maxMag = math.Max(maxMag, math.Abs(float64(ref[i])))
		}
		if err != nil || maxDiff > 1e-4*math.Max(1, maxMag) {
			fmt.Printf("[MDCT-VERIFY] MISMATCH n=%d maxDiff=%g maxMag=%g err=%v\n", n, maxDiff, maxMag, err)
		} else {
			mdct.verifiedCalls++
			if mdct.verifiedCalls%500 == 1 || !mdct.seenN[n] {
				mdct.seenN[n] = true
				fmt.Printf("[MDCT-VERIFY] ok call=%d n=%d maxDiff=%g maxMag=%g\n", mdct.verifiedCalls, n, maxDiff, maxMag)
			}
		}
		c.Ret()
		return
	}

	mdctBackward(t, src, dst)
	putGuestF32s(c, out, dst)
	c.Ret()
}

// mdctBackward computes the inverse MDCT of in[0:n/2] into out[0:n] (in and out must not alias).
func mdctBackward(t *mdctTables, in, out []float32) {
	n := t.n
	n2 := n >> 1
	n4 := n >> 2
	trig := t.trig

	// rotate
	iX := n2 - 7
	oX := n2 + n4
	T := n4
	for {
		oX -= 4
		out[oX+0] = -in[iX+2]*trig[T+3] - in[iX+0]*trig[T+2]
		out[oX+1] = in[iX+0]*trig[T+3] - in[iX+2]*trig[T+2]
		out[oX+2] = -in[iX+6]*trig[T+1] - in[iX+4]*trig[T+0]
		out[oX+3] = in[iX+4]*trig[T+1] - in[iX+6]*trig[T+0]
		iX -= 8
		T += 4
		if iX < 0 {
			break
		}
	}

	iX = n2 - 8
	oX = n2 + n4
	T = n4
	for {
		T -= 4
		out[oX+0] = in[iX+4]*trig[T+3] + in[iX+6]*trig[T+2]
		out[oX+1] = in[iX+4]*trig[T+2] - in[iX+6]*trig[T+3]
		out[oX+2] = in[iX+0]*trig[T+1] + in[iX+2]*trig[T+0]
		out[oX+3] = in[iX+0]*trig[T+0] - in[iX+2]*trig[T+1]
		iX -= 8
		oX += 4
		if iX < 0 {
			break
		}
	}

	mdctButterflies(t, out[n2:], n2)
	mdctBitreverse(t, out)

	// rotate + window
	oX1 := n2 + n4
	oX2 := n2 + n4
	iX = 0
	T = n2
	for {
		oX1 -= 4
		out[oX1+3] = out[iX+0]*trig[T+1] - out[iX+1]*trig[T+0]
		out[oX2+0] = -(out[iX+0]*trig[T+0] + out[iX+1]*trig[T+1])
		out[oX1+2] = out[iX+2]*trig[T+3] - out[iX+3]*trig[T+2]
		out[oX2+1] = -(out[iX+2]*trig[T+2] + out[iX+3]*trig[T+3])
		out[oX1+1] = out[iX+4]*trig[T+5] - out[iX+5]*trig[T+4]
		out[oX2+2] = -(out[iX+4]*trig[T+4] + out[iX+5]*trig[T+5])
		out[oX1+0] = out[iX+6]*trig[T+7] - out[iX+7]*trig[T+6]
		out[oX2+3] = -(out[iX+6]*trig[T+6] + out[iX+7]*trig[T+7])
		oX2 += 4
		iX += 8
		T += 8
		if iX >= oX1 {
			break
		}
	}

	iX = n2 + n4
	oX1 = n4
	oX2 = oX1
	for {
		oX1 -= 4
		iX -= 4
		out[oX1+3] = out[iX+3]
		out[oX2+0] = -out[iX+3]
		out[oX1+2] = out[iX+2]
		out[oX2+1] = -out[iX+2]
		out[oX1+1] = out[iX+1]
		out[oX2+2] = -out[iX+1]
		out[oX1+0] = out[iX+0]
		out[oX2+3] = -out[iX+0]
		oX2 += 4
		if oX2 >= iX {
			break
		}
	}

	iX = n2 + n4
	oX1 = n2 + n4
	oX2 = n2
	for {
		oX1 -= 4
		out[oX1+0] = out[iX+3]
		out[oX1+1] = out[iX+2]
		out[oX1+2] = out[iX+1]
		out[oX1+3] = out[iX+0]
		iX += 4
		if oX1 <= oX2 {
			break
		}
	}
}

func mdctButterflies(t *mdctTables, x []float32, points int) {
	T := t.trig
	stages := t.log2n - 5

	stages--
	if stages > 0 {
		mdctButterflyFirst(T, x, points)
	}
	for i := 1; ; i++ {
		stages--
		if stages <= 0 {
			break
		}
		for j := 0; j < 1<<i; j++ {
			mdctButterflyGeneric(T, x[(points>>i)*j:], points>>i, 4<<i)
		}
	}
	for j := 0; j < points; j += 32 {
		mdctButterfly32(x[j:])
	}
}

func mdctButterflyFirst(T []float32, x []float32, points int) {
	x1 := points - 8
	x2 := (points >> 1) - 8
	t := 0
	for {
		r0 := x[x1+6] - x[x2+6]
		r1 := x[x1+7] - x[x2+7]
		x[x1+6] += x[x2+6]
		x[x1+7] += x[x2+7]
		x[x2+6] = r1*T[t+1] + r0*T[t+0]
		x[x2+7] = r1*T[t+0] - r0*T[t+1]

		r0 = x[x1+4] - x[x2+4]
		r1 = x[x1+5] - x[x2+5]
		x[x1+4] += x[x2+4]
		x[x1+5] += x[x2+5]
		x[x2+4] = r1*T[t+5] + r0*T[t+4]
		x[x2+5] = r1*T[t+4] - r0*T[t+5]

		r0 = x[x1+2] - x[x2+2]
		r1 = x[x1+3] - x[x2+3]
		x[x1+2] += x[x2+2]
		x[x1+3] += x[x2+3]
		x[x2+2] = r1*T[t+9] + r0*T[t+8]
		x[x2+3] = r1*T[t+8] - r0*T[t+9]

		r0 = x[x1+0] - x[x2+0]
		r1 = x[x1+1] - x[x2+1]
		x[x1+0] += x[x2+0]
		x[x1+1] += x[x2+1]
		x[x2+0] = r1*T[t+13] + r0*T[t+12]
		x[x2+1] = r1*T[t+12] - r0*T[t+13]

		x1 -= 8
		x2 -= 8
		t += 16
		if x2 < 0 {
			break
		}
	}
}

func mdctButterflyGeneric(T []float32, x []float32, points, trigint int) {
	x1 := points - 8
	x2 := (points >> 1) - 8
	t := 0
	for {
		r0 := x[x1+6] - x[x2+6]
		r1 := x[x1+7] - x[x2+7]
		x[x1+6] += x[x2+6]
		x[x1+7] += x[x2+7]
		x[x2+6] = r1*T[t+1] + r0*T[t+0]
		x[x2+7] = r1*T[t+0] - r0*T[t+1]
		t += trigint

		r0 = x[x1+4] - x[x2+4]
		r1 = x[x1+5] - x[x2+5]
		x[x1+4] += x[x2+4]
		x[x1+5] += x[x2+5]
		x[x2+4] = r1*T[t+1] + r0*T[t+0]
		x[x2+5] = r1*T[t+0] - r0*T[t+1]
		t += trigint

		r0 = x[x1+2] - x[x2+2]
		r1 = x[x1+3] - x[x2+3]
		x[x1+2] += x[x2+2]
		x[x1+3] += x[x2+3]
		x[x2+2] = r1*T[t+1] + r0*T[t+0]
		x[x2+3] = r1*T[t+0] - r0*T[t+1]
		t += trigint

		r0 = x[x1+0] - x[x2+0]
		r1 = x[x1+1] - x[x2+1]
		x[x1+0] += x[x2+0]
		x[x1+1] += x[x2+1]
		x[x2+0] = r1*T[t+1] + r0*T[t+0]
		x[x2+1] = r1*T[t+0] - r0*T[t+1]
		t += trigint

		x1 -= 8
		x2 -= 8
		if x2 < 0 {
			break
		}
	}
}

func mdctButterfly32(x []float32) {
	r0 := x[30] - x[14]
	r1 := x[31] - x[15]
	x[30] += x[14]
	x[31] += x[15]
	x[14] = r0
	x[15] = r1

	r0 = x[28] - x[12]
	r1 = x[29] - x[13]
	x[28] += x[12]
	x[29] += x[13]
	x[12] = r0*cPI1_8 - r1*cPI3_8
	x[13] = r0*cPI3_8 + r1*cPI1_8

	r0 = x[26] - x[10]
	r1 = x[27] - x[11]
	x[26] += x[10]
	x[27] += x[11]
	x[10] = (r0 - r1) * cPI2_8
	x[11] = (r0 + r1) * cPI2_8

	r0 = x[24] - x[8]
	r1 = x[25] - x[9]
	x[24] += x[8]
	x[25] += x[9]
	x[8] = r0*cPI3_8 - r1*cPI1_8
	x[9] = r1*cPI3_8 + r0*cPI1_8

	r0 = x[22] - x[6]
	r1 = x[7] - x[23]
	x[22] += x[6]
	x[23] += x[7]
	x[6] = r1
	x[7] = r0

	r0 = x[4] - x[20]
	r1 = x[5] - x[21]
	x[20] += x[4]
	x[21] += x[5]
	x[4] = r1*cPI1_8 + r0*cPI3_8
	x[5] = r1*cPI3_8 - r0*cPI1_8

	r0 = x[2] - x[18]
	r1 = x[3] - x[19]
	x[18] += x[2]
	x[19] += x[3]
	x[2] = (r1 + r0) * cPI2_8
	x[3] = (r1 - r0) * cPI2_8

	r0 = x[0] - x[16]
	r1 = x[1] - x[17]
	x[16] += x[0]
	x[17] += x[1]
	x[0] = r1*cPI3_8 + r0*cPI1_8
	x[1] = r1*cPI1_8 - r0*cPI3_8

	mdctButterfly16(x)
	mdctButterfly16(x[16:])
}

func mdctButterfly16(x []float32) {
	r0 := x[1] - x[9]
	r1 := x[0] - x[8]
	x[8] += x[0]
	x[9] += x[1]
	x[0] = (r0 + r1) * cPI2_8
	x[1] = (r0 - r1) * cPI2_8

	r0 = x[3] - x[11]
	r1 = x[10] - x[2]
	x[10] += x[2]
	x[11] += x[3]
	x[2] = r0
	x[3] = r1

	r0 = x[12] - x[4]
	r1 = x[13] - x[5]
	x[12] += x[4]
	x[13] += x[5]
	x[4] = (r0 - r1) * cPI2_8
	x[5] = (r0 + r1) * cPI2_8

	r0 = x[14] - x[6]
	r1 = x[15] - x[7]
	x[14] += x[6]
	x[15] += x[7]
	x[6] = r0
	x[7] = r1

	mdctButterfly8(x)
	mdctButterfly8(x[8:])
}

func mdctButterfly8(x []float32) {
	r0 := x[6] + x[2]
	r1 := x[6] - x[2]
	r2 := x[4] + x[0]
	r3 := x[4] - x[0]

	x[6] = r0 + r2
	x[4] = r0 - r2

	r0 = x[5] - x[1]
	r2 = x[7] - x[3]
	x[0] = r1 + r0
	x[2] = r1 - r0

	r0 = x[5] + x[1]
	r1 = x[7] + x[3]
	x[3] = r2 + r3
	x[1] = r2 - r3
	x[7] = r1 + r0
	x[5] = r1 - r0
}

func mdctBitreverse(t *mdctTables, x []float32) {
	n := t.n
	bit := t.bitrev
	trig := t.trig
	w0 := 0
	w1 := n >> 1
	xb := n >> 1 // x = w0 + n/2
	T := n
	b := 0
	for {
		x0 := xb + int(bit[b+0])
		x1 := xb + int(bit[b+1])

		r0 := x[x0+1] - x[x1+1]
		r1 := x[x0+0] + x[x1+0]
		r2 := r1*trig[T+0] + r0*trig[T+1]
		r3 := r1*trig[T+1] - r0*trig[T+0]

		w1 -= 4

		r0 = (x[x0+1] + x[x1+1]) * .5
		r1 = (x[x0+0] - x[x1+0]) * .5

		x[w0+0] = r0 + r2
		x[w1+2] = r0 - r2
		x[w0+1] = r1 + r3
		x[w1+3] = r3 - r1

		x0 = xb + int(bit[b+2])
		x1 = xb + int(bit[b+3])

		r0 = x[x0+1] - x[x1+1]
		r1 = x[x0+0] + x[x1+0]
		r2 = r1*trig[T+2] + r0*trig[T+3]
		r3 = r1*trig[T+3] - r0*trig[T+2]

		r0 = (x[x0+1] + x[x1+1]) * .5
		r1 = (x[x0+0] - x[x1+0]) * .5

		x[w0+2] = r0 + r2
		x[w1+0] = r0 - r2
		x[w0+3] = r1 + r3
		x[w1+1] = r3 - r1

		T += 4
		b += 4
		w0 += 4
		if w0 >= w1 {
			break
		}
	}
}

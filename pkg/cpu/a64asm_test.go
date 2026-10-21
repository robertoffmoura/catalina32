package cpu

import (
	"encoding/binary"
	"testing"

	"golang.org/x/arch/arm64/arm64asm"
)

// TestA64Encodings checks the arm64 encoder against golang.org/x/arch's disassembler.
func TestA64Encodings(t *testing.T) {
	cases := []struct {
		emit func(a *a64)
		want string
	}{
		{func(a *a64) { a.addI(false, a64Add, 1, 2, 4) }, "add w1, w2, #0x4"},
		{func(a *a64) { a.addI(false, a64Subs, 31, 2, 0x1000) }, "cmp w2, #0x1, lsl #12"},
		{func(a *a64) { a.addI(true, a64Sub, 7, 7, 12) }, "sub x7, x7, #0xc"},
		{func(a *a64) { a.addI(true, a64Subs, 7, 7, 12) }, "subs x7, x7, #0xc"},
		{func(a *a64) { a.addI(false, a64Adds, 2, 19, 1) }, "adds w2, w19, #0x1"},
		{func(a *a64) { a.addR(false, a64Adds, 3, 2, 4, a64LSL, 24) }, "adds w3, w2, w4, lsl #24"},
		{func(a *a64) { a.addR(false, a64Sub, 0, 23, 25, a64LSL, 2) }, "sub w0, w23, w25, lsl #2"},
		{func(a *a64) { a.addR(false, a64Subs, 31, 19, 20, a64LSL, 0) }, "cmp w19, w20"},
		{func(a *a64) { a.addX(true, a64Subs, 31, 2, 2, 6, 0) }, "cmp x2, w2, sxtw"},
		{func(a *a64) { a.addR(false, a64Subs, 2, 31, 19, a64LSL, 0) }, "negs w2, w19"},
		{func(a *a64) { a.logR(false, a64Ands, 31, 2, 2, a64LSL, 0) }, "tst w2, w2"},
		{func(a *a64) { a.logR(false, a64Bic, 8, 8, 14, a64LSL, 0) }, "bic w8, w8, w14"},
		{func(a *a64) { a.logR(false, a64Eor, 13, 8, 8, a64LSR, 4) }, "eor w13, w8, w8, lsr #4"},
		{func(a *a64) { a.logR(false, a64Orr, 13, 13, 8, a64LSL, 1) }, "orr w13, w13, w8, lsl #1"},
		{func(a *a64) { a.logI(a64AndI, 14, 2, 0xFF) }, "and w14, w2, #0xff"},
		{func(a *a64) { a.logI(a64AndI, 14, 2, 0x10) }, "and w14, w2, #0x10"},
		{func(a *a64) { a.logI(a64OrrI, 13, 13, 0x30) }, "orr w13, w13, #0x30"},
		{func(a *a64) { a.logI(a64AndsI, 31, 8, 0x800) }, "tst w8, #0x800"},
		{func(a *a64) { a.logI(a64EorI, 1, 2, 0xFFFF00FF) }, "eor w1, w2, #0xffff00ff"},
		{func(a *a64) { a.logI(a64OrrI, 1, 2, 0xFFFFFF) }, "orr w1, w2, #0xffffff"},
		{func(a *a64) { a.logI(a64AndI, 1, 2, 0x55555555) }, "and w1, w2, #0x55555555"},
		{func(a *a64) { a.logI(a64AndI, 1, 2, 0x80000001) }, "and w1, w2, #0x80000001"},
		{func(a *a64) { a.movI(3, 0x1234) }, "mov w3, #0x1234"},
		{func(a *a64) { a.movI(3, 0x12340000) }, "mov w3, #0x12340000"},
		{func(a *a64) { a.movI(3, 0xFFFFFFF8) }, "mov w3, #0xfffffff8"},
		{func(a *a64) { a.movI(3, 0xFFF8FFFF) }, "mov w3, #0xfff8ffff"},
		{func(a *a64) { a.movI(3, 0xBFFFFFF8) }, "mov w3, #0xfff8\nmovk w3, #0xbfff, lsl #16"},
		{func(a *a64) { a.movI64(3, 0x1234_0000_5678) }, "mov x3, #0x5678\nmovk x3, #0x1234, lsl #32"},
		{func(a *a64) { a.mov(19, 2) }, "mov w19, w2"},
		{func(a *a64) { a.mov64(27, 0) }, "mov x27, x0"},
		{func(a *a64) { a.lslI(2, 19, 24) }, "lsl w2, w19, #24"},
		{func(a *a64) { a.lsrI(2, 19, 24) }, "lsr w2, w19, #24"},
		{func(a *a64) { a.asrI(2, 19, 3) }, "asr w2, w19, #3"},
		{func(a *a64) { a.ubfx(2, 19, 8, 8) }, "ubfx w2, w19, #8, #8"},
		{func(a *a64) { a.bfi(19, 2, 8, 8) }, "bfi w19, w2, #8, #8"},
		{func(a *a64) { a.bfi(19, 2, 0, 16) }, "bfxil w19, w2, #0, #16"},
		{func(a *a64) { a.rorI(2, 19, 5) }, "ror w2, w19, #5"},
		{func(a *a64) { a.lslI64(13, 1, 3) }, "lsl x13, x1, #3"},
		{func(a *a64) { a.lsrI64(13, 1, 30) }, "lsr x13, x1, #30"},
		{func(a *a64) { a.bfm64(1, 21, 32, 31) }, "bfi x1, x21, #32, #32"},
		{func(a *a64) { a.op2(a64Lslv, 2, 19, 13) }, "lsl w2, w19, w13"},
		{func(a *a64) { a.op2(a64Rorv, 2, 19, 13) }, "ror w2, w19, w13"},
		{func(a *a64) { a.op2(a64Adcs, 2, 19, 20) }, "adcs w2, w19, w20"},
		{func(a *a64) { a.op2(a64Sbcs, 2, 19, 20) }, "sbcs w2, w19, w20"},
		{func(a *a64) { a.smull(2, 19, 20) }, "smull x2, w19, w20"},
		{func(a *a64) { a.rev(19, 19) }, "rev w19, w19"},
		{func(a *a64) { a.csel(19, 2, 19, a64NE) }, "csel w19, w2, w19, ne"},
		{func(a *a64) { a.cset(13, a64NE) }, "cset w13, ne"},
		{func(a *a64) { a.csinc(13, 13, a64ZR, a64VC) }, "csinc w13, w13, wzr, vc"},
		{func(a *a64) { a.mrsNZCV(13) }, "mrs x13, s3_3_c4_c2_0"}, // NZCV
		{func(a *a64) { a.msrNZCV(13) }, "msr s3_3_c4_c2_0, x13"},
		{func(a *a64) { a.ldstU(2, a64Ld, false, 19, 27, 0x40) }, "ldr w19, [x27,#64]"},
		{func(a *a64) { a.ldstU(3, a64St, false, 19, 0, 0x48) }, "str x19, [x0,#72]"},
		{func(a *a64) { a.ldstU(4, a64Ld, true, 0, 27, 0x80) }, "ldr q0, [x27,#128]"},
		{func(a *a64) { a.ldstU(4, a64St, true, 0, 27, 0x80) }, "str q0, [x27,#128]"},
		{func(a *a64) { a.ldstU(2, a64Ld, true, 1, 27, 0x84) }, "ldr s1, [x27,#132]"},
		{func(a *a64) { a.ldstU(3, a64St, true, 1, 27, 0x88) }, "str d1, [x27,#136]"},
		{func(a *a64) { a.ldstR(2, a64Ld, false, 19, 9, 0) }, "ldr w19, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(0, a64St, false, 19, 9, 0) }, "strb w19, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(1, a64LdS, false, 19, 9, 0) }, "ldrsh w19, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(0, a64LdS, false, 19, 9, 0) }, "ldrsb w19, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(3, a64Ld, false, 2, 9, 0) }, "ldr x2, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(4, a64Ld, true, 1, 9, 0) }, "ldr q1, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(4, a64St, true, 1, 9, 0) }, "str q1, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(2, a64Ld, true, 1, 9, 0) }, "ldr s1, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldstR(3, a64St, true, 1, 9, 0) }, "str d1, [x9,w0,uxtw]"},
		{func(a *a64) { a.ldrbIdx(14, 11, 14) }, "ldrb w14, [x11,x14]"},
		{func(a *a64) { a.ldstX(3, a64Ld, true, 0, 17, 16) }, "ldr d0, [x17,x16,lsl #3]"},
		{func(a *a64) { a.ldstX(3, a64St, true, 1, 17, 15) }, "str d1, [x17,x15,lsl #3]"},
		{func(a *a64) { a.ldstX(3, a64St, false, 13, 17, 15) }, "str x13, [x17,x15,lsl #3]"},
		{func(a *a64) { a.fneg(0, 0) }, "fneg d0, d0"},
		{func(a *a64) { a.fabs(0, 1) }, "fabs d0, d1"},
		{func(a *a64) { a.ldrhIdx(13, 12, 13) }, "ldrh w13, [x12,x13,lsl #1]"},
		{func(a *a64) { a.stp(19, 20, 0, 0x50) }, "stp x19, x20, [x0,#80]"},
		{func(a *a64) { a.ldp(15, 16, 14, 0) }, "ldp x15, x16, [x14]"},
		{func(a *a64) { a.fop(a64Fadd, false, 0, 0, 1) }, "fadd s0, s0, s1"},
		{func(a *a64) { a.fop(a64Fdiv, true, 0, 0, 1) }, "fdiv d0, d0, d1"},
		{func(a *a64) { a.fop(a64Fsub, true, 0, 0, 1) }, "fsub d0, d0, d1"},
		{func(a *a64) { a.fop(a64Fmul, false, 0, 0, 1) }, "fmul s0, s0, s1"},
		{func(a *a64) { a.fsqrt(true, 0, 0) }, "fsqrt d0, d0"},
		{func(a *a64) { a.fcmp(false, 0, 1) }, "fcmp s0, s1"},
		{func(a *a64) { a.fcmp(true, 0, 1) }, "fcmp d0, d1"},
		{func(a *a64) { a.fcsel(false, 0, 0, 1, a64MI) }, "fcsel s0, s0, s1, mi"},
		{func(a *a64) { a.fcvtS2D(0, 1) }, "fcvt d0, s1"},
		{func(a *a64) { a.fcvtD2S(0, 1) }, "fcvt s0, d1"},
		{func(a *a64) { a.fcvtzs64(false, 1, 0) }, "fcvtzs x1, s0"},
		{func(a *a64) { a.fcvtzs64(true, 1, 0) }, "fcvtzs x1, d0"},
		{func(a *a64) { a.scvtf(false, 0, 19) }, "scvtf s0, w19"},
		{func(a *a64) { a.scvtf(true, 0, 19) }, "scvtf d0, w19"},
		{func(a *a64) { a.fmovToW(1, 0) }, "fmov w1, s0"},
		{func(a *a64) { a.fmovFromW(0, 1) }, "fmov s0, w1"},
		{func(a *a64) { a.vop(a64VFadd, 0, 0, 1) }, "fadd v0.4s, v0.4s, v1.4s"},
		{func(a *a64) { a.vop(a64VFsub, 0, 0, 1) }, "fsub v0.4s, v0.4s, v1.4s"},
		{func(a *a64) { a.vop(a64VFmul, 0, 0, 1) }, "fmul v0.4s, v0.4s, v1.4s"},
		{func(a *a64) { a.vop(a64VFdiv, 0, 0, 1) }, "fdiv v0.4s, v0.4s, v1.4s"},
		{func(a *a64) { a.vop(a64VFcmeq, 2, 0, 0) }, "fcmeq v2.4s, v0.4s, v0.4s"},
		{func(a *a64) { a.vop(a64VFcmgt, 2, 1, 0) }, "fcmgt v2.4s, v1.4s, v0.4s"},
		{func(a *a64) { a.vop(a64VAnd, 0, 0, 1) }, "and v0.16b, v0.16b, v1.16b"},
		{func(a *a64) { a.vop(a64VBic, 0, 1, 0) }, "bic v0.16b, v1.16b, v0.16b"},
		{func(a *a64) { a.vop(a64VOrr, 0, 0, 1) }, "orr v0.16b, v0.16b, v1.16b"},
		{func(a *a64) { a.vop(a64VEor, 0, 0, 1) }, "eor v0.16b, v0.16b, v1.16b"},
		{func(a *a64) { a.vop(a64VBsl, 2, 0, 1) }, "bsl v2.16b, v0.16b, v1.16b"},
		{func(a *a64) { a.vop(a64VBif, 1, 0, 2) }, "bif v1.16b, v0.16b, v2.16b"},
		{func(a *a64) { a.vfsqrt(0, 1) }, "fsqrt v0.4s, v1.4s"},
		{func(a *a64) { a.vop(a64VFcmge, 2, 1, 0) }, "fcmge v2.4s, v1.4s, v0.4s"},
		{func(a *a64) { a.vop(a64VZip1, 0, 0, 1) }, "zip1 v0.4s, v0.4s, v1.4s"},
		{func(a *a64) { a.vnot(2, 2) }, "mvn v2.16b, v2.16b"},
		{func(a *a64) { a.insS(0, 3, 1, 2) }, "mov v0.s[3], v1.s[2]"},
		{func(a *a64) { a.br(16) }, "br x16"},
		{func(a *a64) { a.emit(0x1B000000 | 4<<16 | 31<<10 | 19<<5 | 16) }, "mul w16, w19, w4"}, // endReg
		// jitFlushICache (jit_arm64.s)
		{func(a *a64) { a.emit(0xD53B0022) }, "mrs x2, s3_3_c0_c0_1"}, // CTR_EL0
		{func(a *a64) { a.emit(0xD50B7B23) }, "dc cvau, x3"},
		// (IC IVAU, X3 = 0xD50B7523 is SYS #3, C7, C5, #1, X3: unknown to the disassembler)
		{func(a *a64) { a.emit(0xD5033B9F) }, "dsb ish"},
		{func(a *a64) { a.emit(0xD5033FDF) }, "isb"},
		{func(a *a64) { a.ret() }, "ret"},
	}
	for _, tc := range cases {
		var a a64
		tc.emit(&a)
		got := ""
		for i, w := range a.w {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], w)
			inst, err := arm64asm.Decode(b[:])
			if i > 0 {
				got += "\n"
			}
			if err != nil {
				got += "?" + err.Error()
				continue
			}
			got += arm64asm.GNUSyntax(inst)
		}
		if got != tc.want {
			t.Errorf("got %q, want %q (%08x)", got, tc.want, a.w)
		}
	}
	// Branches to labels.
	var a a64
	l := a.newLabel()
	a.bind(l)
	a.emit(0xD503201F) // nop
	a.b(l)
	a.bcond(a64HI, l)
	a.cbnz(13, l)
	a.tbnz(8, 11, l)
	a.tbz(8, 6, l)
	a.resolve()
	want := []string{"nop ", "b .+0xfffffffffffffffc", "b.hi .+0xfffffffffffffff8", "cbnz w13, .+0xfffffffffffffff4",
		"tbnz w8, #11, .+0xfffffffffffffff0", "tbz w8, #6, .+0xffffffffffffffec"} // -4, -8, ...
	for i, w := range a.w {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], w)
		inst, err := arm64asm.Decode(b[:])
		if err != nil {
			t.Fatalf("%08x: %v", w, err)
		}
		if g := arm64asm.GNUSyntax(inst); g != want[i] {
			t.Errorf("branch %d: got %q, want %q", i, g, want[i])
		}
	}
}

// TestA64BitMask checks the logical-immediate encoder against a brute-force decoder.
func TestA64BitMask(t *testing.T) {
	decode := func(immr, imms uint32) uint32 {
		size := uint32(32)
		for size > 1 && imms&(size) != 0 { // the element size is set by the leading ones of imms
			size >>= 1
		}
		var e uint32
		for i := uint32(0); i <= imms&(size-1); i++ {
			e |= 1 << i
		}
		mask := uint32(1)<<size - 1
		if size == 32 {
			mask = 0xFFFFFFFF
		}
		e = (e>>immr | e<<(size-immr)) & mask
		var v uint32
		for s := uint32(0); s < 32; s += size {
			v |= e << s
		}
		return v
	}
	for _, v := range []uint32{1, 0x10, 0x30, 0xFF, 0x80000001, 0x55555555, 0xFFFF00FF, 0xFFFFFFFE, 0x0F0F0F0F, 0x7FFFFFFF, 0x800} {
		immr, imms, ok := a64BitMask(v)
		if !ok {
			t.Errorf("%#x: not encodable", v)
			continue
		}
		if d := decode(immr, imms); d != v {
			t.Errorf("%#x: encoded as immr %d imms %#x, which is %#x", v, immr, imms, d)
		}
	}
	for _, v := range []uint32{0, 0xFFFFFFFF, 0x41, 0x8D5, 0x12345678} {
		if _, _, ok := a64BitMask(v); ok {
			t.Errorf("%#x: encodable?", v)
		}
	}
}

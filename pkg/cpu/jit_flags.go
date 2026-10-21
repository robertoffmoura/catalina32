package cpu

// What the JIT backends share about guest flags: which flags each cached form reads and
// writes, and which flag results translated code must keep (jitCaptures).

const (
	flagsArith = FlagCF | FlagPF | FlagAF | FlagZF | FlagSF | FlagOF
	flagsLogic = flagsArith &^ FlagAF // logic ops: AF is 0 in the interpreter, undefined in hardware
)

// jitFx is how an instruction uses the guest flags.
type jitFx struct {
	writes       bool   // sets flags (value from the host's, the rest of clear zeroed)
	value, clear uint32 //
	dynamic      bool   // whether it sets them depends on run-time values (shift by CL): it merges them itself
	reads        bool   // reads flags
	native       bool   // can read them from the host's flags right after the instruction that set them
	cc           uint8  // the condition it reads (native readers)
}

func jitFlagFx(d *dInst) jitFx {
	switch d.kind {
	case dAluRMR32, dAluRMR32R, dAluRRM32, dAluRRM32R, dAluEAXImm, dGrp1RM32, dGrp1RM32R, dGrp1RM8,
		dAlu8RMR, dAlu8RRM, dAluALImm, dAlu16RMR, dAlu16RRM, dGrp1RM16:
		v, c := aluFlags(d.aux)
		return jitFx{writes: true, value: v, clear: c, reads: d.aux == 2 || d.aux == 3}
	case dTestRMR32, dTestRMI8, dTestRMR8, dTestALImm, dTest16, dTestRMI32:
		return jitFx{writes: true, value: flagsLogic, clear: flagsArith}
	case dIncR, dDecR, dIncRM32, dDecRM32:
		return jitFx{writes: true, value: flagsArith &^ FlagCF, clear: flagsArith &^ FlagCF}
	case dNeg32, dNeg8, dUcomiss, dUcomisd, dCmpxchg, dXadd:
		return jitFx{writes: true, value: flagsArith, clear: flagsArith}
	case dImul32, dImulRRM32, dMul1:
		return jitFx{writes: true, value: FlagCF | FlagOF, clear: FlagCF | FlagOF}
	case dBt:
		return jitFx{writes: true, value: FlagCF, clear: FlagCF}
	case dCmpxchg8b:
		return jitFx{writes: true, value: FlagZF, clear: FlagZF}
	case dShift32:
		cnt := uint8(d.imm) & 0x1F
		if cnt == 0 {
			return jitFx{}
		}
		v, c := shiftFlags(d.aux, cnt)
		return jitFx{writes: true, value: v, clear: c, reads: d.aux == 2 || d.aux == 3}
	case dShift32CL:
		return jitFx{writes: true, dynamic: true, reads: d.aux == 2 || d.aux == 3}
	case dJcc:
		return jitFx{reads: true, native: true, cc: d.aux}
	case dCmov, dSetcc:
		return jitFx{reads: true, native: !d.isMem, cc: d.aux}
	case dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dCmpEAXJcc, dCmpRMI8Jcc:
		return jitFx{writes: true, value: flagsArith, clear: flagsArith}
	case dTestRMRJcc, dTestRMIJcc:
		return jitFx{writes: true, value: flagsLogic, clear: flagsArith}
	}
	return jitFx{}
}

// shiftFlags is what a shift or rotate by cnt (1..31) sets: rotates only CF (and OF for a
// count of 1, rol); shifts CF, PF, ZF, SF and OF for a count of 1, zeroing AF and otherwise OF.
func shiftFlags(op uint8, cnt uint8) (uint32, uint32) {
	of := uint32(0)
	if cnt == 1 {
		of = FlagOF
	}
	switch op {
	case 0:
		return FlagCF | of, FlagCF | of
	case 1, 2, 3:
		return FlagCF, FlagCF
	}
	return FlagCF | FlagPF | FlagZF | FlagSF | of, flagsArith
}

// ccFlags is the flags condition cc tests.
func ccFlags(cc uint8) uint32 {
	return [8]uint32{FlagOF, FlagCF, FlagZF, FlagCF | FlagZF, FlagSF, FlagPF, FlagSF | FlagOF, FlagZF | FlagSF | FlagOF}[cc>>1&7]
}

func aluFlags(op uint8) (uint32, uint32) {
	if op == 1 || op == 4 || op == 6 { // or, and, xor
		return flagsLogic, flagsArith
	}
	return flagsArith, flagsArith
}

// jitReads is the flags f reads (all of them when it isn't known which).
func jitReads(f jitFx) uint32 {
	if f.cc != 0 || f.native {
		return ccFlags(f.cc)
	}
	return flagsArith
}

// jitCaptures decides, for each instruction setting flags, whether they must be saved: yes if
// something may read them before instructions after it overwrite them: an instruction reading
// flags other than straight from the host's (native: whether instruction k can read
// instruction i's flags from the host's), or, past the block's end, liveOut (the flags the
// code after it may read). An instruction that may leave for the interpreter (exits) needs them
// only if it reads them: one that doesn't runs there with the earlier flags, and if it halts
// the guest (a crash) the flags it reports may be those earlier ones.
func jitCaptures(insts []dInst, exits []bool, liveOut uint32, native func(i, k int) bool) []bool {
	n := len(insts)
	capture := make([]bool, n)
	for i := range insts {
		f := jitFlagFx(&insts[i])
		if !f.writes || f.dynamic {
			continue
		}
		rem := f.clear // the flags i sets that no later instruction has set again
		for k := i + 1; ; k++ {
			if k == n {
				capture[i] = rem&liveOut != 0
				break
			}
			fk := jitFlagFx(&insts[k])
			if fk.reads && jitReads(fk)&rem != 0 && !(fk.native && k == i+1 && !exits[k] && native(i, k)) {
				capture[i] = true
				break
			}
			if fk.writes {
				if fk.dynamic {
					capture[i] = true
					break
				}
				if rem &^= fk.clear; rem == 0 {
					break
				}
			}
		}
	}
	return capture
}

// jitLiveOut is the flags the code after block insts (which ends before end) may read before
// setting them: what its successors read first, when they are known (a branch or call to an
// address in the code range), else all of them.
func (c *CPU) jitLiveOut(insts []dInst, end uint32) uint32 {
	last := &insts[len(insts)-1]
	var next []uint32
	switch {
	case last.kind == dJcc:
		next = []uint32{end, last.imm}
	case isFused(last.kind):
		next = []uint32{end, last.target}
	case last.kind == dJmp || last.kind == dCall:
		next = []uint32{last.imm}
	case endsBlock(last.kind):
		return flagsArith // ret, indirect
	default:
		next = []uint32{end} // the block stopped before an instruction it couldn't hold
	}
	live := uint32(0)
	for _, eip := range next {
		live |= c.jitFlagsRead(eip)
	}
	return live
}

// jitFlagsRead is the flags straight-line code at eip may read before setting them.
func (c *CPU) jitFlagsRead(eip uint32) uint32 {
	rem, live := uint32(flagsArith), uint32(0)
	for n := 0; n < 16; n++ {
		if eip-c.CodeLo >= c.CodeHi-c.CodeLo || eip > ArenaSize-maxInstLen || c.icept != nil && c.icept[eip>>6]&(1<<(eip&63)) != 0 {
			break // outside the code range (code there may change), or a host function
		}
		d := &c.dcache[eip&dcacheMask]
		if d.tag != eip {
			c.decodeCached(d, eip)
		}
		if d.kind == dSlow {
			break // the full decoder's: anything
		}
		f := jitFlagFx(d)
		if f.reads {
			live |= jitReads(f) & rem
		}
		if f.writes && !f.dynamic {
			if rem &^= f.clear; rem == 0 {
				return live
			}
		}
		if endsBlock(d.kind) {
			break
		}
		eip += uint32(d.len)
	}
	return live | rem
}

// jitWrites is the guest registers instruction d may write (a bit per register; all of them for
// forms not listed).
func jitWrites(d *dInst) uint8 {
	const eax, edx, esp = 1 << RegEAX, 1 << RegEDX, 1 << RegESP
	reg, rm := uint8(1)<<d.reg, uint8(1)<<d.rm
	if d.isMem {
		rm = 0
	}
	switch d.kind {
	case dMovStore32, dMovStore32R, dMovImm32, dGrp1RM32, dGrp1RM32R, dShift32, dShift32CL, dIncRM32, dDecRM32,
		dNot32, dNeg32, dMovdFromX, dMov16Store, dMov16Imm, dGrp1RM16:
		if (d.kind == dGrp1RM32 || d.kind == dGrp1RM32R || d.kind == dGrp1RM16) && d.aux == 7 {
			return 0 // cmp
		}
		return rm
	case dAluRMR32, dAluRMR32R, dAlu16RMR:
		if d.aux == 7 {
			return 0
		}
		return rm
	case dAluRRM32, dAluRRM32R, dAlu16RRM:
		if d.aux == 7 {
			return 0
		}
		return reg
	case dAluEAXImm, dAluALImm:
		if d.aux == 7 {
			return 0
		}
		return eax
	case dMovLoad32, dMovLoad32R, dMovRImm32, dLea, dIncR, dDecR, dMovzx8, dMovzx16, dMovsx8, dMovsx16, dCmov,
		dImul32, dImulRRM32, dCvttSS2SI, dCvttSD2SI, dMovmskps, dMov16Load, dBswap:
		return reg
	case dPopR:
		return reg | esp
	case dPushR, dPushImm, dPushRM32, dCall, dCallInd, dRet, dRetImm:
		return esp
	case dXchg, dXadd:
		return reg | rm
	case dCmpxchg:
		return eax | rm
	case dMul1, dCmpxchg8b:
		return eax | edx
	case dMovStore8, dMovLoad8, dMovImm8, dGrp1RM8, dAlu8RMR, dAlu8RRM, dMovR8Imm, dNot8, dNeg8, dSetcc:
		return 0xF // AL..BL, AH..BH: EAX..EBX
	case dJcc, dJmp, dJmpInd, dNop, dTestRMR32, dTestRMI8, dTestRMR8, dTestALImm, dTest16, dTestRMI32, dBt,
		dCmpRMRJcc, dCmpRRMJcc, dCmpRMIJcc, dCmpEAXJcc, dTestRMRJcc, dTestRMIJcc, dCmpRMI8Jcc,
		dMovssLoad, dMovssStore, dMovsdLoad, dMovsdStore, dArithSS, dArithSS2, dArithSD, dArithPS, dLogicPS,
		dMovapsLoad, dMovapsStore, dUcomiss, dUcomisd, dXorps, dCvtSS2SD, dCvtSD2SS, dCvtSI2SS, dCvtSI2SD,
		dPshufd, dMovdToX, dUnpcklps, dCmpps, dShufps, dMovlps, dSSE, dSSEStore,
		dFld, dFst, dFxch, dFsign, dFArith:
		return 0
	}
	return 0xFF
}

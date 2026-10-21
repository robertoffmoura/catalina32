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
	case dTestRMR32, dTestRMI8, dTestRMR8, dTestALImm, dTest16:
		return jitFx{writes: true, value: flagsLogic, clear: flagsArith}
	case dIncR, dDecR, dIncRM32, dDecRM32:
		return jitFx{writes: true, value: flagsArith &^ FlagCF, clear: flagsArith &^ FlagCF}
	case dNeg32, dNeg8, dUcomiss, dUcomisd:
		return jitFx{writes: true, value: flagsArith, clear: flagsArith}
	case dImul32, dImulRRM32:
		return jitFx{writes: true, value: FlagCF | FlagOF, clear: FlagCF | FlagOF}
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

// jitCaptures decides, for each instruction setting flags, whether they must be saved: yes if
// anything may observe them before an instruction overwrites them all (an instruction reading
// flags other than straight from the host's (native: whether instruction k can read instruction
// i's flags from the host's), a slow exit, the block's end).
func jitCaptures(insts []dInst, exits []bool, native func(i, k int) bool) []bool {
	n := len(insts)
	capture := make([]bool, n)
	for i := range insts {
		f := jitFlagFx(&insts[i])
		if !f.writes || f.dynamic {
			continue
		}
		for k := i + 1; ; k++ {
			if k == n || exits[k] {
				capture[i] = true
				break
			}
			fk := jitFlagFx(&insts[k])
			if fk.reads && !(fk.native && k == i+1 && native(i, k)) {
				capture[i] = true
				break
			}
			if fk.writes {
				if fk.dynamic || fk.clear&f.clear != f.clear {
					capture[i] = true // merged before it (it doesn't overwrite them all)
				}
				break
			}
		}
	}
	return capture
}

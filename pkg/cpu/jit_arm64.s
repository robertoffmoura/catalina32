#include "textflag.h"

// func jitCall(prologue uintptr, c *CPU, target uintptr)
// Translated code is entered by a call (BLR; it returns with RET) and uses no stack. The frame
// makes the assembler save LR (and the frame pointer) around the call.
TEXT ·jitCall(SB), NOSPLIT, $16-24
	MOVD	prologue+0(FP), R2
	MOVD	c+8(FP), R0
	MOVD	target+16(FP), R1
	CALL	(R2)
	RET

// func jitFlushICache(addr, n uintptr)
// Makes code written at [addr, addr+n) visible to instruction fetch: clean the data cache lines
// to the point of unification, invalidate the instruction cache lines, then synchronize.
TEXT ·jitFlushICache(SB), NOSPLIT, $0-16
	MOVD	addr+0(FP), R0
	MOVD	n+8(FP), R1
	ADD	R0, R1, R1
	WORD	$0xD53B0022 // MRS R2, CTR_EL0
	UBFX	$16, R2, $4, R4 // DminLine: log2 of the data cache line in words
	MOVD	$4, R5
	LSL	R4, R5, R4
	SUB	$1, R4, R6
	BIC	R6, R0, R3
dloop:
	WORD	$0xD50B7B23 // DC CVAU, R3
	ADD	R4, R3, R3
	CMP	R1, R3
	BLO	dloop
	WORD	$0xD5033B9F // DSB ISH
	AND	$15, R2, R4 // IminLine
	MOVD	$4, R5
	LSL	R4, R5, R4
	SUB	$1, R4, R6
	BIC	R6, R0, R3
iloop:
	WORD	$0xD50B7523 // IC IVAU, R3
	ADD	R4, R3, R3
	CMP	R1, R3
	BLO	iloop
	WORD	$0xD5033B9F // DSB ISH
	WORD	$0xD5033FDF // ISB
	RET

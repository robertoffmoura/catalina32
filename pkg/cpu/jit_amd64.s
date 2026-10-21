#include "textflag.h"

// func jitCall(prologue uintptr, c *CPU, target uintptr)
// Translated code is entered by a call (it returns with RET) and uses no stack besides the
// return address and the 8 bytes of a PUSHFQ/POPFQ.
TEXT ·jitCall(SB), NOSPLIT, $0-24
	MOVQ	prologue+0(FP), AX
	MOVQ	c+8(FP), DI
	MOVQ	target+16(FP), SI
	CALL	AX
	RET

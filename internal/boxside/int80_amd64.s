#include "textflag.h"

// func int80(trap, a1, a2, a3 uintptr) uintptr
//
// Issues a 32-bit system call through the int 0x80 gate from 64-bit code,
// for the self-test of the socketcall rule. Only called in a child process.
TEXT ·int80(SB),NOSPLIT,$0-40
	MOVQ trap+0(FP), AX
	MOVQ a1+8(FP), BX
	MOVQ a2+16(FP), CX
	MOVQ a3+24(FP), DX
	INT $0x80
	MOVQ AX, ret+32(FP)
	RET

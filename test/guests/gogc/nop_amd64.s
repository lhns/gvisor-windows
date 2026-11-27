#include "textflag.h"

// An assembly function: Go reaches it through an ABI wrapper that reloads g (R14)
// from %fs:-8 after the call, the access that fails when the thread's %fs base is lost.
TEXT ·nop(SB),NOSPLIT,$0-0
	RET

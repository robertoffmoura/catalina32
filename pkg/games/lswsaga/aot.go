package lswsaga

import "catalina32/pkg/cpu"

// registerAOT installs the compiled functions; aot_gen.go (generated from the user's own copy of
// the game, never committed: it is a translation of the game's code) replaces this no-op.
var registerAOT = func(*cpu.CPU) {}

// Hottest guest functions in Episode I gameplay (C32_GUEST_PROF), compiled ahead of time by
// cmd/c32aot into aot_gen.go. Regenerate after changing the list:
//
//go:generate go run ../../../cmd/c32aot -game "${C32_BIN_PATH}" -funcs 0x0007ab10,0x0029ce40,0x00075380,0x0007a2b0,0x00044810,0x00042f90,0x0003e9b0,0x0029c340,0x0002cee0,0x0002ed30,0x0003c4f0,0x000334f0,0x000212b0,0x0016e2d0,0x008c53d0,0x008d91c0,0x00232240,0x0006e6e0,0x00105f50,0x000280b0,0x00041ac0,0x0016e740,0x0029b300,0x00298080,0x00027860,0x0006f530,0x002264b0,0x002308e0,0x00043c20,0x00171c20,0x00076ea0,0x000bc0c0,0x008c0660,0x0029a620,0x0003f540,0x00297a20,0x002968c0,0x00037820,0x00143a20 -pkg lswsaga -out aot_gen.go

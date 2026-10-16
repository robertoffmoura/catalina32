package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"

	"lsw64/pkg/macho32"
)

var reg32 = [8]string{"eax", "ecx", "edx", "ebx", "esp", "ebp", "esi", "edi"}
var reg16 = [8]string{"ax", "cx", "dx", "bx", "sp", "bp", "si", "di"}
var reg8 = [8]string{"al", "cl", "dl", "bl", "ah", "ch", "dh", "bh"}
var aluNames = [8]string{"add", "or", "adc", "sbb", "and", "sub", "xor", "cmp"}
var shiftNames = [8]string{"rol", "ror", "rcl", "rcr", "shl", "shr", "sal", "sar"}
var condNames = [16]string{"o", "no", "b", "ae", "e", "ne", "be", "a", "s", "ns", "p", "np", "l", "ge", "le", "g"}

func decodeModRM(mem []byte, pc uint32, sz int) (string, int, uint32) {
	modrm := mem[pc]
	pc++
	mod := (modrm >> 6) & 3
	reg := int((modrm >> 3) & 7)
	rm := int(modrm & 7)
	if mod == 3 {
		switch sz {
		case 1:
			return reg8[rm], reg, pc
		case 2:
			return reg16[rm], reg, pc
		default:
			return reg32[rm], reg, pc
		}
	}
	var expr string
	if rm == 4 {
		sib := mem[pc]
		pc++
		scale := 1 << ((sib >> 6) & 3)
		idx := int((sib >> 3) & 7)
		base := int(sib & 7)
		if base == 5 && mod == 0 {
			disp := binary.LittleEndian.Uint32(mem[pc : pc+4])
			pc += 4
			expr = fmt.Sprintf("0x%x", disp)
		} else {
			expr = reg32[base]
		}
		if idx != 4 {
			if scale > 1 {
				expr += fmt.Sprintf("+%s*%d", reg32[idx], scale)
			} else {
				expr += fmt.Sprintf("+%s", reg32[idx])
			}
		}
	} else if rm == 5 && mod == 0 {
		disp := binary.LittleEndian.Uint32(mem[pc : pc+4])
		pc += 4
		expr = fmt.Sprintf("0x%x", disp)
	} else {
		expr = reg32[rm]
	}
	if mod == 1 {
		d := int8(mem[pc])
		pc++
		if d < 0 {
			expr += fmt.Sprintf("-0x%x", -int(d))
		} else if d > 0 {
			expr += fmt.Sprintf("+0x%x", int(d))
		}
	} else if mod == 2 {
		d := int32(binary.LittleEndian.Uint32(mem[pc : pc+4]))
		pc += 4
		if d < 0 {
			expr += fmt.Sprintf("-0x%x", -int64(d))
		} else if d > 0 {
			expr += fmt.Sprintf("+0x%x", int64(d))
		}
	}
	prefix := "dword"
	if sz == 1 {
		prefix = "byte"
	} else if sz == 2 {
		prefix = "word"
	}
	return fmt.Sprintf("%s [%s]", prefix, expr), reg, pc
}

func disasmRange(mem []byte, start, end uint32) {
	pc := start
	for pc < end {
		insnStart := pc
		opSize16 := false
		repF3 := false
		repF2 := false
		var op uint8
		for {
			op = mem[pc]
			pc++
			if op == 0x66 {
				opSize16 = true
				continue
			}
			if op == 0xF3 {
				repF3 = true
				continue
			}
			if op == 0xF2 {
				repF2 = true
				continue
			}
			if op == 0x2E || op == 0x3E || op == 0x26 || op == 0x36 {
				continue
			}
			break
		}
		sz := 4
		rnames := reg32[:]
		if opSize16 {
			sz = 2
			rnames = reg16[:]
		}
		var text string
		switch {
		case op < 0x40 && (op&7) < 6:
			alu := aluNames[(op>>3)&7]
			switch op & 7 {
			case 0:
				rm, r, next := decodeModRM(mem, pc, 1)
				pc = next
				text = fmt.Sprintf("%s %s, %s", alu, rm, reg8[r])
			case 1:
				rm, r, next := decodeModRM(mem, pc, sz)
				pc = next
				text = fmt.Sprintf("%s %s, %s", alu, rm, rnames[r])
			case 2:
				rm, r, next := decodeModRM(mem, pc, 1)
				pc = next
				text = fmt.Sprintf("%s %s, %s", alu, reg8[r], rm)
			case 3:
				rm, r, next := decodeModRM(mem, pc, sz)
				pc = next
				text = fmt.Sprintf("%s %s, %s", alu, rnames[r], rm)
			case 4:
				imm := mem[pc]
				pc++
				text = fmt.Sprintf("%s al, 0x%x", alu, imm)
			case 5:
				if opSize16 {
					imm := binary.LittleEndian.Uint16(mem[pc : pc+2])
					pc += 2
					text = fmt.Sprintf("%s ax, 0x%x", alu, imm)
				} else {
					imm := binary.LittleEndian.Uint32(mem[pc : pc+4])
					pc += 4
					text = fmt.Sprintf("%s eax, 0x%x", alu, imm)
				}
			}
		case op >= 0x40 && op <= 0x47:
			text = fmt.Sprintf("inc %s", rnames[op&7])
		case op >= 0x48 && op <= 0x4F:
			text = fmt.Sprintf("dec %s", rnames[op&7])
		case op >= 0x50 && op <= 0x57:
			text = fmt.Sprintf("push %s", rnames[op&7])
		case op >= 0x58 && op <= 0x5F:
			text = fmt.Sprintf("pop %s", rnames[op&7])
		case op == 0x68:
			imm := binary.LittleEndian.Uint32(mem[pc : pc+4])
			pc += 4
			text = fmt.Sprintf("push 0x%x", imm)
		case op == 0x69:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			imm := int32(binary.LittleEndian.Uint32(mem[pc : pc+4]))
			pc += 4
			text = fmt.Sprintf("imul %s, %s, %d", rnames[r], rm, imm)
		case op == 0x6A:
			imm := int8(mem[pc])
			pc++
			text = fmt.Sprintf("push %d", imm)
		case op == 0x6B:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			imm := int8(mem[pc])
			pc++
			text = fmt.Sprintf("imul %s, %s, %d", rnames[r], rm, imm)
		case op >= 0x70 && op <= 0x7F:
			rel := int8(mem[pc])
			pc++
			text = fmt.Sprintf("j%s 0x%08x", condNames[op&0xF], uint32(int32(pc)+int32(rel)))
		case op == 0x80 || op == 0x82:
			rm, r, next := decodeModRM(mem, pc, 1)
			pc = next
			imm := mem[pc]
			pc++
			text = fmt.Sprintf("%s %s, 0x%x", aluNames[r], rm, imm)
		case op == 0x81:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			if opSize16 {
				imm := binary.LittleEndian.Uint16(mem[pc : pc+2])
				pc += 2
				text = fmt.Sprintf("%s %s, 0x%x", aluNames[r], rm, imm)
			} else {
				imm := binary.LittleEndian.Uint32(mem[pc : pc+4])
				pc += 4
				text = fmt.Sprintf("%s %s, 0x%x", aluNames[r], rm, imm)
			}
		case op == 0x83:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			imm := int8(mem[pc])
			pc++
			text = fmt.Sprintf("%s %s, %d", aluNames[r], rm, imm)
		case op == 0x84:
			rm, r, next := decodeModRM(mem, pc, 1)
			pc = next
			text = fmt.Sprintf("test %s, %s", rm, reg8[r])
		case op == 0x85:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			text = fmt.Sprintf("test %s, %s", rm, rnames[r])
		case op == 0x88:
			rm, r, next := decodeModRM(mem, pc, 1)
			pc = next
			text = fmt.Sprintf("mov %s, %s", rm, reg8[r])
		case op == 0x89:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			text = fmt.Sprintf("mov %s, %s", rm, rnames[r])
		case op == 0x8A:
			rm, r, next := decodeModRM(mem, pc, 1)
			pc = next
			text = fmt.Sprintf("mov %s, %s", reg8[r], rm)
		case op == 0x8B:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			text = fmt.Sprintf("mov %s, %s", rnames[r], rm)
		case op == 0x8D:
			rm, r, next := decodeModRM(mem, pc, 4)
			pc = next
			text = fmt.Sprintf("lea %s, %s", rnames[r], rm)
		case op == 0x90:
			text = "nop"
		case op == 0x98:
			text = "cwde"
		case op == 0x99:
			text = "cdq"
		case op == 0xA0:
			addr := binary.LittleEndian.Uint32(mem[pc : pc+4])
			pc += 4
			text = fmt.Sprintf("mov al, [0x%x]", addr)
		case op == 0xA1:
			addr := binary.LittleEndian.Uint32(mem[pc : pc+4])
			pc += 4
			text = fmt.Sprintf("mov %s, [0x%x]", rnames[0], addr)
		case op == 0xA2:
			addr := binary.LittleEndian.Uint32(mem[pc : pc+4])
			pc += 4
			text = fmt.Sprintf("mov [0x%x], al", addr)
		case op == 0xA3:
			addr := binary.LittleEndian.Uint32(mem[pc : pc+4])
			pc += 4
			text = fmt.Sprintf("mov [0x%x], %s", addr, rnames[0])
		case op == 0xA8:
			imm := mem[pc]
			pc++
			text = fmt.Sprintf("test al, 0x%x", imm)
		case op == 0xA9:
			imm := binary.LittleEndian.Uint32(mem[pc : pc+4])
			pc += 4
			text = fmt.Sprintf("test eax, 0x%x", imm)
		case op >= 0xB0 && op <= 0xB7:
			imm := mem[pc]
			pc++
			text = fmt.Sprintf("mov %s, 0x%x", reg8[op&7], imm)
		case op >= 0xB8 && op <= 0xBF:
			if opSize16 {
				imm := binary.LittleEndian.Uint16(mem[pc : pc+2])
				pc += 2
				text = fmt.Sprintf("mov %s, 0x%x", rnames[op&7], imm)
			} else {
				imm := binary.LittleEndian.Uint32(mem[pc : pc+4])
				pc += 4
				text = fmt.Sprintf("mov %s, 0x%x", rnames[op&7], imm)
			}
		case op == 0xC0 || op == 0xD0 || op == 0xD2:
			rm, r, next := decodeModRM(mem, pc, 1)
			pc = next
			cnt := "1"
			if op == 0xC0 {
				cnt = fmt.Sprintf("0x%x", mem[pc])
				pc++
			} else if op == 0xD2 {
				cnt = "cl"
			}
			text = fmt.Sprintf("%s %s, %s", shiftNames[r], rm, cnt)
		case op == 0xC1 || op == 0xD1 || op == 0xD3:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			cnt := "1"
			if op == 0xC1 {
				cnt = fmt.Sprintf("0x%x", mem[pc])
				pc++
			} else if op == 0xD3 {
				cnt = "cl"
			}
			text = fmt.Sprintf("%s %s, %s", shiftNames[r], rm, cnt)
		case op == 0xC2:
			imm := binary.LittleEndian.Uint16(mem[pc : pc+2])
			pc += 2
			text = fmt.Sprintf("ret 0x%x", imm)
		case op == 0xC3:
			text = "ret"
		case op == 0xC6:
			rm, _, next := decodeModRM(mem, pc, 1)
			pc = next
			imm := mem[pc]
			pc++
			text = fmt.Sprintf("mov %s, 0x%x", rm, imm)
		case op == 0xC7:
			rm, _, next := decodeModRM(mem, pc, sz)
			pc = next
			if opSize16 {
				imm := binary.LittleEndian.Uint16(mem[pc : pc+2])
				pc += 2
				text = fmt.Sprintf("mov %s, 0x%x", rm, imm)
			} else {
				imm := binary.LittleEndian.Uint32(mem[pc : pc+4])
				pc += 4
				text = fmt.Sprintf("mov %s, 0x%x", rm, imm)
			}
		case op == 0xC9:
			text = "leave"
		case op == 0xE8:
			rel := int32(binary.LittleEndian.Uint32(mem[pc : pc+4]))
			pc += 4
			text = fmt.Sprintf("call 0x%08x", uint32(int32(pc)+rel))
		case op == 0xE9:
			rel := int32(binary.LittleEndian.Uint32(mem[pc : pc+4]))
			pc += 4
			text = fmt.Sprintf("jmp 0x%08x", uint32(int32(pc)+rel))
		case op == 0xEB:
			rel := int8(mem[pc])
			pc++
			text = fmt.Sprintf("jmp 0x%08x", uint32(int32(pc)+int32(rel)))
		case op == 0xF6:
			rm, r, next := decodeModRM(mem, pc, 1)
			pc = next
			if r == 0 || r == 1 {
				imm := mem[pc]
				pc++
				text = fmt.Sprintf("test %s, 0x%x", rm, imm)
			} else {
				ops := [8]string{"test", "test", "not", "neg", "mul", "imul", "div", "idiv"}
				text = fmt.Sprintf("%s %s", ops[r], rm)
			}
		case op == 0xF7:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			if r == 0 || r == 1 {
				if opSize16 {
					imm := binary.LittleEndian.Uint16(mem[pc : pc+2])
					pc += 2
					text = fmt.Sprintf("test %s, 0x%x", rm, imm)
				} else {
					imm := binary.LittleEndian.Uint32(mem[pc : pc+4])
					pc += 4
					text = fmt.Sprintf("test %s, 0x%x", rm, imm)
				}
			} else {
				ops := [8]string{"test", "test", "not", "neg", "mul", "imul", "div", "idiv"}
				text = fmt.Sprintf("%s %s", ops[r], rm)
			}
		case op == 0xFE:
			rm, r, next := decodeModRM(mem, pc, 1)
			pc = next
			if r == 0 {
				text = fmt.Sprintf("inc %s", rm)
			} else {
				text = fmt.Sprintf("dec %s", rm)
			}
		case op == 0xFF:
			rm, r, next := decodeModRM(mem, pc, sz)
			pc = next
			ops := [8]string{"inc", "dec", "call", "callf", "jmp", "jmpf", "push", "?"}
			text = fmt.Sprintf("%s %s", ops[r], rm)
		case op == 0x0F:
			op2 := mem[pc]
			pc++
			switch {
			case op2 >= 0x18 && op2 <= 0x1F:
				_, _, next := decodeModRM(mem, pc, 4)
				pc = next
				text = "nop"
			case op2 >= 0x40 && op2 <= 0x4F:
				rm, r, next := decodeModRM(mem, pc, sz)
				pc = next
				text = fmt.Sprintf("cmov%s %s, %s", condNames[op2&0xF], rnames[r], rm)
			case op2 >= 0x80 && op2 <= 0x8F:
				rel := int32(binary.LittleEndian.Uint32(mem[pc : pc+4]))
				pc += 4
				text = fmt.Sprintf("j%s 0x%08x", condNames[op2&0xF], uint32(int32(pc)+rel))
			case op2 >= 0x90 && op2 <= 0x9F:
				rm, _, next := decodeModRM(mem, pc, 1)
				pc = next
				text = fmt.Sprintf("set%s %s", condNames[op2&0xF], rm)
			case op2 == 0xAF:
				rm, r, next := decodeModRM(mem, pc, sz)
				pc = next
				text = fmt.Sprintf("imul %s, %s", rnames[r], rm)
			case op2 == 0xB6:
				rm, r, next := decodeModRM(mem, pc, 1)
				pc = next
				text = fmt.Sprintf("movzx %s, %s", rnames[r], rm)
			case op2 == 0xB7:
				rm, r, next := decodeModRM(mem, pc, 2)
				pc = next
				text = fmt.Sprintf("movzx %s, %s", rnames[r], rm)
			case op2 == 0xBE:
				rm, r, next := decodeModRM(mem, pc, 1)
				pc = next
				text = fmt.Sprintf("movsx %s, %s", rnames[r], rm)
			case op2 == 0xBF:
				rm, r, next := decodeModRM(mem, pc, 2)
				pc = next
				text = fmt.Sprintf("movsx %s, %s", rnames[r], rm)
			default:
				rm, r, next := decodeModRM(mem, pc, 4)
				pc = next
				_ = repF2
				_ = repF3
				text = fmt.Sprintf("sse_0f_%02x xmm%d, %s", op2, r, rm)
			}
		default:
			text = fmt.Sprintf("db 0x%02x", op)
		}
		fmt.Printf("  0x%08x: %s\n", insnStart, text)
	}
}

func main() {
	startStr := flag.String("start", "0x00270500", "Start VA")
	endStr := flag.String("end", "0x002705b0", "End VA")
	flag.Parse()

	s, _ := strconv.ParseUint(*startStr, 0, 32)
	e, _ := strconv.ParseUint(*endStr, 0, 32)

	binPath := "/workspace/Lego Star Wars Saga/LEGO Star Wars Saga.app/Contents/MacOS/LEGO Star Wars Saga"
	img, err := macho32.LoadImage(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "err: %v\n", err)
		os.Exit(1)
	}
	mem := make([]byte, 0x04000000)
	_ = img.MapIntoArena(mem)
	disasmRange(mem, uint32(s), uint32(e))
}

package macho32

import (
	"encoding/binary"
	"fmt"
	"os"
)

const (
	MhMagic32   = 0xFEEDFACE
	LcSegment32 = 0x1
	LcSymtab    = 0x2
	LcUnixThread = 0x5
	LcDysymtab  = 0xB

	NuEngineSplitAddr = 0x002e3be0
)

type SegmentInfo struct {
	Name     string
	VMAddr   uint32
	VMSize   uint32
	FileOff  uint32
	FileSize uint32
	MaxProt  int32
	InitProt int32
}

type SectionInfo struct {
	SegName   string
	SectName  string
	Addr      uint32
	Size      uint32
	Offset    uint32
	Flags     uint32
	Reserved1 uint32
	Reserved2 uint32
}

type SymbolInfo struct {
	Name   string
	Type   uint8
	Sect   uint8
	Desc   uint16
	Value  uint32
}

type Image struct {
	Raw          []byte
	EntryEIP     uint32
	Segments     []SegmentInfo
	Sections     []SectionInfo
	Symbols      []SymbolInfo
	SymbolsByAddr map[uint32]string
	StubsByAddr  map[uint32]string
	LaPtrByAddr  map[uint32]string
	NlPtrByAddr  map[uint32]string
	FDEs         map[uint32]uint32
	ModInitFuncs []uint32
	NuModInits   []uint32
}

func readCString(b []byte, off int) string {
	if off <= 0 || off >= len(b) {
		return ""
	}
	end := off
	for end < len(b) && b[end] != 0 {
		end++
	}
	return string(b[off:end])
}

func trimNull(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func LoadImage(path string) (*Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 28 {
		return nil, fmt.Errorf("file too small for Mach-O header")
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != MhMagic32 {
		return nil, fmt.Errorf("unexpected Mach-O magic: 0x%08x", magic)
	}
	ncmds := binary.LittleEndian.Uint32(data[16:20])

	img := &Image{
		Raw:           data,
		SymbolsByAddr: make(map[uint32]string),
		StubsByAddr:   make(map[uint32]string),
		LaPtrByAddr:   make(map[uint32]string),
		NlPtrByAddr:   make(map[uint32]string),
		FDEs:          make(map[uint32]uint32),
	}

	var symOff, nSyms, strOff uint32
	var indirectSymOff, nIndirectSyms uint32

	pos := 28
	for i := uint32(0); i < ncmds; i++ {
		if pos+8 > len(data) {
			break
		}
		cmd := binary.LittleEndian.Uint32(data[pos : pos+4])
		cmdsize := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		if cmdsize < 8 || pos+int(cmdsize) > len(data) {
			break
		}

		switch cmd {
		case LcSegment32:
			seg := SegmentInfo{
				Name:     trimNull(data[pos+8 : pos+24]),
				VMAddr:   binary.LittleEndian.Uint32(data[pos+24 : pos+28]),
				VMSize:   binary.LittleEndian.Uint32(data[pos+28 : pos+32]),
				FileOff:  binary.LittleEndian.Uint32(data[pos+32 : pos+36]),
				FileSize: binary.LittleEndian.Uint32(data[pos+36 : pos+40]),
				MaxProt:  int32(binary.LittleEndian.Uint32(data[pos+40 : pos+44])),
				InitProt: int32(binary.LittleEndian.Uint32(data[pos+44 : pos+48])),
			}
			nsects := binary.LittleEndian.Uint32(data[pos+48 : pos+52])
			img.Segments = append(img.Segments, seg)

			spos := pos + 56
			for j := uint32(0); j < nsects; j++ {
				if spos+68 > pos+int(cmdsize) {
					break
				}
				sec := SectionInfo{
					SectName:  trimNull(data[spos : spos+16]),
					SegName:   trimNull(data[spos+16 : spos+32]),
					Addr:      binary.LittleEndian.Uint32(data[spos+32 : spos+36]),
					Size:      binary.LittleEndian.Uint32(data[spos+36 : spos+40]),
					Offset:    binary.LittleEndian.Uint32(data[spos+40 : spos+44]),
					Flags:     binary.LittleEndian.Uint32(data[spos+56 : spos+60]),
					Reserved1: binary.LittleEndian.Uint32(data[spos+60 : spos+64]),
					Reserved2: binary.LittleEndian.Uint32(data[spos+64 : spos+68]),
				}
				img.Sections = append(img.Sections, sec)
				spos += 68
			}

		case LcUnixThread:
			count := binary.LittleEndian.Uint32(data[pos+12 : pos+16])
			if count >= 11 && pos+16+44 <= len(data) {
				img.EntryEIP = binary.LittleEndian.Uint32(data[pos+16+40 : pos+16+44])
			}

		case LcSymtab:
			symOff = binary.LittleEndian.Uint32(data[pos+8 : pos+12])
			nSyms = binary.LittleEndian.Uint32(data[pos+12 : pos+16])
			strOff = binary.LittleEndian.Uint32(data[pos+16 : pos+20])

		case LcDysymtab:
			if cmdsize >= 64 {
				indirectSymOff = binary.LittleEndian.Uint32(data[pos+56 : pos+60])
				nIndirectSyms = binary.LittleEndian.Uint32(data[pos+60 : pos+64])
			}
		}
		pos += int(cmdsize)
	}

	// Parse symbol table
	img.Symbols = make([]SymbolInfo, nSyms)
	for i := uint32(0); i < nSyms; i++ {
		off := int(symOff + i*12)
		if off+12 > len(data) {
			break
		}
		nStrx := binary.LittleEndian.Uint32(data[off : off+4])
		nType := data[off+4]
		nSect := data[off+5]
		nDesc := binary.LittleEndian.Uint16(data[off+6 : off+8])
		nVal := binary.LittleEndian.Uint32(data[off+8 : off+12])
		name := ""
		if nStrx != 0 {
			name = readCString(data, int(strOff+nStrx))
		}
		img.Symbols[i] = SymbolInfo{
			Name:  name,
			Type:  nType,
			Sect:  nSect,
			Desc:  nDesc,
			Value: nVal,
		}
		if (nType&0xE0) == 0 && (nType&0x0E) == 0x0E && nVal != 0 && name != "" {
			img.SymbolsByAddr[nVal] = name
		}
	}

	// Parse indirect symbol tables (__symbol_stub, __la_symbol_ptr, __nl_symbol_ptr)
	indirectSyms := make([]uint32, nIndirectSyms)
	for i := uint32(0); i < nIndirectSyms; i++ {
		off := int(indirectSymOff + i*4)
		if off+4 <= len(data) {
			indirectSyms[i] = binary.LittleEndian.Uint32(data[off : off+4])
		}
	}

	for _, sec := range img.Sections {
		sectType := sec.Flags & 0xFF
		switch sectType {
		case 0x6: // S_NON_LAZY_SYMBOL_POINTERS (__nl_symbol_ptr)
			count := sec.Size / 4
			for j := uint32(0); j < count; j++ {
				idx := sec.Reserved1 + j
				if idx < uint32(len(indirectSyms)) {
					symIdx := indirectSyms[idx]
					if (symIdx&0xC0000000) == 0 && symIdx < uint32(len(img.Symbols)) {
						img.NlPtrByAddr[sec.Addr+j*4] = img.Symbols[symIdx].Name
					}
				}
			}
		case 0x7: // S_LAZY_SYMBOL_POINTERS (__la_symbol_ptr)
			count := sec.Size / 4
			for j := uint32(0); j < count; j++ {
				idx := sec.Reserved1 + j
				if idx < uint32(len(indirectSyms)) {
					symIdx := indirectSyms[idx]
					if (symIdx&0xC0000000) == 0 && symIdx < uint32(len(img.Symbols)) {
						img.LaPtrByAddr[sec.Addr+j*4] = img.Symbols[symIdx].Name
					}
				}
			}
		case 0x8: // S_SYMBOL_STUBS (__symbol_stub)
			stride := sec.Reserved2
			if stride == 0 {
				stride = 6
			}
			count := sec.Size / stride
			for j := uint32(0); j < count; j++ {
				idx := sec.Reserved1 + j
				if idx < uint32(len(indirectSyms)) {
					symIdx := indirectSyms[idx]
					if (symIdx&0xC0000000) == 0 && symIdx < uint32(len(img.Symbols)) {
						img.StubsByAddr[sec.Addr+j*stride] = img.Symbols[symIdx].Name
					}
				}
			}
		}

		if sec.SectName == "__mod_init_func" {
			count := sec.Size / 4
			for j := uint32(0); j < count; j++ {
				off := int(sec.Offset + j*4)
				if off+4 <= len(data) {
					fn := binary.LittleEndian.Uint32(data[off : off+4])
					img.ModInitFuncs = append(img.ModInitFuncs, fn)
					if fn > 0 && fn < NuEngineSplitAddr {
						img.NuModInits = append(img.NuModInits, fn)
					}
				}
			}
		}

		if sec.SectName == "__eh_frame" {
			pos := int(sec.Offset)
			end := int(sec.Offset + sec.Size)
			for pos+4 <= end && pos+4 <= len(data) {
				length := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
				if length == 0 || pos+4+length > end {
					break
				}
				cieID := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
				if cieID != 0 && pos+16 <= end {
					pcRel := int32(binary.LittleEndian.Uint32(data[pos+8 : pos+12]))
					pcBegin := uint32(int32(sec.Addr+uint32(pos-int(sec.Offset))+8) + pcRel)
					pcRange := binary.LittleEndian.Uint32(data[pos+12 : pos+16])
					img.FDEs[pcBegin] = pcRange
				}
				pos += 4 + length
			}
		}
	}

	return img, nil
}

func (img *Image) MapIntoArena(arena []byte) error {
	for _, seg := range img.Segments {
		if seg.VMSize == 0 {
			continue
		}
		if uint64(seg.VMAddr)+uint64(seg.VMSize) > uint64(len(arena)) {
			return fmt.Errorf("segment %s (0x%08x..0x%08x) exceeds arena size 0x%08x",
				seg.Name, seg.VMAddr, seg.VMAddr+seg.VMSize, len(arena))
		}
		if seg.FileSize > 0 {
			copy(arena[seg.VMAddr:seg.VMAddr+seg.FileSize], img.Raw[seg.FileOff:seg.FileOff+seg.FileSize])
		}
	}
	return nil
}

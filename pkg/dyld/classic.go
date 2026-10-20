package dyld

import (
	"encoding/binary"
	"fmt"
)

// Images linked by older linkers have no LC_DYLD_INFO: they are rebased by local relocations
// and bound through the indirect symbol table (pointer and stub sections) and external
// relocations, with the library of each import in its symbol's two-level ordinal.

const (
	indirectLocal = 0x80000000 // INDIRECT_SYMBOL_LOCAL
	indirectAbs   = 0x40000000 // INDIRECT_SYMBOL_ABS
	sNonLazy      = 0x6        // S_NON_LAZY_SYMBOL_POINTERS
	sLazy         = 0x7        // S_LAZY_SYMBOL_POINTERS
	sStubs        = 0x8        // S_SYMBOL_STUBS
	sSelfModify   = 0x04000000 // S_ATTR_SELF_MODIFYING_CODE (i386 __jump_table: 5-byte jmp stubs)
)

// relocBase is what relocation addresses are relative to.
func (img *Image) relocBase() uint32 {
	for _, s := range img.Segs {
		if s.Name == "__PAGEZERO" {
			continue
		}
		if !img.splitSegs || s.InitPro&2 != 0 {
			return s.VMAddr
		}
	}
	return 0
}

func (l *Linker) classicRebase(img *Image) error {
	if img.Slide == 0 {
		return nil
	}
	le := binary.LittleEndian
	base := img.relocBase()
	for i := uint32(0); i < img.nLocRel; i++ {
		o := uint64(img.locRelOff) + uint64(i)*8
		if o+8 > uint64(len(img.raw)) {
			return fmt.Errorf("local relocations run past the file")
		}
		w0, w1 := le.Uint32(img.raw[o:]), le.Uint32(img.raw[o+4:])
		var addr uint32
		var length, typ uint32
		var pcrel bool
		if w0&0x80000000 != 0 { // scattered: r_address in the low 24 bits
			addr = w0 & 0x00ffffff
			typ, length, pcrel = (w0>>24)&0xf, (w0>>28)&3, (w0>>30)&1 != 0
			if typ == 1 { // GENERIC_RELOC_PAIR
				continue
			}
		} else {
			addr = w0
			pcrel, length, typ = (w1>>24)&1 != 0, (w1>>25)&3, (w1>>28)&0xf
		}
		if typ != 0 || pcrel || length != 2 { // only VANILLA 32-bit absolute values move with the image
			continue
		}
		a := base + addr
		if err := l.put32(img, a, l.get32(img, a)+img.Slide); err != nil {
			return err
		}
	}
	return nil
}

func (l *Linker) classicBind(img *Image, deps []*Image) error {
	le := binary.LittleEndian
	target := func(symIndex uint32) (uint32, bool) {
		name, ord, ok := img.symbol(symIndex)
		if !ok {
			return 0, false
		}
		switch {
		case img.flat:
			return l.resolve(img, deps, -2, name)
		case ord == 0x00: // SELF_LIBRARY_ORDINAL
			return l.resolve(img, deps, 0, name)
		case ord == 0xfe: // DYNAMIC_LOOKUP_ORDINAL
			return l.resolve(img, deps, -2, name)
		case ord == 0xff: // EXECUTABLE_ORDINAL
			return l.resolve(img, deps, -1, name)
		}
		return l.resolve(img, deps, int64(ord), name)
	}
	indirect := func(k uint32) (uint32, bool) {
		o := uint64(img.indirectOff) + uint64(k)*4
		if k >= img.nIndirect || o+4 > uint64(len(img.raw)) {
			return 0, false
		}
		return le.Uint32(img.raw[o:]), true
	}
	for _, s := range img.Segs {
		for _, sc := range s.Sections {
			kind := sc.Flags & 0xff
			switch {
			case kind == sNonLazy || kind == sLazy:
				for k := uint32(0); k*4 < sc.Size; k++ {
					idx, ok := indirect(sc.Reserved1 + k)
					if !ok {
						break
					}
					a := sc.Addr + k*4
					switch {
					case idx&indirectLocal != 0: // a local pointer: moves with the image
						if idx&indirectAbs == 0 && img.Slide != 0 {
							if err := l.put32(img, a, l.get32(img, a)+img.Slide); err != nil {
								return err
							}
						}
					case idx&indirectAbs != 0:
					default:
						if t, ok := target(idx); ok {
							if err := l.put32(img, a, t); err != nil {
								return err
							}
						}
					}
				}
			case kind == sStubs && sc.Flags&sSelfModify != 0 && sc.Reserved2 == 5:
				for k := uint32(0); k*5 < sc.Size; k++ {
					idx, ok := indirect(sc.Reserved1 + k)
					if !ok || idx&(indirectLocal|indirectAbs) != 0 {
						continue
					}
					if t, ok := target(idx); ok {
						a := uint64(sc.Addr+k*5) + uint64(img.Slide)
						l.Mem[a] = 0xe9 // jmp rel32
						le.PutUint32(l.Mem[a+1:], t-uint32(a+5))
					}
				}
			}
		}
	}
	base := img.relocBase()
	for i := uint32(0); i < img.nExtRel; i++ {
		o := uint64(img.extRelOff) + uint64(i)*8
		if o+8 > uint64(len(img.raw)) {
			return fmt.Errorf("external relocations run past the file")
		}
		w0, w1 := le.Uint32(img.raw[o:]), le.Uint32(img.raw[o+4:])
		sym, pcrel, length, ext := w1&0xffffff, (w1>>24)&1 != 0, (w1>>25)&3, (w1>>27)&1 != 0
		if !ext || length != 2 {
			continue
		}
		t, ok := target(sym)
		if !ok {
			continue
		}
		a := base + w0
		v := l.get32(img, a) + t // the stored value is the addend
		if pcrel {
			v -= a + img.Slide + 4
		}
		if err := l.put32(img, a, v); err != nil {
			return err
		}
	}
	return nil
}

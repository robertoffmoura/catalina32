// Package dyld is a dynamic linker for i386 Mach-O images in the guest address space: it loads
// an executable and the dylibs it links against or opens (dlopen), slides them to free
// addresses, applies rebases, binds their imports (to other guest images, or to the host's
// replacements of system libraries), and runs their initialisers. Games built from many
// dylibs (Valve's Source games) need it; LEGO Star Wars, one statically linked executable, does
// not use it.
package dyld

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Segment is one LC_SEGMENT of an image (addresses before sliding).
type Segment struct {
	Name             string
	VMAddr, VMSize   uint32
	FileOff, FileSz  uint32
	Sections         []Section
	MaxProt, InitPro uint32
}

// Section is one section of a segment (addresses before sliding).
type Section struct {
	Name, Seg            string
	Addr, Size           uint32
	Offset               uint32
	Flags                uint32
	Reserved1, Reserved2 uint32 // indirect symbol index, stub size (pointer and stub sections)
}

// Image is a parsed i386 Mach-O image.
type Image struct {
	Path    string   // file it was loaded from
	Install string   // LC_ID_DYLIB name ("" for the executable)
	Exec    bool     // MH_EXECUTE
	Deps    []string // LC_LOAD_DYLIB etc., in order: bind ordinal i+1 is Deps[i]
	Weak    []bool   // Deps[i] is a weak (optional) link
	Segs    []Segment
	Entry   uint32 // executable: initial EIP (LC_UNIXTHREAD) or LC_MAIN offset
	MainOff bool   // Entry is an LC_MAIN offset from __TEXT, not an address

	Slide uint32 // load address = original address + Slide

	raw                                    []byte // the i386 slice
	rebaseOff, rebaseSz, bindOff, bindSz   uint32
	weakOff, weakSz, lazyOff, lazySz       uint32
	exportOff, exportSz                    uint32
	symOff, nSyms, strOff, strSz           uint32
	dyldInfo                               bool // LC_DYLD_INFO(_ONLY); without it: classic relocations
	indirectOff, nIndirect                 uint32
	extRelOff, nExtRel, locRelOff, nLocRel uint32
	splitSegs, flat                        bool // MH_SPLIT_SEGS: relocation addresses are from the first writable segment
	exports                                map[string]export
	syms                                   []symAt // SymbolAt's table
}

type export struct {
	addr      uint32 // relative to the image's __TEXT start (unslid vmaddr 0 for dylibs)
	reexport  bool
	ordinal   uint32 // reexport: dependency ordinal
	reexpName string // reexport: name in that dependency ("" = same name)
}

// Mach-O constants.
const (
	mhMagic        = 0xfeedface
	fatMagic       = 0xcafebabe
	cpuTypeI386    = 7
	mhExecute      = 2
	lcSegment      = 0x1
	lcSymtab       = 0x2
	lcUnixThread   = 0x5
	lcLoadDylib    = 0xc
	lcIDDylib      = 0xd
	lcLoadWeak     = 0x18 | 0x80000000
	lcReexport     = 0x1f | 0x80000000
	lcLoadUpward   = 0x23 | 0x80000000
	lcDyldInfo     = 0x22
	lcDyldInfoOnly = 0x22 | 0x80000000
	lcMain         = 0x28 | 0x80000000
	lcDysymtab     = 0xb
)

// Parse reads an i386 Mach-O file (thin, or the i386 slice of a universal file).
func Parse(path string) (*Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := i386Slice(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	img := &Image{Path: path, raw: raw}
	if err := img.parseCommands(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return img, nil
}

func i386Slice(data []byte) ([]byte, error) {
	if len(data) < 28 {
		return nil, errors.New("too small for a Mach-O header")
	}
	if binary.LittleEndian.Uint32(data) == mhMagic {
		if binary.LittleEndian.Uint32(data[4:]) != cpuTypeI386 {
			return nil, errors.New("not i386")
		}
		return data, nil
	}
	if binary.BigEndian.Uint32(data) == fatMagic {
		n := binary.BigEndian.Uint32(data[4:])
		for i := uint32(0); i < n && 8+20*(i+1) <= uint32(len(data)); i++ {
			e := data[8+20*i:]
			if binary.BigEndian.Uint32(e) == cpuTypeI386 {
				off, size := binary.BigEndian.Uint32(e[8:]), binary.BigEndian.Uint32(e[12:])
				if uint64(off)+uint64(size) > uint64(len(data)) {
					return nil, errors.New("truncated i386 slice")
				}
				return data[off : off+size], nil
			}
		}
		return nil, errors.New("universal file without an i386 slice")
	}
	return nil, errors.New("not a Mach-O file")
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func (img *Image) parseCommands() error {
	b := img.raw
	le := binary.LittleEndian
	img.Exec = le.Uint32(b[12:]) == mhExecute
	img.splitSegs = le.Uint32(b[24:])&0x20 != 0
	img.flat = le.Uint32(b[24:])&0x80 == 0 // not MH_TWOLEVEL
	ncmds := le.Uint32(b[16:])
	pos := uint32(28)
	for i := uint32(0); i < ncmds; i++ {
		if pos+8 > uint32(len(b)) {
			return errors.New("load commands run past the file")
		}
		cmd, size := le.Uint32(b[pos:]), le.Uint32(b[pos+4:])
		if size < 8 || pos+size > uint32(len(b)) {
			return fmt.Errorf("bad load command size %d", size)
		}
		c := b[pos : pos+size]
		switch cmd {
		case lcSegment:
			s := Segment{Name: cstr(c[8:24]), VMAddr: le.Uint32(c[24:]), VMSize: le.Uint32(c[28:]),
				FileOff: le.Uint32(c[32:]), FileSz: le.Uint32(c[36:]), MaxProt: le.Uint32(c[40:]), InitPro: le.Uint32(c[44:])}
			ns := le.Uint32(c[48:])
			for k := uint32(0); k < ns && 56+68*(k+1) <= size; k++ {
				sc := c[56+68*k:]
				s.Sections = append(s.Sections, Section{Name: cstr(sc[0:16]), Seg: cstr(sc[16:32]),
					Addr: le.Uint32(sc[32:]), Size: le.Uint32(sc[36:]), Offset: le.Uint32(sc[40:]), Flags: le.Uint32(sc[56:]),
					Reserved1: le.Uint32(sc[60:]), Reserved2: le.Uint32(sc[64:])})
			}
			img.Segs = append(img.Segs, s)
		case lcIDDylib:
			img.Install = cstr(c[le.Uint32(c[8:]):])
		case lcLoadDylib, lcLoadWeak, lcReexport, lcLoadUpward:
			img.Deps = append(img.Deps, cstr(c[le.Uint32(c[8:]):]))
			img.Weak = append(img.Weak, cmd == lcLoadWeak)
		case lcDysymtab:
			img.indirectOff, img.nIndirect = le.Uint32(c[56:]), le.Uint32(c[60:])
			img.extRelOff, img.nExtRel = le.Uint32(c[64:]), le.Uint32(c[68:])
			img.locRelOff, img.nLocRel = le.Uint32(c[72:]), le.Uint32(c[76:])
		case lcDyldInfo, lcDyldInfoOnly:
			img.dyldInfo = true
			img.rebaseOff, img.rebaseSz = le.Uint32(c[8:]), le.Uint32(c[12:])
			img.bindOff, img.bindSz = le.Uint32(c[16:]), le.Uint32(c[20:])
			img.weakOff, img.weakSz = le.Uint32(c[24:]), le.Uint32(c[28:])
			img.lazyOff, img.lazySz = le.Uint32(c[32:]), le.Uint32(c[36:])
			img.exportOff, img.exportSz = le.Uint32(c[40:]), le.Uint32(c[44:])
		case lcSymtab:
			img.symOff, img.nSyms, img.strOff, img.strSz = le.Uint32(c[8:]), le.Uint32(c[12:]), le.Uint32(c[16:]), le.Uint32(c[20:])
		case lcUnixThread:
			// i386_THREAD_STATE: flavor, count, then eax ebx ecx edx edi esi ebp esp ss eflags eip ...
			if size >= 16+11*4 {
				img.Entry = le.Uint32(c[16+10*4:])
			}
		case lcMain:
			img.Entry, img.MainOff = uint32(le.Uint64(c[8:])), true
		}
		pos += size
	}
	return nil
}

// Name is the image's short name (its file name).
func (img *Image) Name() string { return filepath.Base(img.Path) }

// Span returns the image's unslid address range (all segments but __PAGEZERO and __LINKEDIT).
func (img *Image) Span() (lo, hi uint32) {
	lo = ^uint32(0)
	for _, s := range img.Segs {
		if s.Name == "__PAGEZERO" || s.Name == "__LINKEDIT" || s.VMSize == 0 {
			continue
		}
		lo = min(lo, s.VMAddr)
		hi = max(hi, s.VMAddr+s.VMSize)
	}
	if lo > hi {
		lo = hi
	}
	return lo, hi
}

// Section returns the slid address and size of a section ("" segment: any), or false.
func (img *Image) Section(seg, name string) (uint32, uint32, bool) {
	for _, s := range img.Segs {
		for _, sc := range s.Sections {
			if sc.Name == name && (seg == "" || sc.Seg == seg) {
				return sc.Addr + img.Slide, sc.Size, true
			}
		}
	}
	return 0, 0, false
}

// segAddr returns the unslid address of offset off in segment index i.
func (img *Image) segAddr(i int, off uint64) (uint32, error) {
	if i < 0 || i >= len(img.Segs) {
		return 0, fmt.Errorf("segment index %d out of range", i)
	}
	return img.Segs[i].VMAddr + uint32(off), nil
}

func uleb(b []byte, p *int) uint64 {
	var v uint64
	var shift uint
	for *p < len(b) {
		c := b[*p]
		*p++
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			break
		}
		shift += 7
	}
	return v
}

func sleb(b []byte, p *int) int64 {
	var v int64
	var shift uint
	var c byte
	for *p < len(b) {
		c = b[*p]
		*p++
		v |= int64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			break
		}
	}
	if shift < 64 && c&0x40 != 0 {
		v |= -1 << shift
	}
	return v
}

// Exports reads the image's export trie (once).
func (img *Image) Exports() map[string]export {
	if img.exports != nil {
		return img.exports
	}
	img.exports = map[string]export{}
	if img.exportSz == 0 {
		// No export trie (older linkers): the symbol table's defined external symbols.
		img.forSymbols(func(i uint32, name string, typ, sect uint8, desc uint16, value uint32) {
			if typ&0x0e == 0x0e && typ&0x01 != 0 && typ&0x10 == 0 && typ&0xe0 == 0 { // N_SECT, N_EXT, not N_PEXT, not stab
				img.exports[name] = export{addr: value - img.textBase()}
			}
		})
		return img.exports
	}
	if uint64(img.exportOff)+uint64(img.exportSz) > uint64(len(img.raw)) {
		return img.exports
	}
	trie := img.raw[img.exportOff : img.exportOff+img.exportSz]
	var walk func(off int, prefix string, depth int)
	walk = func(off int, prefix string, depth int) {
		if off >= len(trie) || depth > 512 {
			return
		}
		p := off
		termSize := int(uleb(trie, &p))
		if termSize > 0 {
			q := p
			flags := uleb(trie, &q)
			var e export
			switch {
			case flags&0x08 != 0: // EXPORT_SYMBOL_FLAGS_REEXPORT
				e.reexport = true
				e.ordinal = uint32(uleb(trie, &q))
				if q < len(trie) {
					e.reexpName = cstr(trie[q:])
				}
			case flags&0x10 != 0: // STUB_AND_RESOLVER: use the stub
				e.addr = uint32(uleb(trie, &q))
			default:
				e.addr = uint32(uleb(trie, &q))
			}
			img.exports[prefix] = e
		}
		p += termSize
		if p >= len(trie) {
			return
		}
		n := int(trie[p])
		p++
		for i := 0; i < n && p < len(trie); i++ {
			end := p
			for end < len(trie) && trie[end] != 0 {
				end++
			}
			edge := string(trie[p:end])
			p = end + 1
			child := int(uleb(trie, &p))
			walk(child, prefix+edge, depth+1)
		}
	}
	walk(0, "", 0)
	return img.exports
}

// forSymbols calls fn for every nlist entry of the symbol table.
func (img *Image) forSymbols(fn func(i uint32, name string, typ, sect uint8, desc uint16, value uint32)) {
	le := binary.LittleEndian
	for i := uint32(0); i < img.nSyms; i++ {
		o := uint64(img.symOff) + uint64(i)*12
		if o+12 > uint64(len(img.raw)) {
			return
		}
		e := img.raw[o : o+12]
		strx := le.Uint32(e)
		name := ""
		if so := uint64(img.strOff) + uint64(strx); strx != 0 && so < uint64(len(img.raw)) {
			name = cstr(img.raw[so:])
		}
		fn(i, name, e[4], e[5], le.Uint16(e[6:]), le.Uint32(e[8:]))
	}
}

// symbol returns nlist entry i's name and two-level library ordinal.
func (img *Image) symbol(i uint32) (name string, ordinal int, ok bool) {
	le := binary.LittleEndian
	o := uint64(img.symOff) + uint64(i)*12
	if i >= img.nSyms || o+12 > uint64(len(img.raw)) {
		return "", 0, false
	}
	e := img.raw[o : o+12]
	if so := uint64(img.strOff) + uint64(le.Uint32(e)); so < uint64(len(img.raw)) {
		name = cstr(img.raw[so:])
	}
	return name, int(le.Uint16(e[6:]) >> 8), true
}

// textBase is the unslid address of __TEXT (the mach header), which export addresses are from.
func (img *Image) textBase() uint32 {
	for _, s := range img.Segs {
		if s.Name == "__TEXT" {
			return s.VMAddr
		}
	}
	return 0
}

type symAt struct {
	addr uint32
	name string
}

// SymbolAt names the symbol (exported or local) at or below the slid address a, and a's offset
// from it, for diagnostics.
func (img *Image) SymbolAt(a uint32) (string, uint32) {
	if img.syms == nil {
		img.forSymbols(func(i uint32, name string, typ, sect uint8, desc uint16, value uint32) {
			if typ&0x0e == 0x0e && typ&0xe0 == 0 && name != "" {
				img.syms = append(img.syms, symAt{value, name})
			}
		})
		sort.Slice(img.syms, func(i, j int) bool { return img.syms[i].addr < img.syms[j].addr })
	}
	u := a - img.Slide
	i := sort.Search(len(img.syms), func(i int) bool { return img.syms[i].addr > u }) - 1
	if i < 0 {
		return "", 0
	}
	return img.syms[i].name, u - img.syms[i].addr
}

// ForEachSymbol calls fn with every code or data symbol of the image (exported or local) and
// its slid address, for diagnostics.
func (img *Image) ForEachSymbol(fn func(name string, addr uint32)) {
	img.SymbolAt(0)
	for _, s := range img.syms {
		fn(s.name, s.addr+img.Slide)
	}
}

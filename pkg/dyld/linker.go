package dyld

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Linker loads images into guest memory and links them.
type Linker struct {
	Mem    []byte // guest memory
	Lo, Hi uint32 // the region images are placed in (the executable keeps its own addresses)
	next   uint32

	ExecDir string   // @executable_path
	Search  []string // folders searched for dependencies named without a usable path (like DYLD_LIBRARY_PATH)

	// System returns the guest address that stands for symbol sym of system library lib (a
	// host replacement, or a stub). It is called for every import from outside the game.
	System func(lib, sym string) uint32
	// Skip says not to load a guest image (its imports from it then go to System as well).
	Skip func(path string) bool
	// Mapped is told about each image once it is in memory and linked (code ranges, symbols).
	Mapped func(img *Image)

	Images []*Image // in load order
	Main   *Image   // the executable
	Inits  []uint32 // initialisers of every image, dependencies first

	byPath     map[string]*Image
	Unresolved map[string]map[string]bool // guest image -> symbols it lacks (missing exports)
}

// System libraries are the host's: never loaded, always resolved through Linker.System.
func isSystem(name string) bool {
	return strings.HasPrefix(name, "/usr/lib/") || strings.HasPrefix(name, "/System/")
}

// LoadExecutable maps the main executable at its own addresses and links it with everything it
// depends on.
func (l *Linker) LoadExecutable(path string) (*Image, error) {
	img, err := Parse(path)
	if err != nil {
		return nil, err
	}
	if !img.Exec {
		return nil, fmt.Errorf("%s is not an executable", path)
	}
	if l.ExecDir == "" {
		l.ExecDir = filepath.Dir(path)
	}
	l.Main = img
	if err := l.place(img, true); err != nil {
		return nil, err
	}
	return img, l.link(img)
}

// Open loads a dylib (dlopen): name is a path or a bare name looked up like a dependency of from
// (nil: the executable). Loading an image that is already loaded returns it.
func (l *Linker) Open(name string, from *Image) (*Image, error) {
	if from == nil {
		from = l.Main
	}
	path, ok := l.find(name, from)
	if !ok {
		return nil, fmt.Errorf("%s: not found", name)
	}
	if img := l.byPath[path]; img != nil {
		return img, nil
	}
	img, err := Parse(path)
	if err != nil {
		return nil, err
	}
	if err := l.place(img, false); err != nil {
		return nil, err
	}
	return img, l.link(img)
}

// find resolves a dependency name the way dyld does for these games.
func (l *Linker) find(name string, from *Image) (string, bool) {
	var cands []string
	base := filepath.Base(name)
	switch {
	case strings.HasPrefix(name, "@loader_path/"):
		if from != nil {
			cands = append(cands, filepath.Join(filepath.Dir(from.Path), name[len("@loader_path/"):]))
		}
	case strings.HasPrefix(name, "@executable_path/"):
		cands = append(cands, filepath.Join(l.ExecDir, name[len("@executable_path/"):]))
	case filepath.IsAbs(name):
		cands = append(cands, name)
	}
	for _, d := range l.Search {
		cands = append(cands, filepath.Join(d, base))
	}
	if from != nil {
		cands = append(cands, filepath.Join(filepath.Dir(from.Path), base))
	}
	cands = append(cands, filepath.Join(l.ExecDir, base))
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			if real, err := filepath.EvalSymlinks(c); err == nil {
				return filepath.Clean(real), true
			}
			return filepath.Clean(c), true
		}
	}
	return "", false
}

// place chooses the image's address and copies its segments into guest memory.
func (l *Linker) place(img *Image, atOwn bool) error {
	lo, hi := img.Span()
	if atOwn {
		img.Slide = 0
	} else {
		if l.next < l.Lo {
			l.next = l.Lo
		}
		base := (l.next + 0xfff) &^ 0xfff
		size := (hi - lo + 0xfff) &^ 0xfff
		if uint64(base)+uint64(size) > uint64(l.Hi) {
			return fmt.Errorf("%s: no room for %d KB in the image region (%#x-%#x, %#x used)",
				img.Name(), size>>10, l.Lo, l.Hi, l.next)
		}
		img.Slide = base - lo
		l.next = base + size
	}
	for _, s := range img.Segs {
		if s.Name == "__PAGEZERO" || s.Name == "__LINKEDIT" || s.VMSize == 0 {
			continue
		}
		at := uint64(s.VMAddr) + uint64(img.Slide)
		if at+uint64(s.VMSize) > uint64(len(l.Mem)) {
			return fmt.Errorf("%s: segment %s at %#x doesn't fit in guest memory", img.Name(), s.Name, at)
		}
		n := min(s.FileSz, s.VMSize)
		if uint64(s.FileOff)+uint64(n) > uint64(len(img.raw)) {
			return fmt.Errorf("%s: segment %s runs past the file", img.Name(), s.Name)
		}
		copy(l.Mem[at:at+uint64(n)], img.raw[s.FileOff:s.FileOff+n])
		clear(l.Mem[at+uint64(n) : at+uint64(s.VMSize)])
	}
	if l.byPath == nil {
		l.byPath = map[string]*Image{}
	}
	if real, err := filepath.EvalSymlinks(img.Path); err == nil {
		l.byPath[filepath.Clean(real)] = img
	}
	l.byPath[filepath.Clean(img.Path)] = img
	l.Images = append(l.Images, img)
	return nil
}

// link loads the image's dependencies, then rebases and binds it, and records its initialisers.
func (l *Linker) link(img *Image) error {
	deps := make([]*Image, len(img.Deps))
	for i, d := range img.Deps {
		if isSystem(d) {
			continue
		}
		path, ok := l.find(d, img)
		if !ok {
			if img.Weak[i] {
				continue
			}
			return fmt.Errorf("%s: dependency %s not found", img.Name(), d)
		}
		if l.Skip != nil && l.Skip(path) {
			continue
		}
		dep := l.byPath[path]
		if dep == nil {
			var err error
			if dep, err = l.Open(path, img); err != nil {
				return err
			}
		}
		deps[i] = dep
	}
	if img.dyldInfo {
		if err := l.rebase(img); err != nil {
			return fmt.Errorf("%s: rebase: %w", img.Name(), err)
		}
		for _, stream := range [][2]uint32{{img.bindOff, img.bindSz}, {img.lazyOff, img.lazySz}} {
			if err := l.bind(img, deps, stream[0], stream[1], false); err != nil {
				return fmt.Errorf("%s: bind: %w", img.Name(), err)
			}
		}
		if err := l.bind(img, deps, img.weakOff, img.weakSz, true); err != nil {
			return fmt.Errorf("%s: weak bind: %w", img.Name(), err)
		}
	} else {
		if err := l.classicRebase(img); err != nil {
			return fmt.Errorf("%s: rebase: %w", img.Name(), err)
		}
		if err := l.classicBind(img, deps); err != nil {
			return fmt.Errorf("%s: bind: %w", img.Name(), err)
		}
	}
	for _, s := range img.Segs {
		for _, sc := range s.Sections {
			if sc.Flags&0xff == 0x9 { // S_MOD_INIT_FUNC_POINTERS
				for off := uint32(0); off+4 <= sc.Size; off += 4 {
					l.Inits = append(l.Inits, binary.LittleEndian.Uint32(l.Mem[sc.Addr+img.Slide+off:]))
				}
			}
		}
	}
	if l.Mapped != nil {
		l.Mapped(img)
	}
	return nil
}

func (l *Linker) put32(img *Image, addr uint32, v uint32) error {
	at := uint64(addr) + uint64(img.Slide)
	if at+4 > uint64(len(l.Mem)) {
		return fmt.Errorf("fixup at %#x outside guest memory", at)
	}
	binary.LittleEndian.PutUint32(l.Mem[at:], v)
	return nil
}

func (l *Linker) get32(img *Image, addr uint32) uint32 {
	at := uint64(addr) + uint64(img.Slide)
	if at+4 > uint64(len(l.Mem)) {
		return 0
	}
	return binary.LittleEndian.Uint32(l.Mem[at:])
}

func (l *Linker) rebase(img *Image) error {
	if img.Slide == 0 || img.rebaseSz == 0 {
		return nil
	}
	if uint64(img.rebaseOff)+uint64(img.rebaseSz) > uint64(len(img.raw)) {
		return fmt.Errorf("rebase info runs past the file")
	}
	b := img.raw[img.rebaseOff : img.rebaseOff+img.rebaseSz]
	var seg int
	var off uint64
	typ := uint8(1)
	do := func() error {
		addr, err := img.segAddr(seg, off)
		if err != nil {
			return err
		}
		switch typ {
		case 1, 2: // pointer, text absolute 32
			return l.put32(img, addr, l.get32(img, addr)+img.Slide)
		case 3: // text pc-relative 32
			return l.put32(img, addr, l.get32(img, addr)-img.Slide)
		}
		return fmt.Errorf("rebase type %d", typ)
	}
	for p := 0; p < len(b); {
		op, imm := b[p]&0xf0, b[p]&0x0f
		p++
		switch op {
		case 0x00: // DONE
			return nil
		case 0x10: // SET_TYPE_IMM
			typ = imm
		case 0x20: // SET_SEGMENT_AND_OFFSET_ULEB
			seg, off = int(imm), uleb(b, &p)
		case 0x30: // ADD_ADDR_ULEB
			off += uleb(b, &p)
		case 0x40: // ADD_ADDR_IMM_SCALED
			off += uint64(imm) * 4
		case 0x50: // DO_REBASE_IMM_TIMES
			for i := 0; i < int(imm); i++ {
				if err := do(); err != nil {
					return err
				}
				off += 4
			}
		case 0x60: // DO_REBASE_ULEB_TIMES
			n := uleb(b, &p)
			for i := uint64(0); i < n; i++ {
				if err := do(); err != nil {
					return err
				}
				off += 4
			}
		case 0x70: // DO_REBASE_ADD_ADDR_ULEB
			if err := do(); err != nil {
				return err
			}
			off += 4 + uleb(b, &p)
		case 0x80: // DO_REBASE_ULEB_TIMES_SKIPPING_ULEB
			n, skip := uleb(b, &p), uleb(b, &p)
			for i := uint64(0); i < n; i++ {
				if err := do(); err != nil {
					return err
				}
				off += 4 + skip
			}
		default:
			return fmt.Errorf("rebase opcode %#x", op)
		}
	}
	return nil
}

// bind processes a bind stream (regular, lazy, or weak coalescing).
func (l *Linker) bind(img *Image, deps []*Image, start, size uint32, weak bool) error {
	if size == 0 {
		return nil
	}
	if uint64(start)+uint64(size) > uint64(len(img.raw)) {
		return fmt.Errorf("bind info runs past the file")
	}
	b := img.raw[start : start+size]
	var seg int
	var off uint64
	var ordinal int64
	var sym string
	var flags uint8
	var addend int64
	typ := uint8(1)
	do := func() error {
		addr, err := img.segAddr(seg, off)
		if err != nil {
			return err
		}
		var target uint32
		var found bool
		if weak {
			// Coalescing: the first definition in load order wins (keep ours if none).
			if target, found = l.lookupAll(sym); !found {
				return nil
			}
		} else if target, found = l.resolve(img, deps, ordinal, sym); !found {
			if flags&0x1 != 0 { // BIND_SYMBOL_FLAGS_WEAK_IMPORT: may be missing
				target = 0
			} else {
				return nil // recorded as unresolved by resolve; leave the pointer alone
			}
		}
		v := uint32(int64(target) + addend)
		switch typ {
		case 1, 2:
			return l.put32(img, addr, v)
		case 3:
			return l.put32(img, addr, v-(addr+img.Slide+4))
		}
		return fmt.Errorf("bind type %d", typ)
	}
	for p := 0; p < len(b); {
		op, imm := b[p]&0xf0, b[p]&0x0f
		p++
		switch op {
		case 0x00: // DONE (lazy streams continue after it)
		case 0x10: // SET_DYLIB_ORDINAL_IMM
			ordinal = int64(imm)
		case 0x20: // SET_DYLIB_ORDINAL_ULEB
			ordinal = int64(uleb(b, &p))
		case 0x30: // SET_DYLIB_SPECIAL_IMM: 0 self, -1 main executable, -2 flat lookup
			if imm == 0 {
				ordinal = 0
			} else {
				ordinal = int64(int8(imm | 0xf0))
			}
		case 0x40: // SET_SYMBOL_TRAILING_FLAGS_IMM
			flags = imm
			end := p
			for end < len(b) && b[end] != 0 {
				end++
			}
			sym = string(b[p:end])
			p = end + 1
		case 0x50: // SET_TYPE_IMM
			typ = imm
		case 0x60: // SET_ADDEND_SLEB
			addend = sleb(b, &p)
		case 0x70: // SET_SEGMENT_AND_OFFSET_ULEB
			seg, off = int(imm), uleb(b, &p)
		case 0x80: // ADD_ADDR_ULEB
			off += uleb(b, &p)
		case 0x90: // DO_BIND
			if err := do(); err != nil {
				return err
			}
			off += 4
		case 0xa0: // DO_BIND_ADD_ADDR_ULEB
			if err := do(); err != nil {
				return err
			}
			off += 4 + uleb(b, &p)
		case 0xb0: // DO_BIND_ADD_ADDR_IMM_SCALED
			if err := do(); err != nil {
				return err
			}
			off += 4 + uint64(imm)*4
		case 0xc0: // DO_BIND_ULEB_TIMES_SKIPPING_ULEB
			n, skip := uleb(b, &p), uleb(b, &p)
			for i := uint64(0); i < n; i++ {
				if err := do(); err != nil {
					return err
				}
				off += 4 + skip
			}
		default:
			return fmt.Errorf("bind opcode %#x", op)
		}
	}
	return nil
}

// resolve finds sym for img by bind ordinal.
func (l *Linker) resolve(img *Image, deps []*Image, ordinal int64, sym string) (uint32, bool) {
	switch {
	case ordinal == 0:
		if a, ok := l.lookup(img, sym, 0); ok {
			return a, true
		}
	case ordinal == -1:
		if a, ok := l.lookup(l.Main, sym, 0); ok {
			return a, true
		}
	case ordinal == -2:
		if a, ok := l.lookupAll(sym); ok {
			return a, true
		}
		return l.System("", sym), true
	case ordinal >= 1 && int(ordinal) <= len(img.Deps):
		dep := img.Deps[ordinal-1]
		if deps[ordinal-1] == nil { // a system library, or a skipped image
			return l.System(dep, sym), true
		}
		if a, ok := l.lookup(deps[ordinal-1], sym, 0); ok {
			return a, true
		}
		l.missing(img, sym)
		return l.System(dep, sym), true
	}
	l.missing(img, sym)
	return 0, false
}

func (l *Linker) missing(img *Image, sym string) {
	if l.Unresolved == nil {
		l.Unresolved = map[string]map[string]bool{}
	}
	m := l.Unresolved[img.Name()]
	if m == nil {
		m = map[string]bool{}
		l.Unresolved[img.Name()] = m
	}
	m[sym] = true
}

// lookup finds an exported symbol in an image (following re-exports).
func (l *Linker) lookup(img *Image, sym string, depth int) (uint32, bool) {
	if img == nil || depth > 8 {
		return 0, false
	}
	if e, ok := img.Exports()[sym]; ok {
		if !e.reexport {
			return e.addr + img.textBase() + img.Slide, true
		}
		if int(e.ordinal) >= 1 && int(e.ordinal) <= len(img.Deps) {
			name := e.reexpName
			if name == "" {
				name = sym
			}
			dep := img.Deps[e.ordinal-1]
			if isSystem(dep) {
				return l.System(dep, name), true
			}
			if path, ok := l.find(dep, img); ok {
				return l.lookup(l.byPath[path], name, depth+1)
			}
		}
	}
	return 0, false
}

// lookupAll finds sym in any loaded image, in load order (flat namespace, weak coalescing).
func (l *Linker) lookupAll(sym string) (uint32, bool) {
	for _, img := range l.Images {
		if a, ok := l.lookup(img, sym, 0); ok {
			return a, true
		}
	}
	return 0, false
}

// Symbol is dlsym: sym (with its leading underscore) in img, or in every image (img nil).
func (l *Linker) Symbol(img *Image, sym string) (uint32, bool) {
	if img == nil {
		return l.lookupAll(sym)
	}
	return l.lookup(img, sym, 0)
}

// ImageAt returns the image containing guest address a.
func (l *Linker) ImageAt(a uint32) *Image {
	for _, img := range l.Images {
		lo, hi := img.Span()
		if a >= lo+img.Slide && a < hi+img.Slide {
			return img
		}
	}
	return nil
}

// UnresolvedReport lists missing guest symbols per image, sorted.
func (l *Linker) UnresolvedReport() []string {
	var out []string
	for name, syms := range l.Unresolved {
		var s []string
		for k := range syms {
			s = append(s, k)
		}
		sort.Strings(s)
		out = append(out, fmt.Sprintf("%s: %d missing: %s", name, len(s), strings.Join(s, " ")))
	}
	sort.Strings(out)
	return out
}

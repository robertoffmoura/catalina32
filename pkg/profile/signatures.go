package profile

import (
	"bytes"
	"strconv"
	"strings"
)

// Signature is a byte pattern matched against the start of a function. "??" bytes are wildcards
// (relocated operands such as call targets or PIC-relative displacements).
type Signature struct {
	bytes []byte
	mask  []bool // true = must match
}

func MustSignature(hex string) Signature {
	var s Signature
	for _, tok := range strings.Fields(hex) {
		if tok == "??" {
			s.bytes = append(s.bytes, 0)
			s.mask = append(s.mask, false)
			continue
		}
		v, err := strconv.ParseUint(tok, 16, 8)
		if err != nil {
			panic("bad signature byte " + tok)
		}
		s.bytes = append(s.bytes, byte(v))
		s.mask = append(s.mask, true)
	}
	return s
}

// findUnique returns the offset of the only 16-byte-aligned match in text (loaded at guest address
// base). Function entries are 16-byte aligned by the compilers these games were built with;
// requiring uniqueness avoids binding a shim to the wrong function.
func (s Signature) findUnique(text []byte, base uint32) (int, bool) {
	prefix := 0
	for prefix < len(s.mask) && s.mask[prefix] {
		prefix++
	}
	found := -1
	for start := 0; ; {
		i := bytes.Index(text[start:], s.bytes[:prefix])
		if i < 0 {
			break
		}
		off := start + i
		start = off + 1
		if (base+uint32(off))&15 != 0 || off+len(s.bytes) > len(text) || !s.matchAt(text, off) {
			continue
		}
		if found >= 0 {
			return 0, false
		}
		found = off
	}
	return found, found >= 0
}

func (s Signature) matchAt(text []byte, off int) bool {
	for i, b := range s.bytes {
		if s.mask[i] && text[off+i] != b {
			return false
		}
	}
	return true
}

// Signatures identifies statically linked third-party library functions that the runtime replaces
// with native implementations. They match the libogg/libvorbis build shipped in Feral's i386 Mac
// ports (first seen in LEGO Star Wars: The Complete Saga); a game built with a different compiler
// or library version simply falls back to emulating the original code.
var Signatures = map[string]Signature{
	"ogg:oggpack_look": MustSignature("55 89 e5 53 57 56 50 8b 4d 08 8b 75 0c 8b 01 8b 51 04 8d 78 04 8b 59 10 39 df 8d 3c 32 7c 12 8d 34 c7 c1 e3 03 b8 ff ff ff ff 39 de 8b 75 0c 7f"),
	"ogg:oggpack_adv":  MustSignature("55 89 e5 8b 45 08 8b 48 04 03 4d 0c 89 ca c1 fa 1f c1 ea 1d 01 ca c1 fa 03 01 50 0c 01 10 83 e1 07 89 48 04 5d c3"),
	"ogg:oggpack_read": MustSignature("55 89 e5 53 57 56 83 ec 08 8b 55 08 8b 45 0c 8b 1a 8b 7a 04 8d 73 04 8b 4a 10 39 ce 8d 34 07 7c 1e 8d 04 de c1 e1 03 39 c8 8b 45 0c 7e 11 8b 4a"),
	// Static helper with register arguments (ECX = codebook, EDX = oggpack_buffer).
	"vorbis:decode_packed_entry_number": MustSignature("55 89 e5 53 57 56 83 ec 1c 89 55 f0 89 ce 8b 46 24 8b 4e 28 89 4d e4 89 44 24 04 89 14 24 e8 ?? ?? ?? ?? 85 c0 78 18 8b 4e 20 8b 1c 81 85 db 78"),
	"vorbis:vorbis_book_decodev_set":    MustSignature("55 89 e5 53 57 56 83 ec 1c 31 c9 83 7d 14 00 0f 8e c4 00 00 00 8b 45 0c 8b 75 08 8d 40 04 89 45 d8 31 ff"),
	"vorbis:vorbis_book_decodevv_add":   MustSignature("55 89 e5 53 57 56 83 ec 1c 8b 7d 14 8b 4d 10 89 c8 99 f7 ff 89 c6 03 4d 1c 89 c8 99 f7 ff 89 c1 89 4d e8 31 c0 39 ce 0f 8d a5 00 00 00 8b 5d 08"),
	"vorbis:mdct_backward":              MustSignature("55 89 e5 53 57 56 83 ec 4c 8b 55 10 8b 75 08 8b 06 89 c3 d1 fb 89 5d cc 89 c7 c1 ff 02 89 7d d0 83 e0 fc 8d 0c 02 8d 0c 99 8b 76 08 89 75 f0 8d"),
}

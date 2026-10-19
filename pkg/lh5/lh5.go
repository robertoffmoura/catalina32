// Package lh5 decodes LHA's -lh5- format: blocks of static Huffman codes over an 8 KB LZ77
// window, as in Haruhiko Okumura and Haruyasu Yoshizaki's ar002 and the decoders derived from
// it that games embed. The bit reader holds 32 bits instead of ar002's 16, as such derivatives
// often do; the bytes decoded are the same.
//
// All of the decoder's state lives in a State, laid out as ar002 keeps it (code tables, tree,
// window, bit reader, pending match). A runtime that replaces a game's embedded copy of this
// decoder can load that copy's state, decode, and store the state back as the original would
// have left it. Input the original would mis-decode (an inconsistent code table, a run past the
// end of a table) is reported as ErrCorrupt instead, with State partly updated.
package lh5

import "errors"

const (
	DicBits    = 13
	DicSize    = 1 << DicBits              // the window
	NC         = 255 + 256 + 2 - Threshold // literal and match-length codes (510)
	NP         = DicBits + 1               // position codes
	NT         = 16 + 3                    // code-length codes
	NPT        = NT                        // max(NT, NP)
	Threshold  = 3                         // shortest match
	CBits      = 9
	TBits      = 5
	PBits      = 4
	CTableBits = 12
	PTableBits = 8
)

// ErrCorrupt reports input that the reference decoder would not decode meaningfully.
var ErrCorrupt = errors.New("lh5: corrupt input")

// State is the decoder's complete state.
type State struct {
	CTable    [1 << CTableBits]uint16
	CLen      [NC]uint8
	PTTable   [1 << PTableBits]uint16
	PTLen     [NPT]uint8
	Left      [2*NC - 1]uint16
	Right     [2*NC - 1]uint16
	Text      [DicSize]byte // the window; Decode writes into its first count bytes
	BitBuf    uint32        // the next 32 bits of input, most significant first
	SubBitBuf uint32        // the input byte being shifted in
	BitCount  uint32        // bits of SubBitBuf not yet in BitBuf
	BlockSize uint32        // codes left in the current block
	J         int32         // bytes left to copy of the current match (negative: none)
	I         uint32        // window position the match copies from
	In        []byte        // input not yet read (reads past its end give zero bytes)
}

// fillBuf shifts n bits out of BitBuf and refills it from the input.
func (s *State) fillBuf(n uint32) {
	if n < 32 {
		s.BitBuf <<= n
	} else {
		s.BitBuf = 0
	}
	for s.BitCount < n {
		n -= s.BitCount
		if n <= 31 {
			s.BitBuf |= s.SubBitBuf << n
		}
		s.SubBitBuf = 0
		if len(s.In) > 0 {
			s.SubBitBuf = uint32(s.In[0])
			s.In = s.In[1:]
		}
		s.BitCount = 8
	}
	s.BitCount -= n
	if s.BitCount <= 31 {
		s.BitBuf |= s.SubBitBuf >> s.BitCount
	}
}

// getBits reads n (1 to 16) bits.
func (s *State) getBits(n uint32) uint32 {
	x := s.BitBuf >> (32 - n)
	s.fillBuf(n)
	return x
}

// InitBits starts reading In (ar002's init_getbits).
func (s *State) InitBits() {
	s.BitBuf, s.SubBitBuf, s.BitCount = 0, 0, 0
	s.fillBuf(32)
}

// walk follows the code tree from node j while it is at least limit, one BitBuf bit per level
// starting at mask.
func (s *State) walk(j, limit, mask uint32) (uint32, error) {
	for depth := 0; j >= limit; depth++ {
		if int(j) >= len(s.Left) || depth > 32 {
			return 0, ErrCorrupt
		}
		if s.BitBuf&mask != 0 {
			j = uint32(s.Right[j])
		} else {
			j = uint32(s.Left[j])
		}
		mask >>= 1
	}
	return j, nil
}

func (s *State) readPTLen(nn int, nbit uint32, iSpecial int) error {
	n := s.getBits(nbit)
	if n == 0 {
		c := s.getBits(nbit)
		clear(s.PTLen[:nn])
		for i := range s.PTTable {
			s.PTTable[i] = uint16(c)
		}
		return nil
	}
	i := 0
	for i < int(n) {
		c := s.BitBuf >> 29
		if c == 7 {
			for mask := uint32(1) << 28; mask&s.BitBuf != 0; mask >>= 1 {
				c++
			}
		}
		if c < 7 {
			s.fillBuf(3)
		} else {
			s.fillBuf(c - 3)
		}
		if i >= len(s.PTLen) {
			return ErrCorrupt
		}
		s.PTLen[i] = uint8(c)
		i++
		if i == iSpecial {
			c := int(s.getBits(2))
			if i+c > len(s.PTLen) {
				return ErrCorrupt
			}
			clear(s.PTLen[i : i+c])
			i += c
		}
	}
	if i < nn {
		clear(s.PTLen[i:nn])
	}
	return s.makeTable(s.PTLen[:nn], PTableBits, s.PTTable[:])
}

func (s *State) readCLen() error {
	n := s.getBits(CBits)
	if n == 0 {
		c := s.getBits(CBits)
		clear(s.CLen[:])
		for i := range s.CTable {
			s.CTable[i] = uint16(c)
		}
		return nil
	}
	i := 0
	for i < int(n) {
		c, err := s.walk(uint32(s.PTTable[s.BitBuf>>24]), NT, 1<<23)
		if err != nil {
			return err
		}
		s.fillBuf(uint32(s.PTLen[c]))
		if c <= 2 {
			count := 1
			switch c {
			case 1:
				count = int(s.getBits(4)) + 3
			case 2:
				count = int(s.getBits(CBits)) + 20
			}
			if i+count > NC {
				return ErrCorrupt
			}
			clear(s.CLen[i : i+count])
			i += count
		} else {
			if i >= NC {
				return ErrCorrupt
			}
			s.CLen[i] = uint8(c - 2)
			i++
		}
	}
	if i < NC {
		clear(s.CLen[i:])
	}
	return s.makeTable(s.CLen[:], CTableBits, s.CTable[:])
}

// makeTable builds the lookup table for the code lengths bitLen: codes up to tableBits long
// map straight to their symbol, longer ones to a node of the Left/Right tree.
func (s *State) makeTable(bitLen []uint8, tableBits uint32, table []uint16) error {
	var count, weight, start [18]uint16
	for _, l := range bitLen {
		if l > 16 {
			return ErrCorrupt
		}
		count[l]++
	}
	for i := 1; i <= 16; i++ {
		start[i+1] = start[i] + count[i]<<(16-i)
	}
	if start[17] != 0 {
		return ErrCorrupt // ar002's "Bad table"
	}
	jutBits := 16 - tableBits
	for i := uint32(1); i <= tableBits; i++ {
		start[i] >>= jutBits
		weight[i] = 1 << (tableBits - i)
	}
	for i := tableBits + 1; i <= 16; i++ {
		weight[i] = 1 << (16 - i)
	}
	if i := uint32(start[tableBits+1] >> jutBits); i != 0 {
		clear(table[i:])
	}
	avail := uint32(len(bitLen))
	mask := uint32(1) << (15 - tableBits)
	for ch, l := range bitLen {
		if l == 0 {
			continue
		}
		next := uint32(start[l]) + uint32(weight[l])
		if uint32(l) <= tableBits {
			if next > uint32(len(table)) {
				return ErrCorrupt
			}
			for i := uint32(start[l]); i < next; i++ {
				table[i] = uint16(ch)
			}
		} else {
			k := uint32(start[l])
			p := &table[k>>jutBits]
			for i := uint32(l) - tableBits; i != 0; i-- {
				if *p == 0 {
					if int(avail) >= len(s.Left) {
						return ErrCorrupt
					}
					s.Right[avail], s.Left[avail] = 0, 0
					*p = uint16(avail)
					avail++
				}
				if int(*p) >= len(s.Left) {
					return ErrCorrupt
				}
				if k&mask != 0 {
					p = &s.Right[*p]
				} else {
					p = &s.Left[*p]
				}
				k <<= 1
			}
			*p = uint16(ch)
		}
		start[l] = uint16(next)
	}
	return nil
}

func (s *State) decodeC() (uint32, error) {
	if s.BlockSize == 0 {
		s.BlockSize = s.getBits(16)
		if err := s.readPTLen(NT, TBits, 3); err != nil {
			return 0, err
		}
		if err := s.readCLen(); err != nil {
			return 0, err
		}
		if err := s.readPTLen(NP, PBits, -1); err != nil {
			return 0, err
		}
	}
	s.BlockSize--
	j, err := s.walk(uint32(s.CTable[s.BitBuf>>20]), NC, 1<<19)
	if err != nil {
		return 0, err
	}
	s.fillBuf(uint32(s.CLen[j]))
	return j, nil
}

func (s *State) decodeP() (uint32, error) {
	j, err := s.walk(uint32(s.PTTable[s.BitBuf>>24]), NP, 1<<23)
	if err != nil {
		return 0, err
	}
	s.fillBuf(uint32(s.PTLen[j]))
	if j > 1 {
		j = 1<<(j-1) + s.getBits(j-1)
	}
	return j, nil
}

// Decode decodes count (at most DicSize) bytes into Text[:count] (ar002's decode): a match
// still pending from the previous call continues first, and one that does not fit is left
// pending in J and I.
func (s *State) Decode(count int) error {
	r := 0
	copyMatch := func() bool {
		for {
			s.J--
			if s.J < 0 {
				return false
			}
			s.Text[r] = s.Text[s.I]
			s.I = (s.I + 1) & (DicSize - 1)
			r++
			if r == count {
				return true
			}
		}
	}
	if count <= 0 || copyMatch() {
		return nil
	}
	for {
		c, err := s.decodeC()
		if err != nil {
			return err
		}
		if c <= 255 {
			s.Text[r] = byte(c)
			r++
			if r == count {
				return nil
			}
			continue
		}
		s.J = int32(c) - (256 - Threshold)
		p, err := s.decodeP()
		if err != nil {
			return err
		}
		s.I = (uint32(r) - p - 1) & (DicSize - 1)
		if copyMatch() {
			return nil
		}
	}
}

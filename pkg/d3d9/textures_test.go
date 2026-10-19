package d3d9

import (
	"encoding/binary"
	"testing"

	"catalina32/pkg/cpu"
)

// The pitch LockRect reports must be the row layout decodeTexturePixels reads.
func TestTexPitchMatchesDecoder(t *testing.T) {
	const dxt1, dxt5 = 0x31545844, 0x35545844
	for _, tc := range []struct{ w, format, want uint32 }{
		{256, dxt1, 512}, {256, dxt5, 1024}, {2, dxt1, 8}, {6, dxt5, 32},
		{256, 50, 256}, {256, 28, 256}, {256, 21, 1024},
	} {
		if got := texPitch(tc.w, tc.format); got != tc.want {
			t.Errorf("texPitch(%d, %#x) = %d, want %d", tc.w, tc.format, got, tc.want)
		}
	}
	// A DXT1 image written with the reported pitch decodes to the colour of its blocks.
	const w, h = 8, 8
	pitch := texPitch(w, dxt1)
	data := make([]byte, pitch*texRows(h, dxt1))
	for i := 0; i < len(data); i += 8 {
		data[i], data[i+1] = 0x00, 0xF8 // c0 = pure red (RGB565), indices 0
		data[i+2], data[i+3] = 0x00, 0x00
	}
	px := decodeTexturePixels(data, w, h, dxt1)
	for i, p := range px {
		if p&0xFFFFFF != 0x0000FF { // red in the low byte
			t.Fatalf("texel %d = %#x, want red", i, p)
		}
	}
}

// A DDS cube map (six faces, each with its mip chain) loads as six level-0 faces in order.
func TestCubeMapFromDDS(t *testing.T) {
	c := cpu.NewCPU()
	const edge, levels = 4, 3
	face := ddsLevelsSize(edge, edge, 21, levels)
	if face != (16+4+1)*4 {
		t.Fatalf("face size %d, want %d", face, (16+4+1)*4)
	}
	file := make([]byte, 128+6*face)
	le := binary.LittleEndian
	le.PutUint32(file[0:], 0x20534444) // "DDS "
	le.PutUint32(file[8:], 0x20000)    // DDSD_MIPMAPCOUNT
	le.PutUint32(file[12:], edge)
	le.PutUint32(file[16:], edge)
	le.PutUint32(file[28:], levels)
	le.PutUint32(file[88:], 32)           // 32 bits per texel
	le.PutUint32(file[92:], 0x00FF0000)   // red mask: A8R8G8B8
	le.PutUint32(file[112:], ddsAllFaces) // all six faces
	for f := uint32(0); f < 6; f++ {
		for i := uint32(0); i < face; i += 4 { // B, G, R, A: face number in blue, mip data marked 0xEE
			v := []byte{byte(f), 0, 0, 255}
			if i >= edge*edge*4 {
				v = []byte{0xEE, 0xEE, 0xEE, 0xEE}
			}
			copy(file[128+f*face+i:], v)
		}
	}
	const at = 0x100000
	copy(c.Mem[at:], file)
	hd, ok := readDDSHeader(c, at, uint32(len(file)))
	if !ok || hd.w != edge || hd.mips != levels || hd.format != 21 || hd.caps2&ddsAllFaces != ddsAllFaces {
		t.Fatalf("header %+v ok=%v", hd, ok)
	}
	s := &Subsystem{}
	px, raw := s.decodeCube(file[128:], edge, hd.format, ddsLevelsSize(hd.w, hd.h, hd.format, hd.mips))
	if len(px) != 6*edge*edge || raw != nil {
		t.Fatalf("%d texels (raw %d bytes), want %d and no raw data", len(px), len(raw), 6*edge*edge)
	}
	for i, p := range px {
		if want := packRGBA(0, 0, byte(i/(edge*edge)), 255); p != want {
			t.Fatalf("texel %d (face %d) = %#x, want %#x", i, i/(edge*edge), p, want)
		}
	}
}

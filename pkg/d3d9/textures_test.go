package d3d9

import "testing"

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

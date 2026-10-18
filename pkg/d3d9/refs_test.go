package d3d9

import (
	"testing"

	"catalina32/pkg/cpu"
)

func TestReferenceCounts(t *testing.T) {
	c := cpu.NewCPU()
	s := &Subsystem{Resources: map[uint32]*BufferDesc{}, vtTexture9: 0x100, vtSurface9: 0x200}
	tex := s.allocCOMObject(c, s.vtTexture9, 32)
	other := s.allocCOMObject(c, s.vtTexture9, 32)
	s.Resources[tex] = &BufferDesc{Width: 4, Height: 4}

	// A hook binds the texture (BindTexture), the game releases its own reference: the device's
	// reference keeps it alive until another texture is bound to the stage.
	s.BindTexture(c, 0, tex)
	if n := s.release(c, tex); n != 1 {
		t.Fatalf("after the game's Release: count %d, want 1 (the binding)", n)
	}
	if s.refs[tex] == nil {
		t.Fatal("texture destroyed while bound")
	}
	s.BindTexture(c, 0, other)
	if s.refs[tex] != nil || s.Resources[tex] != nil {
		t.Fatal("texture not destroyed after its last reference went away")
	}

	// Child surfaces forward their references to the texture.
	s.Resources[other] = &BufferDesc{Width: 4, Height: 4}
	surf := s.child(c, other, 0, func() uint32 {
		o := s.allocCOMObject(c, s.vtSurface9, 32)
		s.Resources[o] = s.Resources[other]
		return o
	})
	if again := s.child(c, other, 0, nil); again != surf {
		t.Fatalf("GetSurfaceLevel returned %#x then %#x", surf, again)
	}
	if got := s.refs[other].count; got != 4 { // creation + binding + two GetSurfaceLevel
		t.Fatalf("texture count %d, want 4", got)
	}
	s.release(c, surf)
	s.release(c, surf)
	s.release(c, other)
	s.BindTexture(c, 0, 0)
	if s.refs[other] != nil || s.refs[surf] != nil {
		t.Fatal("texture and its child survived their last reference")
	}
}

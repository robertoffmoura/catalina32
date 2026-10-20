package source

import (
	"testing"

	"catalina32/pkg/cpu"
)

// dynamic_cast walks single and multiple inheritance, adjusting for base offsets.
func TestDynamicCast(t *testing.T) {
	m, err := New("", nil, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := m.C
	vt := func(kind string) uint32 { return m.system("", "__ZTVN10__cxxabiv1"+kind+"E") + 8 }
	name := func(s string) uint32 { a := m.data(uint32(len(s)) + 1); c.WriteCString(a, s); return a }
	// class Entity; class Player : Entity (si); class Panel; class Hud : Element, Panel (vmi, Panel at +8)
	tiEntity := m.data(8)
	c.Write32(tiEntity, vt("17__class_type_info"))
	c.Write32(tiEntity+4, name("6Entity"))
	tiPlayer := m.data(12)
	c.Write32(tiPlayer, vt("20__si_class_type_info"))
	c.Write32(tiPlayer+4, name("6Player"))
	c.Write32(tiPlayer+8, tiEntity)
	tiPortal := m.data(12) // class PortalPlayer : Player
	c.Write32(tiPortal, vt("20__si_class_type_info"))
	c.Write32(tiPortal+4, name("12PortalPlayer"))
	c.Write32(tiPortal+8, tiPlayer)
	tiElement, tiPanel := m.data(8), m.data(8)
	for ti, n := range map[uint32]string{tiElement: "7Element", tiPanel: "5Panel"} {
		c.Write32(ti, vt("17__class_type_info"))
		c.Write32(ti+4, name(n))
	}
	tiHud := m.data(32)
	c.Write32(tiHud, vt("21__vmi_class_type_info"))
	c.Write32(tiHud+4, name("3Hud"))
	c.Write32(tiHud+12, 2)
	c.Write32(tiHud+16, tiElement)
	c.Write32(tiHud+20, 0<<8|2) // public, at 0
	c.Write32(tiHud+24, tiPanel)
	c.Write32(tiHud+28, 8<<8|2) // public, at 8
	// Objects: a vtable is { offset to top, type_info, functions... }; objects point at the functions.
	object := func(ti uint32, subs ...uint32) uint32 { // subs: offsets of secondary bases
		obj := c.Malloc(32)
		v := m.data(16)
		c.Write32(v+4, ti)
		c.Write32(obj, v+8)
		for _, off := range subs {
			v2 := m.data(16)
			c.Write32(v2, uint32(-int32(off)))
			c.Write32(v2+4, ti)
			c.Write32(obj+off, v2+8)
		}
		return obj
	}
	player, portal, hud := object(tiPlayer), object(tiPortal), object(tiHud, 8)
	cast := func(src, from, to uint32) uint32 {
		c.Regs[cpu.RegESP] = 0x20000000
		for i, v := range []uint32{0, src, from, to, 0xffffffff} {
			c.Write32(c.Regs[cpu.RegESP]+uint32(4*i), v)
		}
		m.dynamicCast(c)
		return c.Regs[cpu.RegEAX]
	}
	if got := cast(player, tiEntity, tiPlayer); got != player {
		t.Errorf("Entity -> Player: %#x, want %#x", got, player)
	}
	if got := cast(portal, tiEntity, tiPlayer); got != portal {
		t.Errorf("Entity -> Player on a PortalPlayer: %#x, want %#x", got, portal)
	}
	if got := cast(hud, tiElement, tiPanel); got != hud+8 {
		t.Errorf("Element -> Panel (cross cast): %#x, want %#x", got, hud+8)
	}
	if got := cast(hud+8, tiPanel, tiElement); got != hud {
		t.Errorf("Panel -> Element: %#x, want %#x", got, hud)
	}
	if got := cast(player, tiEntity, tiPanel); got != 0 {
		t.Errorf("Entity -> Panel: %#x, want 0", got)
	}
}

func TestSwscanf(t *testing.T) {
	m, err := New("", nil, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := m.C
	out := c.Malloc(64)
	va := c.Malloc(16)
	for i := uint32(0); i < 3; i++ {
		c.Write32(va+4*i, out+4*i)
	}
	if n := m.scan(c, []rune("219,112,147"), []rune("%i,%i,%i"), va, true); n != 3 {
		t.Fatalf("stored %d values, want 3", n)
	}
	if r, g, b := c.Read32(out), c.Read32(out+4), c.Read32(out+8); r != 219 || g != 112 || b != 147 {
		t.Fatalf("got %d,%d,%d", r, g, b)
	}
	c.Write32(va, out)
	c.Write32(va+4, out+8)
	if n := m.scan(c, []rune(" 0x1F  2.5"), []rune("%x %lf"), va, true); n != 2 || c.Read32(out) != 0x1f || c.ReadF64(out+8) != 2.5 {
		t.Fatalf("hex/double: n=%d %#x %v", n, c.Read32(out), c.ReadF64(out+8))
	}
}

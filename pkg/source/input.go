package source

import (
	"encoding/binary"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3d9"
)

// Input: the viewer writes keyboard and mouse events into the GL stream's shared memory
// (glremote.go: the event ring); SDL_PollEvent turns them into SDL2 events.
//
// An event (glEventSize bytes): u32 type, u32 Mac key code, u32 modifier flags (NSEvent),
// i32 x, i32 y (in the game window), i32 dx, i32 dy (motion, or wheel steps in dy), u32 button
// (1 left, 2 middle, 3 right; for keys: 1 = a repeat), text[32] (UTF-8).
const (
	evKeyDown = iota + 1
	evKeyUp
	evMotion
	evButtonDown
	evButtonUp
	evWheel
	evText
	evQuit
)

// SDL2 event types.
const (
	sdlQuit            = 0x100
	sdlKeyDown         = 0x300
	sdlKeyUp           = 0x301
	sdlTextInput       = 0x303
	sdlMouseMotion     = 0x400
	sdlMouseButtonDown = 0x401
	sdlMouseButtonUp   = 0x402
	sdlMouseWheel      = 0x403
	sdlEventSize       = 56
)

// macToSDL maps Mac virtual key codes to SDL scancodes.
var macToSDL = map[uint32]uint32{
	0x00: 4, 0x0B: 5, 0x08: 6, 0x02: 7, 0x0E: 8, 0x03: 9, 0x05: 10, 0x04: 11, 0x22: 12, 0x26: 13, 0x28: 14,
	0x25: 15, 0x2E: 16, 0x2D: 17, 0x1F: 18, 0x23: 19, 0x0C: 20, 0x0F: 21, 0x01: 22, 0x11: 23, 0x20: 24,
	0x09: 25, 0x0D: 26, 0x07: 27, 0x10: 28, 0x06: 29, // A..Z
	0x12: 30, 0x13: 31, 0x14: 32, 0x15: 33, 0x17: 34, 0x16: 35, 0x1A: 36, 0x1C: 37, 0x19: 38, 0x1D: 39, // 1..0
	0x24: 40, 0x35: 41, 0x33: 42, 0x30: 43, 0x31: 44, 0x1B: 45, 0x18: 46, 0x21: 47, 0x1E: 48, 0x2A: 49,
	0x29: 51, 0x27: 52, 0x32: 53, 0x2B: 54, 0x2F: 55, 0x2C: 56, 0x39: 57,
	0x7A: 58, 0x78: 59, 0x63: 60, 0x76: 61, 0x60: 62, 0x61: 63, 0x62: 64, 0x64: 65, 0x65: 66, 0x6D: 67, 0x67: 68, 0x6F: 69, // F1..F12
	0x72: 73, 0x73: 74, 0x74: 75, 0x75: 76, 0x77: 77, 0x79: 78, 0x7C: 79, 0x7B: 80, 0x7D: 81, 0x7E: 82,
	0x47: 83, 0x4B: 84, 0x43: 85, 0x4E: 86, 0x45: 87, 0x4C: 88, 0x53: 89, 0x54: 90, 0x55: 91, 0x56: 92,
	0x57: 93, 0x58: 94, 0x59: 95, 0x5B: 96, 0x5C: 97, 0x52: 98, 0x41: 99, 0x51: 103,
	0x69: 104, 0x6B: 105, 0x71: 106, 0x6A: 107, 0x40: 108, // F13..F17
	0x3B: 224, 0x38: 225, 0x3A: 226, 0x37: 227, 0x3E: 228, 0x3C: 229, 0x3D: 230, 0x36: 231,
}

// sdlKeycode is the SDL keycode of a scancode: the character for printable keys.
func sdlKeycode(sc uint32) uint32 {
	switch {
	case sc >= 4 && sc <= 29:
		return 'a' + sc - 4
	case sc >= 30 && sc <= 38:
		return '1' + sc - 30
	case sc == 39:
		return '0'
	}
	if k, ok := map[uint32]uint32{40: '\r', 41: 27, 42: 8, 43: '\t', 44: ' ', 45: '-', 46: '=', 47: '[', 48: ']',
		49: '\\', 51: ';', 52: '\'', 53: '`', 54: ',', 55: '.', 56: '/', 76: 127}[sc]; ok {
		return k
	}
	return sc | 1<<30
}

// sdlMods converts NSEvent modifier flags to SDL_Keymod.
func sdlMods(f uint32) uint32 {
	var m uint32
	for bit, mod := range map[uint32]uint32{1 << 17: 0x1, 1 << 18: 0x40, 1 << 19: 0x100, 1 << 20: 0x400, 1 << 16: 0x2000} {
		if f&bit != 0 {
			m |= mod
		}
	}
	return m
}

type inputState struct {
	x, y     int32
	buttons  uint32
	relative bool   // SDL_SetRelativeMouseMode
	hidden   bool   // SDL_ShowCursor(0)
	captured bool   // the window holds the Mac's mouse (relative || hidden)
	fb       []byte // the viewer's framebuffer shared memory (game state flags), nil without one
}

// nextEvent takes the viewer's next event, or nil.
func (m *Machine) nextEvent() []byte {
	if m.glr == nil || !m.glOn {
		return nil
	}
	g := m.glr
	w := atomic.LoadUint32(g.word(128))
	r := atomic.LoadUint32(g.word(132))
	if r == w {
		return nil
	}
	off := glEventsOffset + glEventSize*int(r%glEvents)
	e := append([]byte(nil), g.shm[off:off+glEventSize]...)
	atomic.StoreUint32(g.word(132), r+1)
	return e
}

// pollEvent writes the next event as an SDL_Event at p; false when there is none.
func (m *Machine) pollEvent(c *cpu.CPU, p uint32) bool {
	if p == 0 { // SDL_PollEvent(NULL): is there one?
		return m.glr != nil && m.glOn && atomic.LoadUint32(m.glr.word(128)) != atomic.LoadUint32(m.glr.word(132))
	}
	for {
		e := m.nextEvent()
		if e == nil {
			return false
		}
		if m.sdlEvent(c, p, e) {
			return true
		}
	}
}

func (m *Machine) sdlEvent(c *cpu.CPU, p uint32, e []byte) bool {
	le := binary.LittleEndian
	typ, key, mods := le.Uint32(e), le.Uint32(e[4:]), le.Uint32(e[8:])
	x, y := int32(le.Uint32(e[12:])), int32(le.Uint32(e[16:]))
	dx, dy := int32(le.Uint32(e[20:])), int32(le.Uint32(e[24:]))
	button := le.Uint32(e[28:])
	in := &m.input
	if p != 0 {
		clear(c.WBytes(p, sdlEventSize))
		c.Write32(p+4, uint32(m.micros()/1000))
		c.Write32(p+8, 1) // window ID
	}
	put := func(off uint32, v uint32) {
		if p != 0 {
			c.Write32(p+off, v)
		}
	}
	switch typ {
	case evKeyDown, evKeyUp:
		sc, ok := macToSDL[key]
		if !ok {
			return false
		}
		t, state := uint32(sdlKeyDown), uint8(1)
		if typ == evKeyUp {
			t, state = sdlKeyUp, 0
		}
		put(0, t)
		if p != 0 {
			c.Write8(p+12, state)
			c.Write8(p+13, uint8(button))
			c.Write16(p+24, uint16(sdlMods(mods)))
		}
		put(16, sc)
		put(20, sdlKeycode(sc))
	case evText:
		put(0, sdlTextInput)
		if p != 0 {
			copy(c.WBytes(p+12, 32), e[32:63])
		}
	case evMotion:
		if in.captured { // raw movement moves the game's pointer
			x, y = in.x+dx, in.y+dy
			if in.relative { // SDL keeps a relative-mode pointer inside the window
				x, y = max(0, min(x, int32(m.sdl.winW)-1)), max(0, min(y, int32(m.sdl.winH)-1))
			}
		}
		in.x, in.y = x, y
		put(0, sdlMouseMotion)
		put(16, in.buttons)
		put(20, uint32(x))
		put(24, uint32(y))
		put(28, uint32(dx))
		put(32, uint32(dy))
	case evButtonDown, evButtonUp:
		if in.captured {
			x, y = in.x, in.y
		}
		in.x, in.y = x, y
		t, state := uint32(sdlMouseButtonDown), uint8(1)
		if typ == evButtonUp {
			t, state = sdlMouseButtonUp, 0
			in.buttons &^= 1 << (button - 1)
		} else {
			in.buttons |= 1 << (button - 1)
		}
		put(0, t)
		if p != 0 {
			c.Write8(p+16, uint8(button))
			c.Write8(p+17, state)
			c.Write8(p+18, 1)
		}
		put(20, uint32(x))
		put(24, uint32(y))
	case evWheel:
		put(0, sdlMouseWheel)
		put(20, uint32(dy))
	case evQuit:
		put(0, sdlQuit)
	default:
		return false
	}
	return true
}

// updateMouseCapture captures the Mac's mouse while the game hides its cursor or asks for
// relative mode (mouse look): the window hides the pointer and sends raw movement, which moves
// the game's own pointer. (Source looks around by warping its pointer back to the centre each
// frame, unless m_rawinput is on.)
func (m *Machine) updateMouseCapture() {
	in := &m.input
	on := in.relative || in.hidden
	if on == in.captured {
		return
	}
	in.captured = on
	if m.glOn {
		m.glr.scratch = m.glr.scratch[:0]
		m.glr.u32(b2u(on))
		m.glr.send(m.glr.ids["c32MouseMode"])
	}
	m.publishGameState(false)
}

// openFramebufferShm creates the viewer's framebuffer shared memory (C32_SHM_FILE): the
// window maps it for the game state (pointer shown, game ended), audio and controllers.
func (m *Machine) openFramebufferShm() {
	path := os.Getenv("C32_SHM_FILE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	if f.Truncate(d3d9.ShmTotalSize) != nil {
		return
	}
	if b, err := syscall.Mmap(int(f.Fd()), 0, d3d9.ShmTotalSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED); err == nil {
		m.input.fb = b
		m.publishGameState(false)
	}
}

// publishGameState tells the window whether the game shows a pointer, or has ended.
func (m *Machine) publishGameState(ended bool) {
	if m.input.fb == nil {
		return
	}
	var flags uint32
	if !m.input.captured {
		flags |= 1
	}
	if ended {
		flags |= 2
	}
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&m.input.fb[d3d9.ShmGameOffset])), flags)
}

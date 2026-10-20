package source

import (
	"encoding/binary"
	"slices"
	"sync/atomic"
	"unsafe"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3d9"
)

// Game controllers: the window writes each controller's state into the framebuffer shared
// memory (XInput layout: d3d9.ShmPadOffset), and SDL's game controller API reads it. Source
// takes controllers only as SDL events, so changes become events (device added or removed,
// axis, button), with SDL's numbering; the device index is the window's controller slot.

// SDL2 controller event types.
const (
	sdlControllerAxis    = 0x650
	sdlControllerDown    = 0x651
	sdlControllerUp      = 0x652
	sdlControllerAdded   = 0x653
	sdlControllerRemoved = 0x654
)

// XInput button bits in SDL_GameControllerButton order (A, B, X, Y, back, guide, start, left
// stick, right stick, left shoulder, right shoulder, d-pad up, down, left, right).
var padButtons = [15]uint16{0x1000, 0x2000, 0x4000, 0x8000, 0x20, 0, 0x10, 0x40, 0x80, 0x100, 0x200, 0x1, 0x2, 0x4, 0x8}

type padState struct {
	connected bool
	buttons   uint16
	axes      [6]int16 // SDL axes: left x, y, right x, y, left trigger, right trigger
}

type padInput struct {
	last    [d3d9.ShmPads]padState
	pending [][]byte           // SDL events not yet delivered
	heard   [d3d9.ShmPads]bool // the first input from each slot was logged
	polling bool               // the game has polled its events (controllers show from then on)
	handles map[uint32]int     // SDL_GameController / SDL_Joystick handles -> slot
}

// readPad reads slot i's state from the window's shared memory. Until the game first polls
// its events no controller shows: one there at launch then arrives as a newly connected one.
// (inputsystem finds controllers at its start, before the client exists: it can't switch the
// client's "joystick" on then, and config.cfg's "joystick 0" stays. A controller connected
// later, it switches on.)
func (m *Machine) readPad(i int) padState {
	fb := m.input.fb
	if fb == nil || !m.pads.polling || atomic.LoadUint32((*uint32)(unsafe.Pointer(&fb[d3d9.ShmPadOffset]))) != d3d9.ShmPadReady {
		return padState{}
	}
	r := fb[d3d9.ShmPadOffset+4+i*d3d9.ShmPadSize:]
	le := binary.LittleEndian
	p := padState{connected: le.Uint32(r) != 0, buttons: le.Uint16(r[4:])}
	if !p.connected {
		return padState{}
	}
	neg := func(v int16) int16 { // XInput's y axes point up, SDL's down
		if v == -32768 {
			return 32767
		}
		return -v
	}
	p.axes = [6]int16{int16(le.Uint16(r[8:])), neg(int16(le.Uint16(r[10:]))), int16(le.Uint16(r[12:])),
		neg(int16(le.Uint16(r[14:]))), int16(int(r[6]) * 32767 / 255), int16(int(r[7]) * 32767 / 255)}
	return p
}

// pollPads turns controller changes into pending SDL events.
func (m *Machine) pollPads() {
	pi := &m.pads
	pi.polling = true
	ev := func(typ uint32, fill func(e []byte)) {
		e := make([]byte, sdlEventSize)
		binary.LittleEndian.PutUint32(e, typ)
		binary.LittleEndian.PutUint32(e[4:], uint32(m.micros()/1000))
		fill(e)
		pi.pending = append(pi.pending, e)
	}
	for i := range pi.last {
		now, was := m.readPad(i), pi.last[i]
		pi.last[i] = now
		which := func(e []byte) { binary.LittleEndian.PutUint32(e[8:], uint32(i)) }
		if now.connected != was.connected {
			if now.connected {
				ev(sdlControllerAdded, which)
			} else {
				ev(sdlControllerRemoved, which)
			}
			was = padState{}
		}
		if !now.connected {
			continue
		}
		if !pi.heard[i] && (now.buttons != 0 || slices.ContainsFunc(now.axes[:], func(v int16) bool { return v > 8000 || v < -8000 })) {
			pi.heard[i] = true // diagnostics: the controller's input gets this far
			m.logf("[controller] player %d: input reached the game (buttons %#04x, axes %v)", i+1, now.buttons, now.axes)
		}
		for b, bit := range padButtons {
			if bit == 0 || (now.buttons^was.buttons)&bit == 0 {
				continue
			}
			typ, state := uint32(sdlControllerUp), byte(0)
			if now.buttons&bit != 0 {
				typ, state = sdlControllerDown, 1
			}
			ev(typ, func(e []byte) { which(e); e[12] = byte(b); e[13] = state })
		}
		for a := range now.axes {
			if now.axes[a] != was.axes[a] {
				ev(sdlControllerAxis, func(e []byte) {
					which(e)
					e[12] = byte(a)
					binary.LittleEndian.PutUint16(e[16:], uint16(now.axes[a]))
				})
			}
		}
	}
}

func (m *Machine) registerPadShims() {
	pi := &m.pads
	pi.handles = map[uint32]int{}
	open := func(c *cpu.CPU) {
		i := int(c.Arg(0))
		if i < 0 || i >= d3d9.ShmPads || !m.readPad(i).connected {
			ret(c, 0)
			return
		}
		h := c.Malloc(16)
		pi.handles[h] = i
		ret(c, h)
	}
	slot := func(c *cpu.CPU) int {
		if i, ok := pi.handles[c.Arg(0)]; ok {
			return i
		}
		return -1
	}
	name := m.glStr("Xbox 360 Controller")
	for k, v := range map[string]cpu.ThunkFunc{
		"_SDL_NumJoysticks": func(c *cpu.CPU) {
			n := uint32(0)
			for i := 0; i < d3d9.ShmPads; i++ {
				if m.readPad(i).connected {
					n = uint32(i + 1)
				}
			}
			ret(c, n)
		},
		"_SDL_IsGameController":          func(c *cpu.CPU) { ret(c, b2u(int(c.Arg(0)) < d3d9.ShmPads && m.readPad(int(c.Arg(0))).connected)) },
		"_SDL_GameControllerOpen":        open,
		"_SDL_JoystickOpen":              open,
		"_SDL_GameControllerGetJoystick": func(c *cpu.CPU) { ret(c, c.Arg(0)) },
		"_SDL_JoystickInstanceID":        func(c *cpu.CPU) { ret(c, uint32(int32(slot(c)))) },
		"_SDL_JoystickNameForIndex":      func(c *cpu.CPU) { ret(c, name) },
		"_SDL_GameControllerClose":       func(c *cpu.CPU) { delete(pi.handles, c.Arg(0)); c.Ret() },
		"_SDL_JoystickClose":             func(c *cpu.CPU) { delete(pi.handles, c.Arg(0)); c.Ret() },
		"_SDL_JoystickGetDeviceGUID": func(c *cpu.CPU) { // SDL_JoystickGUID (16 bytes) returned through a hidden pointer
			out := c.Arg(0)
			guid := []byte{0x03, 0, 0, 0, 0x5e, 0x04, 0, 0, 0x8e, 0x02, 0, 0, 0x14, 0x01, 0, 0} // an Xbox 360 pad
			copy(c.WBytes(out, 16), guid)
			c.Regs[cpu.RegEAX] = out
			c.RetN(4)
		},
		"_SDL_JoystickGetGUIDString": func(c *cpu.CPU) { // (guid by value: 16 bytes, char *out, int size)
			guid := c.Bytes(c.Regs[cpu.RegESP]+4, 16)
			out, size := c.Arg(4), c.Arg(5)
			const hex = "0123456789abcdef"
			s := make([]byte, 0, 33)
			for _, b := range guid {
				s = append(s, hex[b>>4], hex[b&15])
			}
			if size > 0 {
				n := min(uint32(len(s)), size-1)
				copy(c.WBytes(out, n), s)
				c.Write8(out+n, 0)
			}
			c.Ret()
		},
		"_SDL_HapticOpenFromJoystick": ret0, // no rumble yet
		"_SDL_HapticClose":            retVoid,
		"_SDL_HapticRumbleInit":       retN(0xffffffff),
		"_SDL_HapticRumblePlay":       retN(0xffffffff),
		"_SDL_HapticRumbleStop":       retN(0xffffffff),
	} {
		m.shims[k] = v
	}
}

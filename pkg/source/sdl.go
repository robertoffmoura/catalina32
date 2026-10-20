package source

import (
	"time"

	"catalina32/pkg/cpu"
)

// SDL2, headless for now: one window of the desktop's size, no events, no controllers. The
// native window (pkg/... Metal) replaces it later; this is what the launcher needs to proceed.

const (
	desktopW = 1440
	desktopH = 900
)

type sdlState struct {
	init     uint32
	windows  map[uint32][2]uint32
	frames   uint64
	errorStr uint32
	winW     uint32
	winH     uint32
}

func (m *Machine) registerSDLShims() {
	m.sdl.windows = map[uint32][2]uint32{}
	str := func(s string) uint32 {
		a := m.data(uint32(len(s)) + 1)
		m.C.WriteCString(a, s)
		return a
	}
	m.sdl.errorStr = str("")
	cocoa := str("cocoa")
	for k, v := range map[string]cpu.ThunkFunc{
		"_SDL_WasInit":               func(c *cpu.CPU) { ret(c, m.sdl.init&c.Arg(0)) },
		"_SDL_Init":                  func(c *cpu.CPU) { m.sdl.init |= c.Arg(0); m.connectViewer(); ret(c, 0) },
		"_SDL_InitSubSystem":         func(c *cpu.CPU) { m.sdl.init |= c.Arg(0); ret(c, 0) },
		"_SDL_QuitSubSystem":         func(c *cpu.CPU) { m.sdl.init &^= c.Arg(0); c.Ret() },
		"_SDL_GetError":              func(c *cpu.CPU) { ret(c, m.sdl.errorStr) },
		"_SDL_SetHint":               retN(1),
		"_SDL_GL_LoadLibrary":        ret0,
		"_SDL_GL_UnloadLibrary":      retVoid,
		"_SDL_GetCurrentVideoDriver": func(c *cpu.CPU) { ret(c, cocoa) },
		"_SDL_GL_SetAttribute":       ret0,
		"_SDL_CreateWindow": func(c *cpu.CPU) {
			w := c.Malloc(64)
			m.sdl.windows[w] = [2]uint32{c.Arg(3), c.Arg(4)}
			m.windowSized(c.Arg(3), c.Arg(4))
			m.logf("[sdl] CreateWindow %q %dx%d flags %#x", c.ReadCString(c.Arg(0)), c.Arg(3), c.Arg(4), c.Arg(5))
			ret(c, w)
		},
		"_SDL_DestroyWindow": retVoid,
		"_SDL_GetWindowSize": func(c *cpu.CPU) {
			s := m.sdl.windows[c.Arg(0)]
			if c.Arg(1) != 0 {
				c.Write32(c.Arg(1), s[0])
			}
			if c.Arg(2) != 0 {
				c.Write32(c.Arg(2), s[1])
			}
			c.Ret()
		},
		"_SDL_SetWindowSize": func(c *cpu.CPU) {
			m.sdl.windows[c.Arg(0)] = [2]uint32{c.Arg(1), c.Arg(2)}
			m.logf("[sdl] SetWindowSize %dx%d", c.Arg(1), c.Arg(2))
			m.windowSized(c.Arg(1), c.Arg(2))
			c.Ret()
		},
		"_SDL_GetWindowID":         retN(1),
		"_SDL_GetNumVideoDisplays": retN(1),
		"_SDL_GetDisplayBounds": func(c *cpu.CPU) {
			r := c.Arg(1)
			c.Write32(r, 0)
			c.Write32(r+4, 0)
			w, h := m.desktop()
			c.Write32(r+8, w)
			c.Write32(r+12, h)
			ret(c, 0)
		},
		"_SDL_GetCurrentDisplayMode": m.sdlDisplayMode,
		"_SDL_GetDesktopDisplayMode": m.sdlDisplayMode,
		"_SDL_GL_CreateContext":      func(c *cpu.CPU) { ret(c, c.Malloc(16)) },
		"_SDL_GL_MakeCurrent":        ret0,
		"_SDL_GL_DeleteContext":      retVoid,
		"_SDL_GL_SetSwapInterval":    ret0,
		"_SDL_GL_SwapWindow": func(c *cpu.CPU) {
			m.sdl.frames++
			if m.glOn {
				m.glr.present()
			}
			if m.sdl.frames <= 5 || m.sdl.frames%100 == 0 {
				m.logf("[sdl] frame %d presented at instruction %d", m.sdl.frames, c.Steps)
			}
			c.Ret()
		},
		"_SDL_GL_GetProcAddress": func(c *cpu.CPU) {
			ret(c, m.system("/System/Library/Frameworks/OpenGL.framework/OpenGL", "_"+c.ReadCString(c.Arg(0))))
		},
		"_SDL_GetWindowWMInfo": func(c *cpu.CPU) {
			info := c.Arg(1)
			c.Write32(info+4, 4) // SDL_SYSWM_COCOA
			c.Write32(info+8, c.Arg(0))
			ret(c, 1)
		},
		"_SDL_ShowWindow": retVoid, "_SDL_HideWindow": retVoid, "_SDL_RaiseWindow": retVoid,
		"_SDL_SetWindowTitle": retVoid, "_SDL_SetWindowPosition": retVoid, "_SDL_SetWindowBordered": retVoid,
		"_SDL_SetWindowFullscreen": ret0, "_SDL_SetWindowDisplayMode": ret0, "_SDL_SetWindowGammaRamp": ret0,
		"_SDL_SetWindowGrab": retVoid, "_SDL_GetWindowGrab": ret0, "_SDL_SetWindowIcon": retVoid,
		"_SDL_PollEvent": func(c *cpu.CPU) { ret(c, b2u(m.pollEvent(c, c.Arg(0)))) },
		"_SDL_PushEvent": retN(1),
		"_SDL_WaitEventTimeout": func(c *cpu.CPU) {
			if m.pollEvent(c, c.Arg(0)) {
				ret(c, 1)
				return
			}
			ms := c.Arg(1)
			ret(c, 0)
			m.sleepFor(uint64(ms) * 1000)
		},
		"_SDL_AddEventWatch": retVoid, "_SDL_DelEventWatch": retVoid,
		"_SDL_ShowCursor": func(c *cpu.CPU) { // (toggle: 1 show, 0 hide, -1 query) -> the previous state
			was := b2u(!m.input.hidden)
			if t := int32(c.Arg(0)); t >= 0 {
				m.input.hidden = t == 0
				m.updateMouseCapture()
			}
			ret(c, was)
		},
		"_SDL_WarpMouseInWindow": func(c *cpu.CPU) { // the game's pointer (not the Mac's) moves there
			m.input.x, m.input.y = int32(c.Arg(1)), int32(c.Arg(2))
			c.Ret()
		},
		"_SDL_GetMouseState": func(c *cpu.CPU) {
			if c.Arg(0) != 0 {
				c.Write32(c.Arg(0), uint32(m.input.x))
			}
			if c.Arg(1) != 0 {
				c.Write32(c.Arg(1), uint32(m.input.y))
			}
			ret(c, m.input.buttons)
		},
		"_SDL_SetRelativeMouseMode": func(c *cpu.CPU) { m.input.relative = c.Arg(0) != 0; m.updateMouseCapture(); ret(c, 0) },
		"_SDL_CreateSystemCursor":   func(c *cpu.CPU) { ret(c, c.Malloc(16)) }, "_SDL_SetCursor": retVoid,
		"_SDL_FreeSurface": retVoid, "_SDL_LoadBMP_RW": ret0, "_SDL_RWFromFile": ret0,
		"_SDL_ShowSimpleMessageBox": func(c *cpu.CPU) {
			m.logf("[sdl] message box %q: %q", c.ReadCString(c.Arg(1)), c.ReadCString(c.Arg(2)))
			ret(c, 0)
		},
		"_SDL_ShowMessageBox": func(c *cpu.CPU) {
			d := c.Arg(0)
			m.logf("[sdl] message box %q: %q", c.ReadCString(c.Read32(d+8)), c.ReadCString(c.Read32(d+12)))
			if c.Arg(1) != 0 {
				c.Write32(c.Arg(1), 0)
			}
			ret(c, 0)
		},
	} {
		m.shims[k] = v
	}
}

// SDL_DisplayMode { format, w, h, refresh_rate, driverdata }
func (m *Machine) sdlDisplayMode(c *cpu.CPU) {
	p := c.Arg(1)
	c.Write32(p, 0x16161804) // SDL_PIXELFORMAT_RGB888
	w, h := m.desktop()
	c.Write32(p+4, w)
	c.Write32(p+8, h)
	c.Write32(p+12, 60)
	c.Write32(p+16, 0)
	ret(c, 0)
}

// connectViewer waits for the viewer's GL context (once): with it, GL calls go to the stream.
func (m *Machine) connectViewer() {
	if m.glr == nil || m.glOn || m.glr.gone {
		return
	}
	if m.glOn = m.glr.waitViewer(30 * time.Second); !m.glOn {
		m.logf("[gl] no viewer: drawing nothing")
		m.glr.gone = true
		return
	}
	m.logf("[gl] the viewer is ready")
}

// windowSized tells the viewer the game window's size.
func (m *Machine) windowSized(w, h uint32) {
	m.sdl.winW, m.sdl.winH = w, h
	if m.glOn {
		m.glr.windowSize(w, h)
	}
}

// desktop is the size of the screen the game sees: the viewer's main screen, if known.
func (m *Machine) desktop() (uint32, uint32) {
	if m.glr != nil {
		if w, h := m.glr.screen(); w >= 640 && h >= 480 {
			return w, h
		}
	}
	return desktopW, desktopH
}

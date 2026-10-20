package source

import (
	"strconv"
	"strings"

	"catalina32/pkg/cpu"
)

// A null OpenGL: the GL that ToGL (Direct3D 9 on OpenGL) and the launcher call, answering
// queries like a capable Mac GL 2.1 driver and drawing nothing. It lets the engine run headless
// up to its frame loop; on the Mac these calls go to the system's OpenGL instead.

var glExtensions = strings.Join([]string{
	"GL_ARB_multitexture", "GL_ARB_vertex_buffer_object", "GL_ARB_pixel_buffer_object", "GL_ARB_shader_objects",
	"GL_ARB_vertex_shader", "GL_ARB_fragment_shader", "GL_ARB_shading_language_100", "GL_ARB_vertex_program",
	"GL_ARB_fragment_program", "GL_ARB_occlusion_query", "GL_ARB_texture_non_power_of_two", "GL_ARB_texture_rectangle",
	"GL_ARB_texture_border_clamp", "GL_ARB_depth_texture", "GL_ARB_shadow", "GL_ARB_half_float_pixel", "GL_ARB_half_float_vertex",
	"GL_ARB_texture_float", "GL_ARB_map_buffer_range", "GL_ARB_sync", "GL_ARB_framebuffer_object", "GL_ARB_vertex_array_bgra",
	"GL_EXT_framebuffer_object", "GL_EXT_framebuffer_blit", "GL_EXT_framebuffer_multisample", "GL_EXT_packed_depth_stencil",
	"GL_EXT_texture_compression_s3tc", "GL_EXT_texture_compression_dxt1", "GL_EXT_texture_sRGB", "GL_EXT_framebuffer_sRGB",
	"GL_EXT_texture_filter_anisotropic", "GL_EXT_bgra", "GL_EXT_blend_func_separate", "GL_EXT_blend_equation_separate",
	"GL_EXT_stencil_two_side", "GL_EXT_draw_buffers2", "GL_EXT_gpu_shader4", "GL_EXT_timer_query", "GL_EXT_texture_mirror_clamp",
	"GL_EXT_secondary_color", "GL_EXT_bindable_uniform", "GL_ATI_separate_stencil", "GL_APPLE_fence", "GL_APPLE_flush_buffer_range",
	"GL_APPLE_client_storage", "GL_APPLE_texture_range", "GL_APPLE_vertex_array_object", "GL_NV_fence",
}, " ")

type nullGL struct {
	next    uint32            // object names
	bound   map[uint32]uint32 // buffer target -> buffer
	sizes   map[uint32]uint32 // buffer -> size
	mapped  map[uint32]uint32 // buffer -> guest memory
	strings map[uint32]uint32 // glGetString name -> guest string
	calls   map[string]uint64
	ctx     uint32
}

func isGL(lib, sym string) bool {
	return strings.Contains(lib, "OpenGL") || strings.HasPrefix(sym, "_gl") || strings.HasPrefix(sym, "_CGL")
}

func (m *Machine) glStr(s string) uint32 {
	a := m.data(uint32(len(s)) + 1)
	m.C.WriteCString(a, s)
	return a
}

// glFunc returns a GL function: the stream to the viewer once it is drawing (glremote.go),
// the null GL otherwise.
func (m *Machine) glFunc(sym string) cpu.ThunkFunc {
	null := m.nullGLFunc(sym)
	if m.glr == nil || !strings.HasPrefix(sym, "_gl") {
		return null
	}
	name := strings.TrimPrefix(sym, "_")
	remote := m.glr.fn(name)
	return func(c *cpu.CPU) {
		switch {
		case !m.glOn || m.glr.gone:
			null(c)
		case remote != nil:
			remote(c)
		default:
			m.glr.logOnce(name, "%s isn't in the stream (glspec.go): ignored", name)
			null(c)
		}
	}
}

// nullGLFunc returns the null GL's version of a GL function.
func (m *Machine) nullGLFunc(sym string) cpu.ThunkFunc {
	g := &m.gl
	if g.bound == nil {
		*g = nullGL{next: 1, bound: map[uint32]uint32{}, sizes: map[uint32]uint32{}, mapped: map[uint32]uint32{},
			strings: map[uint32]uint32{
				0x1F00: m.glStr("ATI Technologies Inc."), 0x1F01: m.glStr("AMD Radeon Pro 560 OpenGL Engine"),
				0x1F02: m.glStr("2.1 ATI-3.10.19"), 0x1F03: m.glStr(glExtensions), 0x8B8C: m.glStr("1.20"),
			}, calls: map[string]uint64{}}
	}
	name := strings.TrimPrefix(sym, "_")
	count := func(c *cpu.CPU) { g.calls[name]++ }
	gen := func(c *cpu.CPU) { // glGen*(n, names)
		count(c)
		for i := uint32(0); i < c.Arg(0); i++ {
			c.Write32(c.Arg(1)+4*i, g.next)
			g.next++
		}
		c.Ret()
	}
	newName := func(c *cpu.CPU) { count(c); ret(c, g.next); g.next++ }
	switch {
	case name == "glGetString":
		return func(c *cpu.CPU) { count(c); ret(c, g.strings[c.Arg(0)]) }
	case name == "glGetError":
		return func(c *cpu.CPU) { count(c); ret(c, 0) }
	case name == "glGetIntegerv" || name == "glGetFloatv" || name == "glGetBooleanv" || name == "glGetDoublev":
		return func(c *cpu.CPU) { count(c); m.glGet(c, name, c.Arg(0), c.Arg(1)); c.Ret() }
	case strings.HasPrefix(name, "glGen"):
		return gen
	case name == "glCreateProgramObjectARB" || name == "glCreateShaderObjectARB" || name == "glCreateProgram" || name == "glCreateShader":
		return newName
	case strings.HasPrefix(name, "glGetUniformLocation") || strings.HasPrefix(name, "glGetAttribLocation"):
		return newName
	case strings.HasPrefix(name, "glCheckFramebufferStatus"):
		return func(c *cpu.CPU) { count(c); ret(c, 0x8CD5) } // GL_FRAMEBUFFER_COMPLETE
	case strings.HasPrefix(name, "glGetObjectParameteriv") || strings.HasPrefix(name, "glGetProgramiv") ||
		strings.HasPrefix(name, "glGetShaderiv") || strings.HasPrefix(name, "glGetQueryObject") || name == "glGetTexLevelParameteriv":
		return func(c *cpu.CPU) { // compile/link status, query results...: 1; info log lengths: 0
			count(c)
			pname, out := c.Arg(1), c.Arg(2)
			if name == "glGetTexLevelParameteriv" {
				pname, out = c.Arg(2), c.Arg(3)
			}
			v := uint32(1)
			if pname == 0x8B84 || pname == 0x8B88 { // INFO_LOG_LENGTH, SHADER_SOURCE_LENGTH
				v = 0
			}
			c.Write32(out, v)
			c.Ret()
		}
	case strings.HasPrefix(name, "glGetInfoLog") || strings.HasPrefix(name, "glGetShaderInfoLog") || strings.HasPrefix(name, "glGetProgramInfoLog"):
		return func(c *cpu.CPU) {
			count(c)
			if c.Arg(2) != 0 {
				c.Write32(c.Arg(2), 0)
			}
			if c.Arg(3) != 0 && c.Arg(1) > 0 {
				c.Write8(c.Arg(3), 0)
			}
			c.Ret()
		}
	case name == "glBindBufferARB" || name == "glBindBuffer":
		return func(c *cpu.CPU) { count(c); g.bound[c.Arg(0)] = c.Arg(1); c.Ret() }
	case name == "glBufferDataARB" || name == "glBufferData":
		return func(c *cpu.CPU) {
			count(c)
			b := g.bound[c.Arg(0)]
			if g.sizes[b] < c.Arg(1) {
				g.sizes[b], g.mapped[b] = c.Arg(1), 0
			}
			c.Ret()
		}
	case name == "glMapBufferARB" || name == "glMapBuffer" || name == "glMapBufferRange":
		return func(c *cpu.CPU) {
			count(c)
			b := g.bound[c.Arg(0)]
			if g.mapped[b] == 0 {
				g.mapped[b] = c.Malloc(max(g.sizes[b], 16))
			}
			p := g.mapped[b]
			if name == "glMapBufferRange" {
				p += c.Arg(1)
			}
			ret(c, p)
		}
	case strings.HasPrefix(name, "glUnmapBuffer") || strings.HasPrefix(name, "glIsEnabled") || strings.HasPrefix(name, "glTestFence") ||
		strings.HasPrefix(name, "glIsTexture"):
		return func(c *cpu.CPU) { count(c); ret(c, 1) }
	case name == "glFenceSync":
		return newName
	case name == "glClientWaitSync":
		return func(c *cpu.CPU) { count(c); ret(c, 0x911A) } // GL_ALREADY_SIGNALED
	case strings.HasPrefix(name, "gl"):
		return func(c *cpu.CPU) { count(c); ret(c, 0) }
	}
	return m.cglFunc(name)
}

// glGet answers state queries with a capable GPU's limits.
func (m *Machine) glGet(c *cpu.CPU, fn string, pname, out uint32) {
	var v int32 = 0
	switch pname {
	case 0x0D33: // MAX_TEXTURE_SIZE
		v = 16384
	case 0x8073: // MAX_3D_TEXTURE_SIZE
		v = 2048
	case 0x851C: // MAX_CUBE_MAP_TEXTURE_SIZE
		v = 16384
	case 0x84E2, 0x8872, 0x8B4D, 0x8B4C: // MAX_TEXTURE_UNITS, MAX_TEXTURE_IMAGE_UNITS, combined, vertex
		v = 16
	case 0x8869: // MAX_VERTEX_ATTRIBS
		v = 16
	case 0x8B4A, 0x8B49: // MAX_VERTEX/FRAGMENT_UNIFORM_COMPONENTS
		v = 4096
	case 0x84FF: // MAX_TEXTURE_MAX_ANISOTROPY
		v = 16
	case 0x8D57, 0x8CDF: // MAX_SAMPLES, MAX_COLOR_ATTACHMENTS
		v = 8
	case 0x8824: // MAX_DRAW_BUFFERS
		v = 8
	case 0x0D3A: // MAX_VIEWPORT_DIMS
		c.Write32(out, 16384)
		c.Write32(out+4, 16384)
		return
	case 0x0BA2: // VIEWPORT
		for i, x := range []uint32{0, 0, desktopW, desktopH} {
			c.Write32(out+uint32(4*i), x)
		}
		return
	case 0x0D50, 0x0D52, 0x0D53, 0x0D54, 0x0D55: // SUBPIXEL, RED, GREEN, BLUE, ALPHA BITS
		v = 8
	case 0x0D56: // DEPTH_BITS
		v = 24
	case 0x0D57: // STENCIL_BITS
		v = 8
	case 0x8B87, 0x8B86: // ACTIVE_UNIFORM_MAX_LENGTH ...
		v = 0
	case 0x8D6B: // MAX_ELEMENTS?
		v = 1 << 20
	default:
		m.C.ReportOnce("glget:"+fn+":"+strconv.Itoa(int(pname)), "[gl] %s(%#x): answering 0", fn, pname)
	}
	switch fn {
	case "glGetFloatv":
		c.WriteF32(out, float32(v))
	case "glGetDoublev":
		c.WriteF64(out, float64(v))
	case "glGetBooleanv":
		if v != 0 {
			c.Write8(out, 1)
		} else {
			c.Write8(out, 0)
		}
	default:
		c.Write32(out, uint32(v))
	}
}

// cglFunc: CGL, the Mac's OpenGL context API, with one renderer.
func (m *Machine) cglFunc(name string) cpu.ThunkFunc {
	switch name {
	case "CGLQueryRendererInfo": // (displayMask, &info, &count)
		return func(c *cpu.CPU) { c.Write32(c.Arg(1), c.Malloc(16)); c.Write32(c.Arg(2), 1); ret(c, 0) }
	case "CGLDescribeRenderer": // (info, index, property, &value)
		return func(c *cpu.CPU) {
			v := uint32(1) // accelerated, online, full screen, GPU vertex/fragment processing...
			switch c.Arg(2) {
			case 70: // kCGLRPRendererID: an AMD GPU
				v = 0x00021B00
			case 84: // kCGLRPDisplayMask
				v = 1
			case 120, 121: // kCGLRPVideoMemory, kCGLRPTextureMemory (bytes)
				v = 1 << 30
			case 131, 132: // ...Megabytes
				v = 4096
			case 133: // kCGLRPMajorGLVersion
				v = 2
			case 128: // kCGLRPRendererCount
				v = 1
			}
			c.Write32(c.Arg(3), v)
			ret(c, 0)
		}
	case "CGLDestroyRendererInfo", "CGLDestroyPixelFormat", "CGLDestroyContext", "CGLSetCurrentContext", "CGLSetParameter":
		return ret0
	case "CGLChoosePixelFormat": // (attrs, &pix, &npix)
		return func(c *cpu.CPU) { c.Write32(c.Arg(1), c.Malloc(16)); c.Write32(c.Arg(2), 1); ret(c, 0) }
	case "CGLCreateContext": // (pix, share, &ctx)
		return func(c *cpu.CPU) { c.Write32(c.Arg(2), c.Malloc(64)); ret(c, 0) }
	case "CGLGetCurrentContext":
		return func(c *cpu.CPU) {
			if m.gl.ctx == 0 {
				m.gl.ctx = c.Malloc(64)
			}
			ret(c, m.gl.ctx)
		}
	case "CGLGetParameter":
		return func(c *cpu.CPU) { c.Write32(c.Arg(2), 0); ret(c, 0) }
	}
	return nil
}

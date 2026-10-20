package source

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"catalina32/pkg/cpu"
)

// The OpenGL stream: the game's GL calls (ToGL's, the launcher's) are recorded into shared memory
// (C32_GL_SHM) and the viewer (viewer/C32GL.m) replays them on a real OpenGL context in the
// game's window. glspec.go lists the calls.
//
// Shared memory (little-endian):
//
//	header (glHeaderSize bytes):
//	  0  u32 magic "C32L"        4  u32 version (GLVersion)
//	  8  u32 ring offset         12 u32 ring size
//	  16 u32 reply offset        20 u32 reply size
//	  24 u32 viewer ready (1 once its GL context exists)
//	  28 u32 viewer errors (GL errors and failed shaders, for the log)
//	  32 u64 write index (emulator: bytes written to the ring so far)
//	  40 u64 read index (viewer: bytes consumed)
//	  48 u32 reply token, 52 u32 reply length (viewer, after answering a question)
//	  56 u64 frames presented (viewer)
//	  64 u32 screen width, 68 screen height (viewer: the main screen, in points)
//	  72 u32 emulator state (1 running, 2 ended)
//	  128 u32 event write index (viewer), 132 u32 event read index (emulator)
//	  glEventsOffset: glEvents events of glEventSize bytes (input; see sdl.go)
//	  glQueriesOffset: glQueries entries of 16 bytes, by query name modulo glQueries:
//	                   u32 available (emulator clears it at glBeginQuery, the viewer sets it), u32 0,
//	                   u64 result
//	ring: commands, each u32 id (GLSpec position), u32 payload bytes, payload, padded to 8
//	      bytes; id 0xFFFFFFFF skips to the start of the ring
//	reply: a question's answer
//
// Arguments are encoded as GLSpec says: 4 bytes per integer, float and object name, 8 per
// double; strings and data as u32 length (0xFFFFFFFF: NULL) and the bytes, padded to 4; 'p'
// arguments as u32 mode (0 NULL, 1 offset into the bound buffer, 2 data follows) and u32 value
// (the offset, or the data's length) and the data, padded.
//
// The emulator only waits for the viewer when the game asks something only the GL knows
// (uniform locations, texture contents, strings: glAsk), when the ring is full, and at
// presents (at most two frames in flight).
const (
	glMagic         = 0x4C323343 // "C32L"
	glHeaderSize    = 1 << 20
	glRingSize      = 128 << 20
	glReplySize     = 64 << 20
	glEventsOffset  = 4096
	glEvents        = 1024
	glEventSize     = 64
	glQueriesOffset = 128 << 10
	glQueries       = 16384
	glSkip          = 0xFFFFFFFF
	glFramesAhead   = 2
)

type glMapping struct {
	off, length uint32
	explicit    bool // GL_MAP_FLUSH_EXPLICIT: only flushed ranges are sent
}

// glRemote is the emulator's end of the stream.
type glRemote struct {
	m       *Machine
	shm     []byte
	ring    []byte
	reply   []byte
	w       uint64 // write index
	ids     map[string]int
	calls   []GLCall
	scratch []byte

	next     map[byte]uint32 // object kind -> next name
	bound    map[uint32]uint32
	bufSize  map[uint32]uint32
	shadow   map[uint32]uint32 // buffer -> guest copy the game maps
	mapped   map[uint32]glMapping
	unmapAll map[uint32]bool // buffers whose unmap sends the whole range (APPLE flushing on unmap)
	unpack   [8]int32        // GL_UNPACK_ row length, skip rows, skip pixels, alignment, image height, skip images
	token    uint32
	frames   uint64
	strings  map[uint32]uint32 // glGetString results, in guest memory
	logged   map[string]bool
	gone     bool
	arrays   map[uint32]glClientArray // vertex arrays in game memory, by attribute (glClientIndex)
}

// glClientArray is a vertex array the game keeps in its own memory (no buffer bound): it is
// sent with each draw that uses it (c32ClientArray), for the vertices the draw reads.
type glClientArray struct {
	size, typ, norm, stride, ptr uint32
}

// Attribute numbers c32ClientArray uses for the fixed-function arrays.
var glClientIndex = map[string]uint32{"glVertexPointer": 100, "glTexCoordPointer": 101, "glColorPointer": 102}

func (g *glRemote) word(off int) *uint32 { return (*uint32)(unsafe.Pointer(&g.shm[off])) }
func (g *glRemote) dword(off int) *uint64 {
	return (*uint64)(unsafe.Pointer(&g.shm[off]))
}

// newGLRemote creates the stream file named by C32_GL_SHM (nil if unset or unusable).
func newGLRemote(m *Machine) *glRemote {
	path := os.Getenv("C32_GL_SHM")
	if path == "" {
		return nil
	}
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		m.logf("[gl] cannot create %s: %v", path, err)
		return nil
	}
	defer f.Close()
	size := glHeaderSize + glRingSize + glReplySize
	if err := f.Truncate(int64(size)); err != nil {
		m.logf("[gl] cannot size %s: %v", path, err)
		return nil
	}
	shm, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		m.logf("[gl] cannot map %s: %v", path, err)
		return nil
	}
	le := binary.LittleEndian
	le.PutUint32(shm[4:], GLVersion)
	le.PutUint32(shm[8:], glHeaderSize)
	le.PutUint32(shm[12:], glRingSize)
	le.PutUint32(shm[16:], glHeaderSize+glRingSize)
	le.PutUint32(shm[20:], glReplySize)
	le.PutUint32(shm[72:], 1)
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&shm[0])), glMagic)
	g := &glRemote{m: m, shm: shm, ring: shm[glHeaderSize : glHeaderSize+glRingSize],
		reply: shm[glHeaderSize+glRingSize:], ids: map[string]int{}, calls: ParseGLSpec(),
		next: map[byte]uint32{}, bound: map[uint32]uint32{}, bufSize: map[uint32]uint32{},
		shadow: map[uint32]uint32{}, mapped: map[uint32]glMapping{}, unmapAll: map[uint32]bool{},
		strings: map[uint32]uint32{}, logged: map[string]bool{}, arrays: map[uint32]glClientArray{}}
	g.unpack[3] = 4
	for i, c := range g.calls {
		g.ids[c.Name] = i
	}
	m.logf("[gl] OpenGL stream at %s (the viewer draws once it is ready)", path)
	return g
}

// waitViewer waits up to d for the viewer's GL context.
func (g *glRemote) waitViewer(d time.Duration) bool {
	end := time.Now().Add(d)
	for atomic.LoadUint32(g.word(24)) != 1 {
		if time.Now().After(end) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// screen returns the viewer's screen size (0 if unknown).
func (g *glRemote) screen() (uint32, uint32) {
	return atomic.LoadUint32(g.word(64)), atomic.LoadUint32(g.word(68))
}

func (g *glRemote) logOnce(key, format string, args ...any) {
	if !g.logged[key] {
		g.logged[key] = true
		g.m.logf("[gl] "+format, args...)
	}
}

// --- Writing commands ---

func (g *glRemote) u32(v uint32) { g.scratch = binary.LittleEndian.AppendUint32(g.scratch, v) }
func (g *glRemote) pad4() {
	for len(g.scratch)%4 != 0 {
		g.scratch = append(g.scratch, 0)
	}
}

func (g *glRemote) bytes(b []byte) {
	g.u32(uint32(len(b)))
	g.scratch = append(g.scratch, b...)
	g.pad4()
}

func (g *glRemote) null() { g.u32(0xFFFFFFFF) }

// send writes the command in scratch with id into the ring and publishes it.
func (g *glRemote) send(id int) {
	if g.gone {
		return
	}
	payload := (len(g.scratch) + 7) &^ 7
	need := uint64(8 + payload)
	if need > glRingSize/2 {
		g.logOnce("big", "a %s call with %d bytes is too big for the stream: dropped", g.calls[id].Name, need)
		return
	}
	pos := g.w % glRingSize
	if pos+need > glRingSize { // skip to the start
		g.waitRoom(glRingSize - pos)
		binary.LittleEndian.PutUint32(g.ring[pos:], glSkip)
		g.w += glRingSize - pos
		pos = 0
	}
	g.waitRoom(need)
	le := binary.LittleEndian
	le.PutUint32(g.ring[pos:], uint32(id))
	le.PutUint32(g.ring[pos+4:], uint32(payload))
	copy(g.ring[pos+8:], g.scratch)
	g.w += need
	atomic.StoreUint64(g.dword(32), g.w)
}

// waitRoom waits until n more bytes fit in the ring.
func (g *glRemote) waitRoom(n uint64) {
	for !g.gone && g.w+n-atomic.LoadUint64(g.dword(40)) > glRingSize {
		g.pause()
	}
}

var glPauseStart time.Time

// pause waits a little for the viewer; a viewer that stops answering for 10 s is gone.
func (g *glRemote) pause() {
	if glPauseStart.IsZero() {
		glPauseStart = time.Now()
	}
	time.Sleep(50 * time.Microsecond)
	if time.Since(glPauseStart) > 10*time.Second {
		g.gone = true
		g.m.logf("[gl] the viewer stopped answering: drawing nothing from now on")
	}
}

func (g *glRemote) resumed() { glPauseStart = time.Time{} }

// ask sends a question (its first argument is the reply token) and waits for the answer.
func (g *glRemote) ask(id int, args func()) []byte {
	g.token++
	g.scratch = g.scratch[:0]
	g.u32(g.token)
	args()
	g.send(id)
	for !g.gone && atomic.LoadUint32(g.word(48)) != g.token {
		g.pause()
	}
	g.resumed()
	if g.gone {
		return nil
	}
	n := atomic.LoadUint32(g.word(52))
	return append([]byte(nil), g.reply[:min(n, glReplySize)]...)
}

// --- Encoding the game's calls ---

// stackArgs reads a call's arguments from the guest stack: 4 bytes each, 8 for doubles.
func (g *glRemote) stackArgs(c *cpu.CPU, call *GLCall) []uint32 {
	var out []uint32
	off := uint32(4)
	for _, a := range call.Args {
		v := c.Read32(c.Regs[cpu.RegESP] + off)
		out = append(out, v)
		off += 4
		if a.Kind == 'd' {
			out = append(out, c.Read32(c.Regs[cpu.RegESP]+off))
			off += 4
		}
	}
	return out
}

// encodeArgs writes call's arguments (as stackArgs read them) in wire form. pixels gives the
// mode and value of 'p' arguments.
func (g *glRemote) encodeArgs(c *cpu.CPU, call *GLCall, v []uint32, pixels func(i int) (mode, value uint32)) {
	k := 0
	vals := make([]uint32, len(call.Args)) // each argument's first word, for sizes and counts
	for i, a := range call.Args {
		vals[i] = v[k]
		x := v[k]
		k++
		switch a.Kind {
		case 'i', 'l', 'f', 'o':
			g.u32(x)
		case 'd':
			g.u32(x)
			g.u32(v[k])
			k++
		case 's':
			if x == 0 {
				g.null()
			} else {
				g.bytes(append([]byte(c.ReadCString(x)), 0))
			}
		case 'g', 'x':
			n := vals[i-1]
			for j := uint32(0); j < n; j++ {
				if a.Kind == 'g' {
					g.next[a.Obj]++
					c.Write32(x+4*j, g.next[a.Obj])
				}
				g.u32(c.Read32(x + 4*j))
			}
		case '*':
			n := uint32(a.Mul)
			if a.Of >= 0 {
				n *= vals[a.Of]
			}
			if x == 0 {
				g.null()
			} else {
				g.bytes(c.Bytes(x, n))
			}
		case 'p':
			mode, value := pixels(i)
			g.u32(mode)
			g.u32(value)
			if mode == 2 {
				g.scratch = append(g.scratch, c.Bytes(x, value)...)
				g.pad4()
			}
		}
	}
}

// forward sends a call as the game made it.
func (g *glRemote) forward(c *cpu.CPU, id int, pixels func(i int) (uint32, uint32)) {
	call := &g.calls[id]
	g.scratch = g.scratch[:0]
	g.encodeArgs(c, call, g.stackArgs(c, call), pixels)
	g.send(id)
}

func noPixels(int) (uint32, uint32) { return 0, 0 }

// GL constants the emulator looks at.
const (
	glArrayBuffer        = 0x8892
	glElementArrayBuffer = 0x8893
	glPixelUnpackBuffer  = 0x88EC
	glMapFlushExplicit   = 0x0010
	glBufferFlushUnmap   = 0x8A13
)

// fn returns the emulator side of GL function name (nil: not in the stream).
func (g *glRemote) fn(name string) cpu.ThunkFunc {
	if f := g.local(name); f != nil {
		return f
	}
	id, ok := g.ids[name]
	if !ok {
		return nil
	}
	call := &g.calls[id]
	if !call.Custom {
		return func(c *cpu.CPU) {
			g.forward(c, id, noPixels)
			ret(c, 0)
		}
	}
	return g.custom(name, id)
}

// custom encodes the calls flagged E.
func (g *glRemote) custom(name string, id int) cpu.ThunkFunc {
	switch name {
	case "glBindBufferARB", "glBindBuffer":
		return func(c *cpu.CPU) {
			g.bound[c.Arg(0)] = c.Arg(1)
			g.forward(c, id, noPixels)
			ret(c, 0)
		}
	case "glBufferDataARB", "glBufferData":
		return func(c *cpu.CPU) {
			b, size := g.bound[c.Arg(0)], c.Arg(1)
			if g.bufSize[b] != size {
				g.bufSize[b], g.shadow[b] = size, 0 // a new shadow on the next map
			}
			if g.shadow[b] != 0 && c.Arg(2) != 0 {
				copy(c.WBytes(g.shadow[b], size), c.Bytes(c.Arg(2), size))
			}
			g.forward(c, id, func(int) (uint32, uint32) {
				if c.Arg(2) == 0 {
					return 0, 0
				}
				return 2, size
			})
			ret(c, 0)
		}
	case "glBufferParameteriAPPLE":
		return func(c *cpu.CPU) { // (target, GL_BUFFER_FLUSHING_UNMAP_APPLE, flag): kept here only
			if c.Arg(1) == glBufferFlushUnmap {
				g.unmapAll[g.bound[c.Arg(0)]] = c.Arg(2) != 0
			}
			ret(c, 0)
		}
	case "glCompressedTexImage2D", "glCompressedTexImage2DARB", "glCompressedTexImage3D", "glCompressedTexSubImage2D":
		sizeArg := map[string]int{"glCompressedTexImage3D": 7, "glCompressedTexSubImage2D": 7}[name]
		if sizeArg == 0 {
			sizeArg = 6
		}
		return func(c *cpu.CPU) {
			g.forward(c, id, func(i int) (uint32, uint32) { return g.dataArg(c.Arg(i), c.Arg(sizeArg), glPixelUnpackBuffer) })
			ret(c, 0)
		}
	case "glTexImage2D", "glTexImage3D", "glTexSubImage2D", "glTexSubImage3D":
		// width, height, depth, format, type argument positions
		pos := map[string][5]int{"glTexImage2D": {3, 4, -1, 6, 7}, "glTexImage3D": {3, 4, 5, 7, 8},
			"glTexSubImage2D": {4, 5, -1, 6, 7}, "glTexSubImage3D": {5, 6, 7, 8, 9}}[name]
		return func(c *cpu.CPU) {
			depth := uint32(1)
			if pos[2] >= 0 {
				depth = c.Arg(pos[2])
			}
			size := g.pixelSpan(c.Arg(pos[0]), c.Arg(pos[1]), depth, c.Arg(pos[3]), c.Arg(pos[4]))
			g.forward(c, id, func(i int) (uint32, uint32) { return g.dataArg(c.Arg(i), size, glPixelUnpackBuffer) })
			ret(c, 0)
		}
	case "glDrawElements", "glDrawRangeElements":
		countArg, typeArg := 1, 2
		if name == "glDrawRangeElements" {
			countArg, typeArg = 3, 4
		}
		return func(c *cpu.CPU) {
			isz := map[uint32]uint32{0x1401: 1, 0x1403: 2, 0x1405: 4}[c.Arg(typeArg)]
			size := c.Arg(countArg) * isz
			if len(g.arrays) > 0 {
				lo, hi := uint32(0), uint32(0)
				switch {
				case name == "glDrawRangeElements":
					lo, hi = c.Arg(1), c.Arg(2)+1
				case g.bound[glElementArrayBuffer] == 0 && isz > 0: // indices in game memory: their range
					lo = ^uint32(0)
					for k := uint32(0); k < c.Arg(countArg); k++ {
						var v uint32
						switch isz {
						case 1:
							v = uint32(c.Read8(c.Arg(typeArg+1) + k))
						case 2:
							v = uint32(c.Read16(c.Arg(typeArg+1) + 2*k))
						default:
							v = c.Read32(c.Arg(typeArg+1) + 4*k)
						}
						lo, hi = min(lo, v), max(hi, v+1)
					}
				default:
					g.logOnce("arrayrange", "%s with client arrays and indices in a buffer: vertices not sent", name)
				}
				g.sendArrays(c, lo, hi)
			}
			g.forward(c, id, func(i int) (uint32, uint32) { return g.dataArg(c.Arg(i), size, glElementArrayBuffer) })
			ret(c, 0)
		}
	case "glVertexAttribPointer", "glVertexAttribPointerARB", "glVertexPointer", "glTexCoordPointer", "glColorPointer":
		fixed, isFixed := glClientIndex[name]
		return func(c *cpu.CPU) {
			index, a := fixed, glClientArray{size: c.Arg(0), typ: c.Arg(1), stride: c.Arg(2), ptr: c.Arg(3)}
			if !isFixed { // (index, size, type, normalized, stride, pointer)
				index, a = c.Arg(0), glClientArray{size: c.Arg(1), typ: c.Arg(2), norm: c.Arg(3), stride: c.Arg(4), ptr: c.Arg(5)}
			}
			if g.bound[glArrayBuffer] == 0 && a.ptr != 0 {
				g.arrays[index] = a // sent with the draws
				ret(c, 0)
				return
			}
			delete(g.arrays, index)
			g.forward(c, id, func(i int) (uint32, uint32) { return 1, c.Arg(i) })
			ret(c, 0)
		}
	case "glDrawArrays":
		return func(c *cpu.CPU) {
			g.sendArrays(c, c.Arg(1), c.Arg(1)+c.Arg(2))
			g.forward(c, id, noPixels)
			ret(c, 0)
		}
	case "glPixelStorei":
		return func(c *cpu.CPU) {
			if k := map[uint32]int{0x0CF2: 0, 0x0CF3: 1, 0x0CF4: 2, 0x0CF5: 3, 0x806E: 4, 0x806D: 5}; true {
				if i, ok := k[c.Arg(0)]; ok {
					g.unpack[i] = int32(c.Arg(1))
				}
			}
			g.forward(c, id, noPixels)
			ret(c, 0)
		}
	case "glGenBuffersARB", "glGenBuffers", "glGenFramebuffers", "glGenFramebuffersEXT", "glGenProgramsARB",
		"glGenQueriesARB", "glGenRenderbuffers", "glGenRenderbuffersEXT", "glGenTextures":
		return func(c *cpu.CPU) {
			g.forward(c, id, noPixels) // encodeArgs makes the names
			ret(c, 0)
		}
	case "glCreateProgramObjectARB":
		return func(c *cpu.CPU) {
			g.next['H']++
			h := g.next['H']
			g.scratch = g.scratch[:0]
			g.u32(h)
			g.send(id)
			ret(c, h)
		}
	case "glCreateShaderObjectARB":
		return func(c *cpu.CPU) {
			g.next['H']++
			h := g.next['H']
			g.scratch = g.scratch[:0]
			g.u32(c.Arg(0))
			g.u32(h)
			g.send(id)
			ret(c, h)
		}
	case "glShaderSourceARB": // (shader, count, strings, lengths): sent as one string
		return func(c *cpu.CPU) {
			var src strings.Builder
			for i := uint32(0); i < c.Arg(1); i++ {
				p := c.Read32(c.Arg(2) + 4*i)
				if c.Arg(3) != 0 && int32(c.Read32(c.Arg(3)+4*i)) >= 0 {
					src.Write(c.Bytes(p, c.Read32(c.Arg(3)+4*i)))
				} else {
					src.WriteString(c.ReadCString(p))
				}
			}
			g.scratch = g.scratch[:0]
			g.u32(c.Arg(0))
			g.bytes(append([]byte(src.String()), 0))
			g.send(id)
			ret(c, 0)
		}
	}
	panic("source: no encoder for " + name)
}

// sendArrays sends vertices [first, end) of the client arrays.
func (g *glRemote) sendArrays(c *cpu.CPU, first, end uint32) {
	if end <= first || end-first > 1<<20 {
		return
	}
	for index, a := range g.arrays {
		esize := a.size * map[uint32]uint32{0x1400: 1, 0x1401: 1, 0x1402: 2, 0x1403: 2, 0x1404: 4, 0x1405: 4, 0x1406: 4, 0x140A: 8}[a.typ]
		if a.size == 0x80E1 { // GL_BGRA colours
			esize = 4
		}
		stride := a.stride
		if stride == 0 {
			stride = esize
		}
		n := (end-first-1)*stride + esize
		g.scratch = g.scratch[:0]
		for _, v := range []uint32{index, a.size, a.typ, a.norm, a.stride, first, n} {
			g.u32(v)
		}
		g.bytes(c.Bytes(a.ptr+first*stride, n))
		g.send(g.ids["c32ClientArray"])
	}
}

// dataArg is a 'p' argument: an offset when a buffer is bound to target, else size bytes of data.
func (g *glRemote) dataArg(ptr, size, target uint32) (uint32, uint32) {
	switch {
	case g.bound[target] != 0:
		return 1, ptr
	case ptr == 0:
		return 0, 0
	}
	return 2, size
}

// pixelSpan is the number of bytes a pixel transfer reads, with the current unpack settings.
func (g *glRemote) pixelSpan(w, h, d, format, typ uint32) uint32 {
	comps := map[uint32]uint32{0x1903: 1, 0x1906: 1, 0x1909: 1, 0x190A: 2, 0x1907: 3, 0x80E0: 3, 0x1908: 4,
		0x80E1: 4, 0x1902: 1, 0x84F9: 1, 0x8227: 2, 0x1904: 1, 0x1905: 1}[format]
	if comps == 0 {
		comps = 4
	}
	var bpp uint32
	switch typ {
	case 0x1400, 0x1401: // byte
		bpp = comps
	case 0x1402, 0x1403, 0x140B: // short, half
		bpp = 2 * comps
	case 0x1404, 0x1405, 0x1406: // int, float
		bpp = 4 * comps
	case 0x8033, 0x8034, 0x8363, 0x8364, 0x8365, 0x8366, 0x8368: // packed 16-bit
		bpp = 2
	default: // packed 32-bit (8_8_8_8, 2_10_10_10, 24_8...)
		bpp = 4
	}
	rowLen := w
	if g.unpack[0] > 0 {
		rowLen = uint32(g.unpack[0])
	}
	align := uint32(max(g.unpack[3], 1))
	rowBytes := (rowLen*bpp + align - 1) / align * align
	imgRows := h
	if g.unpack[4] > 0 {
		imgRows = uint32(g.unpack[4])
	}
	skipPix, skipRows, skipImg := uint32(max(g.unpack[2], 0)), uint32(max(g.unpack[1], 0)), uint32(max(g.unpack[5], 0))
	if w == 0 || h == 0 || d == 0 {
		return 0
	}
	return (skipImg+d-1)*imgRows*rowBytes + (skipRows+h-1)*rowBytes + (skipPix+w)*bpp
}

// local answers the calls the emulator handles without the viewer (nil: not one of them).
func (g *glRemote) local(name string) cpu.ThunkFunc {
	switch name {
	case "glGetError":
		return ret0
	case "glCheckFramebufferStatus", "glCheckFramebufferStatusEXT":
		return retN(0x8CD5)
	case "glFinishFenceAPPLE", "glFinishFenceNV", "glSetFenceAPPLE", "glSetFenceNV", "glDeleteFencesAPPLE",
		"glDeleteFencesNV", "glTextureRangeAPPLE", "glStringMarkerGREMEDY", "glDeleteSync", "glWaitSync":
		return ret0 // fences: the stream keeps order, and mapped buffers are copies
	case "glTestFenceAPPLE", "glTestFenceNV", "glIsTexture", "glTestObjectAPPLE":
		return retN(1)
	case "glGenFencesAPPLE", "glGenFencesNV":
		return func(c *cpu.CPU) {
			for i := uint32(0); i < c.Arg(0); i++ {
				g.next['f']++
				c.Write32(c.Arg(1)+4*i, g.next['f'])
			}
			ret(c, 0)
		}
	case "glFenceSync":
		return func(c *cpu.CPU) { g.next['s']++; ret(c, g.next['s']) }
	case "glClientWaitSync":
		return retN(0x911A) // GL_ALREADY_SIGNALED
	case "glGetObjectParameterivARB", "glGetShaderiv", "glGetProgramiv":
		return func(c *cpu.CPU) { // compile and link status: OK (the viewer logs failures)
			v := uint32(1)
			switch c.Arg(1) {
			case 0x8B4E: // GL_OBJECT_TYPE_ARB
				v = 0x8B40 // program object
			case 0x8B86, 0x8B87, 0x8B89, 0x8B8A: // active uniforms/attributes and name lengths
				v = 0
			}
			c.Write32(c.Arg(2), v)
			ret(c, 0)
		}
	case "glGetInfoLogARB", "glGetShaderInfoLog", "glGetProgramInfoLog":
		return func(c *cpu.CPU) {
			if c.Arg(2) != 0 {
				c.Write32(c.Arg(2), 0)
			}
			if c.Arg(3) != 0 && c.Arg(1) > 0 {
				c.Write8(c.Arg(3), 0)
			}
			ret(c, 0)
		}
	case "glGetProgramivARB": // assembly programs: under native limits, no errors
		return func(c *cpu.CPU) { c.Write32(c.Arg(2), 1); ret(c, 0) }
	case "glGetTexParameterPointervAPPLE":
		return func(c *cpu.CPU) { c.Write32(c.Arg(2), 0); ret(c, 0) }
	case "glMapBufferARB", "glMapBuffer":
		return func(c *cpu.CPU) {
			b := g.bound[c.Arg(0)]
			v, set := g.unmapAll[b]
			ret(c, g.mapBuffer(b, 0, g.bufSize[b], set && !v))
		}
	case "glMapBufferRange":
		return func(c *cpu.CPU) {
			b := g.bound[c.Arg(0)]
			ret(c, g.mapBuffer(b, c.Arg(1), c.Arg(2), c.Arg(3)&glMapFlushExplicit != 0))
		}
	case "glFlushMappedBufferRange", "glFlushMappedBufferRangeAPPLE":
		apple := name == "glFlushMappedBufferRangeAPPLE"
		return func(c *cpu.CPU) {
			b := g.bound[c.Arg(0)]
			mp := g.mapped[b]
			off := mp.off + c.Arg(1)
			if apple {
				off = c.Arg(1)
			}
			g.subData(c, c.Arg(0), off, c.Arg(2), g.shadow[b]+off)
			ret(c, 0)
		}
	case "glUnmapBuffer", "glUnmapBufferARB":
		return func(c *cpu.CPU) {
			b := g.bound[c.Arg(0)]
			if mp, ok := g.mapped[b]; ok && !mp.explicit {
				g.subData(c, c.Arg(0), mp.off, mp.length, g.shadow[b]+mp.off)
			}
			delete(g.mapped, b)
			ret(c, 1)
		}
	case "glGetQueryObjectivARB", "glGetQueryObjectuivARB", "glGetQueryObjectiv", "glGetQueryObjectuiv":
		return g.queryObject
	case "glBeginQueryARB", "glBeginQuery":
		id := g.ids["glBeginQueryARB"]
		return func(c *cpu.CPU) {
			atomic.StoreUint32(g.word(glQueriesOffset+16*int(c.Arg(1)%glQueries)), 0)
			g.forward(c, id, noPixels)
			ret(c, 0)
		}
	case "glGetString":
		return func(c *cpu.CPU) {
			name := c.Arg(0)
			if a, ok := g.strings[name]; ok {
				ret(c, a)
				return
			}
			r := g.ask(g.ids["c32GetString"], func() { g.u32(name) })
			a := uint32(0)
			if r != nil {
				a = g.m.data(uint32(len(r)) + 1)
				copy(c.WBytes(a, uint32(len(r))), r)
			}
			g.strings[name] = a
			ret(c, a)
		}
	case "glGetIntegerv", "glGetFloatv", "glGetBooleanv", "glGetDoublev":
		kind := map[string]uint32{"glGetIntegerv": 0, "glGetFloatv": 1, "glGetBooleanv": 2, "glGetDoublev": 3}[name]
		return func(c *cpu.CPU) { // reply: u32 count, then count doubles
			pname, out := c.Arg(0), c.Arg(1)
			r := g.ask(g.ids["c32GetValues"], func() { g.u32(pname); g.u32(kind) })
			if len(r) < 4 {
				ret(c, 0)
				return
			}
			n := binary.LittleEndian.Uint32(r)
			for i := uint32(0); i < n && 4+8*i+8 <= uint32(len(r)); i++ {
				v := math.Float64frombits(binary.LittleEndian.Uint64(r[4+8*i:]))
				switch kind {
				case 0:
					c.Write32(out+4*i, uint32(int32(v)))
				case 1:
					c.WriteF32(out+4*i, float32(v))
				case 2:
					b := uint8(0)
					if v != 0 {
						b = 1
					}
					c.Write8(out+i, b)
				case 3:
					c.WriteF64(out+8*i, v)
				}
			}
			ret(c, 0)
		}
	case "glIsEnabled":
		return func(c *cpu.CPU) {
			r := g.ask(g.ids["c32IsEnabled"], func() { g.u32(c.Arg(0)) })
			if len(r) >= 4 {
				ret(c, binary.LittleEndian.Uint32(r))
				return
			}
			ret(c, 0)
		}
	case "glGetTexLevelParameteriv":
		return func(c *cpu.CPU) {
			r := g.ask(g.ids["c32GetTexLevelParameter"], func() { g.u32(c.Arg(0)); g.u32(c.Arg(1)); g.u32(c.Arg(2)) })
			v := uint32(0)
			if len(r) >= 4 {
				v = binary.LittleEndian.Uint32(r)
			}
			c.Write32(c.Arg(3), v)
			ret(c, 0)
		}
	case "glGetTexImage", "glGetCompressedTexImage", "glGetCompressedTexImageARB":
		compressed := name != "glGetTexImage"
		return func(c *cpu.CPU) {
			target, level := c.Arg(0), c.Arg(1)
			format, typ, out := c.Arg(2), c.Arg(3), c.Arg(4)
			if compressed {
				format, typ, out = 0, 0, c.Arg(2)
			}
			r := g.ask(g.ids["c32GetTexImage"], func() {
				g.u32(target)
				g.u32(level)
				g.u32(format)
				g.u32(typ)
				g.u32(b2u(compressed))
			})
			if out != 0 && len(r) > 0 {
				copy(c.WBytes(out, uint32(len(r))), r)
			}
			ret(c, 0)
		}
	case "glReadPixels":
		return func(c *cpu.CPU) {
			r := g.ask(g.ids["c32ReadPixels"], func() {
				for i := 0; i < 6; i++ {
					g.u32(c.Arg(i))
				}
			})
			if c.Arg(6) != 0 && len(r) > 0 {
				copy(c.WBytes(c.Arg(6), uint32(len(r))), r)
			}
			ret(c, 0)
		}
	case "glGetUniformLocationARB", "glGetUniformLocation", "glGetAttribLocationARB", "glGetAttribLocation":
		q := "c32UniformLocation"
		if strings.Contains(name, "Attrib") {
			q = "c32AttribLocation"
		}
		return func(c *cpu.CPU) {
			r := g.ask(g.ids[q], func() { g.u32(c.Arg(0)); g.bytes(append([]byte(c.ReadCString(c.Arg(1))), 0)) })
			if len(r) >= 4 {
				ret(c, binary.LittleEndian.Uint32(r))
				return
			}
			ret(c, 0xffffffff)
		}
	case "glGetBooleanIndexedvEXT":
		return func(c *cpu.CPU) { c.Write8(c.Arg(2), 1); ret(c, 0) } // colour masks: enabled
	}
	return nil
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// mapBuffer returns the guest copy of buffer b for the game to write [off, off+length) into.
func (g *glRemote) mapBuffer(b, off, length uint32, explicit bool) uint32 {
	if g.shadow[b] == 0 {
		g.shadow[b] = g.m.C.Malloc(max(g.bufSize[b], 16))
	}
	if off+length > g.bufSize[b] {
		length = g.bufSize[b] - min(off, g.bufSize[b])
	}
	g.mapped[b] = glMapping{off, length, explicit}
	return g.shadow[b] + off
}

// subData sends [off, off+n) of a mapped buffer, from guest memory at src.
func (g *glRemote) subData(c *cpu.CPU, target, off, n, src uint32) {
	if n == 0 {
		return
	}
	g.scratch = g.scratch[:0]
	g.u32(target)
	g.u32(off)
	g.u32(n)
	g.bytes(c.Bytes(src, n))
	g.send(g.ids["glBufferSubDataARB"])
}

// queryObject answers from the results the viewer posts; a result not there yet is waited for.
func (g *glRemote) queryObject(c *cpu.CPU) {
	q, pname, out := c.Arg(0), c.Arg(1), c.Arg(2)
	e := glQueriesOffset + 16*int(q%glQueries)
	avail := atomic.LoadUint32(g.word(e))
	if pname == 0x8867 { // GL_QUERY_RESULT_AVAILABLE
		c.Write32(out, avail)
		ret(c, 0)
		return
	}
	for avail == 0 && !g.gone {
		g.pause()
		avail = atomic.LoadUint32(g.word(e))
	}
	g.resumed()
	c.Write32(out, uint32(atomic.LoadUint64(g.dword(e+8))))
	ret(c, 0)
}

// present ends a frame, keeping at most glFramesAhead frames ahead of the viewer.
func (g *glRemote) present() {
	g.frames++
	g.scratch = g.scratch[:0]
	g.u32(uint32(g.frames))
	g.send(g.ids["c32Present"])
	for !g.gone && g.frames > atomic.LoadUint64(g.dword(56))+glFramesAhead {
		g.pause()
	}
	g.resumed()
}

// windowSize tells the viewer the size of the game's window (its back buffer).
func (g *glRemote) windowSize(w, h uint32) {
	g.scratch = g.scratch[:0]
	g.u32(w)
	g.u32(h)
	g.send(g.ids["c32WindowSize"])
}

// ended tells the viewer the game has stopped.
func (g *glRemote) ended() {
	atomic.StoreUint32(g.word(72), 2)
}

var _ = fmt.Sprint

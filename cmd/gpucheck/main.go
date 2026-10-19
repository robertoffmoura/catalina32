// Command gpucheck stands in for the Metal viewer when developing the GPU path without a Mac: it
// maps the command stream (C32_GPU_SHM, see pkg/d3d9/gpu.go), consumes frames like the viewer,
// checks that every command is well formed and refers only to libraries and textures sent
// before, prints statistics, and writes each generated MSL source to -out for compilation checks.
//
//	gpucheck -shm /path/gpu.shm -out dir [-frames N]
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	headerSize = 4096
	magic      = 0x47323343
)

func main() {
	shmPath := flag.String("shm", "", "command stream file (C32_GPU_SHM)")
	out := flag.String("out", "", "directory for the MSL sources")
	frames := flag.Int("frames", 0, "stop after this many frames (0: until interrupted)")
	flag.Parse()
	var m []byte
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		f, err := os.OpenFile(*shmPath, os.O_RDWR, 0)
		if err == nil {
			if st, _ := f.Stat(); st != nil && st.Size() > headerSize {
				m, err = syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				f.Close()
				if err == nil && binary.LittleEndian.Uint32(m) == magic {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "gpucheck: no command stream at", *shmPath)
			os.Exit(1)
		}
	}
	le := binary.LittleEndian
	slotSize, slots := int(le.Uint32(m[8:])), int(le.Uint32(m[12:]))
	word := func(off int) *uint32 { return (*uint32)(unsafe.Pointer(&m[off])) }
	atomic.StoreUint32(word(16), 1)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	c := &checker{libs: map[uint32]bool{}, formats: map[uint32]int{}, cubes: map[uint32]bool{}, textures: map[uint32][2]uint32{}, out: *out}
	var lastFrame uint64
	for n := 0; *frames == 0 || n < *frames; {
		select {
		case <-stop:
			c.report()
			return
		default:
		}
		// The oldest written slot.
		k := -1
		for i := 0; i < slots; i++ {
			if atomic.LoadUint32(word(32+16*i+12)) == 1 && (k < 0 || le.Uint64(m[32+16*i:]) < le.Uint64(m[32+16*k:])) {
				k = i
			}
		}
		if k < 0 {
			time.Sleep(200 * time.Microsecond)
			continue
		}
		atomic.StoreUint32(word(32+16*k+12), 2)
		frame := le.Uint64(m[32+16*k:])
		length := int(le.Uint32(m[32+16*k+8:]))
		if frame <= lastFrame && lastFrame != 0 {
			c.fail("frame %d after %d", frame, lastFrame)
		}
		lastFrame = frame
		data := m[headerSize+k*slotSize : headerSize+k*slotSize+length]
		c.frame(frame, data)
		atomic.StoreUint32(word(32+16*k+12), 0)
		n++
	}
	c.report()
}

type checker struct {
	libs                           map[uint32]bool
	formats                        map[uint32]int       // texture uploads by format
	cubes                          map[uint32]bool      // texture id -> uploaded as a cube
	cubeDraws, cubeBound           int                  // cube samplers in draws; those bound to an uploaded cube
	scissorDraws                   int                  // draws with the scissor test on
	textures                       map[uint32][2]uint32 // id -> size (0 for targets)
	out                            string
	frames, draws, clears, targets int
	bytes, texBytes, maxFrame      int
	errors                         int
	lastLog                        time.Time
}

func (c *checker) fail(format string, args ...any) {
	c.errors++
	if c.errors <= 20 {
		fmt.Printf("gpucheck: "+format+"\n", args...)
	}
}

func (c *checker) frame(frame uint64, d []byte) {
	le := binary.LittleEndian
	c.frames++
	c.bytes += len(d)
	c.maxFrame = max(c.maxFrame, len(d))
	ended := false
	for p := 0; p < len(d); {
		if p+8 > len(d) {
			c.fail("frame %d: truncated command header", frame)
			return
		}
		op, n := le.Uint32(d[p:]), int(le.Uint32(d[p+4:]))
		if n%4 != 0 || p+8+n > len(d) {
			c.fail("frame %d: command %d length %d", frame, op, n)
			return
		}
		b := d[p+8 : p+8+n]
		u := func(i int) uint32 { return le.Uint32(b[4*i:]) }
		switch op {
		case 1: // library
			id, stage, sl := u(0), u(1), int(u(2))
			if 12+sl > n {
				c.fail("library %d: source length %d", id, sl)
				break
			}
			c.libs[id] = true
			if c.out != "" {
				name := filepath.Join(c.out, fmt.Sprintf("lib%04d_%s.metal", id, map[uint32]string{0: "vs", 1: "ps"}[stage]))
				_ = os.WriteFile(name, b[12:12+sl], 0644)
			}
		case 2: // texture
			id, w, h, f := u(0), u(1), u(2), u(3)
			size := map[uint32]int{1: int(w * h * 4), 2: int(w * h / 2), 3: int(w * h), 4: int(w * h)}[f&0xFF]
			if f&0x100 != 0 { // cube: six faces
				size *= 6
			}
			if size == 0 || f&^0x1FF != 0 || 16+size > n || (f&0xFF != 1 && (w%4 != 0 || h%4 != 0)) || (f&0x100 != 0 && w != h) {
				c.fail("texture %d: %dx%d format %#x in %d bytes", id, w, h, f, n)
			}
			c.textures[id] = [2]uint32{w, h}
			c.cubes[id] = f&0x100 != 0
			c.texBytes += size
			c.formats[f]++
		case 3: // texture free
			delete(c.textures, u(0))
		case 4: // target
			c.targets++
			if id := u(0); id != 0 {
				c.textures[id] = [2]uint32{}
			}
		case 5: // clear
			c.clears++
		case 6: // copy (the destination becomes a render target)
			if n != 16 || u(2) == 0 || u(3) == 0 {
				c.fail("frame %d: copy command of %d bytes, size %dx%d", frame, n, u(2), u(3))
			}
			if dst := u(1); dst != 0 {
				c.textures[dst] = [2]uint32{}
			}
		case 7: // draw
			c.draws++
			c.draw(frame, b)
		case 8:
			ended = true
		default:
			c.fail("frame %d: unknown command %d", frame, op)
			return
		}
		p += 8 + n
	}
	if !ended {
		c.fail("frame %d: no frame end", frame)
	}
	if time.Since(c.lastLog) > 5*time.Second {
		c.lastLog = time.Now()
		fmt.Printf("gpucheck: frame %d: %d bytes, %d libraries, %d textures so far\n", frame, len(d), len(c.libs), len(c.textures))
	}
}

func (c *checker) draw(frame uint64, b []byte) {
	le := binary.LittleEndian
	u := func(i int) uint32 { return le.Uint32(b[4*i:]) }
	vs, ps, prim, nIdx, nAttr, nStream, nTex, nVC, nPC := u(0), u(1), u(2), int(u(3)), int(u(4)), int(u(5)), int(u(6)), int(u(7)), int(u(8))
	if !c.libs[vs] || !c.libs[ps] {
		c.fail("frame %d: draw uses library %d/%d before it was sent", frame, vs, ps)
	}
	if prim < 1 || prim > 5 {
		c.fail("frame %d: primitive type %d", frame, prim)
	}
	if u(13) != 0 { // scissor test on
		c.scissorDraws++
	}
	p := 4 * (9 + 4 + 5 + 1 + 15) // header, viewport, scissor, fixed-texture flag, state
	streams := map[uint32]int{}   // stream -> vertex count
	type attr struct{ stream, offset, typ uint32 }
	var attrs []attr
	for i := 0; i < nAttr; i++ {
		attrs = append(attrs, attr{le.Uint32(b[p+4:]), le.Uint32(b[p+8:]), le.Uint32(b[p+12:])})
		p += 16
	}
	for i := 0; i < nStream; i++ {
		st, stride, n := le.Uint32(b[p:]), le.Uint32(b[p+4:]), int(le.Uint32(b[p+8:]))
		if stride > 0 {
			streams[st] = n / int(stride)
		} else {
			streams[st] = 1 << 30
		}
		p += 12 + (n+3)&^3
	}
	for _, a := range attrs {
		if a.stream == 0xFFFFFFFF {
			continue
		}
		if _, ok := streams[a.stream]; !ok {
			c.fail("frame %d: attribute reads stream %d, which the draw does not carry", frame, a.stream)
		}
	}
	for i := 0; i < nTex; i++ {
		id, kind := le.Uint32(b[p+4:]), le.Uint32(b[p+8:])
		if kind > 2 {
			c.fail("frame %d: texture kind %d", frame, kind)
		}
		if kind == 1 { // msl.TexCube
			c.cubeDraws++
			if c.cubes[id] {
				c.cubeBound++
			}
		}
		if _, ok := c.textures[id]; !ok && id != 0 {
			// A texture whose pixels did not fit yet is referenced before its upload.
			c.textures[id] = [2]uint32{}
		}
		p += 36
	}
	minVerts := math.MaxInt
	for _, n := range streams {
		minVerts = min(minVerts, n)
	}
	for i := 0; i < nIdx; i++ {
		if ix := int(le.Uint32(b[p+4*i:])); ix >= minVerts {
			c.fail("frame %d: index %d beyond the %d vertices sent", frame, ix, minVerts)
			break
		}
	}
	p += 4 * nIdx
	if want := p + 16*(nVC+nPC); want != len(b) {
		c.fail("frame %d: draw is %d bytes, fields need %d (idx %d attr %d streams %d tex %d vc %d pc %d)", frame, len(b), want, nIdx, nAttr, nStream, nTex, nVC, nPC)
	}
}

func (c *checker) report() {
	if c.frames == 0 {
		fmt.Println("gpucheck: no frames")
		return
	}
	fmt.Printf("gpucheck: %d frames, %d draws (%.0f per frame), %d clears, %d target switches; %d libraries; average %.2f MB per frame (max %.2f MB), %.1f MB of texture uploads; %d errors\n",
		c.frames, c.draws, float64(c.draws)/float64(c.frames), c.clears, c.targets, len(c.libs),
		float64(c.bytes)/float64(c.frames)/(1<<20), float64(c.maxFrame)/(1<<20), float64(c.texBytes)/(1<<20), c.errors)
	fmt.Printf("gpucheck: texture uploads by format (1 RGBA, 2 BC1, 3 BC2, 4 BC3; +0x100 cube): %v\n", c.formats)
	fmt.Printf("gpucheck: %d cube samplers in draws, %d bound to an uploaded cube map\n", c.cubeDraws, c.cubeBound)
	fmt.Printf("gpucheck: %d draws with the scissor test on\n", c.scissorDraws)
}

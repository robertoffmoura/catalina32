package source

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// GLCheck stands in for the viewer on machines without one (Linux, tests): it consumes the
// OpenGL stream, checks every command against GLSpec, answers questions like a capable GL and
// posts query results. cmd/glcheck runs it on its own.
type GLCheck struct {
	Commands, Bytes, Frames, Questions uint64
	Errors                             []string
	PerCall                            map[string]uint64
}

func (k *GLCheck) fail(format string, args ...any) {
	if len(k.Errors) < 20 {
		k.Errors = append(k.Errors, fmt.Sprintf(format, args...))
	}
}

// RunGLCheck maps the stream at path (waiting for it to appear) and serves it until stop closes.
func RunGLCheck(path string, stop <-chan struct{}) (*GLCheck, error) {
	k := &GLCheck{PerCall: map[string]uint64{}}
	var shm []byte
	for shm == nil {
		select {
		case <-stop:
			return k, nil
		default:
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err == nil {
			if st, _ := f.Stat(); st != nil && st.Size() >= glHeaderSize+glRingSize+glReplySize {
				shm, err = syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					f.Close()
					return k, err
				}
			}
			f.Close()
		}
		if shm == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	for atomic.LoadUint32((*uint32)(unsafe.Pointer(&shm[0]))) != glMagic {
		time.Sleep(time.Millisecond)
	}
	le := binary.LittleEndian
	if v := le.Uint32(shm[4:]); v != GLVersion {
		return k, fmt.Errorf("stream version %d, checker %d", v, GLVersion)
	}
	word := func(off int) *uint32 { return (*uint32)(unsafe.Pointer(&shm[off])) }
	dword := func(off int) *uint64 { return (*uint64)(unsafe.Pointer(&shm[off])) }
	ring := shm[glHeaderSize : glHeaderSize+glRingSize]
	reply := shm[glHeaderSize+glRingSize:]
	calls := ParseGLSpec()
	ids := map[string]int{}
	for i, c := range calls {
		ids[c.Name] = i
	}
	atomic.StoreUint32(word(64), 1440)
	atomic.StoreUint32(word(68), 900)
	atomic.StoreUint32(word(24), 1)
	var r uint64
	activeQuery := map[uint32]uint32{} // target -> query
	answer := func(token uint32, data []byte) {
		copy(reply, data)
		atomic.StoreUint32(word(52), uint32(len(data)))
		atomic.StoreUint32(word(48), token)
		k.Questions++
	}
	locations := uint32(0)
	var trace *bufio.Writer // C32_GLTRACE: each command: name, hash of its bytes, arguments (for diffing two runs)
	if p := os.Getenv("C32_GLTRACE"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			return k, err
		}
		trace = bufio.NewWriter(f)
		defer f.Close()
		defer trace.Flush()
	}
	for {
		w := atomic.LoadUint64(dword(32))
		if r == w {
			select {
			case <-stop:
				return k, nil
			default:
			}
			time.Sleep(20 * time.Microsecond)
			continue
		}
		pos := r % glRingSize
		id := le.Uint32(ring[pos:])
		if id == glSkip {
			r += glRingSize - pos
			atomic.StoreUint64(dword(40), r)
			continue
		}
		n := le.Uint32(ring[pos+4:])
		if pos+8+uint64(n) > glRingSize || n%8 != 0 {
			return k, fmt.Errorf("bad command at %d: id %d, %d bytes", r, id, n)
		}
		p := ring[pos+8 : pos+8+uint64(n)]
		k.Commands++
		k.Bytes += 8 + uint64(n)
		if int(id) >= len(calls) {
			k.fail("unknown call %d", id)
		} else {
			c := &calls[id]
			k.PerCall[c.Name]++
			args, used, err := walkGLArgs(c, p)
			if trace != nil {
				fmt.Fprintf(trace, "%s %08x %x\n", c.Name, crc32.ChecksumIEEE(p), args)
			}
			if err != nil {
				k.fail("%s: %v", c.Name, err)
			} else if int(n)-used >= 8 || used > int(n) {
				k.fail("%s: %d bytes used of %d", c.Name, used, n)
			}
			switch c.Name {
			case "c32Present":
				atomic.AddUint64(dword(56), 1)
				k.Frames++
			case "glBeginQueryARB":
				activeQuery[args[0]] = args[1]
			case "glEndQueryARB":
				q := activeQuery[args[0]]
				e := glQueriesOffset + 16*int(q%glQueries)
				atomic.StoreUint64(dword(e+8), 1000)
				atomic.StoreUint32(word(e), 1)
			case "c32GetString":
				s := map[uint32]string{0x1F00: "ATI Technologies Inc.", 0x1F01: "AMD Radeon Pro 560 OpenGL Engine",
					0x1F02: "2.1 ATI-3.10.19", 0x1F03: glExtensions, 0x8B8C: "1.20"}[args[1]]
				answer(args[0], []byte(s))
			case "c32GetValues":
				out := binary.LittleEndian.AppendUint32(nil, 4)
				for i := 0; i < 4; i++ {
					out = binary.LittleEndian.AppendUint64(out, math.Float64bits(16))
				}
				answer(args[0], out)
			case "c32IsEnabled":
				answer(args[0], make([]byte, 4))
			case "c32GetTexLevelParameter":
				answer(args[0], binary.LittleEndian.AppendUint32(nil, 4))
			case "c32GetTexImage":
				answer(args[0], nil)
			case "c32ReadPixels":
				answer(args[0], make([]byte, args[3]*args[4]*4))
			case "c32UniformLocation", "c32AttribLocation":
				locations++
				answer(args[0], binary.LittleEndian.AppendUint32(nil, locations))
			}
		}
		r += 8 + uint64(n)
		atomic.StoreUint64(dword(40), r)
	}
}

// walkGLArgs decodes a command's arguments as the viewer does: the first word of each argument,
// and the bytes used.
func walkGLArgs(c *GLCall, p []byte) ([]uint32, int, error) {
	le := binary.LittleEndian
	off := 0
	var args []uint32
	need := func(n int) error {
		if off+n > len(p) {
			return fmt.Errorf("argument %d runs past the command", len(args))
		}
		return nil
	}
	for _, a := range c.Args {
		switch a.Kind {
		case 'i', 'l', 'f', 'o':
			if err := need(4); err != nil {
				return nil, 0, err
			}
			args = append(args, le.Uint32(p[off:]))
			off += 4
		case 'd':
			if err := need(8); err != nil {
				return nil, 0, err
			}
			args = append(args, le.Uint32(p[off:]))
			off += 8
		case 'g', 'x':
			cnt := int(args[len(args)-1])
			if err := need(4 * cnt); err != nil {
				return nil, 0, err
			}
			args = append(args, uint32(cnt))
			off += 4 * cnt
		case 's', '*':
			if err := need(4); err != nil {
				return nil, 0, err
			}
			n := le.Uint32(p[off:])
			off += 4
			args = append(args, n)
			if n != 0xFFFFFFFF {
				if err := need(int(n)); err != nil {
					return nil, 0, err
				}
				if a.Kind == 's' && (n == 0 || p[off+int(n)-1] != 0) {
					return nil, 0, fmt.Errorf("string without its NUL")
				}
				off += (int(n) + 3) &^ 3
			}
		case 'p':
			if err := need(8); err != nil {
				return nil, 0, err
			}
			mode, v := le.Uint32(p[off:]), le.Uint32(p[off+4:])
			off += 8
			args = append(args, mode)
			switch mode {
			case 0, 1:
			case 2:
				if err := need(int(v)); err != nil {
					return nil, 0, err
				}
				off += (int(v) + 3) &^ 3
			default:
				return nil, 0, fmt.Errorf("data mode %d", mode)
			}
		}
	}
	return args, off, nil
}

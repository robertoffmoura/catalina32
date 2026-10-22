package source

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3d9"
)

// OpenAL, the engine's sound output: sources play queued buffers, mixed into the window's PCM
// ring (the framebuffer shared memory: 44.1 kHz, stereo, 16-bit), which the window plays.
// Live, a ticker mixes every 10 ms; headless, mixing follows the instruction clock (so runs stay
// reproducible) and goes nowhere.

const (
	alRate      = 44100
	alLead      = 2048 // frames written ahead of the time they play
	alRingMask  = d3d9.ShmAudioFrames - 1
	alInitial   = 0x1011
	alPlaying   = 0x1012
	alPaused    = 0x1013
	alStopped   = 0x1014
	alGain      = 0x100A
	alLooping   = 0x1007
	alBuffer    = 0x1009
	alState     = 0x1010
	alQueued    = 0x1015
	alProcessed = 0x1016
)

type alBuf struct {
	pcm  []int16 // stereo frames
	rate float64
}

type alEntry struct {
	buf  uint32
	done bool
}

type alSrc struct {
	queue   []alEntry
	pos     float64 // in the first unfinished buffer, in its frames
	state   uint32
	gain    float64
	looping bool
	static  bool // one buffer set with AL_BUFFER (not queued)
}

type openAL struct {
	mu       sync.Mutex
	next     uint32
	bufs     map[uint32]*alBuf
	srcs     map[uint32]*alSrc
	listener float64
	ring     []byte // the window's PCM ring (nil: nowhere to play)
	written  uint64 // frames mixed
	start    uint64 // the clock (µs) when mixing started
	started  bool
	// Diagnostics: buffers received, and their samples that aren't silence; when (µs after the
	// start) a playing source ran out of queued sound, which the player hears as a gap.
	buffers, loudIn int
	dry             []uint64
}

func (m *Machine) registerOpenAL() {
	a := &m.al
	a.next, a.bufs, a.srcs, a.listener = 0x6000, map[uint32]*alBuf{}, map[uint32]*alSrc{}, 1
	id := func() uint32 { a.next++; return a.next }
	locked := func(fn cpu.ThunkFunc) cpu.ThunkFunc {
		return func(c *cpu.CPU) {
			a.mu.Lock()
			if !m.Live {
				a.mix(m.micros())
			}
			fn(c)
			a.mu.Unlock()
		}
	}
	src := func(c *cpu.CPU) *alSrc { return a.srcs[c.Arg(0)] }
	// alBufferData and Apple's alBufferDataStatic (buffer, format, data, size, freq): the
	// samples are copied either way.
	bufferData := locked(func(c *cpu.CPU) {
		b := a.bufs[c.Arg(0)]
		if b == nil {
			b = &alBuf{}
			a.bufs[c.Arg(0)] = b
		}
		b.pcm, b.rate = toStereo16(c.Bytes(c.Arg(2), c.Arg(3)), c.Arg(1)), float64(max(c.Arg(4), 1))
		a.buffers++
		for _, v := range b.pcm {
			if v > 64 || v < -64 {
				a.loudIn++
			}
		}
		c.Ret()
	})
	m.alBufferDataStatic = m.hostFunc("alBufferDataStatic", bufferData)
	for k, v := range map[string]cpu.ThunkFunc{
		// The engine's other sound device (AudioQueue) reports failure, so it uses OpenAL.
		"_AudioQueueNewOutput":   retN(0xffffffce), // paramErr
		"_alcOpenDevice":         func(c *cpu.CPU) { m.startOpenAL(); ret(c, 0x5000) },
		"_alcCloseDevice":        retN(1),
		"_alcCreateContext":      retN(0x5001),
		"_alcMakeContextCurrent": retN(1),
		"_alcDestroyContext":     retVoid,
		"_alcGetCurrentContext":  retN(0x5001),
		"_alcGetContextsDevice":  retN(0x5000),
		"_alcGetError":           ret0,
		"_alcGetString":          func(c *cpu.CPU) { ret(c, m.glStr("catalina32")) },
		"_alcGetIntegerv": func(c *cpu.CPU) { // (device, param, size, out): ALC_FREQUENCY and the like
			if c.Arg(3) != 0 && c.Arg(2) > 0 {
				c.Write32(c.Arg(3), alRate)
			}
			c.Ret()
		},
		"_alcIsExtensionPresent": ret0,
		"_alIsExtensionPresent":  ret0,
		"_alGetProcAddress": func(c *cpu.CPU) { // extensions: Apple's alBufferDataStatic (the engine's buffers)
			name := c.ReadCString(c.Arg(0))
			if name == "alBufferDataStatic" {
				ret(c, m.alBufferDataStatic)
				return
			}
			m.C.ReportOnce("al:"+name, "[openal] extension %s: not available", name)
			ret(c, 0)
		},
		"_alGetError":  ret0,
		"_alGetString": func(c *cpu.CPU) { ret(c, m.glStr("1.1")) },
		"_alGenSources": locked(func(c *cpu.CPU) {
			for i := uint32(0); i < c.Arg(0); i++ {
				h := id()
				a.srcs[h] = &alSrc{state: alInitial, gain: 1}
				c.Write32(c.Arg(1)+4*i, h)
			}
			c.Ret()
		}),
		"_alDeleteSources": locked(func(c *cpu.CPU) {
			for i := uint32(0); i < c.Arg(0); i++ {
				delete(a.srcs, c.Read32(c.Arg(1)+4*i))
			}
			c.Ret()
		}),
		"_alGenBuffers": locked(func(c *cpu.CPU) {
			for i := uint32(0); i < c.Arg(0); i++ {
				h := id()
				a.bufs[h] = &alBuf{rate: alRate}
				c.Write32(c.Arg(1)+4*i, h)
			}
			c.Ret()
		}),
		"_alDeleteBuffers": locked(func(c *cpu.CPU) {
			for i := uint32(0); i < c.Arg(0); i++ {
				delete(a.bufs, c.Read32(c.Arg(1)+4*i))
			}
			c.Ret()
		}),
		"_alBufferData": bufferData,
		"_alSourceQueueBuffers": locked(func(c *cpu.CPU) { // (source, n, buffers)
			if s := src(c); s != nil {
				if s.static {
					s.queue, s.static = nil, false
				}
				for i := uint32(0); i < c.Arg(1); i++ {
					s.queue = append(s.queue, alEntry{buf: c.Read32(c.Arg(2) + 4*i)})
				}
			}
			c.Ret()
		}),
		"_alSourceUnqueueBuffers": locked(func(c *cpu.CPU) { // (source, n, &buffers): processed ones only
			s := src(c)
			for i := uint32(0); s != nil && i < c.Arg(1) && len(s.queue) > 0 && s.queue[0].done; i++ {
				c.Write32(c.Arg(2)+4*i, s.queue[0].buf)
				s.queue = s.queue[1:]
			}
			c.Ret()
		}),
		"_alGetSourcei": locked(func(c *cpu.CPU) { // (source, param, &value)
			v := uint32(0)
			if s := src(c); s != nil {
				switch c.Arg(1) {
				case alProcessed:
					for _, e := range s.queue {
						if e.done {
							v++
						}
					}
				case alQueued:
					v = uint32(len(s.queue))
				case alState:
					v = s.state
				case alLooping:
					v = b2u(s.looping)
				}
			}
			c.Write32(c.Arg(2), v)
			c.Ret()
		}),
		"_alSourcePlay": locked(func(c *cpu.CPU) {
			if s := src(c); s != nil {
				if s.state == alStopped || s.state == alInitial {
					for i := range s.queue {
						if s.static {
							s.queue[i].done = false
						}
					}
					s.pos = 0
				}
				s.state = alPlaying
			}
			c.Ret()
		}),
		"_alSourceStop": locked(func(c *cpu.CPU) {
			if s := src(c); s != nil {
				s.state = alStopped
				for i := range s.queue {
					s.queue[i].done = true
				}
			}
			c.Ret()
		}),
		"_alSourcePause": locked(func(c *cpu.CPU) {
			if s := src(c); s != nil && s.state == alPlaying {
				s.state = alPaused
			}
			c.Ret()
		}),
		"_alSourceRewind": locked(func(c *cpu.CPU) {
			if s := src(c); s != nil {
				s.state, s.pos = alInitial, 0
				for i := range s.queue {
					s.queue[i].done = false
				}
			}
			c.Ret()
		}),
		"_alSourcei": locked(func(c *cpu.CPU) {
			if s := src(c); s != nil {
				switch c.Arg(1) {
				case alBuffer:
					s.queue, s.static, s.pos = nil, true, 0
					if c.Arg(2) != 0 {
						s.queue = []alEntry{{buf: c.Arg(2)}}
					}
				case alLooping:
					s.looping = c.Arg(2) != 0
				}
			}
			c.Ret()
		}),
		"_alSourcef": locked(func(c *cpu.CPU) {
			if s := src(c); s != nil && c.Arg(1) == alGain {
				s.gain = float64(c.ArgF32(2))
			}
			c.Ret()
		}),
		"_alListenerf": locked(func(c *cpu.CPU) {
			if c.Arg(0) == alGain {
				a.listener = float64(c.ArgF32(1))
			}
			c.Ret()
		}),
		"_alSource3f": retVoid, "_alSourcefv": retVoid, "_alListener3f": retVoid, "_alListenerfv": retVoid,
		"_alDistanceModel": retVoid, "_alGetSourcef": func(c *cpu.CPU) { c.WriteF32(c.Arg(2), 0); c.Ret() },
	} {
		m.shims[k] = v
	}
}

// startOpenAL starts mixing (into the window's ring when there is one).
func (m *Machine) startOpenAL() {
	a := &m.al
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return
	}
	a.started = true
	a.start = m.micros()
	if fb := m.input.fb; fb != nil {
		a.ring = fb[d3d9.ShmAudioOffset : d3d9.ShmAudioOffset+8+d3d9.ShmAudioFrames*4]
		a.written = atomic.LoadUint64((*uint64)(unsafe.Pointer(&a.ring[0])))
	}
	if m.Live {
		go func() {
			for range time.Tick(10 * time.Millisecond) {
				a.mu.Lock()
				a.mix(m.micros())
				a.mu.Unlock()
			}
		}()
	}
}

// mix produces the frames due by now (µs on the machine's clock), plus the lead.
func (a *openAL) mix(now uint64) {
	if !a.started || now < a.start {
		return
	}
	target := (now-a.start)*alRate/1e6 + alLead
	if target <= a.written {
		return
	}
	n := target - a.written
	if n > d3d9.ShmAudioFrames/2 { // far behind (the machine stalled): skip ahead
		a.written = target - alLead
		n = alLead
	}
	frame := make([]float64, 2*n)
	for _, s := range a.srcs {
		if s.state != alPlaying {
			continue
		}
		g := s.gain * a.listener
		for i := uint64(0); i < n; i++ {
			e := a.front(s)
			if e == nil {
				if !s.looping {
					s.state = alStopped
					a.dry = append(a.dry, now-a.start)
				}
				break
			}
			b := a.bufs[e.buf]
			f := int(s.pos)
			frame[2*i] += float64(b.pcm[2*f]) * g
			frame[2*i+1] += float64(b.pcm[2*f+1]) * g
			s.pos += b.rate / alRate
		}
	}
	if a.ring != nil {
		for i := uint64(0); i < n; i++ {
			k := 8 + ((a.written+i)&alRingMask)*4
			binary.LittleEndian.PutUint16(a.ring[k:], uint16(clamp16(frame[2*i])))
			binary.LittleEndian.PutUint16(a.ring[k+2:], uint16(clamp16(frame[2*i+1])))
		}
		atomic.StoreUint64((*uint64)(unsafe.Pointer(&a.ring[0])), a.written+n)
	}
	a.written += n
}

// front returns the buffer being played (finishing exhausted ones), or nil.
func (a *openAL) front(s *alSrc) *alEntry {
	for {
		i := 0
		for i < len(s.queue) && s.queue[i].done {
			i++
		}
		if i == len(s.queue) {
			if s.looping && s.static && len(s.queue) > 0 {
				s.queue[0].done, s.pos = false, 0
				continue
			}
			return nil
		}
		e := &s.queue[i]
		b := a.bufs[e.buf]
		if b != nil && int(s.pos) < len(b.pcm)/2 {
			return e
		}
		e.done, s.pos = true, 0
	}
}

func clamp16(v float64) int16 {
	return int16(math.Max(-32768, math.Min(32767, v)))
}

// toStereo16 converts AL_FORMAT_MONO8/MONO16/STEREO8/STEREO16 data to stereo 16-bit frames.
func toStereo16(b []byte, format uint32) []int16 {
	var out []int16
	switch format {
	case 0x1100: // mono 8
		for _, v := range b {
			s := int16(int(v)-128) << 8
			out = append(out, s, s)
		}
	case 0x1101: // mono 16
		for i := 0; i+1 < len(b); i += 2 {
			s := int16(binary.LittleEndian.Uint16(b[i:]))
			out = append(out, s, s)
		}
	case 0x1102: // stereo 8
		for _, v := range b {
			out = append(out, int16(int(v)-128)<<8)
		}
	default: // stereo 16
		for i := 0; i+1 < len(b); i += 2 {
			out = append(out, int16(binary.LittleEndian.Uint16(b[i:])))
		}
	}
	if len(out)%2 == 1 {
		out = out[:len(out)-1]
	}
	return out
}

package source

import (
	"unicode/utf16"
	"unicode/utf8"

	"catalina32/pkg/cpu"
)

// Audio and the system services around it: Multiprocessing Services (the semaphores and critical
// regions Miles uses), a null OpenAL that consumes queued buffers in (virtual) real time, and
// iconv for the text conversions vgui does.

type mpSem struct {
	value, max uint32
}

type mpRegion struct {
	owner *gthread
	count int
}

type alSource struct {
	queue []alQueued
}

type alQueued struct {
	buffer uint32
	end    uint64 // microsecond the buffer finishes playing
}

type audioState struct {
	sems     map[uint32]*mpSem
	regions  map[uint32]*mpRegion
	next     uint32
	sources  map[uint32]*alSource
	bufBytes map[uint32]uint32 // buffer -> bytes
	bufRate  map[uint32]uint32 // buffer -> bytes per second
	iconv    map[uint32][2]string
}

const (
	kMPTimeoutErr   = 0xffff8d90 // -29296
	durationForever = 0x7fffffff
)

// mpDuration converts a Duration (ms if positive, µs if negative) to microseconds; 0 = forever.
func mpDuration(d uint32) (uint64, bool) {
	switch {
	case d == durationForever:
		return 0, true
	case int32(d) < 0:
		return uint64(-int64(int32(d))), false
	}
	return uint64(d) * 1000, false
}

func (m *Machine) registerAudioShims() {
	a := &m.audio
	a.sems, a.regions, a.sources = map[uint32]*mpSem{}, map[uint32]*mpRegion{}, map[uint32]*alSource{}
	a.bufBytes, a.bufRate, a.iconv = map[uint32]uint32{}, map[uint32]uint32{}, map[uint32][2]string{}
	a.next = 0x5000
	id := func() uint32 { a.next++; return a.next }
	for k, v := range map[string]cpu.ThunkFunc{
		"_MPCreateCriticalRegion": func(c *cpu.CPU) { h := id(); a.regions[h] = &mpRegion{}; c.Write32(c.Arg(0), h); ret(c, 0) },
		"_MPDeleteCriticalRegion": func(c *cpu.CPU) { delete(a.regions, c.Arg(0)); ret(c, 0) },
		"_MPEnterCriticalRegion":  m.mpEnter,
		"_MPExitCriticalRegion": func(c *cpu.CPU) {
			if r := a.regions[c.Arg(0)]; r != nil && r.owner == m.threads.cur {
				if r.count--; r.count <= 0 {
					r.owner, r.count = nil, 0
				}
			}
			ret(c, 0)
		},
		"_MPCreateSemaphore": func(c *cpu.CPU) {
			h := id()
			a.sems[h] = &mpSem{value: c.Arg(1), max: c.Arg(0)}
			c.Write32(c.Arg(2), h)
			ret(c, 0)
		},
		"_MPDeleteSemaphore": func(c *cpu.CPU) { delete(a.sems, c.Arg(0)); ret(c, 0) },
		"_MPSignalSemaphore": func(c *cpu.CPU) {
			if s := a.sems[c.Arg(0)]; s != nil && s.value < s.max {
				s.value++
			}
			ret(c, 0)
		},
		"_MPWaitOnSemaphore": m.mpWait,
		"_MPYield":           m.yieldShim,

		// OpenAL: one device, one context, sources that play queued buffers in time.
		"_alcOpenDevice":         func(c *cpu.CPU) { ret(c, id()) },
		"_alcCloseDevice":        retN(1),
		"_alcCreateContext":      func(c *cpu.CPU) { ret(c, id()) },
		"_alcMakeContextCurrent": retN(1),
		"_alcDestroyContext":     retVoid,
		"_alcGetCurrentContext":  retN(0x5001),
		"_alcGetContextsDevice":  retN(0x5000),
		"_alcGetError":           ret0,
		"_alcGetString":          func(c *cpu.CPU) { ret(c, m.glStr("Null Audio Device")) },
		"_alcGetIntegerv":        func(c *cpu.CPU) { c.Write32(c.Arg(3), 44100); c.Ret() },
		"_alcIsExtensionPresent": ret0,
		"_alIsExtensionPresent":  ret0,
		"_alGetProcAddress":      ret0,
		"_alGetError":            ret0,
		"_alGetString":           func(c *cpu.CPU) { ret(c, m.glStr("1.1")) },
		"_alGenSources": func(c *cpu.CPU) {
			for i := uint32(0); i < c.Arg(0); i++ {
				h := id()
				a.sources[h] = &alSource{}
				c.Write32(c.Arg(1)+4*i, h)
			}
			c.Ret()
		},
		"_alGenBuffers": func(c *cpu.CPU) {
			for i := uint32(0); i < c.Arg(0); i++ {
				c.Write32(c.Arg(1)+4*i, id())
			}
			c.Ret()
		},
		"_alBufferData": func(c *cpu.CPU) { // (buffer, format, data, size, freq)
			bytesPerFrame := map[uint32]uint32{0x1100: 1, 0x1101: 2, 0x1102: 2, 0x1103: 4}[c.Arg(1)]
			a.bufBytes[c.Arg(0)], a.bufRate[c.Arg(0)] = c.Arg(3), max(bytesPerFrame, 1)*c.Arg(4)
			c.Ret()
		},
		"_alSourceQueueBuffers": func(c *cpu.CPU) { // (source, n, buffers)
			s := a.sources[c.Arg(0)]
			if s == nil {
				c.Ret()
				return
			}
			end := m.micros()
			if len(s.queue) > 0 && s.queue[len(s.queue)-1].end > end {
				end = s.queue[len(s.queue)-1].end
			}
			for i := uint32(0); i < c.Arg(1); i++ {
				b := c.Read32(c.Arg(2) + 4*i)
				if r := a.bufRate[b]; r > 0 {
					end += uint64(a.bufBytes[b]) * 1e6 / uint64(r)
				}
				s.queue = append(s.queue, alQueued{b, end})
			}
			c.Ret()
		},
		"_alSourceUnqueueBuffers": func(c *cpu.CPU) {
			s := a.sources[c.Arg(0)]
			for i := uint32(0); s != nil && i < c.Arg(1) && len(s.queue) > 0; i++ {
				c.Write32(c.Arg(2)+4*i, s.queue[0].buffer)
				s.queue = s.queue[1:]
			}
			c.Ret()
		},
		"_alGetSourcei": func(c *cpu.CPU) { // (source, param, &value)
			s := a.sources[c.Arg(0)]
			v := uint32(0)
			if s != nil {
				switch c.Arg(1) {
				case 0x1016: // AL_BUFFERS_PROCESSED
					for _, q := range s.queue {
						if q.end <= m.micros() {
							v++
						}
					}
				case 0x1015: // AL_BUFFERS_QUEUED
					v = uint32(len(s.queue))
				case 0x1010: // AL_SOURCE_STATE
					v = 0x1012 // AL_PLAYING
				}
			}
			c.Write32(c.Arg(2), v)
			c.Ret()
		},
		"_alSourcePlay": retVoid, "_alSourceStop": retVoid, "_alSourcePause": retVoid, "_alSourceRewind": retVoid,
		"_alSourcef": retVoid, "_alSourcei": retVoid, "_alSource3f": retVoid, "_alSourcefv": retVoid,
		"_alListenerf": retVoid, "_alListener3f": retVoid, "_alListenerfv": retVoid, "_alDistanceModel": retVoid,
		"_alDeleteSources": retVoid, "_alDeleteBuffers": retVoid,

		"_iconv_open":  m.iconvOpen,
		"_iconv":       m.iconv,
		"_iconv_close": ret0,
	} {
		m.shims[k] = v
	}
}

func (m *Machine) mpEnter(c *cpu.CPU) {
	r := m.audio.regions[c.Arg(0)]
	if r == nil {
		ret(c, 0)
		return
	}
	t := m.threads.cur
	if r.owner == nil || r.owner == t || r.owner.done {
		if r.owner != t {
			r.owner, r.count = t, 0
		}
		r.count++
		ret(c, 0)
		return
	}
	us, forever := mpDuration(c.Arg(1))
	if !forever && us == 0 {
		ret(c, kMPTimeoutErr)
		return
	}
	if !m.park(c, func() bool { return r.owner != nil && !r.owner.done }, 0) {
		ret(c, kMPTimeoutErr)
	}
}

// mpWait parks until the semaphore is signalled or the timeout passes; it runs again then.
func (m *Machine) mpWait(c *cpu.CPU) {
	s := m.audio.sems[c.Arg(0)]
	t := m.threads.cur
	if s == nil {
		ret(c, kMPTimeoutErr)
		return
	}
	if s.value > 0 {
		s.value--
		t.mpDeadline = 0
		ret(c, 0)
		return
	}
	if t.mpDeadline != 0 && m.micros() >= t.mpDeadline {
		t.mpDeadline = 0
		ret(c, kMPTimeoutErr)
		return
	}
	us, forever := mpDuration(c.Arg(1))
	if !forever && us == 0 {
		ret(c, kMPTimeoutErr)
		return
	}
	if t.mpDeadline == 0 && !forever {
		t.mpDeadline = m.micros() + us
	}
	deadline := t.mpDeadline
	if !m.park(c, func() bool { return s.value == 0 && (deadline == 0 || m.micros() < deadline) }, deadline) {
		ret(c, kMPTimeoutErr)
	}
}

func (m *Machine) iconvOpen(c *cpu.CPU) {
	to, from := c.ReadCString(c.Arg(0)), c.ReadCString(c.Arg(1))
	h := m.audio.next + 1
	m.audio.next++
	m.audio.iconv[h] = [2]string{to, from}
	ret(c, h)
}

// decodeEnc / encodeEnc handle the encodings games use: UTF-8, UTF-16/UCS-2 and UTF-32/UCS-4/WCHAR_T
// (little-endian).
func encWidth(enc string) int {
	switch enc {
	case "UTF-8", "UTF8", "utf-8", "utf8", "ASCII", "US-ASCII", "ISO-8859-1", "MACINTOSH", "MacRoman":
		return 1
	case "UCS-2", "UCS-2LE", "UTF-16", "UTF-16LE", "UCS2", "UTF16":
		return 2
	}
	return 4
}

func (m *Machine) iconv(c *cpu.CPU) {
	cd, inPP, inLeftP, outPP, outLeftP := c.Arg(0), c.Arg(1), c.Arg(2), c.Arg(3), c.Arg(4)
	conv, ok := m.audio.iconv[cd]
	if !ok || inPP == 0 || c.Read32(inPP) == 0 {
		ret(c, 0)
		return
	}
	in, inLeft := c.Read32(inPP), c.Read32(inLeftP)
	out, outLeft := c.Read32(outPP), c.Read32(outLeftP)
	var runes []rune
	src := c.Bytes(in, inLeft)
	switch encWidth(conv[1]) {
	case 1:
		for len(src) > 0 {
			r, n := utf8.DecodeRune(src)
			runes, src = append(runes, r), src[n:]
		}
	case 2:
		u := make([]uint16, len(src)/2)
		for i := range u {
			u[i] = uint16(src[2*i]) | uint16(src[2*i+1])<<8
		}
		runes = utf16.Decode(u)
	default:
		for i := 0; i+4 <= len(src); i += 4 {
			runes = append(runes, rune(uint32(src[i])|uint32(src[i+1])<<8|uint32(src[i+2])<<16|uint32(src[i+3])<<24))
		}
	}
	var dst []byte
	switch encWidth(conv[0]) {
	case 1:
		for _, r := range runes {
			dst = utf8.AppendRune(dst, r)
		}
	case 2:
		for _, u := range utf16.Encode(runes) {
			dst = append(dst, byte(u), byte(u>>8))
		}
	default:
		for _, r := range runes {
			dst = append(dst, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
		}
	}
	if uint32(len(dst)) > outLeft {
		dst = dst[:outLeft]
	}
	copy(c.WBytes(out, uint32(len(dst))), dst)
	c.Write32(inPP, in+inLeft)
	c.Write32(inLeftP, 0)
	c.Write32(outPP, out+uint32(len(dst)))
	c.Write32(outLeftP, outLeft-uint32(len(dst)))
	ret(c, 0)
}

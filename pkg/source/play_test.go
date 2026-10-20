package source

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"unsafe"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3d9"
)

// C32_SOURCE_DIR=<Portal> go test ./pkg/source -run PlayIO -v
//
// Plays Portal's first chamber headless with a stand-in window (the GL stream checked, a real
// framebuffer shared memory) and feeds it input the way the window does: mouse movement, a
// controller. Then checks that the game itself took them (the launcher posts mouse moves, the
// client turns the view by them, inputsystem gets the controller) and that sound reaches the
// window's audio ring.
func TestPlayIO(t *testing.T) {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set")
	}
	const injectFrom, injectTo, end = 6_500_000_000, 9_000_000_000, 10_000_000_000
	tmp := t.TempDir()
	glPath := filepath.Join(tmp, "gl.shm")
	t.Setenv("C32_GL_SHM", glPath)
	t.Setenv("C32_SHM_FILE", filepath.Join(tmp, "framebuffer.shm"))
	stop := make(chan struct{})
	checked := make(chan struct{})
	go func() {
		if _, err := RunGLCheck(glPath, stop); err != nil {
			t.Errorf("glcheck: %v", err)
		}
		close(checked)
	}()
	log, _ := os.Create(filepath.Join(tmp, "play.log"))
	defer log.Close()
	m, err := New(dir, []string{"-game", "portal", "+map", "testchmb_a_00"}, filepath.Join(tmp, "overlay"), log)
	if err != nil {
		t.Fatal(err)
	}

	// What the game does with the input.
	posted := map[uint32]int{} // launcher: CCocoaEvent types posted to the engine
	calls := map[string]int{}
	var mouseX, mouseY float64 // client: deltas CInput::ApplyMouse receives
	m.Watch(regexp.MustCompile(`^__ZN7CSDLMgr9PostEventERK11CCocoaEventb$|JoystickSDLWatcher|CInputSystem.*Joystick|CInputSystem.*Controller|CInputSystem.*PostButton|^__ZN6CInput10ApplyMouseER6QAngleP8CUserCmdff$`),
		func(name string, c *cpu.CPU) {
			switch {
			case strings.Contains(name, "PostEvent"):
				posted[c.Read32(c.Arg(1))]++
			case strings.Contains(name, "ApplyMouse"):
				mouseX += math.Abs(float64(c.ArgF32(3)))
				mouseY += math.Abs(float64(c.ArgF32(4)))
				calls[name]++
			default:
				calls[name]++
			}
		})

	// The input, as the window writes it.
	moves := 0
	m.OnPoll = func(c *cpu.CPU) {
		if c.Steps < injectFrom || c.Steps > injectTo || m.glr == nil {
			return
		}
		g := m.glr
		if moves < 400 { // a mouse move to the right
			w := atomic.LoadUint32(g.word(128))
			e := g.shm[glEventsOffset+glEventSize*int(w%glEvents):]
			clear(e[:glEventSize])
			binary.LittleEndian.PutUint32(e, evMotion)
			binary.LittleEndian.PutUint32(e[20:], 12)
			atomic.StoreUint32(g.word(128), w+1)
			moves++
		}
		fb := m.input.fb // controller 1: connected, A pressed for a while, left stick right
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&fb[d3d9.ShmPadOffset])), d3d9.ShmPadReady)
		r := fb[d3d9.ShmPadOffset+4:]
		binary.LittleEndian.PutUint32(r, 1)
		a := uint16(0)
		if (c.Steps/200_000_000)%2 == 0 {
			a = 0x1000
		}
		binary.LittleEndian.PutUint16(r[4:], a)
		binary.LittleEndian.PutUint16(r[8:], 0x6000)
	}

	err = m.Run(end)
	close(stop)
	<-checked
	t.Logf("stopped after %d instructions: %v; %d frames", m.C.Steps, err, m.sdl.frames)
	if m.C.Steps < end {
		t.Fatalf("the game stopped early")
	}

	// Sound: the ring advanced, with non-silent samples.
	fb := m.input.fb
	written := atomic.LoadUint64((*uint64)(unsafe.Pointer(&fb[d3d9.ShmAudioOffset])))
	loud := 0
	for i := 0; i < d3d9.ShmAudioFrames*2; i++ {
		if v := int16(binary.LittleEndian.Uint16(fb[d3d9.ShmAudioOffset+8+2*i:])); v > 64 || v < -64 {
			loud++
		}
	}
	t.Logf("OpenAL: %d buffers, %d non-silent samples; sources %d", m.al.buffers, m.al.loudIn, len(m.al.srcs))
	for h, s := range m.al.srcs {
		t.Logf("  source %#x: state %#x gain %.2f queued %d", h, s.state, s.gain, len(s.queue))
	}
	t.Logf("sound: %d frames mixed (%.1f s), %d non-silent samples in the ring", written, float64(written)/alRate, loud)
	t.Logf("mouse: %d moves sent; launcher posted %v (3 = mouse move); ApplyMouse saw |dx| %.0f |dy| %.0f; captured %v",
		moves, posted, mouseX, mouseY, m.input.captured)
	t.Logf("controller: %v", calls)
	if written == 0 || loud == 0 {
		t.Error("no sound reached the window")
	}
	if posted[3] == 0 {
		t.Error("the launcher took no mouse movement")
	}
	if mouseX == 0 {
		t.Error("the view didn't turn: no mouse deltas reached the client")
	}
	joy := 0
	for n, k := range calls {
		if strings.Contains(n, "Joystick") && !strings.Contains(n, "Count") || strings.Contains(n, "PostButton") {
			joy += k
		}
	}
	if joy == 0 {
		t.Error("inputsystem saw no controller")
	}
}

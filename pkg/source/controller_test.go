package source

import (
	"encoding/binary"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3d9"
)

// C32_SOURCE_DIR=<Portal> go test ./pkg/source -run Controller -v: plays the first chamber with a
// controller connected before the game starts, the left stick pushed right and A pressed on and
// off, and follows them through the game: inputsystem's axis and button handling, the client's
// joystick move, the player's command (sidemove). The triggers are pulled too (the right one on
// and off, then the left), and followed to the commands they are bound to (+attack, +attack2:
// the portal gun). C32_CTL_LOAD=<save> loads a saved game from the menu first (with C32_OVERLAY
// holding it).
func TestController(t *testing.T) {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set")
	}
	var injectFrom, end uint64 = 6_500_000_000, 9_000_000_000
	args := []string{"+map", "testchmb_a_00"}
	var console func(*cpu.CPU)
	if save := os.Getenv("C32_CTL_LOAD"); save != "" { // from the menu, load a saved game (in C32_OVERLAY), as a player does
		args = nil
		injectFrom, end = 10_000_000_000, 12_500_000_000
	}
	m, finish := withWindow(t, dir, args...)
	if save := os.Getenv("C32_CTL_LOAD"); save != "" {
		console = consoleAt(t, m, "6500000000:load "+save)
	}
	if v := os.Getenv("C32_CMDS"); v != "" { // more console commands (consoleAt)
		console = consoleAt(t, m, v)
	}
	f32 := func(c *cpu.CPU, a uint32) float32 { return math.Float32frombits(c.Read32(a)) }

	calls := map[string]int{}
	var axes, buttons []string
	m.Watch(regexp.MustCompile(`^__ZN12CInputSystem(18JoystickAxisMotionEiii|22PostButtonPressedEvent.*|12PollJoystickEv)$|^__ZN6CInput(18ControllerCommandsEv|19Joystick_GetForwardEv|16Joystick_GetSideEv)$|^__Z1[34]IN_Attack2?DownRK8CCommand$`),
		func(name string, c *cpu.CPU) {
			calls[name]++
			switch {
			case strings.Contains(name, "AxisMotion") && len(axes) < 16:
				axes = append(axes, strings.Join([]string{strconv.Itoa(int(c.Arg(1))), strconv.Itoa(int(c.Arg(2))), strconv.Itoa(int(int32(c.Arg(3))))}, "/"))
			case strings.Contains(name, "PostButtonPressed") && len(buttons) < 16:
				buttons = append(buttons, strconv.Itoa(int(c.Arg(3))))
			}
		})
	var side, maxSide float32
	m.Around(regexp.MustCompile(`^__ZN6CInput12JoyStickMoveEfP8CUserCmd$`), func(_ string, c *cpu.CPU) func(*cpu.CPU) {
		calls["JoyStickMove"]++
		cmd := c.Arg(2)
		before := f32(c, cmd+28) // CUserCmd: vtable, command_number, tick_count, viewangles[3], forwardmove, sidemove
		return func(c *cpu.CPU) {
			side = f32(c, cmd+28) - before
			if side > maxSide {
				maxSide = side
			}
		}
	})

	// Controller 1 is there before the game starts, as when it is launched with one connected
	// (the window finds controllers within its first ticks).
	binary.LittleEndian.PutUint32(m.input.fb[d3d9.ShmPadOffset:], d3d9.ShmPadReady)
	r := m.input.fb[d3d9.ShmPadOffset+4:]
	binary.LittleEndian.PutUint32(r, 1)
	m.OnPoll = func(c *cpu.CPU) {
		if console != nil {
			console(c)
		}
		if c.Steps < injectFrom {
			return
		}
		a := uint16(0)
		if (c.Steps/200_000_000)%2 == 0 {
			a = 0x1000
		}
		binary.LittleEndian.PutUint16(r[4:], a)
		binary.LittleEndian.PutUint16(r[8:], 0x6000) // left stick right
		r[6], r[7] = 0, 0                            // triggers: right on and off, then left
		switch k := (c.Steps - injectFrom) / 250_000_000; {
		case k == 2 || k == 4:
			r[7] = 255
		case k == 6:
			r[6] = 255
		}
	}
	finish(end)
	t.Logf("calls: %v", calls)
	t.Logf("axis motion (joystick/axis/value): %v; buttons pressed: %v", axes, buttons)
	t.Logf("JoyStickMove: sidemove added %.1f (most %.1f)", side, maxSide)
	if maxSide <= 0 {
		t.Error("the left stick didn't move the player sideways")
	}
	if len(buttons) == 0 {
		t.Error("inputsystem saw no button press")
	}
	if calls["__Z13IN_AttackDownRK8CCommand"] == 0 {
		t.Error("the right trigger didn't reach +attack")
	}
	if calls["__Z14IN_Attack2DownRK8CCommand"] == 0 {
		t.Error("the left trigger didn't reach +attack2")
	}
}

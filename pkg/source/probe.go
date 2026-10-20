package source

import (
	"regexp"

	"catalina32/pkg/cpu"
	"catalina32/pkg/dyld"
)

type probeFrame struct {
	label    string
	ret, esp uint32
}

// Probe logs the first few calls of every function whose symbol matches re (in images loaded
// now and later) with their caller and return value, for finding where a run goes wrong.
func (m *Machine) Probe(re *regexp.Regexp, maxPerFunc int) {
	var frames []probeFrame
	retThunk := m.system("", "<probe return>")
	m.C.RegisterIntercept(retThunk, "<probe return>", func(c *cpu.CPU) {
		esp := c.Regs[cpu.RegESP]
		for i := len(frames) - 1; i >= 0; i-- {
			if f := frames[i]; f.esp < esp {
				m.logf("[probe]   %s returned %#x", f.label, c.Regs[cpu.RegEAX])
				c.EIP = f.ret
				frames = frames[:i]
				return
			}
		}
		c.Halted, c.HaltReason = true, "probe: lost a return address"
	})
	probe := func(img *dyld.Image) {
		img.ForEachSymbol(func(name string, addr uint32) {
			if !re.MatchString(name) || m.C.Intercepts[addr] != nil {
				return
			}
			n := 0
			label := img.Name() + "!" + name
			m.C.RegisterIntercept(addr, label, func(c *cpu.CPU) {
				n++
				esp := c.Regs[cpu.RegESP]
				m.logf("[probe] %s from %s (thread %s)", label, m.where(c.Read32(esp)), m.threads.cur.name)
				frames = append(frames, probeFrame{label, c.Read32(esp), esp})
				c.Write32(esp, retThunk)
				c.StepRaw() // the function's first instruction, then on as usual
				if n >= maxPerFunc {
					c.SetInterceptEnabled(addr, false)
				}
			})
		})
	}
	for _, img := range m.L.Images {
		probe(img)
	}
	prev := m.L.Mapped
	m.L.Mapped = func(img *dyld.Image) {
		prev(img)
		probe(img)
	}
}

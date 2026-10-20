package source

import (
	"encoding/binary"
	"strings"

	"catalina32/pkg/cpu"
)

// The GPU as IOKit describes it. ToGL's renderer table (launcher's GLMDisplayDB) reads the
// display's PCI vendor and device and its model, and works around drivers by vendor: on AMD
// (vendor 0x1002) it doesn't trust gl_ClipVertex (AMD's Mac drivers clip against gl_Position)
// and adjusts the clip planes it gives GL itself. So the vendor is that of the GPU the window
// draws with, from its GL_VENDOR; without it, portals on AMD Macs lose walls and floors to
// misplaced clip planes.

const gpuService = 0x6770 // the display's IOService handle

func (m *Machine) registerIOKitShims() {
	m.cfData = map[uint32][]byte{}
	for k, v := range map[string]cpu.ThunkFunc{
		"_CGDisplayIOServicePort": retN(gpuService),
		"_IORegistryEntrySearchCFProperty": func(c *cpu.CPU) { // (entry, plane, key, allocator, options)
			if c.Arg(0) != gpuService {
				ret(c, 0)
				return
			}
			vendor, model := m.gpu()
			switch m.cfName(c, c.Arg(2)) {
			case "vendor-id":
				m.logf("[gpu] %s (PCI vendor %#04x)", model, vendor)
				ret(c, m.newCFData(c, binary.LittleEndian.AppendUint32(nil, vendor)))
			case "device-id": // unknown: no model-specific workarounds
				ret(c, m.newCFData(c, make([]byte, 4)))
			case "model":
				ret(c, m.newCFData(c, append([]byte(model), 0)))
			default:
				ret(c, 0)
			}
		},
		"_CFDataGetBytePtr": func(c *cpu.CPU) {
			if _, ok := m.cfData[c.Arg(0)]; ok {
				ret(c, c.Read32(c.Arg(0)))
				return
			}
			ret(c, 0)
		},
		"_CFDataGetLength": func(c *cpu.CPU) { ret(c, uint32(len(m.cfData[c.Arg(0)]))) },
		"_IOObjectRelease": retN(0),
	} {
		m.shims[k] = v
	}
}

// gpu returns the window's GPU: its PCI vendor (0 when not AMD, NVIDIA or Intel) and model.
func (m *Machine) gpu() (uint32, string) {
	vendor, renderer := "ATI Technologies Inc.", "AMD Radeon Pro 560 OpenGL Engine" // the null GL's
	if m.glOn {
		vendor, renderer = m.glr.str(0x1F00), m.glr.str(0x1F01)
	}
	return pciVendor(vendor), strings.TrimSuffix(renderer, " OpenGL Engine")
}

// pciVendor maps a GL_VENDOR string to a PCI vendor ID.
func pciVendor(glVendor string) uint32 {
	v := strings.ToLower(glVendor)
	switch {
	case strings.Contains(v, "nvidia"):
		return 0x10de
	case strings.HasPrefix(v, "ati ") || strings.Contains(v, "amd") || strings.Contains(v, "advanced micro devices"):
		return 0x1002
	case strings.Contains(v, "intel"):
		return 0x8086
	}
	return 0
}

// newCFData makes a CFData holding b: the object's first word points to the bytes.
func (m *Machine) newCFData(c *cpu.CPU, b []byte) uint32 {
	p := c.Malloc(uint32(len(b)) + 4)
	c.Write32(p, p+4)
	copy(c.WBytes(p+4, uint32(len(b))), b)
	m.cfData[p] = b
	return p
}

// cfName reads a CFString: one made by CFStringCreateWithCString, or a constant from an
// image's __cfstring section (isa, flags, characters, length).
func (m *Machine) cfName(c *cpu.CPU, s uint32) string {
	if v, ok := m.cfStrings[s]; ok {
		return v
	}
	if s == 0 {
		return ""
	}
	return c.ReadCString(c.Read32(s + 8))
}

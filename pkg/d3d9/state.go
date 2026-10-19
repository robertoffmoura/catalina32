package d3d9

// D3DRENDERSTATETYPE values used by the rasterizer.
const (
	rsZEnable                = 7
	rsZWriteEnable           = 14
	rsAlphaTestEnable        = 15
	rsSrcBlend               = 19
	rsDestBlend              = 20
	rsCullMode               = 22
	rsZFunc                  = 23
	rsAlphaRef               = 24
	rsAlphaFunc              = 25
	rsAlphaBlendEnable       = 27
	rsFogEnable              = 28
	rsFogColor               = 34
	rsFogTableMode           = 35
	rsFogStart               = 36
	rsFogEnd                 = 37
	rsFogDensity             = 38
	rsColorWriteEnable       = 168
	rsScissorTestEnable      = 174
	rsBlendOp                = 171
	rsBlendFactor            = 193
	rsSeparateAlphaBlendEnab = 206
)

// rsDefaults are the D3D9 device defaults for states the rasterizer reads (ZENABLE assumes an
// automatic depth-stencil surface).
var rsDefaults = [256]uint32{
	rsZEnable:          1,
	rsZWriteEnable:     1,
	rsSrcBlend:         2, // ONE
	rsDestBlend:        1, // ZERO
	rsCullMode:         3, // CCW
	rsZFunc:            4, // LESSEQUAL
	rsAlphaFunc:        8, // ALWAYS
	rsColorWriteEnable: 0xF,
	rsBlendOp:          1, // ADD
	rsBlendFactor:      0xFFFFFFFF,
	rsFogEnd:           0x3F800000, // 1.0
	rsFogDensity:       0x3F800000, // 1.0
}

// renderState returns the effective value of a render state: what the game set, or the device
// default. RenderStates itself keeps reporting what it always did to GetRenderState.
func (s *Subsystem) renderState(i int) uint32 {
	if s.rsSet[i] {
		return s.RenderStates[i]
	}
	return rsDefaults[i]
}

// D3DSAMPLERSTATETYPE values used by the rasterizer.
const (
	sampAddressU  = 1
	sampAddressV  = 2
	sampMagFilter = 5
)

// samplerState returns the effective value of a sampler state (default: wrap addressing, point
// filtering).
func (s *Subsystem) samplerState(st, ty int) uint32 {
	if s.sampSet[st][ty] {
		return s.samplerStates[st][ty]
	}
	switch ty {
	case sampAddressU, sampAddressV:
		return 1 // D3DTADDRESS_WRAP
	case sampMagFilter:
		return 1 // D3DTEXF_POINT
	}
	return 0
}

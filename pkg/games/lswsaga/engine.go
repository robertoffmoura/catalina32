package lswsaga

import (
	"lsw64/pkg/cpu"
	"lsw64/pkg/d3d9"
	"lsw64/pkg/profile"
)

// NuEngine data used by the hooks below.
const (
	gNuTexArray       = 0x02c25d08 // NuTex* table indexed by texture id (1-based)
	gBoundLayer0Tex   = 0x02b5af74 // engine's cached stage-0 texture
	gShaderParamTable = 0x00b2842c // global shader parameter table (92-byte entries)
)

// Install registers this game's engine-specific hooks, code patches and lifted functions. Generic
// shims are bound separately through Profile.
func Install(c *cpu.CPU, d3d *d3d9.Subsystem, prof *profile.Profile) {
	// Unidentified MacDoze helper whose result the engine ignores.
	c.RegisterIntercept(0x00620da0, "MacDoze::Unknown_620da0", returnZero)

	// Skip the fullscreen bloom/blur post-processing pass over the software framebuffer.
	c.RegisterIntercept(0x0001d900, "NuPostProcessRender", returnZero)
	// Prevent directional lightmap binding from overwriting the stage 0 diffuse texture (layer0_sampler).
	c.RegisterIntercept(0x0006f840, "NuBindLightmaps", returnZero)
	c.RegisterIntercept(0x00070730, "NuBindLightmapsA", returnZero)
	c.RegisterIntercept(0x000708a0, "NuBindLightmapsB", returnZero)
	// Capture skeletal animation bone matrices uploaded for skinned character meshes.
	c.RegisterIntercept(0x00070110, "NuUploadSkinMatrices", func(c *cpu.CPU) { nuUploadSkinMatrices(d3d, c) })
	// Populate NuShader constant & sampler descriptors so NuShaderBindMaterial binds textures & material colors.
	c.RegisterIntercept(0x001535c0, "NuShaderParseConstantTable", nuShaderParseConstantTable)
	c.RegisterIntercept(0x001538f0, "NuShaderBindMaterial", func(c *cpu.CPU) { nuShaderBindMaterial(d3d, c) })
	// Patch NuTexRead (0x00078df0) at 0x00079088 (jne 0x000791d0 -> 6x NOP) so .GSC textures with
	// flags & 0x3c0c are never skipped.
	for i := uint32(0); i < 6; i++ {
		c.Mem[0x00079088+i] = 0x90
	}

	registerLifted(c)
}

func returnZero(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func nuUploadSkinMatrices(s *d3d9.Subsystem, c *cpu.CPU) {
	buf := c.Arg(0)
	if buf != 0 && buf+4 <= uint32(len(c.Mem)) {
		n := c.Read32(buf)
		if n > 64 {
			n = 64
		}
		s.NumSkinMatrices = n
		ptr := buf + 4
		for i := uint32(0); i < n && ptr+64 <= uint32(len(c.Mem)); i++ {
			for j := uint32(0); j < 16; j++ {
				s.SkinMatrices[i][j] = c.ReadF32(ptr + j*4)
			}
			ptr += 64
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func nuLookupTex(c *cpu.CPU, tid int32) uint32 {
	if tid <= 0 {
		return 0
	}
	texArr := c.Read32(gNuTexArray)
	if texArr != 0 && texArr+uint32(tid)*4 <= uint32(len(c.Mem)) {
		nuTex := c.Read32(texArr + uint32(tid)*4 - 4)
		if nuTex != 0 && nuTex+0x28 <= uint32(len(c.Mem)) {
			return c.Read32(nuTex + 0x24)
		}
	}
	return 0
}

func nuShaderBindMaterial(s *d3d9.Subsystem, c *cpu.CPU) {
	mtl := c.Arg(1)
	s.CurrentMaterial = mtl
	if mtl != 0 && mtl+0x128 <= uint32(len(c.Mem)) {
		tex0 := nuLookupTex(c, int32(c.Read32(mtl+0xb8)))
		tex1 := nuLookupTex(c, int32(c.Read32(mtl+0xbc)))
		texSurf := nuLookupTex(c, int32(c.Read32(mtl+0x100)))
		texSurf2 := nuLookupTex(c, int32(c.Read32(mtl+0xec)))
		s.BoundTextures[0] = tex0
		s.BoundTextures[1] = tex1
		s.BoundTextures[2] = texSurf
		s.BoundTextures[3] = texSurf2
		c.Write32(gBoundLayer0Tex, tex0)

		const inv255 = 1.0 / 255.0
		c0 := c.Read32(mtl + 0xc8)
		s.PSConstants[112] = [4]float32{
			float32(c0&0xFF) * inv255,
			float32((c0>>8)&0xFF) * inv255,
			float32((c0>>16)&0xFF) * inv255,
			float32((c0>>24)&0xFF) * inv255,
		}
		cAmb := c.Read32(mtl + 0x11c)
		s.PSConstants[113] = [4]float32{
			float32(cAmb&0xFF) * inv255,
			float32((cAmb>>8)&0xFF) * inv255,
			float32((cAmb>>16)&0xFF) * inv255,
			float32((cAmb>>24)&0xFF) * inv255,
		}
		cSurf := c.Read32(mtl + 0x120)
		s.VSConstants[114] = [4]float32{
			float32(cSurf&0xFF) * inv255,
			float32((cSurf>>8)&0xFF) * inv255,
			float32((cSurf>>16)&0xFF) * inv255,
			float32((cSurf>>24)&0xFF) * inv255,
		}
		c1 := c.Read32(mtl + 0xcc)
		s.PSConstants[115] = [4]float32{
			float32(c1&0xFF) * inv255,
			float32((c1>>8)&0xFF) * inv255,
			float32((c1>>16)&0xFF) * inv255,
			float32((c1>>24)&0xFF) * inv255,
		}
		s.PSConstants[116] = [4]float32{
			c.ReadF32(mtl + 0xd8),
			c.ReadF32(mtl + 0xdc),
			c.ReadF32(mtl + 0xe0),
			c.ReadF32(mtl + 0xe4),
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func nuShaderParseConstantTable(c *cpu.CPU) {
	shader := c.Arg(0)
	tableBase := c.Read32(gShaderParamTable)
	if tableBase != 0 {
		// Enable global caching (entry+0x18 = 1) for matrix & camera parameters (17..22, 56, 85)
		// so NuShaderSetConstant (0x00232240) always writes live matrices into entry+0x1c.
		for _, pid := range []uint32{17, 18, 19, 20, 21, 22, 56, 85} {
			c.Write32(tableBase+pid*92+0x18, 1)
		}
	}
	if shader != 0 {
		// Node 36: layer0_sampler ([material+0xb8]) -> sampler stage 0
		node36 := shader + 0x1c + 36*12
		c.Write16(node36+0x00, 0)
		c.Write32(node36+0x04, 0x24000000|(36<<16))
		// Node 37: layer1_sampler ([material+0xbc]) -> sampler stage 1
		node37 := shader + 0x1c + 37*12
		c.Write16(node37+0x00, 1)
		c.Write32(node37+0x04, 0x24000000|(37<<16))
		// Node 42: surface_sampler ([material+0x100]) -> sampler stage 2
		node42 := shader + 0x1c + 42*12
		c.Write16(node42+0x00, 2)
		c.Write32(node42+0x04, 0x24000000|(42<<16))
		// Node 43: surface2_sampler ([material+0xec]) -> sampler stage 3
		node43 := shader + 0x1c + 43*12
		c.Write16(node43+0x00, 3)
		c.Write32(node43+0x04, 0x24000000|(43<<16))
		// Node 51: layer0_diffuse ([material+0xc8]) -> PS constant register 112
		node51 := shader + 0x1c + 51*12
		c.Write16(node51+0x00, 112)
		c.Write32(node51+0x04, 0x22000101|(51<<16))
		// Node 52: layer1_diffuse ([material+0xcc]) -> PS constant register 115
		node52 := shader + 0x1c + 52*12
		c.Write16(node52+0x00, 115)
		c.Write32(node52+0x04, 0x22000101|(52<<16))
		// Node 58: surface_diffuse ([material+0xf4]) -> PS constant register 116
		node58 := shader + 0x1c + 58*12
		c.Write16(node58+0x00, 116)
		c.Write32(node58+0x04, 0x22000101|(58<<16))
		// Node 0: ambientColor ([material+0x11c]) -> PS constant register 113
		node0 := shader + 0x1c + 0*12
		c.Write16(node0+0x00, 113)
		c.Write32(node0+0x04, 0x22000101|(0<<16))
		// Node 2: incandescentGlow / surface diffuse ([material+0x120]) -> VS constant register 114
		node2 := shader + 0x1c + 2*12
		c.Write16(node2+0x00, 114)
		c.Write32(node2+0x04, 0x12000101|(2<<16))

		c.Write32(node36+0x08, node37)
		c.Write32(node37+0x08, node42)
		c.Write32(node42+0x08, node43)
		c.Write32(node43+0x08, node51)
		c.Write32(node51+0x08, node52)
		c.Write32(node52+0x08, node58)
		c.Write32(node58+0x08, node0)
		c.Write32(node0+0x08, node2)
		c.Write32(node2+0x08, 0)
		c.Write32(shader+0x10, node36)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

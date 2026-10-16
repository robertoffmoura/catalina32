package d3d9

import (
	"fmt"
	"image"
	"math"
	"os"
	"sync"

	"lsw64/pkg/cpu"
)

const (
	VTableArenaBase = 0x0f000000
	TrampArenaBase  = 0x0f100000
)

type BufferDesc struct {
	DataPtr uint32
	Length  uint32
	Format  uint32
	Width   uint32
	Height  uint32
	Pitch   uint32
	Pixels  []uint32
}

type Subsystem struct {
	VM             *cpu.CPU
	rendering      bool // true while the frame being drawn will be rasterized/exported
	nextVTableAddr uint32
	nextTrampAddr  uint32

	vtD3D9       uint32
	vtDevice9    uint32
	vtTexture9   uint32
	vtCubeTex9   uint32
	vtVolTex9    uint32
	vtSurface9   uint32
	vtVertexBuf9 uint32
	vtIndexBuf9  uint32
	vtVertexDecl uint32
	vtVertexShdr uint32
	vtPixelShdr  uint32
	vtStateBlock uint32
	vtQuery9     uint32
	vtSwapChain9 uint32
	vtD3DXBuffer uint32
	vtDInput8    uint32
	vtDIDevice8  uint32
	vtDSound8    uint32
	vtDSBuffer8  uint32

	d3d9Obj         uint32
	device9Obj      uint32
	defaultBackBuf  uint32
	defaultDepthBuf uint32
	defaultSwapChn  uint32
	sharedAudioBuf  uint32
	dupDSBuffer     uint32

	Resources      map[uint32]*BufferDesc
	RenderStates   map[uint32]uint32
	BoundTextures  [16]uint32
	StreamSources  [16]uint32
	StreamOffsets  [16]uint32
	StreamStrides  [16]uint32
	BoundIndices   uint32
	VSConstants    [256][4]float32
	PSConstants    [256][4]float32

	Framebuffer    *image.RGBA
	ZBuffer        []float32
	FramesDir      string
	DisplayURL     string
	FrameDrawCalls uint64
	DIPCount       uint64
	FrameMu        sync.Mutex
	LatestPNG      []byte

	KeyMu        sync.Mutex
	KeyHoldUntil [256]uint64
	AutoKeysMode string

	FrameCount     uint64
	DrawCallCount  uint64
	TriangleCount  uint64
	ClearCount     uint64
	LastClearColor uint32
	TraceLog       bool
}

func NewSubsystem(c *cpu.CPU) *Subsystem {
	autoKeys := os.Getenv("LSW_AUTO_KEYS")
	if autoKeys == "" {
		autoKeys = "boot"
	}
	s := &Subsystem{
		VM:             c,
		nextVTableAddr: VTableArenaBase,
		nextTrampAddr:  TrampArenaBase,
		Resources:      make(map[uint32]*BufferDesc),
		RenderStates:   make(map[uint32]uint32),
		AutoKeysMode:   autoKeys,
		TraceLog:       true,
	}

	s.vtD3D9 = s.buildVTable(c, "IDirect3D9", 32, s.handleIDirect3D9)
	s.vtDevice9 = s.buildVTable(c, "IDirect3DDevice9", 128, s.handleIDirect3DDevice9)
	s.vtTexture9 = s.buildVTable(c, "IDirect3DTexture9", 32, s.handleIDirect3DTexture9)
	s.vtCubeTex9 = s.buildVTable(c, "IDirect3DCubeTexture9", 32, s.handleIDirect3DCubeTexture9)
	s.vtVolTex9 = s.buildVTable(c, "IDirect3DVolumeTexture9", 32, s.handleIDirect3DVolumeTexture9)
	s.vtSurface9 = s.buildVTable(c, "IDirect3DSurface9", 32, s.handleIDirect3DSurface9)
	s.vtVertexBuf9 = s.buildVTable(c, "IDirect3DVertexBuffer9", 24, s.handleIDirect3DBuffer9)
	s.vtIndexBuf9 = s.buildVTable(c, "IDirect3DIndexBuffer9", 24, s.handleIDirect3DBuffer9)
	s.vtVertexDecl = s.buildVTable(c, "IDirect3DVertexDeclaration9", 16, s.handleSimpleCOM)
	s.vtVertexShdr = s.buildVTable(c, "IDirect3DVertexShader9", 16, s.handleSimpleCOM)
	s.vtPixelShdr = s.buildVTable(c, "IDirect3DPixelShader9", 16, s.handleSimpleCOM)
	s.vtStateBlock = s.buildVTable(c, "IDirect3DStateBlock9", 16, s.handleSimpleCOM)
	s.vtQuery9 = s.buildVTable(c, "IDirect3DQuery9", 16, s.handleIDirect3DQuery9)
	s.vtSwapChain9 = s.buildVTable(c, "IDirect3DSwapChain9", 24, s.handleIDirect3DSwapChain9)
	s.vtD3DXBuffer = s.buildVTable(c, "ID3DXBuffer", 16, s.handleID3DXBuffer)
	s.vtDInput8 = s.buildVTable(c, "IDirectInput8A", 20, s.handleIDirectInput8)
	s.vtDIDevice8 = s.buildVTable(c, "IDirectInputDevice8A", 40, s.handleIDirectInputDevice8)
	s.vtDSound8 = s.buildVTable(c, "IDirectSound8", 24, s.handleIDirectSound8)
	s.vtDSBuffer8 = s.buildVTable(c, "IDirectSoundBuffer8", 32, s.handleIDirectSoundBuffer8)

	// Allocate default backbuffer & depth-stencil surface
	s.defaultBackBuf = s.allocSurface(c, 1280, 720, 22)
	s.defaultDepthBuf = s.allocSurface(c, 1280, 720, 75)
	s.defaultSwapChn = s.allocCOMObject(c, s.vtSwapChain9, 32)
	framesDir := os.Getenv("LSW_FRAMES_DIR")
	if framesDir == "" {
		framesDir = "/workspace/lsw64/frames"
	}
	s.initRasterizer(framesDir)

	return s
}

func (s *Subsystem) buildVTable(c *cpu.CPU, ifaceName string, slots int, handler func(c *cpu.CPU, winSlot int)) uint32 {
	vtAddr := s.nextVTableAddr
	s.nextVTableAddr += uint32(slots * 4)

	for i := 0; i < slots; i++ {
		tramp := s.nextTrampAddr
		s.nextTrampAddr += 4
		c.Write32(vtAddr+uint32(i*4), tramp)

		slotIdx := i
		tag := fmt.Sprintf("%s::slot_%d", ifaceName, slotIdx)
		c.RegisterIntercept(tramp, tag, func(vm *cpu.CPU) {
			// Itanium C++ ABI COM layout:
			// slot 0 = ~IUnknown D1
			// slot 1 = ~IUnknown D0
			// slot 2 = AddRef
			// slot 3 = Release
			// slot 4 = QueryInterface
			// slot >= 5 -> Windows COM slot = slot - 2
			switch slotIdx {
			case 0, 1: // Destructors
				vm.Regs[cpu.RegEAX] = vm.Arg(0)
				vm.Ret()
			case 2: // AddRef
				vm.Regs[cpu.RegEAX] = 2
				vm.Ret()
			case 3: // Release
				vm.Regs[cpu.RegEAX] = 1
				vm.Ret()
			case 4: // QueryInterface(this, riid, ppvObj)
				ppv := vm.Arg(2)
				if ppv != 0 {
					vm.Write32(ppv, vm.Arg(0))
				}
				vm.Regs[cpu.RegEAX] = 0 // S_OK
				vm.Ret()
			default:
				handler(vm, slotIdx-2)
			}
		})
	}
	return vtAddr
}

func (s *Subsystem) allocCOMObject(c *cpu.CPU, vtAddr uint32, size uint32) uint32 {
	if size < 16 {
		size = 16
	}
	obj := c.Calloc(1, size)
	c.Write32(obj, vtAddr)
	return obj
}

func (s *Subsystem) allocSurface(c *cpu.CPU, width, height, format uint32) uint32 {
	if width == 0 {
		width = 64
	}
	if height == 0 {
		height = 64
	}
	obj := s.allocCOMObject(c, s.vtSurface9, 32)
	pitch := width * 4
	bufSize := pitch * height
	if bufSize > 4*1024*1024 {
		bufSize = 4 * 1024 * 1024
	}
	// Guest backing store is allocated lazily (guestBuf on LockRect): GetSurfaceLevel is called every frame and
	// eagerly reserving a 3.6MB buffer each time exhausted the guest heap after ~2800 frames.
	s.Resources[obj] = &BufferDesc{
		Length: bufSize,
		Format: format,
		Width:  width,
		Height: height,
		Pitch:  pitch,
	}
	return obj
}

func (s *Subsystem) allocTexture(c *cpu.CPU, width, height, format uint32) uint32 {
	return s.allocTextureLazy(c, width, height, format)
}

// allocTextureLazy creates a texture object whose pixels live host-side; no guest memory is reserved until
// the game locks it (see guestBuf). Loading hundreds of 1024x1024 textures would otherwise exhaust the guest heap.
func (s *Subsystem) allocTextureLazy(c *cpu.CPU, width, height, format uint32) uint32 {
	if width == 0 {
		width = 64
	}
	if height == 0 {
		height = 64
	}
	obj := s.allocCOMObject(c, s.vtTexture9, 32)
	pitch := width * 4
	bufSize := pitch * height
	if bufSize > 4*1024*1024 {
		bufSize = 4 * 1024 * 1024
	}
	s.Resources[obj] = &BufferDesc{Length: bufSize, Format: format, Width: width, Height: height, Pitch: pitch}
	return obj
}

// guestBuf returns the resource's guest-memory backing store, allocating it on first use.
func (s *Subsystem) guestBuf(c *cpu.CPU, res *BufferDesc) uint32 {
	if res.DataPtr == 0 && res.Length > 0 {
		res.DataPtr = c.Malloc(res.Length)
	}
	return res.DataPtr
}

func (s *Subsystem) RegisterIndirectXThunks(c *cpu.CPU) {
	c.RegisterIntercept(0x006479f0, "Direct3DCreate9", s.thunkDirect3DCreate9)
	c.RegisterIntercept(0x00648650, "D3DPERF_BeginEvent", s.thunkReturnZero)
	c.RegisterIntercept(0x00648690, "D3DPERF_EndEvent", s.thunkReturnZero)

	c.RegisterIntercept(0x00690520, "D3DXCreateTextureFromFileExA", s.thunkD3DXCreateTextureFromFileExA)
	c.RegisterIntercept(0x006910d0, "D3DXCreateTextureFromFileInMemory", s.thunkD3DXCreateTextureFromFileInMemory)
	c.RegisterIntercept(0x00691ef0, "D3DXAssembleShader", s.thunkD3DXAssembleShader)
	c.RegisterIntercept(0x006924e0, "D3DXCompileShader", s.thunkD3DXCompileShader)
	// Note: 0x0068f570 (D3DXMatrixMultiply) and 0x0068f690 (D3DXMatrixPerspectiveLH) are pure SSE math
	// functions in __TEXT,__text; we can either let them run natively on our SSE engine or implement them directly:
	delete(c.Intercepts, 0x0068f570)
	delete(c.Intercepts, 0x0068f690)

	c.RegisterIntercept(0x006ab2f0, "DirectInput8Create", s.thunkDirectInput8Create)
	c.RegisterIntercept(0x006b5670, "DirectSoundCreate8", s.thunkDirectSoundCreate8)
	c.RegisterIntercept(0x006b9450, "XInputGetState", s.thunkXInputGetState)
	c.RegisterIntercept(0x006b9a40, "XInputGetCapabilities", s.thunkXInputGetCapabilities)

	// Bink Video shims
	c.RegisterIntercept(0x008bd050, "BinkOpen", s.thunkReturnZero)
	c.RegisterIntercept(0x008bd640, "BinkClose", s.thunkReturnZero)
	c.RegisterIntercept(0x008bd740, "BinkDoFrame", s.thunkReturnZero)
	c.RegisterIntercept(0x008bd810, "BinkNextFrame", s.thunkReturnZero)
	c.RegisterIntercept(0x008bd850, "BinkWait", s.thunkReturnZero)
	c.RegisterIntercept(0x008bd8e0, "BinkCopyToBuffer", s.thunkReturnZero)
	c.RegisterIntercept(0x008be3d0, "BinkSetSoundTrack", s.thunkReturnZero)

	// Skip fullscreen bloom/blur post-processing pass over software framebuffer
	c.RegisterIntercept(0x0001d900, "NuPostProcessRender", s.thunkReturnZero)
	// Populate NuShader constant & sampler descriptors so NuShaderBindMaterial (0x001538f0) binds textures & material colors
	c.RegisterIntercept(0x001535c0, "NuShaderParseConstantTable", s.thunkNuShaderParseConstantTable)
}

func (s *Subsystem) thunkNuShaderParseConstantTable(c *cpu.CPU) {
	shader := c.Arg(0)
	tableBase := c.Read32(0x00b2842c)
	if tableBase != 0 {
		// Enable global caching (entry+0x18 = 1) for matrix & camera parameters (17..22, 56, 85)
		// so NuShaderSetConstant (0x00232240) always writes live matrices into entry+0x1c.
		for _, pid := range []uint32{17, 18, 19, 20, 21, 22, 56, 85} {
			c.Write32(tableBase+pid*92+0x18, 1)
		}
	}
	if shader != 0 {
		// Node 36: layer0_sampler -> sampler stage 0
		node36 := shader + 0x1c + 36*12
		c.Write16(node36+0x00, 0)
		c.Write32(node36+0x04, 0x24000000|(36<<16))
		// Node 51: layer0_diffuse -> PS constant register 112
		node51 := shader + 0x1c + 51*12
		c.Write16(node51+0x00, 112)
		c.Write32(node51+0x04, 0x22000101|(51<<16))
		c.Write32(node51+0x08, 0)
		c.Write32(node36+0x08, node51)
		c.Write32(shader+0x10, node36)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) thunkReturnZero(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) thunkDirect3DCreate9(c *cpu.CPU) {
	sdkVer := c.Arg(0)
	if s.d3d9Obj == 0 {
		s.d3d9Obj = s.allocCOMObject(c, s.vtD3D9, 32)
	}
	if s.TraceLog {
		fmt.Printf("[D3D9] Direct3DCreate9(SDKVersion=0x%x) -> 0x%08x\n", sdkVer, s.d3d9Obj)
	}
	c.Regs[cpu.RegEAX] = s.d3d9Obj
	c.Ret()
}

func (s *Subsystem) fillD3DCaps9(c *cpu.CPU, ptr uint32) {
	if ptr == 0 {
		return
	}
	clear(c.Mem[ptr : ptr+0x130])
	c.Write32(ptr+0x00, 1)          // DeviceType = D3DDEVTYPE_HAL
	c.Write32(ptr+0x04, 0)          // AdapterOrdinal = 0
	c.Write32(ptr+0x0c, 0x00020000) // Caps2
	c.Write32(ptr+0x10, 0x00000020) // Caps3
	c.Write32(ptr+0x14, 0x8000000F) // PresentationIntervals
	c.Write32(ptr+0x1c, 0x00091000) // DevCaps
	c.Write32(ptr+0x20, 0x00020282) // PrimitiveMiscCaps
	c.Write32(ptr+0x24, 0x07732191) // RasterCaps
	c.Write32(ptr+0x28, 0x000000FF) // ZCmpCaps
	c.Write32(ptr+0x2c, 0x000007FF) // SrcBlendCaps
	c.Write32(ptr+0x30, 0x000007FF) // DestBlendCaps
	c.Write32(ptr+0x34, 0x000000FF) // AlphaCmpCaps
	c.Write32(ptr+0x3c, 0x0001F805) // TextureCaps
	c.Write32(ptr+0x40, 0x1F031F00) // TextureFilterCaps
	c.Write32(ptr+0x44, 0x1F031F00) // CubeTextureFilterCaps
	c.Write32(ptr+0x48, 0x1F031F00) // VolumeTextureFilterCaps
	c.Write32(ptr+0x4c, 0x0000003F) // TextureAddressCaps
	c.Write32(ptr+0x50, 0x0000003F) // VolumeTextureAddressCaps
	c.Write32(ptr+0x58, 4096)       // MaxTextureWidth
	c.Write32(ptr+0x5c, 4096)       // MaxTextureHeight
	c.Write32(ptr+0x60, 2048)       // MaxVolumeExtent
	c.Write32(ptr+0x64, 8192)       // MaxTextureRepeat
	c.Write32(ptr+0x68, 8192)       // MaxTextureAspectRatio
	c.Write32(ptr+0x6c, 16)         // MaxAnisotropy
	c.WriteF32(ptr+0x70, 1e10)      // MaxVertexW
	c.WriteF32(ptr+0x84, 256.0)     // MaxPointSize
	c.Write32(ptr+0x88, 0x000001FF) // StencilCaps
	c.Write32(ptr+0x8c, 8)          // MaxTextureBlendStages
	c.Write32(ptr+0x90, 0x0308BBFF) // TextureOpCaps
	c.Write32(ptr+0x94, 8)          // MaxTextureBlendStages
	c.Write32(ptr+0x98, 8)          // MaxSimultaneousTextures (8 stages!)
	c.Write32(ptr+0xa4, 8)          // MaxActiveLights
	c.Write32(ptr+0xa8, 4)          // MaxUserClipPlanes
	c.Write32(ptr+0xb4, 0x00007FFF) // MaxPrimitiveCount
	c.Write32(ptr+0xb8, 0x00007FFF) // MaxVertexIndex
	c.Write32(ptr+0xbc, 16)         // MaxStreams
	c.Write32(ptr+0xc0, 0x00007FFF) // MaxStreamStride
	c.Write32(ptr+0xc4, 0xFFFE0300) // VertexShaderVersion (vs_3_0)
	c.Write32(ptr+0xc8, 256)        // MaxVertexShaderConst
	c.Write32(ptr+0xcc, 0xFFFF0300) // PixelShaderVersion (ps_3_0)
	c.WriteF32(ptr+0xd0, 65504.0)   // PixelShader1xMaxValue
	c.Write32(ptr+0xd4, 0x00000051) // DevCaps2
	c.Write32(ptr+0xec, 0x0000033F) // DeclTypes
	c.Write32(ptr+0xf0, 4)          // NumSimultaneousRTs
	c.Write32(ptr+0xf4, 0x03000300) // StretchRectFilterCaps
	c.Write32(ptr+0x118, 0x00000080)
	c.Write32(ptr+0x128, 32768)
	c.Write32(ptr+0x12c, 32768)
}

func (s *Subsystem) handleIDirect3D9(c *cpu.CPU, winSlot int) {
	switch winSlot {
	case 4: // GetAdapterCount
		c.Regs[cpu.RegEAX] = 1
	case 5: // GetAdapterIdentifier(this, Adapter, Flags, pIdentifier)
		pIdent := c.Arg(3)
		if pIdent != 0 {
			clear(c.Mem[pIdent : pIdent+1100])
			c.WriteCString(pIdent+0x000, "IndirectX")
			c.WriteCString(pIdent+0x200, "NVIDIA GeForce GTX (lsw64)")
			c.WriteCString(pIdent+0x400, "\\\\.\\DISPLAY1")
			c.Write32(pIdent+0x428, 0x10DE) // VendorId (NVIDIA)
			c.Write32(pIdent+0x42C, 0x0640) // DeviceId
			c.Write32(pIdent+0x430, 0x0001) // SubSysId
			c.Write32(pIdent+0x434, 0x00A1) // Revision
		}
		c.Regs[cpu.RegEAX] = 0
	case 6: // GetAdapterModeCount
		c.Regs[cpu.RegEAX] = 4
	case 7: // EnumAdapterModes(this, Adapter, Format, Mode, pMode)
		modeIdx := c.Arg(3)
		pMode := c.Arg(4)
		widths := [4]uint32{800, 1024, 1280, 1920}
		heights := [4]uint32{600, 768, 720, 1080}
		if pMode != 0 {
			idx := modeIdx & 3
			c.Write32(pMode+0, widths[idx])
			c.Write32(pMode+4, heights[idx])
			c.Write32(pMode+8, 60)
			c.Write32(pMode+12, 22) // D3DFMT_X8R8G8B8
		}
		c.Regs[cpu.RegEAX] = 0
	case 8: // GetAdapterDisplayMode(this, Adapter, pMode)
		pMode := c.Arg(2)
		if pMode != 0 {
			c.Write32(pMode+0, 1280)
			c.Write32(pMode+4, 720)
			c.Write32(pMode+8, 60)
			c.Write32(pMode+12, 22) // D3DFMT_X8R8G8B8
		}
		c.Regs[cpu.RegEAX] = 0
	case 9, 10, 11, 12, 13: // CheckDeviceType, CheckDeviceFormat, CheckDeviceMultiSampleType, CheckDepthStencilMatch, CheckDeviceFormatConversion
		c.Regs[cpu.RegEAX] = 0
	case 14: // GetDeviceCaps(this, Adapter, DeviceType, pCaps)
		s.fillD3DCaps9(c, c.Arg(3))
		c.Regs[cpu.RegEAX] = 0
	case 15: // GetAdapterMonitor
		c.Regs[cpu.RegEAX] = 1
	case 16: // CreateDevice(this, Adapter, DeviceType, hFocusWindow, BehaviorFlags, pPresentationParameters, ppReturnedDeviceInterface)
		ppDev := c.Arg(6)
		if s.device9Obj == 0 {
			s.device9Obj = s.allocCOMObject(c, s.vtDevice9, 64)
		}
		if ppDev != 0 {
			c.Write32(ppDev, s.device9Obj)
		}
		if s.TraceLog {
			fmt.Printf("[D3D9] IDirect3D9::CreateDevice() -> 0x%08x\n", s.device9Obj)
		}
		c.Regs[cpu.RegEAX] = 0
	default:
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (s *Subsystem) handleIDirect3DDevice9(c *cpu.CPU, winSlot int) {
	switch winSlot {
	case 3: // TestCooperativeLevel
		c.Regs[cpu.RegEAX] = 0
	case 4: // GetAvailableTextureMem
		c.Regs[cpu.RegEAX] = 512 * 1024 * 1024
	case 6: // GetDirect3D
		pp := c.Arg(1)
		if pp != 0 {
			c.Write32(pp, s.d3d9Obj)
		}
		c.Regs[cpu.RegEAX] = 0
	case 7: // GetDeviceCaps
		s.fillD3DCaps9(c, c.Arg(1))
		c.Regs[cpu.RegEAX] = 0
	case 8: // GetDisplayMode
		pMode := c.Arg(2)
		if pMode != 0 {
			c.Write32(pMode+0, 1280)
			c.Write32(pMode+4, 720)
			c.Write32(pMode+8, 60)
			c.Write32(pMode+12, 22)
		}
		c.Regs[cpu.RegEAX] = 0
	case 13, 14: // CreateAdditionalSwapChain / GetSwapChain
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.defaultSwapChn)
		}
		c.Regs[cpu.RegEAX] = 0
	case 15: // GetNumberOfSwapChains
		c.Regs[cpu.RegEAX] = 1
	case 16: // Reset
		c.Regs[cpu.RegEAX] = 0
	case 17: // Present
		s.FrameCount++
		if s.rendering {
			s.exportFramePNG()
		}
		s.rendering = s.wantRender(s.FrameCount + 1)
		if s.TraceLog && (s.FrameCount <= 10 || s.FrameCount%30 == 0) {
			fmt.Printf("[D3D9] Present() Frame #%d completed (DrawCalls=%d, Triangles=%d, Clears=%d, LastClearColor=0x%08x)\n",
				s.FrameCount, s.DrawCallCount, s.TriangleCount, s.ClearCount, s.LastClearColor)
		}
		s.FrameDrawCalls = 0
		c.Regs[cpu.RegEAX] = 0
	case 18: // GetBackBuffer(this, iSwapChain, iBackBuffer, Type, ppBackBuffer)
		pp := c.Arg(4)
		if pp != 0 {
			c.Write32(pp, s.defaultBackBuf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 23: // CreateTexture(this, Width, Height, Levels, Usage, Format, Pool, ppTexture, pSharedHandle)
		w := c.Arg(1)
		h := c.Arg(2)
		fmtCode := c.Arg(5)
		pp := c.Arg(7)
		tex := s.allocTexture(c, w, h, fmtCode)
		if pp != 0 {
			c.Write32(pp, tex)
		}
		c.Regs[cpu.RegEAX] = 0
	case 24: // CreateVolumeTexture(this, W, H, D, Levels, Usage, Format, Pool, ppVolTex, pShared)
		w := c.Arg(1)
		h := c.Arg(2)
		fmtCode := c.Arg(6)
		pp := c.Arg(8)
		tex := s.allocCOMObject(c, s.vtVolTex9, 32)
		buf := c.Malloc(65536)
		s.Resources[tex] = &BufferDesc{DataPtr: buf, Length: 65536, Format: fmtCode, Width: w, Height: h, Pitch: w * 4}
		if pp != 0 {
			c.Write32(pp, tex)
		}
		c.Regs[cpu.RegEAX] = 0
	case 25: // CreateCubeTexture(this, EdgeLength, Levels, Usage, Format, Pool, ppCubeTex, pShared)
		edge := c.Arg(1)
		fmtCode := c.Arg(4)
		pp := c.Arg(6)
		tex := s.allocCOMObject(c, s.vtCubeTex9, 32)
		buf := c.Malloc(65536)
		s.Resources[tex] = &BufferDesc{DataPtr: buf, Length: 65536, Format: fmtCode, Width: edge, Height: edge, Pitch: edge * 4}
		if pp != 0 {
			c.Write32(pp, tex)
		}
		c.Regs[cpu.RegEAX] = 0
	case 26: // CreateVertexBuffer(this, Length, Usage, FVF, Pool, ppVertexBuffer, pSharedHandle)
		length := c.Arg(1)
		pp := c.Arg(5)
		if length == 0 {
			length = 4096
		}
		vb := s.allocCOMObject(c, s.vtVertexBuf9, 32)
		dataPtr := c.Malloc(length)
		s.Resources[vb] = &BufferDesc{DataPtr: dataPtr, Length: length}
		if pp != 0 {
			c.Write32(pp, vb)
		}
		c.Regs[cpu.RegEAX] = 0
	case 27: // CreateIndexBuffer(this, Length, Usage, Format, Pool, ppIndexBuffer, pSharedHandle)
		length := c.Arg(1)
		fmtCode := c.Arg(3)
		pp := c.Arg(5)
		if length == 0 {
			length = 4096
		}
		ib := s.allocCOMObject(c, s.vtIndexBuf9, 32)
		dataPtr := c.Malloc(length)
		s.Resources[ib] = &BufferDesc{DataPtr: dataPtr, Length: length, Format: fmtCode}
		if pp != 0 {
			c.Write32(pp, ib)
		}
		c.Regs[cpu.RegEAX] = 0
	case 28: // CreateRenderTarget(this, Width, Height, Format, MultiSample, MultisampleQuality, Lockable, ppSurface, pSharedHandle)
		w := c.Arg(1)
		h := c.Arg(2)
		fmtCode := c.Arg(3)
		pp := c.Arg(7)
		surf := s.allocSurface(c, w, h, fmtCode)
		if pp != 0 {
			c.Write32(pp, surf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 29: // CreateDepthStencilSurface(this, Width, Height, Format, MultiSample, MultisampleQuality, Discard, ppSurface, pSharedHandle)
		w := c.Arg(1)
		h := c.Arg(2)
		fmtCode := c.Arg(3)
		pp := c.Arg(7)
		surf := s.allocSurface(c, w, h, fmtCode)
		if pp != 0 {
			c.Write32(pp, surf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 36: // CreateOffscreenPlainSurface(this, Width, Height, Format, Pool, ppSurface, pSharedHandle)
		w := c.Arg(1)
		h := c.Arg(2)
		fmtCode := c.Arg(3)
		pp := c.Arg(5)
		surf := s.allocSurface(c, w, h, fmtCode)
		if pp != 0 {
			c.Write32(pp, surf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 38: // GetRenderTarget(this, RenderTargetIndex, ppRenderTarget)
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.defaultBackBuf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 40: // GetDepthStencilSurface(this, ppZStencilSurface)
		pp := c.Arg(1)
		if pp != 0 {
			c.Write32(pp, s.defaultDepthBuf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 43: // Clear(this, Count, pRects, Flags, Color, Z, Stencil)
		s.ClearCount++
		flags := c.Arg(3)
		col := c.Arg(4)
		s.LastClearColor = col
		if s.rendering && (flags&1) != 0 && s.FrameDrawCalls == 0 {
			s.clearFramebuffer(col)
		}
		if s.rendering && (flags&2) != 0 {
			s.clearZBuffer(1.0)
		}
		c.Regs[cpu.RegEAX] = 0
	case 48: // GetViewport(this, pViewport)
		vp := c.Arg(1)
		if vp != 0 {
			c.Write32(vp+0, 0)
			c.Write32(vp+4, 0)
			c.Write32(vp+8, 1280)
			c.Write32(vp+12, 720)
			c.WriteF32(vp+16, 0.0)
			c.WriteF32(vp+20, 1.0)
		}
		c.Regs[cpu.RegEAX] = 0
	case 57: // SetRenderState
		s.RenderStates[c.Arg(1)] = c.Arg(2)
		c.Regs[cpu.RegEAX] = 0
	case 58: // GetRenderState
		pVal := c.Arg(2)
		if pVal != 0 {
			c.Write32(pVal, s.RenderStates[c.Arg(1)])
		}
		c.Regs[cpu.RegEAX] = 0
	case 59: // CreateStateBlock(this, Type, ppSB)
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.allocCOMObject(c, s.vtStateBlock, 16))
		}
		c.Regs[cpu.RegEAX] = 0
	case 61: // EndStateBlock(this, ppSB)
		pp := c.Arg(1)
		if pp != 0 {
			c.Write32(pp, s.allocCOMObject(c, s.vtStateBlock, 16))
		}
		c.Regs[cpu.RegEAX] = 0
	case 65: // SetTexture(this, Stage, pTexture)
		stage := c.Arg(1)
		if stage < 16 {
			s.BoundTextures[stage] = c.Arg(2)
		}
		c.Regs[cpu.RegEAX] = 0
	case 66: // GetTexture(this, Stage, ppTexture)
		stage := c.Arg(1)
		pp := c.Arg(2)
		if pp != 0 && stage < 16 {
			c.Write32(pp, s.BoundTextures[stage])
		}
		c.Regs[cpu.RegEAX] = 0
	case 81: // DrawPrimitive(this, PrimitiveType, StartVertex, PrimitiveCount)
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.TriangleCount += uint64(c.Arg(3))
		if vbRes := s.Resources[s.StreamSources[0]]; vbRes != nil {
			stride := s.StreamStrides[0]
			pVerts := vbRes.DataPtr + s.StreamOffsets[0] + c.Arg(2)*stride
			s.rasterizePrimitiveBatch(c, c.Arg(1), c.Arg(3), pVerts, stride, false)
		}
		c.Regs[cpu.RegEAX] = 0
	case 82: // DrawIndexedPrimitive(this, Type, BaseVertexIndex, MinVertexIndex, NumVertices, startIndex, primCount)
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.DIPCount++
		s.TriangleCount += uint64(c.Arg(6))
		vbRes := s.Resources[s.StreamSources[0]]
		ibRes := s.Resources[s.BoundIndices]
		stride := s.StreamStrides[0]
		numV := c.Arg(4)
		prims := c.Arg(6)
		// Skip the 5x5 fullscreen post-process grid from 0x0001d900 so it doesn't overwrite the software framebuffer
		isPostProcessGrid := (numV == 25 && prims == 44 && stride == 28)
		if vbRes != nil && ibRes != nil && !isPostProcessGrid {
			pVerts := vbRes.DataPtr + s.StreamOffsets[0]
			is32 := ibRes.Format == 102 // D3DFMT_INDEX32
			s.rasterizeIndexedBatch(c, c.Arg(1), int32(c.Arg(2)), c.Arg(5), prims, pVerts, stride, ibRes.DataPtr, ibRes.Length, is32, false)
		}
		if s.TraceLog && s.DIPCount <= 15 {
			fmt.Printf("[D3D9-DIP #%d] frame=%d type=%d baseV=%d numV=%d startIdx=%d prims=%d stride=%d tex0=0x%x post=%v\n",
				s.DIPCount, s.FrameCount, c.Arg(1), int32(c.Arg(2)), numV, c.Arg(5), prims, stride, s.BoundTextures[0], isPostProcessGrid)
		}
		c.Regs[cpu.RegEAX] = 0
	case 83: // DrawPrimitiveUP(this, PrimitiveType, PrimitiveCount, pVertexStreamZeroData, VertexStreamZeroStride)
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.TriangleCount += uint64(c.Arg(2))
		s.rasterizePrimitiveBatch(c, c.Arg(1), c.Arg(2), c.Arg(3), c.Arg(4), true)
		c.Regs[cpu.RegEAX] = 0
	case 84: // DrawIndexedPrimitiveUP(this, PrimitiveType, MinVertexIndex, NumVertices, PrimitiveCount, pIndexData, IndexDataFormat, pVertexStreamZeroData, VertexStreamZeroStride)
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.TriangleCount += uint64(c.Arg(4))
		is32 := c.Arg(6) == 102
		s.rasterizeIndexedBatch(c, c.Arg(1), 0, 0, c.Arg(4), c.Arg(7), c.Arg(8), c.Arg(5), 65536, is32, true)
		c.Regs[cpu.RegEAX] = 0
	case 86: // CreateVertexDeclaration(this, pVertexElements, ppDecl)
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.allocCOMObject(c, s.vtVertexDecl, 16))
		}
		c.Regs[cpu.RegEAX] = 0
	case 91: // CreateVertexShader(this, pFunction, ppShader)
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.allocCOMObject(c, s.vtVertexShdr, 16))
		}
		c.Regs[cpu.RegEAX] = 0
	case 94: // SetVertexShaderConstantF(this, StartRegister, pConstantData, Vector4fCount)
		startReg := int(c.Arg(1))
		pData := c.Arg(2)
		vecCount := int(c.Arg(3))
		if pData != 0 {
			for i := 0; i < vecCount && startReg+i < 256; i++ {
				for j := 0; j < 4; j++ {
					s.VSConstants[startReg+i][j] = c.ReadF32(pData + uint32((i*4+j)*4))
				}
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 100: // SetStreamSource(this, StreamNumber, pStreamData, OffsetInBytes, Stride)
		st := c.Arg(1)
		if st < 16 {
			s.StreamSources[st] = c.Arg(2)
			s.StreamOffsets[st] = c.Arg(3)
			s.StreamStrides[st] = c.Arg(4)
		}
		c.Regs[cpu.RegEAX] = 0
	case 102, 104: // SetIndices(this, pIndexData)
		s.BoundIndices = c.Arg(1)
		c.Regs[cpu.RegEAX] = 0
	case 106: // CreatePixelShader(this, pFunction, ppShader)
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.allocCOMObject(c, s.vtPixelShdr, 16))
		}
		c.Regs[cpu.RegEAX] = 0
	case 109: // SetPixelShaderConstantF
		startReg := int(c.Arg(1))
		pData := c.Arg(2)
		vecCount := int(c.Arg(3))
		if pData != 0 {
			for i := 0; i < vecCount && startReg+i < 256; i++ {
				for j := 0; j < 4; j++ {
					s.PSConstants[startReg+i][j] = c.ReadF32(pData + uint32((i*4+j)*4))
				}
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 118: // CreateQuery(this, Type, ppQuery)
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.allocCOMObject(c, s.vtQuery9, 16))
		}
		c.Regs[cpu.RegEAX] = 0
	default:
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (s *Subsystem) fillSurfaceDesc(c *cpu.CPU, obj uint32, pDesc uint32) {
	if pDesc == 0 {
		return
	}
	res := s.Resources[obj]
	w, h, fmtCode := uint32(64), uint32(64), uint32(21)
	if res != nil {
		w, h, fmtCode = res.Width, res.Height, res.Format
	}
	c.Write32(pDesc+0, fmtCode)
	c.Write32(pDesc+4, 1) // Type = D3DRTYPE_SURFACE
	c.Write32(pDesc+8, 0) // Usage
	c.Write32(pDesc+12, 1) // Pool = D3DPOOL_MANAGED
	c.Write32(pDesc+16, 0) // MultiSampleType
	c.Write32(pDesc+20, 0) // MultiSampleQuality
	c.Write32(pDesc+24, w)
	c.Write32(pDesc+28, h)
}

func (s *Subsystem) handleIDirect3DTexture9(c *cpu.CPU, winSlot int) {
	thisPtr := c.Arg(0)
	switch winSlot {
	case 13, 14: // GetLevelCount
		c.Regs[cpu.RegEAX] = 1
		c.Ret()
		return
	case 17: // GetLevelDesc(this, Level, pDesc)
		s.fillSurfaceDesc(c, thisPtr, c.Arg(2))
	case 18: // GetSurfaceLevel(this, Level, ppSurfaceLevel)
		lvl := c.Arg(1)
		pp := c.Arg(2)
		res := s.Resources[thisPtr]
		w, h, fmtCode := uint32(64), uint32(64), uint32(21)
		if res != nil {
			w, h, fmtCode = res.Width, res.Height, res.Format
		}
		surf := s.allocSurface(c, w, h, fmtCode)
		if lvl == 0 && res != nil {
			s.Resources[surf] = res
		}
		if pp != 0 {
			c.Write32(pp, surf)
		}
	case 19: // LockRect(this, Level, pLockedRect, pRect, Flags)
		pLock := c.Arg(2)
		res := s.Resources[thisPtr]
		if pLock != 0 && res != nil {
			c.Write32(pLock+0, res.Pitch)
			c.Write32(pLock+4, s.guestBuf(c, res))
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirect3DCubeTexture9(c *cpu.CPU, winSlot int) {
	thisPtr := c.Arg(0)
	switch winSlot {
	case 13, 14: // GetLevelCount
		c.Regs[cpu.RegEAX] = 1
		c.Ret()
		return
	case 17: // GetLevelDesc(this, Level, pDesc)
		s.fillSurfaceDesc(c, thisPtr, c.Arg(2))
	case 18: // GetCubeMapSurface(this, FaceType, Level, ppCubeMapSurface)
		pp := c.Arg(3)
		if pp != 0 {
			c.Write32(pp, s.allocSurface(c, 64, 64, 21))
		}
	case 19: // LockRect(this, FaceType, Level, pLockedRect, pRect, Flags)
		pLock := c.Arg(3)
		res := s.Resources[thisPtr]
		if pLock != 0 && res != nil {
			c.Write32(pLock+0, res.Pitch)
			c.Write32(pLock+4, s.guestBuf(c, res))
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirect3DVolumeTexture9(c *cpu.CPU, winSlot int) {
	thisPtr := c.Arg(0)
	switch winSlot {
	case 13, 14: // GetLevelCount
		c.Regs[cpu.RegEAX] = 1
		c.Ret()
		return
	case 19: // LockBox(this, Level, pLockedVolume, pBox, Flags)
		pLock := c.Arg(2)
		res := s.Resources[thisPtr]
		if pLock != 0 && res != nil {
			c.Write32(pLock+0, res.Pitch)
			c.Write32(pLock+4, res.Pitch*res.Height)
			c.Write32(pLock+8, s.guestBuf(c, res))
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirect3DSurface9(c *cpu.CPU, winSlot int) {
	thisPtr := c.Arg(0)
	switch winSlot {
	case 12: // GetDesc(this, pDesc)
		s.fillSurfaceDesc(c, thisPtr, c.Arg(1))
	case 13: // LockRect(this, pLockedRect, pRect, Flags)
		pLock := c.Arg(1)
		res := s.Resources[thisPtr]
		if pLock != 0 && res != nil {
			c.Write32(pLock+0, res.Pitch)
			c.Write32(pLock+4, s.guestBuf(c, res))
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirect3DBuffer9(c *cpu.CPU, winSlot int) {
	thisPtr := c.Arg(0)
	switch winSlot {
	case 11: // Lock(this, OffsetToLock, SizeToLock, ppbData, Flags)
		offset := c.Arg(1)
		ppbData := c.Arg(3)
		res := s.Resources[thisPtr]
		if ppbData != 0 && res != nil {
			if offset >= res.Length {
				offset = 0
			}
			c.Write32(ppbData, res.DataPtr+offset)
		}
	case 13: // GetDesc(this, pDesc)
		pDesc := c.Arg(1)
		res := s.Resources[thisPtr]
		if pDesc != 0 && res != nil {
			c.Write32(pDesc+0, res.Format)
			c.Write32(pDesc+4, 6) // D3DRTYPE_VERTEXBUFFER
			c.Write32(pDesc+8, 0)
			c.Write32(pDesc+12, 1)
			c.Write32(pDesc+16, res.Length)
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirect3DQuery9(c *cpu.CPU, winSlot int) {
	if winSlot == 7 { // GetData(this, pData, dwSize, dwGetDataFlags)
		pData := c.Arg(1)
		dwSize := c.Arg(2)
		if pData != 0 && dwSize >= 4 {
			c.Write32(pData, 1)
		}
		c.Regs[cpu.RegEAX] = 0 // S_OK (ready immediately)
		c.Ret()
		return
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirect3DSwapChain9(c *cpu.CPU, winSlot int) {
	switch winSlot {
	case 3: // Present
		s.FrameCount++
	case 5: // GetBackBuffer(this, iBackBuffer, Type, ppBackBuffer)
		pp := c.Arg(3)
		if pp != 0 {
			c.Write32(pp, s.defaultBackBuf)
		}
	case 7: // GetDisplayMode(this, pMode)
		pMode := c.Arg(1)
		if pMode != 0 {
			c.Write32(pMode+0, 1280)
			c.Write32(pMode+4, 720)
			c.Write32(pMode+8, 60)
			c.Write32(pMode+12, 22)
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleID3DXBuffer(c *cpu.CPU, winSlot int) {
	thisPtr := c.Arg(0)
	res := s.Resources[thisPtr]
	switch winSlot {
	case 3: // GetBufferPointer(this)
		if res != nil {
			c.Regs[cpu.RegEAX] = res.DataPtr
		} else {
			c.Regs[cpu.RegEAX] = 0
		}
	case 4: // GetBufferSize(this)
		if res != nil {
			c.Regs[cpu.RegEAX] = res.Length
		} else {
			c.Regs[cpu.RegEAX] = 0
		}
	default:
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (s *Subsystem) handleSimpleCOM(c *cpu.CPU, winSlot int) {
	_ = winSlot
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) thunkD3DXCreateTextureFromFileExA(c *cpu.CPU) {
	// D3DXCreateTextureFromFileInMemoryEx(pDevice, pSrcData, SrcDataSize, Width, Height, MipLevels, Usage, Format, Pool, Filter, MipFilter, ColorKey, pSrcInfo, pPalette, ppTexture)
	pSrc := c.Arg(1)
	srcSize := c.Arg(2)
	pSrcInfo := c.Arg(12)
	ppTex := c.Arg(14)
	w, h, fmtCode, pixels := s.decodeDDSInMemory(c, pSrc, srcSize)
	if pSrcInfo != 0 {
		c.Write32(pSrcInfo+0, w)
		c.Write32(pSrcInfo+4, h)
		c.Write32(pSrcInfo+8, 1)
		c.Write32(pSrcInfo+12, 1)
		c.Write32(pSrcInfo+16, fmtCode)
	}
	if ppTex != 0 {
		tex := s.allocTextureLazy(c, w, h, fmtCode)
		if res := s.Resources[tex]; res != nil && len(pixels) > 0 {
			res.Pixels = pixels
		}
		c.Write32(ppTex, tex)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) thunkD3DXCreateTextureFromFileInMemory(c *cpu.CPU) {
	// D3DXCreateTextureFromFileInMemory(pDevice, pSrcData, SrcDataSize, ppTexture)
	pSrc := c.Arg(1)
	srcSize := c.Arg(2)
	ppTex := c.Arg(3)
	w, h, fmtCode, pixels := s.decodeDDSInMemory(c, pSrc, srcSize)
	if ppTex != 0 {
		tex := s.allocTextureLazy(c, w, h, fmtCode)
		if res := s.Resources[tex]; res != nil && len(pixels) > 0 {
			res.Pixels = pixels
		}
		c.Write32(ppTex, tex)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) allocD3DXBuffer(c *cpu.CPU, size uint32) uint32 {
	obj := s.allocCOMObject(c, s.vtD3DXBuffer, 16)
	dataPtr := c.Malloc(size)
	// Write a minimal valid D3D9 shader bytecode token header (ps_2_0 / vs_2_0 + D3DSIO_END = 0x0000FFFF)
	c.Write32(dataPtr+0, 0xFFFE0200)
	c.Write32(dataPtr+4, 0x0000FFFF)
	s.Resources[obj] = &BufferDesc{DataPtr: dataPtr, Length: size}
	return obj
}

func (s *Subsystem) thunkD3DXAssembleShader(c *cpu.CPU) {
	// 0x00691ef0 is D3DXCreateCubeTextureFromFileInMemory(pDevice, pSrcData, SrcDataSize, ppCubeTexture)
	ppCubeTex := c.Arg(3)
	if ppCubeTex != 0 {
		c.Write32(ppCubeTex, s.allocCOMObject(c, s.vtCubeTex9, 32))
	}
	if s.TraceLog {
		fmt.Printf("[D3DX9] D3DXCreateCubeTextureFromFileInMemory(size=%d) -> S_OK\n", c.Arg(2))
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) thunkD3DXCompileShader(c *cpu.CPU) {
	// D3DXCompileShader(pSrcData, srcDataLen, pDefines, pInclude, pFunctionName, pProfile, Flags, ppShader, ppErrorMsgs, ppConstantTable)
	ppShader := c.Arg(7)
	ppErr := c.Arg(8)
	ppConst := c.Arg(9)
	if ppShader != 0 {
		c.Write32(ppShader, s.allocD3DXBuffer(c, 64))
	}
	if ppErr != 0 {
		c.Write32(ppErr, 0)
	}
	if ppConst != 0 {
		c.Write32(ppConst, 0)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) thunkDirectInput8Create(c *cpu.CPU) {
	// DirectInput8Create(hinst, dwVersion, riidltf, ppvOut, punkOuter)
	ppvOut := c.Arg(3)
	diObj := s.allocCOMObject(c, s.vtDInput8, 16)
	if ppvOut != 0 {
		c.Write32(ppvOut, diObj)
	}
	if s.TraceLog {
		fmt.Printf("[DInput8] DirectInput8Create() -> 0x%08x\n", diObj)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirectInput8(c *cpu.CPU, winSlot int) {
	switch winSlot {
	case 3: // CreateDevice(this, rguid, lplpDirectInputDevice, pUnkOuter)
		fmt.Printf("[DInput8] CreateDevice guid.Data1=0x%08x Data2=0x%04x (6F1D2B61=keyboard 6F1D2B60=mouse) retEIP=0x%08x\n", c.Read32(c.Arg(1)), c.Read16(c.Arg(1)+4), c.Read32(c.Regs[cpu.RegESP]))
		ppDev := c.Arg(2)
		dev := s.allocCOMObject(c, s.vtDIDevice8, 16)
		if ppDev != 0 {
			c.Write32(ppDev, dev)
		}
		c.Regs[cpu.RegEAX] = 0
	case 4: // EnumDevices(this, dwDevType, lpCallback, pvRef, dwFlags)
		devClass := c.Arg(1)
		cb := c.Arg(2)
		pvRef := c.Arg(3)
		// DI8DEVCLASS_ALL=0, DI8DEVCLASS_KEYBOARD=3: report one keyboard (GUID_SysKeyboard). Game controllers: none.
		if cb != 0 && (devClass == 0 || devClass == 3) {
			inst := c.Malloc(580) // sizeof(DIDEVICEINSTANCEA) for DX8
			clear(c.Mem[inst : inst+580])
			c.Write32(inst+0, 580)
			kb := []byte{0x61, 0x2B, 0x1D, 0x6F, 0xA0, 0xD5, 0xCF, 0x11, 0xBF, 0xC7, 0x44, 0x45, 0x53, 0x54, 0x00, 0x00}
			copy(c.Mem[inst+4:], kb)  // guidInstance
			copy(c.Mem[inst+20:], kb) // guidProduct
			c.Write32(inst+36, 0x00000413) // DI8DEVTYPE_KEYBOARD | (DI8DEVTYPEKEYBOARD_PCENH << 8)
			c.WriteCString(inst+40, "Keyboard")
			c.WriteCString(inst+300, "Keyboard")
			if s.TraceLog {
				fmt.Printf("[DInput8] EnumDevices(class=%d) -> reporting keyboard to callback 0x%08x\n", devClass, cb)
			}
			_, _ = c.CallGuestNested(cb, inst, pvRef)
		}
		c.Regs[cpu.RegEAX] = 0
	default:
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

var diSeen = map[[2]uint32]bool{}

func (s *Subsystem) handleIDirectInputDevice8(c *cpu.CPU, winSlot int) {
	switch winSlot {
	case 9: // GetDeviceState(this, cbData, lpvData)
		cbData := c.Arg(1)
		lpvData := c.Arg(2)
		if k := [2]uint32{c.Arg(0), cbData}; !diSeen[k] {
			diSeen[k] = true
			fmt.Printf("[DInput8] GetDeviceState dev=0x%08x cbData=%d buf=0x%08x frame=%d\n", k[0], k[1], lpvData, s.FrameCount)
		}
		if lpvData != 0 && cbData > 0 && cbData <= 4096 {
			clear(c.Mem[lpvData : lpvData+cbData])
			if cbData == 256 {
				s.KeyMu.Lock()
				mode := s.AutoKeysMode
				for dik := 0; dik < 256; dik++ {
					if s.KeyHoldUntil[dik] > 0 && s.FrameCount <= s.KeyHoldUntil[dik] {
						c.Mem[lpvData+uint32(dik)] = 0x80
					}
				}
				s.KeyMu.Unlock()

				f := s.FrameCount
				switch mode {
				case "boot":
					// Auto-advance past initial device selection & F2 prompt, then stop so we don't spam Pause Menu.
					switch {
					case (f >= 200 && f < 208) || (f >= 255 && f < 263) || (f >= 365 && f < 373):
						c.Mem[lpvData+0x1C] = 0x80 // DIK_RETURN
					case f >= 315 && f < 323:
						c.Mem[lpvData+0x3C] = 0x80 // DIK_F2
					}
				case "loop":
					if f >= 200 {
						switch ph := (f - 200) % 120; {
						case ph < 6:
							c.Mem[lpvData+0x1C] = 0x80 // DIK_RETURN
						case ph >= 60 && ph < 66:
							c.Mem[lpvData+0x3C] = 0x80 // DIK_F2
						}
					}
				}
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 10: // GetDeviceData(this, cbObjectData, rgdod, pdwInOut, dwFlags)
		pdwInOut := c.Arg(3)
		if pdwInOut != 0 {
			c.Write32(pdwInOut, 0)
		}
		c.Regs[cpu.RegEAX] = 0
	default:
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (s *Subsystem) thunkDirectSoundCreate8(c *cpu.CPU) {
	// DirectSoundCreate8(lpcGuidDevice, ppDS8, pUnkOuter)
	ppDS8 := c.Arg(1)
	dsObj := s.allocCOMObject(c, s.vtDSound8, 16)
	if ppDS8 != 0 {
		c.Write32(ppDS8, dsObj)
	}
	if s.TraceLog {
		fmt.Printf("[DSound8] DirectSoundCreate8() -> 0x%08x\n", dsObj)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirectSound8(c *cpu.CPU, winSlot int) {
	switch winSlot {
	case 3: // CreateSoundBuffer(this, pcDSBufferDesc, ppDSBuffer, pUnkOuter)
		pDesc := c.Arg(1)
		ppBuf := c.Arg(2)
		bufBytes := uint32(65536)
		if pDesc != 0 {
			sz := c.Read32(pDesc + 8)
			if sz > 0 && sz <= 16*1024*1024 {
				bufBytes = sz
			}
		}
		sb := s.allocCOMObject(c, s.vtDSBuffer8, 32)
		s.Resources[sb] = &BufferDesc{Length: bufBytes}
		if ppBuf != 0 {
			c.Write32(ppBuf, sb)
		}
		c.Regs[cpu.RegEAX] = 0
	case 4: // GetCaps(this, pDSCaps)
		pCaps := c.Arg(1)
		if pCaps != 0 {
			c.Write32(pCaps+4, 0x00000FFF)
		}
		c.Regs[cpu.RegEAX] = 0
	case 5: // DuplicateSoundBuffer(this, pDSBufferOriginal, ppDSBufferDuplicate)
		pOrig := c.Arg(1)
		ppDup := c.Arg(2)
		sb := pOrig
		if sb == 0 {
			if s.dupDSBuffer == 0 {
				s.dupDSBuffer = s.allocCOMObject(c, s.vtDSBuffer8, 32)
				s.Resources[s.dupDSBuffer] = &BufferDesc{Length: 65536}
			}
			sb = s.dupDSBuffer
		}
		if ppDup != 0 {
			c.Write32(ppDup, sb)
		}
		c.Regs[cpu.RegEAX] = 0
	default:
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (s *Subsystem) handleIDirectSoundBuffer8(c *cpu.CPU, winSlot int) {
	thisPtr := c.Arg(0)
	res := s.Resources[thisPtr]
	switch winSlot {
	case 4: // GetCurrentPosition(this, pdwCurrentPlayCursor, pdwCurrentWriteCursor)
		pPlay := c.Arg(1)
		pWrite := c.Arg(2)
		pos := uint32(0)
		if res != nil && res.Length > 0 {
			pos = uint32((c.Steps / 100) % uint64(res.Length))
		}
		if pPlay != 0 {
			c.Write32(pPlay, pos)
		}
		if pWrite != 0 {
			c.Write32(pWrite, (pos+4096)%65536)
		}
		c.Regs[cpu.RegEAX] = 0
	case 9: // GetStatus(this, pdwStatus)
		pStatus := c.Arg(1)
		if pStatus != 0 {
			c.Write32(pStatus, 0)
		}
		c.Regs[cpu.RegEAX] = 0
	case 11: // Lock(this, dwOffset, dwBytes, ppvAudioPtr1, pdwAudioBytes1, ppvAudioPtr2, pdwAudioBytes2, dwFlags)
		dwBytes := c.Arg(2)
		ppv1 := c.Arg(3)
		pdw1 := c.Arg(4)
		ppv2 := c.Arg(5)
		pdw2 := c.Arg(6)
		if res != nil {
			if dwBytes == 0 || dwBytes > res.Length {
				dwBytes = res.Length
			}
			if ppv1 != 0 {
				c.Write32(ppv1, s.guestBuf(c, res))
			}
			if pdw1 != 0 {
				c.Write32(pdw1, dwBytes)
			}
		}
		if ppv2 != 0 {
			c.Write32(ppv2, 0)
		}
		if pdw2 != 0 {
			c.Write32(pdw2, 0)
		}
		c.Regs[cpu.RegEAX] = 0
	default:
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (s *Subsystem) thunkXInputGetState(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0x48F // ERROR_DEVICE_NOT_CONNECTED
	c.Ret()
}

func (s *Subsystem) thunkXInputGetCapabilities(c *cpu.CPU) {
	c.Regs[cpu.RegEAX] = 0x48F // ERROR_DEVICE_NOT_CONNECTED
	c.Ret()
}

var _ = math.Float32frombits

package d3d9

import (
	"encoding/binary"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"catalina32/pkg/cpu"
	"catalina32/pkg/d3dsm"
	"catalina32/pkg/profile"
)

const (
	VTableArenaBase = 0x0f000000
	TrampArenaBase  = 0x0f100000
)

type BufferDesc struct {
	DataPtr       uint32
	Length        uint32
	Format        uint32
	Width         uint32
	Height        uint32
	Pitch         uint32
	Depth         uint32 // volume textures
	Pixels        []uint32
	Raw           []byte // GPU path: level 0 as the game wrote it, for DXT formats (uploaded compressed)
	Channels      uint16
	SampleRate    uint32
	BitsPerSample uint16
	Playing       bool
	Looping       bool
	PlayCursor    uint32
	Volume        int32
	Frequency     uint32
	RefCount      int
	IsDuplicate   bool
	IsRT          bool // texture used as a render target: Pixels is its colour buffer
	Cube          bool // cube texture: Pixels (and Raw) hold the six faces, +X -X +Y -Y +Z -Z
	Mips          bool // D3D gives the texture a mip chain (only level 0 is kept; the GPU path generates the rest)
	Unlocked      bool // the game has written level 0 through a lock at least once
	Stale         bool // level 0 changed since Pixels was decoded
}

// unlocked records a write to level 0 through LockRect/LockBox: the next draw re-decodes it.
func (res *BufferDesc) unlocked() {
	if res != nil {
		res.Unlocked, res.Stale = true, true
	}
}

type VertexDecl struct {
	Elements        []VertexElement
	Summary         string
	HasPos          bool
	PosOffset       uint16
	PosType         uint8
	HasNormal       bool
	NormalOffset    uint16
	NormalType      uint8
	HasColor0       bool
	Color0Offset    uint16
	Color0Type      uint8
	HasColor1       bool
	Color1Offset    uint16
	Color1Type      uint8
	HasTex0         bool
	Tex0Offset      uint16
	Tex0Type        uint8
	HasTex1         bool
	Tex1Offset      uint16
	Tex1Type        uint8
	HasBlendWeight  bool
	BlendWOffset    uint16
	BlendWType      uint8
	HasBlendIndices bool
	BlendIOffset    uint16
	BlendIType      uint8
}

type Subsystem struct {
	VM             *cpu.CPU
	rendering      bool // true while the frame being drawn will be rasterized/exported
	up3D           bool // the current DrawPrimitiveUP-style batch is in-world 3D (classifyUPBatch)
	colorX2        bool // current default-renderer batch uses the 2x vertex colour rule (triState.x2)
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
	vtConstTable uint32
	vtDInput8    uint32
	vtDIDevice8  uint32
	vtDSound8    uint32
	vtDSBuffer8  uint32
	vtDS3DBuffer uint32

	d3d9Obj         uint32
	device9Obj      uint32
	defaultBackBuf  uint32
	defaultDepthBuf uint32
	defaultSwapChn  uint32
	sharedAudioBuf  uint32
	dupDSBuffer     uint32
	dummy3DObj      uint32
	mipScratchBuf   uint32
	mipScratchLen   uint32

	Resources         map[uint32]*BufferDesc
	VertexDecls       map[uint32]*VertexDecl
	CurrentVertexDecl uint32
	CurrentVD         *VertexDecl
	CurrentMaterial   uint32
	RenderStates      [256]uint32
	rsSet             [256]bool // render states the game has set (others take the D3D default)
	BoundTextures     [16]uint32
	StreamSources     [16]uint32
	StreamOffsets     [16]uint32
	StreamStrides     [16]uint32
	BoundIndices      uint32
	VSConstants       [256][4]float32
	PSConstants       [256][4]float32
	SkinMatrices      [64][16]float32
	NumSkinMatrices   uint32
	SkinLogCount      int

	Framebuffer       *image.RGBA
	ZBuffer           []float32
	FramesDir         string
	DisplayURL        string
	FrameDrawCalls    uint64
	DIPCount          uint64
	FrameMu           sync.Mutex
	LatestPNG         []byte
	LatestRGBA        []byte
	shm               []byte // the live viewer's shared memory (C32_SHM_FILE), mapped
	status            atomic.Pointer[viewerStatus]
	browserSeen       atomic.Int64 // unix time the HTTP viewer last fetched a frame
	AudioBuffers      []*BufferDesc
	AudioWriteSamples uint64
	LastAudioTime     time.Time
	LastPresentTime   time.Time
	fpsTime           time.Time // live-mode FPS report window start
	fpsFrame, fpsStep uint64
	renderWait        time.Duration
	shaderDumps       int
	padLast           [ShmPads][12]byte
	padSeen           [ShmPads]bool
	padWaited         bool
	padViewer         bool // the viewer's ready marker was seen
	padPacket         [ShmPads]uint32

	// Real shader pipeline (shaders.go)
	RealShaders                                bool
	shaderCache                                map[string]*compiledShader
	shaderBlobs                                map[uint32]*compiledShader // ID3DXBuffer data -> shader
	shaderObjs                                 map[uint32]*compiledShader // IDirect3D{Vertex,Pixel}Shader9 -> shader
	constTables                                map[uint32]*compiledShader
	curVS, curPS                               *compiledShader
	curVSObj, curPSObj                         uint32             // bound shader objects (as the game sees them)
	drawSeq                                    int32              // draw calls so far in the frame being recorded
	curRT                                      uint32             // render target 0 as set by the game (0: back buffer)
	viewportX, viewportY, viewportW, viewportH uint32             // SetViewport (GPU path only; defaults to the screen)
	scissor                                    [4]uint32          // SetScissorRect: left, top, right, bottom (GPU path only; the target by default)
	refs                                       map[uint32]*comRef // reference counts of device resources (refs.go)
	origin                                     map[uint32]string  // C32_DRAW_LOG: how each texture/surface was created
	devCalls                                   [128]int           // C32_DRAW_LOG: device method calls in the inspected frame
	renderTargets                              map[*BufferDesc]*renderTarget
	curTargetPix                               []byte // emulation side: target of the operations being queued (nil: back buffer)
	curTargetSkip                              bool
	loggedRTSize                               bool
	costMu                                     sync.Mutex
	bins                                       [numBands][]int32 // per-band operation lists (render goroutine)
	bandShaders                                []*progShader
	drawCosts                                  map[int32]*drawCost
	gpu                                        *gpuEncoder // GPU path (C32_GPU_SHM): frames go to the Metal viewer
	gpuFrame                                   bool        // the frame being recorded goes to the GPU
	curTargetDesc                              *BufferDesc // GPU path: target of the operations being queued
	drawPix                                    []byte      // render goroutine: current target (nil: back buffer)
	drawSkip                                   bool
	dumpedTex                                  map[uint32]bool
	shadeWorkers                               []*shadeWorker            // render goroutine only
	expanded                                   []queuedTri               // render goroutine only
	expandedProg                               []queuedTri               // render goroutine only
	progObjs                                   map[uint32]*d3dsm.Program // shader objects created from bytecode
	stubBlobs                                  map[uint32]bool           // placeholder bytecode returned by D3DXCompileShader (not executable)
	curVSProg                                  *d3dsm.Program
	curPSProg                                  *d3dsm.Program
	samplerStates                              [16][14]uint32
	sampSet                                    [16][14]bool
	shaderErrors                               int
	loggedConstTableSet                        bool // emulator time blocked on the previous frame's drawing

	KeyMu        sync.Mutex
	KeyHoldUntil [256]uint64
	AutoKeysMode string
	// InputScript, when set by the game profile, presses scripted keys (DIK-indexed, 0x80 = down)
	// for the C32_AUTO_KEYS mode in effect; used for unattended test runs.
	InputScript  func(s *Subsystem, mode string, keys []byte)
	CubeTexCount uint64
	// Scene milestones, set by the game profile through SceneHook (frame numbers, 0 = not yet):
	// the first interactive menu frame and the first 3D gameplay frame. Headless runs render
	// more often from FirstGameFrame on, and scripted input times key presses from them.
	FirstMenuFrame uint64
	FirstGameFrame uint64
	SceneHook      func(s *Subsystem, ev SceneEvent)

	// OnPresent, when set, is called with the new frame number at each device Present.
	OnPresent func(frame uint64)

	FrameCount     uint64
	DrawCallCount  uint64
	TriangleCount  uint64
	ClearCount     uint64
	LastClearColor uint32
	TraceLog       bool
	frameTris      []queuedTri
	spareTris      []queuedTri    // buffer returned by the render goroutine for reuse
	renderWG       sync.WaitGroup // in-flight frame on the render goroutine
}

func NewSubsystem(c *cpu.CPU) *Subsystem {
	autoKeys := os.Getenv("C32_AUTO_KEYS")
	if autoKeys == "" {
		autoKeys = "boot"
	}
	s := &Subsystem{
		VM:             c,
		nextVTableAddr: VTableArenaBase,
		nextTrampAddr:  TrampArenaBase,
		Resources:      make(map[uint32]*BufferDesc),
		VertexDecls:    make(map[uint32]*VertexDecl),
		AutoKeysMode:   autoKeys,
		TraceLog:       os.Getenv("C32_LIVE") != "1",
		viewportW:      ScreenWidth,
		viewportH:      ScreenHeight,
		scissor:        [4]uint32{0, 0, ScreenWidth, ScreenHeight},
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
	s.vtConstTable = s.buildVTable(c, "ID3DXConstantTable", 40, s.handleID3DXConstantTable)
	s.RealShaders = RealShadersEnabled()
	s.shaderCache = map[string]*compiledShader{}
	s.shaderBlobs = map[uint32]*compiledShader{}
	s.shaderObjs = map[uint32]*compiledShader{}
	s.progObjs = map[uint32]*d3dsm.Program{}
	s.renderTargets = map[*BufferDesc]*renderTarget{}
	s.stubBlobs = map[uint32]bool{}
	s.constTables = map[uint32]*compiledShader{}
	s.vtDInput8 = s.buildVTable(c, "IDirectInput8A", 20, s.handleIDirectInput8)
	s.vtDIDevice8 = s.buildVTable(c, "IDirectInputDevice8A", 40, s.handleIDirectInputDevice8)
	s.vtDSound8 = s.buildVTable(c, "IDirectSound8", 24, s.handleIDirectSound8)
	s.vtDSBuffer8 = s.buildVTable(c, "IDirectSoundBuffer8", 32, s.handleIDirectSoundBuffer8)
	s.vtDS3DBuffer = s.buildVTable(c, "IDirectSound3DBuffer8", 32, s.handleSimpleCOM)

	// Allocate default backbuffer & depth-stencil surface
	s.defaultBackBuf = s.allocSurface(c, 1280, 720, 22)
	s.defaultDepthBuf = s.allocSurface(c, 1280, 720, 75)
	s.defaultSwapChn = s.allocCOMObject(c, s.vtSwapChain9, 32)
	s.dummy3DObj = s.allocCOMObject(c, s.vtDS3DBuffer, 32)
	framesDir := os.Getenv("C32_FRAMES_DIR")
	if framesDir == "" {
		framesDir = "frames"
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
				if s.refs[vm.Arg(0)] != nil {
					vm.Regs[cpu.RegEAX] = uint32(s.addRef(vm.Arg(0)))
					vm.Ret()
					return
				}
				if ifaceName == "IDirectSoundBuffer8" {
					if res := s.Resources[vm.Arg(0)]; res != nil {
						res.RefCount++
						vm.Regs[cpu.RegEAX] = uint32(res.RefCount)
						vm.Ret()
						return
					}
				}
				vm.Regs[cpu.RegEAX] = 2
				vm.Ret()
			case 3: // Release
				if s.refs[vm.Arg(0)] != nil {
					vm.Regs[cpu.RegEAX] = uint32(s.release(vm, vm.Arg(0)))
					vm.Ret()
					return
				}
				if ifaceName == "IDirectSoundBuffer8" {
					thisObj := vm.Arg(0)
					if res := s.Resources[thisObj]; res != nil {
						res.RefCount--
						if res.RefCount <= 0 {
							res.Playing = false
							if res.IsDuplicate {
								for idx, ab := range s.AudioBuffers {
									if ab == res {
										s.AudioBuffers = append(s.AudioBuffers[:idx], s.AudioBuffers[idx+1:]...)
										break
									}
								}
								delete(s.Resources, thisObj)
							}
							vm.Regs[cpu.RegEAX] = 0
							vm.Ret()
							return
						}
						vm.Regs[cpu.RegEAX] = uint32(res.RefCount)
						vm.Ret()
						return
					}
				}
				vm.Regs[cpu.RegEAX] = 1
				vm.Ret()
			case 4: // QueryInterface(this, riid, ppvObj)
				thisObj := vm.Arg(0)
				riid := vm.Arg(1)
				ppv := vm.Arg(2)
				outObj := thisObj
				if ifaceName == "IDirectSoundBuffer8" && riid != 0 && riid+4 <= uint32(len(vm.Mem)) {
					data1 := vm.Read32(riid)
					// IID_IDirectSoundBuffer8 has Data1 == 0x6825a449; 3D buffer/listener interfaces use 0x279afa86 / 0x279afa84
					if data1 != 0x6825a449 {
						outObj = s.dummy3DObj
					} else if res := s.Resources[thisObj]; res != nil {
						res.RefCount++
					}
				}
				if ppv != 0 {
					vm.Write32(ppv, outObj)
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
	if s.tracked(vtAddr) {
		if s.refs == nil {
			s.refs = map[uint32]*comRef{}
		}
		s.refs[obj] = &comRef{count: 1}
	}
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
	pitch := texPitch(width, format)
	bufSize := lockSize(pitch, height, format)
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
	pitch := texPitch(width, format)
	bufSize := lockSize(pitch, height, format)
	s.Resources[obj] = &BufferDesc{Length: bufSize, Format: format, Width: width, Height: height, Pitch: pitch}
	return obj
}

// texPitch is the row pitch LockRect reports for a level w texels wide: the layout
// decodeTexturePixels reads (rows of 4x4 blocks for DXT, one byte per texel for L8/A8/P8, four
// bytes otherwise).
func texPitch(w, format uint32) uint32 {
	switch {
	case format == 0x31545844: // DXT1: 8 bytes per block
		return max((w+3)/4, 1) * 8
	case isDXT(format):
		return max((w+3)/4, 1) * 16
	case format == 50 || format == 28 || format == 41: // L8, A8, P8
		return w
	}
	return w * 4
}

// texRows is the number of pitch-sized rows in a level h texels high.
func texRows(h, format uint32) uint32 {
	if isDXT(format) {
		return (h + 3) / 4
	}
	return h
}

// mipScratch returns the shared buffer that locks of lower mip levels write into (those levels
// are not sampled), grown to at least need bytes.
func (s *Subsystem) mipScratch(c *cpu.CPU, need uint32) uint32 {
	need = max(need, 1<<20)
	if need > s.mipScratchLen {
		c.Free(s.mipScratchBuf)
		s.mipScratchBuf, s.mipScratchLen = c.Malloc(need), need
	}
	return s.mipScratchBuf
}

// isDXT reports whether format is a block-compressed FourCC format (DXT1..DXT5).
func isDXT(format uint32) bool {
	return format >= 0x31545844 && format <= 0x35545844
}

// lockSize is the guest backing store for a w-pitch, h-row image: what the game may write through
// LockRect with the pitch we report (rows of 4x4 blocks for DXT formats). Sizes up to 4 MB keep
// the historical rounding (pitch*height, capped), so guest heap layout is unchanged for them.
func lockSize(pitch, height, format uint32) uint32 {
	return max(min(pitch*height, 4<<20), pitch*texRows(height, format))
}

// guestBuf returns the resource's guest-memory backing store, allocating it on first use.
func (s *Subsystem) guestBuf(c *cpu.CPU, res *BufferDesc) uint32 {
	if res.DataPtr == 0 && res.Length > 0 {
		res.DataPtr = c.Malloc(res.Length)
	}
	return res.DataPtr
}

// SceneEvent tells SceneHook what just happened.
type SceneEvent int

const (
	ScenePresent     SceneEvent = iota // a frame was presented
	SceneIndexedDraw                   // an indexed draw is about to run (before FirstGameFrame only)
)

// unimplemented reports (once per method) a COM method the shims answer with S_OK and nothing
// else, so missing features show up in the log instead of failing silently.
func (s *Subsystem) unimplemented(c *cpu.CPU, iface string, winSlot int) {
	c.ReportOnce(fmt.Sprintf("%s:%d", iface, winSlot), "[D3D9] %s is not implemented: returns S_OK (called from 0x%08x)",
		methodName(iface, winSlot), c.Read32(c.Regs[cpu.RegESP]))
}

// RegisterIndirectXThunks binds the IndirectX (D3D9/D3DX/DirectInput/DirectSound/XInput) and Bink
// shims to the functions the game profile names.
func (s *Subsystem) RegisterIndirectXThunks(c *cpu.CPU, prof *profile.Profile) {
	prof.Bind(c, "Direct3DCreate9", s.thunkDirect3DCreate9)
	prof.Bind(c, "D3DPERF_BeginEvent", s.thunkReturnZero)
	prof.Bind(c, "D3DPERF_EndEvent", s.thunkReturnZero)

	prof.Bind(c, "D3DXCreateTextureFromFileExA", s.thunkD3DXCreateTextureFromFileExA)
	prof.Bind(c, "D3DXCreateTextureFromFileInMemory", s.thunkD3DXCreateTextureFromFileInMemory)
	prof.Bind(c, "D3DXCreateCubeTextureFromFileInMemory", s.thunkD3DXCreateCubeTextureFromFileInMemory)
	prof.Bind(c, "D3DXCompileShader", s.thunkD3DXCompileShader)
	prof.Bind(c, "D3DXMatrixMultiply", thunkD3DXMatrixMultiply)

	prof.Bind(c, "DirectInput8Create", s.thunkDirectInput8Create)
	prof.Bind(c, "DirectSoundCreate8", s.thunkDirectSoundCreate8)
	prof.Bind(c, "XInputGetState", s.thunkXInputGetState)
	prof.Bind(c, "XInputGetCapabilities", s.thunkXInputGetCapabilities)

	// Bink Video shims
	prof.Bind(c, "BinkOpen", s.thunkReturnZero)
	prof.Bind(c, "BinkClose", s.thunkReturnZero)
	prof.Bind(c, "BinkDoFrame", s.thunkReturnZero)
	prof.Bind(c, "BinkNextFrame", s.thunkReturnZero)
	prof.Bind(c, "BinkWait", s.thunkReturnZero)
	prof.Bind(c, "BinkCopyToBuffer", s.thunkReturnZero)
	prof.Bind(c, "BinkSetSoundTrack", s.thunkReturnZero)
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
	clear(c.WBytes(ptr, 0x130))
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
			clear(c.WBytes(pIdent, 1100))
			c.WriteCString(pIdent+0x000, "IndirectX")
			c.WriteCString(pIdent+0x200, "NVIDIA GeForce GTX 1080 Ti")
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
		s.unimplemented(c, "IDirect3D9", winSlot)
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

func (s *Subsystem) handleIDirect3DDevice9(c *cpu.CPU, winSlot int) {
	if s.drawLogging() {
		s.devCalls[winSlot]++
		if winSlot == 17 {
			var b strings.Builder
			for i, n := range s.devCalls {
				if n > 0 {
					fmt.Fprintf(&b, " %d:%d", i, n)
				}
			}
			fmt.Printf("[DEVCOUNT f%d]%s Present(%x, %x, %x, %x)\n", s.FrameCount+1, b.String(), c.Arg(1), c.Arg(2), c.Arg(3), c.Arg(4))
			s.devCalls = [128]int{}
		}
		if name, ok := loggedDeviceCalls[winSlot]; ok {
			fmt.Printf("[DEV f%d] %s(%x, %x, %x, %x, %x) after draw #%d\n", s.FrameCount+1, name, c.Arg(1), c.Arg(2), c.Arg(3), c.Arg(4), c.Arg(5), s.drawSeq)
		}
	}
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
		s.publishStatus(c)
		if s.OnPresent != nil {
			s.OnPresent(s.FrameCount)
		}
		if s.SceneHook != nil {
			s.SceneHook(s, ScenePresent)
		}
		if s.rendering && (s.FrameDrawCalls > 0 || s.FrameCount == 1) {
			s.submitFrame()
		} else {
			s.dropFrameKeepClears()
		}
		if liveMode {
			s.reportFPS(c)
		}
		if s.shm != nil && s.AutoKeysMode == "off" {
			if !s.LastPresentTime.IsZero() {
				const targetFrame = 16667 * time.Microsecond
				if elapsed := time.Since(s.LastPresentTime); elapsed < targetFrame {
					time.Sleep(targetFrame - elapsed)
				}
			}
			s.LastPresentTime = time.Now()
		}
		s.mixAudioFrame(c)
		s.rendering = s.wantRender(s.FrameCount + 1)
		if s.TraceLog && (s.FrameCount <= 10 || s.FrameCount%30 == 0) {
			fmt.Printf("[D3D9] Present() Frame #%d completed (DrawCalls=%d, Triangles=%d, Clears=%d, AudioSamples=%d, Steps=%d)\n",
				s.FrameCount, s.DrawCallCount, s.TriangleCount, s.ClearCount, s.AudioWriteSamples, c.Steps)
			if s.FrameCount%300 == 0 {
				fmt.Printf("[catalina32] guest heap: %d MB in use, high-water mark %d MB\n", c.HeapInUse()>>20, c.HeapPeak()>>20)
			}
		}
		s.FrameDrawCalls = 0
		s.drawSeq = 0
		c.Regs[cpu.RegEAX] = 0
		c.Ret()
		if c.Preempt != nil {
			c.Preempt(c)
		}
		return
	case 18: // GetBackBuffer(this, iSwapChain, iBackBuffer, Type, ppBackBuffer)
		pp := c.Arg(4)
		if pp != 0 {
			c.Write32(pp, s.defaultBackBuf)
			s.addRef(s.defaultBackBuf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 23: // CreateTexture(this, Width, Height, Levels, Usage, Format, Pool, ppTexture, pSharedHandle)
		w := c.Arg(1)
		h := c.Arg(2)
		fmtCode := c.Arg(5)
		pp := c.Arg(7)
		tex := s.allocTexture(c, w, h, fmtCode)
		s.noteOrigin(tex, "CreateTexture(%dx%d fmt %d usage %x pool %d)", w, h, fmtCode, c.Arg(4), c.Arg(6))
		s.Resources[tex].Mips = c.Arg(3) != 1 // Levels (0: a full chain)
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
		pitch := texPitch(w, fmtCode)
		size := max(65536, lockSize(pitch, h, fmtCode)*max(c.Arg(3), 1))
		buf := c.Malloc(size)
		s.Resources[tex] = &BufferDesc{DataPtr: buf, Length: size, Format: fmtCode, Width: w, Height: h, Depth: max(c.Arg(3), 1), Pitch: pitch}
		if pp != 0 {
			c.Write32(pp, tex)
		}
		c.Regs[cpu.RegEAX] = 0
	case 25: // CreateCubeTexture(this, EdgeLength, Levels, Usage, Format, Pool, ppCubeTex, pShared)
		edge := c.Arg(1)
		fmtCode := c.Arg(4)
		pp := c.Arg(6)
		tex := s.allocCOMObject(c, s.vtCubeTex9, 32)
		pitch := texPitch(edge, fmtCode)
		size := max(65536, 6*lockSize(pitch, edge, fmtCode)) // six faces, one after another
		buf := c.Malloc(size)
		s.Resources[tex] = &BufferDesc{DataPtr: buf, Length: size, Format: fmtCode, Width: edge, Height: edge, Pitch: pitch, Cube: true}
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
		s.noteOrigin(surf, "CreateRenderTarget(%dx%d fmt %d)", w, h, fmtCode)
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
	case 34: // StretchRect(this, pSourceSurface, pSourceRect, pDestSurface, pDestRect, Filter)
		s.stretchRect(c.Arg(1), c.Arg(3))
		c.Regs[cpu.RegEAX] = 0
	case 37: // SetRenderTarget(this, RenderTargetIndex, pRenderTarget)
		if c.Arg(1) == 0 {
			s.bind(c, &s.curRT, c.Arg(2))
			s.setRenderTarget(c.Arg(2))
			// D3D9 resets the viewport and the scissor rectangle to the whole new target.
			w, h := s.targetSize(c.Arg(2))
			s.viewportX, s.viewportY, s.viewportW, s.viewportH = 0, 0, w, h
			s.scissor = [4]uint32{0, 0, w, h}
		}
		if s.drawLogging() {
			desc := ""
			if r := s.Resources[c.Arg(2)]; r != nil {
				desc = fmt.Sprintf(" %dx%d f%d %s", r.Width, r.Height, r.Format, s.origin[c.Arg(2)])
			}
			ret := c.Read32(c.Regs[cpu.RegESP])
			var chain []string
			for bp, i := c.Regs[cpu.RegEBP], 0; bp != 0 && uint64(bp)+8 <= uint64(len(c.Mem)) && i < 5; bp, i = c.Read32(bp), i+1 {
				chain = append(chain, fmt.Sprintf("%x", c.Read32(bp+4)))
			}
			fmt.Printf("[RT f%d] SetRenderTarget(%d, %x%s) after draw #%d (backbuffer=%x) from %x <- %s\n", s.FrameCount+1, c.Arg(1), c.Arg(2), desc, s.drawSeq, s.defaultBackBuf, ret, strings.Join(chain, " <- "))
		}
		c.Regs[cpu.RegEAX] = 0
	case 38: // GetRenderTarget(this, RenderTargetIndex, ppRenderTarget)
		pp := c.Arg(2)
		if pp != 0 {
			c.Write32(pp, s.defaultBackBuf)
			s.addRef(s.defaultBackBuf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 40: // GetDepthStencilSurface(this, ppZStencilSurface)
		pp := c.Arg(1)
		if pp != 0 {
			c.Write32(pp, s.defaultDepthBuf)
			s.addRef(s.defaultDepthBuf)
		}
		c.Regs[cpu.RegEAX] = 0
	case 43: // Clear(this, Count, pRects, Flags, Color, Z, Stencil)
		s.ClearCount++
		count, pRects, flags, col, z := c.Arg(1), c.Arg(2), c.Arg(3), c.Arg(4), c.ArgF32(5)
		s.LastClearColor = col
		if s.rendering {
			clear := func(x0, y0, x1, y1 int) {
				if s.gpuFrame {
					cx0, cy0, cx1, cy1 := s.clearClip()
					x0, y0, x1, y1 = max(x0, cx0), max(y0, cy0), min(x1, cx1), min(y1, cy1)
				}
				s.queueClear(col, flags, z, x0, y0, x1, y1)
			}
			if count == 0 || pRects == 0 {
				clear(0, 0, ScreenWidth-1, ScreenHeight-1)
			}
			for i := uint32(0); i < count && pRects != 0 && i < 1024; i++ { // D3DRECT{x1, y1, x2, y2}, x2/y2 exclusive
				r := pRects + i*16
				clear(int(int32(c.Read32(r))), int(int32(c.Read32(r+4))), int(int32(c.Read32(r+8)))-1, int(int32(c.Read32(r+12)))-1)
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 47: // SetViewport(this, pViewport): D3DVIEWPORT9{X, Y, Width, Height, MinZ, MaxZ}
		vp := c.Arg(1)
		if vp != 0 && c.Read32(vp+8) != 0 && c.Read32(vp+12) != 0 {
			s.viewportX, s.viewportY = c.Read32(vp+0), c.Read32(vp+4)
			s.viewportW, s.viewportH = c.Read32(vp+8), c.Read32(vp+12)
		}
		c.Regs[cpu.RegEAX] = 0
	case 75: // SetScissorRect(this, pRect): RECT{left, top, right, bottom}, right/bottom exclusive
		if r := c.Arg(1); r != 0 {
			s.scissor = [4]uint32{c.Read32(r), c.Read32(r + 4), c.Read32(r + 8), c.Read32(r + 12)}
		}
		c.Regs[cpu.RegEAX] = 0
	case 76: // GetScissorRect(this, pRect)
		if r := c.Arg(1); r != 0 {
			for i, v := range s.scissor {
				c.Write32(r+uint32(4*i), v)
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 48: // GetViewport(this, pViewport)
		vp := c.Arg(1)
		if vp != 0 {
			c.Write32(vp+0, s.viewportX)
			c.Write32(vp+4, s.viewportY)
			c.Write32(vp+8, s.viewportW)
			c.Write32(vp+12, s.viewportH)
			c.WriteF32(vp+16, 0.0)
			c.WriteF32(vp+20, 1.0)
		}
		c.Regs[cpu.RegEAX] = 0
	case 57: // SetRenderState
		if rs := c.Arg(1); rs < 256 {
			s.RenderStates[rs] = c.Arg(2)
			s.rsSet[rs] = true
		}
		c.Regs[cpu.RegEAX] = 0
	case 58: // GetRenderState
		pVal := c.Arg(2)
		if pVal != 0 {
			var v uint32
			if rs := c.Arg(1); rs < 256 {
				v = s.RenderStates[rs]
			}
			c.Write32(pVal, v)
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
	case 69: // SetSamplerState(this, Sampler, Type, Value)
		if st, ty := c.Arg(1), c.Arg(2); st < 16 && ty < 14 {
			s.samplerStates[st][ty] = c.Arg(3)
			s.sampSet[st][ty] = true
		}
		c.Regs[cpu.RegEAX] = 0
	case 65: // SetTexture(this, Stage, pTexture)
		stage := c.Arg(1)
		if s.drawLogging() {
			fmt.Printf("[DEV f%d] SetTexture(%d, %x [%s]) after draw #%d\n", s.FrameCount+1, stage, c.Arg(2), s.origin[c.Arg(2)], s.drawSeq)
		}
		if stage < 16 {
			s.bind(c, &s.BoundTextures[stage], c.Arg(2))
		}
		c.Regs[cpu.RegEAX] = 0
	case 66: // GetTextureStageState(this, Stage, Type, pValue): stage states are not tracked
		if p := c.Arg(3); p != 0 {
			c.Write32(p, 0)
		}
		c.Regs[cpu.RegEAX] = 0
	case 64: // GetTexture(this, Stage, ppTexture)
		stage := c.Arg(1)
		pp := c.Arg(2)
		if pp != 0 && stage < 16 {
			c.Write32(pp, s.BoundTextures[stage])
			s.addRef(s.BoundTextures[stage])
		}
		c.Regs[cpu.RegEAX] = 0
	case 81: // DrawPrimitive(this, PrimitiveType, StartVertex, PrimitiveCount)
		s.drawSeq++
		if vbRes := s.Resources[s.StreamSources[0]]; vbRes != nil {
			s.logDraw(c, "DP", c.Arg(1), c.Arg(3), vbRes.DataPtr+s.StreamOffsets[0]+c.Arg(2)*s.StreamStrides[0], s.StreamStrides[0])
		}
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.TriangleCount += uint64(c.Arg(3))
		if s.rendering {
			if vbRes := s.Resources[s.StreamSources[0]]; vbRes != nil {
				stride := s.StreamStrides[0]
				pVerts := vbRes.DataPtr + s.StreamOffsets[0] + c.Arg(2)*stride
				is2D := (stride == 24 || stride == 20)
				s.rasterizePrimitiveBatch(c, c.Arg(1), c.Arg(3), pVerts, stride, is2D)
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 82: // DrawIndexedPrimitive(this, Type, BaseVertexIndex, MinVertexIndex, NumVertices, startIndex, primCount)
		s.drawSeq++
		if vbRes := s.Resources[s.StreamSources[0]]; vbRes != nil {
			s.logDraw(c, "DIP", c.Arg(1), c.Arg(6), vbRes.DataPtr+s.StreamOffsets[0]+uint32(int32(c.Arg(2))+int32(c.Arg(3)))*s.StreamStrides[0], s.StreamStrides[0])
		}
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.DIPCount++
		prims := c.Arg(6)
		s.TriangleCount += uint64(prims)
		vbRes, ibRes := s.Resources[s.StreamSources[0]], s.Resources[s.BoundIndices]
		if s.FirstGameFrame == 0 && s.SceneHook != nil && vbRes != nil && ibRes != nil {
			if s.SceneHook(s, SceneIndexedDraw); s.FirstGameFrame != 0 {
				// Gameplay starts: draw this frame from here on.
				s.rendering = true
				s.frameTris = s.frameTris[:0]
				s.queueClear(0xFF000000, 3, 1, 0, 0, ScreenWidth-1, ScreenHeight-1)
			}
		}
		if s.rendering {
			stride := s.StreamStrides[0]
			if vbRes != nil && ibRes != nil {
				pVerts := vbRes.DataPtr + s.StreamOffsets[0]
				is32 := ibRes.Format == 102 // D3DFMT_INDEX32
				s.rasterizeIndexedBatch(c, c.Arg(1), int32(c.Arg(2)), c.Arg(5), prims, pVerts, stride, ibRes.DataPtr, ibRes.Length, is32, false)
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 83: // DrawPrimitiveUP(this, PrimitiveType, PrimitiveCount, pVertexStreamZeroData, VertexStreamZeroStride)
		s.drawSeq++
		s.logDraw(c, "DPUP", c.Arg(1), c.Arg(2), c.Arg(3), c.Arg(4))
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.TriangleCount += uint64(c.Arg(2))
		s.rasterizePrimitiveBatch(c, c.Arg(1), c.Arg(2), c.Arg(3), c.Arg(4), true)
		c.Regs[cpu.RegEAX] = 0
	case 84: // DrawIndexedPrimitiveUP(this, PrimitiveType, MinVertexIndex, NumVertices, PrimitiveCount, pIndexData, IndexDataFormat, pVertexStreamZeroData, VertexStreamZeroStride)
		s.drawSeq++
		s.logDraw(c, "DIPUP", c.Arg(1), c.Arg(4), c.Arg(7)+c.Arg(2)*c.Arg(8), c.Arg(8))
		s.DrawCallCount++
		s.FrameDrawCalls++
		s.TriangleCount += uint64(c.Arg(4))
		is32 := c.Arg(6) == 102
		s.rasterizeIndexedBatch(c, c.Arg(1), 0, 0, c.Arg(4), c.Arg(7), c.Arg(8), c.Arg(5), 65536, is32, true)
		c.Regs[cpu.RegEAX] = 0
	case 86: // CreateVertexDeclaration(this, pVertexElements, ppDecl)
		pElem := c.Arg(1)
		pp := c.Arg(2)
		decl := s.allocCOMObject(c, s.vtVertexDecl, 16)
		if pElem != 0 {
			vd := &VertexDecl{}
			for i := uint32(0); i < 16 && pElem+i*8+8 <= uint32(len(c.Mem)); i++ {
				stream := c.Read16(pElem + i*8 + 0)
				if stream == 0xFF || stream > 15 {
					break
				}
				off := c.Read16(pElem + i*8 + 2)
				elType := c.Read8(pElem + i*8 + 4)
				usage := c.Read8(pElem + i*8 + 6)
				usageIdx := c.Read8(pElem + i*8 + 7)
				vd.Elements = append(vd.Elements, VertexElement{Stream: stream, Offset: off, Type: elType, Usage: usage, UIdx: usageIdx})
				switch usage {
				case 0: // D3DDECLUSAGE_POSITION
					if usageIdx == 0 {
						vd.HasPos, vd.PosOffset, vd.PosType = true, off, elType
					}
				case 1: // D3DDECLUSAGE_BLENDWEIGHT
					if usageIdx == 0 {
						vd.HasBlendWeight, vd.BlendWOffset, vd.BlendWType = true, off, elType
					}
				case 2: // D3DDECLUSAGE_BLENDINDICES
					if usageIdx == 0 {
						vd.HasBlendIndices, vd.BlendIOffset, vd.BlendIType = true, off, elType
					}
				case 3: // D3DDECLUSAGE_NORMAL
					if usageIdx == 0 {
						vd.HasNormal, vd.NormalOffset, vd.NormalType = true, off, elType
					}
				case 5: // D3DDECLUSAGE_TEXCOORD
					if usageIdx == 0 {
						vd.HasTex0, vd.Tex0Offset, vd.Tex0Type = true, off, elType
					} else if usageIdx == 1 {
						vd.HasTex1, vd.Tex1Offset, vd.Tex1Type = true, off, elType
					}
				case 10: // D3DDECLUSAGE_COLOR
					if usageIdx == 0 {
						vd.HasColor0, vd.Color0Offset, vd.Color0Type = true, off, elType
					} else if usageIdx == 1 {
						vd.HasColor1, vd.Color1Offset, vd.Color1Type = true, off, elType
					}
				}
			}
			s.VertexDecls[decl] = vd
		}
		if pp != 0 {
			c.Write32(pp, decl)
		}
		c.Regs[cpu.RegEAX] = 0
	case 87: // SetVertexDeclaration(this, pDecl)
		s.CurrentVertexDecl = c.Arg(1)
		s.CurrentVD = s.VertexDecls[s.CurrentVertexDecl]
		c.Regs[cpu.RegEAX] = 0
	case 91: // CreateVertexShader(this, pFunction, ppShader)
		pp := c.Arg(2)
		if pp != 0 {
			obj := s.allocCOMObject(c, s.vtVertexShdr, 16)
			if cs := s.shaderBlobs[c.Arg(1)]; cs != nil {
				s.shaderObjs[obj] = cs
			}
			s.logShaderCreate(c, "vs", obj, c.Arg(1))
			s.createProg(c, obj, c.Arg(1))
			c.Write32(pp, obj)
		}
		c.Regs[cpu.RegEAX] = 0
	case 92: // SetVertexShader(this, pShader)
		s.curVS = s.shaderObjs[c.Arg(1)]
		s.curVSObj = c.Arg(1)
		s.curVSProg = s.progObjs[c.Arg(1)]
		c.Regs[cpu.RegEAX] = 0
	case 94: // SetVertexShaderConstantF(this, StartRegister, pConstantData, Vector4fCount)
		startReg := int(c.Arg(1))
		pData := c.Arg(2)
		vecCount := int(c.Arg(3))
		if startReg == 16 {
			s.NumSkinMatrices = 0
		}
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
			s.bind(c, &s.StreamSources[st], c.Arg(2))
			s.StreamOffsets[st] = c.Arg(3)
			s.StreamStrides[st] = c.Arg(4)
		}
		c.Regs[cpu.RegEAX] = 0
	case 102: // SetStreamSourceFreq(this, StreamNumber, Setting): no instancing
		c.Regs[cpu.RegEAX] = 0
	case 104: // SetIndices(this, pIndexData)
		s.bind(c, &s.BoundIndices, c.Arg(1))
		c.Regs[cpu.RegEAX] = 0
	case 106: // CreatePixelShader(this, pFunction, ppShader)
		pp := c.Arg(2)
		if pp != 0 {
			obj := s.allocCOMObject(c, s.vtPixelShdr, 16)
			if cs := s.shaderBlobs[c.Arg(1)]; cs != nil {
				s.shaderObjs[obj] = cs
			}
			s.logShaderCreate(c, "ps", obj, c.Arg(1))
			s.createProg(c, obj, c.Arg(1))
			c.Write32(pp, obj)
		}
		c.Regs[cpu.RegEAX] = 0
	case 107: // SetPixelShader(this, pShader)
		s.curPS = s.shaderObjs[c.Arg(1)]
		s.curPSObj = c.Arg(1)
		s.curPSProg = s.progObjs[c.Arg(1)]
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
	case 12, 41, 42: // ShowCursor, BeginScene, EndScene: nothing to do
		c.Regs[cpu.RegEAX] = 0
	default:
		s.unimplemented(c, "IDirect3DDevice9", winSlot)
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
	c.Write32(pDesc+4, 1)  // Type = D3DRTYPE_SURFACE
	c.Write32(pDesc+8, 0)  // Usage
	c.Write32(pDesc+12, 1) // Pool = D3DPOOL_MANAGED
	c.Write32(pDesc+16, 0) // MultiSampleType
	c.Write32(pDesc+20, 0) // MultiSampleQuality
	c.Write32(pDesc+24, w)
	c.Write32(pDesc+28, h)
}

func (s *Subsystem) handleIDirect3DTexture9(c *cpu.CPU, winSlot int) {
	if s.drawLogging() && winSlot >= 11 {
		fmt.Printf("[Texture f%d] %x slot %d (%x, %x, %x)\n", s.FrameCount+1, c.Arg(0), winSlot, c.Arg(1), c.Arg(2), c.Arg(3))
	}
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
		surf := s.child(c, thisPtr, lvl, func() uint32 {
			surf := s.allocSurface(c, max(w>>lvl, 1), max(h>>lvl, 1), fmtCode)
			s.noteOrigin(surf, "GetSurfaceLevel(%x [%s], %d)", thisPtr, s.origin[thisPtr], lvl)
			if lvl == 0 && res != nil {
				s.Resources[surf] = res
			}
			return surf
		})
		if pp != 0 {
			c.Write32(pp, surf)
		}
	case 19: // LockRect(this, Level, pLockedRect, pRect, Flags)
		lvl := c.Arg(1)
		pLock := c.Arg(2)
		res := s.Resources[thisPtr]
		if pLock != 0 && res != nil {
			if lvl == 0 {
				c.Write32(pLock+0, res.Pitch)
				c.Write32(pLock+4, s.guestBuf(c, res))
			} else {
				pitch := texPitch(max(res.Width>>lvl, 1), res.Format)
				c.Write32(pLock+0, pitch)
				c.Write32(pLock+4, s.mipScratch(c, lockSize(pitch, max(res.Height>>lvl, 1), res.Format)))
			}
		}
	case 20: // UnlockRect(this, Level)
		if c.Arg(1) == 0 {
			s.Resources[thisPtr].unlocked()
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
			face, lvl := c.Arg(1), c.Arg(2)
			c.Write32(pp, s.child(c, thisPtr, face<<16|lvl, func() uint32 { return s.allocSurface(c, 64, 64, 21) }))
		}
	case 19: // LockRect(this, FaceType, Level, pLockedRect, pRect, Flags)
		face, lvl, pLock := min(c.Arg(1), 5), c.Arg(2), c.Arg(3)
		res := s.Resources[thisPtr]
		if pLock != 0 && res != nil {
			if lvl == 0 { // each face has its own slice of the buffer
				c.Write32(pLock+0, res.Pitch)
				c.Write32(pLock+4, s.guestBuf(c, res)+face*lockSize(res.Pitch, res.Height, res.Format))
			} else {
				pitch := texPitch(max(res.Width>>lvl, 1), res.Format)
				c.Write32(pLock+0, pitch)
				c.Write32(pLock+4, s.mipScratch(c, lockSize(pitch, max(res.Height>>lvl, 1), res.Format)))
			}
		}
	case 20: // UnlockRect(this, FaceType, Level)
		if c.Arg(2) == 0 {
			s.Resources[thisPtr].unlocked()
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
		if lvl := c.Arg(1); pLock != 0 && res != nil {
			w, h := max(res.Width>>lvl, 1), max(res.Height>>lvl, 1)
			pitch := texPitch(w, res.Format)
			slice := pitch * texRows(h, res.Format)
			c.Write32(pLock+0, pitch)
			c.Write32(pLock+4, slice)
			if lvl == 0 {
				c.Write32(pLock+8, s.guestBuf(c, res))
			} else { // lower levels are not sampled
				c.Write32(pLock+8, s.mipScratch(c, slice*max(res.Depth>>lvl, 1)))
			}
		}
	case 20: // UnlockBox(this, Level)
		if c.Arg(1) == 0 {
			s.Resources[thisPtr].unlocked()
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) handleIDirect3DSurface9(c *cpu.CPU, winSlot int) {
	if s.drawLogging() && winSlot >= 11 {
		fmt.Printf("[Surface f%d] %x slot %d (%x, %x, %x)\n", s.FrameCount+1, c.Arg(0), winSlot, c.Arg(1), c.Arg(2), c.Arg(3))
	}
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
	case 14: // UnlockRect(this)
		s.Resources[thisPtr].unlocked() // level 0 of its texture when it is GetSurfaceLevel(0)
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
		if len(drawLogFrames) > 0 {
			fmt.Printf("[D3D9] IDirect3DSwapChain9::Present(%x) at frame %d\n", c.Arg(0), s.FrameCount)
		}
		s.FrameCount++
		s.publishStatus(c)
	case 5: // GetBackBuffer(this, iBackBuffer, Type, ppBackBuffer)
		pp := c.Arg(3)
		if pp != 0 {
			c.Write32(pp, s.defaultBackBuf)
			s.addRef(s.defaultBackBuf)
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
		s.unimplemented(c, "ID3DXBuffer", winSlot)
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
	w, h, fmtCode, pixels, raw := s.decodeDDSInMemory(c, pSrc, srcSize)
	mips := true // D3DX_DEFAULT or 0: a full chain, generated when the file has none
	switch levels := c.Arg(5); levels {
	case 1:
		mips = false
	case 0xFFFFFFFD: // D3DX_FROM_FILE
		hd, _ := readDDSHeader(c, pSrc, srcSize)
		mips = hd.mips > 1
	}
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
			res.Pixels, res.Raw, res.Mips = pixels, s.gpuRaw(raw, w, h, fmtCode), mips
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
	w, h, fmtCode, pixels, raw := s.decodeDDSInMemory(c, pSrc, srcSize)
	if ppTex != 0 {
		tex := s.allocTextureLazy(c, w, h, fmtCode)
		if res := s.Resources[tex]; res != nil && len(pixels) > 0 {
			res.Pixels, res.Raw, res.Mips = pixels, s.gpuRaw(raw, w, h, fmtCode), true // a full chain
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

// D3DXCreateCubeTextureFromFileInMemory(pDevice, pSrcData, SrcDataSize, ppCubeTexture): a DDS cube
// map (six faces, +X -X +Y -Y +Z -Z, each followed by its mip chain); level 0 of each face is
// kept. Anything else gives an empty cube texture (sampled as white).
func (s *Subsystem) thunkD3DXCreateCubeTextureFromFileInMemory(c *cpu.CPU) {
	s.CubeTexCount++
	pSrc, size, ppCubeTex := c.Arg(1), c.Arg(2), c.Arg(3)
	hd, ok := readDDSHeader(c, pSrc, size)
	ok = ok && hd.caps2&ddsAllFaces == ddsAllFaces && hd.w == hd.h
	if ppCubeTex != 0 {
		tex := s.allocCOMObject(c, s.vtCubeTex9, 32)
		if ok {
			pitch := texPitch(hd.w, hd.format)
			res := &BufferDesc{Length: 6 * lockSize(pitch, hd.h, hd.format), Format: hd.format, Width: hd.w, Height: hd.h, Pitch: pitch, Cube: true}
			res.Pixels, res.Raw = s.decodeCube(c.Mem[pSrc+128:pSrc+size], hd.w, hd.format, ddsLevelsSize(hd.w, hd.h, hd.format, hd.mips))
			s.Resources[tex] = res
		}
		c.Write32(ppCubeTex, tex)
	}
	if s.TraceLog {
		fmt.Printf("[D3DX9] D3DXCreateCubeTextureFromFileInMemory(size=%d, count=%d) -> %dx%d format %#x, %d levels, decoded=%v\n",
			size, s.CubeTexCount, hd.w, hd.h, hd.format, hd.mips, ok)
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

func (s *Subsystem) thunkD3DXCompileShader(c *cpu.CPU) {
	// D3DXCompileShader(pSrcData, srcDataLen, pDefines, pInclude, pFunctionName, pProfile, Flags, ppShader, ppErrorMsgs, ppConstantTable)
	ppShader := c.Arg(7)
	ppErr := c.Arg(8)
	ppConst := c.Arg(9)
	if dir := os.Getenv("C32_DUMP_SHADERS"); dir != "" {
		src := c.Bytes(c.Arg(0), c.Arg(1))
		entry, profile := c.ReadCString(c.Arg(4)), c.ReadCString(c.Arg(5))
		s.shaderDumps++
		name := fmt.Sprintf("%04d_%s_%s.hlsl", s.shaderDumps, profile, entry)
		_ = os.MkdirAll(dir, 0755)
		_ = os.WriteFile(filepath.Join(dir, name), src, 0644)
	}
	if s.RealShaders && s.compileShaderReal(c) {
		return
	}
	if ppShader != 0 {
		buf := s.allocD3DXBuffer(c, 64)
		s.stubBlobs[s.Resources[buf].DataPtr] = true
		c.Write32(ppShader, buf)
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
		// DI8DEVCLASS_ALL=0, DI8DEVCLASS_KEYBOARD=3 (or DI8DEVTYPE_KEYBOARD=0x13): one keyboard.
		cont := uint32(1)
		if cb != 0 && (devClass == 0 || devClass == 3 || devClass == 0x13) {
			inst := c.Calloc(1, 580) // sizeof(DIDEVICEINSTANCEA) for DX8
			c.Write32(inst+0, 580)
			kb := []byte{0x61, 0x2B, 0x1D, 0x6F, 0xA0, 0xD5, 0xCF, 0x11, 0xBF, 0xC7, 0x44, 0x45, 0x53, 0x54, 0x00, 0x00}
			copy(c.WBytes(inst+4, 16), kb)  // guidInstance
			copy(c.WBytes(inst+20, 16), kb) // guidProduct
			c.Write32(inst+36, 0x00000413)  // DI8DEVTYPE_KEYBOARD | (DI8DEVTYPEKEYBOARD_PCENH << 8)
			c.WriteCString(inst+40, "Keyboard")
			c.WriteCString(inst+300, "Keyboard")
			if s.TraceLog {
				fmt.Printf("[DInput8] EnumDevices(class=%d) -> reporting keyboard to callback 0x%08x\n", devClass, cb)
			}
			cont, _ = c.CallGuestNested(cb, inst, pvRef)
		}
		// DI8DEVCLASS_GAMECTRL=4 (or DI8DEVTYPE_JOYSTICK/GAMEPAD): every connected controller,
		// identified as an Xbox 360 pad so XInput-aware games read it through XInputGetState.
		for user := uint32(0); cb != 0 && cont != 0 && user < ShmPads && (devClass == 0 || devClass == 4 || devClass == 0x14 || devClass == 0x15); user++ {
			if _, ok := s.readPad(user); !ok {
				continue
			}
			inst := c.Calloc(1, 580)
			c.Write32(inst+0, 580)
			// guidInstance: unique per controller; guidProduct: {VID/PID-0000-0000-0000-"PIDVID"}
			copy(c.WBytes(inst+4, 16), []byte{0x00, 0x00, 0x32, 0xC3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(user)})
			c.Write32(inst+20, 0x028E045E) // VID 0x045E, PID 0x028E (Xbox 360 Controller)
			copy(c.WBytes(inst+30, 6), "PIDVID")
			c.Write32(inst+36, 0x00010215) // DI8DEVTYPE_GAMEPAD | DI8DEVTYPEGAMEPAD_STANDARD<<8 | DIDEVTYPE_HID
			c.WriteCString(inst+40, fmt.Sprintf("Controller %d", user+1))
			c.WriteCString(inst+300, "Controller (XBOX 360 For Windows)")
			fmt.Printf("[DInput8] EnumDevices(class=%d) -> controller %d\n", devClass, user+1)
			cont, _ = c.CallGuestNested(cb, inst, pvRef)
		}
		c.Regs[cpu.RegEAX] = 0
	default:
		s.unimplemented(c, "IDirectInput8A", winSlot)
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
		var buf []byte
		if lpvData != 0 && cbData > 0 && cbData <= 4096 {
			buf = c.WBytes(lpvData, cbData)
		}
		if buf != nil {
			clear(buf)
			if cbData == 256 {
				if s.shm != nil {
					var shmKeys [256]byte
					if copy(shmKeys[:], s.shm[32:]) == len(shmKeys) {
						switch shmKeys[0] {
						case 1:
							s.KeyMu.Lock()
							s.AutoKeysMode = "off"
							s.KeyMu.Unlock()
						case 2:
							s.KeyMu.Lock()
							s.AutoKeysMode = "boot"
							s.KeyMu.Unlock()
						case 3:
							s.KeyMu.Lock()
							s.AutoKeysMode = "loop"
							s.KeyMu.Unlock()
						}
						for dik := 1; dik < 256; dik++ {
							if shmKeys[dik] != 0 {
								buf[dik] = 0x80
							}
						}
					}
				}
				s.KeyMu.Lock()
				mode := s.AutoKeysMode
				for dik := 0; dik < 256; dik++ {
					if s.KeyHoldUntil[dik] > 0 && s.FrameCount <= s.KeyHoldUntil[dik] {
						buf[dik] = 0x80
					}
				}
				s.KeyMu.Unlock()

				if s.InputScript != nil && mode != "off" {
					s.InputScript(s, mode, buf)
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
	case 7, 8, 11, 13, 25: // Acquire, Unacquire, SetDataFormat, SetCooperativeLevel, Poll: nothing to do
		c.Regs[cpu.RegEAX] = 0
	default:
		s.unimplemented(c, "IDirectInputDevice8A", winSlot)
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
		channels := uint16(2)
		sampleRate := uint32(44100)
		bits := uint16(16)
		if pDesc != 0 && pDesc+16 <= uint32(len(c.Mem)) {
			sz := c.Read32(pDesc + 8)
			if sz > 0 && sz <= 16*1024*1024 {
				bufBytes = sz
			}
			if pDesc+20 <= uint32(len(c.Mem)) {
				lpwfx := c.Read32(pDesc + 16)
				if lpwfx != 0 && lpwfx+16 <= uint32(len(c.Mem)) {
					if ch := c.Read16(lpwfx + 2); ch >= 1 && ch <= 2 {
						channels = ch
					}
					if sr := c.Read32(lpwfx + 4); sr >= 8000 && sr <= 96000 {
						sampleRate = sr
					}
					if bps := c.Read16(lpwfx + 14); bps == 8 || bps == 16 {
						bits = bps
					}
				}
			}
		}
		sb := s.allocCOMObject(c, s.vtDSBuffer8, 32)
		bd := &BufferDesc{
			Length:        bufBytes,
			Channels:      channels,
			SampleRate:    sampleRate,
			BitsPerSample: bits,
			Frequency:     sampleRate,
			Volume:        0,
			RefCount:      1,
		}
		s.Resources[sb] = bd
		s.AudioBuffers = append(s.AudioBuffers, bd)
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
		sb := s.allocCOMObject(c, s.vtDSBuffer8, 32)
		var bd *BufferDesc
		if orig := s.Resources[pOrig]; orig != nil {
			bd = &BufferDesc{
				DataPtr:       orig.DataPtr,
				Length:        orig.Length,
				Channels:      orig.Channels,
				SampleRate:    orig.SampleRate,
				BitsPerSample: orig.BitsPerSample,
				Frequency:     orig.Frequency,
				Volume:        orig.Volume,
				RefCount:      1,
				IsDuplicate:   true,
			}
		} else {
			bd = &BufferDesc{
				Length:        65536,
				Channels:      2,
				SampleRate:    44100,
				BitsPerSample: 16,
				Frequency:     44100,
				RefCount:      1,
				IsDuplicate:   true,
			}
		}
		s.Resources[sb] = bd
		s.AudioBuffers = append(s.AudioBuffers, bd)
		if ppDup != 0 {
			c.Write32(ppDup, sb)
		}
		c.Regs[cpu.RegEAX] = 0
	case 6: // SetCooperativeLevel: nothing to do
		c.Regs[cpu.RegEAX] = 0
	default:
		s.unimplemented(c, "IDirectSound8", winSlot)
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
		writePos := uint32(4096)
		if res != nil && res.Length > 0 {
			pos = res.PlayCursor % res.Length
			writePos = (pos + 4096) % res.Length
		}
		if pPlay != 0 {
			c.Write32(pPlay, pos)
		}
		if pWrite != 0 {
			c.Write32(pWrite, writePos)
		}
		c.Regs[cpu.RegEAX] = 0
	case 6: // GetVolume(this, plVolume)
		pVol := c.Arg(1)
		if pVol != 0 {
			vol := int32(0)
			if res != nil {
				vol = res.Volume
			}
			c.Write32(pVol, uint32(vol))
		}
		c.Regs[cpu.RegEAX] = 0
	case 8: // GetFrequency(this, pdwFrequency)
		pFreq := c.Arg(1)
		if pFreq != 0 {
			freq := uint32(44100)
			if res != nil && res.Frequency > 0 {
				freq = res.Frequency
			}
			c.Write32(pFreq, freq)
		}
		c.Regs[cpu.RegEAX] = 0
	case 9: // GetStatus(this, pdwStatus)
		pStatus := c.Arg(1)
		if pStatus != 0 {
			status := uint32(0)
			if res != nil && res.Playing {
				status |= 0x1 // DSBSTATUS_PLAYING
				if res.Looping {
					status |= 0x4 // DSBSTATUS_LOOPING
				}
			}
			c.Write32(pStatus, status)
		}
		c.Regs[cpu.RegEAX] = 0
	case 11: // Lock(this, dwOffset, dwBytes, ppvAudioPtr1, pdwAudioBytes1, ppvAudioPtr2, pdwAudioBytes2, dwFlags)
		dwOffset := c.Arg(1)
		dwBytes := c.Arg(2)
		ppv1 := c.Arg(3)
		pdw1 := c.Arg(4)
		ppv2 := c.Arg(5)
		pdw2 := c.Arg(6)
		dwFlags := c.Arg(7)
		if res != nil && res.Length > 0 {
			base := s.guestBuf(c, res)
			if (dwFlags & 0x2) != 0 { // DSBLOCK_ENTIREBUFFER
				dwBytes = res.Length
			}
			if dwBytes == 0 {
				if ppv1 != 0 {
					c.Write32(ppv1, 0)
				}
				if pdw1 != 0 {
					c.Write32(pdw1, 0)
				}
				if ppv2 != 0 {
					c.Write32(ppv2, 0)
				}
				if pdw2 != 0 {
					c.Write32(pdw2, 0)
				}
				c.Regs[cpu.RegEAX] = 0x80070057 // DSERR_INVALIDPARAM
				break
			}
			if (dwFlags & 0x1) != 0 { // DSBLOCK_FROMWRITECURSOR
				dwOffset = (res.PlayCursor + 4096) % res.Length
			} else {
				dwOffset %= res.Length
			}
			if dwBytes > res.Length {
				dwBytes = res.Length
			}
			bytes1 := dwBytes
			bytes2 := uint32(0)
			if dwOffset+dwBytes > res.Length {
				bytes1 = res.Length - dwOffset
				bytes2 = (dwOffset + dwBytes) - res.Length
			}
			if ppv1 != 0 {
				c.Write32(ppv1, base+dwOffset)
			}
			if pdw1 != 0 {
				c.Write32(pdw1, bytes1)
			}
			if ppv2 != 0 {
				if bytes2 > 0 {
					c.Write32(ppv2, base)
				} else {
					c.Write32(ppv2, 0)
				}
			}
			if pdw2 != 0 {
				c.Write32(pdw2, bytes2)
			}
		} else {
			if ppv1 != 0 {
				c.Write32(ppv1, 0)
			}
			if pdw1 != 0 {
				c.Write32(pdw1, 0)
			}
			if ppv2 != 0 {
				c.Write32(ppv2, 0)
			}
			if pdw2 != 0 {
				c.Write32(pdw2, 0)
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 12: // Play(this, dwReserved1, dwPriority, dwFlags)
		if res != nil {
			res.Playing = true
			res.Looping = (c.Arg(3) & 0x1) != 0
		}
		c.Regs[cpu.RegEAX] = 0
	case 13: // SetCurrentPosition(this, dwNewPosition)
		if res != nil && res.Length > 0 {
			res.PlayCursor = c.Arg(1) % res.Length
		}
		c.Regs[cpu.RegEAX] = 0
	case 14: // SetFormat(this, pcfxFormat)
		lpwfx := c.Arg(1)
		if res != nil && lpwfx != 0 && lpwfx+16 <= uint32(len(c.Mem)) {
			if ch := c.Read16(lpwfx + 2); ch >= 1 && ch <= 2 {
				res.Channels = ch
			}
			if sr := c.Read32(lpwfx + 4); sr >= 8000 && sr <= 96000 {
				res.SampleRate = sr
				res.Frequency = sr
			}
			if bps := c.Read16(lpwfx + 14); bps == 8 || bps == 16 {
				res.BitsPerSample = bps
			}
		}
		c.Regs[cpu.RegEAX] = 0
	case 15: // SetVolume(this, lVolume)
		if res != nil {
			res.Volume = int32(c.Arg(1))
		}
		c.Regs[cpu.RegEAX] = 0
	case 17: // SetFrequency(this, dwFrequency)
		if res != nil {
			res.Frequency = c.Arg(1)
		}
		c.Regs[cpu.RegEAX] = 0
	case 18: // Stop(this)
		if res != nil {
			res.Playing = false
		}
		c.Regs[cpu.RegEAX] = 0
	case 19: // Unlock(this, pvAudioPtr1, dwAudioBytes1, pvAudioPtr2, dwAudioBytes2)
		c.Regs[cpu.RegEAX] = 0
	default:
		s.unimplemented(c, "IDirectSoundBuffer8", winSlot)
		c.Regs[cpu.RegEAX] = 0
	}
	c.Ret()
}

// readPad returns the controller record the viewer wrote for user (0..3): the 12-byte
// XINPUT_GAMEPAD (wButtons, bLeftTrigger, bRightTrigger, sThumbLX/LY/RX/RY) and whether the
// pad counts as connected. Games typically enumerate controllers once at startup, so the first
// query waits (up to 3 s) for the viewer's controller scan; with a viewer the first
// ShmHotplugPads slots always report connected (neutral when empty) so pads can be plugged in
// later, and any pad reported once stays connected (neutral if it drops, e.g. Bluetooth sleep).
func (s *Subsystem) readPad(user uint32) ([12]byte, bool) {
	var pad [12]byte
	if s.shm == nil || user >= ShmPads {
		return pad, false
	}
	if !s.padWaited {
		s.padWaited = true
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if binary.LittleEndian.Uint32(s.shm[ShmPadOffset:]) == ShmPadReady {
				break
			}
		}
	}
	if !s.padViewer {
		if binary.LittleEndian.Uint32(s.shm[ShmPadOffset:]) == ShmPadReady {
			s.padViewer = true
		}
	}
	// With a viewer present, the first ShmHotplugPads slots always exist (idle when empty), so
	// controllers connected after the game's one-time enumeration still reach it.
	always := s.padViewer && user < ShmHotplugPads
	var rec [ShmPadSize]byte
	if copy(rec[:], s.shm[ShmPadOffset+4+user*ShmPadSize:]); rec[0] == 0 {
		return pad, s.padSeen[user] || always
	}
	s.padSeen[user] = true
	copy(pad[:], rec[4:16])
	return pad, true
}

const errDeviceNotConnected = 0x48F

// XInputGetState(dwUserIndex, pState): XINPUT_STATE { DWORD dwPacketNumber; XINPUT_GAMEPAD Gamepad; }
func (s *Subsystem) thunkXInputGetState(c *cpu.CPU) {
	user, p := c.Arg(0), c.Arg(1)
	pad, ok := s.readPad(user)
	if !ok {
		c.Regs[cpu.RegEAX] = errDeviceNotConnected
		c.Ret()
		return
	}
	if pad != s.padLast[user] { // the packet number changes whenever the state changes
		s.padLast[user] = pad
		s.padPacket[user]++
	}
	if p != 0 {
		c.Write32(p, s.padPacket[user])
		copy(c.WBytes(p+4, 12), pad[:])
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

// XInputGetCapabilities(dwUserIndex, dwFlags, pCapabilities): XINPUT_CAPABILITIES { BYTE Type;
// BYTE SubType; WORD Flags; XINPUT_GAMEPAD Gamepad; XINPUT_VIBRATION Vibration; }
func (s *Subsystem) thunkXInputGetCapabilities(c *cpu.CPU) {
	user, p := c.Arg(0), c.Arg(2)
	if _, ok := s.readPad(user); !ok {
		c.Regs[cpu.RegEAX] = errDeviceNotConnected
		c.Ret()
		return
	}
	if p != 0 {
		clear(c.WBytes(p, 20))
		c.Write8(p, 1)         // XINPUT_DEVTYPE_GAMEPAD
		c.Write8(p+1, 1)       // XINPUT_DEVSUBTYPE_GAMEPAD
		c.Write16(p+4, 0xF3FF) // every button
		c.Write16(p+6, 0xFFFF)
		for i := uint32(0); i < 4; i++ {
			c.Write16(p+8+2*i, 0xFFC0) // thumbstick resolution
		}
	}
	c.Regs[cpu.RegEAX] = 0
	c.Ret()
}

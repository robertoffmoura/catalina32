// Metal renderer for the emulator's GPU command stream (see C32Metal.h and pkg/d3d9/gpu.go).
// Compiled with ARC.
#import "C32Metal.h"
#import <Metal/Metal.h>
#include <fcntl.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

enum {
    kMagic = 0x47323343, // "C32G"
    kVersion = 5, // 2: gpuDraw gained a per-draw viewport; 3: a scissor rectangle; 4: fog; 5: gpuCopy rects
    kHeaderSize = 4096,
    kCmdLibrary = 1,
    kCmdTexture = 2,
    kCmdTexFree = 3,
    kCmdTarget = 4,
    kCmdClear = 5,
    kCmdCopy = 6,
    kCmdDraw = 7,
    kCmdFrameEnd = 8,
    kDefaultAttr = 0xFFFFFFFF,
    kFirstVertexBuffer = 2,
    kDefaultAttrBuffer = 30,
    kArenaSize = 64 << 20,
    kFramesInFlight = 3,
};

static const MTLPixelFormat kColorFormat = MTLPixelFormatRGBA8Unorm;
static const MTLPixelFormat kDepthFormat = MTLPixelFormatDepth32Float;

// ---- D3D -> Metal --------------------------------------------------------------------------

static MTLVertexFormat vertexFormat(uint32_t d3dType) {
    switch (d3dType) {
    case 0: return MTLVertexFormatFloat;
    case 1: return MTLVertexFormatFloat2;
    case 2: return MTLVertexFormatFloat3;
    case 3: return MTLVertexFormatFloat4;
    case 4: return MTLVertexFormatUChar4Normalized_BGRA; // D3DCOLOR
    case 5: return MTLVertexFormatUChar4;                // UBYTE4
    case 6: return MTLVertexFormatShort2;
    case 7: return MTLVertexFormatShort4;
    case 8: return MTLVertexFormatUChar4Normalized;
    case 9: return MTLVertexFormatShort2Normalized;
    case 10: return MTLVertexFormatShort4Normalized;
    case 11: return MTLVertexFormatUShort2Normalized;
    case 12: return MTLVertexFormatUShort4Normalized;
    case 13: return MTLVertexFormatUInt1010102Normalized; // UDEC3 (approximation)
    case 14: return MTLVertexFormatInt1010102Normalized;  // DEC3N
    case 15: return MTLVertexFormatHalf2;
    case 16: return MTLVertexFormatHalf4;
    }
    return MTLVertexFormatFloat4;
}

static MTLBlendFactor blendFactor(uint32_t b, BOOL alpha) {
    switch (b) {
    case 1: return MTLBlendFactorZero;
    case 2: return MTLBlendFactorOne;
    case 3: return MTLBlendFactorSourceColor;
    case 4: return MTLBlendFactorOneMinusSourceColor;
    case 5: return MTLBlendFactorSourceAlpha;
    case 6: return MTLBlendFactorOneMinusSourceAlpha;
    case 7: return MTLBlendFactorDestinationAlpha;
    case 8: return MTLBlendFactorOneMinusDestinationAlpha;
    case 9: return MTLBlendFactorDestinationColor;
    case 10: return MTLBlendFactorOneMinusDestinationColor;
    case 11: return MTLBlendFactorSourceAlphaSaturated;
    case 14: return alpha ? MTLBlendFactorBlendAlpha : MTLBlendFactorBlendColor;
    case 15: return alpha ? MTLBlendFactorOneMinusBlendAlpha : MTLBlendFactorOneMinusBlendColor;
    }
    return MTLBlendFactorOne;
}

static MTLBlendOperation blendOp(uint32_t op) {
    switch (op) {
    case 2: return MTLBlendOperationSubtract;
    case 3: return MTLBlendOperationReverseSubtract;
    case 4: return MTLBlendOperationMin;
    case 5: return MTLBlendOperationMax;
    }
    return MTLBlendOperationAdd;
}

static MTLCompareFunction compareFunc(uint32_t f) {
    switch (f) {
    case 1: return MTLCompareFunctionNever;
    case 2: return MTLCompareFunctionLess;
    case 3: return MTLCompareFunctionEqual;
    case 4: return MTLCompareFunctionLessEqual;
    case 5: return MTLCompareFunctionGreater;
    case 6: return MTLCompareFunctionNotEqual;
    case 7: return MTLCompareFunctionGreaterEqual;
    }
    return MTLCompareFunctionAlways;
}

static MTLColorWriteMask writeMask(uint32_t m) {
    MTLColorWriteMask r = MTLColorWriteMaskNone;
    if (m & 1) r |= MTLColorWriteMaskRed;
    if (m & 2) r |= MTLColorWriteMaskGreen;
    if (m & 4) r |= MTLColorWriteMaskBlue;
    if (m & 8) r |= MTLColorWriteMaskAlpha;
    return r;
}

static MTLSamplerAddressMode addressMode(uint32_t a) {
    switch (a) {
    case 2: return MTLSamplerAddressModeMirrorRepeat;
    case 3: return MTLSamplerAddressModeClampToEdge;
    case 4: return MTLSamplerAddressModeClampToBorderColor;
    case 5: return MTLSamplerAddressModeMirrorClampToEdge;
    }
    return MTLSamplerAddressModeRepeat;
}

static MTLPrimitiveType primitiveType(uint32_t p) {
    switch (p) {
    case 1: return MTLPrimitiveTypePoint;
    case 2: return MTLPrimitiveTypeLine;
    case 3: return MTLPrimitiveTypeLineStrip;
    case 5: return MTLPrimitiveTypeTriangleStrip;
    }
    return MTLPrimitiveTypeTriangle;
}

// Utility functions: clears, copies between targets and presentation.
static NSString *const kUtilitySource = @
    "#include <metal_stdlib>\n"
    "using namespace metal;\n"
    "struct QOut { float4 pos [[position]]; float2 uv; };\n"
    "vertex QOut quad_vs(uint vid [[vertex_id]], constant float4& r [[buffer(0)]], constant float4& uvRect [[buffer(1)]]) {\n"
    "    float2 p = float2(float((vid << 1) & 2), float(vid & 2));\n"
    "    QOut o; o.pos = float4(p * 2.0 - 1.0, r.x, 1.0);\n"
    "    o.uv = mix(uvRect.xy, uvRect.zw, float2(p.x, 1.0 - p.y)); return o;\n"
    "}\n"
    "fragment float4 clear_fs(constant float4& c [[buffer(0)]]) { return c; }\n"
    "fragment float4 copy_fs(QOut in [[stage_in]], texture2d<float> t [[texture(0)]], sampler s [[sampler(0)]]) {\n"
    "    return t.sample(s, in.uv);\n"
    "}\n";

// ---- Command reader -------------------------------------------------------------------------

typedef struct {
    const uint8_t *p, *end;
} Reader;

static uint32_t rd32(Reader *r) {
    if (r->p + 4 > r->end) { r->p = r->end; return 0; }
    uint32_t v;
    memcpy(&v, r->p, 4);
    r->p += 4;
    return v;
}
static float rdf(Reader *r) {
    uint32_t v = rd32(r);
    float f;
    memcpy(&f, &v, 4);
    return f;
}
static const uint8_t *rdbytes(Reader *r, size_t n) {
    size_t padded = (n + 3) & ~(size_t)3;
    if (r->p + padded > r->end) { r->p = r->end; return NULL; }
    const uint8_t *b = r->p;
    r->p += padded;
    return b;
}

@implementation C32MetalRenderer {
    NSString *_path;
    CAMetalLayer *_layer;
    id<MTLDevice> _device;
    id<MTLCommandQueue> _queue;
    uint8_t *_shm;
    size_t _shmSize;
    ino_t _inode;          // of the mapped file: a new engine run replaces it
    double _lastCheck;
    uint32_t _slotSize, _slotCount;
    NSThread *_thread;
    atomic_bool _stop;
    uint32_t _errors;
    NSString *_errorDir;

    NSMutableDictionary<NSNumber *, id> *_vfuncs, *_ffuncs; // id<MTLFunction> or NSNull (failed)
    NSMutableDictionary<NSNumber *, id<MTLTexture>> *_textures; // textures and render targets
    NSMutableDictionary<NSString *, id<MTLTexture>> *_depths;   // per target size
    NSMutableDictionary<NSString *, id> *_pipelines;            // id<MTLRenderPipelineState> or NSNull
    NSMutableDictionary<NSString *, id<MTLDepthStencilState>> *_depthStates;
    NSMutableDictionary<NSString *, id<MTLSamplerState>> *_samplers;
    id<MTLTexture> _backBuffer, _white2D, _whiteCube, _white3D;
    id<MTLBuffer> _defaultAttr, _zeros;
    id<MTLBuffer> _arena[kFramesInFlight];
    int _arenaIndex;
    NSUInteger _arenaOffset;
    dispatch_semaphore_t _inFlight;
    id<MTLLibrary> _utility;
    id<MTLRenderPipelineState> _clearPipes[2], _copyPipe, _presentPipe;
    id<MTLSamplerState> _linear;

    // Frame state.
    id<MTLCommandBuffer> _cb;
    id<MTLRenderCommandEncoder> _enc;
    id<MTLTexture> _target, _targetDepth;
    MTLWinding _winding; // D3D9's front faces (C32_METAL_WINDING=ccw flips the convention)
}

- (instancetype)initWithPath:(NSString *)path layer:(CAMetalLayer *)layer {
    if ((self = [super init])) {
        _path = [path copy];
        _layer = layer;
        _device = layer.device ?: MTLCreateSystemDefaultDevice();
        _layer.device = _device;
        _layer.pixelFormat = MTLPixelFormatBGRA8Unorm;
        _layer.framebufferOnly = YES;
        _queue = [_device newCommandQueue];
        _vfuncs = [NSMutableDictionary new];
        _ffuncs = [NSMutableDictionary new];
        _textures = [NSMutableDictionary new];
        _depths = [NSMutableDictionary new];
        _pipelines = [NSMutableDictionary new];
        _depthStates = [NSMutableDictionary new];
        _samplers = [NSMutableDictionary new];
        _inFlight = dispatch_semaphore_create(kFramesInFlight);
        _errorDir = [[path stringByDeletingLastPathComponent] stringByAppendingPathComponent:@"msl_errors"];
        atomic_init(&_stop, false);
        const char *wind = getenv("C32_METAL_WINDING");
        _winding = (wind && strcmp(wind, "ccw") == 0) ? MTLWindingCounterClockwise : MTLWindingClockwise;
        [self setUpResources];
    }
    return self;
}

- (void)log:(NSString *)format, ... {
    va_list ap;
    va_start(ap, format);
    NSString *s = [[NSString alloc] initWithFormat:format arguments:ap];
    va_end(ap);
    fprintf(stderr, "[Metal] %s\n", s.UTF8String);
}

- (id<MTLTexture>)whiteTexture:(MTLTextureType)type {
    MTLTextureDescriptor *d = [MTLTextureDescriptor new];
    d.textureType = type;
    d.pixelFormat = kColorFormat;
    d.width = d.height = 1;
    d.depth = 1;
    d.usage = MTLTextureUsageShaderRead;
    id<MTLTexture> t = [_device newTextureWithDescriptor:d];
    uint32_t white = 0xFFFFFFFF;
    int slices = type == MTLTextureTypeCube ? 6 : 1;
    for (int s = 0; s < slices; s++) {
        [t replaceRegion:MTLRegionMake3D(0, 0, 0, 1, 1, 1) mipmapLevel:0 slice:s withBytes:&white bytesPerRow:4 bytesPerImage:4];
    }
    return t;
}

- (void)setUpResources {
    _white2D = [self whiteTexture:MTLTextureType2D];
    _whiteCube = [self whiteTexture:MTLTextureTypeCube];
    _white3D = [self whiteTexture:MTLTextureType3D];
    _backBuffer = [self renderTargetWidth:1280 height:720];
    float def[4] = {0, 0, 0, 1};
    _defaultAttr = [_device newBufferWithBytes:def length:sizeof def options:MTLResourceStorageModeShared];
    _zeros = [_device newBufferWithLength:4096 options:MTLResourceStorageModeShared];
    for (int i = 0; i < kFramesInFlight; i++) {
        _arena[i] = [_device newBufferWithLength:kArenaSize options:MTLResourceStorageModeShared];
    }
    NSError *err = nil;
    _utility = [_device newLibraryWithSource:kUtilitySource options:nil error:&err];
    if (!_utility) {
        [self log:@"utility library: %@", err];
        return;
    }
    id<MTLFunction> qv = [_utility newFunctionWithName:@"quad_vs"];
    for (int color = 0; color < 2; color++) {
        MTLRenderPipelineDescriptor *pd = [MTLRenderPipelineDescriptor new];
        pd.vertexFunction = qv;
        pd.fragmentFunction = [_utility newFunctionWithName:@"clear_fs"];
        pd.colorAttachments[0].pixelFormat = kColorFormat;
        pd.colorAttachments[0].writeMask = color ? MTLColorWriteMaskAll : MTLColorWriteMaskNone;
        pd.depthAttachmentPixelFormat = kDepthFormat;
        _clearPipes[color] = [_device newRenderPipelineStateWithDescriptor:pd error:&err];
    }
    MTLRenderPipelineDescriptor *cd = [MTLRenderPipelineDescriptor new];
    cd.vertexFunction = qv;
    cd.fragmentFunction = [_utility newFunctionWithName:@"copy_fs"];
    cd.colorAttachments[0].pixelFormat = kColorFormat;
    _copyPipe = [_device newRenderPipelineStateWithDescriptor:cd error:&err];
    cd.colorAttachments[0].pixelFormat = _layer.pixelFormat;
    _presentPipe = [_device newRenderPipelineStateWithDescriptor:cd error:&err];
    MTLSamplerDescriptor *sd = [MTLSamplerDescriptor new];
    sd.minFilter = sd.magFilter = MTLSamplerMinMagFilterLinear;
    sd.sAddressMode = sd.tAddressMode = MTLSamplerAddressModeClampToEdge;
    _linear = [_device newSamplerStateWithDescriptor:sd];
}

- (id<MTLTexture>)renderTargetWidth:(NSUInteger)w height:(NSUInteger)h {
    MTLTextureDescriptor *d = [MTLTextureDescriptor texture2DDescriptorWithPixelFormat:kColorFormat width:w height:h mipmapped:NO];
    d.usage = MTLTextureUsageRenderTarget | MTLTextureUsageShaderRead;
    d.storageMode = MTLStorageModePrivate;
    return [_device newTextureWithDescriptor:d];
}

- (id<MTLTexture>)depthForWidth:(NSUInteger)w height:(NSUInteger)h {
    NSString *key = [NSString stringWithFormat:@"%lux%lu", (unsigned long)w, (unsigned long)h];
    id<MTLTexture> t = _depths[key];
    if (!t) {
        MTLTextureDescriptor *d = [MTLTextureDescriptor texture2DDescriptorWithPixelFormat:kDepthFormat width:w height:h mipmapped:NO];
        d.usage = MTLTextureUsageRenderTarget;
        d.storageMode = MTLStorageModePrivate;
        t = [_device newTextureWithDescriptor:d];
        _depths[key] = t;
    }
    return t;
}

// ---- Shared memory and the frame loop -----------------------------------------------------

- (volatile uint32_t *)word:(size_t)off {
    return (volatile uint32_t *)(_shm + off);
}

- (BOOL)mapStream {
    int fd = open(_path.fileSystemRepresentation, O_RDWR);
    if (fd < 0) return NO;
    struct stat st;
    BOOL ok = NO;
    if (fstat(fd, &st) == 0 && st.st_size > kHeaderSize) {
        void *p = mmap(NULL, (size_t)st.st_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
        if (p != MAP_FAILED) {
            uint8_t *m = (uint8_t *)p;
            uint32_t magic = atomic_load((_Atomic uint32_t *)m);
            if (magic == kMagic && *(uint32_t *)(m + 4) == kVersion) {
                _shm = m;
                _shmSize = (size_t)st.st_size;
                _inode = st.st_ino;
                _slotSize = *(uint32_t *)(m + 8);
                _slotCount = *(uint32_t *)(m + 12);
                ok = YES;
            } else {
                munmap(p, (size_t)st.st_size);
            }
        }
    }
    close(fd);
    if (ok) {
        atomic_store((_Atomic uint32_t *)(_shm + 16), 1); // ready
        [self log:@"GPU command stream connected (%u slots of %u MB)", _slotCount, _slotSize >> 20];
    }
    return ok;
}

- (void)start {
    _thread = [[NSThread alloc] initWithTarget:self selector:@selector(run) object:nil];
    _thread.name = @"C32 Metal";
    _thread.qualityOfService = NSQualityOfServiceUserInteractive;
    [_thread start];
}

- (void)stop {
    atomic_store(&_stop, true);
    if (_shm) atomic_store((_Atomic uint32_t *)(_shm + 16), 0);
}

- (void)run {
    while (!atomic_load(&_stop)) {
        @autoreleasepool {
            if (!_shm) {
                if (![self mapStream]) {
                    usleep(200000);
                    continue;
                }
            }
            // The oldest written slot.
            int k = -1;
            uint64_t best = 0;
            for (uint32_t i = 0; i < _slotCount; i++) {
                uint32_t state = atomic_load((_Atomic uint32_t *)(_shm + 32 + 16 * i + 12));
                uint64_t frame = *(uint64_t *)(_shm + 32 + 16 * i);
                if (state == 1 && (k < 0 || frame < best)) {
                    k = (int)i;
                    best = frame;
                }
            }
            if (k < 0) {
                // Idle: every second, check that the file is still the one mapped (a new engine
                // run replaces it) and remap if not.
                double now = CFAbsoluteTimeGetCurrent();
                if (now - _lastCheck > 1.0) {
                    _lastCheck = now;
                    struct stat st;
                    if (stat(_path.fileSystemRepresentation, &st) != 0 || st.st_ino != _inode) {
                        munmap(_shm, _shmSize);
                        _shm = NULL;
                        [_textures removeAllObjects];
                        [_vfuncs removeAllObjects];
                        [_ffuncs removeAllObjects];
                        [_pipelines removeAllObjects];
                        continue;
                    }
                }
                usleep(250);
                continue;
            }
            _Atomic uint32_t *state = (_Atomic uint32_t *)(_shm + 32 + 16 * k + 12);
            atomic_store(state, 2);
            uint32_t length = *(uint32_t *)(_shm + 32 + 16 * k + 8);
            const uint8_t *data = _shm + kHeaderSize + (size_t)k * _slotSize;
            if (length <= _slotSize) {
                [self drawFrame:data length:length];
            }
            atomic_store(state, 0);
        }
    }
}

// ---- Frame drawing --------------------------------------------------------------------------

// arena copies bytes into this frame's upload buffer (256-byte aligned) and returns the offset,
// or uses a one-off buffer when the arena is full.
- (id<MTLBuffer>)upload:(const void *)bytes length:(NSUInteger)n offset:(NSUInteger *)off {
    NSUInteger o = (_arenaOffset + 255) & ~(NSUInteger)255;
    if (n == 0) {
        *off = 0;
        return _zeros;
    }
    if (o + n > kArenaSize) {
        *off = 0;
        return [_device newBufferWithBytes:bytes length:n options:MTLResourceStorageModeShared];
    }
    memcpy((uint8_t *)_arena[_arenaIndex].contents + o, bytes, n);
    _arenaOffset = o + n;
    *off = o;
    return _arena[_arenaIndex];
}

- (void)endPass {
    if (_enc) {
        [_enc endEncoding];
        _enc = nil;
    }
}

- (void)ensurePass {
    if (_enc) return;
    MTLRenderPassDescriptor *rp = [MTLRenderPassDescriptor renderPassDescriptor];
    rp.colorAttachments[0].texture = _target;
    rp.colorAttachments[0].loadAction = MTLLoadActionLoad;
    rp.colorAttachments[0].storeAction = MTLStoreActionStore;
    rp.depthAttachment.texture = _targetDepth;
    rp.depthAttachment.loadAction = MTLLoadActionLoad;
    rp.depthAttachment.storeAction = MTLStoreActionStore;
    _enc = [_cb renderCommandEncoderWithDescriptor:rp];
    MTLViewport vp = {0, 0, (double)_target.width, (double)_target.height, 0, 1};
    [_enc setViewport:vp];
    [_enc setFrontFacingWinding:_winding];
}

- (void)drawFrame:(const uint8_t *)data length:(uint32_t)length {
    dispatch_semaphore_wait(_inFlight, DISPATCH_TIME_FOREVER);
    _arenaIndex = (_arenaIndex + 1) % kFramesInFlight;
    _arenaOffset = 0;
    _cb = [_queue commandBuffer];
    dispatch_semaphore_t sem = _inFlight;
    [_cb addCompletedHandler:^(id<MTLCommandBuffer> b) {
        (void)b;
        dispatch_semaphore_signal(sem);
    }];
    _target = _backBuffer;
    _targetDepth = [self depthForWidth:_backBuffer.width height:_backBuffer.height];
    Reader r = {data, data + length};
    BOOL presented = NO;
    while (r.p + 8 <= r.end) {
        uint32_t op = rd32(&r), n = rd32(&r);
        if (n % 4 != 0 || r.p + n > r.end) break;
        Reader c = {r.p, r.p + n};
        r.p += n;
        switch (op) {
        case kCmdLibrary: [self library:&c]; break;
        case kCmdTexture: [self texture:&c]; break;
        case kCmdTexFree: [_textures removeObjectForKey:@(rd32(&c))]; break;
        case kCmdTarget: [self target:&c]; break;
        case kCmdClear: [self clear:&c]; break;
        case kCmdCopy: [self copy:&c]; break;
        case kCmdDraw: [self draw:&c]; break;
        case kCmdFrameEnd:
            [self present];
            presented = YES;
            break;
        }
    }
    if (!presented) [self endPass];
    [_cb commit];
    _cb = nil;
}

- (void)reportError {
    _errors++;
    if (_shm) atomic_store((_Atomic uint32_t *)(_shm + 20), _errors);
}

- (void)library:(Reader *)r {
    uint32_t libID = rd32(r), stage = rd32(r), n = rd32(r);
    const uint8_t *src = rdbytes(r, n);
    if (!src) return;
    NSString *source = [[NSString alloc] initWithBytes:src length:n encoding:NSUTF8StringEncoding];
    MTLCompileOptions *opt = [MTLCompileOptions new];
    opt.fastMathEnabled = NO; // keep IEEE semantics (inf/NaN), as the software renderer has
    NSError *err = nil;
    id<MTLLibrary> lib = [_device newLibraryWithSource:source options:opt error:&err];
    id<MTLFunction> fn = [lib newFunctionWithName:stage == 0 ? @"vs_main" : @"ps_main"];
    NSMutableDictionary *dict = stage == 0 ? _vfuncs : _ffuncs;
    if (!fn) {
        [[NSFileManager defaultManager] createDirectoryAtPath:_errorDir withIntermediateDirectories:YES attributes:nil error:NULL];
        NSString *file = [_errorDir stringByAppendingPathComponent:[NSString stringWithFormat:@"lib%04u.metal", libID]];
        [source writeToFile:file atomically:NO encoding:NSUTF8StringEncoding error:NULL];
        [self log:@"library %u (%s) failed to compile, source in %@:\n%@", libID, stage == 0 ? "vertex" : "fragment", file, err.localizedDescription];
        [self reportError];
        dict[@(libID)] = [NSNull null];
        return;
    }
    dict[@(libID)] = fn;
}

- (void)texture:(Reader *)r {
    uint32_t texID = rd32(r), w = rd32(r), h = rd32(r), fmt = rd32(r);
    BOOL cube = (fmt & 0x100) != 0; // six faces, +X -X +Y -Y +Z -Z, one after another
    BOOL mips = (fmt & 0x200) != 0 && !cube; // D3D gives it a mip chain: generate one
    fmt &= 0xFF;
    MTLPixelFormat pf = kColorFormat;
    size_t rowBytes = (size_t)w * 4, size = rowBytes * h;
    if (fmt >= 2 && fmt <= 4) { // BC1/BC2/BC3: rows of 4x4 blocks
        pf = fmt == 2 ? MTLPixelFormatBC1_RGBA : (fmt == 3 ? MTLPixelFormatBC2_RGBA : MTLPixelFormatBC3_RGBA);
        rowBytes = (size_t)(w / 4) * (fmt == 2 ? 8 : 16);
        size = rowBytes * (h / 4);
    } else if (fmt != 1) {
        return;
    }
    size_t faceSize = size;
    if (cube) {
        if (w != h) return;
        size *= 6;
    }
    const uint8_t *px = rdbytes(r, size);
    if (!px || w == 0 || h == 0) return;
    MTLTextureDescriptor *d = cube ? [MTLTextureDescriptor textureCubeDescriptorWithPixelFormat:pf size:w mipmapped:NO]
                                   : [MTLTextureDescriptor texture2DDescriptorWithPixelFormat:pf width:w height:h mipmapped:NO];
    d.usage = MTLTextureUsageShaderRead;
    id<MTLTexture> t = [_device newTextureWithDescriptor:d]; // a new texture: frames in flight keep the old one
    if (!t) return; // BC formats need a GPU that supports them (every Mac with Metal 2 does)
    for (NSUInteger face = 0; face < (cube ? 6 : 1); face++) {
        [t replaceRegion:MTLRegionMake2D(0, 0, w, h) mipmapLevel:0 slice:face withBytes:px + face * faceSize
             bytesPerRow:rowBytes bytesPerImage:0]; // one image per call (0 except for 3D and array textures)
    }
    if (mips) t = [self mipmapped:t];
    _textures[@(texID)] = t;
}

// mipmapped returns an RGBA copy of t with a full mip chain, generated on the GPU as D3DX
// generates the chain of a file without one (Metal cannot generate mipmaps of compressed
// formats, so level 0 is drawn into an uncompressed texture first). The work goes into the
// frame's command buffer ahead of the draws that sample it.
- (id<MTLTexture>)mipmapped:(id<MTLTexture>)t {
    MTLTextureDescriptor *d = [MTLTextureDescriptor texture2DDescriptorWithPixelFormat:kColorFormat width:t.width height:t.height mipmapped:YES];
    d.usage = MTLTextureUsageShaderRead | MTLTextureUsageRenderTarget;
    d.storageMode = MTLStorageModePrivate;
    id<MTLTexture> m = [_device newTextureWithDescriptor:d];
    if (!m || !_cb) return t;
    [self endPass];
    MTLViewport vp = {0, 0, (double)t.width, (double)t.height, 0, 1};
    float fullUV[4] = {0, 0, 1, 1};
    [self copyFrom:t to:m pipeline:_copyPipe viewport:vp uv:fullUV load:MTLLoadActionDontCare];
    id<MTLBlitCommandEncoder> blit = [_cb blitCommandEncoder];
    [blit generateMipmapsForTexture:m];
    [blit endEncoding];
    return m;
}

// renderTargetForID returns texture tid as a render target of w x h, (re)creating it if it is
// not one yet (a texture the game copies into or draws into from now on).
- (id<MTLTexture>)renderTargetForID:(uint32_t)tid width:(uint32_t)w height:(uint32_t)h {
    id<MTLTexture> t = _textures[@(tid)];
    if (!t || !(t.usage & MTLTextureUsageRenderTarget) || t.width != w || t.height != h) {
        t = [self renderTargetWidth:MAX(w, 1u) height:MAX(h, 1u)];
        _textures[@(tid)] = t;
    }
    return t;
}

- (void)target:(Reader *)r {
    uint32_t tid = rd32(r), w = rd32(r), h = rd32(r);
    [self endPass];
    _target = tid == 0 ? _backBuffer : [self renderTargetForID:tid width:w height:h];
    _targetDepth = [self depthForWidth:_target.width height:_target.height];
}

- (id<MTLDepthStencilState>)depthStateCompare:(MTLCompareFunction)f write:(BOOL)write {
    NSString *key = [NSString stringWithFormat:@"%lu/%d", (unsigned long)f, write];
    id<MTLDepthStencilState> s = _depthStates[key];
    if (!s) {
        MTLDepthStencilDescriptor *d = [MTLDepthStencilDescriptor new];
        d.depthCompareFunction = f;
        d.depthWriteEnabled = write;
        s = [_device newDepthStencilStateWithDescriptor:d];
        _depthStates[key] = s;
    }
    return s;
}

- (void)clear:(Reader *)r {
    uint32_t flags = rd32(r), color = rd32(r);
    float z = rdf(r);
    int32_t x0 = (int32_t)rd32(r), y0 = (int32_t)rd32(r), x1 = (int32_t)rd32(r), y1 = (int32_t)rd32(r);
    if (!(flags & 3)) return;
    int tw = (int)_target.width, th = (int)_target.height;
    x0 = MAX(x0, 0); y0 = MAX(y0, 0); x1 = MIN(x1, tw - 1); y1 = MIN(y1, th - 1);
    if (x0 > x1 || y0 > y1) return;
    [self ensurePass];
    [_enc setRenderPipelineState:_clearPipes[(flags & 1) ? 1 : 0]];
    [_enc setDepthStencilState:[self depthStateCompare:MTLCompareFunctionAlways write:(flags & 2) != 0]];
    [_enc setCullMode:MTLCullModeNone];
    [_enc setViewport:(MTLViewport){0, 0, (double)tw, (double)th, 0, 1}]; // not the last draw's: the rectangle is the region
    MTLScissorRect sc = {(NSUInteger)x0, (NSUInteger)y0, (NSUInteger)(x1 - x0 + 1), (NSUInteger)(y1 - y0 + 1)};
    [_enc setScissorRect:sc];
    float zv[4] = {z, 0, 0, 0};
    float fullUV[4] = {0, 0, 1, 1};
    float cv[4] = {((color >> 16) & 0xFF) / 255.0f, ((color >> 8) & 0xFF) / 255.0f, (color & 0xFF) / 255.0f, (color >> 24) / 255.0f};
    [_enc setVertexBytes:zv length:sizeof zv atIndex:0];
    [_enc setVertexBytes:fullUV length:sizeof fullUV atIndex:1];
    [_enc setFragmentBytes:cv length:sizeof cv atIndex:0];
    [_enc drawPrimitives:MTLPrimitiveTypeTriangle vertexStart:0 vertexCount:3];
    MTLScissorRect full = {0, 0, _target.width, _target.height};
    [_enc setScissorRect:full];
}

- (id<MTLTexture>)textureForID:(uint32_t)tid {
    return tid == 0 ? _backBuffer : _textures[@(tid)];
}

// copyFrom draws uvRect of src scaled over vp of dst (no depth), as StretchRect does.
- (void)copyFrom:(id<MTLTexture>)src to:(id<MTLTexture>)dst pipeline:(id<MTLRenderPipelineState>)pipe viewport:(MTLViewport)vp uv:(const float *)uvRect load:(MTLLoadAction)load {
    MTLRenderPassDescriptor *rp = [MTLRenderPassDescriptor renderPassDescriptor];
    rp.colorAttachments[0].texture = dst;
    rp.colorAttachments[0].loadAction = load;
    rp.colorAttachments[0].clearColor = MTLClearColorMake(0, 0, 0, 1);
    rp.colorAttachments[0].storeAction = MTLStoreActionStore;
    id<MTLRenderCommandEncoder> e = [_cb renderCommandEncoderWithDescriptor:rp];
    [e setViewport:vp];
    [e setRenderPipelineState:pipe];
    float zv[4] = {0, 0, 0, 0};
    [e setVertexBytes:zv length:sizeof zv atIndex:0];
    [e setVertexBytes:uvRect length:4 * sizeof(float) atIndex:1];
    [e setFragmentTexture:src atIndex:0];
    [e setFragmentSamplerState:_linear atIndex:0];
    [e drawPrimitives:MTLPrimitiveTypeTriangle vertexStart:0 vertexCount:3];
    [e endEncoding];
}

- (void)copy:(Reader *)r {
    uint32_t srcID = rd32(r), dstID = rd32(r), w = rd32(r), h = rd32(r);
    int32_t sx0 = (int32_t)rd32(r), sy0 = (int32_t)rd32(r), sx1 = (int32_t)rd32(r), sy1 = (int32_t)rd32(r);
    int32_t dx0 = (int32_t)rd32(r), dy0 = (int32_t)rd32(r), dx1 = (int32_t)rd32(r), dy1 = (int32_t)rd32(r);
    id<MTLTexture> src = [self textureForID:srcID];
    id<MTLTexture> dst = dstID == 0 ? _backBuffer : [self renderTargetForID:dstID width:w height:h];
    if (!src || !dst || src == dst) return;
    [self endPass];
    // The destination is Y-down (row 0 at the top, as D3D9 surfaces are); the viewport's origin
    // is also top-left, so dst rows map directly. The source is sampled by quad_vs's UV, which
    // already flips Y (see quad_vs), so the source rect's Y range is used as-is too.
    double sw = (double)src.width, sh = (double)src.height;
    float uvRect[4] = {(float)(sx0 / sw), (float)(sy0 / sh), (float)((sx1 + 1) / sw), (float)((sy1 + 1) / sh)};
    MTLViewport vp = {(double)dx0, (double)dy0, (double)(dx1 - dx0 + 1), (double)(dy1 - dy0 + 1), 0, 1};
    [self copyFrom:src to:dst pipeline:_copyPipe viewport:vp uv:uvRect load:MTLLoadActionDontCare];
}

- (id<MTLSamplerState>)samplerU:(uint32_t)u v:(uint32_t)v w:(uint32_t)w mag:(uint32_t)mag min:(uint32_t)mn mip:(uint32_t)mip {
    NSString *key = [NSString stringWithFormat:@"%u/%u/%u/%u/%u/%u", u, v, w, mag, mn, mip];
    id<MTLSamplerState> s = _samplers[key];
    if (!s) {
        MTLSamplerDescriptor *d = [MTLSamplerDescriptor new];
        d.sAddressMode = addressMode(u);
        d.tAddressMode = addressMode(v);
        d.rAddressMode = addressMode(w);
        d.magFilter = mag >= 2 ? MTLSamplerMinMagFilterLinear : MTLSamplerMinMagFilterNearest;
        d.minFilter = mn >= 2 ? MTLSamplerMinMagFilterLinear : MTLSamplerMinMagFilterNearest;
        if (mn == 3 || mag == 3) d.maxAnisotropy = 8;
        // D3DTEXF_NONE / POINT / LINEAR (textures without a chain only have level 0 to sample)
        d.mipFilter = mip == 0 ? MTLSamplerMipFilterNotMipmapped : (mip == 1 ? MTLSamplerMipFilterNearest : MTLSamplerMipFilterLinear);
        d.borderColor = MTLSamplerBorderColorOpaqueBlack;
        s = [_device newSamplerStateWithDescriptor:d];
        _samplers[key] = s;
    }
    return s;
}

- (void)draw:(Reader *)r {
    uint32_t vsID = rd32(r), psID = rd32(r), prim = rd32(r), nIdx = rd32(r), nAttr = rd32(r), nStream = rd32(r),
             nTex = rd32(r), nVC = rd32(r), nPC = rd32(r);
    uint32_t vpX = rd32(r), vpY = rd32(r), vpW = rd32(r), vpH = rd32(r);
    uint32_t scOn = rd32(r), scX = rd32(r), scY = rd32(r), scW = rd32(r), scH = rd32(r);
    uint32_t fixedTex = rd32(r);
    uint32_t blend = rd32(r), src = rd32(r), dst = rd32(r), op = rd32(r), srcA = rd32(r), dstA = rd32(r), opA = rd32(r),
             factor = rd32(r), cw = rd32(r), zEnable = rd32(r), zWrite = rd32(r), zFunc = rd32(r), cull = rd32(r),
             alphaFunc = rd32(r);
    float alphaRef = rdf(r);
    uint32_t fogMode = rd32(r);
    float fogStart = rdf(r), fogEnd = rdf(r), fogDensity = rdf(r);
    uint32_t fogColor = rd32(r);

    typedef struct { uint32_t index, stream, offset, type; } Attr;
    Attr attrs[32];
    uint32_t na = 0;
    for (uint32_t i = 0; i < nAttr; i++) {
        Attr a = {rd32(r), rd32(r), rd32(r), rd32(r)};
        if (na < 32) attrs[na++] = a;
    }
    typedef struct { uint32_t stream, stride, length; const uint8_t *data; } Stream;
    Stream streams[16];
    uint32_t ns = 0;
    for (uint32_t i = 0; i < nStream; i++) {
        Stream s;
        s.stream = rd32(r);
        s.stride = rd32(r);
        s.length = rd32(r);
        s.data = rdbytes(r, s.length);
        if (s.data && ns < 16 && s.stream < 16) streams[ns++] = s;
    }
    typedef struct { uint32_t unit, tid, kind, u, v, w, mag, mn, mip; } Tex;
    Tex texs[16];
    uint32_t nt = 0;
    for (uint32_t i = 0; i < nTex; i++) {
        Tex t = {rd32(r), rd32(r), rd32(r), rd32(r), rd32(r), rd32(r), rd32(r), rd32(r), rd32(r)};
        if (nt < 16) texs[nt++] = t;
    }
    const uint8_t *indices = rdbytes(r, (size_t)nIdx * 4);
    const uint8_t *vconst = rdbytes(r, (size_t)nVC * 16);
    const uint8_t *pconst = rdbytes(r, (size_t)nPC * 16);
    if (!indices || nIdx == 0) return;

    id vf = _vfuncs[@(vsID)], ff = _ffuncs[@(psID)];
    if (!vf || !ff || vf == [NSNull null] || ff == [NSNull null]) return;

    // Pipeline: functions, vertex layout, blending and write mask.
    uint32_t strides[16] = {0};
    for (uint32_t i = 0; i < ns; i++) strides[streams[i].stream] = streams[i].stride;
    NSMutableString *key = [NSMutableString stringWithFormat:@"%u/%u/%u/%u/%u/%u/%u/%u/%u/%u", vsID, psID, blend, src, dst, op, srcA, dstA, opA, cw];
    for (uint32_t i = 0; i < na; i++) {
        Attr a = attrs[i];
        uint32_t stride = a.stream < 16 ? strides[a.stream] : 0;
        [key appendFormat:@"|%u:%u:%u:%u:%u", a.index, a.stream, a.offset, a.type, stride];
    }
    id pipe = _pipelines[key];
    if (!pipe) {
        MTLRenderPipelineDescriptor *pd = [MTLRenderPipelineDescriptor new];
        pd.vertexFunction = vf;
        pd.fragmentFunction = ff;
        MTLVertexDescriptor *vd = [MTLVertexDescriptor vertexDescriptor];
        BOOL usesDefault = NO;
        for (uint32_t i = 0; i < na; i++) {
            Attr a = attrs[i];
            if (a.stream == kDefaultAttr || a.stream >= 16 || strides[a.stream] == 0) {
                vd.attributes[a.index].format = MTLVertexFormatFloat4;
                vd.attributes[a.index].offset = 0;
                vd.attributes[a.index].bufferIndex = kDefaultAttrBuffer;
                usesDefault = YES;
                continue;
            }
            vd.attributes[a.index].format = vertexFormat(a.type);
            vd.attributes[a.index].offset = a.offset;
            vd.attributes[a.index].bufferIndex = kFirstVertexBuffer + a.stream;
            vd.layouts[kFirstVertexBuffer + a.stream].stride = strides[a.stream];
            vd.layouts[kFirstVertexBuffer + a.stream].stepFunction = MTLVertexStepFunctionPerVertex;
        }
        if (usesDefault) {
            vd.layouts[kDefaultAttrBuffer].stride = 16;
            vd.layouts[kDefaultAttrBuffer].stepFunction = MTLVertexStepFunctionConstant;
            vd.layouts[kDefaultAttrBuffer].stepRate = 0;
        }
        pd.vertexDescriptor = vd;
        MTLRenderPipelineColorAttachmentDescriptor *ca = pd.colorAttachments[0];
        ca.pixelFormat = kColorFormat;
        ca.writeMask = writeMask(cw);
        if (blend) {
            ca.blendingEnabled = YES;
            // BOTHSRCALPHA / BOTHINVSRCALPHA set both factors.
            uint32_t s2 = src, d2 = dst, sa = srcA, da = dstA;
            if (src == 12) { s2 = 5; d2 = 6; }
            if (src == 13) { s2 = 6; d2 = 5; }
            if (srcA == 12) { sa = 5; da = 6; }
            if (srcA == 13) { sa = 6; da = 5; }
            ca.sourceRGBBlendFactor = blendFactor(s2, NO);
            ca.destinationRGBBlendFactor = blendFactor(d2, NO);
            ca.rgbBlendOperation = blendOp(op);
            ca.sourceAlphaBlendFactor = blendFactor(sa, YES);
            ca.destinationAlphaBlendFactor = blendFactor(da, YES);
            ca.alphaBlendOperation = blendOp(opA);
        }
        pd.depthAttachmentPixelFormat = kDepthFormat;
        NSError *err = nil;
        pipe = [_device newRenderPipelineStateWithDescriptor:pd error:&err];
        if (!pipe) {
            [self log:@"pipeline %@: %@", key, err.localizedDescription];
            [self reportError];
            pipe = [NSNull null];
        }
        _pipelines[key] = pipe;
    }
    if (pipe == [NSNull null]) return;

    [self ensurePass];
    double vx = MIN((double)vpX, (double)_target.width), vy = MIN((double)vpY, (double)_target.height);
    double vw = MAX(1.0, MIN((double)vpW, (double)_target.width - vx));
    double vh = MAX(1.0, MIN((double)vpH, (double)_target.height - vy));
    [_enc setViewport:(MTLViewport){vx, vy, vw, vh, 0, 1}];
    MTLScissorRect sc = {0, 0, _target.width, _target.height}; // the encoder keeps it: set it for every draw
    if (scOn) {
        sc.x = MIN(scX, _target.width);
        sc.y = MIN(scY, _target.height);
        sc.width = MIN(scW, _target.width - sc.x);
        sc.height = MIN(scH, _target.height - sc.y);
        if (sc.width == 0 || sc.height == 0) return; // nothing passes the scissor test
    }
    [_enc setScissorRect:sc];
    [_enc setRenderPipelineState:pipe];
    [_enc setDepthStencilState:[self depthStateCompare:zEnable ? compareFunc(zFunc) : MTLCompareFunctionAlways write:zEnable && zWrite]];
    [_enc setCullMode:cull == 2 ? MTLCullModeFront : (cull == 3 ? MTLCullModeBack : MTLCullModeNone)];
    [_enc setBlendColorRed:((factor >> 16) & 0xFF) / 255.0f green:((factor >> 8) & 0xFF) / 255.0f blue:(factor & 0xFF) / 255.0f alpha:(factor >> 24) / 255.0f];

    NSUInteger off = 0;
    for (uint32_t i = 0; i < ns; i++) {
        id<MTLBuffer> b = [self upload:streams[i].data length:streams[i].length offset:&off];
        [_enc setVertexBuffer:b offset:off atIndex:kFirstVertexBuffer + streams[i].stream];
    }
    [_enc setVertexBuffer:_defaultAttr offset:0 atIndex:kDefaultAttrBuffer];
    id<MTLBuffer> vb = [self upload:vconst length:(NSUInteger)nVC * 16 offset:&off];
    [_enc setVertexBuffer:vb offset:off atIndex:0];
    // D3D9 pixel centres: D3D9 samples pixel (i, j) at viewport position (i, j), Metal at
    // (i + 0.5, j + 0.5), so geometry moves half a pixel right and down (+x, -y in clip space).
    float vstate[4] = {(float)(1.0 / vw), (float)(-1.0 / vh), 0, 0};
    [_enc setVertexBytes:vstate length:sizeof vstate atIndex:1];
    id<MTLBuffer> pb = [self upload:pconst length:(NSUInteger)nPC * 16 offset:&off];
    [_enc setFragmentBuffer:pb offset:off atIndex:0];
    float pstate[12] = {(float)alphaFunc, alphaRef, fixedTex ? 1.0f : 0.0f, 0, // S[0]: alpha test, texture 0 bound
                        (float)fogMode, fogStart, fogEnd, fogDensity,         // S[1], S[2]: fixed-function fog (msl.FogStage)
                        ((fogColor >> 16) & 0xFF) / 255.0f, ((fogColor >> 8) & 0xFF) / 255.0f, (fogColor & 0xFF) / 255.0f, 0};
    [_enc setFragmentBytes:pstate length:sizeof pstate atIndex:1];

    for (uint32_t i = 0; i < nt; i++) {
        Tex t = texs[i];
        id<MTLTexture> tex = t.tid ? [self textureForID:t.tid] : nil;
        MTLTextureType want = t.kind == 1 ? MTLTextureTypeCube : (t.kind == 2 ? MTLTextureType3D : MTLTextureType2D);
        if (!tex || tex.textureType != want || tex == _target) {
            tex = want == MTLTextureTypeCube ? _whiteCube : (want == MTLTextureType3D ? _white3D : _white2D);
        }
        id<MTLSamplerState> s = [self samplerU:t.u v:t.v w:t.w mag:t.mag min:t.mn mip:t.mip];
        [_enc setFragmentTexture:tex atIndex:t.unit];
        [_enc setFragmentSamplerState:s atIndex:t.unit];
        [_enc setVertexTexture:tex atIndex:t.unit];
        [_enc setVertexSamplerState:s atIndex:t.unit];
    }
    id<MTLBuffer> ib = [self upload:indices length:(NSUInteger)nIdx * 4 offset:&off];
    [_enc drawIndexedPrimitives:primitiveType(prim) indexCount:nIdx indexType:MTLIndexTypeUInt32 indexBuffer:ib indexBufferOffset:off];
}

- (void)present {
    [self endPass];
    id<CAMetalDrawable> drawable = [_layer nextDrawable];
    if (drawable) {
        CGSize ds = _layer.drawableSize;
        double scale = MIN(ds.width / 1280.0, ds.height / 720.0);
        double w = 1280.0 * scale, h = 720.0 * scale;
        MTLViewport vp = {(ds.width - w) / 2, (ds.height - h) / 2, w, h, 0, 1};
        float fullUV[4] = {0, 0, 1, 1};
        [self copyFrom:_backBuffer to:drawable.texture pipeline:_presentPipe viewport:vp uv:fullUV load:MTLLoadActionClear];
        [_cb presentDrawable:drawable];
    }
    _framesPresented++;
    if (!_active) {
        _active = YES;
        [self log:@"first frame presented"];
    }
}

@end

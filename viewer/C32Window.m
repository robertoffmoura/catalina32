#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>
#import <Metal/Metal.h>
#import "C32Metal.h"
#import "C32App.h"
#import "C32GL.h"
#import <AudioToolbox/AudioToolbox.h>
#import <IOKit/hid/IOHIDLib.h>
#import <IOKit/hid/IOHIDKeys.h>
#import <IOKit/hid/IOHIDUsageTables.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <unistd.h>
#include <stdint.h>
#include <string.h>

#define FB_W 1280
#define FB_H 720
#define SHM_HEADER 288
#define SHM_AUDIO_OFFSET (SHM_HEADER + FB_W * FB_H * 4)
#define SHM_AUDIO_FRAMES 16384
#define SHM_PAD_OFFSET (SHM_AUDIO_OFFSET + 8 + SHM_AUDIO_FRAMES * 4)
#define SHM_PAD_READY 0x50323343u /* "C32P" */
#define MAX_PADS 4
/* Game state from the engine: u32 flags (below). */
#define SHM_GAME_OFFSET (SHM_PAD_OFFSET + 4 + MAX_PADS * 16)
#define SHM_GAME_POINTER 1u
#define SHM_GAME_ENDED 2u /* the game quit or the engine stopped */
#define SHM_SIZE (SHM_GAME_OFFSET + 4)

static uint8_t *g_shm = NULL;
static uint64_t g_audioReadIdx = 0;

/* ---- Game controllers (IOKit HID) -> XInput-style records in shared memory ---- */
enum {
    XB_UP = 0x0001, XB_DOWN = 0x0002, XB_LEFT = 0x0004, XB_RIGHT = 0x0008,
    XB_START = 0x0010, XB_BACK = 0x0020, XB_LTHUMB = 0x0040, XB_RTHUMB = 0x0080,
    XB_LB = 0x0100, XB_RB = 0x0200, XB_A = 0x1000, XB_B = 0x2000, XB_X = 0x4000, XB_Y = 0x8000
};

typedef struct {
    IOHIDDeviceRef dev; /* NULL = free slot */
    int isGoogle;       /* Stadia controller: decode its raw input report */
    int rawStadia;      /* a Stadia report was seen: ignore the generic element path */
    int hasZRz;         /* right stick on Z/Rz (triggers on Rx/Ry); otherwise right stick on Rx/Ry */
    uint16_t buttons, hat;
    uint8_t lt, rt;
    int16_t lx, ly, rx, ry;
    char name[64];
} C32Pad;

static C32Pad g_pads[MAX_PADS];
static IOHIDManagerRef g_hid = NULL;
static int g_hidTicks = 0;
static int g_padDebug = 0;

/* Generic HID button usage -> XInput bit (SDL's macOS mapping for the Stadia controller; also
   the common layout of HID gamepads): 1 A, 2 B, 3 X, 4 Y, 5 LB, 6 RB, 7 Back, 8 Start, 9 Guide,
   10 left stick click, 11 right stick click. */
static const uint16_t kButtonMap[12] = {0, XB_A, XB_B, XB_X, XB_Y, XB_LB, XB_RB, XB_BACK, XB_START, 0, XB_LTHUMB, XB_RTHUMB};
static const uint16_t kHatMap[8] = {XB_UP, XB_UP | XB_RIGHT, XB_RIGHT, XB_DOWN | XB_RIGHT,
                                    XB_DOWN, XB_DOWN | XB_LEFT, XB_LEFT, XB_UP | XB_LEFT};

static int16_t axisS16(CFIndex v, CFIndex mn, CFIndex mx, int invert) {
    if (mx <= mn) return 0;
    double f = ((double)(v - mn) / (double)(mx - mn)) * 2.0 - 1.0;
    if (invert) f = -f;
    if (f > 1.0) f = 1.0;
    if (f < -1.0) f = -1.0;
    if (f > -0.02 && f < 0.02) f = 0.0; /* centre jitter */
    return (int16_t)(f * 32767.0);
}

static uint8_t axisU8(CFIndex v, CFIndex mn, CFIndex mx) {
    if (mx <= mn) return 0;
    double f = (double)(v - mn) / (double)(mx - mn);
    if (f < 0.0) f = 0.0;
    if (f > 1.0) f = 1.0;
    return (uint8_t)(f * 255.0 + 0.5);
}

static int16_t stadiaStick(uint8_t v) { /* SDL: 0x80 centre, 0x01..0xFF -> full range */
    if (v == 0x80) return 0;
    double f = ((int)v - 0x80) / 127.0;
    if (f > 1.0) f = 1.0;
    if (f < -1.0) f = -1.0;
    return (int16_t)(f * 32767.0);
}

static C32Pad *padFor(IOHIDDeviceRef dev) {
    for (int i = 0; i < MAX_PADS; i++) {
        if (dev && g_pads[i].dev == dev) return &g_pads[i];
    }
    return NULL;
}

static void padMatched(void *ctx, IOReturn result, void *sender, IOHIDDeviceRef dev) {
    (void)ctx; (void)result; (void)sender;
    if (padFor(dev)) return;
    for (int i = 0; i < MAX_PADS; i++) {
        if (g_pads[i].dev) continue;
        C32Pad *p = &g_pads[i];
        memset(p, 0, sizeof(*p));
        p->dev = dev;
        CFTypeRef name = IOHIDDeviceGetProperty(dev, CFSTR(kIOHIDProductKey));
        if (name && CFGetTypeID(name) == CFStringGetTypeID()) {
            CFStringGetCString((CFStringRef)name, p->name, sizeof(p->name), kCFStringEncodingUTF8);
        } else {
            strcpy(p->name, "Game controller");
        }
        int vendor = 0;
        CFTypeRef vid = IOHIDDeviceGetProperty(dev, CFSTR(kIOHIDVendorIDKey));
        if (vid && CFGetTypeID(vid) == CFNumberGetTypeID()) {
            CFNumberGetValue((CFNumberRef)vid, kCFNumberIntType, &vendor);
        }
        p->isGoogle = (vendor == 0x18D1);
        int z = 0, rz = 0;
        CFArrayRef els = IOHIDDeviceCopyMatchingElements(dev, NULL, kIOHIDOptionsTypeNone);
        if (els) {
            for (CFIndex k = 0; k < CFArrayGetCount(els); k++) {
                IOHIDElementRef el = (IOHIDElementRef)CFArrayGetValueAtIndex(els, k);
                if (IOHIDElementGetUsagePage(el) == kHIDPage_GenericDesktop) {
                    uint32_t u = IOHIDElementGetUsage(el);
                    if (u == kHIDUsage_GD_Z) z = 1;
                    if (u == kHIDUsage_GD_Rz) rz = 1;
                }
            }
            CFRelease(els);
        }
        p->hasZRz = z && rz;
        printf("[controller] player %d: %s (vendor 0x%04x)\n", i + 1, p->name, vendor);
        fflush(stdout);
        return;
    }
}

static void padRemoved(void *ctx, IOReturn result, void *sender, IOHIDDeviceRef dev) {
    (void)ctx; (void)result; (void)sender;
    C32Pad *p = padFor(dev);
    if (p) {
        printf("[controller] disconnected: %s\n", p->name);
        fflush(stdout);
        memset(p, 0, sizeof(*p));
    }
}

static void padValue(void *ctx, IOReturn result, void *sender, IOHIDValueRef value) {
    (void)ctx; (void)result; (void)sender;
    IOHIDElementRef el = IOHIDValueGetElement(value);
    C32Pad *p = padFor(IOHIDElementGetDevice(el));
    if (!p) return;
    uint32_t page = IOHIDElementGetUsagePage(el), usage = IOHIDElementGetUsage(el);
    CFIndex v = IOHIDValueGetIntegerValue(value);
    CFIndex mn = IOHIDElementGetLogicalMin(el), mx = IOHIDElementGetLogicalMax(el);
    if (g_padDebug && (page != kHIDPage_GenericDesktop || usage == kHIDUsage_GD_Hatswitch || v == mn || v == mx)) {
        printf("[controller %d] page 0x%x usage 0x%x value %ld (range %ld..%ld)\n",
               (int)(p - g_pads) + 1, page, usage, (long)v, (long)mn, (long)mx);
        fflush(stdout);
    }
    if (p->rawStadia) return;
    if (page == kHIDPage_Button) {
        if (usage < 12 && kButtonMap[usage]) {
            if (v) p->buttons |= kButtonMap[usage];
            else p->buttons &= (uint16_t)~kButtonMap[usage];
        }
    } else if (page == kHIDPage_GenericDesktop) {
        switch (usage) {
        case kHIDUsage_GD_X: p->lx = axisS16(v, mn, mx, 0); break;
        case kHIDUsage_GD_Y: p->ly = axisS16(v, mn, mx, 1); break;
        case kHIDUsage_GD_Z: if (p->hasZRz) p->rx = axisS16(v, mn, mx, 0); else p->lt = axisU8(v, mn, mx); break;
        case kHIDUsage_GD_Rz: if (p->hasZRz) p->ry = axisS16(v, mn, mx, 1); else p->rt = axisU8(v, mn, mx); break;
        case kHIDUsage_GD_Rx: if (p->hasZRz) p->lt = axisU8(v, mn, mx); else p->rx = axisS16(v, mn, mx, 0); break;
        case kHIDUsage_GD_Ry: if (p->hasZRz) p->rt = axisU8(v, mn, mx); else p->ry = axisS16(v, mn, mx, 1); break;
        case kHIDUsage_GD_Hatswitch: {
            CFIndex h = v - mn;
            p->hat = (h >= 0 && h < 8 && mx - mn >= 7) ? kHatMap[h] : 0;
            break;
        }
        }
    } else if (page == kHIDPage_Simulation) {
        if (usage == kHIDUsage_Sim_Accelerator) p->rt = axisU8(v, mn, mx);
        else if (usage == kHIDUsage_Sim_Brake) p->lt = axisU8(v, mn, mx);
    }
}

/* Stadia controller input report (layout from SDL's HIDAPI Stadia driver):
   [0]=0x03 [1]=hat 0..7 (else neutral) [2]=0x40 Back 0x20 Start 0x10 Guide 0x80 RS
   [3]=0x40 A 0x20 B 0x10 X 0x08 Y 0x04 LB 0x02 RB 0x01 LS  [4..7]=LX LY RX RY (0x80 centre, Y down)
   [8]=LT [9]=RT */
static void padReport(void *ctx, IOReturn result, void *sender, IOHIDReportType type,
                      uint32_t reportID, uint8_t *report, CFIndex length) {
    (void)ctx; (void)result; (void)type;
    C32Pad *p = padFor((IOHIDDeviceRef)sender);
    if (!p || !p->isGoogle) return;
    uint8_t buf[64];
    const uint8_t *d = report;
    if (length > 0 && report[0] != 0x03 && reportID == 0x03 && length < (CFIndex)sizeof(buf)) {
        buf[0] = 0x03; /* report delivered without its ID byte */
        memcpy(buf + 1, report, (size_t)length);
        d = buf;
        length++;
    }
    if (length < 10 || d[0] != 0x03) return;
    p->rawStadia = 1;
    uint16_t b = 0;
    if (d[1] < 8) b |= kHatMap[d[1]];
    if (d[2] & 0x40) b |= XB_BACK;
    if (d[2] & 0x20) b |= XB_START;
    if (d[2] & 0x80) b |= XB_RTHUMB;
    if (d[3] & 0x40) b |= XB_A;
    if (d[3] & 0x20) b |= XB_B;
    if (d[3] & 0x10) b |= XB_X;
    if (d[3] & 0x08) b |= XB_Y;
    if (d[3] & 0x04) b |= XB_LB;
    if (d[3] & 0x02) b |= XB_RB;
    if (d[3] & 0x01) b |= XB_LTHUMB;
    p->buttons = b;
    p->hat = 0;
    p->lx = stadiaStick(d[4]);
    p->ly = (int16_t)-stadiaStick(d[5]);
    p->rx = stadiaStick(d[6]);
    p->ry = (int16_t)-stadiaStick(d[7]);
    p->lt = d[8];
    p->rt = d[9];
}

static void startControllers(void) {
    g_padDebug = getenv("C32_PAD_DEBUG") && strcmp(getenv("C32_PAD_DEBUG"), "1") == 0;
    g_hid = IOHIDManagerCreate(kCFAllocatorDefault, kIOHIDOptionsTypeNone);
    if (!g_hid) return;
    NSArray *matches = @[
        @{@kIOHIDDeviceUsagePageKey: @(kHIDPage_GenericDesktop), @kIOHIDDeviceUsageKey: @(kHIDUsage_GD_GamePad)},
        @{@kIOHIDDeviceUsagePageKey: @(kHIDPage_GenericDesktop), @kIOHIDDeviceUsageKey: @(kHIDUsage_GD_Joystick)},
        @{@kIOHIDDeviceUsagePageKey: @(kHIDPage_GenericDesktop), @kIOHIDDeviceUsageKey: @(kHIDUsage_GD_MultiAxisController)},
    ];
    IOHIDManagerSetDeviceMatchingMultiple(g_hid, (CFArrayRef)matches);
    IOHIDManagerRegisterDeviceMatchingCallback(g_hid, padMatched, NULL);
    IOHIDManagerRegisterDeviceRemovalCallback(g_hid, padRemoved, NULL);
    IOHIDManagerRegisterInputValueCallback(g_hid, padValue, NULL);
    IOHIDManagerRegisterInputReportCallback(g_hid, padReport, NULL);
    IOHIDManagerScheduleWithRunLoop(g_hid, CFRunLoopGetMain(), kCFRunLoopDefaultMode);
    if (IOHIDManagerOpen(g_hid, kIOHIDOptionsTypeNone) != kIOReturnSuccess) {
        printf("[controller] could not open the HID manager; controllers disabled\n");
    }
}

/* Writes controller records for the runtime's XInput emulation. The ready marker goes in once
   the initial device scan has had a few run-loop ticks to report already-connected pads. */
static int writePads(void) {
    if (!g_shm) return 0;
    uint8_t *base = g_shm + SHM_PAD_OFFSET;
    int n = 0;
    for (int i = 0; i < MAX_PADS; i++) {
        C32Pad *p = &g_pads[i];
        uint8_t *r = base + 4 + i * 16;
        uint32_t connected = p->dev ? 1 : 0;
        uint16_t b = (uint16_t)(p->buttons | p->hat);
        n += (int)connected;
        memcpy(r + 4, &b, 2);
        r[6] = p->lt;
        r[7] = p->rt;
        memcpy(r + 8, &p->lx, 2);
        memcpy(r + 10, &p->ly, 2);
        memcpy(r + 12, &p->rx, 2);
        memcpy(r + 14, &p->ry, 2);
        memcpy(r, &connected, 4);
    }
    if (g_hid && ++g_hidTicks >= 6) {
        uint32_t ready = SHM_PAD_READY;
        memcpy(base, &ready, 4);
    }
    return n;
}

static void c32AudioCallback(void *inUserData, AudioQueueRef inAQ, AudioQueueBufferRef inBuffer) {
    (void)inUserData;
    const int nFrames = 1024;
    int16_t *dst = (int16_t *)inBuffer->mAudioData;
    inBuffer->mAudioDataByteSize = nFrames * 4;
    if (!g_shm) {
        memset(dst, 0, nFrames * 4);
        AudioQueueEnqueueBuffer(inAQ, inBuffer, 0, NULL);
        return;
    }
    uint64_t writeIdx = *(uint64_t *)(g_shm + SHM_AUDIO_OFFSET);
    if (g_audioReadIdx == 0 || writeIdx > g_audioReadIdx + 8192 || g_audioReadIdx > writeIdx) {
        g_audioReadIdx = (writeIdx > 2048) ? (writeIdx - 2048) : 0;
    }
    const int16_t *ring = (const int16_t *)(g_shm + SHM_AUDIO_OFFSET + 8);
    for (int i = 0; i < nFrames; i++) {
        if (g_audioReadIdx < writeIdx) {
            uint64_t pos = (g_audioReadIdx & (SHM_AUDIO_FRAMES - 1)) * 2;
            dst[i * 2 + 0] = ring[pos + 0];
            dst[i * 2 + 1] = ring[pos + 1];
            g_audioReadIdx++;
        } else {
            dst[i * 2 + 0] = 0;
            dst[i * 2 + 1] = 0;
        }
    }
    AudioQueueEnqueueBuffer(inAQ, inBuffer, 0, NULL);
}

static int macKeyCodeToDIK(unsigned short kc) {
    switch (kc) {
        case 36: case 76: return 0x1C; // Return / Keypad Enter -> DIK_RETURN
        case 53: return 0x01;          // Escape -> DIK_ESCAPE
        case 49: return 0x39;          // Space -> DIK_SPACE
        case 120: return 0x3C;         // F2 -> DIK_F2
        case 126: return 0xC8;         // Up Arrow -> DIK_UP
        case 125: return 0xD0;         // Down Arrow -> DIK_DOWN
        case 123: return 0xCB;         // Left Arrow -> DIK_LEFT
        case 124: return 0xCD;         // Right Arrow -> DIK_RIGHT
        case 13: return 0x11;          // W -> DIK_W
        case 1:  return 0x1F;          // S -> DIK_S
        case 0:  return 0x1E;          // A -> DIK_A
        case 2:  return 0x20;          // D -> DIK_D
        case 32: return 0x16;          // U -> DIK_U (Jump)
        case 4:  return 0x23;          // H -> DIK_H (Attack)
        case 38: return 0x24;          // J -> DIK_J (Special/Use)
        case 40: return 0x25;          // K -> DIK_K (Switch)
        default: return -1;
    }
}

@interface C32View : NSView {
    CGColorSpaceRef _cs;
}
@end

@implementation C32View
- (instancetype)initWithFrame:(NSRect)frameRect {
    if ((self = [super initWithFrame:frameRect])) {
        _cs = CGColorSpaceCreateDeviceRGB();
    }
    return self;
}
- (void)dealloc {
    if (_cs) CGColorSpaceRelease(_cs);
    [super dealloc];
}
- (BOOL)isOpaque { return YES; }
- (BOOL)acceptsFirstResponder { return YES; }
- (void)keyDown:(NSEvent *)event {
    int dik = macKeyCodeToDIK([event keyCode]);
    if (dik > 0 && g_shm) {
        g_shm[32 + dik] = 0x80;
    }
}
- (void)keyUp:(NSEvent *)event {
    int dik = macKeyCodeToDIK([event keyCode]);
    if (dik > 0 && g_shm) {
        g_shm[32 + dik] = 0x00;
    }
}
- (void)drawRect:(NSRect)dirtyRect {
    CGContextRef ctx = [[NSGraphicsContext currentContext] CGContext];
    if (!ctx) return;
    if (!g_shm) {
        CGContextSetRGBFillColor(ctx, 0.05, 0.07, 0.09, 1.0);
        CGContextFillRect(ctx, self.bounds);
        return;
    }
    CGDataProviderRef provider = CGDataProviderCreateWithData(NULL, g_shm + SHM_HEADER, FB_W * FB_H * 4, NULL);
    CGImageRef img = CGImageCreate(
        FB_W, FB_H, 8, 32, FB_W * 4, _cs,
        kCGBitmapByteOrderDefault | kCGImageAlphaNoneSkipLast,
        provider, NULL, false, kCGRenderingIntentDefault
    );
    if (img) {
        CGContextSetInterpolationQuality(ctx, kCGInterpolationLow);
        CGContextDrawImage(ctx, self.bounds, img);
        CGImageRelease(img);
    }
    CGDataProviderRelease(provider);
}
@end

/* Hosts the CAMetalLayer the GPU renderer presents into (shown once it presents frames). */
@interface C32MetalView : NSView
@end

@implementation C32MetalView
- (instancetype)initWithFrame:(NSRect)frameRect {
    if ((self = [super initWithFrame:frameRect])) {
        [self setWantsLayer:YES];
    }
    return self;
}
- (CALayer *)makeBackingLayer {
    CAMetalLayer *l = [CAMetalLayer layer];
    l.device = MTLCreateSystemDefaultDevice();
    return l;
}
- (BOOL)isOpaque { return YES; }
- (BOOL)acceptsFirstResponder { return NO; }
- (void)updateDrawableSize {
    CAMetalLayer *l = (CAMetalLayer *)[self layer];
    CGFloat scale = [[self window] backingScaleFactor] ?: 1.0;
    l.contentsScale = scale;
    NSSize sz = [self bounds].size;
    l.drawableSize = CGSizeMake(MAX(sz.width * scale, 1), MAX(sz.height * scale, 1));
}
- (void)setFrameSize:(NSSize)newSize {
    [super setFrameSize:newSize];
    [self updateDrawableSize];
}
- (void)viewDidChangeBackingProperties {
    [super viewDidChangeBackingProperties];
    [self updateDrawableSize];
}
@end

@interface AppDelegate : NSObject <NSApplicationDelegate, NSWindowDelegate> {
    C32MetalView *_metalView;
    C32MetalRenderer *_renderer;
    NSView *_glView;          /* Source games: the OpenGL stream's view (C32_GL_SHM; nil otherwise) */
    C32GLRenderer *_glRenderer;
    NSWindow *_window;
    C32View *_fbView;
    NSTextField *_hudLabel; /* viewer mode with C32_HUD=1: frame, FPS, draw counts (nil otherwise) */
    NSString *_shmPath;
    AudioQueueRef _audioQueue;
    uint64_t _lastFrame;
    uint64_t _fpsBaseFrame;
    CFAbsoluteTime _fpsBaseTime;
    double _fps;
    C32GameLaunch *_game; /* game mode: the engine we started (nil: viewer mode) */
    BOOL _quitting;
    BOOL _pointerHidden; /* we called [NSCursor hide] (balanced by one unhide) */
    uint32_t _gameFlags; /* the engine's game state, as of the last tick */
}
- (instancetype)initWithShmPath:(NSString *)path;
- (instancetype)initWithGame:(C32GameLaunch *)game;
@end

@implementation AppDelegate
- (instancetype)initWithShmPath:(NSString *)path {
    if ((self = [super init])) {
        _shmPath = [path copy];
        _fpsBaseTime = CFAbsoluteTimeGetCurrent();
    }
    return self;
}

- (instancetype)initWithGame:(C32GameLaunch *)game {
    if ((self = [self initWithShmPath:[game shmPath]])) {
        _game = [game retain];
    }
    return self;
}

/* Game mode: when the engine stops, the app quits; if it stopped by itself with an error
   (the game moved, changed, failed to start), say why first. */
- (void)engineStopped:(int)status {
    if (_quitting) return;
    if (status != 0) {
        NSAlert *alert = [[[NSAlert alloc] init] autorelease];
        [alert setMessageText:[NSString stringWithFormat:@"%@ stopped", [_game title]]];
        [alert setInformativeText:[_game failureMessage]];
        [alert runModal];
    }
    [NSApp terminate:nil];
}

- (void)tryMapShm {
    if (g_shm) return;
    int fd = open([_shmPath UTF8String], O_RDWR);
    if (fd < 0) return;
    struct stat st;
    if (fstat(fd, &st) == 0 && st.st_size >= SHM_SIZE) {
        void *p = mmap(NULL, SHM_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
        if (p != MAP_FAILED) {
            g_shm = (uint8_t *)p;
        }
    }
    close(fd);
}

- (void)setPointerHidden:(BOOL)hidden {
    if (hidden == _pointerHidden) return;
    _pointerHidden = hidden;
    if (hidden) {
        [NSCursor hide];
    } else {
        [NSCursor unhide];
    }
    if ([_window styleMask] & NSWindowStyleMaskFullScreen) {
        [NSApp setPresentationOptions:[self fullScreenOptions]];
    }
}

/* In full screen without a pointer, the Dock and the menu bar stay hidden: otherwise the
   invisible pointer reveals the menu bar (and the title bar) at the top edge and the Dock at
   the bottom, and the Dock brings the pointer back. Ctrl-Cmd-F, Cmd-Tab and Cmd-Q still work.
   A game that wants a pointer gets the usual auto-hiding. */
- (NSApplicationPresentationOptions)fullScreenOptions {
    if (_gameFlags & SHM_GAME_POINTER) {
        return NSApplicationPresentationFullScreen | NSApplicationPresentationAutoHideDock | NSApplicationPresentationAutoHideMenuBar;
    }
    return NSApplicationPresentationFullScreen | NSApplicationPresentationHideDock | NSApplicationPresentationHideMenuBar;
}

- (NSApplicationPresentationOptions)window:(NSWindow *)window willUseFullScreenPresentationOptions:(NSApplicationPresentationOptions)proposed {
    return [self fullScreenOptions];
}

- (void)tick:(NSTimer *)timer {
    [self tryMapShm];
    if (!g_shm) return;

    int pads = writePads();

    uint64_t frame = *(uint64_t *)(g_shm + 0);
    uint64_t draws = *(uint64_t *)(g_shm + 8);
    uint64_t tris  = *(uint64_t *)(g_shm + 16);
    uint64_t steps = *(uint64_t *)(g_shm + 24);

    CFAbsoluteTime now = CFAbsoluteTimeGetCurrent();
    if (now - _fpsBaseTime >= 0.4) {
        if (_fpsBaseFrame > 0 && frame >= _fpsBaseFrame) {
            _fps = (double)(frame - _fpsBaseFrame) / (now - _fpsBaseTime);
        }
        _fpsBaseFrame = frame;
        _fpsBaseTime = now;
        [_hudLabel setStringValue:[NSString stringWithFormat:
            @"Frame: %llu (%.1f FPS)  |  DrawCalls: %llu  |  Triangles: %llu  |  Steps: %.1fM  |  Controllers: %d  |  Keys: WASD / Enter / U H J K",
            frame, _fps, draws, tris, (double)steps / 1e6, pads]];
    }

    /* In full screen, hide the pointer unless the game wants it (the engine decides from what the
       game asks for: Win32 ShowCursor/SetCursor, a Direct3D device cursor, a windowed device). */
    uint32_t gameFlags = *(volatile uint32_t *)(g_shm + SHM_GAME_OFFSET);
    _gameFlags = gameFlags;
    if (!_game && (gameFlags & SHM_GAME_ENDED)) {
        /* Viewer mode: the engine is someone else's child; close with the game. (Game mode
           waits for the engine process to end, to report a failure.) */
        [NSApp terminate:nil];
        return;
    }
    BOOL fullScreen = ([_window styleMask] & NSWindowStyleMaskFullScreen) != 0;
    [self setPointerHidden:fullScreen && [NSApp isActive] && !(gameFlags & SHM_GAME_POINTER)];

    if (_renderer && [_renderer active] && [_metalView isHidden]) {
        [_metalView setHidden:NO]; /* the GPU renderer draws from now on */
    }
    if (frame != _lastFrame) {
        _lastFrame = frame;
        if ([_metalView isHidden]) {
            [_fbView setNeedsDisplay:YES];
        }
    }
}

- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    BOOL player = _game != nil; /* game mode: only the game */
    const char *hudEnv = getenv("C32_HUD");
    BOOL hud = !player && hudEnv && strcmp(hudEnv, "1") == 0; /* a line of developer stats on top */
    NSRect winRect = NSMakeRect(0, 0, 1280, hud ? 744 : 720);
    _window = [[NSWindow alloc] initWithContentRect:winRect
                                          styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable)
                                            backing:NSBackingStoreBuffered
                                              defer:NO];
    [_window center];
    const char *titleEnv = getenv("C32_TITLE");
    [_window setTitle:player ? [_game title] : titleEnv ? [NSString stringWithUTF8String:titleEnv] : @"catalina32 viewer"];
    [_window setBackgroundColor:[NSColor colorWithCalibratedRed:0.06 green:0.08 blue:0.11 alpha:1.0]];
    [_window setDelegate:self];

    NSView *content = [_window contentView];

    if (player) {
        __block AppDelegate *me = self;
        [[_game engine] setTerminationHandler:^(NSTask *task) {
            int status = [task terminationStatus];
            dispatch_async(dispatch_get_main_queue(), ^{ [me engineStopped:status]; });
        }];
    } else if (hud) {
        _hudLabel = [[NSTextField alloc] initWithFrame:NSMakeRect(12, 722, 1256, 20)];
        [_hudLabel setEditable:NO];
        [_hudLabel setBezeled:NO];
        [_hudLabel setDrawsBackground:NO];
        [_hudLabel setTextColor:[NSColor colorWithCalibratedRed:0.35 green:0.65 blue:1.0 alpha:1.0]];
        [_hudLabel setFont:[NSFont monospacedSystemFontOfSize:11.5 weight:NSFontWeightMedium]];
        [_hudLabel setStringValue:@"Connecting to catalina32 shared memory framebuffer..."];
        [_hudLabel setAutoresizingMask:NSViewWidthSizable | NSViewMinYMargin];
        [content addSubview:_hudLabel];
    }

    _fbView = [[C32View alloc] initWithFrame:NSMakeRect(0, 0, 1280, 720)];
    [_fbView setAutoresizingMask:NSViewWidthSizable | NSViewHeightSizable];
    [content addSubview:_fbView];

    /* GPU renderer: the emulator writes Metal command frames to gpu.shm next to the framebuffer
       (C32_GPU_SHM); C32_RENDERER=software keeps the software path. */
    /* OpenGL renderer (Valve Source games, C32_GL_SHM). Looked up by name: builds without
       C32GL.m (LEGO Star Wars only) still link. */
    const char *rend = getenv("C32_RENDERER");
    const char *glPath = getenv("C32_GL_SHM");
    Class glViewClass = NSClassFromString(@"C32GLView"), glRendererClass = NSClassFromString(@"C32GLRenderer");
    if (glPath && glViewClass && glRendererClass) {
        _glView = [[glViewClass alloc] initWithFrame:[_fbView frame]];
        [_glView setAutoresizingMask:NSViewWidthSizable | NSViewHeightSizable];
        [content addSubview:_glView positioned:NSWindowAbove relativeTo:_fbView];
        _glRenderer = [[glRendererClass alloc] initWithPath:[NSString stringWithUTF8String:glPath] view:(C32GLView *)_glView];
    } else if (!(rend && strcmp(rend, "software") == 0) && MTLCreateSystemDefaultDevice()) {
        _metalView = [[C32MetalView alloc] initWithFrame:[_fbView frame]];
        [_metalView setAutoresizingMask:NSViewWidthSizable | NSViewHeightSizable];
        [_metalView setHidden:YES];
        [content addSubview:_metalView positioned:NSWindowAbove relativeTo:_fbView];
        [_metalView updateDrawableSize];
        const char *gpuPath = getenv("C32_GPU_SHM");
        NSString *path = gpuPath ? [NSString stringWithUTF8String:gpuPath]
                                 : [[_shmPath stringByDeletingLastPathComponent] stringByAppendingPathComponent:@"gpu.shm"];
        _renderer = [[C32MetalRenderer alloc] initWithPath:path layer:(CAMetalLayer *)[_metalView layer]];
        [_renderer start];
    }

    [_glRenderer start];
    // C32_HEADLESS=1: never show the window (Metal still renders into the offscreen back
    // buffer; C32_METAL_SNAPSHOT dumps it to a file for inspection without a screen).
    if (!(getenv("C32_HEADLESS") && strcmp(getenv("C32_HEADLESS"), "1") == 0)) {
        [_window makeKeyAndOrderFront:nil];
        [_window makeFirstResponder:_glView ? _glView : _fbView];
        [NSApp activateIgnoringOtherApps:YES];
    }

    AudioStreamBasicDescription fmt = {0};
    fmt.mSampleRate = 44100.0;
    fmt.mFormatID = kAudioFormatLinearPCM;
    fmt.mFormatFlags = kLinearPCMFormatFlagIsSignedInteger | kLinearPCMFormatFlagIsPacked;
    fmt.mBytesPerPacket = 4;
    fmt.mFramesPerPacket = 1;
    fmt.mBytesPerFrame = 4;
    fmt.mChannelsPerFrame = 2;
    fmt.mBitsPerChannel = 16;
    if (AudioQueueNewOutput(&fmt, c32AudioCallback, NULL, NULL, kCFRunLoopCommonModes, 0, &_audioQueue) == noErr) {
        for (int i = 0; i < 3; i++) {
            AudioQueueBufferRef buf = NULL;
            if (AudioQueueAllocateBuffer(_audioQueue, 1024 * 4, &buf) == noErr) {
                c32AudioCallback(NULL, _audioQueue, buf);
            }
        }
        AudioQueueStart(_audioQueue, NULL);
    }

    startControllers();

    [NSTimer scheduledTimerWithTimeInterval:(1.0 / 60.0)
                                     target:self
                                   selector:@selector(tick:)
                                   userInfo:nil
                                    repeats:YES];
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
    return YES;
}

- (void)applicationWillTerminate:(NSNotification *)notification {
    _quitting = YES;
    [_renderer stop]; /* tells the emulator to stop sending GPU frames */
    [_glRenderer stop];
    if ([[_game engine] isRunning]) [[_game engine] terminate];
}
@end

/* The main menu: Hide and Quit (Cmd-H, Cmd-Q), and View > Enter/Exit Full Screen (Ctrl-Cmd-F).
   Its shortcuts also work in full screen, where the menu bar stays hidden. */
static void setUpMainMenu(void) {
    NSString *name = [[NSBundle mainBundle] objectForInfoDictionaryKey:@"CFBundleName"];
    if (![name length]) name = @"catalina32";
    NSMenu *bar = [[[NSMenu alloc] init] autorelease];
    NSMenuItem *appItem = [[[NSMenuItem alloc] init] autorelease];
    NSMenu *appMenu = [[[NSMenu alloc] initWithTitle:name] autorelease];
    [appMenu addItemWithTitle:[@"Hide " stringByAppendingString:name] action:@selector(hide:) keyEquivalent:@"h"];
    [appMenu addItem:[NSMenuItem separatorItem]];
    [appMenu addItemWithTitle:[@"Quit " stringByAppendingString:name] action:@selector(terminate:) keyEquivalent:@"q"];
    [appItem setSubmenu:appMenu];
    [bar addItem:appItem];
    NSMenuItem *viewItem = [[[NSMenuItem alloc] init] autorelease];
    NSMenu *viewMenu = [[[NSMenu alloc] initWithTitle:@"View"] autorelease];
    NSMenuItem *full = [viewMenu addItemWithTitle:@"Enter Full Screen" action:@selector(toggleFullScreen:) keyEquivalent:@"f"];
    [full setKeyEquivalentModifierMask:NSEventModifierFlagControl | NSEventModifierFlagCommand];
    [viewItem setSubmenu:viewMenu];
    [bar addItem:viewItem];
    [NSApp setMainMenu:bar];
}

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        NSApplication *app = [NSApplication sharedApplication];
        BOOL headless = getenv("C32_HEADLESS") && strcmp(getenv("C32_HEADLESS"), "1") == 0;
        [app setActivationPolicy:headless ? NSApplicationActivationPolicyProhibited : NSApplicationActivationPolicyRegular];
        setUpMainMenu();
        id<NSApplicationDelegate> del = nil;
        /* (Old macOS versions pass -psn_... to apps opened from the Finder: not a path.) */
        NSString *manifest = [[NSBundle mainBundle] pathForResource:@"game" ofType:@"json"];
        if (argc > 1 && argv[1][0] != '-') {
            /* Viewer mode: the engine was started by someone else (run_on_mac.sh, catalina32 run). */
            del = [[AppDelegate alloc] initWithShmPath:[NSString stringWithUTF8String:argv[1]]];
        } else if (manifest) {
            /* Game mode: a game app. */
            NSString *error = nil;
            C32GameLaunch *game = [C32GameLaunch launchManifest:manifest error:&error];
            if (!game) {
                NSAlert *alert = [[[NSAlert alloc] init] autorelease];
                [alert setMessageText:@"The game can't start"];
                [alert setInformativeText:error];
                [alert runModal];
                return 1;
            }
            del = [[AppDelegate alloc] initWithGame:game];
        } else {
            /* Wizard mode: Catalina32.app. */
            del = [[C32Wizard alloc] init];
        }
        [app setDelegate:del];
        [app run];
    }
    return 0;
}

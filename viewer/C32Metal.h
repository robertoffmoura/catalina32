// Metal renderer for the GPU command stream the emulator writes (pkg/d3d9/gpu.go describes the
// format). It runs on its own thread: it takes each written frame from the shared memory,
// draws it into an offscreen back buffer and presents that, scaled, in the layer.
#import <Foundation/Foundation.h>
#import <QuartzCore/CAMetalLayer.h>

@interface C32MetalRenderer : NSObject
// path is the command stream file (C32_GPU_SHM); layer is where frames are presented.
- (instancetype)initWithPath:(NSString *)path layer:(CAMetalLayer *)layer;
- (void)start;
- (void)stop;
// YES once a frame has been presented (the window then shows the layer).
@property(atomic, readonly) BOOL active;
@property(atomic, readonly) uint64_t framesPresented;
@end

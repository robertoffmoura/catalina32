// OpenGL renderer for the stream a Valve Source game's GL calls are recorded into
// (pkg/source/glremote.go describes it, pkg/source/glspec.go lists the calls). It runs on its
// own thread with a real OpenGL 2.1 context: the game draws into an offscreen "window" (a
// framebuffer of the size the game asked for), which each present scales into the view.
// The view also sends keyboard and mouse input back to the game.
#import <Cocoa/Cocoa.h>

@interface C32GLView : NSView
@end

@interface C32GLRenderer : NSObject
// path is the stream file (C32_GL_SHM); view shows the frames and takes the input.
- (instancetype)initWithPath:(NSString *)path view:(C32GLView *)view;
- (void)start;
- (void)stop;
@property(atomic, readonly) BOOL active;
@end

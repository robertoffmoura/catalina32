// The window app's other modes (compiled with ARC; C32Window.m is not).
//
// The same executable is Catalina32.app's wizard and every game app's window:
//   - with a shared-memory path argument: the viewer for an engine someone else started
//     (run_on_mac.sh, `catalina32 run`), as C32Window.m always was;
//   - in a game app (Contents/Resources/game.json): starts the engine for that game (C32GameLaunch)
//     and shows only the game;
//   - otherwise: the wizard that finds games and makes game apps (C32Wizard).
#import <Cocoa/Cocoa.h>

@interface C32GameLaunch : NSObject
@property(readonly) NSString *title;   // the game's name (window title)
@property(readonly) NSString *shmPath; // the framebuffer shared memory the engine creates
@property(readonly) NSString *logPath; // the engine's output
@property(readonly) NSTask *engine;
// Starts Contents/Helpers/catalina32 for the manifest (setting C32_SHM_FILE and C32_GPU_SHM, or
// C32_GL_SHM for a Source game, for both processes), or returns nil with a message for players
// in *error.
+ (instancetype)launchManifest:(NSString *)manifest error:(NSString **)error;
// The reason the engine stopped early, from its log (for an alert).
- (NSString *)failureMessage;
@end

@interface C32Wizard : NSObject <NSApplicationDelegate>
@end

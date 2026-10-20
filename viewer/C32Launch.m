// Game mode: a game app's window starts the engine for its manifest (see C32App.h).
#import "C32App.h"
#include <unistd.h>

@implementation C32GameLaunch

+ (instancetype)launchManifest:(NSString *)manifest error:(NSString **)error {
    C32GameLaunch *g = [C32GameLaunch new];
    NSDictionary *m = nil;
    NSData *data = [NSData dataWithContentsOfFile:manifest];
    if (data) m = [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL];
    if (![m isKindOfClass:[NSDictionary class]]) {
        *error = @"This game app is damaged (its game.json can't be read). Make it again from Catalina32.";
        return nil;
    }
    g->_title = [m[@"name"] isKindOfClass:[NSString class]] ? m[@"name"] : @"catalina32";

    NSFileManager *fm = [NSFileManager defaultManager];
    NSString *work = [NSTemporaryDirectory() stringByAppendingPathComponent:
                      [NSString stringWithFormat:@"catalina32-%d", getpid()]];
    [fm createDirectoryAtPath:[work stringByAppendingPathComponent:@"frames"] withIntermediateDirectories:YES attributes:nil error:NULL];
    g->_shmPath = [work stringByAppendingPathComponent:@"framebuffer.shm"];
    setenv("C32_SHM_FILE", [g->_shmPath fileSystemRepresentation], 1);
    setenv("C32_FRAMES_DIR", [[work stringByAppendingPathComponent:@"frames"] fileSystemRepresentation], 1);
    const char *rend = getenv("C32_RENDERER");
    if (!(rend && strcmp(rend, "software") == 0)) {
        setenv("C32_GPU_SHM", [[work stringByAppendingPathComponent:@"gpu.shm"] fileSystemRepresentation], 1);
    }

    NSString *logs = [NSHomeDirectory() stringByAppendingPathComponent:@"Library/Logs/catalina32"];
    [fm createDirectoryAtPath:logs withIntermediateDirectories:YES attributes:nil error:NULL];
    NSString *safe = [[g->_title componentsSeparatedByCharactersInSet:[NSCharacterSet characterSetWithCharactersInString:@"/:"]] componentsJoinedByString:@"-"];
    g->_logPath = [logs stringByAppendingPathComponent:[safe stringByAppendingPathExtension:@"log"]];
    [fm createFileAtPath:g->_logPath contents:nil attributes:nil];
    NSFileHandle *log = [NSFileHandle fileHandleForWritingAtPath:g->_logPath];

    NSString *engine = [[[NSBundle mainBundle] bundlePath] stringByAppendingPathComponent:@"Contents/Helpers/catalina32"];
    NSTask *task = [NSTask new];
    task.executableURL = [NSURL fileURLWithPath:engine];
    task.arguments = @[ @"run", @"--manifest", manifest, @"--no-window" ];
    if (log) {
        task.standardOutput = log;
        task.standardError = log;
    }
    NSError *err = nil;
    if (![task launchAndReturnError:&err]) {
        *error = [NSString stringWithFormat:@"catalina32 couldn't start: %@", err.localizedDescription];
        return nil;
    }
    g->_engine = task;
    return g;
}

- (NSString *)failureMessage {
    NSString *text = [NSString stringWithContentsOfFile:_logPath encoding:NSUTF8StringEncoding error:NULL];
    NSString *found = nil;
    for (NSString *line in [text componentsSeparatedByString:@"\n"]) {
        if ([line hasPrefix:@"catalina32: "]) found = [line substringFromIndex:12];
    }
    return found ?: [NSString stringWithFormat:@"The game stopped unexpectedly. Details are in %@.", _logPath];
}

@end

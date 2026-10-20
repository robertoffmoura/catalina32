// Wizard mode: Catalina32.app lists the player's 32-bit games and makes a game app for each
// supported one. All the logic is the command line's (Contents/Helpers/catalina32 list, info,
// wrap with --json); this file only presents it (see C32App.h).
#import "C32App.h"

// C32DropView accepts .app bundles dragged onto the window.
@interface C32DropView : NSView
@property(copy) void (^onDrop)(NSArray<NSURL *> *apps);
@end

@implementation C32DropView
- (instancetype)initWithFrame:(NSRect)frame {
    if ((self = [super initWithFrame:frame])) {
        [self registerForDraggedTypes:@[ NSPasteboardTypeFileURL ]];
    }
    return self;
}

- (NSArray<NSURL *> *)appsIn:(id<NSDraggingInfo>)info {
    NSArray *urls = [[info draggingPasteboard] readObjectsForClasses:@[ [NSURL class] ]
                                                             options:@{NSPasteboardURLReadingFileURLsOnlyKey : @YES}];
    NSMutableArray *apps = [NSMutableArray array];
    for (NSURL *u in urls) {
        if ([[u pathExtension] isEqualToString:@"app"]) [apps addObject:u];
    }
    return apps;
}

- (NSDragOperation)draggingEntered:(id<NSDraggingInfo>)info {
    return [[self appsIn:info] count] ? NSDragOperationCopy : NSDragOperationNone;
}

- (BOOL)performDragOperation:(id<NSDraggingInfo>)info {
    NSArray *apps = [self appsIn:info];
    if ([apps count] && self.onDrop) self.onDrop(apps);
    return [apps count] > 0;
}
@end

@interface C32Wizard () <NSTableViewDataSource, NSTableViewDelegate>
@end

@implementation C32Wizard {
    NSWindow *_window;
    NSTableView *_table;
    NSButton *_create, *_choose, *_refresh;
    NSTextField *_status;
    NSMutableArray<NSDictionary *> *_games; // catalina32's Info objects (JSON)
    NSArray<NSURL *> *_pendingOpen;         // apps dropped on the Dock icon before launch finished
}

// cli runs the command line in the background and hands its JSON output to done (nil: failed).
- (void)cli:(NSArray<NSString *> *)args done:(void (^)(id json))done {
    NSString *engine = [[[NSBundle mainBundle] bundlePath] stringByAppendingPathComponent:@"Contents/Helpers/catalina32"];
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
        NSTask *task = [NSTask new];
        task.executableURL = [NSURL fileURLWithPath:engine];
        task.arguments = args;
        NSPipe *pipe = [NSPipe pipe];
        task.standardOutput = pipe;
        id json = nil;
        if ([task launchAndReturnError:NULL]) {
            NSData *data = [[pipe fileHandleForReading] readDataToEndOfFile];
            [task waitUntilExit];
            json = [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL];
        }
        dispatch_async(dispatch_get_main_queue(), ^{ done(json); });
    });
}

- (NSButton *)button:(NSString *)title action:(SEL)action frame:(NSRect)frame {
    NSButton *b = [NSButton buttonWithTitle:title target:self action:action];
    [b setFrame:frame];
    return b;
}

- (NSTextField *)label:(NSString *)text frame:(NSRect)frame size:(CGFloat)size bold:(BOOL)bold {
    NSTextField *l = [NSTextField labelWithString:text];
    [l setFrame:frame];
    [l setFont:bold ? [NSFont boldSystemFontOfSize:size] : [NSFont systemFontOfSize:size]];
    return l;
}

- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    _games = [NSMutableArray array];
    _window = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, 680, 460)
                                          styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable
                                            backing:NSBackingStoreBuffered
                                              defer:NO];
    [_window setTitle:@"Catalina32"];
    C32DropView *content = [[C32DropView alloc] initWithFrame:NSMakeRect(0, 0, 680, 460)];
    __weak C32Wizard *weakSelf = self;
    content.onDrop = ^(NSArray<NSURL *> *apps) { [weakSelf addApps:apps]; };
    [_window setContentView:content];

    [content addSubview:[self label:@"Your 32-bit games" frame:NSMakeRect(20, 418, 640, 24) size:18 bold:YES]];
    NSTextField *intro = [self label:@"Catalina32 makes an app for each supported game. Your game files are never changed. "
                                     @"Drop a game here, or choose one, if it isn't listed."
                               frame:NSMakeRect(20, 378, 640, 36) size:12 bold:NO];
    [intro setTextColor:[NSColor secondaryLabelColor]];
    [[intro cell] setWraps:YES];
    [content addSubview:intro];

    NSScrollView *scroll = [[NSScrollView alloc] initWithFrame:NSMakeRect(20, 64, 640, 304)];
    [scroll setHasVerticalScroller:YES];
    [scroll setBorderType:NSBezelBorder];
    _table = [[NSTableView alloc] initWithFrame:[scroll bounds]];
    NSTableColumn *col = [[NSTableColumn alloc] initWithIdentifier:@"game"];
    [col setWidth:620];
    [_table addTableColumn:col];
    [_table setHeaderView:nil];
    [_table setRowHeight:52];
    [_table setDataSource:self];
    [_table setDelegate:self];
    [_table setTarget:self];
    [_table setDoubleAction:@selector(createApp:)];
    [scroll setDocumentView:_table];
    [content addSubview:scroll];

    _status = [self label:@"Looking for games…" frame:NSMakeRect(20, 24, 330, 20) size:12 bold:NO];
    [_status setTextColor:[NSColor secondaryLabelColor]];
    [content addSubview:_status];
    _refresh = [self button:@"Refresh" action:@selector(refresh:) frame:NSMakeRect(350, 18, 90, 32)];
    _choose = [self button:@"Choose Game…" action:@selector(chooseGame:) frame:NSMakeRect(440, 18, 120, 32)];
    _create = [self button:@"Create App" action:@selector(createApp:) frame:NSMakeRect(560, 18, 104, 32)];
    [_create setKeyEquivalent:@"\r"];
    [_create setEnabled:NO];
    for (NSButton *b in @[ _refresh, _choose, _create ]) [content addSubview:b];

    [_window center];
    [_window makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
    [self refresh:nil];
    if (_pendingOpen) [self addApps:_pendingOpen];
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
    return YES;
}

// A game dropped on the Dock icon, or opened with Catalina32.
- (void)application:(NSApplication *)app openURLs:(NSArray<NSURL *> *)urls {
    if (_table) {
        [self addApps:urls];
    } else {
        _pendingOpen = urls;
    }
}

- (void)refresh:(id)sender {
    [_status setStringValue:@"Looking for games…"];
    [_refresh setEnabled:NO];
    [self cli:@[ @"list", @"--json" ] done:^(id json) {
        [self->_refresh setEnabled:YES];
        if (![json isKindOfClass:[NSArray class]]) {
            [self->_status setStringValue:@"Couldn't look for games."];
            return;
        }
        [self->_games setArray:json];
        [self->_table reloadData];
        NSUInteger n = [self->_games count];
        [self->_status setStringValue:n ? [NSString stringWithFormat:@"%lu game%@ found.", (unsigned long)n, n == 1 ? @"" : @"s"]
                                        : @"No 32-bit games found. Choose one, or drop it here."];
        if (n) [self->_table selectRowIndexes:[NSIndexSet indexSetWithIndex:0] byExtendingSelection:NO];
        [self updateButtons];
    }];
}

// addApps identifies apps the player pointed at and puts them first in the list.
- (void)addApps:(NSArray<NSURL *> *)apps {
    for (NSURL *url in apps) {
        [self cli:@[ @"info", @"--json", [url path] ] done:^(id json) {
            if (![json isKindOfClass:[NSDictionary class]]) return;
            NSUInteger i = [self->_games indexOfObjectPassingTest:^BOOL(NSDictionary *g, NSUInteger idx, BOOL *stop) {
                return [g[@"app"] isEqual:json[@"app"]];
            }];
            if (i != NSNotFound) [self->_games removeObjectAtIndex:i];
            [self->_games insertObject:json atIndex:0];
            [self->_table reloadData];
            [self->_table selectRowIndexes:[NSIndexSet indexSetWithIndex:0] byExtendingSelection:NO];
            [self->_status setStringValue:json[@"detail"] ?: @""];
            [self updateButtons];
        }];
    }
}

- (void)chooseGame:(id)sender {
    NSOpenPanel *panel = [NSOpenPanel openPanel];
    [panel setAllowedFileTypes:@[ @"app" ]];
    [panel setAllowsMultipleSelection:NO];
    [panel setTreatsFilePackagesAsDirectories:NO];
    [panel setMessage:@"Choose a game app"];
    [panel beginSheetModalForWindow:_window completionHandler:^(NSModalResponse result) {
        if (result == NSModalResponseOK) [self addApps:[panel URLs]];
    }];
}

- (NSDictionary *)selectedGame {
    NSInteger row = [_table selectedRow];
    return row >= 0 && row < (NSInteger)[_games count] ? _games[row] : nil;
}

- (void)updateButtons {
    [_create setEnabled:[[self selectedGame][@"status"] isEqual:@"supported"]];
}

- (void)createApp:(id)sender {
    NSDictionary *game = [self selectedGame];
    if (![game[@"status"] isEqual:@"supported"]) return;
    NSString *name = game[@"title"] ?: game[@"name"];
    [_create setEnabled:NO];
    [_status setStringValue:[NSString stringWithFormat:@"Making the app for %@…", name]];
    [self cli:@[ @"wrap", @"--json", game[@"app"] ] done:^(id json) {
        [self updateButtons];
        NSString *app = [json isKindOfClass:[NSDictionary class]] ? json[@"app"] : nil;
        NSAlert *alert = [NSAlert new];
        if (!app) {
            NSString *error = [json isKindOfClass:[NSDictionary class]] ? json[@"error"] : nil;
            [self->_status setStringValue:@"The app wasn't made."];
            [alert setMessageText:[NSString stringWithFormat:@"The app for %@ couldn't be made", name]];
            [alert setInformativeText:error ?: @"catalina32 stopped unexpectedly."];
            [alert beginSheetModalForWindow:self->_window completionHandler:nil];
            return;
        }
        [self->_status setStringValue:[NSString stringWithFormat:@"Made %@.", [app lastPathComponent]]];
        [alert setMessageText:[NSString stringWithFormat:@"%@ is ready", name]];
        [alert setInformativeText:[NSString stringWithFormat:@"Its app is in %@. Open it like any other app; you can keep it in the Dock.\n\nSaves: %@",
                                   [[app stringByDeletingLastPathComponent] stringByAbbreviatingWithTildeInPath],
                                   [game[@"saves"] stringByAbbreviatingWithTildeInPath] ?: @"the game's usual folder"]];
        [alert addButtonWithTitle:@"Play"];
        [alert addButtonWithTitle:@"Show in Finder"];
        [alert addButtonWithTitle:@"Done"];
        [alert beginSheetModalForWindow:self->_window completionHandler:^(NSModalResponse r) {
            NSURL *url = [NSURL fileURLWithPath:app];
            if (r == NSAlertFirstButtonReturn) {
                [[NSWorkspace sharedWorkspace] openURL:url];
            } else if (r == NSAlertSecondButtonReturn) {
                [[NSWorkspace sharedWorkspace] activateFileViewerSelectingURLs:@[ url ]];
            }
        }];
    }];
}

#pragma mark Table

- (NSInteger)numberOfRowsInTableView:(NSTableView *)tableView {
    return (NSInteger)[_games count];
}

- (NSView *)tableView:(NSTableView *)tableView viewForTableColumn:(NSTableColumn *)column row:(NSInteger)row {
    NSDictionary *g = _games[row];
    NSView *cell = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 620, 52)];
    NSImageView *icon = [[NSImageView alloc] initWithFrame:NSMakeRect(8, 8, 36, 36)];
    NSImage *image = [g[@"icon"] isKindOfClass:[NSString class]] ? [[NSImage alloc] initWithContentsOfFile:g[@"icon"]] : nil;
    [icon setImage:image ?: [[NSWorkspace sharedWorkspace] iconForFile:g[@"app"]]];
    [cell addSubview:icon];
    NSString *name = g[@"title"] ?: (g[@"name"] ?: [[g[@"app"] lastPathComponent] stringByDeletingPathExtension]);
    [cell addSubview:[self label:name frame:NSMakeRect(54, 26, 556, 18) size:13 bold:YES]];
    NSTextField *detail = [self label:g[@"detail"] ?: @"" frame:NSMakeRect(54, 8, 556, 16) size:11 bold:NO];
    BOOL ok = [g[@"status"] isEqual:@"supported"];
    [detail setTextColor:ok ? [NSColor systemGreenColor] : [NSColor secondaryLabelColor]];
    [cell addSubview:detail];
    return cell;
}

- (void)tableViewSelectionDidChange:(NSNotification *)notification {
    [self updateButtons];
}

@end

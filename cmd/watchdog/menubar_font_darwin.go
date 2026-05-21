//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>

// patchMenubarFont reaches into NSStatusBar to find the status item that
// fyne.io/systray just created and tightens its button so two text lines
// fit inside the fixed menubar height:
//
//   button.font            = system menubar font at `size` pt (~half default)
//   cell.usesSingleLineMode = NO   (otherwise newlines truncate)
//   cell.lineBreakMode      = clip (avoid mid-word wrap weirdness)
//   button.imagePosition    = left of text so the chart-bars icon and the
//                             two-line title don't fight for the same slot
//
// Uses the private `_items` ivar on NSStatusBar — stable since 10.6, used
// by every status-item theming tool (Bartender, iStat Menus, etc.). The
// alternative is forking fyne.io/systray to expose attributedTitle and
// that's a much larger change for the same outcome.
static void patchMenubarFont(double size) {
    @autoreleasepool {
        NSStatusBar *bar = [NSStatusBar systemStatusBar];
        NSArray *items = [bar valueForKey:@"_items"];
        if (items.count == 0) return;
        // We're the only consumer of fyne's status item in this process, so
        // the most recently added item is ours.
        NSStatusItem *item = items.lastObject;
        NSStatusBarButton *button = item.button;
        if (button == nil) return;
        button.font = [NSFont menuBarFontOfSize:size];
        button.cell.usesSingleLineMode = NO;
        button.cell.lineBreakMode = NSLineBreakByClipping;
        button.imagePosition = NSImageLeft;
        // Two-line titles default to vertically-centred; nudge slightly
        // tighter so the menubar doesn't look like it's been stretched.
        if ([button respondsToSelector:@selector(setImageHugsTitle:)]) {
            [button setImageHugsTitle:YES];
        }
    }
}
*/
import "C"

func setMenubarFontSize(size float64) {
	C.patchMenubarFont(C.double(size))
}

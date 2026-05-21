//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>

// findOurStatusButton walks NSStatusBar's private _items list and
// returns the most recent status item's button. _items is
// NSConcretePointerArray on modern macOS, so we probe both APIs.
static NSStatusBarButton *findOurStatusButton(void) {
    NSStatusBar *bar = [NSStatusBar systemStatusBar];
    id items = [bar valueForKey:@"_items"];
    if (items == nil) return nil;
    NSUInteger n = 0;
    if ([items respondsToSelector:@selector(count)]) {
        n = (NSUInteger)[items count];
    }
    if (n == 0) return nil;
    NSStatusItem *item = nil;
    if ([items respondsToSelector:@selector(objectAtIndex:)]) {
        item = (NSStatusItem *)[items objectAtIndex:n - 1];
    } else if ([items respondsToSelector:@selector(pointerAtIndex:)]) {
        item = (__bridge NSStatusItem *)[items pointerAtIndex:n - 1];
    }
    return item ? item.button : nil;
}

// runOnMain dispatches to the main thread; NSStatusItem mutations must
// run there.
static void runOnMain(void (^block)(void)) {
    if ([NSThread isMainThread]) block();
    else dispatch_async(dispatch_get_main_queue(), block);
}

// resolveSymbol returns an SF Symbol image at the given point size,
// with a fallback chain so Apple renaming the symbol between releases
// doesn't drop the icon entirely.
static NSImage *resolveSymbol(NSString *logicalName, double pointSize) {
    NSArray *chain;
    if ([logicalName isEqualToString:@"thermometer"]) {
        chain = @[@"thermometer.medium", @"thermometer",
                  @"thermometer.high", @"thermometer.snowflake",
                  @"flame.fill"];
    } else if ([logicalName isEqualToString:@"globe"]) {
        chain = @[@"globe", @"network",
                  @"antenna.radiowaves.left.and.right"];
    } else {
        chain = @[logicalName];
    }
    if (@available(macOS 11.0, *)) {
        NSImageSymbolConfiguration *cfg =
            [NSImageSymbolConfiguration configurationWithPointSize:pointSize
                                                            weight:NSFontWeightMedium];
        for (NSString *n in chain) {
            NSImage *img = [NSImage imageWithSystemSymbolName:n
                                    accessibilityDescription:nil];
            if (img == nil) continue;
            return [img imageWithSymbolConfiguration:cfg];
        }
    }
    return nil;
}

// stringSize measures a string at a given font.
static NSSize stringSize(NSString *s, NSFont *font) {
    if (s == nil || s.length == 0) return NSZeroSize;
    return [s sizeWithAttributes:@{NSFontAttributeName: font}];
}

// renderCombinedWidget bakes the whole two-widget pill into one NSImage,
// pixel-perfect, so the entire thing sits in a single NSStatusItem
// (one pill) at the system menubar height. Each "widget" is icon +
// two stacked text rows, mirroring iStatistica's layout.
//
//   [thermometer] 49°C   [globe] ↓41 MB
//                 0 rpm          ↑28 MB
//
// The image is marked as a template, so macOS tints it to match the
// menubar's appearance (white on dark, black on light).
static NSImage *renderCombinedWidget(double textSize, double iconSize,
                                      NSString *icon1Name, NSString *r1l, NSString *r2l,
                                      NSString *icon2Name, NSString *r1r, NSString *r2r) {
    NSFont *font = [NSFont systemFontOfSize:textSize weight:NSFontWeightRegular];

    NSSize r1lSz = stringSize(r1l, font);
    NSSize r2lSz = stringSize(r2l, font);
    NSSize r1rSz = stringSize(r1r, font);
    NSSize r2rSz = stringSize(r2r, font);
    CGFloat leftTextW  = MAX(r1lSz.width, r2lSz.width);
    CGFloat rightTextW = MAX(r1rSz.width, r2rSz.width);

    // Layout constants tuned to match iStatistica's spacing.
    const CGFloat iconTextGap = 3;  // gap between icon and its text column
    const CGFloat widgetGap   = 8;  // gap between the two widgets
    const CGFloat sidePadding = 1;  // tiny padding around the whole pill

    CGFloat totalW = sidePadding
                   + iconSize + iconTextGap + leftTextW
                   + widgetGap
                   + iconSize + iconTextGap + rightTextW
                   + sidePadding;
    // Menubar content height — keep below the system menubar (~22pt)
    // so the bar doesn't expand. 18pt fits a 16pt icon plus a hair of
    // padding above and below.
    CGFloat totalH = 18;

    NSImage *out = [NSImage imageWithSize:NSMakeSize(totalW, totalH)
                                  flipped:NO
                           drawingHandler:^BOOL(NSRect rect) {
        // Drawing is in pixel coords; everything is black-on-transparent
        // so the template tint applied by macOS recolours to match the
        // active menubar appearance.
        NSColor *fg = [NSColor blackColor];
        NSDictionary *textAttrs = @{
            NSFontAttributeName: font,
            NSForegroundColorAttributeName: fg,
        };

        CGFloat x = sidePadding;

        // Vertical positions for the two text rows. Top row starts a
        // bit below the top edge; bottom row sits a bit above the
        // bottom edge. The tiny offsets keep the descender legible.
        CGFloat rowTopY    = totalH - textSize - 1;
        CGFloat rowBottomY = 0;

        // Icon 1 — vertically centred in the cell.
        NSImage *icon1 = resolveSymbol(icon1Name, iconSize);
        if (icon1 != nil) {
            NSRect iconRect = NSMakeRect(x, (totalH - iconSize) / 2, iconSize, iconSize);
            // Render the templated symbol in `fg` so the bitmap we're
            // building is itself a template (set on `out` below).
            [icon1 drawInRect:iconRect
                     fromRect:NSZeroRect
                    operation:NSCompositingOperationSourceOver
                     fraction:1.0
               respectFlipped:YES
                        hints:nil];
        }
        x += iconSize + iconTextGap;

        // Text column 1 — two rows, top + bottom.
        if (r1l.length > 0) {
            [r1l drawAtPoint:NSMakePoint(x, rowTopY) withAttributes:textAttrs];
        }
        if (r2l.length > 0) {
            [r2l drawAtPoint:NSMakePoint(x, rowBottomY) withAttributes:textAttrs];
        }
        x += leftTextW + widgetGap;

        // Icon 2.
        NSImage *icon2 = resolveSymbol(icon2Name, iconSize);
        if (icon2 != nil) {
            NSRect iconRect = NSMakeRect(x, (totalH - iconSize) / 2, iconSize, iconSize);
            [icon2 drawInRect:iconRect
                     fromRect:NSZeroRect
                    operation:NSCompositingOperationSourceOver
                     fraction:1.0
               respectFlipped:YES
                        hints:nil];
        }
        x += iconSize + iconTextGap;

        // Text column 2.
        if (r1r.length > 0) {
            [r1r drawAtPoint:NSMakePoint(x, rowTopY) withAttributes:textAttrs];
        }
        if (r2r.length > 0) {
            [r2r drawAtPoint:NSMakePoint(x, rowBottomY) withAttributes:textAttrs];
        }
        return YES;
    }];
    out.template = YES; // adapt to menubar tint (white on dark, etc.)
    return out;
}

// patchPillImage replaces the fyne-owned status item's button.image
// with the freshly-rendered combined widget. Title is cleared so only
// the image draws.
static void patchPillImage(double textSize, double iconSize,
                            const char *icon1Name, const char *r1l, const char *r2l,
                            const char *icon2Name, const char *r1r, const char *r2r) {
    NSString *i1  = icon1Name ? [NSString stringWithUTF8String:icon1Name] : @"";
    NSString *s1l = r1l       ? [NSString stringWithUTF8String:r1l]       : @"";
    NSString *s2l = r2l       ? [NSString stringWithUTF8String:r2l]       : @"";
    NSString *i2  = icon2Name ? [NSString stringWithUTF8String:icon2Name] : @"";
    NSString *s1r = r1r       ? [NSString stringWithUTF8String:r1r]       : @"";
    NSString *s2r = r2r       ? [NSString stringWithUTF8String:r2r]       : @"";
    runOnMain(^{
        @autoreleasepool {
            NSStatusBarButton *button = findOurStatusButton();
            if (button == nil) return;
            NSImage *combined = renderCombinedWidget(textSize, iconSize,
                                                       i1, s1l, s2l,
                                                       i2, s1r, s2r);
            button.image = combined;
            button.title = @"";
            button.attributedTitle = [[NSAttributedString alloc] initWithString:@""];
            button.imagePosition = NSImageOnly;
        }
    });
}
*/
import "C"
import "unsafe"

// setMenubarPill paints the fyne-owned status item as ONE combined pill
// — both widgets rendered into a single NSImage so the menubar height
// stays bounded while the icons can be at iStat-like sizes (~16pt).
func setMenubarPill(textSize, iconSize float64,
	icon1, row1l, row2l, icon2, row1r, row2r string,
) {
	ci1 := C.CString(icon1)
	defer C.free(unsafe.Pointer(ci1))
	cr1l := C.CString(row1l)
	defer C.free(unsafe.Pointer(cr1l))
	cr2l := C.CString(row2l)
	defer C.free(unsafe.Pointer(cr2l))
	ci2 := C.CString(icon2)
	defer C.free(unsafe.Pointer(ci2))
	cr1r := C.CString(row1r)
	defer C.free(unsafe.Pointer(cr1r))
	cr2r := C.CString(row2r)
	defer C.free(unsafe.Pointer(cr2r))
	C.patchPillImage(C.double(textSize), C.double(iconSize),
		ci1, cr1l, cr2l, ci2, cr1r, cr2r)
}

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

    // Resolve icons up-front so we can preserve their natural aspect
    // ratio in the layout. Forcing thermometer (tall-narrow) into a
    // square iconSize box squished it visually; here we keep the
    // symbol's intrinsic width:height and only constrain the height.
    NSImage *icon1 = resolveSymbol(icon1Name, iconSize);
    NSImage *icon2 = resolveSymbol(icon2Name, iconSize);
    CGFloat targetIconH = iconSize;
    CGFloat icon1W = (icon1 != nil && icon1.size.height > 0)
        ? icon1.size.width * (targetIconH / icon1.size.height)
        : iconSize;
    CGFloat icon2W = (icon2 != nil && icon2.size.height > 0)
        ? icon2.size.width * (targetIconH / icon2.size.height)
        : iconSize;

    // Layout constants tuned to match iStatistica's spacing.
    const CGFloat iconTextGap = 3;  // gap between icon and its text column
    const CGFloat widgetGap   = 8;  // gap between the two widgets
    const CGFloat sidePadding = 1;  // tiny padding around the whole pill

    CGFloat totalW = sidePadding
                   + icon1W + iconTextGap + leftTextW
                   + widgetGap
                   + icon2W + iconTextGap + rightTextW
                   + sidePadding;
    // Match the user's actual menubar height (varies by display
    // density: ~22pt on standard, ~24pt on retina-heavy setups).
    // systemStatusBar.thickness gives the live value; fall back to 22
    // if the API returns something unexpected.
    CGFloat totalH = [NSStatusBar systemStatusBar].thickness;
    if (totalH < 18 || totalH > 32) totalH = 22;

    // Row baselines. Bottom row sits 1pt above the bottom edge; top
    // row baseline lands so its ascender clears the descender of the
    // bottom row. textSize is the font's nominal height; the system
    // font's actual line height ~ textSize * 1.17, so we space rows
    // by textSize + 1 which gives clean separation without overlap.
    CGFloat rowBottomY = 1;
    CGFloat rowTopY    = rowBottomY + textSize + 1;

    NSImage *out = [NSImage imageWithSize:NSMakeSize(totalW, totalH)
                                  flipped:NO
                           drawingHandler:^BOOL(NSRect rect) {
        NSColor *fg = [NSColor blackColor];
        NSDictionary *textAttrs = @{
            NSFontAttributeName: font,
            NSForegroundColorAttributeName: fg,
        };

        CGFloat x = sidePadding;

        // Icon 1 — vertically centred, natural aspect ratio preserved.
        if (icon1 != nil) {
            NSRect iconRect = NSMakeRect(x, (totalH - targetIconH) / 2,
                                          icon1W, targetIconH);
            [icon1 drawInRect:iconRect
                     fromRect:NSZeroRect
                    operation:NSCompositingOperationSourceOver
                     fraction:1.0
               respectFlipped:YES
                        hints:nil];
        }
        x += icon1W + iconTextGap;

        if (r1l.length > 0) {
            [r1l drawAtPoint:NSMakePoint(x, rowTopY) withAttributes:textAttrs];
        }
        if (r2l.length > 0) {
            [r2l drawAtPoint:NSMakePoint(x, rowBottomY) withAttributes:textAttrs];
        }
        x += leftTextW + widgetGap;

        if (icon2 != nil) {
            NSRect iconRect = NSMakeRect(x, (totalH - targetIconH) / 2,
                                          icon2W, targetIconH);
            [icon2 drawInRect:iconRect
                     fromRect:NSZeroRect
                    operation:NSCompositingOperationSourceOver
                     fraction:1.0
               respectFlipped:YES
                        hints:nil];
        }
        x += icon2W + iconTextGap;

        if (r1r.length > 0) {
            [r1r drawAtPoint:NSMakePoint(x, rowTopY) withAttributes:textAttrs];
        }
        if (r2r.length > 0) {
            [r2r drawAtPoint:NSMakePoint(x, rowBottomY) withAttributes:textAttrs];
        }
        return YES;
    }];
    out.template = YES;
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

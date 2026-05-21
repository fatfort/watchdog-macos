//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>

// findOurStatusButton walks NSStatusBar's private _items list and
// returns the most recent status item's button — the one fyne.io/systray
// created for us. _items is NSConcretePointerArray on modern macOS, so
// we probe both the NSArray and NSPointerArray APIs.
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

// runOnMain dispatches a block to the main thread. NSStatusItem updates
// (and NSWindow creation) must happen there; calling from a Go goroutine
// raises NSInternalInconsistencyException.
static void runOnMain(void (^block)(void)) {
    if ([NSThread isMainThread]) {
        block();
    } else {
        dispatch_async(dispatch_get_main_queue(), block);
    }
}

// resolveSymbolImage returns an NSImage for the first SF Symbol name in
// `names` that the running OS recognises, configured to the given point
// size and templated for menubar tint. Returns nil if no name matched.
static NSImage *resolveSymbolImage(NSArray<NSString *> *names, double pointSize) {
    if (@available(macOS 11.0, *)) {
        NSImageSymbolConfiguration *cfg =
            [NSImageSymbolConfiguration configurationWithPointSize:pointSize
                                                            weight:NSFontWeightMedium];
        for (NSString *n in names) {
            NSImage *img = [NSImage imageWithSystemSymbolName:n
                                    accessibilityDescription:nil];
            if (img == nil) continue;
            img = [img imageWithSymbolConfiguration:cfg];
            img.template = YES; // monochrome, follows menubar tint
            return img;
        }
    }
    return nil;
}

// symbolImageFor maps a logical name (thermometer, globe) to its SF
// Symbol fallback chain — SF Symbols are sometimes renamed between OS
// releases, so we probe multiple candidates and use the first that
// resolves.
static NSImage *symbolImageFor(NSString *logicalName, double pointSize) {
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
    return resolveSymbolImage(chain, pointSize);
}

// renderTwoLineTitle builds an NSAttributedString with two stacked
// lines of text — no icons (those go in button.image). Tight line
// spacing and a maximum line height keep the rendered block inside the
// menubar's ~22pt allotment.
static NSAttributedString *renderTwoLineTitle(NSString *row1, NSString *row2, double size) {
    NSFont *font = [NSFont menuBarFontOfSize:size];
    NSMutableParagraphStyle *para = [[NSMutableParagraphStyle alloc] init];
    para.alignment = NSTextAlignmentLeft;
    para.lineBreakMode = NSLineBreakByClipping;
    para.lineSpacing = -3.0;
    para.maximumLineHeight = size + 2.0;
    NSDictionary *attrs = @{
        NSFontAttributeName: font,
        NSParagraphStyleAttributeName: para,
    };
    NSMutableAttributedString *out = [[NSMutableAttributedString alloc] init];
    [out appendAttributedString:[[NSAttributedString alloc] initWithString:(row1 ?: @"")
                                                                attributes:attrs]];
    [out appendAttributedString:[[NSAttributedString alloc] initWithString:@"\n"
                                                                attributes:attrs]];
    [out appendAttributedString:[[NSAttributedString alloc] initWithString:(row2 ?: @"")
                                                                attributes:attrs]];
    [out addAttribute:NSParagraphStyleAttributeName
                value:para
                range:NSMakeRange(0, out.length)];
    return out;
}

// patchPrimaryWidget paints the fyne-owned status item as the thermal
// widget: SF Symbol image to the left, two-line text to the right.
static void patchPrimaryWidget(double textSize, double iconSize,
                                const char *symbolName,
                                const char *row1, const char *row2) {
    NSString *sym = symbolName ? [NSString stringWithUTF8String:symbolName] : @"";
    NSString *r1  = row1 ? [NSString stringWithUTF8String:row1] : @"";
    NSString *r2  = row2 ? [NSString stringWithUTF8String:row2] : @"";
    runOnMain(^{
        @autoreleasepool {
            NSStatusBarButton *button = findOurStatusButton();
            if (button == nil) return;
            NSImage *img = symbolImageFor(sym, iconSize);
            if (img != nil) button.image = img;
            button.imagePosition = NSImageLeading;
            button.imageHugsTitle = YES;
            button.cell.usesSingleLineMode = NO;
            button.cell.lineBreakMode = NSLineBreakByClipping;
            button.attributedTitle = renderTwoLineTitle(r1, r2, textSize);
        }
    });
}

// Secondary NSStatusItem held across collect ticks so updates don't leak
// new status items. Created lazily on first patchSecondaryWidget call.
static NSStatusItem *gSecondaryItem = nil;

// patchSecondaryWidget creates (if needed) and updates a second
// NSStatusItem placed next to the primary — the network widget,
// mirroring how iStat / iStatistica split temperature and network into
// independent menubar items.
static void patchSecondaryWidget(double textSize, double iconSize,
                                  const char *symbolName,
                                  const char *row1, const char *row2) {
    NSString *sym = symbolName ? [NSString stringWithUTF8String:symbolName] : @"";
    NSString *r1  = row1 ? [NSString stringWithUTF8String:row1] : @"";
    NSString *r2  = row2 ? [NSString stringWithUTF8String:row2] : @"";
    runOnMain(^{
        @autoreleasepool {
            if (gSecondaryItem == nil) {
                NSStatusBar *bar = [NSStatusBar systemStatusBar];
                gSecondaryItem = [bar statusItemWithLength:NSVariableStatusItemLength];
                CFRetain((__bridge CFTypeRef)gSecondaryItem);
                gSecondaryItem.button.cell.usesSingleLineMode = NO;
                gSecondaryItem.button.cell.lineBreakMode = NSLineBreakByClipping;
                gSecondaryItem.button.imagePosition = NSImageLeading;
                gSecondaryItem.button.imageHugsTitle = YES;
            }
            NSImage *img = symbolImageFor(sym, iconSize);
            if (img != nil) gSecondaryItem.button.image = img;
            gSecondaryItem.button.attributedTitle = renderTwoLineTitle(r1, r2, textSize);
        }
    });
}
*/
import "C"
import "unsafe"

// setPrimaryWidget paints the fyne-owned status item with a left-icon +
// two-row-text layout: thermal widget.
func setPrimaryWidget(textSize, iconSize float64, symbol, row1, row2 string) {
	cs := C.CString(symbol)
	defer C.free(unsafe.Pointer(cs))
	cr1 := C.CString(row1)
	defer C.free(unsafe.Pointer(cr1))
	cr2 := C.CString(row2)
	defer C.free(unsafe.Pointer(cr2))
	C.patchPrimaryWidget(C.double(textSize), C.double(iconSize), cs, cr1, cr2)
}

// setSecondaryWidget creates (lazily) and updates a second status item
// for the network widget — together with the primary widget they
// reproduce the iStat side-by-side layout.
func setSecondaryWidget(textSize, iconSize float64, symbol, row1, row2 string) {
	cs := C.CString(symbol)
	defer C.free(unsafe.Pointer(cs))
	cr1 := C.CString(row1)
	defer C.free(unsafe.Pointer(cr1))
	cr2 := C.CString(row2)
	defer C.free(unsafe.Pointer(cr2))
	C.patchSecondaryWidget(C.double(textSize), C.double(iconSize), cs, cr1, cr2)
}

//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>

// findOurStatusButton walks NSStatusBar's private _items list (returned
// as NSConcretePointerArray on modern macOS) and returns the most recent
// status item's button — that's the one fyne.io/systray created for us.
// Returns nil on any failure so callers can short-circuit.
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

// sfSymbolAttachment builds an NSTextAttachment from an SF Symbol that
// renders as a monochrome glyph adopting the menubar's foreground colour
// (white on dark mode, black on light) — same look as native macOS
// menubar icons. Returns nil if the symbol name isn't recognised on the
// current OS (we degrade by skipping the icon rather than failing).
static NSTextAttachment *sfSymbolAttachment(NSString *name, double size) {
    if (@available(macOS 11.0, *)) {
        NSImageSymbolConfiguration *cfg =
            [NSImageSymbolConfiguration configurationWithPointSize:size + 2
                                                            weight:NSFontWeightMedium];
        NSImage *img = [NSImage imageWithSystemSymbolName:name
                                accessibilityDescription:nil];
        if (img == nil) return nil;
        img = [img imageWithSymbolConfiguration:cfg];
        img.template = YES; // monochrome, follows menubar tint
        NSTextAttachment *att = [[NSTextAttachment alloc] init];
        att.image = img;
        return att;
    }
    return nil;
}

// renderTwoRowTitle returns an NSAttributedString for a single status-item
// button: one SF Symbol icon followed by two stacked text rows. Used for
// both the primary (fyne) and secondary (cgo-created) status items so the
// two side-by-side widgets share a layout.
static NSAttributedString *renderTwoRowTitle(double size,
                                              NSString *symbol,
                                              NSString *row1,
                                              NSString *row2) {
    NSFont *font = [NSFont menuBarFontOfSize:size];
    NSMutableParagraphStyle *para = [[NSMutableParagraphStyle alloc] init];
    para.alignment = NSTextAlignmentLeft;
    para.lineBreakMode = NSLineBreakByClipping;
    para.lineSpacing = 1.0;
    para.paragraphSpacingBefore = 1.0;
    NSDictionary *attrs = @{
        NSFontAttributeName: font,
        NSParagraphStyleAttributeName: para,
    };

    NSMutableAttributedString *out = [[NSMutableAttributedString alloc] init];
    NSString *iconRow = @"";
    if (symbol.length > 0) {
        NSTextAttachment *att = sfSymbolAttachment(symbol, size);
        if (att != nil) {
            [out appendAttributedString:
                [NSAttributedString attributedStringWithAttachment:att]];
            [out appendAttributedString:
                [[NSAttributedString alloc] initWithString:@" " attributes:attrs]];
            iconRow = @"  "; // pad row 2 so digits align under digits, not the icon
        }
    }
    if (row1.length > 0) {
        [out appendAttributedString:
            [[NSAttributedString alloc] initWithString:row1 attributes:attrs]];
    }
    [out appendAttributedString:
        [[NSAttributedString alloc] initWithString:@"\n" attributes:attrs]];
    [out appendAttributedString:
        [[NSAttributedString alloc] initWithString:iconRow attributes:attrs]];
    if (row2.length > 0) {
        [out appendAttributedString:
            [[NSAttributedString alloc] initWithString:row2 attributes:attrs]];
    }
    [out addAttribute:NSParagraphStyleAttributeName
                value:para
                range:NSMakeRange(0, out.length)];
    return out;
}

// Strings flow in from Go on a worker goroutine; copy them out of the C
// pointer immediately so the block can safely run on the main queue.
static void runOnMain(void (^block)(void)) {
    if ([NSThread isMainThread]) {
        block();
    } else {
        dispatch_async(dispatch_get_main_queue(), block);
    }
}

// patchMenubarTwoRow paints the primary (fyne-owned) status item as a
// single icon + two-row widget. Used for the thermal pair.
static void patchMenubarTwoRow(double size,
                                const char *symbol,
                                const char *row1, const char *row2) {
    NSString *sym  = symbol ? [NSString stringWithUTF8String:symbol] : @"";
    NSString *r1   = row1   ? [NSString stringWithUTF8String:row1]   : @"";
    NSString *r2   = row2   ? [NSString stringWithUTF8String:row2]   : @"";
    runOnMain(^{
        @autoreleasepool {
            NSStatusBarButton *button = findOurStatusButton();
            if (button == nil) return;
            NSFont *font = [NSFont menuBarFontOfSize:size];
            button.font = font;
            button.cell.usesSingleLineMode = NO;
            button.cell.lineBreakMode = NSLineBreakByClipping;
            button.attributedTitle = renderTwoRowTitle(size, sym, r1, r2);
        }
    });
}

// Secondary NSStatusItem (held for the lifetime of the menubar process)
// so we can update its title across collect ticks without leaking new
// status items on every paint.
static NSStatusItem *gSecondaryItem = nil;

// patchMenubarSecondary creates (on first call) and then updates a second
// status item placed alongside the primary. This is the side-by-side
// widget we use for the network pair, mirroring how iStat Menus splits
// "Temperature" and "Network" into separate menubar items.
//
// NSStatusItem (and the NSWindow it backs) MUST be created on the main
// thread — calling -statusItemWithLength: from a Go goroutine raises
// NSInternalInconsistencyException with "NSWindow should only be
// instantiated on the main thread!" — so we hop via dispatch_async.
static void patchMenubarSecondary(double size,
                                   const char *symbol,
                                   const char *row1, const char *row2) {
    NSString *sym  = symbol ? [NSString stringWithUTF8String:symbol] : @"";
    NSString *r1   = row1   ? [NSString stringWithUTF8String:row1]   : @"";
    NSString *r2   = row2   ? [NSString stringWithUTF8String:row2]   : @"";
    runOnMain(^{
        @autoreleasepool {
            if (gSecondaryItem == nil) {
                NSStatusBar *bar = [NSStatusBar systemStatusBar];
                gSecondaryItem = [bar statusItemWithLength:NSVariableStatusItemLength];
                CFRetain((__bridge CFTypeRef)gSecondaryItem);
                gSecondaryItem.button.cell.usesSingleLineMode = NO;
                gSecondaryItem.button.cell.lineBreakMode = NSLineBreakByClipping;
            }
            NSFont *font = [NSFont menuBarFontOfSize:size];
            gSecondaryItem.button.font = font;
            gSecondaryItem.button.attributedTitle = renderTwoRowTitle(size, sym, r1, r2);
        }
    });
}
*/
import "C"
import "unsafe"

// setMenubarPrimary paints the existing (fyne-owned) status item as
// "icon over two rows of text" — used for the thermal widget.
func setMenubarPrimary(size float64, symbol, row1, row2 string) {
	cs := C.CString(symbol)
	defer C.free(unsafe.Pointer(cs))
	cr1 := C.CString(row1)
	defer C.free(unsafe.Pointer(cr1))
	cr2 := C.CString(row2)
	defer C.free(unsafe.Pointer(cr2))
	C.patchMenubarTwoRow(C.double(size), cs, cr1, cr2)
}

// setMenubarSecondary creates (lazily) and updates a second NSStatusItem
// next to the primary one — used for the network widget. Together they
// give the side-by-side iStat layout.
func setMenubarSecondary(size float64, symbol, row1, row2 string) {
	cs := C.CString(symbol)
	defer C.free(unsafe.Pointer(cs))
	cr1 := C.CString(row1)
	defer C.free(unsafe.Pointer(cr1))
	cr2 := C.CString(row2)
	defer C.free(unsafe.Pointer(cr2))
	C.patchMenubarSecondary(C.double(size), cs, cr1, cr2)
}

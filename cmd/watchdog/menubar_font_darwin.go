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

// sfSymbolAttachmentChain tries each name in order, returning the first
// SF Symbol the running OS recognises. Sized for the icon to span two
// menubar rows (~1.7x the text font), and offset down so the bottom of
// the glyph drops into the second row's visual area.
static NSTextAttachment *sfSymbolAttachmentChain(NSArray<NSString *> *names,
                                                   double textSize) {
    if (@available(macOS 11.0, *)) {
        // Icon point size tuned so the symbol matches two stacked text
        // rows in height — at 7pt font + lineSpacing -3 the total row
        // pair is ~12pt tall, so we want the icon glyph in that
        // ballpark. 1.6x gives slight overshoot which reads as "spans
        // both rows" without expanding the menubar height.
        NSImageSymbolConfiguration *cfg =
            [NSImageSymbolConfiguration configurationWithPointSize:textSize * 1.6
                                                            weight:NSFontWeightMedium];
        for (NSString *n in names) {
            NSImage *img = [NSImage imageWithSystemSymbolName:n
                                    accessibilityDescription:nil];
            if (img == nil) continue;
            img = [img imageWithSymbolConfiguration:cfg];
            img.template = YES; // monochrome, follows menubar tint
            NSTextAttachment *att = [[NSTextAttachment alloc] init];
            att.image = img;
            // Small downward offset so the glyph's vertical centre lands
            // between the two text rows. Larger offsets pushed the
            // menubar's total height above the system limit (~22pt).
            CGFloat h = img.size.height;
            CGFloat w = img.size.width;
            att.bounds = NSMakeRect(0, -h * 0.18, w, h);
            return att;
        }
    }
    return nil;
}

// sfSymbolAttachment resolves a single canonical name with a fallback
// chain — SF Symbols are sometimes renamed between releases.
static NSTextAttachment *sfSymbolAttachment(NSString *name, double size) {
    NSArray *chain;
    if ([name isEqualToString:@"thermometer.medium"]) {
        chain = @[@"thermometer.medium", @"thermometer", @"thermometer.high",
                  @"thermometer.snowflake", @"flame.fill"];
    } else if ([name isEqualToString:@"globe"]) {
        chain = @[@"globe", @"network", @"antenna.radiowaves.left.and.right"];
    } else {
        chain = @[name];
    }
    return sfSymbolAttachmentChain(chain, size);
}

// renderCombinedTitle builds the 2-row / 4-column attributed string for
// the single NSStatusItem (one pill, side-by-side widgets):
//
//   [icon1] row1l   [icon2] row1r
//           row2l           row2r
//
// Tab stops keep the second column's arrows aligned under each other
// regardless of row-1 width. Each icon attachment is sized big enough
// (and offset down) to span the visual height of both rows.
static NSAttributedString *renderCombinedTitle(double size,
                                                NSString *icon1, NSString *r1l, NSString *r2l,
                                                NSString *icon2, NSString *r1r, NSString *r2r) {
    NSFont *font = [NSFont menuBarFontOfSize:size];

    // Tab stops scale with font; tuned empirically against the system
    // menubar font at 10pt — the icon column gets ~1.7em, the text
    // column gets ~4.5em, repeat for the second pair.
    CGFloat unit = size;
    CGFloat textCol1 = unit * 1.9;
    CGFloat iconCol2 = textCol1 + unit * 4.5;
    CGFloat textCol2 = iconCol2 + unit * 1.9;

    NSMutableParagraphStyle *para = [[NSMutableParagraphStyle alloc] init];
    para.alignment = NSTextAlignmentLeft;
    para.lineBreakMode = NSLineBreakByClipping;
    para.lineSpacing = -3.0;             // tight enough to fit menubar height
    para.paragraphSpacingBefore = 0;
    para.maximumLineHeight = size + 2;   // hard ceiling so we never push the bar taller
    para.tabStops = @[
        [[NSTextTab alloc] initWithType:NSLeftTabStopType location:textCol1],
        [[NSTextTab alloc] initWithType:NSLeftTabStopType location:iconCol2],
        [[NSTextTab alloc] initWithType:NSLeftTabStopType location:textCol2],
    ];
    para.defaultTabInterval = textCol2 + unit * 4;

    NSDictionary *attrs = @{
        NSFontAttributeName: font,
        NSParagraphStyleAttributeName: para,
    };
    NSAttributedString *(^t)(NSString *) = ^(NSString *s) {
        return [[NSAttributedString alloc] initWithString:s attributes:attrs];
    };

    NSMutableAttributedString *out = [[NSMutableAttributedString alloc] init];

    // Row 1: [icon1]\trow1l\t[icon2]\trow1r
    if (icon1.length > 0) {
        NSTextAttachment *a = sfSymbolAttachment(icon1, size);
        if (a) [out appendAttributedString:
            [NSAttributedString attributedStringWithAttachment:a]];
    }
    [out appendAttributedString:t(@"\t")];
    [out appendAttributedString:t(r1l ?: @"")];
    [out appendAttributedString:t(@"\t")];
    if (icon2.length > 0) {
        NSTextAttachment *a = sfSymbolAttachment(icon2, size);
        if (a) [out appendAttributedString:
            [NSAttributedString attributedStringWithAttachment:a]];
    }
    [out appendAttributedString:t(@"\t")];
    [out appendAttributedString:t(r1r ?: @"")];

    // Row 2: \trow2l\t\trow2r — icon column empty, text columns at the
    // same tab stops so digits and arrows align under their row-1
    // counterparts.
    [out appendAttributedString:t(@"\n\t")];
    [out appendAttributedString:t(r2l ?: @"")];
    [out appendAttributedString:t(@"\t\t")];
    [out appendAttributedString:t(r2r ?: @"")];

    [out addAttribute:NSParagraphStyleAttributeName
                value:para
                range:NSMakeRange(0, out.length)];
    return out;
}

// patchMenubarCombined paints the fyne-owned status item with the
// 2x2 combined widget on the main thread.
static void patchMenubarCombined(double size,
                                  const char *icon1, const char *row1l, const char *row2l,
                                  const char *icon2, const char *row1r, const char *row2r) {
    NSString *i1  = icon1 ? [NSString stringWithUTF8String:icon1] : @"";
    NSString *r1l = row1l ? [NSString stringWithUTF8String:row1l] : @"";
    NSString *r2l = row2l ? [NSString stringWithUTF8String:row2l] : @"";
    NSString *i2  = icon2 ? [NSString stringWithUTF8String:icon2] : @"";
    NSString *r1r = row1r ? [NSString stringWithUTF8String:row1r] : @"";
    NSString *r2r = row2r ? [NSString stringWithUTF8String:row2r] : @"";
    runOnMain(^{
        @autoreleasepool {
            NSStatusBarButton *button = findOurStatusButton();
            if (button == nil) return;
            NSFont *font = [NSFont menuBarFontOfSize:size];
            button.font = font;
            button.cell.usesSingleLineMode = NO;
            button.cell.lineBreakMode = NSLineBreakByClipping;
            button.attributedTitle = renderCombinedTitle(size, i1, r1l, r2l, i2, r1r, r2r);
        }
    });
}
*/
import "C"
import "unsafe"

// setMenubarCombined paints the fyne-owned status item as a 2-row /
// 4-column widget: [icon1] row1l  [icon2] row1r above row2l / row2r,
// arrows aligned to the second-column tab stop.
func setMenubarCombined(size float64,
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
	C.patchMenubarCombined(C.double(size), ci1, cr1l, cr2l, ci2, cr1r, cr2r)
}

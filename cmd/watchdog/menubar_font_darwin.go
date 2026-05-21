//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>

// patchMenubarFont reaches into NSStatusBar to find the status item that
// fyne.io/systray just created and:
//
//  1. Shrinks the button font (size pt) so two stacked lines fit.
//  2. Builds an NSAttributedString with paragraph style allowing newlines,
//     and pins it via setAttributedTitle: — the plain title setter strips
//     newlines unless line-break mode is set on the cell *and* the title
//     is attributed.
//
// imagePosition is deliberately untouched: setting it to NSImageLeft when
// the title is multi-line shifted the icon out of the button's drawing
// rect on macOS Sequoia, making it disappear. fyne.io/systray's default
// (NSImageOnly upgraded to NSImageLeading once a title is set) is what we
// want.
//
// Uses NSStatusBar's private `_items` ivar — stable since 10.6 and the
// same path Bartender / iStat Menus / every status-item theming app uses.
static void patchMenubarFont(double size, const char *text) {
    @autoreleasepool {
        NSStatusBar *bar = [NSStatusBar systemStatusBar];
        // _items returns NSConcretePointerArray on modern macOS, not NSArray —
        // calling -lastObject on that class throws NSInvalidArgumentException.
        // Probe both APIs (NSArray-like and NSPointerArray-like) so we work
        // across Apple's KVC return-type changes.
        id items = [bar valueForKey:@"_items"];
        if (items == nil) return;
        NSUInteger n = 0;
        if ([items respondsToSelector:@selector(count)]) {
            n = (NSUInteger)[items count];
        }
        if (n == 0) return;
        NSStatusItem *item = nil;
        if ([items respondsToSelector:@selector(objectAtIndex:)]) {
            item = (NSStatusItem *)[items objectAtIndex:n - 1];
        } else if ([items respondsToSelector:@selector(pointerAtIndex:)]) {
            void *ptr = [items pointerAtIndex:n - 1];
            item = (__bridge NSStatusItem *)ptr;
        }
        if (item == nil) return;
        NSStatusBarButton *button = item.button;
        if (button == nil) return;

        NSFont *font = [NSFont menuBarFontOfSize:size];
        button.font = font;
        button.cell.usesSingleLineMode = NO;
        button.cell.lineBreakMode = NSLineBreakByClipping;

        if (text != NULL && text[0] != '\0') {
            NSString *s = [NSString stringWithUTF8String:text];
            NSMutableParagraphStyle *para = [[NSMutableParagraphStyle alloc] init];
            para.alignment = NSTextAlignmentLeft;
            para.lineBreakMode = NSLineBreakByClipping;
            para.lineSpacing = -2; // pull the two rows tight
            NSDictionary *attrs = @{
                NSFontAttributeName: font,
                NSParagraphStyleAttributeName: para,
            };
            NSAttributedString *attr =
                [[NSAttributedString alloc] initWithString:s attributes:attrs];
            button.attributedTitle = attr;
        }
    }
}
*/
import "C"
import "unsafe"

// setMenubarFontSize patches the status-item button to use a smaller font
// and multi-line layout. Pass an empty `title` on first call (to apply
// font + cell settings only); pass the live title on subsequent calls
// from paintTitle to refresh the attributed string and keep the
// two-line layout consistent across updates.
func setMenubarFontSize(size float64, title string) {
	cText := C.CString(title)
	defer C.free(unsafe.Pointer(cText))
	C.patchMenubarFont(C.double(size), cText)
}

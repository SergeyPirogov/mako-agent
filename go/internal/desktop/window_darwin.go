//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include <stdlib.h>

static NSWindow *makoWindow;
static WKWebView *makoView;
static NSString *makoHost;

// The board's own pages stay in the window; links elsewhere (the MR, the issue) open in the browser.
// JavaScript's alert/confirm/prompt get native dialogs: WKWebView drops them otherwise.
@interface MakoWindowDelegate : NSObject <WKNavigationDelegate, WKUIDelegate, NSWindowDelegate>
@end

@implementation MakoWindowDelegate
- (BOOL)isBoard:(NSURL *)u {
	return [u.host isEqualToString:makoHost] || [u.scheme isEqualToString:@"about"];
}
- (void)openOutside:(NSURL *)u {
	if ([u.scheme isEqualToString:@"http"] || [u.scheme isEqualToString:@"https"] || [u.scheme isEqualToString:@"mailto"]) {
		[[NSWorkspace sharedWorkspace] openURL:u];
	}
}
- (void)webView:(WKWebView *)wv decidePolicyForNavigationAction:(WKNavigationAction *)a decisionHandler:(void (^)(WKNavigationActionPolicy))decide {
	NSURL *u = a.request.URL;
	if (a.targetFrame != nil && [self isBoard:u]) {
		decide(WKNavigationActionPolicyAllow);
		return;
	}
	[self openOutside:u];
	decide(WKNavigationActionPolicyCancel);
}
- (WKWebView *)webView:(WKWebView *)wv createWebViewWithConfiguration:(WKWebViewConfiguration *)c forNavigationAction:(WKNavigationAction *)a windowFeatures:(WKWindowFeatures *)f {
	[self openOutside:a.request.URL];
	return nil;
}
- (void)webView:(WKWebView *)wv runJavaScriptAlertPanelWithMessage:(NSString *)message initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(void))done {
	NSAlert *alert = [NSAlert new];
	alert.messageText = message;
	[alert addButtonWithTitle:@"OK"];
	[alert beginSheetModalForWindow:makoWindow completionHandler:^(NSModalResponse r) { done(); }];
}
- (void)webView:(WKWebView *)wv runJavaScriptConfirmPanelWithMessage:(NSString *)message initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(BOOL))done {
	NSAlert *alert = [NSAlert new];
	alert.messageText = message;
	[alert addButtonWithTitle:@"OK"];
	[alert addButtonWithTitle:@"Скасувати"];
	[alert beginSheetModalForWindow:makoWindow completionHandler:^(NSModalResponse r) { done(r == NSAlertFirstButtonReturn); }];
}
- (void)webView:(WKWebView *)wv runJavaScriptTextInputPanelWithPrompt:(NSString *)prompt defaultText:(NSString *)text initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(NSString *))done {
	NSAlert *alert = [NSAlert new];
	alert.messageText = prompt;
	[alert addButtonWithTitle:@"OK"];
	[alert addButtonWithTitle:@"Скасувати"];
	NSTextField *field = [[NSTextField alloc] initWithFrame:NSMakeRect(0, 0, 320, 24)];
	field.stringValue = text ?: @"";
	alert.accessoryView = field;
	alert.window.initialFirstResponder = field;
	[alert beginSheetModalForWindow:makoWindow completionHandler:^(NSModalResponse r) {
		done(r == NSAlertFirstButtonReturn ? field.stringValue : nil);
	}];
}
// With the window closed Mako goes back to the menu bar only: no Dock icon.
- (void)windowWillClose:(NSNotification *)n {
	makoView = nil;
	makoWindow = nil;
	[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
}
@end

static MakoWindowDelegate *makoDelegate;

static NSMenuItem *menuItem(NSString *title, SEL action, NSString *key) {
	return [[NSMenuItem alloc] initWithTitle:title action:action keyEquivalent:key];
}

// A menu bar app has no main menu, and without one ⌘C, ⌘V, ⌘A, ⌘W do nothing in the window.
static void makoMainMenu(void) {
	NSMenu *bar = [NSMenu new];

	NSMenu *app = [[NSMenu alloc] initWithTitle:@"Mako"];
	[app addItem:menuItem(@"Сховати Mako", @selector(hide:), @"h")];
	[app addItem:[NSMenuItem separatorItem]];
	[app addItem:menuItem(@"Вийти з Mako", @selector(terminate:), @"q")];
	NSMenuItem *appItem = [NSMenuItem new];
	appItem.submenu = app;
	[bar addItem:appItem];

	NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Редагування"];
	[edit addItem:menuItem(@"Скасувати", @selector(undo:), @"z")];
	NSMenuItem *redo = menuItem(@"Повторити", @selector(redo:), @"z");
	redo.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagShift;
	[edit addItem:redo];
	[edit addItem:[NSMenuItem separatorItem]];
	[edit addItem:menuItem(@"Вирізати", @selector(cut:), @"x")];
	[edit addItem:menuItem(@"Копіювати", @selector(copy:), @"c")];
	[edit addItem:menuItem(@"Вставити", @selector(paste:), @"v")];
	[edit addItem:menuItem(@"Виділити все", @selector(selectAll:), @"a")];
	NSMenuItem *editItem = [NSMenuItem new];
	editItem.submenu = edit;
	[bar addItem:editItem];

	NSMenu *view = [[NSMenu alloc] initWithTitle:@"Вигляд"];
	[view addItem:menuItem(@"Оновити", @selector(reload:), @"r")];
	NSMenuItem *viewItem = [NSMenuItem new];
	viewItem.submenu = view;
	[bar addItem:viewItem];

	NSMenu *window = [[NSMenu alloc] initWithTitle:@"Вікно"];
	[window addItem:menuItem(@"Закрити", @selector(performClose:), @"w")];
	[window addItem:menuItem(@"Згорнути", @selector(performMiniaturize:), @"m")];
	NSMenuItem *windowItem = [NSMenuItem new];
	windowItem.submenu = window;
	[bar addItem:windowItem];
	NSApp.windowsMenu = window;

	NSApp.mainMenu = bar;
}

static void makoOpenWindow(const char *url) {
	NSString *address = [NSString stringWithUTF8String:url];
	dispatch_async(dispatch_get_main_queue(), ^{
		NSURL *u = [NSURL URLWithString:address];
		makoHost = u.host;
		if (makoWindow == nil) {
			if (makoDelegate == nil) {
				makoDelegate = [MakoWindowDelegate new];
			}
			makoMainMenu();
			NSRect frame = NSMakeRect(0, 0, 1240, 820);
			makoWindow = [[NSWindow alloc] initWithContentRect:frame
				styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
				backing:NSBackingStoreBuffered defer:NO];
			makoWindow.releasedWhenClosed = NO;
			makoWindow.title = @"Mako";
			makoWindow.minSize = NSMakeSize(420, 480);
			makoWindow.delegate = makoDelegate;
			[makoWindow center];
			[makoWindow setFrameAutosaveName:@"MakoBoard"];
			WKWebViewConfiguration *config = [WKWebViewConfiguration new];
			makoView = [[WKWebView alloc] initWithFrame:frame configuration:config];
			makoView.navigationDelegate = makoDelegate;
			makoView.UIDelegate = makoDelegate;
			makoView.allowsBackForwardNavigationGestures = YES;
			makoWindow.contentView = makoView;
			[makoView loadRequest:[NSURLRequest requestWithURL:u]];
		} else if (![makoView.URL.absoluteString isEqualToString:address]) {
			[makoView loadRequest:[NSURLRequest requestWithURL:u]];
		}
		// While the window is open Mako is a normal app: in the Dock and in ⌘Tab.
		[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
		[makoWindow makeKeyAndOrderFront:nil];
		[NSApp activateIgnoringOtherApps:YES];
	});
}
*/
import "C"

import "unsafe"

// HasWindow: on macOS the board opens in Mako's own window (WebKit).
func HasWindow() bool { return true }

// OpenWindow shows url in Mako's window, making it if needed. It needs the menu bar's run loop (Run).
func OpenWindow(url string) {
	s := C.CString(url)
	defer C.free(unsafe.Pointer(s))
	C.makoOpenWindow(s)
}

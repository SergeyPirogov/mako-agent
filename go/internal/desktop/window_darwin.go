//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit -framework UserNotifications

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <UserNotifications/UserNotifications.h>
#include <stdlib.h>

static NSWindow *makoWindow;
static WKWebView *makoView;
static NSString *makoHost;
static NSDictionary *makoWords;

// L: the window's word for key in Mako's language (makoSetWords), else the key.
static NSString *L(NSString *key) {
	NSString *w = makoWords[key];
	return w ?: key;
}

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
	[alert addButtonWithTitle:L(@"menu.cancel")];
	[alert beginSheetModalForWindow:makoWindow completionHandler:^(NSModalResponse r) { done(r == NSAlertFirstButtonReturn); }];
}
- (void)webView:(WKWebView *)wv runJavaScriptTextInputPanelWithPrompt:(NSString *)prompt defaultText:(NSString *)text initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(NSString *))done {
	NSAlert *alert = [NSAlert new];
	alert.messageText = prompt;
	[alert addButtonWithTitle:@"OK"];
	[alert addButtonWithTitle:L(@"menu.cancel")];
	NSTextField *field = [[NSTextField alloc] initWithFrame:NSMakeRect(0, 0, 320, 24)];
	field.stringValue = text ?: @"";
	alert.accessoryView = field;
	alert.window.initialFirstResponder = field;
	[alert beginSheetModalForWindow:makoWindow completionHandler:^(NSModalResponse r) {
		done(r == NSAlertFirstButtonReturn ? field.stringValue : nil);
	}];
}
// Closing only hides the window, so the open task, its tab and the scroll are there when it comes back; Mako goes
// back to the menu bar only: no Dock icon.
- (BOOL)windowShouldClose:(NSWindow *)w {
	[w orderOut:nil];
	[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
	return NO;
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
	[app addItem:menuItem(L(@"menu.hide"), @selector(hide:), @"h")];
	[app addItem:[NSMenuItem separatorItem]];
	[app addItem:menuItem(L(@"tray.quit"), @selector(terminate:), @"q")];
	NSMenuItem *appItem = [NSMenuItem new];
	appItem.submenu = app;
	[bar addItem:appItem];

	NSMenu *edit = [[NSMenu alloc] initWithTitle:L(@"menu.edit")];
	[edit addItem:menuItem(L(@"menu.undo"), @selector(undo:), @"z")];
	NSMenuItem *redo = menuItem(L(@"menu.redo"), @selector(redo:), @"z");
	redo.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagShift;
	[edit addItem:redo];
	[edit addItem:[NSMenuItem separatorItem]];
	[edit addItem:menuItem(L(@"menu.cut"), @selector(cut:), @"x")];
	[edit addItem:menuItem(L(@"menu.copy"), @selector(copy:), @"c")];
	[edit addItem:menuItem(L(@"menu.paste"), @selector(paste:), @"v")];
	[edit addItem:menuItem(L(@"menu.selectAll"), @selector(selectAll:), @"a")];
	NSMenuItem *editItem = [NSMenuItem new];
	editItem.submenu = edit;
	[bar addItem:editItem];

	NSMenu *view = [[NSMenu alloc] initWithTitle:L(@"menu.view")];
	[view addItem:menuItem(L(@"menu.reload"), @selector(reload:), @"r")];
	NSMenuItem *viewItem = [NSMenuItem new];
	viewItem.submenu = view;
	[bar addItem:viewItem];

	NSMenu *window = [[NSMenu alloc] initWithTitle:L(@"menu.window")];
	[window addItem:menuItem(L(@"menu.close"), @selector(performClose:), @"w")];
	[window addItem:menuItem(L(@"menu.minimize"), @selector(performMiniaturize:), @"m")];
	NSMenuItem *windowItem = [NSMenuItem new];
	windowItem.submenu = window;
	[bar addItem:windowItem];
	NSApp.windowsMenu = window;

	NSApp.mainMenu = bar;
}

// makoSetWords: the window's words (a JSON object), the main menu made again in them once there is one.
static void makoSetWords(const char *json) {
	NSData *data = [[NSString stringWithUTF8String:json] dataUsingEncoding:NSUTF8StringEncoding];
	NSDictionary *words = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
	dispatch_async(dispatch_get_main_queue(), ^{
		makoWords = words;
		if (makoWindow != nil) {
			makoMainMenu();
		}
	});
}

// The board without a task (the menu, opening Mako again) keeps the page as it is; a task or another page goes there.
static BOOL makoGoesElsewhere(NSURL *now, NSURL *to) {
	if (now == nil || to.fragment.length > 0) {
		return ![now.absoluteString isEqualToString:to.absoluteString];
	}
	NSString *a = now.path.length ? now.path : @"/", *b = to.path.length ? to.path : @"/";
	return ![a isEqualToString:b] || ![now.host isEqualToString:to.host] || ![now.port isEqual:to.port];
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
		} else if (makoGoesElsewhere(makoView.URL, u)) {
			[makoView loadRequest:[NSURLRequest requestWithURL:u]];
		}
		// While the window is open Mako is a normal app: in the Dock and in ⌘Tab.
		[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
		[makoWindow makeKeyAndOrderFront:nil];
		[NSApp activateIgnoringOtherApps:YES];
	});
}

// Opening Mako again (Finder, Launchpad, the Dock, `open`) sends the running app a reopen event: show the board.
@interface MakoReopen : NSObject
@property (copy) NSString *url;
@end

@implementation MakoReopen
- (void)reopen:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply {
	makoOpenWindow(self.url.UTF8String);
}
@end

static MakoReopen *makoReopen;

static void makoOnReopen(const char *url) {
	NSString *address = [NSString stringWithUTF8String:url];
	dispatch_async(dispatch_get_main_queue(), ^{
		makoReopen = [MakoReopen new];
		makoReopen.url = address;
		[[NSAppleEventManager sharedAppleEventManager] setEventHandler:makoReopen
			andSelector:@selector(reopen:withReplyEvent:) forEventClass:kCoreEventClass andEventID:kAEReopenApplication];
	});
}

// Notifications from Mako.app itself: osascript's are Script Editor's, and a click opens Script Editor. A click
// on Mako's opens the task in Mako's window (or the MR in the browser).
@interface MakoNotices : NSObject <UNUserNotificationCenterDelegate>
@end

static MakoNotices *makoNotices;
static NSURL *makoBoard;
static BOOL makoAllowed;

@implementation MakoNotices
- (void)userNotificationCenter:(UNUserNotificationCenter *)c willPresentNotification:(UNNotification *)n withCompletionHandler:(void (^)(UNNotificationPresentationOptions))done {
	done(UNNotificationPresentationOptionBanner | UNNotificationPresentationOptionList | UNNotificationPresentationOptionSound);
}
- (void)userNotificationCenter:(UNUserNotificationCenter *)c didReceiveNotificationResponse:(UNNotificationResponse *)r withCompletionHandler:(void (^)(void))done {
	NSURL *u = [NSURL URLWithString:r.notification.request.content.userInfo[@"url"] ?: @""];
	NSString *act = r.actionIdentifier;
	if (![act isEqualToString:UNNotificationDefaultActionIdentifier] && ![act isEqualToString:UNNotificationDismissActionIdentifier]) {
		// A button: its identifier is the board's path that answers the agent. Only when that fails (already
		// answered, the agent gone) the task opens in the window.
		NSMutableURLRequest *req = [NSMutableURLRequest requestWithURL:[NSURL URLWithString:act relativeToURL:makoBoard]];
		req.HTTPMethod = @"POST";
		NSURL *task = u.host ? u : makoBoard;
		[[[NSURLSession sharedSession] dataTaskWithRequest:req completionHandler:^(NSData *data, NSURLResponse *res, NSError *e) {
			NSDictionary *j = data ? [NSJSONSerialization JSONObjectWithData:data options:0 error:nil] : nil;
			if (e != nil || ![j isKindOfClass:[NSDictionary class]] || ![j[@"ok"] boolValue]) {
				makoOpenWindow(task.absoluteString.UTF8String);
			}
		}] resume];
		done();
		return;
	}
	if (u.host == nil || ([u.host isEqualToString:makoBoard.host] && [u.port isEqual:makoBoard.port])) {
		makoOpenWindow((u.host ? u : makoBoard).absoluteString.UTF8String);
	} else {
		[[NSWorkspace sharedWorkspace] openURL:u];
	}
	done();
}
@end

// Only an app (a bundle) has notifications of its own: `go run` keeps osascript's.
static void makoNotificationsOn(const char *board) {
	if ([NSBundle mainBundle].bundleIdentifier == nil) {
		return;
	}
	makoBoard = [NSURL URLWithString:[NSString stringWithUTF8String:board]];
	dispatch_async(dispatch_get_main_queue(), ^{
		makoNotices = [MakoNotices new];
		UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
		center.delegate = makoNotices;
		[center requestAuthorizationWithOptions:UNAuthorizationOptionAlert | UNAuthorizationOptionSound
			completionHandler:^(BOOL granted, NSError *e) { makoAllowed = granted; }];
	});
}

// The buttons of each tag's notification: macOS knows actions only through categories, one per tag.
static NSMutableDictionary<NSString *, UNNotificationCategory *> *makoCategories;

// makoNotify shows a notification; tag ("" for none) replaces the one shown before with it, actions (a JSON
// array of {title, url, destructive}) are its buttons.
static int makoNotify(const char *title, const char *body, const char *url, const char *tag, const char *actions) {
	if (!makoAllowed) {
		return 0;
	}
	UNMutableNotificationContent *c = [UNMutableNotificationContent new];
	c.title = [NSString stringWithUTF8String:title];
	c.body = [NSString stringWithUTF8String:body];
	c.sound = [UNNotificationSound defaultSound];
	c.userInfo = @{@"url": [NSString stringWithUTF8String:url]};
	NSString *ident = [NSString stringWithUTF8String:tag];
	if (ident.length == 0) {
		ident = [NSUUID UUID].UUIDString;
	}
	NSArray *list = [NSJSONSerialization JSONObjectWithData:[[NSString stringWithUTF8String:actions] dataUsingEncoding:NSUTF8StringEncoding] options:0 error:nil];
	NSMutableArray<UNNotificationAction *> *buttons = [NSMutableArray new];
	if ([list isKindOfClass:[NSArray class]]) {
		for (NSDictionary *a in list) {
			if (![a isKindOfClass:[NSDictionary class]]) {
				continue;
			}
			[buttons addObject:[UNNotificationAction actionWithIdentifier:a[@"url"] title:a[@"title"]
				options:[a[@"destructive"] boolValue] ? UNNotificationActionOptionDestructive : UNNotificationActionOptionNone]];
		}
	}
	NSString *category = [@"mako." stringByAppendingString:ident];
	c.categoryIdentifier = category;
	UNNotificationRequest *req = [UNNotificationRequest requestWithIdentifier:ident content:c trigger:nil];
	dispatch_async(dispatch_get_main_queue(), ^{
		UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
		if (makoCategories == nil) {
			makoCategories = [NSMutableDictionary new];
		}
		if (buttons.count > 0) {
			makoCategories[category] = [UNNotificationCategory categoryWithIdentifier:category actions:buttons intentIdentifiers:@[] options:UNNotificationCategoryOptionNone];
		} else {
			[makoCategories removeObjectForKey:category];
		}
		[center setNotificationCategories:[NSSet setWithArray:makoCategories.allValues]];
		// The categories are set asynchronously: asking for them waits until they are, so the buttons show.
		[center getNotificationCategoriesWithCompletionHandler:^(NSSet<UNNotificationCategory *> *set) {
			[center addNotificationRequest:req withCompletionHandler:nil];
		}];
	});
	return 1;
}

// makoWithdraw takes the notification with the tag away, shown or not yet.
static void makoWithdraw(const char *tag) {
	if (!makoAllowed) { // not an app (no notification center) or not allowed: nothing was shown
		return;
	}
	NSString *ident = [NSString stringWithUTF8String:tag];
	UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
	[center removePendingNotificationRequestsWithIdentifiers:@[ident]];
	[center removeDeliveredNotificationsWithIdentifiers:@[ident]];
}
*/
import "C"

import (
	"encoding/json"
	"strings"
	"unsafe"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
)

// HasWindow: on macOS the board opens in Mako's own window (WebKit).
func HasWindow() bool { return true }

// OpenWindow shows url in Mako's window, making it if needed. It needs the menu bar's run loop (Run).
func OpenWindow(url string) {
	s := C.CString(url)
	defer C.free(unsafe.Pointer(s))
	C.makoOpenWindow(s)
}

// OnReopen shows url in the window when Mako is opened again while it runs. Call it once the menu bar is up:
// AppKit installs its own reopen handler while launching.
func OnReopen(url string) {
	s := C.CString(url)
	defer C.free(unsafe.Pointer(s))
	C.makoOnReopen(s)
}

// NotificationsOn lets Mako.app show its own notifications, clicked to the board at boardURL. macOS asks the user
// once. Call it once the menu bar is up.
func NotificationsOn(boardURL string) {
	s := C.CString(boardURL)
	defer C.free(unsafe.Pointer(s))
	C.makoNotificationsOn(s)
}

// Notify shows a notification of Mako.app, with its buttons; false when it has none (not an app, or not allowed).
func Notify(n notify.Notice) bool {
	actions, _ := json.Marshal(n.Actions)
	if n.Actions == nil {
		actions = []byte("[]")
	}
	t, m, u, g, a := C.CString(n.Title), C.CString(n.Message), C.CString(n.URL), C.CString(n.Tag), C.CString(string(actions))
	defer C.free(unsafe.Pointer(t))
	defer C.free(unsafe.Pointer(m))
	defer C.free(unsafe.Pointer(u))
	defer C.free(unsafe.Pointer(g))
	defer C.free(unsafe.Pointer(a))
	return C.makoNotify(t, m, u, g, a) == 1
}

// Relocalize gives the window's menu and dialogs the words of Mako's language now.
func Relocalize() {
	words := map[string]string{}
	for k, v := range i18n.Strings(i18n.EN) {
		if strings.HasPrefix(k, "menu.") || k == "tray.quit" {
			words[k] = v
		}
	}
	data, _ := json.Marshal(words)
	s := C.CString(string(data))
	defer C.free(unsafe.Pointer(s))
	C.makoSetWords(s)
}

// Withdraw takes Mako.app's notification with the tag away.
func Withdraw(tag string) {
	g := C.CString(tag)
	defer C.free(unsafe.Pointer(g))
	C.makoWithdraw(g)
}

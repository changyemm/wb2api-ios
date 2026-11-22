/* ui.m — 宿主 App 内的原生 UI：三指双击手势、悬浮球、WKWebView 面板、状态自检。
 *
 * 三方定稿（2026-11-22，Muse/Kimi/Claude Code 一致）：
 *  ① 宿主窗口解析：通知携带的 scene/window → foregroundActive → foregroundInactive
 *     → scene 内优先 isKeyWindow，否则 !hidden && alpha>0 && windowLevel==Normal
 *     → 自己持有的 lastHostWindow（校验仍有效）→ 全没有就返回 nil，等下一次通知。
 *  ② 三指双击：直接 addGestureRecognizer 到宿主 UIWindow（3 指、2 击，
 *     cancelsTouchesInView=NO、delaysTouchesBegan/Ended=NO，delegate 允许多手势并存）；
 *     重复执行先去重（先 remove 再 add）；窗口变化时从旧窗口移除、装到新窗口。
 *  ③ 悬浮球：自己持有的透明容器 view，挂到宿主 UIWindow；容器 hitTest 背景返回 nil
 *     （穿透），只有球本身接收触摸；球上 UIPan（拖拽）+ UITap（点击），
 *     tap.require(toFail: pan) 防拖拽误触发；拖拽结束吸附在可显示区域内。
 *  ④ 重装机制：UIScene.didActivate 与 UIWindow.didBecomeKey 两个通知都进同一个幂等
 *     ensureInstalled（手势去重、宿主窗口切换、悬浮球容器重新挂载）；所有 UI 操作主线程。
 *  ⑤ 服务状态自检：唤醒后自检，不常驻轮询。触发点：scene 激活、回到 active、
 *     打开面板前、请求失败后；动作：TCP 连 127.0.0.1:7863（2 秒超时）→ 球色绿/红；
 *     10 秒内不重复查（节流）。
 *
 * 面板（WKWebView）：加载 http://127.0.0.1:7863/panel/（该页与 theme.js/app.js 免鉴权，
 * API 调用才要密钥）。WKUserScript 在 documentStart 时把 api_key（WBAPIKey 读 Live 快照）
 * 注入 localStorage（wb2api.key），面板免输密钥；documentEnd 时注入移动端 CSS 与
 * 「悬浮球」开关按钮（经 WKScriptMessageHandler 回调原生侧）。
 * 注意：WKWebView 的 WebContent 是独立进程，连 127.0.0.1 未在真机实证；若加载失败，
 * 弹 toast 提示用户用 Safari 打开局域网 IP（见 didFailProvisionalNavigation）。
 *
 * 只在 iOS 上编译（CI：Apple clang + iphoneos SDK，-fobjc-arc，链 UIKit/WebKit）。
 * 本地 Linux 编不了（无 UIKit），不要在本机试编 ui.m。
 */
#import <UIKit/UIKit.h>
#import <WebKit/WebKit.h>
#import <arpa/inet.h>
#import <netinet/in.h>
#import <os/log.h>
#import <sys/socket.h>

#import "ui.h"

extern const char *WBAPIKey(void);
extern void WBFree(const char *s);

#define WB2API_PORT 7863
#define WB2API_BALL_SIZE 56.0
#define WB2API_BALL_RADIUS 28.0
#define WB2API_HEALTH_MIN_INTERVAL 10.0
#define WB2API_HEALTH_TIMEOUT_SEC 2

static NSString *const kBallVisibleKey = @"wb2api.ball.visible";
static NSString *const kBallXKey = @"wb2api.ball.x";
static NSString *const kBallYKey = @"wb2api.ball.y";

typedef NS_ENUM(NSInteger, WB2APIStatus) {
    WB2APIStatusUnknown = 0,
    WB2APIStatusRunning = 1,
    WB2APIStatusStopped = 2,
};

@class WB2APIUI;
@class WB2APIPanelViewController;

/* ── 悬浮球容器 ─────────────────────────────────────────────── */
/* 全窗口透明容器：背景触摸返回 nil（穿透，不挡宿主 App），只有球本身接收触摸。 */
@interface WB2APIBallContainerView : UIView
@property (nonatomic, strong) UIView *ball;
@property (nonatomic, strong) UILabel *ballLabel;
@end

@implementation WB2APIBallContainerView

- (UIView *)hitTest:(CGPoint)point withEvent:(UIEvent *)event {
    if (self.ball.hidden) return nil;
    CGPoint p = [self convertPoint:point toView:self.ball];
    if ([self.ball pointInside:p withEvent:event]) return self.ball;
    return nil; // 穿透
}

// 旋转/窗口尺寸变化时把球夹回可视区域内（容器 autoresizing 只改尺寸，不管球的位置）。
- (void)layoutSubviews {
    [super layoutSubviews];
    if (!self.ball) return;
    CGFloat w = self.bounds.size.width;
    CGFloat h = self.bounds.size.height;
    if (w <= 0 || h <= 0) return;
    CGPoint c = self.ball.center;
    c.x = MAX(WB2API_BALL_RADIUS, MIN(w - WB2API_BALL_RADIUS, c.x));
    c.y = MAX(WB2API_BALL_RADIUS, MIN(h - WB2API_BALL_RADIUS, c.y));
    self.ball.center = c;
}

@end

/* ── 面板 VC ────────────────────────────────────────────────── */
@interface WB2APIPanelViewController : UIViewController <WKNavigationDelegate>
@property (nonatomic, weak) WB2APIUI *ui;
@property (nonatomic, strong) WKWebView *webView;
@end

/* ── UI 单例 ────────────────────────────────────────────────── */
@interface WB2APIUI : NSObject <UIGestureRecognizerDelegate, WKScriptMessageHandler>
@property (nonatomic, weak) UIWindow *hostWindow;
@property (nonatomic, strong) UITapGestureRecognizer *tripleTap;
@property (nonatomic, strong) WB2APIBallContainerView *ballContainer;
@property (nonatomic, strong) UIPanGestureRecognizer *ballPan;
@property (nonatomic, strong) UITapGestureRecognizer *ballTap;
@property (nonatomic, strong) UILongPressGestureRecognizer *ballLongPress;
@property (nonatomic, assign) WB2APIStatus status;
@property (nonatomic, assign) BOOL ballVisible;
@property (nonatomic, copy) NSString *pendingToast;
@property (nonatomic, assign) NSTimeInterval lastHealthCheck;
@property (nonatomic, strong) WB2APIPanelViewController *panelVC;
+ (instancetype)shared;
- (void)installWithWBStartRC:(int)rc;
- (void)ensureInstalled;
- (void)ensureInstalledWithScene:(UIWindowScene *)scene window:(UIWindow *)window;
- (void)openPanel;
- (void)toggleBall;
- (void)setBallVisible:(BOOL)visible;
- (void)showToast:(NSString *)msg;
- (void)runHealthCheck;
- (void)updateBallColor;
@end

@implementation WB2APIUI

+ (instancetype)shared {
    static WB2APIUI *s;
    static dispatch_once_t once;
    dispatch_once(&once, ^{ s = [[WB2APIUI alloc] init]; });
    return s;
}

- (instancetype)init {
    if (self = [super init]) {
        _status = WB2APIStatusUnknown;
        NSUserDefaults *d = [NSUserDefaults standardUserDefaults];
        // 悬浮球开关默认开（没存过时也开）。
        if ([d objectForKey:kBallVisibleKey] == nil) {
            _ballVisible = YES;
        } else {
            _ballVisible = [d boolForKey:kBallVisibleKey];
        }
        // 三指双击手势（②）：3 指、2 击，不抢宿主触摸、不延迟。
        _tripleTap = [[UITapGestureRecognizer alloc] initWithTarget:self action:@selector(onTripleTap:)];
        _tripleTap.numberOfTouchesRequired = 3;
        _tripleTap.numberOfTapsRequired = 2;
        _tripleTap.cancelsTouchesInView = NO;
        _tripleTap.delaysTouchesBegan = NO;
        _tripleTap.delaysTouchesEnded = NO;
        _tripleTap.delegate = self;
        [self buildBall];
        // ④ 重装机制：两个通知都进同一个幂等 ensureInstalled。
        NSNotificationCenter *nc = [NSNotificationCenter defaultCenter];
        [nc addObserver:self selector:@selector(onSceneDidActivate:) name:UISceneDidActivateNotification object:nil];
        [nc addObserver:self selector:@selector(onWindowDidBecomeKey:) name:UIWindowDidBecomeKeyNotification object:nil];
    }
    return self;
}

/* ── 悬浮球构造 ── */
- (void)buildBall {
    WB2APIBallContainerView *c = [[WB2APIBallContainerView alloc] initWithFrame:CGRectZero];
    c.userInteractionEnabled = YES;
    c.autoresizingMask = UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
    c.backgroundColor = [UIColor clearColor];
    UIView *ball = [[UIView alloc] initWithFrame:CGRectMake(0, 0, WB2API_BALL_SIZE, WB2API_BALL_SIZE)];
    ball.layer.cornerRadius = WB2API_BALL_RADIUS;
    ball.layer.shadowColor = [UIColor blackColor].CGColor;
    ball.layer.shadowOpacity = 0.3;
    ball.layer.shadowOffset = CGSizeMake(0, 2);
    ball.layer.shadowRadius = 4;
    ball.backgroundColor = [UIColor colorWithWhite:0.6 alpha:0.95]; // 灰 = 未知
    UILabel *lb = [[UILabel alloc] initWithFrame:ball.bounds];
    lb.text = @"WB";
    lb.textColor = [UIColor whiteColor];
    lb.font = [UIFont boldSystemFontOfSize:16];
    lb.textAlignment = NSTextAlignmentCenter;
    lb.autoresizingMask = UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
    [ball addSubview:lb];
    [c addSubview:ball];
    c.ball = ball;
    c.ballLabel = lb;
    _ballContainer = c;
    // 球上：拖拽 + 点击 + 长按隐藏。tap.require(toFail: pan) 防拖拽误触发点击。
    _ballPan = [[UIPanGestureRecognizer alloc] initWithTarget:self action:@selector(onBallPan:)];
    _ballTap = [[UITapGestureRecognizer alloc] initWithTarget:self action:@selector(onBallTap:)];
    _ballLongPress = [[UILongPressGestureRecognizer alloc] initWithTarget:self action:@selector(onBallLongPress:)];
    _ballLongPress.minimumPressDuration = 0.8;
    [_ballTap requireGestureRecognizerToFail:_ballPan];
    [ball addGestureRecognizer:_ballPan];
    [ball addGestureRecognizer:_ballTap];
    [ball addGestureRecognizer:_ballLongPress];
}

/* ── 安装入口（tweak.c 构造器调）── */
- (void)installWithWBStartRC:(int)rc {
    switch (rc) {
        case 0:  self.status = WB2APIStatusRunning; self.pendingToast = @"反代已启动 · 端口 7863"; break;
        case 1:  self.status = WB2APIStatusRunning; self.pendingToast = @"反代已在运行"; break;
        case 2:  self.status = WB2APIStatusRunning; self.pendingToast = @"已有服务在跑（别的 App），本进程只当客户端"; break;
        default: self.status = WB2APIStatusStopped; self.pendingToast = @"反代启动失败，见 wb2api.log"; break;
    }
    [self updateBallColor];
    [self ensureInstalled];
}

- (void)onSceneDidActivate:(NSNotification *)n {
    UIWindowScene *scene = nil;
    if ([n.object isKindOfClass:[UIWindowScene class]]) scene = (UIWindowScene *)n.object;
    [self ensureInstalledWithScene:scene window:nil];
}

- (void)onWindowDidBecomeKey:(NSNotification *)n {
    UIWindow *w = nil;
    if ([n.object isKindOfClass:[UIWindow class]]) w = (UIWindow *)n.object;
    UIWindowScene *scene = w.windowScene;
    [self ensureInstalledWithScene:scene window:w];
}

/* ── ① 宿主窗口解析 ── */
+ (UIWindow *)keyOrNormalWindowInScene:(UIWindowScene *)scene {
    // 两轮都只接受 windowLevel == Normal 的窗口：键盘、alert 等系统窗口
    // （level 高于 Normal）可能成为 keyWindow，但不是宿主 App 的主窗口。
    for (UIWindow *w in scene.windows) {
        if (w.isKeyWindow && !w.hidden && w.alpha > 0 && w.windowLevel == UIWindowLevelNormal) return w;
    }
    for (UIWindow *w in scene.windows) {
        if (!w.hidden && w.alpha > 0 && w.windowLevel == UIWindowLevelNormal) return w;
    }
    return nil;
}

+ (UIWindow *)resolveHostWindowWithScene:(UIWindowScene *)scene window:(UIWindow *)window {
    // 1. 通知携带的 window：须属于前台 scene，且窗口本身有效（可见、不透明、Normal 层级）。
    //    只查 scene 状态不够——键盘、alert 等系统窗口也可能挂在前台 scene 下。
    if (window && window.windowScene &&
        window.windowScene.activationState == UISceneActivationStateForegroundActive &&
        !window.hidden && window.alpha > 0 &&
        window.windowLevel == UIWindowLevelNormal) {
        return window;
    }
    // 2. 通知携带的 scene：只接受前台（Active 或 Inactive）的 scene，
    //    后台 scene 的窗口不能用（用户看不见，挂上去也没意义）。
    if (scene && (scene.activationState == UISceneActivationStateForegroundActive ||
                  scene.activationState == UISceneActivationStateForegroundInactive)) {
        UIWindow *w = [self keyOrNormalWindowInScene:scene];
        if (w) return w;
    }
    // 3. foregroundActive → foregroundInactive，依次查找。
    NSArray *states = @[@(UISceneActivationStateForegroundActive),
                        @(UISceneActivationStateForegroundInactive)];
    for (NSNumber *st in states) {
        for (UIScene *s in [UIApplication sharedApplication].connectedScenes) {
            if (![s isKindOfClass:[UIWindowScene class]]) continue;
            UIWindowScene *ws = (UIWindowScene *)s;
            if (ws.activationState != [st integerValue]) continue;
            UIWindow *w = [self keyOrNormalWindowInScene:ws];
            if (w) return w;
        }
    }
    // 4. 自己持有的 lastHostWindow（校验仍有效：还在 connectedScenes 里、未隐藏）。
    WB2APIUI *ui = [WB2APIUI shared];
    UIWindow *last = ui.hostWindow;
    if (last && last.windowScene && !last.hidden) {
        for (UIScene *s in [UIApplication sharedApplication].connectedScenes) {
            if (s == last.windowScene) return last;
        }
    }
    // 5. 全没有 → nil，等下一次通知。不要挂到后台或不确定的 scene。
    return nil;
}

/* ── ④ 重装机制（幂等）── */
- (void)ensureInstalled {
    [self ensureInstalledWithScene:nil window:nil];
}

- (void)ensureInstalledWithScene:(UIWindowScene *)scene window:(UIWindow *)window {
    if (![NSThread isMainThread]) {
        dispatch_async(dispatch_get_main_queue(), ^{
            [self ensureInstalledWithScene:scene window:window];
        });
        return;
    }
    UIWindow *w = [[self class] resolveHostWindowWithScene:scene window:window];
    if (!w) return; // 没拿到，等下一次通知。
    if (w == self.hostWindow) {
        // 同一个窗口：去重即可（先 remove 再 add，保证只有一个手势实例）；
        // 球容器若被宿主清掉（极少见），也重新挂上。
        [w removeGestureRecognizer:self.tripleTap];
        [w addGestureRecognizer:self.tripleTap];
        if (self.ballVisible && self.ballContainer.superview != w) {
            self.ballContainer.frame = w.bounds;
            [w addSubview:self.ballContainer];
        }
        return;
    }
    // 窗口变了：从旧窗口移除，装到新窗口。
    if (self.hostWindow) {
        [self.hostWindow removeGestureRecognizer:self.tripleTap];
        [self.ballContainer removeFromSuperview];
    }
    self.hostWindow = w;
    [w addGestureRecognizer:self.tripleTap];
    self.ballContainer.frame = w.bounds;
    [self restoreBallPosition];
    if (self.ballVisible) [w addSubview:self.ballContainer];
    [self updateBallColor];
    // 启动 toast 只在第一次拿到窗口时显示一次。
    if (self.pendingToast) {
        NSString *t = self.pendingToast;
        self.pendingToast = nil;
        [self showToast:t];
    }
    // ⑤ 唤醒后自检（不轮询）。
    [self runHealthCheck];
}

- (void)restoreBallPosition {
    NSUserDefaults *d = [NSUserDefaults standardUserDefaults];
    CGFloat w = self.ballContainer.bounds.size.width;
    CGFloat h = self.ballContainer.bounds.size.height;
    if (w <= 0 || h <= 0) return;
    double nx = 1.0, ny = 0.6; // 默认：右边缘、六成高。
    if ([d objectForKey:kBallXKey] != nil) nx = [d doubleForKey:kBallXKey];
    if ([d objectForKey:kBallYKey] != nil) ny = [d doubleForKey:kBallYKey];
    nx = MAX(0.0, MIN(1.0, nx));
    ny = MAX(0.0, MIN(1.0, ny));
    CGFloat x = nx * w;
    CGFloat y = ny * h;
    x = MAX(WB2API_BALL_RADIUS, MIN(w - WB2API_BALL_RADIUS, x));
    y = MAX(WB2API_BALL_RADIUS, MIN(h - WB2API_BALL_RADIUS, y));
    self.ballContainer.ball.center = CGPointMake(x, y);
}

/* ── ② 三指双击 ── */
- (void)onTripleTap:(UITapGestureRecognizer *)gr {
    if (gr.state != UIGestureRecognizerStateRecognized) return;
    [self openPanel];
}

- (BOOL)gestureRecognizer:(UIGestureRecognizer *)gr shouldRecognizeSimultaneouslyWithGestureRecognizer:(UIGestureRecognizer *)other {
    return YES; // 允许多手势并存，不抢宿主 App 的手势。
}

- (BOOL)gestureRecognizerShouldBegin:(UIGestureRecognizer *)gr {
    if (gr != self.tripleTap || !self.hostWindow) return YES;
    // hitTest 过滤：落在 UIControl/UITextField/UITextView 上不触发
    // （避免在输入框、按钮上三指双击误开面板）。
    // 注意：不要过滤 WKWebView——Minis 主界面可能就是全屏 WKWebView
    // （src/ios/WebApp），一刀切会把三指双击这个主入口直接堵死；
    // 三指双击不是 WKWebView 的标准手势（网页双击缩放是单指），不会误触。
    CGPoint p = [gr locationInView:self.hostWindow];
    UIView *hit = [self.hostWindow hitTest:p withEvent:nil];
    for (UIView *v = hit; v; v = v.superview) {
        if ([v isKindOfClass:[UIControl class]] ||
            [v isKindOfClass:[UITextField class]] ||
            [v isKindOfClass:[UITextView class]]) {
            return NO;
        }
    }
    return YES;
}

/* ── ③ 悬浮球：拖拽 / 点击 / 长按隐藏 ── */
- (void)onBallPan:(UIPanGestureRecognizer *)pan {
    UIView *ball = self.ballContainer.ball;
    if (pan.state == UIGestureRecognizerStateChanged) {
        CGPoint t = [pan translationInView:self.ballContainer];
        CGPoint c = ball.center;
        c.x += t.x;
        c.y += t.y;
        CGFloat w = self.ballContainer.bounds.size.width;
        CGFloat h = self.ballContainer.bounds.size.height;
        c.x = MAX(WB2API_BALL_RADIUS, MIN(w - WB2API_BALL_RADIUS, c.x));
        c.y = MAX(WB2API_BALL_RADIUS, MIN(h - WB2API_BALL_RADIUS, c.y));
        ball.center = c;
        [pan setTranslation:CGPointZero inView:self.ballContainer];
    } else if (pan.state == UIGestureRecognizerStateEnded ||
               pan.state == UIGestureRecognizerStateCancelled) {
        // 吸附到左右边缘（在可显示区域内）。
        CGFloat w = self.ballContainer.bounds.size.width;
        CGFloat h = self.ballContainer.bounds.size.height;
        CGFloat x = (ball.center.x < w / 2) ? WB2API_BALL_RADIUS : w - WB2API_BALL_RADIUS;
        CGFloat y = MAX(WB2API_BALL_RADIUS, MIN(h - WB2API_BALL_RADIUS, ball.center.y));
        [UIView animateWithDuration:0.2 animations:^{
            ball.center = CGPointMake(x, y);
        }];
        // 存归一化位置，下次启动/换窗口恢复。
        NSUserDefaults *d = [NSUserDefaults standardUserDefaults];
        [d setDouble:(x / w) forKey:kBallXKey];
        [d setDouble:(y / h) forKey:kBallYKey];
    }
}

- (void)onBallTap:(UITapGestureRecognizer *)tap {
    if (tap.state != UIGestureRecognizerStateRecognized) return;
    [self openPanel];
}

- (void)onBallLongPress:(UILongPressGestureRecognizer *)lp {
    if (lp.state != UIGestureRecognizerStateBegan) return;
    [self setBallVisible:NO];
    [self showToast:@"悬浮球已隐藏，三指双击打开面板可重新显示"];
}

- (void)toggleBall {
    [self setBallVisible:!self.ballVisible];
}

- (void)setBallVisible:(BOOL)visible {
    self.ballVisible = visible;
    [[NSUserDefaults standardUserDefaults] setBool:visible forKey:kBallVisibleKey];
    if (visible) {
        if (self.hostWindow && self.ballContainer.superview != self.hostWindow) {
            self.ballContainer.frame = self.hostWindow.bounds;
            [self.hostWindow addSubview:self.ballContainer];
        }
    } else {
        [self.ballContainer removeFromSuperview];
    }
    // 面板里的开关按钮同步文字（面板开着才需要）。
    if (self.panelVC) {
        NSString *js = [NSString stringWithFormat:
            @"var b=document.getElementById('wbBallBtn');"
            @"if(b){b.textContent='%@';}",
            visible ? @"● 悬浮球：开" : @"○ 悬浮球：关"];
        [self.panelVC.webView evaluateJavaScript:js completionHandler:nil];
    }
}

/* ── ⑤ 服务状态自检 ── */
- (void)runHealthCheck {
    NSTimeInterval now = [NSDate timeIntervalSinceReferenceDate];
    if (now - self.lastHealthCheck < WB2API_HEALTH_MIN_INTERVAL) return; // 节流
    self.lastHealthCheck = now;
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
        int sock = socket(AF_INET, SOCK_STREAM, 0);
        BOOL up = NO;
        if (sock >= 0) {
            struct timeval tv;
            tv.tv_sec = WB2API_HEALTH_TIMEOUT_SEC;
            tv.tv_usec = 0;
            setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
            setsockopt(sock, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
            struct sockaddr_in addr;
            memset(&addr, 0, sizeof(addr));
            addr.sin_family = AF_INET;
            addr.sin_port = htons(WB2API_PORT);
            addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
            up = (connect(sock, (struct sockaddr *)&addr, sizeof(addr)) == 0);
            close(sock);
        }
        WB2APIStatus st = up ? WB2APIStatusRunning : WB2APIStatusStopped;
        dispatch_async(dispatch_get_main_queue(), ^{
            [self updateStatus:st];
        });
    });
}

- (void)updateStatus:(WB2APIStatus)st {
    if (st == self.status) return;
    self.status = st;
    [self updateBallColor];
}

- (void)updateBallColor {
    UIColor *c;
    switch (self.status) {
        case WB2APIStatusRunning: c = [UIColor colorWithRed:0.20 green:0.78 blue:0.40 alpha:0.95]; break;
        case WB2APIStatusStopped: c = [UIColor colorWithRed:0.95 green:0.30 blue:0.25 alpha:0.95]; break;
        default: c = [UIColor colorWithWhite:0.60 alpha:0.95]; break;
    }
    self.ballContainer.ball.backgroundColor = c;
}

/* ── Toast ── */
- (void)showToast:(NSString *)msg {
    if (![NSThread isMainThread]) {
        dispatch_async(dispatch_get_main_queue(), ^{ [self showToast:msg]; });
        return;
    }
    UIWindow *w = self.hostWindow;
    if (!w) { self.pendingToast = msg; return; } // 窗口还没好，先存着。
    UILabel *label = [[UILabel alloc] init];
    label.text = msg;
    label.backgroundColor = [UIColor colorWithWhite:0 alpha:0.75];
    label.textColor = [UIColor whiteColor];
    label.font = [UIFont systemFontOfSize:14];
    label.textAlignment = NSTextAlignmentCenter;
    label.numberOfLines = 0;
    label.layer.cornerRadius = 8;
    label.clipsToBounds = YES;
    CGFloat width = MIN(w.bounds.size.width - 40, 340);
    CGSize size = [label sizeThatFits:CGSizeMake(width - 24, CGFLOAT_MAX)];
    label.frame = CGRectMake((w.bounds.size.width - width) / 2,
                             w.bounds.size.height - 120,
                             width, size.height + 16);
    label.autoresizingMask = UIViewAutoresizingFlexibleTopMargin |
        UIViewAutoresizingFlexibleLeftMargin | UIViewAutoresizingFlexibleRightMargin;
    [w addSubview:label];
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(3 * NSEC_PER_SEC)),
                   dispatch_get_main_queue(), ^{
        [UIView animateWithDuration:0.3 animations:^{
            label.alpha = 0;
        } completion:^(BOOL f) {
            [label removeFromSuperview];
        }];
    });
}

/* ── 面板 ── */
- (void)openPanel {
    if (![NSThread isMainThread]) {
        dispatch_async(dispatch_get_main_queue(), ^{ [self openPanel]; });
        return;
    }
    // 入口兜底（Bug 3 根治）：panelVC 非 nil 不等于面板真的开着。
    // present 被拒时 completion 通常根本不会调用，所以不能只靠它清理。
    // 若 panelVC 既没在呈现（presentingViewController 为 nil）也没在
    // 转场中（isBeingPresented 为 NO），说明上次 present 未成功，
    // 清掉陈旧引用，允许这次重试；反之面板确实开着/正在开，直接 return。
    if (self.panelVC) {
        if (self.panelVC.presentingViewController || self.panelVC.isBeingPresented) return;
        self.panelVC = nil;
    }
    // 注意：不要用上次异步自检的 Stopped 状态拦开面板——状态可能是旧的
    // （服务后来已启动），而 return 在 runHealthCheck 之前，会形成
    // 「Stopped → 不开面板 → 不自检 → 永远 Stopped」的死锁。
    // 面板照常打开：服务真挂了，WKWebView 加载失败会弹 toast 提示。
    UIViewController *root = self.hostWindow.rootViewController;
    if (!root) {
        [self showToast:@"宿主界面还没好，稍后再试"];
        return;
    }
    while (root.presentedViewController) root = root.presentedViewController;
    WB2APIPanelViewController *vc = [[WB2APIPanelViewController alloc] init];
    vc.ui = self;
    vc.modalPresentationStyle = UIModalPresentationFullScreen; // 全屏，不可下滑关闭（关闭只走按钮）。
    self.panelVC = vc;
    [root presentViewController:vc animated:YES completion:^{
        // present 可能被拒（顶层 VC 正转场、不在窗口层级等）。若失败，
        // presentingViewController 为 nil，必须清空 panelVC，否则后续
        // openPanel 都在第一行 return，面板永久锁成「已打开」。
        if (vc.presentingViewController == nil) {
            if (self.panelVC == vc) self.panelVC = nil;
        }
    }];
    // 打开面板也是一次自检触发点（后台跑，不阻塞打开）。
    [self runHealthCheck];
}

/* ── WKScriptMessageHandler（面板里「悬浮球」开关按钮回调）── */
- (void)userContentController:(WKUserContentController *)userContentController
      didReceiveScriptMessage:(WKScriptMessage *)message {
    if (![message.name isEqualToString:@"wb2api"]) return;
    if (![message.body isKindOfClass:[NSDictionary class]]) return;
    NSString *action = message.body[@"action"];
    if ([action isEqualToString:@"toggleBall"]) {
        [self toggleBall];
    }
}

@end

/* ── 面板 VC 实现 ───────────────────────────────────────────── */
@implementation WB2APIPanelViewController

- (void)viewDidLoad {
    [super viewDidLoad];
    self.view.backgroundColor = [UIColor systemBackgroundColor];
    // 顶部关闭条。
    UIButton *close = [UIButton buttonWithType:UIButtonTypeSystem];
    [close setTitle:@"✕ 关闭面板" forState:UIControlStateNormal];
    close.titleLabel.font = [UIFont systemFontOfSize:16];
    [close addTarget:self action:@selector(onClose) forControlEvents:UIControlEventTouchUpInside];
    close.translatesAutoresizingMaskIntoConstraints = NO;
    [self.view addSubview:close];
    [NSLayoutConstraint activateConstraints:@[
        [close.topAnchor constraintEqualToAnchor:self.view.safeAreaLayoutGuide.topAnchor constant:6],
        [close.trailingAnchor constraintEqualToAnchor:self.view.safeAreaLayoutGuide.trailingAnchor constant:-12],
        [close.heightAnchor constraintEqualToConstant:32],
    ]];
    // WKWebView：api_key 在 documentStart 注入 localStorage，面板免输密钥。
    WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
    WKUserContentController *ucc = [[WKUserContentController alloc] init];
    [ucc addScriptMessageHandler:[WB2APIUI shared] name:@"wb2api"];
    NSString *key = @"";
    const char *ck = WBAPIKey();
    if (ck) { key = [NSString stringWithUTF8String:ck]; WBFree(ck); }
    NSString *jsKey = [[key stringByReplacingOccurrencesOfString:@"\\" withString:@"\\\\"]
        stringByReplacingOccurrencesOfString:@"'" withString:@"\\'"];
    NSString *script =
        @"(function(){"
        @"  try{localStorage.setItem('wb2api.key','__KEY__');}catch(e){}"
        @"  function inject(){"
        @"    var st=document.createElement('style');"
        @"    st.textContent='@media (max-width:600px){"
        @"      .main{padding-left:10px!important;padding-right:10px!important;}"
        @"      .stats{grid-template-columns:repeat(2,1fr)!important;}"
        @"      table{display:block;overflow-x:auto;max-width:100%;}"
        @"      .box{overflow-x:auto;}"
        @"    }';"
        @"    document.head.appendChild(st);"
        @"    var nav=document.getElementById('btnNav');"
        @"    if(nav&&nav.parentNode&&!document.getElementById('wbBallBtn')){"
        @"      var b=document.createElement('button');"
        @"      b.id='wbBallBtn';b.className='icon ghost';b.title='悬浮球开关';"
        @"      b.textContent='● 悬浮球：开';"
        @"      b.onclick=function(){window.webkit.messageHandlers.wb2api.postMessage({action:'toggleBall'});};"
        @"      nav.parentNode.insertBefore(b,nav.nextSibling);"
        @"    }"
        @"    try{window.__wb2apiOverflow=document.documentElement.scrollWidth>window.innerWidth;}catch(e){}"
        @"  }"
        @"  if(document.readyState==='loading'){document.addEventListener('DOMContentLoaded',inject);}"
        @"  else{inject();}"
        @"})();";
    script = [script stringByReplacingOccurrencesOfString:@"__KEY__" withString:jsKey];
    WKUserScript *us = [[WKUserScript alloc] initWithSource:script
                                              injectionTime:WKUserScriptInjectionTimeAtDocumentStart
                                           forMainFrameOnly:YES];
    [ucc addUserScript:us];
    cfg.userContentController = ucc;
    WKWebView *wv = [[WKWebView alloc] initWithFrame:CGRectZero configuration:cfg];
    wv.navigationDelegate = self;
    wv.translatesAutoresizingMaskIntoConstraints = NO;
    [self.view addSubview:wv];
    [NSLayoutConstraint activateConstraints:@[
        [wv.topAnchor constraintEqualToAnchor:close.bottomAnchor constant:4],
        [wv.leadingAnchor constraintEqualToAnchor:self.view.leadingAnchor],
        [wv.trailingAnchor constraintEqualToAnchor:self.view.trailingAnchor],
        [wv.bottomAnchor constraintEqualToAnchor:self.view.bottomAnchor],
    ]];
    self.webView = wv;
    NSURL *url = [NSURL URLWithString:@"http://127.0.0.1:7863/panel/"];
    [wv loadRequest:[NSURLRequest requestWithURL:url]];
}

- (void)onClose {
    [self dismissViewControllerAnimated:YES completion:^{
        WB2APIUI *ui = self.ui;
        if (ui && ui.panelVC == self) ui.panelVC = nil;
    }];
}

- (void)webView:(WKWebView *)webView didFinishNavigation:(WKNavigation *)navigation {
    // 验无溢出：scrollWidth 应 <= innerWidth，否则移动端 CSS 没兜住，记日志备查。
    [webView evaluateJavaScript:@"window.__wb2apiOverflow===true"
              completionHandler:^(id r, NSError *e) {
        if ([r boolValue]) {
            os_log(OS_LOG_DEFAULT, "[wb2api] 面板横向溢出（scrollWidth>innerWidth），移动端 CSS 需再调");
        }
    }];
}

- (void)webView:(WKWebView *)webView didFailProvisionalNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    // 过滤 NSURLErrorCancelled (-999)：HTTP 4xx 处主动 Cancel 会连带触发
    // 这个回调；-999 只是“本次导航被取消”，不是真正的加载失败，不弹 toast，
    // 否则 HTTP 404 会连弹两次 toast，第二条还是错的（说“连不上”）。
    if ([error.domain isEqualToString:NSURLErrorDomain] &&
        error.code == NSURLErrorCancelled) {
        return;
    }
    // WKWebView 连 127.0.0.1 未在真机实证；若加载失败，提示用户用 Safari 打开局域网 IP。
    // 注意：toast 必须弹在面板自己的 view 上（见下方 showToast），不能走
    // WB2APIUI 的 hostWindow toast——全屏面板会把 hostWindow 上的 toast 盖住，
    // 用户只看到白屏，什么提示都没有。
    os_log(OS_LOG_DEFAULT, "[wb2api] 面板加载失败: %{public}@", error.localizedDescription);
    [self showToast:@"面板打不开（WKWebView 连不上 127.0.0.1），试试 Safari 打开手机局域网 IP:7863/panel/"];
}

- (void)webView:(WKWebView *)webView didFailNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    // 同上：过滤 -999（主动 Cancel / 被新导航取代的连带回调）。
    if ([error.domain isEqualToString:NSURLErrorDomain] &&
        error.code == NSURLErrorCancelled) {
        return;
    }
    // 导航中途失败（provisional 已过、加载中途断掉），与上面同等处理。
    os_log(OS_LOG_DEFAULT, "[wb2api] 面板导航失败: %{public}@", error.localizedDescription);
    [self showToast:@"面板加载中途失败，试试 Safari 打开手机局域网 IP:7863/panel/"];
}

- (void)webView:(WKWebView *)webView decidePolicyForNavigationResponse:(WKNavigationResponse *)navigationResponse decisionHandler:(void (^)(WKNavigationResponsePolicy))decisionHandler {
    // HTTP 4xx/5xx 不会触发上面两个失败回调（WKWebView 认为导航“成功”，
    // 只是加载了一个错误页）。这里拦下来：状态码 >= 400 就弹 toast 并取消加载，
    // 避免用户对着服务器的错误页发愣。
    NSHTTPURLResponse *resp = nil;
    if ([navigationResponse.response isKindOfClass:[NSHTTPURLResponse class]]) {
        resp = (NSHTTPURLResponse *)navigationResponse.response;
    }
    if (resp && resp.statusCode >= 400) {
        os_log(OS_LOG_DEFAULT, "[wb2api] 面板 HTTP 错误: %{public}ld", (long)resp.statusCode);
        [self showToast:[NSString stringWithFormat:@"面板加载失败（HTTP %ld），请检查反代服务", (long)resp.statusCode]];
        decisionHandler(WKNavigationResponsePolicyCancel);
        return;
    }
    decisionHandler(WKNavigationResponsePolicyAllow);
}

// 面板内 toast：弹在面板自己的 view 上（全屏面板盖住 hostWindow，
// 走 WB2APIUI 的 showToast 用户看不到）。样式与 hostWindow toast 一致，
// 居中显示（盖在空白 webView 之上，不挡顶部的关闭按钮）。
- (void)showToast:(NSString *)msg {
    if (![NSThread isMainThread]) {
        dispatch_async(dispatch_get_main_queue(), ^{ [self showToast:msg]; });
        return;
    }
    UILabel *label = [[UILabel alloc] init];
    label.text = msg;
    label.backgroundColor = [UIColor colorWithWhite:0 alpha:0.75];
    label.textColor = [UIColor whiteColor];
    label.font = [UIFont systemFontOfSize:14];
    label.textAlignment = NSTextAlignmentCenter;
    label.numberOfLines = 0;
    label.layer.cornerRadius = 8;
    label.clipsToBounds = YES;
    CGFloat width = MIN(self.view.bounds.size.width - 40, 340);
    CGSize size = [label sizeThatFits:CGSizeMake(width - 24, CGFLOAT_MAX)];
    label.frame = CGRectMake((self.view.bounds.size.width - width) / 2,
                             (self.view.bounds.size.height - (size.height + 16)) / 2,
                             width, size.height + 16);
    label.autoresizingMask = UIViewAutoresizingFlexibleTopMargin | UIViewAutoresizingFlexibleBottomMargin |
        UIViewAutoresizingFlexibleLeftMargin | UIViewAutoresizingFlexibleRightMargin;
    [self.view addSubview:label];
    // 把 toast 提到最上层（盖在 webView 之上），但不挡关闭按钮（toast 居中，关闭在顶部）。
    [self.view bringSubviewToFront:label];
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(3 * NSEC_PER_SEC)),
                   dispatch_get_main_queue(), ^{
        [UIView animateWithDuration:0.3 animations:^{
            label.alpha = 0;
        } completion:^(BOOL f) {
            [label removeFromSuperview];
        }];
    });
}

- (void)dealloc {
    [self.webView.configuration.userContentController removeScriptMessageHandlerForName:@"wb2api"];
}

@end

/* ── C 入口（tweak.c 构造器调）── */
void wb2api_ui_install(int wbStartRC) {
    // 构造器可能不在主线程，切到主线程再装 UI。
    dispatch_async(dispatch_get_main_queue(), ^{
        [[WB2APIUI shared] installWithWBStartRC:wbStartRC];
    });
}

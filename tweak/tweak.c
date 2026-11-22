/* tweak.c — 寄生式反代的宿主侧薄壳。
 *
 * 不用 theos/Logos：白膜注入器注入的是普通 dylib，%ctor 等价于
 * __attribute__((constructor))。不用 UIKit/Foundation 头文件，
 * 这样整个 dylib 能在 ubuntu CI 上用 zig cc 交叉编译（zig 自带
 * objc/runtime.h 与 libSystem.tbd，不自带 UIKit）。
 *
 * 行为：
 *   加载时：把 stderr 重定向到 <容器>/Library/WBPanel/wb2api.log
 *           （宿主里 Go 的 log 否则无处可看），调 WBStart()。
 *   卸载时：调 WBStop()（%dtor 等价物；实际 iOS 上进程被杀时
 *           destructor 未必跑到，网关随进程死而死，符合"同生共死"）。
 *
 * 数据目录：$HOME/Library/WBPanel（iOS 上 HOME 即 App 容器）。
 * 可用环境变量 WB2API_BASE_DIR 覆盖（调试用）。
 */
#include <errno.h>
#include <os/log.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include "ui.h"

extern int WBStart(const char *baseDir);
extern int WBStop(void);
extern const char *WBVersion(void);
extern const char *WBJSON(void);
extern void WBFree(const char *s);

static char g_base[1024];

__attribute__((constructor))
static void wb_ctor(void) {
    const char *home = getenv("HOME");
    const char *override = getenv("WB2API_BASE_DIR");
    /* 诊断用系统日志：即使文件日志建不出来，也能用 idevicesyslog 看到。
       %{public} 必不可少，否则动态内容会被统一日志涂黑成 <private>。 */
    os_log(OS_LOG_DEFAULT, "[wb2api] ctor start, HOME=%{public}s", home ? home : "(null)");
    if (override && override[0]) {
        snprintf(g_base, sizeof(g_base), "%s", override);
    } else if (home && home[0]) {
        snprintf(g_base, sizeof(g_base), "%s/Library/WBPanel", home);
    } else {
        snprintf(g_base, sizeof(g_base), "/tmp/wb2api");
    }
    os_log(OS_LOG_DEFAULT, "[wb2api] baseDir=%{public}s", g_base);

    /* Go 的 log 走 stderr：重定向到文件，手机上才能排障。 */
    char logpath[1200];
    snprintf(logpath, sizeof(logpath), "%s/wb2api.log", g_base);
    /* 先确保目录存在（WBStart 里也会建，这里为了日志文件先行）。
       不用 system()：它在 iOS 头文件里被标为不可用。 */
    int mk = mkdir(g_base, 0755);
    os_log(OS_LOG_DEFAULT, "[wb2api] mkdir=%{public}d errno=%{public}d (EEXIST=17 表示目录已在)", mk, errno);
    /* #5 日志轮转：文件超 1MB 就转成 .old，避免无限增长。
       注意：NSLog 也写 stderr，所以 Minis 全进程的日志都进这个文件。 */
    struct stat st;
    if (stat(logpath, &st) == 0 && st.st_size > (1 << 20)) {
        char oldpath[1250];
        snprintf(oldpath, sizeof(oldpath), "%s.old", logpath);
        rename(logpath, oldpath);
    }
    FILE *lf = freopen(logpath, "a", stderr);
    os_log(OS_LOG_DEFAULT, "[wb2api] freopen=%{public}s logpath=%{public}s", lf ? "ok" : "FAIL", logpath);

    int rc = WBStart(g_base);
    os_log(OS_LOG_DEFAULT, "[wb2api] WBStart=%{public}d (0=本次启动 1=已启动 2=已有服务 -2=失败)", rc);
    fprintf(stderr, "[wb2api-tweak] WBStart(%s) = %d (%s)\n",
            g_base, rc,
            rc == 0 ? "本次启动" : rc == 1 ? "已启动" : rc == 2 ? "已有服务，只当客户端" : "启动失败");
    /* 原生 UI（v1.1）：三指双击手势、悬浮球、WKWebView 面板、状态自检。
       构造器可能不在主线程，ui 内部会切到主线程；此时窗口可能还没建好，
       install 只注册通知、记下 toast，等窗口出现再挂载。 */
    wb2api_ui_install(rc);
}

__attribute__((destructor))
static void wb_dtor(void) {
    WBStop();
}

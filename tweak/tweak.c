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
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

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
    if (override && override[0]) {
        snprintf(g_base, sizeof(g_base), "%s", override);
    } else if (home && home[0]) {
        snprintf(g_base, sizeof(g_base), "%s/Library/WBPanel", home);
    } else {
        snprintf(g_base, sizeof(g_base), "/tmp/wb2api");
    }

    /* Go 的 log 走 stderr：重定向到文件，手机上才能排障。 */
    char logpath[1200];
    snprintf(logpath, sizeof(logpath), "%s/wb2api.log", g_base);
    /* 先确保目录存在（WBStart 里也会建，这里为了日志文件先行）。
       不用 system()：它在 iOS 头文件里被标为不可用。 */
    (void)mkdir(g_base, 0755);
    FILE *lf = freopen(logpath, "a", stderr);
    (void)lf;

    int rc = WBStart(g_base);
    fprintf(stderr, "[wb2api-tweak] WBStart(%s) = %d (%s)\n",
            g_base, rc,
            rc == 0 ? "本次启动" : rc == 1 ? "已启动" : rc == 2 ? "已有服务，只当客户端" : "启动失败");
}

__attribute__((destructor))
static void wb_dtor(void) {
    WBStop();
}

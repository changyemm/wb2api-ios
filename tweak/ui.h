/* ui.h — 宿主 App 内原生 UI 的 C 入口（tweak.c 调用）。
 *
 * 纯 C 头文件（不 import UIKit），tweak.c 能直接 include。
 * 实现在 ui.m（ObjC，-fobjc-arc，链 UIKit/WebKit，只在 CI 的 Apple clang 下编）。
 */
#ifndef WB2API_UI_H
#define WB2API_UI_H

/* wb2api_ui_install 在宿主 App 里装手势与悬浮球（切到主线程执行）。
 * wbStartRC 是 WBStart 的返回值：0/1/2 = 服务在跑（球绿），负数 = 失败（球红），
 * 其它 = 未知（球灰）。构造器里调，此时窗口可能还没建好——install 只注册通知、
 * 记下 toast，等窗口出现（ensureInstalled）再真正挂载。 */
void wb2api_ui_install(int wbStartRC);

#endif

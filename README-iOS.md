# wb2api-ios — 寄生式 WorkBuddy 反代（iOS dylib）

**一句话：** 一个 `.dylib`，用白膜注入器注入任意 App，App 启动就跑起
WorkBuddy 反代网关（账号池 + OpenAI/Anthropic 兼容 API + 正统 Web 面板），
App 退出网关就停。局域网设备也能连 `http://<手机IP>:7863/panel/`。

**产品定位：寄生式反代。** 它不是一个服务，它是宿主 App 的一个器官。
反代的唯一价值是给 Agent 提供算力；Agent 不在了，反代的存在就是纯粹的负担。
所以：宁可反代跟着 Agent 一起死，也不要让反代在没人用的时候还活着。
不要 launchd daemon、不要 posix_spawn 独立进程、不要常驻服务。

上游面板文档见 [README.md](README.md)（上游原样保留）。

## 架构

```
宿主 App（任意，如 OpenMinis）
  └─ wb2api.dylib（白膜注入器注入，~15 MB）
       ├─ tweak/tweak.c：__attribute__((constructor)) → WBStart($HOME/Library/WBPanel)
       └─ libwb2api.a：Go c-archive（cmd/carchive 精简入口）
            ├─ pool / upstream / auth / scheduler / panel / server（上游 internal/*，原样用）
            ├─ 监听 0.0.0.0:7863，/v1/chat/completions、/v1/messages（Anthropic 原生）、/panel/
            └─ 数据：<baseDir>/auths/（账号凭证）、<baseDir>/data/state.json
```

上游：[misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel)
（MIT，2026-11-21 commit e2d0eb6a，go 1.26.10）。本仓库是它的 fork +
`cmd/carchive`、`tweak/`、`scripts/`、`.github/`；上游代码本身一行未改，
升级时只换上游文件。

## 导出层决策（选 B：精简入口，不改上游）

`cmd/server/main.go` 是 package main，不能 import。但它装配时用到的
`internal/server.Config`、`internal/panel.Config`、`scheduler.Config`、
`pool.New`、`upstream.New`、`auth.LoadDir`、`redisstore.New` 全部是导出、
可直接 import 的包；真正必需的核心装配约 200 行就能在 `cmd/carchive`
里重写一遍。`main()` 其余 ~900 行是 CLI 层（flag 解析、自更新、config
文件读写、livecfg 热更新、审计日志、pprof），插件里不需要。

- **A（抽 internal/runner）否决**：要搬走 1561 行 config.go 的 Config
  类型 + 1151 行 main()，等于把上游 CLI 层整个重构进 internal，跟上游
  频繁更新直接冲突。
- **C1（cmd/server 里加文件调 main()）否决**：`flag.Parse()` 会解析宿主
  App 的 argv（未知 flag → `os.Exit(2)` 直接杀宿主）；
  `updater.HandleSelfUpdate(os.Args)` 会尝试自更新（dylib 里极危险）；
  `main()` 没有返回路径，`WBStop` 无法实现；且往上游包里加文件违背
  "不要改上游"的精神。
- **C2（go:linkname）否决**：脆弱，随编译器版本失效，不可维护。
- **C3（CI 打补丁，gan-jibamao 式）备用**：如果 B 某天撞上未导出符号，
  再用补丁路线兜底，目前不需要。

## iOS SDK：不需要（已实测）

c-archive 编译分两步，Apple SDK 在哪一步都不是必需的：

1. **Go 部分（ubuntu CI 可做）**：`GOOS=darwin GOARCH=arm64 CGO_ENABLED=1`，
   C 编译器用 `zig cc -target aarch64-macos-gnu`，头文件用 zig 自带的
   Darwin 头文件（`zig/lib/libc/include/any-macos-any`，含 stdlib/pthread/
   mach/objc），再加 `-Wno-nullability-completeness`（Go 的 cgo 会把
   zig 的 nullability 警告当错误）。**2026-11-21 本机实测通过**，
   产出 `.a` + `.h`。直接 `GOOS=ios` 不行：runtime/cgo 的
   `gcc_darwin_arm64.c` 要 `<CoreFoundation/CFBundle.h>`，zig 没有。
2. **平台标记**：darwin 编出的 `.a` 每个成员都是 `LC_BUILD_VERSION
   platform=1`。`scripts/patch_archive_platform.py` 解包、逐成员改成
   platform=2、minos=15.0、重新打包（raccoon 的 patch_platform.py 只处理
   单个 Mach-O，对 `.a` 报 0 个成员，不能直接用）。
3. **tweak 链接**：`zig cc -target aarch64-ios-gnu -dynamiclib`，
   zig 自带 `libSystem.tbd`，tweak.c 只用 libc + objc/runtime，
   不用 UIKit/Foundation 头文件，所以也不需要 SDK。
   WKWebView 面板入口需要 UIKit 头文件，v1 不做（面板用手机浏览器开
   `http://<手机IP>:7863/panel/`），v1.1 再接。

## 导出函数

| 函数 | 说明 |
|---|---|
| `WBStart(const char *baseDir)` | 启动。返回 0=本次启动；1=已启动；**2=7863 已有服务，只当客户端**（多 App 单例锁）；负数=失败（见 WBJSON 的 last_error） |
| `WBStop()` | 停止（宿主退出时调）。0=已停，1=本来就没跑 |
| `WBVersion()` | 版本串，WBFree 释放 |
| `WBJSON()` | 状态 JSON（running/listen/version/accounts/healthy/api_key_set/last_error），WBFree 释放 |
| `WBFree(char *)` | 释放 WBVersion/WBJSON 返回的字符串 |

`WBStart` 的 baseDir 是绝对路径：宿主 App 的 CWD 是只读的 app bundle，
相对路径 `./auths` 写不进去，必须由 tweak 传 `$HOME/Library/WBPanel`。
api_key 优先级：环境变量 `WB2A_API_KEY` → `<baseDir>/api_key.txt` →
首次随机生成并落盘（0.0.0.0 上绝不能空 key）。

## 多 App 单例（场景 3）

App A 开着（反代已起），App B 也开着：B 的 `WBStart` 先 dial
127.0.0.1:7863，通了就返回 2，不重复起服务，两个 App 连同一个反代。

## 本地构建（Mac / Linux 均可）

```bash
# 依赖：Go ≥1.26.10、zig 0.15.2、python3、ar
export CGO_ENABLED=1 GOOS=darwin GOARCH=arm64
export CC="zig cc -target aarch64-macos-gnu"
export CGO_CFLAGS="-I<zig>/lib/libc/include/any-macos-any -Wno-nullability-completeness"
go build -trimpath -ldflags="-s -w" -buildmode=c-archive -o libwb2api.a ./cmd/carchive
python3 scripts/patch_archive_platform.py libwb2api.a libwb2api-ios.a
zig cc -target aarch64-ios-gnu -dynamiclib -o wb2api.dylib tweak/tweak.c libwb2api-ios.a
```

CI（`.github/workflows/build.yml`，ubuntu-latest）跑同一套流程，
产物 `wb2api.dylib` + `libwb2api.h` 进 artifact。

## v1 已知限制

- 面板配置页返回 501（LoadConfig/SaveConfig 未接）：api_key 与监听地址
  改不了，只能看面板、管账号、签到、看用量。v1.1 接一个小配置读写。
- 面板是桌面布局：390pt 手机屏上要横向拖。v1.1 在 tweak 的 WKWebView
  里用 WKUserScript 注入移动端 CSS/JS（参考 gan-jibamao 的
  assets/ios-mobile.css / ios-mobile.js 做法，不改上游前端）。
- 无保活：宿主被挂起/杀掉，反代跟着停（符合产品定位，用户自己有越狱手段）。
- 会话粘性关闭（Session=nil）：多轮对话靠客户端传完整 messages，
  账号按权重轮换。v1.1 可再开。
- 用量记录、请求日志、余额趋势采样 v1 未接（对应面板接口返回 501），
  不影响接单与签到。

## 已知风险（用户已接受）

- Go panic 会带崩宿主 App；Go signal handler 可能与宿主冲突；
  Go GC 可能触发 jetsam。缓解：dylib 路线走宿主的 jetsam 额度
  （前台 App 上 GB 级），不是 gan-jibamao daemon 路线的 6MB 天花板，
  但后台挂起时请求会冻结，属预期行为。
- 上游随时可能停更或收紧风控（linguo 版已于 2026-10-21 停更，
  misakano7545 fork 是在读大学生维护，无 SLA）。本仓库 vendor 了
  e2d0eb6a 的完整源码，短期内不受上游停更影响。
- 白膜注入器的具体行为不确定（能不能注入 15MB 的 Go runtime dylib），
  需真机验证。

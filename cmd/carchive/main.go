// cmd/carchive — WorkBuddy 反代的 c-archive 导出层（手机插件版）。
//
// 设计选择（见 README.md §导出层决策）：精简入口，不改上游。
// cmd/server/main.go 是 package main，不能 import；但它装配时用到的
// internal/server.Config、internal/panel.Config、scheduler.Config、
// pool.New、upstream.New、auth.LoadDir、redisstore.New 全部是导出、可直接
// import 的包。main() 里真正必需的核心装配（auth 加载 → pool → upstream
// → scheduler → panel → handler → ListenAndServe）在这里重写一遍，
// 约 200 行；main() 其余 ~900 行是 CLI 层（flag 解析、自更新、config 文件
// 读写、livecfg 热更新、审计日志、pprof……），插件里不需要。
//
// 与 cmd/server/main() 的行为差异（有意为之）：
//   - 不解析 flag、不做自更新（updater.HandleSelfUpdate 在 dylib 里极危险）、
//     不装 signal handler、不调 log.Fatalf（失败以返回值 + WBJSON 报告）。
//   - 配置只读 <baseDir>/config.json 的 features 节（默认全开）与 api_key.txt；
//     监听地址写死 0.0.0.0:7863，api_key 走
//     环境变量 WB2A_API_KEY → <baseDir>/api_key.txt → 首次随机生成并落盘。
//   - 面板配置页（LoadConfig/SaveConfig）v1.1 已接：改 api_key 与 features，
//     热生效、无需重启；用量/请求日志/余额趋势/积分历史/代理池记录器也已接线。
//   - 数据目录不走相对路径：宿主 App 的 CWD 是只读的 app bundle，
//     必须由 tweak 传绝对路径（NSHomeDirectory 下的 Library/WBPanel）。
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/balance"
	"github.com/misakano7545/workbuddy2api-panel/internal/credithist"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/panel"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/redisstore"
	"github.com/misakano7545/workbuddy2api-panel/internal/reqlog"
	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
	"github.com/misakano7545/workbuddy2api-panel/internal/scheduler"
	"github.com/misakano7545/workbuddy2api-panel/internal/server"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// appVersion 面板与 /status 里展示的版本；CI 用 -X 注入。
var appVersion = "ios-0.1.0"

// listenAddr 写死 0.0.0.0:7863：局域网必须能连；端口写死与面板内嵌地址一致
//（gan-jibamao 实测：改 listen 面板就空白）。
const listenAddr = "0.0.0.0:7863"

type runningServer struct {
	srv         *http.Server
	ln          net.Listener
	p           *pool.Pool
	apiKey      string
	keyWarn     string // resolveAPIKey 写盘失败的告警（装配时写入，服务期间只读，#16）
	live        *livecfg.Holder // 热配置持有者（面板改 api_key 后，WBAPIKey 读它拿当前值）
	lastErr     string
	serveErr    error // Serve 意外退出的错误（nil = 正常服务中，#9）
	up          *upstream.Client
	bal         *balance.Recorder
	rec         *usage.Recorder
	reqLog      *reqlog.Recorder
	pn          *panel.Panel
	sch         *scheduler.Scheduler
	balStarted  bool // bal.Start() 已调过（Stop 前未 Start 会死锁，见 teardown）
	recStarted  bool
	proxyCancel context.CancelFunc
	proxiesDone chan struct{} // proxies.Run 退出时关闭（#8）
	schedCancel context.CancelFunc
	schedDone   chan struct{} // sch.Run 退出时关闭（#1）
	serveDone   chan struct{} // Serve goroutine 退出时关闭（#7/#9）
	logStop     chan struct{} // 日志守卫的停信号（#E）
	logDone     chan struct{} // 日志守卫退出时关闭（#E）
	teardownOnce sync.Once
}

var (
	mu           sync.Mutex
	cur          *runningServer
	lastStartErr string // 最近一次启动失败的细节（WBJSON 报告；失败不建 stub，#5）
)

// startError 是启动失败的类型化错误，code 映射成 WBStart 的语义化返回码（#15）。
//   -2 通用启动失败（建目录/读账号/装配失败等）；-3 监听失败（非占用原因）。
type startError struct {
	code int
	err  error
}

func (e *startError) Error() string { return e.err.Error() }
func (e *startError) Unwrap() error { return e.err }

// errPortInUse：7863 已被另一个注入过的 App 占用。不是启动失败，WBStart 返回 2。
var errPortInUse = errors.New("7863 已被占用")

// startReturnCode 把 startLocked 的错误映射成 WBStart 返回码（#15）。
func startReturnCode(err error) C.int {
	if errors.Is(err, errPortInUse) {
		return 2
	}
	var se *startError
	if errors.As(err, &se) {
		return C.int(se.code)
	}
	return -2
}

// recoverPanic 是 panic 隔离（#4）：export 函数与自起 goroutine 的顶层都调它，
// 记日志 + 堆栈，绝不让 panic 穿到宿主 App（会直接崩掉整个 OpenMinis）。
func recoverPanic(where string) {
	if r := recover(); r != nil {
		log.Printf("[wb2api] PANIC (%s): %v\n%s", where, r, debug.Stack())
	}
}

// waitDone 等一个 done channel 关闭，最多 d；超时记日志继续清理。
func waitDone(ch <-chan struct{}, d time.Duration, name string) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(d):
		log.Printf("[wb2api] %s %s 未退出，继续清理", name, d)
	}
}

//export WBStart
// WBStart 启动反代网关。baseDir 为宿主传来的绝对数据目录
//（如 <App容器>/Library/WBPanel），auths/ 与 data/ 建在它下面。
// 返回值：0=本次启动成功；1=本进程已启动过（重复调用）；2=7863 已有
// 别人（另一个注入过的 App）在服务，本进程只当客户端，不重复起服务；
// 负数=启动失败（-1 baseDir 为空；-2 通用失败；-3 监听失败）。
// 失败细节见 WBJSON 的 last_error。
func WBStart(baseDir *C.char) (rc C.int) {
	// #4：panic 隔离。Go 侧任何 panic 都不能穿到宿主 App。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[wb2api] WBStart PANIC: %v\n%s", r, debug.Stack())
			rc = -2
		}
	}()
	if baseDir == nil {
		return -1
	}
	base := C.GoString(baseDir)
	if strings.TrimSpace(base) == "" {
		return -1
	}
	// 先查本进程是否已在跑（#10：mu 只保护状态切换）。
	// 注意顺序：必须先查 cur 再 dial——本进程已在跑时端口就是本进程在听，
	// 先 dial 会误判成「别的 App 在跑」而返回 2。
	mu.Lock()
	if cur != nil && cur.srv != nil {
		mu.Unlock()
		return 1
	}
	mu.Unlock()
	// 单例预检（快速路径，不持锁）：7863 已通，说明另一个注入过的 App 已经在跑。
	// 这只是快速路径，真正的权威判断在 startLocked 的 net.Listen（#6）。
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:7863", 300*time.Millisecond); err == nil {
		conn.Close()
		log.Printf("[wb2api] 7863 已有服务在跑，本进程只当客户端")
		// #C：也要 resolveAPIKey 并存一个只有 key 的客户端 stub，
		// 让 WBAPIKey 能返回本进程的 key（与服务端 key 一致时面板可免密登录）。
		key, warn, kerr := resolveAPIKey(base)
		if kerr != nil {
			log.Printf("[wb2api] 客户端模式 resolveAPIKey 失败: %v", kerr)
		}
		mu.Lock()
		cur = &runningServer{apiKey: key, keyWarn: warn}
		mu.Unlock()
		return 2
	}

	rs, err := startLocked(base)
	if err != nil {
		log.Printf("[wb2api] 启动失败: %v", err)
		code := startReturnCode(err)
		mu.Lock()
		lastStartErr = err.Error()
		if code == 2 {
			// TOCTOU：dial 预检时没人，Listen 时被别的 App 抢了——同样当客户端。
			key, warn, kerr := resolveAPIKey(base)
			if kerr != nil {
				log.Printf("[wb2api] 客户端模式 resolveAPIKey 失败: %v", kerr)
			}
			cur = &runningServer{apiKey: key, keyWarn: warn}
		}
		mu.Unlock()
		return code
	}

	// 先装 cur（只做状态切换），再等端口（不持锁，#10）。
	// 装在前面的原因：等待期间 WBStop 能找到它并停掉；若先等后装，
	// 等待期间 WBStop 会以为没在跑而漏停。
	mu.Lock()
	if cur != nil && cur.srv != nil {
		// 另一个 WBStart 抢先装好了（理论上不会，构造器只调一次）。
		mu.Unlock()
		rs.teardown()
		return 1
	}
	cur = rs
	mu.Unlock()

	// 端口等待（不持锁，#10）：确认端口真正起来（最多 3 秒）。
	if err := waitPortUp(3 * time.Second); err != nil {
		// 超时回滚。WBStop 可能已在等待期间把它停掉（cur 已不是 rs），
		// 那时不用再拆；否则自己拆（teardown 幂等，重复调也安全）。
		mu.Lock()
		mine := cur == rs
		if mine {
			cur = nil
		}
		mu.Unlock()
		if mine {
			rs.teardown()
		}
		log.Printf("[wb2api] 启动失败: %v", err)
		mu.Lock()
		lastStartErr = err.Error()
		mu.Unlock()
		return -3
	}
	return 0
}

// waitPortUp 等端口真正起来（最多 d）；起不来返回错误。
func waitPortUp(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:7863", 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("7863 在 %s 内未起来: %v", d, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

//export WBStop
// WBStop 停止网关（宿主退出时调用；0=已停，1=本来就没跑）。
func WBStop() (rc C.int) {
	// #4：panic 隔离。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[wb2api] WBStop PANIC: %v\n%s", r, debug.Stack())
			rc = 1
		}
	}()
	// 只做状态切换（取走 cur），清理本身不持 mu（#7：Serve goroutine
	// 退出时要拿 mu 写状态，持锁等它会死锁）。
	mu.Lock()
	rs := cur
	cur = nil
	mu.Unlock()
	if rs == nil {
		return 1
	}
	rs.teardown()
	return 0
}

// teardown 逆序停止一个 runningServer 的全部后台服务（#2/#3/#7/#8/#14）。
// 幂等（sync.Once），字段为 nil 时跳过，partial 装配也能调。
// 顺序：先停调度器与代理（不再产生新请求/新任务）→ 等它们退出
// → 停 HTTP 服务（Shutdown，超时则 Close 强制断连，等 Serve 退出）
// → 停任务中心与记录器（最后落盘）→ 关 pool 与闲置连接。
// 调用时不持 mu。
func (rs *runningServer) teardown() {
	rs.teardownOnce.Do(rs.teardownImpl)
}

func (rs *runningServer) teardownImpl() {
	if rs.schedCancel != nil {
		rs.schedCancel()
	}
	if rs.proxyCancel != nil {
		rs.proxyCancel()
	}
	if rs.logStop != nil {
		close(rs.logStop)
	}
	// 等调度器与代理的 goroutine 退出（各最多 2 秒），避免它们还在用
	// 即将被关闭的 pool/upstream。
	waitDone(rs.schedDone, 2*time.Second, "sch.Run")
	waitDone(rs.proxiesDone, 2*time.Second, "proxies.Run")
	waitDone(rs.logDone, 2*time.Second, "logGuard")
	if rs.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		// #7：检查 Shutdown 错误；超时则 Close() 强制断连。
		if err := rs.srv.Shutdown(ctx); err != nil {
			log.Printf("[wb2api] Shutdown 未完成（%v），强制 Close", err)
			if cerr := rs.srv.Close(); cerr != nil {
				log.Printf("[wb2api] Close 失败: %v", cerr)
			}
		}
		cancel()
		// 等 Serve goroutine 退出（最多 2 秒），再关依赖。
		waitDone(rs.serveDone, 2*time.Second, "Serve")
	}
	if rs.pn != nil {
		rs.pn.StopTaskJobs()
	}
	// bal/rec 的 Stop 在未 Start 时会死锁（等一个永远不会关闭的 done），
	// 所以只停已 Start 过的（Start 紧跟 New，见 startLocked）。
	if rs.balStarted && rs.bal != nil {
		rs.bal.Stop()
	}
	if rs.recStarted && rs.rec != nil {
		rs.rec.Stop()
	}
	if rs.reqLog != nil {
		rs.reqLog.Close()
	}
	if rs.p != nil {
		rs.p.Close()
	}
	if rs.up != nil && rs.up.HTTP != nil {
		rs.up.HTTP.CloseIdleConnections()
	}
}

//export WBAPIKey
// WBAPIKey 返回当前生效的访问密钥（Live 快照；面板改完 key 后这里即新值）。
// 供 tweak 的 WKWebView 注入 localStorage（wb2api.key），面板免输密钥。
// 调用方用 WBFree 释放；服务未启动时返回空串。
func WBAPIKey() (s *C.char) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[wb2api] WBAPIKey PANIC: %v\n%s", r, debug.Stack())
			s = C.CString("")
		}
	}()
	mu.Lock()
	defer mu.Unlock()
	if cur != nil && cur.live != nil {
		return C.CString(cur.live.Load().APIKey)
	}
	if cur != nil {
		return C.CString(cur.apiKey)
	}
	return C.CString("")
}

//export WBVersion
// WBVersion 返回版本串；调用方用 WBFree 释放。
func WBVersion() (s *C.char) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[wb2api] WBVersion PANIC: %v\n%s", r, debug.Stack())
			s = C.CString("")
		}
	}()
	return C.CString(appVersion)
}

//export WBJSON
// WBJSON 返回当前状态 JSON；调用方用 WBFree 释放。
// {"running":true,"listen":"0.0.0.0:7863","version":"...",
//  "accounts":0,"healthy":0,"api_key_set":true,"last_error":"",
//  "api_key_warning":""}
func WBJSON() (s *C.char) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[wb2api] WBJSON PANIC: %v\n%s", r, debug.Stack())
			s = C.CString("{}")
		}
	}()
	mu.Lock()
	defer mu.Unlock()
	st := map[string]any{
		// #9：Serve 意外退出后 running 如实变 false（serveErr != nil）。
		"running":         cur != nil && cur.srv != nil && cur.serveErr == nil,
		"listen":          listenAddr,
		"version":         appVersion,
		"accounts":        0,
		"healthy":         0,
		"api_key_set":     false,
		"last_error":      lastStartErr,
		"api_key_warning": "",
	}
	if cur != nil {
		// #G：api_key_set 读 Live 快照（面板改 key 后的当前值），不是启动时的快照。
		key := cur.apiKey
		if cur.live != nil {
			key = cur.live.Load().APIKey
		}
		st["api_key_set"] = key != ""
		st["api_key_warning"] = cur.keyWarn
		// 正在跑的服务：用它自己的 lastErr；客户端 stub：保留最近一次启动失败。
		if cur.srv != nil {
			st["last_error"] = cur.lastErr
		}
		if cur.p != nil {
			total, healthy, _, _, _ := cur.p.CountsDetailed()
			st["accounts"] = total
			st["healthy"] = healthy
		}
	}
	b, err := json.Marshal(st)
	if err != nil {
		return C.CString("{}")
	}
	return C.CString(string(b))
}

//export WBKeyPersistWarning
// WBKeyPersistWarning 返回 api_key 写盘失败的告警（"" = 无告警）。
// 供 tweak 在启动 toast 与面板横幅里暴露（#16）。调用方用 WBFree 释放。
func WBKeyPersistWarning() (s *C.char) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[wb2api] WBKeyPersistWarning PANIC: %v\n%s", r, debug.Stack())
			s = C.CString("")
		}
	}()
	mu.Lock()
	defer mu.Unlock()
	if cur != nil {
		return C.CString(cur.keyWarn)
	}
	return C.CString("")
}

//export WBFree
// WBFree 释放 WBVersion/WBJSON 返回的字符串。
func WBFree(p *C.char) {
	defer recoverPanic("WBFree")
	C.free(unsafe.Pointer(p))
}

// startLocked 做真正的装配（调用时不持 mu；WBStart 保证同一时间只有一个
// 装配在跑）。返回已装配好的 runningServer（Serve 已启动，端口尚未经
// waitPortUp 确认），或一个带返回码的错误。任何中途失败都会先回滚已装配
// 的部分（#3：统一 rollback = rs.teardown()，与 WBStop 共用同一份清理逻辑）。
// 对应 cmd/server/main() 的核心装配段，但去掉 CLI 层。
func startLocked(base string) (*runningServer, error) {
	rs := &runningServer{}
	rollback := func() { rs.teardown() }

	authDir := filepath.Join(base, "auths")
	dataDir := filepath.Join(base, "data")
	// 教训（实测 5.5）：auths/ 不存在时 auth.LoadDir 直接 log.Fatalf。
	// 这里先建目录，失败返回错误不退出宿主。
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return nil, &startError{code: -2, err: fmt.Errorf("建 auths 目录失败: %w", err)}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, &startError{code: -2, err: fmt.Errorf("建 data 目录失败: %w", err)}
	}

	apiKey, keyWarn, err := resolveAPIKey(base)
	if err != nil {
		return nil, &startError{code: -2, err: fmt.Errorf("resolve api_key: %w", err)}
	}
	rs.apiKey = apiKey
	rs.keyWarn = keyWarn

	auths, err := auth.LoadDir(authDir)
	if err != nil {
		return nil, &startError{code: -2, err: fmt.Errorf("load auths: %w", err)}
	}
	log.Printf("[wb2api] loaded %d account(s) from %s", len(auths), authDir)

	// redisstore 未配置 → Noop 纯内存模式（与 main() 行为一致）。
	store := redisstore.New("", "")

	p := pool.New(filepath.Join(dataDir, "state.json"))
	p.SetStore(store)
	p.RestoreFromSnapshot()
	p.SyncToDir(auths)
	rs.p = p

	up := upstream.New() // 默认 node TLS 指纹（与 main() 一致）
	rs.up = up

	// v1.1：features 默认全开；<baseDir>/config.json 的 features 节可逐项覆盖（缺省即开）。
	feats := loadMobileFeatures(base)
	log.Printf("[wb2api] features: tts=%v asr=%v embeddings=%v watermark_strip=%v 3d=%v",
		feats.TTSEnabled, feats.ASREnabled, feats.EmbeddingsEnabled, feats.WatermarkStripEnabled, feats.ThreeDEnabled)
	// 3D 凭证来源常驻（与上游默认 three_d_auto_token=true 一致）：用账号自己的 access token
	// 换 ck_t_，静态凭证表为空。只开开关不接凭证会报 503；常驻则面板上把 3D 从关改开也
	// 能直接用，不用重启。静态 API Key 账号（ck_/pt_ 前缀）签不出来，上游自动跳过并回 503。
	up.ThreeDCredential = upstream.NewAutoThreeDCredentials(up.FetchClientTempKey, upstream.NewStaticThreeDCredentials(nil))

	// Live 热配置持有者：api_key 与 features 的运行期唯一真实来源。
	// handler 与面板都优先读 Live 快照（静态字段只作 Live 为 nil 时的回退）。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:                apiKey,
		TTSEnabled:            feats.TTSEnabled,
		ASREnabled:            feats.ASREnabled,
		EmbeddingsEnabled:     feats.EmbeddingsEnabled,
		WatermarkStripEnabled: feats.WatermarkStripEnabled,
		ThreeDEnabled:         feats.ThreeDEnabled,
	})
	rs.live = live

	// 记录器（⑥⑦⑧⑩）：与 state.json 同目录，随 WBPanel 一起搬移；损坏不影响启动。
	// Start 紧跟 New（#3 回滚依赖：Stop 在未 Start 时会死锁，所以绝不能出现
	// 「已创建、未 Start、失败回滚时调 Stop」的窗口）。
	configPath := filepath.Join(base, "config.json")
	bal := balance.New(filepath.Join(dataDir, "balance_history.json"))
	bal.Start()
	rs.bal = bal
	rs.balStarted = true
	rec := usage.New(filepath.Join(dataDir, "usage.json"))
	rec.Start()
	rs.rec = rec
	rs.recStarted = true
	requestLog := reqlog.New(reqlog.Config{
		Dir:           filepath.Join(dataDir, "request-logs"),
		Enabled:       true,
		RetentionDays: 30,
		MaxBytes:      100 << 20,
	})
	rs.reqLog = requestLog
	creditHist := credithist.New(filepath.Join(dataDir, "credit-history.json"), 2000)
	up.SetCreditObserver(creditHist.Observe)

	// 代理池：失败只记日志、不影响启动（面板代理接口届时回空集）。
	var proxies *proxypool.Manager
	if baseTr, ok := up.HTTP.Transport.(*http.Transport); ok {
		if pm, err := proxypool.New(filepath.Join(dataDir, "proxy-pool.json"), baseTr, upstream.NewDNSDialer(nil)); err != nil {
			log.Printf("[wb2api] proxy pool 不可用: %v", err)
		} else {
			proxies = pm
			// #A：经代理的 HTTPS 用与直连同一套 TLS 指纹与请求头顺序。
			proxies.SetTLSOver(up.TLSOver)
			// #A：把代理池挂到 upstream 客户端上，否则转发请求全部走直连，
			// 面板里能配代理、能健康检查、能绑账号，但代理池静默失效。
			up.ProxyPool = proxies
			// #8：用可取消的 context（不要 Background），WBStop / 启动失败回滚时取消，
			// 否则后台任务永不停止，重启还会再起一个、共用同一个 proxy-pool.json。
			proxyCtx, cancel := context.WithCancel(context.Background())
			rs.proxyCancel = cancel
			rs.proxiesDone = make(chan struct{})
			go func() {
				defer close(rs.proxiesDone)
				defer recoverPanic("proxies.Run")
				proxies.Run(proxyCtx)
			}()
		}
	}

	// scheduler：余额采样记录器挂在余额刷新之后（nil = 不采样）。
	// 默认排程：签到 9/21 点、旅行 9/21 点、活跃 10 点、保活 22 点、成长 11 点；面板手动签到/保活走它。
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up, Balance: bal})
	rs.sch = sch

	// 配置页后端（④）：LoadConfig 回显当前生效值（Live 快照），SaveConfig 合并落盘 + 热应用。
	// configSaveMu 串行化保存（并发两次保存会在读-改-写与热应用之间互踩）。
	var configSaveMu sync.Mutex
	loadCfg := func() (any, error) {
		return mobileConfigForPanel(live), nil
	}
	saveCfg := func(raw []byte) ([]string, error) {
		configSaveMu.Lock()
		defer configSaveMu.Unlock()
		return saveMobileConfig(raw, configPath, filepath.Join(base, "api_key.txt"), live)
	}

	pn := panel.New(panel.Config{
		Pool:             p,
		ProxyPool:        proxies,
		Usage:            rec,
		RequestLog:       requestLog,
		Upstream:         up,
		Scheduler:        sch,
		Balance:          bal,
		CreditHistory:    creditHist,
		ProbeFile:        filepath.Join(dataDir, "output_probes.json"),
		AuthDir:          authDir,
		APIKey:           apiKey,
		Version:          appVersion,
		Live:             live,
		ConfigPath:       configPath,
		LoadConfig:       loadCfg,
		SaveConfig:       saveCfg,
		AutoTasksEnabled: sch.GrowthEnabled,
		RescueToday:      server.RescueToday,
		ModelFallback:    func() map[string]string { return live.Load().ModelFallback },
	})
	rs.pn = pn

	// #B：把面板的任务执行器注入调度器（上游 main.go:545-546）。
	sch.SetGrowthRunner(pn.RunGrowthQueueNow)
	sch.SetSweepRunner(pn.RunTaskSweepNow)
	sch.SetJitter(0, 0)
	sch.SetMakeupCardsPerRound(0)

	// #B：任务中心（上游 main.go:625-628）。状态与主 state 同目录，
	// 重启后自动恢复未完成作业；不接的话任务中心按钮直接 503。
	schedCtx, schedCancel := context.WithCancel(context.Background())
	rs.schedCancel = schedCancel
	if err := pn.StartTaskJobs(schedCtx, filepath.Join(dataDir, "task-jobs.json")); err != nil {
		rollback()
		return nil, &startError{code: -2, err: fmt.Errorf("task jobs: %w", err)}
	}
	pn.ResumeTaskJobs()

	// #B：启动调度器主循环（上游 main.go:632）。签到/旅行/活跃/保活/成长
	// 的定时任务与余额刷新都由它驱动；不启动则账号 token 自然过期。
	rs.schedDone = make(chan struct{})
	go func() {
		defer close(rs.schedDone)
		defer recoverPanic("sch.Run")
		sch.Run(schedCtx)
	}()
	// 上游默认间隔：余额刷新 5 分钟、补领清扫 15 分钟、额度窗口探针缺省开。
	sch.StartBalanceRefresh(schedCtx, 5*time.Minute)
	sch.StartTaskSweep(schedCtx, 15*time.Minute)
	sch.StartModelWindowProbe(schedCtx)

	server.BuildVersion = appVersion
	gw := server.NewHandler(server.Config{
		Pool:                  p,
		Upstream:              up,
		APIKey:                apiKey,
		Panel:                 pn,
		Live:                  live,
		Usage:                 rec,
		RequestLog:            requestLog,
		GlobalEnabled:         true,
		BusyRetry:             true,
		BusyWaitMax:           60 * time.Second,
		Tasks:                 sch,
		// 静态字段与 Live 快照同值（Live 为 nil 时的回退；正常走 Live）。
		TTSEnabled:            feats.TTSEnabled,
		ASREnabled:            feats.ASREnabled,
		EmbeddingsEnabled:     feats.EmbeddingsEnabled,
		WatermarkStripEnabled: feats.WatermarkStripEnabled,
		ThreeDEnabled:         feats.ThreeDEnabled,
	})
	// D11：handler 层限制请求体（32MB），防止超大请求体耗尽内存。
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
		gw.ServeHTTP(w, r)
	})

	// #6：先同步 Listen（Listen 本身就是原子检查，EADDRINUSE 即有人在跑），
	// 再 go Serve。旧代码「先 dial 预检、再 ListenAndServe」在两者之间有竞态窗口。
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		rollback()
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, errPortInUse
		}
		return nil, &startError{code: -3, err: fmt.Errorf("监听 %s 失败: %w", listenAddr, err)}
	}
	rs.ln = ln
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second, // D11：闲置 keep-alive 连接 2 分钟后关闭
		MaxHeaderBytes:    1 << 20,           // D11：请求头上限 1MB
	}
	rs.srv = srv
	rs.serveDone = make(chan struct{})
	go func() {
		defer close(rs.serveDone)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[wb2api] Serve PANIC: %v\n%s", r, debug.Stack())
				mu.Lock()
				if cur == rs {
					rs.serveErr = fmt.Errorf("serve panic: %v", r)
					rs.lastErr = rs.serveErr.Error()
				}
				mu.Unlock()
			}
		}()
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			mu.Lock()
			if cur == rs { // #9：仅当 cur 仍是本次实例才更新状态
				rs.serveErr = err
				rs.lastErr = "serve 意外退出: " + err.Error()
			}
			mu.Unlock()
			log.Printf("[wb2api] serve failed: %v", err)
		}
	}()

	// #E：日志守卫。Go 侧每请求都有 log.Printf，全进 wb2api.log；
	// 轮转只在 tweak.c 构造器做一次，连续多天不重启会无限长大。
	// 每 10 分钟检查一次，超 10MB 就截断（stderr 是 O_APPEND 打开，截断安全）。
	rs.logStop = make(chan struct{})
	rs.logDone = make(chan struct{})
	go rs.logGuard(base)

	log.Printf("[wb2api] listening on %s (api_key=%s)", listenAddr, maskKey(apiKey))
	log.Printf("[wb2api] 管理面板 http://127.0.0.1:7863/panel/")
	return rs, nil
}

// logGuard 定期检查 wb2api.log 大小，超 10MB 就截断（#E）。
func (rs *runningServer) logGuard(base string) {
	defer close(rs.logDone)
	defer recoverPanic("logGuard")
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	logPath := filepath.Join(base, "wb2api.log")
	for {
		select {
		case <-rs.logStop:
			return
		case <-t.C:
			if st, err := os.Stat(logPath); err == nil && st.Size() > 10<<20 {
				if err := os.Truncate(logPath, 0); err != nil {
					log.Printf("[wb2api] 日志截断失败: %v", err)
				} else {
					log.Printf("[wb2api] wb2api.log 超 10MB，已截断")
				}
			}
		}
	}
}

// resolveAPIKey：环境变量 WB2A_API_KEY > <base>/api_key.txt > 首次随机生成并落盘。
// 空 key = 网关完全不鉴权；监听 0.0.0.0 时绝不能空，所以没有就生成一个。
// 返回 (key, warn, err)：
//   - warn != ""：key 已生成但写盘失败（内存里的 key 仍可用，只是下次启动会重新生成；
//     调用方把 warn 存进 keyWarn，面板暴露告警，#16）。
//   - err != nil：致命失败，调用方应放弃启动。
// #9：rand 失败时返回错误（fail closed），绝不用硬编码的公开密钥——
// 局域网可达的服务用公开密钥等于不鉴权。
// #D：读 api_key.txt 时只有「不存在」才重新生成；其他读错误（权限/IO）返回错误，
// 不能悄悄生成新 key 覆盖——否则一次读失败就会让全网客户端同时掉线。
func resolveAPIKey(base string) (string, string, error) {
	if k := strings.TrimSpace(os.Getenv("WB2A_API_KEY")); k != "" {
		return k, "", nil
	}
	keyFile := filepath.Join(base, "api_key.txt")
	b, err := os.ReadFile(keyFile)
	if err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, "", nil
		}
		// 空文件：当作不存在，继续生成新 key。
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("读 api_key.txt 失败: %w", err)
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", fmt.Errorf("生成随机 api_key 失败: %w", err)
	}
	k := hex.EncodeToString(buf[:])
	// #12：原子写（先写 .tmp 再 rename），避免写一半崩掉留下损坏的 key 文件。
	if err := safefile.WriteKeep(keyFile, []byte(k+"\n"), 0o600); err != nil {
		// 写盘失败不致命：内存里的 key 仍可用，只是下次启动会重新生成。
		// 记日志 + 返回告警（调用方存进 keyWarn，面板暴露，#16）。
		warn := "api_key 未能写入磁盘，下次启动会重新生成（旧客户端会掉线）"
		log.Printf("[wb2api] %s: %v", warn, err)
		return k, warn, nil
	}
	return k, "", nil
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return "****"
	}
	return k[:2] + "******" + k[len(k)-2:]
}

// mobileFeatures v1.1 默认全开的 features；config.json 可逐项覆盖。
// 用 *bool 区分「没写」（nil → 默认开）与「显式写 false」（关）。
type mobileFeatures struct {
	TTSEnabled            bool
	ASREnabled            bool
	EmbeddingsEnabled     bool
	WatermarkStripEnabled bool
	ThreeDEnabled         bool
}

// loadMobileFeatures 读 <baseDir>/config.json 的 features 节（上游 config.json 同款键名）。
// 文件不存在、解析失败或某键缺省时，该项默认开；绝不因配置文件问题导致启动失败。
func loadMobileFeatures(base string) mobileFeatures {
	def := mobileFeatures{TTSEnabled: true, ASREnabled: true, EmbeddingsEnabled: true, WatermarkStripEnabled: true, ThreeDEnabled: true}
	raw, err := os.ReadFile(filepath.Join(base, "config.json"))
	if err != nil {
		return def
	}
	var cfg struct {
		Features struct {
			TTSEnabled            *bool `json:"tts_enabled"`
			ASREnabled            *bool `json:"asr_enabled"`
			EmbeddingsEnabled     *bool `json:"embeddings_enabled"`
			WatermarkStripEnabled *bool `json:"watermark_strip_enabled"`
			ThreeDEnabled         *bool `json:"three_d_enabled"`
		} `json:"features"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("[wb2api] config.json 解析失败，用默认全开: %v", err)
		return def
	}
	f := cfg.Features
	if f.TTSEnabled != nil {
		def.TTSEnabled = *f.TTSEnabled
	}
	if f.ASREnabled != nil {
		def.ASREnabled = *f.ASREnabled
	}
	if f.EmbeddingsEnabled != nil {
		def.EmbeddingsEnabled = *f.EmbeddingsEnabled
	}
	if f.WatermarkStripEnabled != nil {
		def.WatermarkStripEnabled = *f.WatermarkStripEnabled
	}
	if f.ThreeDEnabled != nil {
		def.ThreeDEnabled = *f.ThreeDEnabled
	}
	return def
}

// mobileConfigForPanel 组装配置页回显对象：当前生效值（Live 快照），不是只读文件。
// 前端按 CFG_MAP 填表；没写的字段留空，保存时跳过（不下发），所以只返回 api_key 与
// features 是安全的。关键：features 必须回显当前值（否则复选框默认不勾，保存时
// 会把没动过的开关误发成 false 关掉）。
func mobileConfigForPanel(live *livecfg.Holder) map[string]any {
	snap := live.Load()
	return map[string]any{
		"api_key": snap.APIKey,
		"features": map[string]any{
			"tts_enabled":             snap.TTSEnabled,
			"asr_enabled":             snap.ASREnabled,
			"embeddings_enabled":      snap.EmbeddingsEnabled,
			"watermark_strip_enabled": snap.WatermarkStripEnabled,
			"three_d_enabled":         snap.ThreeDEnabled,
		},
	}
}

// saveMobileConfig 面板保存配置：解析提交 → 与磁盘 config.json 深度合并（缺失键保留现值）
// → 校验 → 原子落盘 → api_key 同步写 api_key.txt（重启时 resolveAPIKey 只读它，不读
// config.json；不同步下次启动会用回旧 key）→ 热应用到 Live（api_key 与 features 立即
// 生效，无需重启）。返回需重启字段（手机端恒为空）。
func saveMobileConfig(raw []byte, configPath, keyFile string, live *livecfg.Holder) ([]string, error) {
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	cur := map[string]any{}
	if b, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(b, &cur); err != nil {
			return nil, fmt.Errorf("parse current config: %w", err)
		}
	}
	merged := mergeConfigMaps(cur, incoming)
	// 校验：只认 api_key（字符串，非空——手机端监听 0.0.0.0 必须有密钥）与 features.*（布尔）；
	// 其余键原样保留、不校验（手机端不用）。
	if v, ok := merged["api_key"]; ok {
		k, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("api_key 必须是字符串")
		}
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("api_key 不能为空：手机端监听 0.0.0.0，必须设访问密钥")
		}
	}
	if v, ok := merged["features"]; ok {
		fm, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("features 必须是对象")
		}
		for _, k := range []string{"tts_enabled", "asr_enabled", "embeddings_enabled", "watermark_strip_enabled", "three_d_enabled"} {
			if fv, ok := fm[k]; ok {
				if _, ok := fv.(bool); !ok {
					return nil, fmt.Errorf("features.%s 必须是布尔值", k)
				}
			}
		}
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	if err := safefile.WriteKeep(configPath, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	// api_key 同步写 api_key.txt（重启时的唯一来源）。
	// #10：写失败要返回错误，不能静默忽略——否则 config.json 已是新 key、
	// api_key.txt 还是旧 key，重启后用回旧 key，面板显示与实际分叉。
	// #12：原子写（先写 .tmp 再 rename），与 config.json 同款。
	if v, ok := merged["api_key"]; ok {
		if k, ok := v.(string); ok && strings.TrimSpace(k) != "" {
			if err := safefile.WriteKeep(keyFile, []byte(strings.TrimSpace(k)+"\n"), 0o600); err != nil {
				return nil, fmt.Errorf("write api_key.txt: %w", err)
			}
		}
	}
	// 热应用：从当前快照出发，只覆盖 api_key 与 features（缺失键保留现值）。
	snap := live.Load()
	if v, ok := merged["api_key"]; ok {
		if k, ok := v.(string); ok {
			snap.APIKey = strings.TrimSpace(k)
		}
	}
	if v, ok := merged["features"]; ok {
		if fm, ok := v.(map[string]any); ok {
			if fv, ok := fm["tts_enabled"].(bool); ok {
				snap.TTSEnabled = fv
			}
			if fv, ok := fm["asr_enabled"].(bool); ok {
				snap.ASREnabled = fv
			}
			if fv, ok := fm["embeddings_enabled"].(bool); ok {
				snap.EmbeddingsEnabled = fv
			}
			if fv, ok := fm["watermark_strip_enabled"].(bool); ok {
				snap.WatermarkStripEnabled = fv
			}
			if fv, ok := fm["three_d_enabled"].(bool); ok {
				snap.ThreeDEnabled = fv
			}
		}
	}
	live.Store(snap)
	log.Printf("[wb2api] 配置已保存并热生效（api_key 与 features 无需重启）")
	return []string{}, nil
}

// mergeConfigMaps 深度合并：incoming 里出现的键覆盖 cur；嵌套对象递归合并；
// incoming 里没有的键保留 cur 的值（这就是「缺失字段保留现值」）。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(cur)+len(incoming))
	for k, v := range cur {
		out[k] = v
	}
	for k, v := range incoming {
		if vm, ok := v.(map[string]any); ok {
			if cm, ok := out[k].(map[string]any); ok {
				out[k] = mergeConfigMaps(cm, vm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func main() {}

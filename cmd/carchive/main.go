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
//   - 配置不读 config.json：监听地址写死 0.0.0.0:7863，api_key 走
//     环境变量 WB2A_API_KEY → <baseDir>/api_key.txt → 首次随机生成并落盘。
//   - 面板配置页（LoadConfig/SaveConfig）v1 留空：面板里改配置返回 501，
//     v1.1 再接（见 README）。
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
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/panel"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/redisstore"
	"github.com/misakano7545/workbuddy2api-panel/internal/scheduler"
	"github.com/misakano7545/workbuddy2api-panel/internal/server"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// appVersion 面板与 /status 里展示的版本；CI 用 -X 注入。
var appVersion = "ios-0.1.0"

// listenAddr 写死 0.0.0.0:7863：局域网必须能连；端口写死与面板内嵌地址一致
//（gan-jibamao 实测：改 listen 面板就空白）。
const listenAddr = "0.0.0.0:7863"

type runningServer struct {
	srv       *http.Server
	p         *pool.Pool
	startedAt time.Time
	apiKey    string
	lastErr   string
}

var (
	mu  sync.Mutex
	cur *runningServer
)

//export WBStart
// WBStart 启动反代网关。baseDir 为宿主传来的绝对数据目录
//（如 <App容器>/Library/WBPanel），auths/ 与 data/ 建在它下面。
// 返回值：0=本次启动成功；1=本进程已启动过（重复调用）；2=7863 已有
// 别人（另一个注入过的 App）在服务，本进程只当客户端，不重复起服务；
// 负数=启动失败（-1 baseDir 为空；-2 建目录/读账号失败；-3 监听失败）。
// 失败细节见 WBJSON 的 last_error。
func WBStart(baseDir *C.char) C.int {
	if baseDir == nil {
		return -1
	}
	base := C.GoString(baseDir)
	if strings.TrimSpace(base) == "" {
		return -1
	}
	mu.Lock()
	defer mu.Unlock()
	if cur != nil {
		return 1
	}
	// 单例锁（多 App 场景）：7863 已通，说明另一个注入过的 App 已经在跑，
	// 本进程不重复起服务，只当客户端。
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:7863", 300*time.Millisecond); err == nil {
		conn.Close()
		log.Printf("[wb2api] 7863 已有服务在跑，本进程只当客户端")
		return 2
	}
	if err := startLocked(base); err != nil {
		log.Printf("[wb2api] 启动失败: %v", err)
		if cur != nil {
			cur.lastErr = err.Error()
		} else {
			cur = &runningServer{lastErr: err.Error()}
		}
		return -2
	}
	return 0
}

//export WBStop
// WBStop 停止网关（宿主退出时调用；0=已停，1=本来就没跑）。
func WBStop() C.int {
	mu.Lock()
	defer mu.Unlock()
	if cur == nil || cur.srv == nil {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = cur.srv.Shutdown(ctx)
	if cur.p != nil {
		cur.p.Close()
	}
	cur = nil
	return 0
}

//export WBVersion
// WBVersion 返回版本串；调用方用 WBFree 释放。
func WBVersion() *C.char {
	return C.CString(appVersion)
}

//export WBJSON
// WBJSON 返回当前状态 JSON；调用方用 WBFree 释放。
// {"running":true,"listen":"0.0.0.0:7863","version":"...",
//  "accounts":0,"healthy":0,"api_key_set":true,"last_error":""}
func WBJSON() *C.char {
	mu.Lock()
	defer mu.Unlock()
	st := map[string]any{
		"running":     cur != nil && cur.srv != nil,
		"listen":      listenAddr,
		"version":     appVersion,
		"accounts":    0,
		"healthy":     0,
		"api_key_set": false,
		"last_error":  "",
	}
	if cur != nil {
		st["api_key_set"] = cur.apiKey != ""
		st["last_error"] = cur.lastErr
		if cur.p != nil {
			total, healthy, _, _, _ := cur.p.CountsDetailed()
			st["accounts"] = total
			st["healthy"] = healthy
		}
	}
	b, _ := json.Marshal(st)
	return C.CString(string(b))
}

//export WBFree
// WBFree 释放 WBVersion/WBJSON 返回的字符串。
func WBFree(p *C.char) {
	C.free(unsafe.Pointer(p))
}

// startLocked 做真正的装配（调用时已持 mu）。对应 cmd/server/main() 的
// 核心装配段，但去掉 CLI 层。
func startLocked(base string) error {
	authDir := filepath.Join(base, "auths")
	dataDir := filepath.Join(base, "data")
	// 教训（实测 5.5）：auths/ 不存在时 auth.LoadDir 直接 log.Fatalf。
	// 这里先建目录，失败返回错误不退出宿主。
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return fmt.Errorf("建 auths 目录失败: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("建 data 目录失败: %w", err)
	}

	apiKey := resolveAPIKey(base)

	auths, err := auth.LoadDir(authDir)
	if err != nil {
		return fmt.Errorf("load auths: %w", err)
	}
	log.Printf("[wb2api] loaded %d account(s) from %s", len(auths), authDir)

	// redisstore 未配置 → Noop 纯内存模式（与 main() 行为一致）。
	store := redisstore.New("", "")

	p := pool.New(filepath.Join(dataDir, "state.json"))
	p.SetStore(store)
	p.RestoreFromSnapshot()
	p.SyncToDir(auths)

	up := upstream.New() // 默认 node TLS 指纹（与 main() 一致）

	// scheduler 空 Config 即用默认排程：签到 9/21 点、旅行 9/21 点、
	// 活跃 10 点、保活 22 点、成长 11 点；面板手动签到/保活走它。
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	pn := panel.New(panel.Config{
		Pool:      p,
		Upstream:  up,
		Scheduler: sch,
		AuthDir:   authDir,
		APIKey:    apiKey,
		Version:   appVersion,
		// ConfigPath/LoadConfig/SaveConfig 留空：面板配置页 v1 返回 501。
	})

	server.BuildVersion = appVersion
	gw := server.NewHandler(server.Config{
		Pool:          p,
		Upstream:      up,
		APIKey:        apiKey,
		Panel:         pn,
		GlobalEnabled: true,
		BusyRetry:     true,
		BusyWaitMax:   60 * time.Second,
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}
	cur = &runningServer{srv: srv, p: p, startedAt: time.Now(), apiKey: apiKey}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			mu.Lock()
			if cur != nil {
				cur.lastErr = err.Error()
			}
			mu.Unlock()
			log.Printf("[wb2api] listen failed: %v", err)
		}
	}()
	// 等端口真正起来（最多 3 秒）；起不来就回滚，WBStart 返回失败。
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:7863", 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			_ = srv.Close()
			cur = nil
			return fmt.Errorf("7863 在 3 秒内未起来: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("[wb2api] listening on %s (api_key=%s)", listenAddr, maskKey(apiKey))
	log.Printf("[wb2api] 管理面板 http://127.0.0.1:7863/panel/")
	return nil
}

// resolveAPIKey：环境变量 WB2A_API_KEY > <base>/api_key.txt > 首次随机生成并落盘。
// 空 key = 网关完全不鉴权；监听 0.0.0.0 时绝不能空，所以没有就生成一个。
func resolveAPIKey(base string) string {
	if k := strings.TrimSpace(os.Getenv("WB2A_API_KEY")); k != "" {
		return k
	}
	keyFile := filepath.Join(base, "api_key.txt")
	if b, err := os.ReadFile(keyFile); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k
		}
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "wb2api-mobile-key"
	}
	k := hex.EncodeToString(buf[:])
	_ = os.WriteFile(keyFile, []byte(k+"\n"), 0o600)
	return k
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return "****"
	}
	return k[:2] + "******" + k[len(k)-2:]
}

func main() {}

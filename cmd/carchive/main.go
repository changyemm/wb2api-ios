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
	srv       *http.Server
	p         *pool.Pool
	startedAt time.Time
	apiKey    string
	live      *livecfg.Holder // 热配置持有者（面板改 api_key 后，WBAPIKey 读它拿当前值）
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

//export WBAPIKey
// WBAPIKey 返回当前生效的访问密钥（Live 快照；面板改完 key 后这里即新值）。
// 供 tweak 的 WKWebView 注入 localStorage（wb2api.key），面板免输密钥。
// 调用方用 WBFree 释放；服务未启动时返回空串。
func WBAPIKey() *C.char {
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

	// 记录器（⑥⑦⑧⑩）：与 state.json 同目录，随 WBPanel 一起搬移；损坏不影响启动。
	configPath := filepath.Join(base, "config.json")
	bal := balance.New(filepath.Join(dataDir, "balance_history.json"))
	bal.Start()
	rec := usage.New(filepath.Join(dataDir, "usage.json"))
	rec.Start()
	requestLog := reqlog.New(reqlog.Config{
		Dir:           filepath.Join(dataDir, "request-logs"),
		Enabled:       true,
		RetentionDays: 30,
		MaxBytes:      100 << 20,
	})
	creditHist := credithist.New(filepath.Join(dataDir, "credit-history.json"), 2000)
	up.SetCreditObserver(creditHist.Observe)
	// 代理池：失败只记日志、不影响启动（面板代理接口届时回空集）。
	var proxies *proxypool.Manager
	if baseTr, ok := up.HTTP.Transport.(*http.Transport); ok {
		if pm, err := proxypool.New(filepath.Join(dataDir, "proxy-pool.json"), baseTr, upstream.NewDNSDialer(nil)); err != nil {
			log.Printf("[wb2api] proxy pool 不可用: %v", err)
		} else {
			proxies = pm
			go proxies.Run(context.Background())
		}
	}

	// scheduler：余额采样记录器挂在余额刷新之后（nil = 不采样）。
	// 默认排程：签到 9/21 点、旅行 9/21 点、活跃 10 点、保活 22 点、成长 11 点；面板手动签到/保活走它。
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up, Balance: bal})

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

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}
	cur = &runningServer{srv: srv, p: p, startedAt: time.Now(), apiKey: apiKey, live: live}
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
	if v, ok := merged["api_key"]; ok {
		if k, ok := v.(string); ok && strings.TrimSpace(k) != "" {
			_ = os.WriteFile(keyFile, []byte(strings.TrimSpace(k)+"\n"), 0o600)
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

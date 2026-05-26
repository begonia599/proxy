// claude-proxy: 透明转发 Anthropic Messages API，副线记录用量到 SQLite。
//
// 设计原则：
//   1. 请求/响应 body 永远字节级原样转发，绝不解析后再重组（缓存命中靠字节一致）
//   2. 只动 header 里和"代理身份"必然冲突的部分：x-api-key、host、accept-encoding
//   3. 用 tee 把响应 body 旁路一份给后台 goroutine 解析 usage，不阻塞主转发
//   4. 用量入 SQLite，/admin/stats 提供查询，需 admin token
//
// 文件分工：
//   config.go  .env 解析 + Config
//   usage.go   UsageRecord + usage 解析 + 入库
//   tee.go     响应 body 旁路
//   compat.go  OpenAI Chat Completions 兼容层
//   compat_convert.go  OpenAI ↔ Anthropic 格式转换
//   proxy.go   反向代理装配 + 转发 / v1/models 拦截 handler
//   admin.go   /admin/* JSON API
//   keys.go    proxy key 缓存
//   models.go  模型注册表 + 30 分钟刷新
//   pricing.go 各模型 token 单价
//   storage.go SQLite 持久化层
package main

import (
	_ "embed"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

const (
	upstreamURL = "https://api.anthropic.com"
	listenAddr  = ":8787"
	dbPath      = "claude-proxy.db"
)

// 进程级单例：被 admin handler 和 forward handler 共用。
var (
	store         *Store
	keys          *KeyCache
	modelRegistry *ModelRegistry
)

func main() {
	cfg := loadConfig()

	var err error
	store, err = openStore(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// 加载 proxy key 缓存
	keys = NewKeyCache(store)
	if err := keys.Reload(); err != nil {
		log.Fatalf("load proxy keys: %v", err)
	}
	log.Printf("proxy keys loaded: %d active", keys.Size())

	// 拉一次模型列表；失败也不阻止启动，注册表会保持空 → fail-open
	modelRegistry = NewModelRegistry(store)
	if err := modelRegistry.ReloadFromDB(); err != nil {
		log.Printf("model registry initial DB load failed: %v", err)
	}
	if err := modelRegistry.Refresh(cfg.RealKey); err != nil {
		log.Printf("initial model registry load failed: %v (fail-open, will retry in 30m)", err)
	}
	modelRegistry.RunPeriodic(cfg.RealKey, 30*time.Minute)

	rp := buildReverseProxy(cfg)

	mux := http.NewServeMux()

	mux.HandleFunc("/admin/stats", adminStatsHandler(cfg))
	mux.HandleFunc("/admin/keys", adminKeysHandler(cfg))
	mux.HandleFunc("/admin/keys/", adminKeysHandler(cfg))
	mux.HandleFunc("/admin/models", adminModelsHandler(cfg))
	mux.HandleFunc("/admin/models/", adminModelsHandler(cfg))
	mux.HandleFunc("/admin/logs", adminLogsHandler(cfg))
	mux.HandleFunc("/admin/logs/", adminLogsHandler(cfg))
	mux.HandleFunc("/admin/timeseries", adminTimeseriesHandler(cfg))
	mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		// 仪表盘 HTML 本身不含敏感数据，token 在浏览器里输入。
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set("cache-control", "no-store")
		_, _ = w.Write(dashboardHTML)
	})

	// OpenAI 兼容层
	mux.HandleFunc("/v1/chat/completions", openaiChatHandler(cfg))

	// /v1/models: 按 auth 风格分发——Bearer → OpenAI 格式，x-api-key → Anthropic 格式
	oaiModels := openaiModelsHandler()
	anthModels := modelsListHandler(rp)
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" && strings.HasPrefix(r.Header.Get("authorization"), "Bearer ") {
			oaiModels.ServeHTTP(w, r)
			return
		}
		anthModels.ServeHTTP(w, r)
	})
	mux.HandleFunc("/v1/models/", modelDetailHandler(rp))
	mux.HandleFunc("/", forwardHandler(rp))

	log.Printf("claude-proxy listening on %s, upstream %s", listenAddr, upstreamURL)
	log.Printf("admin token: %s", cfg.AdminToken)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

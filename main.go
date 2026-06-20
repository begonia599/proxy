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
	store            *Store
	keys             *KeyCache
	modelRegistry    *ModelRegistry
	providerRegistry *ProviderRegistry
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

	// 用户系统：清过期 session → 给旧密钥回填 creator(=admin) → 首次启动建 admin 账号
	if n, err := store.PurgeExpiredSessions(); err == nil && n > 0 {
		log.Printf("purged %d expired sessions", n)
	}
	migrateCreators()
	bootStrapAdmin()

	// 多上游：把旧 .env key 迁移成 anthropic-official 服务商（幂等、网络无关），
	// 再载入服务商 + 小组映射缓存。
	migrateLegacyKeyToProvider(store, cfg.GetRealKey())
	providerRegistry = NewProviderRegistry(store)
	if err := providerRegistry.Reload(); err != nil {
		log.Fatalf("load providers: %v", err)
	}
	// 启动时刷新一次各服务商模型库存（失败不阻止启动）。
	go providerRegistry.RefreshAll()
	providerRegistry.RunPeriodic(30 * time.Minute)

	// 旧模型注册表保留：/v1/models 在 key 未绑小组时回落到它（fail-open）。
	modelRegistry = NewModelRegistry(store)
	if err := modelRegistry.ReloadFromDB(); err != nil {
		log.Printf("model registry initial DB load failed: %v", err)
	}
	if k := cfg.GetRealKey(); k != "" {
		if err := modelRegistry.Refresh(k); err != nil {
			log.Printf("initial model registry load failed: %v (fail-open, will retry in 30m)", err)
		}
	}
	modelRegistry.RunPeriodic(cfg.GetRealKey, 30*time.Minute)

	rp := buildReverseProxy(cfg)

	mux := http.NewServeMux()

	// 用户系统：登录 / 登出 / 当前用户 / 改密（无需鉴权的 login + 需鉴权的其余）
	mux.HandleFunc("/auth/login", authLoginHandler())
	mux.HandleFunc("/auth/logout", requireAuth(authLogoutHandler()))
	mux.HandleFunc("/auth/me", authMeHandler())
	mux.HandleFunc("/auth/password", requireAuth(authPasswordHandler()))

	mux.HandleFunc("/admin/stats", adminStatsHandler(cfg))
	mux.HandleFunc("/admin/keys", adminKeysHandler(cfg))
	mux.HandleFunc("/admin/keys/", adminKeysHandler(cfg))
	mux.HandleFunc("/admin/models", adminModelsHandler(cfg))
	mux.HandleFunc("/admin/models/", adminModelsHandler(cfg))
	mux.HandleFunc("/admin/logs", adminLogsHandler(cfg))
	mux.HandleFunc("/admin/logs/", adminLogsHandler(cfg))
	mux.HandleFunc("/admin/timeseries", adminTimeseriesHandler(cfg))
	// 多上游配置：服务商 / 小组 / 映射（上游 key 统一在此管理，无独立 config 页）
	mux.HandleFunc("/admin/providers", adminProvidersHandler(cfg))
	mux.HandleFunc("/admin/providers/", adminProvidersHandler(cfg))
	mux.HandleFunc("/admin/groups", adminGroupsHandler(cfg))
	mux.HandleFunc("/admin/groups/", adminGroupsHandler(cfg))
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
	mux.HandleFunc("/", forwardHandler(cfg, rp))

	log.Printf("claude-proxy listening on %s, upstream %s", listenAddr, upstreamURL)
	// admin token 已废弃：鉴权改用账号密码登录（见启动时的 admin 引导日志）。
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

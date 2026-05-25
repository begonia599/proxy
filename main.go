// claude-proxy: 透明转发 Anthropic Messages API，副线记录用量到 SQLite。
//
// 设计原则：
//   1. 请求/响应 body 永远字节级原样转发，绝不解析后再重组（缓存命中靠字节一致）
//   2. 只动 header 里和"代理身份"必然冲突的部分：x-api-key、host、accept-encoding
//   3. 用 tee 把响应 body 旁路一份给后台 goroutine 解析 usage，不阻塞主转发
//   4. 用量入 SQLite，/admin/stats 提供查询，需 admin token
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

const (
	upstreamURL = "https://api.anthropic.com"
	listenAddr  = ":8787"
	dbPath      = "claude-proxy.db"
)

// ---------- 配置 ----------

type Config struct {
	RealKey    string
	AdminToken string // /admin/* 接口的 bearer
	HideCC     bool
}

func loadConfig() *Config {
	cfg := &Config{
		AdminToken: "admin-secret-change-me",
		HideCC:     false,
	}
	// 先找当前目录的 .env（标准位置）；找不到再退到上一级（兼容旧布局）
	data, err := os.ReadFile(".env")
	if err != nil {
		data, err = os.ReadFile("../.env")
		if err != nil {
			log.Fatalf("read .env: %v (looked in . and ..)", err)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "key=") {
			cfg.RealKey = strings.TrimPrefix(line, "key=")
		}
		if strings.HasPrefix(line, "admin_token=") {
			cfg.AdminToken = strings.TrimPrefix(line, "admin_token=")
		}
	}
	if cfg.RealKey == "" {
		log.Fatal("real key not found in .env")
	}
	return cfg
}

// ---------- 用量记录 ----------

type UsageRecord struct {
	Time          time.Time
	ProxyKey      string
	RequestID     string
	HTTPReqID     string
	Endpoint      string
	Method        string
	Status        int
	Model         string
	InputTokens   int
	OutputTokens  int
	CacheCreate5m int
	CacheCreate1h int
	CacheRead     int
	WebSearchCount int
	CostUSD       float64
	LatencyMs     int64
	Streaming     bool
	StopReason    string
	ClientIP      string
	UserAgent     string
}

var store *Store
var keys *KeyCache
var modelRegistry *ModelRegistry

// errorBodyCap 单边 body 最多入库这么多字节（256 KB）。
// 错误诊断够用了，再大就是浪费 SQLite 体积。
const errorBodyCap = 256 << 10

// reqBodyKey 把请求 body 通过 context 传给 ModifyResponse 回调，
// 让 tee.onClose 在非 2xx 时能把请求体也存进 error_bodies。
type reqBodyKey struct{}

func withReqBody(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, reqBodyKey{}, body)
}
func reqBodyFrom(ctx context.Context) []byte {
	v, _ := ctx.Value(reqBodyKey{}).([]byte)
	return v
}

func truncBody(b []byte) []byte {
	if len(b) <= errorBodyCap {
		return b
	}
	return b[:errorBodyCap]
}

// writeAnthropicError 输出与上游同款的错误结构，让 SDK 客户端能正常解析。
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

func writeRecord(r *UsageRecord, reqBody, respBody []byte) {
	// 算成本：token 部分按模型价格 + web search 按次数
	r.CostUSD = CostOf(r.Model, r.InputTokens, r.OutputTokens,
		r.CacheCreate5m, r.CacheCreate1h, r.CacheRead) +
		WebSearchCost(r.WebSearchCount)

	rowID, err := store.Insert(r)
	if err != nil {
		// 已在 store 里打过日志，不重复
		_ = err
	} else if rowID > 0 && (r.Status < 200 || r.Status >= 300) {
		// 非 2xx 才保存 body（截断到 256 KB 以防 50MB 大请求把 DB 撑爆）
		_ = store.InsertErrorBody(rowID, truncBody(reqBody), truncBody(respBody))
	}

	wsSuffix := ""
	if r.WebSearchCount > 0 {
		wsSuffix = fmt.Sprintf(" web_search=%d", r.WebSearchCount)
	}
	fmt.Printf("[%s] %s %s status=%d in=%d out=%d cache_r=%d cache_w=%d%s cost=$%.6f %dms\n",
		r.Time.Format("15:04:05"), r.ProxyKey, r.Model, r.Status,
		r.InputTokens, r.OutputTokens, r.CacheRead, r.CacheCreate5m+r.CacheCreate1h,
		wsSuffix, r.CostUSD, r.LatencyMs)
}

// ---------- tee reader ----------

type teeBody struct {
	src     io.ReadCloser
	buf     *bytes.Buffer
	maxBuf  int
	onClose func([]byte)
	once    sync.Once
}

func newTeeBody(src io.ReadCloser, onClose func([]byte)) *teeBody {
	return &teeBody{
		src:     src,
		buf:     &bytes.Buffer{},
		maxBuf:  4 << 20,
		onClose: onClose,
	}
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 && t.buf.Len() < t.maxBuf {
		remain := t.maxBuf - t.buf.Len()
		w := n
		if w > remain {
			w = remain
		}
		t.buf.Write(p[:w])
	}
	if err != nil {
		t.fire()
	}
	return n, err
}

func (t *teeBody) Close() error {
	t.fire()
	return t.src.Close()
}

func (t *teeBody) fire() {
	t.once.Do(func() {
		captured := append([]byte(nil), t.buf.Bytes()...)
		go t.onClose(captured)
	})
}

// ---------- usage 解析 ----------

type usageBlock struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreation            struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	ServerToolUse struct {
		WebSearchRequests int `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

func (u usageBlock) toRecord(r *UsageRecord) {
	r.InputTokens = u.InputTokens
	r.OutputTokens = u.OutputTokens
	r.CacheRead = u.CacheReadInputTokens
	// 优先用拆分字段
	if u.CacheCreation.Ephemeral5m > 0 || u.CacheCreation.Ephemeral1h > 0 {
		r.CacheCreate5m = u.CacheCreation.Ephemeral5m
		r.CacheCreate1h = u.CacheCreation.Ephemeral1h
	} else {
		// 老接口只给了 cache_creation_input_tokens 合并值，归到 5m
		r.CacheCreate5m = u.CacheCreationInputTokens
	}
	if u.ServerToolUse.WebSearchRequests > 0 {
		r.WebSearchCount = u.ServerToolUse.WebSearchRequests
	}
}

func parseUsageJSON(body []byte, rec *UsageRecord) {
	var r struct {
		ID         string     `json:"id"`
		Model      string     `json:"model"`
		StopReason string     `json:"stop_reason"`
		Usage      usageBlock `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return
	}
	rec.RequestID = r.ID
	if r.Model != "" {
		rec.Model = r.Model
	}
	rec.StopReason = r.StopReason
	r.Usage.toRecord(rec)
}

func parseUsageSSE(body []byte, rec *UsageRecord) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var ev struct {
			Type    string `json:"type"`
			Message struct {
				ID    string     `json:"id"`
				Model string     `json:"model"`
				Usage usageBlock `json:"usage"`
			} `json:"message"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage usageBlock `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "message_start":
			rec.RequestID = ev.Message.ID
			if ev.Message.Model != "" {
				rec.Model = ev.Message.Model
			}
			ev.Message.Usage.toRecord(rec)
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				rec.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.ServerToolUse.WebSearchRequests > 0 {
				rec.WebSearchCount = ev.Usage.ServerToolUse.WebSearchRequests
			}
			if ev.Delta.StopReason != "" {
				rec.StopReason = ev.Delta.StopReason
			}
		}
	}
}

// ---------- /admin 处理 ----------

func adminStatsHandler(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("authorization")
		expected := "Bearer " + cfg.AdminToken
		if auth != expected {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		q := r.URL.Query()
		f := StatsFilter{
			ProxyKey: q.Get("proxy_key"),
			Model:    q.Get("model"),
		}
		if s := q.Get("since"); s != "" {
			t, err := parseTime(s)
			if err != nil {
				http.Error(w, `{"error":"bad since"}`, http.StatusBadRequest)
				return
			}
			f.Since = t
		}
		if s := q.Get("until"); s != "" {
			t, err := parseTime(s)
			if err != nil {
				http.Error(w, `{"error":"bad until"}`, http.StatusBadRequest)
				return
			}
			f.Until = t
		}

		result, err := store.Stats(f)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}

		w.Header().Set("content-type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
	}
}

// adminKeysHandler 处理 /admin/keys 与 /admin/keys/{key}：
//   GET    /admin/keys              列出全部（?include_revoked=true 含撤销）
//   POST   /admin/keys              创建（body: owner/daily_budget/allowed_models/notes）
//   GET    /admin/keys/{key}        取单条
//   PATCH  /admin/keys/{key}        部分更新
//   DELETE /admin/keys/{key}        软撤销（revoked_at = now）
// 每次写操作后必须 keys.Reload() 让请求路径感知到。
func adminKeysHandler(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+cfg.AdminToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")

		// 从 path 提取 key（可能为空）
		path := strings.TrimPrefix(r.URL.Path, "/admin/keys")
		path = strings.TrimPrefix(path, "/")

		switch r.Method {
		case http.MethodGet:
			if path == "" {
				includeRevoked := r.URL.Query().Get("include_revoked") == "true"
				list, err := store.ListKeys(includeRevoked)
				if err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
					return
				}
				if list == nil {
					list = []KeyMeta{}
				}
				_ = json.NewEncoder(w).Encode(list)
				return
			}
			k, err := store.GetKey(path)
			if err != nil {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(k)

		case http.MethodPost:
			if path != "" {
				http.Error(w, `{"error":"POST goes to /admin/keys, not /admin/keys/{key}"}`, http.StatusBadRequest)
				return
			}
			var req struct {
				Owner         string  `json:"owner"`
				DailyBudget   float64 `json:"daily_budget"`
				AllowedModels string  `json:"allowed_models"`
				Notes         string  `json:"notes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(req.Owner) == "" {
				http.Error(w, `{"error":"owner required"}`, http.StatusBadRequest)
				return
			}
			newKey, err := GenerateProxyKey()
			if err != nil {
				http.Error(w, `{"error":"key gen failed"}`, http.StatusInternalServerError)
				return
			}
			km := &KeyMeta{
				Key:           newKey,
				Owner:         req.Owner,
				DailyBudget:   req.DailyBudget,
				AllowedModels: req.AllowedModels,
				Notes:         req.Notes,
			}
			if err := store.CreateKey(km); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			if err := keys.Reload(); err != nil {
				log.Printf("keys reload after create: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(km)

		case http.MethodPatch:
			if path == "" {
				http.Error(w, `{"error":"key required in path"}`, http.StatusBadRequest)
				return
			}
			var u KeyUpdate
			if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			if err := store.UpdateKey(path, u); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			if err := keys.Reload(); err != nil {
				log.Printf("keys reload after update: %v", err)
			}
			k, _ := store.GetKey(path)
			_ = json.NewEncoder(w).Encode(k)

		case http.MethodDelete:
			if path == "" {
				http.Error(w, `{"error":"key required in path"}`, http.StatusBadRequest)
				return
			}
			if err := store.RevokeKey(path); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			if err := keys.Reload(); err != nil {
				log.Printf("keys reload after revoke: %v", err)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// adminModelsHandler 处理 /admin/models 与 /admin/models/{id}：
//   GET   /admin/models           列出全部 curated 模型（enabled + 时间戳）
//   PATCH /admin/models/{id}      切换 enabled（body: {"enabled":true|false}）
// PATCH 后必须 modelRegistry.ReloadFromDB() 让 IsKnown 立即生效。
func adminModelsHandler(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+cfg.AdminToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")

		path := strings.TrimPrefix(r.URL.Path, "/admin/models")
		path = strings.TrimPrefix(path, "/")

		switch r.Method {
		case http.MethodGet:
			if path != "" {
				http.Error(w, `{"error":"GET single not supported, use list"}`, http.StatusBadRequest)
				return
			}
			list, err := store.ListCuratedModels()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			if list == nil {
				list = []CuratedModel{}
			}
			_ = json.NewEncoder(w).Encode(list)

		case http.MethodPatch:
			if path == "" {
				http.Error(w, `{"error":"model id required in path"}`, http.StatusBadRequest)
				return
			}
			var req struct {
				Enabled *bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			if req.Enabled == nil {
				http.Error(w, `{"error":"enabled field required"}`, http.StatusBadRequest)
				return
			}
			if err := store.SetModelEnabled(path, *req.Enabled); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			if err := modelRegistry.ReloadFromDB(); err != nil {
				log.Printf("model registry reload after toggle: %v", err)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// adminLogsHandler 处理 /admin/logs 与 /admin/logs/{id}：
//   GET /admin/logs?limit=N&before_id=X&proxy_key=...&model=...&status=success|error&since=...&until=...
//   GET /admin/logs/{id}  返回单条详情（含 error_bodies 里保存的 body 快照）
func adminLogsHandler(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+cfg.AdminToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("content-type", "application/json")

		path := strings.TrimPrefix(r.URL.Path, "/admin/logs")
		path = strings.TrimPrefix(path, "/")
		if path != "" {
			var id int64
			if _, err := fmt.Sscanf(path, "%d", &id); err != nil || id <= 0 {
				http.Error(w, `{"error":"bad id"}`, http.StatusBadRequest)
				return
			}
			d, err := store.GetLogDetail(id)
			if err != nil {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(d)
			return
		}

		q := r.URL.Query()
		f := LogFilter{
			ProxyKey:    q.Get("proxy_key"),
			Model:       q.Get("model"),
			StatusClass: q.Get("status"),
		}
		if s := q.Get("since"); s != "" {
			t, err := parseTime(s)
			if err != nil {
				http.Error(w, `{"error":"bad since"}`, http.StatusBadRequest)
				return
			}
			f.Since = t
		}
		if s := q.Get("until"); s != "" {
			t, err := parseTime(s)
			if err != nil {
				http.Error(w, `{"error":"bad until"}`, http.StatusBadRequest)
				return
			}
			f.Until = t
		}
		if s := q.Get("before_id"); s != "" {
			_, _ = fmt.Sscanf(s, "%d", &f.BeforeID)
		}
		if s := q.Get("limit"); s != "" {
			_, _ = fmt.Sscanf(s, "%d", &f.Limit)
		}

		list, err := store.ListLogs(f)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []LogRow{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"logs":  list,
			"count": len(list),
		})
	}
}

// adminTimeseriesHandler 处理 /admin/timeseries：
//   GET /admin/timeseries?granularity=hour|day&since=...&until=...&proxy_key=...&model=...
// 给前端折线图喂数据（输入/输出/总数/缓存/缓存命中率）。
func adminTimeseriesHandler(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+cfg.AdminToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("content-type", "application/json")

		q := r.URL.Query()
		f := StatsFilter{ProxyKey: q.Get("proxy_key"), Model: q.Get("model")}
		if s := q.Get("since"); s != "" {
			t, err := parseTime(s)
			if err != nil {
				http.Error(w, `{"error":"bad since"}`, http.StatusBadRequest)
				return
			}
			f.Since = t
		}
		if s := q.Get("until"); s != "" {
			t, err := parseTime(s)
			if err != nil {
				http.Error(w, `{"error":"bad until"}`, http.StatusBadRequest)
				return
			}
			f.Until = t
		}
		gran := q.Get("granularity")
		if gran == "" {
			gran = "day"
		}
		buckets, err := store.Timeseries(f, gran)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		if buckets == nil {
			buckets = []TimeBucket{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"granularity": gran,
			"buckets":     buckets,
		})
	}
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time: %s", s)
}

// ---------- 主代理 ----------

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

	target, _ := url.Parse(upstreamURL)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1

	origDirector := rp.Director
	rp.Director = func(r *http.Request) {
		origDirector(r)
		r.Host = target.Host
		r.Header.Set("accept-encoding", "identity")

		r.Header.Del("x-api-key")
		r.Header.Del("authorization")
		r.Header.Set("x-api-key", cfg.RealKey)

		if cfg.HideCC {
			r.Header.Set("user-agent", "claude-proxy/0.2")
			r.Header.Del("x-app")
			for h := range r.Header {
				if strings.HasPrefix(strings.ToLower(h), "x-stainless-") {
					r.Header.Del(h)
				}
			}
		}
	}

	rp.ModifyResponse = func(resp *http.Response) error {
		startStr := resp.Request.Header.Get("x-proxy-start")
		var latency int64
		if startStr != "" {
			if t, err := time.Parse(time.RFC3339Nano, startStr); err == nil {
				latency = time.Since(t).Milliseconds()
			}
		}

		base := &UsageRecord{
			Time:      time.Now(),
			ProxyKey:  resp.Request.Header.Get("x-proxy-key-original"),
			HTTPReqID: resp.Header.Get("request-id"),
			Endpoint:  resp.Request.URL.Path,
			Method:    resp.Request.Method,
			Status:    resp.StatusCode,
			Model:     resp.Request.Header.Get("x-proxy-req-model"), // 兜底：错误响应也有归属
			ClientIP:  resp.Request.Header.Get("x-proxy-client-ip"),
			UserAgent: resp.Request.Header.Get("x-proxy-user-agent"),
			LatencyMs: latency,
			Streaming: strings.HasPrefix(resp.Header.Get("content-type"), "text/event-stream"),
		}

		ct := resp.Header.Get("content-type")
		reqBody := reqBodyFrom(resp.Request.Context())
		resp.Body = newTeeBody(resp.Body, func(captured []byte) {
			if base.Streaming {
				parseUsageSSE(captured, base)
			} else if strings.Contains(ct, "application/json") {
				parseUsageJSON(captured, base)
			}
			writeRecord(base, reqBody, captured)
		})
		return nil
	}

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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxyKey := r.Header.Get("x-api-key")
		keyMeta, ok := keys.Get(proxyKey)
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy key")
			return
		}

		r.Header.Set("x-proxy-key-original", proxyKey)
		r.Header.Set("x-proxy-start", time.Now().Format(time.RFC3339Nano))
		r.Header.Set("x-proxy-client-ip", clientIP(r))
		r.Header.Set("x-proxy-user-agent", r.Header.Get("user-agent"))

		// 预算检查：daily_budget > 0 时拦截超额请求。
		// 查 DB 而非缓存内累计值——更准、单 SQL 不到 1ms。
		if keyMeta.DailyBudget > 0 {
			spent, err := store.TodaysCost(proxyKey)
			if err == nil && spent >= keyMeta.DailyBudget {
				log.Printf("budget exceeded: key=%s owner=%s spent=$%.4f budget=$%.2f",
					proxyKey, keyMeta.Owner, spent, keyMeta.DailyBudget)
				writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
					fmt.Sprintf("daily budget exceeded: $%.4f / $%.2f", spent, keyMeta.DailyBudget))
				go writeRecord(&UsageRecord{
					Time:       time.Now(),
					ProxyKey:   proxyKey,
					Endpoint:   r.URL.Path,
					Method:     r.Method,
					Status:     http.StatusTooManyRequests,
					ClientIP:   clientIP(r),
					UserAgent:  r.Header.Get("user-agent"),
					StopReason: "proxy_rejected_budget",
				}, nil, nil)
				return
			}
		}

		// 提前从请求 body 取 model + speed + tools，做几件事：
		//   1) 注入模型归属（错误响应也能归类到正确模型）
		//   2) 预检：未知模型直接拒绝，省一次上游往返
		//   3) 拦截 speed=fast：fast mode 是 6× 计价，本代理不允许，避免账单失控
		//   4) 记录请求声明了哪些服务端工具（web_search/web_fetch/code_execution）
		//      用来判断客户端（如 Claude Code）是否走了带计费的服务端工具
		//
		// 必须读完整个 body 再放回去，否则截断会破坏上游请求。
		// 50MB 上限防 DoS，对正常 Anthropic 请求绰绰有余。
		const maxBody = 50 << 20
		if r.Body != nil && r.Method == http.MethodPost {
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(raw))
				r.ContentLength = int64(len(raw))
				// 把 body 塞进 context，ModifyResponse 的 tee 回调要拿它写 error_bodies
				r = r.WithContext(withReqBody(r.Context(), raw))
				var peek struct {
					Model string `json:"model"`
					Speed string `json:"speed"`
					Tools []struct {
						Type string `json:"type"`
						Name string `json:"name"`
					} `json:"tools"`
				}
				if json.Unmarshal(raw, &peek) == nil {
					if peek.Speed == "fast" {
						log.Printf("reject fast mode: key=%s model=%s", proxyKey, peek.Model)
						writeAnthropicError(w, http.StatusForbidden, "permission_error",
							"fast mode is disabled on this proxy (6x billing protection)")
						go writeRecord(&UsageRecord{
							Time:       time.Now(),
							ProxyKey:   proxyKey,
							Endpoint:   r.URL.Path,
							Method:     r.Method,
							Status:     http.StatusForbidden,
							Model:      peek.Model,
							ClientIP:   clientIP(r),
							UserAgent:  r.Header.Get("user-agent"),
							StopReason: "proxy_rejected_fast",
						}, raw, nil)
						return
					}
					if serverTools := pickServerTools(peek.Tools); len(serverTools) > 0 {
						log.Printf("server tools declared: key=%s model=%s tools=%v",
							proxyKey, peek.Model, serverTools)
					}
					if peek.Model != "" {
						r.Header.Set("x-proxy-req-model", peek.Model)
						if !modelRegistry.IsKnown(peek.Model) {
							known, _ := modelRegistry.Snapshot()
							log.Printf("reject unknown model: %s (known: %d)", peek.Model, len(known))
							writeAnthropicError(w, http.StatusNotFound, "not_found_error",
								"model not available via this proxy: "+peek.Model)
							go writeRecord(&UsageRecord{
								Time:       time.Now(),
								ProxyKey:   proxyKey,
								Endpoint:   r.URL.Path,
								Method:     r.Method,
								Status:     http.StatusNotFound,
								Model:      peek.Model,
								ClientIP:   clientIP(r),
								UserAgent:  r.Header.Get("user-agent"),
								StopReason: "proxy_rejected",
							}, raw, nil)
							return
						}
						if !keyMeta.ModelAllowed(peek.Model) {
							log.Printf("reject by allowlist: key=%s owner=%s model=%s allowed=%s",
								proxyKey, keyMeta.Owner, peek.Model, keyMeta.AllowedModels)
							writeAnthropicError(w, http.StatusForbidden, "permission_error",
								"this proxy key is not permitted to use model: "+peek.Model)
							go writeRecord(&UsageRecord{
								Time:       time.Now(),
								ProxyKey:   proxyKey,
								Endpoint:   r.URL.Path,
								Method:     r.Method,
								Status:     http.StatusForbidden,
								Model:      peek.Model,
								ClientIP:   clientIP(r),
								UserAgent:  r.Header.Get("user-agent"),
								StopReason: "proxy_rejected_allowlist",
							}, raw, nil)
							return
						}
					}
				}
			}
		}

		rp.ServeHTTP(w, r)
	})

	log.Printf("claude-proxy listening on %s, upstream %s", listenAddr, upstreamURL)
	log.Printf("admin token: %s", cfg.AdminToken)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("x-forwarded-for"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	return r.RemoteAddr
}

// pickServerTools 从请求 tools 数组里挑出 Anthropic 服务端工具（带计费的那些）。
// 客户端工具只有 name+input_schema，type 留空；服务端工具的 type 形如
// "web_search_20260209" / "web_fetch_20260209" / "code_execution_20XX..."。
func pickServerTools(tools []struct {
	Type string `json:"type"`
	Name string `json:"name"`
}) []string {
	var out []string
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search_") ||
			strings.HasPrefix(t.Type, "web_fetch_") ||
			strings.HasPrefix(t.Type, "code_execution_") ||
			strings.HasPrefix(t.Type, "computer_") ||
			strings.HasPrefix(t.Type, "bash_") ||
			strings.HasPrefix(t.Type, "text_editor_") {
			out = append(out, t.Type)
		}
	}
	return out
}

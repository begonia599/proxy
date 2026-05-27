// admin.go: /admin/* JSON API（stats / keys / models / logs / timeseries）
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// parseTime 容忍三种时间格式：RFC3339（前端 JSON）、SQL DATETIME、纯日期。
// 解析时统一用 time.Local，让 since=2026-01-01 = 当地零点。
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

// adminConfigHandler 处理 /admin/config：上游 Anthropic key 的查看 / 更新 / 清除。
//
//	GET    /admin/config          → {has_key: bool, masked: "•••…XXXX", length: N}
//	PUT    /admin/config          → body {"upstream_key": "sk-ant-..."} 替换 key
//	DELETE /admin/config          → 清除 key（后续 forward 会返回 503，直到再次设置）
//
// 永远不返回完整 key——只露后缀。.env 写盘走 cfg.SetRealKey,会强制 0600。
func adminConfigHandler(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+cfg.AdminToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")

		switch r.Method {
		case http.MethodGet:
			k := cfg.GetRealKey()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"has_key": k != "",
				"masked":  MaskKey(k),
				"length":  len(k),
			})

		case http.MethodPut:
			var req struct {
				UpstreamKey string `json:"upstream_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			newKey := strings.TrimSpace(req.UpstreamKey)
			if newKey == "" {
				http.Error(w, `{"error":"upstream_key required (use DELETE to clear)"}`, http.StatusBadRequest)
				return
			}
			// 防呆：贴错的 key 太常见，简单校验前缀避免管理员误把 admin_token 之类塞进来。
			if !strings.HasPrefix(newKey, "sk-ant-") {
				http.Error(w, `{"error":"upstream_key must start with sk-ant-"}`, http.StatusBadRequest)
				return
			}
			if err := cfg.SetRealKey(newKey); err != nil {
				log.Printf("set real key: %v", err)
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			log.Printf("upstream key updated via /admin/config (suffix=%s)", MaskKey(newKey))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"masked": MaskKey(newKey),
				"length": len(newKey),
			})

		case http.MethodDelete:
			if err := cfg.SetRealKey(""); err != nil {
				log.Printf("clear real key: %v", err)
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			log.Printf("upstream key cleared via /admin/config")
			_, _ = w.Write([]byte(`{"ok":true}`))

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
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

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func demoAdminStatsHandler(ds *DemoStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := ds.Load()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeDemoJSON(w, demoDashboardStats(st))
	}
}

func demoAdminTimeseriesHandler(ds *DemoStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, _ := ds.Load()
		stats := demoDashboardStats(st)
		input := intFromAny(stats["input_tokens"])
		output := intFromAny(stats["output_tokens"])
		cache := intFromAny(stats["cache_read"])
		var buckets []map[string]any
		for i := 20; i >= 0; i-- {
			day := time.Now().AddDate(0, 0, -i)
			wave := 0.78 + (float64((20-i)%7) / 18.0)
			in := int(float64(input/21) * wave)
			out := int(float64(output/21) * (0.82 + float64(i%5)/30.0))
			cr := int(float64(cache/21) * (0.88 + float64(i%4)/35.0))
			buckets = append(buckets, map[string]any{
				"bucket":         day.Format("01-02"),
				"input_tokens":   in,
				"output_tokens":  out,
				"cache_read":     cr,
				"cache_hit_rate": float64(cr) / float64(in+cr),
			})
		}
		writeDemoJSON(w, map[string]any{"buckets": buckets})
	}
}

func demoAdminKeysHandler(ds *DemoStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/demo/admin/keys")
		path = strings.TrimPrefix(path, "/")
		if unescaped, err := url.PathUnescape(path); err == nil {
			path = unescaped
		}
		switch r.Method {
		case http.MethodGet:
			st, _ := ds.Load()
			if path != "" {
				for _, k := range st.Keys {
					if k.Key == path {
						writeDemoJSON(w, demoKeyView(k, 0))
						return
					}
				}
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			out := make([]map[string]any, 0, len(st.Keys))
			for i, k := range st.Keys {
				out = append(out, demoKeyView(k, i))
			}
			writeDemoJSON(w, out)
		case http.MethodPost:
			var req struct {
				Owner       string  `json:"owner"`
				DailyBudget float64 `json:"daily_budget"`
				Notes       string  `json:"notes"`
				GroupID     *int64  `json:"group_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			model := "gpt-5.5"
			if req.GroupID != nil && *req.GroupID == 3 {
				model = "claude-opus-4.8"
			}
			k, _, err := ds.AddKey(req.Owner, model, req.DailyBudget, req.Notes, req.GroupID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			writeDemoJSON(w, demoKeyView(k, 0))
		case http.MethodPatch:
			var req demoKeyPatch
			_ = json.NewDecoder(r.Body).Decode(&req)
			k, st, err := ds.UpdateKey(path, req)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			idx := 0
			for i, kk := range st.Keys {
				if kk.ID == k.ID {
					idx = i
					break
				}
			}
			writeDemoJSON(w, demoKeyView(k, idx))
		case http.MethodDelete:
			st, err := ds.DeleteKey(path)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			writeDemoJSON(w, map[string]any{"ok": true, "keys": len(st.Keys)})
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func demoAdminProvidersHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/demo/admin/providers")
		path = strings.TrimPrefix(path, "/")
		parts := strings.Split(path, "/")
		if r.Method == http.MethodGet && path == "" {
			writeDemoJSON(w, demoProvidersView())
			return
		}
		if r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "catalog" {
			writeDemoJSON(w, map[string]any{"models": []string{"gpt-5.5", "claude-opus-4.8", "deepseek-reasoner", "glm-5.2-pro"}})
			return
		}
		if r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "models" {
			id, _ := strconv.ParseInt(parts[0], 10, 64)
			writeDemoJSON(w, demoProviderModels(id))
			return
		}
		writeDemoJSON(w, map[string]any{"ok": true})
	}
}

func demoAdminGroupsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/demo/admin/groups")
		path = strings.TrimPrefix(path, "/")
		parts := strings.Split(path, "/")
		if r.Method == http.MethodGet && path == "" {
			writeDemoJSON(w, demoGroupsView())
			return
		}
		if r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "mappings" {
			gid, _ := strconv.ParseInt(parts[0], 10, 64)
			writeDemoJSON(w, demoGroupMappings(gid))
			return
		}
		writeDemoJSON(w, map[string]any{"ok": true})
	}
}

func demoAdminLogsHandler(ds *DemoStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/demo/admin/logs")
		path = strings.TrimPrefix(path, "/")
		st, _ := ds.Load()
		rows := demoLogRows(st.Keys)
		if r.Method == http.MethodGet && path != "" {
			id, _ := strconv.ParseInt(path, 10, 64)
			for _, row := range rows {
				if row["id"] == id {
					if row["status"].(int) >= 200 && row["status"].(int) < 300 {
						row["request_body"] = ""
						row["response_body"] = "(demo 成功请求不保存 body)"
					} else {
						row["request_body"] = demoErrorRequestBody(row)
						row["response_body"] = demoErrorResponseBody(row)
					}
					writeDemoJSON(w, row)
					return
				}
			}
		}
		rows, page, limit, total, totalPages, hasMore := filterDemoLogs(r, rows)
		var nextBeforeID any
		if hasMore && len(rows) > 0 {
			nextBeforeID = rows[len(rows)-1]["id"]
		}
		writeDemoJSON(w, map[string]any{
			"logs":           rows,
			"rows":           rows,
			"count":          len(rows),
			"total":          total,
			"page":           page,
			"limit":          limit,
			"total_pages":    totalPages,
			"has_more":       hasMore,
			"next_before_id": nextBeforeID,
		})
	}
}

func demoDashboardStats(st demoState) map[string]any {
	base := demoStats(st)
	requests := intFromAny(base["requests"])
	input := intFromAny(base["input_tokens"])
	output := intFromAny(base["output_tokens"])
	cache := intFromAny(base["cache_read"])
	cost := floatFromAny(base["cost_usd"])
	web := intFromAny(base["web_search"])
	errs := intFromAny(base["errors"])
	streaming := intFromAny(base["streaming"])
	low := intFromAny(base["low_cache_count"])
	return map[string]any{
		"server_date":      time.Now().Format("2006-01-02"),
		"timezone":         time.Local.String(),
		"requests":         requests,
		"cost_usd":         cost,
		"web_search_count": web,
		"input_tokens":     input,
		"output_tokens":    output,
		"cache_read":       cache,
		"cache_create_5m":  intFromAny(base["cache_write_5m"]),
		"cache_create_1h":  intFromAny(base["cache_write_1h"]),
		"errors":           errs,
		"cache_hit_rate":   float64(cache) / float64(input+cache),
		"risk": map[string]any{
			"recent_hour_cost":   base["risk_hour_cost"],
			"max_request_cost":   base["risk_max_cost"],
			"low_cache_requests": low,
			"max_request_model":  "claude-opus-4.8",
			"max_request_time":   time.Now().Add(-37 * time.Minute).Format(time.RFC3339),
			"streaming_requests": streaming,
		},
		"by_proxy_key": demoBreakdownFromKeys(st.Keys),
		"by_model": []map[string]any{
			demoBreakdown("gpt-5.5", 510000, 16800, .965),
			demoBreakdown("claude-opus-4.8", 388000, 14200, .912),
			demoBreakdown("deepseek-reasoner", 244000, 5100, .785),
			demoBreakdown("glm-5.2-pro", 150000, 1900, .824),
		},
		"by_provider": []map[string]any{
			demoBreakdown("openai-official", 520000, 19000, .951),
			demoBreakdown("anthropic-official", 392000, 14800, .918),
			demoBreakdown("deepseek-hybrid", 267000, 6200, .802),
		},
		"by_endpoint": []map[string]any{
			demoBreakdown("/v1/responses", 610000, 17000, .964),
			demoBreakdown("/v1/messages", 420000, 13900, .914),
			demoBreakdown("/v1/chat/completions", 180000, 2400, .732),
			demoBreakdown("/v1/responses/compact", 82000, 900, .988),
		},
	}
}

func demoBreakdownFromKeys(keys []demoKey) []map[string]any {
	out := make([]map[string]any, 0, len(keys))
	for i, k := range keys {
		out = append(out, demoBreakdown(k.Key, 260000-i*21000, 9200-float64(i)*711, .96-float64(i)*.028))
	}
	return out
}

func demoBreakdown(key string, requests int, cost float64, hit float64) map[string]any {
	return map[string]any{"key": key, "requests": requests, "cost_usd": cost, "cache_hit_rate": hit}
}

func demoKeyView(k demoKey, i int) map[string]any {
	groupID := any(nil)
	if k.GroupID != nil {
		groupID = *k.GroupID
	}
	notes := k.Notes
	if notes == "" {
		notes = "demo key · 2 小时后重置"
	}
	return map[string]any{
		"key":            k.Key,
		"owner":          k.Owner,
		"created_at":     k.CreatedAt,
		"revoked_at":     nil,
		"daily_budget":   k.DailyBudget,
		"allowed_models": "*",
		"notes":          notes,
		"group_id":       groupID,
		"group_name":     demoGroupName(k.GroupID, i),
		"creator":        "demo",
		"today_cost":     42.5 + float64(i)*18.7,
		"today_requests": 82000 + i*17311,
	}
}

func demoGroupName(groupID *int64, i int) string {
	if groupID == nil {
		return "默认透传组"
	}
	switch *groupID {
	case 1:
		return "default"
	case 2:
		return "codex-openai"
	case 3:
		return "claude-code"
	default:
		return fmt.Sprintf("#%d", *groupID)
	}
}

func demoProvidersView() []map[string]any {
	now := time.Now().Add(-2 * time.Hour).UnixMilli()
	return []map[string]any{
		{"id": int64(1), "name": "openai-official", "base_url": "https://api.openai.com", "format": "openai", "enabled": true, "has_key": true, "masked_key": "sk-...demo", "request_overrides": "", "request_overrides_is_default": true, "created_at": now},
		{"id": int64(2), "name": "anthropic-official", "base_url": "https://api.anthropic.com", "format": "anthropic", "enabled": true, "has_key": true, "masked_key": "sk-ant-...demo", "request_overrides": "", "request_overrides_is_default": true, "created_at": now},
		{"id": int64(3), "name": "deepseek-hybrid", "base_url": "https://api.deepseek.com", "openai_base_url": "https://api.deepseek.com", "anthropic_base_url": "https://api.deepseek.com/anthropic", "format": "hybrid", "enabled": true, "has_key": true, "masked_key": "sk-...demo", "request_overrides": "", "request_overrides_is_default": true, "created_at": now},
	}
}

func demoProviderModels(pid int64) []map[string]any {
	models := map[int64][]string{1: {"gpt-5.5", "gpt-5.5-openai-compact"}, 2: {"claude-opus-4.8", "claude-sonnet-4.6"}, 3: {"deepseek-reasoner", "glm-5.2-pro"}}
	var out []map[string]any
	for _, m := range models[pid] {
		out = append(out, map[string]any{
			"provider_id": pid, "upstream_id": m, "first_seen_at": time.Now().Add(-24 * time.Hour), "last_seen_at": time.Now(),
			"effective_price":        map[string]any{"input": 5, "output": 30, "cache_read": .5, "cache_write_5m": 5, "cache_write_1h": 5},
			"effective_price_source": "override",
		})
	}
	return out
}

func demoGroupsView() []map[string]any {
	return []map[string]any{
		{"id": int64(1), "name": "default", "notes": "默认透传组", "passthrough_provider_id": int64(3), "created_at": time.Now().Add(-24 * time.Hour).UnixMilli()},
		{"id": int64(2), "name": "codex-openai", "notes": "Codex Responses", "passthrough_provider_id": int64(1), "created_at": time.Now().Add(-20 * time.Hour).UnixMilli()},
		{"id": int64(3), "name": "claude-code", "notes": "Claude Code Messages", "passthrough_provider_id": int64(2), "created_at": time.Now().Add(-18 * time.Hour).UnixMilli()},
	}
}

func demoGroupMappings(gid int64) []map[string]any {
	return []map[string]any{
		{"id": int64(1), "group_id": gid, "logical_name": "gpt-5.5", "provider_id": int64(1), "upstream_id": "gpt-5.5", "priority": 0, "is_primary": true},
		{"id": int64(2), "group_id": gid, "logical_name": "opus", "provider_id": int64(2), "upstream_id": "claude-opus-4.8", "priority": 0, "is_primary": true},
		{"id": int64(3), "group_id": gid, "logical_name": "reasoner", "provider_id": int64(3), "upstream_id": "deepseek-reasoner", "priority": 10, "is_primary": false},
	}
}

func demoLogRows(keys []demoKey) []map[string]any {
	now := time.Now()
	key := func(i int) string {
		if len(keys) == 0 {
			return "sk-demo-********"
		}
		return keys[i%len(keys)].Key
	}
	return []map[string]any{
		demoLog(1020, now.Add(-2*time.Minute), key(0), 200, "gpt-5.5", "openai-official", "/v1/responses", 1842110, 18442, 1760000, 42.81, 812, true, 0, "completed"),
		demoLog(1019, now.Add(-7*time.Minute), key(1), 200, "claude-opus-4.8", "anthropic-official", "/v1/messages", 920554, 12901, 840000, 28.19, 1240, true, 0, "end_turn"),
		demoLog(1018, now.Add(-11*time.Minute), key(0), 400, "gpt-5.5", "openai-official", "/v1/chat/completions", 0, 0, 0, 0, 31, false, 0, "invalid_request_error"),
		demoLog(1017, now.Add(-18*time.Minute), key(2), 200, "deepseek-reasoner", "deepseek-hybrid", "/v1/messages", 112903, 8440, 69000, 1.92, 650, true, 0, "stop"),
		demoLog(1016, now.Add(-25*time.Minute), key(0), 200, "gpt-5.5", "openai-official", "/v1/responses/compact", 2728810, 6120, 2709000, 18.74, 533, true, 0, "completed"),
		demoLog(1015, now.Add(-33*time.Minute), key(1), 200, "claude-sonnet-4.6", "anthropic-official", "/v1/messages", 486420, 15532, 423500, 7.83, 902, true, 0, "end_turn"),
		demoLog(1014, now.Add(-41*time.Minute), key(3), 200, "glm-5.2-pro", "deepseek-hybrid", "/v1/chat/completions", 76220, 3900, 18200, 0.42, 388, false, 0, "stop"),
		demoLog(1013, now.Add(-54*time.Minute), key(0), 200, "gpt-5.5", "openai-official", "/v1/responses", 910240, 24011, 280000, 31.66, 1410, true, 2, "completed"),
		demoLog(1012, now.Add(-68*time.Minute), key(1), 529, "claude-opus-4.8", "anthropic-official", "/v1/messages", 0, 0, 0, 0, 92, false, 0, "overloaded_error"),
		demoLog(1011, now.Add(-84*time.Minute), key(2), 200, "deepseek-reasoner", "deepseek-hybrid", "/v1/messages", 238884, 11020, 214000, 2.56, 1176, true, 0, "stop"),
		demoLog(1010, now.Add(-102*time.Minute), key(0), 200, "gpt-5.5", "openai-official", "/v1/responses", 145221, 1980, 141000, 1.08, 310, false, 0, "completed"),
		demoLog(1009, now.Add(-126*time.Minute), key(1), 400, "claude-opus-4.8", "anthropic-official", "/v1/messages", 0, 0, 0, 0, 28, false, 0, "invalid_request_error"),
		demoLog(1008, now.Add(-3*time.Hour), key(0), 200, "gpt-5.5", "openai-official", "/v1/responses", 3891200, 32010, 3720000, 59.21, 1688, true, 0, "completed"),
		demoLog(1007, now.Add(-5*time.Hour), key(3), 200, "glm-5.2-pro", "deepseek-hybrid", "/v1/chat/completions", 182450, 7400, 51200, 1.14, 745, true, 0, "stop"),
		demoLog(1006, now.Add(-8*time.Hour), key(1), 200, "claude-haiku-4.5", "anthropic-official", "/v1/messages", 64020, 2100, 58000, 0.23, 260, false, 0, "end_turn"),
		demoLog(1005, now.Add(-11*time.Hour), key(0), 503, "gpt-5.5", "openai-official", "/v1/responses", 0, 0, 0, 0, 65, false, 0, "no_available_channel"),
		demoLog(1004, now.Add(-15*time.Hour), key(2), 200, "deepseek-reasoner", "deepseek-hybrid", "/v1/messages", 321900, 15660, 90220, 4.11, 1532, true, 0, "stop"),
		demoLog(1003, now.Add(-19*time.Hour), key(0), 200, "gpt-5.5", "openai-official", "/v1/responses/compact", 1211000, 3900, 1206000, 5.64, 480, true, 0, "completed"),
		demoLog(1002, now.Add(-22*time.Hour), key(1), 200, "claude-opus-4.8", "anthropic-official", "/v1/messages", 780340, 18220, 751000, 19.07, 1321, true, 0, "end_turn"),
		demoLog(1001, now.Add(-27*time.Hour), key(0), 200, "gpt-5.5", "openai-official", "/v1/responses", 98000, 1240, 0, 8.62, 427, false, 0, "completed"),
	}
}

func demoLog(id int64, ts time.Time, proxyKey string, status int, model, provider, endpoint string, in, out, cache int, cost float64, latency int, streaming bool, webSearch int, stopReason string) map[string]any {
	return map[string]any{
		"id": id, "time": ts.Format(time.RFC3339), "proxy_key": proxyKey, "request_id": fmt.Sprintf("demo_%d", id), "http_request_id": fmt.Sprintf("req_demo_%d", id),
		"endpoint": endpoint, "method": "POST", "status": status, "model": model, "provider": provider, "upstream_model": model,
		"input_tokens": in, "output_tokens": out, "cache_create_5m": in / 20, "cache_create_1h": in / 80, "cache_read": cache,
		"web_search_count": webSearch, "cost_usd": cost, "latency_ms": latency, "streaming": streaming, "stop_reason": stopReason, "client_ip": "demo", "user_agent": demoUserAgent(endpoint),
	}
}

func filterDemoLogs(r *http.Request, rows []map[string]any) ([]map[string]any, int, int, int, int, bool) {
	q := r.URL.Query()
	proxyKey := q.Get("proxy_key")
	model := q.Get("model")
	status := q.Get("status")
	beforeID, _ := strconv.ParseInt(q.Get("before_id"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if beforeID > 0 && row["id"].(int64) >= beforeID {
			continue
		}
		if proxyKey != "" && row["proxy_key"] != proxyKey {
			continue
		}
		if model != "" && row["model"] != model {
			continue
		}
		code := row["status"].(int)
		if status == "success" && (code < 200 || code >= 300) {
			continue
		}
		if status == "error" && code >= 200 && code < 300 {
			continue
		}
		out = append(out, row)
	}
	total := len(out)
	totalPages := 0
	if total > 0 {
		totalPages = (total + limit - 1) / limit
	}
	if beforeID > 0 {
		if len(out) > limit {
			out = out[:limit]
			return out, page, limit, total, totalPages, true
		}
		return out, page, limit, total, totalPages, false
	}
	start := (page - 1) * limit
	if start >= len(out) {
		return []map[string]any{}, page, limit, total, totalPages, false
	}
	end := start + limit
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], page, limit, total, totalPages, page < totalPages
}

func demoUserAgent(endpoint string) string {
	switch endpoint {
	case "/v1/messages":
		return "claude-code/1.0 demo"
	case "/v1/responses", "/v1/responses/compact":
		return "codex-cli/0.1 demo"
	default:
		return "demo-browser"
	}
}

func demoErrorRequestBody(row map[string]any) string {
	model, _ := row["model"].(string)
	endpoint, _ := row["endpoint"].(string)
	return fmt.Sprintf(`{"model":%q,"endpoint":%q,"messages":"[demo redacted]","stream":false}`, model, endpoint)
}

func demoErrorResponseBody(row map[string]any) string {
	status := row["status"].(int)
	reason, _ := row["stop_reason"].(string)
	return fmt.Sprintf(`{"error":{"type":%q,"message":"demo error sample","status":%d}}`, reason, status)
}

func writeDemoJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		return 0
	}
}

func floatFromAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	default:
		return 0
	}
}

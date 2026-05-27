// proxy.go: 反向代理装配 + 转发 handler（/v1/messages 等）+ /v1/models 拦截
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

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

// extractProxyKey 兼容两种鉴权 header：
//   - x-api-key（Anthropic 官方 SDK 用）
//   - Authorization: Bearer <key>（OpenAI 风格，cc-switch 之类的工具会用这种来测连通性）
// 注意：Director 阶段会把这两个 header 都删掉再换成真实 key，所以这里只管接收。
func extractProxyKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	if auth := r.Header.Get("authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
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

// buildReverseProxy 装配真正打到 Anthropic 的反向代理：
//   - Director：换 x-api-key 为真实 key，重置 host / accept-encoding
//   - ModifyResponse：tee 响应 body 给后台 usage 解析
func buildReverseProxy(cfg *Config) *httputil.ReverseProxy {
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
		r.Header.Set("x-api-key", cfg.GetRealKey())

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

	return rp
}

// modelsListHandler 拦截 /v1/models：让客户端 SDK 看到的列表 =
// curated.enabled ∩ key.AllowedModels。不然客户端列出全部模型，调用时却被
// allowlist 拒，体验割裂。注册表为空时回落到上游透传（fail-open）。
func modelsListHandler(rp *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		proxyKey := extractProxyKey(r)
		keyMeta, ok := keys.Get(proxyKey)
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy key")
			return
		}
		body := modelRegistry.FilteredList(func(id string) bool {
			return keyMeta.ModelAllowed(id)
		})
		if body == nil {
			// 缓存还没起来，让请求走原始反向代理（catch-all 会处理）
			rp.ServeHTTP(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(body)
	}
}

// modelDetailHandler 处理 /v1/models/{id}：allowlist 通不过的模型直接 404，否则透传。
func modelDetailHandler(rp *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proxyKey := extractProxyKey(r)
		keyMeta, ok := keys.Get(proxyKey)
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy key")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
		if id == "" {
			rp.ServeHTTP(w, r)
			return
		}
		if _, ok := modelRegistry.ResolveAlias(id); !ok || !keyMeta.ModelAllowed(id) {
			writeAnthropicError(w, http.StatusNotFound, "not_found_error",
				fmt.Sprintf("model not available via this proxy: %s", id))
			return
		}
		rp.ServeHTTP(w, r)
	}
}

// forwardHandler 是 catch-all 入口：校验 key、检查预算、peek 请求 body 做模型/fast 拦截，
// 一切 OK 才把请求交给反向代理。
func forwardHandler(cfg *Config, rp *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 上游 key 缺失（被 admin 清掉或启动时就没配）就直接拒绝，省一次上游 401。
		// Director 会注入空 key，请求必然失败；这里早返回更明确。
		if cfg.GetRealKey() == "" {
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error",
				"upstream API key not configured on proxy")
			return
		}

		proxyKey := extractProxyKey(r)
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
						if _, ok := modelRegistry.ResolveAlias(peek.Model); !ok {
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
	}
}

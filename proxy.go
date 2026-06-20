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

// routeKey 把本次请求解析出的 RouteTarget 通过 context 传给 Director，
// 让反向代理按目标服务商动态设置 host / key。
type routeKey struct{}

func withRoute(ctx context.Context, rt *RouteTarget) context.Context {
	return context.WithValue(ctx, routeKey{}, rt)
}
func routeFrom(ctx context.Context) *RouteTarget {
	v, _ := ctx.Value(routeKey{}).(*RouteTarget)
	return v
}

// rewriteTopLevelModel 把请求体顶层 "model" 字段的值原地替换成 newModel，
// 不整体 re-marshal——保留其余字节布局与键序，维持 prompt cache 命中资格。
//
// 深度感知：只替换 JSON 顶层对象（depth==1）里作为 key 的 "model"，
// 忽略嵌套对象（如 tools 的 input_schema 里恰好叫 model 的属性）。
// 找不到顶层 model 时原样返回（false）。
func rewriteTopLevelModel(raw []byte, newModel string) ([]byte, bool) {
	depth := 0
	inStr := false
	esc := false
	expectKey := false // 仅在 depth==1 的对象内有意义

	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			// depth==1 且期待 key 时，尝试匹配 "model"
			if depth == 1 && expectKey && matchModelKey(raw, i) {
				return spliceModelValue(raw, i, newModel)
			}
			inStr = true
		case '{':
			depth++
			if depth == 1 {
				expectKey = true
			}
		case '[':
			depth++
		case '}':
			depth--
		case ']':
			depth--
		case ':':
			if depth == 1 {
				expectKey = false
			}
		case ',':
			if depth == 1 {
				expectKey = true
			}
		}
	}
	return raw, false
}

// matchModelKey 判断 raw[i] 起的字符串是否恰好是 "model"。
func matchModelKey(raw []byte, i int) bool {
	const key = `"model"`
	if i+len(key) > len(raw) {
		return false
	}
	return string(raw[i:i+len(key)]) == key
}

// spliceModelValue 给定 "model" key 起始下标 i，定位其字符串值并替换为 newModel。
func spliceModelValue(raw []byte, i int, newModel string) ([]byte, bool) {
	j := i + len(`"model"`)
	// 跳过空白
	for j < len(raw) && isJSONSpace(raw[j]) {
		j++
	}
	if j >= len(raw) || raw[j] != ':' {
		return raw, false
	}
	j++
	for j < len(raw) && isJSONSpace(raw[j]) {
		j++
	}
	if j >= len(raw) || raw[j] != '"' {
		return raw, false
	}
	valStart := j + 1
	k := valStart
	for k < len(raw) {
		if raw[k] == '\\' {
			k += 2
			continue
		}
		if raw[k] == '"' {
			break
		}
		k++
	}
	if k >= len(raw) {
		return raw, false
	}
	// JSON-转义 newModel（上游真实模型名一般是 ASCII，但管理员可能配出含引号/反斜杠的
	// upstream_id；不转义会把请求体破坏成非法 JSON）。Marshal 后去掉首尾引号，
	// 因为我们是替换已有引号之间的内容。
	enc, err := json.Marshal(newModel)
	if err != nil {
		return raw, false
	}
	escaped := enc[1 : len(enc)-1] // 去掉 Marshal 加的首尾 "
	out := make([]byte, 0, len(raw)-(k-valStart)+len(escaped))
	out = append(out, raw[:valStart]...)
	out = append(out, escaped...)
	out = append(out, raw[k:]...)
	return out, true
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
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

// buildReverseProxy 装配反向代理。单上游已改为多上游：实际 target / key
// 由每次请求 context 里的 RouteTarget 决定（Director 读取），这里的初始 target
// 只是 httputil 需要的占位。
//   - Director：按 RouteTarget 设 host / x-api-key，重置 accept-encoding
//   - ModifyResponse：tee 响应 body 给后台 usage 解析
func buildReverseProxy(cfg *Config) *httputil.ReverseProxy {
	placeholder, _ := url.Parse(upstreamURL)
	rp := httputil.NewSingleHostReverseProxy(placeholder)
	rp.FlushInterval = -1

	rp.Director = func(r *http.Request) {
		rt := routeFrom(r.Context())
		// 无路由是 bug（forwardHandler 保证总会附上 RouteTarget）。
		// fail closed：标记成无效目标，让 ErrorHandler 返回 502，
		// 绝不回落到主 .env key（否则受限 key 会越权用主 key）。
		if rt == nil || rt.Provider == nil {
			r.URL.Scheme = "http"
			r.URL.Host = "route.invalid"
			r.Host = "route.invalid"
			r.Header.Del("x-api-key")
			r.Header.Del("authorization")
			return
		}
		base := rt.Provider.BaseURL
		realKey := rt.Provider.APIKey
		target, err := url.Parse(base)
		if err != nil {
			target = placeholder
		}

		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		// base_url 可能带路径前缀（如 https://host/api），拼到请求路径前
		if target.Path != "" && target.Path != "/" {
			r.URL.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
		}
		r.Host = target.Host
		r.Header.Set("accept-encoding", "identity")

		r.Header.Del("x-api-key")
		r.Header.Del("authorization")
		r.Header.Set("x-api-key", realKey)

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
			Time:          time.Now(),
			ProxyKey:      resp.Request.Header.Get("x-proxy-key-original"),
			HTTPReqID:     resp.Header.Get("request-id"),
			Endpoint:      resp.Request.URL.Path,
			Method:        resp.Request.Method,
			Status:        resp.StatusCode,
			Model:         resp.Request.Header.Get("x-proxy-req-model"), // 兜底：错误响应也有归属
			Provider:      resp.Request.Header.Get("x-proxy-provider"),
			UpstreamModel: resp.Request.Header.Get("x-proxy-upstream-model"),
			ClientIP:      resp.Request.Header.Get("x-proxy-client-ip"),
			UserAgent:     resp.Request.Header.Get("x-proxy-user-agent"),
			LatencyMs:     latency,
			Streaming:     strings.HasPrefix(resp.Header.Get("content-type"), "text/event-stream"),
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

// modelsListHandler 拦截 /v1/models。
//   - key 绑定了小组：列出该组的逻辑名（下游能用什么，这里就列什么）。
//   - 未绑小组（旧 key）：回落到 curated.enabled ∩ allowed_models 的旧逻辑。
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

		// 绑定小组 → 列逻辑名
		if keyMeta.GroupID != nil {
			names := providerRegistry.LogicalNames(*keyMeta.GroupID)
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write(anthropicModelsListFromNames(names))
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

// anthropicModelsListFromNames 用逻辑名拼一个 Anthropic /v1/models 响应。
func anthropicModelsListFromNames(names []string) []byte {
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{
			"type":         "model",
			"id":           n,
			"display_name": n,
		})
	}
	var first, last string
	if len(names) > 0 {
		first = names[0]
		last = names[len(names)-1]
	}
	out, _ := json.Marshal(map[string]any{
		"data": data, "has_more": false, "first_id": first, "last_id": last,
	})
	return out
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
		// 绑定小组：逻辑名或具体名能路由即视为存在。
		if keyMeta.GroupID != nil {
			if _, err := ResolveRoute(keyMeta, id); err != nil {
				writeAnthropicError(w, http.StatusNotFound, "not_found_error",
					fmt.Sprintf("model not available via this proxy: %s", id))
				return
			}
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"type":"model","id":%q,"display_name":%q}`, id, id)))
			return
		}
		// ResolveAlias 解析短名到完整 ID，用完整 ID 查白名单（兼容短名白名单场景）。
		canonical, ok := modelRegistry.ResolveAlias(id)
		if !ok || !keyMeta.ModelAllowed(canonical) {
			writeAnthropicError(w, http.StatusNotFound, "not_found_error",
				fmt.Sprintf("model not available via this proxy: %s", id))
			return
		}
		rp.ServeHTTP(w, r)
	}
}

// forwardHandler 是 catch-all 入口：校验 key、检查预算、peek 请求 body，
// 用 ResolveRoute 解析目标上游，再按 provider 格式分发：
//   - anthropic：改写 model→真实名，走 ReverseProxy 快路径（字节级透传保缓存）
//   - openai：交给 dispatchToOpenAI 做出站翻译
func forwardHandler(cfg *Config, rp *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		// 提前从请求 body 取 model + speed + tools：
		//   1) 注入模型归属（错误响应也能归类）
		//   2) 拦截 speed=fast（6× 计价）
		//   3) 记录声明的服务端工具
		//   4) 用 ResolveRoute 解析目标上游
		//
		// 必须读完整个 body 再放回去，否则截断会破坏上游请求。50MB 上限防 DoS。
		const maxBody = 50 << 20
		if r.Body != nil && r.Method == http.MethodPost {
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
			if err == nil {
				var peek struct {
					Model string `json:"model"`
					Speed string `json:"speed"`
					Tools []struct {
						Type string `json:"type"`
						Name string `json:"name"`
					} `json:"tools"`
				}
				_ = json.Unmarshal(raw, &peek)

				if peek.Speed == "fast" {
					log.Printf("reject fast mode: key=%s model=%s", proxyKey, peek.Model)
					writeAnthropicError(w, http.StatusForbidden, "permission_error",
						"fast mode is disabled on this proxy (6x billing protection)")
					go writeRecord(&UsageRecord{
						Time: time.Now(), ProxyKey: proxyKey, Endpoint: r.URL.Path, Method: r.Method,
						Status: http.StatusForbidden, Model: peek.Model, ClientIP: clientIP(r),
						UserAgent: r.Header.Get("user-agent"), StopReason: "proxy_rejected_fast",
					}, raw, nil)
					return
				}
				if serverTools := pickServerTools(peek.Tools); len(serverTools) > 0 {
					log.Printf("server tools declared: key=%s model=%s tools=%v",
						proxyKey, peek.Model, serverTools)
				}

				// 只有带 model 的请求需要路由解析（/v1/messages 等）。
				// 不带 model 的（如某些 GET 透传）直接走默认路由。
				if peek.Model != "" {
					r.Header.Set("x-proxy-req-model", peek.Model)
					rt, rerr := ResolveRoute(keyMeta, peek.Model)
					if rerr != nil {
						status, errType, stop := classifyRouteError(rerr)
						log.Printf("route resolve failed: key=%s model=%s: %v", proxyKey, peek.Model, rerr)
						writeAnthropicError(w, status, errType, rerr.Error())
						go writeRecord(&UsageRecord{
							Time: time.Now(), ProxyKey: proxyKey, Endpoint: r.URL.Path, Method: r.Method,
							Status: status, Model: peek.Model, ClientIP: clientIP(r),
							UserAgent: r.Header.Get("user-agent"), StopReason: stop,
						}, raw, nil)
						return
					}

					// 归因 header（ModifyResponse / dispatch 写 usage 用）
					r.Header.Set("x-proxy-provider", rt.Provider.Name)
					r.Header.Set("x-proxy-upstream-model", rt.UpstreamID)

					// 把请求体 model 改写成上游真实名（逻辑名→具体名）。
					// 同名时 rewrite 不动字节，保 prompt cache。
					if rt.UpstreamID != peek.Model {
						if rewritten, ok := rewriteTopLevelModel(raw, rt.UpstreamID); ok {
							raw = rewritten
						}
					}

					// OpenAI 格式上游：走出站翻译路径，不经 ReverseProxy。
					if rt.Provider.Format == "openai" {
						dispatchAnthropicToOpenAI(w, r, rt, raw, &UsageRecord{
							Time: time.Now(), ProxyKey: proxyKey, Endpoint: r.URL.Path, Method: r.Method,
							Model: peek.Model, Provider: rt.Provider.Name, UpstreamModel: rt.UpstreamID,
							ClientIP: clientIP(r), UserAgent: r.Header.Get("user-agent"),
						})
						return
					}

					// Anthropic 格式上游：装回 body + RouteTarget，走 ReverseProxy。
					r.Body = io.NopCloser(bytes.NewReader(raw))
					r.ContentLength = int64(len(raw))
					r = r.WithContext(withReqBody(r.Context(), raw))
					r = r.WithContext(withRoute(r.Context(), rt))
					rp.ServeHTTP(w, r)
					return
				}

				// 无 model 字段（如 count_tokens 之外的少数透传、畸形 body）：
				// 仍必须解析出一个上游目标，绝不能让 Director 回落到主 key。
				// 用默认服务商（旧 key 行为）；解析不出就 fail closed。
				rt, rerr := defaultRoute(keyMeta)
				if rerr != nil {
					status, errType, stop := classifyRouteError(rerr)
					writeAnthropicError(w, status, errType, rerr.Error())
					go writeRecord(&UsageRecord{
						Time: time.Now(), ProxyKey: proxyKey, Endpoint: r.URL.Path, Method: r.Method,
						Status: status, ClientIP: clientIP(r),
						UserAgent: r.Header.Get("user-agent"), StopReason: stop,
					}, raw, nil)
					return
				}
				r.Header.Set("x-proxy-provider", rt.Provider.Name)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				r.ContentLength = int64(len(raw))
				r = r.WithContext(withReqBody(r.Context(), raw))
				r = r.WithContext(withRoute(r.Context(), rt))
			}
		}

		rp.ServeHTTP(w, r)
	}
}

// classifyRouteError 把 resolveError 映射成 (HTTP 状态, 错误类型, stop_reason)。
func classifyRouteError(err error) (int, string, string) {
	if re, ok := err.(*resolveError); ok {
		switch {
		case re.notFound:
			return http.StatusNotFound, "not_found_error", "proxy_rejected"
		case re.forbidden:
			return http.StatusForbidden, "permission_error", "proxy_rejected_allowlist"
		}
	}
	return http.StatusServiceUnavailable, "api_error", "proxy_rejected_route"
}

// defaultRoute 给"无 model 字段"的请求挑一个上游目标。
//   - 绑组 key：取组内任一映射的服务商（避免漏到主 key）；组空则配置错误。
//   - 旧 key：默认服务商。
func defaultRoute(km *KeyMeta) (*RouteTarget, error) {
	if km.GroupID != nil {
		if p, ok := providerRegistry.GroupAnyProvider(*km.GroupID); ok {
			return &RouteTarget{Provider: p, LogicalName: ""}, nil
		}
		return nil, configErr("group has no usable mapping")
	}
	p, ok := defaultProvider()
	if !ok {
		return nil, configErr("no provider configured")
	}
	return &RouteTarget{Provider: p, LogicalName: ""}, nil
}

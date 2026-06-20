// compat.go: OpenAI Chat Completions 兼容层
//
// 在同端口 :8787 接收 OpenAI 格式请求（/v1/chat/completions），
// 转成 Anthropic Messages 格式发上游，再把响应转回 OpenAI 格式。
//
// 不走 httputil.ReverseProxy（需要双向 body 转换），直接 http.Client 调上游。
// 鉴权、预算、模型校验复用现有 extractProxyKey / KeyCache / ModelRegistry。
// 用量追踪复用 parseUsageJSON / parseUsageSSE + writeRecord 管线。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

var compatClient = &http.Client{Timeout: 5 * time.Minute}

// ────────────────────── 错误输出 ──────────────────────

func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(oaiErrorResponse{
		Error: oaiErrorBody{
			Message: message,
			Type:    errType,
		},
	})
}

// ────────────────────── POST /v1/chat/completions ──────────────────────

func openaiChatHandler(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "only POST is allowed")
			return
		}
		if cfg.GetRealKey() == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable, "api_error",
				"upstream API key not configured on proxy")
			return
		}

		startTime := time.Now()

		// ── 鉴权 ──
		proxyKey := extractProxyKey(r)
		keyMeta, ok := keys.Get(proxyKey)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
			return
		}

		cip := clientIP(r)
		ua := r.Header.Get("user-agent")

		// ── 预算检查 ──
		if keyMeta.DailyBudget > 0 {
			spent, err := store.TodaysCost(proxyKey)
			if err != nil {
				log.Printf("compat: budget query error: %v", err)
			} else if spent >= keyMeta.DailyBudget {
				writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error",
					fmt.Sprintf("daily budget exceeded: $%.2f / $%.2f", spent, keyMeta.DailyBudget))
				log.Printf("compat: budget exceeded: key=%s owner=%s spent=$%.4f budget=$%.2f",
					proxyKey, keyMeta.Owner, spent, keyMeta.DailyBudget)
				go writeRecord(&UsageRecord{
					Time: startTime, ProxyKey: proxyKey, Endpoint: "/v1/chat/completions",
					Method: "POST", Status: 429, Model: "", ClientIP: cip, UserAgent: ua,
					LatencyMs: time.Since(startTime).Milliseconds(), StopReason: "proxy_rejected_budget",
				}, nil, nil)
				return
			}
		}

		// ── 读取 + 解析请求体 ──
		const maxBody = 50 << 20
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}

		var oaiReq oaiChatRequest
		if err := json.Unmarshal(raw, &oaiReq); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
			return
		}

		// ── 模型校验 ──
		if oaiReq.Model == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
			return
		}
		if _, ok := modelRegistry.ResolveAlias(oaiReq.Model); !ok {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error",
				"model not available via this proxy: "+oaiReq.Model)
			go writeRecord(&UsageRecord{
				Time: startTime, ProxyKey: proxyKey, Endpoint: "/v1/chat/completions",
				Method: "POST", Status: 404, Model: oaiReq.Model, ClientIP: cip, UserAgent: ua,
				LatencyMs: time.Since(startTime).Milliseconds(), StopReason: "proxy_rejected",
			}, raw, nil)
			return
		}
		if !keyMeta.ModelAllowed(oaiReq.Model) {
			writeOpenAIError(w, http.StatusForbidden, "permission_error",
				"this proxy key is not permitted to use model: "+oaiReq.Model)
			go writeRecord(&UsageRecord{
				Time: startTime, ProxyKey: proxyKey, Endpoint: "/v1/chat/completions",
				Method: "POST", Status: 403, Model: oaiReq.Model, ClientIP: cip, UserAgent: ua,
				LatencyMs: time.Since(startTime).Milliseconds(), StopReason: "proxy_rejected_allowlist",
			}, raw, nil)
			return
		}

		// ── 转换请求 ──
		antReq, err := convertRequest(&oaiReq)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		// ── 调上游 ──
		antBody, _ := json.Marshal(antReq)
		upResp, err := callAnthropic(cfg, antBody)
		if err != nil {
			log.Printf("compat: upstream error: %v", err)
			writeOpenAIError(w, http.StatusBadGateway, "server_error", "upstream request failed")
			return
		}
		defer upResp.Body.Close()

		// 构造基础 UsageRecord
		rec := &UsageRecord{
			Time:      startTime,
			ProxyKey:  proxyKey,
			HTTPReqID: upResp.Header.Get("request-id"),
			Endpoint:  "/v1/chat/completions",
			Method:    "POST",
			Status:    upResp.StatusCode,
			Model:     oaiReq.Model,
			ClientIP:  cip,
			UserAgent: ua,
			Streaming: oaiReq.Stream,
		}

		if oaiReq.Stream && upResp.StatusCode == 200 {
			relayStream(w, r, upResp, rec, raw)
		} else {
			relayNonStream(w, upResp, rec, raw)
		}
	}
}

// ── 上游 HTTP 调用 ──

func callAnthropic(cfg *Config, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", upstreamURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", cfg.GetRealKey())
	req.Header.Set("anthropic-version", "2023-06-01")
	if cfg.HideCC {
		req.Header.Set("user-agent", "claude-proxy/0.2")
	}
	return compatClient.Do(req)
}

// ── 非流式响应处理 ──

func relayNonStream(w http.ResponseWriter, upResp *http.Response, rec *UsageRecord, reqBody []byte) {
	respBody, err := io.ReadAll(io.LimitReader(upResp.Body, 50<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "server_error", "failed to read upstream response")
		return
	}

	rec.LatencyMs = time.Since(rec.Time).Milliseconds()

	// 非 2xx → 转换错误格式后透传状态码
	if upResp.StatusCode < 200 || upResp.StatusCode >= 300 {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(upResp.StatusCode)
		_, _ = w.Write(convertAnthropicError(respBody))
		parseUsageJSON(respBody, rec)
		go writeRecord(rec, reqBody, respBody)
		return
	}

	// 解析用量（复用现有管线）
	parseUsageJSON(respBody, rec)

	// 转换为 OpenAI 格式
	oaiResp, err := convertResponse(respBody)
	if err != nil {
		log.Printf("compat: response conversion failed: %v", err)
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "response conversion failed")
		go writeRecord(rec, reqBody, respBody)
		return
	}

	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(oaiResp)
	go writeRecord(rec, reqBody, respBody)
}

// ── 流式 SSE 中继 ──

func relayStream(w http.ResponseWriter, r *http.Request, upResp *http.Response, rec *UsageRecord, reqBody []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(200)

	st := &streamState{created: time.Now().Unix()}
	var teeBuf bytes.Buffer
	const maxTee = 4 << 20

	scanner := bufio.NewScanner(upResp.Body)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)

	var eventType string
	ctx := r.Context()
	for scanner.Scan() {
		// 客户端断开 → 停止中继，避免对已关闭的连接写入
		if err := ctx.Err(); err != nil {
			break
		}

		line := scanner.Text()

		// tee 给 usage 解析用（限 4MB）
		if teeBuf.Len() < maxTee {
			teeBuf.WriteString(line)
			teeBuf.WriteByte('\n')
		}

		// 解析 SSE 格式：event: xxx\ndata: xxx\n\n
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			if line == "" {
				eventType = ""
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}

		chunks := convertSSEEvent(eventType, []byte(payload), st)
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}

	// scanner.Err 非空表示上游中断/读失败；客户端断开时 ctx.Err() 非 nil。
	// 两种情况都说明流不完整，记录里标记一下，但仍尝试用已收到的数据解析 usage。
	streamBroken := scanner.Err() != nil || ctx.Err() != nil

	// 只有正常结束才发 [DONE]；中断时不发，让客户端感知异常
	if !streamBroken {
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	// 用量入库
	rec.LatencyMs = time.Since(rec.Time).Milliseconds()
	parseUsageSSE(teeBuf.Bytes(), rec)
	if streamBroken && rec.StopReason == "" {
		rec.StopReason = "proxy_stream_interrupted"
	}
	go writeRecord(rec, reqBody, teeBuf.Bytes())
}

// ────────────────────── GET /v1/models（OpenAI 格式） ──────────────────────

func openaiModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "only GET is allowed")
			return
		}

		proxyKey := extractProxyKey(r)
		keyMeta, ok := keys.Get(proxyKey)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
			return
		}

		allowed := func(id string) bool { return keyMeta.ModelAllowed(id) }
		ids := modelRegistry.FilteredModelIDs(allowed)

		// registry 未加载（fail-open）→ 直接问上游，再按 key 白名单过滤后转 OpenAI 格式
		if ids == nil {
			ids = fetchUpstreamModelIDs(allowed)
		}

		now := time.Now().Unix()
		entries := make([]oaiModelEntry, len(ids))
		for i, id := range ids {
			entries[i] = oaiModelEntry{
				ID:      id,
				Object:  "model",
				Created: now,
				OwnedBy: "anthropic",
			}
		}

		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(oaiModelList{
			Object: "list",
			Data:   entries,
		})
	}
}

// fetchUpstreamModelIDs registry 未加载时的回落：直接拉上游 /v1/models，
// 返回通过 allowed 过滤后的模型 ID 列表。失败返回 nil。
func fetchUpstreamModelIDs(allowed func(string) bool) []string {
	resp, err := compatClient.Get(upstreamURL + "/v1/models?limit=100")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}

	var ids []string
	for _, m := range body.Data {
		if m.ID != "" && allowed(m.ID) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

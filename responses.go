package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// openaiResponsesHandler proxies OpenAI Responses requests natively. It is meant
// for Codex clients, which depend on Responses-specific streaming events and the
// compact endpoint. It reuses the existing proxy-key, group routing, budget, and
// usage accounting layers, but does not persist request bodies on errors.
func openaiResponsesHandler(cfg *Config, compact bool) http.HandlerFunc {
	endpoint := "/v1/responses"
	if compact {
		endpoint = "/v1/responses/compact"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "only POST is allowed")
			return
		}

		startTime := time.Now()
		proxyKey := extractProxyKey(r)
		keyMeta, ok := keys.Get(proxyKey)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
			return
		}

		cip := clientIP(r)
		ua := r.Header.Get("user-agent")

		if keyMeta.DailyBudget > 0 {
			spent, err := store.TodaysCost(proxyKey)
			if err != nil {
				log.Printf("responses: budget query error: %v", err)
			} else if spent >= keyMeta.DailyBudget {
				writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error",
					fmt.Sprintf("daily budget exceeded: $%.2f / $%.2f", spent, keyMeta.DailyBudget))
				go writeRecord(&UsageRecord{
					Time: startTime, ProxyKey: proxyKey, Endpoint: endpoint, Method: "POST",
					Status: http.StatusTooManyRequests, ClientIP: cip, UserAgent: ua,
					LatencyMs: time.Since(startTime).Milliseconds(), StopReason: "proxy_rejected_budget",
				}, nil, nil)
				return
			}
		}

		const maxBody = 50 << 20
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}

		var peek struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
			return
		}
		if peek.Model == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
			return
		}
		routeModel := peek.Model
		if compact {
			routeModel = strings.TrimSuffix(routeModel, "-openai-compact")
		}

		rt, rerr := ResolveRoute(keyMeta, routeModel)
		if rerr != nil {
			status, _, stop := classifyRouteError(rerr)
			errType := "server_error"
			switch status {
			case http.StatusNotFound:
				errType = "not_found_error"
			case http.StatusForbidden:
				errType = "permission_error"
			}
			writeOpenAIError(w, status, errType, rerr.Error())
			go writeRecord(&UsageRecord{
				Time: startTime, ProxyKey: proxyKey, Endpoint: endpoint, Method: "POST",
				Status: status, Model: peek.Model, ClientIP: cip, UserAgent: ua,
				LatencyMs: time.Since(startTime).Milliseconds(), StopReason: stop,
			}, nil, nil)
			return
		}
		if !providerSupportsOpenAI(rt.Provider) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Responses API requires an openai or hybrid provider")
			go writeRecord(&UsageRecord{
				Time: startTime, ProxyKey: proxyKey, Endpoint: endpoint, Method: "POST",
				Status: http.StatusBadRequest, Model: peek.Model, Provider: rt.Provider.Name,
				UpstreamModel: rt.UpstreamID, ClientIP: cip, UserAgent: ua,
				LatencyMs: time.Since(startTime).Milliseconds(), StopReason: "proxy_rejected_provider_format",
			}, nil, nil)
			return
		}

		if rt.UpstreamID != peek.Model {
			if rewritten, ok := rewriteTopLevelModel(raw, rt.UpstreamID); ok {
				raw = rewritten
			}
		}
		raw = applyProviderRequestOverridesForProtocol(raw, rt.Provider, protocolOpenAI)

		upstreamPath := "/v1/responses"
		if compact {
			upstreamPath = "/v1/responses/compact"
		}
		upstreamURL := providerOpenAIBaseURL(rt.Provider) + upstreamPath
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(raw))
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to build upstream request")
			return
		}
		copyResponsesRequestHeaders(req.Header, r.Header)
		req.Header.Set("authorization", "Bearer "+rt.Provider.APIKey)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept-encoding", "identity")

		resp, err := openAIResponsesHTTPClient().Do(req)
		if err != nil {
			log.Printf("responses: upstream error: %v", err)
			writeOpenAIError(w, http.StatusBadGateway, "server_error", "upstream request failed")
			go writeRecord(&UsageRecord{
				Time: startTime, ProxyKey: proxyKey, Endpoint: endpoint, Method: "POST",
				Status: http.StatusBadGateway, Model: peek.Model, Provider: rt.Provider.Name,
				UpstreamModel: rt.UpstreamID, ClientIP: cip, UserAgent: ua,
				LatencyMs: time.Since(startTime).Milliseconds(), StopReason: "proxy_upstream_error",
			}, nil, nil)
			return
		}
		defer resp.Body.Close()

		rec := &UsageRecord{
			Time:          startTime,
			ProxyKey:      proxyKey,
			Endpoint:      endpoint,
			Method:        "POST",
			Status:        resp.StatusCode,
			Model:         peek.Model,
			Provider:      rt.Provider.Name,
			UpstreamModel: rt.UpstreamID,
			BillingModel:  rt.UpstreamID,
			ClientIP:      cip,
			UserAgent:     ua,
			Streaming:     strings.HasPrefix(resp.Header.Get("content-type"), "text/event-stream"),
		}

		if rec.Streaming {
			relayResponsesStream(w, r, resp, rec)
			return
		}
		relayResponsesNonStream(w, resp, rec)
	}
}

func openAIResponsesHTTPClient() *http.Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if socksAddr := os.Getenv("CLAUDE_PROXY_UPSTREAM_SOCKS5"); socksAddr != "" {
		if proxyURL, err := url.Parse("socks5://" + socksAddr); err == nil {
			tr.Proxy = http.ProxyURL(proxyURL)
		}
	}
	return &http.Client{Transport: tr}
}

func copyResponsesRequestHeaders(dst, src http.Header) {
	for _, name := range []string{"accept", "openai-organization", "openai-project", "openai-beta", "user-agent"} {
		if v := src.Values(name); len(v) > 0 {
			dst.Del(name)
			for _, item := range v {
				dst.Add(name, item)
			}
		}
	}
}

func relayResponsesNonStream(w http.ResponseWriter, resp *http.Response, rec *UsageRecord) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	rec.LatencyMs = time.Since(rec.Time).Milliseconds()
	parseResponsesUsageJSON(body, rec)

	copyResponsesResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	go writeRecord(rec, nil, body)
}

func relayResponsesStream(w http.ResponseWriter, r *http.Request, resp *http.Response, rec *UsageRecord) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}
	copyResponsesResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	var teeBuf bytes.Buffer
	const maxTee = 4 << 20
	buf := make([]byte, 32<<10)
	streamBroken := false
	for {
		if err := r.Context().Err(); err != nil {
			streamBroken = true
			break
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = w.Write(chunk)
			flusher.Flush()
			if teeBuf.Len() < maxTee {
				remain := maxTee - teeBuf.Len()
				if len(chunk) > remain {
					chunk = chunk[:remain]
				}
				teeBuf.Write(chunk)
			}
		}
		if err != nil {
			if err != io.EOF {
				streamBroken = true
			}
			break
		}
	}

	rec.LatencyMs = time.Since(rec.Time).Milliseconds()
	parseResponsesUsageSSE(teeBuf.Bytes(), rec)
	if streamBroken && rec.StopReason == "" {
		rec.StopReason = "proxy_stream_interrupted"
	}
	go writeRecord(rec, nil, teeBuf.Bytes())
}

func copyResponsesResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"content-type", "cache-control", "request-id", "x-request-id", "openai-processing-ms", "openai-version"} {
		if v := src.Values(name); len(v) > 0 {
			dst.Del(name)
			for _, item := range v {
				dst.Add(name, item)
			}
		}
	}
}

func forceTopLevelStoreFalse(raw []byte) []byte {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return raw
	}
	body["store"] = json.RawMessage("false")
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

type responsesUsage struct {
	InputTokens         int                    `json:"input_tokens"`
	OutputTokens        int                    `json:"output_tokens"`
	TotalTokens         int                    `json:"total_tokens"`
	InputTokensDetails  responsesInputDetails  `json:"input_tokens_details"`
	OutputTokensDetails responsesOutputDetails `json:"output_tokens_details"`
}

type responsesInputDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type responsesOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func (u responsesUsage) toRecord(rec *UsageRecord) {
	if u.InputTokens > 0 {
		rec.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		rec.OutputTokens = u.OutputTokens
	}
	if u.InputTokensDetails.CachedTokens > 0 {
		rec.CacheRead = u.InputTokensDetails.CachedTokens
	}
	if u.InputTokensDetails.CacheWriteTokens > 0 {
		rec.CacheCreate5m = u.InputTokensDetails.CacheWriteTokens
	}
}

func parseResponsesUsageJSON(body []byte, rec *UsageRecord) {
	var r struct {
		ID     string                `json:"id"`
		Model  string                `json:"model"`
		Status string                `json:"status"`
		Usage  responsesUsage        `json:"usage"`
		Output []responsesOutputItem `json:"output"`
		Error  any                   `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return
	}
	if r.ID != "" {
		rec.RequestID = r.ID
	}
	if r.Model != "" {
		rec.UpstreamModel = r.Model
	}
	if r.Status != "" {
		rec.StopReason = r.Status
	}
	r.Usage.toRecord(rec)
	countResponsesWebSearch(r.Output, rec)
}

type responsesOutputItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Action struct {
		Type string `json:"type"`
	} `json:"action"`
}

func countResponsesWebSearch(items []responsesOutputItem, rec *UsageRecord) {
	seen := map[string]bool{}
	for _, it := range items {
		if it.Type != "web_search_call" {
			continue
		}
		if it.Status != "" && it.Status != "completed" {
			continue
		}
		switch it.Action.Type {
		case "", "search", "open_page", "find_in_page":
			id := it.ID
			if id == "" {
				id = fmt.Sprintf("anon-%d", len(seen)+1)
			}
			seen[id] = true
		}
	}
	if len(seen) > rec.WebSearchCount {
		rec.WebSearchCount = len(seen)
	}
}

func parseResponsesUsageSSE(body []byte, rec *UsageRecord) {
	webSearchIDs := map[string]bool{}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				ID     string         `json:"id"`
				Model  string         `json:"model"`
				Status string         `json:"status"`
				Usage  responsesUsage `json:"usage"`
			} `json:"response"`
			Item responsesOutputItem `json:"item"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		if ev.Response.ID != "" {
			rec.RequestID = ev.Response.ID
		}
		if ev.Response.Model != "" {
			rec.UpstreamModel = ev.Response.Model
		}
		if ev.Response.Status != "" {
			rec.StopReason = ev.Response.Status
		}
		ev.Response.Usage.toRecord(rec)
		if ev.Item.Type == "web_search_call" {
			if ev.Item.Status == "" || ev.Item.Status == "completed" {
				switch ev.Item.Action.Type {
				case "", "search", "open_page", "find_in_page":
					id := ev.Item.ID
					if id == "" {
						id = fmt.Sprintf("anon-%d", len(webSearchIDs)+1)
					}
					webSearchIDs[id] = true
				}
			}
		}
	}
	if len(webSearchIDs) > rec.WebSearchCount {
		rec.WebSearchCount = len(webSearchIDs)
	}
}

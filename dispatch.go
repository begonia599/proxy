// dispatch.go: 出站分发——把"规范态 Anthropic 请求 + RouteTarget"发到上游，
// 统一以 Anthropic 格式把响应交还给调用方。
//
// 两个调用方：
//   - proxy.go forwardHandler（下游 Anthropic 客户端）：openai 上游时调
//     dispatchAnthropicToOpenAI，把响应转回 Anthropic 回送下游。
//   - compat.go openaiChatHandler（下游 OpenAI 客户端）：用 callUpstreamAnthropic
//     拿到 Anthropic 规范态响应，再复用现有 relayNonStream/relayStream 转回 OpenAI。
//
// anthropic 格式上游不经此处（走 proxy.go 的 ReverseProxy 快路径）；
// openai 格式上游在此做 Anthropic↔OpenAI 双向翻译。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

var dispatchClient = &http.Client{Timeout: 10 * time.Minute}

// upstreamResult 上游响应的统一视图，body 已是 Anthropic 规范态。
// 非流式：AnthBody 有值。流式：Stream 有值（Anthropic SSE 文本流）。
type upstreamResult struct {
	Status   int
	Stream   io.ReadCloser // Anthropic SSE（流式时）
	AnthBody []byte        // Anthropic JSON（非流式时）
	Err      error
}

// callUpstreamAnthropic 把规范态 Anthropic 请求体发到 rt 指向的上游，
// 返回 Anthropic 规范态结果。无论上游是 anthropic 还是 openai 格式，
// 调用方拿到的都是 Anthropic 格式。
//
// stream 表示下游是否要流式；与上游是否流式保持一致。
func callUpstreamAnthropic(rt *RouteTarget, antRaw []byte, stream bool) *upstreamResult {
	if rt.Provider.Format == "openai" {
		return callOpenAIUpstream(rt, antRaw, stream)
	}
	return callAnthropicUpstream(rt, antRaw)
}

// ── anthropic 格式上游：直发，model 已在 forwardHandler 改写好 ──

func callAnthropicUpstream(rt *RouteTarget, antRaw []byte) *upstreamResult {
	req, err := http.NewRequest("POST", rt.Provider.BaseURL+"/v1/messages", bytes.NewReader(antRaw))
	if err != nil {
		return &upstreamResult{Err: err}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", rt.Provider.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("accept-encoding", "identity")

	resp, err := dispatchClient.Do(req)
	if err != nil {
		return &upstreamResult{Err: err}
	}
	if strings.HasPrefix(resp.Header.Get("content-type"), "text/event-stream") {
		return &upstreamResult{Status: resp.StatusCode, Stream: resp.Body}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	return &upstreamResult{Status: resp.StatusCode, AnthBody: body}
}

// ── openai 格式上游：请求转 OpenAI 发出，响应转回 Anthropic ──

func callOpenAIUpstream(rt *RouteTarget, antRaw []byte, stream bool) *upstreamResult {
	oaiReq, err := convertAnthropicRequestToOpenAI(antRaw)
	if err != nil {
		return &upstreamResult{Err: err}
	}
	oaiReq.Model = rt.UpstreamID // 用上游真实名
	oaiReq.Stream = stream
	if stream {
		oaiReq.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}

	oaiBody, _ := json.Marshal(oaiReq)
	req, err := http.NewRequest("POST", rt.Provider.BaseURL+"/v1/chat/completions", bytes.NewReader(oaiBody))
	if err != nil {
		return &upstreamResult{Err: err}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+rt.Provider.APIKey)
	req.Header.Set("accept-encoding", "identity")

	resp, err := dispatchClient.Do(req)
	if err != nil {
		return &upstreamResult{Err: err}
	}

	// 错误：转成 Anthropic 错误体（非流式处理，下游统一看到 Anthropic 格式错误）
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return &upstreamResult{Status: resp.StatusCode, AnthBody: openaiErrorToAnthropic(raw)}
	}

	if stream {
		// 把 OpenAI SSE 通过管道转成 Anthropic SSE
		pr, pw := io.Pipe()
		go streamOpenAIToAnthropic(resp.Body, pw)
		return &upstreamResult{Status: resp.StatusCode, Stream: pr}
	}

	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	anthBody, err := convertOpenAIResponseToAnthropic(raw)
	if err != nil {
		return &upstreamResult{Err: err}
	}
	return &upstreamResult{Status: resp.StatusCode, AnthBody: anthBody}
}

// streamOpenAIToAnthropic 逐行读 OpenAI SSE，转成 Anthropic SSE 写进 pw。
// 上游结束后写收尾事件并关闭管道。
func streamOpenAIToAnthropic(src io.ReadCloser, pw *io.PipeWriter) {
	defer src.Close()
	st := newOaiToAnthState()
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)

	gotDone := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			gotDone = true
			break
		}
		if out := convertOpenAIChunkToAnthropic([]byte(payload), st); out != "" {
			if _, err := io.WriteString(pw, out); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}

	// 上游读取出错（连接中断 / 超缓冲），或没收到 [DONE] 就 EOF：
	// 视为流不完整，用 CloseWithError 让下游感知中断，不要伪造一个干净的 end_turn 收尾。
	if err := scanner.Err(); err != nil {
		_ = pw.CloseWithError(err)
		return
	}
	if !gotDone {
		_ = pw.CloseWithError(io.ErrUnexpectedEOF)
		return
	}
	_, _ = io.WriteString(pw, st.finishStream())
	_ = pw.Close()
}

// openaiErrorToAnthropic 把 OpenAI 错误体转成 Anthropic 错误体。
func openaiErrorToAnthropic(raw []byte) []byte {
	var oe struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	msg := "upstream error"
	etype := "api_error"
	if json.Unmarshal(raw, &oe) == nil && oe.Error.Message != "" {
		msg = oe.Error.Message
		if oe.Error.Type != "" {
			etype = oe.Error.Type
		}
	}
	out, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": etype, "message": msg},
	})
	return out
}

// ────────────────────── 下游 Anthropic 客户端 → openai 上游 ──────────────────────

// dispatchAnthropicToOpenAI 处理"下游 Anthropic 客户端、上游 openai 格式"象限：
// 发请求 → 把 Anthropic 规范态响应原样回送下游（下游本就要 Anthropic 格式）。
func dispatchAnthropicToOpenAI(w http.ResponseWriter, r *http.Request, rt *RouteTarget, antRaw []byte, rec *UsageRecord) {
	// 下游是否要流式：看请求体 stream 字段
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(antRaw, &peek)

	res := callUpstreamAnthropic(rt, antRaw, peek.Stream)
	if res.Err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+res.Err.Error())
		rec.Status = http.StatusBadGateway
		rec.LatencyMs = time.Since(rec.Time).Milliseconds()
		rec.StopReason = "proxy_upstream_error"
		go writeRecord(rec, antRaw, nil)
		return
	}

	rec.Status = res.Status

	// 流式：把 Anthropic SSE 流原样转发给下游，同时 tee 给 usage 解析
	if res.Stream != nil {
		defer res.Stream.Close()
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming not supported")
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.WriteHeader(res.Status)
		rec.Streaming = true

		var teeBuf bytes.Buffer
		const maxTee = 4 << 20
		buf := make([]byte, 32<<10)
		ctx := r.Context()
		streamBroken := false
		for {
			if ctx.Err() != nil {
				streamBroken = true
				break
			}
			n, err := res.Stream.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				flusher.Flush()
				if teeBuf.Len() < maxTee {
					teeBuf.Write(buf[:n])
				}
			}
			if err != nil {
				// io.EOF = 正常结束；其它错误（含 streamOpenAIToAnthropic 的
				// CloseWithError 传来的上游中断）= 流不完整。
				if err != io.EOF {
					streamBroken = true
				}
				break
			}
		}
		rec.LatencyMs = time.Since(rec.Time).Milliseconds()
		parseUsageSSE(teeBuf.Bytes(), rec)
		if streamBroken && rec.StopReason == "" {
			rec.StopReason = "proxy_stream_interrupted"
		}
		go writeRecord(rec, antRaw, teeBuf.Bytes())
		return
	}

	// 非流式：Anthropic JSON 直接回送
	rec.LatencyMs = time.Since(rec.Time).Milliseconds()
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.AnthBody)
	parseUsageJSON(res.AnthBody, rec)
	go writeRecord(rec, antRaw, res.AnthBody)
}

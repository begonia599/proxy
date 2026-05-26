// usage.go: 用量记录类型 + Anthropic 响应解析（JSON 与 SSE） + 入库
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UsageRecord 一次请求的完整用量快照，最终写进 SQLite。
type UsageRecord struct {
	Time           time.Time
	ProxyKey       string
	RequestID      string
	HTTPReqID      string
	Endpoint       string
	Method         string
	Status         int
	Model          string
	InputTokens    int
	OutputTokens   int
	CacheCreate5m  int
	CacheCreate1h  int
	CacheRead      int
	WebSearchCount int
	CostUSD        float64
	LatencyMs      int64
	Streaming      bool
	StopReason     string
	ClientIP       string
	UserAgent      string
}

// errorBodyCap 单边 body 最多入库这么多字节（256 KB）。
// 错误诊断够用了，再大就是浪费 SQLite 体积。
const errorBodyCap = 256 << 10

func truncBody(b []byte) []byte {
	if len(b) <= errorBodyCap {
		return b
	}
	return b[:errorBodyCap]
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

// usageBlock 对应 Anthropic 响应里的 usage 对象，兼容拆分（ephemeral_5m/1h）和合并字段。
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

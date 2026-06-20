// compat_convert_out.go: 出站转换器（Anthropic ↔ OpenAI），纯函数。
//
// 与 compat_convert.go 的入站方向相反：
//
//	请求：Anthropic Messages（内部规范态） → OpenAI Chat Completions（发给 openai 格式上游）
//	响应：OpenAI Chat Completions → Anthropic Messages（转回规范态）
//	流式：OpenAI chunk → 零或多条 Anthropic SSE 事件
//
// 设计目标：让 openai 格式的上游对系统其余部分"看起来像 Anthropic"，
// 这样下游无论是 Anthropic 客户端还是 OpenAI 客户端都走同一套规范态管线。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ────────────────────── 请求：Anthropic → OpenAI ──────────────────────

// anthropic 请求的解析视图（Content/System 用 RawMessage 容纳 string | []block）。
type anthReqParse struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anthMsgParse  `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anthToolParse `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
}

type anthMsgParse struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthToolParse struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Type        string          `json:"type,omitempty"` // 服务端工具会有 type；客户端工具为空
}

// anthBlock 一个 Anthropic content block 的解析视图（覆盖所有方向用到的字段）。
type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result 的内容：string | []block
	Source    json.RawMessage `json:"source,omitempty"`  // image
}

// convertAnthropicRequestToOpenAI 把内部规范态 Anthropic 请求体转成 OpenAI 请求。
// stream 由调用方按需覆盖（出站统一打开 include_usage 以拿计费）。
func convertAnthropicRequestToOpenAI(antRaw []byte) (*oaiChatRequest, error) {
	var ar anthReqParse
	if err := json.Unmarshal(antRaw, &ar); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}

	out := &oaiChatRequest{
		Model:       ar.Model,
		Temperature: ar.Temperature,
		TopP:        ar.TopP,
		Stream:      ar.Stream,
	}
	if ar.MaxTokens > 0 {
		out.MaxTokens = &ar.MaxTokens
	}
	if len(ar.StopSequences) > 0 {
		out.Stop = ar.StopSequences
	}

	// system → 首条 system 消息
	if sys := extractAnthropicSystem(ar.System); sys != "" {
		out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: sys})
	}

	// messages
	for _, m := range ar.Messages {
		msgs, err := convertAnthropicMessageToOpenAI(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msgs...)
	}

	// tools（跳过服务端工具：openai 上游不认 web_search_* 等）
	for _, t := range ar.Tools {
		if t.Type != "" {
			continue
		}
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, oaiTool{
			Type: "function",
			Function: oaiFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	// tool_choice
	if len(ar.ToolChoice) > 0 && len(out.Tools) > 0 {
		out.ToolChoice = convertAnthropicToolChoice(ar.ToolChoice)
	}

	return out, nil
}

// extractAnthropicSystem 把 system（string | []block）抽成纯文本。
func extractAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// convertAnthropicMessageToOpenAI 把一条 Anthropic 消息转成一条或多条 OpenAI 消息。
// tool_result 块会拆成独立的 role:tool 消息（OpenAI 要求）。
func convertAnthropicMessageToOpenAI(m anthMsgParse) ([]oaiMessage, error) {
	// content 为纯字符串
	var str string
	if json.Unmarshal(m.Content, &str) == nil {
		return []oaiMessage{{Role: m.Role, Content: str}}, nil
	}

	var blocks []anthBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("parse message content: %w", err)
	}

	switch m.Role {
	case "assistant":
		return convertAnthropicAssistantBlocks(blocks), nil
	default: // user（含 tool_result）
		return convertAnthropicUserBlocks(blocks), nil
	}
}

func convertAnthropicAssistantBlocks(blocks []anthBlock) []oaiMessage {
	var textParts []string
	var toolCalls []oaiToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, oaiToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: oaiToolCallFunction{Name: b.Name, Arguments: args},
			})
		}
	}
	msg := oaiMessage{Role: "assistant"}
	if len(textParts) > 0 {
		msg.Content = strings.Join(textParts, "")
	} else {
		msg.Content = nil
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return []oaiMessage{msg}
}

func convertAnthropicUserBlocks(blocks []anthBlock) []oaiMessage {
	var out []oaiMessage
	var parts []oaiContentPart
	flushUser := func() {
		if len(parts) > 0 {
			out = append(out, oaiMessage{Role: "user", Content: parts})
			parts = nil
		}
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, oaiContentPart{Type: "text", Text: b.Text})
		case "image":
			if u := anthropicImageToDataURL(b.Source); u != "" {
				parts = append(parts, oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: u}})
			}
		case "tool_result":
			// tool_result 必须成为独立 role:tool 消息；先把已累计的 user 内容刷出，保序
			flushUser()
			out = append(out, oaiMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    extractAnthropicToolResultText(b.Content),
			})
		}
	}
	flushUser()
	if len(out) == 0 {
		out = append(out, oaiMessage{Role: "user", Content: ""})
	}
	return out
}

// anthropicImageToDataURL 把 Anthropic image source 还原成 OpenAI 能吃的 URL。
// base64 → data URI；url → 原 URL。
func anthropicImageToDataURL(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(raw, &src) != nil {
		return ""
	}
	switch src.Type {
	case "base64":
		return "data:" + src.MediaType + ";base64," + src.Data
	case "url":
		return src.URL
	}
	return ""
}

// extractAnthropicToolResultText 把 tool_result content（string | []block）抽成文本。
func extractAnthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// convertAnthropicToolChoice 把 Anthropic tool_choice 转成 OpenAI 形式。
func convertAnthropicToolChoice(raw json.RawMessage) any {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if tc.Name != "" {
			return map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
		}
	case "none":
		return "none"
	}
	return nil
}

// ────────────────────── 响应：OpenAI → Anthropic（非流式） ──────────────────────

// convertOpenAIResponseToAnthropic 把 OpenAI chat.completion 转成 Anthropic Messages JSON。
func convertOpenAIResponseToAnthropic(oaiBody []byte) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(oaiBody, &resp); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}

	content := []map[string]any{}
	finish := "end_turn"
	if len(resp.Choices) > 0 {
		ch := resp.Choices[0]
		if ch.Message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": ch.Message.Content})
		}
		for _, tc := range ch.Message.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if strings.TrimSpace(tc.Function.Arguments) == "" {
				input = json.RawMessage(`{}`)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
			})
		}
		finish = reverseFinishReason(ch.FinishReason)
	}

	id := resp.ID
	if id == "" {
		id = "msg_unknown"
	}
	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       content,
		"stop_reason":   finish,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

// reverseFinishReason 把 OpenAI finish_reason 反向映射成 Anthropic stop_reason。
func reverseFinishReason(r string) string {
	switch r {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default: // stop / 空
		return "end_turn"
	}
}

// ────────────────────── 响应：OpenAI → Anthropic（流式） ──────────────────────

// oaiToAnthState 流式转换状态机。
type oaiToAnthState struct {
	started      bool
	id           string
	model        string
	textOpen     bool        // index 0 的 text block 是否已开
	nextIndex    int         // 下一个要分配的 Anthropic content block index
	toolBlock    map[int]int // openai tool index → anthropic block index
	curOpenIndex int         // 当前已开但未关的 block index（-1 = 无）
	finish       string
	inTokens     int
	outTokens    int
}

func newOaiToAnthState() *oaiToAnthState {
	return &oaiToAnthState{toolBlock: map[int]int{}, curOpenIndex: -1, finish: "end_turn"}
}

// anthSSE 拼一条 Anthropic SSE 事件文本。
func anthSSE(event string, payload any) string {
	data, _ := json.Marshal(payload)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)
}

// convertOpenAIChunkToAnthropic 处理单条 OpenAI chunk，返回要写出的 Anthropic SSE 文本（可能为空）。
func convertOpenAIChunkToAnthropic(chunkJSON []byte, st *oaiToAnthState) string {
	var ch struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(chunkJSON, &ch) != nil {
		return ""
	}

	var b strings.Builder

	// message_start（首个 chunk）
	if !st.started {
		st.started = true
		st.id = ch.ID
		st.model = ch.Model
		if st.id == "" {
			st.id = "msg_stream"
		}
		b.WriteString(anthSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": st.id, "type": "message", "role": "assistant", "model": st.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}))
	}

	// 末尾 usage chunk（include_usage 时 choices 为空）
	if ch.Usage != nil {
		st.inTokens = ch.Usage.PromptTokens
		st.outTokens = ch.Usage.CompletionTokens
	}

	for _, c := range ch.Choices {
		// 文本增量
		if c.Delta.Content != "" {
			if !st.textOpen {
				// 文本块固定占 index 0；先关掉可能开着的其它块
				st.closeCurrent(&b)
				idx := st.nextIndex
				st.nextIndex++
				st.textOpen = true
				st.curOpenIndex = idx
				b.WriteString(anthSSE("content_block_start", map[string]any{
					"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "text", "text": ""},
				}))
			}
			b.WriteString(anthSSE("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": st.curOpenIndex,
				"delta": map[string]any{"type": "text_delta", "text": c.Delta.Content},
			}))
		}

		// 工具调用增量
		for _, tc := range c.Delta.ToolCalls {
			blockIdx, seen := st.toolBlock[tc.Index]
			if !seen {
				st.closeCurrent(&b)
				blockIdx = st.nextIndex
				st.nextIndex++
				st.toolBlock[tc.Index] = blockIdx
				st.curOpenIndex = blockIdx
				st.textOpen = false
				b.WriteString(anthSSE("content_block_start", map[string]any{
					"type": "content_block_start", "index": blockIdx,
					"content_block": map[string]any{
						"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{},
					},
				}))
			}
			if tc.Function.Arguments != "" {
				b.WriteString(anthSSE("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": blockIdx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				}))
			}
		}

		if c.FinishReason != nil && *c.FinishReason != "" {
			st.finish = reverseFinishReason(*c.FinishReason)
		}
	}

	return b.String()
}

// closeCurrent 关闭当前打开的 content block（如果有）。
func (st *oaiToAnthState) closeCurrent(b *strings.Builder) {
	if st.curOpenIndex >= 0 {
		b.WriteString(anthSSE("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": st.curOpenIndex,
		}))
		st.curOpenIndex = -1
	}
}

// finishStream 在 OpenAI 流结束（[DONE]）时生成收尾的 Anthropic 事件：
// 关闭末块 → message_delta(stop_reason + usage) → message_stop。
func (st *oaiToAnthState) finishStream() string {
	var b strings.Builder
	if !st.started {
		// 上游一个 chunk 都没给：补一个最小 message_start，避免下游解析空流
		st.started = true
		b.WriteString(anthSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_stream", "type": "message", "role": "assistant", "model": st.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}))
	}
	st.closeCurrent(&b)
	b.WriteString(anthSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": st.finish, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": st.inTokens, "output_tokens": st.outTokens},
	}))
	b.WriteString(anthSSE("message_stop", map[string]any{"type": "message_stop"}))
	return b.String()
}

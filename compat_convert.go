// compat_convert.go: OpenAI ↔ Anthropic 格式转换
//
// 纯函数，不依赖 HTTP / IO / 全局状态。
// 请求方向：OpenAI Chat Completions → Anthropic Messages
// 响应方向：Anthropic Messages → OpenAI Chat Completions
// 流式方向：单条 Anthropic SSE 事件 → 零或多条 OpenAI chunk
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ────────────────────── OpenAI 类型 ──────────────────────

type oaiChatRequest struct {
	Model               string       `json:"model"`
	Messages            []oaiMessage `json:"messages"`
	MaxTokens           *int         `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int         `json:"max_completion_tokens,omitempty"`
	Temperature         *float64     `json:"temperature,omitempty"`
	TopP                *float64     `json:"top_p,omitempty"`
	Stop                any          `json:"stop,omitempty"`
	Stream              bool         `json:"stream"`
	Tools               []oaiTool    `json:"tools,omitempty"`
	ToolChoice          any          `json:"tool_choice,omitempty"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`               // string | []oaiContentPart | null
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`  // assistant 发起
	ToolCallID string        `json:"tool_call_id,omitempty"` // role=tool 时
}

type oaiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type oaiTool struct {
	Type     string      `json:"type"` // "function"
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"` // "function"
	Function oaiToolCallFunction `json:"function"`
	Index    *int                `json:"index,omitempty"` // 流式时用
}

type oaiToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ── 响应 ──

type oaiChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"` // "chat.completion"
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage,omitempty"`
}

type oaiChoice struct {
	Index        int        `json:"index"`
	Message      *oaiOutMsg `json:"message,omitempty"` // 非流式
	Delta        *oaiOutMsg `json:"delta,omitempty"`   // 流式
	FinishReason *string    `json:"finish_reason"`     // nullable
}

type oaiOutMsg struct {
	Role      string        `json:"role,omitempty"`
	Content   *string       `json:"content"`                // nullable
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ── Models ──

type oaiModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`   // "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"` // "anthropic"
}

type oaiModelList struct {
	Object string          `json:"object"` // "list"
	Data   []oaiModelEntry `json:"data"`
}

// ── Error ──

type oaiErrorResponse struct {
	Error oaiErrorBody `json:"error"`
}
type oaiErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// ────────────────────── Anthropic 请求类型（构造用） ──────────────────────

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        any                `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []anthropicContentBlock
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	Source    any             `json:"source,omitempty"` // base64Source | urlSource
}

// base64Source 用于 base64 编码的图片/文档源
type base64Source struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// urlSource 用于 URL 形式的图片源——注意字段是 url 不是 data
type urlSource struct {
	Type string `json:"type"` // "url"
	URL  string `json:"url"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ────────────────────── 请求转换 ──────────────────────

const defaultMaxTokens = 4096

func convertRequest(req *oaiChatRequest) (*anthropicRequest, error) {
	ar := &anthropicRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}

	// max_tokens
	switch {
	case req.MaxTokens != nil:
		ar.MaxTokens = *req.MaxTokens
	case req.MaxCompletionTokens != nil:
		ar.MaxTokens = *req.MaxCompletionTokens
	default:
		ar.MaxTokens = defaultMaxTokens
	}

	// stop → stop_sequences
	ar.StopSequences = convertStop(req.Stop)

	// tools
	if len(req.Tools) > 0 {
		ar.Tools = convertToolsToAnthropic(req.Tools)
	}

	// tool_choice
	if req.ToolChoice != nil && len(req.Tools) > 0 {
		ar.ToolChoice = convertToolChoice(req.ToolChoice)
	}

	// messages → system 提取 + 消息转换
	system, msgs, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	ar.System = system
	ar.Messages = msgs
	return ar, nil
}

// ── stop 序列：string | []string | nil ──

func convertStop(v any) []string {
	if v == nil {
		return nil
	}
	switch s := v.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// ── tools: OpenAI function → Anthropic tool ──

func convertToolsToAnthropic(tools []oaiTool) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

// ── tool_choice 转换 ──

func convertToolChoice(v any) any {
	switch tc := v.(type) {
	case string:
		switch tc {
		case "auto":
			return map[string]string{"type": "auto"}
		case "none":
			return map[string]string{"type": "none"}
		case "required":
			return map[string]string{"type": "any"}
		}
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return map[string]string{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// ── messages 转换：提取 system + 角色交替合并 ──

func convertMessages(msgs []oaiMessage) (any, []anthropicMessage, error) {
	var systemParts []string
	var converted []anthropicMessage

	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case "system":
			text := extractTextContent(m.Content)
			if text != "" {
				systemParts = append(systemParts, text)
			}

		case "user":
			blocks := convertUserContent(m.Content)
			converted = append(converted, anthropicMessage{Role: "user", Content: blocks})

		case "assistant":
			blocks := convertAssistantMessage(m)
			converted = append(converted, anthropicMessage{Role: "assistant", Content: blocks})

		case "tool":
			// 连续 tool 消息合并为一条 user 消息
			var toolResults []anthropicContentBlock
			for ; i < len(msgs) && msgs[i].Role == "tool"; i++ {
				toolResults = append(toolResults, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: msgs[i].ToolCallID,
					Content:   extractTextContent(msgs[i].Content),
				})
			}
			i-- // for 循环还会 i++
			converted = append(converted, anthropicMessage{Role: "user", Content: toolResults})
		}
	}

	// Anthropic 要求 user/assistant 严格交替，合并相邻同角色消息
	converted = mergeAdjacentRoles(converted)

	var system any
	if len(systemParts) > 0 {
		system = strings.Join(systemParts, "\n\n")
	}
	return system, converted, nil
}

func extractTextContent(content any) string {
	if content == nil {
		return ""
	}
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" {
					if text, _ := m["text"].(string); text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprintf("%v", content)
}

func convertUserContent(content any) any {
	if content == nil {
		return ""
	}
	switch c := content.(type) {
	case string:
		return c
	case []any:
		blocks := make([]anthropicContentBlock, 0, len(c))
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			t, _ := m["type"].(string)
			switch t {
			case "text":
				text, _ := m["text"].(string)
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
			case "image_url":
				if iu, ok := m["image_url"].(map[string]any); ok {
					url, _ := iu["url"].(string)
					block := convertImageURL(url)
					if block != nil {
						blocks = append(blocks, *block)
					}
				}
			}
		}
		if len(blocks) == 0 {
			return ""
		}
		return blocks
	}
	return fmt.Sprintf("%v", content)
}

// convertImageURL 处理 data URI（base64）和普通 URL
func convertImageURL(url string) *anthropicContentBlock {
	if strings.HasPrefix(url, "data:") {
		// data:image/jpeg;base64,/9j/4AAQ...
		parts := strings.SplitN(url, ",", 2)
		if len(parts) != 2 {
			return nil
		}
		meta := strings.TrimPrefix(parts[0], "data:")
		meta = strings.TrimSuffix(meta, ";base64")
		return &anthropicContentBlock{
			Type:   "image",
			Source: base64Source{Type: "base64", MediaType: meta, Data: parts[1]},
		}
	}
	// HTTP(S) URL → Anthropic url source（字段是 url，不是 data）
	return &anthropicContentBlock{
		Type:   "image",
		Source: urlSource{Type: "url", URL: url},
	}
}

func convertAssistantMessage(m oaiMessage) any {
	if len(m.ToolCalls) == 0 {
		// 纯文本 assistant 消息
		return extractTextContent(m.Content)
	}

	// 有 tool_calls → 构造 content blocks
	var blocks []anthropicContentBlock
	text := extractTextContent(m.Content)
	if text != "" {
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
	}
	for _, tc := range m.ToolCalls {
		// OpenAI 的 arguments 是 JSON 字符串；Anthropic 的 input 是 JSON 对象。
		// 空字符串会导致 omitempty 丢字段（Anthropic 要求 input 存在），补成 {}。
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		blocks = append(blocks, anthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(args),
		})
	}
	return blocks
}

// mergeAdjacentRoles 合并相邻同角色消息（Anthropic 要求交替）
func mergeAdjacentRoles(msgs []anthropicMessage) []anthropicMessage {
	if len(msgs) <= 1 {
		return msgs
	}
	merged := []anthropicMessage{msgs[0]}
	for _, m := range msgs[1:] {
		last := &merged[len(merged)-1]
		if last.Role != m.Role {
			merged = append(merged, m)
			continue
		}
		// 同角色 → 合并 content
		lastBlocks := toContentBlocks(last.Content)
		newBlocks := toContentBlocks(m.Content)
		last.Content = append(lastBlocks, newBlocks...)
	}
	return merged
}

func toContentBlocks(content any) []anthropicContentBlock {
	switch c := content.(type) {
	case []anthropicContentBlock:
		return c
	case string:
		if c == "" {
			return nil
		}
		return []anthropicContentBlock{{Type: "text", Text: c}}
	}
	return nil
}

// ────────────────────── 响应转换（非流式） ──────────────────────

func convertResponse(body []byte) (*oaiChatResponse, error) {
	var ar struct {
		ID         string            `json:"id"`
		Model      string            `json:"model"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("parse anthropic response: %w", err)
	}

	msg := &oaiOutMsg{Role: "assistant"}
	var textParts []string
	var toolCalls []oaiToolCall

	for _, raw := range ar.Content {
		var block struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			args, _ := json.Marshal(json.RawMessage(block.Input))
			toolCalls = append(toolCalls, oaiToolCall{
				ID:   block.ID,
				Type: "function",
				Function: oaiToolCallFunction{
					Name:      block.Name,
					Arguments: string(args),
				},
			})
		}
	}

	if len(textParts) > 0 {
		joined := strings.Join(textParts, "")
		msg.Content = &joined
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	finish := mapStopReason(ar.StopReason)
	return &oaiChatResponse{
		ID:      "chatcmpl-" + ar.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   ar.Model,
		Choices: []oaiChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finish,
		}},
		Usage: &oaiUsage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}, nil
}

// ────────────────────── SSE 流式转换 ──────────────────────

type streamState struct {
	id        string
	model     string
	created   int64
	toolIndex int      // 当前 tool_calls 下标
	curBlock  string   // 当前 block 类型（text / tool_use），用于 content_block_stop 判断
}

// convertSSEEvent 将单条 Anthropic SSE 事件转成零或多条 OpenAI chunk。
func convertSSEEvent(eventType string, payload []byte, st *streamState) []oaiChatResponse {
	switch eventType {
	case "message_start":
		var ev struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return nil
		}
		st.id = "chatcmpl-" + ev.Message.ID
		st.model = ev.Message.Model
		st.created = time.Now().Unix()
		st.toolIndex = 0
		role := "assistant"
		return []oaiChatResponse{makeChunk(st, &oaiOutMsg{Role: role, Content: strPtr("")}, nil)}

	case "content_block_start":
		var ev struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return nil
		}
		if ev.ContentBlock.Type == "tool_use" {
			idx := st.toolIndex
			st.curBlock = "tool_use"
			chunk := makeChunk(st, &oaiOutMsg{
				ToolCalls: []oaiToolCall{{
					Index: &idx,
					ID:    ev.ContentBlock.ID,
					Type:  "function",
					Function: oaiToolCallFunction{
						Name:      ev.ContentBlock.Name,
						Arguments: "",
					},
				}},
			}, nil)
			return []oaiChatResponse{chunk}
		}
		st.curBlock = ev.ContentBlock.Type
		return nil

	case "content_block_delta":
		var ev struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			return []oaiChatResponse{makeChunk(st, &oaiOutMsg{Content: &ev.Delta.Text}, nil)}
		case "input_json_delta":
			idx := st.toolIndex
			return []oaiChatResponse{makeChunk(st, &oaiOutMsg{
				ToolCalls: []oaiToolCall{{
					Index: &idx,
					Function: oaiToolCallFunction{
						Arguments: ev.Delta.PartialJSON,
					},
				}},
			}, nil)}
		}
		return nil

	case "content_block_stop":
		var ev struct {
			Index int `json:"index"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return nil
		}
		// 只有 tool_use block 结束时才推进 toolIndex（text block 不影响）
		if st.curBlock == "tool_use" {
			st.toolIndex++
		}
		st.curBlock = ""
		return nil

	case "message_delta":
		var ev struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return nil
		}
		finish := mapStopReason(ev.Delta.StopReason)
		return []oaiChatResponse{makeChunk(st, &oaiOutMsg{}, &finish)}

	case "message_stop":
		return nil // 调用方负责发 [DONE]

	default: // ping, error 等
		return nil
	}
}

func makeChunk(st *streamState, delta *oaiOutMsg, finishReason *string) oaiChatResponse {
	return oaiChatResponse{
		ID:      st.id,
		Object:  "chat.completion.chunk",
		Created: st.created,
		Model:   st.model,
		Choices: []oaiChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}
}

// ────────────────────── 通用映射 ──────────────────────

func mapStopReason(r string) string {
	switch r {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

func strPtr(s string) *string { return &s }

// convertAnthropicError 把 Anthropic 错误体转成 OpenAI 格式
func convertAnthropicError(body []byte) []byte {
	var ae struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &ae) != nil {
		return body // 解析失败原样返回
	}
	resp := oaiErrorResponse{
		Error: oaiErrorBody{
			Message: ae.Error.Message,
			Type:    ae.Error.Type,
		},
	}
	out, _ := json.Marshal(resp)
	return out
}

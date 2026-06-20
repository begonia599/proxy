package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 请求方向：Anthropic → OpenAI
func TestConvertAnthropicRequestToOpenAI(t *testing.T) {
	in := `{
	  "model": "claude-x",
	  "system": "be brief",
	  "max_tokens": 100,
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": [
	      {"type": "text", "text": "let me check"},
	      {"type": "tool_use", "id": "t1", "name": "get_weather", "input": {"city": "SF"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "t1", "content": "sunny"}
	    ]}
	  ],
	  "tools": [
	    {"name": "get_weather", "description": "w", "input_schema": {"type": "object"}}
	  ]
	}`
	out, err := convertAnthropicRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	// system 成为首条 message
	if len(out.Messages) < 1 || out.Messages[0].Role != "system" {
		t.Fatalf("expected system first, got %+v", out.Messages)
	}
	// 最后一条应是 role:tool
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "t1" {
		t.Fatalf("expected tool message with id t1, got %+v", last)
	}
	if last.Content != "sunny" {
		t.Fatalf("tool content = %v", last.Content)
	}
	// assistant 的 tool_use → tool_calls
	var asst *oaiMessage
	for i := range out.Messages {
		if out.Messages[i].Role == "assistant" {
			asst = &out.Messages[i]
		}
	}
	if asst == nil || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("assistant tool_calls wrong: %+v", asst)
	}
	if asst.ToolCalls[0].Function.Arguments != `{"city": "SF"}` && asst.ToolCalls[0].Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("tool args = %q", asst.ToolCalls[0].Function.Arguments)
	}
	// tools 转换
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools wrong: %+v", out.Tools)
	}
	// max_tokens
	if out.MaxTokens == nil || *out.MaxTokens != 100 {
		t.Fatalf("max_tokens wrong: %+v", out.MaxTokens)
	}
}

func TestConvertAnthropicRequest_SystemBlocks(t *testing.T) {
	in := `{"model":"m","max_tokens":1,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"x"}]}`
	out, err := convertAnthropicRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "a\n\nb" {
		t.Fatalf("system blocks merge wrong: %+v", out.Messages[0])
	}
}

func TestConvertAnthropicRequest_SkipsServerTools(t *testing.T) {
	in := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
	  "tools":[{"type":"web_search_20260209","name":"web_search"},{"name":"calc","input_schema":{"type":"object"}}]}`
	out, err := convertAnthropicRequestToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "calc" {
		t.Fatalf("should skip server tool, got %+v", out.Tools)
	}
}

// 响应方向：OpenAI → Anthropic（非流式）
func TestConvertOpenAIResponseToAnthropic(t *testing.T) {
	in := `{
	  "id": "cmpl-1", "model": "gpt",
	  "choices": [{"message": {"content": "hello", "tool_calls": [
	    {"id": "c1", "type": "function", "function": {"name": "f", "arguments": "{\"a\":1}"}}
	  ]}, "finish_reason": "tool_calls"}],
	  "usage": {"prompt_tokens": 10, "completion_tokens": 5}
	}`
	out, err := convertOpenAIResponseToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Fatalf("envelope wrong: %+v", resp)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %s", resp.StopReason)
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != "text" || resp.Content[1].Type != "tool_use" {
		t.Fatalf("content blocks wrong: %+v", resp.Content)
	}
	if resp.Content[1].Name != "f" {
		t.Fatalf("tool name = %s", resp.Content[1].Name)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	// usage 必须能被现有解析管线读到
	var rec UsageRecord
	parseUsageJSON(out, &rec)
	if rec.InputTokens != 10 || rec.OutputTokens != 5 {
		t.Fatalf("parseUsageJSON didn't read converted usage: %+v", rec)
	}
}

// 流式方向：OpenAI chunks → Anthropic SSE
func TestStreamOpenAIToAnthropic(t *testing.T) {
	st := newOaiToAnthState()
	var sb strings.Builder

	chunks := []string{
		`{"id":"x","model":"gpt","choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"id":"x","choices":[{"delta":{"content":"Hel"}}]}`,
		`{"id":"x","choices":[{"delta":{"content":"lo"}}]}`,
		`{"id":"x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"f","arguments":""}}]}}]}`,
		`{"id":"x","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`{"id":"x","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
	}
	for _, c := range chunks {
		sb.WriteString(convertOpenAIChunkToAnthropic([]byte(c), st))
	}
	sb.WriteString(st.finishStream())
	out := sb.String()

	// 必须包含核心事件
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"text_delta",
		"input_json_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// 用现有 SSE usage 解析器验证 token 能读出
	var rec UsageRecord
	parseUsageSSE([]byte(out), &rec)
	if rec.OutputTokens != 3 {
		t.Fatalf("parseUsageSSE output = %d, want 3\n%s", rec.OutputTokens, out)
	}
	if rec.InputTokens != 7 {
		t.Fatalf("parseUsageSSE input = %d, want 7 (input_tokens must survive openai→anthropic stream)\n%s", rec.InputTokens, out)
	}
	if rec.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %s", rec.StopReason)
	}

	// 整体可被 convertSSEEvent 链路消费（即下游 OpenAI 客户端也能转回）
	// 简单校验每个 data 行是合法 JSON
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var v any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Fatalf("invalid SSE json: %q", line)
			}
		}
	}
}

func TestReverseFinishReason(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
		"content_filter": "end_turn", "": "end_turn",
	}
	for in, want := range cases {
		if got := reverseFinishReason(in); got != want {
			t.Errorf("reverseFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenaiErrorToAnthropic(t *testing.T) {
	in := `{"error":{"message":"bad model","type":"invalid_request_error"}}`
	out := openaiErrorToAnthropic([]byte(in))
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != "error" || e.Error.Message != "bad model" || e.Error.Type != "invalid_request_error" {
		t.Fatalf("converted error wrong: %s", out)
	}
}

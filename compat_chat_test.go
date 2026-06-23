package main

import (
	"encoding/json"
	"testing"
)

func TestForceOpenAIChatStreamUsage(t *testing.T) {
	in := []byte(`{"model":"gpt-5.5","stream":true,"stream_options":{"foo":"bar"},"messages":[]}`)
	got := forceOpenAIChatStreamUsage(in)

	var body struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("result is invalid JSON: %v\n%s", err, got)
	}
	if body.StreamOptions["foo"] != "bar" {
		t.Fatalf("existing stream_options field was not preserved: %#v", body.StreamOptions)
	}
	if body.StreamOptions["include_usage"] != true {
		t.Fatalf("include_usage = %#v, want true", body.StreamOptions["include_usage"])
	}
}

func TestParseOpenAIChatUsageJSON(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl_test",
		"model":"gpt-5.5-2026-04-23",
		"choices":[{"finish_reason":"stop"}],
		"usage":{
			"prompt_tokens":1000,
			"completion_tokens":25,
			"prompt_tokens_details":{"cached_tokens":900}
		}
	}`)
	rec := &UsageRecord{StopReason: "native_openai_chat"}

	parseOpenAIChatUsageJSON(body, rec)

	if rec.RequestID != "chatcmpl_test" {
		t.Fatalf("request id = %q", rec.RequestID)
	}
	if rec.UpstreamModel != "gpt-5.5-2026-04-23" {
		t.Fatalf("upstream model = %q", rec.UpstreamModel)
	}
	if rec.InputTokens != 1000 || rec.OutputTokens != 25 || rec.CacheRead != 900 {
		t.Fatalf("usage = in:%d out:%d cache:%d", rec.InputTokens, rec.OutputTokens, rec.CacheRead)
	}
	if rec.StopReason != "stop" {
		t.Fatalf("stop reason = %q", rec.StopReason)
	}
}

func TestParseOpenAIChatUsageSSE(t *testing.T) {
	body := []byte("data: {\"id\":\"chunk_1\",\"model\":\"gpt-5.5-2026-04-23\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chunk_1\",\"model\":\"gpt-5.5-2026-04-23\",\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2000,\"completion_tokens\":30,\"prompt_tokens_details\":{\"cached_tokens\":1800}}}\n\n" +
		"data: [DONE]\n\n")
	rec := &UsageRecord{StopReason: "native_openai_chat"}

	parseOpenAIChatUsageSSE(body, rec)

	if rec.RequestID != "chunk_1" {
		t.Fatalf("request id = %q", rec.RequestID)
	}
	if rec.UpstreamModel != "gpt-5.5-2026-04-23" {
		t.Fatalf("upstream model = %q", rec.UpstreamModel)
	}
	if rec.InputTokens != 2000 || rec.OutputTokens != 30 || rec.CacheRead != 1800 {
		t.Fatalf("usage = in:%d out:%d cache:%d", rec.InputTokens, rec.OutputTokens, rec.CacheRead)
	}
	if rec.StopReason != "stop" {
		t.Fatalf("stop reason = %q", rec.StopReason)
	}
}

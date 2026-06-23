package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestApplyProviderRequestOverrides_OpenAIDefault(t *testing.T) {
	p := &Provider{Format: "openai"}
	in := []byte(`{"model":"gpt-5.5","metadata":{"client":"codex"},"messages":[]}`)

	got := applyProviderRequestOverrides(in, p)

	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("result invalid JSON: %v\n%s", err, got)
	}
	if body["store"] != false {
		t.Fatalf("store = %#v, want false", body["store"])
	}
	if _, ok := body["metadata"]; ok {
		t.Fatalf("metadata was not deleted: %s", got)
	}
}

func TestApplyProviderRequestOverrides_AnthropicDefaultDoesNotSetStore(t *testing.T) {
	p := &Provider{Format: "anthropic"}
	in := []byte(`{"model":"claude","metadata":{"client":"claude-code"},"messages":[]}`)

	got := applyProviderRequestOverrides(in, p)

	if bytes.Contains(got, []byte(`"store"`)) {
		t.Fatalf("anthropic default must not add store: %s", got)
	}
	if bytes.Contains(got, []byte(`"metadata"`)) {
		t.Fatalf("metadata was not deleted: %s", got)
	}
}

func TestApplyProviderRequestOverrides_CustomSetDelete(t *testing.T) {
	overrides := `{"operations":[
		{"mode":"set","path":"extra.flag","value":true},
		{"mode":"delete","path":"/metadata/client"}
	]}`
	p := &Provider{Format: "openai", RequestOverrides: &overrides}
	in := []byte(`{"metadata":{"client":"x","keep":"y"}}`)

	got := applyProviderRequestOverrides(in, p)

	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("result invalid JSON: %v\n%s", err, got)
	}
	meta := body["metadata"].(map[string]any)
	if _, ok := meta["client"]; ok {
		t.Fatalf("metadata.client was not deleted: %#v", meta)
	}
	extra := body["extra"].(map[string]any)
	if extra["flag"] != true {
		t.Fatalf("extra.flag = %#v", extra["flag"])
	}
}

func TestApplyProviderRequestOverrides_HybridDefaultsAreProtocolAware(t *testing.T) {
	p := &Provider{Format: providerFormatHybrid}
	in := []byte(`{"metadata":{"client":"x"}}`)

	gotAnthropic := applyProviderRequestOverridesForProtocol(in, p, protocolAnthropic)
	if bytes.Contains(gotAnthropic, []byte(`"store"`)) {
		t.Fatalf("hybrid anthropic default must not add store: %s", gotAnthropic)
	}
	if bytes.Contains(gotAnthropic, []byte(`"metadata"`)) {
		t.Fatalf("hybrid anthropic default did not delete metadata: %s", gotAnthropic)
	}

	gotOpenAI := applyProviderRequestOverridesForProtocol(in, p, protocolOpenAI)
	var body map[string]any
	if err := json.Unmarshal(gotOpenAI, &body); err != nil {
		t.Fatalf("openai result invalid JSON: %v\n%s", err, gotOpenAI)
	}
	if body["store"] != false {
		t.Fatalf("hybrid openai default store = %#v, want false", body["store"])
	}
	if _, ok := body["metadata"]; ok {
		t.Fatalf("hybrid openai default did not delete metadata: %s", gotOpenAI)
	}
}

func TestValidateRequestOverrides(t *testing.T) {
	if err := validateRequestOverrides(`{"operations":[{"mode":"set","path":"store","value":false}]}`); err != nil {
		t.Fatalf("valid overrides rejected: %v", err)
	}
	if err := validateRequestOverrides(`{"operations":[{"mode":"set","path":"store"}]}`); err == nil {
		t.Fatal("set without value should be rejected")
	}
	if err := validateRequestOverrides(`{"operations":[{"mode":"replace","path":"store","value":false}]}`); err == nil {
		t.Fatal("unknown mode should be rejected")
	}
}

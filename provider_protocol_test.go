package main

import "testing"

func TestHybridProviderProtocolSupportAndBaseURLs(t *testing.T) {
	p := &Provider{
		Format:           providerFormatHybrid,
		BaseURL:          "https://api.example.com",
		OpenAIBaseURL:    "https://api.example.com/openai",
		AnthropicBaseURL: "https://api.example.com/anthropic",
	}

	if !providerSupportsOpenAI(p) || !providerSupportsAnthropic(p) {
		t.Fatalf("hybrid provider should support both protocols")
	}
	if got := providerOpenAIBaseURL(p); got != "https://api.example.com/openai" {
		t.Fatalf("openai base = %q", got)
	}
	if got := providerAnthropicBaseURL(p); got != "https://api.example.com/anthropic" {
		t.Fatalf("anthropic base = %q", got)
	}
}

func TestProviderProtocolBaseURLFallback(t *testing.T) {
	p := &Provider{Format: providerFormatHybrid, BaseURL: "https://api.example.com/"}

	if got := providerOpenAIBaseURL(p); got != "https://api.example.com" {
		t.Fatalf("openai fallback base = %q", got)
	}
	if got := providerAnthropicBaseURL(p); got != "https://api.example.com" {
		t.Fatalf("anthropic fallback base = %q", got)
	}
}

func TestNormalizeProviderURLs_HybridMirrorsSingleProtocolURL(t *testing.T) {
	p := &Provider{Format: providerFormatHybrid, OpenAIBaseURL: "https://api.example.com/openai/"}

	normalizeProviderURLs(p)

	if p.OpenAIBaseURL != "https://api.example.com/openai" {
		t.Fatalf("openai base = %q", p.OpenAIBaseURL)
	}
	if p.AnthropicBaseURL != p.OpenAIBaseURL {
		t.Fatalf("anthropic base = %q, want mirror", p.AnthropicBaseURL)
	}
	if p.BaseURL != p.OpenAIBaseURL {
		t.Fatalf("base url = %q, want openai base", p.BaseURL)
	}
}

func TestNormalizeProviderURLs_SingleProtocolClearsProtocolURLs(t *testing.T) {
	p := &Provider{
		Format:           providerFormatAnthropic,
		BaseURL:          "https://api.example.com/",
		OpenAIBaseURL:    "https://api.example.com/openai",
		AnthropicBaseURL: "https://api.example.com/anthropic",
	}

	normalizeProviderURLs(p)

	if p.BaseURL != "https://api.example.com" {
		t.Fatalf("base url = %q", p.BaseURL)
	}
	if p.OpenAIBaseURL != "" || p.AnthropicBaseURL != "" {
		t.Fatalf("single protocol should clear protocol urls: openai=%q anthropic=%q", p.OpenAIBaseURL, p.AnthropicBaseURL)
	}
}

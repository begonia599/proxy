package main

import (
	"testing"
	"time"
)

// TestPricing_ProviderOverrideFixesZeroCost 验证 Conflict F 修复：
// 非 Anthropic 模型在静态价表里查不到 → CostOf 记 $0（旧 bug）；
// 设了按服务商计价覆盖后 → tokenCost 用覆盖价，成本非零。
func TestPricing_ProviderOverrideFixesZeroCost(t *testing.T) {
	s := newTempStore(t)
	store = s
	t.Cleanup(func() { store = nil })

	// 一个 openai 格式服务商 + 一条库存模型 gpt-4o
	pid, err := s.CreateProvider(&Provider{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "k", Format: "openai", Enabled: true})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := s.UpsertProviderModel(pid, "gpt-4o", time.UnixMilli(1)); err != nil {
		t.Fatalf("upsert model: %v", err)
	}

	providerRegistry = NewProviderRegistry(s)
	if err := providerRegistry.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(func() { providerRegistry = nil })

	// 旧行为：静态价表查不到 gpt-4o → 0
	if got := CostOf("gpt-4o", 1_000_000, 1_000_000, 0, 0, 0); got != 0 {
		t.Fatalf("static table should not know gpt-4o, got %v", got)
	}
	// 设覆盖前：tokenCost 也是 0（没有覆盖，回落静态表）
	if got := tokenCost("openrouter", "gpt-4o", 1_000_000, 0, 0, 0, 0); got != 0 {
		t.Fatalf("before override cost should be 0, got %v", got)
	}

	// 设覆盖：input $2.5/1M, output $10/1M
	if err := s.SetProviderModelPrice(pid, "gpt-4o", &Price{Input: 2.5, Output: 10}); err != nil {
		t.Fatalf("set price: %v", err)
	}
	if err := providerRegistry.Reload(); err != nil {
		t.Fatalf("reload after price: %v", err)
	}

	// 1M input + 1M output → 2.5 + 10 = 12.5
	got := tokenCost("openrouter", "gpt-4o", 1_000_000, 1_000_000, 0, 0, 0)
	if got != 12.5 {
		t.Fatalf("override cost = %v, want 12.5", got)
	}

	// 清除覆盖 → 回落静态表 → 0
	if err := s.SetProviderModelPrice(pid, "gpt-4o", nil); err != nil {
		t.Fatalf("clear price: %v", err)
	}
	if err := providerRegistry.Reload(); err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	if got := tokenCost("openrouter", "gpt-4o", 1_000_000, 0, 0, 0, 0); got != 0 {
		t.Fatalf("after clear cost should be 0, got %v", got)
	}
}

// TestPricing_StaticFallbackForAnthropic 验证 Anthropic 模型无覆盖时仍走静态价表。
func TestPricing_StaticFallbackForAnthropic(t *testing.T) {
	providerRegistry = nil // 无注册表 → 必走静态表
	got := tokenCost("anthropic-official", "claude-haiku-4-5", 1_000_000, 0, 0, 0, 0)
	if got != 1.0 { // priceHaiku45.Input = 1.0
		t.Fatalf("haiku static input cost = %v, want 1.0", got)
	}
}

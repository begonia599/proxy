package main

import "testing"

// setupRoutingTest 装一个最小的全局 registry 状态供 ResolveRoute 测试用。
// 不碰 DB——直接塞内存缓存。
func setupRoutingTest(t *testing.T) {
	t.Helper()
	store = nil // ResolveRoute 走缓存，不应碰 DB

	providerRegistry = &ProviderRegistry{
		providers:  map[int64]*Provider{},
		byName:     map[string]*Provider{},
		byLogical:  map[int64]map[string][]GroupMapping{},
		byUpstream: map[int64]map[string]GroupMapping{},
	}
	p := &Provider{ID: 1, Name: legacyProviderName, BaseURL: "https://api.anthropic.com", APIKey: "k", Format: "anthropic", Enabled: true}
	providerRegistry.providers[1] = p
	providerRegistry.byName[legacyProviderName] = p

	// 组 10：逻辑名 sonnet → provider 1 / 真实名 real-sonnet
	gm := GroupMapping{ID: 1, GroupID: 10, LogicalName: "sonnet", ProviderID: 1, UpstreamID: "real-sonnet", IsPrimary: true}
	providerRegistry.byLogical[10] = map[string][]GroupMapping{"sonnet": {gm}}
	providerRegistry.byUpstream[10] = map[string]GroupMapping{"real-sonnet": gm}

	// modelRegistry：已知 claude-haiku-4-5（用于旧 key 未知模型校验）
	modelRegistry = &ModelRegistry{valid: map[string]bool{"claude-haiku-4-5": true, "claude-opus-4-5": true}}
}

func TestResolveRoute_GroupLogicalName(t *testing.T) {
	setupRoutingTest(t)
	gid := int64(10)
	km := &KeyMeta{Key: "k", GroupID: &gid}
	rt, err := ResolveRoute(km, "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider.ID != 1 || rt.UpstreamID != "real-sonnet" {
		t.Fatalf("wrong target: %+v", rt)
	}
}

func TestResolveRoute_GroupConcreteNameFallback(t *testing.T) {
	setupRoutingTest(t)
	gid := int64(10)
	km := &KeyMeta{Key: "k", GroupID: &gid}
	rt, err := ResolveRoute(km, "real-sonnet") // 具体名
	if err != nil {
		t.Fatal(err)
	}
	if rt.UpstreamID != "real-sonnet" {
		t.Fatalf("wrong target: %+v", rt)
	}
}

func TestResolveRoute_GroupUnknownModel404(t *testing.T) {
	setupRoutingTest(t)
	gid := int64(10)
	km := &KeyMeta{Key: "k", GroupID: &gid}
	_, err := ResolveRoute(km, "gpt-4o")
	re, ok := err.(*resolveError)
	if !ok || !re.notFound {
		t.Fatalf("expected notFound error, got %v", err)
	}
}

func TestResolveRoute_LegacyAllowlistEnforced(t *testing.T) {
	setupRoutingTest(t)
	// 旧 key（无 group），白名单只允许 haiku
	km := &KeyMeta{Key: "k", AllowedModels: `["claude-haiku-4-5"]`}

	// 允许的模型 → 放行
	if _, err := ResolveRoute(km, "claude-haiku-4-5"); err != nil {
		t.Fatalf("haiku should be allowed: %v", err)
	}
	// 不在白名单 → forbidden（403），不能放行（这是审查发现的越权回归）
	_, err := ResolveRoute(km, "claude-opus-4-5")
	re, ok := err.(*resolveError)
	if !ok || !re.forbidden {
		t.Fatalf("opus must be forbidden for haiku-only key, got %v", err)
	}
}

func TestResolveRoute_LegacyUnknownModel404(t *testing.T) {
	setupRoutingTest(t)
	km := &KeyMeta{Key: "k", AllowedModels: "*"}
	_, err := ResolveRoute(km, "totally-made-up")
	re, ok := err.(*resolveError)
	if !ok || !re.notFound {
		t.Fatalf("unknown model must be 404, got %v", err)
	}
}

func TestResolveRoute_LegacyDefaultProvider(t *testing.T) {
	setupRoutingTest(t)
	km := &KeyMeta{Key: "k", AllowedModels: "*"}
	rt, err := ResolveRoute(km, "claude-opus-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider.Name != legacyProviderName || rt.UpstreamID != "claude-opus-4-5" {
		t.Fatalf("legacy key should route to default provider with verbatim model: %+v", rt)
	}
}

func TestDefaultRoute_GroupNoMasterKeyLeak(t *testing.T) {
	setupRoutingTest(t)
	// 绑组但该组没有任何映射 → 无 model 请求必须报配置错误，绝不回落
	emptyGid := int64(99)
	km := &KeyMeta{Key: "k", GroupID: &emptyGid}
	_, err := defaultRoute(km)
	if err == nil {
		t.Fatal("group with no mappings must fail closed, not fall back to a provider")
	}
}

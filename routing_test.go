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
		passthru:   map[int64]int64{},
	}
	p := &Provider{ID: 1, Name: legacyProviderName, BaseURL: "https://api.anthropic.com", APIKey: "k", Format: "anthropic", Enabled: true}
	providerRegistry.providers[1] = p
	providerRegistry.byName[legacyProviderName] = p

	// 组 10：逻辑名 sonnet → provider 1 / 真实名 real-sonnet
	gm := GroupMapping{ID: 1, GroupID: 10, LogicalName: "sonnet", ProviderID: 1, UpstreamID: "real-sonnet", IsPrimary: true}
	providerRegistry.byLogical[10] = map[string][]GroupMapping{"sonnet": {gm}}
	providerRegistry.byUpstream[10] = map[string]GroupMapping{"real-sonnet": gm}
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

// 组没有透传服务商时，未命中映射的模型必须 404（不静默放行）。
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

// 设了透传服务商的组：未命中映射的模型原样透传给透传服务商。
func TestResolveRoute_GroupPassthrough(t *testing.T) {
	setupRoutingTest(t)
	providerRegistry.passthru[10] = 1 // 组 10 透传给 provider 1
	gid := int64(10)
	km := &KeyMeta{Key: "k", GroupID: &gid}
	rt, err := ResolveRoute(km, "some-unmapped-model")
	if err != nil {
		t.Fatalf("passthrough should route, got %v", err)
	}
	if rt.Provider.ID != 1 || rt.UpstreamID != "some-unmapped-model" {
		t.Fatalf("passthrough wrong target: %+v", rt)
	}
}

func TestDefaultRoute_GroupNoMasterKeyLeak(t *testing.T) {
	setupRoutingTest(t)
	// 绑组但该组既无映射也无透传 → 无 model 请求必须报配置错误，绝不回落
	emptyGid := int64(99)
	km := &KeyMeta{Key: "k", GroupID: &emptyGid}
	_, err := defaultRoute(km)
	if err == nil {
		t.Fatal("group with no mappings/passthrough must fail closed, not fall back to a provider")
	}
}

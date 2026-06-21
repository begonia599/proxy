package main

import (
	"path/filepath"
	"testing"
)

// newTempStore 开一个临时 SQLite store（含完整 schema + migrations），测试用。
func newTempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestEnsureDefaultGroup_MigratesLegacyKeys 验证模型层合并的核心迁移：
// 建默认透传组 → 指向 anthropic-official → 把未绑组的旧 key 迁进去。
func TestEnsureDefaultGroup_MigratesLegacyKeys(t *testing.T) {
	s := newTempStore(t)

	// 旧世界：一个 anthropic-official 服务商 + 一个未绑组的旧 key
	pid, err := s.CreateProvider(&Provider{Name: legacyProviderName, BaseURL: upstreamURL, APIKey: "k", Format: "anthropic", Enabled: true})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := s.CreateKey(&KeyMeta{Key: "sk-proxy-legacy", Owner: "o", Creator: "admin", AllowedModels: "*"}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	ensureDefaultGroup(s)

	// 默认组存在且透传指向 anthropic-official
	g, err := s.GetGroupByName(defaultGroupName)
	if err != nil {
		t.Fatalf("default group missing: %v", err)
	}
	if g.PassthroughProviderID == nil || *g.PassthroughProviderID != pid {
		t.Fatalf("default group passthrough = %v, want %d", g.PassthroughProviderID, pid)
	}

	// 旧 key 已迁进默认组
	k, err := s.GetKey("sk-proxy-legacy")
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if k.GroupID == nil || *k.GroupID != g.ID {
		t.Fatalf("legacy key group_id = %v, want %d", k.GroupID, g.ID)
	}

	// 幂等：再跑一次不应报错也不重复建组
	ensureDefaultGroup(s)
	groups, _ := s.ListGroups()
	n := 0
	for _, gg := range groups {
		if gg.Name == defaultGroupName {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 default group, got %d", n)
	}
}

// TestResolveRoute_Passthrough 验证：绑默认透传组的 key 请求任意模型，
// 原样透传给透传服务商（复刻旧 group_id=NULL 行为）。
func TestResolveRoute_Passthrough(t *testing.T) {
	s := newTempStore(t)
	store = s
	t.Cleanup(func() { store = nil })

	pid, err := s.CreateProvider(&Provider{Name: legacyProviderName, BaseURL: upstreamURL, APIKey: "k", Format: "anthropic", Enabled: true})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	ensureDefaultGroup(s)

	providerRegistry = NewProviderRegistry(s)
	if err := providerRegistry.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(func() { providerRegistry = nil })

	gid := providerRegistry.DefaultGroupID()
	if gid == 0 {
		t.Fatal("default group id not loaded into registry")
	}
	km := &KeyMeta{Key: "k", GroupID: &gid}

	// 任意没有显式映射的模型 → 透传给 anthropic-official，名字不变
	rt, err := ResolveRoute(km, "claude-opus-4-5-20251101")
	if err != nil {
		t.Fatalf("passthrough route failed: %v", err)
	}
	if rt.Provider.ID != pid {
		t.Fatalf("routed to provider %d, want %d", rt.Provider.ID, pid)
	}
	if rt.UpstreamID != "claude-opus-4-5-20251101" {
		t.Fatalf("passthrough should keep model verbatim, got %q", rt.UpstreamID)
	}
}

// TestDeleteGroup_KeysFallBackToDefault 验证删组时其 key 被解绑（group_id→NULL），
// 经 effectiveGroupID 回落默认透传组而非悬空 404。
func TestDeleteGroup_KeysFallBackToDefault(t *testing.T) {
	s := newTempStore(t)
	store = s
	t.Cleanup(func() { store = nil })

	pid, err := s.CreateProvider(&Provider{Name: legacyProviderName, BaseURL: upstreamURL, APIKey: "k", Format: "anthropic", Enabled: true})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	ensureDefaultGroup(s)

	// 自定义组 + 绑它的 key
	gid, err := s.CreateGroup(&Group{Name: "team-a"})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := s.CreateKey(&KeyMeta{Key: "sk-proxy-x", Owner: "o", Creator: "admin", GroupID: &gid}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	// 删组 → key 应被解绑
	if err := s.DeleteGroup(gid); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	k, err := s.GetKey("sk-proxy-x")
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if k.GroupID != nil {
		t.Fatalf("key should be unbound after group delete, got group_id=%v", *k.GroupID)
	}

	// 路由：解绑的 key 经默认透传组走通
	providerRegistry = NewProviderRegistry(s)
	if err := providerRegistry.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(func() { providerRegistry = nil })
	rt, err := ResolveRoute(k, "claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatalf("unbound key should route via default group, got %v", err)
	}
	if rt.Provider.ID != pid {
		t.Fatalf("routed to provider %d, want default %d", rt.Provider.ID, pid)
	}
}

// TestDeleteProvider_ClearsDanglingPassthrough 验证删服务商时，把它当透传出口的组
// passthrough_provider_id 被置 NULL（不留悬空引用）。
func TestDeleteProvider_ClearsDanglingPassthrough(t *testing.T) {
	s := newTempStore(t)
	pid, err := s.CreateProvider(&Provider{Name: legacyProviderName, BaseURL: upstreamURL, APIKey: "k", Format: "anthropic", Enabled: true})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	ensureDefaultGroup(s) // 默认组 passthrough → pid
	g, _ := s.GetGroupByName(defaultGroupName)
	if g.PassthroughProviderID == nil || *g.PassthroughProviderID != pid {
		t.Fatalf("precondition: default group should point at provider %d", pid)
	}

	if err := s.DeleteProvider(pid); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	g2, err := s.GetGroupByName(defaultGroupName)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if g2.PassthroughProviderID != nil {
		t.Fatalf("passthrough should be cleared after provider delete, got %v", *g2.PassthroughProviderID)
	}
}

// TestEnsureDefaultGroup_SkipsDisabledLegacy 验证：anthropic-official 存在但被停用时，
// 默认组透传回落到任一启用的服务商，而非钉死在停用的那个。
func TestEnsureDefaultGroup_SkipsDisabledLegacy(t *testing.T) {
	s := newTempStore(t)
	if _, err := s.CreateProvider(&Provider{Name: legacyProviderName, BaseURL: upstreamURL, APIKey: "k", Format: "anthropic", Enabled: false}); err != nil {
		t.Fatalf("create disabled legacy: %v", err)
	}
	enabledID, err := s.CreateProvider(&Provider{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "k", Format: "openai", Enabled: true})
	if err != nil {
		t.Fatalf("create enabled: %v", err)
	}
	ensureDefaultGroup(s)
	g, err := s.GetGroupByName(defaultGroupName)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if g.PassthroughProviderID == nil || *g.PassthroughProviderID != enabledID {
		t.Fatalf("default passthrough should be the enabled provider %d, got %v", enabledID, g.PassthroughProviderID)
	}
}

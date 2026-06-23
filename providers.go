// providers.go: ProviderRegistry — 多上游服务商 + 小组映射的运行时缓存。
//
// 仿 KeyCache：DB 是真相源，这里持内存索引供路由热路径 O(1) 查询。
// 缓存两类东西：
//  1. providers：id → *Provider（base_url / key / format / enabled）
//  2. 小组映射的两个索引（给 routing.go 用）：
//     byLogical[gid][logical] → 有序映射列表（取 is_primary 或最高优先级）
//     byUpstream[gid][upstreamID] → 映射（具体名兼容回退）
//
// CRUD 后调用 Reload() 刷新。模型库存（provider_models）不进缓存——只配置页用，
// 直接查 DB。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

type ProviderRegistry struct {
	mu         sync.RWMutex
	providers  map[int64]*Provider
	byName     map[string]*Provider                // name → provider（给默认上游回落用，避免热路径查 DB）
	byLogical  map[int64]map[string][]GroupMapping // gid → logical → 有序映射
	byUpstream map[int64]map[string]GroupMapping   // gid → upstreamID → 映射（取第一条）
	passthru   map[int64]int64                     // gid → 透传服务商 id（未命中显式映射时原样透传）
	defGroupID int64                               // 名为 "default" 的组 id（新 key 默认绑定）
	priceIdx   map[int64]map[string]Price          // providerID → upstreamID → 计价覆盖
	store      *Store
	client     *http.Client
}

func NewProviderRegistry(s *Store) *ProviderRegistry {
	return &ProviderRegistry{
		providers:  map[int64]*Provider{},
		byName:     map[string]*Provider{},
		byLogical:  map[int64]map[string][]GroupMapping{},
		byUpstream: map[int64]map[string]GroupMapping{},
		passthru:   map[int64]int64{},
		priceIdx:   map[int64]map[string]Price{},
		store:      s,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Reload 从 DB 重建 providers 缓存 + 小组映射索引 + 小组透传索引。
func (r *ProviderRegistry) Reload() error {
	provs, err := r.store.ListProviders()
	if err != nil {
		return err
	}
	maps, err := r.store.ListAllGroupMappings()
	if err != nil {
		return err
	}
	groups, err := r.store.ListGroups()
	if err != nil {
		return err
	}
	models, err := r.store.ListAllProviderModels()
	if err != nil {
		return err
	}

	provIdx := make(map[int64]*Provider, len(provs))
	nameIdx := make(map[string]*Provider, len(provs))
	for i := range provs {
		provIdx[provs[i].ID] = &provs[i]
		nameIdx[provs[i].Name] = &provs[i]
	}

	passthru := make(map[int64]int64, len(groups))
	var defGroupID int64
	for i := range groups {
		if groups[i].PassthroughProviderID != nil {
			passthru[groups[i].ID] = *groups[i].PassthroughProviderID
		}
		if groups[i].Name == defaultGroupName {
			defGroupID = groups[i].ID
		}
	}

	// 计价覆盖索引：有 price_input 的库存行视为已设覆盖（其余项 NULL → 0）。
	priceIdx := map[int64]map[string]Price{}
	for i := range models {
		m := &models[i]
		if m.PriceInput == nil {
			continue
		}
		if priceIdx[m.ProviderID] == nil {
			priceIdx[m.ProviderID] = map[string]Price{}
		}
		priceIdx[m.ProviderID][m.UpstreamID] = Price{
			Input:        derefFloat(m.PriceInput),
			Output:       derefFloat(m.PriceOutput),
			CacheWrite5m: derefFloat(m.PriceCacheWrite5m),
			CacheWrite1h: derefFloat(m.PriceCacheWrite1h),
			CacheRead:    derefFloat(m.PriceCacheRead),
		}
	}

	byLogical := map[int64]map[string][]GroupMapping{}
	byUpstream := map[int64]map[string]GroupMapping{}
	for _, m := range maps {
		if byLogical[m.GroupID] == nil {
			byLogical[m.GroupID] = map[string][]GroupMapping{}
		}
		byLogical[m.GroupID][m.LogicalName] = append(byLogical[m.GroupID][m.LogicalName], m)

		if byUpstream[m.GroupID] == nil {
			byUpstream[m.GroupID] = map[string]GroupMapping{}
		}
		// 具体名回退只需任一条；优先记 is_primary，否则保留先到的
		if existing, ok := byUpstream[m.GroupID][m.UpstreamID]; !ok || (m.IsPrimary && !existing.IsPrimary) {
			byUpstream[m.GroupID][m.UpstreamID] = m
		}
	}
	// 每个 logical 的映射列表排序：is_primary 优先，其次 priority 升序，再 id
	for _, byLog := range byLogical {
		for _, list := range byLog {
			sort.SliceStable(list, func(i, j int) bool {
				if list[i].IsPrimary != list[j].IsPrimary {
					return list[i].IsPrimary // primary 排前
				}
				if list[i].Priority != list[j].Priority {
					return list[i].Priority < list[j].Priority
				}
				return list[i].ID < list[j].ID
			})
		}
	}

	r.mu.Lock()
	r.providers = provIdx
	r.byName = nameIdx
	r.byLogical = byLogical
	r.byUpstream = byUpstream
	r.passthru = passthru
	r.defGroupID = defGroupID
	r.priceIdx = priceIdx
	r.mu.Unlock()
	return nil
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// Provider 按 id 取服务商。
func (r *ProviderRegistry) Provider(id int64) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// ProviderByName 按名字取服务商（默认上游回落用，O(1)，不查 DB）。
func (r *ProviderRegistry) ProviderByName(name string) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[name]
	return p, ok
}

// FirstEnabledProvider 返回任一启用的服务商（默认上游兜底，按 id 稳定取最小）。
func (r *ProviderRegistry) FirstEnabledProvider() (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best *Provider
	for _, p := range r.providers {
		if !p.Enabled {
			continue
		}
		if best == nil || p.ID < best.ID {
			best = p
		}
	}
	return best, best != nil
}

// PrimaryMapping 返回某小组里 logical 当前主用的映射。
// 优先 is_primary；没有标主用的就取排序后第一条（避免管理员忘设主用导致死路由）。
func (r *ProviderRegistry) PrimaryMapping(groupID int64, logical string) (GroupMapping, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.byLogical[groupID][logical]
	if len(list) == 0 {
		return GroupMapping{}, false
	}
	return list[0], true // 已按 primary 优先排序
}

// MappingByUpstream 具体名兼容：下游直接发上游真实模型名时按此回退。
func (r *ProviderRegistry) MappingByUpstream(groupID int64, upstreamID string) (GroupMapping, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byUpstream[groupID][upstreamID]
	return m, ok
}

// PassthroughProvider 返回某小组的透传服务商（设了且仍启用才返回）。
// 命中时：组内未显式映射的模型，原样透传给它（复刻旧 group_id=NULL 行为）。
func (r *ProviderRegistry) PassthroughProvider(groupID int64) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pid, ok := r.passthru[groupID]
	if !ok {
		return nil, false
	}
	p, ok := r.providers[pid]
	if !ok || !p.Enabled {
		return nil, false
	}
	return p, true
}

// DefaultGroupID 返回名为 "default" 的组 id（新 key 未指定组时默认绑定）。
// 返回 0 表示默认组尚未建立。
func (r *ProviderRegistry) DefaultGroupID() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defGroupID
}

// PriceFor 返回某 (服务商名, 上游真实模型名) 的计价覆盖。
// ok=false 表示没设覆盖 → 调用方应回落静态 Anthropic 价表。
func (r *ProviderRegistry) PriceFor(providerName, upstreamModel string) (Price, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[providerName]
	if !ok {
		return Price{}, false
	}
	pr, ok := r.priceIdx[p.ID][upstreamModel]
	return pr, ok
}

// LogicalNames 返回某小组对外暴露的全部逻辑名（给 /v1/models 列表用）。
func (r *ProviderRegistry) LogicalNames(groupID int64) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byLog := r.byLogical[groupID]
	out := make([]string, 0, len(byLog))
	for name := range byLog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// GroupModelIDs 返回某小组对外可用的全部模型名：显式映射的逻辑名 ∪
// （若设了透传服务商）该服务商的全部库存真实名。给统一的 /v1/models 列表用，
// 取代旧的 curated_models 列表。透传组（如默认组）没有显式映射时，
// 列表即等于透传服务商的库存——复刻旧 curated_models 的呈现。
func (r *ProviderRegistry) GroupModelIDs(groupID int64) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range r.LogicalNames(groupID) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if p, ok := r.PassthroughProvider(groupID); ok && r.store != nil {
		if models, err := r.store.ListProviderModels(p.ID); err == nil {
			for _, m := range models {
				if !seen[m.UpstreamID] {
					seen[m.UpstreamID] = true
					out = append(out, m.UpstreamID)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// GroupAnyProvider 返回某小组任一映射指向的、当前启用的服务商。
// 给"无 model 字段"的请求兜底用：绝不回落主 key，宁可走组内某个真实上游。
func (r *ProviderRegistry) GroupAnyProvider(groupID int64) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, list := range r.byLogical[groupID] {
		for _, m := range list {
			if p, ok := r.providers[m.ProviderID]; ok && p.Enabled {
				return p, true
			}
		}
	}
	return nil, false
}

// UpstreamCatalog 拉取某服务商上游 /v1/models 的实时模型 id 列表，**不写库**。
// 给配置页"拉取上游模型 → 勾选加入大组"用。Anthropic 与 OpenAI 的响应都是
// {data:[{id,...}]}，鉴权头不同。
func (r *ProviderRegistry) UpstreamCatalog(p *Provider) ([]string, error) {
	baseURL := providerAnthropicBaseURL(p)
	if providerSupportsOpenAI(p) {
		// hybrid 默认用 OpenAI 目录拉取；多数兼容供应商的 /v1/models 更接近 OpenAI 形态。
		baseURL = providerOpenAIBaseURL(p)
	}
	url := baseURL + "/v1/models?limit=100"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if providerSupportsOpenAI(p) {
		req.Header.Set("authorization", "Bearer "+p.APIKey)
	} else {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider %s models list: status %d", p.Name, resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// RefreshSeen 后台心跳：拉上游目录，只为**已加入大组**的模型刷新 last_seen
// （不自动添加新模型——大组由管理员手动策展）。上游已下架的模型 last_seen 会变旧，
// 配置页据此提示。
func (r *ProviderRegistry) RefreshSeen(p *Provider) error {
	ids, err := r.UpstreamCatalog(p)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, id := range ids {
		// TouchProviderModel 只 UPDATE 已存在行，不存在则 0 行受影响（不新增）。
		if err := r.store.TouchProviderModel(p.ID, id, now); err != nil {
			log.Printf("touch provider_model %s/%s: %v", p.Name, id, err)
		}
	}
	return nil
}

// RefreshSeenAll 给所有 enabled 服务商刷一次已加入模型的 last_seen。
func (r *ProviderRegistry) RefreshSeenAll() {
	provs, err := r.store.ListProviders()
	if err != nil {
		log.Printf("provider refresh-seen-all: list failed: %v", err)
		return
	}
	for i := range provs {
		if !provs[i].Enabled {
			continue
		}
		if err := r.RefreshSeen(&provs[i]); err != nil {
			log.Printf("provider refresh-seen %s: %v", provs[i].Name, err)
		}
	}
}

// RunPeriodic 每 interval 刷新一次各服务商已加入模型的 last_seen。
func (r *ProviderRegistry) RunPeriodic(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			r.RefreshSeenAll()
		}
	}()
}

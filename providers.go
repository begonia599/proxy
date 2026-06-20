// providers.go: ProviderRegistry — 多上游服务商 + 小组映射的运行时缓存。
//
// 仿 KeyCache / ModelRegistry：DB 是真相源，这里持内存索引供路由热路径 O(1) 查询。
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
	store      *Store
	client     *http.Client
}

func NewProviderRegistry(s *Store) *ProviderRegistry {
	return &ProviderRegistry{
		providers:  map[int64]*Provider{},
		byName:     map[string]*Provider{},
		byLogical:  map[int64]map[string][]GroupMapping{},
		byUpstream: map[int64]map[string]GroupMapping{},
		store:      s,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Reload 从 DB 重建 providers 缓存 + 小组映射索引。
func (r *ProviderRegistry) Reload() error {
	provs, err := r.store.ListProviders()
	if err != nil {
		return err
	}
	maps, err := r.store.ListAllGroupMappings()
	if err != nil {
		return err
	}

	provIdx := make(map[int64]*Provider, len(provs))
	nameIdx := make(map[string]*Provider, len(provs))
	for i := range provs {
		provIdx[provs[i].ID] = &provs[i]
		nameIdx[provs[i].Name] = &provs[i]
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
	r.mu.Unlock()
	return nil
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

// Refresh 拉取某服务商的模型列表，upsert 进 provider_models（大组库存）。
// Anthropic 与 OpenAI 的 /v1/models 响应都是 {data:[{id,...}]}，鉴权头不同。
func (r *ProviderRegistry) Refresh(p *Provider) error {
	url := p.BaseURL + "/v1/models?limit=100"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	switch p.Format {
	case "openai":
		req.Header.Set("authorization", "Bearer "+p.APIKey)
	default: // anthropic
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider %s models list: status %d", p.Name, resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}

	now := time.Now()
	n := 0
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		if err := r.store.UpsertProviderModel(p.ID, m.ID, now); err != nil {
			log.Printf("upsert provider_model %s/%s: %v", p.Name, m.ID, err)
			continue
		}
		n++
	}
	log.Printf("provider %s: discovered %d models", p.Name, n)
	return nil
}

// RefreshAll 刷新所有 enabled 服务商的模型库存。
func (r *ProviderRegistry) RefreshAll() {
	provs, err := r.store.ListProviders()
	if err != nil {
		log.Printf("provider refresh-all: list failed: %v", err)
		return
	}
	for i := range provs {
		if !provs[i].Enabled {
			continue
		}
		if err := r.Refresh(&provs[i]); err != nil {
			log.Printf("provider refresh %s: %v", provs[i].Name, err)
		}
	}
}

// RunPeriodic 每 interval 刷新一次全部服务商模型库存。
func (r *ProviderRegistry) RunPeriodic(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			r.RefreshAll()
		}
	}()
}

// models.go: 模型注册表
//
// 启动时从 /v1/models 拉一次官方列表，后台每 30 分钟刷新。
// 每个上游 ID upsert 进 curated_models 表，管理员可以单独 enable/disable。
// 请求入口用 IsKnown 校验：未知模型或被管理员禁用都直接 404，不浪费上游一跳。
//
// 失败优雅降级：注册表为空时 IsKnown 返回 true（fail-open），
// 让真正的校验回落到 Anthropic 本身。
//
// 同时缓存上游 /v1/models 完整响应（每个 entry 的原始 JSON），
// 用来给 FilteredList 拼"该 key 实际能用的模型列表"，避免客户端 SDK
// 列模型看到全部、调用时被拒的混乱。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// modelEntry 一个上游模型条目，保留原始 JSON 用于按需重发。
type modelEntry struct {
	ID  string
	Raw json.RawMessage
}

type ModelRegistry struct {
	mu         sync.RWMutex
	valid      map[string]bool
	fullList   []modelEntry // 保留上游顺序，给 FilteredList 用
	lastUpdate time.Time
	store      *Store
}

func NewModelRegistry(s *Store) *ModelRegistry {
	return &ModelRegistry{valid: make(map[string]bool), store: s}
}

func (r *ModelRegistry) IsKnown(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.valid) == 0 {
		return true // 注册表未初始化 → 不拦截
	}
	return r.valid[model]
}

func (r *ModelRegistry) Snapshot() ([]string, time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.valid))
	for m := range r.valid {
		out = append(out, m)
	}
	return out, r.lastUpdate
}

// ReloadFromDB 从 curated_models 表里拉 enabled=1 的模型刷新内存映射。
// admin 切换 enable 后必须调用一次。
func (r *ModelRegistry) ReloadFromDB() error {
	if r.store == nil {
		return nil
	}
	ids, err := r.store.EnabledModelIDs()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.valid = ids
	r.lastUpdate = time.Now()
	r.mu.Unlock()
	return nil
}

func (r *ModelRegistry) Refresh(realKey string) error {
	req, err := http.NewRequest("GET", upstreamURL+"/v1/models?limit=100", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", realKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 用 RawMessage 保留每个 entry 的完整 JSON（含 display_name、capabilities 等），
	// 给前端 SDK 反序列化用。
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}

	now := time.Now()
	entries := make([]modelEntry, 0, len(body.Data))
	for _, raw := range body.Data {
		var meta struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil || meta.ID == "" {
			continue
		}
		entries = append(entries, modelEntry{ID: meta.ID, Raw: raw})
		if r.store != nil {
			if err := r.store.UpsertCuratedModel(meta.ID, now); err != nil {
				log.Printf("upsert curated model %s: %v", meta.ID, err)
			}
		}
	}

	if err := r.ReloadFromDB(); err != nil {
		return err
	}

	// 上游偶尔返回 0 条（限流/瞬时故障），别用空列表覆盖好的缓存
	if len(entries) > 0 {
		r.mu.Lock()
		r.fullList = entries
		r.mu.Unlock()
	}

	r.mu.RLock()
	n := len(r.valid)
	r.mu.RUnlock()
	log.Printf("model registry: %d enabled models (upstream returned %d)", n, len(body.Data))
	return nil
}

func (r *ModelRegistry) RunPeriodic(realKey string, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if err := r.Refresh(realKey); err != nil {
				log.Printf("model registry refresh: %v", err)
			}
		}
	}()
}

// FilteredList 返回"该 key 实际可用模型"的 Anthropic /v1/models 格式响应。
// 过滤条件：curated.enabled = true AND allowed(id) = true。
// 缓存为空时返回 nil，调用方应回落到原始上游转发。
func (r *ModelRegistry) FilteredList(allowed func(id string) bool) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.fullList) == 0 {
		return nil
	}

	data := make([]json.RawMessage, 0, len(r.fullList))
	var first, last string
	for _, e := range r.fullList {
		if !r.valid[e.ID] || !allowed(e.ID) {
			continue
		}
		if first == "" {
			first = e.ID
		}
		last = e.ID
		data = append(data, e.Raw)
	}

	out := map[string]any{
		"data":     data,
		"has_more": false,
		"first_id": first,
		"last_id":  last,
	}
	b, _ := json.Marshal(out)
	return b
}

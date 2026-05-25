// models.go: 模型注册表
//
// 启动时从 /v1/models 拉一次官方列表，后台每 30 分钟刷新。
// 每个上游 ID upsert 进 curated_models 表，管理员可以单独 enable/disable。
// 请求入口用 IsKnown 校验：未知模型或被管理员禁用都直接 404，不浪费上游一跳。
//
// 失败优雅降级：注册表为空时 IsKnown 返回 true（fail-open），
// 让真正的校验回落到 Anthropic 本身。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

type ModelRegistry struct {
	mu         sync.RWMutex
	valid      map[string]bool
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

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}

	now := time.Now()
	if r.store != nil {
		for _, m := range body.Data {
			if err := r.store.UpsertCuratedModel(m.ID, now); err != nil {
				log.Printf("upsert curated model %s: %v", m.ID, err)
			}
		}
	}

	if err := r.ReloadFromDB(); err != nil {
		return err
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

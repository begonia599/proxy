package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const demoTTL = 2 * time.Hour

type DemoStore struct {
	mu   sync.Mutex
	path string
}

type demoState struct {
	Keys   []demoKey   `json:"keys"`
	Events []demoEvent `json:"events"`
}

type demoKey struct {
	ID          string  `json:"id"`
	Key         string  `json:"key"`
	Owner       string  `json:"owner"`
	Model       string  `json:"model"`
	DailyBudget float64 `json:"daily_budget"`
	Notes       string  `json:"notes,omitempty"`
	GroupID     *int64  `json:"group_id,omitempty"`
	CreatedAt   int64   `json:"created_at"`
	ExpiresAt   int64   `json:"expires_at"`
	Seed        bool    `json:"seed,omitempty"`
}

type demoEvent struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	CreatedAt int64  `json:"created_at"`
	Seed      bool   `json:"seed,omitempty"`
}

func NewDemoStore(path string) *DemoStore {
	return &DemoStore{path: path}
}

func (s *DemoStore) Load() (demoState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *DemoStore) AddKey(owner, model string, budget float64, notes string, groupID *int64) (demoKey, demoState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return demoKey{}, st, err
	}
	now := time.Now()
	k := demoKey{
		ID:          randomHex(6),
		Key:         "sk-demo-" + randomHex(18),
		Owner:       cleanDemoText(owner, "访客"),
		Model:       cleanDemoText(model, "gpt-5.5"),
		DailyBudget: budget,
		Notes:       cleanDemoText(notes, ""),
		GroupID:     groupID,
		CreatedAt:   now.UnixMilli(),
		ExpiresAt:   now.Add(demoTTL).UnixMilli(),
	}
	st.Keys = append([]demoKey{k}, st.Keys...)
	st.Events = append([]demoEvent{{
		Type:      "key_created",
		Message:   fmt.Sprintf("%s 创建了 %s · 预算 $%.0f", k.Owner, k.Model, k.DailyBudget),
		CreatedAt: now.UnixMilli(),
	}}, st.Events...)
	st = resetDemoSeed(trimDemoState(st, now), now)
	if err := s.saveLocked(st); err != nil {
		return demoKey{}, st, err
	}
	return k, st, nil
}

type demoKeyPatch struct {
	Owner       *string  `json:"owner,omitempty"`
	DailyBudget *float64 `json:"daily_budget,omitempty"`
	Notes       *string  `json:"notes,omitempty"`
	GroupID     *int64   `json:"group_id,omitempty"`
	ClearGroup  bool     `json:"clear_group,omitempty"`
}

func (s *DemoStore) UpdateKey(id string, patch demoKeyPatch) (demoKey, demoState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return demoKey{}, st, err
	}
	now := time.Now()
	for i := range st.Keys {
		if st.Keys[i].ID != id && st.Keys[i].Key != id {
			continue
		}
		if patch.Owner != nil {
			st.Keys[i].Owner = cleanDemoText(*patch.Owner, "访客")
		}
		if patch.DailyBudget != nil {
			st.Keys[i].DailyBudget = *patch.DailyBudget
		}
		if patch.Notes != nil {
			st.Keys[i].Notes = cleanDemoText(*patch.Notes, "")
		}
		if patch.ClearGroup {
			st.Keys[i].GroupID = nil
		} else if patch.GroupID != nil {
			st.Keys[i].GroupID = patch.GroupID
		}
		st.Events = append([]demoEvent{{
			Type:      "key_updated",
			Message:   st.Keys[i].Owner + " 更新了 demo key 设置",
			CreatedAt: now.UnixMilli(),
		}}, st.Events...)
		st = resetDemoSeed(trimDemoState(st, now), now)
		if err := s.saveLocked(st); err != nil {
			return demoKey{}, st, err
		}
		for _, k := range st.Keys {
			if k.ID == id || k.Key == id {
				return k, st, nil
			}
		}
		return demoKey{}, st, nil
	}
	return demoKey{}, st, fmt.Errorf("demo key not found")
}

func (s *DemoStore) DeleteKey(id string) (demoState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return st, err
	}
	now := time.Now()
	next := st.Keys[:0]
	removed := ""
	for _, k := range st.Keys {
		if k.ID == id || k.Key == id {
			removed = k.Owner
			continue
		}
		next = append(next, k)
	}
	st.Keys = next
	if removed != "" {
		st.Events = append([]demoEvent{{
			Type:      "key_deleted",
			Message:   removed + " 撤销了一枚 demo key",
			CreatedAt: now.UnixMilli(),
		}}, st.Events...)
	}
	st = resetDemoSeed(trimDemoState(st, now), now)
	return st, s.saveLocked(st)
}

func (s *DemoStore) loadLocked() (demoState, error) {
	var st demoState
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			now := time.Now()
			st = resetDemoSeed(st, now)
			return st, s.saveLocked(st)
		}
		return st, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &st); err != nil {
			return st, err
		}
	}
	st = resetDemoSeed(trimDemoState(st, time.Now()), time.Now())
	_ = s.saveLocked(st)
	return st, nil
}

func (s *DemoStore) saveLocked(st demoState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil && filepath.Dir(s.path) != "." {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0600)
}

func trimDemoState(st demoState, now time.Time) demoState {
	cutoff := now.Add(-demoTTL).UnixMilli()
	keys := st.Keys[:0]
	for _, k := range st.Keys {
		if k.Seed || k.CreatedAt >= cutoff {
			keys = append(keys, k)
		}
	}
	events := st.Events[:0]
	for _, e := range st.Events {
		if e.Seed || e.CreatedAt >= cutoff {
			events = append(events, e)
		}
	}
	if len(events) > 40 {
		events = events[:40]
	}
	st.Keys = keys
	st.Events = events
	return st
}

func seedDemoState() demoState {
	return seedDemoStateAt(time.Now())
}

func seedDemoStateAt(now time.Time) demoState {
	models := []string{"gpt-5.5", "claude-opus-4.8", "deepseek-reasoner", "glm-5.2-pro"}
	owners := []string{"demo-codex", "demo-claude-code", "demo-billing", "demo-provider"}
	ids := []string{"seed-codex", "seed-claude", "seed-billing", "seed-provider"}
	keys := []string{
		"sk-demo-codex-000000000000000000000000",
		"sk-demo-claude-00000000000000000000000",
		"sk-demo-billing-0000000000000000000000",
		"sk-demo-provider-000000000000000000000",
	}
	st := demoState{}
	for i := range models {
		created := now.Add(-time.Duration(i*17) * time.Minute)
		st.Keys = append(st.Keys, demoKey{
			ID:          ids[i],
			Key:         keys[i],
			Owner:       owners[i],
			Model:       models[i],
			DailyBudget: []float64{2000, 5000, 1200, 800}[i],
			Notes:       []string{"Codex 演示密钥", "Claude Code 演示密钥", "账单展示密钥", "服务商联调密钥"}[i],
			GroupID:     ptrInt64([]int64{2, 3, 1, 1}[i]),
			CreatedAt:   created.UnixMilli(),
			ExpiresAt:   now.Add(demoTTL).UnixMilli(),
			Seed:        true,
		})
	}
	st.Events = []demoEvent{
		{Type: "billing_spike", Message: "低缓存请求被风险摘要捕获", CreatedAt: now.Add(-9 * time.Minute).UnixMilli(), Seed: true},
		{Type: "provider_route", Message: "hybrid 服务商按入口协议透传", CreatedAt: now.Add(-22 * time.Minute).UnixMilli(), Seed: true},
		{Type: "key_created", Message: "demo-codex 创建了 gpt-5.5 key", CreatedAt: now.Add(-34 * time.Minute).UnixMilli(), Seed: true},
	}
	return st
}

func resetDemoSeed(st demoState, now time.Time) demoState {
	seed := seedDemoStateAt(now)
	keys := make([]demoKey, 0, len(st.Keys)+len(seed.Keys))
	for _, k := range st.Keys {
		if !isDemoSeedKey(k) {
			keys = append(keys, k)
		}
	}
	keys = append(keys, seed.Keys...)

	events := make([]demoEvent, 0, len(st.Events)+len(seed.Events))
	for _, e := range st.Events {
		if !isDemoSeedEvent(e) {
			events = append(events, e)
		}
	}
	events = append(events, seed.Events...)
	if len(events) > 40 {
		events = events[:40]
	}
	st.Keys = keys
	st.Events = events
	return st
}

func isDemoSeedKey(k demoKey) bool {
	if k.Seed {
		return true
	}
	switch k.Owner {
	case "demo-codex", "demo-claude-code", "demo-billing", "demo-provider":
		return true
	default:
		return false
	}
}

func isDemoSeedEvent(e demoEvent) bool {
	if e.Seed {
		return true
	}
	return e.Type == "billing_spike" || e.Type == "provider_route" ||
		(e.Type == "key_created" && strings.Contains(e.Message, "demo-codex 创建了"))
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func cleanDemoText(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	if len(v) > 48 {
		v = v[:48]
	}
	return v
}

func ptrInt64(v int64) *int64 {
	return &v
}

func demoStats(st demoState) map[string]any {
	keyCount := len(st.Keys)
	requests := 1292000 + keyCount*317
	input := 982000000 + keyCount*1234567
	output := 184000000 + keyCount*334455
	cacheRead := 843000000 + keyCount*114514
	cacheHit := float64(cacheRead) / float64(input+cacheRead)
	cost := 42891.37 + float64(keyCount)*83.42
	return map[string]any{
		"requests":        requests,
		"cost_usd":        cost,
		"web_search":      18420 + keyCount*11,
		"input_tokens":    input,
		"output_tokens":   output,
		"cache_read":      cacheRead,
		"cache_write_5m":  92100000 + keyCount*9988,
		"cache_write_1h":  18400000 + keyCount*2333,
		"errors":          17 + keyCount%3,
		"streaming":       641000 + keyCount*19,
		"cache_hit_rate":  cacheHit,
		"risk_hour_cost":  918.42 + float64(keyCount)*4.8,
		"risk_max_cost":   129.73,
		"low_cache_count": 8 + keyCount%4,
		"keys":            st.Keys,
		"events":          st.Events,
	}
}

func demoStateHandler(ds *DemoStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		st, err := ds.Load()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(demoStats(st))
	}
}

func demoKeysHandler(ds *DemoStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.Method {
		case http.MethodPost:
			var req struct {
				Owner       string  `json:"owner"`
				Model       string  `json:"model"`
				DailyBudget float64 `json:"daily_budget"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.DailyBudget <= 0 {
				req.DailyBudget = 1000
			}
			k, st, err := ds.AddKey(req.Owner, req.Model, req.DailyBudget, "", nil)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"key": k, "state": demoStats(st)})
		case http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/demo/keys/")
			st, err := ds.DeleteKey(id)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(demoStats(st))
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

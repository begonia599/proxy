// keys.go: proxy key 元数据 + DB 操作
//
// 表设计见 storage.go 的 proxy_keys。
// allowed_models 字段两种值：
//   "*"          表示无限制（受 curated_models.enabled 约束）
//   `["a","b"]`  JSON 数组，表示只允许这几个 model_id
//
// 启动时把全表加载到 KeyCache（内存 map），请求路径 O(1) 查询。
// CRUD 操作完后必须 keyCache.Reload() 刷新。
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type KeyMeta struct {
	Key           string     `json:"key"`
	Owner         string     `json:"owner"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	DailyBudget   float64    `json:"daily_budget"` // USD; 0 = unlimited
	AllowedModels string     `json:"allowed_models"`
	GroupID       *int64     `json:"group_id,omitempty"` // 绑定的小组；nil = 回落 allowed_models（旧行为）
	Notes         string     `json:"notes"`
}

// IsActive returns true if the key has not been revoked.
func (k *KeyMeta) IsActive() bool {
	return k.RevokedAt == nil
}

// ModelAllowed 判断给定 model_id 是否在该 key 的白名单内。
// 不检查 curated_models —— 那是另一层（这里只看 per-key 限制）。
func (k *KeyMeta) ModelAllowed(modelID string) bool {
	if k.AllowedModels == "" || k.AllowedModels == "*" {
		return true
	}
	var list []string
	if err := json.Unmarshal([]byte(k.AllowedModels), &list); err != nil {
		// 解析失败按全允许，避免管理员误改 DB 把朋友全锁死
		return true
	}
	for _, m := range list {
		if m == modelID {
			return true
		}
	}
	return false
}

// ---------- DB 操作 ----------

func (s *Store) ListKeys(includeRevoked bool) ([]KeyMeta, error) {
	q := "SELECT key, owner, created_at, revoked_at, daily_budget, allowed_models, group_id, notes FROM proxy_keys"
	if !includeRevoked {
		q += " WHERE revoked_at IS NULL"
	}
	q += " ORDER BY created_at DESC"
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyMeta
	for rows.Next() {
		var k KeyMeta
		var createdMs int64
		var revokedMs sql.NullInt64
		var groupID sql.NullInt64
		if err := rows.Scan(&k.Key, &k.Owner, &createdMs, &revokedMs,
			&k.DailyBudget, &k.AllowedModels, &groupID, &k.Notes); err != nil {
			return nil, err
		}
		k.CreatedAt = time.UnixMilli(createdMs)
		if revokedMs.Valid {
			t := time.UnixMilli(revokedMs.Int64)
			k.RevokedAt = &t
		}
		if groupID.Valid {
			g := groupID.Int64
			k.GroupID = &g
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) GetKey(key string) (*KeyMeta, error) {
	row := s.db.QueryRow(
		"SELECT key, owner, created_at, revoked_at, daily_budget, allowed_models, group_id, notes FROM proxy_keys WHERE key = ?",
		key)
	var k KeyMeta
	var createdMs int64
	var revokedMs sql.NullInt64
	var groupID sql.NullInt64
	if err := row.Scan(&k.Key, &k.Owner, &createdMs, &revokedMs,
		&k.DailyBudget, &k.AllowedModels, &groupID, &k.Notes); err != nil {
		return nil, err
	}
	k.CreatedAt = time.UnixMilli(createdMs)
	if revokedMs.Valid {
		t := time.UnixMilli(revokedMs.Int64)
		k.RevokedAt = &t
	}
	if groupID.Valid {
		g := groupID.Int64
		k.GroupID = &g
	}
	return &k, nil
}

func (s *Store) CreateKey(k *KeyMeta) error {
	if k.AllowedModels == "" {
		k.AllowedModels = "*"
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(
		"INSERT INTO proxy_keys (key, owner, created_at, daily_budget, allowed_models, group_id, notes) VALUES (?, ?, ?, ?, ?, ?, ?)",
		k.Key, k.Owner, k.CreatedAt.UnixMilli(), k.DailyBudget, k.AllowedModels, k.GroupID, k.Notes)
	return err
}

// UpdateKey 部分更新：传 nil 表示该字段不动。
type KeyUpdate struct {
	Owner         *string  `json:"owner,omitempty"`
	DailyBudget   *float64 `json:"daily_budget,omitempty"`
	AllowedModels *string  `json:"allowed_models,omitempty"`
	GroupID       *int64   `json:"group_id,omitempty"` // 见 ClearGroup：传 -1 表示解绑（置 NULL）
	ClearGroup    bool     `json:"clear_group,omitempty"`
	Notes         *string  `json:"notes,omitempty"`
}

func (s *Store) UpdateKey(key string, u KeyUpdate) error {
	sets := []string{}
	args := []any{}
	if u.Owner != nil {
		sets = append(sets, "owner = ?")
		args = append(args, *u.Owner)
	}
	if u.DailyBudget != nil {
		sets = append(sets, "daily_budget = ?")
		args = append(args, *u.DailyBudget)
	}
	if u.AllowedModels != nil {
		sets = append(sets, "allowed_models = ?")
		args = append(args, *u.AllowedModels)
	}
	if u.ClearGroup {
		sets = append(sets, "group_id = NULL")
	} else if u.GroupID != nil {
		sets = append(sets, "group_id = ?")
		args = append(args, *u.GroupID)
	}
	if u.Notes != nil {
		sets = append(sets, "notes = ?")
		args = append(args, *u.Notes)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, key)
	q := "UPDATE proxy_keys SET " + strings.Join(sets, ", ") + " WHERE key = ?"
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("key not found: %s", key)
	}
	return nil
}

func (s *Store) RevokeKey(key string) error {
	res, err := s.db.Exec(
		"UPDATE proxy_keys SET revoked_at = ? WHERE key = ? AND revoked_at IS NULL",
		time.Now().UnixMilli(), key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("key not found or already revoked: %s", key)
	}
	return nil
}

// TodaysCost 该 key 今天累计花费（按本地时区）。
func (s *Store) TodaysCost(key string) (float64, error) {
	// 本地日历日的 00:00:00 → unix ms
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	row := s.db.QueryRow(
		"SELECT COALESCE(SUM(cost_usd), 0) FROM requests WHERE proxy_key = ? AND ts >= ?",
		key, dayStart.UnixMilli())
	var v float64
	if err := row.Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// ---------- 内存缓存 ----------

type KeyCache struct {
	mu    sync.RWMutex
	m     map[string]*KeyMeta
	store *Store
}

func NewKeyCache(s *Store) *KeyCache { return &KeyCache{m: map[string]*KeyMeta{}, store: s} }

// Reload 从 DB 重新加载所有未撤销的 key。
func (c *KeyCache) Reload() error {
	keys, err := c.store.ListKeys(false)
	if err != nil {
		return err
	}
	fresh := make(map[string]*KeyMeta, len(keys))
	for i := range keys {
		fresh[keys[i].Key] = &keys[i]
	}
	c.mu.Lock()
	c.m = fresh
	c.mu.Unlock()
	return nil
}

func (c *KeyCache) Get(key string) (*KeyMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.m[key]
	return k, ok
}

func (c *KeyCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// ---------- key 生成 ----------

// GenerateProxyKey 返回 "sk-proxy-" + 32 字符（base32 lowercase, no padding）。
// 20 字节随机源 → 160 bit 熵 → 32 base32 字符。
func GenerateProxyKey() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return "sk-proxy-" + strings.ToLower(enc.EncodeToString(b)), nil
}

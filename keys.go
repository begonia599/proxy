// keys.go: proxy key 元数据 + DB 操作
//
// 表设计见 storage.go 的 proxy_keys。
// 模型可见性由 key 绑定的小组（GroupID）决定——组即白名单。
// allowed_models 字段已废弃（模型层合并后不再做 per-key 白名单），仅为兼容老 DB 保留，
// 新建 key 一律写 "*"。
//
// 启动时把全表加载到 KeyCache（内存 map），请求路径 O(1) 查询。
// CRUD 操作完后必须 keyCache.Reload() 刷新。
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"fmt"
	"strings"
	"sync"
	"time"
)

type KeyMeta struct {
	Key           string     `json:"key"`
	Owner         string     `json:"owner"`   // 备注/标签（如"给谁用"），可改
	Creator       string     `json:"creator"` // 归属 = 创建者，钉死不可转让不可改
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	DailyBudget   float64    `json:"daily_budget"`       // USD; 0 = unlimited
	AllowedModels string     `json:"allowed_models"`     // 已废弃：保留列兼容老 DB，恒为 "*"
	GroupID       *int64     `json:"group_id,omitempty"` // 绑定的小组 = 该 key 的模型白名单
	Notes         string     `json:"notes"`
	TodayRequests int64      `json:"today_requests"`
	TodayCost     float64    `json:"today_cost"`
	BudgetRemain  *float64   `json:"budget_remaining"` // nil = unlimited
}

// IsActive returns true if the key has not been revoked.
func (k *KeyMeta) IsActive() bool {
	return k.RevokedAt == nil
}

// ---------- DB 操作 ----------

const keyCols = "key, owner, creator, created_at, revoked_at, daily_budget, allowed_models, group_id, notes"

// scanKey 扫一行到 KeyMeta（creator 用 COALESCE 兜 NULL，旧数据迁移前可能是空）。
func scanKey(s interface {
	Scan(...any) error
}) (*KeyMeta, error) {
	var k KeyMeta
	var createdMs int64
	var revokedMs sql.NullInt64
	var groupID sql.NullInt64
	var creator sql.NullString
	if err := s.Scan(&k.Key, &k.Owner, &creator, &createdMs, &revokedMs,
		&k.DailyBudget, &k.AllowedModels, &groupID, &k.Notes); err != nil {
		return nil, err
	}
	k.Creator = creator.String
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

// ListKeys 列密钥。creatorFilter 非空时只返回该 creator 的（用户系统归属过滤）。
func (s *Store) ListKeys(includeRevoked bool, creatorFilter string) ([]KeyMeta, error) {
	q := "SELECT " + keyCols + " FROM proxy_keys"
	args := []any{}
	conds := []string{}
	if !includeRevoked {
		conds = append(conds, "revoked_at IS NULL")
	}
	if creatorFilter != "" {
		conds = append(conds, "creator = ?")
		args = append(args, creatorFilter)
	}
	if len(conds) > 0 {
		q += " WHERE " + joinAnd(conds)
	}
	q += " ORDER BY created_at DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyMeta
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *Store) GetKey(key string) (*KeyMeta, error) {
	row := s.db.QueryRow("SELECT "+keyCols+" FROM proxy_keys WHERE key = ?", key)
	return scanKey(row)
}

func (s *Store) CreateKey(k *KeyMeta) error {
	if k.AllowedModels == "" {
		k.AllowedModels = "*"
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now()
	}
	// 未指定小组 → 默认绑「默认透传组」，让新 key 也走统一的组路由
	// （而非 group_id=NULL 的 legacy 分支）。默认组未就绪时退回 NULL。
	if k.GroupID == nil && providerRegistry != nil {
		if gid := providerRegistry.DefaultGroupID(); gid > 0 {
			k.GroupID = &gid
		}
	}
	_, err := s.db.Exec(
		"INSERT INTO proxy_keys (key, owner, creator, created_at, daily_budget, allowed_models, group_id, notes) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		k.Key, k.Owner, k.Creator, k.CreatedAt.UnixMilli(), k.DailyBudget, k.AllowedModels, k.GroupID, k.Notes)
	return err
}

// UpdateKey 部分更新：传 nil 表示该字段不动。
type KeyUpdate struct {
	Owner       *string  `json:"owner,omitempty"`
	DailyBudget *float64 `json:"daily_budget,omitempty"`
	GroupID     *int64   `json:"group_id,omitempty"` // 见 ClearGroup：解绑后回落默认透传组
	ClearGroup  bool     `json:"clear_group,omitempty"`
	Notes       *string  `json:"notes,omitempty"`
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

// PopulateTodayUsage enriches admin-facing key rows with today's request count,
// spend, and remaining budget in one aggregate query.
func (s *Store) PopulateTodayUsage(list []KeyMeta) error {
	if len(list) == 0 {
		return nil
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	rows, err := s.db.Query(
		"SELECT proxy_key, COUNT(*), COALESCE(SUM(cost_usd), 0) FROM requests WHERE ts >= ? GROUP BY proxy_key",
		dayStart.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	type usage struct {
		requests int64
		cost     float64
	}
	byKey := make(map[string]usage)
	for rows.Next() {
		var key string
		var u usage
		if err := rows.Scan(&key, &u.requests, &u.cost); err != nil {
			return err
		}
		byKey[key] = u
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range list {
		u := byKey[list[i].Key]
		list[i].TodayRequests = u.requests
		list[i].TodayCost = u.cost
		if list[i].DailyBudget > 0 {
			remaining := list[i].DailyBudget - u.cost
			list[i].BudgetRemain = &remaining
		}
	}
	return nil
}

// ---------- 内存缓存 ----------

type KeyCache struct {
	mu    sync.RWMutex
	m     map[string]*KeyMeta
	store *Store
}

func NewKeyCache(s *Store) *KeyCache { return &KeyCache{m: map[string]*KeyMeta{}, store: s} }

// Reload 从 DB 重新加载所有未撤销的 key（全量，缓存供转发热路径用，与归属过滤无关）。
func (c *KeyCache) Reload() error {
	keys, err := c.store.ListKeys(false, "")
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

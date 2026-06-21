// providers_store.go: providers / provider_models / groups / group_mappings 的
// 数据类型 + SQLite CRUD。表结构见 storage.go。
//
// 设计与 keys.go 同风格：DB 方法挂在 *Store 上，运行时缓存在 providers.go 的
// ProviderRegistry 里。所有时间戳 unix 毫秒。
package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// ────────────────────── 类型 ──────────────────────

// Provider 一个上游服务商。APIKey 仅在内部使用；对外 JSON 由 admin 层脱敏。
type Provider struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	APIKey    string    `json:"-"`      // 永不直接序列化，admin 层用 MaskKey 单独给 masked 字段
	Format    string    `json:"format"` // anthropic | openai
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// ProviderModel 大组里的一条库存记录。Price* 为可选的按服务商计价覆盖
// （nil = 未设，计费时回落静态 Anthropic 价表）。单位 USD / 1M tokens。
type ProviderModel struct {
	ProviderID        int64     `json:"provider_id"`
	UpstreamID        string    `json:"upstream_id"`
	FirstSeenAt       time.Time `json:"first_seen_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	PriceInput        *float64  `json:"price_input,omitempty"`
	PriceOutput       *float64  `json:"price_output,omitempty"`
	PriceCacheWrite5m *float64  `json:"price_cache_write_5m,omitempty"`
	PriceCacheWrite1h *float64  `json:"price_cache_write_1h,omitempty"`
	PriceCacheRead    *float64  `json:"price_cache_read,omitempty"`
}

// Group 一个小组。
// PassthroughProviderID 非空时：组内未命中显式映射的模型，原样透传给该服务商
// （复刻旧 group_id=NULL 行为，也是「整家服务商全量放行」这一档的天然形态）。
type Group struct {
	ID                    int64     `json:"id"`
	Name                  string    `json:"name"`
	Notes                 string    `json:"notes"`
	PassthroughProviderID *int64    `json:"passthrough_provider_id,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

// GroupMapping 小组内一条逻辑名映射。
type GroupMapping struct {
	ID          int64  `json:"id"`
	GroupID     int64  `json:"group_id"`
	LogicalName string `json:"logical_name"`
	ProviderID  int64  `json:"provider_id"`
	UpstreamID  string `json:"upstream_id"`
	Priority    int    `json:"priority"`
	IsPrimary   bool   `json:"is_primary"`
}

// ────────────────────── providers ──────────────────────

const providerCols = "id, name, base_url, api_key, format, enabled, created_at"

func scanProvider(s interface {
	Scan(...any) error
}) (*Provider, error) {
	var p Provider
	var enabled int
	var createdMs int64
	if err := s.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Format, &enabled, &createdMs); err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = time.UnixMilli(createdMs)
	return &p, nil
}

func (s *Store) ListProviders() ([]Provider, error) {
	rows, err := s.db.Query("SELECT " + providerCols + " FROM providers ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Store) GetProvider(id int64) (*Provider, error) {
	row := s.db.QueryRow("SELECT "+providerCols+" FROM providers WHERE id = ?", id)
	return scanProvider(row)
}

// GetProviderByName 给迁移用（按名字查 anthropic-official 是否已存在）。
func (s *Store) GetProviderByName(name string) (*Provider, error) {
	row := s.db.QueryRow("SELECT "+providerCols+" FROM providers WHERE name = ?", name)
	return scanProvider(row)
}

func (s *Store) CreateProvider(p *Provider) (int64, error) {
	if p.Format == "" {
		p.Format = "anthropic"
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	res, err := s.db.Exec(
		"INSERT INTO providers (name, base_url, api_key, format, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		p.Name, strings.TrimRight(p.BaseURL, "/"), p.APIKey, p.Format, boolToInt(p.Enabled), p.CreatedAt.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ProviderUpdate 部分更新：nil 字段不动。
type ProviderUpdate struct {
	Name    *string `json:"name,omitempty"`
	BaseURL *string `json:"base_url,omitempty"`
	APIKey  *string `json:"api_key,omitempty"` // 空字符串 = 不改（避免脱敏值回写覆盖真 key）
	Format  *string `json:"format,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

func (s *Store) UpdateProvider(id int64, u ProviderUpdate) error {
	sets := []string{}
	args := []any{}
	if u.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *u.Name)
	}
	if u.BaseURL != nil {
		sets = append(sets, "base_url = ?")
		args = append(args, strings.TrimRight(*u.BaseURL, "/"))
	}
	if u.APIKey != nil && *u.APIKey != "" {
		sets = append(sets, "api_key = ?")
		args = append(args, *u.APIKey)
	}
	if u.Format != nil {
		sets = append(sets, "format = ?")
		args = append(args, *u.Format)
	}
	if u.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*u.Enabled))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	res, err := s.db.Exec("UPDATE providers SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("provider not found: %d", id)
	}
	return nil
}

// DeleteProvider 同时清理它的库存模型、引用它的映射、以及指向它的小组透传，避免悬挂。
func (s *Store) DeleteProvider(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM provider_models WHERE provider_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM group_mappings WHERE provider_id = ?", id); err != nil {
		return err
	}
	// 清掉把它当透传出口的小组（否则 passthrough_provider_id 悬空，组退化成仅映射可用）。
	if _, err := tx.Exec("UPDATE groups SET passthrough_provider_id = NULL WHERE passthrough_provider_id = ?", id); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM providers WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("provider not found: %d", id)
	}
	return tx.Commit()
}

// ────────────────────── provider_models（大组库存） ──────────────────────

// UpsertProviderModel 刷新时调用：第一次见插入，已见过只更新 last_seen_at。
func (s *Store) UpsertProviderModel(providerID int64, upstreamID string, seenAt time.Time) error {
	ms := seenAt.UnixMilli()
	_, err := s.db.Exec(`
INSERT INTO provider_models (provider_id, upstream_id, first_seen_at, last_seen_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(provider_id, upstream_id) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		providerID, upstreamID, ms, ms)
	return err
}

const providerModelCols = "provider_id, upstream_id, first_seen_at, last_seen_at, " +
	"price_input, price_output, price_cache_write_5m, price_cache_write_1h, price_cache_read"

func scanProviderModel(s interface {
	Scan(...any) error
}) (*ProviderModel, error) {
	var m ProviderModel
	var firstMs, lastMs int64
	if err := s.Scan(&m.ProviderID, &m.UpstreamID, &firstMs, &lastMs,
		&m.PriceInput, &m.PriceOutput, &m.PriceCacheWrite5m, &m.PriceCacheWrite1h, &m.PriceCacheRead); err != nil {
		return nil, err
	}
	m.FirstSeenAt = time.UnixMilli(firstMs)
	m.LastSeenAt = time.UnixMilli(lastMs)
	return &m, nil
}

func (s *Store) ListProviderModels(providerID int64) ([]ProviderModel, error) {
	rows, err := s.db.Query(
		"SELECT "+providerModelCols+" FROM provider_models WHERE provider_id = ? ORDER BY upstream_id",
		providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderModel
	for rows.Next() {
		m, err := scanProviderModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ListAllProviderModels 大组全量（给配置页"勾选模型"用）。
func (s *Store) ListAllProviderModels() ([]ProviderModel, error) {
	rows, err := s.db.Query(
		"SELECT " + providerModelCols + " FROM provider_models ORDER BY provider_id, upstream_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderModel
	for rows.Next() {
		m, err := scanProviderModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// SetProviderModelPrice 设/清某 (provider, upstream) 的计价覆盖。
// p == nil 清空（全部置 NULL，回落静态价表）；否则写入 5 项单价。
// 仅更新已存在的库存行（不存在 → not found，先刷新库存或确认真实名）。
func (s *Store) SetProviderModelPrice(providerID int64, upstreamID string, p *Price) error {
	var res interface {
		RowsAffected() (int64, error)
	}
	var err error
	if p == nil {
		res, err = s.db.Exec(`UPDATE provider_models SET
price_input = NULL, price_output = NULL, price_cache_write_5m = NULL,
price_cache_write_1h = NULL, price_cache_read = NULL
WHERE provider_id = ? AND upstream_id = ?`, providerID, upstreamID)
	} else {
		res, err = s.db.Exec(`UPDATE provider_models SET
price_input = ?, price_output = ?, price_cache_write_5m = ?,
price_cache_write_1h = ?, price_cache_read = ?
WHERE provider_id = ? AND upstream_id = ?`,
			p.Input, p.Output, p.CacheWrite5m, p.CacheWrite1h, p.CacheRead, providerID, upstreamID)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("provider model not found: provider=%d model=%s", providerID, upstreamID)
	}
	return nil
}

// ────────────────────── groups ──────────────────────

const groupCols = "id, name, notes, passthrough_provider_id, created_at"

func scanGroup(s interface {
	Scan(...any) error
}) (*Group, error) {
	var g Group
	var passthrough sql.NullInt64
	var createdMs int64
	if err := s.Scan(&g.ID, &g.Name, &g.Notes, &passthrough, &createdMs); err != nil {
		return nil, err
	}
	if passthrough.Valid {
		v := passthrough.Int64
		g.PassthroughProviderID = &v
	}
	g.CreatedAt = time.UnixMilli(createdMs)
	return &g, nil
}

func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.db.Query("SELECT " + groupCols + " FROM groups ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

func (s *Store) GetGroupByName(name string) (*Group, error) {
	row := s.db.QueryRow("SELECT "+groupCols+" FROM groups WHERE name = ?", name)
	return scanGroup(row)
}

func (s *Store) CreateGroup(g *Group) (int64, error) {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	res, err := s.db.Exec(
		"INSERT INTO groups (name, notes, passthrough_provider_id, created_at) VALUES (?, ?, ?, ?)",
		g.Name, g.Notes, g.PassthroughProviderID, g.CreatedAt.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateGroup 部分更新 name/notes。passthrough 单独走 SetGroupPassthrough
// （需要支持「清空」语义，不能用 nil=不动）。
func (s *Store) UpdateGroup(id int64, name, notes *string) error {
	sets := []string{}
	args := []any{}
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *name)
	}
	if notes != nil {
		sets = append(sets, "notes = ?")
		args = append(args, *notes)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	res, err := s.db.Exec("UPDATE groups SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("group not found: %d", id)
	}
	return nil
}

// GetGroup 按 id 取小组。
func (s *Store) GetGroup(id int64) (*Group, error) {
	row := s.db.QueryRow("SELECT "+groupCols+" FROM groups WHERE id = ?", id)
	return scanGroup(row)
}

// SetGroupPassthrough 设/清小组的透传服务商（nil = 清空，关闭透传）。
func (s *Store) SetGroupPassthrough(id int64, providerID *int64) error {
	res, err := s.db.Exec("UPDATE groups SET passthrough_provider_id = ? WHERE id = ?", providerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("group not found: %d", id)
	}
	return nil
}

// DeleteGroup 连带删除其映射，并把引用该 group 的 proxy key 的 group_id 置 NULL，
// 让它们经 effectiveGroupID 回落到默认透传组（而非悬空 → 404）。
// 调用方删完须 keys.Reload() + providerRegistry.Reload()。
func (s *Store) DeleteGroup(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM group_mappings WHERE group_id = ?", id); err != nil {
		return err
	}
	// 解绑引用它的 key（回落默认组），避免 group_id 悬空导致这些 key 全 404。
	if _, err := tx.Exec("UPDATE proxy_keys SET group_id = NULL WHERE group_id = ?", id); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM groups WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("group not found: %d", id)
	}
	return tx.Commit()
}

// ────────────────────── group_mappings ──────────────────────

const mappingCols = "id, group_id, logical_name, provider_id, upstream_id, priority, is_primary"

func scanMapping(s interface {
	Scan(...any) error
}) (*GroupMapping, error) {
	var m GroupMapping
	var isPrimary int
	if err := s.Scan(&m.ID, &m.GroupID, &m.LogicalName, &m.ProviderID, &m.UpstreamID, &m.Priority, &isPrimary); err != nil {
		return nil, err
	}
	m.IsPrimary = isPrimary != 0
	return &m, nil
}

func (s *Store) ListGroupMappings(groupID int64) ([]GroupMapping, error) {
	rows, err := s.db.Query(
		"SELECT "+mappingCols+" FROM group_mappings WHERE group_id = ? ORDER BY logical_name, priority, id",
		groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupMapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ListAllGroupMappings 全量，给 ProviderRegistry/路由层一次性载入缓存。
func (s *Store) ListAllGroupMappings() ([]GroupMapping, error) {
	rows, err := s.db.Query("SELECT " + mappingCols + " FROM group_mappings ORDER BY group_id, logical_name, priority, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupMapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) CreateMapping(m *GroupMapping) (int64, error) {
	res, err := s.db.Exec(
		"INSERT INTO group_mappings (group_id, logical_name, provider_id, upstream_id, priority, is_primary) VALUES (?, ?, ?, ?, ?, ?)",
		m.GroupID, m.LogicalName, m.ProviderID, m.UpstreamID, m.Priority, boolToInt(m.IsPrimary))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteMapping(id int64) error {
	res, err := s.db.Exec("DELETE FROM group_mappings WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("mapping not found: %d", id)
	}
	return nil
}

// SetPrimaryMapping 把某 (group, logical_name) 下指定 mapping 设为主用，
// 同组同逻辑名的其它行清掉 is_primary。事务保证至多一条 primary。
func (s *Store) SetPrimaryMapping(mappingID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var groupID int64
	var logical string
	if err := tx.QueryRow(
		"SELECT group_id, logical_name FROM group_mappings WHERE id = ?", mappingID).
		Scan(&groupID, &logical); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("mapping not found: %d", mappingID)
		}
		return err
	}
	if _, err := tx.Exec(
		"UPDATE group_mappings SET is_primary = 0 WHERE group_id = ? AND logical_name = ?",
		groupID, logical); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"UPDATE group_mappings SET is_primary = 1 WHERE id = ?", mappingID); err != nil {
		return err
	}
	return tx.Commit()
}

// ────────────────────── 启动迁移 ──────────────────────

const legacyProviderName = "anthropic-official"

// migrateLegacyKeyToProvider 把旧 .env 里的单上游 key 一次性导入成
// 名为 anthropic-official 的服务商。设计成网络无关、幂等：
//   - 已存在同名服务商 → 跳过（不覆盖管理员后续改动）
//   - .env key 为空 → 跳过（没东西可迁移）
//
// 关键：不动现存 proxy key 的 group_id（保持 NULL）。NULL → 路由走 legacy 路径，
// 指向默认服务商（即这里迁移出来的 anthropic-official），行为与迁移前完全一致。
// 因此即便启动时模型刷新失败、没有任何 mapping，旧请求依然正常。
func migrateLegacyKeyToProvider(s *Store, envKey string) {
	if envKey == "" {
		return
	}
	if _, err := s.GetProviderByName(legacyProviderName); err == nil {
		return // 已迁移过
	}
	p := &Provider{
		Name:    legacyProviderName,
		BaseURL: upstreamURL,
		APIKey:  envKey,
		Format:  "anthropic",
		Enabled: true,
	}
	if _, err := s.CreateProvider(p); err != nil {
		log.Printf("legacy key migration: create provider failed: %v", err)
		return
	}
	log.Printf("legacy key migration: imported .env key into provider %q (base=%s)", legacyProviderName, upstreamURL)
}

const defaultGroupName = "default"

// ensureDefaultGroup 建立「默认透传组」并把所有未绑组的旧 key 迁移进去，
// 复刻 group_id=NULL 的旧语义（任意模型透传给默认服务商），让模型层合并后
// 仍零感知。设计成幂等、网络无关：
//   - 默认组已存在 → 只补透传服务商（若缺）
//   - 选不出透传服务商（没有任何服务商）→ 仍建组但 passthrough=NULL，
//     且不迁移旧 key（让它们维持 NULL，继续走 Phase 1 仍保留的 legacy 分支），
//     避免把还没配上游的实例的 key 全锁成 404。
//
// 透传服务商优先 anthropic-official，否则取任一启用的服务商。
func ensureDefaultGroup(s *Store) {
	// 选透传服务商
	var passthrough *int64
	if p, err := s.GetProviderByName(legacyProviderName); err == nil && p.Enabled {
		id := p.ID
		passthrough = &id
	} else if provs, err := s.ListProviders(); err == nil {
		for i := range provs {
			if provs[i].Enabled {
				id := provs[i].ID
				passthrough = &id
				break
			}
		}
	}

	// 取/建默认组
	g, err := s.GetGroupByName(defaultGroupName)
	if err != nil {
		gid, cerr := s.CreateGroup(&Group{Name: defaultGroupName, Notes: "默认透传组（旧 key 自动迁移至此）", PassthroughProviderID: passthrough})
		if cerr != nil {
			log.Printf("default group: create failed: %v", cerr)
			return
		}
		g = &Group{ID: gid, PassthroughProviderID: passthrough}
		log.Printf("default group: created (id=%d, passthrough=%v)", gid, passthrough)
	} else if g.PassthroughProviderID == nil && passthrough != nil {
		// 之前没有可用服务商，现在有了 → 补上透传，并继续迁移旧 key
		if err := s.SetGroupPassthrough(g.ID, passthrough); err != nil {
			log.Printf("default group: set passthrough failed: %v", err)
		} else {
			g.PassthroughProviderID = passthrough
		}
	}

	// 只有当默认组确实能透传时才迁移旧 key（否则维持 NULL 走 legacy 分支）。
	if g.PassthroughProviderID == nil {
		log.Printf("default group: no usable provider yet, leaving legacy keys on NULL group")
		return
	}
	res, err := s.db.Exec("UPDATE proxy_keys SET group_id = ? WHERE group_id IS NULL", g.ID)
	if err != nil {
		log.Printf("default group: migrate legacy keys failed: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("default group: migrated %d legacy key(s) into group %d", n, g.ID)
	}
}

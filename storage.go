// storage.go: SQLite 持久化层
package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS requests (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              INTEGER NOT NULL,           -- unix milliseconds
    proxy_key       TEXT NOT NULL,
    request_id      TEXT,                       -- msg_xxx
    http_request_id TEXT,                       -- 上游响应头 request-id
    endpoint        TEXT,
    method          TEXT,
    status          INTEGER,
    model           TEXT,
    input_tokens    INTEGER DEFAULT 0,
    output_tokens   INTEGER DEFAULT 0,
    cache_create_5m INTEGER DEFAULT 0,
    cache_create_1h INTEGER DEFAULT 0,
    cache_read      INTEGER DEFAULT 0,
    web_search_count INTEGER DEFAULT 0,
    cost_usd        REAL    DEFAULT 0,
    latency_ms      INTEGER DEFAULT 0,
    streaming       INTEGER DEFAULT 0,
    stop_reason     TEXT,
    client_ip       TEXT,
    user_agent      TEXT
);
CREATE INDEX IF NOT EXISTS idx_requests_ts        ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_proxy_key ON requests(proxy_key);
CREATE INDEX IF NOT EXISTS idx_requests_model     ON requests(model);

CREATE TABLE IF NOT EXISTS proxy_keys (
    key             TEXT PRIMARY KEY,           -- sk-proxy-xxxx
    owner           TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    revoked_at      INTEGER,                    -- NULL = active
    daily_budget    REAL NOT NULL DEFAULT 0,    -- USD, 0 = unlimited
    allowed_models  TEXT NOT NULL DEFAULT '*',  -- '*' or JSON array of model IDs
    notes           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_keys_owner ON proxy_keys(owner);

CREATE TABLE IF NOT EXISTS curated_models (
    model_id        TEXT PRIMARY KEY,
    enabled         INTEGER NOT NULL DEFAULT 1, -- admin toggle
    first_seen_at   INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL            -- 上游列表里最后一次出现
);

-- 错误请求的 body 快照（仅 status != 2xx 时写入），与 requests 表 1:1。
-- 单独建表是为了不让常见情况（成功请求）拖累 requests 表大小，且 BLOB 检索不需要走主表索引。
CREATE TABLE IF NOT EXISTS error_bodies (
    request_id      INTEGER PRIMARY KEY,        -- requests.id
    request_body    BLOB,
    response_body   BLOB
);

-- 上游服务商：每个 = base_url + key + 协议格式。
-- 取代原本单一硬编码的 Anthropic 上游，支持多家保底。
CREATE TABLE IF NOT EXISTS providers (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT UNIQUE NOT NULL,                -- anthropic-official / openrouter / ...
    base_url    TEXT NOT NULL,                       -- https://api.anthropic.com 等（不含尾斜杠）
    api_key     TEXT NOT NULL,
    format      TEXT NOT NULL DEFAULT 'anthropic',   -- anthropic | openai
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL
);

-- 大组（全局模型池）：每个服务商发现的模型，自动维护的库存清单。
CREATE TABLE IF NOT EXISTS provider_models (
    provider_id   INTEGER NOT NULL,
    upstream_id   TEXT NOT NULL,                     -- 上游真实模型名
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,
    PRIMARY KEY (provider_id, upstream_id)
);

-- 小组：用户手动建，从大组里勾选模型并映射逻辑名。proxy key 绑定一个小组。
CREATE TABLE IF NOT EXISTS groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT UNIQUE NOT NULL,
    notes       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

-- 小组内的逻辑名映射。一对多：同一 (group_id, logical_name) 可有多行，
-- priority 排序、is_primary 标当前主用那条（不自动故障转移，手动切主用）。
CREATE TABLE IF NOT EXISTS group_mappings (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id      INTEGER NOT NULL,
    logical_name  TEXT NOT NULL,                     -- 对外逻辑名
    provider_id   INTEGER NOT NULL,
    upstream_id   TEXT NOT NULL,                     -- 该映射指向的上游真实名
    priority      INTEGER NOT NULL DEFAULT 0,
    is_primary    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_gm_group ON group_mappings(group_id, logical_name);

-- 用户系统：账号密码登录。当前单角色（admin），schema 为将来分发模式预留。
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,                -- bcrypt
    created_at    INTEGER NOT NULL
);

-- session：登录后签发的会话令牌。存 SHA-256(token) 而非明文，
-- 泄漏 DB 也不能直接复用会话。
CREATE TABLE IF NOT EXISTS sessions (
    token_hash  TEXT PRIMARY KEY,
    user_id     INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
`

// 历史 db 升级用：列已存在时 ALTER 会报错，忽略即可
var migrations = []string{
	"ALTER TABLE requests ADD COLUMN web_search_count INTEGER NOT NULL DEFAULT 0",
	// 多服务商：proxy key 绑定的小组（NULL = 旧行为，回落 allowed_models）
	"ALTER TABLE proxy_keys ADD COLUMN group_id INTEGER",
	// 多服务商：请求归因到具体上游 + 上游真实模型名（model 列仍存下游请求名）
	"ALTER TABLE requests ADD COLUMN provider TEXT",
	"ALTER TABLE requests ADD COLUMN upstream_model TEXT",
	// 用户系统：密钥归属 = 创建者。NULL = 迁移前建的，启动时回填 admin。
	"ALTER TABLE proxy_keys ADD COLUMN creator TEXT",
}

type Store struct {
	db *sql.DB
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// WAL 让读写不互相阻塞，对一边写一边查 stats 友好
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;"); err != nil {
		return nil, fmt.Errorf("set pragma: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// 兼容老 db：列已存在时 ALTER 报 "duplicate column"，吞掉
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			if !isDuplicateColumn(err) {
				log.Printf("migration warn: %v (sql=%q)", err, m)
			}
		}
	}
	return &Store{db: db}, nil
}

func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column")
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Insert(r *UsageRecord) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO requests (
  ts, proxy_key, request_id, http_request_id, endpoint, method, status,
  model, input_tokens, output_tokens, cache_create_5m, cache_create_1h,
  cache_read, web_search_count, cost_usd, latency_ms, streaming, stop_reason, client_ip, user_agent,
  provider, upstream_model
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`,
		r.Time.UnixMilli(), r.ProxyKey, r.RequestID, r.HTTPReqID, r.Endpoint, r.Method, r.Status,
		r.Model, r.InputTokens, r.OutputTokens, r.CacheCreate5m, r.CacheCreate1h,
		r.CacheRead, r.WebSearchCount, r.CostUSD, r.LatencyMs, boolToInt(r.Streaming), r.StopReason,
		r.ClientIP, r.UserAgent, r.Provider, r.UpstreamModel,
	)
	if err != nil {
		log.Printf("insert request: %v", err)
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// InsertErrorBody 保存非 2xx 请求的 body 快照，与 requests.id 关联。
// nil 切片正常写入，对应列就是 NULL（比如 invalid-key 阶段还没读到 body 时）。
func (s *Store) InsertErrorBody(requestID int64, reqBody, respBody []byte) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO error_bodies (request_id, request_body, response_body) VALUES (?, ?, ?)",
		requestID, reqBody, respBody)
	if err != nil {
		log.Printf("insert error body: %v", err)
	}
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- 查询 ----------

type StatsFilter struct {
	Since    time.Time // 含
	Until    time.Time // 不含
	ProxyKey string    // 空 = 所有
	Model    string    // 空 = 所有
}

type Totals struct {
	Requests       int64   `json:"requests"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CacheCreate5m  int64   `json:"cache_create_5m"`
	CacheCreate1h  int64   `json:"cache_create_1h"`
	CacheRead      int64   `json:"cache_read"`
	WebSearchCount int64   `json:"web_search_count"`
	CostUSD        float64 `json:"cost_usd"`
	CacheHitRate   float64 `json:"cache_hit_rate"` // cache_read / (input + cache_read)
	Errors         int64   `json:"errors"`         // status != 2xx
}

type StatsBucket struct {
	Key string `json:"key"`
	Totals
}

type StatsResult struct {
	Totals
	ByProxyKey []StatsBucket `json:"by_proxy_key"`
	ByModel    []StatsBucket `json:"by_model"`
	ByDay      []StatsBucket `json:"by_day"`
}

func (s *Store) buildWhere(f StatsFilter) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if !f.Since.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, "ts < ?")
		args = append(args, f.Until.UnixMilli())
	}
	if f.ProxyKey != "" {
		clauses = append(clauses, "proxy_key = ?")
		args = append(args, f.ProxyKey)
	}
	if f.Model != "" {
		clauses = append(clauses, "model = ?")
		args = append(args, f.Model)
	}
	return "WHERE " + joinAnd(clauses), args
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}

const totalsCols = `
COUNT(*),
COALESCE(SUM(input_tokens), 0),
COALESCE(SUM(output_tokens), 0),
COALESCE(SUM(cache_create_5m), 0),
COALESCE(SUM(cache_create_1h), 0),
COALESCE(SUM(cache_read), 0),
COALESCE(SUM(web_search_count), 0),
COALESCE(SUM(cost_usd), 0),
COALESCE(SUM(CASE WHEN status < 200 OR status >= 300 THEN 1 ELSE 0 END), 0)
`

func (s *Store) scanTotals(rows *sql.Rows, hasKey bool) ([]StatsBucket, error) {
	var out []StatsBucket
	for rows.Next() {
		var b StatsBucket
		if hasKey {
			if err := rows.Scan(&b.Key, &b.Requests, &b.InputTokens, &b.OutputTokens,
				&b.CacheCreate5m, &b.CacheCreate1h, &b.CacheRead, &b.WebSearchCount, &b.CostUSD, &b.Errors); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&b.Requests, &b.InputTokens, &b.OutputTokens,
				&b.CacheCreate5m, &b.CacheCreate1h, &b.CacheRead, &b.WebSearchCount, &b.CostUSD, &b.Errors); err != nil {
				return nil, err
			}
		}
		b.CacheHitRate = hitRate(b.InputTokens, b.CacheRead)
		out = append(out, b)
	}
	return out, rows.Err()
}

func hitRate(input, cacheRead int64) float64 {
	total := input + cacheRead
	if total == 0 {
		return 0
	}
	return float64(cacheRead) / float64(total)
}

func (s *Store) Stats(f StatsFilter) (*StatsResult, error) {
	where, args := s.buildWhere(f)

	// 总计
	row := s.db.QueryRow("SELECT "+totalsCols+" FROM requests "+where, args...)
	var t Totals
	if err := row.Scan(&t.Requests, &t.InputTokens, &t.OutputTokens,
		&t.CacheCreate5m, &t.CacheCreate1h, &t.CacheRead, &t.WebSearchCount, &t.CostUSD, &t.Errors); err != nil {
		return nil, fmt.Errorf("scan totals: %w", err)
	}
	t.CacheHitRate = hitRate(t.InputTokens, t.CacheRead)

	result := &StatsResult{Totals: t}

	// 按 proxy_key
	rows, err := s.db.Query("SELECT proxy_key, "+totalsCols+" FROM requests "+where+" GROUP BY proxy_key ORDER BY SUM(cost_usd) DESC", args...)
	if err != nil {
		return nil, err
	}
	result.ByProxyKey, err = s.scanTotals(rows, true)
	rows.Close()
	if err != nil {
		return nil, err
	}

	// 按 model：只看真实命中上游的请求，否则被拒的胡乱 model 名会污染分布
	modelWhere := where + " AND status >= 200 AND status < 300 AND model IS NOT NULL AND model != ''"
	rows, err = s.db.Query("SELECT model, "+totalsCols+" FROM requests "+modelWhere+" GROUP BY model ORDER BY SUM(cost_usd) DESC", args...)
	if err != nil {
		return nil, err
	}
	result.ByModel, err = s.scanTotals(rows, true)
	rows.Close()
	if err != nil {
		return nil, err
	}

	// 按天
	rows, err = s.db.Query("SELECT strftime('%Y-%m-%d', ts/1000, 'unixepoch', 'localtime'), "+totalsCols+" FROM requests "+where+" GROUP BY 1 ORDER BY 1", args...)
	if err != nil {
		return nil, err
	}
	result.ByDay, err = s.scanTotals(rows, true)
	rows.Close()
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ---------- curated_models ----------

type CuratedModel struct {
	ModelID     string    `json:"model_id"`
	Enabled     bool      `json:"enabled"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// UpsertCuratedModel 在上游列表里看到该 model 时调用：
//   - 第一次见 → 插入，enabled=1（默认放行新模型）
//   - 已见过 → 更新 last_seen_at；不动 enabled（保留管理员配置）
func (s *Store) UpsertCuratedModel(modelID string, seenAt time.Time) error {
	ms := seenAt.UnixMilli()
	_, err := s.db.Exec(`
INSERT INTO curated_models (model_id, enabled, first_seen_at, last_seen_at)
VALUES (?, 1, ?, ?)
ON CONFLICT(model_id) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		modelID, ms, ms)
	return err
}

func (s *Store) ListCuratedModels() ([]CuratedModel, error) {
	rows, err := s.db.Query(
		"SELECT model_id, enabled, first_seen_at, last_seen_at FROM curated_models ORDER BY model_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CuratedModel
	for rows.Next() {
		var m CuratedModel
		var enabled int
		var firstMs, lastMs int64
		if err := rows.Scan(&m.ModelID, &enabled, &firstMs, &lastMs); err != nil {
			return nil, err
		}
		m.Enabled = enabled != 0
		m.FirstSeenAt = time.UnixMilli(firstMs)
		m.LastSeenAt = time.UnixMilli(lastMs)
		out = append(out, m)
	}
	return out, rows.Err()
}

// EnabledModelIDs 返回当前可用模型 ID 集合，用于 ModelRegistry.valid。
func (s *Store) EnabledModelIDs() (map[string]bool, error) {
	rows, err := s.db.Query("SELECT model_id FROM curated_models WHERE enabled = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) SetModelEnabled(modelID string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.Exec("UPDATE curated_models SET enabled = ? WHERE model_id = ?", v, modelID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model not found: %s", modelID)
	}
	return nil
}

// ---------- 时序聚合 ----------

// TimeBucket 一段时间窗内的聚合指标，给前端折线图用。
type TimeBucket struct {
	Bucket         string  `json:"bucket"` // YYYY-MM-DD HH:00 或 YYYY-MM-DD
	Requests       int64   `json:"requests"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CacheCreate    int64   `json:"cache_create"` // 5m + 1h 合并
	CacheRead      int64   `json:"cache_read"`
	WebSearchCount int64   `json:"web_search_count"`
	CostUSD        float64 `json:"cost_usd"`
	CacheHitRate   float64 `json:"cache_hit_rate"`
}

// Timeseries 按 granularity 分桶聚合。
// granularity: "hour"（YYYY-MM-DD HH:00）或 "day"（YYYY-MM-DD），其他默认 day。
func (s *Store) Timeseries(f StatsFilter, granularity string) ([]TimeBucket, error) {
	var fmtStr string
	switch granularity {
	case "hour":
		fmtStr = "%Y-%m-%d %H:00"
	default:
		fmtStr = "%Y-%m-%d"
	}
	where, args := s.buildWhere(f)
	q := "SELECT strftime('" + fmtStr + "', ts/1000, 'unixepoch', 'localtime') AS bucket, " +
		"COUNT(*), " +
		"COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), " +
		"COALESCE(SUM(cache_create_5m + cache_create_1h), 0), " +
		"COALESCE(SUM(cache_read), 0), " +
		"COALESCE(SUM(web_search_count), 0), " +
		"COALESCE(SUM(cost_usd), 0) " +
		"FROM requests " + where + " GROUP BY bucket ORDER BY bucket"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeBucket
	for rows.Next() {
		var b TimeBucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.InputTokens, &b.OutputTokens,
			&b.CacheCreate, &b.CacheRead, &b.WebSearchCount, &b.CostUSD); err != nil {
			return nil, err
		}
		b.CacheHitRate = hitRate(b.InputTokens, b.CacheRead)
		out = append(out, b)
	}
	return out, rows.Err()
}

// LogRow 一条请求记录的展示视图（驱动 /admin/logs UI）。
type LogRow struct {
	ID             int64     `json:"id"`
	Time           time.Time `json:"time"`
	ProxyKey       string    `json:"proxy_key"`
	RequestID      string    `json:"request_id,omitempty"`
	HTTPReqID      string    `json:"http_request_id,omitempty"`
	Endpoint       string    `json:"endpoint"`
	Method         string    `json:"method"`
	Status         int       `json:"status"`
	Model          string    `json:"model"`
	Provider       string    `json:"provider,omitempty"`
	UpstreamModel  string    `json:"upstream_model,omitempty"`
	InputTokens    int       `json:"input_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	CacheCreate5m  int       `json:"cache_create_5m"`
	CacheCreate1h  int       `json:"cache_create_1h"`
	CacheRead      int       `json:"cache_read"`
	WebSearchCount int       `json:"web_search_count"`
	CostUSD        float64   `json:"cost_usd"`
	LatencyMs      int64     `json:"latency_ms"`
	Streaming      bool      `json:"streaming"`
	StopReason     string    `json:"stop_reason,omitempty"`
	ClientIP       string    `json:"client_ip,omitempty"`
	UserAgent      string    `json:"user_agent,omitempty"`
	HasErrorBody   bool      `json:"has_error_body"`
}

type LogFilter struct {
	Since    time.Time // 含
	Until    time.Time // 不含
	ProxyKey string
	Model    string
	// StatusClass: "success" → 2xx, "error" → !2xx, "" → 全部
	StatusClass string
	// 游标分页：返回 id < BeforeID 的，按 id 倒序。0 = 从最新开始
	BeforeID int64
	Limit    int
}

const logCols = `
id, ts, proxy_key, request_id, http_request_id, endpoint, method, status,
COALESCE(model, ''), input_tokens, output_tokens, cache_create_5m, cache_create_1h,
cache_read, web_search_count, cost_usd, latency_ms, streaming,
COALESCE(stop_reason, ''), COALESCE(client_ip, ''), COALESCE(user_agent, ''),
COALESCE(provider, ''), COALESCE(upstream_model, '')
`

func (s *Store) scanLogRow(rows *sql.Rows) (*LogRow, error) {
	var r LogRow
	var ts int64
	var streaming int
	var reqID, httpReqID sql.NullString
	if err := rows.Scan(&r.ID, &ts, &r.ProxyKey, &reqID, &httpReqID,
		&r.Endpoint, &r.Method, &r.Status, &r.Model,
		&r.InputTokens, &r.OutputTokens, &r.CacheCreate5m, &r.CacheCreate1h,
		&r.CacheRead, &r.WebSearchCount, &r.CostUSD, &r.LatencyMs, &streaming,
		&r.StopReason, &r.ClientIP, &r.UserAgent, &r.Provider, &r.UpstreamModel); err != nil {
		return nil, err
	}
	r.Time = time.UnixMilli(ts)
	r.RequestID = reqID.String
	r.HTTPReqID = httpReqID.String
	r.Streaming = streaming != 0
	return &r, nil
}

func (s *Store) ListLogs(f LogFilter) ([]LogRow, error) {
	clauses := []string{"1=1"}
	args := []any{}
	if !f.Since.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, "ts < ?")
		args = append(args, f.Until.UnixMilli())
	}
	if f.ProxyKey != "" {
		clauses = append(clauses, "proxy_key = ?")
		args = append(args, f.ProxyKey)
	}
	if f.Model != "" {
		clauses = append(clauses, "model = ?")
		args = append(args, f.Model)
	}
	switch f.StatusClass {
	case "success":
		clauses = append(clauses, "status >= 200 AND status < 300")
	case "error":
		clauses = append(clauses, "(status < 200 OR status >= 300)")
	}
	if f.BeforeID > 0 {
		clauses = append(clauses, "id < ?")
		args = append(args, f.BeforeID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := "SELECT " + logCols + " FROM requests WHERE " + joinAnd(clauses) +
		" ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogRow
	ids := []int64{}
	for rows.Next() {
		row, err := s.scanLogRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *row)
		ids = append(ids, row.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 标记哪些 row 有 error_body（单独一次 IN 查询，避免 N+1）
	if len(ids) > 0 {
		placeholders := strings.Repeat("?,", len(ids))
		placeholders = placeholders[:len(placeholders)-1]
		idArgs := make([]any, len(ids))
		for i, id := range ids {
			idArgs[i] = id
		}
		hasBody := map[int64]bool{}
		r2, err := s.db.Query("SELECT request_id FROM error_bodies WHERE request_id IN ("+placeholders+")", idArgs...)
		if err == nil {
			for r2.Next() {
				var id int64
				if r2.Scan(&id) == nil {
					hasBody[id] = true
				}
			}
			r2.Close()
		}
		for i := range out {
			out[i].HasErrorBody = hasBody[out[i].ID]
		}
	}

	return out, nil
}

// LogDetail 单条日志的完整视图（含 body 快照）。
type LogDetail struct {
	LogRow
	RequestBody  string `json:"request_body,omitempty"`  // UTF-8，可能是 JSON 字符串
	ResponseBody string `json:"response_body,omitempty"` // 可能是 SSE 文本流或 JSON
}

func (s *Store) GetLogDetail(id int64) (*LogDetail, error) {
	row := s.db.QueryRow("SELECT "+logCols+" FROM requests WHERE id = ?", id)
	var d LogDetail
	var ts int64
	var streaming int
	var reqID, httpReqID sql.NullString
	if err := row.Scan(&d.ID, &ts, &d.ProxyKey, &reqID, &httpReqID,
		&d.Endpoint, &d.Method, &d.Status, &d.Model,
		&d.InputTokens, &d.OutputTokens, &d.CacheCreate5m, &d.CacheCreate1h,
		&d.CacheRead, &d.WebSearchCount, &d.CostUSD, &d.LatencyMs, &streaming,
		&d.StopReason, &d.ClientIP, &d.UserAgent, &d.Provider, &d.UpstreamModel); err != nil {
		return nil, err
	}
	d.Time = time.UnixMilli(ts)
	d.RequestID = reqID.String
	d.HTTPReqID = httpReqID.String
	d.Streaming = streaming != 0

	var reqBody, respBody []byte
	br := s.db.QueryRow("SELECT request_body, response_body FROM error_bodies WHERE request_id = ?", id)
	if err := br.Scan(&reqBody, &respBody); err == nil {
		d.HasErrorBody = true
		d.RequestBody = string(reqBody)
		d.ResponseBody = string(respBody)
	}
	return &d, nil
}

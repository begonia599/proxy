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
CREATE INDEX IF NOT EXISTS idx_requests_key_ts    ON requests(proxy_key, ts);

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
    openai_base_url TEXT,
    anthropic_base_url TEXT,
    api_key     TEXT NOT NULL,
    format      TEXT NOT NULL DEFAULT 'anthropic',   -- anthropic | openai | hybrid
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
	// 多服务商：proxy key 绑定的小组（NULL = 回落默认透传组）
	"ALTER TABLE proxy_keys ADD COLUMN group_id INTEGER",
	// 多服务商：请求归因到具体上游 + 上游真实模型名（model 列仍存下游请求名）
	"ALTER TABLE requests ADD COLUMN provider TEXT",
	"ALTER TABLE requests ADD COLUMN upstream_model TEXT",
	// 用户系统：密钥归属 = 创建者。NULL = 迁移前建的，启动时回填 admin。
	"ALTER TABLE proxy_keys ADD COLUMN creator TEXT",
	// 模型层合并：小组可设「透传服务商」——未命中显式映射时把模型原样发给它。
	// 默认组用它复刻旧 group_id=NULL 的行为（任意模型透传给默认服务商）。
	"ALTER TABLE groups ADD COLUMN passthrough_provider_id INTEGER",
	// 按服务商-模型计价（修复非 Anthropic 上游被记 $0 的 bug）。
	// NULL = 未设，回落静态 Anthropic 价表。单位 USD / 1M tokens。
	"ALTER TABLE provider_models ADD COLUMN price_input REAL",
	"ALTER TABLE provider_models ADD COLUMN price_output REAL",
	"ALTER TABLE provider_models ADD COLUMN price_cache_write_5m REAL",
	"ALTER TABLE provider_models ADD COLUMN price_cache_write_1h REAL",
	"ALTER TABLE provider_models ADD COLUMN price_cache_read REAL",
	// 服务商级请求参数覆盖。NULL = 使用该协议格式的安全默认；空字符串 = 不做覆盖。
	"ALTER TABLE providers ADD COLUMN request_overrides TEXT",
	// 混合协议服务商可为 OpenAI / Anthropic 两套入口配置不同 base URL。
	// NULL/空 = 回落 base_url。
	"ALTER TABLE providers ADD COLUMN openai_base_url TEXT",
	"ALTER TABLE providers ADD COLUMN anthropic_base_url TEXT",
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
	Creator  string    // 非空 = 仅统计该用户创建的 key
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
	CacheBase      int64   `json:"-"`              // provider-normalized cache denominator
	CacheHitRate   float64 `json:"cache_hit_rate"` // cache_read / CacheBase
	Errors         int64   `json:"errors"`         // status != 2xx
}

type StatsBucket struct {
	Key string `json:"key"`
	Totals
}

type RiskSummary struct {
	RecentHourCost    float64 `json:"recent_hour_cost"`
	MaxRequestCost    float64 `json:"max_request_cost"`
	MaxRequestModel   string  `json:"max_request_model,omitempty"`
	MaxRequestTime    string  `json:"max_request_time,omitempty"`
	LowCacheRequests  int64   `json:"low_cache_requests"`
	StreamingRequests int64   `json:"streaming_requests"`
}

type StatsResult struct {
	Totals
	ByProxyKey []StatsBucket `json:"by_proxy_key"`
	ByModel    []StatsBucket `json:"by_model"`
	ByProvider []StatsBucket `json:"by_provider"`
	ByEndpoint []StatsBucket `json:"by_endpoint"`
	ByDay      []StatsBucket `json:"by_day"`
	Risk       RiskSummary   `json:"risk"`
	ServerDate string        `json:"server_date"`
	Timezone   string        `json:"timezone"`
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
	if f.Creator != "" {
		clauses = append(clauses, "proxy_key IN (SELECT key FROM proxy_keys WHERE creator = ?)")
		args = append(args, f.Creator)
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
COALESCE(SUM(CASE WHEN provider LIKE 'openai%' THEN input_tokens ELSE input_tokens + cache_read END), 0),
COALESCE(SUM(CASE WHEN status < 200 OR status >= 300 THEN 1 ELSE 0 END), 0)
`

func (s *Store) scanTotals(rows *sql.Rows, hasKey bool) ([]StatsBucket, error) {
	var out []StatsBucket
	for rows.Next() {
		var b StatsBucket
		if hasKey {
			if err := rows.Scan(&b.Key, &b.Requests, &b.InputTokens, &b.OutputTokens,
				&b.CacheCreate5m, &b.CacheCreate1h, &b.CacheRead, &b.WebSearchCount, &b.CostUSD, &b.CacheBase, &b.Errors); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&b.Requests, &b.InputTokens, &b.OutputTokens,
				&b.CacheCreate5m, &b.CacheCreate1h, &b.CacheRead, &b.WebSearchCount, &b.CostUSD, &b.CacheBase, &b.Errors); err != nil {
				return nil, err
			}
		}
		b.CacheHitRate = hitRateFromBase(b.CacheRead, b.CacheBase)
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

func hitRateFromBase(cacheRead, cacheBase int64) float64 {
	if cacheBase <= 0 {
		return 0
	}
	return float64(cacheRead) / float64(cacheBase)
}

func (s *Store) Stats(f StatsFilter) (*StatsResult, error) {
	where, args := s.buildWhere(f)

	// 总计
	row := s.db.QueryRow("SELECT "+totalsCols+" FROM requests "+where, args...)
	var t Totals
	if err := row.Scan(&t.Requests, &t.InputTokens, &t.OutputTokens,
		&t.CacheCreate5m, &t.CacheCreate1h, &t.CacheRead, &t.WebSearchCount, &t.CostUSD, &t.CacheBase, &t.Errors); err != nil {
		return nil, fmt.Errorf("scan totals: %w", err)
	}
	t.CacheHitRate = hitRateFromBase(t.CacheRead, t.CacheBase)

	now := time.Now()
	zone, _ := now.Zone()
	result := &StatsResult{Totals: t, ServerDate: now.Format("2006-01-02"), Timezone: zone}

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

	// 按 provider：区分 Anthropic / OpenAI / 其它上游。
	rows, err = s.db.Query("SELECT COALESCE(NULLIF(provider, ''), '(unknown)'), "+totalsCols+" FROM requests "+where+" GROUP BY 1 ORDER BY SUM(cost_usd) DESC", args...)
	if err != nil {
		return nil, err
	}
	result.ByProvider, err = s.scanTotals(rows, true)
	rows.Close()
	if err != nil {
		return nil, err
	}

	// 按 endpoint：区分 /v1/messages、/v1/responses、/v1/chat/completions 等调用面。
	rows, err = s.db.Query("SELECT COALESCE(NULLIF(endpoint, ''), '(unknown)'), "+totalsCols+" FROM requests "+where+" GROUP BY 1 ORDER BY SUM(cost_usd) DESC", args...)
	if err != nil {
		return nil, err
	}
	result.ByEndpoint, err = s.scanTotals(rows, true)
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

	if err := s.populateRiskSummary(result, f); err != nil {
		return nil, err
	}

	return result, nil
}

func (s *Store) populateRiskSummary(result *StatsResult, f StatsFilter) error {
	where, args := s.buildWhere(f)

	var risk RiskSummary
	if result.Requests > 0 {
		risk.StreamingRequests = 0
	}
	row := s.db.QueryRow("SELECT COALESCE(SUM(CASE WHEN streaming = 1 THEN 1 ELSE 0 END), 0), "+
		"COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 AND (input_tokens + cache_read) >= 1000 AND cache_read * 1.0 / (input_tokens + cache_read) < 0.20 THEN 1 ELSE 0 END), 0) "+
		"FROM requests "+where, args...)
	if err := row.Scan(&risk.StreamingRequests, &risk.LowCacheRequests); err != nil {
		return fmt.Errorf("scan risk counts: %w", err)
	}

	maxRow := s.db.QueryRow("SELECT COALESCE(cost_usd, 0), COALESCE(model, ''), COALESCE(strftime('%Y-%m-%d %H:%M:%S', ts/1000, 'unixepoch', 'localtime'), '') "+
		"FROM requests "+where+" ORDER BY cost_usd DESC, id DESC LIMIT 1", args...)
	if err := maxRow.Scan(&risk.MaxRequestCost, &risk.MaxRequestModel, &risk.MaxRequestTime); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("scan max request: %w", err)
	}

	recent := f
	hourAgo := time.Now().Add(-time.Hour)
	if recent.Since.IsZero() || recent.Since.Before(hourAgo) {
		recent.Since = hourAgo
	}
	recentWhere, recentArgs := s.buildWhere(recent)
	if err := s.db.QueryRow("SELECT COALESCE(SUM(cost_usd), 0) FROM requests "+recentWhere, recentArgs...).Scan(&risk.RecentHourCost); err != nil {
		return fmt.Errorf("scan recent hour cost: %w", err)
	}

	result.Risk = risk
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
		"COALESCE(SUM(cost_usd), 0), " +
		"COALESCE(SUM(CASE WHEN provider LIKE 'openai%' THEN input_tokens ELSE input_tokens + cache_read END), 0) " +
		"FROM requests " + where + " GROUP BY bucket ORDER BY bucket"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeBucket
	for rows.Next() {
		var b TimeBucket
		var cacheBase int64
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.InputTokens, &b.OutputTokens,
			&b.CacheCreate, &b.CacheRead, &b.WebSearchCount, &b.CostUSD, &cacheBase); err != nil {
			return nil, err
		}
		b.CacheHitRate = hitRateFromBase(b.CacheRead, cacheBase)
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
	Creator  string // 非空 = 仅返回该用户创建的 key 的日志
	// StatusClass: "success" → 2xx, "error" → !2xx, "" → 全部
	StatusClass string
	// 游标分页：返回 id < BeforeID 的，按 id 倒序。0 = 从最新开始
	BeforeID int64
	Limit    int
	Offset   int
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

func logFilterWhere(f LogFilter) (string, []any) {
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
	if f.Creator != "" {
		clauses = append(clauses, "proxy_key IN (SELECT key FROM proxy_keys WHERE creator = ?)")
		args = append(args, f.Creator)
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
	return joinAnd(clauses), args
}

func (s *Store) CountLogs(f LogFilter) (int, error) {
	where, args := logFilterWhere(f)
	row := s.db.QueryRow("SELECT COUNT(*) FROM requests WHERE "+where, args...)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) ListLogs(f LogFilter) ([]LogRow, error) {
	where, args := logFilterWhere(f)
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 501 {
		limit = 501
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	q := "SELECT " + logCols + " FROM requests WHERE " + where +
		" ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit)
	args = append(args, offset)
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

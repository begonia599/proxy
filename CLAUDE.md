# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o claude-proxy .     # single binary, no sub-packages
cp .env.example .env           # fill in key= and admin_token=
./claude-proxy                 # listens on :8787
```

Go 1.21+ required. Only direct dependency: `modernc.org/sqlite` (pure Go, no CGO).

No tests exist yet. No Makefile, no CI, no linter config.

## Architecture

Single-process Anthropic Messages API transparent proxy. All Go files are `package main` in the repo root. Three global singletons (`store`, `keys`, `modelRegistry`) are initialized in `main.go` and shared across handlers.

### Request flow (POST /v1/chat/completions — OpenAI compat)

```
openaiChatHandler (compat.go)
  → same auth + budget + model validation as forwardHandler
  → json.Unmarshal into oaiChatRequest
  → convertRequest (compat_convert.go): OpenAI messages/tools/system → Anthropic format
  → direct http.Client POST to upstream /v1/messages (NOT ReverseProxy)
  → non-streaming: read full response → convertResponse → OpenAI JSON
  → streaming: line-by-line SSE relay, convertSSEEvent per event → OpenAI chunks + Flush
  → reuse parseUsageJSON/parseUsageSSE + writeRecord for cost tracking
```

### Request flow (POST /v1/messages)

```
forwardHandler (proxy.go)
  → extractProxyKey (x-api-key or Authorization: Bearer)
  → KeyCache.Get (O(1) in-memory lookup)
  → store.TodaysCost (SQL SUM per request if budget > 0)
  → full body read (up to 50MB) to peek model/speed/tools
  → reject fast mode (6x billing), unknown models, disallowed models
  → store body in context (for error capture in ModifyResponse)
  → httputil.ReverseProxy.ServeHTTP
      Director: swap x-api-key to real key, force accept-encoding: identity
      ModifyResponse: wrap resp.Body with teeBody → async usage parse → DB insert
```

### Key design constraints

- **Byte-level forwarding**: request/response bodies are never deserialized then re-serialized. This preserves prompt cache hit eligibility (hash depends on identical bytes).
- **accept-encoding: identity** is forced because teeBody reads raw bytes; compressed responses would break the usage parser.
- **Fail-open model validation**: empty registry (e.g. upstream unreachable at startup) allows all models through rather than bricking the proxy.
- **Budget check hits DB every request** (intentional trade-off: accuracy over speed, single SQL under 1ms).

### File responsibilities

| File | Role |
|---|---|
| `main.go` | Entry point, mux wiring, embeds `dashboard.html` via `go:embed` |
| `config.go` | `.env` parser → `Config{RealKey, AdminToken, HideCC}` |
| `proxy.go` | Reverse proxy assembly, `forwardHandler`, model list handlers, all pre-flight checks |
| `keys.go` | `KeyMeta` + `KeyCache` (sync.RWMutex map backed by SQLite), CRUD, key generation (sk-proxy- + 160-bit base32) |
| `models.go` | `ModelRegistry` — in-memory enabled set, 30-min upstream refresh, `FilteredList` (curated ∩ key allowlist) |
| `pricing.go` | Static price table, exact match then family-prefix fallback, `CostOf()` |
| `storage.go` | SQLite schema (WAL mode), all DB ops: insert, stats, timeseries, cursor-paginated logs |
| `admin.go` | `/admin/*` JSON API handlers (stats, keys CRUD, model toggle, logs) |
| `usage.go` | `UsageRecord`, JSON + SSE response parsing, `writeRecord` (cost compute + DB insert) |
| `tee.go` | `teeBody` — wraps io.ReadCloser, copies up to 4MB to buffer, fires async callback on EOF |
| `compat.go` | OpenAI Chat Completions handler — translates `/v1/chat/completions` (non-streaming + SSE), direct `http.Client` to upstream (not ReverseProxy) |
| `compat_convert.go` | Pure conversion functions: OpenAI ↔ Anthropic request/response/SSE types and mapping |
| `dashboard.html` | 1500-line single-file SPA (Chinese UI), inline CSS/JS, no build step |

### Database

SQLite with WAL mode. Four tables: `requests` (audit log), `proxy_keys`, `curated_models`, `error_bodies` (blob snapshots for non-2xx). All timestamps are unix milliseconds (INTEGER). No migration framework — schema uses `CREATE TABLE IF NOT EXISTS` plus manual `ALTER TABLE` with duplicate-column error swallowing.

### Error response format

Proxy endpoints return Anthropic-compatible error JSON (`{"type":"error","error":{"type":"...","message":"..."}}`). OpenAI compat endpoints return `{"error":{"message":"...","type":"...","param":null,"code":null}}`. Admin endpoints return simple `{"error":"message"}`.

### Concurrency

`KeyCache` and `ModelRegistry` use `sync.RWMutex`. `teeBody` uses `sync.Once` for its callback. SQLite WAL allows concurrent reads during writes.

## Conventions

- Chinese comments throughout, English for struct tags
- Top-of-file comment blocks explain each file's responsibility
- HTTP handlers are camelCase + "Handler" suffix, returning `http.HandlerFunc`
- Empty JSON arrays return `[]` not `null`
- Log list uses cursor-based pagination (`before_id`)
- Hardcoded constants: listen `:8787`, db `claude-proxy.db`, upstream `https://api.anthropic.com`

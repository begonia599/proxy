# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o claude-proxy .     # single binary, no sub-packages
cp .env.example .env           # fill in key= and admin_token=
./claude-proxy                 # listens on :8787
```

Go 1.21+ required. Only direct dependency: `modernc.org/sqlite` (pure Go, no CGO).

Tests: `go test ./...` (routing, passthrough/default-group migration, per-provider pricing, OpenAI-compat conversion, model rewrite). No Makefile, no CI, no linter config.

## Architecture

Single-process Anthropic Messages API transparent proxy. All Go files are `package main` in the repo root. Three global singletons (`store`, `keys`, `providerRegistry`) are initialized in `main.go` and shared across handlers.

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
| `main.go` | Entry point, mux wiring, embeds `dashboard.html` via `go:embed`, legacy-key migration + default-group migration + registry init |
| `config.go` | `.env` parser → `Config{RealKey, AdminToken, HideCC}` (RealKey now only the legacy default upstream) |
| `proxy.go` | Reverse proxy assembly, route-aware Director, `forwardHandler`, model list handlers, `rewriteTopLevelModel` (surgical model byte-rewrite) |
| `keys.go` | `KeyMeta` + `KeyCache` (sync.RWMutex map backed by SQLite), CRUD, key gen; `GroupID` binds a key to a group (its model allowlist); new keys default to the `default` group |
| `routing.go` | `ResolveRoute(key, model) → RouteTarget` — the routing core. Every key resolves via its **effective group** (`effectiveGroupID`: own group, else default): logical name → primary mapping → upstream-name fallback → group passthrough provider |
| `providers.go` | `ProviderRegistry` — in-memory providers + group-mapping indexes (byLogical/byUpstream) + passthrough index + per-(provider,model) price index, 30-min model-inventory refresh |
| `providers_store.go` | DB types + CRUD for providers / provider_models (w/ price cols) / groups (w/ `passthrough_provider_id`) / group_mappings; `migrateLegacyKeyToProvider`, `ensureDefaultGroup` |
| `dispatch.go` | Outbound dispatch: `callUpstreamAnthropic` always returns Anthropic-canonical (translates for openai-format providers); `dispatchAnthropicToOpenAI` |
| `pricing.go` | Static price table (`CostOf`/`lookupPrice`) + `Price.Cost`; `tokenCost` resolves per-provider override (`ProviderRegistry.PriceFor`) → static fallback → 0 |
| `storage.go` | SQLite schema (WAL mode), all request/stats/log DB ops |
| `admin.go` | `/admin/*` JSON API (stats, keys CRUD w/ group_id, logs; `GET /admin/models` = unified cross-provider inventory + prices) |
| `admin_providers.go` | `/admin/providers` (+ model price set) + `/admin/groups` (+ mappings, + group passthrough) CRUD handlers |
| `usage.go` | `UsageRecord` (now w/ Provider, UpstreamModel), JSON + SSE response parsing, `writeRecord` |
| `tee.go` | `teeBody` — wraps io.ReadCloser, copies up to 4MB to buffer, fires async callback on EOF |
| `compat.go` | OpenAI Chat Completions inbound handler — routes via `ResolveRoute` + `callUpstreamAnthropic` |
| `compat_convert.go` | Inbound conversion: OpenAI → Anthropic (request) and Anthropic → OpenAI (response/SSE) |
| `compat_convert_out.go` | Outbound conversion: Anthropic → OpenAI (request) and OpenAI → Anthropic (response/SSE) |
| `dashboard.html` | Single-file SPA (Chinese UI): overview/keys/providers/groups/models(inventory+pricing)/logs tabs |

### Routing (group-based, single source of truth)

```
ResolveRoute(keyMeta, requestedModel):
  gid = effectiveGroupID(key)   # key's own GroupID, else the default group
  logical name hit  → primary mapping for that name
  else upstream_id match → concrete-name fallback (existing clients unchanged)
  else group has passthrough provider → model passed through unchanged to it
  else → 404 not_found
```

The legacy `ModelRegistry` / `curated_models` / per-key `allowed_models` layer has been **retired** — the provider→group→mapping system is the single source of truth. A startup-created **default passthrough group** (→ `anthropic-official`) reproduces the old `group_id = NULL` behavior; `ensureDefaultGroup` migrates pre-existing keys into it.

- **Canonical form = Anthropic Messages.** Inbound adapter (compat_convert.go) and outbound adapter (compat_convert_out.go) sandwich a single internal format, so all 4 client×provider combos reuse the same pipeline.
- **Anthropic→Anthropic fast path** stays byte-level via ReverseProxy; logical→concrete model rename uses `rewriteTopLevelModel` (surgical splice, no re-marshal) to preserve prompt-cache bytes.
- **No auto-failover.** Routing picks the current primary; upstream errors pass straight through to the downstream client.
- **Pricing** is keyed on `(provider, upstream_model)` with an optional per-model override (`provider_models.price_*`), falling back to the static Anthropic family table; this fixes $0-cost for non-Anthropic upstreams.

### Database

SQLite with WAL mode. Tables: `requests` (audit log, + `provider`/`upstream_model` cols), `proxy_keys` (+ `group_id`; `allowed_models` retained but unused), `error_bodies`, plus the provider system: `providers`, `provider_models` (auto-discovered inventory + nullable `price_*` override cols), `groups` (+ `passthrough_provider_id`), `group_mappings` (logical-name → provider+upstream, one-to-many w/ `is_primary`). `curated_models` table is retained but no longer read (legacy). All timestamps unix milliseconds. No migration framework — `CREATE TABLE IF NOT EXISTS` + manual `ALTER TABLE` with duplicate-column error swallowing.

### Error response format

Proxy endpoints return Anthropic-compatible error JSON (`{"type":"error","error":{"type":"...","message":"..."}}`). OpenAI compat endpoints return `{"error":{"message":"...","type":"...","param":null,"code":null}}`. Admin endpoints return simple `{"error":"message"}`.

### Concurrency

`KeyCache` and `ProviderRegistry` use `sync.RWMutex`. `teeBody` uses `sync.Once` for its callback. SQLite WAL allows concurrent reads during writes.

## Conventions

- Chinese comments throughout, English for struct tags
- Top-of-file comment blocks explain each file's responsibility
- HTTP handlers are camelCase + "Handler" suffix, returning `http.HandlerFunc`
- Empty JSON arrays return `[]` not `null`
- Log list uses cursor-based pagination (`before_id`)
- Hardcoded constants: listen `:8787`, db `claude-proxy.db`, upstream `https://api.anthropic.com`

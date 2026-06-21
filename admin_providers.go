// admin_providers.go: /admin/providers、/admin/groups 的 JSON CRUD。
//
// 风格与 admin.go 的 adminKeysHandler 一致：Bearer admin token 鉴权，
// 路径尾段当 id，写操作后 providerRegistry.Reload() 让路由热路径感知。
// api_key 对外用 MaskKey 脱敏，永不回传明文。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// providerView 是 Provider 的对外视图：脱敏 key + has_key 标记。
type providerView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Format    string `json:"format"`
	Enabled   bool   `json:"enabled"`
	HasKey    bool   `json:"has_key"`
	MaskedKey string `json:"masked_key"`
	CreatedAt int64  `json:"created_at"`
}

func toProviderView(p *Provider) providerView {
	return providerView{
		ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, Format: p.Format,
		Enabled: p.Enabled, HasKey: p.APIKey != "", MaskedKey: MaskKey(p.APIKey),
		CreatedAt: p.CreatedAt.UnixMilli(),
	}
}

// adminProvidersHandler 处理 /admin/providers 与 /admin/providers/{id}[/...]：
//
//	GET    /admin/providers                       列出全部（脱敏）
//	POST   /admin/providers                       新建 {name,base_url,api_key,format,enabled}
//	PATCH  /admin/providers/{id}                  部分更新（api_key 空 = 不改）
//	DELETE /admin/providers/{id}                  删除（连带清库存/映射/被引透传）
//	GET    /admin/providers/{id}/catalog          拉上游实时模型列表（不写库，给策展用）
//	GET    /admin/providers/{id}/models           列已加入大组的模型（含计价）
//	POST   /admin/providers/{id}/models           批量加入大组 {upstream_ids:[...]}
//	DELETE /admin/providers/{id}/models/{upstream} 移出大组
//	POST   /admin/providers/{id}/models/{upstream}/price 设/清计价覆盖
func adminProvidersHandler(cfg *Config) http.HandlerFunc {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")

		path := strings.TrimPrefix(r.URL.Path, "/admin/providers")
		path = strings.TrimPrefix(path, "/")
		// 可能是 "{id}" 或 "{id}/refresh" 或 "{id}/models"
		var idPart, sub string
		if path != "" {
			parts := strings.SplitN(path, "/", 2)
			idPart = parts[0]
			if len(parts) == 2 {
				sub = parts[1]
			}
		}

		switch {
		case r.Method == http.MethodGet && idPart == "":
			list, err := store.ListProviders()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			views := make([]providerView, 0, len(list))
			for i := range list {
				views = append(views, toProviderView(&list[i]))
			}
			_ = json.NewEncoder(w).Encode(views)

		case r.Method == http.MethodPost && idPart == "":
			var req struct {
				Name    string `json:"name"`
				BaseURL string `json:"base_url"`
				APIKey  string `json:"api_key"`
				Format  string `json:"format"`
				Enabled *bool  `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.BaseURL) == "" {
				http.Error(w, `{"error":"name and base_url required"}`, http.StatusBadRequest)
				return
			}
			if req.Format == "" {
				req.Format = "anthropic"
			}
			if req.Format != "anthropic" && req.Format != "openai" {
				http.Error(w, `{"error":"format must be anthropic or openai"}`, http.StatusBadRequest)
				return
			}
			enabled := true
			if req.Enabled != nil {
				enabled = *req.Enabled
			}
			p := &Provider{Name: req.Name, BaseURL: req.BaseURL, APIKey: req.APIKey, Format: req.Format, Enabled: enabled}
			id, err := store.CreateProvider(p)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			p.ID = id
			reloadProviders()
			// 不再自动拉取库存——管理员在「管理模型」里手动拉上游目录并勾选加入大组。
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(toProviderView(p))

		case r.Method == http.MethodPatch && idPart != "":
			id, ok := parseID(w, idPart)
			if !ok {
				return
			}
			var u ProviderUpdate
			if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			if u.Format != nil && *u.Format != "anthropic" && *u.Format != "openai" {
				http.Error(w, `{"error":"format must be anthropic or openai"}`, http.StatusBadRequest)
				return
			}
			if err := store.UpdateProvider(id, u); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			reloadProviders()
			p, _ := store.GetProvider(id)
			_ = json.NewEncoder(w).Encode(toProviderView(p))

		case r.Method == http.MethodDelete && idPart != "":
			id, ok := parseID(w, idPart)
			if !ok {
				return
			}
			if err := store.DeleteProvider(id); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			reloadProviders()
			_, _ = w.Write([]byte(`{"ok":true}`))

		case r.Method == http.MethodGet && sub == "catalog":
			id, ok := parseID(w, idPart)
			if !ok {
				return
			}
			p, err := store.GetProvider(id)
			if err != nil {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			ids, err := providerRegistry.UpstreamCatalog(p)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
				return
			}
			if ids == nil {
				ids = []string{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": ids})

		case r.Method == http.MethodGet && sub == "models":
			id, ok := parseID(w, idPart)
			if !ok {
				return
			}
			models, err := store.ListProviderModels(id)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
				return
			}
			if models == nil {
				models = []ProviderModel{}
			}
			_ = json.NewEncoder(w).Encode(models)

		// POST /admin/providers/{id}/models —— 批量把选中的上游模型加入大组。
		case r.Method == http.MethodPost && sub == "models":
			id, ok := parseID(w, idPart)
			if !ok {
				return
			}
			var req struct {
				UpstreamIDs []string `json:"upstream_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			now := time.Now()
			added := 0
			for _, m := range req.UpstreamIDs {
				m = strings.TrimSpace(m)
				if m == "" {
					continue
				}
				if err := store.UpsertProviderModel(id, m, now); err != nil {
					log.Printf("add provider_model %d/%s: %v", id, m, err)
					continue
				}
				added++
			}
			reloadProviders()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "added": added})

		// POST /admin/providers/{id}/models/{upstream}/price —— 设/清按服务商计价覆盖。
		// upstream 真实名可能含 "/"（如 openrouter 的 anthropic/claude-...），
		// 故用前后缀切割而非按段解析。
		case r.Method == http.MethodPost && strings.HasPrefix(sub, "models/") && strings.HasSuffix(sub, "/price"):
			id, ok := parseID(w, idPart)
			if !ok {
				return
			}
			upstream := strings.TrimSuffix(strings.TrimPrefix(sub, "models/"), "/price")
			if upstream == "" {
				http.Error(w, `{"error":"upstream model id required in path"}`, http.StatusBadRequest)
				return
			}
			var req struct {
				Clear        bool     `json:"clear"`
				Input        *float64 `json:"input"`
				Output       *float64 `json:"output"`
				CacheWrite5m *float64 `json:"cache_write_5m"`
				CacheWrite1h *float64 `json:"cache_write_1h"`
				CacheRead    *float64 `json:"cache_read"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			var price *Price
			if !req.Clear {
				price = &Price{
					Input:        derefFloat(req.Input),
					Output:       derefFloat(req.Output),
					CacheWrite5m: derefFloat(req.CacheWrite5m),
					CacheWrite1h: derefFloat(req.CacheWrite1h),
					CacheRead:    derefFloat(req.CacheRead),
				}
				// 防退化覆盖：omitted 字段被 derefFloat 归 0 并存为非 NULL，会把对应 token
				// 计成 $0。要求 input/output 必须 > 0；缓存项留空(=0)是合法的"不计缓存"。
				if price.Input <= 0 || price.Output <= 0 {
					http.Error(w, `{"error":"input and output price must be > 0; use {\"clear\":true} to remove the override"}`, http.StatusBadRequest)
					return
				}
			}
			if err := store.SetProviderModelPrice(id, upstream, price); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			reloadProviders()
			_, _ = w.Write([]byte(`{"ok":true}`))

		// DELETE /admin/providers/{id}/models/{upstream} —— 把模型移出大组。
		case r.Method == http.MethodDelete && strings.HasPrefix(sub, "models/"):
			id, ok := parseID(w, idPart)
			if !ok {
				return
			}
			upstream := strings.TrimPrefix(sub, "models/")
			if upstream == "" {
				http.Error(w, `{"error":"upstream model id required in path"}`, http.StatusBadRequest)
				return
			}
			if err := store.DeleteProviderModel(id, upstream); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			reloadProviders()
			_, _ = w.Write([]byte(`{"ok":true}`))

		default:
			http.Error(w, `{"error":"method/path not supported"}`, http.StatusBadRequest)
		}
	}
	return requireAuth(h)
}

// adminGroupsHandler 处理 /admin/groups 与 /admin/groups/{id}[/mappings[/{mid}[/primary]]]：
//
//	GET    /admin/groups                       列出全部小组
//	POST   /admin/groups                       新建 {name,notes}
//	PATCH  /admin/groups/{id}                  改 name/notes
//	DELETE /admin/groups/{id}                  删除（连带映射）
//	GET    /admin/groups/{id}/mappings         列该组映射
//	POST   /admin/groups/{id}/mappings         新建映射 {logical_name,provider_id,upstream_id,is_primary}
//	DELETE /admin/groups/{id}/mappings/{mid}   删映射
//	POST   /admin/groups/{id}/mappings/{mid}/primary  设为主用
func adminGroupsHandler(cfg *Config) http.HandlerFunc {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")

		path := strings.TrimPrefix(r.URL.Path, "/admin/groups")
		path = strings.TrimPrefix(path, "/")
		parts := []string{}
		if path != "" {
			parts = strings.Split(path, "/")
		}

		switch {
		// /admin/groups
		case len(parts) == 0:
			switch r.Method {
			case http.MethodGet:
				list, err := store.ListGroups()
				if err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
					return
				}
				if list == nil {
					list = []Group{}
				}
				_ = json.NewEncoder(w).Encode(list)
			case http.MethodPost:
				var req struct {
					Name  string `json:"name"`
					Notes string `json:"notes"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
					return
				}
				if strings.TrimSpace(req.Name) == "" {
					http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
					return
				}
				g := &Group{Name: req.Name, Notes: req.Notes}
				id, err := store.CreateGroup(g)
				if err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
					return
				}
				g.ID = id
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(g)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}

		// /admin/groups/{id}
		case len(parts) == 1:
			id, ok := parseID(w, parts[0])
			if !ok {
				return
			}
			switch r.Method {
			case http.MethodPatch:
				var req struct {
					Name                  *string `json:"name"`
					Notes                 *string `json:"notes"`
					PassthroughProviderID *int64  `json:"passthrough_provider_id"`
					ClearPassthrough      bool    `json:"clear_passthrough"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
					return
				}
				if err := store.UpdateGroup(id, req.Name, req.Notes); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
					return
				}
				// 透传服务商：clear_passthrough=true 清空；否则给了 id 就设。影响路由，需 reload。
				if req.ClearPassthrough {
					if err := store.SetGroupPassthrough(id, nil); err != nil {
						http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
						return
					}
				} else if req.PassthroughProviderID != nil {
					if err := store.SetGroupPassthrough(id, req.PassthroughProviderID); err != nil {
						http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
						return
					}
				}
				reloadProviders()
				_, _ = w.Write([]byte(`{"ok":true}`))
			case http.MethodDelete:
				// 默认透传组是兜底，删了会让回落它的 key 全失配；拒绝。
				if id == providerRegistry.DefaultGroupID() {
					http.Error(w, `{"error":"cannot delete the default group"}`, http.StatusBadRequest)
					return
				}
				if err := store.DeleteGroup(id); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
					return
				}
				// 先刷 key 缓存（DeleteGroup 已把它们 group_id 置 NULL → 回落默认组），
				// 再刷 registry 丢弃该组，避免中间窗口出现 key 指向已不存在的组。
				if err := keys.Reload(); err != nil {
					log.Printf("keys reload after group delete: %v", err)
				}
				reloadProviders()
				_, _ = w.Write([]byte(`{"ok":true}`))
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}

		// /admin/groups/{id}/mappings
		case len(parts) == 2 && parts[1] == "mappings":
			gid, ok := parseID(w, parts[0])
			if !ok {
				return
			}
			switch r.Method {
			case http.MethodGet:
				list, err := store.ListGroupMappings(gid)
				if err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
					return
				}
				if list == nil {
					list = []GroupMapping{}
				}
				_ = json.NewEncoder(w).Encode(list)
			case http.MethodPost:
				var req struct {
					LogicalName string `json:"logical_name"`
					ProviderID  int64  `json:"provider_id"`
					UpstreamID  string `json:"upstream_id"`
					Priority    int    `json:"priority"`
					IsPrimary   bool   `json:"is_primary"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
					return
				}
				if strings.TrimSpace(req.LogicalName) == "" || req.ProviderID == 0 || strings.TrimSpace(req.UpstreamID) == "" {
					http.Error(w, `{"error":"logical_name, provider_id, upstream_id required"}`, http.StatusBadRequest)
					return
				}
				m := &GroupMapping{
					GroupID: gid, LogicalName: req.LogicalName, ProviderID: req.ProviderID,
					UpstreamID: req.UpstreamID, Priority: req.Priority, IsPrimary: req.IsPrimary,
				}
				id, err := store.CreateMapping(m)
				if err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
					return
				}
				m.ID = id
				// 若标了主用，确保同组同名其它行清主用
				if req.IsPrimary {
					_ = store.SetPrimaryMapping(id)
				}
				reloadProviders()
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(m)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}

		// /admin/groups/{id}/mappings/{mid}
		case len(parts) == 3 && parts[1] == "mappings":
			mid, ok := parseID(w, parts[2])
			if !ok {
				return
			}
			if r.Method != http.MethodDelete {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			if err := store.DeleteMapping(mid); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			reloadProviders()
			_, _ = w.Write([]byte(`{"ok":true}`))

		// /admin/groups/{id}/mappings/{mid}/primary
		case len(parts) == 4 && parts[1] == "mappings" && parts[3] == "primary":
			mid, ok := parseID(w, parts[2])
			if !ok {
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			if err := store.SetPrimaryMapping(mid); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
				return
			}
			reloadProviders()
			_, _ = w.Write([]byte(`{"ok":true}`))

		default:
			http.Error(w, `{"error":"path not supported"}`, http.StatusBadRequest)
		}
	}
	return requireAuth(h)
}

// ── 小工具 ──

func parseID(w http.ResponseWriter, s string) (int64, bool) {
	var id int64
	if _, err := fmt.Sscanf(s, "%d", &id); err != nil || id <= 0 {
		http.Error(w, `{"error":"bad id"}`, http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func reloadProviders() {
	if err := providerRegistry.Reload(); err != nil {
		log.Printf("provider registry reload: %v", err)
	}
}

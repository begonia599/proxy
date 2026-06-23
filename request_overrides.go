package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type requestOverrideConfig struct {
	Operations []requestOverrideOperation `json:"operations"`
}

type requestOverrideOperation struct {
	Mode        string          `json:"mode"`
	Description string          `json:"description,omitempty"`
	Path        string          `json:"path"`
	Value       json.RawMessage `json:"value,omitempty"`
}

const anthropicDefaultRequestOverrides = `{
  "operations": [
    {
      "mode": "delete",
      "description": "删除顶层 metadata，减少客户端信息暴露",
      "path": "metadata"
    }
  ]
}`

const openAIDefaultRequestOverrides = `{
  "operations": [
    {
      "mode": "set",
      "description": "强制 store=false，阻止上游存储对话内容",
      "path": "store",
      "value": false
    },
    {
      "mode": "delete",
      "description": "删除顶层 metadata，减少客户端信息暴露",
      "path": "metadata"
    }
  ]
}`

func defaultRequestOverrides(format string) string {
	if format == providerFormatOpenAI {
		return openAIDefaultRequestOverrides
	}
	return anthropicDefaultRequestOverrides
}

func effectiveRequestOverrides(p *Provider) string {
	return effectiveRequestOverridesForProtocol(p, p.Format)
}

func effectiveRequestOverridesForProtocol(p *Provider, protocol string) string {
	if p == nil {
		return ""
	}
	if p.RequestOverrides != nil {
		return *p.RequestOverrides
	}
	return defaultRequestOverrides(protocol)
}

func validateRequestOverrides(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var cfg requestOverrideConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return fmt.Errorf("invalid request_overrides JSON: %w", err)
	}
	for i, op := range cfg.Operations {
		if op.Mode != "set" && op.Mode != "delete" {
			return fmt.Errorf("operation %d: mode must be set or delete", i)
		}
		if strings.TrimSpace(op.Path) == "" {
			return fmt.Errorf("operation %d: path is required", i)
		}
		if op.Mode == "set" && len(op.Value) == 0 {
			return fmt.Errorf("operation %d: set requires value", i)
		}
	}
	return nil
}

func applyProviderRequestOverrides(raw []byte, p *Provider) []byte {
	return applyProviderRequestOverridesForProtocol(raw, p, p.Format)
}

func applyProviderRequestOverridesForProtocol(raw []byte, p *Provider, protocol string) []byte {
	overrides := strings.TrimSpace(effectiveRequestOverridesForProtocol(p, protocol))
	if overrides == "" {
		return raw
	}
	var cfg requestOverrideConfig
	if err := json.Unmarshal([]byte(overrides), &cfg); err != nil {
		return raw
	}
	for _, op := range cfg.Operations {
		raw = applyRequestOverride(raw, op)
	}
	return raw
}

func applyRequestOverride(raw []byte, op requestOverrideOperation) []byte {
	path := parseOverridePath(op.Path)
	if len(path) == 0 {
		return raw
	}
	if len(path) == 1 && op.Mode == "delete" && path[0] == "metadata" {
		return stripTopLevelMetadata(raw)
	}
	if len(path) == 1 && op.Mode == "set" && path[0] == "store" && string(op.Value) == "false" {
		return forceTopLevelStoreFalse(raw)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return raw
	}
	switch op.Mode {
	case "delete":
		deleteJSONPath(body, path)
	case "set":
		var val any
		if err := json.Unmarshal(op.Value, &val); err != nil {
			return raw
		}
		setJSONPath(body, path, val)
	default:
		return raw
	}
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

func parseOverridePath(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if strings.HasPrefix(path, "/") {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		for i := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(parts[i], "~1", "/"), "~0", "~")
		}
		return parts
	}
	parts := strings.Split(path, ".")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func setJSONPath(root map[string]any, path []string, value any) {
	cur := root
	for _, part := range path[:len(path)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}

func deleteJSONPath(root map[string]any, path []string) {
	cur := root
	for _, part := range path[:len(path)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, path[len(path)-1])
}

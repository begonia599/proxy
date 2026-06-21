// routing.go: 下游请求 → 上游路由解析。
//
// 把 (proxy key, 请求模型名) 解析成一个 RouteTarget：去哪个服务商、用什么真实模型名、
// 走什么协议格式。这是多上游架构的中枢。
//
// 解析优先级（所有 key 都按其「有效小组」解析——自己绑的组，或未绑时回落默认透传组）：
//  1. 逻辑名命中 → 取该逻辑名当前主用映射
//  2. 否则按上游真实名回退（具体名兼容，现有 Claude Code 不改配置）
//  3. 组设了透传服务商 → 模型原样透传（默认透传组即用此复刻旧 group_id=NULL 行为）
//  4. 都没有 → not_found
//
// 不做自动故障转移：解析只挑"当前主用"那一条，上游报错原样回送下游。
package main

import (
	"fmt"
)

// RouteTarget 一次请求最终要打到的上游目标。
type RouteTarget struct {
	Provider    *Provider // 目标服务商（base_url / key / format）
	UpstreamID  string    // 改写进请求体的真实模型名
	LogicalName string    // 下游请求的原始名（用于日志/计费归因）
}

// resolveError 区分三类失败，让调用方映射正确状态码与 stop_reason：
//
//	notFound  → 404 not_found_error          （未知模型 / 组里没有）
//	forbidden → 403 permission_error         （白名单拒绝）
//	否则      → 503 api_error                （配置不完整：无可用上游）
type resolveError struct {
	notFound  bool
	forbidden bool
	msg       string
}

func (e *resolveError) Error() string { return e.msg }

func notFoundErr(format string, a ...any) error {
	return &resolveError{notFound: true, msg: fmt.Sprintf(format, a...)}
}
func forbiddenErr(format string, a ...any) error {
	return &resolveError{forbidden: true, msg: fmt.Sprintf(format, a...)}
}
func configErr(format string, a ...any) error {
	return &resolveError{msg: fmt.Sprintf(format, a...)}
}

// effectiveGroupID 返回 key 实际生效的小组 id：自己绑的组，或未绑时回落默认透传组。
// 返回 0 表示连默认组都没有（实例还没配任何服务商/组）。
func effectiveGroupID(km *KeyMeta) int64 {
	if km.GroupID != nil {
		return *km.GroupID
	}
	return providerRegistry.DefaultGroupID()
}

// ResolveRoute 把 (key 元数据, 下游请求模型名) 解析为 RouteTarget。
func ResolveRoute(km *KeyMeta, model string) (*RouteTarget, error) {
	if model == "" {
		return nil, notFoundErr("model is required")
	}
	gid := effectiveGroupID(km)
	if gid == 0 {
		return nil, configErr("no group configured (set up a provider/group in /admin)")
	}

	// 1. 逻辑名命中
	if m, ok := providerRegistry.PrimaryMapping(gid, model); ok {
		return buildTarget(m, model)
	}
	// 2. 具体名兼容：按上游真实名回退
	if m, ok := providerRegistry.MappingByUpstream(gid, model); ok {
		return buildTarget(m, model)
	}
	// 3. 透传：组设了透传服务商 → 模型原样发给它
	if p, ok := providerRegistry.PassthroughProvider(gid); ok {
		return &RouteTarget{Provider: p, UpstreamID: model, LogicalName: model}, nil
	}
	return nil, notFoundErr("model not available in this key's group: %s", model)
}

// buildTarget 把一条映射变成 RouteTarget，校验它指向的服务商存在且启用。
func buildTarget(m GroupMapping, logical string) (*RouteTarget, error) {
	p, ok := providerRegistry.Provider(m.ProviderID)
	if !ok {
		return nil, configErr("mapping %q points to missing provider %d", logical, m.ProviderID)
	}
	if !p.Enabled {
		return nil, configErr("provider %q is disabled", p.Name)
	}
	return &RouteTarget{Provider: p, UpstreamID: m.UpstreamID, LogicalName: logical}, nil
}

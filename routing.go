// routing.go: 下游请求 → 上游路由解析。
//
// 把 (proxy key, 请求模型名) 解析成一个 RouteTarget：去哪个服务商、用什么真实模型名、
// 走什么协议格式。这是多上游架构的中枢。
//
// 解析优先级：
//  1. key 绑定了小组（GroupID != nil）：小组即白名单。
//     a. 逻辑名命中 → 取该逻辑名当前主用映射
//     b. 否则按上游真实名回退（具体名兼容，现有 Claude Code 不改配置）
//     c. 都没有 → not_found
//  2. key 没绑小组（GroupID == nil，旧 key）：保持迁移前语义——
//     a. 未知模型（ResolveAlias 查不到）→ not_found（省一次上游往返）
//     b. 不在 allowed_models 白名单 → forbidden（403）
//     c. 通过 → 回落默认服务商 anthropic-official，model 原样透传
//     若默认服务商不存在 → 配置不完整错误。
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

// ResolveRoute 把 (key 元数据, 下游请求模型名) 解析为 RouteTarget。
func ResolveRoute(km *KeyMeta, model string) (*RouteTarget, error) {
	if model == "" {
		return nil, notFoundErr("model is required")
	}

	// ── 绑定了小组：小组即白名单 ──
	if km.GroupID != nil {
		gid := *km.GroupID
		// a. 逻辑名命中
		if m, ok := providerRegistry.PrimaryMapping(gid, model); ok {
			return buildTarget(m, model)
		}
		// b. 具体名兼容：按上游真实名回退
		if m, ok := providerRegistry.MappingByUpstream(gid, model); ok {
			return buildTarget(m, model)
		}
		return nil, notFoundErr("model not available in this key's group: %s", model)
	}

	// ── 未绑小组（旧 key）：保持迁移前的校验语义 ──
	// 未知模型直接拒（省一次上游往返）；注册表为空时 ResolveAlias fail-open 放行。
	canonical, ok := modelRegistry.ResolveAlias(model)
	if !ok {
		return nil, notFoundErr("model not available via this proxy: %s", model)
	}
	// 旧 key 仍用 allowed_models 白名单（绑组的 key 不走这条）。
	if !km.ModelAllowed(canonical) {
		return nil, forbiddenErr("this proxy key is not permitted to use model: %s", model)
	}

	p, ok := defaultProvider()
	if !ok {
		return nil, configErr("no provider configured (set one in /admin or migrate .env key)")
	}
	// model 原样透传（保 prompt cache），不替换成 canonical——与迁移前行为一致。
	return &RouteTarget{Provider: p, UpstreamID: model, LogicalName: model}, nil
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

// defaultProvider 返回旧 key 的回落服务商：优先 anthropic-official，
// 否则取任一启用的服务商（容忍管理员改了名字）。全程查内存缓存，不碰 DB（热路径）。
func defaultProvider() (*Provider, bool) {
	if p, ok := providerRegistry.ProviderByName(legacyProviderName); ok && p.Enabled {
		return p, true
	}
	return providerRegistry.FirstEnabledProvider()
}

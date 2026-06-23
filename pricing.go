// pricing.go: 模型价格表 + 成本估算
//
// 单位：USD per 1M tokens
// 价格对照 https://platform.claude.com/docs/en/about-claude/pricing （2026-05 核对）
// 缓存倍率官方定义：5m=1.25× / 1h=2× / 读=0.1×（相对 base input）
// 注意：Opus 4.5 起重新定价到 $5/$25，比 Opus 4 / 4.1 的 $15/$75 便宜一档
// 模型版本号匹配优先精确，否则按前缀回退到家族价。
package main

import "strings"

type Price struct {
	Input        float64
	Output       float64
	CacheWrite5m float64
	CacheWrite1h float64
	CacheRead    float64
}

var (
	priceOpusNew = Price{5.0, 25.0, 6.25, 10.0, 0.50}   // Opus 4.5 / 4.6 / 4.7
	priceOpusOld = Price{15.0, 75.0, 18.75, 30.0, 1.50} // Opus 4 / 4.1
	priceSonnet  = Price{3.0, 15.0, 3.75, 6.0, 0.30}    // Sonnet 4 / 4.5 / 4.6
	priceHaiku45 = Price{1.0, 5.0, 1.25, 2.0, 0.10}     // Haiku 4.5
)

var priceTable = map[string]Price{
	// 锁定版（带日期）
	"claude-haiku-4-5-20251001":  priceHaiku45,
	"claude-sonnet-4-5-20250929": priceSonnet,
	"claude-opus-4-5-20251101":   priceOpusNew,
	"claude-opus-4-1-20250805":   priceOpusOld,
	"claude-opus-4-20250514":     priceOpusOld,
	"claude-sonnet-4-20250514":   priceSonnet,

	// 别名
	"claude-haiku-4-5":  priceHaiku45,
	"claude-sonnet-4-6": priceSonnet,
	"claude-opus-4-6":   priceOpusNew,
	"claude-opus-4-7":   priceOpusNew,
}

// 家族级回退（前缀匹配），按当前定价档位估算未知新版
var familyFallback = []struct {
	Prefix string
	Price  Price
}{
	{"claude-opus-", priceOpusNew}, // 假定后续 Opus 沿用新定价档；老 Opus 已在表中精确命中
	{"claude-sonnet-", priceSonnet},
	{"claude-haiku-", priceHaiku45},
}

func lookupPrice(model string) (Price, bool) {
	if p, ok := priceTable[model]; ok {
		return p, true
	}
	for _, f := range familyFallback {
		if strings.HasPrefix(model, f.Prefix) {
			return f.Price, true
		}
	}
	return Price{}, false
}

// Cost 按 token 用量算 USD（单价单位 USD / 1M tokens）。
func (p Price) Cost(input, output, cacheCreate5m, cacheCreate1h, cacheRead int) float64 {
	const M = 1_000_000.0
	return (float64(input)*p.Input +
		float64(output)*p.Output +
		float64(cacheCreate5m)*p.CacheWrite5m +
		float64(cacheCreate1h)*p.CacheWrite1h +
		float64(cacheRead)*p.CacheRead) / M
}

// CostOf 按静态价表算 USD（未知模型 → 0）。
func CostOf(model string, input, output, cacheCreate5m, cacheCreate1h, cacheRead int) float64 {
	p, ok := lookupPrice(model)
	if !ok {
		return 0
	}
	return p.Cost(input, output, cacheCreate5m, cacheCreate1h, cacheRead)
}

// tokenCost 算 token 成本，解析顺序：
//  1. 该服务商对该上游模型设的计价覆盖（修复非 Anthropic 上游被记 $0）
//  2. 静态 Anthropic 家族价表（按真实模型名匹配）
//  3. 都没有 → 0
func tokenCost(providerName, model string, input, output, cacheCreate5m, cacheCreate1h, cacheRead int) float64 {
	// OpenAI Responses usage reports input_tokens inclusive of cached_tokens.
	// Anthropic reports input_tokens separately from cache_read_input_tokens.
	// Normalize here so cached OpenAI input is not billed once at full input
	// price and again at cached-input price.
	if strings.HasPrefix(providerName, "openai") && cacheRead > 0 {
		input -= cacheRead
		if input < 0 {
			input = 0
		}
	}
	if providerRegistry != nil {
		if p, ok := providerRegistry.PriceFor(providerName, model); ok {
			return p.Cost(input, output, cacheCreate5m, cacheCreate1h, cacheRead)
		}
	}
	return CostOf(model, input, output, cacheCreate5m, cacheCreate1h, cacheRead)
}

// Web search 工具：$10 / 1000 次搜索，与模型无关。
// https://platform.claude.com/docs/en/about-claude/pricing#web-search-tool
const WebSearchUSDPerCall = 0.01

func WebSearchCost(searches int) float64 {
	if searches <= 0 {
		return 0
	}
	return float64(searches) * WebSearchUSDPerCall
}

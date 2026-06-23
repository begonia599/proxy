package main

import "strings"

const (
	providerFormatAnthropic = "anthropic"
	providerFormatOpenAI    = "openai"
	providerFormatHybrid    = "hybrid"

	protocolAnthropic = "anthropic"
	protocolOpenAI    = "openai"
)

func validProviderFormat(format string) bool {
	return format == providerFormatAnthropic || format == providerFormatOpenAI || format == providerFormatHybrid
}

func providerSupportsAnthropic(p *Provider) bool {
	return p != nil && (p.Format == providerFormatAnthropic || p.Format == providerFormatHybrid)
}

func providerSupportsOpenAI(p *Provider) bool {
	return p != nil && (p.Format == providerFormatOpenAI || p.Format == providerFormatHybrid)
}

func providerAnthropicBaseURL(p *Provider) string {
	if p == nil {
		return ""
	}
	if strings.TrimSpace(p.AnthropicBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(p.AnthropicBaseURL), "/")
	}
	return strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
}

func providerOpenAIBaseURL(p *Provider) string {
	if p == nil {
		return ""
	}
	if strings.TrimSpace(p.OpenAIBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	}
	return strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
}

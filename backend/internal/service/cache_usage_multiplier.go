package service

import "fmt"

type CacheUsageMultiplierRule struct {
	ThresholdTokens int     `json:"threshold_tokens"`
	Multiplier      float64 `json:"multiplier"`
}

type CacheUsageMultiplierPolicy struct {
	Input         CacheUsageMultiplierRule `json:"input"`
	Output        CacheUsageMultiplierRule `json:"output"`
	CacheRead     CacheUsageMultiplierRule `json:"cache_read"`
	CacheCreation CacheUsageMultiplierRule `json:"cache_creation"`
}

func defaultCacheUsageMultiplierPolicy() CacheUsageMultiplierPolicy {
	rule := CacheUsageMultiplierRule{Multiplier: 1}
	return CacheUsageMultiplierPolicy{Input: rule, Output: rule, CacheRead: rule, CacheCreation: rule}
}

func validateCacheUsageMultiplier(policy CacheUsageMultiplierPolicy) error {
	for name, rule := range map[string]CacheUsageMultiplierRule{"input": policy.Input, "output": policy.Output, "cache_read": policy.CacheRead, "cache_creation": policy.CacheCreation} {
		if rule.ThresholdTokens < 0 || rule.ThresholdTokens > cacheAbsoluteMaxInputTokens || !validCacheFloat(rule.Multiplier, 1000) {
			return fmt.Errorf("config.usage_multiplier.%s requires threshold_tokens 0..4000000 and multiplier 0..1000", name)
		}
	}
	return nil
}

func multiplyCacheUsageField(value int, rule CacheUsageMultiplierRule) int {
	if value <= 0 {
		return 0
	}
	if value <= rule.ThresholdTokens || rule.Multiplier == 1 {
		return value
	}
	return cacheRounded(float64(value) * rule.Multiplier)
}

func (p *cacheEmulationPlan) projectUsageMultiplier(u *ClaudeUsage) {
	raw := *u
	policy := p.policyConfig().UsageMultiplier
	u.InputTokens = multiplyCacheUsageField(raw.InputTokens, policy.Input)
	u.OutputTokens = multiplyCacheUsageField(raw.OutputTokens, policy.Output)
	u.CacheReadInputTokens = multiplyCacheUsageField(raw.CacheReadInputTokens, policy.CacheRead)
	u.CacheCreationInputTokens = multiplyCacheUsageField(raw.CacheCreationInputTokens, policy.CacheCreation)
	if normalizeCacheProtocol(p.policyConfig().Protocol) == PlatformGemini {
		u.CacheCreationInputTokens = 0
	}
	u.CacheCreation5mTokens, u.CacheCreation1hTokens = scaleCacheCreationTTLToTotal(max(raw.CacheCreation5mTokens, 0), max(raw.CacheCreation1hTokens, 0), u.CacheCreationInputTokens)
	p.enforceUsageLimits(u, raw)
}

func usageMultiplierPlan(cfg CacheStrategyConfig, body []byte, model, wire string) *cacheEmulationPlan {
	policy := DefaultCacheUsagePolicy()
	policy.Enabled = cfg.Usage.Enabled
	// This strategy has no timing, cache-evidence, frequency or sampling controls.
	policy.SkipNonStreamUsageProjection = false
	return &cacheEmulationPlan{profile: &cacheProfile{model: model, rawBody: append([]byte(nil), body...), protocolFamily: wire, policy: cfg}, usagePolicy: policy, nonStream: !requestBodyWantsStream(body)}
}

func (p *cacheEmulationPlan) projectOpenAIUsageMultiplier(dst *OpenAIUsage) {
	raw := *dst
	u := ClaudeUsage{InputTokens: max(raw.InputTokens-raw.CacheReadInputTokens-raw.CacheCreationInputTokens, 0), OutputTokens: raw.OutputTokens, CacheReadInputTokens: raw.CacheReadInputTokens, CacheCreationInputTokens: raw.CacheCreationInputTokens}
	p.projectUsageMultiplier(&u)
	dst.InputTokens = saturatingAddInt(u.InputTokens, saturatingAddInt(u.CacheReadInputTokens, u.CacheCreationInputTokens))
	dst.OutputTokens, dst.CacheReadInputTokens, dst.CacheCreationInputTokens = u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens
	// Keep the provider's TTL family; unrelated buckets must not be invented.
	for _, pair := range []struct {
		a, b             *int
		beforeA, beforeB int
	}{
		{&dst.CacheCreation5mTokens, &dst.CacheCreation1hTokens, raw.CacheCreation5mTokens, raw.CacheCreation1hTokens},
		{&dst.CacheCreation30mInputTokens, &dst.CacheCreation12hInputTokens, raw.CacheCreation30mInputTokens, raw.CacheCreation12hInputTokens},
	} {
		if pair.beforeA > 0 || pair.beforeB > 0 {
			*pair.a, *pair.b = scaleCacheCreationTTLToTotal(max(pair.beforeA, 0), max(pair.beforeB, 0), u.CacheCreationInputTokens)
		} else {
			*pair.a, *pair.b = 0, 0
		}
	}
	// Image sub-buckets are parts of the corresponding totals, not extra token buckets.
	dst.ImageInputTokens = scaleCacheUsageDetail(max(raw.ImageInputTokens-raw.ImageCacheReadTokens, 0), max(raw.InputTokens-raw.CacheReadInputTokens-raw.CacheCreationInputTokens, 0), u.InputTokens)
	dst.ImageCacheReadTokens = scaleCacheUsageDetail(raw.ImageCacheReadTokens, raw.CacheReadInputTokens, u.CacheReadInputTokens)
	dst.ImageInputTokens = saturatingAddInt(dst.ImageInputTokens, dst.ImageCacheReadTokens)
	dst.ImageOutputTokens = scaleCacheUsageDetail(raw.ImageOutputTokens, raw.OutputTokens, u.OutputTokens)
}

func scaleCacheUsageDetail(detail, before, after int) int {
	if detail <= 0 || before <= 0 || after <= 0 {
		return 0
	}
	return min(after, cacheRounded(float64(min(detail, before))*float64(after)/float64(before)))
}

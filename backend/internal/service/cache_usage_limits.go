package service

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/resources"
	"github.com/tidwall/gjson"
)

type cacheModelLimits struct {
	Input   int `json:"max_input_tokens"`
	Output  int `json:"max_output_tokens"`
	Context int `json:"context_window"`
}

var cacheModelCatalog struct {
	sync.Once
	models map[string]cacheModelLimits
}

func lookupCacheModelLimits(model string) cacheModelLimits {
	cacheModelCatalog.Do(func() { _ = json.Unmarshal(resources.ModelCatalogJSON, &cacheModelCatalog.models) })
	key := strings.ToLower(strings.TrimSpace(model))
	key = strings.TrimPrefix(key, "models/")
	keys := []string{key, canonicalModelNameForPricing(key), normalizeModelNameForPricing(key)}
	for _, k := range keys {
		if limits, ok := cacheModelCatalog.models[k]; ok && limits.Input > 0 {
			return limits
		}
	}
	// Unknown aliases cannot justify expanding an advertised context window.
	return cacheModelLimits{Input: 200000, Output: 64000, Context: 200000}
}

func (p *cacheEmulationPlan) enforceUsageLimits(u *ClaudeUsage, raw ClaudeUsage) {
	if p != nil && u != nil {
		defer enforceNonzeroClaudeUsage(u, raw)
	}
	if p == nil || p.profile == nil || u == nil || p.skipProjection() || !p.usagePolicy.Enabled {
		return
	}
	cfg := p.policyConfig()
	if cfg.Kind == CacheStrategyKindDisabled {
		return
	}
	if cfg.Kind == CacheStrategyKindToolPlus {
		_, rawMode, _ := p.selectedPlusProfile()
		if rawMode {
			return
		}
	}
	limits := lookupCacheModelLimits(p.profile.model)
	inputCap := minPositiveCacheLimit(limits.Input, limits.Context, cfg.ReportedInputMaxTokens, cfg.MaxSimulatedInputTokens, cfg.Limits.MaxInputTokens)
	if cfg.Kind == CacheStrategyKindUsageMultiplier {
		inputCap = minPositiveCacheLimit(limits.Input, limits.Context, cfg.Limits.MaxInputTokens)
	}
	outputCap := minPositiveCacheLimit(limits.Output, cfg.Limits.MaxOutputTokens)
	body := p.profile.rawBody
	for _, key := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens", "generationConfig.maxOutputTokens"} {
		if value := gjson.GetBytes(body, key).Int(); value > 0 {
			outputCap = min(outputCap, int(value))
		}
	}
	policy := p.usagePolicy
	if cfg.Kind == CacheStrategyKindUsageMultiplier {
		policy = CacheUsagePolicy{FinalOutputGuardEnabled: boolPtr(false)}
		cfg.CreationControl = CacheCreationControl{}
		cfg.MaxNewCreationTokensPerRequest = 0
	}
	if cfg.Kind == CacheStrategyKindBounded {
		defer func() { p.costReported = cacheTokenCost(*u, p.pricing) }()
	}
	if raw.OutputTokens <= 0 {
		u.OutputTokens = 0
	}
	u.OutputTokens = min(max(u.OutputTokens, 0), outputCap)
	if policy.OutputGuardOn() {
		u.OutputTokens = min(u.OutputTokens, minPositiveCacheLimit(policy.FinalOutputMaxTokens))
	}
	if limits.Context > 0 {
		inputCap = min(inputCap, max(limits.Context-u.OutputTokens, 0))
	}
	u.InputTokens = max(u.InputTokens, 0)
	u.CacheReadInputTokens = min(max(u.CacheReadInputTokens, 0), cacheAbsoluteMaxInputTokens)
	u.CacheCreationInputTokens = min(max(u.CacheCreationInputTokens, 0), cacheAbsoluteMaxInputTokens)
	explicit := normalizeCacheProtocol(cfg.Protocol) == PlatformGemini && gjson.GetBytes(body, "cachedContent").String() != ""
	if !explicit {
		u.CacheReadInputTokens = min(u.CacheReadInputTokens, minPositiveCacheLimit(policy.FinalCacheReadMaxTokens))
		u.CacheCreationInputTokens = min(u.CacheCreationInputTokens, minPositiveCacheLimit(policy.FinalCacheCreationMaxTokens))
	}
	if cfg.Kind == CacheStrategyKindToolPlus {
		shape, unshaped, _ := p.selectedPlusProfile()
		if !unshaped {
			u.InputTokens = min(u.InputTokens, minPositiveCacheLimit(shape.Input.MaxTokens))
			u.OutputTokens = min(u.OutputTokens, minPositiveCacheLimit(shape.Output.MaxTokens))
			if !explicit {
				u.CacheReadInputTokens = min(u.CacheReadInputTokens, minPositiveCacheLimit(shape.CacheRead.MaxTokens))
				u.CacheCreationInputTokens = min(u.CacheCreationInputTokens, minPositiveCacheLimit(shape.CacheCreation.MaxTokens))
			}
		}
	}
	if cfg.CreationControl.Enabled {
		if p.creationControlDecision.enabled {
			u.CacheCreationInputTokens = min(u.CacheCreationInputTokens, max(p.creationControlDecision.allowed, 0))
		}
		u.CacheCreationInputTokens = min(u.CacheCreationInputTokens, minPositiveCacheLimit(cfg.CreationControl.MaxCreationTokensPerEvent))
	}
	u.CacheCreationInputTokens = min(u.CacheCreationInputTokens, minPositiveCacheLimit(cfg.MaxNewCreationTokensPerRequest))
	if policy.CacheCreation.Mode == CacheUsageFieldSampleMax && policy.CacheCreation.MaxTokens > 0 {
		u.CacheCreationInputTokens = min(u.CacheCreationInputTokens, policy.CacheCreation.MaxTokens)
	}
	if policy.Output.Mode == CacheUsageFieldSampleMax && policy.Output.MaxTokens > 0 {
		u.OutputTokens = min(u.OutputTokens, policy.Output.MaxTokens)
	}
	if policy.Input.Mode == CacheUsageFieldSampleMax && policy.Input.MaxTokens > 0 {
		u.InputTokens = min(u.InputTokens, policy.Input.MaxTokens)
	}
	if !explicit && policy.CacheRead.Mode == CacheUsageFieldSampleMax && policy.CacheRead.MaxTokens > 0 {
		u.CacheReadInputTokens = min(u.CacheReadInputTokens, policy.CacheRead.MaxTokens)
	}
	for _, field := range []struct {
		value  *int
		policy CacheUsageFieldPolicy
		cache  bool
	}{
		{&u.InputTokens, policy.Input, false}, {&u.OutputTokens, policy.Output, false},
		{&u.CacheReadInputTokens, policy.CacheRead, true}, {&u.CacheCreationInputTokens, policy.CacheCreation, true},
	} {
		if explicit && field.cache {
			continue
		}
		if field.policy.Mode == CacheUsageFieldSampleTarget && field.policy.TargetTokens > 0 {
			*field.value = min(*field.value, safeScaledTokenLimit(field.policy.TargetTokens, field.policy.NormalMaxMultiplier))
		}
	}
	// Shrink the three buckets together so a hot request remains a cache hit at the model boundary.
	u.InputTokens = min(u.InputTokens, cacheAbsoluteMaxInputTokens)
	total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if total > inputCap {
		before := []int{u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens}
		fields := []*int{&u.InputTokens, &u.CacheReadInputTokens, &u.CacheCreationInputTokens}
		remaining := inputCap
		for i, field := range fields {
			*field = int(int64(before[i]) * int64(inputCap) / int64(total))
			remaining -= *field
		}
		for i, field := range fields {
			add := min(remaining, before[i]-*field)
			*field += add
			remaining -= add
		}
	}
	if raw.InputTokens == 0 && raw.CacheReadInputTokens == 0 && raw.CacheCreationInputTokens == 0 && p.profile.runtimeInputTokens == 0 {
		u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens = 0, 0, 0
	}
	if cfg.Kind == CacheStrategyKindUsageMultiplier {
		u.CacheCreation5mTokens, u.CacheCreation1hTokens = scaleCacheCreationTTLToTotal(u.CacheCreation5mTokens, u.CacheCreation1hTokens, u.CacheCreationInputTokens)
	} else {
		u.CacheCreation5mTokens, u.CacheCreation1hTokens = capCacheCreationBreakdown(u.CacheCreation5mTokens, u.CacheCreation1hTokens, u.CacheCreationInputTokens)
	}
}

func minPositiveCacheLimit(values ...int) int {
	limit := cacheAbsoluteMaxInputTokens
	for _, v := range values {
		if v > 0 {
			limit = min(limit, v)
		}
	}
	return limit
}

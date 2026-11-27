package service

import (
	"context"
	"math"

	"github.com/tidwall/gjson"
)

type geminiCacheProtocol struct{}

func (geminiCacheProtocol) matches(wire string) bool {
	return wire == "gemini" || wire == "gemini_generate_content"
}

func (geminiCacheProtocol) normalize(c *CacheStrategyConfig) {
	c.ForcedTTLTier = ""
	c.CreationControl = CacheCreationControl{}
	c.CreationRatio = 0
	c.Usage.CacheCreation = CacheUsageFieldPolicy{Mode: CacheUsageFieldPreserve, NormalMaxMultiplier: 1.1}
	c.Usage.FinalCacheCreationMaxTokens = 0
	c.Usage.FinalCacheCreationJitterMinTokens, c.Usage.FinalCacheCreationJitterMaxTokens = 0, 0
	c.HitShaping.MinCacheCreationTokens, c.HitShaping.MinCreationRatio = 0, 0
}

func (geminiCacheProtocol) prepare(_ *GatewayService, ctx context.Context, account *Account, group *Group, body []byte, model, _ string, _ int) *cacheEmulationPlan {
	cfg, enabled := effectiveCacheStrategyConfig(group)
	if !enabled || account == nil || account.ID <= 0 {
		return nil
	}
	profile := buildGeminiCacheProfile(body, model, cfg)
	p := &cacheEmulationPlan{profile: profile, usagePolicy: cfg.Usage, nonStream: !requestBodyWantsStream(body)}
	if profile == nil {
		return nil
	}
	// Explicit resources retain upstream accounting; no synthetic state is attached.
	strict := cfg.Usage.StrictCacheEligibilityNonStream
	if requestBodyWantsStream(body) {
		strict = cfg.Usage.StrictCacheEligibilityStream
	}
	if strict && profile.lastCacheableBreakpoint() == nil && gjson.GetBytes(body, "cachedContent").String() == "" {
		p.projectionIneligible = true
		return p
	}
	if gjson.GetBytes(body, "cachedContent").String() != "" {
		return p
	}
	if cfg.Kind == CacheStrategyKindToolPlus {
		_, _, disable := p.selectedPlusProfile()
		if disable {
			profile.breakpoints = nil
			return p
		}
	}
	p.cacheKey = cacheStrategyCacheKey(account, group, "gemini_generate_content", model, body, false, 0)
	if p.cacheKey != 0 {
		p.usage = globalCacheTracker.compute(ctx, nil, p.cacheKey, profile)
		if p.usage != nil {
			write := p.usage.CacheCreationInputTokens
			if cfg.MaxNewCreationTokensPerRequest > 0 {
				write = min(write, cfg.MaxNewCreationTokensPerRequest)
			}
			if !cfg.IncrementalCreateEnabled && p.usage.CacheReadInputTokens > 0 {
				write = 0
			}
			limitCacheProfileWriteSet(profile, p.usage.CacheReadInputTokens, write)
		}
	}
	return p
}

// Gemini has three generation buckets: uncached prompt, cached prompt, output.
func projectGeminiCacheUsage(p *cacheEmulationPlan, raw ClaudeUsage) (result ClaudeUsage) {
	if p == nil {
		return raw
	}
	p.repairClaudeUsage(&raw)
	defer func() { enforceNonzeroClaudeUsage(&result, raw) }()
	defer func() { p.enforceUsageLimits(&result, raw) }()
	if p.guardOnly || p.skipProjection() || !p.usagePolicy.Enabled {
		p.limitRepairedUsage(&raw)
		return raw
	}
	if p.policyConfig().Kind == CacheStrategyKindUsageMultiplier {
		u := raw
		p.projectUsageMultiplier(&u)
		return u
	}
	cfg, policy, seed := p.policyConfig(), p.usagePolicy, p.usageSeed()
	if cfg.Kind == CacheStrategyKindToolPlus {
		policy.OutputUpliftEnabled = boolPtr(false)
	}
	u := raw
	total := saturatingAddInt(max(raw.InputTokens, 0), max(raw.CacheReadInputTokens, 0))
	explicit := gjson.GetBytes(p.profile.rawBody, "cachedContent").String() != ""
	preserve := explicit || policy.PreserveUpstreamCacheUsage && raw.CacheReadInputTokens > 0
	hasRead := raw.CacheReadInputTokens > 0 || p.usage != nil && p.usage.CacheReadInputTokens > 0
	if !preserve {
		if cfg.TokenScale > 1 && total >= cfg.ScaleMinInputTokens {
			total = cacheRounded(float64(total) * cfg.TokenScale)
		}
		total = max(total, cfg.ReportedInputMinTokens)
		total = min(total, cachePositiveCap(cfg.ReportedInputMaxTokens), cachePositiveCap(cfg.MaxSimulatedInputTokens))
		read := 0
		if p.usage != nil && p.profile.totalInputTokens > 0 {
			ratio := cfg.ReadRatio
			if cfg.RatioMode == CacheRatioModeUniform {
				ratio = cfg.UsageRatio
			}
			read = cacheRounded(float64(total) * float64(p.usage.CacheReadInputTokens) / float64(p.profile.totalInputTokens) * ratio)
		}
		if hasRead && cfg.HitShaping.Enabled {
			h := cfg.HitShaping
			ratio := math.Max(0, math.Min(1, h.TargetHitRatio+h.RatioJitter*(2*cacheShapingUnit(seed)-1)))
			target := max(cacheRounded(float64(total)*ratio), h.MinCacheReadTokens)
			if h.MaxReadMultiplier > 0 {
				target = min(target, cacheRounded(float64(max(read, 1))*h.MaxReadMultiplier))
			}
			read = target
		}
		if cfg.Kind != CacheStrategyKindStableSegment {
			read = scaleCacheTokens(read, cfg.CoverageRatio)
		}
		if cfg.MaxCoverageTokens > 0 {
			read = min(read, cfg.MaxCoverageTokens)
		}
		u.CacheReadInputTokens = min(max(read, 0), total)
		u.InputTokens = total - u.CacheReadInputTokens
	}
	input := projectUsageField(policy.Input, u.InputTokens, seed^0x11)
	if input < u.InputTokens && policy.Input.MoveDeltaToCacheRead && hasRead && !explicit {
		u.CacheReadInputTokens += u.InputTokens - input
	}
	u.InputTokens = input
	u.OutputTokens = projectUsageFieldWithRaw(policy.Output, u.OutputTokens, raw.OutputTokens, seed^0x22)
	if !explicit {
		u.CacheReadInputTokens = projectUsageField(policy.CacheRead, u.CacheReadInputTokens, seed^0x33)
	}
	if cfg.Kind == CacheStrategyKindToolPlus {
		profile, rawMode, disable := p.selectedPlusProfile()
		mode := "inherit"
		if rawMode {
			mode = "raw"
		}
		if disable && !explicit {
			u.InputTokens = saturatingAddInt(u.InputTokens, u.CacheReadInputTokens)
			u.CacheReadInputTokens = 0
		}

		if mode == "raw" {
			if disable && !explicit {
				raw.InputTokens = saturatingAddInt(raw.InputTokens, raw.CacheReadInputTokens)
				raw.CacheReadInputTokens = 0
			}
			p.limitRepairedUsage(&raw)
			return raw
		}
		beforeRead := u.CacheReadInputTokens
		u.InputTokens = cacheShapeField(cacheShapeField(u.InputTokens, profile.Input, seed^1, false), profile.Input, seed^1, true)
		u.OutputTokens = cacheShapeField(cacheShapeField(u.OutputTokens, profile.Output, seed^2, false), profile.Output, seed^2, true)
		if !explicit {
			u.CacheReadInputTokens = cacheShapeField(cacheShapeField(u.CacheReadInputTokens, profile.CacheRead, seed^3, false), profile.CacheRead, seed^3, true)
		}
		if profile.CapOverflowToInput && !explicit {
			u.InputTokens = saturatingAddInt(u.InputTokens, max(beforeRead-u.CacheReadInputTokens, 0))
		}
	}
	if policy.OutputGuardOn() {
		if policy.OutputUpliftOn() && u.OutputTokens > policy.OutputUpliftMinTokens {
			u.OutputTokens = cacheRounded(float64(u.OutputTokens) * (1 + float64(min(policy.OutputUpliftPercent, 200))/100))
		}
		u.OutputTokens = applyFinalCapWithJitter(u.OutputTokens, policy.FinalOutputMaxTokens, policy.FinalOutputJitterMinTokens, policy.FinalOutputJitterMaxTokens, seed^0x55)
	}
	if cfg.Kind == CacheStrategyKindStableSegment && !explicit && u.CacheReadInputTokens > 0 {
		lo, hi := cfg.StableSegment.ReportedInputMinTokens, cfg.StableSegment.ReportedInputMaxTokens
		if hi >= lo && hi > 0 {
			target := lo + int(splitmix64(seed)%uint64(hi-lo+1))
			if u.InputTokens > hi {
				u.CacheReadInputTokens += u.InputTokens - target
				u.InputTokens = target
			} else if u.InputTokens < lo {
				transfer := min(target-u.InputTokens, u.CacheReadInputTokens)
				u.InputTokens += transfer
				u.CacheReadInputTokens -= transfer
			}
		}
	}
	if !explicit && u.InputTokens < cfg.UncachedInputMinTokens {
		band := uncachedInputFloor(cfg)
		target := jitterWithin(band.min, max(band.min, band.max), seed)
		transfer := min(target-u.InputTokens, u.CacheReadInputTokens)
		u.InputTokens += transfer
		u.CacheReadInputTokens -= transfer
	}
	if !hasRead && cfg.ColdRequestGuard {
		u.CacheReadInputTokens = 0
		u.InputTokens = min(u.InputTokens, raw.InputTokens)
	}
	if !explicit {
		u.CacheReadInputTokens = applyFinalCapWithJitter(u.CacheReadInputTokens, policy.FinalCacheReadMaxTokens, policy.FinalCacheReadJitterMinTokens, policy.FinalCacheReadJitterMaxTokens, seed^0x66)
	}

	if cfg.Kind == CacheStrategyKindBounded {
		if policy.Input.Mode == CacheUsageFieldSampleMax && policy.Input.MaxTokens > 0 {
			u.InputTokens = min(u.InputTokens, policy.Input.MaxTokens)
		}
		if !explicit {
			u.CacheReadInputTokens = min(u.CacheReadInputTokens, cachePositiveCap(policy.FinalCacheReadMaxTokens))
		}
		if policy.OutputGuardOn() {
			u.OutputTokens = min(u.OutputTokens, cachePositiveCap(policy.FinalOutputMaxTokens))
		}
	}
	u.CacheCreationInputTokens, u.CacheCreation5mTokens, u.CacheCreation1hTokens = 0, 0, 0
	return u
}

package service

import "math"

// Advanced projection uses split input buckets internally; OpenAI totals are converted at the boundary.
func (p *cacheEmulationPlan) projectClaude(dst *ClaudeUsage) {
	if p == nil || dst == nil || normalizeCacheProtocol(p.policyConfig().Protocol) != PlatformAnthropic {
		return
	}
	p.repairClaudeUsage(dst)
	defer func(raw ClaudeUsage) { enforceNonzeroClaudeUsage(dst, raw) }(*dst)
	if p.guardOnly || p.skipProjection() || !p.usagePolicy.Enabled {
		p.limitRepairedUsage(dst)
		return
	}
	if p.policyConfig().Kind == CacheStrategyKindUsageMultiplier {
		p.projectUsageMultiplier(dst)
		return
	}
	raw := *dst
	if p.policyConfig().Kind == CacheStrategyKindToolPlus {
		_, rawMode, disable := p.selectedPlusProfile()
		if rawMode {
			if disable {
				dst.InputTokens = saturatingAddInt(dst.InputTokens, saturatingAddInt(dst.CacheReadInputTokens, dst.CacheCreationInputTokens))
				dst.CacheReadInputTokens, dst.CacheCreationInputTokens, dst.CacheCreation5mTokens, dst.CacheCreation1hTokens = 0, 0, 0, 0
			}
			p.limitRepairedUsage(dst)
			return
		}
	}
	defer p.enforceUsageLimits(dst, raw)
	policy := p.usagePolicy
	if p.policyConfig().Kind == CacheStrategyKindToolPlus {
		policy.Input.Mode = CacheUsageFieldPreserve
		policy.OutputUpliftEnabled = boolPtr(false)
	}
	if p.policyConfig().Kind == CacheStrategyKindToolPlus && p.result() != nil {
		dst.InputTokens = p.usage.InputTokens
		dst.CacheReadInputTokens = p.usage.CacheReadInputTokens
		dst.CacheCreationInputTokens = p.usage.CacheCreationInputTokens
		dst.CacheCreation5mTokens = p.usage.CacheCreation5mInputTokens
		dst.CacheCreation1hTokens = p.usage.CacheCreation1hInputTokens
	} else {
		projectClaudeUsage(dst, p.result(), policy, p.usageSeed())
	}
	cfg := p.policyConfig()
	if !claudeUsageHasCacheEvidence(&raw) {
		if p.result() == nil {
			applyReportedInputWithoutCacheClaude(dst, p)
		} else if cfg.Kind != CacheStrategyKindToolPlus {
			shaped := &cacheEmulationUsage{InputTokens: dst.InputTokens, CacheReadInputTokens: dst.CacheReadInputTokens, CacheCreationInputTokens: dst.CacheCreationInputTokens, CacheCreation5mInputTokens: dst.CacheCreation5mTokens, CacheCreation1hInputTokens: dst.CacheCreation1hTokens}
			constrainReportedCacheUsage(shaped, p.profile.reportedInputTokens, uncachedInputFloor(cfg), p.usageSeed())
			dst.InputTokens = shaped.InputTokens
			dst.CacheReadInputTokens = shaped.CacheReadInputTokens
			dst.CacheCreationInputTokens = shaped.CacheCreationInputTokens
			dst.CacheCreation5mTokens = shaped.CacheCreation5mInputTokens
			dst.CacheCreation1hTokens = shaped.CacheCreation1hInputTokens
		}
	}
	if cfg.Kind == CacheStrategyKindToolPlus {
		p.shapePlus(dst, raw)
	} else if cfg.HitShaping.Enabled || cfg.Kind == CacheStrategyKindBounded {
		p.shapeHit(dst, raw)
	}
	if cfg.Kind == CacheStrategyKindStableSegment && (dst.CacheReadInputTokens > 0 || dst.CacheCreationInputTokens > 0) {
		applyStableReportedInputRange(dst, cfg.StableSegment, p.usageSeed())
	}
	if cfg.ColdRequestGuard && p.result() != nil && p.result().CacheReadInputTokens == 0 && raw.CacheReadInputTokens == 0 && raw.CacheCreationInputTokens == 0 {
		constrainColdUsage(dst, raw, p.profile.runtimeInputTokens)
	}
	if cfg.Kind == CacheStrategyKindBounded {
		p.enforceBounds(dst)
	}
	applyTTLTierToClaudeUsage(dst, resolveReportedTTLTier(cfg, p.profile.clientTTLTier(), raw.CacheCreation5mTokens, raw.CacheCreation1hTokens))
	if cfg.Kind == CacheStrategyKindBounded {
		p.costReported = cacheTokenCost(*dst, p.pricing)
	}
}

func (p *cacheEmulationPlan) projectOpenAI(dst *OpenAIUsage) {
	if p == nil || dst == nil || normalizeCacheProtocol(p.policyConfig().Protocol) != PlatformAnthropic {
		return
	}
	p.repairOpenAIUsage(dst)
	defer func(raw OpenAIUsage) {
		u := ClaudeUsage{InputTokens: max(dst.InputTokens-dst.CacheReadInputTokens-dst.CacheCreationInputTokens, 0), OutputTokens: dst.OutputTokens, CacheReadInputTokens: dst.CacheReadInputTokens, CacheCreationInputTokens: dst.CacheCreationInputTokens}
		enforceNonzeroClaudeUsage(&u, ClaudeUsage{OutputTokens: raw.OutputTokens})
		dst.InputTokens = saturatingAddInt(u.InputTokens, saturatingAddInt(u.CacheReadInputTokens, u.CacheCreationInputTokens))
		dst.OutputTokens, dst.CacheReadInputTokens, dst.CacheCreationInputTokens = u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens
	}(*dst)
	if p.guardOnly || p.skipProjection() || !p.usagePolicy.Enabled {
		u := ClaudeUsage{InputTokens: max(dst.InputTokens-dst.CacheReadInputTokens-dst.CacheCreationInputTokens, 0), OutputTokens: dst.OutputTokens, CacheReadInputTokens: dst.CacheReadInputTokens, CacheCreationInputTokens: dst.CacheCreationInputTokens, rawUsage: dst.rawUsage}
		p.limitRepairedUsage(&u)
		dst.InputTokens = saturatingAddInt(u.InputTokens, saturatingAddInt(u.CacheReadInputTokens, u.CacheCreationInputTokens))
		dst.OutputTokens = u.OutputTokens
		return
	}
	if p.policyConfig().Kind == CacheStrategyKindUsageMultiplier {
		p.projectOpenAIUsageMultiplier(dst)
		return
	}
	rawUsage := *dst
	defer func() {
		u := ClaudeUsage{InputTokens: max(dst.InputTokens-dst.CacheReadInputTokens-dst.CacheCreationInputTokens, 0), OutputTokens: dst.OutputTokens, CacheReadInputTokens: dst.CacheReadInputTokens, CacheCreationInputTokens: dst.CacheCreationInputTokens}
		raw := ClaudeUsage{InputTokens: max(rawUsage.InputTokens-rawUsage.CacheReadInputTokens-rawUsage.CacheCreationInputTokens, 0), OutputTokens: rawUsage.OutputTokens, CacheReadInputTokens: rawUsage.CacheReadInputTokens, CacheCreationInputTokens: rawUsage.CacheCreationInputTokens}
		p.enforceUsageLimits(&u, raw)
		dst.InputTokens = saturatingAddInt(u.InputTokens, saturatingAddInt(u.CacheReadInputTokens, u.CacheCreationInputTokens))
		dst.OutputTokens = u.OutputTokens
		dst.CacheReadInputTokens = u.CacheReadInputTokens
		dst.CacheCreationInputTokens = u.CacheCreationInputTokens
	}()
	cfg := p.policyConfig()
	if cfg.Kind != CacheStrategyKindStableSegment && cfg.Kind != CacheStrategyKindToolPlus && cfg.Kind != CacheStrategyKindBounded && !cfg.HitShaping.Enabled && !cfg.ColdRequestGuard {
		evidence := openAIUsageHasCacheEvidence(dst)
		projectOpenAIUsage(dst, p.result(), p.usagePolicy, p.usageSeed())
		if !evidence {
			if p.result() == nil {
				applyReportedInputWithoutCacheOpenAI(dst, p)
			} else {
				constrainOpenAIUsageTotal(dst, p.profile.reportedInputTokens, uncachedInputFloor(cfg).min)
			}
		}
		return
	}
	// OpenAI input_tokens includes read and creation; Claude input_tokens excludes both.
	u := ClaudeUsage{InputTokens: max(dst.InputTokens-dst.CacheReadInputTokens-dst.CacheCreationInputTokens, 0), OutputTokens: dst.OutputTokens, CacheReadInputTokens: dst.CacheReadInputTokens, CacheCreationInputTokens: dst.CacheCreationInputTokens, UpstreamBillingScale: dst.UpstreamBillingScale, rawUsage: dst.rawUsage}
	p.projectClaude(&u)
	dst.InputTokens = saturatingAddInt(u.InputTokens, saturatingAddInt(u.CacheReadInputTokens, u.CacheCreationInputTokens))
	dst.OutputTokens = u.OutputTokens
	dst.CacheReadInputTokens = u.CacheReadInputTokens
	dst.CacheCreationInputTokens = u.CacheCreationInputTokens
}

func cacheTokenCost(u ClaudeUsage, p *ModelPricing) float64 {
	if p == nil {
		return 0
	}
	write5m, write1h := p.CacheCreationPricePerToken, p.CacheCreationPricePerToken
	if p.SupportsCacheBreakdown {
		if p.CacheCreation5mPrice > 0 {
			write5m = p.CacheCreation5mPrice
		}
		if p.CacheCreation1hPrice > 0 {
			write1h = p.CacheCreation1hPrice
		}
	}
	assigned := min(max(u.CacheCreation1hTokens, 0), max(u.CacheCreationInputTokens, 0))
	return float64(u.InputTokens)*p.InputPricePerToken + float64(u.OutputTokens)*p.OutputPricePerToken + float64(u.CacheReadInputTokens)*p.CacheReadPricePerToken + float64(u.CacheCreationInputTokens-assigned)*write5m + float64(assigned)*write1h
}

func (p *cacheEmulationPlan) creationAllowed() bool {
	return p.result() != nil && (!p.creationControlDecision.enabled || p.creationControlDecision.allowed > 0)
}

func cachePositiveCap(v int) int {
	if v > 0 {
		return v
	}
	return cacheAbsoluteMaxInputTokens
}
func cacheShapingUnit(seed uint64) float64 { return float64(splitmix64(seed)%10001) / 10000 }
func cacheRounded(v float64) int {
	return int(math.Min(math.Max(math.Round(v), 0), float64(cacheAbsoluteMaxInputTokens)))
}

func (p *cacheEmulationPlan) shapingCaps() (int, int, int) {
	c := p.policyConfig()
	seed := p.usageSeed()
	input := 0
	if c.Usage.Input.Mode == CacheUsageFieldSampleMax {
		input = c.Usage.Input.MaxTokens
	} else if c.Usage.Input.Mode == CacheUsageFieldSampleTarget {
		input = safeScaledTokenLimit(c.Usage.Input.TargetTokens, c.Usage.Input.NormalMaxMultiplier)
	}
	read, write := cachePositiveCap(c.Usage.FinalCacheReadMaxTokens), cachePositiveCap(c.Usage.FinalCacheCreationMaxTokens)
	if c.CreationControl.Enabled && p.creationControlDecision.enabled {
		write = min(write, max(p.creationControlDecision.allowed, 0))
	}
	if c.HitShaping.MaxReadMultiplier > 0 {
		read = min(read, cacheRounded(float64(max(p.profile.runtimeInputTokens, 1))*c.HitShaping.MaxReadMultiplier))
	}
	if c.Kind == CacheStrategyKindBounded {
		input = cachePositiveCap(input)
		input = max(1, input-int(float64(input)*.15*cacheShapingUnit(seed^0x2545f4914f6cdd1d)))
		read = max(1, read-int(float64(read)*.05*cacheShapingUnit(seed^0x9e6c63d0676a9a99)))
		if c.CreationControl.Enabled && c.CreationControl.MaxCreationTokensPerEvent > 0 {
			write = min(write, c.CreationControl.MaxCreationTokensPerEvent)
		}
		if write > 0 {
			write = max(1, write-int(float64(write)*.05*cacheShapingUnit(seed^0x7c8f1e2b5a3d9f01)))
		}
	}
	return cachePositiveCap(input), read, write
}

func (p *cacheEmulationPlan) enforceBounds(u *ClaudeUsage) {
	input, read, write := p.shapingCaps()
	h := p.policyConfig().HitShaping
	if u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens > 0 {
		u.InputTokens = min(max(u.InputTokens, 1), input)
	}
	clamp := func(v, lo, hi int) int {
		if v <= 0 || hi < lo {
			return 0
		}
		return min(max(v, lo), hi)
	}
	rmin, wmin := 0, 0
	if h.Enabled {
		rmin, wmin = h.MinCacheReadTokens, h.MinCacheCreationTokens
	}
	u.CacheReadInputTokens = clamp(u.CacheReadInputTokens, rmin, read)
	if !p.creationAllowed() {
		write = 0
	}
	u.CacheCreationInputTokens = clamp(u.CacheCreationInputTokens, wmin, write)
}

func (p *cacheEmulationPlan) shapeHit(u *ClaudeUsage, raw ClaudeUsage) {
	c := p.policyConfig()
	h := c.HitShaping
	bounded := c.Kind == CacheStrategyKindBounded
	if !h.Enabled {
		return
	}
	_, readCap, writeCap := p.shapingCaps()
	allowed := p.creationAllowed()
	reusable := p.result() != nil && p.result().CacheReadInputTokens > 0 || raw.CacheReadInputTokens > 0
	target := math.Min(h.TargetHitRatio+h.RatioJitter*cacheShapingUnit(p.usageSeed()^0x3c6ef372fe94f82b), .99)
	total := saturatingAddInt(u.InputTokens, saturatingAddInt(u.CacheReadInputTokens, u.CacheCreationInputTokens))
	base := *u
	split := func(ratio float64) ClaudeUsage {
		v := base
		read := min(cacheRounded(float64(total)*ratio), readCap)
		rest := max(total-read, 0)
		if allowed {
			v.InputTokens = min(max(v.InputTokens, 0), rest)
			v.CacheCreationInputTokens = min(max(rest-v.InputTokens, 0), writeCap)
			v.InputTokens += max(rest-v.InputTokens-v.CacheCreationInputTokens, 0)
		} else {
			v.InputTokens = rest
			v.CacheCreationInputTokens = 0
		}
		v.CacheReadInputTokens = read
		return v
	}
	if reusable && target > 0 {
		*u = split(target)
	}
	floor := cacheTokenCost(raw, p.pricing) * h.MinCostRatio
	if bounded && allowed && h.CostFloorTopUpInput {
		floor += globalCacheTracker.controlState(p.cacheKey).costDeficit
	}
	if !bounded && !h.ReadAwareCostFloor && reusable && cacheTokenCost(*u, p.pricing) < floor && cacheTokenCost(split(0), p.pricing) >= floor {
		lo, hi := 0.0, target
		for i := 0; i < 40; i++ {
			mid := (lo + hi) / 2
			if cacheTokenCost(split(mid), p.pricing) >= floor {
				lo = mid
			} else {
				hi = mid
			}
		}
		*u = split(math.Max(0, lo-h.RatioJitter*cacheShapingUnit(p.usageSeed()^0x9f1d2b7c44e8a613)))
	}
	if !allowed {
		u.InputTokens = saturatingAddInt(u.InputTokens, u.CacheCreationInputTokens)
		u.CacheCreationInputTokens = 0
	}
	if allowed && h.MinCreationRatio > 0 {
		u.CacheCreationInputTokens = max(u.CacheCreationInputTokens, min(writeCap, max(h.MinCacheCreationTokens, cacheRounded(float64(p.profile.runtimeInputTokens)*h.MinCreationRatio))))
	}
	if u.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = min(readCap, max(u.CacheReadInputTokens, h.MinCacheReadTokens))
	}
	if u.CacheCreationInputTokens > 0 && allowed {
		u.CacheCreationInputTokens = min(writeCap, max(u.CacheCreationInputTokens, h.MinCacheCreationTokens))
	}
	if p.pricing != nil && floor > 0 {
		if !bounded && h.ReadAwareCostFloor && reusable && target > 0 {
			for i := 0; i < 8 && cacheTokenCost(*u, p.pricing) < floor; i++ {
				share := target / (1 - target)
				readRoom := max(readCap-u.CacheReadInputTokens, 0)
				if readRoom == 0 {
					share = 0
				}
				room, price := 0, 0.0
				if allowed && u.CacheCreationInputTokens < writeCap {
					room = writeCap - u.CacheCreationInputTokens
					price = p.pricing.CacheCreationPricePerToken
				} else if h.CostFloorTopUpInput {
					room = cacheAbsoluteMaxInputTokens - u.InputTokens
					price = p.pricing.InputPricePerToken
				}
				unit := price + share*p.pricing.CacheReadPricePerToken
				if room <= 0 || unit <= 0 {
					break
				}
				n := min(room, int(math.Ceil((floor-cacheTokenCost(*u, p.pricing))/unit)))
				if share > 0 {
					n = min(n, int(float64(readRoom)/share))
					if n <= 0 {
						u.CacheReadInputTokens += readRoom
						continue
					}
				}
				if allowed && u.CacheCreationInputTokens < writeCap {
					u.CacheCreationInputTokens += n
				} else {
					u.InputTokens += n
				}
				u.CacheReadInputTokens += min(readRoom, cacheRounded(float64(n)*share))
			}
		}
		add := func(value *int, cap int, price float64) {
			gap := floor - cacheTokenCost(*u, p.pricing)
			if gap > 0 && price > 0 && *value < cap {
				*value += min(cap-*value, int(math.Ceil(gap/price)))
			}
		}
		if allowed {
			add(&u.CacheCreationInputTokens, writeCap, p.pricing.CacheCreationPricePerToken)
		}
		if bounded && reusable {
			add(&u.CacheReadInputTokens, readCap, p.pricing.CacheReadPricePerToken)
		}
		if h.CostFloorTopUpInput {
			cap := cacheAbsoluteMaxInputTokens
			if bounded {
				cap, _, _ = p.shapingCaps()
			}
			add(&u.InputTokens, cap, p.pricing.InputPricePerToken)
		}
	}
	if reusable && target > 0 {
		u.CacheReadInputTokens = max(u.CacheReadInputTokens, min(readCap, int(math.Ceil(target*float64(u.InputTokens+u.CacheCreationInputTokens)/(1-target)))))
	}
	if bounded && p.pricing != nil && h.CostFloorTopUpInput && allowed {
		p.costFloor = cacheTokenCost(raw, p.pricing) * h.MinCostRatio
		p.costReported = cacheTokenCost(*u, p.pricing)
	}
}

func constrainColdUsage(u *ClaudeUsage, raw ClaudeUsage, estimated int) {
	total := raw.InputTokens
	if total <= 0 {
		total = estimated
	}
	hadCache := u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0
	u.CacheReadInputTokens = 0
	u.InputTokens = min(max(u.InputTokens, 1), total)
	if u.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = max(total-u.InputTokens, 0)
	} else if hadCache {
		u.InputTokens = total
	}
}

func (p *cacheEmulationPlan) selectedPlusProfile() (CacheShapingProfile, bool, bool) {
	policy := p.policyConfig().ToolPlus
	mode := "inherit"
	profile := policy.Shaping
	small := policy.SmallRequest.Enabled && p.profile.runtimeInputTokens <= policy.SmallRequest.MaxInputTokens
	if small {
		mode = policy.SmallRequest.Mode
		if mode == "custom" {
			profile = policy.SmallRequest.Profile
		}
	} else if p.nonStream {
		mode = policy.NonStream.Mode
		if mode == "custom" {
			profile = policy.NonStream.Profile
		}
	}
	return profile, mode == "raw", small && policy.SmallRequest.DisableCache
}

func cacheShapeField(v int, f CacheFieldShaping, seed uint64, capOnly bool) int {
	if v <= 0 {
		return 0
	}
	if !capOnly {
		return cacheRounded(float64(v) * f.Multiplier * math.Max(0, 1+f.JitterRatio*(2*cacheShapingUnit(seed)-1)))
	}
	cap := cachePositiveCap(f.MaxTokens)
	if f.MaxTokens > 0 && f.MaxJitterTokens > 0 {
		cap -= int(splitmix64(seed) % uint64(min(f.MaxJitterTokens, cap-1)+1))
	}
	return min(max(v, f.MinTokens), cap)
}

func (p *cacheEmulationPlan) shapePlus(u *ClaudeUsage, raw ClaudeUsage) {
	profile, unshaped, disable := p.selectedPlusProfile()
	seed := p.usageSeed()
	if disable {
		u.InputTokens += u.CacheReadInputTokens + u.CacheCreationInputTokens
		u.CacheReadInputTokens = 0
		u.CacheCreationInputTokens = 0
	}
	if unshaped {
		return
	}
	// Input/read/write amplification and frequency control are prepared once, independently of SSE previews.
	u.OutputTokens = cacheShapeField(u.OutputTokens, profile.Output, seed^0xbb67ae8584caa73b, false)
	overflow := 0
	for _, field := range []struct {
		v *int
		f CacheFieldShaping
		s uint64
	}{{&u.CacheReadInputTokens, profile.CacheRead, 0x3c6ef372fe94f82b}, {&u.CacheCreationInputTokens, profile.CacheCreation, 0xa54ff53a5f1d36f1}} {
		before := *field.v
		*field.v = cacheShapeField(before, field.f, seed^field.s^0x510e527fade682d1, true)
		overflow += max(before-*field.v, 0)
	}
	if profile.CapOverflowToInput {
		u.InputTokens += overflow
	}
	if u.InputTokens <= 0 && profile.Input.MinTokens > 0 {
		n := min(profile.Input.MinTokens, u.CacheReadInputTokens)
		u.CacheReadInputTokens -= n
		u.InputTokens += n
		n = min(profile.Input.MinTokens-u.InputTokens, u.CacheCreationInputTokens)
		u.CacheCreationInputTokens -= n
		u.InputTokens += n
	}
	u.InputTokens = cacheShapeField(u.InputTokens, profile.Input, seed^0x6a09e667f3bcc908^0x510e527fade682d1, true)
	if u.OutputTokens == 0 && raw.OutputTokens > 0 {
		u.OutputTokens = profile.Output.MinTokens
	}
	u.OutputTokens = cacheShapeField(u.OutputTokens, profile.Output, seed^0xbb67ae8584caa73b^0x510e527fade682d1, true)
	if p.pricing != nil && p.pricing.InputPricePerToken > 0 {
		gap := cacheTokenCost(raw, p.pricing)*profile.MinCostRatio - cacheTokenCost(*u, p.pricing)
		if gap > 0 {
			u.InputTokens = saturatingAddInt(u.InputTokens, cacheRounded(math.Ceil(gap/p.pricing.InputPricePerToken)))
		}
	}
}

func (p *cacheEmulationPlan) preparePlus() {
	if p.usage == nil {
		return
	}
	profile, raw, disable := p.selectedPlusProfile()
	u := p.usage
	seed := p.usageSeed()
	if disable {
		u.InputTokens += u.CacheReadInputTokens + u.CacheCreationInputTokens
		u.CacheReadInputTokens = 0
		u.CacheCreationInputTokens = 0
		p.profile.breakpoints = nil
		p.creationControlDecision = cacheCreationControlDecision{}
	}
	if raw {
		return
	}
	u.InputTokens = cacheShapeField(u.InputTokens, profile.Input, seed^0x6a09e667f3bcc908, false)
	u.CacheReadInputTokens = cacheShapeField(u.CacheReadInputTokens, profile.CacheRead, seed^0x3c6ef372fe94f82b, false)
	u.CacheCreationInputTokens = cacheShapeField(u.CacheCreationInputTokens, profile.CacheCreation, seed^0xa54ff53a5f1d36f1, false)
	if !disable {
		before := u.CacheCreationInputTokens
		p.creationControlDecision = applyCacheCreationControl(p.cacheKey, p.policyConfig().CreationControl, u)
		suppressed := max(before-u.CacheCreationInputTokens, 0)
		switch p.policyConfig().ToolPlus.SuppressedCreationTarget {
		case "cache_read":
			u.CacheReadInputTokens += suppressed
		case "drop":
		default:
			u.InputTokens += suppressed
		}
	}
}

func applyStableReportedInputRange(u *ClaudeUsage, p StableSegmentCachePolicy, seed uint64) {
	total := max(saturatingAddInt(u.InputTokens, saturatingAddInt(u.CacheReadInputTokens, u.CacheCreationInputTokens)), 0)
	if total <= 0 {
		return
	}
	minInput := min(max(p.ReportedInputMinTokens, 0), total)
	maxInput := total
	if p.ReportedInputMaxTokens > 0 {
		maxInput = min(max(p.ReportedInputMaxTokens, minInput), total)
	}
	current := min(max(u.InputTokens, 0), total)
	if current >= minInput && current <= maxInput {
		return
	}
	span := maxInput - minInput + 1
	state := seed
	for _, v := range []int{total, current, u.CacheCreationInputTokens, u.CacheReadInputTokens, u.CacheCreation5mTokens, u.CacheCreation1hTokens} {
		state = splitmix64(state ^ uint64(max(v, 0)))
	}
	target := minInput + int(splitmix64(state^0x6751d5c488a2c713)%uint64(max(span, 1)))
	if target > current {
		delta := target - current
		fromCreation := min(delta, max(u.CacheCreationInputTokens, 0))
		u.CacheCreationInputTokens -= fromCreation
		delta -= fromCreation
		u.CacheReadInputTokens = max(u.CacheReadInputTokens-delta, 0)
		u.InputTokens = target
	} else if target < current {
		delta := current - target
		u.InputTokens = target
		u.CacheCreationInputTokens = saturatingAddInt(u.CacheCreationInputTokens, delta)
		if u.CacheCreation1hTokens > 0 && u.CacheCreation5mTokens == 0 {
			u.CacheCreation1hTokens = saturatingAddInt(u.CacheCreation1hTokens, delta)
		} else {
			u.CacheCreation5mTokens = saturatingAddInt(u.CacheCreation5mTokens, delta)
		}
	}
	u.CacheCreation5mTokens, u.CacheCreation1hTokens = capCacheCreationBreakdown(u.CacheCreation5mTokens, u.CacheCreation1hTokens, u.CacheCreationInputTokens)
}

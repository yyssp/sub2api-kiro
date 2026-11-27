package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func advancedCachePlan(kind string) *cacheEmulationPlan {
	cfg := DefaultCacheStrategyConfig(kind)
	cfg.Usage.PreserveUpstreamCacheUsage = false
	cfg.Usage.OutputUpliftEnabled = boolPtr(false)
	cfg.ColdRequestGuard = false
	return &cacheEmulationPlan{profile: &cacheProfile{policy: cfg, runtimeInputTokens: 10000, reportedInputTokens: 10000}, usagePolicy: cfg.Usage, cacheKey: 902345, usage: &cacheEmulationUsage{InputTokens: 100, CacheReadInputTokens: 9000, CacheCreationInputTokens: 900}, pricing: &ModelPricing{InputPricePerToken: 3e-6, OutputPricePerToken: 15e-6, CacheReadPricePerToken: .3e-6, CacheCreationPricePerToken: 3.75e-6}}
}

func TestAdvancedCacheConfigDefaultsAndAliases(t *testing.T) {
	for _, kind := range []string{"stable_segment_cache", "prefix_tool_plus", "bounded_high_cache", "kiro_rs_tool_plus", "current_high_cache", "prefix_tool"} {
		cfg, err := ParseCacheStrategyConfig(map[string]any{"kind": kind})
		require.NoError(t, err, kind)
		require.Equal(t, normalizeCacheKind(kind), cfg.Kind)
		require.Equal(t, 1.0, cfg.ToolPlus.Shaping.Input.Multiplier)
		require.Equal(t, 0.04, cfg.HitShaping.RatioJitter)
	}
	c := DefaultCacheStrategyConfig(CacheStrategyKindBounded)
	c.HitShaping.TargetHitRatio = 1.0
	_, err := NormalizeCacheStrategyConfig(c)
	require.Error(t, err)
	c = DefaultCacheStrategyConfig(CacheStrategyKindToolPlus)
	c.ToolPlus.Shaping.Input.MinTokens = 100
	c.ToolPlus.Shaping.Input.MaxTokens = 10
	_, err = NormalizeCacheStrategyConfig(c)
	require.Error(t, err)
}

func TestAdvancedCacheHitShapingCostFloorAndFrequency(t *testing.T) {
	for _, aware := range []bool{false, true} {
		for _, allowed := range []bool{false, true} {
			p := advancedCachePlan(CacheStrategyKindPrefix)
			p.profile.policy.HitShaping.Enabled = true
			p.profile.policy.HitShaping.ReadAwareCostFloor = aware
			p.creationControlDecision = cacheCreationControlDecision{enabled: true, allowed: 0}
			if allowed {
				p.creationControlDecision.allowed = 900
			}
			u := ClaudeUsage{InputTokens: 10000, OutputTokens: 200}
			raw := u
			p.projectClaude(&u)
			require.GreaterOrEqual(t, cacheTokenCost(u, p.pricing), cacheTokenCost(raw, p.pricing)*(1-1e-9))
			if !allowed {
				require.Zero(t, u.CacheCreationInputTokens)
			}
			require.Positive(t, u.CacheReadInputTokens)
		}
	}
}

func TestAdvancedCacheBoundedCapsRemainFinal(t *testing.T) {
	p := advancedCachePlan(CacheStrategyKindBounded)
	c := &p.profile.policy
	c.HitShaping.Enabled = true
	c.HitShaping.MinCacheReadTokens = 10
	c.HitShaping.MinCacheCreationTokens = 10
	c.Usage.Input = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 50}
	c.Usage.FinalCacheReadMaxTokens = 1000
	c.Usage.FinalCacheCreationMaxTokens = 100
	p.usagePolicy = c.Usage
	u := ClaudeUsage{InputTokens: 100000, OutputTokens: 30}
	p.projectClaude(&u)
	require.LessOrEqual(t, u.InputTokens, 50)
	require.LessOrEqual(t, u.CacheReadInputTokens, 1000)
	require.LessOrEqual(t, u.CacheCreationInputTokens, 100)
	require.Greater(t, p.costFloor, p.costReported)
	p.profile.breakpoints = []cacheBreakpoint{}
	p.commit()
	require.Positive(t, globalCacheTracker.controlState(p.cacheKey).costDeficit)
}

func TestAdvancedCachePlusProfilesAndFrequency(t *testing.T) {
	resetCacheTracker()
	p := advancedCachePlan(CacheStrategyKindToolPlus)
	p.profile.policy.ToolPlus.Shaping.CacheCreation.Multiplier = 3
	p.profile.policy.ToolPlus.Shaping.Output.Multiplier = 2
	p.profile.policy.CreationControl = CacheCreationControl{Enabled: true, MaxCreationTokensPerEvent: 1000}
	p.preparePlus()
	require.Equal(t, 2700, p.creationControlDecision.attempted)
	require.LessOrEqual(t, p.usage.CacheCreationInputTokens, 1000)
	u := ClaudeUsage{InputTokens: 10000, OutputTokens: 80}
	p.projectClaude(&u)
	require.Equal(t, 160, u.OutputTokens)
	p.nonStream = true
	p.profile.policy.ToolPlus.NonStream.Mode = "raw"
	_, raw, _ := p.selectedPlusProfile()
	require.True(t, raw)
	p.profile.policy.ToolPlus.SmallRequest.Enabled = true
	p.profile.policy.ToolPlus.SmallRequest.MaxInputTokens = 20000
	p.profile.policy.ToolPlus.SmallRequest.Mode = "custom"
	p.profile.policy.ToolPlus.SmallRequest.Profile.Output.Multiplier = 4
	profile, raw, _ := p.selectedPlusProfile()
	require.False(t, raw)
	require.Equal(t, 4.0, profile.Output.Multiplier)
}

func TestAdvancedCacheTTLStateSeparateFromDisplay(t *testing.T) {
	resetCacheTracker()
	p := advancedCachePlan(CacheStrategyKindBounded)
	p.profile.blocks = []cacheBlock{{prefixFingerprint: [32]byte{1}, cumulativeTokens: 1000}}
	p.profile.breakpoints = []cacheBreakpoint{{blockIndex: 0, ttl: time.Hour}}
	p.profile.minCacheable = 0
	p.profile.policy.ForcedTTLTier = CacheTTLTier1h
	p.commit()
	entry := globalCacheTracker.entries[p.cacheKey][[32]byte{1}]
	require.Equal(t, 5*time.Minute, entry.ttl)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), entry.expiresAt, time.Second)
	body := []byte(`{"cache_control":{"type":"ephemeral"},"tools":[{"name":"x","cache_control":{"type":"ephemeral","ttl":"1h"},"input_schema":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}],"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}],"metadata":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}`)
	got := normalizeUpstreamCacheTTL(body)
	require.Contains(t, string(got), `"ttl":"5m"`)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(got, &parsed))
	require.Equal(t, "1h", parsed["metadata"].(map[string]any)["cache_control"].(map[string]any)["ttl"])
	require.Equal(t, "1h", parsed["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)["cache_control"].(map[string]any)["ttl"])
	require.Contains(t, string(body), `"ttl":"1h"`)
}

func TestAdvancedCacheStableSegmentPartialWriteAndSuccess(t *testing.T) {
	resetCacheTracker()
	id := int64(98137)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindStableSegment)
	cfg.StableSegment.MinTotalInputTokens = 1
	cfg.StableSegment.MinSegmentTokens = 1
	cfg.MaxNewCreationTokensPerRequest = 100
	cfg.MinCacheableTokens = 0
	cfg.AllowDerivedSession = true
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: id, Name: "stable", Enabled: true, Revision: 1, Config: cfg})
	t.Cleanup(func() { GlobalCacheStrategyRegistry().Delete(id) })
	group := &Group{ID: 98137, CacheStrategyID: &id}
	account := &Account{ID: 98137, Platform: PlatformAnthropic}
	system := make([]map[string]any, 8)
	for i := range system {
		system[i] = map[string]any{"type": "text", "text": strings.Repeat("stable system ", 500)}
	}
	bodyBytes, err := json.Marshal(map[string]any{"stream": true, "model": "claude-sonnet-4-6", "metadata": map[string]any{"session_id": "stable-segment-session"}, "system": system, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	require.NoError(t, err)
	body := bodyBytes
	svc := &GatewayService{}
	cfg.MaxNewCreationTokensPerRequest = 2000
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: id, Name: "stable", Enabled: true, Revision: 1, Config: cfg})
	first := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 10000)
	require.NotNil(t, first)
	require.NotNil(t, first.result())
	require.Zero(t, first.result().CacheReadInputTokens)
	require.Positive(t, first.result().CacheCreationInputTokens)
	again := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 10000)
	require.Zero(t, again.result().CacheReadInputTokens, "uncommitted request must stay cold")
	first.commit()
	hit := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 10000)
	require.Equal(t, first.profile.committedCacheTokens, hit.result().CacheReadInputTokens, "partial prefix write must not become a full hit")
}

func TestAdvancedCacheEligibilityDoesNotCommitOrRewrite(t *testing.T) {
	p := advancedCachePlan(CacheStrategyKindBounded)
	p.projectionIneligible = true
	raw := ClaudeUsage{InputTokens: 20, OutputTokens: 3}
	got := raw
	p.projectClaude(&got)
	require.Equal(t, raw.billingUsageTokens(), got.billingUsageTokens())
	p.commit()
	require.False(t, p.committed.Load(), "ineligible request must not commit")
}

func TestAdvancedCacheReadAwareFloorReducesContext(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		var contexts [2]int
		for i, aware := range []bool{false, true} {
			p := advancedCachePlan(CacheStrategyKindPrefix)
			p.profile.model = "claude-sonnet-4-6"
			p.profile.policy.MaxSimulatedInputTokens = 0
			p.profile.runtimeInputTokens = 28388
			p.profile.policy.HitShaping.Enabled = true
			p.profile.policy.HitShaping.ReadAwareCostFloor = aware
			p.profile.policy.HitShaping.RatioJitter = 0
			p.profile.policy.Usage.Input.Mode = CacheUsageFieldPreserve
			p.usagePolicy = p.profile.policy.Usage
			p.profile.reportedInputTokens = 58180
			p.usage = &cacheEmulationUsage{InputTokens: 4310, CacheReadInputTokens: 53870}
			p.creationControlDecision = cacheCreationControlDecision{enabled: true}
			if allowed {
				p.creationControlDecision.allowed = 1000
			}
			raw := ClaudeUsage{InputTokens: 28388, OutputTokens: 27}
			u := raw
			p.projectClaude(&u)
			total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
			contexts[i] = total
			require.GreaterOrEqual(t, float64(u.CacheReadInputTokens)/float64(total), .9-1e-6)
			require.GreaterOrEqual(t, cacheTokenCost(u, p.pricing), cacheTokenCost(raw, p.pricing)*(1-1e-9))
			if aware {
				require.LessOrEqual(t, cacheTokenCost(u, p.pricing), cacheTokenCost(raw, p.pricing)*1.005)
			}
		}
		require.Less(t, float64(contexts[1]), float64(contexts[0])*.75)
	}
}

func TestAdvancedCacheBoundedSweep(t *testing.T) {
	for seed := 0; seed < 200; seed++ {
		p := advancedCachePlan(CacheStrategyKindBounded)
		p.cacheKey = uint64(seed + 1)
		c := &p.profile.policy
		c.HitShaping.Enabled = true
		c.HitShaping.TargetHitRatio = float64(seed%99) / 100
		c.HitShaping.MinCacheReadTokens = 10
		c.HitShaping.MinCacheCreationTokens = 10
		c.Usage.Input = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 50}
		c.Usage.FinalCacheReadMaxTokens = 1000
		c.Usage.FinalCacheCreationMaxTokens = 100
		p.usagePolicy = c.Usage
		p.creationControlDecision = cacheCreationControlDecision{enabled: true, allowed: seed % 3}
		u := ClaudeUsage{InputTokens: 1000 + seed*1000, OutputTokens: seed}
		p.projectClaude(&u)
		require.GreaterOrEqual(t, u.InputTokens, 1)
		require.LessOrEqual(t, u.InputTokens, 50)
		require.LessOrEqual(t, u.CacheReadInputTokens, 1000)
		require.LessOrEqual(t, u.CacheCreationInputTokens, 100)
		if seed%3 == 0 {
			require.Zero(t, u.CacheCreationInputTokens)
		}
		require.Equal(t, u.CacheCreationInputTokens, u.CacheCreation5mTokens+u.CacheCreation1hTokens)
	}
}

func TestAdvancedCacheStrictEligibilityUsesPrefixNotTail(t *testing.T) {
	p := &cacheProfile{model: "claude-sonnet-4-6", pendingBlocks: []pendingCacheBlock{{tokens: 20, breakpointTTL: durationPointer(cacheDefaultTTL)}, {tokens: 10000}}}
	require.False(t, requestQualifiesForPromptCache(p))
	p.pendingBlocks[0].tokens = 1500
	require.True(t, requestQualifiesForPromptCache(p))
	p.model = "claude-haiku-4-5"
	require.False(t, requestQualifiesForPromptCache(p))
	p.rawBody = []byte(`{"cache_control":{"type":"ephemeral"}}`)
	require.True(t, requestQualifiesForPromptCache(p))
}

func durationPointer(v time.Duration) *time.Duration { return &v }

func TestAdvancedStableReportedInputRangeRedistributesBuckets(t *testing.T) {
	stable := DefaultStableSegmentCachePolicy()
	usage := ClaudeUsage{InputTokens: 100, CacheReadInputTokens: 7000, CacheCreationInputTokens: 900, CacheCreation5mTokens: 900}
	applyStableReportedInputRange(&usage, stable, 11)
	require.GreaterOrEqual(t, usage.InputTokens, 32)
	require.LessOrEqual(t, usage.InputTokens, 256)
	require.Equal(t, usage.CacheCreationInputTokens, usage.CacheCreation5mTokens+usage.CacheCreation1hTokens)
	total := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	require.Equal(t, 8000, total)
}

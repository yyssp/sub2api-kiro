package service

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func multiplierTestPlan(t *testing.T, protocol string) *cacheEmulationPlan {
	t.Helper()
	cfg, err := ParseCacheStrategyConfig(map[string]any{"kind": CacheStrategyKindUsageMultiplier, "protocol": protocol})
	require.NoError(t, err)
	return usageMultiplierPlan(cfg, []byte(`{"stream":true}`), "claude-sonnet-4-6", protocol)
}

func TestCacheUsageMultiplierThresholds(t *testing.T) {
	for _, multiplier := range []float64{0, .5, 1, 1.5, 2, 1000} {
		for _, value := range []int{0, 99, 100, 101, 10000} {
			got := multiplyCacheUsageField(value, CacheUsageMultiplierRule{ThresholdTokens: 100, Multiplier: multiplier})
			expected := value
			if value > 100 {
				expected = cacheRounded(float64(value) * multiplier)
			}
			require.Equal(t, expected, got)
		}
	}
}

func TestCacheUsageMultiplierIndependentRawBuckets(t *testing.T) {
	p := multiplierTestPlan(t, PlatformAnthropic)
	p.profile.policy.UsageMultiplier = CacheUsageMultiplierPolicy{Input: CacheUsageMultiplierRule{ThresholdTokens: 100, Multiplier: .5}, Output: CacheUsageMultiplierRule{ThresholdTokens: 1000, Multiplier: 2}, CacheRead: CacheUsageMultiplierRule{Multiplier: 1.5}, CacheCreation: CacheUsageMultiplierRule{Multiplier: 0.25}}
	raw := ClaudeUsage{InputTokens: 2000, OutputTokens: 1001, CacheReadInputTokens: 4000, CacheCreationInputTokens: 1000, CacheCreation5mTokens: 600, CacheCreation1hTokens: 400}
	u := raw
	p.projectClaude(&u)
	require.Equal(t, 1000, u.InputTokens)
	require.Equal(t, 2002, u.OutputTokens)
	require.Equal(t, 6000, u.CacheReadInputTokens)
	require.Equal(t, 250, u.CacheCreationInputTokens)
	require.Equal(t, 150, u.CacheCreation5mTokens)
	require.Equal(t, 100, u.CacheCreation1hTokens)
	// Old timing/sampling/cost controls must not interfere with direct multipliers.
	p.profile.policy.ColdRequestGuard = true
	p.profile.policy.HitShaping.Enabled = true
	p.profile.policy.MaxSimulatedInputTokens = 10
	p.profile.policy.MaxNewCreationTokensPerRequest = 1
	p.profile.policy.CreationControl = CacheCreationControl{Enabled: true, MaxCreationTokensPerEvent: 1}
	p.profile.policy.Usage.SkipNonStreamUsageProjection = true
	p.nonStream = true
	v := raw
	p.projectClaude(&v)
	require.Equal(t, u, v)
}

func TestCacheUsageMultiplierOpenAITotalAndNoInventedCache(t *testing.T) {
	p := multiplierTestPlan(t, PlatformAnthropic)
	p.profile.policy.UsageMultiplier.Input.Multiplier = .5
	p.profile.policy.UsageMultiplier.Output.Multiplier = 2
	p.profile.policy.UsageMultiplier.CacheRead.Multiplier = 2
	p.profile.policy.UsageMultiplier.CacheCreation.Multiplier = .5
	u := OpenAIUsage{InputTokens: 10000, CacheReadInputTokens: 4000, CacheCreationInputTokens: 2000, OutputTokens: 100}
	p.projectOpenAI(&u)
	require.Equal(t, 11000, u.InputTokens)
	require.Equal(t, 8000, u.CacheReadInputTokens)
	require.Equal(t, 1000, u.CacheCreationInputTokens)
	require.Equal(t, 200, u.OutputTokens)
	v := ClaudeUsage{InputTokens: 1000, OutputTokens: 100}
	p.projectClaude(&v)
	require.Zero(t, v.CacheReadInputTokens)
	require.Zero(t, v.CacheCreationInputTokens)
}

func TestCacheUsageMultiplierGeminiExplicitAndLimits(t *testing.T) {
	p := multiplierTestPlan(t, PlatformGemini)
	p.profile.model = "gemini-2.5-pro"
	p.profile.rawBody = []byte(`{"cachedContent":"cachedContents/native","generationConfig":{"maxOutputTokens":250}}`)
	p.profile.policy.UsageMultiplier.Input.Multiplier = .5
	p.profile.policy.UsageMultiplier.Output.Multiplier = 2
	p.profile.policy.UsageMultiplier.CacheRead.Multiplier = 2
	u := projectGeminiCacheUsage(p, ClaudeUsage{InputTokens: 2000, CacheReadInputTokens: 4000, OutputTokens: 200})
	require.Equal(t, 1000, u.InputTokens)
	require.Equal(t, 8000, u.CacheReadInputTokens)
	require.Equal(t, 250, u.OutputTokens)
	require.Zero(t, u.CacheCreationInputTokens)
	p.profile.policy.Limits.MaxInputTokens = 500
	v := projectGeminiCacheUsage(p, ClaudeUsage{InputTokens: 2000, CacheReadInputTokens: 4000, OutputTokens: 200})
	require.Equal(t, 500, v.InputTokens+v.CacheReadInputTokens)
}

func TestCacheUsageMultiplierNoTrackerOrSession(t *testing.T) {
	resetCacheTracker()
	t.Cleanup(resetCacheTracker)
	id := int64(914882)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindUsageMultiplier)
	cfg.AllowDerivedSession = false
	cfg.CacheHistory = false
	cfg.CacheCurrentUserStablePrefix = false
	cfg.CacheSystem = false
	cfg.CacheTools = false
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	cfg.UsageMultiplier.Output = CacheUsageMultiplierRule{ThresholdTokens: 100, Multiplier: 2}
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: id, Enabled: true, Config: cfg})
	t.Cleanup(func() { GlobalCacheStrategyRegistry().Delete(id) })
	p := cachePlanForProtocol(context.Background(), nil, &Group{ID: id, CacheStrategyID: &id}, []byte(`{}`), "claude-sonnet-4-6", "anthropic_messages", 0)
	require.NotNil(t, p)
	require.Zero(t, p.cacheKey)
	require.Nil(t, p.result())
	u := ClaudeUsage{InputTokens: 1000, OutputTokens: 101}
	p.projectClaude(&u)
	require.Equal(t, 202, u.OutputTokens)
	p.commit()
	require.Empty(t, globalCacheTracker.entries)
	require.Empty(t, globalCacheTracker.controls)
	require.Nil(t, cachePlanForProtocol(context.Background(), nil, &Group{ID: id, CacheStrategyID: &id}, []byte(`{}`), "claude-sonnet-4-6", "gemini_generate_content", 0))
}

func TestCacheUsageMultiplierValidation(t *testing.T) {
	for _, rule := range []CacheUsageMultiplierRule{{Multiplier: -1}, {Multiplier: 1001}, {Multiplier: math.Inf(1)}, {Multiplier: math.NaN()}, {ThresholdTokens: -1, Multiplier: 1}, {ThresholdTokens: 4000001, Multiplier: 1}} {
		c := DefaultCacheStrategyConfig(CacheStrategyKindUsageMultiplier)
		c.UsageMultiplier.Output = rule
		_, err := NormalizeCacheStrategyConfig(c)
		require.Error(t, err)
	}
}

func TestCacheUsageMultiplierLegacyPrepareEntryPoints(t *testing.T) {
	id := int64(914883)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindUsageMultiplier)
	cfg.UsageMultiplier.Output.Multiplier = 2
	cfg.Usage.StrictCacheEligibilityNonStream = true
	cfg.Usage.SkipNonStreamUsageProjection = true
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: id, Enabled: true, Config: cfg})
	t.Cleanup(func() { GlobalCacheStrategyRegistry().Delete(id) })
	group := &Group{ID: id, CacheStrategyID: &id}
	svc := &GatewayService{}
	for _, prepare := range []func(context.Context, *Account, *Group, []byte, string, int) *cacheEmulationPlan{svc.prepareCacheEmulationUsage, svc.prepareResponsesCacheUsage, svc.prepareChatCompletionsCacheUsage} {
		p := prepare(context.Background(), nil, group, []byte(`{}`), "claude-sonnet-4-6", 0)
		require.NotNil(t, p)
		u := ClaudeUsage{OutputTokens: 101}
		p.projectClaude(&u)
		require.Equal(t, 202, u.OutputTokens)
		require.Zero(t, p.cacheKey)
	}
}

func TestCacheUsageMultiplierZeroAndIdentity(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		p := multiplierTestPlan(t, protocol)
		raw := ClaudeUsage{InputTokens: 1000, OutputTokens: 80, CacheReadInputTokens: 500, CacheCreationInputTokens: 200, CacheCreation5mTokens: 200}
		if protocol == PlatformGemini {
			raw.CacheCreationInputTokens, raw.CacheCreation5mTokens = 0, 0
		}
		u := raw
		p.projectUsageMultiplier(&u)
		require.Equal(t, raw, u)
		p.profile.policy.UsageMultiplier = CacheUsageMultiplierPolicy{}
		u = raw
		p.projectUsageMultiplier(&u)
		require.Equal(t, 1, u.InputTokens)
		require.Equal(t, 1, u.OutputTokens)
		require.Zero(t, u.CacheReadInputTokens)
		require.Zero(t, u.CacheCreationInputTokens)
		require.Zero(t, u.CacheCreation5mTokens)
	}
}

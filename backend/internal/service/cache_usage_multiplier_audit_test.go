//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCacheMultiplierAuditSSEWriteZeroClearsDetails(t *testing.T) {
	p := multiplierTestPlan(t, PlatformAnthropic)
	p.profile.policy.UsageMultiplier.CacheCreation.Multiplier = 0
	raw := ClaudeUsage{InputTokens: 1000, OutputTokens: 100, CacheCreationInputTokens: 1000, CacheCreation5mTokens: 600, CacheCreation1hTokens: 400}
	event := map[string]any{"type": "message_delta", "usage": map[string]any{"cache_creation_input_tokens": 1000, "cache_creation": map[string]any{"ephemeral_5m_input_tokens": 600, "ephemeral_1h_input_tokens": 400}}}
	require.True(t, rewriteClaudeSSEUsageEvent(event, &raw, p))
	usage := event["usage"].(map[string]any)
	details := usage["cache_creation"].(map[string]any)
	require.Equal(t, 0, usage["cache_creation_input_tokens"])
	require.Equal(t, 0, details["ephemeral_5m_input_tokens"])
	require.Equal(t, 0, details["ephemeral_1h_input_tokens"])
}

func TestCacheMultiplierAuditWriteBreakdownPreservesRatioAtLimit(t *testing.T) {
	p := multiplierTestPlan(t, PlatformAnthropic)
	p.profile.policy.UsageMultiplier.CacheCreation.Multiplier = 2
	p.profile.policy.Limits.MaxInputTokens = 600
	u := ClaudeUsage{InputTokens: 0, CacheCreationInputTokens: 1000, CacheCreation5mTokens: 600, CacheCreation1hTokens: 400}
	p.projectClaude(&u)
	require.Equal(t, 600, u.CacheCreationInputTokens)
	require.Equal(t, 360, u.CacheCreation5mTokens)
	require.Equal(t, 240, u.CacheCreation1hTokens)
}

func TestCacheMultiplierAuditOpenAIWriteDetailsAndCost(t *testing.T) {
	for _, factor := range []float64{0, .5, 2} {
		t.Run(fmt.Sprint(factor), func(t *testing.T) {
			p := multiplierTestPlan(t, PlatformAnthropic)
			p.profile.policy.UsageMultiplier.CacheCreation.Multiplier = factor
			u := OpenAIUsage{InputTokens: 3000, OutputTokens: 100, CacheCreationInputTokens: 1000, CacheCreation5mTokens: 600, CacheCreation1hTokens: 400}
			p.projectOpenAI(&u)
			require.Equal(t, int(1000*factor), u.CacheCreationInputTokens)
			require.Equal(t, int(600*factor), u.CacheCreation5mTokens)
			require.Equal(t, int(400*factor), u.CacheCreation1hTokens)
			cost := (&BillingService{}).computeCacheCreationCost(&ModelPricing{SupportsCacheBreakdown: true, CacheCreation5mPrice: 2, CacheCreation1hPrice: 4}, UsageTokens{CacheCreationTokens: u.CacheCreationInputTokens, CacheCreation5mTokens: u.CacheCreation5mTokens, CacheCreation1hTokens: u.CacheCreation1hTokens}, 2, 1)
			require.Equal(t, 2800*factor, cost)
		})
	}
}

func TestCacheMultiplierAuditOpenAINativeTTLDetails(t *testing.T) {
	for _, factor := range []float64{0, .5, 2} {
		p := multiplierTestPlan(t, PlatformAnthropic)
		p.profile.policy.UsageMultiplier.CacheCreation.Multiplier = factor
		u := OpenAIUsage{InputTokens: 3000, CacheCreationInputTokens: 1000, CacheCreation30mInputTokens: 600, CacheCreation12hInputTokens: 400}
		p.projectOpenAI(&u)
		require.Equal(t, int(600*factor), u.CacheCreation30mInputTokens)
		require.Equal(t, int(400*factor), u.CacheCreation12hInputTokens)
	}
}

func TestCacheMultiplierAuditPropertySweep(t *testing.T) {
	rng := rand.New(rand.NewSource(726))
	for i := 0; i < 1200; i++ {
		protocol := PlatformAnthropic
		if i%2 == 0 {
			protocol = PlatformGemini
		}
		p := multiplierTestPlan(t, protocol)
		p.profile.model = "unknown-test-model"
		p.profile.policy.Limits = CacheStrategyLimits{MaxInputTokens: 12000, MaxOutputTokens: 1000}
		fields := []*CacheUsageMultiplierRule{&p.profile.policy.UsageMultiplier.Input, &p.profile.policy.UsageMultiplier.Output, &p.profile.policy.UsageMultiplier.CacheRead, &p.profile.policy.UsageMultiplier.CacheCreation}
		for _, field := range fields {
			field.ThresholdTokens = rng.Intn(2000)
			field.Multiplier = []float64{0, .01, .5, 1, 1.5, 2, 1000}[rng.Intn(7)]
		}
		raw := ClaudeUsage{InputTokens: rng.Intn(100000), OutputTokens: rng.Intn(5000), CacheReadInputTokens: rng.Intn(100000), CacheCreationInputTokens: rng.Intn(100000)}
		raw.CacheCreation5mTokens = raw.CacheCreationInputTokens * 3 / 5
		raw.CacheCreation1hTokens = raw.CacheCreationInputTokens - raw.CacheCreation5mTokens
		if protocol == PlatformGemini {
			raw.CacheCreationInputTokens, raw.CacheCreation5mTokens, raw.CacheCreation1hTokens = 0, 0, 0
		}
		u := raw
		p.projectUsageMultiplier(&u)
		require.LessOrEqual(t, u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens, 12000)
		require.LessOrEqual(t, u.OutputTokens, 1000)
		require.GreaterOrEqual(t, u.InputTokens, 0)
		require.GreaterOrEqual(t, u.CacheReadInputTokens, 0)
		require.GreaterOrEqual(t, u.CacheCreationInputTokens, 0)
		require.Equal(t, u.CacheCreationInputTokens, u.CacheCreation5mTokens+u.CacheCreation1hTokens)
		before, _ := json.Marshal(raw)
		again := raw
		p.projectUsageMultiplier(&again)
		after, _ := json.Marshal(raw)
		require.Equal(t, before, after)
		require.Equal(t, u, again)
	}
}

func TestCacheMultiplierAuditOpenAIImageDetailsRespectTotals(t *testing.T) {
	for _, factor := range []float64{0, .5, 2} {
		p := multiplierTestPlan(t, PlatformAnthropic)
		p.profile.policy.UsageMultiplier.Input.Multiplier = factor
		p.profile.policy.UsageMultiplier.CacheRead.Multiplier = factor
		p.profile.policy.UsageMultiplier.Output.Multiplier = factor
		u := OpenAIUsage{InputTokens: 2000, CacheReadInputTokens: 1000, ImageInputTokens: 400, ImageCacheReadTokens: 300, OutputTokens: 1000, ImageOutputTokens: 200}
		p.projectOpenAI(&u)
		require.Equal(t, int(400*factor), u.ImageInputTokens)
		require.Equal(t, int(100*factor), u.billingUsageTokens().ImageInputTokens)
		require.Equal(t, int(300*factor), u.ImageCacheReadTokens)
		require.Equal(t, int(200*factor), u.ImageOutputTokens)
	}
}

func TestCacheMultiplierAuditSplitImageMultipliers(t *testing.T) {
	p := multiplierTestPlan(t, PlatformAnthropic)
	p.profile.policy.UsageMultiplier.Input.Multiplier = .5
	p.profile.policy.UsageMultiplier.CacheRead.Multiplier = 2
	u := OpenAIUsage{InputTokens: 2000, CacheReadInputTokens: 1000, ImageInputTokens: 400, ImageCacheReadTokens: 300}
	p.projectOpenAI(&u)
	require.Equal(t, 650, u.ImageInputTokens)
	require.Equal(t, 600, u.ImageCacheReadTokens)
	require.Equal(t, 50, u.billingUsageTokens().ImageInputTokens)
}

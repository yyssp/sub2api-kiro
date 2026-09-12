package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// isolatedUsagePolicy disables the unrelated final guards so the tests below
// exercise one usage field at a time. The production defaults are covered by
// the integration and large-window probes.
func isolatedUsagePolicy() CacheUsagePolicy {
	return CacheUsagePolicy{
		Enabled:                    true,
		PreserveUpstreamCacheUsage: true,
		Input:                      CacheUsageFieldPolicy{Mode: CacheUsageFieldRaw, NormalMaxMultiplier: 1.1},
		Output:                     CacheUsageFieldPolicy{Mode: CacheUsageFieldRaw, NormalMaxMultiplier: 1.1},
		CacheRead:                  CacheUsageFieldPolicy{Mode: CacheUsageFieldPreserve, NormalMaxMultiplier: 1.1},
		CacheCreation:              CacheUsageFieldPolicy{Mode: CacheUsageFieldPreserve, NormalMaxMultiplier: 1.1},
		OutputUpliftEnabled:        boolPtr(false),
		FinalOutputGuardEnabled:    boolPtr(false),
	}
}

func TestUsageProjectionInputModesAndDeltaRouting(t *testing.T) {
	t.Run("raw and preserve keep the computed uncached input", func(t *testing.T) {
		for _, mode := range []CacheUsageFieldMode{CacheUsageFieldRaw, CacheUsageFieldPreserve} {
			policy := isolatedUsagePolicy()
			policy.Input.Mode = mode
			usage := &ClaudeUsage{InputTokens: 120000, OutputTokens: 7}

			projectClaudeUsage(usage, nil, policy, 11)

			require.Equal(t, 120000, usage.InputTokens, "mode=%s", mode)
		}
	})

	t.Run("sample max caps without moving delta", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.Input = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleMax, MaxTokens: 50000,
			NormalMaxMultiplier: 1.1,
		}
		usage := &ClaudeUsage{InputTokens: 120000, OutputTokens: 7}

		projectClaudeUsage(usage, nil, policy, 11)

		require.Greater(t, usage.InputTokens, 0)
		require.LessOrEqual(t, usage.InputTokens, 50000)
		require.Zero(t, usage.CacheReadInputTokens)
		require.Zero(t, usage.CacheCreationInputTokens)
	})

	t.Run("sample target stays in target band", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.Input = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleTarget, TargetTokens: 50000,
			NormalMaxMultiplier: 1.2,
		}
		seen := map[int]struct{}{}
		for seed := uint64(1); seed <= 40; seed++ {
			usage := &ClaudeUsage{InputTokens: 120000, OutputTokens: 7}
			projectClaudeUsage(usage, nil, policy, seed)

			require.GreaterOrEqual(t, usage.InputTokens, 42500)
			require.LessOrEqual(t, usage.InputTokens, 60000)
			seen[usage.InputTokens] = struct{}{}
		}
		require.Greater(t, len(seen), 5, "sample_target must vary by request seed")
	})

	t.Run("move delta to cache read only when cache evidence exists", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.Input = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleMax, MaxTokens: 50000,
			MoveDeltaToCacheRead: true, NormalMaxMultiplier: 1.1,
		}

		withEvidence := &ClaudeUsage{
			InputTokens: 120000, OutputTokens: 7, CacheReadInputTokens: 5000,
		}
		projectClaudeUsage(withEvidence, nil, policy, 11)
		require.LessOrEqual(t, withEvidence.InputTokens, 50000)
		require.Greater(t, withEvidence.CacheReadInputTokens, 5000)
		require.Equal(t, 125000, withEvidence.InputTokens+withEvidence.CacheReadInputTokens)

		withoutEvidence := &ClaudeUsage{InputTokens: 120000, OutputTokens: 7}
		projectClaudeUsage(withoutEvidence, nil, policy, 11)
		// There is no local/upstream cache bucket to receive the difference on a
		// cold request, so the authoritative uncached input remains unchanged.
		require.Equal(t, 120000, withoutEvidence.InputTokens)
		require.Zero(t, withoutEvidence.CacheReadInputTokens)
		require.Zero(t, withoutEvidence.CacheCreationInputTokens)
	})
}

func TestUsageProjectionOutputModesUpliftAndFinalGuard(t *testing.T) {
	t.Run("raw and preserve keep output", func(t *testing.T) {
		for _, mode := range []CacheUsageFieldMode{CacheUsageFieldRaw, CacheUsageFieldPreserve} {
			policy := isolatedUsagePolicy()
			policy.Output.Mode = mode
			usage := &ClaudeUsage{InputTokens: 1, OutputTokens: 2400}

			projectClaudeUsage(usage, nil, policy, 11)

			require.Equal(t, 2400, usage.OutputTokens, "mode=%s", mode)
		}
	})

	t.Run("sample max caps output and remains variable", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.Output = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleMax, MaxTokens: 1000,
			NormalMaxMultiplier: 1.1,
		}
		seen := map[int]struct{}{}
		for seed := uint64(1); seed <= 40; seed++ {
			usage := &ClaudeUsage{InputTokens: 1, OutputTokens: 2400}
			projectClaudeUsage(usage, nil, policy, seed)

			require.Greater(t, usage.OutputTokens, 0)
			require.LessOrEqual(t, usage.OutputTokens, 1000)
			seen[usage.OutputTokens] = struct{}{}
		}
		require.Greater(t, len(seen), 5)
	})

	t.Run("sample target stays in target band", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.Output = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleTarget, TargetTokens: 1000,
			NormalMaxMultiplier: 1.2,
		}
		usage := &ClaudeUsage{InputTokens: 1, OutputTokens: 2400}

		projectClaudeUsage(usage, nil, policy, 11)

		require.GreaterOrEqual(t, usage.OutputTokens, 850)
		require.LessOrEqual(t, usage.OutputTokens, 1200)
	})

	t.Run("uplift applies above threshold and not at threshold", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.OutputUpliftEnabled = boolPtr(true)
		policy.OutputUpliftMinTokens = 1000
		policy.OutputUpliftPercent = 50
		policy.FinalOutputGuardEnabled = boolPtr(true)

		above := &ClaudeUsage{InputTokens: 1, OutputTokens: 2000}
		projectClaudeUsage(above, nil, policy, 11)
		require.Equal(t, 3000, above.OutputTokens)

		atThreshold := &ClaudeUsage{InputTokens: 1, OutputTokens: 1000}
		projectClaudeUsage(atThreshold, nil, policy, 11)
		require.Equal(t, 1000, atThreshold.OutputTokens)
	})

	t.Run("final output cap runs after uplift and jitters", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.OutputUpliftEnabled = boolPtr(true)
		policy.OutputUpliftMinTokens = 1000
		policy.OutputUpliftPercent = 50
		policy.FinalOutputGuardEnabled = boolPtr(true)
		policy.FinalOutputMaxTokens = 800
		policy.FinalOutputJitterMinTokens = 100
		policy.FinalOutputJitterMaxTokens = 200

		usage := &ClaudeUsage{InputTokens: 1, OutputTokens: 10000}
		projectClaudeUsage(usage, nil, policy, 11)

		require.GreaterOrEqual(t, usage.OutputTokens, 600)
		require.LessOrEqual(t, usage.OutputTokens, 700)
	})

	t.Run("disabled final output guard disables uplift and cap together", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.OutputUpliftEnabled = boolPtr(true)
		policy.OutputUpliftMinTokens = 1000
		policy.OutputUpliftPercent = 50
		policy.FinalOutputGuardEnabled = boolPtr(false)
		policy.FinalOutputMaxTokens = 800

		usage := &ClaudeUsage{InputTokens: 1, OutputTokens: 10000}
		projectClaudeUsage(usage, nil, policy, 11)

		require.Equal(t, 10000, usage.OutputTokens)
	})
}

func TestOpenAIUsageProjectionInputAndOutputModes(t *testing.T) {
	t.Run("sample max with cache evidence routes delta to read", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.Input = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleMax, MaxTokens: 50000,
			MoveDeltaToCacheRead: true, NormalMaxMultiplier: 1.1,
		}
		policy.Output = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleTarget, TargetTokens: 1000,
			NormalMaxMultiplier: 1.2,
		}
		usage := &OpenAIUsage{
			InputTokens: 120000, OutputTokens: 2400, CacheReadInputTokens: 5000,
		}

		projectOpenAIUsage(usage, nil, policy, 11)

		require.Equal(t, 120000, usage.InputTokens)
		require.Greater(t, usage.CacheReadInputTokens, 5000)
		require.GreaterOrEqual(t, usage.OutputTokens, 850)
		require.LessOrEqual(t, usage.OutputTokens, 1200)
	})

	t.Run("sample max without cache evidence caps total input", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.Input = CacheUsageFieldPolicy{
			Mode: CacheUsageFieldSampleMax, MaxTokens: 50000,
			NormalMaxMultiplier: 1.1,
		}
		usage := &OpenAIUsage{InputTokens: 120000, OutputTokens: 7}

		projectOpenAIUsage(usage, nil, policy, 11)

		require.Greater(t, usage.InputTokens, 0)
		require.LessOrEqual(t, usage.InputTokens, 50000)
		require.Zero(t, usage.CacheReadInputTokens)
		require.Zero(t, usage.CacheCreationInputTokens)
	})

	t.Run("final output guard preserves the same cap semantics", func(t *testing.T) {
		policy := isolatedUsagePolicy()
		policy.OutputUpliftEnabled = boolPtr(true)
		policy.OutputUpliftMinTokens = 1000
		policy.OutputUpliftPercent = 50
		policy.FinalOutputGuardEnabled = boolPtr(true)
		policy.FinalOutputMaxTokens = 800
		policy.FinalOutputJitterMinTokens = 100
		policy.FinalOutputJitterMaxTokens = 200
		usage := &OpenAIUsage{InputTokens: 1, OutputTokens: 10000}

		projectOpenAIUsage(usage, nil, policy, 11)

		require.GreaterOrEqual(t, usage.OutputTokens, 600)
		require.LessOrEqual(t, usage.OutputTokens, 700)
	})
}

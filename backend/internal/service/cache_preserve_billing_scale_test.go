package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 2026-09-16 真实上游实测：actual=30174、billable=10841、excluded=19333，
// 而 input+cache_read+cache_creation 恒等于 billable。这份 usage 是计费口径，
// preserve_upstream_cache_usage 采信它会把上下文规模一起缩到三分之一。
func TestUpstreamUsageIsBillingScale(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "measured relay payload",
			json: `{"input_tokens":21,"cache_read_input_tokens":10820,"cache_creation_input_tokens":0,` +
				`"kiro_actual_input_tokens":30174,"kiro_billable_input_tokens":10841,"kiro_excluded_input_tokens":19333}`,
			want: true,
		},
		{
			name: "excluded alone is enough",
			json: `{"input_tokens":21,"kiro_excluded_input_tokens":19333}`,
			want: true,
		},
		{
			name: "actual greater than billable without excluded",
			json: `{"kiro_actual_input_tokens":30174,"kiro_billable_input_tokens":10841}`,
			want: true,
		},
		{
			// 判据刻意不是「有没有 kiro_ 前缀字段」：不打折的 Kiro 上游
			// （excluded==0 ⇒ billable==真实 prompt）那份 usage 是可以采信的，
			// 不该因为顺带带了几个耗时字段就整份作废。
			name: "kiro fields without discount stay trustworthy",
			json: `{"input_tokens":30174,"kiro_actual_input_tokens":30174,` +
				`"kiro_billable_input_tokens":30174,"kiro_excluded_input_tokens":0,"kiro_total_ms":8123}`,
			want: false,
		},
		{
			name: "plain anthropic usage",
			json: `{"input_tokens":21,"cache_read_input_tokens":10820,"output_tokens":9}`,
			want: false,
		},
		{
			name: "missing usage",
			json: `{}`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, upstreamUsageIsBillingScale(gjson.Parse(tc.json)))
			// map 版本（通用 SSE 路径已把事件解成 map）必须给出同一结论。
			require.Equal(t, tc.want, upstreamUsageMapIsBillingScale(gjsonToMap(t, tc.json)),
				"map discriminator must match the gjson one")
		})
	}
}

func gjsonToMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	out := map[string]any{}
	gjson.Parse(raw).ForEach(func(key, value gjson.Result) bool {
		out[key.String()] = value.Value()
		return true
	})
	return out
}

func TestUpstreamCacheUsageIsTrustworthy(t *testing.T) {
	require.False(t, upstreamCacheUsageIsTrustworthy(false, false), "switch off ⇒ 永远整形")
	require.False(t, upstreamCacheUsageIsTrustworthy(false, true))
	require.True(t, upstreamCacheUsageIsTrustworthy(true, false), "开关开着且是真实口径 ⇒ 采信上游")
	require.False(t, upstreamCacheUsageIsTrustworthy(true, true), "计费口径不能采信，哪怕开关开着")
}

// preserve 开着时上游真值本该放出来；但计费口径的 usage 必须继续走整形，
// 否则下游会看到「上下文只有三分之一」的账单口径。
func TestProjectUsageRefusesBillingScaleUpstream(t *testing.T) {
	simulated := &cacheEmulationUsage{
		InputTokens:                19,
		CacheReadInputTokens:       120000,
		CacheCreationInputTokens:   2400,
		CacheCreation5mInputTokens: 2400,
	}

	t.Run("claude keeps trustworthy upstream values", func(t *testing.T) {
		usage := &ClaudeUsage{InputTokens: 21, OutputTokens: 9, CacheReadInputTokens: 10820}

		projectClaudeUsage(usage, simulated, isolatedUsagePolicy(), 7)

		require.Equal(t, 10820, usage.CacheReadInputTokens)
		require.Zero(t, usage.CacheCreationInputTokens)
	})

	t.Run("claude reshapes billing-scale upstream values", func(t *testing.T) {
		usage := &ClaudeUsage{
			InputTokens: 21, OutputTokens: 9, CacheReadInputTokens: 10820,
			UpstreamBillingScale: true,
		}

		projectClaudeUsage(usage, simulated, isolatedUsagePolicy(), 7)

		require.Equal(t, simulated.CacheReadInputTokens, usage.CacheReadInputTokens)
		require.Equal(t, simulated.CacheCreationInputTokens, usage.CacheCreationInputTokens)
	})

	t.Run("openai keeps trustworthy upstream values", func(t *testing.T) {
		usage := &OpenAIUsage{InputTokens: 21, OutputTokens: 9, CacheReadInputTokens: 10820}

		projectOpenAIUsage(usage, simulated, isolatedUsagePolicy(), 7)

		require.Equal(t, 10820, usage.CacheReadInputTokens)
		require.Zero(t, usage.CacheCreationInputTokens)
	})

	t.Run("openai reshapes billing-scale upstream values", func(t *testing.T) {
		usage := &OpenAIUsage{
			InputTokens: 21, OutputTokens: 9, CacheReadInputTokens: 10820,
			UpstreamBillingScale: true,
		}

		projectOpenAIUsage(usage, simulated, isolatedUsagePolicy(), 7)

		require.Equal(t, simulated.CacheReadInputTokens, usage.CacheReadInputTokens)
		require.Equal(t, simulated.CacheCreationInputTokens, usage.CacheCreationInputTokens)
	})
}

// UpstreamBillingScale 是我们自己的判定结果，不能出现在回给客户端的 JSON 里。
func TestUpstreamBillingScaleIsNotSerialized(t *testing.T) {
	claude, err := json.Marshal(ClaudeUsage{InputTokens: 1, UpstreamBillingScale: true})
	require.NoError(t, err)
	openai, err := json.Marshal(OpenAIUsage{InputTokens: 1, UpstreamBillingScale: true})
	require.NoError(t, err)

	for _, encoded := range []string{string(claude), string(openai)} {
		require.NotContains(t, encoded, "UpstreamBillingScale")
		require.NotContains(t, encoded, "billing_scale")
	}
}

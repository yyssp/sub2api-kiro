//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCacheParameterModelMinimumOverridesAllKinds(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		for _, kind := range []string{CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded} {
			t.Run(protocol+"/"+kind, func(t *testing.T) {
				c := DefaultCacheStrategyConfigForProtocol(kind, protocol)
				c.MinCacheableTokens = 1000000
				c.ModelMinCacheableOverrides = map[string]int{"claude": 20, "claude-sonnet": 10, "claude-sonnet-4-5": 0, "gemini": 20, "models/gemini-2.5-pro": 0}
				require.Zero(t, cacheModelMinimum(c, "claude-sonnet-4-5-20250929"))
				require.Equal(t, 10, cacheModelMinimum(c, "claude-sonnet-4-6"))
				require.Equal(t, 1000000, cacheModelMinimum(c, "other"))
				require.Zero(t, cacheModelMinimum(c, "models/gemini-2.5-pro"))
			})
		}
	}
}

func TestCacheParameterDynamicTailRetainsEarlierPrefix(t *testing.T) {
	for _, allow := range []bool{false, true} {
		p := &cacheProfile{pendingBlocks: []pendingCacheBlock{
			{value: map[string]any{"kind": "system", "block": map[string]any{"type": "text", "text": "stable"}}, tokens: 1000},
			{value: map[string]any{"block": map[string]any{"type": "web_search_tool_result", "content": "fresh"}}, tokens: 2000},
			{value: map[string]any{"block": map[string]any{"type": "text", "text": "later"}}, tokens: 1000},
		}}
		cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
		if allow {
			cfg.DynamicContentMode = CacheDynamicContentAllow
		}
		excludeDynamicCacheTail(p, cfg)
		require.Len(t, p.pendingBlocks, map[bool]int{false: 1, true: 3}[allow])
	}
	require.False(t, cacheValueIsDynamic(map[string]any{"kind": "tool", "tool": map[string]any{"input_schema": map[string]any{"properties": map[string]any{"dynamic": true}}}}))
	require.True(t, cacheValueIsDynamic(map[string]any{"value": map[string]any{"parts": []any{map[string]any{"functionResponse": map[string]any{"name": "web_search"}}}}}))
}

func TestCacheParameterAnthropicModelOverrideChangesEligibility(t *testing.T) {
	resetCacheTracker()
	defer resetCacheTracker()
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1000000
	cfg.ModelMinCacheableOverrides = map[string]int{"claude-sonnet-4-5": 0}
	id := int64(907771)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: id, Name: "parameter gate", Enabled: true, Revision: 1, Config: cfg})
	defer GlobalCacheStrategyRegistry().Delete(id)
	group := &Group{ID: 907771, CacheStrategyID: &id}
	account := &Account{ID: 907771, Platform: PlatformAnthropic}
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"session_id": "minimum"}, "system": strings.Repeat("stable prefix ", 500), "messages": []any{map[string]any{"role": "user", "content": "question"}}})
	plan := (&GatewayService{}).prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-5", 4000)
	require.NotNil(t, plan)
	require.NotNil(t, plan.result())
	require.Positive(t, plan.result().CacheCreationInputTokens)
}

func TestCacheParameterToolResultFlagFiltersNestedBlocks(t *testing.T) {
	for _, kind := range []string{CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded} {
		t.Run(kind, func(t *testing.T) {
			cfg := DefaultCacheStrategyConfig(kind)
			cfg.CacheToolResults = false
			cfg.StableSegment.MinTotalInputTokens = 0
			index := 0
			blocks := []pendingCacheBlock{{value: map[string]any{"kind": "message", "block": map[string]any{"type": "tool_result", "content": "result"}}, tokens: 2000, messageIndex: &index, isMessageEnd: true}}
			require.Empty(t, filterCacheStrategyBlocks(blocks, cfg))
			p := &cacheProfile{pendingBlocks: blocks, totalInputTokens: 2000}
			prepareStableSegmentProfile(p, cfg)
			require.Empty(t, p.pendingBlocks)
		})
	}
}

func TestCacheParameterStableAutoTTLUsesConfiguredDuration(t *testing.T) {
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindStableSegment)
	cfg.DefaultTTLSeconds = 1
	cfg.StableSegment.MinTotalInputTokens = 0
	p := &cacheProfile{pendingBlocks: []pendingCacheBlock{{value: map[string]any{"kind": "system"}, tokens: 2000}}, totalInputTokens: 2000}
	prepareStableSegmentProfile(p, cfg)
	require.NotNil(t, p.pendingBlocks[0].breakpointTTL)
	require.EqualValues(t, 1000000000, *p.pendingBlocks[0].breakpointTTL)
}

func TestCacheParameterGeminiPlusDoesNotStackLegacyOutputUplift(t *testing.T) {
	cfg := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindToolPlus, PlatformGemini)
	cfg.Usage.OutputUpliftEnabled = boolPtr(true)
	cfg.Usage.OutputUpliftMinTokens = 1000
	cfg.Usage.OutputUpliftPercent = 50
	cfg.ToolPlus.Shaping.Output.Multiplier = 2
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"question"}]}],"generationConfig":{"maxOutputTokens":10000}}`)
	plan := &cacheEmulationPlan{profile: buildGeminiCacheProfile(body, "gemini-2.5-pro", cfg), usagePolicy: cfg.Usage, nonStream: true}
	usage := projectGeminiCacheUsage(plan, ClaudeUsage{InputTokens: 1000, OutputTokens: 1200})
	require.Equal(t, 2400, usage.OutputTokens)
}

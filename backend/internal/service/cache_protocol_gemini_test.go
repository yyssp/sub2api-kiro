package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCacheProtocolConfigRoundTrip(t *testing.T) {
	legacy, err := ParseCacheStrategyConfig(map[string]any{"kind": "prefix"})
	require.NoError(t, err)
	require.Equal(t, PlatformAnthropic, legacy.Protocol)
	gemini, err := ParseCacheStrategyConfig(map[string]any{"protocol": "gemini"})
	require.NoError(t, err)
	require.True(t, gemini.AllowDerivedSession)
	require.Equal(t, CacheBreakpointAuto, gemini.BreakpointMode)
	for _, kind := range []string{CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded, CacheStrategyKindDisabled} {
		c := DefaultCacheStrategyConfig(kind)
		c.Protocol = PlatformGemini
		parsed, err := ParseCacheStrategyConfig(CacheStrategyConfigMap(c))
		require.NoError(t, err)
		require.Equal(t, PlatformGemini, parsed.Protocol)
		require.Zero(t, parsed.CreationRatio)
		require.Equal(t, c.Kind, parsed.Kind)
	}
	_, err = ParseCacheStrategyConfig(map[string]any{"protocol": "invalid"})
	require.Error(t, err)
}

func geminiCacheTestGroup(t *testing.T, kind string) (*Group, *Account) {
	t.Helper()
	resetCacheTracker()
	id := int64(995871)
	c := DefaultCacheStrategyConfig(kind)
	c.Protocol = PlatformGemini
	c.AllowDerivedSession = true
	c.MinCacheableTokens = 1
	c.StableSegment.MinSegmentTokens = 1
	c.StableSegment.MinTotalInputTokens = 1
	c.TokenScale = 1
	c.CoverageRatio = .85
	c.UncachedInputMinTokens = 0
	c.Usage.OutputUpliftEnabled = boolPtr(false)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: id, Name: "Gemini test", Enabled: true, Revision: 1, Config: c})
	t.Cleanup(func() { GlobalCacheStrategyRegistry().Delete(id); resetCacheTracker() })
	return &Group{ID: 8541, CacheStrategyID: &id}, geminiSignalTestAccount()
}

func geminiCacheTestBody(extra string) []byte {
	body := map[string]any{"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": strings.Repeat("stable instructions ", 200)}}}, "contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "first question"}}}}}
	contents := body["contents"].([]any)
	if extra != "" {
		contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"text": "answer"}}}, map[string]any{"role": "user", "parts": []any{map[string]any{"text": extra}}})
		body["contents"] = contents
	}
	b, _ := json.Marshal(body)
	return b
}

func TestCacheProtocolGeminiColdHotAllModes(t *testing.T) {
	for _, kind := range []string{CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded} {
		t.Run(kind, func(t *testing.T) {
			group, account := geminiCacheTestGroup(t, kind)
			first := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody(""), "gemini-2.5-pro", "gemini_generate_content", 0)
			require.NotNil(t, first)
			require.NotNil(t, first.usage)
			require.Zero(t, first.usage.CacheReadInputTokens)
			raw := ClaudeUsage{InputTokens: 4000, OutputTokens: 20}
			cold := projectGeminiCacheUsage(first, raw)
			require.Zero(t, cold.CacheReadInputTokens)
			require.Zero(t, cold.CacheCreationInputTokens)
			first.commit()
			next := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody("next"), "gemini-2.5-pro", "gemini_generate_content", 0)
			require.NotNil(t, next)
			require.Positive(t, next.usage.CacheReadInputTokens)
			hot := projectGeminiCacheUsage(next, raw)
			require.Positive(t, hot.CacheReadInputTokens)
			require.Zero(t, hot.CacheCreationInputTokens)
			require.Equal(t, 4000, hot.InputTokens+hot.CacheReadInputTokens)
			require.Nil(t, cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody(""), "gemini-2.5-pro", "anthropic_messages", 0))
		})
	}
}

func TestCacheProtocolGeminiNativeWireAndBilling(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "json"}[stream], func(t *testing.T) {
			group, account := geminiCacheTestGroup(t, CacheStrategyKindPrefix)
			reply := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4000,"candidatesTokenCount":20,"thoughtsTokenCount":10,"toolUsePromptTokenCount":3,"totalTokenCount":4033}}`
			contentType := "application/json"
			if stream {
				reply = "data: " + reply + "\n\n"
				contentType = "text/event-stream"
			}
			for turn := 0; turn < 2; turn++ {
				c, rec := newGeminiNativeTestContext(t)
				SetCacheGroupContext(c, group)
				svc := newGeminiSignalService(contentType, reply)
				action := "generateContent"
				if stream {
					action = "streamGenerateContent"
				}
				result, err := svc.ForwardNative(context.Background(), c, account, "gemini-2.5-pro", action, stream, geminiCacheTestBody("next"))
				require.NoError(t, err)
				wire := strings.TrimSpace(rec.Body.String())
				wire = strings.TrimPrefix(wire, "data: ")
				metadata := gjson.Get(wire, "usageMetadata")
				require.Equal(t, result.Usage.InputTokens+result.Usage.CacheReadInputTokens, int(metadata.Get("promptTokenCount").Int()))
				require.Equal(t, result.Usage.OutputTokens, int(metadata.Get("candidatesTokenCount").Int()+metadata.Get("thoughtsTokenCount").Int()))
				require.Equal(t, result.Usage.InputTokens+result.Usage.CacheReadInputTokens+result.Usage.OutputTokens+3, int(metadata.Get("totalTokenCount").Int()))
				require.Zero(t, result.Usage.CacheCreationInputTokens)
				if turn == 0 {
					require.Zero(t, result.Usage.CacheReadInputTokens)
				} else {
					require.Positive(t, result.Usage.CacheReadInputTokens)
				}
			}
		})
	}
}

func TestCacheProtocolGeminiFailureDoesNotCommit(t *testing.T) {
	for _, reply := range []string{
		`{"candidates":[{"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":4000}}`,
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":4000}}`,
		`{"error":{"code":500,"message":"failed"}}`,
	} {
		t.Run(reply[:min(len(reply), 45)], func(t *testing.T) {
			group, account := geminiCacheTestGroup(t, CacheStrategyKindPrefix)
			c, _ := newGeminiNativeTestContext(t)
			SetCacheGroupContext(c, group)
			_, err := newGeminiSignalService("text/event-stream", "data: "+reply+"\n\n").ForwardNative(context.Background(), c, account, "gemini-2.5-pro", "streamGenerateContent", true, geminiCacheTestBody(""))
			require.NoError(t, err)
			next := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody("next"), "gemini-2.5-pro", "gemini_generate_content", 0)
			require.Zero(t, next.usage.CacheReadInputTokens)
		})
	}
}

func TestCacheProtocolGeminiExplicitCacheAndDisabledProjection(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindPrefix)
	body := []byte(`{"cachedContent":"cachedContents/native","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	p := cachePlanForProtocol(context.Background(), account, group, body, "gemini-2.5-pro", "gemini_generate_content", 0)
	require.Zero(t, p.cacheKey)
	p.usagePolicy.CacheRead = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 2}
	p.usagePolicy.FinalCacheReadMaxTokens = 1
	raw := ClaudeUsage{InputTokens: 10, CacheReadInputTokens: 100, OutputTokens: 20}
	require.Equal(t, 100, projectGeminiCacheUsage(p, raw).CacheReadInputTokens)
	p.usagePolicy.Enabled = false
	require.Equal(t, raw, projectGeminiCacheUsage(p, raw))
	// A Gemini strategy must not shape Claude usage even in the no-plan fallback.
	c, _ := gin.CreateTestContext(nil)
	SetCacheGroupContext(c, group)
	u := raw
	mergeAndCommitCachePlan(c, &u, false)
	require.Equal(t, raw, u)
}

func TestCacheProtocolGeminiIsolationTTLAndCancel(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindPrefix)
	c, _ := newGeminiNativeTestContext(t)
	SetCacheGroupContext(c, group)
	prepareGeminiCacheContext(context.Background(), c, account, geminiCacheTestBody(""), "gemini-2.5-pro", true)
	original := cachePlanFromContext(c)
	require.NotNil(t, original)
	c.Set(geminiCacheCompletedKey, true)
	finishGeminiCachePlan(c, false)
	next := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody("next"), "gemini-2.5-pro", "gemini_generate_content", 0)
	require.Zero(t, next.usage.CacheReadInputTokens)
	original.commit()
	other := geminiCacheTestBody("next")
	var request map[string]any
	require.NoError(t, json.Unmarshal(other, &request))
	request["session_id"] = "different"
	other, _ = json.Marshal(request)
	isolated := cachePlanForProtocol(context.Background(), account, group, other, "gemini-2.5-pro", "gemini_generate_content", 0)
	require.Zero(t, isolated.usage.CacheReadInputTokens)
	key := original.cacheKey
	globalCacheTracker.mu.Lock()
	for fingerprint, entry := range globalCacheTracker.entries[key] {
		entry.expiresAt = time.Now().Add(-time.Second)
		globalCacheTracker.entries[key][fingerprint] = entry
	}
	globalCacheTracker.mu.Unlock()
	expired := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody("next"), "gemini-2.5-pro", "gemini_generate_content", 0)
	require.Zero(t, expired.usage.CacheReadInputTokens)
	strategy := GlobalCacheStrategyRegistry().Get(*group.CacheStrategyID)
	strategy.Config.Protocol = PlatformAnthropic
	GlobalCacheStrategyRegistry().Put(strategy)
	require.Nil(t, cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody(""), "gemini-2.5-pro", "gemini_generate_content", 0))
}

func TestCacheProtocolGeminiStableReadCapIsFinal(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindStableSegment)
	first := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody(""), "gemini-2.5-pro", "gemini_generate_content", 0)
	first.commit()
	p := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody("next"), "gemini-2.5-pro", "gemini_generate_content", 0)
	p.usagePolicy.FinalCacheReadMaxTokens = 50
	p.usagePolicy.FinalCacheReadJitterMinTokens = 0
	p.usagePolicy.FinalCacheReadJitterMaxTokens = 0
	u := projectGeminiCacheUsage(p, ClaudeUsage{InputTokens: 4000, OutputTokens: 20})
	require.LessOrEqual(t, u.CacheReadInputTokens, 50)
	require.Zero(t, u.CacheCreationInputTokens)
}

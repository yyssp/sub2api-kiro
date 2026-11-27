//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCacheInteractionTerminalFramesProjectOnce(t *testing.T) {
	payload := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"id":"msg_audit","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":10000}}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":80}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	for _, kind := range []string{CacheStrategyKindToolPlus, CacheStrategyKindUsageMultiplier} {
		for _, path := range []string{"chat", "responses", "native_chat", "native_responses"} {
			t.Run(kind+"/"+path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				p := advancedCachePlan(kind)
				p.profile.model = "claude-sonnet-4-6"
				p.profile.policy.ToolPlus.Shaping.Output.Multiplier = 2
				p.profile.policy.UsageMultiplier.Output = CacheUsageMultiplierRule{ThresholdTokens: 79, Multiplier: 2}
				setCachePlanContext(c, p)
				resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}
				var usageOutput int
				switch path {
				case "chat":
					r, e := (&GatewayService{}).handleCCStreamingFromAnthropic(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now())
					require.NoError(t, e)
					usageOutput = r.Usage.OutputTokens
				case "responses":
					r, e := (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
					require.NoError(t, e)
					usageOutput = r.Usage.OutputTokens
				case "native_chat":
					r, e := (&OpenAIGatewayService{}).handleCCStreamingFromNativeAnthropic(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now())
					require.NoError(t, e)
					usageOutput = r.Usage.OutputTokens
				case "native_responses":
					r, e := (&OpenAIGatewayService{}).handleResponsesStreamingFromNativeAnthropic(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
					require.NoError(t, e)
					usageOutput = r.Usage.OutputTokens
				}
				require.Equal(t, 160, usageOutput, "80 output at 2x must not become 320 on message_stop")
				for _, line := range strings.Split(rec.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if data == "[DONE]" {
						continue
					}
					if strings.Contains(path, "responses") {
						if gjson.Get(data, "type").String() == "response.completed" {
							require.Equal(t, int64(160), gjson.Get(data, "response.usage.output_tokens").Int())
						}
					} else if gjson.Get(data, "usage").Exists() {
						require.Equal(t, int64(160), gjson.Get(data, "usage.completion_tokens").Int())
					}
				}
			})
		}

	}
}

func TestCacheInteractionGeminiDisabledSmallRequestDoesNotCommit(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindToolPlus)
	cfg := GlobalCacheStrategyRegistry().Get(*group.CacheStrategyID).Config
	cfg.ToolPlus.SmallRequest.Enabled = true
	cfg.ToolPlus.SmallRequest.MaxInputTokens = 4000000
	cfg.ToolPlus.SmallRequest.DisableCache = true
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: *group.CacheStrategyID, Enabled: true, Revision: 2, Config: cfg})
	body := geminiCacheTestBody("")
	p := cachePlanForProtocol(context.Background(), account, group, body, "gemini-2.5-pro", "gemini_generate_content", 0)
	u := projectGeminiCacheUsage(p, ClaudeUsage{InputTokens: 4000, OutputTokens: 80})
	require.Zero(t, u.CacheReadInputTokens)
	p.commit()
	cfg.ToolPlus.SmallRequest.Enabled = false
	// Keep the identity unchanged: turning off the override must not discover hidden writes.
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: *group.CacheStrategyID, Enabled: true, Revision: 2, Config: cfg})
	next := cachePlanForProtocol(context.Background(), account, group, body, "gemini-2.5-pro", "gemini_generate_content", 0)
	require.Zero(t, next.usage.CacheReadInputTokens)
}

func TestCacheInteractionGeminiExcludedContentDoesNotCount(t *testing.T) {
	cfg := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindToolAware, PlatformGemini)
	cfg.MinCacheableTokens = 1
	cfg.CacheSystem = false
	cfg.CacheTools = false
	cfg.CacheToolResults = false
	body := []byte(`{"systemInstruction":{"parts":[{"text":"` + strings.Repeat("excluded system ", 1000) + `"}]},"contents":[{"role":"user","parts":[{"text":"eligible history"},{"functionResponse":{"name":"tool","response":{"text":"` + strings.Repeat("excluded tool result ", 1000) + `"}}}]},{"role":"user","parts":[{"text":"current"}]}]}`)
	profile := buildGeminiCacheProfile(body, "gemini-2.5-pro", cfg)
	require.NotNil(t, profile.lastCacheableBreakpoint())
	require.Less(t, profile.lastCacheableBreakpoint().cumulativeTokens, 100)
	require.Greater(t, profile.totalInputTokens, 1000)
	// Excluded dependencies remain in the fingerprint, so changed earlier content invalidates reuse.
	changed := buildGeminiCacheProfile([]byte(strings.Replace(string(body), "excluded system", "changed system", 1)), "gemini-2.5-pro", cfg)
	require.NotEqual(t, profile.blocks[profile.lastCacheableBreakpoint().blockIndex].prefixFingerprint, changed.blocks[changed.lastCacheableBreakpoint().blockIndex].prefixFingerprint)
}

func TestCacheInteractionOpenAIStableInputRange(t *testing.T) {
	p := advancedCachePlan(CacheStrategyKindStableSegment)
	p.profile.model = "claude-sonnet-4-6"
	p.profile.policy.StableSegment.ReportedInputMinTokens = 20
	p.profile.policy.StableSegment.ReportedInputMaxTokens = 30
	p.profile.policy.UncachedInputMinTokens = 0
	p.profile.policy.UncachedInputMaxTokens = 0
	u := OpenAIUsage{InputTokens: 10000, OutputTokens: 80}
	p.projectOpenAI(&u)
	uncached := u.InputTokens - u.CacheReadInputTokens - u.CacheCreationInputTokens
	require.GreaterOrEqual(t, uncached, 20)
	require.LessOrEqual(t, uncached, 30)
}

func TestCacheInteractionPreviewDeterministicAndCommitOnce(t *testing.T) {
	resetCacheTracker()
	t.Cleanup(resetCacheTracker)
	p := advancedCachePlan(CacheStrategyKindBounded)
	p.creationControlDecision = cacheCreationControlDecision{enabled: true, attempted: 900, allowed: 800}
	raw := ClaudeUsage{InputTokens: 10000, OutputTokens: 80}
	first := raw
	p.projectClaude(&first)
	for i := 0; i < 20; i++ {
		u := raw
		p.projectClaude(&u)
		require.Equal(t, first, u)
	}
	p.commit()
	p.commit()
	require.Equal(t, 1, globalCacheTracker.controlState(p.cacheKey).successfulRequests)
}

func TestCacheInteractionGeminiMultiframeNative(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindToolPlus)
	cfg := GlobalCacheStrategyRegistry().Get(*group.CacheStrategyID).Config
	cfg.ToolPlus.Shaping.Output.Multiplier = 2
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: *group.CacheStrategyID, Enabled: true, Revision: 2, Config: cfg})
	frames := []string{
		`{"candidates":[{"content":{"parts":[{"text":"first"}]}}],"usageMetadata":{"promptTokenCount":4000,"candidatesTokenCount":10}}`,
		`{"candidates":[{"content":{"parts":[{"text":"last"}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":20,"thoughtsTokenCount":10}}`,
	}
	c, rec := newGeminiNativeTestContext(t)
	SetCacheGroupContext(c, group)
	reply := "data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"
	result, err := newGeminiSignalService("text/event-stream", reply).ForwardNative(context.Background(), c, account, "gemini-2.5-pro", "streamGenerateContent", true, geminiCacheTestBody(""))
	require.NoError(t, err)
	require.Equal(t, 60, result.Usage.OutputTokens)
	require.Equal(t, 4000, result.Usage.InputTokens+result.Usage.CacheReadInputTokens)
	var last map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &last))
		}
	}
	wire, _ := json.Marshal(last)
	require.Equal(t, int64(4000), gjson.GetBytes(wire, "usageMetadata.promptTokenCount").Int())
}

func TestCacheInteractionFailedReadDoesNotRefreshTTL(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindPrefix)
	body := geminiCacheTestBody("")
	p := cachePlanForProtocol(context.Background(), account, group, body, "gemini-2.5-pro", "gemini_generate_content", 0)
	p.commit()
	before := map[[32]byte]time.Time{}
	globalCacheTracker.mu.Lock()
	for fp, entry := range globalCacheTracker.entries[p.cacheKey] {
		entry.expiresAt = time.Now().Add(time.Second)
		globalCacheTracker.entries[p.cacheKey][fp] = entry
		before[fp] = entry.expiresAt
	}
	globalCacheTracker.mu.Unlock()
	next := cachePlanForProtocol(context.Background(), account, group, body, "gemini-2.5-pro", "gemini_generate_content", 0)
	require.Positive(t, next.usage.CacheReadInputTokens)
	globalCacheTracker.mu.Lock()
	for fp, entry := range globalCacheTracker.entries[p.cacheKey] {
		require.Equal(t, before[fp], entry.expiresAt)
	}
	globalCacheTracker.mu.Unlock()
	next.commit()
	globalCacheTracker.mu.Lock()
	for fp, entry := range globalCacheTracker.entries[p.cacheKey] {
		require.True(t, entry.expiresAt.After(before[fp]))
	}
	globalCacheTracker.mu.Unlock()
}

func TestCacheInteractionSystemOnlyAllowed(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		c := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindPrefix, protocol)
		c.CacheHistory = false
		c.CacheCurrentUserStablePrefix = false
		_, err := NormalizeCacheStrategyConfig(c)
		require.NoError(t, err)
	}
}

func TestCacheInteractionGeminiImageAndFailurePassthrough(t *testing.T) {
	for _, image := range []bool{true, false} {
		group, account := geminiCacheTestGroup(t, CacheStrategyKindToolPlus)
		c, _ := newGeminiNativeTestContext(t)
		SetCacheGroupContext(c, group)
		prepareGeminiCacheContext(context.Background(), c, account, geminiCacheTestBody(""), "gemini-2.5-pro", true)
		p := cachePlanFromContext(c)
		p.profile.policy.ToolPlus.Shaping.Output.Multiplier = 2
		body := []byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4000,"candidatesTokenCount":1000,"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":900}]}}`)
		if !image {
			body = []byte(`{"error":{"code":500},"usageMetadata":{"promptTokenCount":4000}}`)
		}
		out, u := shapeGeminiResponse(c, body)
		require.JSONEq(t, string(body), string(out))
		require.Equal(t, extractGeminiUsage(body).billingUsageTokens(), u.billingUsageTokens())
	}
}

func TestCacheInteractionCanceledClaudeDoesNotCommit(t *testing.T) {
	resetCacheTracker()
	t.Cleanup(resetCacheTracker)
	p := advancedCachePlan(CacheStrategyKindToolPlus)
	p.creationControlDecision = cacheCreationControlDecision{enabled: true, attempted: 900, allowed: 800}
	c, _ := gin.CreateTestContext(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
	setCachePlanContext(c, p)
	u := ClaudeUsage{InputTokens: 10000, OutputTokens: 80}
	mergeAndCommitCachePlan(c, &u, true)
	commitCachePlan(c)
	require.False(t, p.committed.Load())
	require.Empty(t, globalCacheTracker.controls)
}

func TestCacheInteractionControlStateIdleCleanup(t *testing.T) {
	tracker := &cacheTracker{entries: map[uint64]map[[32]byte]cacheEntry{}, controls: map[uint64]cacheControlState{}}
	now := time.Now()
	tracker.controls[1] = cacheControlState{successfulRequests: 8, lastUsedAt: now.Add(-2 * time.Hour), windowStart: now.Add(-4 * time.Hour)}
	tracker.controls[2] = cacheControlState{successfulRequests: 8, lastUsedAt: now.Add(-time.Minute)}
	tracker.controls[3] = cacheControlState{successfulRequests: 8, lastUsedAt: now.Add(-2 * time.Hour), retention: 24 * time.Hour}
	tracker.pruneLocked(now)
	require.NotContains(t, tracker.controls, uint64(1))
	require.Contains(t, tracker.controls, uint64(2))
	require.Contains(t, tracker.controls, uint64(3))
}

func TestCacheInteractionGeminiStableFloor(t *testing.T) {
	p := advancedCachePlan(CacheStrategyKindStableSegment)
	p.profile.policy.Protocol = PlatformGemini
	p.profile.policy.UncachedInputMinTokens = 0
	p.profile.policy.UncachedInputMaxTokens = 0
	p.profile.policy.StableSegment.ReportedInputMinTokens = 100
	p.profile.policy.StableSegment.ReportedInputMaxTokens = 200
	p.usage = &cacheEmulationUsage{CacheReadInputTokens: 9999}
	p.profile.totalInputTokens = 10000
	u := projectGeminiCacheUsage(p, ClaudeUsage{InputTokens: 10000, OutputTokens: 80})
	require.GreaterOrEqual(t, u.InputTokens, 100)
	require.LessOrEqual(t, u.InputTokens, 200)
}

func TestCacheInteractionConcurrentColdPlansStayIsolated(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindPrefix)
	const count = 12
	plans := make(chan *cacheEmulationPlan, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plans <- cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody(""), "gemini-2.5-pro", "gemini_generate_content", 0)
		}()
	}
	wg.Wait()
	close(plans)
	for p := range plans {
		require.Zero(t, p.usage.CacheReadInputTokens)
		p.commit()
		p.commit()
	}
	next := cachePlanForProtocol(context.Background(), account, group, geminiCacheTestBody("next"), "gemini-2.5-pro", "gemini_generate_content", 0)
	require.Positive(t, next.usage.CacheReadInputTokens)
}

func TestCacheInteractionExplicitCacheCannotOverflowLimits(t *testing.T) {
	p := advancedCachePlan(CacheStrategyKindPrefix)
	p.profile.policy.Protocol = PlatformGemini
	p.profile.model = "gemini-2.5-pro"
	p.profile.rawBody = []byte(`{"cachedContent":"cachedContents/native"}`)
	u := ClaudeUsage{InputTokens: 1 << 62, CacheReadInputTokens: 1 << 62, CacheCreationInputTokens: 1 << 62, OutputTokens: 1 << 62}
	p.enforceUsageLimits(&u, u)
	require.LessOrEqual(t, u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens, 1048576)
	require.GreaterOrEqual(t, u.InputTokens, 0)
	require.GreaterOrEqual(t, u.CacheReadInputTokens, 0)
}

func TestCacheInteractionStableZeroRatiosDoNotInventWrites(t *testing.T) {
	p := advancedCachePlan(CacheStrategyKindStableSegment)
	p.profile.policy.UsageRatio = 0
	p.profile.policy.ReadRatio = 0
	p.profile.policy.CreationRatio = 0
	p.usage = &cacheEmulationUsage{InputTokens: 10000}
	u := ClaudeUsage{InputTokens: 10000, OutputTokens: 80}
	p.projectClaude(&u)
	require.Zero(t, u.CacheReadInputTokens)
	require.Zero(t, u.CacheCreationInputTokens)
}

func TestCacheInteractionGeminiStableZeroReadRatio(t *testing.T) {
	p := advancedCachePlan(CacheStrategyKindStableSegment)
	p.profile.policy.Protocol = PlatformGemini
	p.profile.policy.UsageRatio, p.profile.policy.ReadRatio = 0, 0
	p.profile.totalInputTokens = 10000
	u := projectGeminiCacheUsage(p, ClaudeUsage{InputTokens: 10000, OutputTokens: 80})
	require.Zero(t, u.CacheReadInputTokens)
	require.Zero(t, u.CacheCreationInputTokens)
}

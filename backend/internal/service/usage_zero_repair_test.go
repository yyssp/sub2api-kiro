//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestZeroUsageRegularAnthropicStreamMissingFinalUsage(t *testing.T) {
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindUsageMultiplier)
	cfg.UsageMultiplier.Output.Multiplier = 2
	p := &cacheEmulationPlan{profile: &cacheProfile{policy: cfg, model: "claude-sonnet-4-5"}, usagePolicy: cfg.Usage}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	setCachePlanContext(c, p)
	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"real response content\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	s := &GatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}
	r, err := s.handleStreamingResponse(context.Background(), &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, c, &Account{ID: 1}, time.Now(), "claude-sonnet-4-5", "claude-sonnet-4-5", false)
	require.NoError(t, err)
	require.Positive(t, r.usage.OutputTokens)
	require.Equal(t, r.usage.rawUsage.OutputTokens*2, r.usage.OutputTokens)
	require.Zero(t, r.usage.rawUsage.UpstreamTokens.OutputTokens)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.Contains(line, `"type":"message_delta"`) {
			require.EqualValues(t, r.usage.OutputTokens, gjson.Get(strings.TrimPrefix(line, "data: "), "usage.output_tokens").Int())
		}
	}
}

func TestZeroUsageEveryStrategyKeepsInputAndRealOutputPositive(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		for _, kind := range []string{CacheStrategyKindDisabled, CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded, CacheStrategyKindUsageMultiplier} {
			t.Run(protocol+"/"+kind, func(t *testing.T) {
				cfg := DefaultCacheStrategyConfigForProtocol(kind, protocol)
				cfg.UsageMultiplier = CacheUsageMultiplierPolicy{}
				cfg.ToolPlus.Shaping.Input.Multiplier = 0
				cfg.ToolPlus.Shaping.Output.Multiplier = 0
				cfg.Limits.MaxInputTokens = 1
				cfg.Limits.MaxOutputTokens = 1
				p := &cacheEmulationPlan{profile: &cacheProfile{model: "claude-sonnet-4-5", policy: cfg}, usagePolicy: cfg.Usage, nonStream: true, guardOnly: kind == CacheStrategyKindDisabled}
				u := ClaudeUsage{InputTokens: 100, OutputTokens: 100, CacheReadInputTokens: 200}
				if protocol == PlatformGemini {
					u = projectGeminiCacheUsage(p, u)
				} else {
					p.projectClaude(&u)
				}
				require.Positive(t, u.InputTokens)
				require.Positive(t, u.OutputTokens)
				if kind != CacheStrategyKindDisabled {
					require.LessOrEqual(t, u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens, 1)
					require.LessOrEqual(t, u.OutputTokens, 1)
				}
			})
		}
	}
}

func TestZeroUsageRepairUsesContentAndRetainsUpstreamCostTokens(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		cfg := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindUsageMultiplier, protocol)
		cfg.UsageMultiplier.Output.Multiplier = 2
		p := &cacheEmulationPlan{profile: &cacheProfile{policy: cfg, rawBody: []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)}, usagePolicy: cfg.Usage, nonStream: true, outputEvidence: &usageOutputEvidence{}}
		text := "The actual upstream reply contains useful text."
		if protocol == PlatformAnthropic {
			p.outputEvidence.observeAnthropic([]byte(`{"type":"message","content":[{"type":"text","text":"` + text + `"}]}`))
		} else {
			p.outputEvidence.observeGemini([]byte(`{"candidates":[{"content":{"parts":[{"text":"` + text + `"}]},"finishReason":"STOP"}]}`))
		}
		u := ClaudeUsage{}
		if protocol == PlatformGemini {
			u = projectGeminiCacheUsage(p, u)
		} else {
			p.projectClaude(&u)
		}
		require.Positive(t, u.InputTokens)
		estimate := anthropictokenizer.CountTokens(text)
		require.Equal(t, estimate, u.rawUsage.OutputTokens)
		require.Equal(t, estimate*2, u.OutputTokens)
		require.Zero(t, u.rawUsage.UpstreamTokens.OutputTokens)
		require.Zero(t, u.rawUsage.UpstreamTokens.InputTokens)
		require.False(t, u.rawUsage.EmptyResponse)
	}
}

func TestZeroUsageRepairEmptyWhitespaceAndToolOutput(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		empty      bool
	}{
		{"blank", `{"type":"message","content":[{"type":"text","text":"  \n  "}]}`, true},
		{"tool", `{"type":"message","content":[{"type":"tool_use","name":"lookup","input":{}}]}`, false},
		{"thinking", `{"type":"message","content":[{"type":"thinking","thinking":"Reason through the request."}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultCacheStrategyConfig(CacheStrategyKindUsageMultiplier)
			p := &cacheEmulationPlan{profile: &cacheProfile{policy: cfg}, usagePolicy: cfg.Usage, nonStream: true, outputEvidence: &usageOutputEvidence{}}
			p.outputEvidence.observeAnthropic([]byte(tc.body))
			u := ClaudeUsage{InputTokens: 50}
			p.projectClaude(&u)
			require.Equal(t, tc.empty, u.rawUsage.EmptyResponse)
			if tc.empty {
				require.Zero(t, u.OutputTokens)
			} else {
				require.Positive(t, u.OutputTokens)
			}
		})
	}
}

func TestZeroUsageStreamRepairsOnlyAfterTerminalAndCombinesFragments(t *testing.T) {
	e := &usageOutputEvidence{}
	e.observeAnthropic([]byte(`{"type":"message_start","message":{"usage":{"output_tokens":0}}}`))
	require.False(t, e.final)
	e.observeAnthropic([]byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}`))
	e.observeAnthropic([]byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"world!"}}`))
	e.observeAnthropic([]byte(`{"type":"message_delta","usage":{"output_tokens":0}}`))
	require.True(t, e.final)
	require.Equal(t, anthropictokenizer.CountTokens("hello world!"), e.estimate())
}

func TestZeroUsageRepairedOutputRespectsRequestCapWhenProjectionSkipped(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		for _, mode := range []string{"guard", "skip", "raw"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				cfg := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindToolPlus, protocol)
				cfg.Usage.SkipNonStreamUsageProjection = mode == "skip"
				if mode == "raw" {
					cfg.ToolPlus.NonStream.Mode = "raw"
				}
				p := &cacheEmulationPlan{profile: &cacheProfile{policy: cfg, model: "claude-sonnet-4-5", rawBody: []byte(`{"max_tokens":1}`)}, usagePolicy: cfg.Usage, nonStream: true, guardOnly: mode == "guard", outputEvidence: &usageOutputEvidence{}}
				p.outputEvidence.append("a real upstream response with several tokens")
				u := ClaudeUsage{InputTokens: 10}
				if protocol == PlatformGemini {
					u = projectGeminiCacheUsage(p, u)
				} else {
					p.projectClaude(&u)
				}
				require.Equal(t, 1, u.OutputTokens)
				require.Greater(t, u.rawUsage.OutputTokens, 1)
				require.Zero(t, u.rawUsage.UpstreamTokens.OutputTokens)
				if protocol == PlatformAnthropic {
					p.emptyResponse = false
					ou := OpenAIUsage{InputTokens: 10}
					p.projectOpenAI(&ou)
					require.Equal(t, 1, ou.OutputTokens)
				}
			})
		}
	}
}

func TestZeroUsageTruncatedWhitespaceCannotHideRealContent(t *testing.T) {
	e := &usageOutputEvidence{}
	e.append(strings.Repeat(" ", usageOutputEvidenceMaxBytes))
	e.append("real content after the capture limit")
	require.Positive(t, e.estimate())
}

func TestZeroUsageInterruptedStreamRepairsBeforeShaping(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		for _, text := range []string{"", "some real response content"} {
			cfg := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindUsageMultiplier, protocol)
			cfg.UsageMultiplier.Output.Multiplier = 2
			p := &cacheEmulationPlan{profile: &cacheProfile{policy: cfg}, usagePolicy: cfg.Usage, outputEvidence: &usageOutputEvidence{}}
			p.outputEvidence.append(text)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			setCachePlanContext(c, p)
			u := &ClaudeUsage{InputTokens: 100}
			// An earlier partial usage snapshot must not suppress the final repair.
			p.repairClaudeUsage(u)
			finishUnreportedUsage(c, u)
			require.Equal(t, 100, u.rawUsage.UpstreamTokens.InputTokens)
			require.Zero(t, u.rawUsage.UpstreamTokens.OutputTokens)
			if text == "" {
				require.True(t, u.rawUsage.EmptyResponse)
				require.Zero(t, u.OutputTokens)
			} else {
				require.False(t, u.rawUsage.EmptyResponse)
				require.Equal(t, u.rawUsage.OutputTokens*2, u.OutputTokens)
			}
		}
	}
}

func TestZeroUsageStandardAnthropicKeepsUpstreamTTLBeforeOverride(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			account, settings, _ := cacheTTLOverrideBoundaryAccount(t, "account/1h")
			cfg := DefaultCacheStrategyConfig(CacheStrategyKindUsageMultiplier)
			p := &cacheEmulationPlan{profile: &cacheProfile{policy: cfg, model: "claude-sonnet-4"}, usagePolicy: cfg.Usage, nonStream: !stream}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			setCachePlanContext(c, p)
			tc := cacheTTLOverrideBoundaryCase{name: "mixed", snapshots: []string{`{"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":600,"ephemeral_1h_input_tokens":400}}`}, aggregate: 1000, present: true, ttl5m: 600, ttl1h: 400}
			payload := cacheTTLOverrideBoundaryResponse(tc, stream, 0)
			svc := &GatewayService{cfg: &config.Config{}, settingService: settings, rateLimitService: &RateLimitService{}}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}
			var u *ClaudeUsage
			if stream {
				r, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "claude-sonnet-4", "claude-sonnet-4", false)
				require.NoError(t, err)
				u = r.usage
			} else {
				var err error
				u, err = svc.handleNonStreamingResponse(context.Background(), resp, c, account, "claude-sonnet-4", "claude-sonnet-4")
				require.NoError(t, err)
			}
			require.Zero(t, u.CacheCreation5mTokens)
			require.Equal(t, 1000, u.CacheCreation1hTokens)
			require.Equal(t, 600, u.rawUsage.UpstreamTokens.CacheCreation5mTokens)
			require.Equal(t, 400, u.rawUsage.UpstreamTokens.CacheCreation1hTokens)
		})
	}
}

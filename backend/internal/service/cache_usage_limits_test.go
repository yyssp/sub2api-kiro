//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheUsageExtremeMatrix(t *testing.T) {
	kinds := []string{CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded}
	models := []struct {
		name                   string
		input, output, context int
	}{{"claude-sonnet-4-5", 200000, 64000, 0}, {"claude-sonnet-4-6", 1000000, 64000, 0}, {"gemini-2.5-pro", 1048576, 65535, 0}, {"unknown-alias", 200000, 64000, 200000}}
	for _, wire := range []string{"anthropic", "openai", "gemini"} {
		for _, kind := range kinds {
			for _, model := range models {
				for _, stream := range []bool{false, true} {
					for _, n := range []int{0, 1, 10000, 1500000, 4000000} {
						t.Run(fmt.Sprintf("%s/%s/%s/stream=%t/input=%d", wire, kind, model.name, stream, n), func(t *testing.T) {
							p := advancedCachePlan(kind)
							p.nonStream = !stream
							c := &p.profile.policy
							if wire == "gemini" {
								c.Protocol = PlatformGemini
								cacheProtocolModules[PlatformGemini].normalize(c)
							}
							c.TokenScale = 3
							c.MaxSimulatedInputTokens = 0
							c.ReportedInputMaxTokens = 0
							c.HitShaping.Enabled = true
							c.HitShaping.MinCostRatio = 5
							c.HitShaping.ReadAwareCostFloor = true
							c.PreserveUpstreamCacheUsage = false
							c.Usage.PreserveUpstreamCacheUsage = false
							c.Usage.FinalCacheReadMaxTokens = 4000000
							c.Usage.FinalCacheCreationMaxTokens = 4000000
							c.ToolPlus.Shaping.Input.Multiplier = 1000
							c.ToolPlus.Shaping.Output.Multiplier = 1000
							c.ToolPlus.Shaping.CacheRead.Multiplier = 1000
							c.ToolPlus.Shaping.CacheCreation.Multiplier = 1000
							c.ToolPlus.Shaping.MinCostRatio = 100
							p.usagePolicy = c.Usage
							p.profile.model = model.name
							p.profile.runtimeInputTokens = n
							p.profile.totalInputTokens = max(n, 1)
							p.profile.reportedInputTokens = n
							p.profile.rawBody = []byte(`{"max_tokens":777,"generationConfig":{"maxOutputTokens":777}}`)
							p.usage = &cacheEmulationUsage{InputTokens: n / 10, CacheReadInputTokens: n * 8 / 10, CacheCreationInputTokens: n / 10}
							if kind == CacheStrategyKindToolPlus && wire != "gemini" {
								p.preparePlus()
							}
							raw := ClaudeUsage{InputTokens: n, OutputTokens: 10000}
							var u ClaudeUsage
							switch wire {
							case "gemini":
								u = projectGeminiCacheUsage(p, raw)
							case "openai":
								v := OpenAIUsage{InputTokens: n, OutputTokens: 10000}
								p.projectOpenAI(&v)
								u = ClaudeUsage{InputTokens: v.InputTokens - v.CacheReadInputTokens - v.CacheCreationInputTokens, CacheReadInputTokens: v.CacheReadInputTokens, CacheCreationInputTokens: v.CacheCreationInputTokens, OutputTokens: v.OutputTokens}
							default:
								u = raw
								p.projectClaude(&u)
							}
							total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
							require.LessOrEqual(t, total, model.input)
							require.LessOrEqual(t, u.OutputTokens, 777)
							require.GreaterOrEqual(t, u.InputTokens, 0)
							require.GreaterOrEqual(t, u.CacheReadInputTokens, 0)
							require.GreaterOrEqual(t, u.CacheCreationInputTokens, 0)
							if model.context > 0 {
								require.LessOrEqual(t, total+u.OutputTokens, model.context)
							}
							if n == 0 {
								require.Positive(t, u.InputTokens)
							}
							if wire == "gemini" {
								require.Zero(t, u.CacheCreationInputTokens)
							} else if wire == "anthropic" {
								require.Equal(t, u.CacheCreationInputTokens, u.CacheCreation5mTokens+u.CacheCreation1hTokens)
							}
						})
					}
				}
			}
		}
	}
}

func TestCacheUsageFinalParameterPrecedence(t *testing.T) {
	for _, kind := range []string{CacheStrategyKindPrefix, CacheStrategyKindToolPlus, CacheStrategyKindBounded} {
		t.Run(kind, func(t *testing.T) {
			p := advancedCachePlan(kind)
			c := &p.profile.policy
			c.Limits = CacheStrategyLimits{MaxInputTokens: 1000, MaxOutputTokens: 70}
			c.HitShaping.Enabled = true
			c.HitShaping.MinCostRatio = 5
			c.Usage.Input = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 40}
			c.Usage.CacheRead = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 300}
			c.Usage.CacheCreation = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 200}
			c.Usage.Output = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 50}
			c.Usage.FinalCacheReadMaxTokens = 250
			c.Usage.FinalCacheCreationMaxTokens = 100
			c.MaxNewCreationTokensPerRequest = 80
			c.ToolPlus.Shaping.MinCostRatio = 100
			c.ToolPlus.Shaping.Input.MaxTokens = 30
			p.usagePolicy = c.Usage
			u := ClaudeUsage{InputTokens: 10000, OutputTokens: 1000}
			p.projectClaude(&u)
			require.LessOrEqual(t, u.InputTokens, 40)
			require.LessOrEqual(t, u.CacheReadInputTokens, 250)
			require.LessOrEqual(t, u.CacheCreationInputTokens, 80)
			require.LessOrEqual(t, u.OutputTokens, 50)
			if kind == CacheStrategyKindToolPlus {
				require.LessOrEqual(t, u.InputTokens, 30)
			}
		})
	}
}

func TestCacheUsageRawAndSkipRemainUntouched(t *testing.T) {
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		for _, mode := range []string{"disabled", "skip", "raw"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				p := advancedCachePlan(CacheStrategyKindToolPlus)
				p.profile.policy.Protocol = protocol
				p.nonStream = true
				switch mode {
				case "disabled":
					p.usagePolicy.Enabled = false
				case "skip":
					p.usagePolicy.SkipNonStreamUsageProjection = true
				case "raw":
					p.profile.policy.ToolPlus.NonStream.Mode = "raw"
				}
				raw := ClaudeUsage{InputTokens: 1500000, CacheReadInputTokens: 1300000, OutputTokens: 200000}
				u := raw
				if protocol == PlatformGemini {
					u = projectGeminiCacheUsage(p, raw)
				} else {
					p.projectClaude(&u)
				}
				u.rawUsage = nil
				require.Equal(t, raw, u)
			})
		}
	}
}

func TestCacheConfigRejectExtremeParameters(t *testing.T) {
	for _, key := range []string{"reported_input_min_tokens", "uncached_input_max_tokens", "state_ttl_seconds", "max_entries_global", "max_simulated_input_tokens"} {
		t.Run(key, func(t *testing.T) {
			_, err := ParseCacheStrategyConfig(map[string]any{key: int64(1 << 62)})
			require.Error(t, err)
		})
	}
	_, err := ParseCacheStrategyConfig(map[string]any{"tool_plus": map[string]any{"shaping": map[string]any{"input": map[string]any{"max_tokens": int64(1 << 62)}}}})
	require.Error(t, err)
}

// Frontend consumes these backend defaults; regeneration is explicit and reviewed.
func TestCacheDefaultsSharedSnapshot(t *testing.T) {
	configs := map[string]CacheStrategyConfig{}
	for _, protocol := range []string{PlatformAnthropic, PlatformGemini} {
		for _, kind := range []string{CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded, CacheStrategyKindDisabled, CacheStrategyKindUsageMultiplier} {
			c, err := ParseCacheStrategyConfig(map[string]any{"protocol": protocol, "kind": kind})
			require.NoError(t, err)
			configs[protocol+"/"+kind] = c
		}
	}
	path := filepath.Join("..", "..", "..", "frontend", "src", "views", "admin", "cacheStrategyDefaults.json")
	b, err := json.MarshalIndent(configs, "", "  ")
	require.NoError(t, err)
	b = append(b, '\n')
	if os.Getenv("UPDATE_CACHE_DEFAULTS") == "1" {
		require.NoError(t, os.WriteFile(path, b, 0644))
	}
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.JSONEq(t, string(b), string(actual), "regenerate with UPDATE_CACHE_DEFAULTS=1")
}

func TestCacheUsageMillionModelExamples(t *testing.T) {
	for _, tc := range []struct {
		model string
		cap   int
	}{{"claude-sonnet-4-5", 200000}, {"claude-sonnet-4-6", 1000000}, {"gemini-2.5-pro", 1048576}} {
		t.Run(tc.model, func(t *testing.T) {
			p := advancedCachePlan(CacheStrategyKindPrefix)
			p.profile.model = tc.model
			p.profile.runtimeInputTokens = 1500000
			p.profile.totalInputTokens = 1500000
			p.profile.reportedInputTokens = 1500000
			c := &p.profile.policy
			c.MaxSimulatedInputTokens = 0
			c.UncachedInputMinTokens = 0
			c.UncachedInputMaxTokens = 0
			c.Usage.FinalCacheReadMaxTokens = 0
			c.Usage.FinalCacheCreationMaxTokens = 0
			c.CreationControl.Enabled = false
			p.usagePolicy = c.Usage
			p.usage = &cacheEmulationUsage{InputTokens: 100, CacheReadInputTokens: 1499900}
			raw := ClaudeUsage{InputTokens: 1500000, OutputTokens: 100}
			u := raw
			if tc.model == "gemini-2.5-pro" {
				c.Protocol = PlatformGemini
				u = projectGeminiCacheUsage(p, raw)
			} else {
				p.projectClaude(&u)
			}
			total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
			require.LessOrEqual(t, total, tc.cap)
			require.Positive(t, u.CacheReadInputTokens)
			t.Logf("AUDIT model=%s raw_input=1500000 final_input=%d read=%d write=%d output=%d input_total=%d", tc.model, u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, u.OutputTokens, total)
		})
	}
}

func TestCacheUsageGeminiDefaultNativeExtremeWire(t *testing.T) {
	for _, kind := range []string{CacheStrategyKindPrefix, CacheStrategyKindToolAware, CacheStrategyKindStableSegment, CacheStrategyKindToolPlus, CacheStrategyKindBounded} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
				group, account := geminiCacheTestGroup(t, kind)
				c, err := ParseCacheStrategyConfig(map[string]any{"protocol": PlatformGemini, "kind": kind})
				require.NoError(t, err)
				c.Limits = CacheStrategyLimits{MaxInputTokens: 25000, MaxOutputTokens: 100}
				GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: *group.CacheStrategyID, Name: "defaults", Enabled: true, Revision: 2, Config: c})
				reply := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1500000,"candidatesTokenCount":200000,"thoughtsTokenCount":100000,"toolUsePromptTokenCount":3,"totalTokenCount":1800003}}`
				contentType := "application/json"
				action := "generateContent"
				if stream {
					reply = "data: " + reply + "\n\n"
					contentType = "text/event-stream"
					action = "streamGenerateContent"
				}
				body := []byte(`{"generationConfig":{"maxOutputTokens":80},"systemInstruction":{"parts":[{"text":"` + strings.Repeat("stable system ", 2000) + `"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
				for turn := 0; turn < 2; turn++ {
					ctx, rec := newGeminiNativeTestContext(t)
					SetCacheGroupContext(ctx, group)
					result, err := newGeminiSignalService(contentType, reply).ForwardNative(context.Background(), ctx, account, "gemini-2.5-pro", action, stream, body)
					require.NoError(t, err)
					wire := strings.TrimPrefix(strings.TrimSpace(rec.Body.String()), "data: ")
					meta := gjson.Get(wire, "usageMetadata")
					require.Equal(t, result.Usage.InputTokens+result.Usage.CacheReadInputTokens, int(meta.Get("promptTokenCount").Int()))
					require.Equal(t, result.Usage.OutputTokens, int(meta.Get("candidatesTokenCount").Int()+meta.Get("thoughtsTokenCount").Int()))
					require.Equal(t, int(meta.Get("promptTokenCount").Int()+meta.Get("candidatesTokenCount").Int()+meta.Get("thoughtsTokenCount").Int()+3), int(meta.Get("totalTokenCount").Int()))
					require.LessOrEqual(t, result.Usage.InputTokens+result.Usage.CacheReadInputTokens, 25000)
					require.LessOrEqual(t, result.Usage.OutputTokens, 80)
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
}

func TestCacheUsageNoBreakpointStillEnforcesLimits(t *testing.T) {
	group, account := geminiCacheTestGroup(t, CacheStrategyKindPrefix)
	c := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	c.Limits = CacheStrategyLimits{MaxInputTokens: 1000, MaxOutputTokens: 50}
	c.AllowDerivedSession = false
	c.HitShaping.Enabled = true
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: *group.CacheStrategyID, Enabled: true, Revision: 3, Config: c})
	ctx, _ := gin.CreateTestContext(nil)
	p := prepareCachePlanForContext(context.Background(), ctx, account, group, []byte(`{"max_tokens":40,"messages":[{"role":"user","content":"hi"}]}`), "claude-sonnet-4-5", "anthropic_messages", 1500000)
	require.NotNil(t, p)
	require.Nil(t, p.usage)
	u := ClaudeUsage{InputTokens: 1500000, OutputTokens: 10000}
	mergeAndCommitCachePlan(ctx, &u, false)
	require.LessOrEqual(t, u.InputTokens, 1000)
	require.LessOrEqual(t, u.OutputTokens, 40)
	require.Zero(t, u.CacheReadInputTokens)
	require.Zero(t, u.CacheCreationInputTokens)
}

func TestCacheUsageCreationWindowRemainsFinal(t *testing.T) {
	resetCacheTracker()
	t.Cleanup(resetCacheTracker)
	for i := 0; i < 6; i++ {
		p := advancedCachePlan(CacheStrategyKindPrefix)
		c := &p.profile.policy
		c.HitShaping.Enabled = true
		c.HitShaping.ReadAwareCostFloor = true
		c.HitShaping.MinCostRatio = 5
		c.CreationControl = CacheCreationControl{Enabled: true, MaxCreationTokensPerEvent: 1000, CreationBudgetWindowSeconds: 300, MaxCreationTokensPerWindow: 1600}
		p.usage = &cacheEmulationUsage{InputTokens: 100, CacheReadInputTokens: 9000, CacheCreationInputTokens: 900}
		p.creationControlDecision = applyCacheCreationControl(p.cacheKey, c.CreationControl, p.usage)
		u := ClaudeUsage{InputTokens: 10000, OutputTokens: 20}
		p.projectClaude(&u)
		require.LessOrEqual(t, u.CacheCreationInputTokens, p.creationControlDecision.allowed)
		p.commit()
		require.LessOrEqual(t, globalCacheTracker.controlState(p.cacheKey).windowTokens, 1600)
	}
}

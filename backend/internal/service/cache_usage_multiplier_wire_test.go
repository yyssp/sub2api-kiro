//go:build unit

package service

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCacheUsageMultiplierGeminiNativeWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			group, account := geminiCacheTestGroup(t, CacheStrategyKindUsageMultiplier)
			cfg := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindUsageMultiplier, PlatformGemini)
			cfg.UsageMultiplier.Input = CacheUsageMultiplierRule{ThresholdTokens: 100, Multiplier: .5}
			cfg.UsageMultiplier.Output = CacheUsageMultiplierRule{ThresholdTokens: 1000, Multiplier: 1.5}
			cfg.UsageMultiplier.CacheRead = CacheUsageMultiplierRule{Multiplier: 2}
			cfg.Usage.SkipNonStreamUsageProjection = true
			cfg.Usage.StrictCacheEligibilityStream = true
			cfg.Usage.StrictCacheEligibilityNonStream = true
			GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: *group.CacheStrategyID, Enabled: true, Revision: 2, Config: cfg})
			reply := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":6000,"cachedContentTokenCount":4000,"candidatesTokenCount":1000,"thoughtsTokenCount":200,"totalTokenCount":7200}}`
			contentType := "application/json"
			action := "generateContent"
			if stream {
				reply = "data: " + reply + "\n\n"
				contentType = "text/event-stream"
				action = "streamGenerateContent"
			}
			c, rec := newGeminiNativeTestContext(t)
			SetCacheGroupContext(c, group)
			result, err := newGeminiSignalService(contentType, reply).ForwardNative(context.Background(), c, account, "gemini-2.5-pro", action, stream, []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
			require.NoError(t, err)
			require.Equal(t, 1000, result.Usage.InputTokens)
			require.Equal(t, 8000, result.Usage.CacheReadInputTokens)
			require.Equal(t, 1800, result.Usage.OutputTokens)
			require.Zero(t, result.Usage.CacheCreationInputTokens)
			wire := strings.TrimPrefix(strings.TrimSpace(rec.Body.String()), "data: ")
			meta := gjson.Get(wire, "usageMetadata")
			require.Equal(t, int64(9000), meta.Get("promptTokenCount").Int())
			require.Equal(t, int64(1500), meta.Get("candidatesTokenCount").Int())
			require.Equal(t, int64(300), meta.Get("thoughtsTokenCount").Int())
			require.Equal(t, int64(10800), meta.Get("totalTokenCount").Int())
			require.Empty(t, globalCacheTracker.entries)
			require.Empty(t, globalCacheTracker.controls)
		})
	}
}

func TestCacheMultiplierAuditGeminiThresholdCrossingFrames(t *testing.T) {
	for _, factor := range []float64{0, .5, 1.5, 2} {
		t.Run(fmt.Sprint(factor), func(t *testing.T) {
			group, account := geminiCacheTestGroup(t, CacheStrategyKindUsageMultiplier)
			cfg := DefaultCacheStrategyConfigForProtocol(CacheStrategyKindUsageMultiplier, PlatformGemini)
			cfg.UsageMultiplier.Output = CacheUsageMultiplierRule{ThresholdTokens: 1000, Multiplier: factor}
			GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: *group.CacheStrategyID, Enabled: true, Revision: 2, Config: cfg})
			frames := []string{
				`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":4000,"candidatesTokenCount":999}}`,
				`{"candidates":[{"content":{"parts":[{"text":"b"}]}}],"usageMetadata":{"candidatesTokenCount":1000}}`,
				`{"candidates":[{"content":{"parts":[{"text":"c"}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1001}}`,
			}
			c, rec := newGeminiNativeTestContext(t)
			SetCacheGroupContext(c, group)
			r, e := newGeminiSignalService("text/event-stream", "data: "+strings.Join(frames, "\n\ndata: ")+"\n\n").ForwardNative(context.Background(), c, account, "gemini-2.5-pro", "streamGenerateContent", true, []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
			require.NoError(t, e)
			expected := int(math.Round(1001 * factor))
			require.Equal(t, expected, r.Usage.OutputTokens)
			require.Equal(t, 4000, r.Usage.InputTokens)
			var actual []int
			for _, line := range strings.Split(rec.Body.String(), "\n") {
				if strings.HasPrefix(line, "data:") {
					meta := gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "usageMetadata")
					actual = append(actual, int(meta.Get("candidatesTokenCount").Int()+meta.Get("thoughtsTokenCount").Int()))
					require.Equal(t, int64(4000), meta.Get("promptTokenCount").Int())
				}
			}
			require.Equal(t, []int{999, 1000, expected}, actual)
		})
	}
}

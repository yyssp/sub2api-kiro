package service

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func shapeGeminiResponse(c *gin.Context, body []byte) ([]byte, *ClaudeUsage) {
	p := cachePlanFromContext(c)
	raw := extractGeminiUsage(body)
	if raw == nil || p == nil || normalizeCacheProtocol(p.policyConfig().Protocol) != PlatformGemini || p.skipProjection() || !p.usagePolicy.Enabled {
		return body, raw
	}
	if _, failed := detectGeminiResponseSignalInBody(body); failed || c != nil && c.GetBool(geminiCacheFailedKey) {
		return body, raw
	}
	if p.policyConfig().Kind == CacheStrategyKindToolPlus {
		_, rawMode, _ := p.selectedPlusProfile()
		if rawMode {
			return body, raw
		}
	}
	// Streaming usage frames are cumulative but may omit unchanged prompt/detail fields.
	// Keep only raw metadata so later frames never amplify an already projected value.
	if c != nil && !p.nonStream {
		metadata := map[string]json.RawMessage{}
		if previous, ok := c.Get(geminiCacheRawMetadataKey); ok {
			for key, value := range previous.(map[string]json.RawMessage) {
				metadata[key] = value
			}
		}
		var incoming map[string]json.RawMessage
		if json.Unmarshal([]byte(gjson.GetBytes(body, "usageMetadata").Raw), &incoming) == nil {
			for key, value := range incoming {
				metadata[key] = value
			}
			c.Set(geminiCacheRawMetadataKey, metadata)
			if merged, err := sjson.SetBytes(body, "usageMetadata", metadata); err == nil {
				body = merged
				raw = extractGeminiUsage(body)
			}
		}
	}
	if raw.ImageOutputTokens > 0 {
		// Text shaping cannot describe or reprice image modality tokens.
		return body, raw
	}
	u := projectGeminiCacheUsage(p, *raw)
	metadata := gjson.GetBytes(body, "usageMetadata")
	thoughts := int(metadata.Get("thoughtsTokenCount").Int())
	if raw.OutputTokens > 0 {
		thoughts = min(u.OutputTokens, cacheRounded(float64(thoughts)*float64(u.OutputTokens)/float64(raw.OutputTokens)))
	} else {
		thoughts = 0
	}
	values := map[string]int{"promptTokenCount": saturatingAddInt(u.InputTokens, u.CacheReadInputTokens), "cachedContentTokenCount": u.CacheReadInputTokens, "candidatesTokenCount": u.OutputTokens - thoughts, "totalTokenCount": saturatingAddInt(saturatingAddInt(u.InputTokens, u.CacheReadInputTokens), saturatingAddInt(u.OutputTokens, max(int(metadata.Get("toolUsePromptTokenCount").Int()), 0)))}
	if metadata.Get("thoughtsTokenCount").Exists() {
		values["thoughtsTokenCount"] = thoughts
	}
	updated := body
	for field, value := range values {
		var err error
		updated, err = sjson.SetBytes(updated, "usageMetadata."+field, value)
		if err != nil {
			return body, raw
		}
	}
	if u.InputTokens == raw.InputTokens && u.CacheReadInputTokens == raw.CacheReadInputTokens && u.OutputTokens == raw.OutputTokens {
		return body, raw
	}
	// Modality breakdowns describe upstream totals and cannot describe synthetic buckets.
	for _, field := range []string{"promptTokensDetails", "cacheTokensDetails", "candidatesTokensDetails"} {
		if field == "candidatesTokensDetails" && raw.ImageOutputTokens > 0 {
			continue
		}
		updated, _ = sjson.DeleteBytes(updated, "usageMetadata."+field)
	}
	return updated, &u
}

func prepareGeminiCacheContext(ctx context.Context, c *gin.Context, a *Account, body []byte, model string, stream bool) {
	// Stream selection lives in the URL, unlike Anthropic's request JSON.
	local, err := sjson.SetBytes(body, "stream", stream)
	if err != nil {
		return
	}
	setCachePlanContext(c, nil)
	if c != nil {
		c.Set(geminiCacheRawMetadataKey, map[string]json.RawMessage{})
		c.Set(geminiCacheCompletedKey, false)
		c.Set(geminiCacheFailedKey, false)
	}
	prepareCachePlanForContext(ctx, c, a, cacheGroupFromContext(c, a), local, model, "gemini_generate_content", 0)
}

const geminiCacheRawMetadataKey = "sub2api.gemini_cache_raw_metadata"

const geminiCacheCompletedKey = "sub2api.gemini_cache_completed"
const geminiCacheFailedKey = "sub2api.gemini_cache_failed"

func observeGeminiCacheCompletion(c *gin.Context, body []byte) {
	if c == nil || cachePlanFromContext(c) == nil {
		return
	}
	if _, failed := detectGeminiResponseSignalInBody(body); failed {
		c.Set(geminiCacheFailedKey, true)
	}
	if gjson.GetBytes(body, "candidates.0.finishReason").String() != "" {
		c.Set(geminiCacheCompletedKey, true)
	}
}

func finishGeminiCachePlan(c *gin.Context, success bool) {
	if c == nil || !success || !c.GetBool(geminiCacheCompletedKey) || c.GetBool(geminiCacheFailedKey) {
		return
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return
	}
	commitCachePlan(c)
}

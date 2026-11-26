package service

import (
	"encoding/json"
	"time"
	"unicode/utf8"
)

func buildGeminiCacheProfile(body []byte, model string, cfg CacheStrategyConfig) *cacheProfile {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	p := &cacheProfile{model: model, protocolFamily: "gemini_generate_content", rawBody: append([]byte(nil), body...), policy: cfg, policyEnabled: true, minCacheable: cfg.MinCacheableTokens}
	p.minCacheable = cacheModelMinimum(cfg, model)
	ttl := time.Duration(cfg.DefaultTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = cacheDefaultTTL
	}
	currentUserTokens := 0
	add := func(kind string, value any, index *int, eligible bool) {
		if value == nil {
			return
		}
		encoded, err := canonicalJSON(value)
		if err != nil {
			return
		}
		tokens := max((utf8.RuneCount(encoded)+3)/4, 1)
		p.totalInputTokens = saturatingAddInt(p.totalInputTokens, tokens)
		block := pendingCacheBlock{value: map[string]any{"kind": kind, "value": value}, tokens: tokens, messageIndex: index, isMessageEnd: index != nil}
		if cfg.Kind == CacheStrategyKindStableSegment && index != nil && cfg.StableSegment.HistoryMessageLimit > 0 {
			contents, _ := request["contents"].([]any)
			if *index < len(contents)-1-cfg.StableSegment.HistoryMessageLimit {
				eligible = false
			}
		}
		if cfg.Kind == CacheStrategyKindStableSegment {
			eligible = eligible && (kind == "system" && cfg.StableSegment.AutoCacheSystem || (kind == "tool" || kind == "tool_config") && cfg.StableSegment.AutoCacheTools || index != nil && cfg.StableSegment.AutoCacheHistoryMessageEnds)
		}
		if index != nil {
			contents, _ := request["contents"].([]any)
			if *index == len(contents)-1 {
				if cfg.CurrentUserStablePrefixMaxTokens <= 0 || currentUserTokens+tokens > cfg.CurrentUserStablePrefixMaxTokens {
					eligible = false
				}
				currentUserTokens = saturatingAddInt(currentUserTokens, tokens)
			}
		}
		if eligible {
			block.breakpointTTL = &ttl
		} else {
			block.tokens = 0
		}
		p.pendingBlocks = append(p.pendingBlocks, block)
	}
	add("system", request["systemInstruction"], nil, cfg.CacheSystem)
	add("tool", request["tools"], nil, cfg.CacheTools)
	add("tool_config", request["toolConfig"], nil, cfg.CacheTools)
	contents, _ := request["contents"].([]any)
	for i, content := range contents {
		eligible := cfg.CacheHistory && i < len(contents)-1
		if i == len(contents)-1 && cfg.CacheCurrentUserStablePrefix {
			eligible = true
		}
		object, _ := content.(map[string]any)
		parts, _ := object["parts"].([]any)
		kind := "message"
		for _, part := range parts {
			pm, _ := part.(map[string]any)
			if pm["functionResponse"] != nil {
				kind = "tool_result"
				if cfg.Kind != CacheStrategyKindToolAware && cfg.Kind != CacheStrategyKindToolPlus {
					eligible = eligible && cfg.CacheToolResults
				}
			}
			// File/image token counts cannot be inferred from the JSON byte length.
			if pm["inlineData"] != nil || pm["fileData"] != nil || pm["inline_data"] != nil || pm["file_data"] != nil {
				return &cacheProfile{model: model, protocolFamily: p.protocolFamily, rawBody: p.rawBody, policy: cfg}
			}
		}
		if cfg.Kind == CacheStrategyKindToolAware || cfg.Kind == CacheStrategyKindToolPlus {
			for j, part := range parts {
				pm, _ := part.(map[string]any)
				partKind := "message"
				partEligible := eligible
				if pm["functionResponse"] != nil {
					partKind = "tool_result"
					partEligible = partEligible && cfg.CacheToolResults
				}
				add(partKind, map[string]any{"role": object["role"], "part": part}, &i, partEligible)
				if len(p.pendingBlocks) > 0 {
					p.pendingBlocks[len(p.pendingBlocks)-1].isMessageEnd = j == len(parts)-1
				}
			}
		} else {
			add(kind, content, &i, eligible)
		}
	}
	p.runtimeInputTokens, p.reportedInputTokens = p.totalInputTokens, p.totalInputTokens
	p.preludeCanonical, _ = canonicalJSON(map[string]any{"protocol": "gemini", "toolConfig": request["toolConfig"]})
	if cfg.Kind == CacheStrategyKindStableSegment {
		if p.totalInputTokens < cfg.StableSegment.MinTotalInputTokens {
			p.pendingBlocks = nil
		}
	}
	excludeDynamicCacheTail(p, cfg)
	rebuildCacheProfile(p)
	eligible := p.breakpoints[:0]
	for _, b := range p.breakpoints {
		if p.pendingBlocks[b.blockIndex].breakpointTTL != nil {
			eligible = append(eligible, b)
		}
	}
	p.breakpoints = eligible
	return p
}

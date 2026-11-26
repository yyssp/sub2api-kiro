package service

import (
	"encoding/json"
	"strings"
	"time"
)

func cacheStateTTL(c CacheStrategyConfig, display time.Duration) time.Duration {
	if c.StateTTLSeconds <= 0 {
		return display
	}
	return minDuration(time.Duration(c.StateTTLSeconds)*time.Second, cacheDefaultTTL)
}

func prepareStableSegmentProfile(p *cacheProfile, c CacheStrategyConfig) {
	s := c.StableSegment
	p.scaleBreakpointsToInputTokens = false
	if p.totalInputTokens < s.MinTotalInputTokens {
		p.pendingBlocks = nil
		return
	}
	last := -1
	for _, b := range p.pendingBlocks {
		if b.messageIndex != nil {
			last = max(last, *b.messageIndex)
		}
	}
	var payload map[string]any
	_ = json.Unmarshal(p.rawBody, &payload)
	currentUserTokens := 0
	currentUserMax := c.CurrentUserStablePrefixMaxTokens
	if messages, ok := payload["messages"].([]any); ok && len(messages) > 0 {
		last = len(messages) - 1
		if tail, ok := messages[last].(map[string]any); ok && tail["role"] != "user" {
			last++
		}
	}
	out := make([]pendingCacheBlock, 0, len(p.pendingBlocks))
	for _, b := range p.pendingBlocks {
		if b.messageIndex != nil && (*b.messageIndex == last && !c.CacheCurrentUserStablePrefix || s.HistoryMessageLimit > 0 && *b.messageIndex < last-s.HistoryMessageLimit) {
			continue
		}
		kind := ""
		if v, ok := b.value.(map[string]any); ok {
			kind, _ = v["kind"].(string)
		}
		if kind == "system" && !c.CacheSystem || kind == "tool" && !c.CacheTools || b.messageIndex != nil && !c.CacheHistory && *b.messageIndex < last || !c.CacheToolResults && (strings.Contains(kind, "tool") && kind != "tool" || cacheValueIsToolResult(b.value)) {
			continue
		}
		if b.messageIndex != nil && *b.messageIndex == last {
			if !c.CacheCurrentUserStablePrefix || currentUserMax <= 0 || currentUserTokens+max(b.tokens, 0) > currentUserMax {
				continue
			}
			currentUserTokens += max(b.tokens, 0)
		}
		explicit := b.breakpointTTL != nil && !p.defaultedBreakpoints && s.HonorExplicitCacheControl
		auto := kind == "system" && s.AutoCacheSystem || kind == "tool" && s.AutoCacheTools || b.messageIndex != nil && b.isMessageEnd && s.AutoCacheHistoryMessageEnds
		originalTTL := b.breakpointTTL
		b.breakpointTTL = nil
		if explicit || auto {
			ttl := cacheDefaultTTL
			if explicit && originalTTL != nil {
				ttl = *originalTTL
			} else if c.DefaultTTLSeconds > 0 {
				ttl = time.Duration(c.DefaultTTLSeconds) * time.Second
			}
			b.breakpointTTL = &ttl
		}
		out = append(out, b)
	}
	p.pendingBlocks = out
	p.defaultedBreakpoints = false
}

func requestQualifiesForPromptCache(p *cacheProfile) bool {
	var payload map[string]any
	_ = json.Unmarshal(p.rawBody, &payload)
	total := 0
	prefix := 0
	for _, b := range p.pendingBlocks {
		total += max(b.tokens, 0)
		if b.breakpointTTL != nil && !p.defaultedBreakpoints {
			prefix = total
		}
	}
	if extractCacheTTL(payload) != nil {
		prefix = total
	}
	model := strings.ToLower(strings.ReplaceAll(p.model, "_", "-"))
	minimum := 1024
	if strings.Contains(model, "haiku") {
		minimum = 4096
		if strings.Contains(model, "3-5") || strings.Contains(model, "3.5") {
			minimum = 2048
		}
	} else if strings.Contains(model, "opus") {
		minimum = 4096
	}
	return prefix >= minimum
}

func cacheIneligiblePlan(group *Group, body []byte) *cacheEmulationPlan {
	cfg, enabled := effectiveCacheStrategyConfig(group)
	if !enabled {
		return nil
	}
	strict := cfg.Usage.StrictCacheEligibilityNonStream
	stream := requestBodyWantsStream(body)
	if stream {
		strict = cfg.Usage.StrictCacheEligibilityStream
	}
	if !strict && !(cfg.Usage.SkipNonStreamUsageProjection && !stream) {
		return nil
	}
	return &cacheEmulationPlan{profile: &cacheProfile{policy: cfg, rawBody: body}, usagePolicy: cfg.Usage, projectionIneligible: true, nonStream: !stream}
}

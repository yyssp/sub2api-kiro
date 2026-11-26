package service

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
)

// Protocol modules own wire semantics; tracker and field shaping remain shared.
type cacheProtocolModule interface {
	normalize(*CacheStrategyConfig)
	matches(string) bool
	prepare(*GatewayService, context.Context, *Account, *Group, []byte, string, string, int) *cacheEmulationPlan
}

var cacheProtocolModules = map[string]cacheProtocolModule{
	PlatformAnthropic: anthropicCacheProtocol{},
	PlatformGemini:    geminiCacheProtocol{},
}

const cacheRequestProtocolKey = "sub2api.cache_request_protocol"

func normalizeCacheProtocol(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		return PlatformAnthropic
	}
	return protocol
}

func cacheProtocolMatches(configured, wire string) bool {
	module := cacheProtocolModules[normalizeCacheProtocol(configured)]
	return module != nil && module.matches(wire)
}

func cacheRequestProtocol(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value := c.GetString(cacheRequestProtocolKey); value != "" {
		return value
	}
	if c.Request != nil && c.Request.URL != nil {
		path := c.Request.URL.Path
		if strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent") || strings.Contains(path, ":countTokens") {
			return PlatformGemini
		}
	}
	return ""
}

func prepareProtocolCachePlan(s *GatewayService, ctx context.Context, account *Account, group *Group, body []byte, model, wire string, input int) *cacheEmulationPlan {
	cfg, enabled := effectiveCacheStrategyConfig(group)
	if !enabled || !cacheProtocolMatches(cfg.Protocol, wire) {
		return nil
	}
	if cfg.Kind == CacheStrategyKindUsageMultiplier {
		return usageMultiplierPlan(cfg, body, model, wire)
	}
	module := cacheProtocolModules[normalizeCacheProtocol(cfg.Protocol)]
	if module == nil {
		return nil
	}
	return module.prepare(s, ctx, account, group, body, model, wire, input)
}

type anthropicCacheProtocol struct{}

func (anthropicCacheProtocol) normalize(_ *CacheStrategyConfig) {}

func (anthropicCacheProtocol) matches(wire string) bool {
	switch wire {
	case "", "anthropic", "anthropic_messages", "openai_responses", "openai_chat_completions":
		return true
	default:
		return false
	}
}

func (anthropicCacheProtocol) prepare(s *GatewayService, ctx context.Context, a *Account, g *Group, body []byte, model, wire string, input int) *cacheEmulationPlan {
	var p *cacheEmulationPlan
	switch wire {
	case "openai_responses":
		p = s.prepareResponsesCacheUsage(ctx, a, g, body, model, input)
	case "openai_chat_completions":
		p = s.prepareChatCompletionsCacheUsage(ctx, a, g, body, model, input)
	default:
		p = s.prepareCacheEmulationUsage(ctx, a, g, body, model, input)
	}
	if p == nil {
		p = cacheIneligiblePlan(g, body)
	}
	return p
}

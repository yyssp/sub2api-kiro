package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Evidence belongs to one upstream attempt. Only response content is counted;
// envelope IDs, usage metadata and finish markers cannot turn an empty reply into output.
type usageOutputEvidence struct {
	text     strings.Builder
	snapshot string
	final    bool
	tool     bool
	content  bool
}

const usageOutputEvidenceMaxBytes = 4 << 20

func (e *usageOutputEvidence) append(text string) {
	if strings.TrimSpace(text) != "" {
		e.content = true
	}
	if remaining := usageOutputEvidenceMaxBytes - e.text.Len(); remaining > 0 {
		_, _ = e.text.WriteString(text[:min(len(text), remaining)])
	}
}

func (e *usageOutputEvidence) estimate() int {
	if e == nil {
		return 0
	}
	text := strings.TrimSpace(e.text.String())
	full := strings.TrimSpace(e.snapshot)
	n := max(anthropictokenizer.CountTokens(text), anthropictokenizer.CountTokens(full))
	if e.tool {
		n = max(n, 1)
	}
	if e.content {
		n = max(n, 1)
	}
	return n
}

func (e *usageOutputEvidence) observeAnthropic(body []byte) {
	if e == nil {
		return
	}
	r := gjson.ParseBytes(body)
	switch r.Get("type").String() {
	case "content_block_delta":
		delta := r.Get("delta")
		for _, field := range []string{"text", "thinking", "partial_json"} {
			e.append(delta.Get(field).String())
		}
	case "content_block_start":
		block := r.Get("content_block")
		if block.Get("type").String() == "tool_use" || block.Get("type").String() == "server_tool_use" {
			e.tool = true
			e.append(block.Get("name").String())
		}
		if block.Get("type").String() == "redacted_thinking" && block.Get("data").String() != "" {
			e.tool = true
		}
		e.append(block.Get("text").String())
		e.append(block.Get("thinking").String())
	case "message_delta", "message_stop":
		e.final = true
	case "message_start":
	default:
		if r.Get("content").IsArray() {
			var text strings.Builder
			for _, block := range r.Get("content").Array() {
				switch block.Get("type").String() {
				case "tool_use", "server_tool_use":
					e.tool = true
					_, _ = text.WriteString(block.Get("name").String())
					_, _ = text.WriteString(block.Get("input").Raw)
				case "redacted_thinking":
					if block.Get("data").String() != "" {
						e.tool = true
					}
				default:
					_, _ = text.WriteString(block.Get("text").String())
					_, _ = text.WriteString(block.Get("thinking").String())
				}
			}
			e.snapshot = text.String()[:min(text.Len(), usageOutputEvidenceMaxBytes)]
			e.content = e.content || strings.TrimSpace(text.String()) != ""
			e.final = true
		}
	}
}

func (e *usageOutputEvidence) observeGemini(body []byte) {
	if e == nil {
		return
	}
	r := gjson.ParseBytes(body)
	for !r.Get("candidates").Exists() && r.Get("response").IsObject() {
		r = r.Get("response")
	}
	for _, candidate := range r.Get("candidates").Array() {
		for _, part := range candidate.Get("content.parts").Array() {
			e.append(part.Get("text").String())
			if call := part.Get("functionCall"); call.IsObject() {
				e.tool = true
				e.append(call.Get("name").String())
				e.append(call.Get("args").Raw)
			}
		}
		if candidate.Get("finishReason").String() != "" {
			e.final = true
		}
	}
}

func (e *usageOutputEvidence) observeOpenAI(body []byte, event string) {
	if e == nil {
		return
	}
	r := gjson.ParseBytes(body)
	if event == "" {
		event = r.Get("type").String()
	}
	if strings.HasSuffix(event, ".delta") {
		if delta := r.Get("delta"); delta.Type == gjson.String {
			e.append(delta.String())
		}
	}
	for _, choice := range r.Get("choices").Array() {
		for _, field := range []string{"delta", "message"} {
			m := choice.Get(field)
			e.append(m.Get("content").String())
			e.append(m.Get("reasoning_content").String())
			if len(m.Get("tool_calls").Array()) > 0 || m.Get("function_call").IsObject() {
				e.tool = true
			}
		}
		if choice.Get("finish_reason").Type == gjson.String {
			e.final = true
		}
	}
	if r.Get("response").IsObject() {
		r = r.Get("response")
	}
	if r.Get("output").IsArray() {
		var text strings.Builder
		for _, item := range r.Get("output").Array() {
			if item.Get("type").String() == "function_call" {
				e.tool = true
				_, _ = text.WriteString(item.Get("arguments").String())
			}
			for _, block := range item.Get("content").Array() {
				_, _ = text.WriteString(block.Get("text").String())
			}
			for _, block := range item.Get("summary").Array() {
				_, _ = text.WriteString(block.Get("text").String())
			}
		}
		e.snapshot = text.String()[:min(text.Len(), usageOutputEvidenceMaxBytes)]
		e.content = e.content || strings.TrimSpace(text.String()) != ""
	}
	if isUpstreamResponseModelTerminalEvent(event) || r.Get("status").String() == "completed" {
		e.final = true
	}
}

func (p *cacheEmulationPlan) repairClaudeUsage(u *ClaudeUsage) {
	if p == nil || u == nil || u.rawUsage != nil {
		return
	}
	upstream := u.billingUsageTokens()
	if u.InputTokens <= 0 {
		if u.CacheReadInputTokens > 0 {
			u.CacheReadInputTokens--
			u.InputTokens = 1
		} else if u.CacheCreationInputTokens > 0 {
			u.CacheCreationInputTokens--
			u.InputTokens = 1
			u.CacheCreation5mTokens, u.CacheCreation1hTokens = capCacheCreationBreakdown(u.CacheCreation5mTokens, u.CacheCreation1hTokens, u.CacheCreationInputTokens)
		} else {
			u.InputTokens = 1
			if p.profile != nil {
				u.InputTokens = max(1, p.profile.runtimeInputTokens)
				if u.InputTokens == 1 {
					if normalizeCacheProtocol(p.policyConfig().Protocol) == PlatformGemini {
						if profile := buildGeminiCacheProfile(p.profile.rawBody, p.profile.model, p.policyConfig()); profile != nil {
							u.InputTokens = max(1, profile.runtimeInputTokens)
						}
					} else {
						u.InputTokens = max(1, estimateKiroInputTokens(context.Background(), anthropictokenizer.AlgorithmOfficial, p.profile.rawBody))
					}
				}
			}
		}
	}
	if u.OutputTokens <= 0 && p.outputEvidence != nil && (p.nonStream || p.outputEvidence.final) {
		u.OutputTokens = p.outputEvidence.estimate()
	}
	u.captureRawUsage()
	u.rawUsage.UpstreamTokens = upstream
	u.rawUsage.EmptyResponse = upstream.OutputTokens <= 0 && u.OutputTokens <= 0 && p.outputEvidence != nil && (p.nonStream || p.outputEvidence.final)
	p.emptyResponse = u.rawUsage.EmptyResponse
}

func enforceNonzeroClaudeUsage(u *ClaudeUsage, raw ClaudeUsage) {
	if u == nil {
		return
	}
	if u.InputTokens <= 0 {
		if u.CacheReadInputTokens > 0 {
			u.CacheReadInputTokens--
		} else if u.CacheCreationInputTokens > 0 {
			u.CacheCreationInputTokens--
			u.CacheCreation5mTokens, u.CacheCreation1hTokens = capCacheCreationBreakdown(u.CacheCreation5mTokens, u.CacheCreation1hTokens, u.CacheCreationInputTokens)
		}
		u.InputTokens = 1
	}
	if raw.OutputTokens > 0 {
		u.OutputTokens = max(u.OutputTokens, 1)
	}
}

func (p *cacheEmulationPlan) limitRepairedUsage(u *ClaudeUsage) {
	if p == nil || p.profile == nil || u == nil || u.rawUsage == nil {
		return
	}
	upstream := u.rawUsage.UpstreamTokens
	limits := lookupCacheModelLimits(p.profile.model)
	if upstream.OutputTokens <= 0 && u.OutputTokens > 0 {
		cap := minPositiveCacheLimit(limits.Output, p.policyConfig().Limits.MaxOutputTokens)
		for _, key := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens", "generationConfig.maxOutputTokens"} {
			if value := gjson.GetBytes(p.profile.rawBody, key).Int(); value > 0 {
				cap = min(cap, int(value))
			}
		}
		u.OutputTokens = min(u.OutputTokens, cap)
	}
	if upstream.InputTokens <= 0 && upstream.CacheReadTokens == 0 && upstream.CacheCreationTokens == 0 {
		cap := minPositiveCacheLimit(limits.Input, p.policyConfig().Limits.MaxInputTokens)
		if limits.Context > 0 {
			cap = min(cap, max(limits.Context-u.OutputTokens, 1))
		}
		u.InputTokens = min(u.InputTokens, cap)
	}
}

func (raw *RawUsageSnapshot) upstreamClaudeUsage() ClaudeUsage {
	copy := *raw
	copy.Tokens = raw.UpstreamTokens
	return copy.claudeUsage()
}

func (p *cacheEmulationPlan) repairOpenAIUsage(u *OpenAIUsage) {
	if u == nil || u.rawUsage != nil {
		return
	}
	upstream := u.billingUsageTokens()
	claude := ClaudeUsage{InputTokens: upstream.InputTokens, OutputTokens: upstream.OutputTokens, CacheReadInputTokens: upstream.CacheReadTokens, CacheCreationInputTokens: upstream.CacheCreationTokens, CacheCreation5mTokens: upstream.CacheCreation5mTokens, CacheCreation1hTokens: upstream.CacheCreation1hTokens}
	p.repairClaudeUsage(&claude)
	u.InputTokens = saturatingAddInt(claude.InputTokens, saturatingAddInt(claude.CacheReadInputTokens, claude.CacheCreationInputTokens))
	u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens = claude.OutputTokens, claude.CacheReadInputTokens, claude.CacheCreationInputTokens
	u.captureRawUsage()
	u.rawUsage.UpstreamTokens = upstream
	u.rawUsage.EmptyResponse = claude.rawUsage.EmptyResponse
}

func (raw *RawUsageSnapshot) isEmptyResponse() bool { return raw != nil && raw.EmptyResponse }

func markEmptyUsageResponse(c *gin.Context, usage *ClaudeUsage) {
	if c == nil || usage == nil || !usage.rawUsage.isEmptyResponse() {
		return
	}
	setOpsUpstreamError(c, 502, "upstream returned no output content", "zero output usage and zero response-content estimate")
	MarkOpsStreamErrorValue(c, OpsStreamError{ErrType: "upstream_error", Code: "EMPTY_RESPONSE", Message: "upstream returned no output content", IntendedStatus: 502, CountTowardsSLA: true, NonStream: cachePlanFromContext(c) != nil && cachePlanFromContext(c).nonStream})
}

// A disconnected stream may never emit final usage. Billing still needs to
// distinguish delivered content from a genuinely empty attempt.
func finishUnreportedUsage(c *gin.Context, usage *ClaudeUsage) {
	p := cachePlanFromContext(c)
	if p == nil || usage == nil || p.outputEvidence == nil {
		return
	}
	if usage.rawUsage != nil && (usage.OutputTokens > 0 || usage.rawUsage.EmptyResponse || usage.rawUsage.UpstreamTokens.OutputTokens > 0) {
		return
	}
	p.outputEvidence.final = true
	if usage.rawUsage != nil {
		*usage = usage.rawUsage.upstreamClaudeUsage()
	}
	if normalizeCacheProtocol(p.policyConfig().Protocol) == PlatformGemini {
		*usage = projectGeminiCacheUsage(p, *usage)
	} else {
		p.projectClaude(usage)
	}
	markEmptyUsageResponse(c, usage)
}

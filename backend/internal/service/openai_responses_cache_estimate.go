package service

import (
	"context"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// estimateOpenAIResponsesInputTokens returns a Responses-shape token estimate
// that understands the OpenAI Responses request surface.
//
// The generic estimateKiroInputTokens helper is anthropic-centric: it counts
// system/messages/tools and falls back to a 1-token minimum for valid JSON
// bodies that do not look like Anthropic Messages. That is too small for
// Responses requests, where the cache policy ratio and usage projection can
// round the synthetic cache buckets down to zero. This helper uses the
// Responses block flattener so the cache plan receives a realistic token
// baseline before read/write shaping.
func estimateOpenAIResponsesInputTokens(ctx context.Context, body []byte) int {
	if len(body) == 0 {
		return 0
	}

	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return estimateKiroInputTokens(ctx, body)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return estimateKiroInputTokens(ctx, body)
	}

	blocks := flattenResponsesCacheBlocks(ctx, payload)
	if len(blocks) == 0 {
		return estimateKiroInputTokens(ctx, body)
	}

	effectiveTools, err := apicompat.EffectiveResponsesTools(&req)
	if err != nil {
		return estimateKiroInputTokens(ctx, body)
	}

	return countKiroResponsesInputTokens(payload, blocks, len(effectiveTools))
}

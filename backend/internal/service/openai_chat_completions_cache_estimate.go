package service

import (
	"context"
	"encoding/json"
)

// estimateOpenAIChatCompletionsInputTokens returns a token estimate for the
// native Chat Completions request shape. The cache plan must be built from the
// public protocol body, not from an intermediate Anthropic conversion, because
// the latter can drop fields (for example tool metadata) and under-report the
// stable prefix used for cache shaping.
func estimateOpenAIChatCompletionsInputTokens(ctx context.Context, body []byte) int {
	if len(body) == 0 {
		return 0
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return estimateKiroInputTokens(ctx, body)
	}
	blocks := flattenChatCompletionsCacheBlocks(ctx, payload)
	if len(blocks) == 0 {
		return estimateKiroInputTokens(ctx, body)
	}

	tools, _ := payload["tools"].([]any)
	functions, _ := payload["functions"].([]any)
	return countKiroChatCompletionsInputTokens(payload, blocks, len(tools)+len(functions))
}

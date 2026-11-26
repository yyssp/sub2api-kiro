package service

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Only protocol cache markers are changed; tool inputs, schemas and metadata remain opaque.
func normalizeUpstreamCacheTTL(body []byte) []byte {
	if !strings.Contains(string(body), `"cache_control"`) {
		return body
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return body
	}
	changed := false
	marker := func(obj map[string]any) {
		if cc, ok := obj["cache_control"].(map[string]any); ok && strings.EqualFold(cacheAsString(cc["type"]), "ephemeral") && cc["ttl"] != "5m" {
			cc["ttl"] = "5m"
			changed = true
		}
	}
	var content func(any)
	content = func(v any) {
		if blocks, ok := v.([]any); ok {
			for _, v := range blocks {
				if block, ok := v.(map[string]any); ok {
					marker(block)
					if block["type"] == "tool_result" {
						content(block["content"])
					}
				}
			}
		}
	}
	marker(root)
	content(root["system"])
	if tools, ok := root["tools"].([]any); ok {
		for _, v := range tools {
			if tool, ok := v.(map[string]any); ok {
				marker(tool)
			}
		}
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, v := range messages {
			if msg, ok := v.(map[string]any); ok {
				content(msg["content"])
			}
		}
	}
	if !changed {
		return body
	}
	result, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return result
}

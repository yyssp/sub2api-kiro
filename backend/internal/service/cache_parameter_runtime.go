package service

import "strings"

func cacheModelMinimum(c CacheStrategyConfig, model string) int {
	minimum := max(c.MinCacheableTokens, 0)
	model = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
	best := -1
	for key, value := range c.ModelMinCacheableOverrides {
		key = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(key), "models/"))
		if key != "" && (model == key || strings.HasPrefix(model, key+"-")) && len(key) > best {
			minimum, best = value, len(key)
		}
	}
	return minimum
}

func cacheValueIsDynamic(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			if cacheValueIsDynamic(child) {
				return true
			}
		}
	case map[string]any:
		if v["volatile"] == true || v["dynamic"] == true || v["cacheable"] == false {
			return true
		}
		kind, _ := v["type"].(string)
		switch kind {
		case "web_search_tool_result", "web_search_result", "search_result":
			return true
		case "server_tool_use", "tool_use", "function_call":
			if cacheDynamicToolName(cacheAsString(v["name"])) {
				return true
			}
		}
		for key, child := range v {
			if (key == "functionResponse" || key == "functionCall") && cacheDynamicToolName(cacheAsStringValue(child, "name")) {
				return true
			}
			// Tool schemas and arguments are opaque application data.
			if key == "block" || key == "part" || key == "value" || key == "content" || key == "parts" {
				if cacheValueIsDynamic(child) {
					return true
				}
			}
		}
	}
	return false
}

func cacheAsStringValue(value any, key string) string {
	if object, ok := value.(map[string]any); ok {
		return cacheAsString(object[key])
	}
	return ""
}

func cacheDynamicToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web_search", "web_search_preview", "google_search", "google_search_retrieval", "current_time", "get_current_time", "random":
		return true
	}
	return false
}

func excludeDynamicCacheTail(p *cacheProfile, c CacheStrategyConfig) {
	if p == nil || c.DynamicContentMode == CacheDynamicContentAllow {
		return
	}
	for i, block := range p.pendingBlocks {
		if cacheValueIsDynamic(block.value) {
			p.pendingBlocks = p.pendingBlocks[:i]
			return
		}
	}
}

func cacheValueIsToolResult(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			if cacheValueIsToolResult(child) {
				return true
			}
		}
	case map[string]any:
		if v["type"] == "tool_result" || v["type"] == "function_call_output" || v["functionResponse"] != nil {
			return true
		}
		for _, key := range []string{"block", "part", "value", "parts", "content"} {
			if cacheValueIsToolResult(v[key]) {
				return true
			}
		}
	}
	return false
}

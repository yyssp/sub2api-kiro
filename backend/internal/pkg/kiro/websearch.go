package kiro

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

const minimalWebSearchDescription = "Search the web for information. Use this tool again when the previous search results are insufficient or need refinement."
const remoteWebSearchDescription = "WebSearch looks up information outside the model's training data. Supports multiple queries to gather comprehensive information."

var cachedWebSearchDescription atomic.Value // stores string

type MCPRequest struct {
	ID      string `json:"id"`
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type MCPResponse struct {
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	} `json:"result,omitempty"`
	Error *struct {
		Code    *int    `json:"code,omitempty"`
		Message *string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

type WebSearchResults struct {
	Results []WebSearchResult `json:"results"`
	// ErrorCode 非空表示搜索执行失败（Anthropic web_search_tool_result_error 的 error_code）。
	// 失败要如实告诉模型和客户端，伪装成"无结果"会让模型据此编造结论。
	ErrorCode string `json:"-"`
}

// WebSearchErrorUnavailable 对应 Anthropic 协议的 error_code=unavailable。
const WebSearchErrorUnavailable = "unavailable"

// NewWebSearchErrorResults 构造一个表示搜索失败的结果。
func NewWebSearchErrorResults(code string) *WebSearchResults {
	return &WebSearchResults{ErrorCode: code}
}

func (r *WebSearchResults) failed() bool {
	return r != nil && r.ErrorCode != ""
}

// kiroWebSearchMaxQueryRunes 是发给上游 MCP 的 query 长度上限。
// 2ue_kiro.rs 实测约 560 字符时上游返回 invalid_tool_input。
const kiroWebSearchMaxQueryRunes = 200

type WebSearchResult struct {
	Title                string  `json:"title"`
	URL                  string  `json:"url"`
	Snippet              *string `json:"snippet,omitempty"`
	PublishedDate        *int64  `json:"publishedDate,omitempty"`
	ID                   *string `json:"id,omitempty"`
	Domain               *string `json:"domain,omitempty"`
	MaxVerbatimWordLimit *int    `json:"maxVerbatimWordLimit,omitempty"`
	PublicDomain         *bool   `json:"publicDomain,omitempty"`
}

type SearchIndicator struct {
	ToolUseID string
	Query     string
	Results   *WebSearchResults
}

func GetCachedWebSearchDescription() string {
	if v := cachedWebSearchDescription.Load(); v != nil {
		desc, _ := v.(string)
		return strings.TrimSpace(desc)
	}
	return ""
}

func SetCachedWebSearchDescription(desc string) {
	cachedWebSearchDescription.Store(strings.TrimSpace(desc))
}

func BuildMcpEndpoint(region string) string {
	if strings.TrimSpace(region) == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)
}

func ParseSearchResults(resp *MCPResponse) *WebSearchResults {
	if resp == nil || resp.Result == nil || len(resp.Result.Content) == 0 {
		return nil
	}
	for _, item := range resp.Result.Content {
		if item.Type != "" && item.Type != "text" {
			continue
		}
		var results WebSearchResults
		if err := json.Unmarshal([]byte(item.Text), &results); err == nil {
			return &results
		}
	}
	return nil
}

// ExtractSearchQuery 只从当前轮（末条 user 消息）提取搜索词。
//
// 不回溯更早的 user 消息：当前轮没有搜索词时复用历史 query，
// 会让本轮搜索的是上一个问题。
func ExtractSearchQuery(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	arr := messages.Array()
	if len(arr) == 0 {
		return ""
	}
	msg := arr[len(arr)-1]
	if msg.Get("role").String() != "user" {
		return ""
	}
	text := extractSearchText(msg.Get("content"))
	const prefix = "Perform a web search for the query: "
	text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
	return truncateSearchQuery(text)
}

func truncateSearchQuery(query string) string {
	runes := []rune(query)
	if len(runes) <= kiroWebSearchMaxQueryRunes {
		return query
	}
	return strings.TrimSpace(string(runes[:kiroWebSearchMaxQueryRunes]))
}

func extractSearchText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	for _, block := range content.Array() {
		if block.Get("type").String() == "text" {
			if text := strings.TrimSpace(block.Get("text").String()); text != "" {
				return text
			}
		}
	}
	return ""
}

func GenerateToolUseID() string {
	return "01" + randomBase62(22)
}

func ReplaceWebSearchToolDescription(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, err
	}
	rawTools, ok := payload["tools"].([]any)
	if !ok {
		return body, nil
	}

	replaced := make([]any, 0, len(rawTools))
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			replaced = append(replaced, rawTool)
			continue
		}
		name := getInterfaceString(tool["name"])
		toolType := getInterfaceString(tool["type"])
		if !isWebSearchToolName(name, toolType) {
			replaced = append(replaced, rawTool)
			continue
		}
		replaced = append(replaced, map[string]any{
			"name":        "web_search",
			"description": minimalWebSearchDescription,
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query to execute",
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		})
	}

	payload["tools"] = replaced
	updated, err := json.Marshal(payload)
	if err != nil {
		return body, err
	}
	return updated, nil
}

func InjectToolResultsClaude(claudePayload []byte, toolUseID, query string, results *WebSearchResults) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(claudePayload, &payload); err != nil {
		return claudePayload, fmt.Errorf("parse claude payload: %w", err)
	}

	rawMessages, ok := payload["messages"].([]any)
	if !ok {
		return claudePayload, fmt.Errorf("claude payload missing messages array")
	}

	assistantMsg := map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    toolUseID,
				"name":  "web_search",
				"input": map[string]any{"query": query},
			},
		},
	}

	userContent := []any{
		map[string]any{
			"type":        "tool_result",
			"tool_use_id": toolUseID,
			"content":     formatToolResultText(results),
		},
	}
	if guidance := searchGuidanceText(); guidance != "" {
		userContent = append(userContent, map[string]any{
			"type": "text",
			"text": guidance,
		})
	}
	userMsg := map[string]any{
		"role":    "user",
		"content": userContent,
	}

	rawMessages = append(rawMessages, assistantMsg, userMsg)
	payload["messages"] = rawMessages
	updated, err := json.Marshal(payload)
	if err != nil {
		return claudePayload, fmt.Errorf("marshal updated payload: %w", err)
	}
	return updated, nil
}

func InjectSearchIndicatorsInResponse(responsePayload []byte, searches []SearchIndicator) ([]byte, error) {
	if len(searches) == 0 {
		return responsePayload, nil
	}

	var response map[string]any
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		return responsePayload, err
	}
	content, _ := response["content"].([]any)
	updated := make([]any, 0, len(searches)*2+len(content))
	for _, search := range searches {
		updated = append(updated, map[string]any{
			"type":  "server_tool_use",
			"id":    search.ToolUseID,
			"name":  "web_search",
			"input": map[string]any{"query": search.Query},
		})
		updated = append(updated, map[string]any{
			"type":    "web_search_tool_result",
			"content": buildSearchResultContent(search.Results),
		})
	}
	updated = append(updated, content...)
	response["content"] = updated

	encoded, err := json.Marshal(response)
	if err != nil {
		return responsePayload, err
	}
	return encoded, nil
}

// buildSearchResultContent 生成 web_search_tool_result.content：
// 成功时为结果数组，失败时为 Anthropic 协议的 web_search_tool_result_error 对象。
func buildSearchResultContent(results *WebSearchResults) any {
	if results.failed() {
		return map[string]any{"type": "web_search_tool_result_error", "error_code": results.ErrorCode}
	}
	content := make([]map[string]any, 0)
	if results == nil {
		return content
	}
	for _, result := range results.Results {
		snippet := ""
		if result.Snippet != nil {
			snippet = strings.TrimSpace(*result.Snippet)
		}
		content = append(content, map[string]any{
			"type":              "web_search_result",
			"title":             result.Title,
			"url":               result.URL,
			"encrypted_content": snippet,
			"page_age":          nil,
		})
	}
	return content
}

func ExtractWebSearchToolUseFromResponse(responsePayload []byte) (toolUseID, query string, ok bool) {
	content := gjson.GetBytes(responsePayload, "content")
	if !content.IsArray() {
		return "", "", false
	}
	for _, block := range content.Array() {
		if block.Get("type").String() != "tool_use" {
			continue
		}
		name := block.Get("name").String()
		if !isWebSearchToolName(name, "") {
			continue
		}
		query = strings.TrimSpace(block.Get("input.query").String())
		if query == "" {
			continue
		}
		return block.Get("id").String(), query, true
	}
	return "", "", false
}

func isWebSearchToolName(name, toolType string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	toolType = strings.ToLower(strings.TrimSpace(toolType))
	if strings.HasPrefix(toolType, "web_search") || toolType == "google_search" {
		return true
	}
	switch name {
	case "web_search", "web_search_20250305", "google_search", "remote_web_search":
		return true
	default:
		return false
	}
}

func getInterfaceString(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	default:
		return strings.TrimSpace(fmt.Sprint(val))
	}
}

func formatToolResultText(results *WebSearchResults) string {
	if results.failed() {
		return "Web search failed: the search service is temporarily unavailable (" + results.ErrorCode + "). " +
			"No results were retrieved. Tell the user the search could not be completed instead of answering from assumed results."
	}
	if results == nil || len(results.Results) == 0 {
		return "No search results found."
	}
	payload, err := json.MarshalIndent(results.Results, "", "  ")
	if err != nil {
		return "Found search results, but failed to format them."
	}
	return fmt.Sprintf("Found %d search result(s):\n\n%s", len(results.Results), string(payload))
}

func searchGuidanceText() string {
	now := time.Now()
	return fmt.Sprintf(`<search_guidance>
Current date: %s (%s)

IMPORTANT: Evaluate the search results above carefully. If the results are:
- Mostly spam, SEO junk, or unrelated websites
- Missing actual information about the query topic
- Outdated or not matching the requested time frame

Then you MUST use the web_search tool again with a refined query. Try:
- Rephrasing in English for better coverage
- Using more specific keywords
- Adding date context

Do NOT apologize for bad results without first attempting a re-search.
</search_guidance>`, now.Format("January 2, 2006"), now.Format("Monday"))
}

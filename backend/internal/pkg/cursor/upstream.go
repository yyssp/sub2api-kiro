package cursor

import (
	"net/http"
	"sort"
	"strings"
	"sync"
)

const (
	cursorAPIBase = "https://api2.cursor.sh"
	cursorWebBase = "https://cursor.com"
)

var (
	upstreamMu       sync.RWMutex
	cursorProxyBase  = envOr("CURSOR_PROXY_BASE", "")
	cursorProxyToken = envOr("CURSOR_PROXY_TOKEN", "")
	sandModels       = map[string]struct{}{}
)

// ConfigureUpstream 配置 Cursor 官方请求的转换代理。
// proxyBase 为空时直连官方；非空时保持官方 path 不变，仅把 scheme/host 切到代理。
func ConfigureUpstream(proxyBase, proxyToken string) {
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	cursorProxyBase = strings.TrimRight(strings.TrimSpace(proxyBase), "/")
	cursorProxyToken = strings.TrimSpace(proxyToken)
}

// ConfigureSandModels configures additional explicit model IDs that must use
// Cursor's Sand request surface. Standard Claude Code models are handled by
// useSandInferenceService independently of this list because Cursor now
// rejects Sand traffic on AgentService/Run(SSE).
func ConfigureSandModels(models []string) {
	next := make(map[string]struct{}, len(models))
	for _, model := range models {
		if normalized := normalizeSurfaceModel(model); normalized != "" {
			next[normalized] = struct{}{}
		}
	}
	upstreamMu.Lock()
	sandModels = next
	upstreamMu.Unlock()
}

// SandModels returns a stable normalized copy for diagnostics and settings.
func SandModels() []string {
	upstreamMu.RLock()
	defer upstreamMu.RUnlock()
	models := make([]string, 0, len(sandModels))
	for model := range sandModels {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

// cursorClientTypeForModel maps a model to the Cursor quota surface.
func cursorClientTypeForModel(model string) string {
	model = normalizeSurfaceModel(model)
	upstreamMu.RLock()
	_, isSand := sandModels[model]
	upstreamMu.RUnlock()
	if isSand {
		return "sand"
	}
	// Claude Code 的标准别名必须独立于动态 AvailableModels 缓存判定：
	// Cursor 当前只接受这些模型走 InferenceService/Stream 的 Sand 面。
	if _, ok := ResolveClaudeCodeModel(model); ok || isClaudeCodeModelName(model) {
		return "sand"
	}
	return "cli"
}

// agentServiceClientTypeForModel returns the client identity required by
// Cursor's AgentService endpoints. It is intentionally separate from
// cursorClientTypeForModel because AgentService rejects sand traffic.
func agentServiceClientTypeForModel(model string) string {
	_ = model
	return "cli"
}

// useSandInferenceService reports whether a request must use Cursor's direct
// Sand InferenceService/Stream endpoint.
func useSandInferenceService(model string) bool {
	model = normalizeSurfaceModel(model)
	upstreamMu.RLock()
	_, isSand := sandModels[model]
	upstreamMu.RUnlock()
	if isSand {
		return true
	}
	if _, ok := ResolveClaudeCodeModel(model); ok || isClaudeCodeModelName(model) {
		return true
	}
	return isSand
}

// useSandInferenceServiceForRequest keeps the quota surface separate from the
// request transport. Claude Code text-only requests can use direct Sand, but
// Claude Code tool/MCP/Agent requests must stay on AgentService/RunSSE because
// the Sand InferenceService schema is not an Anthropic tool declaration surface.
func useSandInferenceServiceForRequest(model string, tools []ToolDef) bool {
	if !useSandInferenceService(model) {
		return false
	}
	if len(tools) == 0 {
		return true
	}
	model = normalizeSurfaceModel(model)
	if _, ok := ResolveClaudeCodeModel(model); ok || isClaudeCodeModelName(model) {
		return false
	}
	return true
}

func normalizeSurfaceModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.TrimPrefix(model, "cursor/")
}

func CursorProxyBase() string {
	upstreamMu.RLock()
	defer upstreamMu.RUnlock()
	return cursorProxyBase
}

func CursorProxyTokenSet() bool {
	upstreamMu.RLock()
	defer upstreamMu.RUnlock()
	return cursorProxyToken != ""
}

func cursorAPIURL(path string) string {
	return cursorUpstreamURL(cursorAPIBase, path)
}

func cursorAgentURL(path string) string {
	return cursorUpstreamURL(agentBaseURL, path)
}

func cursorWebURL(path string) string {
	return cursorUpstreamURL(cursorWebBase, path)
}

func cursorUpstreamURL(officialBase, path string) string {
	upstreamMu.RLock()
	proxyBase := cursorProxyBase
	upstreamMu.RUnlock()
	if proxyBase != "" {
		return joinBasePath(proxyBase, path)
	}
	return joinBasePath(officialBase, path)
}

func joinBasePath(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	path = strings.TrimSpace(path)
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func applyCursorProxyAuth(req *http.Request) {
	upstreamMu.RLock()
	token := cursorProxyToken
	proxyBase := cursorProxyBase
	upstreamMu.RUnlock()
	if proxyBase == "" || token == "" {
		return
	}
	req.Header.Set("X-Cursor-Proxy-Token", token)
}

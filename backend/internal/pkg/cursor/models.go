package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// ModelParameterValue 是 Cursor requested_model 中的一项已解析参数。
// 参数保留原始字符串值；Cursor 会按模型目录定义校验 value。
type ModelParameterValue struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// ModelParameterDefinition 是 Cursor 模型目录声明的一个参数及其可选值。
type ModelParameterDefinition struct {
	ID     string   `json:"id"`
	Values []string `json:"values"`
}

// ModelParameterDefaults 保存模型目录提供的非 Max/Max 默认参数。
type ModelParameterDefaults struct {
	NonMax []ModelParameterValue `json:"nonMax,omitempty"`
	Max    []ModelParameterValue `json:"max,omitempty"`
}

// ModelVariant 是 Cursor 模型目录中的 legacy slug 变体。
type ModelVariant struct {
	Slug       string                `json:"slug"`
	MaxMode    bool                  `json:"maxMode,omitempty"`
	Parameters []ModelParameterValue `json:"parameters,omitempty"`
}

// ModelMeta 模型元信息(供管理台模型页展示 / 路由判定 / Sand requested_model)
type ModelMeta struct {
	ID      string `json:"id"`
	Family  string `json:"family"`
	Vision  bool   `json:"vision"`
	Tools   bool   `json:"tools"`
	Think   bool   `json:"think"`
	MaxMode bool   `json:"maxMode"`
	// 以下字段来自 Cursor AvailableModels 的模型目录；旧缓存没有这些字段时
	// 保持 nil，Sand 层再按 legacy slug 做兼容推导。
	Aliases    []string                   `json:"aliases,omitempty"`
	Parameters []ModelParameterDefinition `json:"parameters,omitempty"`
	Defaults   ModelParameterDefaults     `json:"defaults,omitempty"`
	Variants   []ModelVariant             `json:"variants,omitempty"`
}

// DefaultModels 静态兜底清单(仅在动态拉取失败/号池为空时使用)。
// 真实清单以账号 AvailableModels 动态拉取为准(见 liveModels / RefreshModelsFrom)。
var DefaultModels = []ModelMeta{
	{ID: "default", Family: "auto", Vision: true, Tools: true, MaxMode: true},
	// Cursor 自研
	{ID: "composer-2.5", Family: "composer", Think: true, MaxMode: true},
	{ID: "composer-2", Family: "composer", Think: true, MaxMode: true},
	// Anthropic 全阵容(官方多数默认隐藏, 动态清单常缺)
	{ID: "claude-opus-5-thinking-high", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-opus-5", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-sonnet-5-thinking-high", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-sonnet-5", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-fable-5-thinking-high", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-fable-5", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-opus-4.8", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-opus-4.7", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-opus-4.6", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-sonnet-4.6", Family: "claude", Vision: true, Think: true, MaxMode: true},
	{ID: "claude-4.5-sonnet", Family: "claude", Vision: true, MaxMode: true},
	{ID: "claude-4.5-sonnet-thinking", Family: "claude", Vision: true, MaxMode: true},
	{ID: "claude-4.5-haiku", Family: "claude", Vision: true, MaxMode: false},
	{ID: "claude-4.5-haiku-thinking", Family: "claude", Vision: true, Think: true, MaxMode: false},
	{ID: "claude-fable-5-1", Family: "claude", Vision: true, Think: true, MaxMode: true},
	// OpenAI
	{ID: "gpt-5.6-terra-high", Family: "gpt", Vision: true, Think: true, MaxMode: true},
	{ID: "gpt-5.6-sol-high", Family: "gpt", Vision: true, Think: true, MaxMode: true},
	{ID: "gpt-5.5", Family: "gpt", Vision: true, Think: true, MaxMode: true},
	{ID: "gpt-5.5-codex", Family: "gpt", Vision: true, Think: true, MaxMode: true},
	{ID: "gpt-5.3-codex", Family: "gpt", Vision: true, Think: true, MaxMode: true},
	// Google
	{ID: "gemini-3.7-flash-high", Family: "gemini", Vision: true, Think: true, MaxMode: true},
	{ID: "gemini-3.1-pro", Family: "gemini", Vision: true, Think: true, MaxMode: true},
	{ID: "gemini-3-flash", Family: "gemini", Vision: true, Think: true, MaxMode: true},
	// xAI / 其他
	{ID: "cursor-grok-4.6-high", Family: "grok", Think: true, MaxMode: true},
	{ID: "grok-4.5", Family: "grok", Think: true, MaxMode: true},
	{ID: "kimi-k3-high", Family: "kimi", Think: true, MaxMode: true},
	{ID: "glm-5.2-high", Family: "glm", Think: true, MaxMode: true},
}

// liveModels 由账号 AvailableModels 动态拉取的完整清单(空则回落 DefaultModels)。
var (
	modelMu    sync.RWMutex
	liveModels []ModelMeta
	liveSet    map[string]bool
)

// SetLiveModels 更新动态模型清单缓存(同时重建路由判定集合)。开启持久化后同步原子落盘。
func SetLiveModels(ms []ModelMeta) {
	set := make(map[string]bool, len(ms))
	for _, m := range ms {
		set[strings.ToLower(m.ID)] = true
	}
	modelMu.Lock()
	liveModels = ms
	liveSet = set
	path := livePath
	modelMu.Unlock()
	if path != "" {
		if b, err := json.Marshal(ms); err == nil {
			tmp := path + ".tmp"
			if os.WriteFile(tmp, b, 0600) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
}

// livePath 动态清单持久化路径(空=不持久化)
var livePath string

// EnableModelsPersistence 开启动态模型清单落盘: 进程重启(每次部署)后恢复最近一次成功拉取的
// 全量清单(实测 207 个), 避免账号全失效/重启窗口期管理台退化成静态兜底小清单。
func EnableModelsPersistence(path string) {
	modelMu.Lock()
	livePath = path
	modelMu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var ms []ModelMeta
	if json.Unmarshal(b, &ms) != nil || len(ms) == 0 {
		return
	}
	set := make(map[string]bool, len(ms))
	for _, m := range ms {
		set[strings.ToLower(m.ID)] = true
	}
	modelMu.Lock()
	liveModels = ms
	liveSet = set
	modelMu.Unlock()
	log.Printf("[Cursor] 恢复动态模型清单 %d 个 <- %s", len(ms), path)
}

// currentModels 返回用于兼容路由/旧接口的当前清单。它采用动态 ∪ 兜底
// (同 ID 以动态元信息为准, 兜底仅补缺)，不能作为管理台“上游真实模型列表”的数据源。
// 管理台必须调用 liveModelsSnapshot，避免把静态兼容项误显示成 Cursor 当前可用模型。
// 注意: 路由判定(inModelSet/normalizeCursorModel)仍只看纯动态 liveSet, 不受兜底扩充污染。
func currentModels() []ModelMeta {
	modelMu.RLock()
	defer modelMu.RUnlock()
	if len(liveModels) == 0 {
		return DefaultModels
	}
	seen := make(map[string]bool, len(liveModels))
	out := make([]ModelMeta, 0, len(liveModels)+8)
	for _, m := range liveModels {
		out = append(out, m)
		seen[strings.ToLower(m.ID)] = true
	}
	for _, m := range DefaultModels {
		if !seen[strings.ToLower(m.ID)] {
			out = append(out, m)
		}
	}
	return out
}

// LiveModelCount 当前动态清单数量(0 表示尚未拉取, 用兜底)。
func LiveModelCount() int {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return len(liveModels)
}

// inModelSet 判断模型名(小写)是否在当前生效清单中。
func inModelSet(m string) bool {
	modelMu.RLock()
	if liveSet != nil {
		ok := liveSet[m]
		modelMu.RUnlock()
		return ok
	}
	modelMu.RUnlock()
	for _, x := range DefaultModels {
		if strings.ToLower(x.ID) == m {
			return true
		}
	}
	return false
}

// familyOf 由模型名推断家族(供元信息与路由)。
func familyOf(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "grok"):
		return "grok"
	case strings.Contains(n, "composer"):
		return "composer"
	case strings.Contains(n, "gemini"):
		return "gemini"
	case strings.Contains(n, "kimi"):
		return "kimi"
	case strings.Contains(n, "glm"):
		return "glm"
	case strings.Contains(n, "codex"), strings.HasPrefix(n, "gpt"), strings.HasPrefix(n, "o1"),
		strings.HasPrefix(n, "o3"), strings.HasPrefix(n, "o4"):
		return "gpt"
	case strings.Contains(n, "claude"), strings.Contains(n, "opus"), strings.Contains(n, "sonnet"),
		strings.Contains(n, "haiku"), strings.Contains(n, "fable"):
		return "claude"
	case n == "default" || n == "auto":
		return "auto"
	}
	return "other"
}

// IsCursorModel 判断模型名是否属于 Cursor 池(供按模型名路由的兜底)。
// 规则: cursor/ 前缀; 精确命中当前清单(动态207个); 或 Cursor 独有家族前缀。
// 注意 claude/glm 与 Qoder 存在重名, 故这两类仅在带 cursor/ 前缀或精确命中动态清单时才判为 Cursor。
func IsCursorModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(m, "cursor/") || strings.HasPrefix(m, "cursor-") {
		return true
	}
	if _, ok := ResolveClaudeCodeModel(model); ok || isClaudeCodeModelName(model) {
		return true
	}
	if inModelSet(m) {
		return true
	}
	// Cursor 独有/低冲突家族前缀(claude/glm 不在此列, 避免抢 Qoder 的同名模型)
	for _, p := range []string{"composer", "gpt-", "gpt5", "gpt-5", "o1", "o3", "o4", "gemini", "grok", "kimi"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// normalizeCursorModel 将裸模型名(缺 reasoning 后缀, 如 "gpt-5.6-luna")归一到动态清单中的有效变体,
// 优先 medium/default/high。已在清单中或找不到任何变体则原样返回。解决 Codex++ 等客户端用裸名导致的
// ERROR_BAD_MODEL_NAME(继而被网关误当可重试而反复换号刷屏)。
func normalizeCursorModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	// ⚠️ 这里曾把 "auto" 翻译成 Cursor 协议的 "default" 再发上游。已移除。
	//
	// 原因不是翻译写错了，而是**服务端选路的模型不可计费**：Cursor 的
	// agent.v1 响应信封(AgentServerMessage/InteractionUpdate)没有任何 model
	// 字段，上游不会告诉我们 auto 最终选了谁；响应流也不带 usage。于是按真实
	// 模型计价这条路在协议层就不存在，只能整体按最贵模型兜底(见
	// gateway_usage_billing.go 的 cursorConservativeFallbackBillingModel)。
	//
	// 因此网关一律要求显式模型：auto/default/composer 这类 Cursor 服务端选路
	// 别名不再由我们代为构造，由 ValidateDownstreamModel 在进协议层之前拒绝。
	if m == "" || inModelSet(m) {
		return model
	}
	modelMu.RLock()
	defer modelMu.RUnlock()
	if liveSet == nil {
		return model
	}
	for _, suf := range []string{"-medium", "-default", "-high", "-medium-fast", "-none", "-low", "-max"} {
		if liveSet[m+suf] {
			return m + suf
		}
	}
	for id := range liveSet {
		if strings.HasPrefix(id, m+"-") {
			return id
		}
	}
	return model
}

// claudeCodeModelSpec 描述 Claude Code 对外可见的标准名称及其可能对应的
// Cursor 上游模型前缀。Cursor 的 AvailableModels 会返回带能力/速度后缀的
// 内部 ID，因此解析时必须从当前动态清单中选择真实存在的完整 ID。
type claudeCodeModelSpec struct {
	ID       string
	Alias    []string
	Bases    []string
	Fallback string
}

// 顺序同时决定 /models 的稳定展示顺序和无版本 alias 的选择优先级。
// 仅将当前清单中能解析到真实上游 ID 的标准项对外暴露。
var claudeCodeModelSpecs = []claudeCodeModelSpec{
	{ID: "opus", Bases: []string{"claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-4-6-opus", "claude-4-5-opus"}, Fallback: "claude-opus-5"},
	{ID: "sonnet", Bases: []string{"claude-sonnet-5", "claude-4-6-sonnet", "claude-4-5-sonnet", "claude-4-sonnet"}, Fallback: "claude-sonnet-5"},
	{ID: "haiku", Bases: []string{"claude-4-5-haiku"}, Fallback: "claude-haiku-4-5"},
	{ID: "fable", Bases: []string{"claude-fable-5"}, Fallback: "claude-fable-5"},
	{ID: "opusplan", Bases: []string{"claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-4-6-opus", "claude-4-5-opus"}, Fallback: "claude-opus-5"},
	{ID: "claude-opus-5", Bases: []string{"claude-opus-5"}, Fallback: "claude-opus-5"},
	{ID: "claude-opus-4-8", Bases: []string{"claude-opus-4-8"}, Fallback: "claude-opus-4-8"},
	{ID: "claude-opus-4-7", Bases: []string{"claude-opus-4-7"}, Fallback: "claude-opus-4-7"},
	{ID: "claude-opus-4-6", Bases: []string{"claude-4-6-opus"}, Fallback: "claude-opus-4-6"},
	{ID: "claude-opus-4-5", Bases: []string{"claude-4-5-opus"}, Fallback: "claude-opus-4-5"},
	{ID: "claude-opus-4-1", Bases: []string{"claude-opus-4-1"}, Fallback: "claude-opus-4-1"},
	{ID: "claude-opus-4-0", Bases: []string{"claude-opus-4-0", "claude-4-opus"}, Fallback: "claude-opus-4-0"},
	{ID: "claude-sonnet-5", Bases: []string{"claude-sonnet-5"}, Fallback: "claude-sonnet-5"},
	{ID: "claude-sonnet-4-6", Bases: []string{"claude-4-6-sonnet"}, Fallback: "claude-sonnet-4-6"},
	{ID: "claude-sonnet-4-5", Bases: []string{"claude-4-5-sonnet"}, Fallback: "claude-sonnet-4-5"},
	{ID: "claude-sonnet-4", Bases: []string{"claude-sonnet-4", "claude-4-sonnet"}, Fallback: "claude-sonnet-4"},
	{ID: "claude-sonnet-4-0", Bases: []string{"claude-4-sonnet"}, Fallback: "claude-sonnet-4-0"},
	{ID: "claude-haiku-4-5", Bases: []string{"claude-4-5-haiku"}, Fallback: "claude-haiku-4-5"},
	{ID: "claude-fable-5-1", Alias: []string{"fable-5-1"}, Bases: []string{"claude-fable-5-1"}, Fallback: "claude-fable-5-1"},
	{ID: "claude-fable-5", Bases: []string{"claude-fable-5"}, Fallback: "claude-fable-5"},
}

func normalizeClaudeModelKey(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	// Standard Claude IDs are commonly written with either 4.6 or 4-6.
	// Only the Claude resolver applies this normalization; non-Claude Cursor IDs
	// continue through their original normalization path.
	return strings.ReplaceAll(m, ".", "-")
}

// trimClaudeReleaseDate removes Anthropic's canonical YYYYMMDD release suffix
// only while matching a public Claude model specification. Exact dynamic Cursor
// IDs are resolved before this helper runs, so a future Cursor ID with a release
// date remains usable as-is.
func trimClaudeReleaseDate(key string) string {
	if !strings.HasPrefix(key, "claude-") {
		return key
	}
	lastDash := strings.LastIndex(key, "-")
	if lastDash < 0 || len(key)-lastDash-1 != 8 {
		return key
	}
	for _, c := range key[lastDash+1:] {
		if c < '0' || c > '9' {
			return key
		}
	}
	return key[:lastDash]
}

func claudeResolutionModels() []ModelMeta {
	modelMu.RLock()
	defer modelMu.RUnlock()
	if len(liveModels) > 0 {
		out := make([]ModelMeta, len(liveModels))
		copy(out, liveModels)
		return out
	}
	out := make([]ModelMeta, len(DefaultModels))
	copy(out, DefaultModels)
	return out
}

func claudeModelIndex() map[string]string {
	index := make(map[string]string)
	for _, m := range claudeResolutionModels() {
		if familyOf(m.ID) != "claude" {
			continue
		}
		index[strings.ToLower(m.ID)] = m.ID
		index[normalizeClaudeModelKey(m.ID)] = m.ID
	}
	return index
}

func claudeSpecForKey(key string) (claudeCodeModelSpec, string, string, bool) {
	for _, spec := range claudeCodeModelSpecs {
		if key == spec.ID {
			return spec, "", "", true
		}
		for _, alias := range spec.Alias {
			if key == alias {
				return spec, "", "", true
			}
		}
		for _, base := range spec.Bases {
			if key == base {
				return spec, base, "", true
			}
			if strings.HasPrefix(key, base+"-") {
				return spec, base, strings.TrimPrefix(key, base+"-"), true
			}
		}
		// Standard versioned IDs (for example claude-sonnet-4-6-thinking)
		// use the public ID itself as the base, while the upstream base may
		// use the legacy claude-4.6-sonnet spelling.
		if strings.HasPrefix(key, spec.ID+"-") {
			return spec, "", strings.TrimPrefix(key, spec.ID+"-"), true
		}
		for _, alias := range spec.Alias {
			if strings.HasPrefix(key, alias+"-") {
				return spec, "", strings.TrimPrefix(key, alias+"-"), true
			}
		}
	}
	return claudeCodeModelSpec{}, "", "", false
}

// isClaudeCodeModelName reports whether a model name belongs to the public
// Claude Code compatibility surface, independently of the last dynamic
// AvailableModels response. This distinction matters during startup and when
// an account-specific model list temporarily omits Claude variants: the
// request must still use Sand's InferenceService surface, while resolution to
// an exact upstream ID remains subject to the live list.
func isClaudeCodeModelName(model string) bool {
	raw := strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(raw), "cursor/") {
		raw = strings.TrimSpace(raw[len("cursor/"):])
	}
	if raw == "" {
		return false
	}
	key := trimClaudeReleaseDate(normalizeClaudeModelKey(raw))
	_, _, _, ok := claudeSpecForKey(key)
	return ok
}

func claudeVariantSuffixes(suffix string) []string {
	suffix = strings.Trim(strings.TrimSpace(suffix), "-")
	if suffix == "" {
		return nil
	}
	out := []string{suffix}
	add := func(s string) {
		if s == "" {
			return
		}
		for _, existing := range out {
			if existing == s {
				return
			}
		}
		out = append(out, s)
	}
	// Older Cursor IDs put "thinking" at the end (high-thinking), while newer
	// IDs put it first (thinking-high). Accept both spellings.
	if strings.HasPrefix(suffix, "thinking-") {
		add(strings.TrimPrefix(suffix, "thinking-") + "-thinking")
	} else if strings.HasSuffix(suffix, "-thinking") {
		add("thinking-" + strings.TrimSuffix(suffix, "-thinking"))
	} else if suffix == "thinking" {
		add("medium-thinking")
		add("high-thinking")
		add("max-thinking")
		add("thinking-medium")
		add("thinking-high")
		add("thinking-max")
	}
	if strings.HasSuffix(suffix, "-fast") {
		add(strings.TrimSuffix(suffix, "-fast"))
	} else {
		add(suffix + "-fast")
	}
	return out
}

func claudeCandidateIDs(base, suffix string) []string {
	bases := []string{base}
	// Dynamic Cursor IDs for older versions use claude-4.6-opus while the
	// public Claude spelling is claude-opus-4-6.
	if strings.HasPrefix(base, "claude-opus-4-") {
		version := strings.TrimPrefix(base, "claude-opus-")
		bases = append(bases, "claude-"+version+"-opus")
	}
	if strings.HasPrefix(base, "claude-sonnet-4-") {
		version := strings.TrimPrefix(base, "claude-sonnet-")
		bases = append(bases, "claude-"+version+"-sonnet")
	}
	if strings.HasPrefix(base, "claude-haiku-") {
		version := strings.TrimPrefix(base, "claude-haiku-")
		bases = append(bases, "claude-"+version+"-haiku")
	}
	out := make([]string, 0, len(bases)*12)
	add := func(s string) {
		for _, existing := range out {
			if existing == s {
				return
			}
		}
		out = append(out, s)
	}
	suffixes := claudeVariantSuffixes(suffix)
	for _, b := range bases {
		if suffix == "" {
			add(b)
		} else {
			for _, s := range suffixes {
				add(b + "-" + s)
			}
		}
		// A bare standard ID should use a stable non-thinking variant. If the
		// requested tier is unavailable, fall through to other valid tiers.
		defaultSuffixes := []string{"medium", "high", "low", "xhigh", "max", "none", "thinking-high", "high-thinking"}
		if strings.Contains(suffix, "thinking") {
			defaultSuffixes = []string{"thinking-high", "thinking-medium", "thinking-low", "thinking-xhigh", "thinking-max", "high-thinking", "max-thinking"}
		}
		for _, s := range defaultSuffixes {
			add(b + "-" + s)
		}
	}
	return out
}

// ErrServerSideRoutedModel 表示下游显式请求了 Cursor 的服务端选路别名
// (auto/default/composer)。这类模型由 Cursor 服务端挑选真实模型，而 agent.v1
// 响应里不含 model 字段，网关无法得知实际服务方，因此无法按真实模型计费。
var ErrServerSideRoutedModel = errors.New("cursor: server-side routed model is not billable; request an explicit model")

// ErrUnsupportedDownstreamModel 表示模型名无法解析为任何标准 Claude 协议模型。
// 含 Cursor 自有模型名(composer-2.5)与第三方名(gpt-5)：对下游暴露的必须是各场景
// 的标准协议模型，Cursor 协议内部名不构成公开契约。
var ErrUnsupportedDownstreamModel = errors.New("cursor: unsupported model")

// ValidateDownstreamModel 在进入协议层之前校验下游请求的模型名。
//
// 必须显式传模型：
//   - 空模型不再默认成 Cursor 的 "default"(原 agent.go 的兜底)，否则用户什么都
//     不传就会静默走到服务端选路，落到最贵模型的保守计费上。
//   - auto/default/composer 一律拒绝，理由见 ErrServerSideRoutedModel。
//   - 只有能解析到标准 Claude 模型的名字放行；Cursor 协议内部名不对外暴露。
func ValidateDownstreamModel(model string) error {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return fmt.Errorf("%w: model is required", ErrUnsupportedDownstreamModel)
	}
	if strings.HasPrefix(strings.ToLower(raw), "cursor/") {
		raw = strings.TrimSpace(raw[len("cursor/"):])
	}
	if isServerSideRoutedModelAlias(raw) {
		return fmt.Errorf("%w: %q", ErrServerSideRoutedModel, model)
	}
	if _, ok := ResolveClaudeCodeModel(raw); ok {
		return nil
	}
	if _, ok := fallbackClaudeCodeModel(raw); ok {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnsupportedDownstreamModel, model)
}

// isServerSideRoutedModelAlias 判定 Cursor 的服务端选路别名。
// 与 gateway_usage_billing.go 的 cursorUnpriceableModelAlias 是同一组语义：
// 那边是「万一漏到计费阶段则按最贵模型兜底」，这边是「在入口就挡住」。
func isServerSideRoutedModelAlias(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "auto" || m == "default" {
		return true
	}
	// composer 系整体是 Cursor 自研的服务端选路面，含 composer-2.5 等变体。
	return strings.HasPrefix(m, "composer")
}

// ResolveClaudeCodeModel 将 Claude Code 标准模型名解析为当前动态 Cursor
// 清单中的真实上游 ID。输入可带 cursor/ 前缀，也兼容旧 Cursor 变体。
func ResolveClaudeCodeModel(model string) (string, bool) {
	raw := strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(raw), "cursor/") {
		raw = strings.TrimSpace(raw[len("cursor/"):])
	}
	if raw == "" {
		return "", false
	}
	index := claudeModelIndex()
	if id, ok := index[strings.ToLower(raw)]; ok {
		return id, true
	}
	key := normalizeClaudeModelKey(raw)
	if id, ok := index[key]; ok {
		return id, true
	}
	spec, matchedBase, suffix, ok := claudeSpecForKey(trimClaudeReleaseDate(key))
	if !ok {
		return "", false
	}
	bases := spec.Bases
	if matchedBase != "" {
		bases = []string{matchedBase}
	}
	for _, base := range bases {
		for _, candidate := range claudeCandidateIDs(base, suffix) {
			if id, exists := index[candidate]; exists {
				return id, true
			}
		}
	}
	return "", false
}

// fallbackClaudeCodeModel returns a stable public Claude model ID for the
// short window before a dynamic Cursor model list is available. It is only a
// transport fallback; callers still use ResolveClaudeCodeModel whenever an
// exact live ID exists.
func fallbackClaudeCodeModel(model string) (string, bool) {
	raw := strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(raw), "cursor/") {
		raw = strings.TrimSpace(raw[len("cursor/"):])
	}
	key := trimClaudeReleaseDate(normalizeClaudeModelKey(raw))
	spec, matchedBase, _, ok := claudeSpecForKey(key)
	if !ok {
		return "", false
	}
	if matchedBase != "" {
		// Preserve an explicitly requested legacy spelling/version when no
		// dynamic list can tell us which spelling this account accepts.
		return matchedBase, true
	}
	if spec.Fallback != "" {
		return spec.Fallback, true
	}
	if len(spec.Bases) > 0 {
		return spec.Bases[0], true
	}
	return "", false
}

func normalizeClaudeEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return ""
	}
}

// applyClaudeEffortModel maps Anthropic's effort tier to an exact thinking
// variant from the live Cursor model list. It deliberately does not choose a
// nearby tier: silently downgrading or upgrading effort would change semantics.
func applyClaudeEffortModel(model, effort string) string {
	effort = normalizeClaudeEffort(effort)
	if effort == "" || familyOf(model) != "claude" {
		return model
	}
	if meta, ok := claudeModelMeta(model); ok && meta.Think {
		// An explicitly selected thinking model always wins over output_config.
		return model
	}
	key := normalizeClaudeModelKey(model)
	spec, matchedBase, _, ok := claudeSpecForKey(key)
	if !ok {
		return model
	}
	if matchedBase == "" {
		for _, base := range spec.Bases {
			baseKey := normalizeClaudeModelKey(base)
			if key == baseKey || strings.HasPrefix(key, baseKey+"-") {
				matchedBase = base
				break
			}
		}
	}
	if matchedBase == "" {
		return model
	}

	var best ModelMeta
	bestSet := false
	for _, candidate := range claudeResolutionModels() {
		if familyOf(candidate.ID) != "claude" || !candidate.Think ||
			!claudeModelMatchesBase(candidate.ID, matchedBase) ||
			claudeThinkingTier(candidate.ID) != effort {
			continue
		}
		// Prefer the non-fast ID when both aliases are available; both are
		// otherwise valid live Cursor IDs.
		if !bestSet || (strings.HasSuffix(strings.ToLower(best.ID), "-fast") &&
			!strings.HasSuffix(strings.ToLower(candidate.ID), "-fast")) {
			best, bestSet = candidate, true
		}
	}
	if bestSet {
		return best.ID
	}
	return model
}

func claudeModelMeta(model string) (ModelMeta, bool) {
	want := strings.ToLower(strings.TrimSpace(model))
	for _, candidate := range claudeResolutionModels() {
		if strings.ToLower(strings.TrimSpace(candidate.ID)) == want {
			return candidate, true
		}
	}
	return ModelMeta{}, false
}

func claudeModelMatchesBase(model, base string) bool {
	modelKey := normalizeClaudeModelKey(model)
	baseKey := normalizeClaudeModelKey(base)
	if modelKey == baseKey {
		return false
	}

	// A base such as claude-fable-5 must match only its capability variants
	// (for example -thinking-high), not a newer release that merely shares the
	// textual prefix (for example claude-fable-5-1-thinking-high).
	baseSpellings := []string{baseKey}
	if strings.HasPrefix(baseKey, "claude-opus-4-") {
		version := strings.TrimPrefix(baseKey, "claude-opus-")
		baseSpellings = append(baseSpellings, "claude-"+version+"-opus")
	}
	if strings.HasPrefix(baseKey, "claude-sonnet-4-") {
		version := strings.TrimPrefix(baseKey, "claude-sonnet-")
		baseSpellings = append(baseSpellings, "claude-"+version+"-sonnet")
	}
	if strings.HasPrefix(baseKey, "claude-haiku-") {
		version := strings.TrimPrefix(baseKey, "claude-haiku-")
		baseSpellings = append(baseSpellings, "claude-"+version+"-haiku")
	}

	for _, spelling := range baseSpellings {
		prefix := spelling + "-"
		if !strings.HasPrefix(modelKey, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(modelKey, prefix)
		suffix = strings.TrimSuffix(suffix, "-fast")
		switch suffix {
		case "low", "medium", "high", "xhigh", "max", "none",
			"thinking", "thinking-low", "thinking-medium", "thinking-high",
			"thinking-xhigh", "thinking-max", "low-thinking",
			"medium-thinking", "high-thinking", "xhigh-thinking", "max-thinking":
			return true
		}
	}
	return false
}

func claudeThinkingTier(model string) string {
	key := strings.TrimSuffix(normalizeClaudeModelKey(model), "-fast")
	for _, tier := range []string{"low", "medium", "high", "xhigh", "max"} {
		if strings.HasSuffix(key, "-thinking-"+tier) || strings.HasSuffix(key, "-"+tier+"-thinking") {
			return tier
		}
	}
	return ""
}

// ClaudeCodeModelIDs 返回对外暴露给 Claude Code CLI 的标准模型名。
// 只暴露 claude- 前缀的完整标准 ID(如 claude-opus-5)；opus/sonnet/opusplan
// 等短别名仅保留调用时的解析能力(ResolveClaudeCodeModel), 不进对外列表。
// 非 Claude Cursor 模型仍可通过旧的显式 ID 调用，但不会污染 Claude Code
// 的标准模型列表；只有能解析到当前清单真实 ID 的项才会返回。
func ClaudeCodeModelIDs() []string {
	index := claudeModelIndex()
	out := make([]string, 0, len(claudeCodeModelSpecs))
	seen := make(map[string]bool)
	for _, spec := range claudeCodeModelSpecs {
		if !strings.HasPrefix(spec.ID, "claude-") {
			continue
		}
		resolved := false
		for _, base := range spec.Bases {
			for _, candidate := range claudeCandidateIDs(base, "") {
				if _, ok := index[candidate]; ok {
					resolved = true
					break
				}
			}
			if resolved {
				break
			}
		}
		if resolved && !seen[spec.ID] {
			out = append(out, spec.ID)
			seen[spec.ID] = true
		}
	}
	return out
}

//nolint:unused // retained for compatibility with the former model display layer.
var familyVendor = map[string]string{
	"claude": "Anthropic", "gpt": "OpenAI", "gemini": "Google",
	"grok": "xAI", "kimi": "Moonshot", "glm": "Zhipu",
	"composer": "Cursor", "auto": "Cursor",
}

// ModelIDs 返回当前 Cursor 兼容清单 ID，供需要兼容旧 Cursor 原生 ID 的内部调用；
// 对外 Claude Code 主列表必须使用 ClaudeCodeModelIDs。
func ModelIDs() []string {
	ms := currentModels()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// liveModelsSnapshot 返回最近一次成功从 Cursor AvailableModels 拉取的原始清单。
// 管理台模型页必须使用这份清单，不能使用 ClaudeCodeModelIDs 或 currentModels：
// 前者是对外 Claude Code 协议列表，后者包含静态兜底，两者都不代表 Cursor
// 上游当前实际返回的完整模型集合。
func liveModelsSnapshot() []ModelMeta {
	modelMu.RLock()
	defer modelMu.RUnlock()
	out := make([]ModelMeta, len(liveModels))
	copy(out, liveModels)
	return out
}

// modelQuotaBucket 返回模型默认请求所消耗的三类 Cursor 额度桶。
// 该函数复用真实请求记账使用的 usageBucketForModel，管理台展示与运行时
// 的归属保持一致；Claude Code 带工具请求的例外由 toolQuotaBucketForModel
// 单独标注。
//
//nolint:unused // retained for compatibility with the former model display layer.
func modelQuotaBucket(model string) string {
	bucket := usageBucketForModel(model)
	switch bucket {
	case "cursor", "other", "grokbot":
		return bucket
	default:
		// 动态清单中的未知模型统一归入 Other，避免前端把未知模型
		// 误显示为 Cursor/GrokBot 专属额度；空模型不会正常出现在清单中。
		return "other"
	}
}

//nolint:unused // retained for compatibility with the former model display layer.
func modelQuotaLabel(bucket string) string {
	switch bucket {
	case "cursor":
		return "Cursor Models"
	case "grokbot":
		return "GrokBot"
	case "other":
		return "Other Models"
	default:
		return "Other Models"
	}
}

// modelQuotaBuckets 返回一个模型在实际请求路径上可能使用的额度集合。
// 默认请求使用 modelQuotaBucket；Claude Code 声明工具/MCP/Agent 后改走
// AgentService，对应 Other Models。这里不把“命名模型不可用时降级到 Auto”
// 算作同一模型的额度，因为那已经是另一个实际模型 ID。
//
//nolint:unused // retained for compatibility with the former model display layer.
func modelQuotaBuckets(model string) []string {
	primary := modelQuotaBucket(model)
	out := []string{primary}
	if tool := toolQuotaBucketForModel(model); tool != "" && tool != primary {
		out = append(out, tool)
	}
	return out
}

// toolQuotaBucketForModel 描述 Claude Code 模型的工具/MCP/Agent 请求。
// 纯文本 Claude 请求使用 GrokBot/Sand；声明工具后必须使用 AgentService，
// 归入 Other Models。非 Claude 模型没有额外的工具额度桶。
//
//nolint:unused // retained for compatibility with the former model display layer.
func toolQuotaBucketForModel(model string) string {
	if isClaudeCodeModelName(model) {
		return "other"
	}
	return ""
}

// ⚠️ 已移除 ModelInfo()：它是 ai2api 管理面板的模型展示行构造（含定价字段）。
// sub2api 的模型列表与计费走既有通用机制（模型名驱动定价），不在协议层做。

// modelTier 由模型名归类展示层级
//
//nolint:unused // retained for compatibility with the former model display layer.
func modelTier(id string) string {
	low := strings.ToLower(id)
	switch {
	case strings.Contains(low, "opus"), strings.Contains(low, "-max"):
		return "至尊"
	case strings.Contains(low, "grok"), strings.Contains(low, "gpt-5"), strings.Contains(low, "sonnet-5"),
		strings.Contains(low, "composer"), strings.Contains(low, "fable"), strings.Contains(low, "codex"):
		return "旗舰"
	case strings.Contains(low, "flash"), strings.Contains(low, "mini"), strings.Contains(low, "nano"),
		strings.Contains(low, "haiku"), strings.Contains(low, "-low"):
		return "轻量"
	case low == "default" || low == "auto":
		return "自动"
	}
	return "标准"
}

// ⚠️ 已移除 ModelsHandler：ai2api 的 /cursor/v1/models HTTP 端点。
// sub2api 的模型列表由既有 /v1/models 与 defaultModelIDsForPlatform 提供。

// AdminModels 返回管理台账号详情页要展示的 Cursor 模型清单。
//
// ⚠️ 与 ClaudeCodeModelIDs 的区别：后者是**对外网关协议**暴露的标准 Claude
// Code 模型名；这里要展示的是 Cursor 上游实际返回的完整清单（含 composer /
// grok 等无法映射成 Claude 名字的模型），所以走 liveModelsSnapshot，
// 拉取失败时回落静态兜底 DefaultModels。两者混用会让管理台少列一半模型。
func AdminModels() []ModelMeta {
	if live := liveModelsSnapshot(); len(live) > 0 {
		return live
	}
	out := make([]ModelMeta, len(DefaultModels))
	copy(out, DefaultModels)
	return out
}

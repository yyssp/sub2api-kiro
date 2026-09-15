package routes

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// kiroRsToolTemplateConfig 是「内置模版：kiro-rs-tool 对齐」的完整参数。
//
// 它对应参考实现 2ue_kiro.rs 的 `kiro_rs_tool_template()`（config.rs:2283）：
// 该模板先把 CacheSimulationPolicy 与 PromptCacheCreationControlConfig **整组归零**，
// 再叠加 KiroRsToolCachePolicy 的 8 个值。两套参数在 kiro.rs 里是互斥分组，
// 不是可以拼在一起的扁平默认值 —— 我们的 CacheStrategyConfig 是单一扁平结构，
// 所以互斥关系必须靠这里的显式赋值还原。
//
// 唯一需要翻译而不是照抄的字段：kiro.rs 的 reportedInputMinTokens/reportedInputMaxTokens
// （默认 32/4096）约束的是**未缓存的 input 桶**，对应我们的 UncachedInput*，
// 而不是同名的 ReportedInput*（后者在本仓库约束的是**上报总输入**）。
// 照抄同名字段会把 30K token 的请求整体截到 4096，是量级错误。
func kiroRsToolTemplateConfig() service.CacheStrategyConfig {
	cfg := service.DefaultCacheStrategyConfig(service.CacheStrategyKindToolAware)

	// —— KiroRsToolCachePolicy 的 8 个值（config.rs:1844，默认见 config.rs:4460-4470）——
	cfg.CoverageRatio = 1.0                  // coverageRatio: 1.0
	cfg.MaxCoverageTokens = 0                // maxCoverageTokens: 0（不限）
	cfg.IncrementalCreateEnabled = true      // incrementalCreateEnabled: true
	cfg.MaxNewCreationTokensPerRequest = 0   // maxNewCreationTokensPerRequest: 0（不限）
	cfg.CacheCurrentUserStablePrefix = false // cacheCurrentUserStablePrefix: false
	cfg.CurrentUserStablePrefixMaxTokens = 0 // currentUserStablePrefixMaxTokens: 0
	cfg.UncachedInputMinTokens = 32          // reportedInputMinTokens: 32
	cfg.UncachedInputMaxTokens = 4096        // reportedInputMaxTokens: 4096

	// —— 本地模拟整组关闭（kiro_rs_tool_template 归零 CacheSimulationPolicy）——
	cfg.TokenScale = 1
	cfg.ScaleMinInputTokens = 0
	cfg.MaxSimulatedInputTokens = 0
	cfg.CapJitterMinTokens = 0
	cfg.CapJitterMaxTokens = 0
	cfg.ReportedInputMinTokens = 0
	cfg.ReportedInputMaxTokens = 0

	// —— 创建控制整组关闭（kiro.rs 该模板不启用 PromptCacheCreationControlConfig）——
	cfg.CreationControl = service.CacheCreationControl{}

	// —— 比例：kiro-rs-tool 不缩放上报值，覆盖多少就报多少 ——
	cfg.RatioMode = service.CacheRatioModeUniform
	cfg.UsageRatio = 1
	cfg.ReadRatio = 1
	cfg.CreationRatio = 1

	// —— 缓存面：tools + system + 历史消息，与 kiro_rs_tool_cache_blocks 的取材一致 ——
	cfg.CacheSystem = true
	cfg.CacheTools = true
	cfg.CacheHistory = true
	cfg.CacheToolResults = true
	cfg.BreakpointMode = service.CacheBreakpointHybrid
	cfg.DynamicContentMode = service.CacheDynamicContentExclude

	// —— 作用域：kiro.rs 的 PromptCacheScope 只有 conversation_id + route_namespace，
	// 不含凭证/账号（prompt_cache.rs:25-29，测试 current_scope_is_session_only_...）——
	cfg.ScopeMode = service.CacheScopeModeGroupSession

	// —— TTL / 容量：对齐 DEFAULT_PROMPT_CACHE_TTL=300s、HOUR=3600s 与 bounds 默认值 ——
	cfg.DefaultTTLSeconds = 300
	cfg.HourTTLSeconds = 3600
	// 0 = 不设门槛。kiro.rs 的 kiro_rs_tool 路径本来就不做 min_cacheable 检查
	// （该门槛只存在于 CurrentHighCache），门槛留着会让小请求整档被拒、静默零缓存。
	cfg.MinCacheableTokens = 0
	// 无 session_id 的客户端（curl、Cherry Studio）靠 system+tools+首轮用户消息
	// 派生稳定会话标识，否则这批流量建不出档案、拿不到缓存。
	cfg.AllowDerivedSession = true
	cfg.MaxEntriesPerScope = 200
	cfg.MaxEntriesGlobal = 20000
	cfg.EstimatedBytesLimit = 256 << 20
	cfg.ExpireAfterIdleSeconds = 3600

	// —— usage 投影：kiro-rs-tool 直接上报算出来的桶，不做二次采样/抬升 ——
	cfg.Usage = service.DefaultCacheUsagePolicy()
	cfg.Usage.Enabled = true
	cfg.Usage.Input = service.CacheUsageFieldPolicy{Mode: service.CacheUsageFieldRaw, NormalMaxMultiplier: 1.1}
	cfg.Usage.Output = service.CacheUsageFieldPolicy{Mode: service.CacheUsageFieldRaw, NormalMaxMultiplier: 1.1}
	cfg.Usage.CacheRead = service.CacheUsageFieldPolicy{Mode: service.CacheUsageFieldRaw, NormalMaxMultiplier: 1.1}
	cfg.Usage.CacheCreation = service.CacheUsageFieldPolicy{Mode: service.CacheUsageFieldRaw, NormalMaxMultiplier: 1.1}
	cfg.Usage.OutputUpliftEnabled = boolPtr(false)
	cfg.Usage.OutputUpliftMinTokens = 0
	cfg.Usage.OutputUpliftPercent = 0

	// 三组最终上限保留 DefaultCacheStrategyConfig 的通用护栏
	// （读 700000 / 写 400000 / 输出 200000，均带 12345~45312 扣减区间）。
	// 严格照抄 kiro.rs 该档应当是「不设上限」，但真实流量一旦异常放大就会把
	// 离谱数值直接上报出去，没有任何兜底。实测该档峰值单轮 cache_read ≈42 万、
	// creation ≈3 万，距上限仍有一倍以上余量：正常形态不会被夹，只在异常时兜底。
	cfg.Usage.FinalOutputGuardEnabled = boolPtr(true)
	cfg.Usage.FinalOutputMaxTokens = 200000
	cfg.Usage.FinalOutputJitterMinTokens = 12345
	cfg.Usage.FinalOutputJitterMaxTokens = 45312
	cfg.Usage.FinalCacheReadMaxTokens = 700000
	cfg.Usage.FinalCacheReadJitterMinTokens = 12345
	cfg.Usage.FinalCacheReadJitterMaxTokens = 45312
	cfg.Usage.FinalCacheCreationMaxTokens = 400000
	cfg.Usage.FinalCacheCreationJitterMinTokens = 12345
	cfg.Usage.FinalCacheCreationJitterMaxTokens = 45312

	// 上游（Kiro FREE 档）实测不下发任何 cache 字段，必须由本地合成，
	// 否则 preserve 会让整条策略静默失效 —— 那测的是 mock，不是策略。
	cfg.PreserveUpstreamCacheUsage = false
	cfg.Usage.PreserveUpstreamCacheUsage = false

	return cfg
}

func boolPtr(v bool) *bool { return &v }

// kiroRsTemplateTurn 是一轮会话产出的一条缓存记录。
type kiroRsTemplateTurn struct {
	Turn     int `json:"turn"`
	Input    int `json:"input_tokens"`
	Read     int `json:"cache_read_input_tokens"`
	Creation int `json:"cache_creation_input_tokens"`
	Output   int `json:"output_tokens"`
}

// runKiroRsTemplateSession 用真实网关 + 真实调度器驱动一场多轮会话，上游是 mock 账号。
// 负载量级对齐真实 Claude Code 会话（system ≈25K token、15 个工具 schema ≈20K、
// 每轮用户消息 ≈8K）：玩具级负载会让 min_cacheable_tokens / uncached_input 这类
// 阈值从未被触及，结论失真。
func runKiroRsTemplateSession(t *testing.T, baseID int64, name string, cfg service.CacheStrategyConfig, turns int) []kiroRsTemplateTurn {
	t.Helper()

	strategyID := baseID
	newRouteCacheStrategyWithConfig(strategyID, name, cfg)
	t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

	state := &routeCacheUpstream{inputBase: 40000, outputBase: 900}
	upstream := httptest.NewTLSServer(http.HandlerFunc(state.handler))
	t.Cleanup(upstream.Close)

	groupID := strategyID + 10
	group := &service.Group{
		ID:               groupID,
		Platform:         service.PlatformAnthropic,
		Status:           service.StatusActive,
		Hydrated:         true,
		RateMultiplier:   1,
		SubscriptionType: service.SubscriptionTypeStandard,
		CacheStrategyID:  &strategyID,
	}
	groupRepo := &routeCacheGroupRepo{groups: map[int64]*service.Group{groupID: group}}
	account := service.Account{
		ID:          strategyID + 20,
		Name:        name + " mock account",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 4,
		AccountGroups: []service.AccountGroup{
			{GroupID: groupID},
		},
		Credentials: map[string]any{"api_key": "mock", "base_url": upstream.URL},
		Extra:       map[string]any{"anthropic_passthrough": true},
	}
	accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{account.ID: account}}
	gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
	userID := strategyID + 30
	apiKey := &service.APIKey{
		ID:      strategyID + 40,
		UserID:  userID,
		Key:     "route-cache-test-key",
		Status:  service.StatusActive,
		User:    &service.User{ID: userID, Role: service.RoleUser, Status: service.StatusActive, Balance: 1000000, Concurrency: 8},
		GroupID: &groupID,
		Group:   group,
	}
	router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, &routeCacheHTTPUpstream{client: upstream.Client()})

	// system ≈25K token：整场会话稳定不变，是缓存的主体。
	systemText := strings.Repeat("You are Claude Code, an agentic CLI tool. Follow the repository conventions strictly. ", 1200)
	tools := make([]map[string]any, 0, 15)
	for i := 0; i < 15; i++ {
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("tool_%02d", i),
			"description": strings.Repeat(fmt.Sprintf("Tool %02d performs a well-specified repository operation with detailed constraints. ", i), 18),
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": strings.Repeat("absolute path argument. ", 12)},
					"content": map[string]any{"type": "string", "description": strings.Repeat("payload argument. ", 12)},
				},
				"required": []string{"path"},
			},
		})
	}

	sessionID := fmt.Sprintf("kiro-rs-template-session-%d", strategyID)
	messages := []map[string]any{}
	records := make([]kiroRsTemplateTurn, 0, turns)

	for turn := 1; turn <= turns; turn++ {
		// 每轮追加一个新的用户提问（≈8K token），历史逐轮增长 —— 这才是前缀缓存该命中的形态。
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type": "text",
				"text": strings.Repeat(fmt.Sprintf("Turn %d: please analyze the module and report findings in detail. ", turn), 400),
			}},
		})

		payload := map[string]any{
			"model":    "claude-sonnet-4-6",
			"metadata": map[string]any{"session_id": sessionID},
			"system": []map[string]any{{
				"type":          "text",
				"text":          systemText,
				"cache_control": map[string]any{"type": "ephemeral"},
			}},
			"tools":      tools,
			"messages":   messages,
			"max_tokens": 1024,
			"stream":     false,
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)

		resp := routeCacheRequest(t, router, string(body))
		usage, ok := resp["usage"].(map[string]any)
		require.True(t, ok, "第 %d 轮缺少 usage", turn)

		records = append(records, kiroRsTemplateTurn{
			Turn:     turn,
			Input:    kiroRsUsageInt(usage, "input_tokens"),
			Read:     kiroRsUsageInt(usage, "cache_read_input_tokens"),
			Creation: kiroRsUsageInt(usage, "cache_creation_input_tokens"),
			Output:   kiroRsUsageInt(usage, "output_tokens"),
		})

		// 追加助手回复，让下一轮的历史前缀真实增长。
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("Assistant reply for turn %d.", turn)}},
		})
	}

	require.Equal(t, turns, state.callCount(), "每一轮都必须真的打到上游，不能被短路")
	return records
}

// TestKiroRsToolTemplateRealDispatch 验证「内置模版：kiro-rs-tool 对齐」不改一行
// 生产代码就能复现 2ue_kiro.rs 的缓存行为，并落出 60 条缓存记录。
func TestKiroRsToolTemplateRealDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 60 轮增长的历史会突破默认 1MB 体上限；这里放宽的是测试网关，不是策略。
	withRouteCacheTestMaxBodySize(t, 64<<20)

	cfg := kiroRsToolTemplateConfig()
	normalized, err := service.NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err, "模版必须能通过生产校验，否则它不是一份可用的配置")
	// 校验没有悄悄改写模版语义：这几项是 kiro-rs-tool 的身份特征。
	require.Equal(t, service.CacheStrategyKindToolAware, normalized.Kind)
	require.Equal(t, 1.0, normalized.CoverageRatio)
	require.Equal(t, float64(1), normalized.TokenScale)
	require.False(t, normalized.CreationControl.Enabled)
	require.Equal(t, 32, normalized.UncachedInputMinTokens)
	require.Equal(t, 0, normalized.MinCacheableTokens, "0 = 不设门槛，不能被 normalize 改写")
	require.True(t, normalized.AllowDerivedSession)
	require.Equal(t, 4096, normalized.UncachedInputMaxTokens)

	const turns = 60
	records := runKiroRsTemplateSession(t, 99701, "kiro-rs-tool aligned template", normalized, turns)
	require.Len(t, records, turns)

	// —— 断言 1：首轮冷启动，只建不读 ——
	require.Zero(t, records[0].Read, "首轮必须是冷的")
	require.Greater(t, records[0].Creation, 0, "首轮必须建起缓存")

	// —— 断言 2：缓存必须真的用起来，而不是每轮重建 ——
	warmTurns, totalRead, totalCreation := 0, 0, 0
	for _, r := range records {
		if r.Read > 0 {
			warmTurns++
		}
		totalRead += r.Read
		totalCreation += r.Creation
	}
	require.GreaterOrEqual(t, warmTurns, turns-5,
		"除冷启动外几乎每轮都应命中；命中轮数 %d/%d 说明前缀没有稳定下来", warmTurns, turns)
	require.Greater(t, totalRead, totalCreation,
		"kiro-rs-tool 的形态是「读远大于写」，实际 read=%d creation=%d", totalRead, totalCreation)

	// —— 断言 3：cache_read 随会话增长 ——
	require.Greater(t, records[turns-1].Read, records[1].Read,
		"末轮 cache_read 必须显著高于次轮，否则缓存没有随会话增长")

	// —— 断言 4：未缓存 input 落在 kiro.rs 的 [32, 4096] 带内 ——
	for _, r := range records {
		require.GreaterOrEqual(t, r.Input, 0, "turn %d", r.Turn)
		if r.Read+r.Creation > 0 {
			require.LessOrEqual(t, r.Input, 4096,
				"turn %d 的未缓存 input=%d 超出 kiro.rs 的 reportedInputMaxTokens=4096", r.Turn, r.Input)
		}
	}

	// —— 断言 5：token_scale=1 不放大上报总量 ——
	// 这是 kiro-rs-tool 与 prefix 档最直观的分野，所以拿同一份负载在 prefix 档
	// （token_scale=2）下的实测值做对照，而不是一个拍脑袋的常数。
	prefixCfg, err := service.NormalizeCacheStrategyConfig(service.DefaultCacheStrategyConfig(service.CacheStrategyKindPrefix))
	require.NoError(t, err)
	prefixRecords := runKiroRsTemplateSession(t, 99801, "prefix baseline for contrast", prefixCfg, turns)

	last := records[turns-1]
	lastTotal := last.Input + last.Read + last.Creation
	prefixLast := prefixRecords[turns-1]
	prefixTotal := prefixLast.Input + prefixLast.Read + prefixLast.Creation
	require.Greater(t, lastTotal, 0)
	require.Less(t, lastTotal, prefixTotal,
		"kiro-rs-tool（token_scale=1）末轮上报总量 %d 必须低于 prefix 档（token_scale=2）的 %d",
		lastTotal, prefixTotal)

	// —— 输出 60 条缓存记录，供人工核对 ——
	t.Logf("=== kiro-rs-tool 对齐模版：%d 轮真实调度产出的缓存记录 ===", turns)
	t.Logf("%4s | %12s | %12s | %14s | %8s", "turn", "input", "cache_read", "cache_create", "output")
	for _, r := range records {
		t.Logf("%4d | %12d | %12d | %14d | %8d", r.Turn, r.Input, r.Read, r.Creation, r.Output)
	}
	hitRate := float64(warmTurns) / float64(turns) * 100
	t.Logf("命中轮数 %d/%d（%.1f%%），累计 cache_read=%d，累计 cache_creation=%d，读写比 %.2f",
		warmTurns, turns, hitRate, totalRead, totalCreation,
		float64(totalRead)/math.Max(float64(totalCreation), 1))
	t.Logf("末轮上报总量：kiro-rs-tool=%d vs prefix(token_scale=2)=%d", lastTotal, prefixTotal)
}

func kiroRsUsageInt(usage map[string]any, key string) int {
	value, ok := usage[key]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	default:
		return 0
	}
}

// TestKiroRsToolTemplateForcesShapingWithoutProfile 钉死「分组挂了生效策略就一定整形」。
//
// 用一个建不出缓存档案的请求（无 session_id 且关掉派生会话），验证 usage 仍按策略
// 投影，而不是把上游原始数字透传出去。这条路径以前直接 return，是策略被大量真实
// 流量绕过的主因。
func TestKiroRsToolTemplateForcesShapingWithoutProfile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := kiroRsToolTemplateConfig()
	// 关掉派生会话 + 保留断点门槛，制造「策略生效但建不出档案」的局面。
	cfg.AllowDerivedSession = false
	// 让 output 整形可观测：上游固定回 900，这里封到 500。
	cfg.Usage.FinalOutputGuardEnabled = boolPtr(true)
	cfg.Usage.FinalOutputMaxTokens = 500
	cfg.Usage.FinalOutputJitterMinTokens = 0
	cfg.Usage.FinalOutputJitterMaxTokens = 0
	normalized, err := service.NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)

	strategyID := int64(99901)
	newRouteCacheStrategyWithConfig(strategyID, "kiro-rs-tool forced shaping", normalized)
	t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

	state := &routeCacheUpstream{inputBase: 40000, outputBase: 900}
	upstream := httptest.NewTLSServer(http.HandlerFunc(state.handler))
	t.Cleanup(upstream.Close)

	groupID := strategyID + 10
	group := &service.Group{
		ID:               groupID,
		Platform:         service.PlatformAnthropic,
		Status:           service.StatusActive,
		Hydrated:         true,
		RateMultiplier:   1,
		SubscriptionType: service.SubscriptionTypeStandard,
		CacheStrategyID:  &strategyID,
	}
	groupRepo := &routeCacheGroupRepo{groups: map[int64]*service.Group{groupID: group}}
	account := service.Account{
		ID:            strategyID + 20,
		Name:          "forced-shaping-mock",
		Platform:      service.PlatformAnthropic,
		Type:          service.AccountTypeAPIKey,
		Status:        service.StatusActive,
		Schedulable:   true,
		Concurrency:   4,
		AccountGroups: []service.AccountGroup{{GroupID: groupID}},
		Credentials:   map[string]any{"api_key": "mock", "base_url": upstream.URL},
		Extra:         map[string]any{"anthropic_passthrough": true},
	}
	accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{account.ID: account}}
	gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
	userID := strategyID + 30
	apiKey := &service.APIKey{
		ID:      strategyID + 40,
		UserID:  userID,
		Key:     "route-cache-test-key",
		Status:  service.StatusActive,
		User:    &service.User{ID: userID, Role: service.RoleUser, Status: service.StatusActive, Balance: 1000000, Concurrency: 8},
		GroupID: &groupID,
		Group:   group,
	}
	router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, &routeCacheHTTPUpstream{client: upstream.Client()})

	// 无 metadata.session_id、无 cache_control：建不出缓存档案。
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":128,"stream":false}`
	resp := routeCacheRequest(t, router, body)
	usage, ok := resp["usage"].(map[string]any)
	require.True(t, ok)

	output := kiroRsUsageInt(usage, "output_tokens")
	t.Logf("无档案时上报：input=%d read=%d creation=%d output=%d",
		kiroRsUsageInt(usage, "input_tokens"), kiroRsUsageInt(usage, "cache_read_input_tokens"),
		kiroRsUsageInt(usage, "cache_creation_input_tokens"), output)

	// 核心断言：output 被策略封顶，说明整形确实发生了（上游回的是 900）。
	require.LessOrEqual(t, output, 500,
		"挂了生效策略就必须整形；output=%d 说明上游原值被透传了", output)
	// 诚实性：确实没有缓存，不能凭空造出 cache_read。
	require.Zero(t, kiroRsUsageInt(usage, "cache_read_input_tokens"))
	require.Zero(t, kiroRsUsageInt(usage, "cache_creation_input_tokens"))
}

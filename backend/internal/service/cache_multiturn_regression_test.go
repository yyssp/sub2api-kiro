package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 多轮累积对话的回归测试。
//
// 缓存策略的真实使用场景是 Claude Code 会话：上下文逐轮增长，每轮几十 K token。
// 单轮测试覆盖不到「本轮结果影响下一轮」的状态机行为，而缓存失效恰恰发生在那里。

// buildMultiTurnBody 构造第 turn 轮的请求体，前缀跨轮完全一致。
func buildMultiTurnBody(sessionID string, turn int) []byte {
	stable := strings.Repeat("stable system instructions for the coding agent. ", 40)
	chunk := strings.Repeat("source file content under review. ", 80)

	messages := make([]map[string]any, 0, turn*2)
	for i := 1; i <= turn; i++ {
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("Round %d: review this file.\n\n%s", i, chunk),
			}},
		})
		if i < turn {
			messages = append(messages, map[string]any{
				"role":    "assistant",
				"content": []map[string]any{{"type": "text", "text": "Analysis for round " + fmt.Sprint(i) + ". " + chunk}},
			})
		}
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-6",
		"metadata": map[string]any{"session_id": sessionID},
		"system": []map[string]any{{
			"type": "text", "text": stable,
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"messages": messages,
	})
	return body
}

// cache_history 与 cache_current_user_stable_prefix 同时关闭时，历史消息被过滤掉，
// 只剩当前用户消息，而它在没有显式 cache_control 时按设计不作为断点 —— 没有任何
// 内容可缓存。这种组合必须在写入前被拒绝，否则策略会静默失效（缓存恒为 0）。
func TestCacheHistoryAndCurrentUserBothDisabledIsRejected(t *testing.T) {
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindToolAware)
	cfg.CacheHistory = false
	cfg.CacheCurrentUserStablePrefix = false

	_, err := NormalizeCacheStrategyConfig(cfg)
	require.Error(t, err, "该组合没有任何内容可缓存，必须拒绝")
	require.Contains(t, err.Error(), "cache_history")
}

// 关掉 history 但保留 current-user stable prefix 时，缓存仍应正常工作。
func TestCacheHistoryDisabledStillCachesViaCurrentUserPrefix(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900302)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindToolAware)
	cfg.MinCacheableTokens = 1024
	cfg.AllowDerivedSession = true
	cfg.CacheHistory = false
	cfg.CacheCurrentUserStablePrefix = true
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "no history", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9303, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9304, Platform: PlatformAnthropic}
	svc := &GatewayService{}

	cacheActiveTurns := 0
	for turn := 1; turn <= 12; turn++ {
		body := buildMultiTurnBody("no-history-session", turn)
		plan := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 6000*turn)
		require.NotNil(t, plan)
		if r := plan.result(); r != nil && (r.CacheReadInputTokens > 0 || r.CacheCreationInputTokens > 0) {
			cacheActiveTurns++
		}
		plan.commit()
	}

	require.Greater(t, cacheActiveTurns, 1,
		"保留 current-user stable prefix 时缓存应能跨轮生效")
}

// min_creation_delta_tokens 高于 max_creation_tokens_per_event 时，创建请求
// 要么因低于下限被丢弃，要么被上限截断，两个条件无法同时满足 —— 缓存几乎永远
// 建不起来。这种自相矛盾的组合必须在写入前拒绝。
func TestCreationControlDeltaAboveEventCapIsRejected(t *testing.T) {
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.CreationControl = CacheCreationControl{
		Enabled:                   true,
		MinCreationDeltaTokens:    2048,
		MaxCreationTokensPerEvent: 2000,
	}

	_, err := NormalizeCacheStrategyConfig(cfg)
	require.Error(t, err, "下限高于单次上限的组合必须拒绝")
	require.Contains(t, err.Error(), "min_creation_delta_tokens")
}

// 缓存是旁路能力：畸形请求体、离谱配置都不能让请求失败。
func TestCachePlanNeverFailsRequestOnMalformedBody(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900303)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.AllowDerivedSession = true
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "malformed", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9305, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9306, Platform: PlatformAnthropic}

	bodies := [][]byte{
		nil,
		[]byte(``),
		[]byte(`{`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(`{"messages":"not-an-array"}`),
		[]byte(`{"messages":[{"role":"user","content":[{"type":"text"}]}]}`),
		[]byte(`{"system":12345,"messages":[]}`),
		[]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":"bad"}]}]}`),
	}

	for _, body := range bodies {
		require.NotPanics(t, func() {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
			plan := prepareCachePlanForContext(context.Background(), ctx, account, group,
				body, "claude-sonnet-4-6", "anthropic_messages", 500)

			usage := &ClaudeUsage{InputTokens: 500, OutputTokens: 20}
			mergeAndCommitCachePlan(ctx, usage, true)
			// 无论缓存是否生效，usage 必须仍是可用的
			require.GreaterOrEqual(t, usage.InputTokens, 0)
			_ = plan
		}, "body=%q 不能让请求失败", string(body))
	}
}

// 缓存把整个前缀吃光时，上报的 input_tokens 会掉到 0，出现 input=0 + cache_read>0
// 这种真实 API 不存在的组合。未缓存 input 下限必须把它顶回来，而且顶回来的值要在
// [min, max] 区间内抖动 —— 每轮都是同一个数字同样一眼假。
func TestCappedTurnsKeepNonZeroJitteredInput(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900304)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1024
	cfg.AllowDerivedSession = true
	// 触顶场景：上报总量被压到 30000，缓存足以覆盖全部。
	cfg.ReportedInputMaxTokens = 30000
	cfg.CoverageRatio = 1
	cfg.CreationControl = CacheCreationControl{}
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	require.Positive(t, cfg.UncachedInputMinTokens, "prefix 默认必须带未缓存 input 下限")
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "capped", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9305, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9306, Platform: PlatformAnthropic}
	svc := &GatewayService{}

	inputs := map[int]int{}
	cachedTurns := 0
	for turn := 1; turn <= 12; turn++ {
		body := buildMultiTurnBody("capped-session", turn)
		plan := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 60000*turn)
		require.NotNil(t, plan)
		if r := plan.result(); r != nil && (r.CacheReadInputTokens > 0 || r.CacheCreationInputTokens > 0) {
			cachedTurns++
			require.GreaterOrEqual(t, r.InputTokens, cfg.UncachedInputMinTokens,
				"第 %d 轮出现 input=%d，缓存不能把 input 吃到下限以下", turn, r.InputTokens)
			require.LessOrEqual(t, r.InputTokens, cfg.UncachedInputMaxTokens,
				"第 %d 轮 input=%d 超出抖动区间上限", turn, r.InputTokens)
			inputs[r.InputTokens]++
		}
		plan.commit()
	}

	require.Greater(t, cachedTurns, 1, "触顶场景本身要能产生缓存，否则这条测试没意义")
	require.Greater(t, len(inputs), 1,
		"触顶的 %d 轮里 input_tokens 只有 %d 个取值，等于每条都一样", cachedTurns, len(inputs))
}

// 同一个请求重试时，抖动出来的 input 必须稳定，否则计费对不上。
func TestUncachedInputJitterIsDeterministic(t *testing.T) {
	for _, seed := range []uint64{0, 1, 7, 1 << 40} {
		a := jitterWithin(32, 4096, seed)
		b := jitterWithin(32, 4096, seed)
		require.Equal(t, a, b)
		require.GreaterOrEqual(t, a, 32)
		require.LessOrEqual(t, a, 4096)
	}
}

// prefix 与 tool_aware 是两套互斥的参数（对应 kiro.rs 的 CurrentHighCache 与
// KiroRsTool 模板），不能共用同一份默认值。
func TestKindDefaultsAreSeparate(t *testing.T) {
	prefix := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	tool := DefaultCacheStrategyConfig(CacheStrategyKindToolAware)

	// prefix ≈ CurrentHighCache：本地模拟 + 创建限流都开。
	require.True(t, prefix.CreationControl.Enabled)
	require.Positive(t, prefix.CapJitterMaxTokens)

	// tool_aware ≈ KiroRsTool：模拟与限流整组关掉，全量覆盖。
	require.False(t, tool.CreationControl.Enabled)
	require.Zero(t, tool.CapJitterMaxTokens)
	require.Zero(t, tool.MaxSimulatedInputTokens)
	require.Equal(t, 1.0, tool.CoverageRatio)
	require.Equal(t, 32, tool.UncachedInputMinTokens)
	require.Equal(t, 4096, tool.UncachedInputMaxTokens)

	disabled := DefaultCacheStrategyConfig(CacheStrategyKindDisabled)
	require.False(t, disabled.CreationControl.Enabled)
	require.Zero(t, disabled.UncachedInputMinTokens)
}

// smallPayloadCacheConfig 给玩具级负载的单测用。
//
// 生产默认值对齐 kiro.rs：创建增量下限 12000 token、未缓存 input 下限 1024。
// 而这些单测的请求体只有一两千 token，带着生产默认值跑，创建永远达不到下限，
// 结果恒为 nil —— 测的就不是被测行为了。需要真实量级的用例请直接用
// DefaultCacheStrategyConfig，并把负载做到每轮几十 K。
func smallPayloadCacheConfig(kind string) CacheStrategyConfig {
	cfg := DefaultCacheStrategyConfig(kind)
	cfg.CreationControl = CacheCreationControl{}
	cfg.UncachedInputMinTokens, cfg.UncachedInputMaxTokens = 0, 0
	return cfg
}

// 创建控制压制一次上报，不能连带把「本轮写进了哪段前缀」也抹掉。
//
// 曾经的顺序是「先跑创建控制，再按压制后的额度挑写入集」。自动断点每轮位置都会前移，
// 于是被压制的那一轮什么都没提交，下一轮读不到东西、又生成一次被压制的创建 ——
// 整场会话的 cache_read 恒为 0，且没有任何报错。这正是 kiro.rs 用
// with_allowed_creation() 划清的边界：创建控制只改上报数字，不决定前缀是否推进。
func TestCreationControlSuppressionDoesNotStallCacheGrowth(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900305)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.AllowDerivedSession = true
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	// 默认值必须自带节流，否则这条测试测不到压制路径。
	require.True(t, cfg.CreationControl.Enabled)
	require.Positive(t, cfg.CreationControl.MinCreationIntervalSeconds)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "throttled growth", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9307, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9308, Platform: PlatformAnthropic}
	svc := &GatewayService{}

	// 单轮几十 K 的真实量级：小负载下创建根本达不到默认的增量下限。
	reads := make([]int, 0, 6)
	for turn := 1; turn <= 6; turn++ {
		body := throttledGrowthBody("throttled-session", turn)
		plan := svc.prepareCacheEmulationUsage(
			context.Background(), account, group, body, "claude-sonnet-4-5-20250929", 40000*turn)
		require.NotNil(t, plan)
		if r := plan.result(); r != nil {
			reads = append(reads, r.CacheReadInputTokens)
		} else {
			reads = append(reads, 0)
		}
		plan.commit()
	}

	// 60 秒最小间隔意味着 6 轮里只有第 1 轮能上报创建，后面全被压制 ——
	// 而 cache_read 必须照常跟着上下文往上走。
	//
	// 前两轮不做断言：断点位置由 token 估算推导，上下文体量变化时会整体平移，
	// 早期几轮对不上指纹是正常的。回归要抓的是「一路恒为 0」，不是前两轮的抖动。
	require.Zero(t, reads[0], "首轮没有可读前缀")
	for i := 3; i < len(reads); i++ {
		require.Greater(t, reads[i], reads[i-1],
			"第 %d 轮 cache_read=%d 没有超过上一轮 %d —— 前缀停止推进了", i+1, reads[i], reads[i-1])
	}
	require.Positive(t, reads[len(reads)-1], "末轮 cache_read 仍为 0，缓存整场未生效")
}

// throttledGrowthBody 构造贴近真实 Claude Code 请求的请求体：
// 无显式 cache_control（靠 hybrid 自动断点）、session 走 metadata.user_id、带工具定义。
func throttledGrowthBody(session string, turn int) []byte {
	system := "You are a senior engineer on a large Go codebase.\n\n" +
		strings.Repeat("func handler() error { return nil }\n", 1600)
	chunk := strings.Repeat("source file content under review here. ", 3000)
	reply := strings.Repeat("Analysis complete for the previous round. ", 800)

	messages := make([]map[string]any, 0, turn*2)
	for i := 1; i <= turn; i++ {
		messages = append(messages, map[string]any{
			"role": "user", "content": fmt.Sprintf("Round %d\n\n%s", i, chunk),
		})
		if i < turn {
			messages = append(messages, map[string]any{"role": "assistant", "content": reply})
		}
	}

	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5-20250929", "max_tokens": 1024,
		"system":   system,
		"metadata": map[string]any{"user_id": session},
		"tools": []map[string]any{{
			"name": "read_file", "description": "Read a file from the repository.",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		}},
		"messages": messages,
	})
	return body
}

// 触顶抖动不能把上报值整体拉低一个量级。
//
// cap_jitter 的默认 12k~24k 是按 kiro.rs 30 万的模拟上限定的。用户把
// reported_input_max_tokens 设成 3 万时照搬，会把 30000 砍成 6000~18000 ——
// 那不是"触顶带点噪声"，而是一个完全不同的数量级。
func TestCapJitterStaysProportionalToTheCap(t *testing.T) {
	const cap30k = 30000
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	for seed := uint64(0); seed < 200; seed++ {
		got := applyReportedInputJitter(cap30k, cfg.CapJitterMinTokens, cfg.CapJitterMaxTokens, seed, 0)
		require.GreaterOrEqual(t, got, cap30k*3/4,
			"seed=%d 抖动后 %d 低于上限 %d 的 75%%", seed, got, cap30k)
		require.LessOrEqual(t, got, cap30k)
	}

	// 夹取方式对齐 kiro.rs 的 cap_jitter()：上限取「触顶值的 8%」，
	// min 直接 .min(max) 夹到 max 而不是按比例缩放。配置的 12k~24k 都超过
	// 30000 的 8%（2400）时，区间会塌成一个点 —— 这与参考实现一致。
	// 想在小上限下拿到一个区间，就把 cap_jitter_min/max 配到上限的 8% 以内。
	narrowed := map[int]struct{}{}
	for seed := uint64(0); seed < 200; seed++ {
		narrowed[applyReportedInputJitter(cap30k, cfg.CapJitterMinTokens, cfg.CapJitterMaxTokens, seed, 0)] = struct{}{}
	}
	require.Len(t, narrowed, 1, "两端都超过 8% 时应一起夹到同一个点")

	// 配在 8% 以内时，区间必须照常散开。
	inBand := map[int]struct{}{}
	for seed := uint64(0); seed < 200; seed++ {
		inBand[applyReportedInputJitter(cap30k, 400, 2000, seed, 0)] = struct{}{}
	}
	require.Greater(t, len(inBand), 1, "8% 以内的区间不应被夹平")

	// 上限本身很大时，默认区间应原样生效，不被比例限制吃掉。
	const cap300k = 300000
	big := applyReportedInputJitter(cap300k, cfg.CapJitterMinTokens, cfg.CapJitterMaxTokens, 7, 0)
	require.GreaterOrEqual(t, big, cap300k-cfg.CapJitterMaxTokens)
	require.LessOrEqual(t, big, cap300k-cfg.CapJitterMinTokens)
}

// 上限扣减区间：触顶的记录不能全部落在同一个数字上。
//
// 用户的验收标准第 5 条要求「到达极限时给一个随机变量做增减」，第 2 条要求
// 连续几条数据不能都是同一个缓存数值。裸 min() 会让所有触顶请求上报一模一样的
// 上限值（一整列 700000），一眼就是伪造的。
func TestFinalCapJitterSpreadsCappedValues(t *testing.T) {
	const capTokens = 650000
	const jMin, jMax = 23456, 54321

	seen := map[int]struct{}{}
	for seed := uint64(1); seed <= 40; seed++ {
		got := applyFinalCapWithJitter(capTokens*2, capTokens, jMin, jMax, seed)
		require.GreaterOrEqual(t, got, capTokens-jMax)
		require.LessOrEqual(t, got, capTokens-jMin)
		seen[got] = struct{}{}
	}
	// 40 次取样必须散开，否则等于没有抖动。
	require.Greater(t, len(seen), 10)

	// 同一 seed（同一请求）必须稳定，重试不能换一个数字。
	require.Equal(t,
		applyFinalCapWithJitter(capTokens*2, capTokens, jMin, jMax, 9),
		applyFinalCapWithJitter(capTokens*2, capTokens, jMin, jMax, 9))

	// 未触顶的值原样返回：这些值本来就各不相同，再减一次会凭空少报 token。
	require.Equal(t, 1000, applyFinalCapWithJitter(1000, capTokens, jMin, jMax, 3))

	// 没配上限（0）= 不限制，扣减区间不得凭空生效。
	require.Equal(t, 9_000_000, applyFinalCapWithJitter(9_000_000, 0, jMin, jMax, 3))

	// 扣减量超过上限本身时夹到上限，结果不能变成负数。
	require.GreaterOrEqual(t, applyFinalCapWithJitter(5000, 100, 9999, 99999, 3), 0)
}

func TestSampleMaxJitterSpreadsCappedValues(t *testing.T) {
	const maxTokens = 200

	seen := map[int]struct{}{}
	for seed := uint64(1); seed <= 100; seed++ {
		got := projectUsageField(CacheUsageFieldPolicy{
			Mode:      CacheUsageFieldSampleMax,
			MaxTokens: maxTokens,
		}, maxTokens*2, seed)
		require.Greater(t, got, 0)
		require.LessOrEqual(t, got, maxTokens)
		seen[got] = struct{}{}
	}
	require.Greater(t, len(seen), 5, "sample_max 触顶后应产生多个确定性取值")

	require.Equal(t,
		projectUsageField(CacheUsageFieldPolicy{
			Mode:      CacheUsageFieldSampleMax,
			MaxTokens: maxTokens,
		}, maxTokens*2, 9),
		projectUsageField(CacheUsageFieldPolicy{
			Mode:      CacheUsageFieldSampleMax,
			MaxTokens: maxTokens,
		}, maxTokens*2, 9),
		"同一 seed 的 sample_max 触顶抖动必须稳定")

	require.Equal(t, 99, projectUsageField(CacheUsageFieldPolicy{
		Mode:      CacheUsageFieldSampleMax,
		MaxTokens: maxTokens,
	}, 99, 3), "未触顶值必须原样返回")
	require.Equal(t, maxTokens, projectUsageField(CacheUsageFieldPolicy{
		Mode:      CacheUsageFieldSampleMax,
		MaxTokens: maxTokens,
	}, maxTokens, 3), "刚好等于上限时不应额外扣减")
	require.Zero(t, projectUsageField(CacheUsageFieldPolicy{
		Mode:      CacheUsageFieldSampleMax,
		MaxTokens: maxTokens,
	}, 0, 3), "原始 0 不应被抖动成正数")
}

func TestFinalCapJitterNormalizationKeepsSmallCapsPositiveAndVariable(t *testing.T) {
	minJitter, maxJitter := normalizeFinalCapJitter(8000, 12345, 45312)
	require.Less(t, minJitter, maxJitter)
	require.Less(t, maxJitter, 8000)

	seen := map[int]struct{}{}
	for seed := uint64(1); seed <= 100; seed++ {
		got := applyFinalCapWithJitter(16000, 8000, 12345, 45312, seed)
		require.Greater(t, got, 0)
		require.LessOrEqual(t, got, 8000)
		seen[got] = struct{}{}
	}
	require.Greater(t, len(seen), 5)

	for _, capTokens := range []int{1, 2, 10, 12345} {
		for seed := uint64(0); seed < 20; seed++ {
			got := applyFinalCapWithJitter(capTokens*2, capTokens, 12345, 45312, seed)
			require.GreaterOrEqual(t, got, 1)
			require.LessOrEqual(t, got, capTokens)
		}
	}
}

func TestNormalizeCacheStrategyConfigScalesCollapsedFinalCapJitter(t *testing.T) {
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.Usage.FinalCacheReadMaxTokens = 80000
	cfg.Usage.FinalCacheReadJitterMinTokens = 12345
	cfg.Usage.FinalCacheReadJitterMaxTokens = 45312
	cfg.Usage.FinalOutputMaxTokens = 8000
	cfg.Usage.FinalOutputJitterMinTokens = 12345
	cfg.Usage.FinalOutputJitterMaxTokens = 45312

	normalized, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	require.Less(t,
		normalized.Usage.FinalCacheReadJitterMinTokens,
		normalized.Usage.FinalCacheReadJitterMaxTokens,
	)
	require.Less(t,
		normalized.Usage.FinalOutputJitterMinTokens,
		normalized.Usage.FinalOutputJitterMaxTokens,
	)
	require.Less(t,
		normalized.Usage.FinalOutputJitterMaxTokens,
		normalized.Usage.FinalOutputMaxTokens,
	)
}

// 配置缺失不能报错，也不能改变原有行为。
func TestUsagePolicyDegradesSafelyWhenUnconfigured(t *testing.T) {
	// 存量策略的 JSON 里没有 final_output_guard_enabled 这个字段。
	// 裸 bool 会反序列化成 false，等于静默关掉输出上限 —— 必须补成开启。
	var legacy CacheUsagePolicy
	require.NoError(t, json.Unmarshal([]byte(`{"enabled":true,"final_output_max_tokens":4096}`), &legacy))
	require.Nil(t, legacy.FinalOutputGuardEnabled)
	require.True(t, legacy.OutputGuardOn(), "未配置的总开关必须视为开启")

	normalized := normalizeCacheUsagePolicy(legacy)
	require.NotNil(t, normalized.FinalOutputGuardEnabled)
	require.True(t, *normalized.FinalOutputGuardEnabled)

	// 显式关掉时才真的不生效。
	off := CacheUsagePolicy{Enabled: true, FinalOutputGuardEnabled: boolPtr(false), FinalOutputMaxTokens: 100}
	usage := &ClaudeUsage{OutputTokens: 9999}
	applyUsageProjectionClaude(usage, 0, 9999, off, 1, false)
	require.Equal(t, 9999, usage.OutputTokens, "总开关关掉后输出上限不得生效")

	// 上限为 0 时扣减区间一并清零，页面不会留下一组无效数字。
	cleared := normalizeCacheUsagePolicy(CacheUsagePolicy{
		Enabled:                       true,
		FinalCacheReadJitterMinTokens: 500, FinalCacheReadJitterMaxTokens: 900,
	})
	require.Zero(t, cleared.FinalCacheReadJitterMinTokens)
	require.Zero(t, cleared.FinalCacheReadJitterMaxTokens)

	// min > max 的手工输入要被收敛，不能报错。
	swapped := normalizeCacheUsagePolicy(CacheUsagePolicy{
		Enabled: true, FinalCacheCreationMaxTokens: 1000,
		FinalCacheCreationJitterMinTokens: 800, FinalCacheCreationJitterMaxTokens: 200,
	})
	require.LessOrEqual(t, swapped.FinalCacheCreationJitterMinTokens, swapped.FinalCacheCreationJitterMaxTokens)
	require.NoError(t, validateCacheUsagePolicy(swapped))
}

// 分组没绑策略、或绑了但没启用，都必须按默认逻辑放行，不产生任何错误。
func TestUnboundOrDisabledStrategyFallsBackWithoutError(t *testing.T) {
	resetCacheTracker()
	svc := &GatewayService{}
	account := &Account{ID: 9402, Platform: PlatformAnthropic}
	body := throttledGrowthBody("fallback-session", 2)

	// 1) 完全没绑策略。
	unbound := &Group{ID: 9401, Platform: PlatformAnthropic}
	require.False(t, hasBoundCacheStrategy(unbound))
	require.Nil(t, svc.prepareCacheEmulationUsage(
		context.Background(), account, unbound, body, "claude-sonnet-4-5-20250929", 40000))

	// 2) 绑了策略但 Enabled=false。
	strategyID := int64(900401)
	baseCfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	// 这个请求体没有真实会话标识，不放开派生会话就挑不出缓存键，
	// 第 3 步会拿到 nil 而掩盖掉「启用后才生效」的对比。
	baseCfg.AllowDerivedSession = true
	cfg, err := NormalizeCacheStrategyConfig(baseCfg)
	require.NoError(t, err)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "bound but off", Enabled: false, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	bound := &Group{ID: 9403, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	require.True(t, hasBoundCacheStrategy(bound), "绑定关系仍然存在，只是没启用")
	_, enabled := effectiveCacheStrategyConfig(bound)
	require.False(t, enabled)
	require.Nil(t, svc.prepareCacheEmulationUsage(
		context.Background(), account, bound, body, "claude-sonnet-4-5-20250929", 40000))

	// 3) 绑定且启用，策略才真正生效。
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "bound and on", Enabled: true, Revision: 2, Config: cfg,
	})
	require.NotNil(t, svc.prepareCacheEmulationUsage(
		context.Background(), account, bound, body, "claude-sonnet-4-5-20250929", 40000))
}

// skip_non_stream_usage_projection 只影响非流式，且必须真的生效。
func TestSkipNonStreamUsageProjectionTakesEffect(t *testing.T) {
	streamBody := []byte(`{"model":"m","stream":true}`)
	nonStreamBody := []byte(`{"model":"m","stream":false}`)
	absentBody := []byte(`{"model":"m"}`)

	require.True(t, requestBodyWantsStream(streamBody))
	require.False(t, requestBodyWantsStream(nonStreamBody))
	require.False(t, requestBodyWantsStream(absentBody), "字段缺失按非流式算")
	require.False(t, requestBodyWantsStream(nil), "请求体畸形也不能 panic")

	on := CacheUsagePolicy{Enabled: true, SkipNonStreamUsageProjection: true}
	require.True(t, (&cacheEmulationPlan{nonStream: true, usagePolicy: on}).skipProjection())
	require.False(t, (&cacheEmulationPlan{nonStream: false, usagePolicy: on}).skipProjection(),
		"流式响应不受这个开关影响")

	off := CacheUsagePolicy{Enabled: true}
	require.False(t, (&cacheEmulationPlan{nonStream: true, usagePolicy: off}).skipProjection(),
		"未配置时保持原有的投影行为")
	require.False(t, (*cacheEmulationPlan)(nil).skipProjection())
}

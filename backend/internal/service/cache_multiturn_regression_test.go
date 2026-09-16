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

func TestCreationControlAccumulatesPendingAndJittersEventCap(t *testing.T) {
	resetCacheTracker()
	control := CacheCreationControl{
		Enabled:                      true,
		MinCreationDeltaTokens:       12000,
		MinSuccessfulRequestsBetween: 0,
		MinCreationIntervalSeconds:   0,
		MaxCreationTokensPerEvent:    100000,
	}

	// The first creation is released immediately, just like kiro.rs. Later
	// sub-threshold creations accumulate instead of disappearing forever.
	first := &cacheEmulationUsage{CacheCreationInputTokens: 5000}
	decision := applyCacheCreationControl(101, control, first)
	require.Equal(t, 5000, decision.allowed)
	globalCacheTracker.recordSuccess(101, decision.attempted, decision.allowed)

	second := &cacheEmulationUsage{CacheCreationInputTokens: 2000}
	decision = applyCacheCreationControl(101, control, second)
	require.Zero(t, decision.allowed)
	globalCacheTracker.recordSuccess(101, decision.attempted, decision.allowed)

	third := &cacheEmulationUsage{CacheCreationInputTokens: 5000}
	decision = applyCacheCreationControl(101, control, third)
	require.Zero(t, decision.allowed)
	globalCacheTracker.recordSuccess(101, decision.attempted, decision.allowed)

	fourth := &cacheEmulationUsage{CacheCreationInputTokens: 6000}
	decision = applyCacheCreationControl(101, control, fourth)
	require.Equal(t, 6000, decision.allowed,
		"pending 7k + current 6k reaches the 12k release threshold")

	// A capped event is never a fixed 100k. Different request/cache keys get
	// different stable deductions while remaining inside the kiro.rs band.
	seen := map[int]struct{}{}
	for key := uint64(1); key <= 200; key++ {
		usage := &cacheEmulationUsage{CacheCreationInputTokens: 150000}
		decision := applyCacheCreationControl(key+1000, control, usage)
		require.GreaterOrEqual(t, decision.allowed, 88000)
		require.LessOrEqual(t, decision.allowed, 100000-100000/33)
		require.NotEqual(t, 100000, decision.allowed)
		seen[decision.allowed] = struct{}{}
	}
	require.Greater(t, len(seen), 10, "event cap jitter must produce multiple stable values")
}

// prefix 默认值必须能每轮都放行一点 cache_creation。
//
// 三个频率闸门（min_creation_delta_tokens / min_successful_requests_between /
// min_creation_interval_seconds）原本抄的是 kiro.rs 的线上配置，那边一次创建写整段
// 前缀（几万 token、间隔很久）；我们 incremental_create_enabled 默认开着，每轮只写
// 增量（实测几百~几千 token、间隔不到 1 秒），于是每个闸门都能单独把每轮上报的
// creation 压成 0。2026-09-16 真实上游 20 轮实测：12000/2/6 这组值下 creation 只有
// 2/20 轮非零；三个闸门归零后是 20/20。上限保留 —— 那只是防离谱值的护栏。
func TestPrefixDefaultReleasesCreationEveryTurn(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900307)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.AllowDerivedSession = true
	require.True(t, cfg.CreationControl.Enabled, "上限护栏要留着")
	require.Zero(t, cfg.CreationControl.MinCreationDeltaTokens)
	require.Zero(t, cfg.CreationControl.MinSuccessfulRequestsBetween)
	require.Zero(t, cfg.CreationControl.MinCreationIntervalSeconds)
	require.Positive(t, cfg.CreationControl.MaxCreationTokensPerEvent)
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "prefix default", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9319, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9320, Platform: PlatformAnthropic}
	svc := &GatewayService{}

	const turns = 20
	creationTurns, bothTurns := 0, 0
	creationValues := map[int]struct{}{}
	lastRead := 0
	for turn := 1; turn <= turns; turn++ {
		body := buildMultiTurnBody("prefix-default-session", turn)
		plan := svc.prepareCacheEmulationUsage(context.Background(), account, group, body,
			"claude-sonnet-4-6", 6000*turn)
		require.NotNil(t, plan)
		r := plan.result()
		if r == nil {
			// 前缀还没到可缓存下限的那几轮没有估算结果，不计入统计。
			plan.commit()
			continue
		}
		if r.CacheCreationInputTokens > 0 {
			creationTurns++
			creationValues[r.CacheCreationInputTokens] = struct{}{}
		}
		if r.CacheCreationInputTokens > 0 && r.CacheReadInputTokens > 0 {
			bothTurns++
		}
		require.GreaterOrEqual(t, r.CacheReadInputTokens, lastRead,
			"第 %d 轮 cache_read 回退：%d → %d", turn, lastRead, r.CacheReadInputTokens)
		lastRead = r.CacheReadInputTokens
		plan.commit()
	}

	// 第 1 轮前缀还没到可缓存下限（没有估算结果），第 2 轮才刚写入第一段前缀、
	// 没有可读的已缓存内容，所以 creation 的分母是 turns-1、both 的是 turns-2。
	require.GreaterOrEqual(t, creationTurns, turns-1,
		"默认值下每轮都该放行增量 creation，实际只有 %d/%d 轮", creationTurns, turns)
	require.GreaterOrEqual(t, bothTurns, turns-2,
		"read 与 create 同时为正的轮次只有 %d/%d", bothTurns, turns)
	require.Greater(t, len(creationValues), 10,
		"creation 只有 %d 个取值，每轮同一个数字一眼假", len(creationValues))
}

// 闸门归零后仍然留着的「窗口上限」是有副作用的，这里把它钉死成已知行为：
// 默认 600k/300s，一旦本窗口内累计放行量用完，后续轮次的 creation 会被压成 0
// （read 不受影响）。单轮 5 万量级的重会话大约十来轮就会撞上。
// 这是刻意保留的护栏（防一次上报离谱数值），要放宽得显式调
// max_creation_tokens_per_window —— 但别再把它误诊成闸门回归。
func TestCreationWindowBudgetStillThrottlesHeavySessions(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900309)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.AllowDerivedSession = true
	require.Equal(t, 600000, cfg.CreationControl.MaxCreationTokensPerWindow)
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "window budget", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9321, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9322, Platform: PlatformAnthropic}
	svc := &GatewayService{}

	released, throttled := 0, 0
	for turn := 1; turn <= 20; turn++ {
		body := buildMultiTurnBody("window-budget-session", turn)
		// 每轮 +6 万 token 的重会话：单轮放行量 5 万上下，很快吃掉 600k 窗口预算。
		plan := svc.prepareCacheEmulationUsage(context.Background(), account, group, body,
			"claude-sonnet-4-6", 60000*turn)
		require.NotNil(t, plan)
		r := plan.result()
		if r == nil {
			plan.commit()
			continue
		}
		if r.CacheCreationInputTokens > 0 {
			released++
		} else {
			throttled++
			require.Positive(t, r.CacheReadInputTokens, "窗口限流只影响 creation，read 必须照常")
		}
		plan.commit()
	}

	require.Positive(t, released, "窗口预算用完之前必须先正常放行")
	require.Positive(t, throttled, "600k/300s 的窗口预算在重会话里必须真的会命中")
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
// 生产默认值带着未缓存 input 下限 1024 与创建上限（单次 10 万 / 窗口 60 万），
// 而这些单测的请求体只有一两千 token，带着生产默认值跑，结果常常恒为 nil ——
// 测的就不是被测行为了。需要真实量级的用例请直接用 DefaultCacheStrategyConfig，
// 并把负载做到每轮几十 K。
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
	// 节流必须显式配：默认值里三个频率闸门已归零（见
	// TestPrefixDefaultReleasesCreationEveryTurn），靠默认值就测不到压制路径了。
	cfg.CreationControl.MinCreationIntervalSeconds = 6
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
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

	// 6 秒最小间隔意味着这组无等待调用里只有第 1 轮能上报创建，后面全被压制 ——
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

// 连续会话里每一轮都新增了真实内容，这部分必须计入 cache_creation，不能被抹平成 0。
//
// 缺陷形态：tracker 算出的 creation 是对的（实测每轮约 530 token），但
// constrainReportedCacheUsage 为了把 input_tokens 抬回 uncached 下限，会**先砍
// cache_creation**。read 早已占满上报总量，而 creation 体量小，于是被整个抹掉 ——
// cache_creation 从命中那一轮起恒为 0，本轮确实写入的新前缀被报成"全是命中"。
//
// 2026-09-16 实测（jinnyapi，22 轮真实会话）：cache_creation 只在前几轮非零，
// 之后一路为 0，而 cache_read 持续增长并把新增内容全吞了进去。
func TestGrowingTurnsReportCreationForNewContent(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900311)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindToolAware)
	cfg.AllowDerivedSession = true
	// 对齐线上策略，并关掉创建节流，把干扰项排除掉，只留「下限如何让位」这一个变量。
	cfg.CoverageRatio = 1
	cfg.CreationControl.Enabled = false
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	require.Positive(t, cfg.UncachedInputMinTokens, "没有 uncached 下限就触发不到让位逻辑")
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "growing turns", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9313, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9314, Platform: PlatformAnthropic}
	svc := &GatewayService{}

	creations := make([]int, 0, 8)
	reads := make([]int, 0, 8)
	for turn := 1; turn <= 8; turn++ {
		body := smallGrowthBody("growing-session", turn)
		plan := svc.prepareCacheEmulationUsage(
			context.Background(), account, group, body, "claude-sonnet-4-5-20250929", 0)
		require.NotNil(t, plan)
		if r := plan.result(); r != nil {
			creations = append(creations, r.CacheCreationInputTokens)
			reads = append(reads, r.CacheReadInputTokens)
		} else {
			creations = append(creations, 0)
			reads = append(reads, 0)
		}
		plan.commit()
	}

	// 只断言「已经命中」的那些轮次：命中之前没有 read 可让位，creation 本来就该是全量。
	checked := 0
	for i := range creations {
		if reads[i] <= 0 {
			continue
		}
		checked++
		require.Positive(t, creations[i],
			"第 %d 轮命中了缓存(read=%d)但 cache_creation=0 —— 本轮新增内容被抹掉了 (reads=%v creations=%v)",
			i+1, reads[i], reads, creations)
	}
	require.Positive(t, checked, "整场没有命中，这条用例没测到目标路径")
}

// smallGrowthBody 贴近实测的真实比例：约 8K 稳定前缀 + 每轮新增几百 token 的
// tool_use/tool_result。体量比 throttledGrowthBody 小得多，而「下限让位」恰恰只在
// 新增量远小于上报总量时才会把 creation 整个吃掉。
func smallGrowthBody(session string, turn int) []byte {
	system := strings.Repeat("You are a senior engineer working in a large Go repository. ", 420)
	messages := make([]map[string]any, 0, turn*3)
	for i := 1; i <= turn; i++ {
		messages = append(messages, map[string]any{
			"role": "user", "content": fmt.Sprintf("Round %d: please continue.", i),
		})
		if i < turn {
			messages = append(messages, map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Working on it."},
					{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", i), "name": "read_file",
						"input": map[string]any{"path": fmt.Sprintf("/src/mod_%d.go", i)}},
				},
			})
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": fmt.Sprintf("toolu_%d", i),
						"content": strings.Repeat("line of file content. ", 90)},
				},
			})
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5-20250929", "max_tokens": 64,
		"system":   system,
		"metadata": map[string]any{"user_id": session},
		"tools": []map[string]any{{
			"name": "read_file", "description": "Read a file from the repository.",
			"input_schema": map[string]any{"type": "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"}},
		}},
		"messages": messages,
	})
	return body
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

// 小上限是 sample_max 抖动最容易退化的地方：比例带 maxTokens/20 在 <=20 时
// floor 成 1，jitterWithin(1,1,…) 直接忽略 seed，于是每条触顶记录都是同一个值。
// 线上策略配 max_tokens=20，真实会话 22 轮里 input_tokens 22 次全是 19 —— 正是
// 抖动本该防住的「数值一动不动」。上面那个用例用 200 覆盖不到这一段。
func TestSampleMaxJitterStaysVariableAtSmallCaps(t *testing.T) {
	for _, maxTokens := range []int{5, 10, 20, 40} {
		seen := map[int]struct{}{}
		for seed := uint64(1); seed <= 500; seed++ {
			got := projectUsageField(CacheUsageFieldPolicy{
				Mode:      CacheUsageFieldSampleMax,
				MaxTokens: maxTokens,
			}, maxTokens*2, seed)
			require.Greater(t, got, 0, "上限 %d 的抖动结果必须为正", maxTokens)
			require.LessOrEqual(t, got, maxTokens, "sample_max 必须仍是天花板")
			seen[got] = struct{}{}
		}
		require.Greater(t, len(seen), 1,
			"上限 %d 触顶后只产生了 %d 种取值，抖动带已退化成单值", maxTokens, len(seen))
	}
}

func TestSampleTargetCacheCreationMatchesKiroBuckets(t *testing.T) {
	policy := CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleTarget,
		TargetTokens:        50000,
		NormalMaxMultiplier: 1.5,
	}

	seen := map[int]struct{}{}
	zeros := 0
	for seed := uint64(1); seed <= 200; seed++ {
		got := projectCacheCreationField(policy, 120000, 80000, seed)
		if got == 0 {
			zeros++
			continue
		}
		require.LessOrEqual(t, got, 75000)
		require.Greater(t, got, 0)
		seen[got] = struct{}{}
	}
	require.Greater(t, zeros, 10,
		"with an existing cache read, sample-target should retain kiro.rs's ~20%% zero bucket")
	require.Greater(t, len(seen), 10,
		"sample-target creation should vary across request fingerprints")

	require.Equal(t,
		projectCacheCreationField(policy, 120000, 80000, 17),
		projectCacheCreationField(policy, 120000, 80000, 17),
		"same request seed must be stable for retries")

	for seed := uint64(1); seed <= 20; seed++ {
		got := projectCacheCreationField(policy, 60000, 0, seed)
		require.Greater(t, got, 0, "cold sample-target creation should not use the read-only zero bucket")
		require.LessOrEqual(t, got, 60000)
	}
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

func TestUsageProjectionSeedVariesByRequestBodyButRetriesStayStable(t *testing.T) {
	first := &cacheEmulationPlan{
		cacheKey: 42,
		profile:  &cacheProfile{rawBody: []byte(`{"round":1}`), model: "m"},
	}
	retry := &cacheEmulationPlan{
		cacheKey: 42,
		profile:  &cacheProfile{rawBody: []byte(`{"round":1}`), model: "m"},
	}
	nextTurn := &cacheEmulationPlan{
		cacheKey: 42,
		profile:  &cacheProfile{rawBody: []byte(`{"round":2}`), model: "m"},
	}
	require.Equal(t, first.usageSeed(), retry.usageSeed())
	require.NotEqual(t, first.usageSeed(), nextTurn.usageSeed())

	policy := DefaultCacheUsagePolicy()
	policy.FinalCacheReadMaxTokens = 80000
	policy.FinalCacheReadJitterMinTokens = 12000
	policy.FinalCacheReadJitterMaxTokens = 24000
	a := &ClaudeUsage{InputTokens: 1, CacheReadInputTokens: 200000}
	b := &ClaudeUsage{InputTokens: 1, CacheReadInputTokens: 200000}
	projectClaudeUsage(a, nil, policy, first.usageSeed())
	projectClaudeUsage(b, nil, policy, nextTurn.usageSeed())
	require.NotEqual(t, a.CacheReadInputTokens, b.CacheReadInputTokens,
		"capped cache-read values must not be conversation-wide constants")
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

// preserve_upstream_cache_usage 必须真的能把上游真值放出来。
//
// 缺陷形态：判据写的是 `simulated == nil && policy.Preserve...`，而只要分组挂了
// 生效的策略，simulated 就必然非 nil —— 两个条件永不同真，开关恒不生效。UI 上
// 「优先保留上游缓存 usage」是个死开关，运维打开它什么也不会发生。
//
// 2026-09-16 实测（jinnyapi）：上游那轮下发 0/0/21，网关照样报 19/0/5300（253x）。
func TestPreserveUpstreamCacheUsageIsNotDeadWhenStrategyBound(t *testing.T) {
	policy := DefaultCacheUsagePolicy()
	policy.Enabled = true
	policy.PreserveUpstreamCacheUsage = true

	// 挂了策略 ⇒ 合成值必然存在，正是开关此前失效的场景。
	simulated := &cacheEmulationUsage{
		InputTokens: 19, CacheReadInputTokens: 0, CacheCreationInputTokens: 5300,
	}
	upstream := ClaudeUsage{
		InputTokens: 21, OutputTokens: 7,
		CacheReadInputTokens: 3638, CacheCreationInputTokens: 9356,
	}

	got := upstream
	projectClaudeUsage(&got, simulated, policy, 42)
	require.Equal(t, upstream.CacheReadInputTokens, got.CacheReadInputTokens,
		"preserve 开着且上游确有 cache_read，却被合成值覆盖了")
	require.Equal(t, upstream.CacheCreationInputTokens, got.CacheCreationInputTokens,
		"preserve 开着且上游确有 cache_creation，却被合成值覆盖了")

	// 反向：preserve 关掉时仍必须强制整形，否则上游某一档开始下发 cache 字段
	// 就会让上报曲线断档。
	policy.PreserveUpstreamCacheUsage = false
	forced := upstream
	projectClaudeUsage(&forced, simulated, policy, 42)
	require.Equal(t, simulated.CacheCreationInputTokens, forced.CacheCreationInputTokens,
		"preserve 关着时应当由策略决定上报值")

	// 上游没下发 cache 字段时，即便 preserve 开着也只能用合成值 —— 没有真值可保留。
	policy.PreserveUpstreamCacheUsage = true
	noEvidence := ClaudeUsage{InputTokens: 21, OutputTokens: 7}
	projectClaudeUsage(&noEvidence, simulated, policy, 42)
	require.Equal(t, simulated.CacheCreationInputTokens, noEvidence.CacheCreationInputTokens,
		"上游无 cache 字段时应当回落到合成值")
}

// uncached 下限的让位顺序在 constrain*UsageTotal 里也必须是「先 read 后 creation」。
//
// 这是根因 ② 的同构缺陷，只是换了个函数：constrainReportedCacheUsage 作用在合成值上，
// 而 constrain*UsageTotal 作用在**最终上报值**上（流式 gateway_upstream_response.go:1227、
// 非流式 :1578、passthrough gateway_anthropic_passthrough.go:642 都走它）。原实现
// 先砍 cache_creation：连续会话里 read 体量远大于每轮新增，creation 会被整个吃掉，
// 于是本轮确实写入的新前缀被报成"全是命中"。
//
// tool_aware 档把 MaxSimulatedInputTokens 置 0，totalCap<=0 直接返回，所以之前的
// 验证绕过了这里；prefix 档（本地模拟开启）会命中。
func TestUsageTotalFloorTakesRoomFromReadNotCreation(t *testing.T) {
	// deficit(1024-19=1005) 大于 creation(530)：先砍 creation 会把它整个抹成 0。
	claude := ClaudeUsage{
		InputTokens: 19, OutputTokens: 40,
		CacheReadInputTokens: 60000, CacheCreationInputTokens: 530,
		CacheCreation5mTokens: 530,
	}
	constrainClaudeUsageTotal(&claude, 80000, 1024)
	require.Equal(t, 530, claude.CacheCreationInputTokens,
		"cache_creation 被下限吃掉了：本轮真实写入在账单上消失 (usage=%+v)", claude)
	require.Equal(t, 58995, claude.CacheReadInputTokens, "让位应当来自 cache_read")
	require.GreaterOrEqual(t, claude.InputTokens, 1024, "uncached 下限没被顶回来")
	require.Equal(t, 530, claude.CacheCreation5mTokens, "分桶必须跟着总额，不能对不上")

	openai := OpenAIUsage{
		InputTokens:          19 + 60000 + 530,
		CacheReadInputTokens: 60000, CacheCreationInputTokens: 530,
	}
	constrainOpenAIUsageTotal(&openai, 80000, 1024)
	require.Equal(t, 530, openai.CacheCreationInputTokens,
		"OpenAI 侧同样不能拿 creation 补下限 (usage=%+v)", openai)
	require.Equal(t, 58995, openai.CacheReadInputTokens, "让位应当来自 cache_read")
	require.Equal(t, openai.InputTokens,
		openai.CacheReadInputTokens+openai.CacheCreationInputTokens+
			max(openai.InputTokens-openai.CacheReadInputTokens-openai.CacheCreationInputTokens, 0),
		"OpenAI 的 input_tokens 是含缓存的总额，三者必须自洽")

	// read 不够让位时才允许动 creation —— 否则 input 永远顶不到下限。
	thin := ClaudeUsage{InputTokens: 0, CacheReadInputTokens: 100, CacheCreationInputTokens: 5000}
	constrainClaudeUsageTotal(&thin, 6000, 1024)
	require.Zero(t, thin.CacheReadInputTokens, "read 应当先被掏空")
	require.Equal(t, 4076, thin.CacheCreationInputTokens, "read 不够时由 creation 补齐差额")
	require.Equal(t, 1924, thin.InputTokens)
}

// 走真实上报链路（prepareCachePlanForContext → mergeAndCommitCachePlan）复现根因 ④：
// 策略把 usage.input 压到 sample_max=20，投影后 input≈19 远低于 1024 的 uncached 下限，
// 补差额时若先砍 creation，每轮那 500 多 token 的真实新增会被整个抹掉。
//
// 对齐线上策略 755 的形态（用户原话：「我压制了最大值为20」）。
func TestReportedUsageKeepsCreationWhenInputFloorKicksIn(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900321)
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.AllowDerivedSession = true
	cfg.CoverageRatio = 1
	cfg.MinCacheableTokens = 1
	cfg.CreationControl.Enabled = false
	cfg.Usage.Input = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 20}
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	floor := uncachedInputFloor(cfg)
	require.Positive(t, floor.min, "没有 uncached 下限就触发不到让位逻辑")
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "input floor", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9317, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9318, Platform: PlatformAnthropic}

	creations := make([]int, 0, 8)
	reads := make([]int, 0, 8)
	for turn := 1; turn <= 8; turn++ {
		body := smallGrowthBody("input-floor-session", turn)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
		plan := prepareCachePlanForContext(context.Background(), ctx, account, group, body,
			"claude-sonnet-4-5-20250929", "anthropic_messages", 0)
		require.NotNil(t, plan)
		// 上游不下发 cache 字段（Kiro FREE 档的真实形态），走强制整形这条路。
		usage := &ClaudeUsage{InputTokens: 9000 + 600*turn, OutputTokens: 300}
		mergeAndCommitCachePlan(ctx, usage, true)
		creations = append(creations, usage.CacheCreationInputTokens)
		reads = append(reads, usage.CacheReadInputTokens)
		require.GreaterOrEqual(t, usage.InputTokens, floor.min,
			"第 %d 轮 input=%d 低于下限，这条用例没测到目标分支", turn, usage.InputTokens)
	}

	checked := 0
	for i := range creations {
		if reads[i] <= 0 {
			continue
		}
		checked++
		require.Positive(t, creations[i],
			"第 %d 轮命中缓存(read=%d)但上报 cache_creation=0 —— 下限补差额时吃掉了本轮新增 (reads=%v creations=%v)",
			i+1, reads[i], reads, creations)
	}
	require.Positive(t, checked, "整场没有命中，这条用例没测到目标路径")
}

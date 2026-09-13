//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ⚠️ 缓存策略是**分组级**能力（group.CacheStrategyID），不是平台级的。
// 本文件守住的是「Cursor 账号没有被排除在这条通用链路之外」。
//
// 这类缺口不会报错：策略照常保存、页面照常显示已绑定，只是缓存恒为 0，
// 用户按全价付费却以为自己开了缓存。只有实跑一遍策略才能证伪。

// buildCursorCacheBody 构造一个带稳定长前缀的多轮 Anthropic 请求体。
//
// ⚠️ 负载必须够大：小负载在 MinCacheableTokens 之下会被直接判为不可缓存，
// 测出来「缓存没生效」是假阳性，结论完全失真。
func buildCursorCacheBody(session string, turn int) []byte {
	stable := strings.Repeat("你是一个严谨的代码助手，回答前先复核事实。", 400)

	msgs := []map[string]any{}
	for i := 1; i <= turn; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": fmt.Sprintf("%s 第 %d 轮问题", stable, i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("第 %d 轮回答", i)},
		)
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4.5",
		"system":   stable,
		"messages": msgs,
		"metadata": map[string]any{"user_id": session},
	})
	return body
}

func putCursorCacheStrategy(t *testing.T, id int64) {
	t.Helper()
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindToolAware)
	cfg.MinCacheableTokens = 1024
	cfg.AllowDerivedSession = true
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: id, Name: "cursor cache", Enabled: true, Revision: 1, Config: cfg,
	})
	t.Cleanup(func() { GlobalCacheStrategyRegistry().Delete(id) })
}

// 绑定了缓存策略的 Cursor 分组必须真的产出缓存计划。
//
// nil 计划意味着这条链路在 Cursor 上是死的——策略配了也白配。
func TestCursorAccountGetsCachePlan(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(970101)
	putCursorCacheStrategy(t, strategyID)

	group := &Group{ID: 9702, Platform: PlatformCursor, CacheStrategyID: &strategyID}
	account := &Account{ID: 9703, Platform: PlatformCursor}
	svc := &GatewayService{}

	plan := svc.prepareCacheEmulationUsage(context.Background(), account, group,
		buildCursorCacheBody("cursor-plan", 1), "claude-sonnet-4.5", 20000)

	require.NotNil(t, plan, "Cursor 分组没有产出缓存计划：缓存策略在该平台上完全失效")
}

// 多轮对话下缓存必须真正命中，而不只是「计划非 nil」。
func TestCursorCacheActuallyHitsAcrossTurns(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(970201)
	putCursorCacheStrategy(t, strategyID)

	group := &Group{ID: 9704, Platform: PlatformCursor, CacheStrategyID: &strategyID}
	account := &Account{ID: 9705, Platform: PlatformCursor}
	svc := &GatewayService{}

	hits := 0
	for turn := 1; turn <= 10; turn++ {
		plan := svc.prepareCacheEmulationUsage(context.Background(), account, group,
			buildCursorCacheBody("cursor-multiturn", turn), "claude-sonnet-4.5", 20000*turn)
		require.NotNil(t, plan)
		if r := plan.result(); r != nil &&
			(r.CacheReadInputTokens > 0 || r.CacheCreationInputTokens > 0) {
			hits++
		}
		plan.commit()
	}

	require.Greater(t, hits, 1, "Cursor 多轮对话没有产生任何缓存命中")
}

// 缓存结果必须真的写进回给客户端、并最终落库的 usage 字段。
//
// ⚠️ 这是「缓存生效」与「缓存被计费」之间的分界：计划算得再对，
// 不合并进 ClaudeUsage 就等于没发生——usage_logs 的 cache_* 列会恒为 0。
func TestCursorCacheTokensReachUsage(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(970301)
	putCursorCacheStrategy(t, strategyID)

	group := &Group{ID: 9706, Platform: PlatformCursor, CacheStrategyID: &strategyID}
	account := &Account{ID: 9707, Platform: PlatformCursor}

	var merged ClaudeUsage
	for turn := 1; turn <= 6; turn++ {
		c, _ := gin.CreateTestContext(httptest.NewRecorder()) // 每轮独立请求上下文
		body := buildCursorCacheBody("cursor-usage", turn)
		plan := prepareCachePlanForContext(context.Background(), c, account, group, body,
			"claude-sonnet-4.5", "anthropic_messages", 20000*turn)
		require.NotNil(t, plan)

		usage := ClaudeUsage{InputTokens: 20000 * turn, OutputTokens: 100}
		mergeAndCommitCachePlan(c, &usage, true)
		if usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0 {
			merged = usage
		}
	}

	require.True(t,
		merged.CacheReadInputTokens > 0 || merged.CacheCreationInputTokens > 0,
		"缓存 token 没有进入 ClaudeUsage：usage_logs 的 cache_* 列会恒为 0，用户按全价计费")
}

// 未绑定策略的 Cursor 分组不得凭空产生缓存计费。
func TestCursorWithoutStrategyHasNoSyntheticCache(t *testing.T) {
	resetCacheTracker()
	group := &Group{ID: 9708, Platform: PlatformCursor} // 未绑定策略
	account := &Account{ID: 9709, Platform: PlatformCursor}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := buildCursorCacheBody("cursor-nostrategy", 3)
	prepareCachePlanForContext(context.Background(), c, account, group, body,
		"claude-sonnet-4.5", "anthropic_messages", 20000)

	usage := ClaudeUsage{InputTokens: 20000, OutputTokens: 100}
	mergeAndCommitCachePlan(c, &usage, true)

	require.Zero(t, usage.CacheCreationInputTokens,
		"未绑定缓存策略却产生了缓存创建计费")
}

package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 用真实抓包数据驱动的回归测试。
//
// 为什么不能只用手写 mock：此前多轮本地 mock 都测通了，真实上游却有问题 ——
// mock 里上游返回的 usage 是我们假设的，而真实上游自己做过缓存计算，会把数值压得
// 很小（实测 0/0/21、514/1320/0 这种量级），算法各家还不一样。请求体这一侧同理：
// 内置提示词、15 个工具定义、tool_result 全文、cache_control 的位置与 ttl 都会
// 影响断点推导，任何裁剪都会让 token 量级失真、结论跟着失真。
//
// fixture：kiro-capture 抓的 jinnyapi 真实会话（seq 34-45，12 轮连续对话），
// 凭证已脱敏，请求体未做任何裁剪。
type capturedTurn struct {
	Seq           int             `json:"seq"`
	Body          json.RawMessage `json:"body"`
	UpstreamUsage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"upstream_usage"`
	// 上游未做缓存扣减前的真实输入量，用来判断上报值有没有脱离量级。
	KiroActualInputTokens int `json:"kiro_actual_input_tokens"`
}

func loadCapturedSession(t *testing.T) []capturedTurn {
	t.Helper()
	f, err := os.Open("testdata/real_session_jinnyapi12.json.gz")
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer zr.Close()
	var doc struct {
		Turns []capturedTurn `json:"turns"`
	}
	require.NoError(t, json.NewDecoder(zr).Decode(&doc))
	require.Len(t, doc.Turns, 12, "fixture 轮数变了，形态门槛的判定依赖连续 12 轮")
	return doc.Turns
}

// 线上策略 755 的形态：prefix 档 + usage.input 压到 sample_max=20
// （用户原话「我压制了最大值为20」），正是「数据死板」被观测到的那套配置。
func capturedSessionStrategyConfig(t *testing.T) CacheStrategyConfig {
	t.Helper()
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.AllowDerivedSession = true
	cfg.CoverageRatio = 1
	cfg.MinCacheableTokens = 1
	cfg.CreationControl.Enabled = false
	cfg.Usage.Input = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax, MaxTokens: 20}
	cfg, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	return cfg
}

func replayCapturedSession(t *testing.T, cfg CacheStrategyConfig, strategyID int64, turns []capturedTurn) []ClaudeUsage {
	t.Helper()
	resetCacheTracker()
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "captured replay", Enabled: true, Revision: 1, Config: cfg,
	})
	t.Cleanup(func() { GlobalCacheStrategyRegistry().Delete(strategyID) })

	group := &Group{ID: 9331, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9332, Platform: PlatformAnthropic}

	reported := make([]ClaudeUsage, 0, len(turns))
	for _, turn := range turns {
		body := []byte(turn.Body)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest("POST", "/v1/messages?beta=true", bytes.NewReader(body))
		plan := prepareCachePlanForContext(context.Background(), ctx, account, group, body,
			"claude-haiku-4-5-20251001", "anthropic_messages", turn.KiroActualInputTokens)
		require.NotNil(t, plan, "第 %d 条抓包请求没建出缓存计划", turn.Seq)

		// 上游真值原样喂进去，不做任何美化。
		usage := &ClaudeUsage{
			InputTokens:              turn.UpstreamUsage.InputTokens,
			OutputTokens:             turn.UpstreamUsage.OutputTokens,
			CacheReadInputTokens:     turn.UpstreamUsage.CacheReadInputTokens,
			CacheCreationInputTokens: turn.UpstreamUsage.CacheCreationInputTokens,
		}
		mergeAndCommitCachePlan(ctx, usage, true)
		reported = append(reported, *usage)
	}
	return reported
}

// 形态门槛：强制整形（preserve 关）时，上报曲线不能「死板」。
//
// 「死板」在这里有可判定的定义，不靠主观感受：
//  1. input_tokens 整场只有一个取值（实测恒为 19）
//  2. cache_creation 只在头一两轮非零，之后恒为 0
//  3. cache_read 与 cache_creation 从不在同一轮同时出现
//
// 三条任一成立，客户看账单就会不认账。
func TestCapturedSessionReportsNonRigidUsageShape(t *testing.T) {
	turns := loadCapturedSession(t)
	cfg := capturedSessionStrategyConfig(t)
	// 强制整形这条默认路径：上报值完全由策略决定。
	cfg.PreserveUpstreamCacheUsage = false
	cfg.Usage.PreserveUpstreamCacheUsage = false

	reported := replayCapturedSession(t, cfg, 900341, turns)

	inputs := map[int]int{}
	hitTurns, creationTurns, bothTurns := 0, 0, 0
	for i, u := range reported {
		inputs[u.InputTokens]++
		if u.CacheReadInputTokens > 0 {
			hitTurns++
			require.Positive(t, u.CacheCreationInputTokens,
				"第 %d 轮（抓包 seq=%d）命中缓存(read=%d)但 cache_creation=0 —— 本轮真实新增被抹掉了",
				i+1, turns[i].Seq, u.CacheReadInputTokens)
		}
		if u.CacheCreationInputTokens > 0 {
			creationTurns++
		}
		if u.CacheReadInputTokens > 0 && u.CacheCreationInputTokens > 0 {
			bothTurns++
		}
		require.LessOrEqual(t, u.CacheCreation5mTokens+u.CacheCreation1hTokens, u.CacheCreationInputTokens,
			"第 %d 轮分桶之和超过 cache_creation 总额，下游计费会算错", i+1)
	}

	require.Greater(t, len(inputs), 1,
		"整场 %d 轮 input_tokens 只有 %d 个取值 —— 这就是「恒为 19」的死板形态 (%v)",
		len(reported), len(inputs), inputs)
	require.Greater(t, hitTurns, 1, "抓包会话本身要能命中缓存，否则这条用例没测到目标路径")
	require.Greater(t, creationTurns, 2,
		"cache_creation 只在 %d 轮非零 —— 退化成「只有头一两轮有写入」", creationTurns)
	require.Positive(t, bothTurns, "read 与 creation 从不同轮同现，缓存写入在账单上不可见")
}

// preserve 打开时，上游真值必须原样上报。
//
// 这一条用真实抓包的上游 usage 兜住根因 ③：判据曾写成
// `simulated == nil && policy.Preserve...`，而挂了生效策略 simulated 必然非 nil，
// 开关恒不生效。抓包里上游给的是 514/1320/0 这种量级，被合成值覆盖后会放大两个数量级。
func TestCapturedSessionPreservesUpstreamUsageWhenEnabled(t *testing.T) {
	turns := loadCapturedSession(t)
	cfg := capturedSessionStrategyConfig(t)
	cfg.PreserveUpstreamCacheUsage = true
	cfg.Usage.PreserveUpstreamCacheUsage = true

	reported := replayCapturedSession(t, cfg, 900342, turns)

	checked := 0
	for i, u := range reported {
		up := turns[i].UpstreamUsage
		if up.CacheReadInputTokens <= 0 && up.CacheCreationInputTokens <= 0 {
			// 上游没下发 cache 字段，没有真值可保留 —— 这几轮本就该走合成值。
			continue
		}
		checked++
		require.Equal(t, up.CacheReadInputTokens, u.CacheReadInputTokens,
			"第 %d 轮（seq=%d）上游 cache_read=%d 被合成值覆盖成 %d",
			i+1, turns[i].Seq, up.CacheReadInputTokens, u.CacheReadInputTokens)
		require.Equal(t, up.CacheCreationInputTokens, u.CacheCreationInputTokens,
			"第 %d 轮（seq=%d）上游 cache_creation=%d 被合成值覆盖成 %d",
			i+1, turns[i].Seq, up.CacheCreationInputTokens, u.CacheCreationInputTokens)
	}
	require.Greater(t, checked, 5, "抓包里带 cache 字段的轮次太少，这条用例没测到目标路径")
}

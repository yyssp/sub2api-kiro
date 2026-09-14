//go:build unit

package service

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

// buildKiroRequestContext 构造一个真实走过守卫的 requestCtx。
// 直接手搓 KiroRequestContext 不行 —— PayloadTrim 是包内类型，跨包造不出来。
func buildKiroRequestContext(t *testing.T, msgCount, blobLen int, weightLimit string) kiropkg.KiroRequestContext {
	t.Helper()
	if weightLimit != "" {
		t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", weightLimit)
	}
	blob := strings.Repeat("x", blobLen)
	var msgs []map[string]any
	for i := 0; i < msgCount; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": blob})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "done?"})
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5-20250929", "max_tokens": 100, "messages": msgs,
	})
	require.NoError(t, err)

	res, err := kiropkg.BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "arn:test", "AI_EDITOR", nil)
	require.NoError(t, err)
	return res.Context
}

// G1-B 的核心：被裁剪过的响应必须留痕。
//
// 没有这些头，"裁掉半部历史"和"本来就没超限"在调用方看来完全一样（都是 200），
// 用户拿到一个自信但缺上下文的答案却无从察觉。
func TestSetKiroPayloadTrimHeadersMarksTrimmedResponse(t *testing.T) {
	requestCtx := buildKiroRequestContext(t, 60, 8000, "200000")
	stats := requestCtx.PayloadTrimStats()
	require.True(t, stats.Triggered(), "前置条件：必须真的触发了守卫")

	header := http.Header{}
	setKiroPayloadTrimHeaders(header, requestCtx)

	require.NotEmpty(t, header.Get(kiroTrimHeaderTrimmed))
	require.Equal(t, "true", header.Get(kiroTrimHeaderTrimmed), "丢了整轮历史时必须是 true")
	require.NotEqual(t, "0", header.Get(kiroTrimHeaderDroppedItems))
	require.NotEmpty(t, header.Get(kiroTrimHeaderOriginalBytes))
	require.NotEmpty(t, header.Get(kiroTrimHeaderFinalBytes))
	require.Contains(t, header.Get(kiroTrimHeaderStages), "history_trim")

	// final 必须真的比 original 小，否则这组头是在撒谎。
	require.Less(t, header.Get(kiroTrimHeaderFinalBytes), header.Get(kiroTrimHeaderOriginalBytes))
}

// 未触发时一个头都不许加 —— 绝大多数请求走这条路径。
func TestSetKiroPayloadTrimHeadersSilentWhenUntriggered(t *testing.T) {
	requestCtx := buildKiroRequestContext(t, 1, 10, "")
	require.False(t, requestCtx.PayloadTrimStats().Triggered())

	header := http.Header{}
	setKiroPayloadTrimHeaders(header, requestCtx)
	require.Empty(t, header, "未超限时不得添加任何响应头")
}

// nil header 不得 panic：调用点在响应路径上，panic 等于把请求打挂。
func TestSetKiroPayloadTrimHeadersNilSafe(t *testing.T) {
	requestCtx := buildKiroRequestContext(t, 60, 8000, "200000")
	require.NotPanics(t, func() { setKiroPayloadTrimHeaders(nil, requestCtx) })
}

// 仅压缩（历史没丢）时 trimmed 必须是 false。
// 两种严重程度不能混为一谈：压缩只是截断工具输出，整轮对话仍在。
func TestSetKiroPayloadTrimHeadersDistinguishesCompressFromTrim(t *testing.T) {
	t.Setenv("KIRO_MAX_PAYLOAD_WEIGHT", "60000")

	// 多轮工具调用累积：每条都在翻译阶段的 12000 字符闸门以下（不会被提前压掉），
	// 但合计足以超限。这才是本阶段真正要解决的场景。
	msgs := []map[string]any{
		{"role": "user", "content": "第一轮暗号 ALPHA-7"},
	}
	for i := 0; i < 12; i++ {
		id := "tu_" + strconv.Itoa(i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": id, "name": "read_file", "input": map[string]any{"path": "/a"}},
			}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": id, "content": strings.Repeat("L", 5000)},
			}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "继续"})

	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5-20250929", "max_tokens": 100, "messages": msgs,
	})
	require.NoError(t, err)

	res, err := kiropkg.BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "arn:test", "AI_EDITOR", nil)
	require.NoError(t, err)

	stats := res.Context.PayloadTrimStats()
	require.True(t, stats.Compressed, "必须先压缩")
	require.False(t, stats.Trimmed, "压缩够用时不得丢历史")

	header := http.Header{}
	setKiroPayloadTrimHeaders(header, res.Context)
	require.Equal(t, "false", header.Get(kiroTrimHeaderTrimmed),
		"仅压缩时 trimmed 必须是 false —— 上下文并未丢失")
	require.Equal(t, "0", header.Get(kiroTrimHeaderDroppedItems))
	require.NotEqual(t, "0", header.Get(kiroTrimHeaderCompressedItems))
	require.Contains(t, header.Get(kiroTrimHeaderStages), "history_tool_results")
}

// on_upstream_400 预检路径：超阈值但守卫刻意没动负载，因此一个裁剪头都不许加。
//
// 真实上游验证时发现的回归：这条路径曾复用 StillOversized 标记，导致响应带上
// 一组 original==final 的裁剪头，与"真的裁掉了历史"无法区分。
func TestOnUpstream400PreflightEmitsNoTrimHeaders(t *testing.T) {
	blob := strings.Repeat("x", 8000)
	var msgs []map[string]any
	for i := 0; i < 60; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": blob})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"})
	}
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5-20250929", "max_tokens": 100, "messages": msgs,
	})
	require.NoError(t, err)

	res, err := kiropkg.BuildKiroPayloadWithGuard(body, "claude-sonnet-4.5", "arn:test", "AI_EDITOR", nil,
		kiropkg.KiroPayloadGuardConfig{MaxWeight: 200000, Behavior: kiropkg.KiroOversizeOnUpstream400})
	require.NoError(t, err)

	stats := res.Context.PayloadTrimStats()
	require.True(t, stats.DeferredToUpstream, "前置条件：必须真的走了 defer 路径")

	header := http.Header{}
	setKiroPayloadTrimHeaders(header, res.Context)
	require.Empty(t, header, "守卫没动过负载，不得添加任何裁剪头")
}

// reject 行为必须返回可判别的错误类型，供 handler 映射成 413 而不是 502。
// 请求根本没发出去，回 502 会让用户去排查上游。
func TestKiroRejectBehaviorSurfacesTypedError(t *testing.T) {
	blob := strings.Repeat("x", 8000)
	var msgs []map[string]any
	for i := 0; i < 60; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": blob})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"})
	}
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5-20250929", "max_tokens": 100, "messages": msgs,
	})
	require.NoError(t, err)

	_, err = kiropkg.BuildKiroPayloadWithGuard(body, "claude-sonnet-4.5", "arn:test", "AI_EDITOR", nil,
		kiropkg.KiroPayloadGuardConfig{MaxWeight: 200000, Behavior: kiropkg.KiroOversizeReject})

	require.Error(t, err)
	var tooLarge *kiropkg.ErrKiroPayloadTooLarge
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, 200000, tooLarge.Limit)
	require.Greater(t, tooLarge.Weight, tooLarge.Limit)
}

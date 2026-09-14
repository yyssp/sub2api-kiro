//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/stretchr/testify/require"
)

// 守卫通知头绝不能被上游头白名单过滤掉。
//
// 流式路径的形态是：openKiroAnthropicStreamResponse 把 x-sub2api-context-*
// 写进它返回的 resp.Header，handleStreamingResponse 再用
// responseheaders.WriteFilteredHeaders 往客户端转发。那个过滤器是
// **白名单**语义(见 FilterHeaders: 不在 allowed 里就 continue)，
// 而这几个头是我们自己加的、不是上游回的，因此会被整组丢掉。
//
// 后果是最坏的一种：裁掉半部历史照样返回 200，响应上没有任何痕迹，
// 调用方拿到一个自信但缺上下文的答案。2026-09-14 真实上游实测复现：
// 服务端 dropped_history_items=14，客户端收到的响应头里
// 一个 x-sub2api-context-* 都没有。
func TestKiroTrimHeadersSurviveUpstreamHeaderFilter(t *testing.T) {
	// 复现过滤器对这几个头的真实判定。
	filter := responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{})
	upstream := http.Header{}
	upstream.Set(kiroTrimHeaderTrimmed, "true")
	upstream.Set(kiroTrimHeaderDroppedItems, "14")
	upstream.Set(kiroTrimHeaderStages, "history_trim")

	passed := responseheaders.FilterHeaders(upstream, filter)
	require.Empty(t, passed.Get(kiroTrimHeaderTrimmed),
		"前提校验：白名单本就会丢掉这些头，所以不能依赖它转发")

	// 修复的做法：调用 handleStreamingResponse 之前手工搬到 gin 的 writer 上。
	client := http.Header{}
	copyKiroTrimHeaders(client, upstream)

	require.Equal(t, "true", client.Get(kiroTrimHeaderTrimmed),
		"裁剪发生了却没有任何响应头，调用方无从察觉上下文已残缺")
	require.Equal(t, "14", client.Get(kiroTrimHeaderDroppedItems))
	require.Equal(t, "history_trim", client.Get(kiroTrimHeaderStages))
}

// 未触发守卫时不得凭空加头，否则客户端会误以为每次都被裁了。
func TestKiroTrimHeadersAbsentWhenNotTriggered(t *testing.T) {
	client := http.Header{}
	copyKiroTrimHeaders(client, http.Header{})

	for _, name := range kiroTrimHeaderNames {
		require.Empty(t, client.Get(name),
			"守卫未触发时 %s 不该出现", name)
	}
}

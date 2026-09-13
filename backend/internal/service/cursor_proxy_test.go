//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ⚠️ Cursor 按「设备指纹 + 出口 IP」做风控。账号级代理把号池分散到不同出口，
// 是避免整池被连带判定异常的唯一手段。这条链路全是 string 赋值，
// 漏接不会报错：请求照常成功，只是出口 IP 不对，不抓包发现不了。

func TestCursorAccountProxyURL_ReturnsBoundProxy(t *testing.T) {
	proxyID := int64(9)
	account := &Account{
		ID:      1,
		ProxyID: &proxyID,
		Proxy: &Proxy{
			Protocol: "http",
			Host:     "10.0.0.5",
			Port:     8080,
		},
	}
	require.Equal(t, "http://10.0.0.5:8080", cursorAccountProxyURL(account))
}

// ⚠️ 只有 ProxyID 而 Proxy 关联没预加载时必须返回空串，不能解引用。
//
// 这不是假想情况：账号是否带 Proxy 取决于查询有没有 Preload，
// 不同调用路径并不一致。少一个判空就是一次线上 panic。
func TestCursorAccountProxyURL_ProxyIDWithoutRelationIsSafe(t *testing.T) {
	proxyID := int64(9)
	require.NotPanics(t, func() {
		require.Empty(t, cursorAccountProxyURL(&Account{ID: 1, ProxyID: &proxyID}))
	})
}

func TestCursorAccountProxyURL_NoProxyMeansDirect(t *testing.T) {
	require.Empty(t, cursorAccountProxyURL(&Account{ID: 1}))
	require.Empty(t, cursorAccountProxyURL(nil))
}

// ⚠️ 代理必须真的进到协议层账号里。
//
// cursorProtocolAccount 是业务层账号到协议层账号的唯一映射点，
// 漏掉这一个字段赋值，前面所有代理逻辑都拿不到地址，等于全部空转。
func TestCursorProtocolAccount_CarriesProxyURL(t *testing.T) {
	proxyID := int64(3)
	account := &Account{
		ID:          77,
		Platform:    PlatformCursor,
		Schedulable: true,
		ProxyID:     &proxyID,
		Proxy: &Proxy{
			Protocol: "socks5",
			Host:     "proxy.internal",
			Port:     1080,
			Username: "u",
			Password: "p",
		},
	}

	got := cursorProtocolAccount(account)
	require.Equal(t, "socks5://u:p@proxy.internal:1080", got.ProxyURL,
		"协议层账号必须带上代理地址，否则账号级代理对 Cursor 完全不生效")
}

func TestCursorProtocolAccount_NoProxyLeavesEmpty(t *testing.T) {
	got := cursorProtocolAccount(&Account{ID: 78, Platform: PlatformCursor, Schedulable: true})
	require.Empty(t, got.ProxyURL)
}

package kiro

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsKiroRemoteAddressAllowed(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", // 云元数据
		"100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fc00::1",
		"::ffff:127.0.0.1", // IPv4-mapped 回环
		"64:ff9b::a00:1",   // NAT64 映射到 10.0.0.1
	}
	for _, raw := range blocked {
		require.False(t, isKiroRemoteAddressAllowed(netip.MustParseAddr(raw)), raw)
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		require.True(t, isKiroRemoteAddressAllowed(netip.MustParseAddr(raw)), raw)
	}
}

// 生产 client 必须在建连时拒绝回环地址，而不是只校验 URL 字面量。
func TestKiroRemoteFetchClientBlocksLoopback(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	_, err = newKiroRemoteFetchClient(kiroRemoteImageTimeout).Do(req)
	require.ErrorIs(t, err, errKiroRemoteAddressBlocked)
	require.False(t, hit, "请求不得到达内网目标")
}

// 跳转到内网由建连校验拦截（每一跳都重新拨号）；这里只锁定跳转次数与协议限制。
func TestKiroRemoteFetchClientRedirectPolicy(t *testing.T) {
	client := newKiroRemoteFetchClient(kiroRemoteImageTimeout)
	require.NotNil(t, client.CheckRedirect)
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/a.png", nil)
	require.Error(t, client.CheckRedirect(req, make([]*http.Request, kiroRemoteImageMaxRedirects)))
	ftp, _ := http.NewRequest(http.MethodGet, "ftp://example.com/a.png", nil)
	require.Error(t, client.CheckRedirect(ftp, nil))
}

func TestBuildKiroImageFromRemoteURLRejectsInternalTarget(t *testing.T) {
	_, ok := buildKiroImageFromURL("http://169.254.169.254/latest/meta-data/")
	require.False(t, ok)
}

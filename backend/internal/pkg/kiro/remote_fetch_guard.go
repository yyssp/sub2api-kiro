package kiro

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// 远程图片 URL 来自下游请求体，任何调用方都能填。直接用默认 http.Client 拉取，
// 等于让调用方借网关探测内网（含云元数据 169.254.169.254）。
//
// 校验放在 Dialer.Control：此时拿到的是真实要连的 IP，DNS rebinding 与
// 重定向到内网都会在建连时被拦下，不存在先校验后解析的 TOCTOU 窗口。
// 不走环境代理：经代理时目标由代理解析，网关无法校验真实目标。

const kiroRemoteImageMaxRedirects = 3

var errKiroRemoteAddressBlocked = errors.New("kiro remote fetch: destination address is not allowed")

var kiroRemoteBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 可映射到内网 IPv4
}

func isKiroRemoteAddressAllowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return false
	}
	for _, prefix := range kiroRemoteBlockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// kiroRemoteAddressAllowed 是建连时的判定入口；测试用它放行 httptest 的回环地址。
var kiroRemoteAddressAllowed = isKiroRemoteAddressAllowed

func kiroRemoteDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !kiroRemoteAddressAllowed(addr) {
		return fmt.Errorf("%w: %s", errKiroRemoteAddressBlocked, host)
	}
	return nil
}

func newKiroRemoteFetchClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, Control: kiroRemoteDialControl}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= kiroRemoteImageMaxRedirects {
				return errors.New("kiro remote fetch: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("kiro remote fetch: redirect scheme %q not allowed", req.URL.Scheme)
			}
			return nil
		},
	}
}

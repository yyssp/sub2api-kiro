package cursor

import (
	"net/http"
	"net/url"
	"testing"
)

// ⚠️ Cursor 按「设备指纹 + 出口 IP」做风控：整个号池从同一个出口 IP 打过去
// 容易被连带判定异常。账号级代理是把号池分散到不同出口的唯一手段。
//
// 这条链路上的每一处都没有编译期保护：字段是 string，缓存键是 struct，
// 漏接只会让管理端配好的代理静默失效——请求照常成功，只是出口 IP 不对，
// 不抓包根本发现不了。

// transportOf 取出 client 的 *http.Transport，便于断言代理配置。
func transportOf(t *testing.T, cl *http.Client) *http.Transport {
	t.Helper()
	tr, ok := cl.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型为 %T，期望 *http.Transport", cl.Transport)
	}
	return tr
}

// ⚠️ 这是本组最重要的一条：连接池按账号缓存，而代理配在 Transport 上。
//
// 缓存键如果只有账号 ID，管理端给账号改绑代理后会一直命中旧连接池，
// 新代理永远不生效，且没有任何报错。
func TestClientFor_ProxyChangeCreatesNewPool(t *testing.T) {
	c := NewClient()
	acct := &Account{ID: 42}

	direct := c.clientFor(acct)

	acct.ProxyURL = "http://127.0.0.1:18080"
	viaProxy := c.clientFor(acct)

	if direct == viaProxy {
		t.Fatal("改绑代理后仍复用旧连接池：新代理不会生效，且无任何报错")
	}
	if transportOf(t, viaProxy).Proxy == nil {
		t.Fatal("代理账号的 Transport.Proxy 为 nil，说明代理没接上")
	}
}

// 同账号 + 同代理必须复用连接池：每请求新建会让连接池失去意义，
// 并且 Cursor 上游按连接数收敛 MAX_CONCURRENT_STREAMS。
func TestClientFor_SameAccountSameProxyReusesPool(t *testing.T) {
	c := NewClient()
	acct := &Account{ID: 7, ProxyURL: "http://127.0.0.1:18080"}

	if c.clientFor(acct) != c.clientFor(acct) {
		t.Fatal("同账号同代理未复用连接池")
	}
}

// 不同账号即使共用同一个代理，也必须各自独立连接池：
// 共享会让它们互相争抢 MAX_CONCURRENT_STREAMS。
func TestClientFor_DifferentAccountsDoNotSharePool(t *testing.T) {
	c := NewClient()
	a := &Account{ID: 1, ProxyURL: "http://127.0.0.1:18080"}
	b := &Account{ID: 2, ProxyURL: "http://127.0.0.1:18080"}

	if c.clientFor(a) == c.clientFor(b) {
		t.Fatal("不同账号共用了连接池，会互相饿死 HTTP/2 流")
	}
}

// ⚠️ SOCKS5 必须改 DialContext 而不是 Proxy。
//
// 直接设 Transport.Proxy 对 SOCKS5 是静默无效的；并且 proxyurl.Parse 会把
// socks5:// 升级成 socks5h://，让 DNS 也在代理端解析。本地解析 DNS 会造成
// DNS 泄漏：出口 IP 换了，DNS 查询仍从本机发出。
func TestNewH2Client_SOCKS5ConfiguresDialer(t *testing.T) {
	viaSocks := newH2Client("socks5://127.0.0.1:11080")
	tr := transportOf(t, viaSocks)

	if tr.DialContext == nil {
		t.Fatal("SOCKS5 未设置 DialContext，代理不会生效")
	}
	// ⚠️ SOCKS5 走 DialContext，绝不能同时把 Proxy 设成该地址：
	// http.Transport 会把它当 HTTP 代理去 CONNECT，与 SOCKS 握手冲突。
	if tr.Proxy != nil {
		if u, err := tr.Proxy(&http.Request{URL: mustURL(t, "https://api2.cursor.sh/")}); err == nil && u != nil {
			t.Fatalf("SOCKS5 场景下 Transport.Proxy 仍返回 %s，会与 SOCKS 握手冲突", u)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// ⚠️ 非法代理地址必须退回直连而不是 panic/返回 nil client。
// 一个配错的代理不应该让账号彻底不可用。
func TestNewH2Client_InvalidProxyFallsBackToDirect(t *testing.T) {
	cl := newH2Client("://not-a-valid-url")
	if cl == nil {
		t.Fatal("非法代理导致返回 nil client")
	}
	if transportOf(t, cl) == nil {
		t.Fatal("非法代理导致 Transport 缺失")
	}
}

// 空代理必须保留 ProxyFromEnvironment：部署侧可能靠环境变量统一走代理，
// 直接置 nil 会把这条既有能力关掉。
func TestNewH2Client_EmptyProxyKeepsEnvironmentProxy(t *testing.T) {
	if transportOf(t, newH2Client("")).Proxy == nil {
		t.Fatal("空代理时 Transport.Proxy 为 nil：环境变量代理被静默关闭")
	}
}

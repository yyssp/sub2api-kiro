package cursor

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// 账号订阅信息（membership）来自 cursor.com 的网页端接口，用 web 会话 cookie 鉴权：
//
//	/api/usage-summary -> 权威源：membershipType
//	/api/auth/stripe   -> 兜底：usage-summary 缺 membershipType 时再取
//
// membership 决定 Free 套餐是否只能用 Auto（见 membershipDisallowsNamedModels），
// 建号后不补齐就一直为空，调度侧只能按「未知套餐」放行。

const dashboardUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// fetchMembershipFn 是 membership 拉取实现，测试经 SetFetchMembershipFnForTest 替换。
var fetchMembershipFn = fetchMembership

// FetchMembership 经账号绑定的代理拉取订阅类型（free / pro / ...）。
// session 是 uid::JWT 形态的会话；拉取失败返回 ok=false，调用方不应写空值覆盖已有记录。
func FetchMembership(session, proxyURL string) (membership string, ok bool) {
	return fetchMembershipFn(session, proxyURL)
}

// SetFetchMembershipFnForTest 替换 membership 拉取实现，仅供测试使用；
// 返回一个恢复原实现的函数。
func SetFetchMembershipFnForTest(fn func(session, proxyURL string) (string, bool)) func() {
	prev := fetchMembershipFn
	fetchMembershipFn = fn
	return func() { fetchMembershipFn = prev }
}

func fetchMembership(session, proxyURL string) (string, bool) {
	session = strings.TrimSpace(session)
	if session == "" {
		return "", false
	}
	cl := &http.Client{Timeout: 20 * time.Second}
	if strings.TrimSpace(proxyURL) != "" {
		cl.Transport = newH2Client(proxyURL).Transport
	}
	cookie := "WorkosCursorSessionToken=" + session
	if b, ok := getDashboardJSON(cl, cursorWebURL("/api/usage-summary"), cookie); ok {
		var d struct {
			MembershipType string `json:"membershipType"`
		}
		if json.Unmarshal(b, &d) == nil && strings.TrimSpace(d.MembershipType) != "" {
			return strings.TrimSpace(d.MembershipType), true
		}
	}
	if b, ok := getDashboardJSON(cl, cursorWebURL("/api/auth/stripe"), cookie); ok {
		var st struct {
			MembershipType string `json:"membershipType"`
		}
		if json.Unmarshal(b, &st) == nil && strings.TrimSpace(st.MembershipType) != "" {
			return strings.TrimSpace(st.MembershipType), true
		}
	}
	return "", false
}

func getDashboardJSON(cl *http.Client, url, cookie string) ([]byte, bool) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", dashboardUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://cursor.com")
	req.Header.Set("Referer", "https://cursor.com/dashboard")
	applyCursorProxyAuth(req)
	resp, err := cl.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	return body, true
}

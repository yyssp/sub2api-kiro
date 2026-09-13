package cursor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

const cursorAuthRefreshSkew = 5 * time.Minute

// Cursor OAuth client id observed in public reverse-engineered clients.
// Keep it overridable so upstream changes do not require a code patch.
var cursorOAuthClientID = envOr("CURSOR_OAUTH_CLIENT_ID", "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB")

var (
	cursorAuthRefreshHTTPClient = &http.Client{Timeout: 20 * time.Second}
	cursorAuthRefreshFn         = refreshCursorAuth

	// 刷新链路的按代理连接池。
	//
	// ⚠️ 刷新必须和对话走同一个出口 IP。Cursor 按设备指纹 + 出口 IP 做风控，
	// 账号配了代理却让 /oauth/token 从本机 IP 出去，等于主动暴露
	// 「同一账号从两个 IP 活动」，比不配代理更容易触发风控。
	cursorAuthRefreshPoolMu sync.Mutex
	cursorAuthRefreshPool   = map[string]*http.Client{}
)

// authRefreshClient 返回对应代理的刷新用 client；proxyURL 为空时用默认直连单例。
func authRefreshClient(proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return cursorAuthRefreshHTTPClient
	}
	cursorAuthRefreshPoolMu.Lock()
	defer cursorAuthRefreshPoolMu.Unlock()
	if cl, ok := cursorAuthRefreshPool[proxyURL]; ok {
		return cl
	}
	cl := newH2Client(proxyURL)
	cl.Timeout = 20 * time.Second
	cursorAuthRefreshPool[proxyURL] = cl
	return cl
}

func authRefreshDue(a Account, now time.Time, skew time.Duration) bool {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return false
	}
	if a.AccountExpiry.IsZero() {
		return false
	}
	return !now.Before(a.AccountExpiry.Add(-skew))
}

func authRefreshExpired(a Account, now time.Time) bool {
	return !a.AccountExpiry.IsZero() && !now.Before(a.AccountExpiry)
}

// confirmedAuthRefreshFailure reports errors that prove the saved refresh
// credential cannot be used anymore. Network failures, proxy EOFs, and 5xx
// responses are deliberately excluded: they must cool the account briefly,
// not disable it permanently.
func confirmedAuthRefreshFailure(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, marker := range []string{
		"invalid_grant",
		"invalid refresh token",
		"refresh token invalid",
		"re-authentication required",
		"shouldlogout",
		"unauthorized",
		"401",
		"403",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func authRefreshFailureReason(err error) string {
	if confirmedAuthRefreshFailure(err) {
		return "Cursor OAuth refresh 被拒绝，需要重新认证"
	}
	return "Cursor OAuth refresh 暂时失败，账号保留并短暂冷却"
}

func refreshCursorAuth(refreshToken string) (accessToken, rotatedRefreshToken string, err error) {
	return refreshCursorAuthVia(refreshToken, "")
}

func refreshCursorAuthVia(refreshToken, proxyURL string) (accessToken, rotatedRefreshToken string, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", "", fmt.Errorf("missing Cursor refresh token")
	}
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     cursorOAuthClientID,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, cursorAPIURL("/oauth/token"), bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Cursor/"+clientVersion)
	applyCursorProxyAuth(req)

	resp, err := authRefreshClient(proxyURL).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return "", "", readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("Cursor /oauth/token: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", err
	}
	if shouldLogout, ok := parsed["shouldLogout"].(bool); ok && shouldLogout {
		return "", "", fmt.Errorf("Cursor /oauth/token: refresh token invalid, re-authentication required")
	}
	accessToken = firstJSONString(parsed, "accessToken", "access_token")
	if accessToken == "" {
		return "", "", fmt.Errorf("Cursor /oauth/token: missing accessToken")
	}
	rotatedRefreshToken = firstJSONString(parsed, "refreshToken", "refresh_token")
	return accessToken, rotatedRefreshToken, nil
}

func firstJSONString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// ⚠️ 已移除的三个 *Store 方法：refreshLock / RefreshAuthToken / markAuthRefreshFailure。
// 它们是 ai2api 的 JSON 文件号池状态写回（改 s.accounts[i] + markDirty），
// 在 sub2api 侧由 service 层写 accounts 表取代（见 cursor_token_provider.go /
// cursor_token_refresher.go）。本文件只保留「是否该刷新」的判定与实际的
// token 兑换调用，保持协议层无状态。

// ── 业务层入口（sub2api 新增）─────────────────────────────────────────

// AuthRefreshSkew 是「提前多久刷新」的推荐提前量，供业务层复用，
// 避免 service 层另写一个漂移的常量。
const AuthRefreshSkew = cursorAuthRefreshSkew

// AuthRefreshDue 判断账号是否到了该刷新 access token 的时刻。
// AccountExpiry 为零值（未知有效期）时返回 false：宁可等 401 再刷，
// 也不要对每个未解析出 exp 的账号无条件打刷新接口。
func AuthRefreshDue(a Account, now time.Time, skew time.Duration) bool {
	return authRefreshDue(a, now, skew)
}

// AuthRefreshExpired 判断 access token 是否已经过期。
func AuthRefreshExpired(a Account, now time.Time) bool {
	return authRefreshExpired(a, now)
}

// ConfirmedAuthRefreshFailure 区分「凭证确认失效」与「临时故障」。
//
// ⚠️ 业务层必须用它决定是否 SetError 停用账号：网络抖动、代理 EOF、5xx
// 都不在此列，只应短暂冷却。把临时故障当成确认失效会批量误停号池。
func ConfirmedAuthRefreshFailure(err error) bool {
	return confirmedAuthRefreshFailure(err)
}

// AuthRefreshFailureReason 返回可写入 accounts.error_message 的中文原因。
func AuthRefreshFailureReason(err error) string {
	return authRefreshFailureReason(err)
}

// RefreshAuthToken 用 refresh token 兑换新的 access token（直连）。
// 第二个返回值是上游轮换后的新 refresh token，可能为空（表示不轮换，沿用旧值）。
func RefreshAuthToken(refreshToken string) (accessToken, rotatedRefreshToken string, err error) {
	return cursorAuthRefreshFn(refreshToken)
}

// RefreshAuthTokenVia 经账号绑定的代理兑换新的 access token。
//
// ⚠️ 刷新必须与对话共用出口 IP，否则同一账号会从两个 IP 活动，
// 反而更容易触发 Cursor 的风控。proxyURL 为空时等价于 RefreshAuthToken。
//
// ⚠️ 被测试替换掉实现时（SetAuthRefreshFnForTest）必须走同一个钩子，
// 否则所有打桩的测试会在这条分支上真的发起网络请求。
func RefreshAuthTokenVia(refreshToken, proxyURL string) (accessToken, rotatedRefreshToken string, err error) {
	if strings.TrimSpace(proxyURL) == "" {
		return cursorAuthRefreshFn(refreshToken)
	}
	// 仅当实现未被替换时才启用代理分支；被替换则一律尊重桩实现。
	if isDefaultAuthRefreshFn() {
		return refreshCursorAuthVia(refreshToken, proxyURL)
	}
	return cursorAuthRefreshFn(refreshToken)
}

// isDefaultAuthRefreshFn 判断 token 兑换实现是否仍是内置实现。
func isDefaultAuthRefreshFn() bool {
	return reflect.ValueOf(cursorAuthRefreshFn).Pointer() == reflect.ValueOf(refreshCursorAuth).Pointer()
}

// SetAuthRefreshFnForTest 替换 token 兑换实现，仅供测试使用；
// 返回一个恢复原实现的函数。
func SetAuthRefreshFnForTest(fn func(refreshToken string) (string, string, error)) func() {
	prev := cursorAuthRefreshFn
	cursorAuthRefreshFn = fn
	return func() { cursorAuthRefreshFn = prev }
}

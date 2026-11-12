package cursor

// Cursor Dashboard API Key（crsr_ 前缀）的兑换。
//
// 实测（2026-11-12，真实账号 + 美国出口代理）：
//   - POST api2.cursor.sh/auth/exchange_user_api_key，Bearer=crsr_ key，body={}
//     → 200 {accessToken, refreshToken}，两者是**同一个** JWT（type=api_key_token，寿命 60 分钟）。
//   - 该 JWT 可用于 AgentService/Run、Dashboard、AvailableModels 等接口，
//     但 Sand 的 InferenceService/Stream 返回 ERROR_NOT_LOGGED_IN。
//   - 拿它去 /oauth/token 走 refresh_token 授权返回 shouldLogout=true：**不能**按
//     session token 那样续期，唯一的续期方式是用 crsr_ key 重新兑换。
//
// 因此 crsr_ key 本身必须作为长期凭证保存，access token 只是它派生的 60 分钟缓存。
// 官方 @cursor/sdk 的做法相同（exchange_user_api_key + 按 exp 重新兑换）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIKeyPrefix 是 Cursor Dashboard API Key 的固定前缀。
const APIKeyPrefix = "crsr_"

// TokenTypeAPIKey 是 crsr_ key 兑换出的 JWT 的 type 声明。
const TokenTypeAPIKey = "api_key_token"

// IsAPIKey 判断字符串是否为 Cursor Dashboard API Key。
func IsAPIKey(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, APIKeyPrefix) && len(s) > len(APIKeyPrefix)+16 && !strings.ContainsAny(s, " \t.")
}

// ErrAPIKeyRejected 表示上游确认该 API Key 无效（401）。与网络/5xx 区分开：
// 只有它才说明凭证本身不可用、应当停号；其余失败只应短暂重试。
var ErrAPIKeyRejected = errors.New("cursor API key rejected")

// exchangeAPIKeyFn 是兑换实现，测试经 SetExchangeAPIKeyFnForTest 替换。
var exchangeAPIKeyFn = exchangeAPIKey

// ExchangeAPIKeyVia 经账号代理把 crsr_ key 兑换成可调用 AgentService 的 access token。
//
// ⚠️ 与 RefreshAuthTokenVia 同理：必须与该账号后续对话共用出口 IP。proxyURL 为空时直连。
func ExchangeAPIKeyVia(apiKey, proxyURL string) (string, error) {
	return exchangeAPIKeyFn(apiKey, proxyURL)
}

// SetExchangeAPIKeyFnForTest 替换兑换实现，仅供测试使用；返回恢复函数。
func SetExchangeAPIKeyFnForTest(fn func(apiKey, proxyURL string) (string, error)) func() {
	prev := exchangeAPIKeyFn
	exchangeAPIKeyFn = fn
	return func() { exchangeAPIKeyFn = prev }
}

func exchangeAPIKey(apiKey, proxyURL string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if !IsAPIKey(apiKey) {
		return "", fmt.Errorf("%w: not a crsr_ API key", ErrAPIKeyRejected)
	}
	req, err := http.NewRequest(http.MethodPost, cursorAPIURL("/auth/exchange_user_api_key"), bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	applyCursorProxyAuth(req)

	client := &http.Client{Timeout: 30 * time.Second}
	if strings.TrimSpace(proxyURL) != "" {
		client.Transport = newH2Client(proxyURL).Transport
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cursor API key exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: HTTP %d %s", ErrAPIKeyRejected, resp.StatusCode, summarizeUpstreamHTTPError(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("cursor API key exchange: HTTP %d %s", resp.StatusCode, summarizeUpstreamHTTPError(body))
	}
	var parsed struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("cursor API key exchange: decode response: %w", err)
	}
	access := strings.TrimSpace(parsed.AccessToken)
	if access == "" || strings.Count(access, ".") != 2 {
		return "", errors.New("cursor API key exchange: response has no usable accessToken")
	}
	return access, nil
}

package cursor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const cursorAuthRefreshSkew = 5 * time.Minute

// Cursor OAuth client id observed in public reverse-engineered clients.
// Keep it overridable so upstream changes do not require a code patch.
var cursorOAuthClientID = envOr("CURSOR_OAUTH_CLIENT_ID", "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB")

var (
	cursorAuthRefreshHTTPClient = &http.Client{Timeout: 20 * time.Second}
	cursorAuthRefreshFn         = refreshCursorAuth
)

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

	resp, err := cursorAuthRefreshHTTPClient.Do(req)
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

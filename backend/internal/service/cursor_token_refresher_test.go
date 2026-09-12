//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

func cursorRefreshAccount(creds map[string]any) *Account {
	return &Account{ID: 11, Platform: PlatformCursor, Type: AccountTypeOAuth, Credentials: creds}
}

func TestCursorTokenRefresher_CanRefreshOnlyOAuthCursor(t *testing.T) {
	r := NewCursorTokenRefresher()
	require.True(t, r.CanRefresh(cursorRefreshAccount(nil)))
	require.False(t, r.CanRefresh(&Account{Platform: PlatformCursor, Type: AccountTypeAPIKey}))
	require.False(t, r.CanRefresh(&Account{Platform: PlatformKiro, Type: AccountTypeOAuth}))
	require.False(t, r.CanRefresh(nil))
}

func TestCursorTokenRefresher_NoRefreshTokenMeansNoRefresh(t *testing.T) {
	// 没有 refresh_token 时后台任务不该反复尝试——它永远不可能成功。
	r := NewCursorTokenRefresher()
	require.False(t, r.NeedsRefresh(cursorRefreshAccount(map[string]any{
		CursorCredAccessToken: "tok",
	}), 0))
}

func TestCursorTokenRefresher_MissingAccessTokenNeedsRefresh(t *testing.T) {
	r := NewCursorTokenRefresher()
	require.True(t, r.NeedsRefresh(cursorRefreshAccount(map[string]any{
		CursorCredRefreshToken: "refresh",
	}), 0))
}

func TestCursorTokenRefresher_UnparsableExpiryDoesNotTriggerBackgroundRefresh(t *testing.T) {
	// ⚠️ 这是刻意的取舍：解析不出 exp 时返回 false。
	// 返回 true 会让后台任务每一轮都给这类账号打一次刷新接口，
	// 把「读不出有效期」放大成对上游的持续压测。由请求路径 401 强刷兜底。
	r := NewCursorTokenRefresher()
	require.False(t, r.NeedsRefresh(cursorRefreshAccount(map[string]any{
		CursorCredAccessToken:  "not-a-jwt",
		CursorCredRefreshToken: "refresh",
	}), 0))
}

func TestCursorTokenRefresher_RefreshMergesCredentials(t *testing.T) {
	restore := cursor.SetAuthRefreshFnForTest(func(refresh string) (string, string, error) {
		require.Equal(t, "old-refresh", refresh)
		return "new-access", "new-refresh", nil
	})
	defer restore()

	r := NewCursorTokenRefresher()
	acc := cursorRefreshAccount(map[string]any{
		CursorCredAccessToken:  "old-access",
		CursorCredRefreshToken: "old-refresh",
		CursorCredMachineID:    "machine-abc",
	})

	creds, err := r.Refresh(context.Background(), acc)
	require.NoError(t, err)
	require.Equal(t, "new-access", creds[CursorCredAccessToken])
	require.Equal(t, "new-refresh", creds[CursorCredRefreshToken])
	// machine_id 是设备指纹种子，必须原样保留——重新生成会让上游视为换设备。
	require.Equal(t, "machine-abc", creds[CursorCredMachineID])
}

func TestCursorTokenRefresher_EmptyRotatedRefreshKeepsOldToken(t *testing.T) {
	// 上游返回空 refreshToken 表示「不轮换」。写空会让账号永久失去续期能力。
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "new-access", "", nil
	})
	defer restore()

	r := NewCursorTokenRefresher()
	creds, err := r.Refresh(context.Background(), cursorRefreshAccount(map[string]any{
		CursorCredRefreshToken: "keep-me",
	}))
	require.NoError(t, err)
	require.Equal(t, "keep-me", creds[CursorCredRefreshToken])
}

func TestCursorTokenRefresher_EmptyAccessTokenIsAnError(t *testing.T) {
	// 上游 200 但没给 access_token 时必须报错，否则会把空 token 写进库，
	// 之后每次请求都 401，而账号看起来"刚刷新过"。
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "", "rotated", nil
	})
	defer restore()

	r := NewCursorTokenRefresher()
	_, err := r.Refresh(context.Background(), cursorRefreshAccount(map[string]any{
		CursorCredRefreshToken: "refresh",
	}))
	require.Error(t, err)
}

func TestCursorTokenRefresher_PropagatesUpstreamError(t *testing.T) {
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "", "", errors.New("upstream 503")
	})
	defer restore()

	r := NewCursorTokenRefresher()
	_, err := r.Refresh(context.Background(), cursorRefreshAccount(map[string]any{
		CursorCredRefreshToken: "refresh",
	}))
	require.ErrorContains(t, err, "503")
}

func TestCursorTokenRefresher_CacheKeyMatchesProvider(t *testing.T) {
	// 刷新器与 provider 必须用同一个 key，否则后台刷完的 token
	// 请求路径读不到，等于后台刷新完全没生效。
	acc := cursorRefreshAccount(nil)
	require.Equal(t, CursorTokenCacheKey(acc), NewCursorTokenRefresher().CacheKey(acc))
}

func TestCursorRefreshWindowIsWiderThanRequestPathSkew(t *testing.T) {
	// 后台窗口必须比请求路径的 skew 宽，否则请求路径会先撞上同步刷新，
	// 后台任务就失去了"提前换掉"的意义。
	require.Greater(t, cursorRefreshWindow, cursorTokenRefreshSkew)
	require.Equal(t, 15*time.Minute, cursorRefreshWindow)
}

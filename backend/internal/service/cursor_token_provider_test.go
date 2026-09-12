//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// cursorTokenProviderRepo 记录 SetError / 凭证写回，用于断言
// 「什么情况下才允许停用账号」。
type cursorTokenProviderRepo struct {
	mockAccountRepoForGemini
	mu            sync.Mutex
	setErrorCalls int
	setErrorMsg   string
	updated       map[string]any
	byID          *Account
}

func (r *cursorTokenProviderRepo) SetError(_ context.Context, _ int64, errorMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setErrorCalls++
	r.setErrorMsg = errorMsg
	return nil
}

func (r *cursorTokenProviderRepo) Update(_ context.Context, account *Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if account != nil {
		r.updated = shallowCopyMap(account.Credentials)
	}
	return nil
}

func (r *cursorTokenProviderRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		return nil, errors.New("account not found")
	}
	return r.byID, nil
}

func (r *cursorTokenProviderRepo) credentials() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updated
}

func (r *cursorTokenProviderRepo) errorCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.setErrorCalls
}

func newCursorOAuthAccount(creds map[string]any) *Account {
	return &Account{
		ID: 7, Platform: PlatformCursor, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Credentials: creds,
	}
}

func TestCursorTokenProvider_ReturnsExistingTokenWithoutRefresh(t *testing.T) {
	// AccountExpiry 解析不出（非 JWT）时不应无条件打刷新接口。
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		t.Fatal("不应触发刷新")
		return "", "", nil
	})
	defer restore()

	repo := &cursorTokenProviderRepo{}
	p := NewCursorTokenProvider(repo, nil)
	token, err := p.GetAccessToken(context.Background(), newCursorOAuthAccount(map[string]any{
		CursorCredAccessToken:  "existing-token",
		CursorCredRefreshToken: "refresh-token",
	}))
	require.NoError(t, err)
	require.Equal(t, "existing-token", token)
}

func TestCursorTokenProvider_RefreshesWhenAccessTokenMissing(t *testing.T) {
	restore := cursor.SetAuthRefreshFnForTest(func(refresh string) (string, string, error) {
		require.Equal(t, "refresh-token", refresh)
		return "fresh-token", "rotated-refresh", nil
	})
	defer restore()

	repo := &cursorTokenProviderRepo{}
	p := NewCursorTokenProvider(repo, nil)
	token, err := p.GetAccessToken(context.Background(), newCursorOAuthAccount(map[string]any{
		CursorCredRefreshToken: "refresh-token",
	}))
	require.NoError(t, err)
	require.Equal(t, "fresh-token", token)

	creds := repo.credentials()
	require.Equal(t, "fresh-token", creds[CursorCredAccessToken])
	require.Equal(t, "rotated-refresh", creds[CursorCredRefreshToken], "轮换后的 refresh token 应落库")
}

func TestCursorTokenProvider_EmptyRotatedRefreshKeepsOldToken(t *testing.T) {
	// ⚠️ 上游返回空 refreshToken 表示「不轮换，沿用旧值」。
	// 若把空值写回去，账号会永久失去续期能力。
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "fresh-token", "", nil
	})
	defer restore()

	repo := &cursorTokenProviderRepo{}
	p := NewCursorTokenProvider(repo, nil)
	_, err := p.ForceRefreshAccessToken(context.Background(), newCursorOAuthAccount(map[string]any{
		CursorCredAccessToken:  "old",
		CursorCredRefreshToken: "keep-me",
	}))
	require.NoError(t, err)
	require.Equal(t, "keep-me", repo.credentials()[CursorCredRefreshToken],
		"上游未轮换时必须保留原 refresh token")
}

func TestCursorTokenProvider_TransientRefreshFailureDoesNotDisableAccount(t *testing.T) {
	// ⚠️ 网络抖动/5xx 绝不能 SetError 停号，否则一次上游抖动会批量误停号池。
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "", "", errors.New("dial tcp: connection reset by peer")
	})
	defer restore()

	repo := &cursorTokenProviderRepo{}
	p := NewCursorTokenProvider(repo, nil)
	_, err := p.ForceRefreshAccessToken(context.Background(), newCursorOAuthAccount(map[string]any{
		CursorCredRefreshToken: "refresh-token",
	}))
	require.Error(t, err)
	require.Zero(t, repo.errorCalls(), "临时性刷新失败不得停用账号")
}

func TestCursorTokenProvider_ConfirmedRefreshFailureDisablesAccount(t *testing.T) {
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "", "", errors.New("Cursor /oauth/token: refresh token invalid, re-authentication required")
	})
	defer restore()

	repo := &cursorTokenProviderRepo{}
	p := NewCursorTokenProvider(repo, nil)
	_, err := p.ForceRefreshAccessToken(context.Background(), newCursorOAuthAccount(map[string]any{
		CursorCredRefreshToken: "refresh-token",
	}))
	require.Error(t, err)
	require.Equal(t, 1, repo.errorCalls(), "确认失效应停用账号")
	require.Contains(t, repo.setErrorMsg, "重新认证")
}

func TestCursorTokenProvider_FallsBackToValidTokenOnRefreshFailure(t *testing.T) {
	// 刷新失败但手上的 token 还没过期时，本次请求应继续使用它。
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "", "", errors.New("temporary upstream 503")
	})
	defer restore()

	acc := newCursorOAuthAccount(map[string]any{
		CursorCredAccessToken:  "still-valid",
		CursorCredRefreshToken: "refresh-token",
	})
	repo := &cursorTokenProviderRepo{}
	p := NewCursorTokenProvider(repo, nil)
	token, err := p.GetAccessToken(context.Background(), acc)
	require.NoError(t, err)
	require.Equal(t, "still-valid", token)
}

func TestCursorTokenProvider_RejectsNonCursorAccount(t *testing.T) {
	p := NewCursorTokenProvider(&cursorTokenProviderRepo{}, nil)
	_, err := p.GetAccessToken(context.Background(), &Account{ID: 1, Platform: PlatformKiro, Type: AccountTypeOAuth})
	require.Error(t, err)

	_, err = p.GetAccessToken(context.Background(), nil)
	require.Error(t, err)
}

func TestCursorTokenProvider_MissingRefreshTokenSurfacesError(t *testing.T) {
	p := NewCursorTokenProvider(&cursorTokenProviderRepo{}, nil)
	_, err := p.GetAccessToken(context.Background(), newCursorOAuthAccount(map[string]any{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "access_token")
}

func TestCursorTokenCacheKey_IsPerAccount(t *testing.T) {
	// ⚠️ 同一台设备导入的多个账号共用 machine_id；缓存 key 必须按账号区分，
	// 否则不同账号的 token 与刷新锁会互相覆盖（Kiro 踩过同样的坑）。
	a := &Account{ID: 1, Platform: PlatformCursor, Credentials: map[string]any{CursorCredMachineID: "same-machine"}}
	b := &Account{ID: 2, Platform: PlatformCursor, Credentials: map[string]any{CursorCredMachineID: "same-machine"}}
	require.NotEqual(t, CursorTokenCacheKey(a), CursorTokenCacheKey(b))
	require.Equal(t, "cursor:account:1", CursorTokenCacheKey(a))
	require.Equal(t, "cursor:account:0", CursorTokenCacheKey(nil))
}

func TestCursorAccessTokenTTL_DerivesFromJWTExpiry(t *testing.T) {
	// 非 JWT / 解析不出 exp 时退回默认 TTL，而不是 0（0 会让缓存立即失效）。
	require.Equal(t, 30*time.Minute, cursorAccessTokenTTL("not-a-jwt"))
	require.Greater(t, cursorAccessTokenTTL(""), time.Duration(0))
}

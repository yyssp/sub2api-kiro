//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// Cursor Dashboard API Key（crsr_）账号的生命周期。
//
// 关键事实（2026-11-12 真实账号实测）：exchange_user_api_key 返回的 JWT 是
// type=api_key_token、寿命 60 分钟；/oauth/token 对它返回 shouldLogout。所以：
//   - crsr_ key 必须长期保存，续期只能重新兑换；
//   - 绝不能给这类账号写 refresh_token，否则后台刷新会被 shouldLogout 判成「确认失效」停号。

const testCursorAPIKey = "crsr_0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

type cursorAPIKeyExchangeCall struct{ key, proxyURL string }

func stubCursorAPIKeyExchange(t *testing.T, access string, err error) *[]cursorAPIKeyExchangeCall {
	t.Helper()
	var calls []cursorAPIKeyExchangeCall
	t.Cleanup(cursor.SetExchangeAPIKeyFnForTest(func(key, proxyURL string) (string, error) {
		calls = append(calls, cursorAPIKeyExchangeCall{key: key, proxyURL: proxyURL})
		return access, err
	}))
	return &calls
}

func apiKeyToken(t *testing.T, sub, email string, exp time.Time) string {
	return makeCursorJWT(t, cursor.TokenTypeAPIKey, email, sub, exp)
}

func TestCreateCursorAccount_ExchangesAPIKeyViaAccountProxy(t *testing.T) {
	access := apiKeyToken(t, "auth0|user_K", "k@cursor.test", time.Now().Add(time.Hour))
	calls := stubCursorAPIKeyExchange(t, access, nil)
	proxyID := int64(9)
	proxy := &Proxy{ID: proxyID, Protocol: "http", Host: "10.0.0.9", Port: 3128}
	svc := &adminServiceImpl{proxyRepo: &cursorExchangeProxyRepo{proxies: map[int64]*Proxy{proxyID: proxy}}}

	creds, err := svc.exchangeCursorAPIKeyOnCreate(context.Background(), &CreateAccountInput{
		Platform: PlatformCursor, Type: AccountTypeOAuth, ProxyID: &proxyID,
		Credentials: map[string]any{CursorCredAPIKey: testCursorAPIKey, CursorCredMachineID: "keep-me"},
	})
	require.NoError(t, err)
	require.Len(t, *calls, 1, "crsr_ key 必须在建号时兑换")
	require.Equal(t, testCursorAPIKey, (*calls)[0].key)
	require.Equal(t, proxy.URL(), (*calls)[0].proxyURL, "兑换必须走账号代理，与后续对话共用出口 IP")

	require.Equal(t, access, creds[CursorCredAccessToken])
	require.Equal(t, testCursorAPIKey, creds[CursorCredAPIKey], "crsr_ key 是唯一的续期凭证，必须保留")
	require.Equal(t, "user_K::"+access, creds[CursorCredSession], "session 用于按 uid 去重与管理台展示")
	require.Equal(t, "k@cursor.test", creds[CursorCredEmail])
	require.Equal(t, "keep-me", creds[CursorCredMachineID])
	require.NotContains(t, creds, CursorCredRefreshToken,
		"api_key_token 不能走 /oauth/token（shouldLogout），写了 refresh_token 会被后台刷新误停号")
}

func TestCreateCursorAccount_APIKeyExchangeFailureRejectsCreate(t *testing.T) {
	t.Run("rejected key is a 400", func(t *testing.T) {
		stubCursorAPIKeyExchange(t, "", fmt.Errorf("%w: HTTP 401", cursor.ErrAPIKeyRejected))
		_, err := (&adminServiceImpl{}).exchangeCursorAPIKeyOnCreate(context.Background(), &CreateAccountInput{
			Platform: PlatformCursor, Credentials: map[string]any{CursorCredAPIKey: testCursorAPIKey},
		})
		require.ErrorIs(t, err, ErrCursorAPIKeyRejected)
	})
	t.Run("transient failure still rejects create", func(t *testing.T) {
		stubCursorAPIKeyExchange(t, "", errors.New("proxy EOF"))
		_, err := (&adminServiceImpl{}).exchangeCursorAPIKeyOnCreate(context.Background(), &CreateAccountInput{
			Platform: PlatformCursor, Credentials: map[string]any{CursorCredAPIKey: testCursorAPIKey},
		})
		require.Error(t, err, "没有任何可用 access token 的账号建出来必定不可用，不能静默落库")
		require.NotErrorIs(t, err, ErrCursorAPIKeyRejected, "网络失败不能被说成 key 无效")
	})
}

func TestCreateCursorAccount_NonAPIKeyCredentialsSkipExchange(t *testing.T) {
	calls := stubCursorAPIKeyExchange(t, "unused", nil)
	session := makeCursorJWT(t, "session", "", "user_S", time.Now().Add(24*time.Hour))
	for _, in := range []*CreateAccountInput{
		{Platform: PlatformCursor, Credentials: map[string]any{CursorCredAccessToken: session}},
		{Platform: PlatformKiro, Credentials: map[string]any{CursorCredAPIKey: testCursorAPIKey}},
		{Platform: PlatformCursor, Credentials: map[string]any{CursorCredAPIKey: "not-a-crsr-key"}},
	} {
		creds, err := (&adminServiceImpl{}).exchangeCursorAPIKeyOnCreate(context.Background(), in)
		require.NoError(t, err)
		require.Equal(t, in.Credentials, creds)
	}
	require.Empty(t, *calls)
}

func TestCursorTokenProvider_APIKeyAccountReExchangesInsteadOfOAuthRefresh(t *testing.T) {
	expired := apiKeyToken(t, "auth0|user_K", "", time.Now().Add(-time.Minute))
	fresh := apiKeyToken(t, "auth0|user_K", "", time.Now().Add(time.Hour))
	exchanges := stubCursorAPIKeyExchange(t, fresh, nil)
	oauthCalls := 0
	t.Cleanup(cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		oauthCalls++
		return "", "", errors.New("must not be called")
	}))

	repo := &cursorLifecycleRepo{}
	account := &Account{ID: 5, Platform: PlatformCursor, Type: AccountTypeOAuth,
		Credentials: map[string]any{CursorCredAPIKey: testCursorAPIKey, CursorCredAccessToken: expired}}
	token, err := NewCursorTokenProvider(repo, nil).GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, fresh, token)
	require.Len(t, *exchanges, 1, "临期的 api_key_token 必须用 crsr_ key 重新兑换")
	require.Zero(t, oauthCalls, "api_key_token 走 /oauth/token 只会得到 shouldLogout")
	require.Len(t, repo.credWrites, 1, "兑换结果要原子落库（UpdateCredentials）")
	require.Equal(t, fresh, repo.credWrites[0][CursorCredAccessToken])
	require.Equal(t, testCursorAPIKey, repo.credWrites[0][CursorCredAPIKey])
	require.NotContains(t, repo.credWrites[0], CursorCredRefreshToken)
}

func TestCursorTokenProvider_RejectedAPIKeyDisablesAccountButTransientDoesNot(t *testing.T) {
	expired := apiKeyToken(t, "auth0|user_K", "", time.Now().Add(-time.Minute))
	newAccount := func() *Account {
		return &Account{ID: 5, Platform: PlatformCursor, Type: AccountTypeOAuth,
			Credentials: map[string]any{CursorCredAPIKey: testCursorAPIKey, CursorCredAccessToken: expired}}
	}

	stubCursorAPIKeyExchange(t, "", fmt.Errorf("%w: HTTP 401", cursor.ErrAPIKeyRejected))
	repo := &cursorAPIKeyErrorRepo{}
	_, err := NewCursorTokenProvider(repo, nil).GetAccessToken(context.Background(), newAccount())
	require.Error(t, err)
	require.Len(t, repo.errors, 1, "key 被上游明确拒绝才停号")

	stubCursorAPIKeyExchange(t, "", errors.New("dial tcp: i/o timeout"))
	repo = &cursorAPIKeyErrorRepo{}
	_, err = NewCursorTokenProvider(repo, nil).GetAccessToken(context.Background(), newAccount())
	require.Error(t, err)
	require.Empty(t, repo.errors, "网络抖动不能停号，否则一次代理故障会批量清空号池")
}

func TestCursorTokenRefresher_APIKeyAccountNeedsAndPerformsReExchange(t *testing.T) {
	soon := apiKeyToken(t, "auth0|user_K", "", time.Now().Add(time.Minute))
	fresh := apiKeyToken(t, "auth0|user_K", "", time.Now().Add(time.Hour))
	stubCursorAPIKeyExchange(t, fresh, nil)
	account := &Account{ID: 5, Platform: PlatformCursor, Type: AccountTypeOAuth,
		Credentials: map[string]any{CursorCredAPIKey: testCursorAPIKey, CursorCredAccessToken: soon}}

	r := NewCursorTokenRefresher()
	require.True(t, r.NeedsRefresh(account, 0), "没有 refresh_token 的 API Key 账号也必须被后台续期")
	creds, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, fresh, creds[CursorCredAccessToken])
	require.Equal(t, testCursorAPIKey, creds[CursorCredAPIKey])
	require.NotContains(t, creds, CursorCredRefreshToken)
}

func TestImportCursorCredentials_PreviewsAPIKeyWithoutNetwork(t *testing.T) {
	calls := stubCursorAPIKeyExchange(t, "unused", nil)
	svc := &CursorOAuthService{accountRepo: &cursorLifecycleRepo{}}
	result, err := svc.ImportCursorCredentials(context.Background(), &CursorImportInput{Content: testCursorAPIKey})
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	require.Equal(t, testCursorAPIKey, result.Entries[0].APIKey)
	require.Equal(t, cursor.TokenTypeAPIKeyEntry, result.Entries[0].TokenType)
	require.Empty(t, result.Entries[0].AccessToken, "预览阶段不兑换")
	require.Empty(t, *calls, "预览不联网：N 条 key 不该触发 N 次上游请求")
}

func TestImportedCredentialUpdates_APIKeyMustBelongToSameUser(t *testing.T) {
	existing := &Account{ID: 3, Platform: PlatformCursor, Credentials: map[string]any{
		CursorCredAccessToken: makeCursorJWT(t, "session", "", "auth0|user_A", time.Now().Add(24*time.Hour)),
		CursorCredMachineID:   "m",
	}}
	svc := &CursorOAuthService{}

	stubCursorAPIKeyExchange(t, apiKeyToken(t, "auth0|user_A", "", time.Now().Add(time.Hour)), nil)
	updates, err := svc.ImportedCredentialUpdates(existing, map[string]any{CursorCredAPIKey: testCursorAPIKey})
	require.NoError(t, err)
	require.Equal(t, testCursorAPIKey, updates[CursorCredAPIKey])
	require.NotContains(t, updates, CursorCredMachineID)

	stubCursorAPIKeyExchange(t, apiKeyToken(t, "auth0|user_B", "", time.Now().Add(time.Hour)), nil)
	_, err = svc.ImportedCredentialUpdates(existing, map[string]any{CursorCredAPIKey: testCursorAPIKey})
	require.ErrorIs(t, err, ErrCursorCredentialUserMismatch, "别人的 key 不能写进这个账号")
}

// cursorAPIKeyErrorRepo 记录 SetError，验证停号判定。
type cursorAPIKeyErrorRepo struct {
	cursorLifecycleRepo
	errors []string
}

func (r *cursorAPIKeyErrorRepo) SetError(_ context.Context, _ int64, msg string) error {
	r.errors = append(r.errors, msg)
	return nil
}

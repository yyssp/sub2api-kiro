//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// cursorExchangeProxyRepo 只实现建号兑换路径会用到的 GetByID，其余方法未实现即 panic。
type cursorExchangeProxyRepo struct {
	ProxyRepository
	proxies map[int64]*Proxy
}

func (r *cursorExchangeProxyRepo) GetByID(_ context.Context, id int64) (*Proxy, error) {
	if p, ok := r.proxies[id]; ok {
		return p, nil
	}
	return nil, ErrProxyNotFound
}

type cursorExchangeCall struct {
	session  string
	proxyURL string
}

// stubCursorExchange 替换 web token 兑换实现，记录每次调用；ok=false 模拟 web token 已失效。
func stubCursorExchange(t *testing.T, access, refresh string, ok bool) *[]cursorExchangeCall {
	t.Helper()
	var calls []cursorExchangeCall
	restore := cursor.SetExchangeWebTokenFnForTest(func(session, proxyURL string) (string, string, bool) {
		calls = append(calls, cursorExchangeCall{session: session, proxyURL: proxyURL})
		return access, refresh, ok
	})
	t.Cleanup(restore)
	return &calls
}

func TestCreateCursorAccount_ExchangesWebTokenViaAccountProxy(t *testing.T) {
	webTok := makeCursorJWT(t, "web", "", "user_web", time.Now().Add(2*time.Hour))
	sessionTok := makeCursorJWT(t, "session", "new@cursor.test", "user_web", time.Now().Add(60*24*time.Hour))
	calls := stubCursorExchange(t, sessionTok, "refresh-xyz", true)

	proxyID := int64(7)
	proxy := &Proxy{ID: proxyID, Protocol: "http", Host: "10.0.0.7", Port: 3128}
	svc := &adminServiceImpl{proxyRepo: &cursorExchangeProxyRepo{proxies: map[int64]*Proxy{proxyID: proxy}}}

	creds := svc.exchangeCursorWebTokenOnCreate(context.Background(), &CreateAccountInput{
		Name:        "c1",
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		ProxyID:     &proxyID,
		Credentials: map[string]any{CursorCredAccessToken: webTok, CursorCredMachineID: "keep-me"},
	})

	require.Len(t, *calls, 1, "web token 必须在建号时兑换")
	require.Equal(t, "user_web::"+webTok, (*calls)[0].session, "兑换要用 uid::JWT 形态的 web 会话")
	require.Equal(t, proxy.URL(), (*calls)[0].proxyURL, "兑换必须走账号绑定的代理，与后续对话共用出口 IP")

	require.Equal(t, sessionTok, creds[CursorCredAccessToken], "落库的应是兑换后的长效 session token")
	require.Equal(t, "refresh-xyz", creds[CursorCredRefreshToken], "兑换得到的 refresh_token 是自动续期的前提")
	require.Equal(t, "user_web::"+sessionTok, creds[CursorCredSession])
	require.Equal(t, "new@cursor.test", creds[CursorCredEmail])
	require.Equal(t, "keep-me", creds[CursorCredMachineID], "兑换不得改动设备指纹种子")
}

func TestCreateCursorAccount_ExchangeFailureKeepsOriginalToken(t *testing.T) {
	webTok := makeCursorJWT(t, "web", "", "user_web", time.Now().Add(2*time.Hour))
	calls := stubCursorExchange(t, "", "", false)

	svc := &adminServiceImpl{}
	creds := svc.exchangeCursorWebTokenOnCreate(context.Background(), &CreateAccountInput{
		Platform:    PlatformCursor,
		Credentials: map[string]any{CursorCredAccessToken: webTok},
	})

	require.Len(t, *calls, 1)
	require.Empty(t, (*calls)[0].proxyURL, "未绑定代理时直连")
	require.Equal(t, webTok, creds[CursorCredAccessToken], "兑换失败保留原 token，不拒绝建号")
	require.NotContains(t, creds, CursorCredRefreshToken)
}

func TestCreateCursorAccount_SkipsExchangeWhenNotNeeded(t *testing.T) {
	webTok := makeCursorJWT(t, "web", "", "user_web", time.Now().Add(2*time.Hour))
	sessionTok := makeCursorJWT(t, "session", "", "user_s", time.Now().Add(60*24*time.Hour))
	cases := []struct {
		name     string
		platform string
		creds    map[string]any
	}{
		{"session token", PlatformCursor, map[string]any{CursorCredAccessToken: sessionTok}},
		{"web token already has refresh_token", PlatformCursor, map[string]any{CursorCredAccessToken: webTok, CursorCredRefreshToken: "r"}},
		{"missing access_token", PlatformCursor, map[string]any{}},
		{"non-string access_token", PlatformCursor, map[string]any{CursorCredAccessToken: 42}},
		{"other platform with web-looking token", PlatformKiro, map[string]any{CursorCredAccessToken: webTok}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubCursorExchange(t, "should-not-be-used", "", true)
			svc := &adminServiceImpl{}
			creds := svc.exchangeCursorWebTokenOnCreate(context.Background(), &CreateAccountInput{
				Platform:    tc.platform,
				Credentials: tc.creds,
			})
			require.Empty(t, *calls, "不需要兑换时不得请求上游")
			require.Equal(t, tc.creds, creds)
		})
	}
	require.Nil(t, (&adminServiceImpl{}).exchangeCursorWebTokenOnCreate(context.Background(), nil))
}

// 接线守卫：兑换钩子必须挂在 CreateAccount 上——批量导入走的就是它（经 BatchCreate）。
// 钩子写好却不接线，正是本缺陷本身：web token 原样落库，几小时后账号失效。
func TestCreateAccount_WiresCursorWebTokenExchange(t *testing.T) {
	webTok := makeCursorJWT(t, "web", "", "user_web", time.Now().Add(2*time.Hour))
	sessionTok := makeCursorJWT(t, "session", "", "user_web", time.Now().Add(60*24*time.Hour))
	calls := stubCursorExchange(t, sessionTok, "refresh-1", true)

	repo := &cursorCreateAccountRepo{}
	svc := &adminServiceImpl{accountRepo: repo, groupRepo: &cursorCreateGroupRepo{}}
	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "c1",
		Platform:             PlatformCursor,
		Type:                 AccountTypeOAuth,
		Concurrency:          1,
		SkipDefaultGroupBind: true,
		Credentials:          map[string]any{CursorCredAccessToken: webTok},
	})
	require.NoError(t, err)
	require.Len(t, *calls, 1)
	require.Equal(t, sessionTok, account.Credentials[CursorCredAccessToken])
	require.Equal(t, "refresh-1", account.Credentials[CursorCredRefreshToken])
	require.NotEmpty(t, account.Credentials[CursorCredMachineID], "建号仍须铸造设备指纹种子")
	require.Same(t, account, repo.created)
}

type cursorCreateAccountRepo struct {
	AccountRepository
	created *Account
}

func (r *cursorCreateAccountRepo) Create(_ context.Context, account *Account) error {
	account.ID = 1
	r.created = account
	return nil
}

type cursorCreateGroupRepo struct {
	GroupRepository
}

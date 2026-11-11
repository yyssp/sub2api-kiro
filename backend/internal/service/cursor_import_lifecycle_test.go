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

// cursorLifecycleRepo 是导入去重 / 补齐路径用到的最小账号仓储。
type cursorLifecycleRepo struct {
	AccountRepository
	accounts    []Account
	listErr     error
	updated     []*Account
	credWrites  []map[string]any
	extraWrites []map[string]any
}

func (r *cursorLifecycleRepo) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	r.credWrites = append(r.credWrites, credentials)
	return nil
}

func (r *cursorLifecycleRepo) ListAllWithFilters(_ context.Context, platform, _, _, _ string, _ int64, _ string) ([]Account, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		if a.Platform == platform {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *cursorLifecycleRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			a := r.accounts[i]
			return &a, nil
		}
	}
	return nil, ErrAccountNotFound
}

func (r *cursorLifecycleRepo) Update(_ context.Context, account *Account) error {
	r.updated = append(r.updated, account)
	return nil
}

func (r *cursorLifecycleRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

func cursorSessionTok(t *testing.T, sub, email string) string {
	return makeCursorJWT(t, "session", email, sub, time.Now().Add(60*24*time.Hour))
}

func TestImportCursorCredentials_AnnotatesExistingAccountByUID(t *testing.T) {
	oldTok := cursorSessionTok(t, "auth0|user_A", "a@x.test")
	newTok := cursorSessionTok(t, "auth0|user_A", "a@x.test")
	otherTok := cursorSessionTok(t, "auth0|user_B", "b@x.test")
	repo := &cursorLifecycleRepo{accounts: []Account{
		{ID: 9, Name: "a-newer-dup", Platform: PlatformCursor, Status: StatusError, Credentials: map[string]any{CursorCredAccessToken: oldTok}},
		{ID: 3, Name: "a", Platform: PlatformCursor, Status: StatusActive, Credentials: map[string]any{CursorCredSession: "user_A::" + oldTok}},
		{ID: 5, Name: "kiro-same-sub", Platform: PlatformKiro, Credentials: map[string]any{CursorCredAccessToken: otherTok}},
	}}
	svc := &CursorOAuthService{accountRepo: repo}

	result, err := svc.ImportCursorCredentials(context.Background(), &CursorImportInput{Content: newTok + "\n" + otherTok})
	require.NoError(t, err)
	require.Len(t, result.Entries, 2)

	require.Equal(t, "user_A", result.Entries[0].UserID)
	require.NotNil(t, result.Entries[0].Existing, "同一 uid 的新 token 必须认出已有账号（token 会轮换，按 token 去重认不出）")
	require.Equal(t, int64(3), result.Entries[0].Existing.ID, "多个旧账号时取 ID 最小的，结果稳定")
	require.Nil(t, result.Entries[1].Existing, "其他平台的账号不参与 Cursor 去重")
}

func TestImportCursorCredentials_ExistingLookupFailureStillPreviews(t *testing.T) {
	svc := &CursorOAuthService{accountRepo: &cursorLifecycleRepo{listErr: errors.New("db down")}}
	result, err := svc.ImportCursorCredentials(context.Background(), &CursorImportInput{Content: cursorSessionTok(t, "user_A", "")})
	require.NoError(t, err, "查库失败不应让预览失败")
	require.Len(t, result.Entries, 1)
	require.Nil(t, result.Entries[0].Existing)
}

func TestImportedCredentialUpdates_OnlyTokenFieldsAndSameUser(t *testing.T) {
	oldTok := cursorSessionTok(t, "user_A", "old@x.test")
	newTok := cursorSessionTok(t, "user_A", "new@x.test")
	existing := &Account{ID: 3, Platform: PlatformCursor, Credentials: map[string]any{
		CursorCredAccessToken: oldTok, CursorCredRefreshToken: "old-refresh",
		CursorCredMachineID: "machine-keep", CursorCredMembership: "pro",
	}}
	svc := &CursorOAuthService{}

	updates, err := svc.ImportedCredentialUpdates(existing, map[string]any{
		CursorCredAccessToken: newTok,
		CursorCredMachineID:   "attacker-machine",
		"group_ids":           []int64{1},
	})
	require.NoError(t, err)
	require.Equal(t, newTok, updates[CursorCredAccessToken])
	require.Equal(t, "user_A::"+newTok, updates[CursorCredSession])
	require.Equal(t, "new@x.test", updates[CursorCredEmail])
	require.NotContains(t, updates, CursorCredMachineID, "换 token 不能换设备指纹")
	require.NotContains(t, updates, CursorCredRefreshToken, "新凭证没带 refresh_token 时不得清空旧值")
	require.NotContains(t, updates, "group_ids")

	merged := MergeCredentials(existing.Credentials, updates)
	require.Equal(t, "machine-keep", merged[CursorCredMachineID])
	require.Equal(t, "old-refresh", merged[CursorCredRefreshToken])
	require.Equal(t, "pro", merged[CursorCredMembership])
}

func TestImportedCredentialUpdates_RejectsDifferentUser(t *testing.T) {
	existing := &Account{ID: 3, Platform: PlatformCursor, Credentials: map[string]any{
		CursorCredAccessToken: cursorSessionTok(t, "user_A", ""),
	}}
	svc := &CursorOAuthService{}
	_, err := svc.ImportedCredentialUpdates(existing, map[string]any{CursorCredAccessToken: cursorSessionTok(t, "user_B", "")})
	require.Error(t, err, "existing_account_id 只是预览标注，不能把别人的 token 写进任意账号")

	_, err = svc.ImportedCredentialUpdates(&Account{Platform: PlatformKiro}, map[string]any{CursorCredAccessToken: "x"})
	require.Error(t, err)
	_, err = svc.ImportedCredentialUpdates(existing, map[string]any{})
	require.Error(t, err)
}

func TestImportedCredentialUpdates_ExchangesWebTokenViaAccountProxy(t *testing.T) {
	webTok := makeCursorJWT(t, "web", "", "user_A", time.Now().Add(2*time.Hour))
	sessionTok := cursorSessionTok(t, "user_A", "a@x.test")
	calls := stubCursorExchange(t, sessionTok, "fresh-refresh", true)
	proxyID := int64(4)
	proxy := &Proxy{ID: proxyID, Protocol: "socks5", Host: "10.0.0.4", Port: 1080}
	existing := &Account{ID: 3, Platform: PlatformCursor, ProxyID: &proxyID, Proxy: proxy,
		Credentials: map[string]any{CursorCredAccessToken: cursorSessionTok(t, "user_A", "")}}

	updates, err := (&CursorOAuthService{}).ImportedCredentialUpdates(existing, map[string]any{CursorCredAccessToken: webTok})
	require.NoError(t, err)
	require.Len(t, *calls, 1)
	require.Equal(t, proxy.URL(), (*calls)[0].proxyURL)
	require.Equal(t, sessionTok, updates[CursorCredAccessToken])
	require.Equal(t, "fresh-refresh", updates[CursorCredRefreshToken])
}

func TestProvisionCursorAccount_FillsMembershipAndOptionalTrial(t *testing.T) {
	tok := cursorSessionTok(t, "user_A", "a@x.test")
	proxyID := int64(4)
	proxy := &Proxy{ID: proxyID, Protocol: "http", Host: "10.0.0.4", Port: 8080}
	repo := &cursorLifecycleRepo{accounts: []Account{{
		ID: 3, Platform: PlatformCursor, ProxyID: &proxyID, Proxy: proxy,
		Credentials: map[string]any{CursorCredAccessToken: tok, CursorCredMachineID: "m"},
	}}}
	var gotSession, gotProxy string
	restore := cursor.SetFetchMembershipFnForTest(func(session, proxyURL string) (string, bool) {
		gotSession, gotProxy = session, proxyURL
		return "free_trial", true
	})
	t.Cleanup(restore)

	svc := &CursorOAuthService{accountRepo: repo}
	result, err := svc.ProvisionCursorAccount(context.Background(), 3, CursorProvisionOptions{})
	require.NoError(t, err)
	require.Equal(t, "free_trial", result.Membership)
	require.Equal(t, "user_A::"+tok, gotSession, "套餐接口用 uid::JWT 会话 cookie")
	require.Equal(t, proxy.URL(), gotProxy, "补齐同样走账号代理")
	require.Empty(t, repo.updated, "异步补齐不得整行写回，会覆盖用户同期的修改")
	require.Len(t, repo.credWrites, 1)
	require.Equal(t, "free_trial", repo.credWrites[0][CursorCredMembership])
	require.Equal(t, "m", repo.credWrites[0][CursorCredMachineID])
	require.Empty(t, result.TrialStatus, "Sand Trial 默认不激活")
	require.Empty(t, repo.extraWrites)
}

func TestProvisionCursorAccount_MembershipFailureDoesNotOverwrite(t *testing.T) {
	repo := &cursorLifecycleRepo{accounts: []Account{{
		ID: 3, Platform: PlatformCursor,
		Credentials: map[string]any{CursorCredAccessToken: cursorSessionTok(t, "user_A", ""), CursorCredMembership: "pro"},
	}}}
	t.Cleanup(cursor.SetFetchMembershipFnForTest(func(string, string) (string, bool) { return "", false }))

	result, err := (&CursorOAuthService{accountRepo: repo}).ProvisionCursorAccount(context.Background(), 3, CursorProvisionOptions{})
	require.NoError(t, err)
	require.Empty(t, result.Membership)
	require.Empty(t, repo.updated)
	require.Empty(t, repo.credWrites, "拉取失败不能用空值覆盖已有套餐")

	_, err = (&CursorOAuthService{accountRepo: &cursorLifecycleRepo{accounts: []Account{{ID: 7, Platform: PlatformKiro}}}}).
		ProvisionCursorAccount(context.Background(), 7, CursorProvisionOptions{})
	require.Error(t, err)
}

func TestClassifyStartSandTrialError(t *testing.T) {
	require.Equal(t, cursor.TrialStatusActivated, cursor.ClassifyStartSandTrialError(nil))
	require.Equal(t, cursor.TrialStatusAlready, cursor.ClassifyStartSandTrialError(errors.New(`StartSandTrial: HTTP 400 {"error":"Already funded"}`)))
	require.Equal(t, cursor.TrialStatusNotEligible, cursor.ClassifyStartSandTrialError(errors.New("user not eligible")))
	require.Equal(t, cursor.TrialStatusFailed, cursor.ClassifyStartSandTrialError(errors.New("HTTP 500")))
}

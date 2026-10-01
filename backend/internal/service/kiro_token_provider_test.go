//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type kiroTokenProviderRepo struct {
	mockAccountRepoForGemini
	setErrorCalls int
	setErrorID    int64
	setErrorMsg   string
}

func (r *kiroTokenProviderRepo) SetError(_ context.Context, id int64, errorMsg string) error {
	r.setErrorCalls++
	r.setErrorID = id
	r.setErrorMsg = errorMsg
	return nil
}

type kiroTokenProviderSequenceRepo struct {
	kiroTokenProviderRepo
	accounts []*Account
	reads    int
}

func (r *kiroTokenProviderSequenceRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	if len(r.accounts) == 0 {
		return nil, errors.New("account not found")
	}
	idx := r.reads
	if idx >= len(r.accounts) {
		idx = len(r.accounts) - 1
	}
	r.reads++
	return r.accounts[idx], nil
}

type stubKiroAccountTokenRefresher struct {
	tokenInfo *KiroTokenInfo
	err       error
}

func (s *stubKiroAccountTokenRefresher) RefreshAccountToken(context.Context, *Account) (*KiroTokenInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.tokenInfo, nil
}

func (s *stubKiroAccountTokenRefresher) BuildAccountCredentials(tokenInfo *KiroTokenInfo) map[string]any {
	if tokenInfo == nil {
		return nil
	}
	creds := map[string]any{
		"access_token":  tokenInfo.AccessToken,
		"refresh_token": tokenInfo.RefreshToken,
		"expires_at":    tokenInfo.ExpiresAt,
	}
	if tokenInfo.ProfileArn != "" {
		creds["profile_arn"] = tokenInfo.ProfileArn
	}
	return creds
}

func TestKiroTokenProviderGetAccessTokenReturnsRefreshedToken(t *testing.T) {
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	account := &Account{
		ID:       88,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"expires_at":    past,
		},
	}
	repo := &refreshAPIAccountRepo{account: account}
	cache := &refreshAPICacheStub{lockResult: true}
	executor := &refreshAPIExecutorStub{
		needsRefresh: true,
		credentials: map[string]any{
			"access_token":  "new-access",
			"refresh_token": "rotated-refresh",
			"expires_at":    future,
		},
	}
	api := NewOAuthRefreshAPI(repo, cache)
	provider := NewKiroTokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(api, executor)

	token, err := provider.GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "new-access", token)
	require.Equal(t, "new-access", account.GetCredential("access_token"))
	require.Equal(t, "rotated-refresh", account.GetCredential("refresh_token"))
	require.Equal(t, 1, executor.refreshCalls)
}

func TestKiroTokenProviderForceRefreshInvalidGrantSetsError(t *testing.T) {
	account := &Account{
		ID:          42,
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "old-refresh"},
	}
	repo := &kiroTokenProviderRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{account.ID: account},
		},
	}
	provider := NewKiroTokenProvider(repo, nil, nil)
	provider.kiroOAuthService = &stubKiroAccountTokenRefresher{err: errors.New("invalid_grant: token revoked")}

	token, err := provider.ForceRefreshAccessToken(context.Background(), account)
	require.Error(t, err)
	require.Empty(t, token)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, account.ID, repo.setErrorID)
	require.Contains(t, repo.setErrorMsg, "Token refresh failed (non-retryable)")
	require.Contains(t, repo.setErrorMsg, "invalid_grant")
}

func TestKiroTokenProviderForceRefreshRaceRecoveryDoesNotSetError(t *testing.T) {
	usedAccount := &Account{
		ID:          42,
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "old-refresh"},
	}
	latestAccount := &Account{
		ID:          42,
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "new-refresh", "access_token": "fresh-access", "_token_version": int64(2)},
	}
	repo := &kiroTokenProviderSequenceRepo{accounts: []*Account{usedAccount, latestAccount}}
	provider := NewKiroTokenProvider(repo, nil, nil)
	provider.kiroOAuthService = &stubKiroAccountTokenRefresher{err: errors.New("invalid_grant: token revoked")}

	token, err := provider.ForceRefreshAccessToken(context.Background(), usedAccount)
	require.NoError(t, err)
	require.Equal(t, "fresh-access", token)
	require.Equal(t, 0, repo.setErrorCalls)
}

type countingKiroAccountTokenRefresher struct {
	stubKiroAccountTokenRefresher
	calls int
}

func (s *countingKiroAccountTokenRefresher) RefreshAccountToken(ctx context.Context, account *Account) (*KiroTokenInfo, error) {
	s.calls++
	return s.stubKiroAccountTokenRefresher.RefreshAccountToken(ctx, account)
}

// 刷新失败后的短窗口内不再打上游；窗口过后或成功后恢复。
func TestKiroTokenProviderForceRefreshCachesRecentFailure(t *testing.T) {
	account := &Account{ID: 4301, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "rt"}}
	provider := NewKiroTokenProvider(nil, nil, nil)
	refresher := &countingKiroAccountTokenRefresher{stubKiroAccountTokenRefresher: stubKiroAccountTokenRefresher{err: errors.New("upstream 503")}}
	provider.kiroOAuthService = refresher

	_, err := provider.ForceRefreshAccessToken(context.Background(), account)
	require.Error(t, err)
	_, err = provider.ForceRefreshAccessToken(context.Background(), account)
	require.EqualError(t, err, "upstream 503")
	require.Equal(t, 1, refresher.calls, "负缓存窗口内不得重复刷新")

	// 模拟窗口过期后上游恢复。
	provider.refreshFailures.entries.Store(account.ID, kiroRefreshFailure{err: err, until: time.Now().Add(-time.Second)})
	refresher.err = nil
	refresher.tokenInfo = &KiroTokenInfo{AccessToken: "fresh", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	token, err := provider.ForceRefreshAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "fresh", token)
	require.Equal(t, 2, refresher.calls)
	require.NoError(t, provider.refreshFailures.recent(account.ID, time.Now()))
}

func TestKiroRefreshFailureCacheTTLByErrorKind(t *testing.T) {
	var c kiroRefreshFailureCache
	now := time.Now()
	c.record(1, errors.New("upstream 503"), now)
	require.Error(t, c.recent(1, now.Add(kiroRefreshFailureTTL-time.Second)))
	require.NoError(t, c.recent(1, now.Add(kiroRefreshFailureTTL+time.Second)))

	c.record(2, errors.New("invalid_grant: token revoked"), now)
	require.Error(t, c.recent(2, now.Add(time.Minute)), "不可重试错误窗口更长")
}

func TestKiroTokenProviderForceRefreshDoesNotCacheCanceledContext(t *testing.T) {
	account := &Account{ID: 4302, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "rt"}}
	provider := NewKiroTokenProvider(nil, nil, nil)
	provider.kiroOAuthService = &stubKiroAccountTokenRefresher{err: context.Canceled}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := provider.ForceRefreshAccessToken(ctx, account)
	require.Error(t, err)
	require.NoError(t, provider.refreshFailures.recent(account.ID, time.Now()))
}

package service

import (
	"context"
	"strings"
	"time"
)

const kiroRefreshWindow = 15 * time.Minute

type KiroTokenRefresher struct {
	kiroOAuthService *KiroOAuthService
}

func NewKiroTokenRefresher(kiroOAuthService *KiroOAuthService) *KiroTokenRefresher {
	return &KiroTokenRefresher{
		kiroOAuthService: kiroOAuthService,
	}
}

func (r *KiroTokenRefresher) CacheKey(account *Account) string {
	return KiroTokenCacheKey(account)
}

func (r *KiroTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.Platform == PlatformKiro && account.Type == AccountTypeOAuth
}

func (r *KiroTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if strings.TrimSpace(account.GetCredential("refresh_token")) == "" {
		return false
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return true
	}
	return time.Until(*expiresAt) <= kiroRefreshWindow
}

func (r *KiroTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	tokenInfo, err := r.kiroOAuthService.RefreshAccountToken(ctx, account)
	if err != nil {
		return nil, err
	}

	newCredentials := r.kiroOAuthService.BuildAccountCredentials(tokenInfo)
	// 在新 refresh_token 覆盖旧值之前固化设备指纹。
	oldCredentials := ensureKiroMachineIDCredential(account, cloneKiroCredentials(account.Credentials))
	return MergeCredentials(oldCredentials, newCredentials), nil
}

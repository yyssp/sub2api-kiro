package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kiroTokenRefreshSkew = 3 * time.Minute
	kiroTokenCacheSkew   = 5 * time.Minute
)

type KiroTokenCache = GeminiTokenCache

type kiroAccountTokenRefresher interface {
	RefreshAccountToken(ctx context.Context, account *Account) (*KiroTokenInfo, error)
	BuildAccountCredentials(tokenInfo *KiroTokenInfo) map[string]any
}

type KiroTokenProvider struct {
	accountRepo      AccountRepository
	tokenCache       KiroTokenCache
	kiroOAuthService kiroAccountTokenRefresher
	refreshAPI       *OAuthRefreshAPI
	executor         OAuthRefreshExecutor
	refreshPolicy    ProviderRefreshPolicy
	refreshFailures  kiroRefreshFailureCache
}

// 刷新失败的短期负缓存时长。窗口内同账号的强制刷新直接返回上次的错误，
// 不再打 OIDC：上游 5xx/限流期间，每个 401/403 请求都去刷新会放大故障，
// 还可能触发刷新接口自身的限流。不可重试错误（refresh_token 失效）窗口更长。
const (
	kiroRefreshFailureTTL             = 15 * time.Second
	kiroRefreshNonRetryableFailureTTL = 5 * time.Minute
)

type kiroRefreshFailure struct {
	err   error
	until time.Time
}

type kiroRefreshFailureCache struct {
	entries sync.Map // map[int64]kiroRefreshFailure
}

func (c *kiroRefreshFailureCache) recent(accountID int64, now time.Time) error {
	v, ok := c.entries.Load(accountID)
	if !ok {
		return nil
	}
	failure, ok := v.(kiroRefreshFailure)
	if !ok || !now.Before(failure.until) {
		c.entries.Delete(accountID)
		return nil
	}
	return failure.err
}

func (c *kiroRefreshFailureCache) record(accountID int64, err error, now time.Time) {
	ttl := kiroRefreshFailureTTL
	if isNonRetryableRefreshError(err) {
		ttl = kiroRefreshNonRetryableFailureTTL
	}
	c.entries.Store(accountID, kiroRefreshFailure{err: err, until: now.Add(ttl)})
}

func (c *kiroRefreshFailureCache) clear(accountID int64) {
	c.entries.Delete(accountID)
}

func NewKiroTokenProvider(
	accountRepo AccountRepository,
	tokenCache KiroTokenCache,
	kiroOAuthService *KiroOAuthService,
) *KiroTokenProvider {
	return &KiroTokenProvider{
		accountRepo:      accountRepo,
		tokenCache:       tokenCache,
		kiroOAuthService: kiroOAuthService,
		refreshPolicy:    GeminiProviderRefreshPolicy(),
	}
}

func (p *KiroTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

func (p *KiroTokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

func (p *KiroTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformKiro || account.Type != AccountTypeOAuth {
		return "", errors.New("not a kiro oauth account")
	}

	cacheKey := KiroTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
	}

	expiresAt := account.GetCredentialAsTime("expires_at")
	needsRefresh := expiresAt == nil || time.Until(*expiresAt) <= kiroTokenRefreshSkew

	if needsRefresh && p.refreshAPI != nil && p.executor != nil {
		result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, p.executor, kiroTokenRefreshSkew)
		if err != nil {
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
		} else if result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache && p.tokenCache != nil {
				if token, cacheErr := p.tokenCache.GetAccessToken(ctx, cacheKey); cacheErr == nil && strings.TrimSpace(token) != "" {
					return token, nil
				}
			}
		} else {
			if result.Account != nil {
				account = result.Account
			}
			if len(result.NewCredentials) > 0 {
				account.Credentials = shallowCopyMap(result.NewCredentials)
			}
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	} else if needsRefresh && p.tokenCache != nil {
		locked, lockErr := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, 30*time.Second)
		if lockErr == nil && locked {
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		}
	}

	accessToken := account.GetCredential("access_token")
	if strings.TrimSpace(accessToken) == "" {
		return "", errors.New("access_token not found in credentials")
	}

	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil {
			accessToken = latestAccount.GetCredential("access_token")
			if strings.TrimSpace(accessToken) == "" {
				return "", errors.New("access_token not found after version check")
			}
		} else {
			ttl := 30 * time.Minute
			if expiresAt != nil {
				until := time.Until(*expiresAt)
				switch {
				case until > kiroTokenCacheSkew:
					ttl = until - kiroTokenCacheSkew
				case until > 0:
					ttl = until
				default:
					ttl = time.Minute
				}
			}
			_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
		}
	}

	return accessToken, nil
}

// KiroTokenCacheKey 返回账号级 access token 缓存 key。
// 必须账号唯一：曾用 client_id_hash / client_id 作 key，但它们标识的是 Kiro IDE
// 的 OAuth client 注册而非用户身份——同一台机器/同一次 device registration 导入的
// 多个 BuilderId/Enterprise 账号会共用同一个 client_id_hash，导致不同账号的 token、
// 刷新锁、用量查询互相覆盖串用。改用 account.ID，与其它 provider(Claude/OpenAI/Grok)一致。
func KiroTokenCacheKey(account *Account) string {
	if account == nil {
		return "kiro:account:0"
	}
	return "kiro:account:" + strconv.FormatInt(account.ID, 10)
}

func (p *KiroTokenProvider) ForceRefreshAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformKiro || account.Type != AccountTypeOAuth {
		return "", errors.New("not a kiro oauth account")
	}
	if p.kiroOAuthService == nil {
		return "", errors.New("kiro oauth service is nil")
	}

	if cached := p.refreshFailures.recent(account.ID, time.Now()); cached != nil {
		return "", cached
	}

	cacheKey := KiroTokenCacheKey(account)
	lockHeld := false
	if p.tokenCache != nil {
		locked, lockErr := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, 30*time.Second)
		if lockErr == nil && locked {
			lockHeld = true
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		}
	}

	if p.accountRepo != nil {
		if latestAccount, err := p.accountRepo.GetByID(ctx, account.ID); err == nil && latestAccount != nil {
			account = latestAccount
		}
	}

	tokenInfo, err := p.kiroOAuthService.RefreshAccountToken(ctx, account)
	if err != nil {
		if !lockHeld {
			if latestAccount, stale := CheckTokenVersion(ctx, account, p.accountRepo); stale && latestAccount != nil {
				account = latestAccount
				if accessToken := strings.TrimSpace(account.GetCredential("access_token")); accessToken != "" {
					_ = p.cacheAccessToken(ctx, account, accessToken)
					return accessToken, nil
				}
			}
		}
		if isNonRetryableRefreshError(err) && p.accountRepo != nil {
			errorMsg := "Token refresh failed (non-retryable): " + err.Error()
			_ = p.accountRepo.SetError(ctx, account.ID, errorMsg)
		}
		// 请求被取消不代表刷新失败，不能缓存。
		if ctx.Err() == nil {
			p.refreshFailures.record(account.ID, err, time.Now())
		}
		return "", err
	}
	p.refreshFailures.clear(account.ID)

	oldCredentials := ensureKiroMachineIDCredential(account, cloneKiroCredentials(account.Credentials))
	newCredentials := MergeCredentials(oldCredentials, p.kiroOAuthService.BuildAccountCredentials(tokenInfo))
	newCredentials["_token_version"] = time.Now().UnixMilli()
	if err := persistAccountCredentials(ctx, p.accountRepo, account, newCredentials); err != nil {
		return "", err
	}

	accessToken := strings.TrimSpace(account.GetCredential("access_token"))
	if accessToken == "" {
		accessToken = strings.TrimSpace(tokenInfo.AccessToken)
	}
	if accessToken == "" {
		return "", errors.New("access_token not found after kiro refresh")
	}

	// 刷新成功后解析并回填 profileArn（与后台刷新 postRefreshActions 保持一致）
	_ = kiroResolveAndPersistProfileArn(ctx, p.accountRepo, account, accessToken)

	if err := p.cacheAccessToken(ctx, account, accessToken); err != nil {
		return "", err
	}
	return accessToken, nil
}

func (p *KiroTokenProvider) cacheAccessToken(ctx context.Context, account *Account, accessToken string) error {
	if p.tokenCache == nil || account == nil || strings.TrimSpace(accessToken) == "" {
		return nil
	}
	ttl := 30 * time.Minute
	if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt != nil {
		until := time.Until(*expiresAt)
		switch {
		case until > kiroTokenCacheSkew:
			ttl = until - kiroTokenCacheSkew
		case until > 0:
			ttl = until
		default:
			ttl = time.Minute
		}
	}
	return p.tokenCache.SetAccessToken(ctx, KiroTokenCacheKey(account), accessToken, ttl)
}

package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

const (
	cursorTokenRefreshSkew = 5 * time.Minute
	cursorTokenCacheSkew   = 5 * time.Minute
)

// CursorTokenCache 复用既有的 token 缓存接口（与 Kiro 同样的别名手法），
// 避免为新平台另立一套缓存抽象。
type CursorTokenCache = GeminiTokenCache

// CursorTokenProvider 负责向转发链路提供可用的 Cursor access token。
//
// 与 Kiro 的差异：Cursor 的刷新是「用 refresh_token 换 access_token」的单步
// HTTP 调用（协议层 cursor.RefreshAuthToken），不经过 OAuthRefreshAPI 那套
// provider 配置，所以这里不持有 refreshAPI/executor。
type CursorTokenProvider struct {
	accountRepo AccountRepository
	tokenCache  CursorTokenCache
}

func NewCursorTokenProvider(accountRepo AccountRepository, tokenCache CursorTokenCache) *CursorTokenProvider {
	return &CursorTokenProvider{accountRepo: accountRepo, tokenCache: tokenCache}
}

// CursorTokenCacheKey 返回账号级 access token 缓存 key。
// 必须账号唯一——同一台设备导入的多个 Cursor 账号会共用 machine_id，
// 用它作 key 会让不同账号的 token 与刷新锁互相覆盖（Kiro 踩过同样的坑）。
func CursorTokenCacheKey(account *Account) string {
	if account == nil {
		return "cursor:account:0"
	}
	return "cursor:account:" + strconv.FormatInt(account.ID, 10)
}

// GetAccessToken 返回可用的 access token，必要时先刷新。
func (p *CursorTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformCursor {
		return "", errors.New("not a cursor account")
	}

	cacheKey := CursorTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
	}

	accessToken := strings.TrimSpace(account.GetCredential(CursorCredAccessToken))
	protoAccount := cursorProtocolAccount(account)
	now := time.Now()

	// 到期或即将到期才刷新。AccountExpiry 零值（解析不出 exp）时
	// AuthRefreshDue 返回 false——宁可等 401 再强刷，也不要对每个
	// 未知有效期的账号无条件打刷新接口。
	needsRefresh := accessToken == "" ||
		cursor.AuthRefreshExpired(protoAccount, now) ||
		cursor.AuthRefreshDue(protoAccount, now, cursorTokenRefreshSkew)

	if needsRefresh && strings.TrimSpace(account.GetCredential(CursorCredRefreshToken)) != "" {
		refreshed, err := p.ForceRefreshAccessToken(ctx, account)
		if err != nil {
			// 刷新失败但手上仍有一个未过期的 token 时继续用它：
			// 临时性失败不应让本次请求直接失败。
			if accessToken != "" && !cursor.AuthRefreshExpired(protoAccount, now) {
				return accessToken, nil
			}
			return "", err
		}
		return refreshed, nil
	}

	if accessToken == "" {
		return "", errors.New("access_token not found in credentials")
	}

	p.cacheAccessToken(ctx, account, accessToken)
	return accessToken, nil
}

// ForceRefreshAccessToken 强制用 refresh_token 兑换新的 access token 并落库。
func (p *CursorTokenProvider) ForceRefreshAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformCursor {
		return "", errors.New("not a cursor account")
	}

	cacheKey := CursorTokenCacheKey(account)
	lockHeld := false
	if p.tokenCache != nil {
		if locked, lockErr := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, 30*time.Second); lockErr == nil && locked {
			lockHeld = true
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		}
	}

	// 没抢到锁说明别的协程正在刷；先看它是否已经写好了新 token，
	// 避免同一账号并发刷新把 refresh token 轮换链打断。
	if !lockHeld && p.accountRepo != nil {
		if latest, err := p.accountRepo.GetByID(ctx, account.ID); err == nil && latest != nil {
			if token := strings.TrimSpace(latest.GetCredential(CursorCredAccessToken)); token != "" &&
				token != strings.TrimSpace(account.GetCredential(CursorCredAccessToken)) {
				return token, nil
			}
		}
	}

	refreshToken := strings.TrimSpace(account.GetCredential(CursorCredRefreshToken))
	if refreshToken == "" {
		return "", errors.New("refresh_token not found in credentials")
	}

	accessToken, rotatedRefresh, err := cursor.RefreshAuthToken(refreshToken)
	if err != nil {
		// ⚠️ 只有「确认失效」才停用账号。网络抖动/代理 EOF/5xx 一律不写
		// SetError，否则一次上游抖动会批量误停整个号池。
		if cursor.ConfirmedAuthRefreshFailure(err) && p.accountRepo != nil {
			_ = p.accountRepo.SetError(ctx, account.ID, cursor.AuthRefreshFailureReason(err))
		}
		return "", err
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", errors.New("access_token not found after cursor refresh")
	}

	newCredentials := MergeCredentials(account.Credentials, map[string]any{
		CursorCredAccessToken: accessToken,
	})
	// 上游轮换了 refresh token 才覆盖；返回空表示沿用旧值，
	// 写空会直接丢失续期能力。
	if rotated := strings.TrimSpace(rotatedRefresh); rotated != "" {
		newCredentials[CursorCredRefreshToken] = rotated
	}
	newCredentials["_token_version"] = time.Now().UnixMilli()
	if err := persistAccountCredentials(ctx, p.accountRepo, account, newCredentials); err != nil {
		return "", err
	}

	p.cacheAccessToken(ctx, account, accessToken)
	return accessToken, nil
}

// cursorAccessTokenTTL 由 JWT exp 推导缓存时长，预留 cursorTokenCacheSkew 提前量。
func cursorAccessTokenTTL(accessToken string) time.Duration {
	ttl := 30 * time.Minute
	expiry := cursor.JWTExpiry(accessToken)
	if expiry.IsZero() {
		return ttl
	}
	switch until := time.Until(expiry); {
	case until > cursorTokenCacheSkew:
		return until - cursorTokenCacheSkew
	case until > 0:
		return until
	default:
		return time.Minute
	}
}

func (p *CursorTokenProvider) cacheAccessToken(ctx context.Context, account *Account, accessToken string) {
	if p.tokenCache == nil || account == nil || strings.TrimSpace(accessToken) == "" {
		return
	}
	_ = p.tokenCache.SetAccessToken(ctx, CursorTokenCacheKey(account), accessToken, cursorAccessTokenTTL(accessToken))
}

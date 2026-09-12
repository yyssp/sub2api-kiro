package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// cursorRefreshWindow 是后台主动续期的提前量。
// 比 cursorTokenRefreshSkew(5min) 宽，让后台任务先于请求路径把 token 换掉，
// 避免请求路径上出现同步刷新的尾延迟。
const cursorRefreshWindow = 15 * time.Minute

// CursorTokenRefresher 接入既有的 TokenRefreshService 注册表。
//
// ⚠️ 与 Kiro 的结构性差异：Kiro 的刷新走 KiroOAuthService（有 provider 配置、
// 有 BuildAccountCredentials），Cursor 只是「refresh_token 换 access_token」的
// 单步 HTTP 调用，没有 OAuth provider 概念，所以这里直接委托给
// CursorTokenProvider，不引入一个空壳 OAuthService。
type CursorTokenRefresher struct{}

func NewCursorTokenRefresher() *CursorTokenRefresher {
	return &CursorTokenRefresher{}
}

func (r *CursorTokenRefresher) CacheKey(account *Account) string {
	return CursorTokenCacheKey(account)
}

func (r *CursorTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.Platform == PlatformCursor && account.Type == AccountTypeOAuth
}

func (r *CursorTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if strings.TrimSpace(account.GetCredential(CursorCredRefreshToken)) == "" {
		return false
	}
	if strings.TrimSpace(account.GetCredential(CursorCredAccessToken)) == "" {
		return true
	}

	// ⚠️ 解析不出 exp（非 JWT / 字段缺失）时返回 false，而不是 true。
	// 返回 true 会让后台任务对这类账号每轮都打一次刷新接口，
	// 把一个"读不出有效期"的小问题放大成对上游的持续压测。
	// 这类账号由请求路径上的 401 强刷兜底。
	pa := cursorProtocolAccount(account)
	now := time.Now()
	return cursor.AuthRefreshExpired(pa, now) || cursor.AuthRefreshDue(pa, now, cursorRefreshWindow)
}

// Refresh 返回合并后的 credentials 供上层落库。
//
// ⚠️ 这里刻意不自己写库：TokenRefreshService 负责持久化。
// CursorTokenProvider.ForceRefreshAccessToken 会写一次库，
// 所以这里用底层的 refreshCursorCredentials 而不是复用它，避免重复写。
func (r *CursorTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if !r.CanRefresh(account) {
		return nil, errors.New("cursor refresher: not a refreshable cursor account")
	}
	refreshToken := strings.TrimSpace(account.GetCredential(CursorCredRefreshToken))
	if refreshToken == "" {
		return nil, errors.New("cursor refresher: missing refresh_token")
	}

	accessToken, rotatedRefresh, err := cursor.RefreshAuthToken(refreshToken)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("cursor refresher: upstream returned empty access_token")
	}

	updates := map[string]any{CursorCredAccessToken: accessToken}
	// 空 rotatedRefresh 表示上游不轮换，沿用旧值。写空会让账号永久失去续期能力。
	if strings.TrimSpace(rotatedRefresh) != "" {
		updates[CursorCredRefreshToken] = rotatedRefresh
	}
	// email 从 JWT 反解，便于管理台展示；解不出就不覆盖既有值。
	if email := strings.TrimSpace(cursor.JWTEmail(accessToken)); email != "" {
		updates[CursorCredEmail] = email
	}

	_ = ctx
	return MergeCredentials(account.Credentials, updates), nil
}

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// Cursor Dashboard API Key（crsr_）账号的凭证生命周期。
//
// 与 session token 账号的唯一区别在「怎么拿到新 access_token」：
//   - session 账号：refresh_token → /oauth/token；
//   - API Key 账号：crsr_ key → /auth/exchange_user_api_key。兑换出的 JWT（type=api_key_token）
//     寿命 60 分钟，且 /oauth/token 对它返回 shouldLogout，不能按 refresh 续期。
//
// 所以这类账号把 crsr_ key 存在 credentials.api_key，access_token 只是它派生的缓存，
// 并且**绝不**写入 refresh_token——否则后台刷新会拿 api_key_token 去打 /oauth/token，
// 收到 shouldLogout 后被判成「确认失效」直接停号。

// isCursorAPIKeyAccount 判断凭证是否属于 API Key 账号。
func isCursorAPIKeyAccount(credentials map[string]any) bool {
	key, _ := credentials[CursorCredAPIKey].(string)
	return cursor.IsAPIKey(key)
}

// cursorAPIKeyExchangeUpdates 用 crsr_ key 兑换 access token，返回需要写回的凭证字段。
//
// 写回 session（uid::JWT）与 email：管理台展示、导入去重（按 uid）都依赖它们。
// 显式清空 refresh_token：防止旧值（例如由 session 账号改成 API Key 账号）把后台
// 刷新引到 /oauth/token 上。
func cursorAPIKeyExchangeUpdates(apiKey, proxyURL string) (map[string]any, error) {
	access, err := cursor.ExchangeAPIKeyVia(apiKey, proxyURL)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{
		CursorCredAccessToken:  access,
		CursorCredRefreshToken: "",
	}
	if _, session, _ := cursor.NormalizeToken(access); strings.TrimSpace(session) != "" {
		updates[CursorCredSession] = session
	}
	if email := strings.TrimSpace(cursor.JWTEmail(access)); email != "" {
		updates[CursorCredEmail] = email
	}
	return updates, nil
}

// exchangeCursorAPIKeyForCreate 是建号时的 API Key 兑换：凭证里带 crsr_ key 就先兑换出
// access token 再落库。
//
// ⚠️ 与 web token 兑换失败「保留原 token 继续建号」不同，这里兑换失败必须拒绝建号：
// API Key 账号没有任何可用的 access token，建出来的账号一定不可用，还会被选号命中。
func exchangeCursorAPIKeyForCreate(platform string, credentials map[string]any, proxyURL string) (map[string]any, error) {
	if platform != PlatformCursor || !isCursorAPIKeyAccount(credentials) {
		return credentials, nil
	}
	key, _ := credentials[CursorCredAPIKey].(string)
	updates, err := cursorAPIKeyExchangeUpdates(strings.TrimSpace(key), proxyURL)
	if err != nil {
		if errors.Is(err, cursor.ErrAPIKeyRejected) {
			return nil, ErrCursorAPIKeyRejected
		}
		return nil, fmt.Errorf("cursor API key exchange failed: %w", err)
	}
	merged := MergeCredentials(credentials, updates)
	delete(merged, CursorCredRefreshToken)
	return merged, nil
}

// refreshCursorAPIKeyAccessToken 用 crsr_ key 重新兑换 access token 并原子落库。
// 由 CursorTokenProvider / CursorTokenRefresher 在 token 临期或缺失时调用。
func refreshCursorAPIKeyAccessToken(ctx context.Context, repo AccountRepository, account *Account) (string, error) {
	key := strings.TrimSpace(account.GetCredential(CursorCredAPIKey))
	updates, err := cursorAPIKeyExchangeUpdates(key, cursorAccountProxyURL(account))
	if err != nil {
		// 只有上游明确拒绝 key（401/403）才停号；网络/代理/5xx 只让本次失败，账号保留。
		if errors.Is(err, cursor.ErrAPIKeyRejected) && repo != nil {
			_ = repo.SetError(ctx, account.ID, "Cursor API Key 被拒绝（已失效或被删除），需要重新导入")
		}
		return "", err
	}
	merged := MergeCredentials(account.Credentials, updates)
	delete(merged, CursorCredRefreshToken)
	merged["_token_version"] = time.Now().UnixMilli()
	if err := persistAccountCredentials(ctx, repo, account, merged); err != nil {
		return "", err
	}
	access, _ := updates[CursorCredAccessToken].(string)
	return access, nil
}

// exchangeCursorAPIKeyOnCreate 是 CreateAccount 的 Cursor 旁挂钩子：解析账号绑定的代理后
// 调用 exchangeCursorAPIKeyForCreate。非 Cursor / 非 API Key 凭证原样返回。
func (s *adminServiceImpl) exchangeCursorAPIKeyOnCreate(ctx context.Context, input *CreateAccountInput) (map[string]any, error) {
	if input == nil {
		return nil, nil
	}
	if input.Platform != PlatformCursor || !isCursorAPIKeyAccount(input.Credentials) {
		return input.Credentials, nil
	}
	proxyURL := ""
	if input.ProxyID != nil && s.proxyRepo != nil {
		if p, err := s.proxyRepo.GetByID(ctx, *input.ProxyID); err == nil && p != nil {
			proxyURL = p.URL()
		}
	}
	return exchangeCursorAPIKeyForCreate(input.Platform, input.Credentials, proxyURL)
}

// cursorAccessTokenDue 判断 access token 是否已过期或进入提前续期窗口。
//
// ⚠️ 不能直接用 cursor.AuthRefreshDue：它要求凭证里有 refresh_token，API Key 账号没有，
// 结果是临期 token 永远不会被提前续期，只能等完全过期后请求失败一次才刷新。
// 这里对 API Key 账号单独按 JWT exp 判断；其余账号沿用 AuthRefreshDue 的原语义。
func cursorAccessTokenDue(account *Account, pa cursor.Account, now time.Time, skew time.Duration) bool {
	if cursor.AuthRefreshExpired(pa, now) {
		return true
	}
	if isCursorAPIKeyAccount(account.Credentials) {
		return !pa.AccountExpiry.IsZero() && !now.Before(pa.AccountExpiry.Add(-skew))
	}
	return cursor.AuthRefreshDue(pa, now, skew)
}

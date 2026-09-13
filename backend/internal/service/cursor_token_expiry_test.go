//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// Cursor 的 token 有效期必须来自 access token 自身的 JWT exp，
// 不能来自 accounts.expires_at。
//
// 两个方向都会出事，所以两个方向都钉住：
//
//  1. 读 accounts.expires_at ⇒ 导入路径从不写该列，恒为 NULL ⇒ AccountExpiry
//     恒为零值 ⇒ AuthRefreshDue/AuthRefreshExpired 恒为 false ⇒ 主动续期永不触发。
//  2. 写 accounts.expires_at ⇒ 该列语义是「账号/订阅有效期」，被
//     AutoPauseExpiredAccounts 扫描，对 auto_pause_on_expired=TRUE（列默认值）
//     的账号置 schedulable=FALSE。access token 只有几小时寿命，
//     写进去等于让整个 Cursor 号池在几小时内被自动暂停扫描停用。
func TestCursorProtocolAccountExpiryComesFromAccessToken(t *testing.T) {
	exp := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	token := makeCursorJWT(t, "session", "", "auth0|test", exp)

	// accounts.expires_at 故意留空——真实导入路径就是这样。
	account := &Account{
		ID:          1,
		Platform:    PlatformCursor,
		Schedulable: true,
		Credentials: map[string]any{
			CursorCredAccessToken:  token,
			CursorCredRefreshToken: "rt-1",
		},
	}

	pa := cursorProtocolAccount(account)
	require.False(t, pa.AccountExpiry.IsZero(),
		"AccountExpiry 为零值：主动续期判定会恒为 false，token 只能等 401 被动刷新")
	require.Equal(t, exp, pa.AccountExpiry.UTC().Truncate(time.Second))

	// 有效期已知 ⇒ 到期前进入刷新窗口时必须判定为「该刷了」。
	require.True(t, cursor.AuthRefreshDue(pa, exp.Add(-time.Minute), cursorTokenRefreshSkew),
		"临近到期未判定为需要刷新")
	require.True(t, cursor.AuthRefreshExpired(pa, exp.Add(time.Minute)),
		"已过期未判定为过期")
	require.False(t, cursor.AuthRefreshExpired(pa, exp.Add(-time.Hour)),
		"未到期被误判为过期，会触发无谓强刷")
}

// accounts.expires_at 是订阅有效期，绝不能参与 token 刷新判定：
// 即便它被管理端设成了一个很远的未来值，token 自身过期时仍必须判定为过期。
func TestCursorProtocolAccountIgnoresSubscriptionExpiresAt(t *testing.T) {
	tokenExp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	subscriptionExp := time.Now().AddDate(1, 0, 0)

	account := &Account{
		ID:          2,
		Platform:    PlatformCursor,
		Schedulable: true,
		ExpiresAt:   &subscriptionExp, // 管理端录入的订阅有效期
		Credentials: map[string]any{
			CursorCredAccessToken:  makeCursorJWT(t, "session", "", "auth0|test", tokenExp),
			CursorCredRefreshToken: "rt-2",
		},
	}

	pa := cursorProtocolAccount(account)
	require.Equal(t, tokenExp, pa.AccountExpiry.UTC().Truncate(time.Second),
		"AccountExpiry 取到了 accounts.expires_at（订阅有效期）而不是 token 的 exp")
	require.True(t, cursor.AuthRefreshExpired(pa, tokenExp.Add(time.Minute)),
		"token 已过期却因订阅未到期被判定为有效")
}

// 解析不出 exp（非 JWT / 缺 exp 声明）时必须退化成零值。
// 协议层对零值的约定是「有效期未知，不主动刷新，等 401 再强刷」——
// 瞎猜一个到期时间会对整池未知有效期的账号无条件打刷新接口。
func TestCursorProtocolAccountExpiryUnknownStaysZero(t *testing.T) {
	for name, token := range map[string]string{
		"空 token":  "",
		"非 JWT":    "not-a-jwt",
		"缺 exp 声明": makeCursorJWT(t, "session", "", "auth0|test", time.Time{}),
	} {
		t.Run(name, func(t *testing.T) {
			pa := cursorProtocolAccount(&Account{
				ID:          3,
				Platform:    PlatformCursor,
				Schedulable: true,
				Credentials: map[string]any{
					CursorCredAccessToken:  token,
					CursorCredRefreshToken: "rt-3",
				},
			})
			require.True(t, pa.AccountExpiry.IsZero(), "有效期未知时应为零值")
			require.False(t, cursor.AuthRefreshDue(pa, time.Now(), cursorTokenRefreshSkew),
				"有效期未知却判定为需要刷新，会对整池账号无条件打刷新接口")
		})
	}
}

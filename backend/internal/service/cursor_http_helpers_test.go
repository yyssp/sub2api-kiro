package service

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

func TestReadCursorQuota_MissingOrEmptyDefaultsToUnknown(t *testing.T) {
	// ⚠️ 缺失必须退化成 unknown 而不是 0%：
	// 0% 会被 AccountUsableForModel 当作"额度充足"，把从未抓取过额度的
	// 账号误判为可用；反之把 unknown 当耗尽会误清空号池。
	cases := map[string]*Account{
		"nil account": nil,
		"nil extra":   {Platform: PlatformCursor},
		"empty extra": {Platform: PlatformCursor, Extra: map[string]any{}},
		"nil value":   {Platform: PlatformCursor, Extra: map[string]any{CursorExtraQuotaKey: nil}},
		"wrong type":  {Platform: PlatformCursor, Extra: map[string]any{CursorExtraQuotaKey: "not-an-object"}},
	}
	for name, acc := range cases {
		t.Run(name, func(t *testing.T) {
			q := readCursorQuota(acc)
			require.Equal(t, CursorQuotaStateUnknown, q.Cursor.State)
			require.Equal(t, CursorQuotaStateUnknown, q.Other.State)
			require.Equal(t, CursorQuotaStateUnknown, q.GrokBot.State)
			require.Nil(t, q.FetchedAt)
		})
	}
}

func TestCursorQuota_WriteReadRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	acc := &Account{Platform: PlatformCursor}
	want := CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 12.5},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 3, Enabled: true},
		FetchedAt: &now,
	}
	writeCursorQuota(acc, want)

	got := readCursorQuota(acc)
	require.Equal(t, want.Cursor, got.Cursor)
	require.Equal(t, want.Other, got.Other)
	require.Equal(t, want.GrokBot, got.GrokBot)
	require.NotNil(t, got.FetchedAt)
	require.True(t, now.Equal(*got.FetchedAt))
}

func TestCursorQuota_SurvivesJSONBRoundTrip(t *testing.T) {
	// extra 落库再读回会经过 JSON 编解码，数值变成 float64。
	// 这条用例锁住「过一次 JSON 之后语义不变」。
	now := time.Now().UTC().Truncate(time.Second)
	src := &Account{Platform: PlatformCursor}
	writeCursorQuota(src, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 42.25},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateRequestFailed, Percent: 7, Enabled: true},
		FetchedAt: &now,
	})

	raw, err := json.Marshal(src.Extra)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	got := readCursorQuota(&Account{Platform: PlatformCursor, Extra: decoded})
	require.Equal(t, CursorQuotaStateExhausted, got.Cursor.State)
	require.Equal(t, 100.0, got.Cursor.Percent)
	require.Equal(t, CursorQuotaStateAvailable, got.Other.State)
	require.Equal(t, 42.25, got.Other.Percent)
	require.Equal(t, CursorQuotaStateRequestFailed, got.GrokBot.State)
	require.True(t, got.GrokBot.Enabled)
	require.NotNil(t, got.FetchedAt)
}

func TestReadCursorQuota_ToleratesDirtyNumericTypes(t *testing.T) {
	// 历史数据 / 管理端手工写入可能让 percent 变成字符串或 json.Number。
	acc := &Account{Platform: PlatformCursor, Extra: map[string]any{
		CursorExtraQuotaKey: map[string]any{
			cursor.QuotaBucketCursor:  map[string]any{"state": "available", "percent": "55.5"},
			cursor.QuotaBucketOther:   map[string]any{"state": "AVAILABLE", "percent": json.Number("10")},
			cursor.QuotaBucketGrokBot: map[string]any{"state": "  exhausted  ", "percent": 100},
		},
	}}
	q := readCursorQuota(acc)
	require.Equal(t, 55.5, q.Cursor.Percent)
	require.Equal(t, CursorQuotaStateAvailable, q.Cursor.State)
	require.Equal(t, 10.0, q.Other.Percent)
	require.Equal(t, CursorQuotaStateAvailable, q.Other.State, "state 应大小写不敏感")
	require.Equal(t, CursorQuotaStateExhausted, q.GrokBot.State, "state 应容忍空白")
}

func TestReadCursorQuota_UnknownStateStringDegradesToUnknown(t *testing.T) {
	acc := &Account{Platform: PlatformCursor, Extra: map[string]any{
		CursorExtraQuotaKey: map[string]any{
			cursor.QuotaBucketCursor: map[string]any{"state": "bogus-state", "percent": 10},
		},
	}}
	require.Equal(t, CursorQuotaStateUnknown, readCursorQuota(acc).Cursor.State)
}

func TestIsCursorDirectModeAccount(t *testing.T) {
	tests := []struct {
		name string
		acc  *Account
		want bool
	}{
		{"nil", nil, false},
		{"其它平台", &Account{Platform: PlatformKiro, Type: AccountTypeOAuth}, false},
		{"cursor oauth", &Account{Platform: PlatformCursor, Type: AccountTypeOAuth}, true},
		{"cursor api_key 无 base_url", &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey}, true},
		{"cursor api_key 有 base_url", &Account{
			Platform: PlatformCursor, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://example.invalid"},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isCursorDirectModeAccount(tt.acc))
		})
	}
}

func TestCursorAccountUsableForModel_SingleBucketExhaustionDoesNotDisableAccount(t *testing.T) {
	// ⚠️ 这是三桶设计的核心断言：cursor 桶耗尽，grokbot 桶仍可用时，
	// 账号对 Claude Code 模型必须仍然可调度。
	now := time.Now().UTC()
	acc := &Account{
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		Schedulable: true,
		Status:      StatusActive,
		Credentials: map[string]any{CursorCredMembership: "pro"},
	}
	writeCursorQuota(acc, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 0},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 0, Enabled: true},
		FetchedAt: &now,
	})

	require.False(t, CursorAccountUsableForModel(acc, "auto"),
		"cursor 桶已耗尽，auto 模型应不可用")
	require.True(t, CursorAccountUsableForModel(acc, "claude-sonnet-4.5"),
		"grokbot 桶可用时 Claude Code 模型必须仍可调度——单桶耗尽不得整号停摆")
}

func TestCursorAccountUsableForModel_NeverFetchedQuotaIsUsable(t *testing.T) {
	// FetchedAt 为 nil（从未抓取）时不得当成 0% 可用之外的任何判断，
	// 更不能当成耗尽。
	acc := &Account{
		Platform: PlatformCursor, Type: AccountTypeOAuth,
		Schedulable: true, Status: StatusActive,
		Credentials: map[string]any{CursorCredMembership: "pro"},
	}
	require.True(t, CursorAccountUsableForModel(acc, "auto"))
}

func TestCursorAccountUsableForModel_CredentialFailureBlocksAccount(t *testing.T) {
	now := time.Now().UTC()
	acc := &Account{
		Platform: PlatformCursor, Type: AccountTypeOAuth,
		Schedulable: true, Status: StatusActive,
		ErrorMessage: "Cursor 认证失败：token expired",
		Credentials:  map[string]any{CursorCredMembership: "pro"},
	}
	writeCursorQuota(acc, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateAvailable},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateAvailable, Enabled: true},
		FetchedAt: &now,
	})
	require.False(t, CursorAccountUsableForModel(acc, "auto"),
		"error_message 指示凭证失效时不应继续调度")
}

func TestCursorProtocolAccount_MapsCredentialsAndRuntimeStateSeparately(t *testing.T) {
	expires := time.Now().Add(2 * time.Hour).UTC()
	now := time.Now().UTC().Truncate(time.Second)
	acc := &Account{
		ID:       42,
		Platform: PlatformCursor,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		// 运行时状态走既有列，不进 credentials
		Schedulable:  false,
		ErrorMessage: "boom",
		ExpiresAt:    &expires,
		Credentials: map[string]any{
			CursorCredAccessToken:  " tok ",
			CursorCredRefreshToken: "refresh",
			CursorCredSession:      "uid::tok",
			CursorCredMachineID:    "machine",
			CursorCredEmail:        "a@example.com",
			CursorCredMembership:   "pro",
		},
	}
	writeCursorQuota(acc, CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 20},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 30},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100, Enabled: true},
		FetchedAt: &now,
	})

	got := cursorProtocolAccount(acc)
	require.Equal(t, int64(42), got.ID)
	require.Equal(t, "tok", got.AccessToken, "应 trim 空白")
	require.Equal(t, "machine", got.MachineID)
	require.Equal(t, "pro", got.Membership)
	require.Equal(t, 20.0, got.CursorModelsPct)
	require.Equal(t, 30.0, got.OtherModelsPct)
	require.Equal(t, CursorQuotaStateExhausted, got.GrokBotState)
	require.True(t, got.GrokBotEnabled)
	// 运行时状态来自列
	require.True(t, got.Disabled, "Schedulable=false 应投影成 Disabled")
	require.Equal(t, "boom", got.LastError)
	// AccountExpiry 是 access token 自身的有效期（JWT exp），与 accounts.expires_at
	// （订阅有效期）无关。这里的 access token 不是 JWT，解不出 exp ⇒ 必须是零值，
	// 而不是回落到 ExpiresAt 列。详见 cursor_token_expiry_test.go。
	require.True(t, got.AccountExpiry.IsZero(),
		"AccountExpiry 取到了 accounts.expires_at；该列是订阅有效期，"+
			"被 AutoPauseExpiredAccounts 扫描，不能与 token 有效期混用")
	require.True(t, now.Equal(got.UsageAt))
}

func TestValidateCursorQuotaFromExtra(t *testing.T) {
	t.Run("缺失或为空时通过", func(t *testing.T) {
		require.NoError(t, ValidateCursorQuotaFromExtra(nil))
		require.NoError(t, ValidateCursorQuotaFromExtra(map[string]any{}))
		require.NoError(t, ValidateCursorQuotaFromExtra(map[string]any{CursorExtraQuotaKey: nil}))
	})
	t.Run("合法结构通过", func(t *testing.T) {
		require.NoError(t, ValidateCursorQuotaFromExtra(map[string]any{
			CursorExtraQuotaKey: map[string]any{
				cursor.QuotaBucketCursor: map[string]any{"state": "available", "percent": 10},
				cursor.QuotaBucketOther:  map[string]any{"state": "exhausted", "percent": "100"},
			},
		}))
	})
	t.Run("非对象被拒", func(t *testing.T) {
		require.Error(t, ValidateCursorQuotaFromExtra(map[string]any{CursorExtraQuotaKey: "nope"}))
		require.Error(t, ValidateCursorQuotaFromExtra(map[string]any{
			CursorExtraQuotaKey: map[string]any{cursor.QuotaBucketCursor: "nope"},
		}))
	})
	t.Run("未知 state 被拒", func(t *testing.T) {
		require.Error(t, ValidateCursorQuotaFromExtra(map[string]any{
			CursorExtraQuotaKey: map[string]any{
				cursor.QuotaBucketCursor: map[string]any{"state": "bogus"},
			},
		}))
	})
	t.Run("越界或非数值 percent 被拒", func(t *testing.T) {
		// "NaN"/"Inf" 能被 strconv.ParseFloat 成功解析，且 NaN 的任何比较
		// 都为 false，会同时穿过区间校验和 `pct < 100` 判定——必须显式挡住。
		for _, bad := range []any{-1, 101, "NaN", "Inf", "-Inf", math.NaN(), math.Inf(1), "abc"} {
			require.Error(t, ValidateCursorQuotaFromExtra(map[string]any{
				CursorExtraQuotaKey: map[string]any{
					cursor.QuotaBucketCursor: map[string]any{"percent": bad},
				},
			}), "percent=%v 应被拒", bad)
		}
	})
}

func TestReadCursorQuota_NaNPercentDegradesInsteadOfDisablingAccount(t *testing.T) {
	// 库里已有的脏数据绕不过管理端校验，只能在读口兜住：
	// NaN 若被采信，CursorModelsPct < 100 恒为 false，账号会被静默停调度。
	now := time.Now().UTC()
	acc := &Account{
		Platform: PlatformCursor, Type: AccountTypeOAuth,
		Schedulable: true, Status: StatusActive,
		Credentials: map[string]any{CursorCredMembership: "pro"},
		Extra: map[string]any{CursorExtraQuotaKey: map[string]any{
			cursor.QuotaBucketCursor: map[string]any{"state": "available", "percent": "NaN"},
			"fetched_at":             now.Format(time.RFC3339),
		}},
	}
	q := readCursorQuota(acc)
	require.Equal(t, 0.0, q.Cursor.Percent, "NaN 应被丢弃而非采信")
	require.False(t, math.IsNaN(q.Cursor.Percent))
	require.True(t, CursorAccountUsableForModel(acc, "auto"),
		"脏 percent 不得把账号静默判成不可调度")
}

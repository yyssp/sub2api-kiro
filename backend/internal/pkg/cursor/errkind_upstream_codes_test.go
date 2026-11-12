package cursor

import "testing"

// 本组钉的是 errkind.go 新增的四类上游错误识别，以及它们在 ClassifyCursorErr
// 判定顺序里的位置。样例文案取自社区实现记录的真实上游响应
// （YeautyYE src/retry.rs 的测试用例、mediavee cpa-cursor 的错误码表）。

// 版本过旧借用 vision rate limit 错误码：必须识别出来，且不得被当成额度或限流。
func TestIsOutdatedClientErr(t *testing.T) {
	for _, text := range []string{
		"Connect error 429: ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT: Update Required — Your version of Cursor is no longer supported. [resource_exhausted]",
		"ERROR_OUTDATED_CLIENT: please update",
		"Your version of Cursor is no longer supported",
	} {
		if !IsOutdatedClientErr(text) {
			t.Errorf("IsOutdatedClientErr(%q) = false, want true", text)
		}
		if got := ClassifyCursorErr(text); got != ErrTransient {
			t.Errorf("ClassifyCursorErr(%q) = %v, want ErrTransient（版本过旧不得标桶也不得停号）", text, got)
		}
	}
	// 真正的 Grok Bot 额度耗尽同样用 vision 错误码，但没有 update required：仍是额度。
	quota := "ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT: You are out of usage - Upgrade to a paid plan to use more Grok Bot"
	if IsOutdatedClientErr(quota) {
		t.Fatalf("真实额度耗尽不得被识别成版本过旧: %q", quota)
	}
	if got := ClassifyCursorErr(quota); got != ErrQuota {
		t.Fatalf("Grok Bot 额度耗尽应为 ErrQuota, got %v", got)
	}
}

// 封号必须停号，且优先于额度措辞——否则只标桶不停号，号会被反复选中。
func TestIsAccountBlockedErr(t *testing.T) {
	for _, text := range []string{
		"ERROR_SUSPICIOUS_USAGE_BLOCKED: unusual usage detected, usage limit applied",
		"ERROR_ACCOUNT_CLOSED: this account has been closed",
	} {
		if !IsAccountBlockedErr(text) {
			t.Errorf("IsAccountBlockedErr(%q) = false, want true", text)
		}
		if got := ClassifyCursorErr(text); got != ErrAuth {
			t.Errorf("ClassifyCursorErr(%q) = %v, want ErrAuth", text, got)
		}
	}
	if IsAccountBlockedErr("You are out of usage") {
		t.Fatal("普通额度耗尽不得被当成封号")
	}
}

// 用户级限流：PRO 那条带 "exceeded your usage limit"，修复前被判成额度耗尽
// 而永久标满桶；FREE 那条被判成纯瞬时，换号后账号不进冷却。
func TestIsUserRateLimitErr(t *testing.T) {
	for _, text := range []string{
		"Connect error 429: ERROR_PRO_USER_RATE_LIMIT_EXCEEDED: Rate limit exceeded. — You have exceeded your usage limit. [resource_exhausted]",
		"Connect error 429: ERROR_FREE_USER_RATE_LIMIT_EXCEEDED: Rate limit exceeded [resource_exhausted]",
		"ERROR_USER_RATE_LIMIT_EXCEEDED",
		"ERROR_API_RATE_LIMIT_EXCEEDED: named model allowance exhausted",
	} {
		if !IsUserRateLimitErr(text) {
			t.Errorf("IsUserRateLimitErr(%q) = false, want true", text)
		}
		if got := ClassifyCursorErr(text); got == ErrQuota {
			t.Errorf("ClassifyCursorErr(%q) = ErrQuota：用户级限流会自愈，不得标桶耗尽", text)
		}
	}
	// 通用 ERROR_RATE_LIMITED 不是用户级限流：保持原有瞬时语义。
	if IsUserRateLimitErr("ERROR_RATE_LIMITED: too many requests") {
		t.Fatal("通用限流不得被当成用户级限流")
	}
}

// 地区封锁：地点词与否定词必须同时出现；区域故障措辞不得误伤。
func TestIsRegionBlockedErr(t *testing.T) {
	for _, text := range []string{
		"Connect error 502: ERROR_OPENAI: This model is not available in your country or region [internal]",
		"Connect error 403: ERROR_CUSTOM_MESSAGE: Model not available in your region [permission_denied]",
		"ERROR_UNSUPPORTED_REGION",
		"该模型在您所在的地区不支持",
	} {
		if !IsRegionBlockedErr(text) {
			t.Errorf("IsRegionBlockedErr(%q) = false, want true", text)
		}
		// ⚠️ "Connect error 403" 曾因通用兜底的 "403" 被判成凭证失效而停号。
		if got := ClassifyCursorErr(text); got == ErrAuth {
			t.Errorf("ClassifyCursorErr(%q) = ErrAuth：地区封锁是出口问题，不得停号", text)
		}
	}
	for _, text := range []string{
		"Connect error 502: regional endpoint unavailable [unavailable]",
		"region us-east-1 is degraded, try again",
		"ERROR_UNAUTHORIZED: not logged in",
	} {
		if IsRegionBlockedErr(text) {
			t.Errorf("IsRegionBlockedErr(%q) = true, want false（只有地点词不构成地区封锁）", text)
		}
	}
}

// 判定顺序回归：新增规则不得破坏「high load 先于额度」「额度先于限流」。
func TestClassifyCursorErr_ExistingOrderPreserved(t *testing.T) {
	cases := map[string]ErrKind{
		"ERROR_RESOURCE_EXHAUSTED: high load, please switch to auto": ErrTransient,
		"You are out of usage - Upgrade to a paid plan":              ErrQuota,
		"ERROR_RATE_LIMITED: rate limit, try again":                  ErrTransient,
		"ERROR_UNAUTHORIZED: not logged in":                          ErrAuth,
	}
	for text, want := range cases {
		if got := ClassifyCursorErr(text); got != want {
			t.Errorf("ClassifyCursorErr(%q) = %v, want %v", text, got, want)
		}
	}
}

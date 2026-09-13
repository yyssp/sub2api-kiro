//go:build unit

package service

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestPrepareCursorMachineIDForCreate_MintsOnCreate 是 B 项落库半边的主护栏。
// 纯文本导入（每行一个 token，最常见的导入方式）不带 machine_id，
// 建号时必须铸造一个并落库，否则协议层永远只能用派生值。
func TestPrepareCursorMachineIDForCreate_MintsOnCreate(t *testing.T) {
	creds := map[string]any{CursorCredAccessToken: "jwt-token"}

	out := prepareCursorMachineIDForCreate(PlatformCursor, creds)

	machineID, _ := out[CursorCredMachineID].(string)
	if strings.TrimSpace(machineID) == "" {
		t.Fatal("建号时未铸造 machine_id，设备指纹无法固化")
	}
	if len(machineID) != 64 {
		t.Fatalf("machine_id 应为 64 个十六进制字符, 实际 %d: %q", len(machineID), machineID)
	}
	if _, err := hex.DecodeString(machineID); err != nil {
		t.Fatalf("machine_id 应是合法 hex: %v", err)
	}
}

// TestPrepareCursorMachineIDForCreate_PreservesImported 导出文件带了
// machine_id 时必须沿用，不能覆盖——那是账号已有的设备身份。
func TestPrepareCursorMachineIDForCreate_PreservesImported(t *testing.T) {
	const imported = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	creds := map[string]any{
		CursorCredAccessToken: "jwt-token",
		CursorCredMachineID:   imported,
	}

	out := prepareCursorMachineIDForCreate(PlatformCursor, creds)

	if got, _ := out[CursorCredMachineID].(string); got != imported {
		t.Fatalf("导入的 machine_id 被覆盖: 期望 %q, 实际 %q", imported, got)
	}
}

// TestPrepareCursorMachineIDForCreate_IgnoresOtherPlatforms 确认这是纯旁挂钩子：
// 不能给别的平台凭证塞进一个它们不认识的字段。
func TestPrepareCursorMachineIDForCreate_IgnoresOtherPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformKiro, PlatformAnthropic, PlatformOpenAI} {
		creds := map[string]any{"access_token": "x"}
		out := prepareCursorMachineIDForCreate(platform, creds)
		if _, exists := out[CursorCredMachineID]; exists {
			t.Fatalf("平台 %s 的凭证不应被写入 machine_id", platform)
		}
	}
}

// TestEnsureMachineID_IsIdempotent 幂等性是本次改动的核心安全属性：
// 存量账号的指纹只允许变动一次（补齐），此后必须恒定。
func TestEnsureMachineID_IsIdempotent(t *testing.T) {
	creds := EnsureMachineID(map[string]any{CursorCredAccessToken: "jwt"})
	first, _ := creds[CursorCredMachineID].(string)
	if first == "" {
		t.Fatal("首次调用应铸造 machine_id")
	}

	for i := 0; i < 5; i++ {
		creds = EnsureMachineID(creds)
		if got, _ := creds[CursorCredMachineID].(string); got != first {
			t.Fatalf("第 %d 次调用改变了 machine_id: %q -> %q", i+2, first, got)
		}
	}
}

func TestEnsureMachineID_TreatsBlankAsMissing(t *testing.T) {
	for _, blank := range []any{"", "   ", nil} {
		creds := EnsureMachineID(map[string]any{CursorCredMachineID: blank})
		if got, _ := creds[CursorCredMachineID].(string); strings.TrimSpace(got) == "" {
			t.Fatalf("空白值 %#v 应被视为缺失并重新铸造", blank)
		}
	}
}

// TestBuildAccountCredentials_DoesNotMintOnRefresh 是本次改动最关键的反向护栏。
//
// BuildAccountCredentials 在**刷新**路径上也会被调用，而 MergeCredentials
// 是「新值覆盖旧值」。若它无条件铸造 machine_id，每次刷新都会换一个设备指纹，
// 比修复前更糟。
func TestBuildAccountCredentials_DoesNotMintOnRefresh(t *testing.T) {
	const existing = "1111111111111111111111111111111111111111111111111111111111111111"
	svc := &CursorOAuthService{}

	// 模拟刷新：上游返回的 info 不带 machine_id。
	built := svc.BuildAccountCredentials(CursorTokenInfo{AccessToken: "new-jwt"})
	if _, minted := built[CursorCredMachineID]; minted {
		t.Fatal("BuildAccountCredentials 不应铸造 machine_id —— 刷新路径会用它覆盖已落库的指纹")
	}

	// 走真实的合并顺序，确认既有指纹存活。
	merged := MergeCredentials(
		map[string]any{CursorCredMachineID: existing, CursorCredAccessToken: "old-jwt"},
		built,
	)
	if got, _ := merged[CursorCredMachineID].(string); got != existing {
		t.Fatalf("刷新后设备指纹发生漂移: 期望 %q, 实际 %q", existing, got)
	}
	if got, _ := merged[CursorCredAccessToken].(string); got != "new-jwt" {
		t.Fatalf("access_token 应被刷新值覆盖, 实际 %q", got)
	}
}

// TestCursorTokenRefresher_BackfillsButNeverRotates 覆盖刷新链路的两个要求：
// 存量账号补铸一次，已有指纹永不更换。
func TestCursorTokenRefresher_BackfillsButNeverRotates(t *testing.T) {
	t.Run("存量账号补铸", func(t *testing.T) {
		merged := EnsureMachineID(MergeCredentials(
			map[string]any{CursorCredAccessToken: "old"},
			map[string]any{CursorCredAccessToken: "new"},
		))
		if got, _ := merged[CursorCredMachineID].(string); strings.TrimSpace(got) == "" {
			t.Fatal("刷新时应给没有 machine_id 的存量账号补铸")
		}
	})

	t.Run("已有指纹不变", func(t *testing.T) {
		const existing = "2222222222222222222222222222222222222222222222222222222222222222"
		merged := EnsureMachineID(MergeCredentials(
			map[string]any{CursorCredMachineID: existing, CursorCredAccessToken: "old"},
			map[string]any{CursorCredAccessToken: "new"},
		))
		if got, _ := merged[CursorCredMachineID].(string); got != existing {
			t.Fatalf("刷新不应更换 machine_id: 期望 %q, 实际 %q", existing, got)
		}
	})
}

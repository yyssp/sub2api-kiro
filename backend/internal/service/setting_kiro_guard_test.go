//go:build unit

package service

import (
	"context"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func newSettingServiceForKiroGuardTest(t *testing.T, seed map[string]string) *SettingService {
	t.Helper()
	// 缓存是包级的，用例之间必须互相隔离，否则先跑的用例会把值漏给后跑的。
	kiroPayloadGuardSF.Forget("kiro_payload_guard")
	kiroPayloadGuardCache.Store((*cachedKiroPayloadGuardSettings)(nil))
	t.Cleanup(func() {
		kiroPayloadGuardSF.Forget("kiro_payload_guard")
		kiroPayloadGuardCache.Store((*cachedKiroPayloadGuardSettings)(nil))
	})

	repo := newMockSettingRepo()
	for k, v := range seed {
		repo.data[k] = v
	}
	return NewSettingService(repo, &config.Config{})
}

// 非法行为值必须回落到默认，**绝不能**变成 reject。
// 配置写错的后果应当是"退回默认策略"，而不是对全部 Kiro 请求拒服务。
func TestNormalizeKiroOversizeBehavior(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"reject", KiroOversizeBehaviorReject},
		{"  REJECT  ", KiroOversizeBehaviorReject},
		{"on_upstream_400", KiroOversizeBehaviorOnUpstream400},
		{"On_Upstream_400", KiroOversizeBehaviorOnUpstream400},
		{"compress_then_trim", KiroOversizeBehaviorCompressThenTrim},
		{"", KiroOversizeBehaviorCompressThenTrim},
		{"garbage", KiroOversizeBehaviorCompressThenTrim},
		{"trim", KiroOversizeBehaviorCompressThenTrim},
	} {
		require.Equal(t, tc.want, normalizeKiroOversizeBehavior(tc.in), "输入 %q", tc.in)
	}
}

// 阈值夹取：0 表示"未提供"（调用方据此跳过写入），其余夹到合法区间。
func TestClampKiroOversizeThreshold(t *testing.T) {
	require.Zero(t, clampKiroOversizeThreshold(0), "0 = 未提供，不能被夹成下界")
	require.Zero(t, clampKiroOversizeThreshold(-1))
	require.Equal(t, kiroOversizeThresholdMin, clampKiroOversizeThreshold(1))
	require.Equal(t, kiroOversizeThresholdMax, clampKiroOversizeThreshold(99_999_999),
		"过大的值等于关掉守卫，必须夹住")
	require.Equal(t, 800_000, clampKiroOversizeThreshold(800_000))
}

func TestParseKiroOversizeThreshold(t *testing.T) {
	require.Equal(t, kiroOversizeThresholdFallback, parseKiroOversizeThreshold(""))
	require.Equal(t, kiroOversizeThresholdFallback, parseKiroOversizeThreshold("not-a-number"))
	require.Equal(t, kiroOversizeThresholdFallback, parseKiroOversizeThreshold("0"),
		"0 在 DB 里没有意义，应回落到默认而不是变成无限制")
	require.Equal(t, 800_000, parseKiroOversizeThreshold(" 800000 "))
	require.Equal(t, kiroOversizeThresholdMax, parseKiroOversizeThreshold("99999999"))
}

// 默认阈值必须与 kiro 包同源，避免两处各写一个数字而漂移。
func TestKiroThresholdFallbackMatchesPackageDefault(t *testing.T) {
	require.Equal(t, kiropkg.DefaultMaxPayloadWeight, kiroOversizeThresholdFallback)
}

// 空 DB（全新部署）必须拿到默认策略，而不是零值。
// 零阈值会让守卫把每个请求都判成超限。
func TestGetKiroPayloadGuardSettingsDefaultsOnEmptyDB(t *testing.T) {
	svc := newSettingServiceForKiroGuardTest(t, nil)

	behavior, threshold := svc.GetKiroPayloadGuardSettings(context.Background())
	require.Equal(t, KiroOversizeBehaviorCompressThenTrim, behavior)
	require.Equal(t, kiropkg.DefaultMaxPayloadWeight, threshold)
}

func TestGetKiroPayloadGuardSettingsReadsStoredValues(t *testing.T) {
	svc := newSettingServiceForKiroGuardTest(t, map[string]string{
		SettingKeyKiroOversizeBehavior:  KiroOversizeBehaviorReject,
		SettingKeyKiroOversizeThreshold: "900000",
	})

	behavior, threshold := svc.GetKiroPayloadGuardSettings(context.Background())
	require.Equal(t, KiroOversizeBehaviorReject, behavior)
	require.Equal(t, 900_000, threshold)
}

// DB 里存了脏数据时也必须给出可用配置，不能把网关打挂。
func TestGetKiroPayloadGuardSettingsToleratesCorruptValues(t *testing.T) {
	svc := newSettingServiceForKiroGuardTest(t, map[string]string{
		SettingKeyKiroOversizeBehavior:  "??",
		SettingKeyKiroOversizeThreshold: "abc",
	})

	behavior, threshold := svc.GetKiroPayloadGuardSettings(context.Background())
	require.Equal(t, KiroOversizeBehaviorCompressThenTrim, behavior)
	require.Equal(t, kiropkg.DefaultMaxPayloadWeight, threshold)
}

// 页面上改完必须立刻生效，不能等最长 60s 的缓存 TTL。
func TestRefreshKiroPayloadGuardCacheTakesEffectImmediately(t *testing.T) {
	svc := newSettingServiceForKiroGuardTest(t, nil)

	_, threshold := svc.GetKiroPayloadGuardSettings(context.Background())
	require.Equal(t, kiropkg.DefaultMaxPayloadWeight, threshold, "先把默认值灌进缓存")

	refreshKiroPayloadGuardCache(KiroOversizeBehaviorOnUpstream400, 700_000)

	behavior, threshold := svc.GetKiroPayloadGuardSettings(context.Background())
	require.Equal(t, KiroOversizeBehaviorOnUpstream400, behavior)
	require.Equal(t, 700_000, threshold)
}

// 刷新时传 0（"未提供"）不得把阈值写成 0 —— 那会让守卫把所有请求判成超限。
func TestRefreshKiroPayloadGuardCacheRejectsZeroThreshold(t *testing.T) {
	svc := newSettingServiceForKiroGuardTest(t, nil)

	refreshKiroPayloadGuardCache(KiroOversizeBehaviorReject, 0)

	_, threshold := svc.GetKiroPayloadGuardSettings(context.Background())
	require.Equal(t, kiropkg.DefaultMaxPayloadWeight, threshold,
		"0 必须回落到默认，否则每个请求都会被判超限")
}

// 设置层字符串 -> kiro 包行为类型的映射必须完备。
func TestToKiroGuardBehavior(t *testing.T) {
	require.Equal(t, kiropkg.KiroOversizeReject, toKiroGuardBehavior(KiroOversizeBehaviorReject))
	require.Equal(t, kiropkg.KiroOversizeOnUpstream400, toKiroGuardBehavior(KiroOversizeBehaviorOnUpstream400))
	require.Equal(t, kiropkg.KiroOversizeCompressThenTrim, toKiroGuardBehavior(KiroOversizeBehaviorCompressThenTrim))
	require.Equal(t, kiropkg.KiroOversizeCompressThenTrim, toKiroGuardBehavior("nonsense"))
}

// parseSettings：空 DB 给默认值，有值时按值来。
func TestParseSettingsKiroGuard(t *testing.T) {
	svc := newSettingServiceForKiroGuardTest(t, nil)

	got := svc.parseSettings(map[string]string{})
	require.Equal(t, KiroOversizeBehaviorCompressThenTrim, got.KiroOversizeBehavior)
	require.Equal(t, kiropkg.DefaultMaxPayloadWeight, got.KiroOversizeThreshold)

	got = svc.parseSettings(map[string]string{
		SettingKeyKiroOversizeBehavior:  KiroOversizeBehaviorOnUpstream400,
		SettingKeyKiroOversizeThreshold: "650000",
	})
	require.Equal(t, KiroOversizeBehaviorOnUpstream400, got.KiroOversizeBehavior)
	require.Equal(t, 650_000, got.KiroOversizeThreshold)
}

// buildSystemSettingsUpdates：写回 DB 时必须归一化 + 夹取。
func TestBuildSystemSettingsUpdatesKiroGuard(t *testing.T) {
	svc := newSettingServiceForKiroGuardTest(t, nil)

	updates, err := svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
		KiroOversizeBehavior:  "REJECT",
		KiroOversizeThreshold: 750_000,
	})
	require.NoError(t, err)
	require.Equal(t, KiroOversizeBehaviorReject, updates[SettingKeyKiroOversizeBehavior])
	require.Equal(t, strconv.Itoa(750_000), updates[SettingKeyKiroOversizeThreshold])

	// 阈值为 0（前端没填）时不得写入，避免用零值覆盖已有配置。
	updates, err = svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
		KiroOversizeBehavior:  KiroOversizeBehaviorCompressThenTrim,
		KiroOversizeThreshold: 0,
	})
	require.NoError(t, err)
	require.NotContains(t, updates, SettingKeyKiroOversizeThreshold,
		"0 表示未提供，不能覆盖已存的阈值")

	// 超大值必须被夹住，而不是原样落库把守卫变成摆设。
	updates, err = svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
		KiroOversizeBehavior:  KiroOversizeBehaviorCompressThenTrim,
		KiroOversizeThreshold: 99_999_999,
	})
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(kiroOversizeThresholdMax), updates[SettingKeyKiroOversizeThreshold])
}

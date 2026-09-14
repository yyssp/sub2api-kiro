package service

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"golang.org/x/sync/singleflight"
)

// Kiro 体积守卫阈值的合法区间(加权口径, 见 SettingKeyKiroOversizeThreshold 注释)。
//
// 下界 10,000: 再低会把正常的短对话也裁掉, 属于配置事故而非策略选择。
// 上界 5,000,000: 实测上游在 ~1.36M 就 400, 留出数倍余量给未来可能放宽的档位,
// 但仍拦住"填个 99999999 等于关掉守卫"这种误配。
const (
	kiroOversizeThresholdMin      = 10_000
	kiroOversizeThresholdMax      = 5_000_000
	kiroOversizeThresholdFallback = kiropkg.DefaultMaxPayloadWeight
)

type cachedKiroPayloadGuardSettings struct {
	behavior  string
	threshold int
	expiresAt int64
}

var (
	kiroPayloadGuardCache atomic.Value // *cachedKiroPayloadGuardSettings
	kiroPayloadGuardSF    singleflight.Group
)

const (
	kiroPayloadGuardCacheTTL = 60 * time.Second
	// 读 DB 失败时用短 TTL, 让下一次请求很快重试, 而不是把兜底值钉死一分钟。
	kiroPayloadGuardErrorTTL  = 5 * time.Second
	kiroPayloadGuardDBTimeout = 3 * time.Second
)

// normalizeKiroOversizeBehavior 把任意输入收敛到三个合法值之一。
//
// 非法值一律回落到 compress_then_trim 而不是报错: 配置写错的后果应当是
// "退回默认策略", 绝不能变成 reject 而对全部 Kiro 请求拒服务。
func normalizeKiroOversizeBehavior(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case KiroOversizeBehaviorReject:
		return KiroOversizeBehaviorReject
	case KiroOversizeBehaviorOnUpstream400:
		return KiroOversizeBehaviorOnUpstream400
	default:
		return KiroOversizeBehaviorCompressThenTrim
	}
}

// clampKiroOversizeThreshold 把阈值夹到合法区间。
// 返回 0 表示"未提供", 调用方据此跳过写入以免用零值覆盖已有配置。
func clampKiroOversizeThreshold(v int) int {
	if v <= 0 {
		return 0
	}
	if v < kiroOversizeThresholdMin {
		return kiroOversizeThresholdMin
	}
	if v > kiroOversizeThresholdMax {
		return kiroOversizeThresholdMax
	}
	return v
}

// parseKiroOversizeThreshold 解析 DB 中的字符串值, 非法时回落到默认阈值。
func parseKiroOversizeThreshold(raw string) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return kiroOversizeThresholdFallback
	}
	if clamped := clampKiroOversizeThreshold(v); clamped > 0 {
		return clamped
	}
	return kiroOversizeThresholdFallback
}

// GetKiroPayloadGuardSettings 返回体积守卫配置, 带进程内缓存。
//
// 每个 Kiro 请求都要读, 禁止在热路径上直接打 DB。
func (s *SettingService) GetKiroPayloadGuardSettings(ctx context.Context) (string, int) {
	if cached, ok := kiroPayloadGuardCache.Load().(*cachedKiroPayloadGuardSettings); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.behavior, cached.threshold
		}
	}
	val, _, _ := kiroPayloadGuardSF.Do("kiro_payload_guard", func() (any, error) {
		if cached, ok := kiroPayloadGuardCache.Load().(*cachedKiroPayloadGuardSettings); ok && cached != nil {
			if time.Now().UnixNano() < cached.expiresAt {
				return *cached, nil
			}
		}
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kiroPayloadGuardDBTimeout)
		defer cancel()
		values, err := s.settingRepo.GetMultiple(dbCtx, []string{
			SettingKeyKiroOversizeBehavior,
			SettingKeyKiroOversizeThreshold,
		})
		if err != nil {
			slog.Warn("failed to get kiro payload guard settings", "error", err)
			fallback := cachedKiroPayloadGuardSettings{
				behavior:  KiroOversizeBehaviorCompressThenTrim,
				threshold: kiroOversizeThresholdFallback,
				expiresAt: time.Now().Add(kiroPayloadGuardErrorTTL).UnixNano(),
			}
			kiroPayloadGuardCache.Store(&fallback)
			return fallback, nil
		}
		entry := cachedKiroPayloadGuardSettings{
			behavior:  normalizeKiroOversizeBehavior(values[SettingKeyKiroOversizeBehavior]),
			threshold: parseKiroOversizeThreshold(values[SettingKeyKiroOversizeThreshold]),
			expiresAt: time.Now().Add(kiroPayloadGuardCacheTTL).UnixNano(),
		}
		kiroPayloadGuardCache.Store(&entry)
		return entry, nil
	})
	if entry, ok := val.(cachedKiroPayloadGuardSettings); ok {
		return entry.behavior, entry.threshold
	}
	return KiroOversizeBehaviorCompressThenTrim, kiroOversizeThresholdFallback
}

// refreshKiroPayloadGuardCache 在设置写入后立即刷新缓存, 避免最长 60s 的生效延迟。
func refreshKiroPayloadGuardCache(behavior string, threshold int) {
	kiroPayloadGuardSF.Forget("kiro_payload_guard")
	if threshold <= 0 {
		threshold = kiroOversizeThresholdFallback
	}
	kiroPayloadGuardCache.Store(&cachedKiroPayloadGuardSettings{
		behavior:  normalizeKiroOversizeBehavior(behavior),
		threshold: threshold,
		expiresAt: time.Now().Add(kiroPayloadGuardCacheTTL).UnixNano(),
	})
}

// toKiroGuardBehavior 把设置层的字符串映射为 kiro 包的行为类型。
func toKiroGuardBehavior(behavior string) kiropkg.KiroOversizeBehavior {
	switch normalizeKiroOversizeBehavior(behavior) {
	case KiroOversizeBehaviorReject:
		return kiropkg.KiroOversizeReject
	case KiroOversizeBehaviorOnUpstream400:
		return kiropkg.KiroOversizeOnUpstream400
	default:
		return kiropkg.KiroOversizeCompressThenTrim
	}
}

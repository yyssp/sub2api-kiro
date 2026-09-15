package service

import "testing"

func boolPtrTTL(v bool) *bool { return &v }

// 优先级链：强制档位 > 上游返回 > 客户端声明 > 5m（协议缺省）。
func TestResolveReportedTTLTierPriorityChain(t *testing.T) {
	cases := []struct {
		name       string
		policy     CacheStrategyConfig
		clientTier string
		upstream5m int
		upstream1h int
		want       string
	}{
		// 第 1 级：强制档位压过其余所有来源。
		{
			name:       "forced 5m overrides upstream 1h and client 1h",
			policy:     CacheStrategyConfig{ForcedTTLTier: CacheTTLTier5m},
			clientTier: CacheTTLTier1h,
			upstream1h: 5000,
			want:       CacheTTLTier5m,
		},
		{
			name:       "forced 1h overrides upstream 5m and client 5m",
			policy:     CacheStrategyConfig{ForcedTTLTier: CacheTTLTier1h},
			clientTier: CacheTTLTier5m,
			upstream5m: 5000,
			want:       CacheTTLTier1h,
		},
		// 第 2 级：上游不守契约时以上游为准，保护平台成本。
		{
			name:       "upstream 1h beats client 5m (third-party ignored the contract)",
			clientTier: CacheTTLTier5m,
			upstream1h: 3000,
			want:       CacheTTLTier1h,
		},
		{
			name:       "upstream 5m beats client 1h",
			clientTier: CacheTTLTier1h,
			upstream5m: 3000,
			want:       CacheTTLTier5m,
		},
		{
			name:       "mixed upstream buckets take the larger side",
			clientTier: CacheTTLTier5m,
			upstream5m: 100,
			upstream1h: 900,
			want:       CacheTTLTier1h,
		},
		// 关掉采信上游后，第 2 级被跳过。
		{
			name:       "trust disabled falls back to client tier",
			policy:     CacheStrategyConfig{TrustUpstreamTTLTier: boolPtrTTL(false)},
			clientTier: CacheTTLTier5m,
			upstream1h: 9000,
			want:       CacheTTLTier5m,
		},
		// 第 3 级：上游未表态时听客户端的。
		{
			name:       "client 1h wins when upstream is silent",
			clientTier: CacheTTLTier1h,
			want:       CacheTTLTier1h,
		},
		// 第 4 级：全部未表态 ⇒ 协议缺省 5m。
		{
			name:       "nobody declared anything falls back to protocol default 5m",
			clientTier: CacheTTLTierUnset,
			want:       CacheTTLTier5m,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveReportedTTLTier(tc.policy, tc.clientTier, tc.upstream5m, tc.upstream1h)
			if got != tc.want {
				t.Fatalf("resolveReportedTTLTier = %q, want %q", got, tc.want)
			}
		})
	}
}

// 存量策略的 JSONB 记录里没有 trust_upstream_ttl_tier 这个 key，反序列化得到 nil。
// 它必须等价于 true，否则升级后所有存量分组会静默停止跟随上游，平台开始吃差价。
func TestTrustUpstreamTTLTierDefaultsToTrueWhenUnset(t *testing.T) {
	if !trustUpstreamTTLTier(CacheStrategyConfig{}) {
		t.Fatal("nil TrustUpstreamTTLTier must be treated as enabled")
	}
	if trustUpstreamTTLTier(CacheStrategyConfig{TrustUpstreamTTLTier: boolPtrTTL(false)}) {
		t.Fatal("explicit false must disable upstream trust")
	}
}

// 上游两个桶都为 0 表示「没表态」，不能被当成 5m —— 那会吃掉客户端的 1h 声明。
func TestUpstreamTTLTierTreatsZeroBucketsAsUnset(t *testing.T) {
	if got := upstreamTTLTier(0, 0); got != CacheTTLTierUnset {
		t.Fatalf("upstreamTTLTier(0,0) = %q, want unset", got)
	}
}

// 计费硬约束：ephemeral_5m + ephemeral_1h 必须恒等于 cache_creation_input_tokens。
// 任何一侧对不上，下游都会算错钱（常见表现是按 0 计费）。
func TestApplyTTLTierKeepsBucketsConsistentWithTotal(t *testing.T) {
	for _, tier := range []string{CacheTTLTier5m, CacheTTLTier1h} {
		usage := &ClaudeUsage{
			CacheCreationInputTokens: 12345,
			// 故意塞入与目标档位相反的残留值，验证另一侧确实被清零。
			CacheCreation5mTokens: 777,
			CacheCreation1hTokens: 888,
		}
		applyTTLTierToClaudeUsage(usage, tier)

		if sum := usage.CacheCreation5mTokens + usage.CacheCreation1hTokens; sum != usage.CacheCreationInputTokens {
			t.Fatalf("tier %s: buckets sum to %d, want %d", tier, sum, usage.CacheCreationInputTokens)
		}
		if tier == CacheTTLTier1h && usage.CacheCreation1hTokens != 12345 {
			t.Fatalf("tier 1h: got %d in the 1h bucket", usage.CacheCreation1hTokens)
		}
		if tier == CacheTTLTier5m && usage.CacheCreation5mTokens != 12345 {
			t.Fatalf("tier 5m: got %d in the 5m bucket", usage.CacheCreation5mTokens)
		}
	}
}

// 没有新建缓存时两个桶都必须归零，不能残留上一轮投影的数字。
func TestApplyTTLTierZeroesBucketsWithoutCreation(t *testing.T) {
	usage := &ClaudeUsage{
		CacheCreationInputTokens: 0,
		CacheCreation5mTokens:    500,
		CacheCreation1hTokens:    600,
	}
	applyTTLTierToClaudeUsage(usage, CacheTTLTier1h)

	if usage.CacheCreation5mTokens != 0 || usage.CacheCreation1hTokens != 0 {
		t.Fatalf("expected both buckets zeroed, got 5m=%d 1h=%d",
			usage.CacheCreation5mTokens, usage.CacheCreation1hTokens)
	}
}

func TestNormalizeCacheStrategyConfigValidatesForcedTTLTier(t *testing.T) {
	base := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)

	for _, valid := range []string{CacheTTLTierUnset, CacheTTLTier5m, CacheTTLTier1h} {
		cfg := base
		cfg.ForcedTTLTier = valid
		if _, err := NormalizeCacheStrategyConfig(cfg); err != nil {
			t.Fatalf("forced_ttl_tier %q should be accepted: %v", valid, err)
		}
	}

	cfg := base
	cfg.ForcedTTLTier = "30m"
	if _, err := NormalizeCacheStrategyConfig(cfg); err == nil {
		t.Fatal("forced_ttl_tier=30m must be rejected")
	}

	// 大小写与空格要被归一，否则 "1H" 会静默退化成「不强制」。
	cfg = base
	cfg.ForcedTTLTier = " 1H "
	normalized, err := NormalizeCacheStrategyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if normalized.ForcedTTLTier != CacheTTLTier1h {
		t.Fatalf("expected normalized to %q, got %q", CacheTTLTier1h, normalized.ForcedTTLTier)
	}
}

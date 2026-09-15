package routes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// dispatchTTLTierCase 走一次真实调度，返回上报给客户端的三个缓存数值。
func dispatchTTLTierCase(
	t *testing.T,
	seed int64,
	mutate func(cfg *service.CacheStrategyConfig),
	upstreamCreation int,
	upstreamTier string,
	clientDeclares1h bool,
) (creation, five, hour int) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := kiroRsToolTemplateConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	normalized, err := service.NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)

	strategyID := seed
	newRouteCacheStrategyWithConfig(strategyID, fmt.Sprintf("ttl-tier-%d", seed), normalized)
	t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

	state := &routeCacheUpstream{
		inputBase:             40000,
		outputBase:            900,
		upstreamCacheCreation: upstreamCreation,
		upstreamCacheTier:     upstreamTier,
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(state.handler))
	t.Cleanup(upstream.Close)

	groupID := strategyID + 10
	group := &service.Group{
		ID:               groupID,
		Platform:         service.PlatformAnthropic,
		Status:           service.StatusActive,
		Hydrated:         true,
		RateMultiplier:   1,
		SubscriptionType: service.SubscriptionTypeStandard,
		CacheStrategyID:  &strategyID,
	}
	groupRepo := &routeCacheGroupRepo{groups: map[int64]*service.Group{groupID: group}}
	account := service.Account{
		ID:            strategyID + 20,
		Name:          "ttl-tier-mock",
		Platform:      service.PlatformAnthropic,
		Type:          service.AccountTypeAPIKey,
		Status:        service.StatusActive,
		Schedulable:   true,
		Concurrency:   4,
		AccountGroups: []service.AccountGroup{{GroupID: groupID}},
		Credentials:   map[string]any{"api_key": "mock", "base_url": upstream.URL},
		Extra:         map[string]any{"anthropic_passthrough": true},
	}
	accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{account.ID: account}}
	gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
	userID := strategyID + 30
	apiKey := &service.APIKey{
		ID:      strategyID + 40,
		UserID:  userID,
		Key:     "route-cache-test-key",
		Status:  service.StatusActive,
		User:    &service.User{ID: userID, Role: service.RoleUser, Status: service.StatusActive, Balance: 1000000, Concurrency: 8},
		GroupID: &groupID,
		Group:   group,
	}
	router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, &routeCacheHTTPUpstream{client: upstream.Client()})

	// 客户端声明档位靠 system 块上的 cache_control.ttl 表达。
	ttlField := ""
	if clientDeclares1h {
		ttlField = `,"ttl":"1h"`
	}
	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","metadata":{"user_id":"ttl-tier-session"},`+
		`"system":[{"type":"text","text":"%s","cache_control":{"type":"ephemeral"%s}}],`+
		`"messages":[{"role":"user","content":"hi"}],"max_tokens":128,"stream":false}`,
		strings.Repeat("You are Claude Code, an agentic CLI tool. Follow repository conventions. ", 1200),
		ttlField)

	resp := routeCacheRequest(t, router, body)
	usage, ok := resp["usage"].(map[string]any)
	require.True(t, ok)

	creation = kiroRsUsageInt(usage, "cache_creation_input_tokens")
	if cc, ok := usage["cache_creation"].(map[string]any); ok {
		five = kiroRsUsageInt(cc, "ephemeral_5m_input_tokens")
		hour = kiroRsUsageInt(cc, "ephemeral_1h_input_tokens")
	}
	return creation, five, hour
}

// 第 2 级：上游不守契约（客户端要 5m、上游按 1h 返回）时，上报必须跟上游走，
// 否则平台按 5m 收费、按 1h 付费，差价即为亏损。
func TestDispatchReportsUpstreamTierWhenClientDisagrees(t *testing.T) {
	creation, five, hour := dispatchTTLTierCase(t, 99310, nil, 5000, "1h", false)

	require.Positive(t, creation, "本用例需要产生缓存创建才有意义")
	require.Equal(t, creation, five+hour, "两个桶之和必须等于 cache_creation_input_tokens")
	require.Zero(t, five, "上游按 1h 计费，不能上报到 5m 桶")
	require.Equal(t, creation, hour)
}

// 第 1 级：强制档位压过上游。
func TestDispatchForcedTierOverridesUpstream(t *testing.T) {
	creation, five, hour := dispatchTTLTierCase(t, 99320, func(cfg *service.CacheStrategyConfig) {
		cfg.ForcedTTLTier = service.CacheTTLTier5m
	}, 5000, "1h", true)

	require.Positive(t, creation)
	require.Equal(t, creation, five+hour)
	require.Equal(t, creation, five, "强制 5m 必须压过上游的 1h 与客户端的 1h")
	require.Zero(t, hour)
}

// 关掉「采信上游」后退回客户端声明，平台自行承担差价。
func TestDispatchFallsBackToClientTierWhenUpstreamTrustDisabled(t *testing.T) {
	trust := false
	creation, five, hour := dispatchTTLTierCase(t, 99330, func(cfg *service.CacheStrategyConfig) {
		cfg.TrustUpstreamTTLTier = &trust
	}, 5000, "1h", false)

	require.Positive(t, creation)
	require.Equal(t, creation, five+hour)
	require.Equal(t, creation, five, "关掉采信上游后应回到客户端声明的 5m")
	require.Zero(t, hour)
}

// 第 3 级：上游没表态时听客户端的。
func TestDispatchHonoursClientTierWhenUpstreamSilent(t *testing.T) {
	creation, five, hour := dispatchTTLTierCase(t, 99340, nil, 0, "", true)

	require.Positive(t, creation)
	require.Equal(t, creation, five+hour)
	require.Equal(t, creation, hour, "上游未表态时应采用客户端声明的 1h")
	require.Zero(t, five)
}

// 第 4 级：三级都没表态 ⇒ 协议缺省 5m，且两个桶仍与总额自洽。
func TestDispatchDefaultsToFiveMinutesWhenNobodyDeclares(t *testing.T) {
	creation, five, hour := dispatchTTLTierCase(t, 99350, nil, 0, "", false)

	require.Positive(t, creation)
	require.Equal(t, creation, five+hour)
	require.Equal(t, creation, five)
	require.Zero(t, hour)
}

package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type cacheStrategyHTTPMockUpstream struct {
	mu       sync.Mutex
	calls    int
	bodies   [][]byte
	response string
}

func (m *cacheStrategyHTTPMockUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	m.mu.Lock()
	m.calls++
	m.bodies = append(m.bodies, append([]byte(nil), body...))
	callIndex := m.calls
	response := m.response
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("x-request-id", fmt.Sprintf("mock-cache-%d", callIndex))
	_, _ = io.WriteString(w, response)
}

func newCacheStrategyHTTPServer(t *testing.T, response string) (*httptest.Server, *cacheStrategyHTTPMockUpstream) {
	t.Helper()
	mock := &cacheStrategyHTTPMockUpstream{response: response}
	server := httptest.NewTLSServer(http.HandlerFunc(mock.handler))
	t.Cleanup(server.Close)
	return server, mock
}

func newCacheStrategyHTTPService(t *testing.T, servers ...*httptest.Server) *GatewayService {
	t.Helper()
	roots := x509.NewCertPool()
	for _, server := range servers {
		if server != nil {
			roots.AddCert(server.Certificate())
		}
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.MaxLineSize = defaultMaxLineSize
	cfg.Security.URLAllowlist.Enabled = true
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	cfg.Security.URLAllowlist.UpstreamHosts = []string{"127.0.0.1"}
	return &GatewayService{
		httpUpstream:        &cacheProtocolHTTPUpstream{client: client},
		tlsFPProfileService: &TLSFingerprintProfileService{},
		cfg:                 cfg,
	}
}

func newCacheStrategyAccount(id int64, name, baseURL string) *Account {
	return &Account{
		ID:       id,
		Name:     name,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  fmt.Sprintf("mock-key-%d", id),
			"base_url": baseURL,
		},
	}
}

func newCacheStrategyGroup(id, strategyID int64) *Group {
	return &Group{
		ID:              id,
		Platform:        PlatformAnthropic,
		CacheStrategyID: &strategyID,
	}
}

func registerCacheStrategyProfile(t *testing.T, id int64, name string, cfg CacheStrategyConfig) {
	t.Helper()
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       id,
		Name:     name,
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	t.Cleanup(func() {
		GlobalCacheStrategyRegistry().Delete(id)
	})
}

func anthropicCacheStreamResponse(inputTokens, readTokens, creationTokens, outputTokens int) string {
	return strings.Join([]string{
		"event: message_start",
		fmt.Sprintf(`data: {"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":0}}}`, inputTokens, readTokens, creationTokens),
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"",
		"event: message_delta",
		fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, outputTokens),
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
}

func anthropicPromptTokensStreamResponse(promptTokens, cachedTokens, cacheCreationTokens, outputTokens int) string {
	return strings.Join([]string{
		"event: message_start",
		fmt.Sprintf(`data: {"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"prompt_tokens":%d,"cached_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d},"cache_creation_input_tokens":%d,"output_tokens":0}}}`, promptTokens, cachedTokens, cachedTokens, cacheCreationTokens),
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"",
		"event: message_delta",
		fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, outputTokens),
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
}

func TestGroupCacheStrategyHTTPMultiAccountAndRevisionIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetCacheTracker()

	strategyID := int64(99101)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.DefaultTTLSeconds = 300
	cfg.HourTTLSeconds = 3600
	// 默认作用域已改成「分组 + 会话」（换账号仍命中）。这条测试验证的是
	// 按账号隔离，属于可选行为，必须显式声明。
	cfg.ScopeMode = CacheScopeModeGroupAccountSession
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "http integration",
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	t.Cleanup(func() {
		GlobalCacheStrategyRegistry().Delete(strategyID)
	})

	responseBody := anthropicCacheStreamResponse(10, 0, 0, 5)
	serverA, mockA := newCacheStrategyHTTPServer(t, responseBody)
	serverB, mockB := newCacheStrategyHTTPServer(t, responseBody)
	service := newCacheStrategyHTTPService(t, serverA, serverB)

	accountA := newCacheStrategyAccount(99111, "account-a", serverA.URL)
	accountB := newCacheStrategyAccount(99112, "account-b", serverB.URL)
	groupA := newCacheStrategyGroup(99121, strategyID)
	groupB := newCacheStrategyGroup(99122, strategyID)

	body := []byte(`{"model":"claude-sonnet-4-6","prompt_cache_key":"session-http-e2e","input":"stable project history that should be cached","stream":false}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), "responses")
	require.NoError(t, err)
	parsed.Group = groupA

	firstCtx, firstRec := cacheResponsesContext(body)
	first, err := service.ForwardAsResponses(context.Background(), firstCtx, accountA, body, parsed)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Zero(t, first.Usage.CacheReadInputTokens)
	require.Greater(t, first.Usage.CacheCreationInputTokens, 0)
	require.Equal(t, first.Usage.CacheCreationInputTokens, first.Usage.CacheCreation5mTokens)
	require.Zero(t, first.Usage.CacheCreation1hTokens)
	require.Equal(t, int64(1), func() int64 { mockA.mu.Lock(); defer mockA.mu.Unlock(); return int64(mockA.calls) }())
	require.Zero(t, func() int64 { mockB.mu.Lock(); defer mockB.mu.Unlock(); return int64(mockB.calls) }())
	require.Equal(t, int64(first.Usage.InputTokens+first.Usage.CacheReadInputTokens+first.Usage.CacheCreationInputTokens), gjson.Get(firstRec.Body.String(), "usage.input_tokens").Int())

	secondCtx, secondRec := cacheResponsesContext(body)
	second, err := service.ForwardAsResponses(context.Background(), secondCtx, accountA, body, parsed)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Greater(t, second.Usage.CacheReadInputTokens, 0)
	require.Zero(t, second.Usage.CacheCreationInputTokens)
	require.Equal(t, second.Usage.CacheReadInputTokens, int(gjson.Get(secondRec.Body.String(), "usage.input_tokens_details.cached_tokens").Int()))

	otherAccountCtx, otherAccountRec := cacheResponsesContext(body)
	otherAccount, err := service.ForwardAsResponses(context.Background(), otherAccountCtx, accountB, body, parsed)
	require.NoError(t, err)
	require.NotNil(t, otherAccount)
	require.Zero(t, otherAccount.Usage.CacheReadInputTokens)
	require.Greater(t, otherAccount.Usage.CacheCreationInputTokens, 0)
	require.Equal(t, otherAccount.Usage.CacheCreationInputTokens, otherAccount.Usage.CacheCreation5mTokens)
	require.Zero(t, otherAccount.Usage.CacheCreation1hTokens)
	require.Equal(t, int64(1), func() int64 { mockB.mu.Lock(); defer mockB.mu.Unlock(); return int64(mockB.calls) }())
	require.Equal(t, int64(2), func() int64 { mockA.mu.Lock(); defer mockA.mu.Unlock(); return int64(mockA.calls) }())
	require.Equal(t, int64(otherAccount.Usage.InputTokens+otherAccount.Usage.CacheReadInputTokens+otherAccount.Usage.CacheCreationInputTokens), gjson.Get(otherAccountRec.Body.String(), "usage.input_tokens").Int())

	parsedOtherGroup, err := ParseGatewayRequest(NewRequestBodyRef(body), "responses")
	require.NoError(t, err)
	parsedOtherGroup.Group = groupB
	otherGroupCtx, _ := cacheResponsesContext(body)
	otherGroup, err := service.ForwardAsResponses(context.Background(), otherGroupCtx, accountA, body, parsedOtherGroup)
	require.NoError(t, err)
	require.NotNil(t, otherGroup)
	require.Zero(t, otherGroup.Usage.CacheReadInputTokens)
	require.Greater(t, otherGroup.Usage.CacheCreationInputTokens, 0)

	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "http integration",
		Enabled:  true,
		Revision: 2,
		Config:   cfg,
	})

	parsedRevision, err := ParseGatewayRequest(NewRequestBodyRef(body), "responses")
	require.NoError(t, err)
	parsedRevision.Group = groupA
	revisionCtx, revisionRec := cacheResponsesContext(body)
	revisionResult, err := service.ForwardAsResponses(context.Background(), revisionCtx, accountA, body, parsedRevision)
	require.NoError(t, err)
	require.NotNil(t, revisionResult)
	require.Zero(t, revisionResult.Usage.CacheReadInputTokens)
	require.Greater(t, revisionResult.Usage.CacheCreationInputTokens, 0)
	require.Equal(t, revisionResult.Usage.CacheReadInputTokens, int(gjson.Get(revisionRec.Body.String(), "usage.input_tokens_details.cached_tokens").Int()))
}

func TestGroupCacheStrategyHTTPUsagePolicyShapesUpstreamCacheBuckets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetCacheTracker()

	strategyID := int64(99151)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.Usage = DefaultCacheUsagePolicy()
	cfg.Usage.Input = CacheUsageFieldPolicy{Mode: CacheUsageFieldRaw}
	cfg.Usage.Output = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleMax,
		MaxTokens:           5,
		NormalMaxMultiplier: 1.1,
	}
	cfg.Usage.CacheRead = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleMax,
		MaxTokens:           5,
		NormalMaxMultiplier: 1.1,
	}
	cfg.Usage.CacheCreation = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleMax,
		MaxTokens:           2,
		NormalMaxMultiplier: 1.1,
	}
	cfg.PreserveUpstreamCacheUsage = true

	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "usage projection",
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	t.Cleanup(func() {
		GlobalCacheStrategyRegistry().Delete(strategyID)
	})

	server, _ := newCacheStrategyHTTPServer(t, anthropicPromptTokensStreamResponse(20, 11, 4, 9))
	service := newCacheStrategyHTTPService(t, server)
	account := newCacheStrategyAccount(99152, "account-usage", server.URL)
	group := newCacheStrategyGroup(99153, strategyID)

	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"usage-policy-session"},"messages":[{"role":"user","content":"stable cache policy test"}],"stream":false}`)
	recCtx, rec := cacheChatContext(body)
	SetCacheGroupContext(recCtx, group)
	parsed := &ParsedRequest{Group: group}

	result, err := service.ForwardAsChatCompletions(context.Background(), recCtx, account, body, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Greater(t, result.Usage.InputTokens, 0)
	require.LessOrEqual(t, result.Usage.InputTokens, 20)
	require.Greater(t, result.Usage.CacheReadInputTokens, 0)
	require.LessOrEqual(t, result.Usage.CacheReadInputTokens, 5)
	require.Greater(t, result.Usage.CacheCreationInputTokens, 0)
	require.LessOrEqual(t, result.Usage.CacheCreationInputTokens, 2)
	require.Greater(t, result.Usage.OutputTokens, 0)
	require.LessOrEqual(t, result.Usage.OutputTokens, 5)
	require.Equal(t, result.Usage.CacheReadInputTokens, int(gjson.Get(rec.Body.String(), "usage.prompt_tokens_details.cached_tokens").Int()))
	require.Contains(t, rec.Body.String(), fmt.Sprintf(`"cache_creation_tokens":%d`, result.Usage.CacheCreationInputTokens))
	require.Equal(t,
		result.Usage.InputTokens+result.Usage.CacheReadInputTokens+result.Usage.CacheCreationInputTokens,
		int(gjson.Get(rec.Body.String(), "usage.prompt_tokens").Int()),
	)
}

func TestGroupCacheStrategyHTTPMultipleStrategyProfiles(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("prefix high cache policy", func(t *testing.T) {
		resetCacheTracker()
		strategyID := int64(99201)
		cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
		cfg.MinCacheableTokens = 1
		cfg.CoverageRatio = 0.72
		cfg.UsageRatio = 0.95
		cfg.ReadRatio = 0.95
		cfg.CreationRatio = 0.95
		cfg.TokenScale = 1.08
		cfg.ScaleMinInputTokens = 1
		cfg.MaxSimulatedInputTokens = 10000
		cfg.ReportedInputMaxTokens = 5000
		cfg.PreserveUpstreamCacheUsage = false
		cfg.Usage = DefaultCacheUsagePolicy()
		cfg.Usage.PreserveUpstreamCacheUsage = false
		cfg.Usage.Input = CacheUsageFieldPolicy{
			Mode:                 CacheUsageFieldSampleMax,
			MaxTokens:            5,
			NormalMaxMultiplier:  1.1,
			MoveDeltaToCacheRead: true,
		}
		cfg.Usage.Output = CacheUsageFieldPolicy{
			Mode:                CacheUsageFieldSampleMax,
			MaxTokens:           3,
			NormalMaxMultiplier: 1.1,
		}
		cfg.Usage.CacheRead = CacheUsageFieldPolicy{
			Mode:                CacheUsageFieldSampleMax,
			MaxTokens:           32,
			NormalMaxMultiplier: 1.1,
		}
		cfg.Usage.CacheCreation = CacheUsageFieldPolicy{
			Mode:                CacheUsageFieldSampleMax,
			MaxTokens:           18,
			NormalMaxMultiplier: 1.1,
		}
		cfg.Usage.FinalOutputMaxTokens = 12
		cfg.Usage.FinalCacheReadMaxTokens = 32
		cfg.Usage.FinalCacheCreationMaxTokens = 18
		// 本子测试的第三步断言「换账号后重新创建」，走的是按账号隔离。
		// 默认作用域已是「分组 + 会话」（换账号共享），下一个子测试覆盖那条路径。
		cfg.ScopeMode = CacheScopeModeGroupAccountSession
		registerCacheStrategyProfile(t, strategyID, "prefix-high-cache", cfg)

		server, _ := newCacheStrategyHTTPServer(t, anthropicCacheStreamResponse(10, 0, 0, 5))
		svc := newCacheStrategyHTTPService(t, server)
		group := newCacheStrategyGroup(99211, strategyID)
		body := []byte(`{"model":"claude-sonnet-4-6","prompt_cache_key":"prefix-high-cache","input":"stable prefix cache shaping test with enough repeated content to produce a reusable prefix","stream":false}`)

		firstCtx, firstRec := cacheResponsesContext(body)
		SetCacheGroupContext(firstCtx, group)
		first, err := svc.ForwardAsResponses(context.Background(), firstCtx, newCacheStrategyAccount(99221, "prefix-a", server.URL), body, &ParsedRequest{Group: group})
		require.NoError(t, err)
		require.NotNil(t, first)
		require.Greater(t, first.Usage.InputTokens, 0)
		require.LessOrEqual(t, first.Usage.InputTokens, 5)
		require.Equal(t, 0, first.Usage.CacheReadInputTokens)
		require.Greater(t, first.Usage.CacheCreationInputTokens, 0)
		require.LessOrEqual(t, first.Usage.CacheCreationInputTokens, 18)
		require.Greater(t, first.Usage.OutputTokens, 0)
		require.LessOrEqual(t, first.Usage.OutputTokens, 3)
		require.Equal(t, first.Usage.CacheCreationInputTokens, int(gjson.Get(firstRec.Body.String(), "usage.cache_creation_input_tokens").Int()))
		t.Logf("prefix first final usage: input=%d read=%d creation=%d output=%d", first.Usage.InputTokens, first.Usage.CacheReadInputTokens, first.Usage.CacheCreationInputTokens, first.Usage.OutputTokens)

		secondCtx, secondRec := cacheResponsesContext(body)
		SetCacheGroupContext(secondCtx, group)
		second, err := svc.ForwardAsResponses(context.Background(), secondCtx, newCacheStrategyAccount(99221, "prefix-a", server.URL), body, &ParsedRequest{Group: group})
		require.NoError(t, err)
		require.NotNil(t, second)
		require.Greater(t, second.Usage.InputTokens, 0)
		require.LessOrEqual(t, second.Usage.InputTokens, 5)
		require.Greater(t, second.Usage.CacheReadInputTokens, 0)
		require.Zero(t, second.Usage.CacheCreationInputTokens)
		require.Greater(t, second.Usage.OutputTokens, 0)
		require.LessOrEqual(t, second.Usage.OutputTokens, 3)
		require.Equal(t, second.Usage.CacheReadInputTokens, int(gjson.Get(secondRec.Body.String(), "usage.input_tokens_details.cached_tokens").Int()))
		t.Logf("prefix warm final usage: input=%d read=%d creation=%d output=%d", second.Usage.InputTokens, second.Usage.CacheReadInputTokens, second.Usage.CacheCreationInputTokens, second.Usage.OutputTokens)

		thirdCtx, _ := cacheResponsesContext(body)
		SetCacheGroupContext(thirdCtx, group)
		third, err := svc.ForwardAsResponses(context.Background(), thirdCtx, newCacheStrategyAccount(99222, "prefix-b", server.URL), body, &ParsedRequest{Group: group})
		require.NoError(t, err)
		require.NotNil(t, third)
		require.Greater(t, third.Usage.InputTokens, 0)
		require.LessOrEqual(t, third.Usage.InputTokens, 5)
		require.Equal(t, 0, third.Usage.CacheReadInputTokens)
		require.Greater(t, third.Usage.CacheCreationInputTokens, 0)
		t.Logf("prefix switched-account final usage: input=%d read=%d creation=%d output=%d", third.Usage.InputTokens, third.Usage.CacheReadInputTokens, third.Usage.CacheCreationInputTokens, third.Usage.OutputTokens)
	})

	t.Run("tool aware group session shared across accounts", func(t *testing.T) {
		resetCacheTracker()
		strategyID := int64(99202)
		cfg := smallPayloadCacheConfig(CacheStrategyKindToolAware)
		cfg.MinCacheableTokens = 1
		cfg.RatioMode = CacheRatioModeIndependent
		cfg.ReadRatio = 0.64
		cfg.CreationRatio = 0.44
		cfg.ScopeMode = CacheScopeModeGroupSession
		cfg.AllowDerivedSession = true
		cfg.PreserveUpstreamCacheUsage = true
		cfg.Usage = DefaultCacheUsagePolicy()
		cfg.Usage.PreserveUpstreamCacheUsage = true
		cfg.Usage.Input = CacheUsageFieldPolicy{
			Mode:                 CacheUsageFieldSampleMax,
			MaxTokens:            5,
			NormalMaxMultiplier:  1.1,
			MoveDeltaToCacheRead: true,
		}
		cfg.Usage.Output = CacheUsageFieldPolicy{
			Mode:                CacheUsageFieldSampleMax,
			MaxTokens:           7,
			NormalMaxMultiplier: 1.1,
		}
		cfg.Usage.CacheRead = CacheUsageFieldPolicy{
			Mode:                CacheUsageFieldSampleMax,
			MaxTokens:           5,
			NormalMaxMultiplier: 1.1,
		}
		cfg.Usage.CacheCreation = CacheUsageFieldPolicy{
			Mode:                CacheUsageFieldSampleMax,
			MaxTokens:           2,
			NormalMaxMultiplier: 1.1,
		}
		cfg.Usage.FinalOutputMaxTokens = 24
		cfg.Usage.FinalCacheReadMaxTokens = 64
		cfg.Usage.FinalCacheCreationMaxTokens = 48
		registerCacheStrategyProfile(t, strategyID, "tool-aware-group-session", cfg)

		server, _ := newCacheStrategyHTTPServer(t, anthropicPromptTokensStreamResponse(20, 11, 4, 9))
		svc := newCacheStrategyHTTPService(t, server)
		group := newCacheStrategyGroup(99212, strategyID)
		body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"usage-policy-session"},"messages":[{"role":"user","content":"stable cache policy test"}],"stream":false}`)

		firstCtx, firstRec := cacheChatContext(body)
		SetCacheGroupContext(firstCtx, group)
		first, err := svc.ForwardAsChatCompletions(context.Background(), firstCtx, newCacheStrategyAccount(99231, "tool-a", server.URL), body, &ParsedRequest{Group: group})
		require.NoError(t, err)
		require.NotNil(t, first)
		require.Greater(t, first.Usage.InputTokens, 0)
		require.LessOrEqual(t, first.Usage.InputTokens, 5)
		require.Greater(t, first.Usage.CacheReadInputTokens, 0)
		require.LessOrEqual(t, first.Usage.CacheReadInputTokens, 5)
		require.Greater(t, first.Usage.CacheCreationInputTokens, 0)
		require.Greater(t, first.Usage.OutputTokens, 0)
		require.LessOrEqual(t, first.Usage.OutputTokens, 7)
		require.Equal(t, first.Usage.CacheReadInputTokens, int(gjson.Get(firstRec.Body.String(), "usage.prompt_tokens_details.cached_tokens").Int()))
		t.Logf("tool-aware first final usage: input=%d read=%d creation=%d output=%d", first.Usage.InputTokens, first.Usage.CacheReadInputTokens, first.Usage.CacheCreationInputTokens, first.Usage.OutputTokens)

		secondCtx, secondRec := cacheChatContext(body)
		SetCacheGroupContext(secondCtx, group)
		second, err := svc.ForwardAsChatCompletions(context.Background(), secondCtx, newCacheStrategyAccount(99232, "tool-b", server.URL), body, &ParsedRequest{Group: group})
		require.NoError(t, err)
		require.NotNil(t, second)
		require.Greater(t, second.Usage.InputTokens, 0)
		require.LessOrEqual(t, second.Usage.InputTokens, 5)
		require.Greater(t, second.Usage.CacheReadInputTokens, 0)
		require.LessOrEqual(t, second.Usage.CacheCreationInputTokens, first.Usage.CacheCreationInputTokens)
		require.Greater(t, second.Usage.OutputTokens, 0)
		require.LessOrEqual(t, second.Usage.OutputTokens, 7)
		require.Equal(t, second.Usage.CacheReadInputTokens, int(gjson.Get(secondRec.Body.String(), "usage.prompt_tokens_details.cached_tokens").Int()))
		t.Logf("tool-aware shared-account final usage: input=%d read=%d creation=%d output=%d", second.Usage.InputTokens, second.Usage.CacheReadInputTokens, second.Usage.CacheCreationInputTokens, second.Usage.OutputTokens)
	})

	t.Run("disabled strategy keeps upstream usage untouched", func(t *testing.T) {
		resetCacheTracker()
		strategyID := int64(99203)
		cfg := smallPayloadCacheConfig(CacheStrategyKindDisabled)
		registerCacheStrategyProfile(t, strategyID, "disabled-cache", cfg)

		server, _ := newCacheStrategyHTTPServer(t, anthropicCacheStreamResponse(10, 0, 0, 5))
		svc := newCacheStrategyHTTPService(t, server)
		group := newCacheStrategyGroup(99213, strategyID)
		body := []byte(`{"model":"claude-sonnet-4-6","prompt_cache_key":"disabled-cache","input":"stable prompt that should not produce shaped cache usage","stream":false}`)

		ctx, rec := cacheResponsesContext(body)
		result, err := svc.ForwardAsResponses(context.Background(), ctx, newCacheStrategyAccount(99233, "disabled-a", server.URL), body, &ParsedRequest{Group: group})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Zero(t, result.Usage.CacheReadInputTokens)
		require.Zero(t, result.Usage.CacheCreationInputTokens)
		require.Equal(t, int64(10), gjson.Get(rec.Body.String(), "usage.input_tokens").Int())
		require.Zero(t, gjson.Get(rec.Body.String(), "usage.cache_read_input_tokens").Int())
		require.Zero(t, gjson.Get(rec.Body.String(), "usage.cache_creation_input_tokens").Int())
		t.Logf("disabled final usage: input=%d read=%d creation=%d output=%d", result.Usage.InputTokens, result.Usage.CacheReadInputTokens, result.Usage.CacheCreationInputTokens, result.Usage.OutputTokens)
	})
}

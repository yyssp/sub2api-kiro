package routes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// routeCacheAccountRepo supplies only the repository operations exercised by
// the real gateway scheduler. The embedded interface keeps this test double
// deliberately narrow; an unexpected production call fails loudly.
type routeCacheAccountRepo struct {
	service.AccountRepository

	mu       sync.Mutex
	accounts map[int64]service.Account
}

func (r *routeCacheAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, platform string) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform != platform || !account.IsSchedulable() {
			continue
		}
		found := false
		for _, group := range account.AccountGroups {
			if group.GroupID == groupID {
				found = true
				break
			}
		}
		if found {
			out = append(out, account)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *routeCacheAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform && account.IsSchedulable() {
			out = append(out, account)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *routeCacheAccountRepo) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]service.Account, error) {
	var out []service.Account
	for _, platform := range platforms {
		accounts, err := r.ListSchedulableByGroupIDAndPlatform(ctx, groupID, platform)
		if err != nil {
			return nil, err
		}
		out = append(out, accounts...)
	}
	return out, nil
}

func (r *routeCacheAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account, ok := r.accounts[id]
	if !ok {
		return nil, service.ErrAccountNotFound
	}
	copy := account
	return &copy, nil
}

func (r *routeCacheAccountRepo) disable(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	account.Status = service.StatusDisabled
	r.accounts[id] = account
}

type routeCacheGroupRepo struct {
	service.GroupRepository
	groups map[int64]*service.Group
}

func (r *routeCacheGroupRepo) GetByIDLite(_ context.Context, id int64) (*service.Group, error) {
	group, ok := r.groups[id]
	if !ok {
		return nil, service.ErrGroupNotFound
	}
	return group, nil
}

func (r *routeCacheGroupRepo) GetByID(ctx context.Context, id int64) (*service.Group, error) {
	return r.GetByIDLite(ctx, id)
}

type routeCacheGatewayCache struct {
	service.GatewayCache

	mu       sync.Mutex
	sessions map[string]int64
}

func (c *routeCacheGatewayCache) key(groupID int64, session string) string {
	return strconv.FormatInt(groupID, 10) + ":" + session
}

func (c *routeCacheGatewayCache) GetSessionAccountID(_ context.Context, groupID int64, session string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	accountID, ok := c.sessions[c.key(groupID, session)]
	if !ok {
		return 0, service.ErrStickySessionNotFound
	}
	return accountID, nil
}

func (c *routeCacheGatewayCache) SetSessionAccountID(_ context.Context, groupID int64, session string, accountID int64, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[c.key(groupID, session)] = accountID
	return nil
}

func (c *routeCacheGatewayCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *routeCacheGatewayCache) DeleteSessionAccountID(_ context.Context, groupID int64, session string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, c.key(groupID, session))
	return nil
}

type routeCacheConcurrency struct {
	service.ConcurrencyCache
}

func (routeCacheConcurrency) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}
func (routeCacheConcurrency) ReleaseAccountSlot(context.Context, int64, string) error { return nil }
func (routeCacheConcurrency) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}
func (routeCacheConcurrency) ReleaseUserSlot(context.Context, int64, string) error { return nil }
func (routeCacheConcurrency) GetAccountConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}
func (routeCacheConcurrency) GetAccountConcurrencyBatch(context.Context, []int64) (map[int64]int, error) {
	return map[int64]int{}, nil
}
func (routeCacheConcurrency) GetUserConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}
func (routeCacheConcurrency) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	out := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, account := range accounts {
		out[account.ID] = &service.AccountLoadInfo{AccountID: account.ID}
	}
	return out, nil
}
func (routeCacheConcurrency) GetUsersLoadBatch(context.Context, []service.UserWithConcurrency) (map[int64]*service.UserLoadInfo, error) {
	return map[int64]*service.UserLoadInfo{}, nil
}
func (routeCacheConcurrency) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (routeCacheConcurrency) DecrementAccountWaitCount(context.Context, int64) error { return nil }
func (routeCacheConcurrency) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return 0, nil
}
func (routeCacheConcurrency) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (routeCacheConcurrency) DecrementWaitCount(context.Context, int64) error { return nil }
func (routeCacheConcurrency) CleanupExpiredAccountSlots(context.Context, int64) error {
	return nil
}
func (routeCacheConcurrency) CleanupExpiredAccountSlotKeys(context.Context) error    { return nil }
func (routeCacheConcurrency) CleanupStaleProcessSlots(context.Context, string) error { return nil }

type routeCacheHTTPUpstream struct {
	client *http.Client
}

func (u routeCacheHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}
func (u routeCacheHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.client.Do(req)
}

type routeCacheUpstream struct {
	mu         sync.Mutex
	calls      int
	bodies     [][]byte
	input      []int
	output     []int
	response   []int
	failNext   bool
	failAll    bool
	inputBase  int
	outputBase int
}

func (u *routeCacheUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	u.mu.Lock()
	index := u.calls
	u.calls++
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	fail := u.failNext || u.failAll
	u.failNext = false
	u.mu.Unlock()

	if fail {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", fmt.Sprintf("mock-upstream-failed-%d", index+1))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"mock failure"}}`)
		return
	}

	input := 80 + index*13
	output := 16 + index*4
	if u.inputBase > 0 || u.outputBase > 0 {
		if u.inputBase > 0 {
			input = u.inputBase + index*13
		}
		if u.outputBase > 0 {
			output = u.outputBase + index*4
		}
	}
	u.mu.Lock()
	u.input = append(u.input, input)
	u.output = append(u.output, output)
	u.response = append(u.response, index)
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-request-id", fmt.Sprintf("mock-upstream-%d", index+1))
	if gjson.GetBytes(body, "stream").Bool() {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mock_%d\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", index+1, input, output)
		return
	}
	_, _ = fmt.Fprintf(w, `{"id":"msg_mock_%d","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":%d}}`, index+1, input, output)
}

func (u *routeCacheUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *routeCacheUpstream) usageAt(index int) (int, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.input[index], u.output[index]
}

func (u *routeCacheUpstream) lastBody() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return nil
	}
	return append([]byte(nil), u.bodies[len(u.bodies)-1]...)
}

func (u *routeCacheUpstream) failOnce() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failNext = true
}

func (u *routeCacheUpstream) setFailAll(value bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failAll = value
}

func newRouteCacheStrategy() (int64, service.CacheStrategyConfig) {
	cfg := service.DefaultCacheStrategyConfig(service.CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.DefaultTTLSeconds = 300
	cfg.HourTTLSeconds = 3600
	cfg.ReportedInputMaxTokens = 200000
	cfg.MaxNewCreationTokensPerRequest = 20000
	cfg.CreationControl.MaxCreationTokensPerEvent = 20000
	cfg.Usage = service.DefaultCacheUsagePolicy()
	cfg.Usage.PreserveUpstreamCacheUsage = false
	cfg.Usage.CacheRead = service.CacheUsageFieldPolicy{
		Mode:                service.CacheUsageFieldSampleMax,
		MaxTokens:           9,
		NormalMaxMultiplier: 1,
	}
	cfg.Usage.CacheCreation = service.CacheUsageFieldPolicy{
		Mode:                service.CacheUsageFieldSampleMax,
		MaxTokens:           7,
		NormalMaxMultiplier: 1,
	}
	return newRouteCacheStrategyWithConfig(99501, "route dispatch integration", cfg)
}

func newRouteCacheStrategyWithConfig(strategyID int64, name string, cfg service.CacheStrategyConfig) (int64, service.CacheStrategyConfig) {
	service.GlobalCacheStrategyRegistry().Put(&service.CacheStrategy{
		ID:       strategyID,
		Name:     name,
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	return strategyID, cfg
}

func highCacheUsageRouteConfig() service.CacheStrategyConfig {
	cfg := service.DefaultCacheStrategyConfig(service.CacheStrategyKindPrefix)
	cfg.UsageRatio = 0.98
	cfg.ReadRatio = 0.98
	cfg.CreationRatio = 0.98
	cfg.TokenScale = 1.15
	cfg.ScaleMinInputTokens = 20000
	cfg.MaxSimulatedInputTokens = 128000
	cfg.Usage.FinalCacheReadMaxTokens = 100000
	cfg.Usage.FinalCacheCreationMaxTokens = 50000
	cfg.Usage.OutputUpliftMinTokens = 1000
	cfg.Usage.OutputUpliftPercent = 50
	cfg.Usage.FinalOutputMaxTokens = 16384
	return cfg
}

func claudeCodeUsageRouteConfig() service.CacheStrategyConfig {
	cfg := service.DefaultCacheStrategyConfig(service.CacheStrategyKindToolAware)
	cfg.TokenScale = 1.12
	cfg.ScaleMinInputTokens = 20000
	cfg.MaxSimulatedInputTokens = 128000
	cfg.Usage.Input = service.CacheUsageFieldPolicy{
		Mode:                 service.CacheUsageFieldSampleMax,
		MaxTokens:            96,
		NormalMaxMultiplier:  1.1,
		MoveDeltaToCacheRead: true,
	}
	cfg.Usage.CacheCreation = service.CacheUsageFieldPolicy{
		Mode:                service.CacheUsageFieldSampleTarget,
		TargetTokens:        3000,
		NormalMaxMultiplier: 1.2,
	}
	cfg.Usage.FinalCacheReadMaxTokens = 100000
	cfg.Usage.FinalCacheCreationMaxTokens = 50000
	cfg.Usage.OutputUpliftMinTokens = 1000
	cfg.Usage.OutputUpliftPercent = 50
	cfg.Usage.FinalOutputMaxTokens = 16384
	return cfg
}

func inputShapingUsageRouteConfig() service.CacheStrategyConfig {
	cfg := highCacheUsageRouteConfig()
	cfg.Usage.Input = service.CacheUsageFieldPolicy{
		Mode:                 service.CacheUsageFieldSampleMax,
		MaxTokens:            96,
		NormalMaxMultiplier:  1.1,
		MoveDeltaToCacheRead: true,
	}
	cfg.Usage.Output = service.CacheUsageFieldPolicy{Mode: service.CacheUsageFieldRaw}
	cfg.Usage.CacheRead = service.CacheUsageFieldPolicy{Mode: service.CacheUsageFieldPreserve}
	cfg.Usage.CacheCreation = service.CacheUsageFieldPolicy{Mode: service.CacheUsageFieldPreserve}
	return cfg
}

func lowFrequencyCreationUsageRouteConfig() service.CacheStrategyConfig {
	cfg := service.DefaultCacheStrategyConfig(service.CacheStrategyKindPrefix)
	cfg.RatioMode = service.CacheRatioModeIndependent
	cfg.CoverageRatio = 0.78
	cfg.UsageRatio = 0.75
	cfg.ReadRatio = 1
	cfg.CreationRatio = 0.55
	cfg.MaxCoverageTokens = 1024
	cfg.MaxNewCreationTokensPerRequest = 1024
	cfg.Usage.CacheRead = service.CacheUsageFieldPolicy{
		Mode:                service.CacheUsageFieldSampleMax,
		MaxTokens:           64,
		NormalMaxMultiplier: 1.1,
	}
	cfg.Usage.CacheCreation = service.CacheUsageFieldPolicy{
		Mode:                service.CacheUsageFieldSampleMax,
		MaxTokens:           32,
		NormalMaxMultiplier: 1.1,
	}
	cfg.Usage.FinalCacheReadMaxTokens = 64
	cfg.Usage.FinalCacheCreationMaxTokens = 32
	cfg.CreationControl = service.CacheCreationControl{
		Enabled:                      true,
		MinCreationDeltaTokens:       0,
		MinSuccessfulRequestsBetween: 2,
		MaxCreationTokensPerEvent:    1024,
		CreationBudgetWindowSeconds:  300,
		MaxCreationTokensPerWindow:   2048,
	}
	return cfg
}

func readPriorityUsageRouteConfig() service.CacheStrategyConfig {
	cfg := service.DefaultCacheStrategyConfig(service.CacheStrategyKindToolAware)
	cfg.RatioMode = service.CacheRatioModeIndependent
	cfg.CoverageRatio = 0.9
	cfg.UsageRatio = 0.9
	cfg.ReadRatio = 1
	cfg.CreationRatio = 0.35
	cfg.MaxCoverageTokens = 1536
	cfg.MaxNewCreationTokensPerRequest = 1536
	cfg.IncrementalCreateEnabled = false
	cfg.Usage.CacheRead = service.CacheUsageFieldPolicy{
		Mode:                service.CacheUsageFieldSampleMax,
		MaxTokens:           80,
		NormalMaxMultiplier: 1.1,
	}
	cfg.Usage.CacheCreation = service.CacheUsageFieldPolicy{
		Mode:                service.CacheUsageFieldSampleMax,
		MaxTokens:           24,
		NormalMaxMultiplier: 1.1,
	}
	cfg.Usage.FinalCacheReadMaxTokens = 80
	cfg.Usage.FinalCacheCreationMaxTokens = 24
	cfg.CreationControl = service.CacheCreationControl{
		Enabled:                      true,
		MinCreationDeltaTokens:       0,
		MinSuccessfulRequestsBetween: 1,
		MaxCreationTokensPerEvent:    1536,
		CreationBudgetWindowSeconds:  600,
		MaxCreationTokensPerWindow:   3072,
	}
	return cfg
}

func noCacheUsageRouteConfig() service.CacheStrategyConfig {
	cfg := service.DefaultCacheStrategyConfig(service.CacheStrategyKindDisabled)
	return cfg
}

func newRouteCacheTestRouter(t *testing.T, group *service.Group, apiKey *service.APIKey, accountRepo *routeCacheAccountRepo, groupRepo *routeCacheGroupRepo, gatewayCache *routeCacheGatewayCache, upstream *routeCacheHTTPUpstream) (*gin.Engine, *service.BillingCacheService) {
	t.Helper()
	cfg := &config.Config{
		RunMode: config.RunModeSimple,
		Gateway: config.GatewayConfig{
			MaxBodySize:     1024 * 1024,
			TextMaxBodySize: 1024 * 1024,
			Scheduling: config.GatewaySchedulingConfig{
				LoadBatchEnabled:         false,
				StickySessionMaxWaiting:  3,
				StickySessionWaitTimeout: time.Second,
				FallbackWaitTimeout:      time.Second,
				FallbackMaxWaiting:       3,
			},
		},
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           true,
				AllowPrivateHosts: true,
				AllowInsecureHTTP: true,
				UpstreamHosts:     []string{"127.0.0.1"},
			},
		},
	}

	concurrencyService := service.NewConcurrencyService(routeCacheConcurrency{})
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheService.Stop)
	deferred := service.NewDeferredService(accountRepo, nil, time.Minute)
	tlsProfiles := &service.TLSFingerprintProfileService{}
	gatewayService := service.NewGatewayService(
		accountRepo,
		groupRepo,
		nil, nil, nil, nil, nil,
		gatewayCache,
		cfg,
		nil,
		concurrencyService,
		nil, nil,
		billingCacheService,
		nil,
		upstream,
		deferred,
		nil, nil, nil, nil, nil, nil, nil,
		tlsProfiles,
		nil, nil, nil, nil, nil,
	)
	gatewayHandler := handler.NewGatewayHandler(
		gatewayService, nil, nil, nil, nil, concurrencyService, billingCacheService,
		nil, nil, nil, nil, nil, nil, nil, cfg, nil,
	)

	router := gin.New()
	auth := servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyAPIKey), apiKey)
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{
			UserID:      apiKey.User.ID,
			Concurrency: apiKey.User.Concurrency,
		})
		c.Set(string(servermiddleware.ContextKeyUserRole), apiKey.User.Role)
		c.Next()
	})
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{Gateway: gatewayHandler, OpenAIGateway: &handler.OpenAIGatewayHandler{}},
		auth, nil, nil, nil, nil, nil, cfg,
	)
	return router, billingCacheService
}

func routeCacheRequest(t *testing.T, router http.Handler, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer route-cache-test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "route-cache-integration-test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), rec.Body.String())
	return payload
}

func routeCacheStreamRequest(t *testing.T, router http.Handler, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer route-cache-test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "route-cache-integration-test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	return rec.Body.String()
}

func streamUsageFromAnthropicBody(body string) (input, read, creation int) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		switch gjson.Get(data, "type").String() {
		case "message_start":
			usage := gjson.Get(data, "message.usage")
			input = int(usage.Get("input_tokens").Int())
			read = int(usage.Get("cache_read_input_tokens").Int())
			creation = int(usage.Get("cache_creation_input_tokens").Int())
		case "message_delta":
			usage := gjson.Get(data, "usage")
			if usage.Get("cache_read_input_tokens").Exists() {
				read = int(usage.Get("cache_read_input_tokens").Int())
			}
			if usage.Get("cache_creation_input_tokens").Exists() {
				creation = int(usage.Get("cache_creation_input_tokens").Int())
			}
		}
	}
	return input, read, creation
}

func TestGatewayCacheStrategyRealDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	strategyID, cfg := newRouteCacheStrategy()
	t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

	upstreamAState := &routeCacheUpstream{}
	upstreamBState := &routeCacheUpstream{}
	upstreamA := httptest.NewTLSServer(http.HandlerFunc(upstreamAState.handler))
	upstreamB := httptest.NewTLSServer(http.HandlerFunc(upstreamBState.handler))
	t.Cleanup(upstreamA.Close)
	t.Cleanup(upstreamB.Close)

	groupID := int64(99511)
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
	accountA := service.Account{
		ID:          99521,
		Name:        "mock-upstream-a",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		AccountGroups: []service.AccountGroup{
			{GroupID: groupID},
		},
		Credentials: map[string]any{"api_key": "mock-a", "base_url": upstreamA.URL},
		Extra:       map[string]any{"anthropic_passthrough": true},
	}
	accountB := service.Account{
		ID:          99522,
		Name:        "mock-upstream-b",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		AccountGroups: []service.AccountGroup{
			{GroupID: groupID},
		},
		Credentials: map[string]any{"api_key": "mock-b", "base_url": upstreamB.URL},
		Extra:       map[string]any{"anthropic_passthrough": true},
	}
	accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{
		accountA.ID: accountA,
		accountB.ID: accountB,
	}}
	gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
	user := &service.User{ID: 99531, Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 4}
	apiKey := &service.APIKey{
		ID:      99541,
		UserID:  user.ID,
		Key:     "route-cache-test-key",
		Status:  service.StatusActive,
		User:    user,
		GroupID: &groupID,
		Group:   group,
	}
	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only TLS mocks
	}}
	httpUpstream := &routeCacheHTTPUpstream{client: httpClient}
	router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, httpUpstream)

	body := `{"model":"claude-sonnet-4-6","metadata":{"session_id":"dispatch-session-a"},"system":[{"type":"text","text":"stable project instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"first stable prompt","cache_control":{"type":"ephemeral"}}]}],"max_tokens":128,"stream":false}`

	first := routeCacheRequest(t, router, body)
	firstUsage := first["usage"].(map[string]any)
	t.Logf("cold usage: input=%v cache_read=%v cache_creation=%v output=%v", firstUsage["input_tokens"], firstUsage["cache_read_input_tokens"], firstUsage["cache_creation_input_tokens"], firstUsage["output_tokens"])
	require.Equal(t, float64(0), firstUsage["cache_read_input_tokens"], "first request must be cold")
	require.Greater(t, int(firstUsage["cache_creation_input_tokens"].(float64)), 0, "policy must create a cache on first request")
	require.LessOrEqual(t, int(firstUsage["cache_creation_input_tokens"].(float64)), 7)
	require.LessOrEqual(t, int(firstUsage["input_tokens"].(float64))+int(firstUsage["cache_read_input_tokens"].(float64))+int(firstUsage["cache_creation_input_tokens"].(float64)), cfg.ReportedInputMaxTokens)
	require.Equal(t, 1, upstreamAState.callCount()+upstreamBState.callCount())

	second := routeCacheRequest(t, router, body)
	secondUsage := second["usage"].(map[string]any)
	t.Logf("warm usage: input=%v cache_read=%v cache_creation=%v output=%v", secondUsage["input_tokens"], secondUsage["cache_read_input_tokens"], secondUsage["cache_creation_input_tokens"], secondUsage["output_tokens"])
	require.Greater(t, int(secondUsage["cache_read_input_tokens"].(float64)), 0, "same session should hit the account-local cache")
	require.Equal(t, float64(0), secondUsage["cache_creation_input_tokens"])
	require.LessOrEqual(t, int(secondUsage["cache_read_input_tokens"].(float64)), 9)
	require.LessOrEqual(t, int(secondUsage["input_tokens"].(float64))+int(secondUsage["cache_read_input_tokens"].(float64))+int(secondUsage["cache_creation_input_tokens"].(float64)), cfg.ReportedInputMaxTokens)
	require.Equal(t, 2, upstreamAState.callCount()+upstreamBState.callCount())

	// A disabled account forces a real scheduler switch. The new account must
	// start cold and must not reuse the first account's cache state.
	accountRepo.disable(accountA.ID)
	third := routeCacheRequest(t, router, body)
	thirdUsage := third["usage"].(map[string]any)
	t.Logf("switched-account usage: input=%v cache_read=%v cache_creation=%v output=%v", thirdUsage["input_tokens"], thirdUsage["cache_read_input_tokens"], thirdUsage["cache_creation_input_tokens"], thirdUsage["output_tokens"])
	require.Equal(t, 0, int(thirdUsage["cache_read_input_tokens"].(float64)), "cache state must not cross account boundaries")
	require.Greater(t, int(thirdUsage["cache_creation_input_tokens"].(float64)), 0)
	require.Equal(t, 1, upstreamBState.callCount())

	// Upstream usage is intentionally non-constant; prove the mock generated
	// distinct values while the shaped buckets remain within policy bounds.
	firstInput, firstOutput := upstreamAState.usageAt(0)
	secondInput, secondOutput := upstreamAState.usageAt(1)
	require.NotEqual(t, firstInput, secondInput)
	require.NotEqual(t, firstOutput, secondOutput)
	require.NotEmpty(t, gjson.GetBytes(upstreamAState.lastBody(), "metadata.session_id").String())
}

func TestGatewayCacheStrategyRealDispatchRevisionInvalidatesCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	strategyID, _ := newRouteCacheStrategy()
	t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

	state := &routeCacheUpstream{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(state.handler))
	t.Cleanup(upstream.Close)

	groupID := int64(99561)
	group := &service.Group{
		ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive,
		Hydrated: true, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard,
		CacheStrategyID: &strategyID,
	}
	groupRepo := &routeCacheGroupRepo{groups: map[int64]*service.Group{groupID: group}}
	account := service.Account{
		ID: 99562, Name: "revision-account", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		AccountGroups: []service.AccountGroup{{GroupID: groupID}},
		Credentials:   map[string]any{"api_key": "mock", "base_url": upstream.URL},
		Extra:         map[string]any{"anthropic_passthrough": true},
	}
	accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{account.ID: account}}
	gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
	user := &service.User{ID: 99563, Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 2}
	apiKey := &service.APIKey{ID: 99564, UserID: user.ID, Key: "route-cache-test-key", Status: service.StatusActive, User: user, GroupID: &groupID, Group: group}
	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only TLS mock
	}}
	router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, &routeCacheHTTPUpstream{client: httpClient})

	body := `{"model":"claude-sonnet-4-6","metadata":{"session_id":"revision-session"},"system":[{"type":"text","text":"stable revision prompt","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"turn one","cache_control":{"type":"ephemeral"}}]}],"max_tokens":64,"stream":false}`
	first := routeCacheRequest(t, router, body)
	require.Greater(t, int(first["usage"].(map[string]any)["cache_creation_input_tokens"].(float64)), 0)
	second := routeCacheRequest(t, router, body)
	require.Greater(t, int(second["usage"].(map[string]any)["cache_read_input_tokens"].(float64)), 0)

	current := service.GlobalCacheStrategyRegistry().Get(strategyID)
	require.NotNil(t, current)
	next := *current
	next.Revision = 2
	service.GlobalCacheStrategyRegistry().Put(&next)

	afterRevision := routeCacheRequest(t, router, body)
	afterUsage := afterRevision["usage"].(map[string]any)
	t.Logf("revision usage: input=%v cache_read=%v cache_creation=%v output=%v", afterUsage["input_tokens"], afterUsage["cache_read_input_tokens"], afterUsage["cache_creation_input_tokens"], afterUsage["output_tokens"])
	require.Equal(t, float64(0), afterUsage["cache_read_input_tokens"], "strategy revision is part of cache identity")
	require.Greater(t, int(afterUsage["cache_creation_input_tokens"].(float64)), 0)
	require.Equal(t, 3, state.callCount())
}

func TestGatewayCacheStrategyRealDispatchStreamingUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	strategyID, _ := newRouteCacheStrategy()
	t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

	state := &routeCacheUpstream{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(state.handler))
	t.Cleanup(upstream.Close)

	groupID := int64(99571)
	group := &service.Group{
		ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive,
		Hydrated: true, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard,
		CacheStrategyID: &strategyID,
	}
	groupRepo := &routeCacheGroupRepo{groups: map[int64]*service.Group{groupID: group}}
	account := service.Account{
		ID: 99572, Name: "stream-account", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		AccountGroups: []service.AccountGroup{{GroupID: groupID}},
		Credentials:   map[string]any{"api_key": "mock", "base_url": upstream.URL},
		Extra:         map[string]any{"anthropic_passthrough": true},
	}
	accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{account.ID: account}}
	gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
	user := &service.User{ID: 99573, Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 2}
	apiKey := &service.APIKey{ID: 99574, UserID: user.ID, Key: "route-cache-test-key", Status: service.StatusActive, User: user, GroupID: &groupID, Group: group}
	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only TLS mock
	}}
	router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, &routeCacheHTTPUpstream{client: httpClient})

	body := `{"model":"claude-sonnet-4-6","metadata":{"session_id":"stream-session"},"system":[{"type":"text","text":"stable stream instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"stream turn","cache_control":{"type":"ephemeral"}}]}],"max_tokens":64,"stream":true}`
	first := routeCacheStreamRequest(t, router, body)
	firstInput, firstRead, firstCreation := streamUsageFromAnthropicBody(first)
	t.Logf("stream cold usage: input=%d cache_read=%d cache_creation=%d", firstInput, firstRead, firstCreation)
	require.Equal(t, 0, firstRead, "stream first request must be cold")
	require.Greater(t, firstCreation, 0)
	require.Greater(t, firstInput, 0)

	second := routeCacheStreamRequest(t, router, body)
	secondInput, secondRead, secondCreation := streamUsageFromAnthropicBody(second)
	t.Logf("stream warm usage: input=%d cache_read=%d cache_creation=%d", secondInput, secondRead, secondCreation)
	require.Greater(t, secondRead, 0, "stream second request should read the committed prefix")
	require.Zero(t, secondCreation)
	require.Greater(t, secondInput, 0)
	require.Equal(t, 2, state.callCount())
}

func TestGatewayCacheStrategyRealDispatchFailureDoesNotCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	strategyID, _ := newRouteCacheStrategy()
	t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

	state := &routeCacheUpstream{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(state.handler))
	t.Cleanup(upstream.Close)

	groupID := int64(99581)
	group := &service.Group{
		ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive,
		Hydrated: true, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard,
		CacheStrategyID: &strategyID,
	}
	groupRepo := &routeCacheGroupRepo{groups: map[int64]*service.Group{groupID: group}}
	account := service.Account{
		ID: 99582, Name: "failure-account", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		AccountGroups: []service.AccountGroup{{GroupID: groupID}},
		Credentials:   map[string]any{"api_key": "mock", "base_url": upstream.URL},
		Extra:         map[string]any{"anthropic_passthrough": true},
	}
	accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{account.ID: account}}
	gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
	user := &service.User{ID: 99583, Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 2}
	apiKey := &service.APIKey{ID: 99584, UserID: user.ID, Key: "route-cache-test-key", Status: service.StatusActive, User: user, GroupID: &groupID, Group: group}
	httpClient := &http.Client{Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only TLS mock
	}}
	router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, &routeCacheHTTPUpstream{client: httpClient})

	body := `{"model":"claude-sonnet-4-6","metadata":{"session_id":"failure-session"},"system":[{"type":"text","text":"stable failure prompt","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"turn one","cache_control":{"type":"ephemeral"}}]}],"max_tokens":64,"stream":false}`
	state.setFailAll(true)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer route-cache-test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "route-cache-integration-test")
	failed := httptest.NewRecorder()
	router.ServeHTTP(failed, req)
	require.GreaterOrEqual(t, failed.Code, http.StatusBadGateway)
	require.Equal(t, http.StatusBadGateway, failed.Code, "a failed Anthropic upstream must surface as a gateway error")
	state.setFailAll(false)

	success := routeCacheRequest(t, router, body)
	usage := success["usage"].(map[string]any)
	t.Logf("failed request status=%d upstream_calls=%d; recovery usage: input=%v cache_read=%v cache_creation=%v output=%v",
		failed.Code,
		state.callCount(),
		usage["input_tokens"],
		usage["cache_read_input_tokens"],
		usage["cache_creation_input_tokens"],
		usage["output_tokens"],
	)
	require.Equal(t, float64(0), usage["cache_read_input_tokens"], "failed upstream response must not commit a readable prefix")
	require.Greater(t, int(usage["cache_creation_input_tokens"].(float64)), 0)
	// API-key passthrough failover excludes the account after the first
	// retryable 500. The next request is a fresh scheduler cycle, so the
	// recovery call is the second upstream call rather than a same-account
	// five-attempt loop.
	require.Equal(t, 2, state.callCount())
}

func TestGatewayCacheStrategyTemplateMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	usageInt := func(usage map[string]any, key string) int {
		if usage == nil {
			return 0
		}
		value, ok := usage[key]
		if !ok || value == nil {
			return 0
		}
		switch v := value.(type) {
		case float64:
			return int(v)
		case float32:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		case json.Number:
			i, err := v.Int64()
			if err != nil {
				return 0
			}
			return int(i)
		default:
			return 0
		}
	}

	type matrixCase struct {
		name       string
		strategyID int64
		cfg        service.CacheStrategyConfig
		inputBase  int
		outputBase int
		checkCold  func(*testing.T, map[string]any)
		checkWarm  func(*testing.T, map[string]any)
	}

	cases := []matrixCase{
		{
			name:       "high_cache_prefix",
			strategyID: 99601,
			cfg:        highCacheUsageRouteConfig(),
			inputBase:  180,
			outputBase: 24,
			checkCold: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Greater(t, usageInt(usage, "input_tokens"), 96)
				require.Zero(t, usageInt(usage, "cache_read_input_tokens"))
				require.Greater(t, usageInt(usage, "cache_creation_input_tokens"), 0)
				require.LessOrEqual(t, usageInt(usage, "cache_creation_input_tokens"), 4096)
			},
			checkWarm: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Greater(t, usageInt(usage, "cache_read_input_tokens"), 0)
				require.Zero(t, usageInt(usage, "cache_creation_input_tokens"))
			},
		},
		{
			name:       "claude_code_tool_aware",
			strategyID: 99602,
			cfg:        claudeCodeUsageRouteConfig(),
			inputBase:  5000,
			outputBase: 24,
			checkCold: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.LessOrEqual(t, usageInt(usage, "input_tokens"), 96)
				require.Zero(t, usageInt(usage, "cache_read_input_tokens"))
				require.GreaterOrEqual(t, usageInt(usage, "cache_creation_input_tokens"), 2550)
				require.LessOrEqual(t, usageInt(usage, "cache_creation_input_tokens"), 3600)
			},
			checkWarm: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.LessOrEqual(t, usageInt(usage, "input_tokens"), 96)
				require.Greater(t, usageInt(usage, "cache_read_input_tokens"), 0)
				require.Zero(t, usageInt(usage, "cache_creation_input_tokens"))
			},
		},
		{
			name:       "input_shaping_prefix",
			strategyID: 99603,
			cfg:        inputShapingUsageRouteConfig(),
			inputBase:  180,
			outputBase: 24,
			checkCold: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.LessOrEqual(t, usageInt(usage, "input_tokens"), 96)
				require.Zero(t, usageInt(usage, "cache_read_input_tokens"))
				require.Greater(t, usageInt(usage, "cache_creation_input_tokens"), 0)
			},
			checkWarm: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.LessOrEqual(t, usageInt(usage, "input_tokens"), 96)
				require.Greater(t, usageInt(usage, "cache_read_input_tokens"), 0)
				require.Zero(t, usageInt(usage, "cache_creation_input_tokens"))
			},
		},
		{
			name:       "low_frequency_creation",
			strategyID: 99605,
			cfg:        lowFrequencyCreationUsageRouteConfig(),
			inputBase:  420,
			outputBase: 30,
			checkCold: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Equal(t, 0, usageInt(usage, "cache_read_input_tokens"))
				require.Greater(t, usageInt(usage, "cache_creation_input_tokens"), 0)
				require.LessOrEqual(t, usageInt(usage, "cache_creation_input_tokens"), 32)
			},
			checkWarm: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Greater(t, usageInt(usage, "cache_read_input_tokens"), 0)
				require.LessOrEqual(t, usageInt(usage, "cache_read_input_tokens"), 64)
				require.Zero(t, usageInt(usage, "cache_creation_input_tokens"))
			},
		},
		{
			name:       "read_priority",
			strategyID: 99606,
			cfg:        readPriorityUsageRouteConfig(),
			inputBase:  520,
			outputBase: 36,
			checkCold: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Equal(t, 0, usageInt(usage, "cache_read_input_tokens"))
				require.Greater(t, usageInt(usage, "cache_creation_input_tokens"), 0)
				require.LessOrEqual(t, usageInt(usage, "cache_creation_input_tokens"), 24)
			},
			checkWarm: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Greater(t, usageInt(usage, "cache_read_input_tokens"), 0)
				require.LessOrEqual(t, usageInt(usage, "cache_read_input_tokens"), 80)
				require.Zero(t, usageInt(usage, "cache_creation_input_tokens"))
			},
		},
		{
			name:       "no_cache_disabled",
			strategyID: 99604,
			cfg:        noCacheUsageRouteConfig(),
			inputBase:  180,
			outputBase: 24,
			checkCold: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Equal(t, 180, usageInt(usage, "input_tokens"))
				require.Zero(t, usageInt(usage, "cache_read_input_tokens"))
				require.Zero(t, usageInt(usage, "cache_creation_input_tokens"))
			},
			checkWarm: func(t *testing.T, usage map[string]any) {
				t.Helper()
				require.Equal(t, 193, usageInt(usage, "input_tokens"))
				require.Zero(t, usageInt(usage, "cache_read_input_tokens"))
				require.Zero(t, usageInt(usage, "cache_creation_input_tokens"))
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Helper()
			strategyID, _ := newRouteCacheStrategyWithConfig(tc.strategyID, tc.name, tc.cfg)
			t.Cleanup(func() { service.GlobalCacheStrategyRegistry().Delete(strategyID) })

			state := &routeCacheUpstream{inputBase: tc.inputBase, outputBase: tc.outputBase}
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
			accountID := strategyID + 20
			account := service.Account{
				ID:          accountID,
				Name:        tc.name + "-account",
				Platform:    service.PlatformAnthropic,
				Type:        service.AccountTypeAPIKey,
				Status:      service.StatusActive,
				Schedulable: true,
				Concurrency: 1,
				AccountGroups: []service.AccountGroup{
					{GroupID: groupID},
				},
				Credentials: map[string]any{"api_key": "mock", "base_url": upstream.URL},
				Extra:       map[string]any{"anthropic_passthrough": true},
			}
			accountRepo := &routeCacheAccountRepo{accounts: map[int64]service.Account{account.ID: account}}
			gatewayCache := &routeCacheGatewayCache{sessions: make(map[string]int64)}
			userID := strategyID + 30
			apiKey := &service.APIKey{
				ID:      strategyID + 40,
				UserID:  userID,
				Key:     "route-cache-test-key-" + tc.name,
				Status:  service.StatusActive,
				User:    &service.User{ID: userID, Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 4},
				GroupID: &groupID,
				Group:   group,
			}
			httpClient := &http.Client{Transport: &http.Transport{
				Proxy:           nil,
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only TLS mock
			}}
			router, _ := newRouteCacheTestRouter(t, group, apiKey, accountRepo, groupRepo, gatewayCache, &routeCacheHTTPUpstream{client: httpClient})

			body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"matrix-%s"},"system":[{"type":"text","text":"stable matrix instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"%s","cache_control":{"type":"ephemeral"}}]}],"max_tokens":128,"stream":false}`, tc.name, strings.Repeat("matrix prompt "+tc.name+" ", 512))

			first := routeCacheRequest(t, router, body)
			firstUsage := first["usage"].(map[string]any)
			tc.checkCold(t, firstUsage)

			second := routeCacheRequest(t, router, body)
			secondUsage := second["usage"].(map[string]any)
			tc.checkWarm(t, secondUsage)

			require.Equal(t, 2, state.callCount())
			firstInput, firstOutput := state.usageAt(0)
			secondInput, secondOutput := state.usageAt(1)
			require.NotEqual(t, firstInput, secondInput)
			require.NotEqual(t, firstOutput, secondOutput)
			require.NotEmpty(t, gjson.GetBytes(state.lastBody(), "metadata.session_id").String())
		})
	}
}

// Compile-time checks catch accidental drift in the production interfaces.
var (
	_ service.AccountRepository = (*routeCacheAccountRepo)(nil)
	_ service.GroupRepository   = (*routeCacheGroupRepo)(nil)
	_ service.GatewayCache      = (*routeCacheGatewayCache)(nil)
	_ service.ConcurrencyCache  = routeCacheConcurrency{}
	_ service.HTTPUpstream      = routeCacheHTTPUpstream{}
)

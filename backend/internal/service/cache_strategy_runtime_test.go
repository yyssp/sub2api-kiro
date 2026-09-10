package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func resetCacheTracker() {
	globalCacheTracker = &cacheTracker{
		entries:  make(map[uint64]map[[32]byte]cacheEntry),
		controls: make(map[uint64]cacheControlState),
	}
}

func cacheRequestBody(label string, oneHour bool) []byte {
	ttl := ""
	if oneHour {
		ttl = `,"ttl":"1h"`
	}
	return []byte(fmt.Sprintf(
		`{"model":"claude-sonnet-4-6","metadata":{"session_id":"cache-session-%s"},"messages":[{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"%s}}]}]}`,
		label,
		strings.Repeat("cacheable prompt chunk "+label+" ", 512),
		ttl,
	))
}

type cacheStrategyRepoStub struct {
	listBoundGroups     []Group
	listBoundGroupsErr  error
	setGroupBindings    []int64
	setGroupBindingsErr error
}

func (s *cacheStrategyRepoStub) Create(context.Context, *CacheStrategy) error {
	panic("unexpected Create call")
}

func (s *cacheStrategyRepoStub) GetByID(context.Context, int64) (*CacheStrategy, error) {
	panic("unexpected GetByID call")
}

func (s *cacheStrategyRepoStub) List(context.Context, string) ([]CacheStrategy, error) {
	panic("unexpected List call")
}

func (s *cacheStrategyRepoStub) Update(context.Context, *CacheStrategy) error {
	panic("unexpected Update call")
}

func (s *cacheStrategyRepoStub) Delete(context.Context, int64) error {
	panic("unexpected Delete call")
}

func (s *cacheStrategyRepoStub) CountBoundGroups(context.Context, int64) (int, error) {
	return 0, nil
}

func (s *cacheStrategyRepoStub) ListBoundGroups(context.Context, int64) ([]Group, error) {
	if s.listBoundGroupsErr != nil {
		return nil, s.listBoundGroupsErr
	}
	out := make([]Group, len(s.listBoundGroups))
	copy(out, s.listBoundGroups)
	return out, nil
}

func (s *cacheStrategyRepoStub) GetGroupBindings(_ context.Context, groupIDs []int64) ([]Group, error) {
	byID := make(map[int64]Group, len(s.listBoundGroups))
	for _, group := range s.listBoundGroups {
		byID[group.ID] = group
	}
	out := make([]Group, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		if group, ok := byID[groupID]; ok {
			out = append(out, group)
		}
	}
	return out, nil
}

func (s *cacheStrategyRepoStub) SetGroupBindings(_ context.Context, _ int64, groupIDs []int64) error {
	if s.setGroupBindingsErr != nil {
		return s.setGroupBindingsErr
	}
	s.setGroupBindings = append(s.setGroupBindings[:0], groupIDs...)
	return nil
}

func (s *cacheStrategyRepoStub) ReplaceGroupBindings(_ context.Context, _ int64, groupIDs []int64) error {
	s.setGroupBindings = append(s.setGroupBindings[:0], groupIDs...)
	return nil
}

type cacheStrategyAuthInvalidatorStub struct {
	groupIDs []int64
}

func (s *cacheStrategyAuthInvalidatorStub) InvalidateAuthCacheByKey(context.Context, string) {}

func (s *cacheStrategyAuthInvalidatorStub) InvalidateAuthCacheByUserID(context.Context, int64) {}

func (s *cacheStrategyAuthInvalidatorStub) InvalidateAuthCacheByGroupID(_ context.Context, groupID int64) {
	s.groupIDs = append(s.groupIDs, groupID)
}

func TestCacheStrategyBindingAppliesToAnthropicMessagesProfile(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900001)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "anthropic-prefix",
		Enabled:  true,
		Revision: 1,
		Config: func() CacheStrategyConfig {
			cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
			cfg.CoverageRatio = 0.5
			cfg.UsageRatio = 1
			return cfg
		}(),
	})
	group := &Group{ID: 9001, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9002, Platform: PlatformAnthropic}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"bound-session"},"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("bound strategy ", 512) + `","cache_control":{"type":"ephemeral"}}]}]}`)

	first := (&GatewayService{}).buildCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2000)
	require.NotNil(t, first)
	require.Equal(t, 1000, first.CacheCreationInputTokens)
	require.Equal(t, 1000, first.InputTokens)

	second := (&GatewayService{}).buildCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2000)
	require.NotNil(t, second)
	require.Equal(t, 1000, second.CacheReadInputTokens)
	require.Zero(t, second.CacheCreationInputTokens)
}

func TestCacheSessionKeyReadsClaudeCodeJSONMetadataUserID(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5-20250929","metadata":{"user_id":"{\"device_id\":\"device-a\",\"session_id\":\"claude-code-session-42\"}"},"messages":[{"role":"user","content":"follow-up"}]}`)
	require.Equal(t, "claude-code-session-42", cacheSessionKey(body, false))
}

func TestCacheStrategyBindGroupsInvalidatesOldAndNewGroupAuthSnapshots(t *testing.T) {
	repo := &cacheStrategyRepoStub{
		listBoundGroups: []Group{
			{ID: 11},
			{ID: 12},
		},
	}
	invalidator := &cacheStrategyAuthInvalidatorStub{}
	svc := NewCacheStrategyService(repo, invalidator)

	err := svc.BindGroups(context.Background(), 42, []int64{12, 13})
	require.NoError(t, err)
	require.Equal(t, []int64{12, 13}, repo.setGroupBindings)
	require.ElementsMatch(t, []int64{11, 12, 13}, invalidator.groupIDs)
}

func TestCacheStrategyBindGroupsRejectsAlreadyBoundGroup(t *testing.T) {
	repo := &cacheStrategyRepoStub{
		listBoundGroups: []Group{
			{ID: 21, Name: "alpha", CacheStrategyID: cacheStrategyPtrInt64(1001)},
		},
	}
	svc := NewCacheStrategyService(repo)

	err := svc.BindGroups(context.Background(), 1002, []int64{21})
	require.Error(t, err)
	require.Equal(t, 409, infraerrors.Code(err))
	require.Equal(t, "CACHE_STRATEGY_GROUP_CONFLICT", infraerrors.Reason(err))
	require.Contains(t, infraerrors.Message(err), "group is already bound to a cache strategy")
	md := infraerrors.FromError(err).Metadata
	require.Equal(t, "21", md["group_id"])
	require.Equal(t, "alpha", md["group_name"])
	require.Equal(t, "1001", md["current_strategy_id"])
	require.Equal(t, "1002", md["requested_strategy_id"])
	require.Contains(t, md["reason"], "unbind")
	require.Empty(t, repo.setGroupBindings)
}

func TestCacheStrategyBindGroupsRejectsRepeatedBindingToSameStrategy(t *testing.T) {
	repo := &cacheStrategyRepoStub{
		listBoundGroups: []Group{
			{ID: 31, Name: "beta", CacheStrategyID: cacheStrategyPtrInt64(1003)},
		},
	}
	svc := NewCacheStrategyService(repo)

	err := svc.BindGroups(context.Background(), 1003, []int64{31})
	require.Error(t, err)
	require.Equal(t, 409, infraerrors.Code(err))
	require.Equal(t, "CACHE_STRATEGY_GROUP_CONFLICT", infraerrors.Reason(err))
	md := infraerrors.FromError(err).Metadata
	require.Equal(t, "31", md["group_id"])
	require.Equal(t, "beta", md["group_name"])
	require.Equal(t, "1003", md["current_strategy_id"])
	require.Equal(t, "1003", md["requested_strategy_id"])
	require.Contains(t, md["reason"], "repeated binding")
	require.Empty(t, repo.setGroupBindings)
}

func TestCacheStrategyReplaceGroupsAllowsExistingOwnerAndPreservesConflictChecks(t *testing.T) {
	repo := &cacheStrategyRepoStub{
		listBoundGroups: []Group{
			{ID: 31, Name: "owned", CacheStrategyID: cacheStrategyPtrInt64(1003)},
			{ID: 32, Name: "other", CacheStrategyID: cacheStrategyPtrInt64(1004)},
		},
	}
	svc := NewCacheStrategyService(repo)

	require.NoError(t, svc.ReplaceGroups(context.Background(), 1003, []int64{31}))
	require.Equal(t, []int64{31}, repo.setGroupBindings)

	err := svc.ReplaceGroups(context.Background(), 1003, []int64{31, 32})
	require.Error(t, err)
	require.Equal(t, 409, infraerrors.Code(err))
	md := infraerrors.FromError(err).Metadata
	require.Equal(t, "32", md["group_id"])
	require.Equal(t, "1004", md["current_strategy_id"])
	require.Equal(t, "1003", md["requested_strategy_id"])
}

func TestBoundCacheStrategyIsolatesCacheByAccount(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900003)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "account isolation",
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9005, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	body := cacheRequestBody("bound account isolation", false)
	svc := &GatewayService{}

	first := svc.buildCacheEmulationUsage(
		context.Background(),
		&Account{ID: 9006, Platform: PlatformAnthropic},
		group,
		body,
		"claude-sonnet-4-6",
		2000,
	)
	require.NotNil(t, first)
	require.Greater(t, first.CacheCreationInputTokens, 0)
	require.Zero(t, first.CacheReadInputTokens)

	switchedAccount := svc.buildCacheEmulationUsage(
		context.Background(),
		&Account{ID: 9007, Platform: PlatformAnthropic},
		group,
		body,
		"claude-sonnet-4-6",
		2000,
	)
	require.NotNil(t, switchedAccount)
	require.Greater(t, switchedAccount.CacheCreationInputTokens, 0)
	require.Zero(t, switchedAccount.CacheReadInputTokens)
}

func TestCacheTrackerFallsBackToOlderBreakpointWhenCreationIsCapped(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900004)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.CoverageRatio = 1
	cfg.MaxNewCreationTokensPerRequest = 1500
	cfg.IncrementalCreateEnabled = false
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "bounded older breakpoint",
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9007, Platform: PlatformOpenAI, CacheStrategyID: &strategyID}
	account := &Account{ID: 9008, Platform: PlatformOpenAI}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"bounded-older-breakpoint"},"messages":[]}`)

	// Build more than cachePrefixLookbackLimit complete historical boundaries.
	// The creation cap permits only the first boundary to be committed. The
	// second identical request must still find that older cached prefix.
	blocks := make([]pendingCacheBlock, 0, cachePrefixLookbackLimit+3)
	ttl := cacheDefaultTTL
	for i := 0; i < cachePrefixLookbackLimit+3; i++ {
		messageIndex := i
		blocks = append(blocks, pendingCacheBlock{
			value:         map[string]any{"kind": "message", "message_index": i, "text": fmt.Sprintf("stable-%d", i)},
			tokens:        1000,
			breakpointTTL: &ttl,
			messageIndex:  &messageIndex,
			isMessageEnd:  true,
		})
	}
	profile, ok := buildCacheProfileFromBlocksWithMin(
		"claude-sonnet-4-6",
		(cachePrefixLookbackLimit+3)*1000+1000,
		map[string]any{"protocol": "chat_completions", "model": "claude-sonnet-4-6"},
		blocks,
		1,
	)
	require.True(t, ok)
	profile.rawBody = append([]byte(nil), body...)
	profile.protocolFamily = "openai_chat_completions"

	svc := &GatewayService{}
	first := svc.prepareCacheEmulationPlanFromProfile(account, group, profile, profile.totalInputTokens)
	require.NotNil(t, first)
	require.NotNil(t, first.result())
	require.Zero(t, first.result().CacheReadInputTokens)
	require.Equal(t, 1000, first.result().CacheCreationInputTokens)
	first.commit()

	secondProfile, ok := buildCacheProfileFromBlocksWithMin(
		"claude-sonnet-4-6",
		(cachePrefixLookbackLimit+3)*1000+1000,
		map[string]any{"protocol": "chat_completions", "model": "claude-sonnet-4-6"},
		blocks,
		1,
	)
	require.True(t, ok)
	secondProfile.rawBody = append([]byte(nil), body...)
	secondProfile.protocolFamily = "openai_chat_completions"
	second := svc.prepareCacheEmulationPlanFromProfile(account, group, secondProfile, secondProfile.totalInputTokens)
	require.NotNil(t, second)
	require.NotNil(t, second.result())
	require.Equal(t, 1000, second.result().CacheReadInputTokens)
	require.Zero(t, second.result().CacheCreationInputTokens)
}

func TestCacheStrategyDisabledBindingSkipsReadAndWrite(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900002)
	cfg := smallPayloadCacheConfig(CacheStrategyKindDisabled)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{ID: strategyID, Name: "disabled", Enabled: true, Revision: 1, Config: cfg})
	group := &Group{ID: 9003, Platform: PlatformKiro, CacheStrategyID: &strategyID}
	account := &Account{ID: 9004, Platform: PlatformKiro}
	require.Nil(t, (&GatewayService{}).buildCacheEmulationUsage(context.Background(), account, group, cacheRequestBody("disabled binding", false), "claude-sonnet-4-6", 2000))
}

func TestCacheStrategySnapshotKeepsExplicitDisabledBinding(t *testing.T) {
	strategyID := int64(900003)
	cfg := smallPayloadCacheConfig(CacheStrategyKindDisabled)
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "显式关闭缓存",
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	group := &Group{ID: 9004, Platform: PlatformKiro, CacheStrategyID: &strategyID}
	apiKey := &APIKey{ID: 9005, Group: group}

	id, name := cacheStrategySnapshotForAPIKey(apiKey)
	require.NotNil(t, id)
	require.NotNil(t, name)
	require.Equal(t, strategyID, *id)
	require.Equal(t, "显式关闭缓存", *name)
}

func TestCacheUsagePolicyProjectsInputOutputAndCacheBuckets(t *testing.T) {
	policy := DefaultCacheUsagePolicy()
	policy.Input = CacheUsageFieldPolicy{
		Mode:                 CacheUsageFieldSampleMax,
		MaxTokens:            100,
		MoveDeltaToCacheRead: true,
		NormalMaxMultiplier:  1.1,
	}
	policy.Output = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleMax,
		MaxTokens:           20,
		NormalMaxMultiplier: 1.1,
	}
	policy.CacheRead = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleMax,
		MaxTokens:           50,
		NormalMaxMultiplier: 1.1,
	}
	policy.CacheCreation = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleMax,
		MaxTokens:           70,
		NormalMaxMultiplier: 1.1,
	}
	usage := &ClaudeUsage{
		InputTokens:              200,
		OutputTokens:             40,
		CacheReadInputTokens:     20,
		CacheCreationInputTokens: 80,
		CacheCreation5mTokens:    50,
		CacheCreation1hTokens:    30,
	}
	projectClaudeUsage(usage, nil, policy, 42)
	require.Equal(t, 100, usage.InputTokens)
	require.Equal(t, 20, usage.OutputTokens)
	require.Equal(t, 50, usage.CacheReadInputTokens)
	require.Equal(t, 70, usage.CacheCreationInputTokens)
	require.Equal(t, 50, usage.CacheCreation5mTokens)
	require.Equal(t, 20, usage.CacheCreation1hTokens)
}

func TestCacheUsagePolicySampleTargetIsDeterministic(t *testing.T) {
	policy := DefaultCacheUsagePolicy()
	policy.Output = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleTarget,
		TargetTokens:        100,
		NormalMaxMultiplier: 1.2,
	}
	a := &ClaudeUsage{InputTokens: 1, OutputTokens: 1000}
	b := &ClaudeUsage{InputTokens: 1, OutputTokens: 1000}
	projectClaudeUsage(a, nil, policy, 99)
	projectClaudeUsage(b, nil, policy, 99)
	require.Equal(t, a.OutputTokens, b.OutputTokens)
	require.GreaterOrEqual(t, a.OutputTokens, 1)
	require.LessOrEqual(t, a.OutputTokens, 1000)
}

func TestNormalizeCacheStrategyConfigUsageDefaultsAndValidation(t *testing.T) {
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.Usage.Input = CacheUsageFieldPolicy{Mode: CacheUsageFieldSampleMax}
	_, err := NormalizeCacheStrategyConfig(cfg)
	require.Error(t, err)

	cfg.Usage.Input = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleTarget,
		TargetTokens:        96,
		NormalMaxMultiplier: 1.1,
	}
	normalized, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, CacheUsageFieldSampleTarget, normalized.Usage.Input.Mode)
	require.Equal(t, 96, normalized.Usage.Input.TargetTokens)
}

func TestBoundCacheStrategySeparatesModelAndSessionScopes(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900004)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "scope isolation", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9008, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9009, Platform: PlatformAnthropic}
	svc := &GatewayService{}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"session-a"},"system":"stable","messages":[{"role":"user","content":"long stable prompt"}]}`)

	first := svc.buildCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2000)
	require.NotNil(t, first)
	require.Zero(t, first.CacheReadInputTokens)
	require.Greater(t, first.CacheCreationInputTokens, 0)

	sameSession := svc.buildCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2000)
	require.NotNil(t, sameSession)
	require.Greater(t, sameSession.CacheReadInputTokens, 0)

	otherSessionBody := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"session-b"},"system":"stable","messages":[{"role":"user","content":"long stable prompt"}]}`)
	otherSession := svc.buildCacheEmulationUsage(context.Background(), account, group, otherSessionBody, "claude-sonnet-4-6", 2000)
	require.NotNil(t, otherSession)
	require.Zero(t, otherSession.CacheReadInputTokens)
	require.Greater(t, otherSession.CacheCreationInputTokens, 0)

	otherModel := svc.buildCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-5", 2000)
	require.NotNil(t, otherModel)
	require.Zero(t, otherModel.CacheReadInputTokens)
	require.Greater(t, otherModel.CacheCreationInputTokens, 0)
}

func TestBoundCacheStrategyGroupSessionSharesCacheAcrossAccounts(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900006)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.ScopeMode = CacheScopeModeGroupSession
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "group session scope", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9012, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	body := cacheRequestBody("group session shared cache", false)
	svc := &GatewayService{}

	first := svc.buildCacheEmulationUsage(
		context.Background(),
		&Account{ID: 9013, Platform: PlatformAnthropic},
		group,
		body,
		"claude-sonnet-4-6",
		2000,
	)
	require.NotNil(t, first)
	require.Zero(t, first.CacheReadInputTokens)
	require.Greater(t, first.CacheCreationInputTokens, 0)

	sharedAcrossAccount := svc.buildCacheEmulationUsage(
		context.Background(),
		&Account{ID: 9014, Platform: PlatformAnthropic},
		group,
		body,
		"claude-sonnet-4-6",
		2000,
	)
	require.NotNil(t, sharedAcrossAccount)
	require.Greater(t, sharedAcrossAccount.CacheReadInputTokens, 0)
	require.Zero(t, sharedAcrossAccount.CacheCreationInputTokens)
}

func TestCreationCapAlsoLimitsCommittedBreakpoints(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900005)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.MaxNewCreationTokensPerRequest = 2500
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "creation cap", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9010, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9011, Platform: PlatformAnthropic}
	svc := &GatewayService{}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"creation-cap-session"},"system":"stable system","messages":[{"role":"user","content":[{"type":"text","text":"first historical turn","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":"second historical turn","cache_control":{"type":"ephemeral"}}]}]}`)

	first := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 4000)
	require.NotNil(t, first)
	require.NotNil(t, first.result())
	require.LessOrEqual(t, first.result().CacheCreationInputTokens, 2500)
	first.commit()

	second := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 4000)
	require.NotNil(t, second)
	require.NotNil(t, second.result())
	require.LessOrEqual(t, second.result().CacheReadInputTokens, 2500)
}

func TestCreationCapBelowCompleteBreakpointDoesNotFabricateUsageOrState(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900007)
	cfg := smallPayloadCacheConfig(CacheStrategyKindToolAware)
	cfg.MinCacheableTokens = 1
	cfg.MaxNewCreationTokensPerRequest = 90
	cfg.Usage.Input = CacheUsageFieldPolicy{
		Mode:                 CacheUsageFieldSampleMax,
		MaxTokens:            96,
		NormalMaxMultiplier:  1.1,
		MoveDeltaToCacheRead: true,
	}
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "creation cap below breakpoint", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9015, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9016, Platform: PlatformAnthropic}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"creation-cap-below-breakpoint"},"system":[{"type":"text","text":"Stable tool instructions","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"lookup","description":"Stable lookup tool","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"Use lookup tool","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"lookup","input":{"q":"stable"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"stable tool result"}]}]}`)

	plan := (&GatewayService{}).prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 310)
	require.NotNil(t, plan)
	require.Nil(t, plan.result(), "a cap below the first complete breakpoint must suppress creation")

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	setCachePlanContext(ctx, plan)
	usage := &ClaudeUsage{InputTokens: 310, OutputTokens: 18}
	mergeAndCommitCachePlan(ctx, usage, true)
	require.Equal(t, 310, usage.InputTokens, "without cache evidence input must remain uncached")
	require.Zero(t, usage.CacheReadInputTokens)
	require.Zero(t, usage.CacheCreationInputTokens)

	next := (&GatewayService{}).prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 310)
	require.NotNil(t, next)
	require.Nil(t, next.result(), "suppressed creation must not leave a hidden readable entry")
}

func TestAutoBreakpointsExcludeCurrentUserWithoutExplicitClientBreakpoint(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900010)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.BreakpointMode = CacheBreakpointAuto
	cfg.CacheCurrentUserStablePrefix = false
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "auto excludes current user", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9021, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9022, Platform: PlatformAnthropic}
	svc := &GatewayService{}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"auto-current-user"},"system":"stable system","messages":[{"role":"user","content":"stable history"},{"role":"assistant","content":"stable answer"},{"role":"user","content":"current dynamic request"}]}`)

	first := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2400)
	require.NotNil(t, first)
	require.NotNil(t, first.result())
	require.Greater(t, first.result().CacheCreationInputTokens, 0)
	first.commit()

	second := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2400)
	require.NotNil(t, second)
	require.NotNil(t, second.result())
	require.Greater(t, second.result().CacheReadInputTokens, 0)
	require.Less(t, second.result().CacheReadInputTokens, 2400,
		"the current user turn must not be part of the automatically cached prefix")
}

func TestExplicitClientBreakpointCanIncludeCurrentUser(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900011)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.BreakpointMode = CacheBreakpointHybrid
	cfg.CacheCurrentUserStablePrefix = false
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "explicit current user breakpoint", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9023, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9024, Platform: PlatformAnthropic}
	svc := &GatewayService{}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"explicit-current-user"},"system":"stable system","messages":[{"role":"user","content":[{"type":"text","text":"explicitly cache this request","cache_control":{"type":"ephemeral"}}]}]}`)

	first := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 1200)
	require.NotNil(t, first)
	require.NotNil(t, first.result())
	require.Greater(t, first.result().CacheCreationInputTokens, 0)
	first.commit()

	second := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 1200)
	require.NotNil(t, second)
	require.NotNil(t, second.result())
	require.Greater(t, second.result().CacheReadInputTokens, 0)
}

func TestZeroProjectedCacheUsageDoesNotCommitHiddenProfile(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900008)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.RatioMode = CacheRatioModeIndependent
	cfg.ReadRatio = 0
	cfg.CreationRatio = 0
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "zero projected cache", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9017, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9018, Platform: PlatformAnthropic}
	body := cacheRequestBody("zero-projected-cache", false)
	svc := &GatewayService{}

	first := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2000)
	require.NotNil(t, first)
	require.Nil(t, first.result(), "zero read/create ratios must produce no reportable cache usage")
	first.commit()

	second := svc.prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2000)
	require.NotNil(t, second)
	require.Nil(t, second.result(), "a zero-usage plan must not become readable on the next request")
}

func TestReportedInputCapJitterIsDeterministicAndBounded(t *testing.T) {
	first := applyReportedInputJitter(100, 3, 7, 42, 10)
	retry := applyReportedInputJitter(100, 3, 7, 42, 10)
	require.Equal(t, first, retry, "same normalized profile must produce stable jitter")
	require.GreaterOrEqual(t, first, 90)
	require.LessOrEqual(t, first, 97)

	differentProfile := applyReportedInputJitter(100, 3, 7, 43, 10)
	require.NotEqual(t, first, differentProfile, "different profiles should be allowed to vary")
	require.GreaterOrEqual(t, differentProfile, 90)
	require.LessOrEqual(t, differentProfile, 97)

	require.Equal(t, 100, applyReportedInputJitter(100, 3, 7, 42, 100), "lower bound prevents underflow")
}

func TestReportedInputMinimumRemainsUncachedWhenFeasible(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900009)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.ReportedInputMinTokens = 64
	cfg.ReportedInputMaxTokens = 120
	cfg.TokenScale = 1.5
	cfg.ScaleMinInputTokens = 1
	cfg.MaxSimulatedInputTokens = 128
	cfg.CapJitterMinTokens = 5
	cfg.CapJitterMaxTokens = 9
	cfg.Usage.Input = CacheUsageFieldPolicy{
		Mode:                 CacheUsageFieldSampleMax,
		MaxTokens:            70,
		NormalMaxMultiplier:  1.1,
		MoveDeltaToCacheRead: true,
	}
	cfg.Usage.CacheCreation = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleTarget,
		TargetTokens:        24,
		NormalMaxMultiplier: 1.25,
	}
	cfg.Usage.CacheRead = CacheUsageFieldPolicy{
		Mode:                CacheUsageFieldSampleTarget,
		TargetTokens:        30,
		NormalMaxMultiplier: 1.2,
	}
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "reported input minimum", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9019, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9020, Platform: PlatformAnthropic}
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"reported-input-minimum"},"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("stable prompt ", 120) + `","cache_control":{"type":"ephemeral"}}]}]}`)
	plan := (&GatewayService{}).prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 400)
	require.NotNil(t, plan)
	require.NotNil(t, plan.result())
	t.Logf("reported minimum plan: input=%d read=%d creation=%d", plan.result().InputTokens, plan.result().CacheReadInputTokens, plan.result().CacheCreationInputTokens)
	require.GreaterOrEqual(t, plan.result().InputTokens, cfg.ReportedInputMinTokens)
	require.LessOrEqual(t, plan.result().InputTokens+plan.result().CacheReadInputTokens+plan.result().CacheCreationInputTokens, cfg.ReportedInputMaxTokens)
}

func TestReportedInputCapAppliesWhenNoCacheBucketIsReportable(t *testing.T) {
	resetCacheTracker()
	strategyID := int64(900010)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.RatioMode = CacheRatioModeIndependent
	cfg.ReadRatio = 0
	cfg.CreationRatio = 0
	cfg.ReportedInputMinTokens = 120
	cfg.ReportedInputMaxTokens = 180
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID: strategyID, Name: "raw input cap without cache", Enabled: true, Revision: 1, Config: cfg,
	})
	defer GlobalCacheStrategyRegistry().Delete(strategyID)

	group := &Group{ID: 9024, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
	account := &Account{ID: 9025, Platform: PlatformAnthropic}
	body := cacheRequestBody("raw-input-cap-without-cache", false)
	plan := (&GatewayService{}).prepareCacheEmulationUsage(context.Background(), account, group, body, "claude-sonnet-4-6", 2000)
	require.NotNil(t, plan)
	require.Nil(t, plan.result())

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	setCachePlanContext(ctx, plan)
	usage := &ClaudeUsage{InputTokens: 2000, OutputTokens: 10}
	mergeAndCommitCachePlan(ctx, usage, true)
	require.GreaterOrEqual(t, usage.InputTokens, cfg.ReportedInputMinTokens)
	require.LessOrEqual(t, usage.InputTokens, cfg.ReportedInputMaxTokens)
	require.Zero(t, usage.CacheReadInputTokens)
	require.Zero(t, usage.CacheCreationInputTokens)
}

func TestNormalizeCacheStrategyConfigAcceptsToolAwareKind(t *testing.T) {
	cfg := smallPayloadCacheConfig(CacheStrategyKindToolAware)
	normalized, err := NormalizeCacheStrategyConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, CacheStrategyKindToolAware, normalized.Kind)
}

func cacheStrategyPtrInt64(v int64) *int64 { return &v }

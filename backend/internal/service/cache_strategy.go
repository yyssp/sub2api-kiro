package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	CacheStrategyKindDisabled         = "disabled"
	CacheStrategyKindPrefix           = "prefix"
	CacheStrategyKindToolAware        = "tool_aware"
	CacheRatioModeUniform             = "uniform"
	CacheRatioModeIndependent         = "independent"
	CacheBreakpointClientOnly         = "client_only"
	CacheBreakpointAuto               = "auto"
	CacheBreakpointHybrid             = "hybrid"
	CacheScopeModeGroupAccountSession = "group_account_session"
	CacheScopeModeGroupSession        = "group_session"
	CacheDynamicContentExclude        = "exclude"
	CacheDynamicContentAllow          = "allow"
)

var (
	// ErrCacheStrategyGroupConflict is returned when a group that is being
	// bound already owns a cache strategy. A group must be explicitly
	// unbound before it can be attached to another strategy, and repeated
	// binding to the same strategy is rejected as well so accidental
	// overwrites are visible to administrators.
	ErrCacheStrategyGroupConflict = infraerrors.Conflict(
		"CACHE_STRATEGY_GROUP_CONFLICT",
		"group is already bound to a cache strategy",
	)
)

// CacheUsageFieldMode controls how one usage bucket is projected to the
// downstream Claude Code compatible response.
type CacheUsageFieldMode string

const (
	CacheUsageFieldRaw          CacheUsageFieldMode = "raw"
	CacheUsageFieldPreserve     CacheUsageFieldMode = "preserve"
	CacheUsageFieldSampleMax    CacheUsageFieldMode = "sample_max"
	CacheUsageFieldSampleTarget CacheUsageFieldMode = "sample_target"
)

type CacheUsageFieldPolicy struct {
	Mode                 CacheUsageFieldMode `json:"mode"`
	MaxTokens            int                 `json:"max_tokens"`
	TargetTokens         int                 `json:"target_tokens"`
	NormalMaxMultiplier  float64             `json:"normal_max_multiplier"`
	MoveDeltaToCacheRead bool                `json:"move_delta_to_cache_read"`
}

// CacheUsagePolicy mirrors the four independent usage controls from
// 2ue_kiro.rs without carrying any path/Kiro-specific semantics.
type CacheUsagePolicy struct {
	Enabled                     bool                  `json:"enabled"`
	PreserveUpstreamCacheUsage  bool                  `json:"preserve_upstream_cache_usage"`
	Input                       CacheUsageFieldPolicy `json:"input"`
	Output                      CacheUsageFieldPolicy `json:"output"`
	CacheRead                   CacheUsageFieldPolicy `json:"cache_read"`
	CacheCreation               CacheUsageFieldPolicy `json:"cache_creation"`
	FinalCacheReadMaxTokens     int                   `json:"final_cache_read_max_tokens"`
	FinalCacheCreationMaxTokens int                   `json:"final_cache_creation_max_tokens"`
	OutputUpliftMinTokens       int                   `json:"output_uplift_min_tokens"`
	OutputUpliftPercent         int                   `json:"output_uplift_percent"`
	FinalOutputMaxTokens        int                   `json:"final_output_max_tokens"`
}

func defaultCacheUsageFieldPolicy(mode CacheUsageFieldMode) CacheUsageFieldPolicy {
	return CacheUsageFieldPolicy{Mode: mode, NormalMaxMultiplier: 1.1}
}

func DefaultCacheUsagePolicy() CacheUsagePolicy {
	return CacheUsagePolicy{
		Enabled:                    true,
		PreserveUpstreamCacheUsage: true,
		Input:                      defaultCacheUsageFieldPolicy(CacheUsageFieldRaw),
		Output:                     defaultCacheUsageFieldPolicy(CacheUsageFieldRaw),
		CacheRead:                  defaultCacheUsageFieldPolicy(CacheUsageFieldPreserve),
		CacheCreation:              defaultCacheUsageFieldPolicy(CacheUsageFieldPreserve),
	}
}

type CacheStrategyConfig struct {
	Kind                             string               `json:"kind"`
	RatioMode                        string               `json:"ratio_mode"`
	CoverageRatio                    float64              `json:"coverage_ratio"`
	UsageRatio                       float64              `json:"usage_ratio"`
	ReadRatio                        float64              `json:"read_ratio"`
	CreationRatio                    float64              `json:"creation_ratio"`
	CacheSystem                      bool                 `json:"cache_system"`
	CacheTools                       bool                 `json:"cache_tools"`
	CacheHistory                     bool                 `json:"cache_history"`
	CacheToolResults                 bool                 `json:"cache_tool_results"`
	CacheCurrentUserStablePrefix     bool                 `json:"cache_current_user_stable_prefix"`
	CurrentUserStablePrefixMaxTokens int                  `json:"current_user_stable_prefix_max_tokens"`
	BreakpointMode                   string               `json:"breakpoint_mode"`
	AllowDerivedSession              bool                 `json:"allow_derived_session"`
	DynamicContentMode               string               `json:"dynamic_content_mode"`
	ScopeMode                        string               `json:"scope_mode"`
	MaxCoverageTokens                int                  `json:"max_coverage_tokens"`
	MaxNewCreationTokensPerRequest   int                  `json:"max_new_creation_tokens_per_request"`
	IncrementalCreateEnabled         bool                 `json:"incremental_create_enabled"`
	MinCacheableTokens               int                  `json:"min_cacheable_tokens"`
	ModelMinCacheableOverrides       map[string]int       `json:"model_min_cacheable_overrides,omitempty"`
	ReportedInputMinTokens           int                  `json:"reported_input_min_tokens"`
	ReportedInputMaxTokens           int                  `json:"reported_input_max_tokens"`
	TokenScale                       float64              `json:"token_scale"`
	ScaleMinInputTokens              int                  `json:"scale_min_input_tokens"`
	MaxSimulatedInputTokens          int                  `json:"max_simulated_input_tokens"`
	DefaultTTLSeconds                int                  `json:"default_ttl_seconds"`
	HourTTLSeconds                   int                  `json:"hour_ttl_seconds"`
	MaxEntriesPerScope               int                  `json:"max_entries_per_scope"`
	MaxEntriesGlobal                 int                  `json:"max_entries_global"`
	EstimatedBytesLimit              int64                `json:"estimated_bytes_limit"`
	ExpireAfterIdleSeconds           int                  `json:"expire_after_idle_seconds"`
	CapJitterMinTokens               int                  `json:"cap_jitter_min_tokens"`
	CapJitterMaxTokens               int                  `json:"cap_jitter_max_tokens"`
	PreserveUpstreamCacheUsage       bool                 `json:"preserve_upstream_cache_usage"`
	CreationControl                  CacheCreationControl `json:"creation_control"`
	Usage                            CacheUsagePolicy     `json:"usage"`
}

type CacheCreationControl struct {
	Enabled                      bool `json:"enabled"`
	MinCreationDeltaTokens       int  `json:"min_creation_delta_tokens"`
	MinSuccessfulRequestsBetween int  `json:"min_successful_requests_between"`
	MinCreationIntervalSeconds   int  `json:"min_creation_interval_seconds"`
	MaxCreationTokensPerEvent    int  `json:"max_creation_tokens_per_event"`
	CreationBudgetWindowSeconds  int  `json:"creation_budget_window_seconds"`
	MaxCreationTokensPerWindow   int  `json:"max_creation_tokens_per_window"`
}

func DefaultCacheStrategyConfig(kind string) CacheStrategyConfig {
	if kind == "" {
		kind = CacheStrategyKindPrefix
	}
	c := CacheStrategyConfig{
		Kind: kind, RatioMode: CacheRatioModeUniform, CoverageRatio: 0.85, UsageRatio: 1,
		ReadRatio: 1, CreationRatio: 1, CacheSystem: true, CacheTools: true,
		CacheHistory: true, CacheToolResults: true, BreakpointMode: CacheBreakpointHybrid,
		CacheCurrentUserStablePrefix: false, CurrentUserStablePrefixMaxTokens: 0,
		DynamicContentMode: CacheDynamicContentExclude, ScopeMode: CacheScopeModeGroupAccountSession,
		PreserveUpstreamCacheUsage: true,
		IncrementalCreateEnabled:   true, MinCacheableTokens: 1024, TokenScale: 1,
		ScaleMinInputTokens: 20000, DefaultTTLSeconds: 300, HourTTLSeconds: 3600, MaxEntriesPerScope: 128,
		MaxEntriesGlobal: 10000, EstimatedBytesLimit: 64 << 20,
		Usage: DefaultCacheUsagePolicy(),
	}
	if kind == CacheStrategyKindDisabled {
		c.CacheSystem, c.CacheTools, c.CacheHistory, c.CacheToolResults = false, false, false, false
		c.CoverageRatio, c.UsageRatio, c.ReadRatio, c.CreationRatio = 0, 0, 0, 0
		c.IncrementalCreateEnabled = false
		c.Usage = DefaultCacheUsagePolicy()
		c.Usage.Enabled = false
		c.PreserveUpstreamCacheUsage = false
	}
	return c
}

func NormalizeCacheStrategyConfig(in CacheStrategyConfig) (CacheStrategyConfig, error) {
	if in.Kind == "" {
		in.Kind = CacheStrategyKindPrefix
	}
	switch in.Kind {
	case CacheStrategyKindDisabled, CacheStrategyKindPrefix, CacheStrategyKindToolAware:
	default:
		return in, fmt.Errorf("config.kind must be disabled, prefix or tool_aware")
	}
	if in.RatioMode == "" {
		in.RatioMode = CacheRatioModeUniform
	}
	if in.RatioMode != CacheRatioModeUniform && in.RatioMode != CacheRatioModeIndependent {
		return in, fmt.Errorf("config.ratio_mode must be uniform or independent")
	}
	if in.BreakpointMode == "" {
		in.BreakpointMode = CacheBreakpointHybrid
	}
	switch in.BreakpointMode {
	case CacheBreakpointClientOnly, CacheBreakpointAuto, CacheBreakpointHybrid:
	default:
		return in, fmt.Errorf("config.breakpoint_mode must be client_only, auto or hybrid")
	}
	if in.DynamicContentMode == "" {
		in.DynamicContentMode = CacheDynamicContentExclude
	}
	if in.DynamicContentMode != CacheDynamicContentExclude && in.DynamicContentMode != CacheDynamicContentAllow {
		return in, fmt.Errorf("config.dynamic_content_mode must be exclude or allow")
	}
	if in.ScopeMode == "" {
		in.ScopeMode = CacheScopeModeGroupAccountSession
	}
	if in.ScopeMode != CacheScopeModeGroupAccountSession && in.ScopeMode != CacheScopeModeGroupSession {
		return in, fmt.Errorf("config.scope_mode must be group_account_session or group_session")
	}
	for name, value := range map[string]float64{
		"coverage_ratio": in.CoverageRatio, "usage_ratio": in.UsageRatio,
		"read_ratio": in.ReadRatio, "creation_ratio": in.CreationRatio,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return in, fmt.Errorf("config.%s must be between 0 and 1", name)
		}
	}
	if in.TokenScale < 1 || in.TokenScale > 3 || math.IsNaN(in.TokenScale) || math.IsInf(in.TokenScale, 0) {
		return in, fmt.Errorf("config.token_scale must be between 1 and 3")
	}
	if in.ReportedInputMinTokens < 0 || in.ReportedInputMaxTokens < 0 ||
		(in.ReportedInputMaxTokens > 0 && in.ReportedInputMinTokens > in.ReportedInputMaxTokens) {
		return in, errors.New("config.reported_input_min_tokens must be <= reported_input_max_tokens")
	}
	if in.DefaultTTLSeconds <= 0 || in.DefaultTTLSeconds > 3600 || in.HourTTLSeconds <= 0 || in.HourTTLSeconds > 3600 {
		return in, errors.New("config TTL must be between 1 and 3600 seconds")
	}
	if in.MinCacheableTokens < 0 || in.MaxCoverageTokens < 0 || in.MaxNewCreationTokensPerRequest < 0 {
		return in, errors.New("config token limits must be non-negative")
	}
	if in.ScaleMinInputTokens < 0 || in.MaxSimulatedInputTokens < 0 || in.CapJitterMinTokens < 0 || in.CapJitterMaxTokens < 0 {
		return in, errors.New("config token projection limits must be non-negative")
	}
	if in.CapJitterMinTokens > in.CapJitterMaxTokens && in.CapJitterMaxTokens > 0 {
		return in, errors.New("config.cap_jitter_min_tokens must be <= cap_jitter_max_tokens")
	}
	if in.HourTTLSeconds < in.DefaultTTLSeconds {
		return in, errors.New("config.hour_ttl_seconds must be >= default_ttl_seconds")
	}
	if in.CreationControl.MinCreationDeltaTokens < 0 ||
		in.CreationControl.MinSuccessfulRequestsBetween < 0 ||
		in.CreationControl.MinCreationIntervalSeconds < 0 ||
		in.CreationControl.MaxCreationTokensPerEvent < 0 ||
		in.CreationControl.CreationBudgetWindowSeconds < 0 ||
		in.CreationControl.MaxCreationTokensPerWindow < 0 {
		return in, errors.New("config.creation_control values must be non-negative")
	}
	in.Usage = normalizeCacheUsagePolicy(in.Usage)
	in.Usage.PreserveUpstreamCacheUsage = in.PreserveUpstreamCacheUsage
	if err := validateCacheUsagePolicy(in.Usage); err != nil {
		return in, err
	}
	if in.CacheCurrentUserStablePrefix == false {
		in.CurrentUserStablePrefixMaxTokens = 0
	}
	if in.Kind == CacheStrategyKindDisabled {
		in = DefaultCacheStrategyConfig(CacheStrategyKindDisabled)
	}
	if in.RatioMode == CacheRatioModeUniform {
		in.ReadRatio, in.CreationRatio = in.UsageRatio, in.UsageRatio
	}
	if in.ModelMinCacheableOverrides == nil {
		in.ModelMinCacheableOverrides = map[string]int{}
	}
	return in, nil
}

func normalizeCacheUsagePolicy(in CacheUsagePolicy) CacheUsagePolicy {
	if in.Input.Mode == "" {
		in.Input.Mode = CacheUsageFieldRaw
	}
	if in.Output.Mode == "" {
		in.Output.Mode = CacheUsageFieldRaw
	}
	if in.CacheRead.Mode == "" {
		in.CacheRead.Mode = CacheUsageFieldPreserve
	}
	if in.CacheCreation.Mode == "" {
		in.CacheCreation.Mode = CacheUsageFieldPreserve
	}
	for _, p := range []*CacheUsageFieldPolicy{&in.Input, &in.Output, &in.CacheRead, &in.CacheCreation} {
		if p.NormalMaxMultiplier <= 0 || math.IsNaN(p.NormalMaxMultiplier) || math.IsInf(p.NormalMaxMultiplier, 0) {
			p.NormalMaxMultiplier = 1.1
		}
		if p.MaxTokens < 0 {
			p.MaxTokens = 0
		}
		if p.TargetTokens < 0 {
			p.TargetTokens = 0
		}
	}
	if in.FinalCacheReadMaxTokens < 0 {
		in.FinalCacheReadMaxTokens = 0
	}
	if in.FinalCacheCreationMaxTokens < 0 {
		in.FinalCacheCreationMaxTokens = 0
	}
	if in.OutputUpliftMinTokens < 0 {
		in.OutputUpliftMinTokens = 0
	}
	if in.OutputUpliftPercent < 0 {
		in.OutputUpliftPercent = 0
	}
	if in.OutputUpliftPercent > 200 {
		in.OutputUpliftPercent = 200
	}
	if in.FinalOutputMaxTokens < 0 {
		in.FinalOutputMaxTokens = 0
	}
	return in
}

func validateCacheUsagePolicy(in CacheUsagePolicy) error {
	if !in.Enabled {
		return nil
	}
	for name, p := range map[string]CacheUsageFieldPolicy{
		"input": in.Input, "output": in.Output,
		"cache_read": in.CacheRead, "cache_creation": in.CacheCreation,
	} {
		switch p.Mode {
		case CacheUsageFieldRaw, CacheUsageFieldPreserve:
			if math.IsNaN(p.NormalMaxMultiplier) || math.IsInf(p.NormalMaxMultiplier, 0) || p.NormalMaxMultiplier < 1 {
				return fmt.Errorf("config.usage.%s.normal_max_multiplier must be at least 1", name)
			}
		case CacheUsageFieldSampleMax:
			if p.MaxTokens <= 0 {
				return fmt.Errorf("config.usage.%s.max_tokens must be greater than 0 for sample_max", name)
			}
		case CacheUsageFieldSampleTarget:
			if p.TargetTokens <= 0 {
				return fmt.Errorf("config.usage.%s.target_tokens must be greater than 0 for sample_target", name)
			}
			if p.NormalMaxMultiplier < 1 || math.IsNaN(p.NormalMaxMultiplier) || math.IsInf(p.NormalMaxMultiplier, 0) {
				return fmt.Errorf("config.usage.%s.normal_max_multiplier must be at least 1", name)
			}
		default:
			return fmt.Errorf("config.usage.%s.mode is invalid", name)
		}
	}
	return nil
}

func CacheStrategyConfigMap(c CacheStrategyConfig) map[string]any {
	b, _ := json.Marshal(c)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func ParseCacheStrategyConfig(raw map[string]any) (CacheStrategyConfig, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return CacheStrategyConfig{}, err
	}
	c := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	if err := json.Unmarshal(b, &c); err != nil {
		return CacheStrategyConfig{}, err
	}
	return NormalizeCacheStrategyConfig(c)
}

type CacheStrategy struct {
	ID          int64               `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Enabled     bool                `json:"enabled"`
	Revision    int64               `json:"revision"`
	Config      CacheStrategyConfig `json:"config"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

type CacheStrategyRepository interface {
	Create(context.Context, *CacheStrategy) error
	GetByID(context.Context, int64) (*CacheStrategy, error)
	List(context.Context, string) ([]CacheStrategy, error)
	Update(context.Context, *CacheStrategy) error
	Delete(context.Context, int64) error
	CountBoundGroups(context.Context, int64) (int, error)
	ListBoundGroups(context.Context, int64) ([]Group, error)
	GetGroupBindings(context.Context, []int64) ([]Group, error)
	SetGroupBindings(context.Context, int64, []int64) error
	ReplaceGroupBindings(context.Context, int64, []int64) error
}

type cacheStrategyCASRepository interface {
	UpdateIfRevision(context.Context, *CacheStrategy, int64) (bool, error)
}

type CacheStrategyService struct {
	repo                 CacheStrategyRepository
	registry             *CacheStrategyRegistry
	authCacheInvalidator APIKeyAuthCacheInvalidator
}

func NewCacheStrategyService(repo CacheStrategyRepository, invalidator ...APIKeyAuthCacheInvalidator) *CacheStrategyService {
	svc := &CacheStrategyService{repo: repo, registry: globalCacheStrategyRegistry}
	if len(invalidator) > 0 {
		svc.authCacheInvalidator = invalidator[0]
	}
	return svc
}

func (s *CacheStrategyService) Create(ctx context.Context, strategy *CacheStrategy) (*CacheStrategy, error) {
	if strategy == nil {
		return nil, errors.New("strategy is nil")
	}
	c, err := NormalizeCacheStrategyConfig(strategy.Config)
	if err != nil {
		return nil, err
	}
	strategy.Config, strategy.Revision = c, 1
	if err := s.repo.Create(ctx, strategy); err != nil {
		return nil, err
	}
	s.registry.Put(strategy)
	return strategy, nil
}

func (s *CacheStrategyService) GetByID(ctx context.Context, id int64) (*CacheStrategy, error) {
	strategy, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if strategy != nil {
		s.registry.Put(strategy)
	}
	return strategy, nil
}

func (s *CacheStrategyService) List(ctx context.Context, search string) ([]CacheStrategy, error) {
	strategies, err := s.repo.List(ctx, search)
	if err != nil {
		return nil, err
	}
	for i := range strategies {
		s.registry.Put(&strategies[i])
	}
	return strategies, nil
}

func (s *CacheStrategyService) Update(ctx context.Context, strategy *CacheStrategy, expectedRevision int64) (*CacheStrategy, error) {
	if strategy == nil {
		return nil, errors.New("strategy is nil")
	}
	if expectedRevision > 0 && strategy.Revision != expectedRevision {
		return nil, fmt.Errorf("cache strategy revision conflict")
	}
	c, err := NormalizeCacheStrategyConfig(strategy.Config)
	if err != nil {
		return nil, err
	}
	strategy.Config, strategy.Revision = c, strategy.Revision+1
	if casRepo, ok := s.repo.(cacheStrategyCASRepository); ok && expectedRevision > 0 {
		updated, err := casRepo.UpdateIfRevision(ctx, strategy, expectedRevision)
		if err != nil {
			return nil, err
		}
		if !updated {
			return nil, fmt.Errorf("cache strategy revision conflict")
		}
	} else if err := s.repo.Update(ctx, strategy); err != nil {
		return nil, err
	}
	s.registry.Put(strategy)
	return strategy, nil
}

func (s *CacheStrategyService) Delete(ctx context.Context, id int64) error {
	count, err := s.repo.CountBoundGroups(ctx, id)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("cache strategy is still bound to %d group(s)", count)
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.registry.Delete(id)
	return nil
}

func (s *CacheStrategyService) SetEnabled(ctx context.Context, id int64, enabled bool) (*CacheStrategy, error) {
	strategy, err := s.repo.GetByID(ctx, id)
	if err != nil || strategy == nil {
		return strategy, err
	}
	strategy.Enabled = enabled
	strategy.Revision++
	if err := s.repo.Update(ctx, strategy); err != nil {
		return nil, err
	}
	s.registry.Put(strategy)
	return strategy, nil
}

func (s *CacheStrategyService) BindGroups(ctx context.Context, id int64, groupIDs []int64) error {
	// Perform a friendly preflight before entering the repository transaction.
	// The repository repeats this check under row locks so concurrent bind
	// requests cannot bypass the one-strategy-per-group invariant. Reapplying
	// a strategy to an already-bound group is rejected as an accidental
	// duplicate operation.
	groups, err := s.repo.GetGroupBindings(ctx, groupIDs)
	if err != nil {
		return err
	}
	byID := make(map[int64]Group, len(groups))
	for i := range groups {
		byID[groups[i].ID] = groups[i]
	}
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, groupID := range groupIDs {
		if groupID <= 0 {
			continue
		}
		if _, ok := seen[groupID]; ok {
			continue
		}
		seen[groupID] = struct{}{}
		if group, ok := byID[groupID]; ok && group.CacheStrategyID != nil && *group.CacheStrategyID > 0 {
			return cacheStrategyGroupConflict(group, id)
		}
	}

	if s.authCacheInvalidator != nil {
		oldGroups, err := s.repo.ListBoundGroups(ctx, id)
		if err != nil {
			return err
		}
		if err := s.repo.SetGroupBindings(ctx, id, groupIDs); err != nil {
			return err
		}
		s.invalidateAuthCacheForGroupIDs(ctx, groupIDsFromGroups(oldGroups), groupIDs)
		return nil
	}
	if err := s.repo.SetGroupBindings(ctx, id, groupIDs); err != nil {
		return err
	}
	return nil
}

// ReplaceGroups updates the complete desired binding set for an existing
// strategy. Groups already owned by this strategy remain valid; groups owned
// by another strategy still fail with the same structured conflict as the
// strict BindGroups operation. The repository performs the replacement
// atomically under row locks.
func (s *CacheStrategyService) ReplaceGroups(ctx context.Context, id int64, groupIDs []int64) error {
	var oldGroupIDs []int64
	if s.authCacheInvalidator != nil {
		oldGroups, err := s.repo.ListBoundGroups(ctx, id)
		if err != nil {
			return err
		}
		oldGroupIDs = groupIDsFromGroups(oldGroups)
	}
	groups, err := s.repo.GetGroupBindings(ctx, groupIDs)
	if err != nil {
		return err
	}
	byID := make(map[int64]Group, len(groups))
	for i := range groups {
		byID[groups[i].ID] = groups[i]
	}
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, groupID := range groupIDs {
		if groupID <= 0 {
			continue
		}
		if _, ok := seen[groupID]; ok {
			continue
		}
		seen[groupID] = struct{}{}
		if group, ok := byID[groupID]; ok && group.CacheStrategyID != nil &&
			*group.CacheStrategyID > 0 && *group.CacheStrategyID != id {
			return cacheStrategyGroupConflict(group, id)
		}
	}
	if err := s.repo.ReplaceGroupBindings(ctx, id, groupIDs); err != nil {
		return err
	}
	if s.authCacheInvalidator == nil {
		return nil
	}
	// Replacement can remove bindings as well as add them. Invalidate both the
	// old and desired sets so auth/group snapshots cannot retain stale policy.
	s.invalidateAuthCacheForGroupIDs(ctx, oldGroupIDs, groupIDs)
	return nil
}

func cacheStrategyGroupConflict(group Group, requestedStrategyID int64) error {
	currentStrategyID := int64(0)
	if group.CacheStrategyID != nil {
		currentStrategyID = *group.CacheStrategyID
	}
	reason := "group is already bound to another cache strategy; unbind it before rebinding"
	if currentStrategyID == requestedStrategyID {
		reason = "group is already bound to this cache strategy; repeated binding is not allowed"
	}
	groupLabel := strings.TrimSpace(group.Name)
	if groupLabel == "" {
		groupLabel = "group " + strconv.FormatInt(group.ID, 10)
	}
	return ErrCacheStrategyGroupConflict.WithMetadata(map[string]string{
		"group_id":              strconv.FormatInt(group.ID, 10),
		"group_name":            groupLabel,
		"current_strategy_id":   strconv.FormatInt(currentStrategyID, 10),
		"requested_strategy_id": strconv.FormatInt(requestedStrategyID, 10),
		"reason":                reason,
	})
}

// CacheStrategyGroupConflictForRepository lets persistence adapters return the
// same structured conflict envelope after their row-lock validation.
func CacheStrategyGroupConflictForRepository(group Group, requestedStrategyID int64) error {
	return cacheStrategyGroupConflict(group, requestedStrategyID)
}

func groupIDsFromGroups(groups []Group) []int64 {
	if len(groups) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(groups))
	for i := range groups {
		if groups[i].ID > 0 {
			ids = append(ids, groups[i].ID)
		}
	}
	return ids
}

func (s *CacheStrategyService) invalidateAuthCacheForGroupIDs(ctx context.Context, groupLists ...[]int64) {
	if s == nil || s.authCacheInvalidator == nil {
		return
	}
	seen := make(map[int64]struct{})
	for _, list := range groupLists {
		for i := range list {
			groupID := list[i]
			if groupID <= 0 {
				continue
			}
			if _, ok := seen[groupID]; ok {
				continue
			}
			seen[groupID] = struct{}{}
			s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, groupID)
		}
	}
}

func (s *CacheStrategyService) BoundGroupCount(ctx context.Context, id int64) (int, error) {
	return s.repo.CountBoundGroups(ctx, id)
}

func (s *CacheStrategyService) ListBoundGroups(ctx context.Context, id int64) ([]Group, error) {
	return s.repo.ListBoundGroups(ctx, id)
}

type CacheStrategyRegistry struct {
	mu         sync.RWMutex
	items      map[int64]*CacheStrategy
	generation atomic.Uint64
}

func NewCacheStrategyRegistry() *CacheStrategyRegistry {
	return &CacheStrategyRegistry{items: make(map[int64]*CacheStrategy)}
}
func (r *CacheStrategyRegistry) Put(s *CacheStrategy) {
	if r == nil || s == nil {
		return
	}
	copy := *s
	copy.Config.ModelMinCacheableOverrides = cloneIntMap(s.Config.ModelMinCacheableOverrides)
	copy.Config.Usage = cloneCacheUsagePolicy(s.Config.Usage)
	r.mu.Lock()
	r.items[s.ID] = &copy
	r.mu.Unlock()
	r.generation.Add(1)
}
func (r *CacheStrategyRegistry) Get(id int64) *CacheStrategy {
	if r == nil || id <= 0 {
		return nil
	}
	r.mu.RLock()
	s := r.items[id]
	r.mu.RUnlock()
	if s == nil {
		return nil
	}
	copy := *s
	copy.Config.ModelMinCacheableOverrides = cloneIntMap(s.Config.ModelMinCacheableOverrides)
	copy.Config.Usage = cloneCacheUsagePolicy(s.Config.Usage)
	return &copy
}
func (r *CacheStrategyRegistry) Delete(id int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.items, id)
	r.mu.Unlock()
	r.generation.Add(1)
}
func (r *CacheStrategyRegistry) Generation() uint64 {
	if r == nil {
		return 0
	}
	return r.generation.Load()
}

var globalCacheStrategyRegistry = NewCacheStrategyRegistry()

func GlobalCacheStrategyRegistry() *CacheStrategyRegistry { return globalCacheStrategyRegistry }

func SortCacheStrategyIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func NormalizeCacheStrategyName(name string) string { return strings.TrimSpace(name) }

func cloneIntMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneCacheUsagePolicy(in CacheUsagePolicy) CacheUsagePolicy {
	in.Input = in.Input
	in.Output = in.Output
	in.CacheRead = in.CacheRead
	in.CacheCreation = in.CacheCreation
	return in
}

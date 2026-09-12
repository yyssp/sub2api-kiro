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
	Enabled                    bool `json:"enabled"`
	PreserveUpstreamCacheUsage bool `json:"preserve_upstream_cache_usage"`
	// SkipNonStreamUsageProjection 关掉非流式响应的用量投影：非流式拿得到完整的
	// 上游 usage，有些场景更希望原样透传而不是套一遍缓存整形。
	// 对齐 kiro.rs 的 skipNonStreamUsageProjection。
	SkipNonStreamUsageProjection bool                  `json:"skip_non_stream_usage_projection"`
	Input                        CacheUsageFieldPolicy `json:"input"`
	Output                       CacheUsageFieldPolicy `json:"output"`
	CacheRead                    CacheUsageFieldPolicy `json:"cache_read"`
	CacheCreation                CacheUsageFieldPolicy `json:"cache_creation"`
	// Final*MaxTokens 是上报值的最终硬上限。裸 min() 会让所有触顶记录落在同一个
	// 数字上（一列整齐的 700000），因此每个上限都配一对抖动区间：触顶时在
	// [jitter_min, jitter_max] 内按请求指纹回退一点，回退量最多为上限减一，
	// 确保触顶结果始终为正数。
	// 对齐 kiro.rs pathPolicy() 的 final*JitterMin/MaxTokens
	// （ui/src/lib/runtime-config-defaults.ts:92）。
	FinalCacheReadMaxTokens           int `json:"final_cache_read_max_tokens"`
	FinalCacheReadJitterMinTokens     int `json:"final_cache_read_jitter_min_tokens"`
	FinalCacheReadJitterMaxTokens     int `json:"final_cache_read_jitter_max_tokens"`
	FinalCacheCreationMaxTokens       int `json:"final_cache_creation_max_tokens"`
	FinalCacheCreationJitterMinTokens int `json:"final_cache_creation_jitter_min_tokens"`
	FinalCacheCreationJitterMaxTokens int `json:"final_cache_creation_jitter_max_tokens"`
	// OutputUpliftEnabled 是输出放大的独立开关。此前只能靠把阈值或比例填成 0
	// 来关闭，属于「用数值兼作开关」，关掉就得把配好的数字清空，再开又要重填。
	// 同样用指针：存量策略的 JSON 没有这个键，nil 在 normalize 里按「阈值与比例
	// 是否都为正」推断，保证既有行为不变。
	OutputUpliftEnabled   *bool `json:"output_uplift_enabled,omitempty"`
	OutputUpliftMinTokens int   `json:"output_uplift_min_tokens"`
	OutputUpliftPercent   int   `json:"output_uplift_percent"`
	// FinalOutputGuardEnabled 是输出上限那一组的总开关：关掉之后放大与最终上限
	// 都不生效，方便临时排查而不用把数值清零再填回来。对齐 kiro.rs 的
	// finalOutputGuardEnabled。
	//
	// 用指针而不是裸 bool：这个字段是后加的，库里存量策略的 JSON 根本没有它。
	// 裸 bool 反序列化会得到 false，等于把这些策略的输出上限静默关掉 —— 配置缺失
	// 必须保持原行为，所以 nil 在 normalize 里补成 true。
	FinalOutputGuardEnabled    *bool `json:"final_output_guard_enabled,omitempty"`
	FinalOutputMaxTokens       int   `json:"final_output_max_tokens"`
	FinalOutputJitterMinTokens int   `json:"final_output_jitter_min_tokens"`
	FinalOutputJitterMaxTokens int   `json:"final_output_jitter_max_tokens"`
}

// OutputGuardOn 读取输出上限总开关。未配置（nil）视为开启，保证存量策略与
// 没填这个字段的请求维持原行为。
func (p CacheUsagePolicy) OutputGuardOn() bool {
	return p.FinalOutputGuardEnabled == nil || *p.FinalOutputGuardEnabled
}

// OutputUpliftOn 读取输出放大开关。未配置（nil）时退回旧语义 ——
// 「阈值与比例都为正才放大」—— 这样存量策略的行为一字不变。
func (p CacheUsagePolicy) OutputUpliftOn() bool {
	if p.OutputUpliftEnabled == nil {
		return p.OutputUpliftMinTokens > 0 && p.OutputUpliftPercent > 0
	}
	return *p.OutputUpliftEnabled
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
		// 上限与扣减区间对齐 kiro.rs 的 pathPolicy()
		// （ui/src/lib/runtime-config-defaults.ts:84）。此前这里全留 0（不限制），
		// 等于把参考实现的护栏整组丢掉：上报值可以无限涨，也不会有触顶抖动。
		//
		// 三组上限统一都配波动区间：参考实现的读取上限两端留 0，靠更早阶段的
		// cap_jitter 打散 reported_input，但那样触顶记录仍会挤在同一个数字上，
		// 与「每个上限波动区间都要有值」的要求不符，这里统一给到 12345~45312。
		FinalCacheReadMaxTokens:           700000,
		FinalCacheReadJitterMinTokens:     12345,
		FinalCacheReadJitterMaxTokens:     45312,
		FinalCacheCreationMaxTokens:       400000,
		FinalCacheCreationJitterMinTokens: 12345,
		FinalCacheCreationJitterMaxTokens: 45312,
		OutputUpliftEnabled:               boolPtr(true),
		OutputUpliftMinTokens:             1000,
		OutputUpliftPercent:               50,
		FinalOutputGuardEnabled:           boolPtr(true),
		FinalOutputMaxTokens:              200000,
		FinalOutputJitterMinTokens:        12345,
		FinalOutputJitterMaxTokens:        45312,
	}
}

type CacheStrategyConfig struct {
	Kind                             string         `json:"kind"`
	RatioMode                        string         `json:"ratio_mode"`
	CoverageRatio                    float64        `json:"coverage_ratio"`
	UsageRatio                       float64        `json:"usage_ratio"`
	ReadRatio                        float64        `json:"read_ratio"`
	CreationRatio                    float64        `json:"creation_ratio"`
	CacheSystem                      bool           `json:"cache_system"`
	CacheTools                       bool           `json:"cache_tools"`
	CacheHistory                     bool           `json:"cache_history"`
	CacheToolResults                 bool           `json:"cache_tool_results"`
	CacheCurrentUserStablePrefix     bool           `json:"cache_current_user_stable_prefix"`
	CurrentUserStablePrefixMaxTokens int            `json:"current_user_stable_prefix_max_tokens"`
	BreakpointMode                   string         `json:"breakpoint_mode"`
	AllowDerivedSession              bool           `json:"allow_derived_session"`
	DynamicContentMode               string         `json:"dynamic_content_mode"`
	ScopeMode                        string         `json:"scope_mode"`
	MaxCoverageTokens                int            `json:"max_coverage_tokens"`
	MaxNewCreationTokensPerRequest   int            `json:"max_new_creation_tokens_per_request"`
	IncrementalCreateEnabled         bool           `json:"incremental_create_enabled"`
	MinCacheableTokens               int            `json:"min_cacheable_tokens"`
	ModelMinCacheableOverrides       map[string]int `json:"model_min_cacheable_overrides,omitempty"`
	ReportedInputMinTokens           int            `json:"reported_input_min_tokens"`
	ReportedInputMaxTokens           int            `json:"reported_input_max_tokens"`
	// UncachedInput* 约束的是上报 usage 里那份「未命中缓存」的 input_tokens，
	// 与 ReportedInput*（约束的是 input 总量）不是一回事。缓存把整个前缀吃光时
	// input 会掉到 0，而真实 API 不存在 input=0 且 cache_read>0 的组合，
	// 所以低于下限时要从 creation/read 里退还一部分。
	// 退还目标在 [min, max] 内按请求指纹抖动，避免每条都是同一个数字。
	// 注意：input 已经高于下限时不做任何处理 —— 不会把 input 反向塞进 creation，
	// 那会把便宜的 input 计成更贵的 creation。
	UncachedInputMinTokens     int                  `json:"uncached_input_min_tokens"`
	UncachedInputMaxTokens     int                  `json:"uncached_input_max_tokens"`
	TokenScale                 float64              `json:"token_scale"`
	ScaleMinInputTokens        int                  `json:"scale_min_input_tokens"`
	MaxSimulatedInputTokens    int                  `json:"max_simulated_input_tokens"`
	DefaultTTLSeconds          int                  `json:"default_ttl_seconds"`
	HourTTLSeconds             int                  `json:"hour_ttl_seconds"`
	MaxEntriesPerScope         int                  `json:"max_entries_per_scope"`
	MaxEntriesGlobal           int                  `json:"max_entries_global"`
	EstimatedBytesLimit        int64                `json:"estimated_bytes_limit"`
	ExpireAfterIdleSeconds     int                  `json:"expire_after_idle_seconds"`
	CapJitterMinTokens         int                  `json:"cap_jitter_min_tokens"`
	CapJitterMaxTokens         int                  `json:"cap_jitter_max_tokens"`
	PreserveUpstreamCacheUsage bool                 `json:"preserve_upstream_cache_usage"`
	CreationControl            CacheCreationControl `json:"creation_control"`
	Usage                      CacheUsagePolicy     `json:"usage"`
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
		// 作用域默认「分组 + 会话」：缓存按策略与会话隔离，同一会话换账号仍可命中。
		// 带上账号会让每次账号切换都重新建缓存，前缀白白重写一遍。
		DynamicContentMode: CacheDynamicContentExclude, ScopeMode: CacheScopeModeGroupSession,
		PreserveUpstreamCacheUsage: true,
		IncrementalCreateEnabled:   true, MinCacheableTokens: 1024, TokenScale: 2,
		// 缓存容量与生命周期对齐 kiro.rs 的页面默认值
		// （ui/src/lib/runtime-config-defaults.ts:516）：条目 TTL 86400 秒、
		// 单作用域 200 条、全局 20000 条、估算字节上限 256MB。
		// 原来的 300 秒 TTL 是「断点默认 TTL」的值，被误用成了条目存活时间：
		// 5 分钟后整条缓存就被清掉，长会话每隔几分钟就要重建一次前缀。
		ScaleMinInputTokens: 20000, DefaultTTLSeconds: 300, HourTTLSeconds: 3600, MaxEntriesPerScope: 200,
		MaxEntriesGlobal: 20000, EstimatedBytesLimit: 256 << 20, ExpireAfterIdleSeconds: 3600,
		// 触顶抖动。留 0 的话所有触顶请求会上报同一个数值，一眼看去就是伪造的。
		CapJitterMinTokens: 12000, CapJitterMaxTokens: 24000,
		UncachedInputMinTokens: 1024, UncachedInputMaxTokens: 4096,
		// 创建控制的默认限额对齐线上 kiro.rs 运行配置：
		// 5 分钟窗口 60 万、单次 10 万、增量下限 1.2 万、最小间隔 6 秒、
		// 至少间隔 2 次成功请求。旧的 60 秒 / 3 万 / 12 万组合会让快速会话
		// 看起来像「十几条请求才写一次」，并把大量记录压成 30k。
		CreationControl: CacheCreationControl{
			Enabled:                      true,
			MinCreationDeltaTokens:       12000,
			MinSuccessfulRequestsBetween: 2,
			MinCreationIntervalSeconds:   6,
			MaxCreationTokensPerEvent:    100000,
			CreationBudgetWindowSeconds:  300,
			MaxCreationTokensPerWindow:   600000,
		},
		Usage: DefaultCacheUsagePolicy(),
	}
	if kind == CacheStrategyKindToolAware {
		// tool_aware 对齐 kiro.rs 的 KiroRsTool 模板：那条路径把「本地模拟」整组关掉
		// （token_scale / 触顶抖动 / 模拟上限全部归零）、创建控制也关掉，
		// 只靠 KiroRsToolCachePolicy 的几个值工作 —— 全量覆盖 + 一个很小的未缓存 input 区间。
		// 与 prefix（对应 CurrentHighCache，模拟与限流都开）是两套互斥的参数，不是叠加。
		c.CoverageRatio = 1
		c.TokenScale, c.ScaleMinInputTokens, c.MaxSimulatedInputTokens = 1, 0, 0
		c.CapJitterMinTokens, c.CapJitterMaxTokens = 0, 0
		c.UncachedInputMinTokens, c.UncachedInputMaxTokens = 32, 4096
		c.CreationControl = CacheCreationControl{}
	}
	if kind == CacheStrategyKindDisabled {
		c.CacheSystem, c.CacheTools, c.CacheHistory, c.CacheToolResults = false, false, false, false
		c.CoverageRatio, c.UsageRatio, c.ReadRatio, c.CreationRatio = 0, 0, 0, 0
		c.IncrementalCreateEnabled = false
		c.CapJitterMinTokens, c.CapJitterMaxTokens = 0, 0
		c.UncachedInputMinTokens, c.UncachedInputMaxTokens = 0, 0
		c.CreationControl = CacheCreationControl{}
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
	// cache_history 与 cache_current_user_stable_prefix 同时关闭时，历史消息被过滤掉，
	// 只剩当前用户消息，而它在没有显式 cache_control 时按设计不作为断点 —— 结果是
	// 没有任何内容可缓存，策略静默失效（表现为缓存恒为 0）。这种组合只可能是误配。
	if in.Kind != CacheStrategyKindDisabled && !in.CacheHistory && !in.CacheCurrentUserStablePrefix {
		return in, fmt.Errorf(
			"config.cache_history 与 config.cache_current_user_stable_prefix 不能同时为 false：" +
				"两者都关闭后没有任何内容可缓存，策略会静默失效")
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
		in.ScopeMode = CacheScopeModeGroupSession
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
	if in.UncachedInputMinTokens < 0 || in.UncachedInputMaxTokens < 0 {
		return in, errors.New("config.uncached_input_* tokens must be non-negative")
	}
	if in.UncachedInputMaxTokens > 0 && in.UncachedInputMinTokens > in.UncachedInputMaxTokens {
		return in, errors.New("config.uncached_input_min_tokens must be <= uncached_input_max_tokens")
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
	// 增量下限高于单次创建上限时，任何一次创建都无法同时满足两者：小于下限的被丢弃，
	// 剩下的又会被上限截断。结果是缓存几乎永远建不起来（缓存恒为 0）。
	if in.CreationControl.Enabled &&
		in.CreationControl.MinCreationDeltaTokens > 0 &&
		in.CreationControl.MaxCreationTokensPerEvent > 0 &&
		in.CreationControl.MinCreationDeltaTokens > in.CreationControl.MaxCreationTokensPerEvent {
		return in, errors.New(
			"config.creation_control.min_creation_delta_tokens 不能大于 max_creation_tokens_per_event：" +
				"两者冲突会导致缓存几乎无法创建")
	}
	in.Usage = normalizeCacheUsagePolicy(in.Usage)
	in.Usage.PreserveUpstreamCacheUsage = in.PreserveUpstreamCacheUsage
	if err := validateCacheUsagePolicy(in.Usage); err != nil {
		return in, err
	}
	if !in.CacheCurrentUserStablePrefix {
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
	if in.OutputUpliftMinTokens < 0 {
		in.OutputUpliftMinTokens = 0
	}
	if in.OutputUpliftPercent < 0 {
		in.OutputUpliftPercent = 0
	}
	if in.OutputUpliftPercent > 200 {
		in.OutputUpliftPercent = 200
	}
	if in.FinalOutputGuardEnabled == nil {
		// 缺失即开启，见字段注释。补成显式值后写回库，存量策略下次读出来就不再依赖默认。
		in.FinalOutputGuardEnabled = boolPtr(true)
	}
	if in.OutputUpliftEnabled == nil {
		// 缺失时按旧语义推断，把隐式行为固化成显式开关，行为不变。
		in.OutputUpliftEnabled = boolPtr(in.OutputUpliftMinTokens > 0 && in.OutputUpliftPercent > 0)
	}
	// 三组「上限 + 扣减区间」统一按同一规则收敛：负数归零、按上限比例缩放，
	// 并保证扣减量最多为 cap-1。以前直接把两端夹到 cap，较小的 cap 会把
	// jitter_min/max 压成同一个数字，触顶值也就重新变成一整列常数；扣减量等于
	// cap 时还会把最终上报值归零。上限为 0（不限制）时区间一并清零。
	for _, g := range []struct{ cap_, jMin, jMax *int }{
		{&in.FinalCacheReadMaxTokens, &in.FinalCacheReadJitterMinTokens, &in.FinalCacheReadJitterMaxTokens},
		{&in.FinalCacheCreationMaxTokens, &in.FinalCacheCreationJitterMinTokens, &in.FinalCacheCreationJitterMaxTokens},
		{&in.FinalOutputMaxTokens, &in.FinalOutputJitterMinTokens, &in.FinalOutputJitterMaxTokens},
	} {
		*g.cap_ = max(*g.cap_, 0)
		*g.jMin, *g.jMax = normalizeFinalCapJitter(*g.cap_, *g.jMin, *g.jMax)
	}
	return in
}

// normalizeFinalCapJitter keeps final-cap jitter usable for every positive cap.
//
// Jitter is a deduction, so cap-1 is the largest safe deduction: subtracting
// cap itself would report zero. When a legacy/default range is larger than a
// small cap, scale both ends proportionally instead of clamping both to the
// same point. This preserves a real interval and keeps old configurations
// usable without a database migration.
func normalizeFinalCapJitter(capTokens, jitterMin, jitterMax int) (int, int) {
	capTokens = max(capTokens, 0)
	jitterMin = max(jitterMin, 0)
	jitterMax = max(jitterMax, 0)
	if capTokens <= 1 || jitterMax == 0 {
		return 0, 0
	}
	if jitterMax < jitterMin {
		jitterMin, jitterMax = jitterMax, jitterMin
	}

	safeMax := capTokens - 1
	if jitterMax > safeMax {
		scale := float64(safeMax) / float64(jitterMax)
		jitterMin = int(math.Floor(float64(jitterMin) * scale))
		jitterMax = safeMax
	}
	if jitterMin >= jitterMax {
		// A single-point input is not useful as jitter. Pick a lower bound that
		// retains at least two possible deductions while staying deterministic.
		jitterMin = jitterMax / 2
		if jitterMin >= jitterMax {
			jitterMin = jitterMax - 1
		}
	}
	return max(jitterMin, 0), max(jitterMax, 0)
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
	return in
}

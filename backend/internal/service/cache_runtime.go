package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
)

const (
	cacheDefaultTTL       = 5 * time.Minute
	cacheOneHourTTL       = time.Hour
	cacheMaxSupportedTTL  = time.Hour
	kiroTokensPerTool     = 150
	kiroTokensPerMessage  = 4
	cacheMinTokensDefault = 1024
	cacheMinTokensOpus    = 4096
	// cacheMinTokensGPT 与 default 同值但语义独立：1024 对齐 OpenAI 官方的最小
	// 缓存粒度，不应随 default 一起调整。见 minimumCacheableTokens。
	cacheMinTokensGPT        = 1024
	cachePrefixLookbackLimit = 10
	// Hard safety ceiling used when the model capability registry does not
	// provide a context window. Strategy values can further reduce it.
	cacheAbsoluteMaxInputTokens = 1_000_000
)

type cacheEmulationUsage struct {
	InputTokens                int
	CacheReadInputTokens       int
	CacheCreationInputTokens   int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

type cacheEntry struct {
	tokens             int
	ttl                time.Duration
	expiresAt          time.Time
	lastUsedAt         time.Time
	estimatedBytes     int64
	expireAfterIdleSec int
}

type cacheTracker struct {
	mu       sync.Mutex
	entries  map[uint64]map[[32]byte]cacheEntry
	controls map[uint64]cacheControlState
}

type cacheControlState struct {
	successfulRequests      int
	successfulSinceCreation int
	lastCreationAt          time.Time
	windowStart             time.Time
	windowTokens            int
}

var globalCacheTracker = &cacheTracker{
	entries:  make(map[uint64]map[[32]byte]cacheEntry),
	controls: make(map[uint64]cacheControlState),
}

// cacheEmulationPlan 把缓存估算拆成"计算"与"落盘"两步：prepare 阶段只读 tracker
// 得到估算结果，commit() 才会把本次前缀写入 tracker。调用方应在确认上游请求成功后
// 再 commit()，避免请求失败/未发出时就把内容错误标记为已缓存，污染下一次请求的估算。
type cacheEmulationPlan struct {
	usage       *cacheEmulationUsage
	cacheKey    uint64
	profile     *cacheProfile
	usagePolicy CacheUsagePolicy
	committed   atomic.Bool
}

func (p *cacheEmulationPlan) result() *cacheEmulationUsage {
	if p == nil {
		return nil
	}
	return p.usage
}

func (p *cacheEmulationPlan) commit() {
	if p == nil || p.profile == nil || p.cacheKey == 0 {
		return
	}
	if !p.committed.CompareAndSwap(false, true) {
		return
	}
	globalCacheTracker.update(p.cacheKey, p.profile)
	creationTokens := 0
	if p.usage != nil {
		creationTokens = p.usage.CacheCreationInputTokens
	}
	globalCacheTracker.recordSuccess(p.cacheKey, creationTokens)
}

// mergeCacheUsageIntoClaudeUsage fills synthetic cache usage only when the
// upstream did not provide authoritative cache buckets. The runtime owns the
// local prefix state, while upstream usage remains the source of truth when it
// is present (this prevents double counting on native Anthropic providers).
func mergeCacheUsageIntoClaudeUsage(dst *ClaudeUsage, simulated *cacheEmulationUsage) {
	if dst == nil || simulated == nil {
		return
	}
	if dst.CacheReadInputTokens > 0 || dst.CacheCreationInputTokens > 0 ||
		dst.CacheCreation5mTokens > 0 || dst.CacheCreation1hTokens > 0 {
		return
	}
	dst.InputTokens = simulated.InputTokens
	dst.CacheReadInputTokens = simulated.CacheReadInputTokens
	dst.CacheCreationInputTokens = simulated.CacheCreationInputTokens
	dst.CacheCreation5mTokens = simulated.CacheCreation5mInputTokens
	dst.CacheCreation1hTokens = simulated.CacheCreation1hInputTokens
}

func mergeCacheUsageIntoOpenAIUsage(dst *OpenAIUsage, simulated *cacheEmulationUsage) {
	if dst == nil || simulated == nil {
		return
	}
	if dst.CacheReadInputTokens > 0 || dst.CacheCreationInputTokens > 0 {
		return
	}
	dst.InputTokens = simulated.InputTokens + simulated.CacheReadInputTokens + simulated.CacheCreationInputTokens
	dst.CacheReadInputTokens = simulated.CacheReadInputTokens
	dst.CacheCreationInputTokens = simulated.CacheCreationInputTokens
}

func projectClaudeUsage(dst *ClaudeUsage, simulated *cacheEmulationUsage, policy CacheUsagePolicy, seed uint64) {
	if dst == nil || !policy.Enabled {
		return
	}
	rawInput, rawOutput := dst.InputTokens, dst.OutputTokens
	hadCacheEvidence := policy.PreserveUpstreamCacheUsage &&
		(dst.CacheReadInputTokens > 0 || dst.CacheCreationInputTokens > 0 ||
			dst.CacheCreation5mTokens > 0 || dst.CacheCreation1hTokens > 0)
	if !hadCacheEvidence && simulated != nil {
		dst.InputTokens = simulated.InputTokens
		dst.CacheReadInputTokens = simulated.CacheReadInputTokens
		dst.CacheCreationInputTokens = simulated.CacheCreationInputTokens
		dst.CacheCreation5mTokens = simulated.CacheCreation5mInputTokens
		dst.CacheCreation1hTokens = simulated.CacheCreation1hInputTokens
	}
	if !hadCacheEvidence && simulated != nil {
		rawInput = 0
	}
	applyUsageProjectionClaude(dst, rawInput, rawOutput, policy, seed, hadCacheEvidence || dst.CacheReadInputTokens > 0)
}

func projectOpenAIUsage(dst *OpenAIUsage, simulated *cacheEmulationUsage, policy CacheUsagePolicy, seed uint64) {
	if dst == nil || !policy.Enabled {
		return
	}
	rawInput, rawOutput := dst.InputTokens, dst.OutputTokens
	hadCacheEvidence := policy.PreserveUpstreamCacheUsage &&
		(dst.CacheReadInputTokens > 0 || dst.CacheCreationInputTokens > 0)
	if !hadCacheEvidence && simulated != nil {
		dst.InputTokens = simulated.InputTokens + simulated.CacheReadInputTokens + simulated.CacheCreationInputTokens
		dst.CacheReadInputTokens = simulated.CacheReadInputTokens
		dst.CacheCreationInputTokens = simulated.CacheCreationInputTokens
	}
	if !hadCacheEvidence && simulated != nil {
		rawInput = 0
	}
	applyUsageProjectionOpenAI(dst, rawInput, rawOutput, policy, seed, hadCacheEvidence || dst.CacheReadInputTokens > 0)
}

func applyUsageProjectionClaude(dst *ClaudeUsage, rawInput, rawOutput int, policy CacheUsagePolicy, seed uint64, hadReadEvidence bool) {
	input := dst.InputTokens
	if policy.Input.Mode == CacheUsageFieldRaw && rawInput > 0 {
		input = rawInput
	}
	cacheEvidence := hadReadEvidence || dst.CacheReadInputTokens > 0 || dst.CacheCreationInputTokens > 0
	if !(policy.Input.MoveDeltaToCacheRead && !cacheEvidence) {
		input = projectUsageField(policy.Input, input, seed^0x11)
	} else if rawInput > 0 {
		// Without an actual local/upstream cache bucket, keep the authoritative
		// uncached input value. Moving this delta into cache_creation would
		// fabricate cache usage and would not be readable on the next request.
		input = rawInput
	}
	if input < 0 {
		input = 0
	}
	if input < dst.InputTokens && policy.Input.MoveDeltaToCacheRead && cacheEvidence {
		delta := dst.InputTokens - input
		if hadReadEvidence || dst.CacheReadInputTokens > 0 {
			dst.CacheReadInputTokens += delta
		} else {
			dst.CacheCreationInputTokens += delta
			dst.CacheCreation5mTokens += delta
		}
	}
	dst.InputTokens = input
	dst.OutputTokens = projectUsageFieldWithRaw(policy.Output, dst.OutputTokens, rawOutput, seed^0x22)
	dst.CacheReadInputTokens = projectUsageField(policy.CacheRead, dst.CacheReadInputTokens, seed^0x33)
	dst.CacheCreationInputTokens = projectUsageField(policy.CacheCreation, dst.CacheCreationInputTokens, seed^0x44)
	dst.CacheCreation5mTokens, dst.CacheCreation1hTokens = capCacheCreationBreakdown(
		dst.CacheCreation5mTokens, dst.CacheCreation1hTokens, dst.CacheCreationInputTokens,
	)
	if policy.OutputUpliftPercent > 0 && policy.OutputUpliftMinTokens > 0 &&
		dst.OutputTokens > policy.OutputUpliftMinTokens {
		dst.OutputTokens += int(math.Round(float64(dst.OutputTokens) * float64(cacheMinInt(policy.OutputUpliftPercent, 200)) / 100))
	}
	if policy.FinalOutputMaxTokens > 0 {
		dst.OutputTokens = cacheMinInt(dst.OutputTokens, policy.FinalOutputMaxTokens)
	}
	if policy.FinalCacheReadMaxTokens > 0 {
		dst.CacheReadInputTokens = cacheMinInt(dst.CacheReadInputTokens, policy.FinalCacheReadMaxTokens)
	}
	if policy.FinalCacheCreationMaxTokens > 0 {
		dst.CacheCreationInputTokens = cacheMinInt(dst.CacheCreationInputTokens, policy.FinalCacheCreationMaxTokens)
		dst.CacheCreation5mTokens, dst.CacheCreation1hTokens = capCacheCreationBreakdown(
			dst.CacheCreation5mTokens, dst.CacheCreation1hTokens, dst.CacheCreationInputTokens,
		)
	}
}

func applyUsageProjectionOpenAI(dst *OpenAIUsage, rawInput, rawOutput int, policy CacheUsagePolicy, seed uint64, hadReadEvidence bool) {
	total := cacheMaxInt(dst.InputTokens, 0)
	read := cacheMaxInt(dst.CacheReadInputTokens, 0)
	creation := cacheMaxInt(dst.CacheCreationInputTokens, 0)
	uncached := cacheMaxInt(total-read-creation, 0)
	if policy.Input.Mode == CacheUsageFieldRaw && rawInput > 0 {
		total = rawInput
		uncached = cacheMaxInt(total-read-creation, 0)
	}
	cacheEvidence := hadReadEvidence || read > 0 || creation > 0
	projected := uncached
	if !(policy.Input.MoveDeltaToCacheRead && !cacheEvidence) {
		projected = projectUsageField(policy.Input, uncached, seed^0x11)
	}
	if projected < uncached && policy.Input.MoveDeltaToCacheRead && cacheEvidence {
		delta := uncached - projected
		if hadReadEvidence || read > 0 {
			read += delta
		} else {
			creation += delta
		}
	}
	uncached = projected
	read = projectUsageField(policy.CacheRead, read, seed^0x33)
	creation = projectUsageField(policy.CacheCreation, creation, seed^0x44)
	if policy.FinalCacheReadMaxTokens > 0 {
		read = cacheMinInt(read, policy.FinalCacheReadMaxTokens)
	}
	if policy.FinalCacheCreationMaxTokens > 0 {
		creation = cacheMinInt(creation, policy.FinalCacheCreationMaxTokens)
	}
	dst.InputTokens = uncached + read + creation
	dst.CacheReadInputTokens = read
	dst.CacheCreationInputTokens = creation
	dst.OutputTokens = projectUsageFieldWithRaw(policy.Output, dst.OutputTokens, rawOutput, seed^0x22)
	if policy.OutputUpliftPercent > 0 && policy.OutputUpliftMinTokens > 0 &&
		dst.OutputTokens > policy.OutputUpliftMinTokens {
		dst.OutputTokens += int(math.Round(float64(dst.OutputTokens) * float64(cacheMinInt(policy.OutputUpliftPercent, 200)) / 100))
	}
	if policy.FinalOutputMaxTokens > 0 {
		dst.OutputTokens = cacheMinInt(dst.OutputTokens, policy.FinalOutputMaxTokens)
	}
}

func projectUsageFieldWithRaw(policy CacheUsageFieldPolicy, current, raw int, seed uint64) int {
	if policy.Mode == CacheUsageFieldRaw && raw > 0 {
		current = raw
	}
	return projectUsageField(policy, current, seed)
}

func projectUsageField(policy CacheUsageFieldPolicy, current int, seed uint64) int {
	current = cacheMaxInt(current, 0)
	switch policy.Mode {
	case CacheUsageFieldRaw, CacheUsageFieldPreserve:
		return current
	case CacheUsageFieldSampleMax:
		if policy.MaxTokens <= 0 {
			return current
		}
		return cacheMinInt(current, policy.MaxTokens)
	case CacheUsageFieldSampleTarget:
		if policy.TargetTokens <= 0 {
			return current
		}
		high := int(math.Round(float64(policy.TargetTokens) * cacheMaxFloat(policy.NormalMaxMultiplier, 1)))
		high = cacheMaxInt(high, policy.TargetTokens)
		low := cacheMaxInt(int(math.Round(float64(policy.TargetTokens)*0.85)), 1)
		if high < low {
			high = low
		}
		if current < low {
			return current
		}
		span := high - low + 1
		if span <= 1 {
			return cacheMinInt(current, low)
		}
		n := int(splitmix64(seed^uint64(current))%uint64(span)) + low
		return cacheMinInt(current, n)
	default:
		return current
	}
}

func cacheMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func cacheMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func cacheMaxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func capCacheCreationBreakdown(cache5m, cache1h, total int) (int, int) {
	total = cacheMaxInt(total, 0)
	cache5m = cacheMinInt(cacheMaxInt(cache5m, 0), total)
	cache1h = cacheMinInt(cacheMaxInt(cache1h, 0), total-cache5m)
	if cache5m+cache1h < total && cache5m == 0 && cache1h == 0 {
		cache5m = total
	}
	return cache5m, cache1h
}

func splitmix64(v uint64) uint64 {
	v += 0x9e3779b97f4a7c15
	v = (v ^ (v >> 30)) * 0xbf58476d1ce4e5b9
	v = (v ^ (v >> 27)) * 0x94d049bb133111eb
	return v ^ (v >> 31)
}

func cacheGroupFromContext(c *gin.Context, account *Account) *Group {
	if c != nil {
		if value, ok := c.Get("sub2api.group"); ok {
			if group, ok := value.(*Group); ok && group != nil {
				return group
			}
		}
	}
	if account != nil {
		for _, group := range account.Groups {
			if group != nil {
				return group
			}
		}
	}
	return nil
}

func cachePlanFromContext(c *gin.Context) *cacheEmulationPlan {
	if c == nil {
		return nil
	}
	if value, ok := c.Get(cachePlanContextKey); ok {
		if plan, ok := value.(*cacheEmulationPlan); ok {
			return plan
		}
	}
	return nil
}

func setCachePlanContext(c *gin.Context, plan *cacheEmulationPlan) {
	if c == nil {
		return
	}
	if plan == nil {
		c.Set(cachePlanContextKey, (*cacheEmulationPlan)(nil))
		return
	}
	c.Set(cachePlanContextKey, plan)
}

const cachePlanContextKey = "sub2api.cache_plan"

// SetCacheGroupContext records the group selected by the request router. OpenAI
// compatibility services do not receive a group argument in their public API,
// so handlers must set this immediately after authenticating the API key.
func SetCacheGroupContext(c *gin.Context, group *Group) {
	if c == nil {
		return
	}
	if group == nil {
		c.Set("sub2api.group", (*Group)(nil))
		return
	}
	c.Set("sub2api.group", group)
}

// cachePlanForProtocol prepares a group-bound plan for any Claude Code
// compatible protocol. It intentionally has no Kiro-specific behavior; the
// historical method names remain only for source compatibility with existing
// callers and tests.
func cachePlanForProtocol(ctx context.Context, account *Account, group *Group, body []byte, model, protocol string, inputTokens int) *cacheEmulationPlan {
	gs := &GatewayService{}
	switch protocol {
	case "openai_responses":
		return gs.prepareResponsesCacheUsage(ctx, account, group, body, model, inputTokens)
	case "openai_chat_completions":
		return gs.prepareChatCompletionsCacheUsage(ctx, account, group, body, model, inputTokens)
	default:
		return gs.prepareCacheEmulationUsage(ctx, account, group, body, model, inputTokens)
	}
}

func prepareCachePlanForContext(ctx context.Context, c *gin.Context, account *Account, group *Group, body []byte, model, protocol string, inputTokens int) *cacheEmulationPlan {
	if existing := cachePlanFromContext(c); existing != nil {
		return existing
	}
	plan := cachePlanForProtocol(ctx, account, group, body, model, protocol, inputTokens)
	setCachePlanContext(c, plan)
	return plan
}

// mergeAndCommitCachePlan merges synthetic buckets only when the upstream did
// not provide authoritative cache usage, then records the prefix after a
// complete successful response. Callers must pass success=false for stream
// interruption, client cancellation, missing terminal events, or upstream
// errors.
func mergeAndCommitCachePlan(c *gin.Context, usage *ClaudeUsage, success bool) {
	plan := cachePlanFromContext(c)
	if plan == nil {
		return
	}
	if usage != nil {
		upstreamEvidence := claudeUsageHasCacheEvidence(usage)
		projectClaudeUsage(usage, plan.result(), plan.usagePolicy, plan.cacheKey)
		if !upstreamEvidence {
			if plan.result() == nil {
				applyReportedInputWithoutCacheClaude(usage, plan)
			} else {
				constrainClaudeUsageTotal(usage, plan.profile.reportedInputTokens, plan.profile.policy.ReportedInputMinTokens)
			}
		}
	}
	if success {
		plan.commit()
	}
}

func commitCachePlan(c *gin.Context) {
	if plan := cachePlanFromContext(c); plan != nil {
		plan.commit()
	}
}

func mergeAndCommitOpenAICachePlan(c *gin.Context, usage *OpenAIUsage, success bool) {
	plan := cachePlanFromContext(c)
	if plan == nil {
		return
	}
	if usage != nil {
		upstreamEvidence := openAIUsageHasCacheEvidence(usage)
		projectOpenAIUsage(usage, plan.result(), plan.usagePolicy, plan.cacheKey)
		if !upstreamEvidence {
			if plan.result() == nil {
				applyReportedInputWithoutCacheOpenAI(usage, plan)
			} else {
				constrainOpenAIUsageTotal(usage, plan.profile.reportedInputTokens, plan.profile.policy.ReportedInputMinTokens)
			}
		}
	}
	if success {
		plan.commit()
	}
}

func claudeUsageHasCacheEvidence(usage *ClaudeUsage) bool {
	return usage != nil && (usage.CacheReadInputTokens > 0 ||
		usage.CacheCreationInputTokens > 0 ||
		usage.CacheCreation5mTokens > 0 ||
		usage.CacheCreation1hTokens > 0)
}

func openAIUsageHasCacheEvidence(usage *OpenAIUsage) bool {
	return usage != nil && (usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0)
}

// When a strategy has no reportable cache bucket (for example a cold request
// with creation disabled), the profile still owns the configured input
// projection. Without this path, raw upstream input would bypass
// reported_input_min/max and make those settings no-ops on the first request.
func applyReportedInputWithoutCacheClaude(usage *ClaudeUsage, plan *cacheEmulationPlan) {
	if usage == nil || plan == nil || plan.profile == nil {
		return
	}
	if plan.profile.reportedInputTokens > 0 {
		usage.InputTokens = plan.profile.reportedInputTokens
	}
}

func applyReportedInputWithoutCacheOpenAI(usage *OpenAIUsage, plan *cacheEmulationPlan) {
	if usage == nil || plan == nil || plan.profile == nil {
		return
	}
	if plan.profile.reportedInputTokens > 0 {
		usage.InputTokens = plan.profile.reportedInputTokens
		usage.CacheReadInputTokens = 0
		usage.CacheCreationInputTokens = 0
	}
}

// constrainClaudeUsageTotal applies the profile's projected total cap after
// field-level sampling. It keeps cache reads as the strongest evidence,
// removes creation first, and then preserves the configured uncached floor
// whenever the cap leaves enough room.
func constrainClaudeUsageTotal(usage *ClaudeUsage, totalCap, minInput int) {
	if usage == nil || totalCap <= 0 {
		return
	}
	totalCap = max(totalCap, 0)
	usage.InputTokens = max(usage.InputTokens, 0)
	usage.CacheReadInputTokens = max(usage.CacheReadInputTokens, 0)
	usage.CacheCreationInputTokens = max(usage.CacheCreationInputTokens, 0)
	usage.CacheCreation5mTokens = max(usage.CacheCreation5mTokens, 0)
	usage.CacheCreation1hTokens = max(usage.CacheCreation1hTokens, 0)
	usage.CacheCreation5mTokens, usage.CacheCreation1hTokens = capCacheCreationBreakdown(
		usage.CacheCreation5mTokens, usage.CacheCreation1hTokens, usage.CacheCreationInputTokens,
	)
	minInput = min(max(minInput, 0), totalCap)
	total := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	if total > totalCap {
		overflow := total - totalCap
		reduceCreation := min(overflow, usage.CacheCreationInputTokens)
		usage.CacheCreationInputTokens -= reduceCreation
		overflow -= reduceCreation
		if overflow > 0 {
			reduceInput := min(overflow, max(usage.InputTokens-minInput, 0))
			usage.InputTokens -= reduceInput
			overflow -= reduceInput
		}
		if overflow > 0 {
			usage.CacheReadInputTokens = max(usage.CacheReadInputTokens-overflow, 0)
		}
	}
	if usage.InputTokens < minInput {
		deficit := minInput - usage.InputTokens
		reduceCreation := min(deficit, usage.CacheCreationInputTokens)
		usage.CacheCreationInputTokens -= reduceCreation
		deficit -= reduceCreation
		if deficit > 0 {
			reduceRead := min(deficit, usage.CacheReadInputTokens)
			usage.CacheReadInputTokens -= reduceRead
		}
		usage.InputTokens = max(totalCap-usage.CacheReadInputTokens-usage.CacheCreationInputTokens, 0)
	}
	usage.CacheCreation5mTokens, usage.CacheCreation1hTokens = capCacheCreationBreakdown(
		usage.CacheCreation5mTokens, usage.CacheCreation1hTokens, usage.CacheCreationInputTokens,
	)
}

func constrainOpenAIUsageTotal(usage *OpenAIUsage, totalCap, minInput int) {
	if usage == nil || totalCap <= 0 {
		return
	}
	totalCap = max(totalCap, 0)
	usage.InputTokens = max(usage.InputTokens, 0)
	usage.CacheReadInputTokens = max(usage.CacheReadInputTokens, 0)
	usage.CacheCreationInputTokens = max(usage.CacheCreationInputTokens, 0)
	uncached := max(usage.InputTokens-usage.CacheReadInputTokens-usage.CacheCreationInputTokens, 0)
	minInput = min(max(minInput, 0), totalCap)
	if usage.InputTokens > totalCap {
		overflow := usage.InputTokens - totalCap
		reduceCreation := min(overflow, usage.CacheCreationInputTokens)
		usage.CacheCreationInputTokens -= reduceCreation
		overflow -= reduceCreation
		if overflow > 0 {
			reduceUncached := min(overflow, max(uncached-minInput, 0))
			uncached -= reduceUncached
			overflow -= reduceUncached
		}
		if overflow > 0 {
			usage.CacheReadInputTokens = max(usage.CacheReadInputTokens-overflow, 0)
		}
		usage.InputTokens = uncached + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	}
	if uncached < minInput {
		deficit := minInput - uncached
		reduceCreation := min(deficit, usage.CacheCreationInputTokens)
		usage.CacheCreationInputTokens -= reduceCreation
		deficit -= reduceCreation
		if deficit > 0 {
			reduceRead := min(deficit, usage.CacheReadInputTokens)
			usage.CacheReadInputTokens -= reduceRead
		}
		uncached = max(totalCap-usage.CacheReadInputTokens-usage.CacheCreationInputTokens, 0)
		usage.InputTokens = uncached + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	}
}

func commitOpenAICachePlan(c *gin.Context) {
	if plan := cachePlanFromContext(c); plan != nil {
		plan.commit()
	}
}

func (s *GatewayService) buildCacheEmulationUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *cacheEmulationUsage {
	plan := s.prepareCacheEmulationUsage(ctx, account, group, body, model, inputTokens)
	plan.commit()
	return plan.result()
}

func (s *GatewayService) prepareCacheEmulationUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *cacheEmulationPlan {
	NormalizeGroupRuntimeFields(group)
	if group == nil || account == nil || account.ID <= 0 || len(body) == 0 {
		return nil
	}
	minCacheable := minimumCacheableTokens(model)
	if policy, enabled := effectiveCacheStrategyConfig(group); enabled && policy.MinCacheableTokens > 0 {
		minCacheable = policy.MinCacheableTokens
	}
	profile, ok := buildCacheProfileWithMin(ctx, body, model, inputTokens, minCacheable)
	if !ok {
		return nil
	}
	profile.rawBody = append([]byte(nil), body...)
	profile.protocolFamily = "anthropic_messages"
	return s.prepareCacheEmulationPlanFromProfile(account, group, profile, inputTokens)
}

func (s *GatewayService) buildResponsesCacheUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *cacheEmulationUsage {
	plan := s.prepareResponsesCacheUsage(ctx, account, group, body, model, inputTokens)
	plan.commit()
	return plan.result()
}

func (s *GatewayService) prepareResponsesCacheUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *cacheEmulationPlan {
	NormalizeGroupRuntimeFields(group)
	if group == nil || account == nil || account.ID <= 0 || len(body) == 0 {
		return nil
	}
	minCacheable := minimumCacheableTokens(model)
	if policy, enabled := effectiveCacheStrategyConfig(group); enabled && policy.MinCacheableTokens > 0 {
		minCacheable = policy.MinCacheableTokens
	}
	profile, ok := buildResponsesCacheProfileWithMin(ctx, body, model, inputTokens, minCacheable)
	if !ok {
		return nil
	}
	profile.rawBody = append([]byte(nil), body...)
	profile.protocolFamily = "openai_responses"
	return s.prepareCacheEmulationPlanFromProfile(account, group, profile, inputTokens)
}

func (s *GatewayService) buildChatCompletionsCacheUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *cacheEmulationUsage {
	plan := s.prepareChatCompletionsCacheUsage(ctx, account, group, body, model, inputTokens)
	plan.commit()
	return plan.result()
}

func (s *GatewayService) prepareChatCompletionsCacheUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *cacheEmulationPlan {
	NormalizeGroupRuntimeFields(group)
	if group == nil || account == nil || account.ID <= 0 || len(body) == 0 {
		return nil
	}
	minCacheable := minimumCacheableTokens(model)
	if policy, enabled := effectiveCacheStrategyConfig(group); enabled && policy.MinCacheableTokens > 0 {
		minCacheable = policy.MinCacheableTokens
	}
	profile, ok := buildChatCompletionsCacheProfileWithMin(ctx, body, model, inputTokens, minCacheable)
	if !ok {
		return nil
	}
	profile.rawBody = append([]byte(nil), body...)
	effectiveInputTokens := inputTokens
	if effectiveInputTokens <= 0 {
		effectiveInputTokens = profile.totalInputTokens
	}
	profile.protocolFamily = "openai_chat_completions"
	return s.prepareCacheEmulationPlanFromProfile(account, group, profile, effectiveInputTokens)
}

func (s *GatewayService) prepareCacheEmulationPlanFromProfile(account *Account, group *Group, profile *cacheProfile, inputTokens int) *cacheEmulationPlan {
	if group == nil || account == nil || account.ID <= 0 || profile == nil {
		return nil
	}
	policy, enabled := effectiveCacheStrategyConfig(group)
	if !enabled {
		return nil
	}
	profile.policy = policy
	profile.policyEnabled = true
	applyCacheStrategyToProfile(profile, policy, modelFromProfile(profile))
	if policy.MinCacheableTokens > 0 {
		profile.minCacheable = policy.MinCacheableTokens
	}
	cacheKey := cacheStrategyCacheKey(account, group, profile.protocolFamily, profile.model, profile.rawBody)
	if cacheKey == 0 {
		return nil
	}
	result := globalCacheTracker.compute(cacheKey, profile)
	if result == nil {
		return nil
	}
	rawReadTokens := result.CacheReadInputTokens
	rawCreationTokens := result.CacheCreationInputTokens
	// Creation controls govern the actual state transition, not only the
	// projected usage. Apply them in runtime token space before any reporting
	// ratio is applied, then trim the commit profile to the same boundary.
	if policy.MaxNewCreationTokensPerRequest > 0 && rawCreationTokens > policy.MaxNewCreationTokensPerRequest {
		rawCreationTokens = policy.MaxNewCreationTokensPerRequest
	}
	if !policy.IncrementalCreateEnabled && rawReadTokens > 0 {
		rawCreationTokens = 0
	}
	controlUsage := &cacheEmulationUsage{
		CacheReadInputTokens:       rawReadTokens,
		CacheCreationInputTokens:   rawCreationTokens,
		CacheCreation5mInputTokens: result.CacheCreation5mInputTokens,
		CacheCreation1hInputTokens: result.CacheCreation1hInputTokens,
	}
	if policy.CreationControl.MinCreationDeltaTokens > 0 && controlUsage.CacheCreationInputTokens < policy.CreationControl.MinCreationDeltaTokens {
		controlUsage.CacheCreationInputTokens = 0
		controlUsage.CacheCreation5mInputTokens = 0
		controlUsage.CacheCreation1hInputTokens = 0
	}
	applyCacheCreationControl(cacheKey, policy.CreationControl, controlUsage)
	rawCreationTokens = controlUsage.CacheCreationInputTokens
	if rawCreationTokens <= 0 {
		controlUsage.CacheCreation5mInputTokens = 0
		controlUsage.CacheCreation1hInputTokens = 0
	}
	rawCreationTokens = limitCacheProfileWriteSet(profile, rawReadTokens, rawCreationTokens)
	controlUsage.CacheCreationInputTokens = rawCreationTokens
	controlUsage.CacheCreation5mInputTokens, controlUsage.CacheCreation1hInputTokens =
		scaleCacheCreationTTLToTotal(
			controlUsage.CacheCreation5mInputTokens,
			controlUsage.CacheCreation1hInputTokens,
			rawCreationTokens,
		)
	result.CacheReadInputTokens = rawReadTokens
	result.CacheCreationInputTokens = rawCreationTokens
	result.CacheCreation5mInputTokens = controlUsage.CacheCreation5mInputTokens
	result.CacheCreation1hInputTokens = controlUsage.CacheCreation1hInputTokens
	if policy.RatioMode == CacheRatioModeUniform {
		ratio := policy.UsageRatio
		result.CacheReadInputTokens = scaleCacheTokens(result.CacheReadInputTokens, ratio)
		result.CacheCreationInputTokens = scaleCacheTokens(result.CacheCreationInputTokens, ratio)
		result.CacheCreation5mInputTokens = scaleCacheTokens(result.CacheCreation5mInputTokens, ratio)
		result.CacheCreation1hInputTokens = scaleCacheTokens(result.CacheCreation1hInputTokens, ratio)
	} else {
		creationRatio, readRatio := policy.CreationRatio, policy.ReadRatio
		result.CacheReadInputTokens = scaleCacheTokens(result.CacheReadInputTokens, readRatio)
		result.CacheCreationInputTokens = scaleCacheTokens(result.CacheCreationInputTokens, creationRatio)
		result.CacheCreation5mInputTokens, result.CacheCreation1hInputTokens = scaleCacheCreationTTLTokens(
			result.CacheCreation5mInputTokens,
			result.CacheCreation1hInputTokens,
			result.CacheCreationInputTokens,
			creationRatio,
		)
	}
	reportedTotal := profile.reportedInputTokens
	if reportedTotal <= 0 {
		reportedTotal = inputTokens
	}
	constrainReportedCacheUsage(result, reportedTotal, policy.ReportedInputMinTokens)
	if result.CacheReadInputTokens == 0 && result.CacheCreationInputTokens == 0 {
		// No cache usage is reportable after creation/read controls and ratios
		// have been applied. Do not commit a profile that would make hidden
		// entries readable on a later request.
		profile.breakpoints = nil
		result = nil
	}
	return &cacheEmulationPlan{
		usage: result, cacheKey: cacheKey, profile: profile, usagePolicy: policy.Usage,
	}
}

// constrainReportedCacheUsage keeps the reported buckets within the projected
// total and preserves the configured uncached-input floor whenever feasible.
// Cache creation is trimmed before cache reads because writes are optional
// state transitions, while a hit is evidence of an already-existing prefix.
func constrainReportedCacheUsage(result *cacheEmulationUsage, reportedTotal, minInput int) {
	if result == nil {
		return
	}
	reportedTotal = max(reportedTotal, 0)
	result.CacheReadInputTokens = min(max(result.CacheReadInputTokens, 0), reportedTotal)
	remaining := max(reportedTotal-result.CacheReadInputTokens, 0)
	result.CacheCreationInputTokens = min(max(result.CacheCreationInputTokens, 0), remaining)
	result.CacheCreation5mInputTokens, result.CacheCreation1hInputTokens = capCacheCreationBreakdown(
		result.CacheCreation5mInputTokens,
		result.CacheCreation1hInputTokens,
		result.CacheCreationInputTokens,
	)

	minInput = min(max(minInput, 0), reportedTotal)
	input := reportedTotal - result.CacheReadInputTokens - result.CacheCreationInputTokens
	if input < minInput {
		deficit := minInput - input
		reduceCreation := min(deficit, result.CacheCreationInputTokens)
		result.CacheCreationInputTokens -= reduceCreation
		deficit -= reduceCreation
		if deficit > 0 {
			reduceRead := min(deficit, result.CacheReadInputTokens)
			result.CacheReadInputTokens -= reduceRead
		}
		result.CacheCreation5mInputTokens, result.CacheCreation1hInputTokens = capCacheCreationBreakdown(
			result.CacheCreation5mInputTokens,
			result.CacheCreation1hInputTokens,
			result.CacheCreationInputTokens,
		)
		input = reportedTotal - result.CacheReadInputTokens - result.CacheCreationInputTokens
	}
	result.InputTokens = max(input, 0)
}

// limitCacheProfileWriteSet keeps only complete breakpoints that can be
// justified by the allowed read + creation token budget. This prevents hidden
// cache state when a usage ratio, per-request cap, or creation controller
// suppresses part of the candidate creation.
func limitCacheProfileWriteSet(profile *cacheProfile, readTokens, creationTokens int) int {
	if profile == nil {
		return 0
	}
	limit := max(readTokens, 0) + max(creationTokens, 0)
	if limit <= 0 {
		profile.breakpoints = nil
		return 0
	}
	filtered := make([]cacheBreakpoint, 0, len(profile.breakpoints))
	actualTarget := 0
	for _, breakpoint := range profile.breakpoints {
		tokens := profile.cacheTokensForBreakpoint(profile.blocks[breakpoint.blockIndex].cumulativeTokens)
		if tokens <= 0 || tokens > limit {
			continue
		}
		filtered = append(filtered, breakpoint)
		actualTarget = tokens
	}
	profile.breakpoints = filtered
	if actualTarget <= readTokens {
		return 0
	}
	return actualTarget - readTokens
}

func modelFromProfile(profile *cacheProfile) string {
	if profile == nil {
		return ""
	}
	return profile.model
}

func applyCacheStrategyToProfile(profile *cacheProfile, policy CacheStrategyConfig, model string) {
	if profile == nil {
		return
	}
	profile.pendingBlocks = filterCacheStrategyBlocks(profile.pendingBlocks, policy)
	if policy.BreakpointMode == CacheBreakpointAuto {
		applyDefaultCacheBreakpoints(profile.pendingBlocks, cacheDefaultTTL)
		profile.defaultedBreakpoints = true
	}
	rebuildCacheProfile(profile)
	// Automatically-derived breakpoints must stop before the current dynamic
	// user turn. An explicit client cache_control remains authoritative, while
	// auto/hybrid fallback only caches stable history/system/tool prefixes.
	if profile.defaultedBreakpoints && !policy.CacheCurrentUserStablePrefix {
		removeFinalMessageBreakpoints(profile)
	}
	runtimeInputTokens := profile.runtimeInputTokens
	if runtimeInputTokens <= 0 {
		runtimeInputTokens = profile.totalInputTokens
	}
	reportedInputTokens := runtimeInputTokens
	if policy.ReportedInputMinTokens > 0 {
		reportedInputTokens = max(reportedInputTokens, policy.ReportedInputMinTokens)
	}
	capApplied := false
	if policy.ReportedInputMaxTokens > 0 {
		if reportedInputTokens > policy.ReportedInputMaxTokens {
			reportedInputTokens = policy.ReportedInputMaxTokens
			capApplied = true
		}
	}
	if policy.TokenScale > 1 && reportedInputTokens >= policy.ScaleMinInputTokens {
		reportedInputTokens = int(math.Round(float64(reportedInputTokens) * policy.TokenScale))
		if policy.MaxSimulatedInputTokens > 0 {
			if reportedInputTokens > policy.MaxSimulatedInputTokens {
				reportedInputTokens = policy.MaxSimulatedInputTokens
				capApplied = true
			}
		}
	}
	// Token scaling can move the value back above the explicit reported-input
	// maximum, so enforce that cap again after scaling.
	if policy.ReportedInputMaxTokens > 0 && reportedInputTokens > policy.ReportedInputMaxTokens {
		reportedInputTokens = policy.ReportedInputMaxTokens
		capApplied = true
	}
	if reportedInputTokens > cacheAbsoluteMaxInputTokens {
		reportedInputTokens = cacheAbsoluteMaxInputTokens
		capApplied = true
	}
	if capApplied {
		reportedInputTokens = applyReportedInputJitter(
			reportedInputTokens,
			policy.CapJitterMinTokens,
			policy.CapJitterMaxTokens,
			profileJitterSeed(profile),
			policy.ReportedInputMinTokens,
		)
	}
	profile.reportedInputTokens = reportedInputTokens
	if profile.defaultedBreakpoints {
		defaultTTL := time.Duration(policy.DefaultTTLSeconds) * time.Second
		if defaultTTL <= 0 {
			defaultTTL = cacheDefaultTTL
		}
		for i := range profile.breakpoints {
			profile.breakpoints[i].ttl = minDuration(defaultTTL, cacheMaxSupportedTTL)
		}
	}
	if policy.HourTTLSeconds > 0 {
		hourTTL := minDuration(time.Duration(policy.HourTTLSeconds)*time.Second, cacheMaxSupportedTTL)
		for i := range profile.breakpoints {
			if profile.breakpoints[i].ttl >= cacheOneHourTTL {
				profile.breakpoints[i].ttl = hourTTL
			}
		}
	}
	if policy.BreakpointMode == CacheBreakpointClientOnly && profile.defaultedBreakpoints {
		profile.breakpoints = nil
	}
}

func removeFinalMessageBreakpoints(profile *cacheProfile) {
	if profile == nil || len(profile.breakpoints) == 0 || len(profile.pendingBlocks) == 0 {
		return
	}
	lastMessage := -1
	for _, block := range profile.pendingBlocks {
		if block.messageIndex != nil && *block.messageIndex > lastMessage {
			lastMessage = *block.messageIndex
		}
	}
	if lastMessage < 0 {
		return
	}
	filtered := profile.breakpoints[:0]
	for _, breakpoint := range profile.breakpoints {
		if breakpoint.blockIndex >= 0 && breakpoint.blockIndex < len(profile.pendingBlocks) {
			block := profile.pendingBlocks[breakpoint.blockIndex]
			if block.messageIndex != nil && *block.messageIndex == lastMessage {
				continue
			}
		}
		filtered = append(filtered, breakpoint)
	}
	profile.breakpoints = filtered
}

// applyReportedInputJitter applies a deterministic reduction only when a
// reported-input cap was actually reached. The reduction never crosses the
// configured lower bound and is derived from the normalized profile, so
// retries of the same request remain stable while different profiles can vary.
func applyReportedInputJitter(capped, minJitter, maxJitter int, seed uint64, lowerBound int) int {
	if capped <= 0 || maxJitter <= 0 {
		return capped
	}
	if minJitter < 0 {
		minJitter = 0
	}
	if maxJitter < minJitter {
		maxJitter = minJitter
	}
	maxAllowed := capped - max(lowerBound, 0)
	if maxAllowed <= 0 {
		return capped
	}
	minJitter = min(minJitter, maxAllowed)
	maxJitter = min(maxJitter, maxAllowed)
	if maxJitter < minJitter {
		maxJitter = minJitter
	}
	span := maxJitter - minJitter + 1
	jitter := minJitter
	if span > 1 {
		jitter += int(splitmix64(seed) % uint64(span))
	}
	return max(capped-jitter, max(lowerBound, 0))
}

func profileJitterSeed(profile *cacheProfile) uint64 {
	if profile == nil {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write(profile.rawBody)
	_, _ = h.Write([]byte(profile.model))
	return h.Sum64()
}

func filterCacheStrategyBlocks(blocks []pendingCacheBlock, policy CacheStrategyConfig) []pendingCacheBlock {
	if len(blocks) == 0 {
		return nil
	}
	lastMessage := -1
	hasEarlierStable := false
	for _, block := range blocks {
		if block.messageIndex != nil && *block.messageIndex > lastMessage {
			lastMessage = *block.messageIndex
		}
	}
	for _, block := range blocks {
		if block.messageIndex == nil || (lastMessage >= 0 && *block.messageIndex < lastMessage) {
			hasEarlierStable = true
			break
		}
	}
	out := make([]pendingCacheBlock, 0, len(blocks))
	currentUserTokens := 0
	currentUserMax := policy.CurrentUserStablePrefixMaxTokens
	for _, block := range blocks {
		kind := ""
		if value, ok := block.value.(map[string]any); ok {
			kind, _ = value["kind"].(string)
		}
		if kind == "tool" && !policy.CacheTools {
			continue
		}
		if kind == "system" && !policy.CacheSystem {
			continue
		}
		if strings.Contains(kind, "tool") && kind != "tool" && !policy.CacheToolResults {
			continue
		}
		if block.messageIndex != nil {
			if *block.messageIndex < lastMessage && !policy.CacheHistory {
				continue
			}
			if *block.messageIndex == lastMessage && !policy.CacheCurrentUserStablePrefix &&
				block.breakpointTTL == nil && hasEarlierStable {
				continue
			}
			if *block.messageIndex == lastMessage && currentUserMax > 0 {
				if currentUserTokens >= currentUserMax {
					continue
				}
				if currentUserTokens+max(block.tokens, 0) > currentUserMax {
					continue
				}
				currentUserTokens += max(block.tokens, 0)
			}
		}
		out = append(out, block)
	}
	if currentUserMax > 0 && len(out) > 0 {
		// A token cap may drop the original final block (and its breakpoint).
		// Keep the retained prefix cacheable by moving the same TTL marker to
		// the last retained block of the current message.
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].messageIndex != nil && *out[i].messageIndex == lastMessage {
				if out[i].breakpointTTL == nil {
					ttl := cacheDefaultTTL
					out[i].breakpointTTL = &ttl
				}
				break
			}
		}
	}
	return out
}

func rebuildCacheProfile(profile *cacheProfile) {
	if profile == nil {
		return
	}
	prefixState := make([]byte, 8+len(profile.preludeCanonical))
	binary.BigEndian.PutUint64(prefixState[:8], uint64(len(profile.preludeCanonical)))
	copy(prefixState[8:], profile.preludeCanonical)
	profile.blocks = nil
	profile.breakpoints = nil
	cumulativeTokens := 0
	var activeTTL *time.Duration
	seenBreakpoints := make(map[int]struct{})
	for index, block := range profile.pendingBlocks {
		cumulativeTokens += max(block.tokens, 0)
		blockJSON, err := canonicalJSON(block.value)
		if err != nil {
			continue
		}
		blockHash := sha256.Sum256(blockJSON)
		h := sha256.New()
		_, _ = h.Write(prefixState)
		_, _ = h.Write(blockHash[:])
		prefixFingerprint := [32]byte(h.Sum(nil))
		prefixState = prefixFingerprint[:]
		profile.blocks = append(profile.blocks, cacheBlock{prefixFingerprint: prefixFingerprint, cumulativeTokens: cumulativeTokens})
		if block.breakpointTTL != nil {
			ttl := minDuration(*block.breakpointTTL, cacheMaxSupportedTTL)
			activeTTL = &ttl
			if _, ok := seenBreakpoints[index]; !ok {
				profile.breakpoints = append(profile.breakpoints, cacheBreakpoint{blockIndex: index, ttl: ttl})
				seenBreakpoints[index] = struct{}{}
			}
		}
		if block.isMessageEnd && block.messageIndex != nil && activeTTL != nil {
			if _, ok := seenBreakpoints[index]; !ok {
				profile.breakpoints = append(profile.breakpoints, cacheBreakpoint{blockIndex: index, ttl: *activeTTL})
				seenBreakpoints[index] = struct{}{}
			}
		}
	}
}

func scaleCacheCreationTTLToTotal(tokens5m, tokens1h, total int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	if tokens5m <= 0 && tokens1h <= 0 {
		return total, 0
	}
	base := tokens5m + tokens1h
	if base <= 0 {
		return total, 0
	}
	five := int(math.Round(float64(tokens5m) * float64(total) / float64(base)))
	if five < 0 {
		five = 0
	}
	if five > total {
		five = total
	}
	return five, total - five
}

func scaleCacheCreationTTLTokens(tokens5m, tokens1h, scaledTotal int, ratio float64) (int, int) {
	if scaledTotal <= 0 || ratio <= 0 {
		return 0, 0
	}
	if tokens5m <= 0 && tokens1h <= 0 {
		return 0, 0
	}
	if tokens1h <= 0 {
		return scaledTotal, 0
	}
	if tokens5m <= 0 {
		return 0, scaledTotal
	}
	scaled5m := scaleCacheTokens(tokens5m, ratio)
	if scaled5m > scaledTotal {
		scaled5m = scaledTotal
	}
	scaled1h := scaledTotal - scaled5m
	return scaled5m, scaled1h
}

func scaleCacheTokens(tokens int, ratio float64) int {
	if tokens <= 0 || ratio <= 0 {
		return 0
	}
	if ratio >= 1 {
		return tokens
	}
	return int(math.Round(float64(tokens) * ratio))
}

type cacheProfile struct {
	totalInputTokens     int
	runtimeInputTokens   int
	reportedInputTokens  int
	minCacheable         int
	model                string
	protocolFamily       string
	rawBody              []byte
	policy               CacheStrategyConfig
	policyEnabled        bool
	defaultedBreakpoints bool
	preludeCanonical     []byte
	pendingBlocks        []pendingCacheBlock
	// scaleBreakpointsToInputTokens 决定断点累计值是否归一化到 totalInputTokens 所在的
	// token 空间，三条协议路径都必须置为 true。
	//
	// 两侧计数口径本就不同：断点累计值由 countKiroMessageContentTokens 逐块累加，只统计
	// text/thinking 正文；而 totalInputTokens 来自 countKiroInputTokensFromPayload，统计
	// 的是整个 messages 的序列化 JSON，并额外计入每消息 kiroTokensPerMessage、每工具
	// kiroTokensPerTool。后者天然包含 JSON 结构开销（字段名、role 包装、转义、tool_use 的
	// id/name），前者对这些一律计 0。
	//
	// 因为 InputTokens = totalInputTokens - CacheRead - CacheCreation（见
	// prepareCacheEmulationPlanFromProfile），不归一化就等于让分子分母各用一套口径，
	// cache_read 被系统性低估：tool_use / tool_result 密集的 Claude Code 流量里缺口可达
	// 25%~45%，即使前缀完全命中，cache_read/totalInputTokens 也只能到 55%~75%。
	//
	// 归一化后最后一个可缓存断点恰好映射为 totalInputTokens，靠前断点按累计占比等比缩放。
	// 这依赖「最后一个可缓存断点落在最后一个块上」，三条路径各有保证：
	//   - responses / chat_completions：applyDefaultCacheBreakpoints 显式在末块放断点。
	//   - Anthropic 且客户端下发了 cache_control：buildCacheProfileFromBlocks 的消息边界
	//     断点传播（一旦出现任一 cache_control，其后每个消息末尾都会补断点，而消息块恒排在
	//     tools/system 之后）。
	//   - Anthropic 且客户端未下发：buildCacheProfile 的兜底同样走
	//     applyDefaultCacheBreakpoints。
	scaleBreakpointsToInputTokens bool
	blocks                        []cacheBlock
	breakpoints                   []cacheBreakpoint
}

type cacheBlock struct {
	prefixFingerprint [32]byte
	cumulativeTokens  int
}

type cacheBreakpoint struct {
	blockIndex int
	ttl        time.Duration
}

type resolvedCacheBreakpoint struct {
	blockIndex       int
	cumulativeTokens int
	ttl              time.Duration
}

type pendingCacheBlock struct {
	value         any
	tokens        int
	breakpointTTL *time.Duration
	messageIndex  *int
	isMessageEnd  bool
}

func buildCacheProfile(ctx context.Context, body []byte, model string, inputTokens int) (*cacheProfile, bool) {
	return buildCacheProfileWithMin(ctx, body, model, inputTokens, minimumCacheableTokens(model))
}

func buildCacheProfileWithMin(ctx context.Context, body []byte, model string, inputTokens int, minCacheable int) (*cacheProfile, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	blocks := flattenCacheBlocks(ctx, payload)
	if len(blocks) == 0 {
		return nil, false
	}
	// 客户端未下发任何 cache_control 时兜底补断点。
	//
	// Anthropic 路径的断点原本完全依赖客户端：无 cache_control ⇒ 无断点 ⇒
	// lastCacheableBreakpoint 为 nil ⇒ buildCacheProfileFromBlocks 返回 false ⇒ usage
	// 为 nil，该请求零 cache_read 却仍计入命中率分母。Claude Code 会下发，但 curl、
	// Cherry Studio 等 OpenAI 风格客户端普遍不发，这批流量是纯粹的分母污染。
	//
	// 只在「完全没有客户端断点」时兜底，不与客户端断点混用：若客户端已标注，额外补断点等于
	// 在真实 API 不会缓存的位置模拟出 cache_read，会高估命中率、少计费。
	defaultedBreakpoints := false
	if !hasClientCacheBreakpoint(blocks) {
		applyDefaultCacheBreakpoints(blocks, cacheDefaultTTL)
		defaultedBreakpoints = true
	}
	totalTokens := inputTokens
	if totalTokens <= 0 {
		totalTokens = countKiroInputTokensFromPayload(ctx, payload)
	}
	prelude := map[string]any{
		"model":       payload["model"],
		"tool_choice": payload["tool_choice"],
	}
	profile, ok := buildCacheProfileFromBlocksWithMin(model, totalTokens, prelude, blocks, minCacheable)
	if ok {
		// 与 responses / chat_completions 路径对齐，理由见 scaleBreakpointsToInputTokens。
		profile.scaleBreakpointsToInputTokens = true
		profile.defaultedBreakpoints = defaultedBreakpoints
	}
	return profile, ok
}

func buildResponsesCacheProfile(ctx context.Context, body []byte, model string, inputTokens int) (*cacheProfile, bool) {
	return buildResponsesCacheProfileWithMin(ctx, body, model, inputTokens, minimumCacheableTokens(model))
}

func buildResponsesCacheProfileWithMin(ctx context.Context, body []byte, model string, inputTokens int, minCacheable int) (*cacheProfile, bool) {
	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	blocks := flattenResponsesCacheBlocks(ctx, payload)
	if len(blocks) == 0 {
		return nil, false
	}
	ttl := cacheDefaultTTL
	applyDefaultCacheBreakpoints(blocks, ttl)

	effectiveTools, err := apicompat.EffectiveResponsesTools(&req)
	if err != nil {
		return nil, false
	}
	effectiveModel := strings.TrimSpace(model)
	if effectiveModel == "" {
		effectiveModel, _ = payload["model"].(string)
	}
	totalTokens := inputTokens
	if totalTokens <= 0 {
		totalTokens = countKiroResponsesInputTokens(payload, blocks, len(effectiveTools))
	}
	prelude := map[string]any{
		"protocol":             "responses",
		"model":                effectiveModel,
		"tool_choice":          payload["tool_choice"],
		"tools":                kiroJSONCompatibleValue(effectiveTools),
		"prompt_cache_key":     payload["prompt_cache_key"],
		"previous_response_id": payload["previous_response_id"],
		"reasoning_effort":     kiroNestedValue(payload, "reasoning", "effort"),
		"text_format":          kiroNestedValue(payload, "text", "format"),
	}
	profile, ok := buildCacheProfileFromBlocksWithMin(effectiveModel, totalTokens, prelude, blocks, minCacheable)
	if ok {
		profile.scaleBreakpointsToInputTokens = true
		profile.defaultedBreakpoints = true
	}
	return profile, ok
}

func buildChatCompletionsCacheProfile(ctx context.Context, body []byte, model string, inputTokens int) (*cacheProfile, bool) {
	return buildChatCompletionsCacheProfileWithMin(ctx, body, model, inputTokens, minimumCacheableTokens(model))
}

func buildChatCompletionsCacheProfileWithMin(ctx context.Context, body []byte, model string, inputTokens int, minCacheable int) (*cacheProfile, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	blocks := flattenChatCompletionsCacheBlocks(ctx, payload)
	if len(blocks) == 0 {
		return nil, false
	}
	applyDefaultCacheBreakpoints(blocks, cacheDefaultTTL)

	effectiveModel := strings.TrimSpace(model)
	if effectiveModel == "" {
		effectiveModel, _ = payload["model"].(string)
	}
	tools, _ := payload["tools"].([]any)
	functions, _ := payload["functions"].([]any)
	totalTokens := inputTokens
	if totalTokens <= 0 {
		totalTokens = countKiroChatCompletionsInputTokens(payload, blocks, len(tools)+len(functions))
	}
	prelude := map[string]any{
		"protocol":            "chat_completions",
		"model":               effectiveModel,
		"instructions":        payload["instructions"],
		"tool_choice":         payload["tool_choice"],
		"function_call":       payload["function_call"],
		"tools":               kiroJSONCompatibleValue(payload["tools"]),
		"functions":           kiroJSONCompatibleValue(payload["functions"]),
		"parallel_tool_calls": payload["parallel_tool_calls"],
		"reasoning_effort":    payload["reasoning_effort"],
		"response_format":     kiroJSONCompatibleValue(payload["response_format"]),
	}
	profile, ok := buildCacheProfileFromBlocksWithMin(effectiveModel, totalTokens, prelude, blocks, minCacheable)
	if ok {
		profile.scaleBreakpointsToInputTokens = true
		profile.defaultedBreakpoints = true
	}
	return profile, ok
}

func buildCacheProfileFromBlocks(model string, totalTokens int, preludeValue any, blocks []pendingCacheBlock) (*cacheProfile, bool) {
	return buildCacheProfileFromBlocksWithMin(model, totalTokens, preludeValue, blocks, minimumCacheableTokens(model))
}

func buildCacheProfileFromBlocksWithMin(model string, totalTokens int, preludeValue any, blocks []pendingCacheBlock, minCacheable int) (*cacheProfile, bool) {
	prelude, err := canonicalJSON(preludeValue)
	if err != nil {
		return nil, false
	}
	prefixState := make([]byte, 8+len(prelude))
	binary.BigEndian.PutUint64(prefixState[:8], uint64(len(prelude)))
	copy(prefixState[8:], prelude)

	profile := &cacheProfile{
		totalInputTokens:    max(totalTokens, 0),
		runtimeInputTokens:  max(totalTokens, 0),
		reportedInputTokens: max(totalTokens, 0),
		minCacheable:        max(minCacheable, 0),
		model:               strings.TrimSpace(model),
		preludeCanonical:    append([]byte(nil), prelude...),
		pendingBlocks:       append([]pendingCacheBlock(nil), blocks...),
	}
	cumulativeTokens := 0
	var activeTTL *time.Duration
	seenBreakpoints := make(map[int]struct{})
	for index, block := range blocks {
		cumulativeTokens += max(block.tokens, 0)
		blockJSON, err := canonicalJSON(block.value)
		if err != nil {
			return nil, false
		}
		blockHash := sha256.Sum256(blockJSON)
		h := sha256.New()
		_, _ = h.Write(prefixState)
		_, _ = h.Write(blockHash[:])
		prefixFingerprint := [32]byte(h.Sum(nil))
		prefixState = prefixFingerprint[:]
		profile.blocks = append(profile.blocks, cacheBlock{prefixFingerprint: prefixFingerprint, cumulativeTokens: cumulativeTokens})

		if block.breakpointTTL != nil {
			ttl := minDuration(*block.breakpointTTL, cacheMaxSupportedTTL)
			activeTTL = &ttl
			if _, ok := seenBreakpoints[index]; !ok {
				profile.breakpoints = append(profile.breakpoints, cacheBreakpoint{blockIndex: index, ttl: ttl})
				seenBreakpoints[index] = struct{}{}
			}
		}
		if block.isMessageEnd && block.messageIndex != nil && activeTTL != nil {
			if _, ok := seenBreakpoints[index]; !ok {
				profile.breakpoints = append(profile.breakpoints, cacheBreakpoint{blockIndex: index, ttl: *activeTTL})
				seenBreakpoints[index] = struct{}{}
			}
		}
	}
	if profile.lastCacheableBreakpoint() == nil {
		return nil, false
	}
	return profile, true
}

func flattenCacheBlocks(ctx context.Context, payload map[string]any) []pendingCacheBlock {
	var blocks []pendingCacheBlock
	if tools, ok := payload["tools"].([]any); ok {
		for toolIndex, tool := range tools {
			value := stripCacheControl(tool)
			blocks = append(blocks, pendingCacheBlock{
				value: map[string]any{"kind": "tool", "tool_index": toolIndex, "tool": value},
				// 工具块按序列化后的真实 token 计数，不用 kiroTokensPerTool 拍平值。
				//
				// 真实 Claude Code 工具 schema 单个 300~1500 token，15 个工具实测约 21000，
				// 而拍平估算只给 15*150=2250，低估约 9 倍。后果不是「命中率略低」而是
				// 「整个 profile 被拒」：cacheableBreakpoints 用累计值与 minCacheable 比较，
				// opus 系阈值 4096（见 minimumCacheableTokens，该值是被
				// TestKiroMinimumCacheableTokens 钉死的显式契约，不应为此调低）。带 tools 的
				// opus 短请求累计值卡在 2000 上下 → lastCacheableBreakpoint 为 nil →
				// buildCacheProfileFromBlocks 返回 false → 该请求零缓存，却仍然计入
				// 命中率分母。
				//
				// 只改这里（B 侧）不动 countKiroInputTokensFromPayload 等计数器（T 侧）：
				// P1 归一化后命中率 = B_matched/B_last，T 被约掉，所以修正 B 即可解决问题；
				// 而 T 还喂给计费兜底与 count_tokens 对外契约，改动会牵连账单。两侧工具口径
				// 因此不一致，但归一化把 B_last 精确映射到 T，该差异不会外泄。
				tokens:        countKiroSerializedValueTokens(value),
				breakpointTTL: extractCacheTTL(tool),
			})
		}
	}
	for systemIndex, systemBlock := range normalizeKiroSystemBlocks(payload["system"]) {
		value := stripCacheControl(systemBlock)
		canonicalizeKiroSystemBlock(value)
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "system", "system_index": systemIndex, "block": value},
			tokens: countKiroSystemBlockTokens(systemBlock), breakpointTTL: extractCacheTTL(systemBlock),
		})
	}
	messages, _ := payload["messages"].([]any)
	for messageIndex, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		role, _ := message["role"].(string)
		content := message["content"]
		switch typed := content.(type) {
		case string:
			mi := messageIndex
			block := map[string]any{"type": "text", "text": typed}
			blocks = append(blocks, pendingCacheBlock{
				value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "block_index": 0, "block": block},
				tokens: countKiroMessageContentTokens(ctx, block), messageIndex: &mi, isMessageEnd: true,
			})
		case []any:
			lastBlockIndex := len(typed) - 1
			for blockIndex, rawBlock := range typed {
				mi := messageIndex
				value := stripCacheControl(rawBlock)
				blocks = append(blocks, pendingCacheBlock{
					value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "block_index": blockIndex, "block": value},
					tokens: countKiroMessageContentTokens(ctx, rawBlock), breakpointTTL: extractCacheTTL(rawBlock), messageIndex: &mi, isMessageEnd: blockIndex == lastBlockIndex,
				})
			}
		}
	}
	return blocks
}

func flattenResponsesCacheBlocks(ctx context.Context, payload map[string]any) []pendingCacheBlock {
	var blocks []pendingCacheBlock
	if instructions, ok := payload["instructions"].(string); strings.TrimSpace(instructions) != "" && ok {
		block := map[string]any{"type": "input_text", "text": strings.TrimSpace(instructions)}
		blocks = append(blocks, pendingCacheBlock{
			value:        map[string]any{"kind": "instructions", "block": block},
			tokens:       countKiroMessageContentTokens(ctx, block),
			isMessageEnd: true,
		})
	}

	switch input := payload["input"].(type) {
	case string:
		block := map[string]any{"type": "input_text", "text": input}
		blocks = append(blocks, pendingCacheBlock{
			value:        map[string]any{"kind": "input", "input_index": 0, "role": "user", "block_index": 0, "block": block},
			tokens:       countKiroMessageContentTokens(ctx, block),
			isMessageEnd: true,
		})
	case []any:
		cacheableItemCount := countResponsesCacheableInputItems(input)
		seenCacheableItems := 0
		for itemIndex, rawItem := range input {
			if !isResponsesCacheableInputItem(rawItem) {
				blocks = appendKiroResponsesInputItemBlocks(ctx, blocks, itemIndex, rawItem, false)
				continue
			}
			seenCacheableItems++
			isStableHistoryItem := seenCacheableItems < cacheableItemCount
			blocks = appendKiroResponsesInputItemBlocks(ctx, blocks, itemIndex, rawItem, isStableHistoryItem)
		}
	}
	return blocks
}

func flattenChatCompletionsCacheBlocks(ctx context.Context, payload map[string]any) []pendingCacheBlock {
	var blocks []pendingCacheBlock
	if instructions, ok := payload["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		block := map[string]any{"type": "input_text", "text": strings.TrimSpace(instructions)}
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "instructions", "block": block},
			tokens: countKiroMessageContentTokens(ctx, block),
		})
	}

	messages, _ := payload["messages"].([]any)
	for messageIndex, rawMessage := range messages {
		markMessageEnd := isChatCompletionsCacheableMessage(rawMessage)
		blocks = appendKiroChatCompletionsMessageBlocks(ctx, blocks, messageIndex, rawMessage, markMessageEnd)
	}
	return blocks
}

func isChatCompletionsCacheableMessage(rawMessage any) bool {
	message, ok := rawMessage.(map[string]any)
	if !ok {
		return false
	}
	role, _ := message["role"].(string)
	return strings.TrimSpace(role) != ""
}

func appendKiroChatCompletionsMessageBlocks(ctx context.Context, blocks []pendingCacheBlock, messageIndex int, rawMessage any, markMessageEnd bool) []pendingCacheBlock {
	start := len(blocks)
	message, ok := rawMessage.(map[string]any)
	if !ok {
		return blocks
	}
	role, _ := message["role"].(string)
	name, _ := message["name"].(string)

	if content, ok := message["content"]; ok {
		blocks = appendKiroChatCompletionsContentBlocks(ctx, blocks, messageIndex, role, name, content)
	}
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		block := map[string]any{"type": "reasoning", "thinking": reasoning}
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "message_reasoning", "message_index": messageIndex, "role": role, "name": name, "block": block},
			tokens: countKiroMessageContentTokens(ctx, block),
		})
	}
	if toolCalls, ok := message["tool_calls"]; ok {
		value := kiroJSONCompatibleValue(toolCalls)
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "message_tool_calls", "message_index": messageIndex, "role": role, "name": name, "block": value},
			tokens: countKiroSerializedValueTokens(value),
		})
	}
	if toolCallID, ok := message["tool_call_id"].(string); ok && toolCallID != "" {
		value := map[string]any{"tool_call_id": toolCallID}
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "message_tool_call_id", "message_index": messageIndex, "role": role, "name": name, "block": value},
			tokens: countKiroSerializedValueTokens(value),
		})
	}
	if functionCall, ok := message["function_call"]; ok {
		value := kiroJSONCompatibleValue(functionCall)
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "message_function_call", "message_index": messageIndex, "role": role, "name": name, "block": value},
			tokens: countKiroSerializedValueTokens(value),
		})
	}
	return markKiroResponsesInputItemEnd(blocks, start, messageIndex, markMessageEnd)
}

func appendKiroChatCompletionsContentBlocks(ctx context.Context, blocks []pendingCacheBlock, messageIndex int, role string, name string, content any) []pendingCacheBlock {
	switch typed := content.(type) {
	case string:
		block := map[string]any{"type": "text", "text": typed}
		return append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "name": name, "block_index": 0, "block": block},
			tokens: countKiroMessageContentTokens(ctx, block),
		})
	case []any:
		for blockIndex, rawBlock := range typed {
			block := rawBlock
			if text, ok := rawBlock.(string); ok {
				block = map[string]any{"type": "text", "text": text}
			}
			blocks = append(blocks, pendingCacheBlock{
				value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "name": name, "block_index": blockIndex, "block": kiroJSONCompatibleValue(block)},
				tokens: countKiroMessageContentTokens(ctx, block),
			})
		}
	case map[string]any:
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "name": name, "block_index": 0, "block": kiroJSONCompatibleValue(typed)},
			tokens: countKiroMessageContentTokens(ctx, typed),
		})
	}
	return blocks
}

// applyDefaultCacheBreakpoints 在每个消息末尾与最后一个块上放置断点。
//
// 注意它会无条件覆写 breakpointTTL。responses / chat_completions 是 OpenAI 形态、客户端
// 不下发 cache_control，无值可覆盖；Anthropic 路径必须先用 hasClientCacheBreakpoint
// 判空后才可调用，否则会把客户端显式的 ttl:"1h" 降级成默认 5m。
func applyDefaultCacheBreakpoints(blocks []pendingCacheBlock, ttl time.Duration) {
	if len(blocks) == 0 {
		return
	}
	for i := range blocks {
		if blocks[i].isMessageEnd {
			blocks[i].breakpointTTL = &ttl
		}
	}
	// Keep a stable prelude cacheable even when the request has no historical
	// message boundary (for example, system/instructions plus the first user
	// turn). The final dynamic user turn is intentionally handled separately.
	lastMessage := -1
	for _, block := range blocks {
		if block.messageIndex != nil && *block.messageIndex > lastMessage {
			lastMessage = *block.messageIndex
		}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].messageIndex == nil || lastMessage < 0 || *blocks[i].messageIndex < lastMessage {
			blocks[i].breakpointTTL = &ttl
			break
		}
	}
	blocks[len(blocks)-1].breakpointTTL = &ttl
}

// hasClientCacheBreakpoint 判断客户端是否下发了任何 cache_control 断点。
func hasClientCacheBreakpoint(blocks []pendingCacheBlock) bool {
	for i := range blocks {
		if blocks[i].breakpointTTL != nil {
			return true
		}
	}
	return false
}

func countResponsesCacheableInputItems(input []any) int {
	count := 0
	for _, rawItem := range input {
		if isResponsesCacheableInputItem(rawItem) {
			count++
		}
	}
	return count
}

func isResponsesCacheableInputItem(rawItem any) bool {
	if _, ok := rawItem.(string); ok {
		return true
	}
	item, ok := rawItem.(map[string]any)
	if !ok {
		return false
	}
	itemType, _ := item["type"].(string)
	return itemType != "additional_tools"
}

func appendKiroResponsesInputItemBlocks(ctx context.Context, blocks []pendingCacheBlock, itemIndex int, rawItem any, markMessageEnd bool) []pendingCacheBlock {
	start := len(blocks)
	if text, ok := rawItem.(string); ok {
		block := map[string]any{"type": "input_text", "text": text}
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": "user", "block_index": 0, "block": block},
			tokens: countKiroMessageContentTokens(ctx, block),
		})
		return markKiroResponsesInputItemEnd(blocks, start, itemIndex, markMessageEnd)
	}
	item, ok := rawItem.(map[string]any)
	if !ok {
		return blocks
	}
	itemType, _ := item["type"].(string)
	if itemType == "additional_tools" {
		return blocks
	}
	role, _ := item["role"].(string)
	if role == "" && (itemType == "message" || itemType == "") {
		role = "user"
	}
	if content, ok := item["content"]; ok {
		blocks = appendKiroResponsesContentBlocks(ctx, blocks, itemIndex, role, content)
		return markKiroResponsesInputItemEnd(blocks, start, itemIndex, markMessageEnd)
	}
	value := kiroJSONCompatibleValue(item)
	blocks = append(blocks, pendingCacheBlock{
		value:  map[string]any{"kind": "input_item", "input_index": itemIndex, "type": itemType, "block": value},
		tokens: countKiroSerializedValueTokens(value),
	})
	return markKiroResponsesInputItemEnd(blocks, start, itemIndex, markMessageEnd)
}

func markKiroResponsesInputItemEnd(blocks []pendingCacheBlock, start, itemIndex int, markMessageEnd bool) []pendingCacheBlock {
	if !markMessageEnd || len(blocks) <= start {
		return blocks
	}
	last := len(blocks) - 1
	mi := itemIndex
	blocks[last].messageIndex = &mi
	blocks[last].isMessageEnd = true
	return blocks
}

func appendKiroResponsesContentBlocks(ctx context.Context, blocks []pendingCacheBlock, itemIndex int, role string, content any) []pendingCacheBlock {
	switch typed := content.(type) {
	case string:
		block := map[string]any{"type": "input_text", "text": typed}
		return append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": role, "block_index": 0, "block": block},
			tokens: countKiroMessageContentTokens(ctx, block),
		})
	case []any:
		for blockIndex, rawBlock := range typed {
			block := rawBlock
			if text, ok := rawBlock.(string); ok {
				block = map[string]any{"type": "input_text", "text": text}
			}
			blocks = append(blocks, pendingCacheBlock{
				value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": role, "block_index": blockIndex, "block": kiroJSONCompatibleValue(block)},
				tokens: countKiroMessageContentTokens(ctx, block),
			})
		}
	case map[string]any:
		blocks = append(blocks, pendingCacheBlock{
			value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": role, "block_index": 0, "block": kiroJSONCompatibleValue(typed)},
			tokens: countKiroMessageContentTokens(ctx, typed),
		})
	}
	return blocks
}

func countKiroResponsesInputTokens(payload map[string]any, blocks []pendingCacheBlock, toolCount int) int {
	tokens := toolCount * kiroTokensPerTool
	for _, block := range blocks {
		tokens += max(block.tokens, 0)
	}
	if input, ok := payload["input"].([]any); ok {
		tokens += len(input) * kiroTokensPerMessage
	}
	return max(tokens, 1)
}

func countKiroChatCompletionsInputTokens(payload map[string]any, blocks []pendingCacheBlock, toolCount int) int {
	tokens := toolCount * kiroTokensPerTool
	for _, block := range blocks {
		tokens += max(block.tokens, 0)
	}
	if messages, ok := payload["messages"].([]any); ok {
		tokens += len(messages) * kiroTokensPerMessage
	}
	return max(tokens, 1)
}

func kiroNestedValue(payload map[string]any, keys ...string) any {
	var current any = payload
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

func kiroJSONCompatibleValue(value any) any {
	b, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return value
	}
	return out
}

func normalizeKiroSystemBlocks(system any) []any {
	switch typed := system.(type) {
	case nil:
		return nil
	case string:
		return []any{map[string]any{"type": "text", "text": typed}}
	case []any:
		return typed
	default:
		return []any{typed}
	}
}

func canonicalizeKiroSystemBlock(value any) {
	obj, ok := value.(map[string]any)
	if !ok {
		return
	}
	blockType, _ := obj["type"].(string)
	if blockType != "" && blockType != "text" {
		return
	}
	text, _ := obj["text"].(string)
	if strings.HasPrefix(text, "x-anthropic-billing-header:") {
		obj["text"] = "__anthropic_billing_header__"
	}
}

func extractCacheTTL(value any) *time.Duration {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	cc, ok := obj["cache_control"].(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(cacheAsString(cc["type"])), "ephemeral") {
		return nil
	}
	ttl := cacheDefaultTTL
	if strings.EqualFold(strings.TrimSpace(cacheAsString(cc["ttl"])), "1h") {
		ttl = cacheOneHourTTL
	}
	return &ttl
}

func (p *cacheProfile) cacheableBreakpoints() []resolvedCacheBreakpoint {
	if p == nil {
		return nil
	}
	resolved := make([]resolvedCacheBreakpoint, 0, len(p.breakpoints))
	for _, breakpoint := range p.breakpoints {
		if breakpoint.blockIndex < 0 || breakpoint.blockIndex >= len(p.blocks) {
			continue
		}
		block := p.blocks[breakpoint.blockIndex]
		if block.cumulativeTokens < p.minCacheable {
			continue
		}
		resolved = append(resolved, resolvedCacheBreakpoint{blockIndex: breakpoint.blockIndex, cumulativeTokens: block.cumulativeTokens, ttl: breakpoint.ttl})
	}
	return resolved
}

func (p *cacheProfile) lastCacheableBreakpoint() *resolvedCacheBreakpoint {
	breakpoints := p.cacheableBreakpoints()
	if len(breakpoints) == 0 {
		return nil
	}
	last := breakpoints[len(breakpoints)-1]
	return &last
}

func (t *cacheTracker) compute(cacheKey uint64, profile *cacheProfile) *cacheEmulationUsage {
	out := &cacheEmulationUsage{}
	if t == nil || profile == nil || cacheKey == 0 {
		return out
	}
	lastBreakpoint := profile.lastCacheableBreakpoint()
	if lastBreakpoint == nil {
		return out
	}
	lastBreakpointTokens := profile.cacheTokensForBreakpoint(lastBreakpoint.cumulativeTokens)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)

	matchedTokens := 0
	if accountEntries := t.entries[cacheKey]; accountEntries != nil {
		breakpoints := profile.cacheableBreakpoints()
		// The common case is a hit in one of the newest breakpoints, so keep
		// the bounded lookback as the fast path. Creation limits can, however,
		// persist only an older breakpoint (for example the first complete
		// block of a large request). If the fast path misses, scan the older
		// breakpoints as a correctness fallback; otherwise a valid cached
		// prefix would be reported as a cold request solely because the
		// profile contains more than cachePrefixLookbackLimit boundaries.
		lookup := func(from, to int) bool {
			for i := from; i >= to; i-- {
				breakpoint := breakpoints[i]
				candidate := profile.blocks[breakpoint.blockIndex]
				entry, ok := accountEntries[candidate.prefixFingerprint]
				if !ok || !entry.expiresAt.After(now) {
					continue
				}
				// 只续期命中的这一条即可：整条前缀链的续期由 commit() → update() 完成，
				// 它会遍历当前 profile 的全部 cacheableBreakpoints 并推后 expiresAt，而命中点
				// 之前的断点都属于当前 profile。此处再遍历一遍是冗余的。
				entry.expiresAt = now.Add(entry.ttl)
				entry.lastUsedAt = now
				accountEntries[candidate.prefixFingerprint] = entry
				matchedTokens = profile.cacheTokensForBreakpoint(breakpoint.cumulativeTokens)
				return true
			}
			return false
		}
		if len(breakpoints) > 0 {
			fastStart := len(breakpoints) - 1
			fastEnd := max(len(breakpoints)-cachePrefixLookbackLimit, 0)
			found := lookup(fastStart, fastEnd)
			if !found && fastEnd > 0 {
				lookup(fastEnd-1, 0)
			}
		}
	}
	newTokens := max(lastBreakpointTokens-matchedTokens, 0)
	out.CacheReadInputTokens = max(matchedTokens, 0)
	out.CacheCreationInputTokens = newTokens
	out.CacheCreation5mInputTokens, out.CacheCreation1hInputTokens = profile.ttlBreakdown(matchedTokens)
	return out
}

func (p *cacheProfile) cacheTokensForBreakpoint(cumulativeTokens int) int {
	if p == nil {
		return 0
	}
	if !p.scaleBreakpointsToInputTokens {
		return min(max(cumulativeTokens, 0), p.runtimeInputTokens)
	}
	lastBreakpoint := p.lastCacheableBreakpoint()
	if lastBreakpoint == nil || lastBreakpoint.cumulativeTokens <= 0 {
		return min(max(cumulativeTokens, 0), p.runtimeInputTokens)
	}
	scaled := int(math.Round(float64(max(cumulativeTokens, 0)) * float64(p.runtimeInputTokens) / float64(lastBreakpoint.cumulativeTokens)))
	if p.policyEnabled {
		scaled = scaleCacheTokens(scaled, p.policy.CoverageRatio)
		if p.policy.MaxCoverageTokens > 0 {
			scaled = min(scaled, p.policy.MaxCoverageTokens)
		}
	}
	return min(max(scaled, 0), p.runtimeInputTokens)
}

func (p *cacheProfile) ttlBreakdown(matchedTokens int) (int, int) {
	lastBreakpoint := p.lastCacheableBreakpoint()
	if lastBreakpoint == nil {
		return 0, 0
	}
	newTokens := max(p.cacheTokensForBreakpoint(lastBreakpoint.cumulativeTokens)-matchedTokens, 0)
	if newTokens == 0 {
		return 0, 0
	}
	if lastBreakpoint.ttl >= cacheOneHourTTL {
		return 0, newTokens
	}
	return newTokens, 0
}

func (t *cacheTracker) update(cacheKey uint64, profile *cacheProfile) {
	if t == nil || profile == nil || cacheKey == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	accountEntries := t.entries[cacheKey]
	if accountEntries == nil {
		accountEntries = make(map[[32]byte]cacheEntry)
		t.entries[cacheKey] = accountEntries
	}
	for _, breakpoint := range profile.cacheableBreakpoints() {
		block := profile.blocks[breakpoint.blockIndex]
		expiresAt := now.Add(breakpoint.ttl)
		entry, ok := accountEntries[block.prefixFingerprint]
		if ok {
			entry.tokens = max(entry.tokens, block.cumulativeTokens)
			entry.ttl = maxDuration(entry.ttl, breakpoint.ttl)
			if expiresAt.After(entry.expiresAt) {
				entry.expiresAt = expiresAt
			}
			entry.lastUsedAt = now
			if profile.policy.ExpireAfterIdleSeconds > 0 {
				entry.expireAfterIdleSec = profile.policy.ExpireAfterIdleSeconds
			}
			accountEntries[block.prefixFingerprint] = entry
			continue
		}
		accountEntries[block.prefixFingerprint] = cacheEntry{
			tokens:             block.cumulativeTokens,
			ttl:                breakpoint.ttl,
			expiresAt:          expiresAt,
			lastUsedAt:         now,
			estimatedBytes:     64,
			expireAfterIdleSec: profile.policy.ExpireAfterIdleSeconds,
		}
	}
	t.enforceBoundsLocked(cacheKey, profile.policy)
}

func (t *cacheTracker) controlState(cacheKey uint64) cacheControlState {
	if t == nil || cacheKey == 0 {
		return cacheControlState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.controls == nil {
		t.controls = make(map[uint64]cacheControlState)
	}
	state := t.controls[cacheKey]
	if state.windowStart.IsZero() {
		state.windowStart = time.Now()
		t.controls[cacheKey] = state
	}
	return state
}

func (t *cacheTracker) recordSuccess(cacheKey uint64, creationTokens int) {
	if t == nil || cacheKey == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.controls == nil {
		t.controls = make(map[uint64]cacheControlState)
	}
	state := t.controls[cacheKey]
	state.successfulRequests++
	if creationTokens > 0 {
		state.lastCreationAt = now
		state.successfulSinceCreation = 0
	} else {
		state.successfulSinceCreation++
	}
	if state.windowStart.IsZero() {
		state.windowStart = now
	}
	state.windowTokens += max(creationTokens, 0)
	t.controls[cacheKey] = state
}

func applyCacheCreationControl(cacheKey uint64, control CacheCreationControl, usage *cacheEmulationUsage) {
	if usage == nil || !control.Enabled || usage.CacheCreationInputTokens <= 0 {
		return
	}
	state := globalCacheTracker.controlState(cacheKey)
	now := time.Now()
	if control.MinSuccessfulRequestsBetween > 0 &&
		!state.lastCreationAt.IsZero() &&
		state.successfulSinceCreation < control.MinSuccessfulRequestsBetween {
		usage.CacheCreationInputTokens = 0
		usage.CacheCreation5mInputTokens = 0
		usage.CacheCreation1hInputTokens = 0
		return
	}
	if control.MinCreationIntervalSeconds > 0 &&
		!state.lastCreationAt.IsZero() &&
		now.Sub(state.lastCreationAt) < time.Duration(control.MinCreationIntervalSeconds)*time.Second {
		usage.CacheCreationInputTokens = 0
		usage.CacheCreation5mInputTokens = 0
		usage.CacheCreation1hInputTokens = 0
		return
	}
	if control.MaxCreationTokensPerEvent > 0 && usage.CacheCreationInputTokens > control.MaxCreationTokensPerEvent {
		usage.CacheCreationInputTokens = control.MaxCreationTokensPerEvent
		usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens =
			scaleCacheCreationTTLToTotal(usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
	if control.CreationBudgetWindowSeconds > 0 {
		window := time.Duration(control.CreationBudgetWindowSeconds) * time.Second
		if state.windowStart.IsZero() || now.Sub(state.windowStart) >= window {
			state.windowStart = now
			state.windowTokens = 0
			globalCacheTracker.setControlState(cacheKey, state)
		}
	}
	if control.MaxCreationTokensPerWindow > 0 {
		remaining := control.MaxCreationTokensPerWindow - state.windowTokens
		if remaining <= 0 {
			usage.CacheCreationInputTokens = 0
			usage.CacheCreation5mInputTokens = 0
			usage.CacheCreation1hInputTokens = 0
		} else if usage.CacheCreationInputTokens > remaining {
			usage.CacheCreationInputTokens = remaining
			usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens =
				scaleCacheCreationTTLToTotal(usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
		}
	}
}

func (t *cacheTracker) setControlState(cacheKey uint64, state cacheControlState) {
	if t == nil || cacheKey == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.controls == nil {
		t.controls = make(map[uint64]cacheControlState)
	}
	t.controls[cacheKey] = state
}

func (t *cacheTracker) pruneLocked(now time.Time) {
	for cacheKey, accountEntries := range t.entries {
		for fp, entry := range accountEntries {
			if !entry.expiresAt.After(now) {
				delete(accountEntries, fp)
				continue
			}
			if entry.expireAfterIdleSec > 0 && !entry.lastUsedAt.IsZero() &&
				now.Sub(entry.lastUsedAt) >= time.Duration(entry.expireAfterIdleSec)*time.Second {
				delete(accountEntries, fp)
			}
		}
		if len(accountEntries) == 0 {
			delete(t.entries, cacheKey)
		}
	}
	if t.controls != nil {
		for key, state := range t.controls {
			if state.windowStart.IsZero() {
				continue
			}
			if state.successfulRequests == 0 && state.lastCreationAt.IsZero() && now.Sub(state.windowStart) > time.Hour {
				delete(t.controls, key)
			}
		}
	}
}

func (t *cacheTracker) enforceBoundsLocked(cacheKey uint64, policy CacheStrategyConfig) {
	if t == nil {
		return
	}
	entries := t.entries[cacheKey]
	if policy.MaxEntriesPerScope > 0 && len(entries) > policy.MaxEntriesPerScope {
		removeLeastRecentlyUsed(entries, len(entries)-policy.MaxEntriesPerScope)
	}
	if policy.MaxEntriesGlobal <= 0 && policy.EstimatedBytesLimit <= 0 {
		return
	}

	type entryRef struct {
		key       uint64
		fp        [32]byte
		lastUsed  time.Time
		estimated int64
	}
	all := make([]entryRef, 0)
	var totalBytes int64
	for key, scopeEntries := range t.entries {
		for fp, entry := range scopeEntries {
			estimated := entry.estimatedBytes
			if estimated <= 0 {
				estimated = 64
			}
			all = append(all, entryRef{key: key, fp: fp, lastUsed: entry.lastUsedAt, estimated: estimated})
			totalBytes += estimated
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].lastUsed.Before(all[j].lastUsed) })
	remove := 0
	for len(all)-remove > policy.MaxEntriesGlobal && policy.MaxEntriesGlobal > 0 {
		ref := all[remove]
		delete(t.entries[ref.key], ref.fp)
		remove++
		totalBytes -= ref.estimated
	}
	for totalBytes > policy.EstimatedBytesLimit && remove < len(all) && policy.EstimatedBytesLimit > 0 {
		ref := all[remove]
		if scope, ok := t.entries[ref.key]; ok {
			if _, exists := scope[ref.fp]; exists {
				delete(scope, ref.fp)
				totalBytes -= ref.estimated
			}
		}
		remove++
	}
	for key, scope := range t.entries {
		if len(scope) == 0 {
			delete(t.entries, key)
		}
	}
}

func removeLeastRecentlyUsed(entries map[[32]byte]cacheEntry, count int) {
	if count <= 0 || len(entries) == 0 {
		return
	}
	type ref struct {
		fp       [32]byte
		lastUsed time.Time
	}
	refs := make([]ref, 0, len(entries))
	for fp, entry := range entries {
		refs = append(refs, ref{fp: fp, lastUsed: entry.lastUsedAt})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].lastUsed.Before(refs[j].lastUsed) })
	if count > len(refs) {
		count = len(refs)
	}
	for _, item := range refs[:count] {
		delete(entries, item.fp)
	}
}

// cacheCredentialKey 返回模拟缓存 tracker 的一级命名空间键，按账号维度隔离。
//
// 这里必须用 account.ID，不能用凭证内容拼接，也不能用 kiropkg.BuildAccountKey：
//   - refresh_token 会轮转。上游返回非空即覆盖写回（见 pkg/kiro/oauth.go 的 RefreshToken
//     与 KiroOAuthService.BuildAccountCredentials），刷新窗口 kiroRefreshWindow=15min、
//     access_token 典型 1h 有效，即每小时至少一次。凭证一旦参与计算，键就随之改变，该账号
//     已积累的全部前缀指纹一次性作废、退回冷启动，命中率被反复打回。
//   - client_id_hash / client_id 是「OAuth 客户端应用」标识，同一应用注册被多账号共用，
//     会大量重复。用它做键（BuildAccountKey 的优先级短路正是先取它）会把不同账号合并进
//     同一命名空间，产生跨账号误命中：cache_read 按 1/10 价计费而上游实际全价，属少计费，
//     比冷启动严重得多。
//
// account.ID 同时满足三个必要属性：轮转时稳定、账号间唯一、恒定存在。调用方
// prepareCacheEmulationPlanFromProfile 及三个 prepare 入口均已前置校验 account.ID > 0，
// 故此处无需兜底分支。
//
// 取舍：同一上游账号若被导入成两行 accounts 记录，两者不再共享缓存，会多算一次
// cache_creation、少算 cache_read——偏保守（多计费），方向上优于误命中导致的少计费。
func cacheCredentialKey(account *Account) uint64 {
	if account == nil || account.ID <= 0 {
		return 0
	}
	h := fnv.New64a()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(account.ID))
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

func cacheCredentialIdentity(account *Account) string {
	if account == nil {
		return ""
	}
	parts := make([]string, 0, 8)
	for _, key := range []string{"client_id_hash", "client_id", "refresh_token", "profile_arn", "kiro_api_key", "kiroApiKey", "api_key"} {
		if value := strings.TrimSpace(account.GetCredential(key)); value != "" {
			parts = append(parts, key+":"+value)
		}
	}
	if len(parts) == 0 && account.ID > 0 {
		parts = append(parts, "account:"+fmt.Sprint(account.ID))
	}
	return strings.Join(parts, "|")
}

// minimumCacheableTokens 返回「前缀至少多少 token 才值得记进缓存」的阈值。
// 目前只有两档特例：GPT-5.6 系列取 1024（对齐 OpenAI 官方最小缓存粒度），opus 系取
// 4096；其余 Kiro 模型走默认档。GPT 用 kiropkg.IsKiroGPTModel 精确匹配，opus 仍用
// 子串以覆盖 -thinking 与带日期后缀的变体。
// 各模型的期望值由 TestKiroMinimumCacheableTokens 钉死。
func minimumCacheableTokens(model string) int {
	if kiropkg.IsKiroGPTModel(model) {
		return cacheMinTokensGPT
	}
	if strings.Contains(strings.ToLower(model), "opus") {
		return cacheMinTokensOpus
	}
	return cacheMinTokensDefault
}

func stripCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if k == "cache_control" {
				continue
			}
			out[k] = stripCacheControl(child)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = stripCacheControl(child)
		}
		return out
	default:
		return v
	}
}

func countKiroInputTokensFromPayload(ctx context.Context, payload map[string]any) int {
	if payload == nil {
		return 1
	}
	tokens := 0
	for _, block := range normalizeKiroSystemBlocks(payload["system"]) {
		tokens += countKiroSystemBlockTokens(block)
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) > 0 {
		sanitizedMessages, imageTokens := sanitizeKiroImagesForTokenEstimate(ctx, messages)
		canonical, err := canonicalJSON(sanitizedMessages)
		if err == nil {
			tokens += anthropictokenizer.CountTokens(string(canonical))
		}
		tokens += imageTokens
		tokens += len(messages) * kiroTokensPerMessage
	}
	if tools, ok := payload["tools"].([]any); ok {
		tokens += len(tools) * kiroTokensPerTool
	}
	return max(tokens, 1)
}

func countKiroSystemBlockTokens(value any) int {
	switch typed := value.(type) {
	case string:
		return anthropictokenizer.CountTokens(typed)
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return anthropictokenizer.CountTokens(text)
		}
		return 0
	default:
		return 0
	}
}

func countKiroMessageContentTokens(ctx context.Context, value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		return anthropictokenizer.CountTokens(typed)
	case []any:
		total := 0
		for _, item := range typed {
			total += countKiroMessageContentTokens(ctx, item)
		}
		return total
	case map[string]any:
		if mediaType, source, ok := kiroImageTokenSource(typed); ok {
			return kiropkg.EstimateImageTokens(ctx, mediaType, source)
		}
		if text, ok := typed["text"].(string); ok {
			return anthropictokenizer.CountTokens(text)
		}
		if thinking, ok := typed["thinking"].(string); ok {
			return anthropictokenizer.CountTokens(thinking)
		}
		if input, ok := typed["input"]; ok {
			return countKiroSerializedValueTokens(input)
		}
		if content, ok := typed["content"]; ok {
			return countKiroMessageContentTokens(ctx, content)
		}
		return 0
	default:
		return 0
	}
}

func sanitizeKiroImagesForTokenEstimate(ctx context.Context, value any) (any, int) {
	switch typed := value.(type) {
	case []any:
		out := make([]any, len(typed))
		tokens := 0
		for i, item := range typed {
			out[i], tokens = sanitizeKiroImageItem(ctx, item, tokens)
		}
		return out, tokens
	case map[string]any:
		if mediaType, source, ok := kiroImageTokenSource(typed); ok {
			return sanitizeKiroImageBlock(typed), kiropkg.EstimateImageTokens(ctx, mediaType, source)
		}
		out := make(map[string]any, len(typed))
		tokens := 0
		for key, item := range typed {
			out[key], tokens = sanitizeKiroImageItem(ctx, item, tokens)
		}
		return out, tokens
	default:
		return value, 0
	}
}

func sanitizeKiroImageItem(ctx context.Context, value any, currentTokens int) (any, int) {
	sanitized, tokens := sanitizeKiroImagesForTokenEstimate(ctx, value)
	return sanitized, currentTokens + tokens
}

func sanitizeKiroImageBlock(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		switch typed := item.(type) {
		case map[string]any:
			out[key] = sanitizeKiroImageBlock(typed)
		case []any:
			items := make([]any, len(typed))
			for i, child := range typed {
				if childMap, ok := child.(map[string]any); ok {
					items[i] = sanitizeKiroImageBlock(childMap)
				} else {
					items[i] = child
				}
			}
			out[key] = items
		case string:
			lowerKey := strings.ToLower(key)
			if lowerKey == "data" || lowerKey == "url" || lowerKey == "image_url" {
				out[key] = "[image]"
			} else {
				out[key] = typed
			}
		default:
			out[key] = item
		}
	}
	return out
}

func kiroImageTokenSource(value map[string]any) (mediaType, source string, ok bool) {
	kind, _ := value["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image":
		mediaType, source = kiroImageSourceFields(value)
		return mediaType, source, true
	case "image_url", "input_image":
		mediaType, source = kiroImageSourceFields(value)
		if raw, exists := value["image_url"]; exists {
			switch typed := raw.(type) {
			case string:
				source = typed
			case map[string]any:
				if url, found := typed["url"].(string); found {
					source = url
				}
			}
		}
		return mediaType, source, true
	default:
		return "", "", false
	}
}

func kiroImageSourceFields(value map[string]any) (mediaType, source string) {
	container := value
	if nested, ok := value["source"].(map[string]any); ok {
		container = nested
	}
	for _, key := range []string{"media_type", "mediaType", "mime_type"} {
		if candidate, ok := container[key].(string); ok && strings.TrimSpace(candidate) != "" {
			mediaType = candidate
			break
		}
	}
	for _, key := range []string{"data", "url"} {
		if candidate, ok := container[key].(string); ok && strings.TrimSpace(candidate) != "" {
			source = candidate
			break
		}
	}
	return mediaType, source
}

func countKiroSerializedValueTokens(value any) int {
	canonical, err := canonicalJSON(value)
	if err != nil {
		return 0
	}
	return anthropictokenizer.CountTokens(string(canonical))
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			if k == "cache_control" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_ = buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				_ = buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			_, _ = buf.Write(kb)
			_ = buf.WriteByte(':')
			// Position counters and transport-generated identifiers must not
			// change a cache fingerprint. Preserve the field shape with null,
			// matching the reference canonicalizer, so semantically equivalent
			// tool/message blocks remain comparable.
			if isCachePositionKey(k) || isCacheVolatileKey(k, x[k], x) {
				_, _ = buf.WriteString("null")
				continue
			}
			if err := writeCanonicalJSON(buf, x[k]); err != nil {
				return err
			}
		}
		_ = buf.WriteByte('}')
		return nil
	case []any:
		_ = buf.WriteByte('[')
		for i, child := range x {
			if i > 0 {
				_ = buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, child); err != nil {
				return err
			}
		}
		_ = buf.WriteByte(']')
		return nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, _ = buf.Write(b)
		return nil
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index", "input_index":
		return true
	default:
		return false
	}
}

func isCacheVolatileKey(key string, value any, object map[string]any) bool {
	switch key {
	case "tool_use_id", "toolUseId", "request_id", "requestId", "message_id", "messageId":
		return true
	case "id":
		id, _ := value.(string)
		if id == "" {
			return false
		}
		kind, _ := object["type"].(string)
		return kind == "tool_use" || kind == "tool_result" || kind == "message" ||
			strings.HasPrefix(id, "toolu_") || strings.HasPrefix(id, "srvtoolu_") ||
			strings.HasPrefix(id, "msg_") || strings.HasPrefix(id, "req_") ||
			looksLikeUUID(id)
	default:
		return false
	}
}

func looksLikeUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range []byte(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func cacheAsString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (u *cacheEmulationUsage) toKiroUsage() *kiropkg.Usage {
	if u == nil {
		return nil
	}
	return &kiropkg.Usage{
		InputTokens:                u.InputTokens,
		CacheReadInputTokens:       u.CacheReadInputTokens,
		CacheCreationInputTokens:   u.CacheCreationInputTokens,
		CacheCreation5mInputTokens: u.CacheCreation5mInputTokens,
		CacheCreation1hInputTokens: u.CacheCreation1hInputTokens,
	}
}

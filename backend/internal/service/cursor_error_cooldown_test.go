//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// 本组钉的是按真实上游错误码补充的三类归类及其处置：
//
//   - 版本过旧：不换号、不写任何账号状态（号池里每个号结果都一样）；
//   - 用户级限流：账号短暂冷却 + 换号，绝不标桶耗尽（限流会自愈）；
//   - 地区封锁：账号×模型冷却 + 换号，绝不停号（是出口问题，不是凭证问题）；
//   - 封号：停号。
//
// 每一类都对应一个修复前的错误处置，断言里写明了被修掉的那个后果。

// cursorCooldownRepo 记录 recordCursorFailure 对账号状态的全部写入。
type cursorCooldownRepo struct {
	AccountRepository
	tempUnsched []cursorTempUnschedCall
	modelLimits []cursorModelLimitCall
	errors      []string
	updates     []*Account
}

type cursorTempUnschedCall struct {
	id     int64
	until  time.Time
	reason string
}

type cursorModelLimitCall struct {
	id      int64
	scope   string
	resetAt time.Time
	reason  string
}

func (r *cursorCooldownRepo) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.tempUnsched = append(r.tempUnsched, cursorTempUnschedCall{id: id, until: until, reason: reason})
	return nil
}

func (r *cursorCooldownRepo) SetModelRateLimit(_ context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	call := cursorModelLimitCall{id: id, scope: scope, resetAt: resetAt}
	if len(reason) > 0 {
		call.reason = reason[0]
	}
	r.modelLimits = append(r.modelLimits, call)
	return nil
}

func (r *cursorCooldownRepo) SetError(_ context.Context, _ int64, msg string) error {
	r.errors = append(r.errors, msg)
	return nil
}

// GetByID：markCursorBucketExhausted 先重读库里最新快照再标桶。
// 返回 not found 时它退回内存快照，对本组断言没有影响。
func (r *cursorCooldownRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return nil, ErrAccountNotFound
}

// UpdateExtra 是 markCursorBucketExhausted 的落库口（原子 JSONB 合并）：
// 写入 cursor_quota 即说明桶被标了。整行 Update 在这条路径上已被禁止。
func (r *cursorCooldownRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if _, ok := updates[CursorExtraQuotaKey]; ok {
		r.updates = append(r.updates, &Account{ID: id, Extra: updates})
	}
	return nil
}

func (r *cursorCooldownRepo) wroteAnything() bool {
	return len(r.tempUnsched)+len(r.modelLimits)+len(r.errors)+len(r.updates) > 0
}

func newCursorCooldownFixture(t *testing.T) (*GatewayService, *cursorCooldownRepo, *gin.Context, *Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &cursorCooldownRepo{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	acc := &Account{
		ID: 42, Name: "cursor-a", Platform: PlatformCursor, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{CursorCredMembership: "pro"},
	}
	return &GatewayService{accountRepo: repo}, repo, c, acc
}

const (
	cursorOutdatedClientSample = "Connect error 429: ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT: Update Required — " +
		"Your version of Cursor is no longer supported. [resource_exhausted]"
	cursorProUserRateLimitSample = "Connect error 429: ERROR_PRO_USER_RATE_LIMIT_EXCEEDED: Rate limit exceeded. — " +
		"You have exceeded your usage limit. [resource_exhausted]"
	cursorFreeUserRateLimitSample = "Connect error 429: ERROR_FREE_USER_RATE_LIMIT_EXCEEDED: Rate limit exceeded [resource_exhausted]"
	cursorRegionBlockedSample     = "Connect error 403: ERROR_CUSTOM_MESSAGE: Model not available in your region [permission_denied]"
	// 2026-11-12 真实账号原文（AgentService END 帧）。
	cursorDeviceLimitSample = "ERROR_CUSTOM_MESSAGE: Too many computers. - Too many computers used within the last 24 hours " +
		"for the same Cursor account. Please try again later, use a different Cursor account, or contact us at " +
		"hi@cursor.com if you believe this is a mistake."
)

// ---- 归类 ----

func TestClassifyCursorError_OutdatedClient(t *testing.T) {
	got := classifyCursorError("claude-sonnet-5", cursorOutdatedClientSample)
	require.Equal(t, cursorErrorOutdatedClient, got.Category,
		"版本过旧借用 rate limit 错误码，修复前被判成瞬时错误，导致整个号池被换一遍")
	require.Equal(t, http.StatusBadGateway, got.StatusCode)
	require.Equal(t, cursorOutdatedClientMessage, got.Message, "对外文案固定，提示管理员更新版本号")
	require.Contains(t, got.Message, "CURSOR_CLIENT_VERSION")
	require.Empty(t, got.QuotaBucket, "版本过旧不得标任何额度桶")
	require.False(t, cursorShouldDisableAccount(got))
}

func TestClassifyCursorError_UserRateLimitIsNotQuotaExhaustion(t *testing.T) {
	for _, text := range []string{cursorProUserRateLimitSample, cursorFreeUserRateLimitSample} {
		got := classifyCursorError("claude-sonnet-5", text)
		require.Equal(t, cursorErrorUserRateLimit, got.Category, text)
		require.Equal(t, http.StatusTooManyRequests, got.StatusCode)
		require.Equal(t, "rate_limit_error", got.ErrorType)
		require.Empty(t, got.QuotaBucket,
			"PRO 那条带 exceeded your usage limit，修复前被判成额度耗尽而永久标满桶")
		require.False(t, cursorShouldDisableAccount(got))
	}
}

func TestClassifyCursorError_RegionBlocked(t *testing.T) {
	got := classifyCursorError("claude-sonnet-5", cursorRegionBlockedSample)
	require.Equal(t, cursorErrorRegionBlocked, got.Category)
	require.Equal(t, http.StatusForbidden, got.StatusCode)
	require.Equal(t, cursorRegionBlockedMessage, got.Message, "对外文案提示检查代理出口地区")
	require.Equal(t, "claude-sonnet-5", got.CooldownModel)
	require.False(t, cursorShouldDisableAccount(got),
		"Connect error 403 修复前被通用兜底判成凭证失效而停号——出口问题不是凭证问题")

	outage := classifyCursorError("claude-sonnet-5", "Connect error 502: regional endpoint unavailable [unavailable]")
	require.NotEqual(t, cursorErrorRegionBlocked, outage.Category, "区域故障措辞不得被当成地区封锁")
}

func TestClassifyCursorError_AccountBlockedDisablesAccount(t *testing.T) {
	for _, text := range []string{
		"ERROR_SUSPICIOUS_USAGE_BLOCKED: unusual usage detected, usage limit applied",
		"ERROR_ACCOUNT_CLOSED: this account has been closed",
	} {
		got := classifyCursorError("claude-sonnet-5", text)
		require.Equal(t, cursorErrorAuthError, got.Category, text)
		require.Equal(t, cursor.ErrAuth, got.Kind)
		require.Empty(t, got.QuotaBucket, "封号文案带 usage limit 时也不得落到额度分支")
		require.True(t, cursorShouldDisableAccount(got))
	}
}

func TestClassifyCursorError_DeviceLimit(t *testing.T) {
	got := classifyCursorError("claude-opus-4-8-medium", cursorDeviceLimitSample)
	require.Equal(t, cursorErrorDeviceLimit, got.Category,
		"修复前正文的 too many 让它落到瞬时错误：不写冷却，同一个账号被连打 16 次")
	require.Equal(t, http.StatusTooManyRequests, got.StatusCode)
	require.Equal(t, "rate_limit_error", got.ErrorType)
	require.Equal(t, cursorDeviceLimitMessage, got.Message, "不得把「换一个 Cursor 账号 / 发邮件」原文透给网关调用方")
	require.Empty(t, got.QuotaBucket)
	require.False(t, cursorShouldDisableAccount(got), "凭证仍有效，不得停号")
}

// ---- 调度契约 ----

func TestCursorFailoverError_NewCategories(t *testing.T) {
	outdated := cursorFailoverError(classifyCursorError("claude-sonnet-5", cursorOutdatedClientSample), false)
	require.False(t, outdated.ShouldRetryNextAccount(), "版本过旧换号只会把整个号池烧一遍")
	require.Equal(t, GatewayFailureScopeProvider, outdated.Scope, "不得归因到账号")

	for _, text := range []string{cursorProUserRateLimitSample, cursorFreeUserRateLimitSample, cursorDeviceLimitSample, cursorRegionBlockedSample} {
		err := cursorFailoverError(classifyCursorError("claude-sonnet-5", text), false)
		require.True(t, err.ShouldRetryNextAccount(), "该账号此刻不行，别的账号可能行: %s", text)
		require.Equal(t, GatewayFailureScopeAccount, err.Scope)
		require.False(t, err.RetryableOnSameAccount, "限流窗口和出口地区都不会在几百毫秒内变化，不得同号重试")
	}
}

// ---- recordCursorFailure 的账号状态写入 ----

func TestRecordCursorFailure_OutdatedClientWritesNoAccountState(t *testing.T) {
	svc, repo, c, acc := newCursorCooldownFixture(t)
	svc.recordCursorFailure(context.Background(), c, acc, classifyCursorError("claude-sonnet-5", cursorOutdatedClientSample))

	require.False(t, repo.wroteAnything(), "版本过旧是网关配置问题，不得写任何账号状态")
	events, _ := c.Get(OpsUpstreamErrorsKey)
	require.Len(t, events, 1, "ops 事件必须照常上报，运维才看得到")
}

func TestRecordCursorFailure_UserRateLimitCoolsDownAccountWithoutMarkingBucket(t *testing.T) {
	for _, text := range []string{cursorProUserRateLimitSample, cursorFreeUserRateLimitSample} {
		svc, repo, c, acc := newCursorCooldownFixture(t)
		before := time.Now()
		svc.recordCursorFailure(context.Background(), c, acc, classifyCursorError("claude-sonnet-5", text))

		require.Len(t, repo.tempUnsched, 1, "用户级限流必须让账号冷却，否则下个请求又选中它: %s", text)
		call := repo.tempUnsched[0]
		require.Equal(t, int64(42), call.id)
		require.WithinDuration(t, before.Add(cursorUserRateLimitCooldown), call.until, 5*time.Second)
		require.Contains(t, call.reason, cursorUserRateLimitReason)
		require.Empty(t, repo.updates, "不得调用桶耗尽写入：限流会自愈，标桶会让账号在下次额度刷新前一直不可调度")
		require.Empty(t, repo.errors, "不得停号")
		require.Empty(t, repo.modelLimits)
	}
}

// ⚠️ 单账号分组的放大链路：瞬时错误按 503 回给 failover，选号耗尽后
// HandleSelectionExhausted 只对 503 清空排除列表、退避 2s 再选回同一个账号，
// 直到换号次数用完。设备数超限必须同时满足两点才能掐断这条链路：
//   - 状态码不是 503，选号耗尽时不走退避重选；
//   - 账号落冷却，后续请求不再选中它。
func TestCursorDeviceLimit_DoesNotTriggerSingleAccountBackoff(t *testing.T) {
	err := cursorFailoverError(classifyCursorError("claude-opus-4-8-medium", cursorDeviceLimitSample), false)
	require.NotEqual(t, http.StatusServiceUnavailable, err.StatusCode,
		"503 会触发单账号退避重选，一个请求就把同一个账号打十几次")
	require.False(t, err.RetryableOnSameAccount)
}

func TestRecordCursorFailure_DeviceLimitCoolsDownAccount(t *testing.T) {
	svc, repo, c, acc := newCursorCooldownFixture(t)
	before := time.Now()
	svc.recordCursorFailure(context.Background(), c, acc, classifyCursorError("claude-opus-4-8-medium", cursorDeviceLimitSample))

	require.Len(t, repo.tempUnsched, 1, "设备数超限必须让账号冷却，否则下个请求又选中它")
	call := repo.tempUnsched[0]
	require.WithinDuration(t, before.Add(cursorDeviceLimitCooldown), call.until, 5*time.Second)
	require.Contains(t, call.reason, cursorDeviceLimitReason)
	require.Empty(t, repo.errors, "不得停号：凭证有效，窗口滑过后自动恢复")
	require.Empty(t, repo.updates, "不得标桶")
	require.Empty(t, repo.modelLimits, "设备数是账号维度，与模型无关")
}

func TestRecordCursorFailure_RegionBlockedCoolsDownAccountModel(t *testing.T) {
	svc, repo, c, acc := newCursorCooldownFixture(t)
	before := time.Now()
	svc.recordCursorFailure(context.Background(), c, acc, classifyCursorError("claude-sonnet-5", cursorRegionBlockedSample))

	require.Len(t, repo.modelLimits, 1)
	call := repo.modelLimits[0]
	require.Equal(t, "claude-sonnet-5", call.scope, "冷却必须落在目标模型上，账号对其它模型仍可用")
	require.WithinDuration(t, before.Add(cursorRegionBlockedCooldown), call.resetAt, 5*time.Second)
	require.Contains(t, call.reason, cursorRegionBlockedReason)
	require.Empty(t, repo.errors, "地区封锁不得停号")
	require.Empty(t, repo.updates, "地区封锁不得标桶")
	require.Empty(t, repo.tempUnsched, "有模型时不得整号冷却")
}

// ⚠️ 冷却键必须和选号读取的键一致，否则写了等于没写。
// 选号用 GetMappedModel(请求原始模型)；开启渠道映射时转发用的是映射后的模型。
// 两者不同时两个键都要写，并用真实的选号判定验证冷却确实生效。
func TestRecordCursorFailure_RegionCooldownIsVisibleToModelSelection(t *testing.T) {
	svc, repo, c, acc := newCursorCooldownFixture(t)
	ctx := context.WithValue(context.Background(), ctxkey.Model, "claude-opus-5")
	svc.recordCursorFailure(ctx, c, acc, classifyCursorError("claude-sonnet-5", cursorRegionBlockedSample))

	scopes := []string{}
	for _, call := range repo.modelLimits {
		scopes = append(scopes, call.scope)
	}
	require.ElementsMatch(t, []string{"claude-sonnet-5", "claude-opus-5"}, scopes)

	// 把写入的冷却回放到账号上，走选号同一条判定。
	limits := map[string]any{}
	for _, call := range repo.modelLimits {
		limits[call.scope] = map[string]any{"rate_limit_reset_at": call.resetAt.UTC().Format(time.RFC3339)}
	}
	acc.Extra = map[string]any{modelRateLimitsKey: limits}
	require.False(t, acc.IsSchedulableForModelWithContext(context.Background(), "claude-opus-5"),
		"选号按请求原始模型判定，冷却必须对它生效")
	require.True(t, acc.IsSchedulableForModelWithContext(context.Background(), "claude-haiku-4-5"),
		"其它模型不受影响")
}

func TestRecordCursorFailure_RegionBlockedWithoutModelFallsBackToAccountCooldown(t *testing.T) {
	svc, repo, c, acc := newCursorCooldownFixture(t)
	svc.recordCursorFailure(context.Background(), c, acc, classifyCursorError("", cursorRegionBlockedSample))

	require.Empty(t, repo.modelLimits)
	require.Len(t, repo.tempUnsched, 1, "拿不到模型时宁可整号冷却，也不能让确定性 403 的账号被反复选中")
}

func TestRecordCursorFailure_AccountBlockedDisablesAccount(t *testing.T) {
	svc, repo, c, acc := newCursorCooldownFixture(t)
	svc.recordCursorFailure(context.Background(), c, acc,
		classifyCursorError("claude-sonnet-5", "ERROR_ACCOUNT_CLOSED: this account has been closed"))

	require.Len(t, repo.errors, 1, "封号必须停号")
	require.Empty(t, repo.tempUnsched)
	require.Empty(t, repo.updates)
}

// 回归：真正的额度耗尽仍然只标桶。
func TestRecordCursorFailure_QuotaStillMarksBucketOnly(t *testing.T) {
	svc, repo, c, acc := newCursorCooldownFixture(t)
	svc.recordCursorFailure(context.Background(), c, acc,
		classifyCursorError("claude-sonnet-5", "You are out of usage - Upgrade to a paid plan"))

	require.Len(t, repo.updates, 1, "额度耗尽仍应标桶")
	require.Empty(t, repo.tempUnsched, "额度耗尽不叠加临时冷却：桶状态已表达恢复时机")
	require.Empty(t, repo.errors)
}

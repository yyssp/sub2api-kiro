package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// CursorUsageFetcher 把 Cursor 上游的两个 dashboard 端点拉成三桶额度快照。
//
// ⚠️ Cursor 的额度体系与 Kiro / Anthropic 无对应关系，不要照搬它们的建模：
//   - 没有 5h / 7d 滑动窗口，按**计费周期**结算，上游不返回 resetsAt，
//     所以不能塞进 UsageProgress（会凭空捏造一个重置时间）。
//   - 三个桶（cursor / other / grokbot）来自**两个不同端点**，
//     其中任意一个失败都不能把另一个的结果一起丢掉。
//   - 必须区分「字段没返回」与「确认耗尽」：把 unknown 当耗尽会误清号池，
//     当可用则会让请求反复撞上游 quota 错误。
type CursorUsageFetcher struct {
	client *cursor.Client
}

func NewCursorUsageFetcher() *CursorUsageFetcher {
	return &CursorUsageFetcher{client: sharedCursorClient()}
}

// cursorQuotaStaleAfter 是额度快照的有效期。超过则认为需要重新拉取。
const cursorQuotaStaleAfter = 10 * time.Minute

// Fetch 拉取一次三桶额度。
//
// 返回的 CursorQuota 永远是完整的三桶结构：拉取失败的桶为 request_failed，
// 上游未返回字段的桶为 unknown——调用方据此决定是否写回，而不是拿空结构覆盖。
func (f *CursorUsageFetcher) Fetch(ctx context.Context, account *Account, accessToken string) (CursorQuota, error) {
	now := time.Now().UTC()
	q := CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateUnknown},
		Other:     CursorBucketQuota{State: CursorQuotaStateUnknown},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateUnknown},
		FetchedAt: &now,
	}
	if account == nil {
		return q, fmt.Errorf("cursor usage: account is nil")
	}
	if f == nil || f.client == nil {
		return q, fmt.Errorf("cursor usage: client not initialized")
	}

	pa := cursorProtocolAccount(account)
	if token := strings.TrimSpace(accessToken); token != "" {
		pa.AccessToken = token
	}
	if strings.TrimSpace(pa.AccessToken) == "" {
		return q, fmt.Errorf("cursor usage: missing access token")
	}

	// 两个端点各自独立；一个挂掉不影响另一个的结果落库。
	period := f.fetchPeriod(ctx, &pa)
	sand := f.fetchSand(ctx, &pa)

	q.Cursor = cursorBucketFromPercent(period, period.AutoPercent)
	q.Other = cursorBucketFromPercent(period, period.APIPercent)
	q.GrokBot = cursorBucketFromSand(sand)

	if !period.OK && !sand.OK {
		return q, fmt.Errorf("cursor usage: both dashboard endpoints failed")
	}
	return q, nil
}

// fetchPeriod / fetchSand 把同步的协议层调用套上 ctx 取消语义。
// 协议层的 Client 方法不吃 ctx（它维护 per-account HTTP/2 连接池），
// 这里不改协议层签名，只在 service 侧保证请求方放弃时能及时返回。
func (f *CursorUsageFetcher) fetchPeriod(ctx context.Context, pa *cursor.Account) cursor.PeriodUsage {
	type result struct{ pu cursor.PeriodUsage }
	ch := make(chan result, 1)
	go func() { ch <- result{f.client.GetCurrentPeriodUsage(pa)} }()
	select {
	case <-ctx.Done():
		return cursor.PeriodUsage{}
	case r := <-ch:
		return r.pu
	}
}

func (f *CursorUsageFetcher) fetchSand(ctx context.Context, pa *cursor.Account) cursor.SandUsage {
	type result struct{ su cursor.SandUsage }
	ch := make(chan result, 1)
	go func() { ch <- result{f.client.GetSandUsage(pa)} }()
	select {
	case <-ctx.Done():
		return cursor.SandUsage{}
	case r := <-ch:
		return r.su
	}
}

// cursorBucketFromPercent 把 period 端点的一个百分比字段映射成桶状态。
//
// ⚠️ 请求失败时状态是 request_failed 而不是 unknown：前者表示「这次没拿到，
// 下次重试」，后者表示「上游确实没给这个字段」，两者的调度含义不同。
func cursorBucketFromPercent(pu cursor.PeriodUsage, pct float64) CursorBucketQuota {
	if !pu.OK {
		return CursorBucketQuota{State: CursorQuotaStateRequestFailed}
	}
	b := CursorBucketQuota{Percent: pct}
	if pct >= 100 {
		b.State = CursorQuotaStateExhausted
		return b
	}
	b.State = CursorQuotaStateAvailable
	return b
}

// cursorBucketFromSand 复用协议层已验证的 EffectiveState()。
// 那里处理了「上游在 100% 时仍返回 hasAvailableUsage=true」的已知上游 bug，
// 不要在 service 层重新推导，否则会丢掉这个修正。
func cursorBucketFromSand(su cursor.SandUsage) CursorBucketQuota {
	state := su.EffectiveState()
	b := CursorBucketQuota{
		State:   state,
		Enabled: state == CursorQuotaStateAvailable,
	}
	if su.UsagePercentPresent {
		b.Percent = su.UsagePercent
	}
	return b
}

// getCursorUsage 是 AccountUsageService 的 Cursor 分支实现。
//
// 拉取成功后把快照写回 extra（经唯一写口 writeCursorQuota）并落库，
// 让调度器的 CursorAccountUsableForModel 读到最新状态。
func (s *AccountUsageService) getCursorUsage(ctx context.Context, account *Account) (*UsageInfo, error) {
	if account == nil || account.Platform != PlatformCursor {
		return nil, fmt.Errorf("cursor usage: not a cursor account")
	}

	now := time.Now().UTC()
	info := &UsageInfo{
		Source:           "active",
		UpdatedAt:        &now,
		CursorMembership: strings.TrimSpace(account.GetCredential(CursorCredMembership)),
	}

	var accessToken string
	if s.cursorTokenProvider != nil {
		token, err := s.cursorTokenProvider.GetAccessToken(ctx, account)
		if err != nil {
			// 降级返回而非 500：额度拉不到不该让管理台整页报错。
			cached := readCursorQuota(account)
			info.CursorQuota = &cached
			info.Error = err.Error()
			info.ErrorCode = "unauthenticated"
			return info, nil
		}
		accessToken = token
	}

	quota, err := s.cursorUsageFetcher().Fetch(ctx, account, accessToken)
	if err != nil {
		info.CursorQuota = &quota
		info.Error = err.Error()
		info.ErrorCode = "network_error"
		return info, nil
	}

	writeCursorQuota(account, quota)
	if s.accountRepo != nil {
		if updateErr := s.accountRepo.Update(ctx, account); updateErr != nil {
			// 落库失败不影响本次返回值，只是下次还得重拉。
			info.Error = updateErr.Error()
		}
	}

	info.CursorQuota = &quota
	s.tryClearRecoverableAccountError(ctx, account)
	return info, nil
}

// cursorUsageFetcher 惰性构造，避免给 AccountUsageService 的构造函数加参数
// （加参数会改 Wire provider 签名，放大与上游的合并冲突面）。
func (s *AccountUsageService) cursorUsageFetcher() *CursorUsageFetcher {
	if s.cursorFetcher == nil {
		s.cursorFetcher = NewCursorUsageFetcher()
	}
	return s.cursorFetcher
}

// CursorQuotaIsStale 判断快照是否需要刷新。从未抓取（FetchedAt 为 nil）一律算 stale，
// 不能当作「0% 可用」。
func CursorQuotaIsStale(q CursorQuota, now time.Time) bool {
	if q.FetchedAt == nil {
		return true
	}
	return now.Sub(*q.FetchedAt) >= cursorQuotaStaleAfter
}

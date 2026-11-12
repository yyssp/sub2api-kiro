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
// 拉取成功后把快照写回 extra 并原子落库（refreshCursorQuota），
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

	// ⚠️ 判 nil 必须在装进接口之前：nil 的 *CursorTokenProvider 装进接口后
	// 接口本身不为 nil，refreshCursorQuota 会对它调方法。
	var tokens cursorQuotaTokenSource
	if s.cursorTokenProvider != nil {
		tokens = s.cursorTokenProvider
	}
	res := refreshCursorQuota(ctx, s.accountRepo, tokens, s.cursorUsageFetcher(), account)
	info.CursorQuota = &res.Quota
	switch {
	case res.TokenErr != nil:
		// 降级返回而非 500：额度拉不到不该让管理台整页报错。
		info.Error = res.TokenErr.Error()
		info.ErrorCode = "unauthenticated"
		return info, nil
	case res.FetchErr != nil:
		info.Error = res.FetchErr.Error()
		info.ErrorCode = "network_error"
		return info, nil
	case res.PersistErr != nil:
		// 落库失败不影响本次返回值，只是下次还得重拉。
		info.Error = res.PersistErr.Error()
	}

	s.tryClearRecoverableAccountError(ctx, account)
	return info, nil
}

// cursorQuotaTokenSource 是额度刷新取 token 的最小依赖（*CursorTokenProvider 实现）。
type cursorQuotaTokenSource interface {
	GetAccessToken(ctx context.Context, account *Account) (string, error)
}

// cursorQuotaFetcher 是额度刷新拉取上游的最小依赖（*CursorUsageFetcher 实现）。
type cursorQuotaFetcher interface {
	Fetch(ctx context.Context, account *Account, accessToken string) (CursorQuota, error)
}

// cursorQuotaRefreshResult 是一次额度刷新的结果。三个 error 互斥地描述
// 失败停在哪一步，调用方据此决定怎么呈现，而不是自己再推一遍。
type cursorQuotaRefreshResult struct {
	Quota      CursorQuota
	TokenErr   error // 取 token 失败：未拉取、未落库，Quota 为库里的旧快照
	FetchErr   error // 两个端点都失败：未落库，Quota 为本次失败结构
	PersistErr error // 拉到了但落库失败：内存已更新
}

// refreshCursorQuota 是「取 token → 拉两个 dashboard 端点 → 原子落库」的唯一实现，
// 管理台手动查看（getCursorUsage）与后台自动刷新（CursorQuotaRefreshService）共用。
//
// ⚠️ 取 token 失败绝不写成额度耗尽：凭证问题由 token provider 自己判定并停号，
// 这里把它记成"桶满"会让账号在凭证恢复后仍被额度闸门挡住。
//
// ⚠️ 两个端点都失败时不落库：失败结构里全是 request_failed，写进去会覆盖掉
// 上一次真实读到的快照（含刚标记的 exhausted），且 FetchedAt 被刷新后
// 下一轮刷新还会把它当成"新鲜"跳过。
func refreshCursorQuota(
	ctx context.Context,
	repo AccountRepository,
	tokens cursorQuotaTokenSource,
	fetcher cursorQuotaFetcher,
	account *Account,
) cursorQuotaRefreshResult {
	var accessToken string
	if tokens != nil {
		token, err := tokens.GetAccessToken(ctx, account)
		if err != nil {
			return cursorQuotaRefreshResult{Quota: readCursorQuota(account), TokenErr: err}
		}
		accessToken = token
	}

	quota, err := fetcher.Fetch(ctx, account, accessToken)
	if err != nil {
		return cursorQuotaRefreshResult{Quota: quota, FetchErr: err}
	}
	return cursorQuotaRefreshResult{Quota: quota, PersistErr: persistCursorQuota(ctx, repo, account, quota)}
}

// persistCursorQuota 把额度快照写进内存 account 并原子落库。
//
// ⚠️ 必须走 UpdateExtra（JSONB 合并），不能 Update 整行写回：
//   - 整行写回会把管理员同期在后台做的修改（分组、并发、代理……）覆盖回旧值；
//   - 并发请求各自标记不同的桶时，整行写回是 last-writer-wins，先写的那个桶
//     会被后写的那份旧快照抹掉。
//
// 内存里的 account.Extra 仍同步更新：同一请求后续可能还会读它。
func persistCursorQuota(ctx context.Context, repo AccountRepository, account *Account, q CursorQuota) error {
	if account == nil {
		return nil
	}
	writeCursorQuota(account, q)
	if repo == nil {
		return nil
	}
	return repo.UpdateExtra(ctx, account.ID, map[string]any{CursorExtraQuotaKey: cursorQuotaPayload(q)})
}

// cursorUsageFetcher 惰性构造，避免给 AccountUsageService 的构造函数加参数
// （加参数会改 Wire provider 签名，放大与上游的合并冲突面）。
func (s *AccountUsageService) cursorUsageFetcher() cursorQuotaFetcher {
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

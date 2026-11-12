package service

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// CursorQuotaRefreshService 周期性刷新 Cursor 账号的三桶额度快照。
//
// ⚠️ 没有它，号池容量只减不增：请求撞上额度错误时 markCursorBucketExhausted
// 会把桶标成耗尽，而唯一的恢复途径是管理员手动打开额度面板（getCursorUsage）。
// 计费周期重置后账号早就恢复了，调度器却仍按耗尽把它排除在外。
//
// 只刷「需要刷」的账号（见 cursorQuotaRefreshDue），新鲜且可用的快照不碰：
// dashboard 接口是 Cursor 的风控面之一，没有理由每个周期把全池扫一遍。
type CursorQuotaRefreshService struct {
	accountRepo AccountRepository
	tokens      cursorQuotaTokenSource
	fetcher     cursorQuotaFetcher

	parentCtx    context.Context
	parentCancel context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	started      bool
	stopped      bool
	cycleMu      sync.Mutex
	now          func() time.Time
	lockCache    LeaderLockCache
	db           *sql.DB
	instanceID   string
}

const (
	// cursorQuotaRefreshCycleInterval 取 5 分钟：与快照有效期（cursorQuotaStaleAfter=10m）
	// 错开一半，过期的快照最迟一个周期内被发现；更密只是空转查库。
	cursorQuotaRefreshCycleInterval = 5 * time.Minute
	// cursorQuotaRefreshMaxPerCycle 限制单周期刷新的账号数。每个账号要打两个
	// dashboard 端点，号池上百个账号同时到期时一口气全刷，等于对 Cursor 发起一次
	// 突发扫描；分摊到后续周期即可，耗尽账号的恢复晚几分钟无伤大雅。
	cursorQuotaRefreshMaxPerCycle = 20
	// cursorQuotaRefreshConcurrency 与 OpenCode Go 刷新同档：够快，又不至于
	// 让同一出口 IP 上并发打出一串 dashboard 请求。
	cursorQuotaRefreshConcurrency = 4
	// cursorQuotaRefreshAccountTimeout 是单账号（取 token + 两个端点）的上限。
	// 账号代理慢或挂死时必须及时放弃，否则一个坏代理就能卡住整个周期。
	cursorQuotaRefreshAccountTimeout = 30 * time.Second
	// cursorQuotaRefreshBackstopAge 是兜底刷新阈值：即便快照显示全部可用，
	// 超过这个年龄也重拉一次。百分比会随使用增长，太旧的"可用"不可信。
	cursorQuotaRefreshBackstopAge = time.Hour

	cursorQuotaRefreshLeaderLockKey = "cursor:quota:refresh:leader"
	// cursorQuotaRefreshLeaderLockTTL 必须大于一个周期的最坏耗时：
	// MaxPerCycle/Concurrency 批 × 单账号超时 = 5 × 30s = 150s。
	cursorQuotaRefreshLeaderLockTTL = 4 * time.Minute
)

func NewCursorQuotaRefreshService(
	accountRepo AccountRepository,
	tokens cursorQuotaTokenSource,
	fetcher cursorQuotaFetcher,
) *CursorQuotaRefreshService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CursorQuotaRefreshService{
		accountRepo:  accountRepo,
		tokens:       tokens,
		fetcher:      fetcher,
		parentCtx:    ctx,
		parentCancel: cancel,
		now:          time.Now,
		instanceID:   uuid.NewString(),
	}
}

// ProvideCursorQuotaRefreshService 构造并启动后台刷新。
//
// tokenProvider 必须是 Wire 图里那一个实例：它带 token 缓存，另起一个会让
// 后台刷新绕过缓存、对每个账号都重新走一遍续期判断。
func ProvideCursorQuotaRefreshService(
	accountRepo AccountRepository,
	tokenProvider *CursorTokenProvider,
	lockCache LeaderLockCache,
	db *sql.DB,
) *CursorQuotaRefreshService {
	var tokens cursorQuotaTokenSource
	if tokenProvider != nil {
		tokens = tokenProvider
	}
	svc := NewCursorQuotaRefreshService(accountRepo, tokens, NewCursorUsageFetcher())
	svc.lockCache = lockCache
	svc.db = db
	svc.Start()
	return svc
}

func (s *CursorQuotaRefreshService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *CursorQuotaRefreshService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *CursorQuotaRefreshService) runLoop() {
	defer s.wg.Done()
	s.RunDue(s.parentCtx)
	ticker := time.NewTicker(cursorQuotaRefreshCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			s.RunDue(s.parentCtx)
		}
	}
}

// RunDue 执行一个刷新周期，返回实际尝试刷新的账号数。
func (s *CursorQuotaRefreshService) RunDue(ctx context.Context) int {
	if s == nil || s.accountRepo == nil || s.fetcher == nil {
		return 0
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	release, acquired := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db,
		cursorQuotaRefreshLeaderLockKey, s.instanceID, cursorQuotaRefreshLeaderLockTTL)
	if !acquired {
		return 0
	}
	defer release()

	// ⚠️ status 必须传空串，不能传 StatusActive：仓储层的 active 过滤同时要求
	// 未处于限流/临时冷却窗口，而后者恰恰是最需要刷新额度的账号。
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformCursor, "", "", "", 0, "")
	if err != nil {
		logger.LegacyPrintf("service.cursor_quota_refresh", "list_accounts_failed: err=%v", err)
		return 0
	}
	due := selectCursorQuotaRefreshDue(accounts, s.currentTime(), cursorQuotaRefreshMaxPerCycle)
	if len(due) == 0 {
		return 0
	}

	var group errgroup.Group
	group.SetLimit(cursorQuotaRefreshConcurrency)
	for i := range due {
		account := due[i]
		group.Go(func() error {
			s.refreshOne(ctx, account)
			return nil
		})
	}
	_ = group.Wait()
	return len(due)
}

func (s *CursorQuotaRefreshService) refreshOne(parent context.Context, account *Account) {
	ctx, cancel := context.WithTimeout(parent, cursorQuotaRefreshAccountTimeout)
	defer cancel()
	res := refreshCursorQuota(ctx, s.accountRepo, s.tokens, s.fetcher, account)
	switch {
	case res.TokenErr != nil:
		// 只记日志：凭证失效由 token provider 自己判定并停号，这里不落任何状态。
		logger.LegacyPrintf("service.cursor_quota_refresh", "skip_token_unavailable: account_id=%d err=%v", account.ID, res.TokenErr)
	case res.FetchErr != nil:
		logger.LegacyPrintf("service.cursor_quota_refresh", "fetch_failed: account_id=%d err=%v", account.ID, res.FetchErr)
	case res.PersistErr != nil:
		logger.LegacyPrintf("service.cursor_quota_refresh", "persist_failed: account_id=%d err=%v", account.ID, res.PersistErr)
	}
}

func (s *CursorQuotaRefreshService) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// selectCursorQuotaRefreshDue 挑出本周期要刷新的账号，最久没刷的排前面。
//
// 只看直连模式的 active 账号：error/disabled 账号要么凭证已坏（刷也是白刷），
// 要么是管理员主动停用的，不该被后台任务去碰它的上游。
func selectCursorQuotaRefreshDue(accounts []Account, now time.Time, limit int) []*Account {
	type candidate struct {
		account   *Account
		fetchedAt time.Time // 零值 = 从未抓取，排最前
	}
	var due []candidate
	for i := range accounts {
		account := &accounts[i]
		if !isCursorDirectModeAccount(account) || account.Status != StatusActive {
			continue
		}
		q := readCursorQuota(account)
		if !cursorQuotaRefreshDue(q, now) {
			continue
		}
		c := candidate{account: account}
		if q.FetchedAt != nil {
			c.fetchedAt = *q.FetchedAt
		}
		due = append(due, c)
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].fetchedAt.Before(due[j].fetchedAt) })
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	out := make([]*Account, len(due))
	for i := range due {
		out[i] = due[i].account
	}
	return out
}

// cursorQuotaRefreshDue 判断一份快照是否需要后台刷新：
//
//	(a) 从未抓取过；
//	(b) 已过期，且有桶处于「调度会拦、但可能已经变化」的状态——
//	    耗尽的可能已随计费周期重置，没读到的（request_failed/unknown）需要补读；
//	(c) 过期超过兜底阈值——"全部可用"的旧快照同样不可信。
//
// 已过期但没有可恢复状态、且未到兜底阈值的不刷：调度对它们的判断不会因
// 刷新而改变，刷新只会多打上游。
func cursorQuotaRefreshDue(q CursorQuota, now time.Time) bool {
	if q.FetchedAt == nil {
		return true
	}
	if !CursorQuotaIsStale(q, now) {
		return false
	}
	if now.Sub(*q.FetchedAt) >= cursorQuotaRefreshBackstopAge {
		return true
	}
	return cursorBucketMayRecover(q.Cursor.State) ||
		cursorBucketMayRecover(q.Other.State) ||
		cursorGrokBotMayRecover(q.GrokBot.State)
}

// cursorBucketMayRecover：cursor/other 桶只要不是 available 就值得重读。
// period 端点总会返回这两个百分比，unknown/unavailable 只可能来自旧数据或异常。
func cursorBucketMayRecover(state string) bool {
	return state != CursorQuotaStateAvailable
}

// cursorGrokBotMayRecover：grokbot 桶的 unavailable/unknown 是**稳态**——
// 账号没有 Sand 权益时上游一直这么回。把它们算进 (b) 会让每个没开 Sand 的
// 账号（号池的大多数）每 10 分钟被重刷一次，"按需刷新"名存实亡；
// 权益后来开通的情况由兜底阈值 (c) 覆盖。
func cursorGrokBotMayRecover(state string) bool {
	return state == CursorQuotaStateExhausted || state == CursorQuotaStateRequestFailed
}

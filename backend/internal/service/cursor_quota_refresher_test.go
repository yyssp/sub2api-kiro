//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// cursorQuotaRefreshRepo 只实现刷新链路会碰到的方法；其它方法走嵌入的 nil 接口，
// 一旦被调用就 panic——这正是我们想要的断言：Update（整行写回）不得再出现。
type cursorQuotaRefreshRepo struct {
	AccountRepository
	mu          sync.Mutex
	accounts    []Account
	byID        map[int64]*Account
	extraWrites []cursorQuotaExtraWrite
	listCalls   int
}

type cursorQuotaExtraWrite struct {
	id      int64
	updates map[string]any
}

func (r *cursorQuotaRefreshRepo) ListAllWithFilters(_ context.Context, platform, _, _, _ string, _ int64, _ string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	out := make([]Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		if a.Platform == platform {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *cursorQuotaRefreshRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.byID[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, ErrAccountNotFound
}

func (r *cursorQuotaRefreshRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraWrites = append(r.extraWrites, cursorQuotaExtraWrite{id: id, updates: updates})
	return nil
}

func (r *cursorQuotaRefreshRepo) writtenIDs() map[int64]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[int64]bool{}
	for _, w := range r.extraWrites {
		out[w.id] = true
	}
	return out
}

type stubCursorQuotaFetcher struct {
	mu    sync.Mutex
	calls []int64
	quota CursorQuota
	err   error
}

func (f *stubCursorQuotaFetcher) Fetch(_ context.Context, account *Account, _ string) (CursorQuota, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, account.ID)
	return f.quota, f.err
}

type stubCursorQuotaTokens struct {
	failFor map[int64]bool
}

func (s stubCursorQuotaTokens) GetAccessToken(_ context.Context, account *Account) (string, error) {
	if s.failFor[account.ID] {
		return "", errors.New("refresh token revoked")
	}
	return "tok", nil
}

func quotaRefreshTestAccount(id int64, q *CursorQuota) Account {
	a := Account{ID: id, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	if q != nil {
		writeCursorQuota(&a, *q)
	}
	return a
}

func cursorQuotaAt(fetched time.Time, cursorState, otherState, grokState string) *CursorQuota {
	return &CursorQuota{
		Cursor:    CursorBucketQuota{State: cursorState},
		Other:     CursorBucketQuota{State: otherState},
		GrokBot:   CursorBucketQuota{State: grokState},
		FetchedAt: &fetched,
	}
}

func freshAvailableQuota(now time.Time) CursorQuota {
	return CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 5},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 5},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateUnavailable},
		FetchedAt: &now,
	}
}

func TestCursorQuotaRefreshDue_SelectionRules(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute)
	stale := now.Add(-2 * cursorQuotaStaleAfter)
	ancient := now.Add(-2 * cursorQuotaRefreshBackstopAge)
	A, X, U, F, K := CursorQuotaStateAvailable, CursorQuotaStateExhausted, CursorQuotaStateUnavailable,
		CursorQuotaStateRequestFailed, CursorQuotaStateUnknown

	cases := []struct {
		name string
		q    CursorQuota
		want bool
	}{
		{"(a) 从未抓取", CursorQuota{}, true},
		{"新鲜快照即使耗尽也不刷（10 分钟内的结论可信）", *cursorQuotaAt(fresh, X, X, X), false},
		{"(b) 过期 + cursor 桶耗尽：可能已随周期重置", *cursorQuotaAt(stale, X, A, U), true},
		{"(b) 过期 + other 桶没读到", *cursorQuotaAt(stale, A, F, U), true},
		{"(b) 过期 + cursor 桶 unknown", *cursorQuotaAt(stale, K, A, U), true},
		{"(b) 过期 + grokbot 耗尽", *cursorQuotaAt(stale, A, A, X), true},
		{"(b) 过期 + grokbot 抓取失败", *cursorQuotaAt(stale, A, A, F), true},
		{"过期但全部可用：调度本来就放行，不刷", *cursorQuotaAt(stale, A, A, A), false},
		{"过期 + grokbot unavailable 是没开 Sand 的稳态，不刷", *cursorQuotaAt(stale, A, A, U), false},
		{"过期 + grokbot unknown 同属稳态，不刷", *cursorQuotaAt(stale, A, A, K), false},
		{"(c) 超过兜底阈值：全部可用也要重拉", *cursorQuotaAt(ancient, A, A, U), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, cursorQuotaRefreshDue(tc.q, now))
		})
	}
}

func TestSelectCursorQuotaRefreshDue_FiltersAndOrdersOldestFirst(t *testing.T) {
	now := time.Now().UTC()
	older := now.Add(-3 * cursorQuotaStaleAfter)
	newer := now.Add(-2 * cursorQuotaStaleAfter)
	fresh := freshAvailableQuota(now)

	errored := quotaRefreshTestAccount(4, nil)
	errored.Status = StatusError
	apiKeyGateway := quotaRefreshTestAccount(5, nil)
	apiKeyGateway.Type = AccountTypeAPIKey
	apiKeyGateway.Credentials = map[string]any{"base_url": "https://my-gateway.example"}

	accounts := []Account{
		quotaRefreshTestAccount(1, cursorQuotaAt(newer, CursorQuotaStateExhausted, CursorQuotaStateAvailable, CursorQuotaStateUnavailable)),
		quotaRefreshTestAccount(2, &fresh),
		quotaRefreshTestAccount(3, cursorQuotaAt(older, CursorQuotaStateExhausted, CursorQuotaStateAvailable, CursorQuotaStateUnavailable)),
		errored,
		apiKeyGateway,
		quotaRefreshTestAccount(6, nil),
	}
	got := selectCursorQuotaRefreshDue(accounts, now, 0)
	ids := make([]int64, 0, len(got))
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	require.Equal(t, []int64{6, 3, 1}, ids,
		"新鲜可用(2)、error 状态(4)、走自有网关的 apikey 账号(5) 必须跳过；从未抓取的排最前，其余按抓取时间从旧到新")
}

func TestCursorQuotaRefreshService_RespectsPerCycleLimit(t *testing.T) {
	repo := &cursorQuotaRefreshRepo{}
	for i := 1; i <= cursorQuotaRefreshMaxPerCycle+7; i++ {
		repo.accounts = append(repo.accounts, quotaRefreshTestAccount(int64(i), nil))
	}
	now := time.Now().UTC()
	fetcher := &stubCursorQuotaFetcher{quota: freshAvailableQuota(now)}
	svc := NewCursorQuotaRefreshService(repo, stubCursorQuotaTokens{}, fetcher)

	require.Equal(t, cursorQuotaRefreshMaxPerCycle, svc.RunDue(context.Background()))
	require.Len(t, fetcher.calls, cursorQuotaRefreshMaxPerCycle, "单周期刷新数必须封顶，不能对 Cursor dashboard 突发全池扫描")
}

func TestCursorQuotaRefreshService_PersistsViaUpdateExtraAndSkipsTokenFailures(t *testing.T) {
	stale := time.Now().UTC().Add(-2 * cursorQuotaStaleAfter)
	repo := &cursorQuotaRefreshRepo{accounts: []Account{
		quotaRefreshTestAccount(1, cursorQuotaAt(stale, CursorQuotaStateExhausted, CursorQuotaStateAvailable, CursorQuotaStateUnavailable)),
		quotaRefreshTestAccount(2, nil),
	}}
	now := time.Now().UTC()
	fetched := CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 12},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 40},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateUnavailable},
		FetchedAt: &now,
	}
	fetcher := &stubCursorQuotaFetcher{quota: fetched}
	svc := NewCursorQuotaRefreshService(repo, stubCursorQuotaTokens{failFor: map[int64]bool{2: true}}, fetcher)

	require.Equal(t, 2, svc.RunDue(context.Background()))

	require.Equal(t, []int64{1}, fetcher.calls, "取不到 token 的账号不得去打 dashboard")
	require.Len(t, repo.extraWrites, 1, "取 token 失败只记日志，不得落任何额度状态（停号由 token provider 负责）")
	w := repo.extraWrites[0]
	require.Equal(t, int64(1), w.id)
	require.Equal(t, []string{CursorExtraQuotaKey}, mapKeys(w.updates), "只能原子合并 cursor_quota 这一个 key")

	// 落库形状必须与内存写口完全一致，读回来是刚写进去的东西。
	readBack := readCursorQuota(&Account{Extra: map[string]any{CursorExtraQuotaKey: w.updates[CursorExtraQuotaKey]}})
	require.Equal(t, CursorQuotaStateAvailable, readBack.Cursor.State, "耗尽的桶在周期重置后必须被刷新恢复")
	require.InDelta(t, 12, readBack.Cursor.Percent, 0.001)
	require.NotNil(t, readBack.FetchedAt)
}

func TestRefreshCursorQuota_BothEndpointsFailedDoesNotPersist(t *testing.T) {
	stale := time.Now().UTC().Add(-2 * cursorQuotaStaleAfter)
	account := quotaRefreshTestAccount(1, cursorQuotaAt(stale, CursorQuotaStateExhausted, CursorQuotaStateAvailable, CursorQuotaStateUnavailable))
	repo := &cursorQuotaRefreshRepo{}
	fetcher := &stubCursorQuotaFetcher{
		quota: CursorQuota{
			Cursor:  CursorBucketQuota{State: CursorQuotaStateRequestFailed},
			Other:   CursorBucketQuota{State: CursorQuotaStateRequestFailed},
			GrokBot: CursorBucketQuota{State: CursorQuotaStateRequestFailed},
		},
		err: errors.New("cursor usage: both dashboard endpoints failed"),
	}

	res := refreshCursorQuota(context.Background(), repo, stubCursorQuotaTokens{}, fetcher, &account)

	require.Error(t, res.FetchErr)
	require.Empty(t, repo.extraWrites, "两个端点都失败时不得落库：会抹掉上次真实读到的快照并刷新 FetchedAt")
	require.Equal(t, CursorQuotaStateExhausted, readCursorQuota(&account).Cursor.State, "内存快照同样不得被失败结构覆盖")
}

func TestGetCursorUsage_PersistsViaUpdateExtra(t *testing.T) {
	now := time.Now().UTC()
	repo := &cursorQuotaRefreshRepo{}
	fetcher := &stubCursorQuotaFetcher{quota: freshAvailableQuota(now)}
	svc := &AccountUsageService{accountRepo: repo, cursorFetcher: fetcher}
	account := quotaRefreshTestAccount(9, nil)

	info, err := svc.getCursorUsage(context.Background(), &account)

	require.NoError(t, err)
	require.Empty(t, info.Error)
	require.True(t, repo.writtenIDs()[9], "管理台手动拉取同样必须走 UpdateExtra，不能整行覆盖管理员同期的修改")
	require.Equal(t, CursorQuotaStateAvailable, readCursorQuota(&account).Cursor.State, "内存 account.Extra 也要同步更新")
}

func TestGetCursorUsage_BothEndpointsFailedKeepsSnapshot(t *testing.T) {
	stale := time.Now().UTC().Add(-2 * cursorQuotaStaleAfter)
	repo := &cursorQuotaRefreshRepo{}
	fetcher := &stubCursorQuotaFetcher{err: errors.New("cursor usage: both dashboard endpoints failed")}
	svc := &AccountUsageService{accountRepo: repo, cursorFetcher: fetcher}
	account := quotaRefreshTestAccount(9, cursorQuotaAt(stale, CursorQuotaStateExhausted, CursorQuotaStateAvailable, CursorQuotaStateUnavailable))

	info, err := svc.getCursorUsage(context.Background(), &account)

	require.NoError(t, err, "额度拉不到降级返回，不让管理台整页报错")
	require.Equal(t, "network_error", info.ErrorCode)
	require.Empty(t, repo.extraWrites)
}

func TestMarkCursorBucketExhausted_UsesUpdateExtraOnFreshSnapshot(t *testing.T) {
	now := time.Now().UTC()
	// 请求开始时载入的快照：other 可用。
	loaded := quotaRefreshTestAccount(3, &CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 10},
		Other:     CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 10},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateUnavailable},
		FetchedAt: &now,
	})
	// 请求进行期间，另一个请求已把 other 桶标成耗尽并落库。
	latest := quotaRefreshTestAccount(3, &CursorQuota{
		Cursor:    CursorBucketQuota{State: CursorQuotaStateAvailable, Percent: 10},
		Other:     CursorBucketQuota{State: CursorQuotaStateExhausted, Percent: 100},
		GrokBot:   CursorBucketQuota{State: CursorQuotaStateUnavailable},
		FetchedAt: &now,
	})
	repo := &cursorQuotaRefreshRepo{byID: map[int64]*Account{3: &latest}}
	svc := &GatewayService{accountRepo: repo}

	svc.markCursorBucketExhausted(context.Background(), &loaded, cursor.QuotaBucketCursor)

	require.Len(t, repo.extraWrites, 1, "必须走 UpdateExtra 原子合并，不能 Update 整行写回")
	written := readCursorQuota(&Account{Extra: map[string]any{CursorExtraQuotaKey: repo.extraWrites[0].updates[CursorExtraQuotaKey]}})
	require.Equal(t, CursorQuotaStateExhausted, written.Cursor.State)
	require.Equal(t, CursorQuotaStateExhausted, written.Other.State,
		"必须以库里最新快照为底：用请求开始时的旧快照会把别的请求刚标的耗尽抹掉")
	require.Equal(t, CursorQuotaStateExhausted, readCursorQuota(&loaded).Cursor.State, "内存 account 也要同步")
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

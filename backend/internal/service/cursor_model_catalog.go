package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// CursorModelCatalogService 周期性从 Cursor 拉取 AvailableModels 模型目录。
//
// ⚠️ 没有它，Claude Code 名称解析永远落到 cursor.DefaultModels 静态兜底：
// claude-opus-4-8 会被解析成 claude-opus-4.8 发往上游，而 Cursor 只认目录里的
// 档位变体 ID（claude-opus-4-8-medium 等），结果是每个请求都 ERROR_BAD_MODEL_NAME。
// 2026-11-12 真实账号实测：目录 250 个模型全为扁平变体 ID，基础名一律被拒。
//
// 目录是账号无关的公共信息，所以只需挑一个健康账号拉一次；拉取失败保留上一次
// 成功的目录（SetLiveModels 只在非空时替换），并落盘以便重启后立即可用。
type CursorModelCatalogService struct {
	accountRepo AccountRepository
	tokens      cursorQuotaTokenSource
	fetch       func(ctx context.Context, account *Account, accessToken string) (int, error)

	parentCtx    context.Context
	parentCancel context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	started      bool
	stopped      bool
	lockCache    LeaderLockCache
	db           *sql.DB
	instanceID   string
}

const (
	// cursorModelCatalogInterval 取 1 小时：目录只在 Cursor 上新/下架模型时变化，
	// 社区实现（cpa-cursor）同样按小时刷新；更密只是多打上游。
	cursorModelCatalogInterval = time.Hour
	// cursorModelCatalogFetchTimeout 是单次拉取（取 token + AvailableModels）的上限。
	cursorModelCatalogFetchTimeout = 60 * time.Second
	// cursorModelCatalogMaxAttempts 是单周期最多尝试的账号数：第一个账号可能
	// 恰好凭证失效或代理挂掉，换两三个就够，不在一个周期里把号池扫一遍。
	cursorModelCatalogMaxAttempts = 3

	cursorModelCatalogLeaderLockKey = "cursor:model_catalog:leader"
	cursorModelCatalogLeaderLockTTL = 5 * time.Minute
	cursorModelCatalogFileName      = "cursor_models.json"
)

func NewCursorModelCatalogService(accountRepo AccountRepository, tokens cursorQuotaTokenSource) *CursorModelCatalogService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CursorModelCatalogService{
		accountRepo:  accountRepo,
		tokens:       tokens,
		fetch:        fetchCursorModelCatalog,
		parentCtx:    ctx,
		parentCancel: cancel,
		instanceID:   uuid.NewString(),
	}
}

// ProvideCursorModelCatalogService 构造并启动目录同步：先恢复落盘的目录，再后台拉取。
func ProvideCursorModelCatalogService(
	accountRepo AccountRepository,
	tokenProvider *CursorTokenProvider,
	lockCache LeaderLockCache,
	db *sql.DB,
) *CursorModelCatalogService {
	var tokens cursorQuotaTokenSource
	if tokenProvider != nil {
		tokens = tokenProvider
	}
	// 重启后立即恢复上一次成功的目录：否则在首次拉取完成前，所有请求都按静态兜底解析。
	cursor.EnableModelsPersistence(filepath.Join(cursorModelCatalogDataDir(), cursorModelCatalogFileName))
	svc := NewCursorModelCatalogService(accountRepo, tokens)
	svc.lockCache = lockCache
	svc.db = db
	svc.Start()
	return svc
}

func (s *CursorModelCatalogService) Start() {
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

func (s *CursorModelCatalogService) Stop() {
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

func (s *CursorModelCatalogService) runLoop() {
	defer s.wg.Done()
	s.RunOnce(s.parentCtx)
	ticker := time.NewTicker(cursorModelCatalogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			s.RunOnce(s.parentCtx)
		}
	}
}

// RunOnce 执行一次目录同步，返回拉到的模型数（0 表示本次未更新）。
func (s *CursorModelCatalogService) RunOnce(ctx context.Context) int {
	if s == nil || s.accountRepo == nil || s.fetch == nil {
		return 0
	}
	release, acquired := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db,
		cursorModelCatalogLeaderLockKey, s.instanceID, cursorModelCatalogLeaderLockTTL)
	if !acquired {
		return 0
	}
	defer release()

	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformCursor, "", "", "", 0, "")
	if err != nil {
		logger.LegacyPrintf("service.cursor_model_catalog", "list_accounts_failed: err=%v", err)
		return 0
	}
	for i, account := range selectCursorCatalogAccounts(accounts) {
		if i >= cursorModelCatalogMaxAttempts {
			break
		}
		n, err := s.syncFrom(ctx, account)
		if err != nil {
			logger.LegacyPrintf("service.cursor_model_catalog", "fetch_failed: account_id=%d err=%v", account.ID, err)
			continue
		}
		if n > 0 {
			logger.LegacyPrintf("service.cursor_model_catalog", "synced: account_id=%d models=%d", account.ID, n)
			return n
		}
	}
	return 0
}

func (s *CursorModelCatalogService) syncFrom(parent context.Context, account *Account) (int, error) {
	ctx, cancel := context.WithTimeout(parent, cursorModelCatalogFetchTimeout)
	defer cancel()
	token := ""
	if s.tokens != nil {
		t, err := s.tokens.GetAccessToken(ctx, account)
		if err != nil {
			return 0, err
		}
		token = t
	}
	return s.fetch(ctx, account, token)
}

// fetchCursorModelCatalog 经账号代理拉取 AvailableModels 并刷新全局目录（非空才替换）。
func fetchCursorModelCatalog(ctx context.Context, account *Account, accessToken string) (int, error) {
	pa := cursorProtocolAccount(account)
	if strings.TrimSpace(accessToken) != "" {
		pa.AccessToken = accessToken
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := cursor.RefreshModelsFrom(sharedCursorClient(), &pa)
		ch <- result{n, err}
	}()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case r := <-ch:
		return r.n, r.err
	}
}

// selectCursorCatalogAccounts 挑出可用于拉目录的账号：直连模式、active、可调度，
// 最近额度刷新成功（更可能凭证健康）的排前面，结果按 ID 稳定。
func selectCursorCatalogAccounts(accounts []Account) []*Account {
	var out []*Account
	for i := range accounts {
		account := &accounts[i]
		if !isCursorDirectModeAccount(account) || account.Status != StatusActive || !account.Schedulable {
			continue
		}
		out = append(out, account)
	}
	sort.SliceStable(out, func(i, j int) bool {
		fi, fj := readCursorQuota(out[i]).FetchedAt, readCursorQuota(out[j]).FetchedAt
		switch {
		case fi != nil && fj == nil:
			return true
		case fi == nil && fj != nil:
			return false
		case fi != nil && fj != nil && !fi.Equal(*fj):
			return fi.After(*fj)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// cursorModelCatalogDataDir 返回目录落盘位置，口径同 setup.GetDataDir（不能直接 import：
// setup 依赖 repository，repository 依赖 service，会形成循环）。
func cursorModelCatalogDataDir() string {
	if dir := strings.TrimSpace(os.Getenv("DATA_DIR")); dir != "" {
		return dir
	}
	if info, err := os.Stat("/app/data"); err == nil && info.IsDir() {
		return "/app/data"
	}
	return "."
}

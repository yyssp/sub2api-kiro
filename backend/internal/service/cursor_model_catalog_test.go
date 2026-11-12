//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 模型目录同步：挑健康账号、失败换号、上限内收手。目录本身由 cursor.RefreshModelsFrom 写入（非空才替换）。
func TestCursorModelCatalogService_SyncsFromFirstHealthyAccount(t *testing.T) {
	now := time.Now().UTC()
	fresh := map[string]any{CursorExtraQuotaKey: map[string]any{"fetched_at": now.Format(time.RFC3339)}}
	repo := &cursorLifecycleRepo{accounts: []Account{
		{ID: 1, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusError, Schedulable: true},
		{ID: 2, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: false},
		{ID: 3, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true},
		{ID: 4, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: fresh},
	}}
	svc := NewCursorModelCatalogService(repo, nil)
	var tried []int64
	svc.fetch = func(_ context.Context, account *Account, _ string) (int, error) {
		tried = append(tried, account.ID)
		if account.ID == 4 {
			return 0, errors.New("proxy EOF")
		}
		return 250, nil
	}

	require.Equal(t, 250, svc.RunOnce(context.Background()))
	require.Equal(t, []int64{4, 3}, tried,
		"只用 active+可调度的账号；最近额度刷新成功的优先；失败就换下一个，成功即止")
}

func TestCursorModelCatalogService_StopsAfterMaxAttempts(t *testing.T) {
	var accounts []Account
	for i := int64(1); i <= 6; i++ {
		accounts = append(accounts, Account{ID: i, Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true})
	}
	svc := NewCursorModelCatalogService(&cursorLifecycleRepo{accounts: accounts}, nil)
	calls := 0
	svc.fetch = func(context.Context, *Account, string) (int, error) {
		calls++
		return 0, errors.New("upstream down")
	}
	require.Zero(t, svc.RunOnce(context.Background()))
	require.Equal(t, cursorModelCatalogMaxAttempts, calls, "上游整体故障时不能一个周期把号池扫一遍")
}

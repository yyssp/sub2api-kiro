//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// 本文件守护「接入面钩子」。Go 的 switch 没有穷尽性检查——漏一个 case 会静默
// 落到 default，编译和现有测试都不会报错，只在运行时表现为「Cursor 账号建得出来
// 但调度不到 / 不刷新 / 不出现在模型列表里」。这类漏改只能靠断言清单挡住。

func TestCursorIsAConcreteRequestPlatform(t *testing.T) {
	// composite 分组要能显式路由到 cursor。漏这个钩子会让 composite_model_routes
	// 的 cursor 路由行在校验阶段就被拒。
	require.True(t, isConcreteRequestPlatform(PlatformCursor))
}

func TestCursorAppearsInCompositeMatchingPlatforms(t *testing.T) {
	require.Contains(t, matchingPlatforms(PlatformComposite), PlatformCursor)
	require.Equal(t, []string{PlatformCursor}, matchingPlatforms(PlatformCursor))
}

func TestCursorIsInSchedulerSnapshotPlatforms(t *testing.T) {
	// ⚠️ 这里是 [11]string 定长数组：新增平台必须同时改长度，
	// 漏改会编译失败（这是好事），但改错顺序不会——所以断言内容而不只是长度。
	platforms := schedulerSnapshotPlatforms()
	require.Contains(t, platforms[:], PlatformCursor)
	require.Len(t, platforms, 11)
}

func TestCursorIsInAllPlatformsForErrorPassthrough(t *testing.T) {
	// 漏这个钩子的症状：管理台的错误透传规则页选不到 cursor。
	require.Contains(t, model.AllPlatforms(), PlatformCursor)
}

func TestCursorIsOAuthOnlyRestricted(t *testing.T) {
	// cursor 凭证本质是 OAuth 会话 token；require_oauth_only 分组必须能过滤掉
	// apikey 型 cursor 账号，否则该开关对 cursor 静默失效。
	require.True(t, isOAuthOnlyRestrictedPlatform(PlatformCursor))
	require.True(t, groupSupportsOAuthOnlyFilter(PlatformCursor))
}

func TestCursorDefaultModelListIsClaudeCodeNames(t *testing.T) {
	// ⚠️ 管理台分组默认模型清单必须是标准 Claude Code 名，不能是 Cursor 原生 ID
	// （如 composer / grok-*）——客户端按 Claude 协议发请求，拿到原生 ID 会直接 404。
	ids := defaultModelsListCandidateIDs(PlatformCursor)
	require.NotEmpty(t, ids)
	require.Equal(t, cursor.ClaudeCodeModelIDs(), ids)
	for _, id := range ids {
		require.NotContains(t, id, "composer")
	}
}

func TestCursorIsInCompositeDefaultModelCandidates(t *testing.T) {
	require.Subset(t, compositeDefaultModelsListCandidateIDs(), cursor.ClaudeCodeModelIDs())
}

func TestCursorAccountIsCursor(t *testing.T) {
	require.True(t, (&Account{Platform: PlatformCursor}).IsCursor())
	require.False(t, (&Account{Platform: PlatformKiro}).IsCursor())
}

// TestCursorTokenCacheIsInvalidatedOnCredentialChange 守护一个易漏的钩子：
// token_cache_invalidator 的 switch 默认分支是 `return nil`——漏掉 cursor
// 不会报错，只会让改完凭证后旧 token 继续被缓存命中，表现为「重新导入账号后
// 还是 401」这种极难排查的问题。
func TestCursorTokenCacheIsInvalidatedOnCredentialChange(t *testing.T) {
	cache := &recordingTokenCache{}
	inv := NewCompositeTokenCacheInvalidator(cache)
	acc := &Account{ID: 42, Platform: PlatformCursor, Type: AccountTypeOAuth}

	require.NoError(t, inv.InvalidateToken(context.Background(), acc))
	require.Contains(t, cache.deleted, CursorTokenCacheKey(acc))
}

// recordingTokenCache 只记录被删除的 key，用于断言失效范围。
type recordingTokenCache struct {
	deleted []string
}

func (c *recordingTokenCache) GetAccessToken(context.Context, string) (string, error) { return "", nil }
func (c *recordingTokenCache) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}
func (c *recordingTokenCache) DeleteAccessToken(_ context.Context, key string) error {
	c.deleted = append(c.deleted, key)
	return nil
}
func (c *recordingTokenCache) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}
func (c *recordingTokenCache) ReleaseRefreshLock(context.Context, string) error { return nil }

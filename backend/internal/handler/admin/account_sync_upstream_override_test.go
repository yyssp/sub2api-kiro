package admin

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 编辑弹层改了 base_url / API Key 还没保存时拉上游模型：用表单当前值覆盖账号的副本，
// 留空的字段沿用已保存值，且绝不能改动原账号对象（它可能来自缓存/快照）。
func TestApplyUpstreamModelSyncOverride(t *testing.T) {
	strp := func(s string) *string { return &s }
	newAccount := func() *service.Account {
		return &service.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://old.example.com", "api_key": "sk-old", "model_mapping": map[string]any{"a": "a"}}}
	}

	t.Run("form values override a copy", func(t *testing.T) {
		original := newAccount()
		got := applyUpstreamModelSyncOverride(original, strp(" https://new.example.com "), strp("sk-new"))
		require.Equal(t, "https://new.example.com", got.Credentials["base_url"])
		require.Equal(t, "sk-new", got.Credentials["api_key"])
		require.Equal(t, map[string]any{"a": "a"}, got.Credentials["model_mapping"], "其余凭证字段保留")
		require.Equal(t, "https://old.example.com", original.Credentials["base_url"], "原账号不得被改：未保存的值不能泄漏进共享对象")
		require.Equal(t, "sk-old", original.Credentials["api_key"])
	})

	t.Run("blank api key keeps the saved key", func(t *testing.T) {
		got := applyUpstreamModelSyncOverride(newAccount(), strp("https://new.example.com"), strp("  "))
		require.Equal(t, "sk-old", got.Credentials["api_key"], "编辑弹层 key 留空表示不修改")
		require.Equal(t, "https://new.example.com", got.Credentials["base_url"])
	})

	t.Run("no overrides returns the account unchanged", func(t *testing.T) {
		original := newAccount()
		require.Same(t, original, applyUpstreamModelSyncOverride(original, nil, nil))
		require.Same(t, original, applyUpstreamModelSyncOverride(original, strp(""), strp("")))
		require.Nil(t, applyUpstreamModelSyncOverride(nil, strp("x"), strp("y")))
	})
}

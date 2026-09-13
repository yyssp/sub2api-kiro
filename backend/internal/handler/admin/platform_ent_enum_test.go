//go:build unit

package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/channelmonitor"
	"github.com/Wei-Shaw/sub2api/ent/channelmonitorrequesttemplate"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Ent 的 provider 枚举是**生成代码**里的校验器，改了 ent/schema 不重新
// `go generate ./ent` 就不会生效——而且没有任何编译错误。
//
// 实际踩过的坑：迁移 239 放行了 cursor、binding tag 也补了 cursor，
// 建 cursor 渠道监控仍然 500：
//
//	ent: validator failed for field "ChannelMonitor.provider":
//	channelmonitor: invalid enum value for provider field: "cursor"
//
// 因为 ent/channelmonitor/channelmonitor.go 还是旧的生成产物。
// 这个测试调用生成出来的 ProviderValidator，让「schema 改了但忘了 generate」
// 直接测试失败，而不是等到运行时 500。
func TestEntProviderEnumAcceptsCursor(t *testing.T) {
	t.Run("channel_monitor", func(t *testing.T) {
		if err := channelmonitor.ProviderValidator(channelmonitor.Provider(service.PlatformCursor)); err != nil {
			t.Fatalf("ChannelMonitor.provider 不接受 cursor（ent 生成代码可能未同步，跑 `go generate ./ent`）: %v", err)
		}
	})

	t.Run("channel_monitor_request_template", func(t *testing.T) {
		if err := channelmonitorrequesttemplate.ProviderValidator(
			channelmonitorrequesttemplate.Provider(service.PlatformCursor),
		); err != nil {
			t.Fatalf("ChannelMonitorRequestTemplate.provider 不接受 cursor（ent 生成代码可能未同步）: %v", err)
		}
	})

	// 反向：不存在的平台仍须被拒绝，否则说明枚举被改成了放行一切。
	t.Run("rejects_unknown", func(t *testing.T) {
		if err := channelmonitor.ProviderValidator(channelmonitor.Provider("not_a_platform")); err == nil {
			t.Fatal("未知 provider 竟然通过校验——枚举失效了")
		}
	})
}

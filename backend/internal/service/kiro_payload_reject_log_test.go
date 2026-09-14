//go:build unit

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// behavior=reject 必须留下 kiro.payload_rejected 日志。
//
// 2026-09-14 实测的真实缺口：reject 时 buildKiroPayloadForAccountWithArn 带着
// ErrKiroPayloadTooLarge 提前 return，走不到后面的 logKiroPayloadTrim。
// 结果是客户端收到 413、服务端却一条日志都没有 —— 运维无从判断
// 是阈值配置过严还是客户端真的发了超大请求。
//
// 这里把全局 logger 指向临时文件，断言那条 WARN 真的落盘（而不是断言
// 某个中间布尔量），因为丢日志恰恰是"中间状态都对、最终没输出"的故障。
func TestKiroPayloadRejectEmitsWarnLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	require.NoError(t, logger.Init(logger.InitOptions{
		Level:       "info",
		Format:      "json",
		ServiceName: "test",
		Output:      logger.OutputOptions{ToFile: true, FilePath: logPath},
	}))

	// 复现早退分支里的日志动作：判别 + 落字段。
	err := fmt.Errorf("build kiro payload: %w",
		&kiropkg.ErrKiroPayloadTooLarge{Weight: 93569, Limit: 60000})

	weight, limit, ok := kiroPayloadTooLargeDetail(err)
	require.True(t, ok, "reject 错误必须可判别，否则日志分支根本不会进")

	logger.L().Warn("kiro.payload_rejected",
		zap.Int("original_weight", weight),
		zap.Int("limit_weight", limit),
		zap.Int64("account_id", 794),
	)
	logger.Sync()

	data, readErr := os.ReadFile(logPath)
	require.NoError(t, readErr)
	out := string(data)

	require.Contains(t, out, "kiro.payload_rejected",
		"reject 路径必须可观测：客户端拿到 413 而日志为空，等于故障不可排查")
	// 两个数值都必须在场：只有 weight 无法判断是阈值太严还是请求真的超大。
	require.Contains(t, out, "93569")
	require.Contains(t, out, "60000")
}

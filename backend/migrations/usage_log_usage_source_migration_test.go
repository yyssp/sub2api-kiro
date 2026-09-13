package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageLogUsageSourceMigration 校验 240 号迁移新增 usage_logs.usage_source。
//
// 该列回答的是「这笔账单的 token 是估出来的还是上游实测的」。Cursor 的
// agent.v1 不返回任何 token 用量字段，整条链路按网关侧分词器推算；不落这一列，
// 估算行和实测行写进库后完全无法区分——估算口径一旦出偏差，没有任何办法
// 圈出受影响的计费范围。
func TestUsageLogUsageSourceMigration(t *testing.T) {
	content, err := FS.ReadFile("240_add_usage_log_usage_source.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// ⚠️ 必须 IF NOT EXISTS：迁移会在已有环境上重复执行。
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS usage_source TEXT",
		"新增列必须幂等，否则重跑迁移会直接失败")

	// ⚠️ 必须可空且无默认值：存量行属于 Cursor 接入之前，
	// 全部是上游实测用量，不能被回填成 'estimated'。
	require.NotContains(t, strings.ToUpper(sql), "NOT NULL",
		"该列必须可空：存量行没有来源信息，NULL 即表示未声明")
	require.NotContains(t, strings.ToUpper(sql), "DEFAULT ",
		"不得设默认值：默认值会把存量的实测行误标成某一种来源")

	require.Contains(t, sql, "COMMENT ON COLUMN usage_logs.usage_source",
		"必须留注释说明取值与 NULL 的含义，否则后来人无法解读这一列")
}

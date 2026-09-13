//go:build unit

package repository

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ⚠️ usage_logs 的插入路径把同一份列顺序复制在 5 个地方：
//
//	1. usageLogInsertArgTypes 类型数组
//	2. 本文件里每个 INSERT / CTE 的列清单（共 4 处）
//	3. VALUES 的 $N 占位符个数
//	4. prepareUsageLogInsert().args 的取值顺序
//	5. usageLogSelectColumns 的读取顺序
//
// 编译器一个都管不住：全是字符串和 []any。任何一处漏改，插入不会报错，
// 只会把值写进**错位的列**——后面所有列跟着整体平移。等价于静默的数据损坏，
// 而且要等到有人去查报表才会发现。
//
// 本测试是这条约束唯一的自动化护栏。新增列时它会告诉你还差哪一处。

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	require.NoError(t, err)
	return string(b)
}

// argTypeColumns 解析类型数组里的列名注释。
func argTypeColumns(t *testing.T, src string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)usageLogInsertArgTypes = \[\.\.\.\]string\{(.*?)\n\}`).FindStringSubmatch(src)
	require.Len(t, m, 2, "找不到 usageLogInsertArgTypes；该数组是列顺序的唯一事实来源")
	cols := regexp.MustCompile(`"\w+",\s*//\s*(\w+)`).FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c[1])
	}
	return out
}

// insertColumnLists 解析所有 INSERT INTO usage_logs (...) 的列清单。
func insertColumnLists(t *testing.T, src string) [][]string {
	t.Helper()
	re := regexp.MustCompile(`(?s)INSERT INTO usage_logs \(\s*(.*?)\s*\)\s*(?:VALUES|\n\s*SELECT)`)
	blocks := re.FindAllStringSubmatch(src, -1)
	require.NotEmpty(t, blocks, "没有解析到任何 INSERT 列清单")
	out := make([][]string, 0, len(blocks))
	for _, b := range blocks {
		var cols []string
		for _, line := range strings.Split(b[1], "\n") {
			if s := strings.TrimSuffix(strings.TrimSpace(line), ","); s != "" {
				cols = append(cols, s)
			}
		}
		out = append(out, cols)
	}
	return out
}

// 每个 INSERT 的列清单都必须与类型数组逐项同序。
func TestUsageLogInsertColumnListsMatchArgTypes(t *testing.T) {
	src := readSourceFile(t, "usage_log_repo_insert.go")
	want := argTypeColumns(t, src)
	require.NotEmpty(t, want)

	for i, got := range insertColumnLists(t, src) {
		require.Equal(t, want, got,
			"第 %d 个 INSERT 的列顺序与 usageLogInsertArgTypes 不一致：值会写进错位的列", i+1)
	}
}

// 占位符个数必须等于列数，否则 Postgres 直接报错或参数错位。
func TestUsageLogInsertPlaceholderCountMatchesColumns(t *testing.T) {
	src := readSourceFile(t, "usage_log_repo_insert.go")
	want := len(argTypeColumns(t, src))

	maxPh := 0
	for _, m := range regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(src, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > maxPh {
			maxPh = n
		}
	}
	require.Equal(t, want, maxPh,
		"占位符最大编号与列数不符：新增列后漏改了 VALUES 的 $N 列表")
}

// args 的元素个数必须等于列数。
func TestUsageLogInsertArgsCountMatchesColumns(t *testing.T) {
	src := readSourceFile(t, "usage_log_repo_insert.go")
	want := len(argTypeColumns(t, src))

	m := regexp.MustCompile(`(?s)args: \[\]any\{(.*?)\n\t\t\},`).FindStringSubmatch(src)
	require.Len(t, m, 2, "找不到 prepareUsageLogInsert 的 args 列表")

	got := 0
	for _, line := range strings.Split(m[1], "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "//") {
			got++
		}
	}
	require.Equal(t, want, got, "args 元素数与列数不符：取值会与列错位")
}

// SELECT 列清单必须覆盖全部插入列（外加主键 id）。
//
// 只写不读同样是缺陷：列写进去了但 scan 不出来，上层永远拿到零值。
func TestUsageLogSelectColumnsCoverAllInsertedColumns(t *testing.T) {
	insertSrc := readSourceFile(t, "usage_log_repo_insert.go")
	querySrc := readSourceFile(t, "usage_log_repo_query.go")

	m := regexp.MustCompile(`usageLogSelectColumns = "(.*?)"`).FindStringSubmatch(querySrc)
	require.Len(t, m, 2, "找不到 usageLogSelectColumns")

	selected := map[string]bool{}
	for _, c := range strings.Split(m[1], ",") {
		selected[strings.TrimSpace(c)] = true
	}

	for _, col := range argTypeColumns(t, insertSrc) {
		require.True(t, selected[col],
			"列 %q 会被写入但不在 SELECT 清单里：读回来永远是零值", col)
	}
}

// usage_source 必须真的接进插入与读取两侧。
//
// 单独钉住它而不只依赖上面的通用断言：这一列是「哪些账单是估算的」的唯一
// 依据，漏接会让 Cursor 的估算计费与实测计费永久混在一起、无法回溯。
func TestUsageSourceColumnIsWiredBothWays(t *testing.T) {
	require.Contains(t, argTypeColumns(t, readSourceFile(t, "usage_log_repo_insert.go")),
		"usage_source", "usage_source 未接入插入路径")
	require.Contains(t, readSourceFile(t, "usage_log_repo_query.go"),
		"log.UsageSource = &usageSource.String", "usage_source 未接入读取路径")
}

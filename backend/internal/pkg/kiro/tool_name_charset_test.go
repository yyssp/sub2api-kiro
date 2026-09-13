package kiro

import (
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/stretchr/testify/require"
)

// AWS 官方服务模型 user-service-2.json 对 ToolName 的约束：
// { "max": 64, "pattern": "[a-zA-Z0-9_-]+" }
// 我们保守取 63（kiroMaxToolNameLen），留 1 位余量。
var kiroToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// A-1：`$` 是最常见的非法字符（Claude Code 内置工具名带 `$` 前缀）。
func TestMapKiroToolNameSanitizesDollarPrefix(t *testing.T) {
	ctx := &KiroRequestContext{}
	got := mapKiroToolName("$WEB_SEARCH", ctx)

	require.True(t, strings.HasPrefix(got, "_WEB_SEARCH"),
		"`$` 必须被替换为 `_`，实际=%q", got)
	require.Regexp(t, kiroToolNamePattern, got)
	require.Equal(t, "$WEB_SEARCH", ctx.ToolNameMap[got],
		"清洗过的名字必须能反查回原名，否则回包里的 tool_use.name 对不上客户端")
}

// A-2：连字符是**合法**字符，不得被清洗掉。
// （此前我曾误判「MCP 连字符导致 400」，已被 4 个独立来源证伪。）
func TestMapKiroToolNamePreservesHyphen(t *testing.T) {
	ctx := &KiroRequestContext{}
	got := mapKiroToolName("$MUTLI_1.N.1-Read", ctx)

	require.Contains(t, got, "-Read", "连字符合法，必须原样保留，实际=%q", got)
	require.NotContains(t, got, ".", "`.` 非法，必须被清洗")
	require.Regexp(t, kiroToolNamePattern, got)
}

// A-16：完全合法的 MCP 工具名必须**原样穿过**，不得有任何改写，
// 也不得在 ToolNameMap 里留痕（留痕会让回包走无谓的还原分支）。
func TestMapKiroToolNameLeavesValidMCPNameUntouched(t *testing.T) {
	ctx := &KiroRequestContext{}
	const name = "mcp__context7__resolve-library-id"
	got := mapKiroToolName(name, ctx)

	require.Equal(t, name, got)
	require.Empty(t, ctx.ToolNameMap, "未改写的名字不应写入反查表")
}

// A-3：清洗是多对一的（`a.b` 和 `a-b` 都可能映射到同一串），
// 必须靠「对原始名取哈希」把单射性找回来。
func TestMapKiroToolNameCharsetCollisionsStayInjective(t *testing.T) {
	ctx := &KiroRequestContext{}
	a := mapKiroToolName("a.b", ctx)
	b := mapKiroToolName("a b", ctx)
	c := mapKiroToolName("a/b", ctx)

	require.NotEqual(t, a, b)
	require.NotEqual(t, a, c)
	require.NotEqual(t, b, c)
	require.Equal(t, "a.b", ctx.ToolNameMap[a])
	require.Equal(t, "a b", ctx.ToolNameMap[b])
	require.Equal(t, "a/b", ctx.ToolNameMap[c])
}

// A-4：清洗 + 截断这两步组合起来，结果仍须同时满足长度与字符集。
// 顺序不能反：先截断再清洗，会让截断后残留的非法字符仍触发 400。
func TestMapKiroToolNameLongAndDirtyNameSatisfiesBothConstraints(t *testing.T) {
	ctx := &KiroRequestContext{}
	dirty := strings.Repeat("组件.名称/", 20) + "$end"
	got := mapKiroToolName(dirty, ctx)

	require.LessOrEqual(t, len(got), kiroMaxToolNameLen)
	require.Regexp(t, kiroToolNamePattern, got)
	require.Equal(t, dirty, ctx.ToolNameMap[got])
}

// A-20：纯 CJK / 空格名。清洗后可能全是 `_`，
// 此时哈希后缀是唯一的区分手段。
func TestMapKiroToolNameCJKNamesRemainDistinguishable(t *testing.T) {
	ctx := &KiroRequestContext{}
	a := mapKiroToolName("读取文件", ctx)
	b := mapKiroToolName("写入文件", ctx)

	require.Regexp(t, kiroToolNamePattern, a)
	require.Regexp(t, kiroToolNamePattern, b)
	require.NotEqual(t, a, b, "两个不同的 CJK 工具名不能塌缩成同一个")
	require.Equal(t, "读取文件", ctx.ToolNameMap[a])
	require.Equal(t, "写入文件", ctx.ToolNameMap[b])
}

// A-5：同一个原始名在 tools 定义处和 history 里必须映射成**同一个**短名。
// 二者不一致会让上游看到「引用了未声明的工具」，直接 400。
func TestMapKiroToolNameIsDeterministicAcrossCallSites(t *testing.T) {
	ctxA := &KiroRequestContext{}
	ctxB := &KiroRequestContext{}
	const name = "$MUTLI_1.N.1-Read"

	require.Equal(t, mapKiroToolName(name, ctxA), mapKiroToolName(name, ctxB))
}

// web_search 的特判必须先于字符集清洗生效（它本身就是合法字符集，
// 这里锁住的是「特判没被新逻辑挤掉」）。
func TestMapKiroToolNameWebSearchAliasStillApplies(t *testing.T) {
	ctx := &KiroRequestContext{}
	require.Equal(t, "remote_web_search", mapKiroToolName("web_search", ctx))
}

func TestMapKiroToolNameEmptyStaysEmpty(t *testing.T) {
	ctx := &KiroRequestContext{}
	require.Equal(t, "", mapKiroToolName("   ", ctx))
	require.Empty(t, ctx.ToolNameMap)
}

// 端到端：整个 payload 里出现的所有工具名都必须合法。
// 这是唯一能挡住「新增了某条工具名写入路径却忘了走 mapKiroToolName」的断言。
func TestBuildKiroPayloadAllToolNamesAreUpstreamLegal(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-5-20250929",
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"t1","name":"$MUTLI_1.N.1-Read","input":{"path":"/a"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":"ok"}
			]}
		],
		"tools": [
			{"name":"$MUTLI_1.N.1-Read","description":"d","input_schema":{"type":"object","properties":{}}},
			{"name":"读取文件","description":"d","input_schema":{"type":"object","properties":{}}},
			{"name":"mcp__context7__resolve-library-id","description":"d","input_schema":{"type":"object","properties":{}}}
		]
	}`)

	result, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "arn:test", "AI_EDITOR", nil)
	require.NoError(t, err)

	names := extractAllToolNames(string(result.Payload))
	require.NotEmpty(t, names, "断言本身要有东西可断，否则这个测试是空转的")

	for _, name := range names {
		require.Regexp(t, kiroToolNamePattern, name,
			"payload 中出现了上游会拒绝的工具名 %q", name)
		require.LessOrEqual(t, len(name), kiroMaxToolNameLen, "工具名 %q 超长", name)
	}

	// 三个原始名里有两个需要改写，第三个合法名不该进反查表。
	require.Len(t, result.Context.ToolNameMap, 2)
}

// extractAllToolNames 收集 payload 里所有承载「工具名」语义的字段，
// 覆盖 tools 定义、history 中的 toolUses 以及 toolResults。
func extractAllToolNames(payload string) []string {
	var names []string
	for _, path := range []string{
		`conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.#.toolSpecification.name`,
		`conversationState.history.#.assistantResponseMessage.toolUses.#.name`,
		`conversationState.history.#.userInputMessage.userInputMessageContext.tools.#.toolSpecification.name`,
	} {
		collectStringLeaves(gjson.Get(payload, path), &names)
	}
	return names
}

func collectStringLeaves(res gjson.Result, out *[]string) {
	if res.IsArray() {
		for _, item := range res.Array() {
			collectStringLeaves(item, out)
		}
		return
	}
	if s := res.String(); s != "" {
		*out = append(*out, s)
	}
}

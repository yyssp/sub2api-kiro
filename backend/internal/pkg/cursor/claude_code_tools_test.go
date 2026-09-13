package cursor

import (
	"strings"
	"testing"
)

// claudeCodeMarkerTools 是「携带动态工具目录的主 Claude Code 请求」的特征工具。
func claudeCodeMarkerTools() []ToolDef {
	return []ToolDef{
		{Name: "Skill"},
		{Name: "ToolSearch"},
		{Name: "Edit"},
	}
}

func toolNameSet(tools []ToolDef) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, td := range tools {
		out[strings.ToLower(strings.TrimSpace(td.Name))] = true
	}
	return out
}

// 主 Claude Code 请求必须补齐 Bash/Read/Agent。
//
// 漏补的后果不是少一个能力：Cursor 上游仍会返回 shell/read 原生分支，
// 本轮 tools 里没有对应定义时 agent.go 抛 ErrUndeclaredUpstreamTool，整轮夭折。
func TestMergeClaudeCodeCoreTools_SeedsBashReadAgent(t *testing.T) {
	merged, changed := MergeClaudeCodeCoreTools("claude-sonnet-4.5", claudeCodeMarkerTools())
	if !changed {
		t.Fatal("主 Claude Code 请求未补齐核心工具，上游返回 shell/read 分支时会抛 ErrUndeclaredUpstreamTool")
	}
	names := toolNameSet(merged)
	for _, want := range []string{"bash", "read", "agent"} {
		if !names[want] {
			t.Errorf("核心工具 %q 未补齐", want)
		}
	}
	// 客户端原有工具必须原样保留。
	for _, want := range []string{"skill", "toolsearch", "edit"} {
		if !names[want] {
			t.Errorf("客户端声明的工具 %q 被丢失", want)
		}
	}
}

// ⚠️ 只能补 Bash/Read/Agent。把 LS/Glob/Grep/Delete 等客户端本轮没声明的工具
// 伪造进去，上游一旦选中，CLI 会以 "No such tool available" 拒绝整轮——比不补更糟。
func TestMergeClaudeCodeCoreTools_DoesNotSeedUndeclaredExtras(t *testing.T) {
	merged, _ := MergeClaudeCodeCoreTools("claude-sonnet-4.5", claudeCodeMarkerTools())
	names := toolNameSet(merged)
	for _, forbidden := range []string{"ls", "glob", "grep", "delete", "write", "todowrite"} {
		if names[forbidden] {
			t.Errorf("补入了客户端未声明的工具 %q：上游选中它时 CLI 会拒绝整轮", forbidden)
		}
	}
}

// 子 Agent / 受限权限 / 显式 --tools 集合（marker < 2）必须原样保留。
func TestMergeClaudeCodeCoreTools_SkipsRestrictedToolSets(t *testing.T) {
	cases := map[string][]ToolDef{
		"无 marker":     {{Name: "Read"}, {Name: "Task"}},
		"仅 1 个 marker": {{Name: "Skill"}, {Name: "Read"}},
	}
	for name, tools := range cases {
		t.Run(name, func(t *testing.T) {
			merged, changed := MergeClaudeCodeCoreTools("claude-sonnet-4.5", tools)
			if changed {
				t.Errorf("受限工具集被补齐了：%v", toolNameSet(merged))
			}
		})
	}
}

// 纯文本请求（无工具）走 direct Sand 路径，凭空加工具会改变上游应答形态。
func TestMergeClaudeCodeCoreTools_LeavesToollessRequestsAlone(t *testing.T) {
	if _, changed := MergeClaudeCodeCoreTools("claude-sonnet-4.5", nil); changed {
		t.Error("无工具的纯文本请求被补齐了工具")
	}
}

// 非 Claude Code 模型不得被凭空增加能力。
func TestMergeClaudeCodeCoreTools_IgnoresNonClaudeModels(t *testing.T) {
	if _, changed := MergeClaudeCodeCoreTools("gpt-4o", claudeCodeMarkerTools()); changed {
		t.Error("非 Claude 模型被补齐了 Claude Code 核心工具")
	}
}

// Auto/default（Free 套餐唯一可用）仍然是 Claude Code 请求。
func TestMergeClaudeCodeCoreTools_HandlesAutoModel(t *testing.T) {
	if _, changed := MergeClaudeCodeCoreTools("auto", claudeCodeMarkerTools()); !changed {
		t.Error("Auto 模型下未补齐核心工具")
	}
}

// 已声明的工具不得被重复补入。
func TestMergeClaudeCodeCoreTools_DoesNotDuplicate(t *testing.T) {
	tools := append(claudeCodeMarkerTools(), ToolDef{Name: "Bash"}, ToolDef{Name: "Read"})
	merged, _ := MergeClaudeCodeCoreTools("claude-sonnet-4.5", tools)
	count := 0
	for _, td := range merged {
		if strings.EqualFold(strings.TrimSpace(td.Name), "Bash") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Bash 出现 %d 次，应为 1 次", count)
	}
}

// Skill/ToolSearch/DeferredToolPlaceholder 必须被摘掉，功能性工具必须保留。
//
// Skill 的结果由 Claude Code 本地展开整棵 skill 树，透给上游会让**下一轮请求
// 在客户端侧**被判 "Prompt is too long" 而硬卡死，网关侧看不到任何线索。
func TestSuppressClaudeCodeContextExpansionTools(t *testing.T) {
	tools := []ToolDef{
		{Name: "Skill"}, {Name: "ToolSearch"}, {Name: "DeferredToolPlaceholder"},
		{Name: "Read"}, {Name: "Bash"}, {Name: "mcp__github__create_issue"},
	}
	filtered, suppressed := SuppressClaudeCodeContextExpansionTools("claude-sonnet-4.5", tools)
	if len(suppressed) != 3 {
		t.Fatalf("应抑制 3 个元工具，实际 %d：%v", len(suppressed), suppressed)
	}
	names := toolNameSet(filtered)
	for _, gone := range []string{"skill", "toolsearch", "deferredtoolplaceholder"} {
		if names[gone] {
			t.Errorf("元工具 %q 未被抑制", gone)
		}
	}
	for _, kept := range []string{"read", "bash", "mcp__github__create_issue"} {
		if !names[kept] {
			t.Errorf("功能性工具 %q 被误删", kept)
		}
	}
}

// 没有元工具时必须原样返回，不产生无谓的约束文案注入。
func TestSuppressClaudeCodeContextExpansionTools_NoopWithoutMetaTools(t *testing.T) {
	tools := []ToolDef{{Name: "Read"}, {Name: "Bash"}}
	_, suppressed := SuppressClaudeCodeContextExpansionTools("claude-sonnet-4.5", tools)
	if len(suppressed) != 0 {
		t.Errorf("无元工具时不应有抑制项：%v", suppressed)
	}
}

// ⚠️ 顺序约束：先补齐后抑制。反过来会让补齐静默失效——
// 抑制摘掉的正是补齐用来判定的 marker 工具。
func TestMergeThenSuppress_OrderMatters(t *testing.T) {
	tools := claudeCodeMarkerTools()

	// 正确顺序：先 merge 后 suppress。
	merged, changed := MergeClaudeCodeCoreTools("claude-sonnet-4.5", tools)
	if !changed {
		t.Fatal("正确顺序下补齐未触发")
	}
	final, suppressed := SuppressClaudeCodeContextExpansionTools("claude-sonnet-4.5", merged)
	if len(suppressed) == 0 {
		t.Fatal("正确顺序下抑制未触发")
	}
	names := toolNameSet(final)
	if !names["bash"] || !names["read"] {
		t.Error("正确顺序下核心工具应当已补齐并保留")
	}

	// 错误顺序：先 suppress 会摘掉 marker，导致 merge 不再触发。
	filteredFirst, _ := SuppressClaudeCodeContextExpansionTools("claude-sonnet-4.5", tools)
	if _, changedAfter := MergeClaudeCodeCoreTools("claude-sonnet-4.5", filteredFirst); changedAfter {
		t.Error("顺序反了却仍然补齐了——本测试已无法守住顺序约束，请重新确认 marker 判定逻辑")
	}
}

// 约束文案必须前置到首条消息，且多轮不得重复累积。
func TestAppendClaudeCodeContextExpansionConstraint(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: "帮我改代码"}}

	once := AppendClaudeCodeContextExpansionConstraint(msgs)
	if !strings.HasPrefix(once[0].Content, ClaudeCodeContextExpansionConstraint) {
		t.Fatal("约束文案未前置到首条消息")
	}
	if !strings.Contains(once[0].Content, "帮我改代码") {
		t.Fatal("原始消息内容丢失")
	}

	twice := AppendClaudeCodeContextExpansionConstraint(once)
	if strings.Count(twice[0].Content, ClaudeCodeContextExpansionConstraint) != 1 {
		t.Error("约束文案重复注入，多轮会不断累积")
	}

	// 不得就地改写调用方的切片。
	if strings.HasPrefix(msgs[0].Content, ClaudeCodeContextExpansionConstraint) {
		t.Error("就地改写了入参切片")
	}
}

func TestAppendClaudeCodeContextExpansionConstraint_EmptyMessages(t *testing.T) {
	out := AppendClaudeCodeContextExpansionConstraint(nil)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("空消息列表应产生一条 user 消息，实际 %+v", out)
	}
}

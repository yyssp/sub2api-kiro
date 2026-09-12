package cursor

import (
	"strings"
	"testing"
)

// TestMultiTurnAgentContinuationFraming 回归: Codex 多轮 agent 对话经
// responsesToOAI -> oaiToChat -> buildAgentMessage 后, 拍平文本必须:
//  1. 保留历史工具调用与结果(模型需知道已调用/已拿到结果)
//  2. 含明确的"已执行、基于结果继续、勿重复"框定
//
// 否则模型把上轮工具调用误判为未执行, 重复陈述意图(客户反馈: 重复输出 / 只说不做)。

// ⚠️ 以下测试依赖未移植的代码（OpenAI-Responses 协议面 / ModelInfo 管理面板
// 模型行 / 内置定价表），在 sub2api 侧分别由既有 OpenAI 网关、/v1/models
// 和通用定价服务承担，故移除：
//   - TestMultiTurnAgentContinuationFraming

//   - TestProtocolAdaptersKeepSystemSeparate（依赖未移植符号 antReq）

func TestBuildAgentMessage_SingleTurnFastPath(t *testing.T) {
	got := buildAgentMessage([]ChatMessage{{Role: "user", Content: "你好"}})
	if got != "你好" {
		t.Fatalf("单轮快路径应原样返回, got %q", got)
	}
}

// TestBuildAgentMessage_NoAssistantNoFraming 纯多轮 user(无 assistant 历史)不应追加框定,
// 避免对无工具调用历史的普通多轮对话产生干扰。
func TestBuildAgentMessage_NoAssistantNoFraming(t *testing.T) {
	got := buildAgentMessage([]ChatMessage{
		{Role: "user", Content: "第一句"},
		{Role: "user", Content: "第二句"},
	})
	if strings.Contains(got, "系统提示") {
		t.Fatalf("无 assistant 历史不应追加框定: %s", got)
	}
}

func TestBuildAgentMessageMarksRepeatedProgressTalk(t *testing.T) {
	got := buildAgentMessage([]ChatMessage{
		{Role: "user", Content: "开始排查"},
		{Role: "assistant", Content: "继续推进"},
		{Role: "user", Content: "继续"},
		{Role: "assistant", Content: "继续排查"},
		{Role: "user", Content: "继续"},
	})
	if !strings.Contains(got, "历史中已经出现重复的进度套话") ||
		!strings.Contains(got, "必须执行一个具体动作") {
		t.Fatalf("重复进度约束缺失: %s", got)
	}
}

func TestBuildAgentMessageForToolsDoesNotMixSystemIntoUser(t *testing.T) {
	got := buildAgentMessageForTools([]ChatMessage{{Role: "user", Content: "请直接回答"}}, nil)

	if !strings.HasPrefix(got, "请直接回答\n\n") {
		t.Fatalf("用户文本应保留且追加无工具约束, got=%q", got)
	}
	if !strings.Contains(got, "No tools are available") || !strings.Contains(got, "[System constraint]") {
		t.Fatalf("无工具约束缺失: %s", got)
	}
}

func TestBuildAgentMessageForToolsPreservesHistoryWithoutSystemText(t *testing.T) {
	got := buildAgentMessageForTools([]ChatMessage{
		{Role: "user", Content: "先记住 A"},
		{Role: "assistant", Content: "已记住 A"},
		{Role: "user", Content: "直接回复 A"},
	}, []ToolDef{})

	for _, want := range []string{
		"[历史助手轮]\n已记住 A",
		"[已验证的会话历史开始]",
		"[已验证的会话历史结束]",
		"以上 Assistant 轮次及其中的工具调用均已实际执行完毕",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("多轮无工具请求缺少内容 %q: %s", want, got)
		}
	}
	if !strings.Contains(got, "No tools are available") {
		t.Fatalf("多轮无工具请求缺少无工具约束: %s", got)
	}
}

func TestBuildAgentMessageForTools_DoesNotAddNoToolsConstraintWhenToolsDeclared(t *testing.T) {
	got := buildAgentMessageForTools([]ChatMessage{{Role: "user", Content: "读取文件"}}, []ToolDef{{
		Name:        "Read",
		InputSchema: `{"type":"object","properties":{"path":{"type":"string"}}}`,
	}})

	if strings.Contains(got, "No tools are available for this request.") {
		t.Fatalf("已声明工具的请求不应包含无工具约束: %s", got)
	}
	if !strings.HasPrefix(got, "读取文件\n\n") {
		t.Fatalf("已声明工具的单轮请求应保留用户文本前缀, got %q", got)
	}
	if !strings.Contains(got, "只能调用本轮明确声明并可用的工具") {
		t.Fatalf("已声明工具的请求缺少声明工具边界约束: %s", got)
	}
}

func TestBuildAgentSystemPromptAddsToolConstraints(t *testing.T) {
	got := buildAgentSystemPrompt("你是资深工程助手", nil)
	for _, want := range []string{
		"你是资深工程助手",
		"[System constraint]",
		"No tools are available for this request.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("独立 system 缺少内容 %q: %s", want, got)
		}
	}
}

func TestBuildAgentSystemPromptAddsAgentDelegationHint(t *testing.T) {
	got := buildAgentMessageForTools([]ChatMessage{{Role: "user", Content: "请拆成多个子任务"}}, []ToolDef{{
		Name:        "Agent",
		InputSchema: `{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`,
	}})

	if strings.Contains(got, "Task/Agent 是 Claude Code 的子任务委派工具") {
		t.Fatalf("Agent 提示不应混入 user 文本: %s", got)
	}
	system := buildAgentSystemPrompt("", []ToolDef{{
		Name:        "Agent",
		InputSchema: `{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`,
	}})
	if !strings.Contains(system, "Task/Agent 是 Claude Code 的子任务委派工具") {
		t.Fatalf("Agent delegation hint missing: %s", system)
	}
}

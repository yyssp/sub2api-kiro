package cursor

import (
	"strings"
	"testing"
)

// 协议转换的边界补充。
//
// 主体覆盖已在 agent_multiturn_test.go / anthropic_messages_test.go /
// multimodal_test.go / native_edit_readfile_test.go 中；这里只补两处
// 没有直接断言的分支，避免与既有用例重复。

// Task/Agent 在 wire 上统一叫 Agent：Cursor agent.v1 目前只认这个名字。
// Claude Code 不同版本会把同一个委派工具叫 Task 或 Agent，直接把 Task
// 发上去会被上游忽略，模型于是改用未声明的 native 工具。
//
// 既有的 TestBuildAgentClientMessageUsesRawAgentToolName 只验证了编码结果，
// 这里直接锁 toolWireName 自身的映射与前缀规则。
func TestToolWireName_TaskNormalizedToAgent(t *testing.T) {
	for _, name := range []string{"Task", "task", "Agent", "agent"} {
		if got := toolWireName(ToolDef{Name: name}); got != "Agent" {
			t.Errorf("toolWireName(%q) = %q, want Agent", name, got)
		}
	}
	// 其他工具要带前缀，避免与 Cursor 原生工具重名后被当成原生实现。
	if got := toolWireName(ToolDef{Name: "Read"}); got == "Read" {
		t.Error("普通工具应带 wire 前缀以区别于 Cursor 原生工具")
	}
}

// input_schema 是 protobuf Struct，不是 JSON 字符串。
// 非法 schema 必须回落成空 object，而不是产出空字节：
// 空字段会让 Cursor 忽略整个 MCP 目录，转而生成未声明的 native read/shell。
func TestEncodeMCPInputSchema_InvalidSchemaFallsBackToObject(t *testing.T) {
	got := encodeMCPInputSchema("{not json")
	if len(got) == 0 {
		t.Fatal("非法 schema 应回落为空 object，而不是产出空字节")
	}

	// 合法 schema 的属性名以 Struct key 形式明文出现。
	withField := encodeMCPInputSchema(`{"type":"object","properties":{"file_path":{"type":"string"}}}`)
	if !strings.Contains(string(withField), "file_path") {
		t.Error("schema 的属性名应以 Struct key 形式出现")
	}
}

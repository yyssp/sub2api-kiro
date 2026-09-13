//go:build unit

package cursor

import (
	"strings"
	"testing"
)

// buildToolCallUpdateFrame 构造一条 ToolCallStartedUpdate：
// field1 = call_id, field2 = 工具 oneof（field15 = mcp_tool_call）。
// 用 pbBytes 而非 protoString：后者会跳过空值，而本用例需要把空 ID 也发上线。
func buildToolCallUpdateFrame(callID, toolName string) []byte {
	args := protoString(1, toolName)
	mcpCall := protoMessage(1, args)
	inner := protoMessage(15, mcpCall)
	return append(pbBytes(1, []byte(callID)), protoMessage(2, inner)...)
}

// declaredTools 声明测试工具。parseToolCallUpdate 会拒绝未声明的工具
// （这是有意的上游防护），因此不能传 nil。
func declaredTools(names ...string) map[string]ToolDef {
	out := make(map[string]ToolDef, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = ToolDef{Name: n, InputSchema: "{}"}
	}
	return out
}

// TestParseToolCallUpdate_SanitizesEmbeddedNewline 覆盖 C 项的核心场景：
// grok 会把两个 tool-call ID 用换行拼接后返回，我们必须只取第一个。
// 还原改动前的 call.ID = string(cid.Data) 会让本用例失败。
func TestParseToolCallUpdate_SanitizesEmbeddedNewline(t *testing.T) {
	frame := buildToolCallUpdateFrame("toolu_aaa\ntoolu_bbb", "my_tool")

	call, err := parseToolCallUpdate(frame, declaredTools("my_tool"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if strings.ContainsAny(call.ID, "\r\n") {
		t.Fatalf("tool-call ID 仍含换行: %q", call.ID)
	}
	if call.ID != "toolu_aaa" {
		t.Fatalf("应取第一行 ID, 实际 %q", call.ID)
	}
}

func TestParseToolCallUpdate_StripsControlCharsAndSpace(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"前后空白", "  toolu_abc  ", "toolu_abc"},
		{"内嵌制表符", "toolu_a\tbc", "toolu_abc"},
		{"内嵌 NUL", "toolu_a\x00bc", "toolu_abc"},
		{"CRLF 拼接", "toolu_aaa\r\ntoolu_bbb", "toolu_aaa"},
		{"首行为空", "\ntoolu_bbb", "toolu_bbb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := parseToolCallUpdate(buildToolCallUpdateFrame(tc.raw, "my_tool"), declaredTools("my_tool"))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if call.ID != tc.want {
				t.Fatalf("期望 %q, 实际 %q", tc.want, call.ID)
			}
		})
	}
}

// TestParseToolCallUpdate_PreservesNormalID 反向护栏：
// 防止「净化」误伤正常 ID（含下划线、连字符、大小写）。
func TestParseToolCallUpdate_PreservesNormalID(t *testing.T) {
	const want = "toolu_01A-bcDEF_9"
	call, err := parseToolCallUpdate(buildToolCallUpdateFrame(want, "my_tool"), declaredTools("my_tool"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if call.ID != want {
		t.Fatalf("正常 ID 被改写: 期望 %q, 实际 %q", want, call.ID)
	}
}

// TestSanitizeToolCallID_AllEmptyInputs 确认净化后为空时返回空串，
// 交由下游 cursor_runtime.go 的兜底逻辑生成 ID，而不是塞一个畸形值下去。
func TestSanitizeToolCallID_AllEmptyInputs(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\n", "\x00\x01"} {
		if got := sanitizeToolCallID(raw); got != "" {
			t.Fatalf("输入 %q 应净化为空串, 实际 %q", raw, got)
		}
	}
}

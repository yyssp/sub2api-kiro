package service

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// 校验必须挂在 forwardCursorMessages 里，且挂在模型映射「之后」。
// 只校验 originalModel 会让一条 "claude-opus-4-6 -> auto" 的映射绕过整道闸门。
func TestForwardCursorMessages_ValidatesMappedModel(t *testing.T) {
	src, err := os.ReadFile("cursor_runtime.go")
	if err != nil {
		t.Fatalf("read cursor_runtime.go: %v", err)
	}
	body := string(src)

	idx := strings.Index(body, "func (s *GatewayService) forwardCursorMessages(")
	if idx < 0 {
		t.Fatal("forwardCursorMessages 不存在")
	}
	fn := body[idx:]
	if end := strings.Index(fn, "\nfunc "); end > 0 {
		fn = fn[:end]
	}

	if !strings.Contains(fn, "cursor.ValidateDownstreamModel(mappedModel)") {
		t.Fatal("forwardCursorMessages 未对 mappedModel 调用 ValidateDownstreamModel；" +
			"漏掉会让 auto/composer 直达上游并按最贵模型静默兜底计费")
	}

	// 必须在映射之后
	mapIdx := strings.Index(fn, "GetMappedModel(")
	valIdx := strings.Index(fn, "cursor.ValidateDownstreamModel(")
	if mapIdx < 0 || valIdx < 0 || valIdx < mapIdx {
		t.Fatal("模型校验必须发生在 GetMappedModel 之后，否则映射出的 auto 可绕过校验")
	}

	// 必须在进协议层之前
	reqIdx := strings.Index(fn, "cursorRequestFromBody(")
	if reqIdx < 0 || valIdx > reqIdx {
		t.Fatal("模型校验必须发生在 cursorRequestFromBody 之前")
	}
}

// 模型名错误是客户端错误，必须 400 且不 failover。
// 走 failover 会把整个号池按同一个原因烧一遍。
func TestCursorInvalidModel_UsesNonFailoverContract(t *testing.T) {
	src, err := os.ReadFile("cursor_runtime.go")
	if err != nil {
		t.Fatalf("read cursor_runtime.go: %v", err)
	}
	body := string(src)
	idx := strings.Index(body, "cursor.ValidateDownstreamModel(mappedModel)")
	if idx < 0 {
		t.Fatal("找不到校验调用点")
	}
	// 只取该 if 块本身（到块尾的 "\n\t}"），再宽会吃进下面 token 获取那段
	// 合法的 cursorCredentialFailover。注释行要剔除：块内注释正是在解释
	// 「为什么不能 failover」，带上会让下面的断言自己打自己。
	window := body[idx:]
	if end := strings.Index(window, "\n\t}"); end > 0 {
		window = window[:end]
	}
	var code []string
	for _, line := range strings.Split(window, "\n") {
		if trimmed := strings.TrimSpace(line); !strings.HasPrefix(trimmed, "//") {
			code = append(code, line)
		}
	}
	window = strings.Join(code, "\n")

	if strings.Contains(window, "Failover") || strings.Contains(window, "failover") {
		t.Fatal("模型校验失败不得走 failover 契约：客户端错误换号重试只会烧光号池")
	}
	if !strings.Contains(window, "BetaBlockedError") {
		t.Fatal("模型校验失败应复用 BetaBlockedError（handler 对其为 400 invalid_request_error 且不 failover）")
	}
}

// 文案要能让用户知道该换成什么，否则只会反复重试同一个 auto。
func TestCursorInvalidModelMessage_IsActionable(t *testing.T) {
	msg := cursorInvalidModelMessage("auto", cursor.ErrServerSideRoutedModel)
	if !strings.Contains(msg, "auto") {
		t.Fatalf("文案应包含被拒模型名: %q", msg)
	}
	if !strings.Contains(msg, "claude-") {
		t.Fatalf("文案应给出可用的标准模型示例: %q", msg)
	}

	other := cursorInvalidModelMessage("gpt-5", cursor.ErrUnsupportedDownstreamModel)
	if !strings.Contains(other, "gpt-5") || !strings.Contains(other, "claude-") {
		t.Fatalf("不支持模型的文案同样要可操作: %q", other)
	}
	// 两类错误的文案必须可区分：选路别名要解释「为什么不给用」
	if msg == other {
		t.Fatal("服务端选路别名与普通不支持模型应给出不同文案")
	}
	if !errors.Is(cursor.ErrServerSideRoutedModel, cursor.ErrServerSideRoutedModel) {
		t.Fatal("sanity")
	}
}

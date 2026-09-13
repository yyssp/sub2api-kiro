package cursor

import (
	"errors"
	"strings"
	"testing"
)

// 网关只对下游暴露标准 Claude 协议模型；Cursor 的服务端选路别名
// (auto/default/composer) 必须被拒绝，因为 agent.v1 响应信封里没有 model
// 字段，无法得知实际服务方，也就无法按真实模型计费。

func TestValidateDownstreamModel_RejectsServerSideRoutedAliases(t *testing.T) {
	for _, model := range []string{
		"auto", "AUTO", " auto ",
		"default", "Default",
		"composer", "composer-2", "composer-2.5",
		"cursor/auto", "cursor/composer-2.5",
	} {
		err := ValidateDownstreamModel(model)
		if err == nil {
			t.Fatalf("ValidateDownstreamModel(%q) = nil, 服务端选路别名必须被拒绝", model)
		}
		if !errors.Is(err, ErrServerSideRoutedModel) {
			t.Fatalf("ValidateDownstreamModel(%q) err = %v, 应为 ErrServerSideRoutedModel", model, err)
		}
	}
}

func TestValidateDownstreamModel_RejectsEmptyModel(t *testing.T) {
	// 空模型曾在 agent.go 里兜底成 "default"，等于静默走服务端选路。
	for _, model := range []string{"", "   "} {
		err := ValidateDownstreamModel(model)
		if err == nil {
			t.Fatalf("ValidateDownstreamModel(%q) = nil, 空模型必须被拒绝", model)
		}
		if !errors.Is(err, ErrUnsupportedDownstreamModel) {
			t.Fatalf("ValidateDownstreamModel(%q) err = %v, 应为 ErrUnsupportedDownstreamModel", model, err)
		}
	}
}

func TestValidateDownstreamModel_RejectsNonClaudeProtocolModels(t *testing.T) {
	// Cursor 协议内部名与第三方名都不是对外契约。
	for _, model := range []string{
		"gpt-5", "gpt-5.6-luna", "gemini-3-pro", "grok-4", "kimi-k2",
		"totally-bogus-model",
	} {
		err := ValidateDownstreamModel(model)
		if err == nil {
			t.Fatalf("ValidateDownstreamModel(%q) = nil, 非标准 Claude 模型必须被拒绝", model)
		}
		if !errors.Is(err, ErrUnsupportedDownstreamModel) {
			t.Fatalf("ValidateDownstreamModel(%q) err = %v, 应为 ErrUnsupportedDownstreamModel", model, err)
		}
	}
}

// 反向护栏：过度收紧会把正常的标准模型也挡掉，那比漏放更糟（全平台不可用）。
func TestValidateDownstreamModel_AcceptsStandardClaudeModels(t *testing.T) {
	for _, model := range []string{
		"claude-opus-4-6",
		"claude-sonnet-4-5-20250929",
		"claude-opus-4.6",
		"cursor/claude-opus-4-6",
	} {
		if err := ValidateDownstreamModel(model); err != nil {
			t.Fatalf("ValidateDownstreamModel(%q) = %v, 标准 Claude 模型必须放行", model, err)
		}
	}
}

// 错误文案要带上原始模型名，否则用户拿到 400 也不知道是哪个模型被拒。
func TestValidateDownstreamModel_ErrorMentionsModel(t *testing.T) {
	err := ValidateDownstreamModel("auto")
	if err == nil || !strings.Contains(err.Error(), "auto") {
		t.Fatalf("错误文案应包含被拒模型名, got %v", err)
	}
}

// normalizeCursorModel 不得再把 auto 变成 default：那是「代为构造服务端选路」。
func TestNormalizeCursorModel_NoLongerManufacturesDefault(t *testing.T) {
	if got := normalizeCursorModel("auto"); got == "default" {
		t.Fatal("normalizeCursorModel(\"auto\") 仍返回 \"default\"，服务端选路别名不得由网关代为构造")
	}
}

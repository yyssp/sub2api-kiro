//go:build unit

package service

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func readUsageBillingSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("gateway_usage_billing.go")
	require.NoError(t, err)
	return string(src)
}

// ⚠️ 本组钉的是「Cursor 的不可定价模型不得静默免费」。
//
// 缺陷链路（逐环核实）：
//  1. Cursor 客户端可以直接请求 model="auto"/"default"/"composer"，
//     这些是 Cursor 的服务端选模型别名，被原样透传给上游。
//  2. 计费用的是 ForwardResult.Model = originalModel（客户端请求名），
//     而 auto/default/composer 在内置目录与 LiteLLM 里都没有价格条目。
//  3. billableModelWithFallback 的候选是 UpstreamModel 与 Model——
//     对 auto 请求这两个同样是 auto，全部无价，于是原样返回。
//  4. CalculateTokenCostForRequest 返回 ErrModelPricingUnavailable，
//     落到 `return &CostBreakdown{ActualCost: 0}`——**请求免费送**。
//
// Kiro 早就有 calculateKiroConservativeTokenCost 专门挡这个，
// 但它 gated 在 opts.IsKiroAccount，Cursor 永远够不到。
//
// 方向选择：与 Kiro 一致取「保守」而非「精确」。auto 由 Cursor 服务端选模型，
// 响应里拿不到真实模型名，不存在可推导的精确价；此时宁可按池内最贵计价
// （多收可退，免费送不可追回），也不能记 0。

func TestShouldUseCursorConservativeBillingFallback_AutoModelIsCovered(t *testing.T) {
	// auto/default/composer 都是服务端选模型别名，均无价可循。
	for _, model := range []string{"auto", "default", "composer", "AUTO", " auto "} {
		result := &ForwardResult{Model: model}
		opts := &recordUsageOpts{IsCursorAccount: true}
		require.True(t, shouldUseCursorConservativeBillingFallback(result, model, opts),
			"model=%q 无价可循，必须走保守兜底而不是记 0 元", model)
	}
}

// ⚠️ 非 Cursor 账号不得被这条兜底影响。
func TestShouldUseCursorConservativeBillingFallback_OnlyAppliesToCursor(t *testing.T) {
	result := &ForwardResult{Model: "auto"}
	require.False(t, shouldUseCursorConservativeBillingFallback(result, "auto", &recordUsageOpts{}),
		"非 Cursor 账号不得走 Cursor 的兜底价")
	require.False(t, shouldUseCursorConservativeBillingFallback(result, "auto", nil),
		"opts 为 nil 时不得兜底")
	require.False(t, shouldUseCursorConservativeBillingFallback(nil, "auto", &recordUsageOpts{IsCursorAccount: true}),
		"result 为 nil 时不得兜底")
}

// ⚠️ 反面护栏：有明确价格的具体模型必须照常走正常计费。
//
// 少了这条，把兜底改成「Cursor 一律按最贵计价」也能让上面变绿——
// 那会让所有 Cursor 请求被超额收费。
func TestShouldUseCursorConservativeBillingFallback_ConcreteModelNotAffected(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4.5", "gpt-5.1", "claude-opus-4-6"} {
		result := &ForwardResult{Model: model}
		opts := &recordUsageOpts{IsCursorAccount: true}
		require.False(t, shouldUseCursorConservativeBillingFallback(result, model, opts),
			"model=%q 有明确定价，必须走正常计费，不能按最贵兜底", model)
	}
}

// ⚠️ 接线守卫：IsCursorAccount 必须真的在 recordUsage 里被赋值。
//
// 谓词写好却没人设这个标志，就等于兜底永不触发——
// 与 CursorAccountUsableForModel 此前「零生产调用者」是同一类缺陷。
func TestRecordUsage_SetsIsCursorAccountFlag(t *testing.T) {
	src := readUsageBillingSource(t)
	require.Contains(t, src, "opts.IsCursorAccount = account != nil && account.Platform == PlatformCursor",
		"IsCursorAccount 必须在 recordUsage 主链路里按平台赋值，否则保守兜底永不生效")
}

// ⚠️ 接线守卫：兜底必须挂在定价失败的那条分支上。
func TestCalculateTokenCost_WiresCursorConservativeFallback(t *testing.T) {
	src := readUsageBillingSource(t)
	require.Contains(t, src, "shouldUseCursorConservativeBillingFallback(result, billingModel, opts)",
		"定价失败分支必须咨询 Cursor 保守兜底，否则仍会落到 ActualCost: 0")
}

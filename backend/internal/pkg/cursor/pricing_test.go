package cursor

// ⚠️ 以下测试依赖未移植的代码（OpenAI-Responses 协议面 / ModelInfo 管理面板
// 模型行 / 内置定价表），在 sub2api 侧分别由既有 OpenAI 网关、/v1/models
// 和通用定价服务承担，故移除：
//   - TestLookupModelPricingVariantFallback

//   - TestNormalizePricingModelIDOnlyStripsClaudeReleaseDate（依赖未移植符号 normalizePricingModelID）

//   - TestEstimateRequestCostUSD_UsesVariantPricing（依赖未移植符号 EstimateRequestCostUSD）
//   - TestEstimateRequestCostUSD_UsesOriginalRatesWithoutCurrencyConversion（依赖未移植符号 EstimateRequestCostUSD）

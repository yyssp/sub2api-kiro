package service

import (
	"context"
	"time"
)

// LegacyLongContextRule preserves the historical marginal long-context billing
// API for callers that still pass an explicit platform rule. Normal Gemini
// requests use the unified catalog-driven pricing path instead.
type LegacyLongContextRule struct {
	Threshold  int
	Multiplier float64
}

const (
	geminiLegacyLongContextThreshold  = 200000
	geminiLegacyLongContextMultiplier = 2.0
)

// LegacyLongContextRule returns the compatibility rule for the Gemini
// platform. Other platforms have no legacy marginal rule.
func (s *BillingService) LegacyLongContextRule(platform string) *LegacyLongContextRule {
	if platform == PlatformGemini {
		return &LegacyLongContextRule{
			Threshold:  geminiLegacyLongContextThreshold,
			Multiplier: geminiLegacyLongContextMultiplier,
		}
	}
	return nil
}

// TokenCostRequest 通用网关 token 计费请求。
type TokenCostRequest struct {
	Ctx             context.Context
	Model           string
	Group           *Group
	Tokens          UsageTokens
	RateMultiplier  float64
	PricingAt       time.Time
	ServiceTier     string
	ReasoningEffort string
	Resolver        *ModelPricingResolver
	// Resolved 为调用方预先解析的定价（Resolver.Resolve 的结果），nil 表示未解析。
	Resolved *ResolvedPricing
	// LegacyLongContext is a compatibility-only marginal billing rule. It is
	// ignored when group or channel pricing is explicitly resolved.
	LegacyLongContext *LegacyLongContextRule
}

func legacyLongContextApplies(resolved *ResolvedPricing, group *Group, rule *LegacyLongContextRule) bool {
	if rule == nil || rule.Threshold <= 0 {
		return false
	}
	if resolved != nil && (resolved.Source == PricingSourceGroup || resolved.Source == PricingSourceChannel) {
		return false
	}
	return group == nil || group.LongContextPricingEnabled
}

// CalculateTokenCostForRequest 按通用网关的路径选择计算 token 费用：
//  1. 分组/渠道显式定价，或有解析器与分组 → 统一计费
//     （区间、分组卡、目录长上下文阶梯均在其中，阶梯由目录数据驱动）；
//  2. 否则按模型目录直接计费。
//
// 模型广场的阶梯表查询与网关使用同一入口，保证展示与扣费同源。
func (s *BillingService) CalculateTokenCostForRequest(req TokenCostRequest) (*CostBreakdown, error) {
	resolved := req.Resolved
	if resolved != nil && (resolved.Source == PricingSourceGroup || resolved.Source == PricingSourceChannel) {
		return s.CalculateCostUnified(s.tokenCostInput(req, resolved))
	}
	if legacyLongContextApplies(resolved, req.Group, req.LegacyLongContext) {
		return s.CalculateCostWithLongContext(req.Model, req.Tokens, req.RateMultiplier,
			req.LegacyLongContext.Threshold, req.LegacyLongContext.Multiplier)
	}
	if req.Resolver != nil && req.Group != nil {
		return s.CalculateCostUnified(s.tokenCostInput(req, resolved))
	}
	if req.ReasoningEffort != "" {
		return s.CalculateCostUnified(s.tokenCostInput(req, resolved))
	}
	return s.CalculateCost(req.Model, req.Tokens, req.RateMultiplier)
}

func (s *BillingService) tokenCostInput(req TokenCostRequest, resolved *ResolvedPricing) CostInput {
	input := CostInput{
		Ctx:             req.Ctx,
		Model:           req.Model,
		Group:           req.Group,
		Tokens:          req.Tokens,
		RequestCount:    1,
		RateMultiplier:  req.RateMultiplier,
		PricingAt:       req.PricingAt,
		ServiceTier:     req.ServiceTier,
		ReasoningEffort: req.ReasoningEffort,
		Resolver:        req.Resolver,
		Resolved:        resolved,
	}
	if req.Group != nil {
		gid := req.Group.ID
		input.GroupID = &gid
	}
	return input
}

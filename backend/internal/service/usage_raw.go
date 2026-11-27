package service

import "math"

// RawUsageSnapshot uses mutually exclusive input buckets, matching usage_logs.
// It stays private on usage objects and survives value copies across protocols.
type RawUsageSnapshot struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	Tokens              UsageTokens
	UpstreamTokens      UsageTokens
	EmptyResponse       bool
}

func (u *ClaudeUsage) captureRawUsage() {
	if u == nil || u.rawUsage != nil {
		return
	}
	u.rawUsage = &RawUsageSnapshot{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens, CacheCreationTokens: u.billingUsageTokens().CacheCreationTokens,
		Tokens:         u.billingUsageTokens(),
		UpstreamTokens: u.billingUsageTokens(),
	}
}

func (u *OpenAIUsage) captureRawUsage() {
	if u == nil || u.rawUsage != nil {
		return
	}
	tokens := u.billingUsageTokens()
	u.rawUsage = &RawUsageSnapshot{
		InputTokens: tokens.InputTokens, OutputTokens: tokens.OutputTokens,
		CacheReadTokens: tokens.CacheReadTokens, CacheCreationTokens: tokens.CacheCreationTokens,
		Tokens:         tokens,
		UpstreamTokens: tokens,
	}
}

func (raw *RawUsageSnapshot) claudeUsage() ClaudeUsage {
	t := raw.Tokens
	return ClaudeUsage{InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
		CacheReadInputTokens: t.CacheReadTokens, CacheCreationInputTokens: t.CacheCreationTokens,
		CacheCreation5mTokens: t.CacheCreation5mTokens, CacheCreation1hTokens: t.CacheCreation1hTokens,
		ImageOutputTokens: t.ImageOutputTokens}
}

func shapingCostValue(cost *CostBreakdown) *float64 {
	if cost == nil || cost.BillingMode != string(BillingModeToken) || math.IsNaN(cost.TotalCost) || math.IsInf(cost.TotalCost, 0) || cost.TotalCost < 0 {
		return nil
	}
	value := cost.TotalCost
	return &value
}

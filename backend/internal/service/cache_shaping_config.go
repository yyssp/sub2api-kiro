package service

import (
	"fmt"
	"math"
)

type CacheHitShapingPolicy struct {
	Enabled                bool    `json:"enabled"`
	TargetHitRatio         float64 `json:"target_hit_ratio"`
	RatioJitter            float64 `json:"ratio_jitter"`
	MinCacheReadTokens     int     `json:"min_cache_read_tokens"`
	MinCacheCreationTokens int     `json:"min_cache_creation_tokens"`
	MinCostRatio           float64 `json:"min_cost_ratio"`
	MaxReadMultiplier      float64 `json:"max_read_multiplier"`
	MinCreationRatio       float64 `json:"min_creation_ratio"`
	ReadAwareCostFloor     bool    `json:"read_aware_cost_floor"`
	CostFloorTopUpInput    bool    `json:"cost_floor_top_up_input"`
}

func DefaultCacheHitShapingPolicy() CacheHitShapingPolicy {
	return CacheHitShapingPolicy{TargetHitRatio: .9, RatioJitter: .04, MinCacheReadTokens: 1024, MinCacheCreationTokens: 1024, MinCostRatio: 1, CostFloorTopUpInput: true}
}

type StableSegmentCachePolicy struct {
	MinTotalInputTokens         int  `json:"min_total_input_tokens"`
	MinSegmentTokens            int  `json:"min_segment_tokens"`
	ReportedInputMinTokens      int  `json:"reported_input_min_tokens"`
	ReportedInputMaxTokens      int  `json:"reported_input_max_tokens"`
	HistoryMessageLimit         int  `json:"history_message_limit"`
	HonorExplicitCacheControl   bool `json:"honor_explicit_cache_control"`
	AutoCacheSystem             bool `json:"auto_cache_system"`
	AutoCacheTools              bool `json:"auto_cache_tools"`
	AutoCacheHistoryMessageEnds bool `json:"auto_cache_history_message_ends"`
}

func DefaultStableSegmentCachePolicy() StableSegmentCachePolicy {
	return StableSegmentCachePolicy{MinTotalInputTokens: 4096, MinSegmentTokens: 1024, ReportedInputMinTokens: 32, ReportedInputMaxTokens: 256, HistoryMessageLimit: 40, HonorExplicitCacheControl: true, AutoCacheSystem: true, AutoCacheTools: true, AutoCacheHistoryMessageEnds: true}
}

type CacheFieldShaping struct {
	Multiplier      float64 `json:"multiplier"`
	JitterRatio     float64 `json:"jitter_ratio"`
	MinTokens       int     `json:"min_tokens"`
	MaxTokens       int     `json:"max_tokens"`
	MaxJitterTokens int     `json:"max_jitter_tokens"`
}

type CacheShapingProfile struct {
	Input              CacheFieldShaping `json:"input"`
	Output             CacheFieldShaping `json:"output"`
	CacheRead          CacheFieldShaping `json:"cache_read"`
	CacheCreation      CacheFieldShaping `json:"cache_creation"`
	CapOverflowToInput bool              `json:"cap_overflow_to_input"`
	MinCostRatio       float64           `json:"min_cost_ratio"`
}

type CacheShapingOverride struct {
	Mode    string              `json:"mode"`
	Profile CacheShapingProfile `json:"profile"`
}

type CacheSmallRequestShaping struct {
	Enabled        bool                `json:"enabled"`
	MaxInputTokens int                 `json:"max_input_tokens"`
	DisableCache   bool                `json:"disable_cache"`
	Mode           string              `json:"mode"`
	Profile        CacheShapingProfile `json:"profile"`
}

type CacheToolPlusPolicy struct {
	Shaping                  CacheShapingProfile      `json:"shaping"`
	NonStream                CacheShapingOverride     `json:"non_stream"`
	SmallRequest             CacheSmallRequestShaping `json:"small_request"`
	SuppressedCreationTarget string                   `json:"suppressed_creation_target"`
}

func defaultCacheShapingProfile() CacheShapingProfile {
	f := CacheFieldShaping{Multiplier: 1}
	return CacheShapingProfile{Input: f, Output: f, CacheRead: f, CacheCreation: f}
}

func DefaultCacheToolPlusPolicy() CacheToolPlusPolicy {
	p := defaultCacheShapingProfile()
	return CacheToolPlusPolicy{Shaping: p, NonStream: CacheShapingOverride{Mode: "inherit", Profile: p}, SmallRequest: CacheSmallRequestShaping{MaxInputTokens: 4096, Mode: "inherit", Profile: p}, SuppressedCreationTarget: "input"}
}

func normalizeCacheKind(kind string) string {
	switch kind {
	case "no_cache":
		return CacheStrategyKindDisabled
	case "current_high_cache":
		return CacheStrategyKindPrefix
	case "prefix_tool", "kiro_rs_tool":
		return CacheStrategyKindToolAware
	case "kiro_rs_tool_plus":
		return CacheStrategyKindToolPlus
	}
	return kind
}

func validateCacheExtensions(c CacheStrategyConfig) error {
	if c.StateTTLSeconds > 300 {
		return fmt.Errorf("config.state_ttl_seconds must be between 0 and 300")
	}
	if c.StableSegment.ReportedInputMaxTokens > 0 && c.StableSegment.ReportedInputMinTokens > c.StableSegment.ReportedInputMaxTokens {
		return fmt.Errorf("config.stable_segment.reported_input_min_tokens must not exceed reported_input_max_tokens")
	}
	for name, v := range map[string]int{"state_ttl_seconds": c.StateTTLSeconds, "stable_segment.min_total_input_tokens": c.StableSegment.MinTotalInputTokens, "stable_segment.min_segment_tokens": c.StableSegment.MinSegmentTokens, "stable_segment.reported_input_min_tokens": c.StableSegment.ReportedInputMinTokens, "stable_segment.reported_input_max_tokens": c.StableSegment.ReportedInputMaxTokens, "stable_segment.history_message_limit": c.StableSegment.HistoryMessageLimit, "hit_shaping.min_cache_read_tokens": c.HitShaping.MinCacheReadTokens, "hit_shaping.min_cache_creation_tokens": c.HitShaping.MinCacheCreationTokens, "tool_plus.small_request.max_input_tokens": c.ToolPlus.SmallRequest.MaxInputTokens} {
		if v < 0 {
			return fmt.Errorf("config.%s cannot be negative", name)
		}
	}
	for _, r := range []struct {
		name   string
		v, max float64
	}{{"target_hit_ratio", c.HitShaping.TargetHitRatio, .99}, {"ratio_jitter", c.HitShaping.RatioJitter, .2}, {"min_cost_ratio", c.HitShaping.MinCostRatio, 5}, {"max_read_multiplier", c.HitShaping.MaxReadMultiplier, 50}, {"min_creation_ratio", c.HitShaping.MinCreationRatio, 5}} {
		if !validCacheFloat(r.v, r.max) {
			return fmt.Errorf("config.hit_shaping.%s must be between 0 and %g", r.name, r.max)
		}
	}
	for name, p := range map[string]CacheShapingProfile{"shaping": c.ToolPlus.Shaping, "non_stream": c.ToolPlus.NonStream.Profile, "small_request": c.ToolPlus.SmallRequest.Profile} {
		if !validCacheFloat(p.MinCostRatio, 100) {
			return fmt.Errorf("config.tool_plus.%s.min_cost_ratio must be between 0 and 100", name)
		}
		for field, f := range map[string]CacheFieldShaping{"input": p.Input, "output": p.Output, "cache_read": p.CacheRead, "cache_creation": p.CacheCreation} {
			if !validCacheFloat(f.Multiplier, 1000) || !validCacheFloat(f.JitterRatio, 1) || f.MinTokens < 0 || f.MaxTokens < 0 || f.MaxJitterTokens < 0 || (f.MaxTokens > 0 && f.MinTokens > f.MaxTokens) {
				return fmt.Errorf("config.tool_plus.%s.%s has invalid shaping bounds", name, field)
			}
		}
	}
	for _, mode := range []string{c.ToolPlus.NonStream.Mode, c.ToolPlus.SmallRequest.Mode} {
		if mode != "" && mode != "inherit" && mode != "custom" && mode != "raw" {
			return fmt.Errorf("config.tool_plus mode must be inherit, custom or raw")
		}
	}
	switch c.ToolPlus.SuppressedCreationTarget {
	case "", "input", "cache_read", "drop":
	default:
		return fmt.Errorf("config.tool_plus.suppressed_creation_target must be input, cache_read or drop")
	}
	return nil
}

func validCacheFloat(v, max float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= max
}

package routes

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 前端 kiroRsToolConfig() 产出的 JSON（字段值逐项抄自
// frontend/src/views/admin/cacheStrategyTemplates.ts）。
const kiroRsToolFrontendJSON = `{
  "kind": "tool_aware",
  "ratio_mode": "uniform",
  "coverage_ratio": 1,
  "usage_ratio": 1,
  "read_ratio": 1,
  "creation_ratio": 1,
  "cache_system": true,
  "cache_tools": true,
  "cache_history": true,
  "cache_tool_results": true,
  "cache_current_user_stable_prefix": false,
  "current_user_stable_prefix_max_tokens": 0,
  "breakpoint_mode": "hybrid",
  "allow_derived_session": true,
  "dynamic_content_mode": "exclude",
  "scope_mode": "group_session",
  "max_coverage_tokens": 0,
  "max_new_creation_tokens_per_request": 0,
  "incremental_create_enabled": true,
  "min_cacheable_tokens": 0,
  "reported_input_min_tokens": 0,
  "reported_input_max_tokens": 0,
  "uncached_input_min_tokens": 32,
  "uncached_input_max_tokens": 4096,
  "token_scale": 1,
  "scale_min_input_tokens": 0,
  "max_simulated_input_tokens": 0,
  "default_ttl_seconds": 300,
  "hour_ttl_seconds": 3600,
  "max_entries_per_scope": 200,
  "max_entries_global": 20000,
  "estimated_bytes_limit": 268435456,
  "expire_after_idle_seconds": 3600,
  "cap_jitter_min_tokens": 0,
  "cap_jitter_max_tokens": 0,
  "preserve_upstream_cache_usage": false,
  "usage": {
    "enabled": true,
    "preserve_upstream_cache_usage": false,
    "input": {"mode":"raw","max_tokens":0,"target_tokens":0,"normal_max_multiplier":1.1,"move_delta_to_cache_read":false},
    "output": {"mode":"raw","max_tokens":0,"target_tokens":0,"normal_max_multiplier":1.1,"move_delta_to_cache_read":false},
    "cache_read": {"mode":"raw","max_tokens":0,"target_tokens":0,"normal_max_multiplier":1.1,"move_delta_to_cache_read":false},
    "cache_creation": {"mode":"raw","max_tokens":0,"target_tokens":0,"normal_max_multiplier":1.1,"move_delta_to_cache_read":false},
    "skip_non_stream_usage_projection": false,
    "final_cache_read_max_tokens": 700000,
    "final_cache_read_jitter_min_tokens": 12345,
    "final_cache_read_jitter_max_tokens": 45312,
    "final_cache_creation_max_tokens": 400000,
    "final_cache_creation_jitter_min_tokens": 12345,
    "final_cache_creation_jitter_max_tokens": 45312,
    "output_uplift_enabled": false,
    "output_uplift_min_tokens": 0,
    "output_uplift_percent": 0,
    "final_output_guard_enabled": true,
    "final_output_max_tokens": 200000,
    "final_output_jitter_min_tokens": 12345,
    "final_output_jitter_max_tokens": 45312
  },
  "creation_control": {
    "enabled": false,
    "min_creation_delta_tokens": 0,
    "min_successful_requests_between": 0,
    "min_creation_interval_seconds": 0,
    "max_creation_tokens_per_event": 0,
    "creation_budget_window_seconds": 0,
    "max_creation_tokens_per_window": 0
  }
}`

// TestKiroRsToolFrontendTemplateMatchesBackend 钉死前端内置模板与后端已验证参数的一致性。
//
// 前端模板要经过「JSON 序列化 → 后端反序列化 → NormalizeCacheStrategyConfig」
// 才真正生效，归一化会改写值（比如缺省的 final_* 护栏、multiplier 兜底）。
// 参数表面相同不等于整形后的 usage 形态相同，所以这里比的是**归一化之后**的
// 完整配置，而不是逐字段抄写。
//
// 上面的 JSON 必须与 frontend/src/views/admin/cacheStrategyTemplates.ts 的
// kiroRsToolConfig() 保持同步；前端侧的取值由 cacheStrategyTemplates.spec.ts 钉死。
func TestKiroRsToolFrontendTemplateMatchesBackend(t *testing.T) {
	var fromJSON service.CacheStrategyConfig
	require.NoError(t, json.Unmarshal([]byte(kiroRsToolFrontendJSON), &fromJSON))
	normalizedJSON, err := service.NormalizeCacheStrategyConfig(fromJSON)
	require.NoError(t, err)

	normalizedGo, err := service.NormalizeCacheStrategyConfig(kiroRsToolTemplateConfig())
	require.NoError(t, err)

	a, _ := json.MarshalIndent(normalizedJSON, "", " ")
	b, _ := json.MarshalIndent(normalizedGo, "", " ")
	if string(a) != string(b) {
		t.Errorf("前端模板与后端已验证参数不一致（归一化后）\n--- frontend ---\n%s\n--- backend ---\n%s", a, b)
	}
}

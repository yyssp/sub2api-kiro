import { apiClient } from "../client";

export interface CacheStrategyConfig {
  kind: "disabled" | "prefix" | "tool_aware";
  ratio_mode: "uniform" | "independent";
  coverage_ratio: number;
  usage_ratio: number;
  read_ratio: number;
  creation_ratio: number;
  cache_system: boolean;
  cache_tools: boolean;
  cache_history: boolean;
  cache_tool_results: boolean;
  cache_current_user_stable_prefix: boolean;
  current_user_stable_prefix_max_tokens: number;
  breakpoint_mode: "client_only" | "auto" | "hybrid";
  allow_derived_session: boolean;
  dynamic_content_mode: "exclude" | "allow";
  scope_mode: "group_account_session" | "group_session";
  max_coverage_tokens: number;
  max_new_creation_tokens_per_request: number;
  incremental_create_enabled: boolean;
  min_cacheable_tokens: number;
  reported_input_min_tokens: number;
  reported_input_max_tokens: number;
  token_scale: number;
  scale_min_input_tokens: number;
  max_simulated_input_tokens: number;
  default_ttl_seconds: number;
  hour_ttl_seconds: number;
  max_entries_per_scope: number;
  max_entries_global: number;
  estimated_bytes_limit: number;
  expire_after_idle_seconds: number;
  cap_jitter_min_tokens: number;
  cap_jitter_max_tokens: number;
  preserve_upstream_cache_usage: boolean;
  usage: {
    enabled: boolean;
    preserve_upstream_cache_usage?: boolean;
    /** 非流式响应原样透传上游 usage，不套缓存整形；未配置时保持投影。 */
    skip_non_stream_usage_projection?: boolean;
    input: CacheUsageFieldPolicy;
    output: CacheUsageFieldPolicy;
    cache_read: CacheUsageFieldPolicy;
    cache_creation: CacheUsageFieldPolicy;
    /**
     * 每个 final_*_max_tokens 都配一对扣减区间：触顶时在
     * [jitter_min, jitter_max] 内按请求指纹回退一点，避免所有触顶记录
     * 都落在同一个数字上。上限为 0（不限制）时扣减区间会被后端清零。
     */
    final_cache_read_max_tokens: number;
    final_cache_read_jitter_min_tokens: number;
    final_cache_read_jitter_max_tokens: number;
    final_cache_creation_max_tokens: number;
    final_cache_creation_jitter_min_tokens: number;
    final_cache_creation_jitter_max_tokens: number;
    output_uplift_min_tokens: number;
    output_uplift_percent: number;
    /** 输出放大 + 输出最终上限的总开关；未配置（undefined）视为开启。 */
    final_output_guard_enabled?: boolean;
    final_output_max_tokens: number;
    final_output_jitter_min_tokens: number;
    final_output_jitter_max_tokens: number;
  };
  creation_control: {
    enabled: boolean;
    min_creation_delta_tokens: number;
    min_successful_requests_between: number;
    min_creation_interval_seconds: number;
    max_creation_tokens_per_event: number;
    creation_budget_window_seconds: number;
    max_creation_tokens_per_window: number;
  };
}

export type CacheUsageFieldMode =
  "raw" | "preserve" | "sample_max" | "sample_target";

export interface CacheUsageFieldPolicy {
  mode: CacheUsageFieldMode;
  max_tokens: number;
  target_tokens: number;
  normal_max_multiplier: number;
  move_delta_to_cache_read: boolean;
}

export interface CacheStrategy {
  id: number;
  name: string;
  description: string;
  enabled: boolean;
  revision: number;
  config: CacheStrategyConfig;
  bound_group_count?: number;
  created_at: string;
  updated_at: string;
}

const cacheStrategiesAPI = {
  async list(
    search = "",
  ): Promise<Array<CacheStrategy & { bound_group_count: number }>> {
    const { data } = await apiClient.get("/admin/cache-strategies", {
      params: { search },
    });
    return data;
  },
  async create(
    payload: Pick<CacheStrategy, "name" | "description" | "enabled" | "config">,
  ): Promise<CacheStrategy> {
    const { data } = await apiClient.post("/admin/cache-strategies", payload);
    return data;
  },
  async update(
    id: number,
    payload: Partial<
      Pick<CacheStrategy, "name" | "description" | "enabled" | "config">
    > & { expected_revision?: number },
  ): Promise<CacheStrategy> {
    const { data } = await apiClient.put(
      `/admin/cache-strategies/${id}`,
      payload,
    );
    return data;
  },
  async duplicate(id: number, name?: string): Promise<CacheStrategy> {
    const { data } = await apiClient.post(
      `/admin/cache-strategies/${id}/duplicate`,
      name ? { name } : {},
    );
    return data;
  },
  async setEnabled(id: number, enabled: boolean): Promise<CacheStrategy> {
    const { data } = await apiClient.post(
      `/admin/cache-strategies/${id}/${enabled ? "enable" : "disable"}`,
    );
    return data;
  },
  async remove(id: number): Promise<void> {
    await apiClient.delete(`/admin/cache-strategies/${id}`);
  },
  async groups(id: number): Promise<
    Array<{
      id: number;
      name: string;
      platform: string;
      cache_strategy_id?: number | null;
    }>
  > {
    const { data } = await apiClient.get(
      `/admin/cache-strategies/${id}/groups`,
    );
    return data;
  },
  async bindGroups(id: number, group_ids: number[]): Promise<void> {
    await apiClient.put(`/admin/cache-strategies/${id}/groups`, { group_ids });
  },
  async replaceGroups(id: number, group_ids: number[]): Promise<void> {
    await apiClient.put(`/admin/cache-strategies/${id}/groups/replace`, {
      group_ids,
    });
  },
};

export default cacheStrategiesAPI;

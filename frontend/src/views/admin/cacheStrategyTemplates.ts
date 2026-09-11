import type {
  CacheStrategyConfig,
  CacheUsageFieldPolicy,
} from "@/api/admin/cacheStrategies";

// 缓存类型（config.kind）说明：
//
// - prefix「前缀缓存」：把请求开头那段稳定内容（system、tools 定义、已结束的
//   历史轮次）整体当作缓存前缀，按 token 位置切断点。只要前缀逐字节一致就能命中，
//   适合上下文逐轮追加的普通会话 —— 也是绝大多数场景该选的类型。
//
// - tool_aware「工具缓存」：在前缀缓存的基础上额外感知工具调用结构，把
//   tool_use / tool_result 这类频繁变动的块单独处理，避免一次工具调用就把
//   整个前缀顶掉。适合 Claude Code 这种每轮都夹带工具往返的客户端。
//
// - disabled「关闭」：不产生任何缓存证据，请求原样转发。
export type CacheStrategyTemplateId =
  "blank" | "high_cache" | "steady_growth" | "rapid_growth";

export interface CacheStrategyTemplate {
  id: Exclude<CacheStrategyTemplateId, "blank">;
  nameKey: string;
  descriptionKey: string;
  createConfig: () => CacheStrategyConfig;
}

function usageField(
  mode: CacheUsageFieldPolicy["mode"],
  overrides: Partial<CacheUsageFieldPolicy> = {},
): CacheUsageFieldPolicy {
  return {
    mode,
    max_tokens: 0,
    target_tokens: 0,
    normal_max_multiplier: 1.1,
    move_delta_to_cache_read: false,
    ...overrides,
  };
}

export function createDefaultCacheStrategyConfig(
  kind: CacheStrategyConfig["kind"] = "prefix",
): CacheStrategyConfig {
  const config: CacheStrategyConfig = {
    kind,
    ratio_mode: "uniform",
    coverage_ratio: 0.85,
    usage_ratio: 1,
    read_ratio: 1,
    creation_ratio: 1,
    cache_system: true,
    cache_tools: true,
    cache_history: true,
    cache_tool_results: true,
    cache_current_user_stable_prefix: false,
    current_user_stable_prefix_max_tokens: 0,
    breakpoint_mode: "hybrid",
    allow_derived_session: false,
    dynamic_content_mode: "exclude",
    // 默认「分组 + 会话」：缓存按策略与会话隔离，同一会话换账号仍可命中。
    // 带上账号会让每次账号切换都重新建缓存，前缀白白重写一遍。
    scope_mode: "group_session",
    max_coverage_tokens: 0,
    max_new_creation_tokens_per_request: 0,
    incremental_create_enabled: true,
    min_cacheable_tokens: 1024,
    reported_input_min_tokens: 0,
    reported_input_max_tokens: 0,
    token_scale: 1,
    scale_min_input_tokens: 0,
    max_simulated_input_tokens: 0,
    default_ttl_seconds: 300,
    hour_ttl_seconds: 3600,
    // 容量与生命周期对齐 kiro.rs 页面默认值：单作用域 200 条、全局 20000 条、
    // 估算字节上限 256MB、空闲 1 小时过期。
    max_entries_per_scope: 200,
    max_entries_global: 20000,
    estimated_bytes_limit: 268435456,
    expire_after_idle_seconds: 3600,
    cap_jitter_min_tokens: 0,
    cap_jitter_max_tokens: 0,
    preserve_upstream_cache_usage: true,
    usage: {
      enabled: true,
      preserve_upstream_cache_usage: true,
      input: usageField("raw"),
      output: usageField("raw"),
      cache_read: usageField("preserve"),
      cache_creation: usageField("preserve"),
      skip_non_stream_usage_projection: false,
      // 上限与扣减区间对齐 kiro.rs 的 pathPolicy()。此前全留 0（不限制），
      // 等于把参考实现的护栏整组丢掉。读取上限在参考实现里不配扣减区间，照搬。
      final_cache_read_max_tokens: 700000,
      final_cache_read_jitter_min_tokens: 0,
      final_cache_read_jitter_max_tokens: 0,
      final_cache_creation_max_tokens: 400000,
      final_cache_creation_jitter_min_tokens: 20000,
      final_cache_creation_jitter_max_tokens: 45000,
      output_uplift_enabled: true,
      output_uplift_min_tokens: 1000,
      output_uplift_percent: 50,
      final_output_guard_enabled: true,
      final_output_max_tokens: 200000,
      final_output_jitter_min_tokens: 5000,
      final_output_jitter_max_tokens: 12000,
    },
    creation_control: {
      enabled: false,
      min_creation_delta_tokens: 0,
      min_successful_requests_between: 0,
      min_creation_interval_seconds: 0,
      max_creation_tokens_per_event: 0,
      creation_budget_window_seconds: 0,
      max_creation_tokens_per_window: 0,
    },
  };

  if (kind === "disabled") {
    config.cache_system = false;
    config.cache_tools = false;
    config.cache_history = false;
    config.cache_tool_results = false;
    config.coverage_ratio = 0;
    config.usage_ratio = 0;
    config.read_ratio = 0;
    config.creation_ratio = 0;
    config.incremental_create_enabled = false;
    config.usage.enabled = false;
  }

  return config;
}

function highCacheConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.coverage_ratio = 0.98;
  config.usage_ratio = 0.98;
  config.read_ratio = 0.98;
  config.creation_ratio = 0.98;
  config.token_scale = 1.6;
  config.scale_min_input_tokens = 20000;
  config.max_coverage_tokens = 300000;
  config.max_new_creation_tokens_per_request = 30000;
  config.max_simulated_input_tokens = 300000;
  config.cap_jitter_min_tokens = 12000;
  config.cap_jitter_max_tokens = 24000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  // 三组上限都配上扣减区间，触顶记录才不会全是同一个数字。
  config.usage.final_cache_read_max_tokens = 700000;
  config.usage.final_cache_read_jitter_min_tokens = 20000;
  config.usage.final_cache_read_jitter_max_tokens = 45000;
  config.usage.final_cache_creation_max_tokens = 400000;
  config.usage.final_cache_creation_jitter_min_tokens = 20000;
  config.usage.final_cache_creation_jitter_max_tokens = 45000;
  config.usage.output_uplift_enabled = true;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 16384;
  config.usage.final_output_jitter_min_tokens = 512;
  config.usage.final_output_jitter_max_tokens = 1300;
  return config;
}

// 稳步增长：每轮写入恒定额度，缓存呈一条平稳上升的直线。
//
// 24 轮 mock 回归实测：每轮 create 稳定在 30000，cache_read 从 0 线性涨到
// 约 846K，中途没有停滞。关键在于创建控制的三个节流项全部让路 ——
// 最小间隔 60 秒是默认值，但真实会话相邻两轮只隔几秒，留着会让第 1 轮之后
// 每一轮都被压制；窗口预算同理，跑得比窗口重置还快时会在中途卡死。
// 节奏只由「单次上限」一项决定，这才是可预测的稳步增长。
function steadyGrowthConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.coverage_ratio = 0.9;
  config.allow_derived_session = true;
  config.creation_control.enabled = true;
  config.creation_control.min_creation_interval_seconds = 0;
  config.creation_control.min_creation_delta_tokens = 0;
  config.creation_control.min_successful_requests_between = 0;
  config.creation_control.max_creation_tokens_per_event = 30000;
  config.creation_control.creation_budget_window_seconds = 0;
  config.creation_control.max_creation_tokens_per_window = 0;
  config.usage.final_cache_read_max_tokens = 650000;
  config.usage.final_cache_read_jitter_min_tokens = 23456;
  config.usage.final_cache_read_jitter_max_tokens = 54321;
  config.usage.final_cache_creation_max_tokens = 300000;
  config.usage.final_cache_creation_jitter_min_tokens = 23456;
  config.usage.final_cache_creation_jitter_max_tokens = 54321;
  config.usage.output_uplift_enabled = true;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 64000;
  config.usage.final_output_jitter_min_tokens = 2000;
  config.usage.final_output_jitter_max_tokens = 5000;
  return config;
}

// 快速增长：几轮之内就冲到较大数值，且每轮增量不规整。
//
// 24 轮 mock 回归实测：第 1 轮 create 就有 56051，到第 24 轮 cache_read 约 921K，
// 单轮创建在 40K~56K 之间浮动 —— 因为额度由覆盖率推导的实际前缀长度决定，
// 而不是被单次上限削平，看上去就没有稳步增长那条直线那么规律。
// 这里给窗口预算留了一个足够大的值（200 万）作为兜底，防止异常流量无限写入。
function rapidGrowthConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.coverage_ratio = 0.98;
  config.allow_derived_session = true;
  config.creation_control.enabled = true;
  config.creation_control.min_creation_interval_seconds = 0;
  config.creation_control.min_creation_delta_tokens = 0;
  config.creation_control.min_successful_requests_between = 0;
  config.creation_control.max_creation_tokens_per_event = 120000;
  config.creation_control.creation_budget_window_seconds = 300;
  config.creation_control.max_creation_tokens_per_window = 2000000;
  config.usage.final_cache_read_max_tokens = 650000;
  config.usage.final_cache_read_jitter_min_tokens = 23456;
  config.usage.final_cache_read_jitter_max_tokens = 54321;
  config.usage.final_cache_creation_max_tokens = 300000;
  config.usage.final_cache_creation_jitter_min_tokens = 23456;
  config.usage.final_cache_creation_jitter_max_tokens = 54321;
  config.usage.output_uplift_enabled = true;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 64000;
  config.usage.final_output_jitter_min_tokens = 2000;
  config.usage.final_output_jitter_max_tokens = 5000;
  return config;
}

export const cacheStrategyTemplates: CacheStrategyTemplate[] = [
  {
    id: "high_cache",
    nameKey: "admin.cacheStrategies.templates.highCache.name",
    descriptionKey: "admin.cacheStrategies.templates.highCache.description",
    createConfig: highCacheConfig,
  },
  {
    id: "steady_growth",
    nameKey: "admin.cacheStrategies.templates.steadyGrowth.name",
    descriptionKey: "admin.cacheStrategies.templates.steadyGrowth.description",
    createConfig: steadyGrowthConfig,
  },
  {
    id: "rapid_growth",
    nameKey: "admin.cacheStrategies.templates.rapidGrowth.name",
    descriptionKey: "admin.cacheStrategies.templates.rapidGrowth.description",
    createConfig: rapidGrowthConfig,
  },
];

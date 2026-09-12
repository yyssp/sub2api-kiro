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
  | "blank"
  | "high_cache"
  | "steady_growth"
  | "rapid_growth"
  | "large_window"
  | "large_read_controlled_write"
  | "large_read_small_write"
  | "large_write_controlled_read";

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
    // 模拟与触顶参数对齐参考实现的通用默认值：token_scale 2.0、
    // 起算阈值 20000、模拟上限 30 万、触顶抖动 12000~24000。
    // 这几项此前留 0，等于把「本地模拟」整组关掉，空白策略建出来不具备
    // 参考实现的默认形态。
    token_scale: 2,
    scale_min_input_tokens: 20000,
    max_simulated_input_tokens: 300000,
    default_ttl_seconds: 300,
    hour_ttl_seconds: 3600,
    // 容量与生命周期采用通用参考实现的页面默认值：单作用域 200 条、全局 20000 条、
    // 估算字节上限 256MB、空闲 1 小时过期。
    max_entries_per_scope: 200,
    max_entries_global: 20000,
    estimated_bytes_limit: 268435456,
    expire_after_idle_seconds: 3600,
    cap_jitter_min_tokens: 12000,
    cap_jitter_max_tokens: 24000,
    preserve_upstream_cache_usage: true,
    usage: {
      enabled: true,
      preserve_upstream_cache_usage: true,
      input: usageField("raw"),
      output: usageField("raw"),
      cache_read: usageField("preserve"),
      cache_creation: usageField("preserve"),
      skip_non_stream_usage_projection: false,
      // 上限与扣减区间对齐参考实现的通用 path policy。此前全留 0（不限制），
      // 等于把参考实现的护栏整组丢掉。读取上限在参考实现里不配扣减区间，照搬。
      final_cache_read_max_tokens: 700000,
      final_cache_read_jitter_min_tokens: 12345,
      final_cache_read_jitter_max_tokens: 45312,
      final_cache_creation_max_tokens: 400000,
      final_cache_creation_jitter_min_tokens: 12345,
      final_cache_creation_jitter_max_tokens: 45312,
      output_uplift_enabled: true,
      output_uplift_min_tokens: 1000,
      output_uplift_percent: 50,
      final_output_guard_enabled: true,
      final_output_max_tokens: 200000,
      final_output_jitter_min_tokens: 12345,
      final_output_jitter_max_tokens: 45312,
    },
    // 创建控制对齐参考实现的 PromptCacheCreationControlConfig：
    // （5 分钟窗口 60 万、单次 10 万、增量下限 1.2 万、最小间隔 6 秒、
    // 最少间隔 2 次成功请求）。这组值避免快速会话被 60 秒/30k 默认值
    // 压成低频、固定的缓存写入。
    creation_control: {
      enabled: true,
      min_creation_delta_tokens: 12000,
      min_successful_requests_between: 2,
      min_creation_interval_seconds: 6,
      max_creation_tokens_per_event: 100000,
      creation_budget_window_seconds: 300,
      max_creation_tokens_per_window: 600000,
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
  config.token_scale = 2;
  config.scale_min_input_tokens = 20000;
  config.max_coverage_tokens = 300000;
  config.max_new_creation_tokens_per_request = 100000;
  config.max_simulated_input_tokens = 300000;
  config.cap_jitter_min_tokens = 12000;
  config.cap_jitter_max_tokens = 24000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  // 三组上限都配上波动区间（统一 12345~45312），触顶记录才不会全是同一个数字。
  // 输出上限 16384 小于波动上限，后端 normalize 会把区间夹成 12345~16384，
  // 仍是一段有效区间，不会塌缩成单值。
  config.usage.final_cache_read_max_tokens = 700000;
  config.usage.final_cache_read_jitter_min_tokens = 12345;
  config.usage.final_cache_read_jitter_max_tokens = 45312;
  config.usage.final_cache_creation_max_tokens = 400000;
  config.usage.final_cache_creation_jitter_min_tokens = 12345;
  config.usage.final_cache_creation_jitter_max_tokens = 45312;
  config.usage.output_uplift_enabled = true;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 16384;
  config.usage.final_output_jitter_min_tokens = 12345;
  config.usage.final_output_jitter_max_tokens = 45312;
  return config;
}

// 稳步增长：每轮写入恒定额度，缓存呈一条平稳上升的直线。
//
// 逐轮增长时保留完整的上报额度，不用 30k 的旧单次上限把每轮写入压平。
// 创建控制的三个节流项全部让路，节奏只由实际前缀增量和 100k 单次上限决定。
// 节奏只由「单次上限」一项决定，这才是可预测的稳步增长。
function steadyGrowthConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.coverage_ratio = 0.9;
  config.allow_derived_session = true;
  config.creation_control.enabled = true;
  config.creation_control.min_creation_interval_seconds = 0;
  config.creation_control.min_creation_delta_tokens = 0;
  config.creation_control.min_successful_requests_between = 0;
  config.creation_control.max_creation_tokens_per_event = 100000;
  config.creation_control.creation_budget_window_seconds = 0;
  config.creation_control.max_creation_tokens_per_window = 0;
  config.usage.final_cache_read_max_tokens = 650000;
  config.usage.final_cache_read_jitter_min_tokens = 12345;
  config.usage.final_cache_read_jitter_max_tokens = 45312;
  config.usage.final_cache_creation_max_tokens = 300000;
  config.usage.final_cache_creation_jitter_min_tokens = 12345;
  config.usage.final_cache_creation_jitter_max_tokens = 45312;
  config.usage.output_uplift_enabled = true;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 64000;
  config.usage.final_output_jitter_min_tokens = 12345;
  config.usage.final_output_jitter_max_tokens = 45312;
  return config;
}

// 快速增长：几轮之内就冲到较大数值，且每轮增量不规整。
// 单次上限使用大窗口场景同档的 120k；窗口预算保留 200 万作为压力测试兜底，
// 防止高吞吐测试被默认 600k 窗口过早截停。
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
  config.usage.final_cache_read_jitter_min_tokens = 12345;
  config.usage.final_cache_read_jitter_max_tokens = 45312;
  config.usage.final_cache_creation_max_tokens = 300000;
  config.usage.final_cache_creation_jitter_min_tokens = 12345;
  config.usage.final_cache_creation_jitter_max_tokens = 45312;
  config.usage.output_uplift_enabled = true;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 64000;
  config.usage.final_output_jitter_min_tokens = 12345;
  config.usage.final_output_jitter_max_tokens = 45312;
  return config;
}

// 大窗口压测：让 read/write 都有机会接近高上限。
//
// 该模板不是线上默认值。它把模拟输入上限、覆盖上限、单次 creation 上限和最终
// usage 上限一起抬高；cache_creation 使用 raw，避免 sample-target 在 150k 目标附近
// 反复采样，便于验证 700k read / 500k creation 的真实边界。
function largeWindowConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.coverage_ratio = 1;
  config.usage_ratio = 1;
  config.read_ratio = 1;
  config.creation_ratio = 1;
  config.allow_derived_session = true;
  // Keep the current user turn cacheable so the stress probe can create a
  // second large prefix and exercise both the read and creation caps.
  config.cache_current_user_stable_prefix = true;
  config.max_coverage_tokens = 3000000;
  // Leave candidate creation uncapped so the final usage cap below is the
  // boundary being tested, rather than an earlier state-transition limit.
  config.max_new_creation_tokens_per_request = 0;
  config.token_scale = 2;
  config.scale_min_input_tokens = 20000;
  config.max_simulated_input_tokens = 3000000;
  config.cap_jitter_min_tokens = 20000;
  config.cap_jitter_max_tokens = 50000;
  config.usage.input = usageField("raw");
  config.usage.output = usageField("raw");
  config.usage.cache_read = usageField("preserve");
  config.usage.cache_creation = usageField("raw");
  config.usage.final_cache_read_max_tokens = 700000;
  config.usage.final_cache_read_jitter_min_tokens = 10000;
  config.usage.final_cache_read_jitter_max_tokens = 30000;
  config.usage.final_cache_creation_max_tokens = 500000;
  config.usage.final_cache_creation_jitter_min_tokens = 10000;
  config.usage.final_cache_creation_jitter_max_tokens = 30000;
  config.creation_control.enabled = false;
  config.creation_control.min_creation_delta_tokens = 0;
  config.creation_control.min_successful_requests_between = 0;
  config.creation_control.min_creation_interval_seconds = 0;
  config.creation_control.max_creation_tokens_per_event = 0;
  config.creation_control.creation_budget_window_seconds = 0;
  config.creation_control.max_creation_tokens_per_window = 0;
  return config;
}

// 大读、可控写：读取允许进入 700k 档，写入则以 120k 为目标、180k 为最终上限，
// 并用创建控制把写入频率限制为每两次成功请求最多释放一次。
function largeReadControlledWriteConfig(): CacheStrategyConfig {
  const config = largeWindowConfig();
  config.usage.cache_creation = usageField("sample_target", {
    target_tokens: 120000,
    normal_max_multiplier: 1.25,
  });
  config.usage.final_cache_creation_max_tokens = 180000;
  config.usage.final_cache_creation_jitter_min_tokens = 8000;
  config.usage.final_cache_creation_jitter_max_tokens = 20000;
  config.max_new_creation_tokens_per_request = 180000;
  config.creation_control.enabled = true;
  config.creation_control.min_creation_delta_tokens = 20000;
  config.creation_control.min_successful_requests_between = 1;
  config.creation_control.min_creation_interval_seconds = 0;
  config.creation_control.max_creation_tokens_per_event = 180000;
  config.creation_control.creation_budget_window_seconds = 300;
  config.creation_control.max_creation_tokens_per_window = 600000;
  return config;
}

// 大读、小写：保留 700k 读取能力，但把每次可新增/上报的 creation 收敛到约
// 30k 目标和 60k 上限，适合需要缓存命中但不希望写入占满 usage 的分组。
function largeReadSmallWriteConfig(): CacheStrategyConfig {
  const config = largeWindowConfig();
  config.usage.cache_creation = usageField("sample_target", {
    target_tokens: 30000,
    normal_max_multiplier: 1.5,
  });
  config.usage.final_cache_creation_max_tokens = 60000;
  config.usage.final_cache_creation_jitter_min_tokens = 4000;
  config.usage.final_cache_creation_jitter_max_tokens = 12000;
  config.max_new_creation_tokens_per_request = 60000;
  config.creation_control.enabled = true;
  config.creation_control.min_creation_delta_tokens = 0;
  config.creation_control.min_successful_requests_between = 0;
  config.creation_control.min_creation_interval_seconds = 0;
  config.creation_control.max_creation_tokens_per_event = 60000;
  config.creation_control.creation_budget_window_seconds = 300;
  config.creation_control.max_creation_tokens_per_window = 300000;
  return config;
}

// 大写、可控读：保留 500k 写入能力，但把 cache_read 的最终上报值限制在 250k。
// 这里的 read 控制是 usage 上报控制，不会删除 tracker 中已经存在的缓存前缀。
function largeWriteControlledReadConfig(): CacheStrategyConfig {
  const config = largeWindowConfig();
  config.usage.cache_read = usageField("sample_max", {
    max_tokens: 250000,
  });
  config.usage.final_cache_read_max_tokens = 250000;
  config.usage.final_cache_read_jitter_min_tokens = 5000;
  config.usage.final_cache_read_jitter_max_tokens = 15000;
  config.max_new_creation_tokens_per_request = 500000;
  config.creation_control.enabled = true;
  config.creation_control.min_creation_delta_tokens = 0;
  config.creation_control.min_successful_requests_between = 0;
  config.creation_control.min_creation_interval_seconds = 0;
  config.creation_control.max_creation_tokens_per_event = 500000;
  config.creation_control.creation_budget_window_seconds = 300;
  config.creation_control.max_creation_tokens_per_window = 1000000;
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
  {
    id: "large_window",
    nameKey: "admin.cacheStrategies.templates.largeWindow.name",
    descriptionKey: "admin.cacheStrategies.templates.largeWindow.description",
    createConfig: largeWindowConfig,
  },
  {
    id: "large_read_controlled_write",
    nameKey: "admin.cacheStrategies.templates.largeReadControlledWrite.name",
    descriptionKey:
      "admin.cacheStrategies.templates.largeReadControlledWrite.description",
    createConfig: largeReadControlledWriteConfig,
  },
  {
    id: "large_read_small_write",
    nameKey: "admin.cacheStrategies.templates.largeReadSmallWrite.name",
    descriptionKey:
      "admin.cacheStrategies.templates.largeReadSmallWrite.description",
    createConfig: largeReadSmallWriteConfig,
  },
  {
    id: "large_write_controlled_read",
    nameKey: "admin.cacheStrategies.templates.largeWriteControlledRead.name",
    descriptionKey:
      "admin.cacheStrategies.templates.largeWriteControlledRead.description",
    createConfig: largeWriteControlledReadConfig,
  },
];

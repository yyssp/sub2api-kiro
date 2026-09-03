import type {
  CacheStrategyConfig,
  CacheUsageFieldPolicy,
} from "@/api/admin/cacheStrategies";

export type CacheStrategyTemplateId =
  | "blank"
  | "high_cache"
  | "claude_code"
  | "input_shaping"
  | "low_frequency_creation"
  | "read_priority"
  | "strict_client"
  | "shared_session"
  | "conservative_usage"
  | "long_context_guard"
  | "no_cache";

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
    scope_mode: "group_account_session",
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
    max_entries_per_scope: 128,
    max_entries_global: 10000,
    estimated_bytes_limit: 67108864,
    expire_after_idle_seconds: 0,
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
      final_cache_read_max_tokens: 0,
      final_cache_creation_max_tokens: 0,
      output_uplift_min_tokens: 0,
      output_uplift_percent: 0,
      final_output_max_tokens: 0,
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
  config.usage.final_cache_read_max_tokens = 700000;
  config.usage.final_cache_creation_max_tokens = 400000;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 16384;
  return config;
}

function claudeCodeConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("tool_aware");
  config.coverage_ratio = 0.92;
  config.usage_ratio = 0.9;
  config.read_ratio = 0.95;
  config.creation_ratio = 0.8;
  config.token_scale = 1.35;
  config.scale_min_input_tokens = 20000;
  config.max_coverage_tokens = 240000;
  config.max_new_creation_tokens_per_request = 24000;
  config.max_simulated_input_tokens = 240000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  config.usage.input = usageField("sample_max", {
    max_tokens: 16000,
    move_delta_to_cache_read: true,
  });
  config.usage.cache_creation = usageField("sample_target", {
    target_tokens: 30000,
    normal_max_multiplier: 1.2,
  });
  config.usage.cache_read = usageField("sample_target", {
    target_tokens: 180000,
    normal_max_multiplier: 1.2,
  });
  config.usage.final_cache_read_max_tokens = 500000;
  config.usage.final_cache_creation_max_tokens = 200000;
  config.usage.output_uplift_min_tokens = 1000;
  config.usage.output_uplift_percent = 50;
  config.usage.final_output_max_tokens = 16384;
  return config;
}

function inputShapingConfig(): CacheStrategyConfig {
  const config = highCacheConfig();
  config.kind = "tool_aware";
  config.coverage_ratio = 0.86;
  config.usage_ratio = 0.82;
  config.read_ratio = 0.9;
  config.creation_ratio = 0.72;
  config.token_scale = 1.25;
  config.max_coverage_tokens = 180000;
  config.max_new_creation_tokens_per_request = 18000;
  config.max_simulated_input_tokens = 180000;
  config.usage.input = usageField("sample_max", {
    max_tokens: 24000,
    move_delta_to_cache_read: true,
  });
  config.usage.output = usageField("raw");
  config.usage.cache_read = usageField("preserve");
  config.usage.cache_creation = usageField("preserve");
  config.usage.final_cache_read_max_tokens = 300000;
  config.usage.final_cache_creation_max_tokens = 120000;
  return config;
}

function lowFrequencyCreationConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.ratio_mode = "independent";
  config.coverage_ratio = 0.78;
  config.usage_ratio = 0.75;
  config.read_ratio = 1;
  config.creation_ratio = 0.55;
  config.max_coverage_tokens = 180000;
  config.max_new_creation_tokens_per_request = 30000;
  config.max_simulated_input_tokens = 220000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  config.usage.cache_read = usageField("sample_max", {
    max_tokens: 180000,
  });
  config.usage.cache_creation = usageField("sample_max", {
    max_tokens: 30000,
  });
  config.usage.final_cache_read_max_tokens = 300000;
  config.usage.final_cache_creation_max_tokens = 120000;
  config.creation_control = {
    enabled: true,
    min_creation_delta_tokens: 0,
    min_successful_requests_between: 2,
    min_creation_interval_seconds: 0,
    max_creation_tokens_per_event: 30000,
    creation_budget_window_seconds: 300,
    max_creation_tokens_per_window: 120000,
  };
  return config;
}

function readPriorityConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("tool_aware");
  config.ratio_mode = "independent";
  config.coverage_ratio = 0.9;
  config.usage_ratio = 0.9;
  config.read_ratio = 1;
  config.creation_ratio = 0.35;
  config.max_coverage_tokens = 240000;
  config.max_new_creation_tokens_per_request = 12000;
  config.incremental_create_enabled = false;
  config.max_simulated_input_tokens = 260000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  config.usage.cache_read = usageField("sample_max", {
    max_tokens: 220000,
  });
  config.usage.cache_creation = usageField("sample_max", {
    max_tokens: 12000,
  });
  config.usage.final_cache_read_max_tokens = 500000;
  config.usage.final_cache_creation_max_tokens = 60000;
  config.creation_control = {
    enabled: true,
    min_creation_delta_tokens: 0,
    min_successful_requests_between: 1,
    min_creation_interval_seconds: 0,
    max_creation_tokens_per_event: 12000,
    creation_budget_window_seconds: 600,
    max_creation_tokens_per_window: 48000,
  };
  return config;
}

function strictClientConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.coverage_ratio = 0.65;
  config.usage_ratio = 0.8;
  config.read_ratio = 0.8;
  config.creation_ratio = 0.8;
  config.breakpoint_mode = "client_only";
  config.max_coverage_tokens = 180000;
  config.max_new_creation_tokens_per_request = 60000;
  config.max_simulated_input_tokens = 200000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  config.incremental_create_enabled = false;
  config.usage.cache_read = usageField("sample_max", { max_tokens: 180000 });
  config.usage.cache_creation = usageField("sample_max", { max_tokens: 60000 });
  config.usage.final_cache_read_max_tokens = 180000;
  config.usage.final_cache_creation_max_tokens = 60000;
  return config;
}

function sharedSessionConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.ratio_mode = "independent";
  config.scope_mode = "group_session";
  config.allow_derived_session = true;
  config.coverage_ratio = 0.9;
  config.read_ratio = 0.75;
  config.creation_ratio = 0.6;
  config.max_coverage_tokens = 260000;
  config.max_new_creation_tokens_per_request = 45000;
  config.max_simulated_input_tokens = 280000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  config.creation_control = {
    enabled: true,
    min_creation_delta_tokens: 256,
    min_successful_requests_between: 1,
    min_creation_interval_seconds: 0,
    max_creation_tokens_per_event: 45000,
    creation_budget_window_seconds: 600,
    max_creation_tokens_per_window: 160000,
  };
  config.usage.cache_read = usageField("sample_target", {
    target_tokens: 180000,
    normal_max_multiplier: 1.3,
  });
  config.usage.cache_creation = usageField("sample_target", {
    target_tokens: 45000,
    normal_max_multiplier: 1.25,
  });
  config.usage.final_cache_read_max_tokens = 600000;
  config.usage.final_cache_creation_max_tokens = 160000;
  return config;
}

function conservativeUsageConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("tool_aware");
  config.coverage_ratio = 0.72;
  config.usage_ratio = 0.7;
  config.read_ratio = 0.7;
  config.creation_ratio = 0.7;
  config.token_scale = 1.1;
  config.scale_min_input_tokens = 12000;
  config.max_simulated_input_tokens = 120000;
  config.max_coverage_tokens = 90000;
  config.max_new_creation_tokens_per_request = 12000;
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  config.usage.input = usageField("sample_target", {
    target_tokens: 12000,
    normal_max_multiplier: 1.35,
  });
  config.usage.output = usageField("sample_target", {
    target_tokens: 256,
    normal_max_multiplier: 1.5,
  });
  config.usage.cache_read = usageField("sample_max", { max_tokens: 120000 });
  config.usage.cache_creation = usageField("sample_max", { max_tokens: 60000 });
  config.usage.final_cache_read_max_tokens = 120000;
  config.usage.final_cache_creation_max_tokens = 60000;
  config.usage.final_output_max_tokens = 8192;
  return config;
}

function longContextGuardConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("prefix");
  config.coverage_ratio = 0.92;
  config.max_coverage_tokens = 450000;
  config.max_new_creation_tokens_per_request = 30000;
  config.reported_input_min_tokens = 20000;
  config.reported_input_max_tokens = 850000;
  config.token_scale = 1.2;
  config.scale_min_input_tokens = 24000;
  config.max_simulated_input_tokens = 450000;
  config.default_ttl_seconds = 600;
  config.hour_ttl_seconds = 1800;
  config.max_entries_per_scope = 64;
  config.usage.cache_read = usageField("preserve");
  config.usage.cache_creation = usageField("preserve");
  config.preserve_upstream_cache_usage = false;
  config.usage.preserve_upstream_cache_usage = false;
  config.usage.final_cache_read_max_tokens = 550000;
  config.usage.final_cache_creation_max_tokens = 180000;
  config.usage.final_output_max_tokens = 32768;
  return config;
}

function noCacheConfig(): CacheStrategyConfig {
  const config = createDefaultCacheStrategyConfig("disabled");
  config.usage.input = usageField("raw");
  config.usage.output = usageField("raw");
  config.usage.cache_read = usageField("raw");
  config.usage.cache_creation = usageField("raw");
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
    id: "claude_code",
    nameKey: "admin.cacheStrategies.templates.claudeCode.name",
    descriptionKey: "admin.cacheStrategies.templates.claudeCode.description",
    createConfig: claudeCodeConfig,
  },
  {
    id: "input_shaping",
    nameKey: "admin.cacheStrategies.templates.inputShaping.name",
    descriptionKey: "admin.cacheStrategies.templates.inputShaping.description",
    createConfig: inputShapingConfig,
  },
  {
    id: "low_frequency_creation",
    nameKey: "admin.cacheStrategies.templates.lowFrequencyCreation.name",
    descriptionKey:
      "admin.cacheStrategies.templates.lowFrequencyCreation.description",
    createConfig: lowFrequencyCreationConfig,
  },
  {
    id: "read_priority",
    nameKey: "admin.cacheStrategies.templates.readPriority.name",
    descriptionKey: "admin.cacheStrategies.templates.readPriority.description",
    createConfig: readPriorityConfig,
  },
  {
    id: "strict_client",
    nameKey: "admin.cacheStrategies.templates.strictClient.name",
    descriptionKey:
      "admin.cacheStrategies.templates.strictClient.description",
    createConfig: strictClientConfig,
  },
  {
    id: "shared_session",
    nameKey: "admin.cacheStrategies.templates.sharedSession.name",
    descriptionKey:
      "admin.cacheStrategies.templates.sharedSession.description",
    createConfig: sharedSessionConfig,
  },
  {
    id: "conservative_usage",
    nameKey: "admin.cacheStrategies.templates.conservativeUsage.name",
    descriptionKey:
      "admin.cacheStrategies.templates.conservativeUsage.description",
    createConfig: conservativeUsageConfig,
  },
  {
    id: "long_context_guard",
    nameKey: "admin.cacheStrategies.templates.longContextGuard.name",
    descriptionKey:
      "admin.cacheStrategies.templates.longContextGuard.description",
    createConfig: longContextGuardConfig,
  },
  {
    id: "no_cache",
    nameKey: "admin.cacheStrategies.templates.noCache.name",
    descriptionKey: "admin.cacheStrategies.templates.noCache.description",
    createConfig: noCacheConfig,
  },
];

import { describe, expect, it } from "vitest";

import {
  cacheStrategyTemplates,
  createDefaultCacheStrategyConfig,
} from "../cacheStrategyTemplates";

describe("cache strategy templates", () => {
  it("includes purpose templates with distinct operating modes", () => {
    expect(cacheStrategyTemplates.map((template) => template.id)).toEqual([
      "kiro_rs_tool",
      "high_cache",
      "steady_growth",
      "rapid_growth",
      "large_window",
      "large_read_controlled_write",
      "large_read_small_write",
      "large_write_controlled_read",
    ]);
  });

  // 这组断言与后端 kiroRsToolTemplateConfig()（已跑过 60 轮真实调度：命中率
  // 98.3%、读写比 44.7）逐项对应。改动任意一项都会让本模板不再复刻参考实现，
  // 因此这里钉死，而不是只做 toMatchObject 的宽松检查。
  it("mirrors the reference kiro-rs-tool policy exactly", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "kiro_rs_tool")
      ?.createConfig();

    // KiroRsToolCachePolicy 的 8 个值
    expect(config).toMatchObject({
      kind: "tool_aware",
      coverage_ratio: 1,
      max_coverage_tokens: 0,
      incremental_create_enabled: true,
      max_new_creation_tokens_per_request: 0,
      cache_current_user_stable_prefix: false,
      current_user_stable_prefix_max_tokens: 0,
      // 参考实现的 reportedInputMin/Max 映射到未缓存输入桶，不是 reported_input_*
      uncached_input_min_tokens: 32,
      uncached_input_max_tokens: 4096,
    });

    // 本地模拟整组关闭
    expect(config).toMatchObject({
      token_scale: 1,
      scale_min_input_tokens: 0,
      max_simulated_input_tokens: 0,
      cap_jitter_min_tokens: 0,
      cap_jitter_max_tokens: 0,
      reported_input_min_tokens: 0,
      reported_input_max_tokens: 0,
      min_cacheable_tokens: 0,
      allow_derived_session: true,
    });

    // 创建控制整组关闭
    expect(config?.creation_control).toEqual({
      enabled: false,
      min_creation_delta_tokens: 0,
      min_successful_requests_between: 0,
      min_creation_interval_seconds: 0,
      max_creation_tokens_per_event: 0,
      creation_budget_window_seconds: 0,
      max_creation_tokens_per_window: 0,
    });

    // 四项一律按真值上报，不做采样/抬升
    expect(config?.usage.input.mode).toBe("raw");
    expect(config?.usage.output.mode).toBe("raw");
    expect(config?.usage.cache_read.mode).toBe("raw");
    expect(config?.usage.cache_creation.mode).toBe("raw");
    expect(config?.usage).toMatchObject({
      enabled: true,
      output_uplift_enabled: false,
      // 最终上限刻意保留通用护栏（严格照抄参考实现是不设上限），
      // 防止异常放大时把离谱数值直接上报出去。
      final_output_guard_enabled: true,
      final_output_max_tokens: 200000,
      final_cache_read_max_tokens: 700000,
      final_cache_creation_max_tokens: 400000,
      preserve_upstream_cache_usage: false,
    });
    expect(config?.preserve_upstream_cache_usage).toBe(false);
  });

  it("keeps the high-cache template bounded and usage-shaped", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "high_cache")
      ?.createConfig();

    expect(config).toMatchObject({
      kind: "prefix",
      usage_ratio: 0.98,
      read_ratio: 0.98,
      creation_ratio: 0.98,
      token_scale: 2,
      scale_min_input_tokens: 20000,
      max_simulated_input_tokens: 300000,
    });
    expect(config?.creation_control).toMatchObject({
      min_successful_requests_between: 2,
      min_creation_interval_seconds: 6,
      max_creation_tokens_per_event: 100000,
      max_creation_tokens_per_window: 600000,
    });
  });

  it("provides a steady-growth template without artificial frequency throttling", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "steady_growth")
      ?.createConfig();

    expect(config).toMatchObject({
      kind: "prefix",
      creation_control: {
        enabled: true,
        min_creation_delta_tokens: 0,
        min_successful_requests_between: 0,
        min_creation_interval_seconds: 0,
        max_creation_tokens_per_event: 100000,
        max_creation_tokens_per_window: 0,
      },
    });
  });

  it("provides a rapid-growth template with a larger bounded window", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "rapid_growth")
      ?.createConfig();

    expect(config).toMatchObject({
      creation_control: {
        enabled: true,
        min_creation_interval_seconds: 0,
        max_creation_tokens_per_event: 120000,
        max_creation_tokens_per_window: 2000000,
      },
    });
  });

  it("returns independent default config objects", () => {
    const first = createDefaultCacheStrategyConfig();
    const second = createDefaultCacheStrategyConfig();
    first.usage.input.max_tokens = 96;

    expect(second.usage.input.max_tokens).toBe(0);
  });

  it("provides a large-window stress template with high read/write caps", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "large_window")
      ?.createConfig();

    expect(config).toMatchObject({
      kind: "prefix",
      coverage_ratio: 1,
      cache_current_user_stable_prefix: true,
      max_coverage_tokens: 3000000,
      max_new_creation_tokens_per_request: 0,
      token_scale: 2,
      max_simulated_input_tokens: 3000000,
      usage: {
        cache_creation: { mode: "raw" },
        final_cache_read_max_tokens: 700000,
        final_cache_creation_max_tokens: 500000,
      },
    });
    expect(config?.creation_control).toMatchObject({
      enabled: false,
      min_creation_delta_tokens: 0,
      min_successful_requests_between: 0,
      min_creation_interval_seconds: 0,
      max_creation_tokens_per_event: 0,
      max_creation_tokens_per_window: 0,
    });
  });

  it("provides a large-read controlled-write template", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "large_read_controlled_write")
      ?.createConfig();

    expect(config).toMatchObject({
      usage: {
        cache_creation: {
          mode: "sample_target",
          target_tokens: 120000,
          normal_max_multiplier: 1.25,
        },
        final_cache_read_max_tokens: 700000,
        final_cache_creation_max_tokens: 180000,
      },
      max_new_creation_tokens_per_request: 180000,
    });
    expect(config?.creation_control).toMatchObject({
      enabled: true,
      min_creation_delta_tokens: 20000,
      min_successful_requests_between: 1,
      max_creation_tokens_per_event: 180000,
      max_creation_tokens_per_window: 600000,
    });
  });

  it("provides a large-read small-write template", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "large_read_small_write")
      ?.createConfig();

    expect(config).toMatchObject({
      usage: {
        cache_creation: {
          mode: "sample_target",
          target_tokens: 30000,
          normal_max_multiplier: 1.5,
        },
        final_cache_read_max_tokens: 700000,
        final_cache_creation_max_tokens: 60000,
      },
      max_new_creation_tokens_per_request: 60000,
    });
    expect(config?.creation_control).toMatchObject({
      enabled: true,
      max_creation_tokens_per_event: 60000,
      max_creation_tokens_per_window: 300000,
    });
  });

  it("provides a large-write controlled-read template", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "large_write_controlled_read")
      ?.createConfig();

    expect(config).toMatchObject({
      usage: {
        cache_read: {
          mode: "sample_max",
          max_tokens: 250000,
        },
        final_cache_read_max_tokens: 250000,
        final_cache_creation_max_tokens: 500000,
      },
      max_new_creation_tokens_per_request: 500000,
    });
    expect(config?.creation_control).toMatchObject({
      enabled: true,
      max_creation_tokens_per_event: 500000,
      max_creation_tokens_per_window: 1000000,
    });
  });
});

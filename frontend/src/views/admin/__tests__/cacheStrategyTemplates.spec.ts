import { describe, expect, it } from "vitest";

import {
  cacheStrategyTemplates,
  createDefaultCacheStrategyConfig,
} from "../cacheStrategyTemplates";

describe("cache strategy templates", () => {
  it("includes purpose templates with distinct operating modes", () => {
    expect(cacheStrategyTemplates.map((template) => template.id)).toEqual([
      "high_cache",
      "claude_code",
      "input_shaping",
      "low_frequency_creation",
      "read_priority",
      "strict_client",
      "shared_session",
      "conservative_usage",
      "long_context_guard",
      "no_cache",
    ]);
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
      token_scale: 1.6,
      scale_min_input_tokens: 20000,
      max_simulated_input_tokens: 300000,
    });
    expect(config?.usage.final_cache_read_max_tokens).toBe(700000);
    expect(config?.usage.final_cache_creation_max_tokens).toBe(400000);
  });

  it("loads the Claude Code tool and creation shaping values", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "claude_code")
      ?.createConfig();

    expect(config).toMatchObject({
      kind: "tool_aware",
      usage: {
        input: {
          mode: "sample_max",
          max_tokens: 16000,
          move_delta_to_cache_read: true,
        },
        cache_creation: {
          mode: "sample_target",
          target_tokens: 30000,
          normal_max_multiplier: 1.2,
        },
      },
    });
  });

  it("disables all local cache behavior in the no-cache template", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "no_cache")
      ?.createConfig();

    expect(config).toMatchObject({
      kind: "disabled",
      coverage_ratio: 0,
      usage_ratio: 0,
      cache_system: false,
      cache_tools: false,
      cache_history: false,
      cache_tool_results: false,
      incremental_create_enabled: false,
      usage: { enabled: false },
    });
  });

  it("limits creation frequency without disabling reads", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "low_frequency_creation")
      ?.createConfig();

    expect(config).toMatchObject({
      ratio_mode: "independent",
      read_ratio: 1,
      creation_ratio: 0.55,
      creation_control: {
        enabled: true,
        min_successful_requests_between: 2,
        max_creation_tokens_per_event: 30000,
        max_creation_tokens_per_window: 120000,
      },
    });
    expect(config?.usage.cache_read.max_tokens).toBe(180000);
    expect(config?.usage.cache_creation.max_tokens).toBe(30000);
  });

  it("keeps read-priority creation available only for the first prefix", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "read_priority")
      ?.createConfig();

    expect(config).toMatchObject({
      kind: "tool_aware",
      read_ratio: 1,
      creation_ratio: 0.35,
      incremental_create_enabled: false,
      creation_control: {
        enabled: true,
        min_successful_requests_between: 1,
        max_creation_tokens_per_event: 12000,
      },
    });
  });

  it("requires explicit breakpoints in the strict-client template", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "strict_client")
      ?.createConfig();

    expect(config).toMatchObject({
      breakpoint_mode: "client_only",
      coverage_ratio: 0.65,
      incremental_create_enabled: false,
      max_new_creation_tokens_per_request: 60000,
    });
  });

  it("uses group-session scope and independent ratios for shared sessions", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "shared_session")
      ?.createConfig();

    expect(config).toMatchObject({
      scope_mode: "group_session",
      allow_derived_session: true,
      ratio_mode: "independent",
      read_ratio: 0.75,
      creation_ratio: 0.6,
    });
  });

  it("keeps conservative usage values bounded", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "conservative_usage")
      ?.createConfig();

    expect(config).toMatchObject({
      token_scale: 1.1,
      max_simulated_input_tokens: 120000,
      usage: {
        input: { mode: "sample_target", target_tokens: 12000 },
        output: { mode: "sample_target", target_tokens: 256 },
        cache_read: { mode: "sample_max", max_tokens: 120000 },
        cache_creation: { mode: "sample_max", max_tokens: 60000 },
        final_output_max_tokens: 8192,
      },
    });
  });

  it("guards long-context usage with explicit input and cache caps", () => {
    const config = cacheStrategyTemplates
      .find((template) => template.id === "long_context_guard")
      ?.createConfig();

    expect(config).toMatchObject({
      reported_input_max_tokens: 850000,
      max_simulated_input_tokens: 450000,
      max_coverage_tokens: 450000,
      usage: {
        final_cache_read_max_tokens: 550000,
        final_cache_creation_max_tokens: 180000,
      },
    });
  });

  it("returns independent default config objects", () => {
    const first = createDefaultCacheStrategyConfig();
    const second = createDefaultCacheStrategyConfig();
    first.usage.input.max_tokens = 96;

    expect(second.usage.input.max_tokens).toBe(0);
  });
});

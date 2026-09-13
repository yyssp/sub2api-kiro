import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { PLATFORM_QUOTA_PLATFORMS, normalizePlatformQuotasMap } from "@/api/admin/settings";

// ⚠️ 这里钉的是「默认限额表格的渲染行必须覆盖全部平台」。
//
// 缺陷形态：normalizePlatformQuotasMap 早已按 11 个平台填好 form 数据，
// 但两处表格的 v-for 是手写的 6 元组字面量（漏掉 cursor/kimi/zhipu/
// deepseek/minimax）。后果不是报错而是**静默漏配**：
//   管理员在设置页看不到 cursor 那一行 → 永远设不上限额
//   → 后端 DailyLimitUSD 为 nil → nil 即无限额 → 该平台无限花钱。
//
// 类型系统挡不住：PlatformQuotaPlatform union 有全部 11 个，
// 模板里的 `as const` 数组比它自己的类型更窄，TS 视为合法子集。
//
// 因此断言绑定到同一个数组来源，而不是「列表里有 cursor」——
// 后者下次新增平台会重演同样的漏配。

const here = dirname(fileURLToPath(import.meta.url));
const settingsViewSource = readFileSync(resolve(here, "../SettingsView.vue"), "utf-8");

describe("SettingsView 默认限额表格的平台行", () => {
  it("不得用手写平台字面量渲染限额行", () => {
    // 手写列表一旦出现，就与 normalizePlatformQuotasMap 的数据源分叉了。
    const hardcoded = settingsViewSource.match(
      /v-for="p in \(\[\s*['"]anthropic['"]/g,
    );
    expect(
      hardcoded,
      "限额表格仍在用手写平台数组渲染：它会与 PLATFORM_QUOTA_PLATFORMS 分叉，新增平台时静默漏配额度",
    ).toBeNull();
  });

  it("两处限额表格都绑定到 PLATFORM_QUOTA_PLATFORMS", () => {
    const bound = settingsViewSource.match(/v-for="p in PLATFORM_QUOTA_PLATFORMS"/g);
    expect(
      bound?.length,
      "应有两处限额表格（全局默认 + 认证来源默认）绑定到统一平台清单",
    ).toBe(2);
  });

  it("统一清单覆盖全部 11 个平台且包含 cursor", () => {
    expect(PLATFORM_QUOTA_PLATFORMS).toContain("cursor");
    expect(PLATFORM_QUOTA_PLATFORMS).toHaveLength(11);
  });

  it("清单中每个平台都能在归一化后的表单数据里拿到非空档位", () => {
    // 模板用了 `form.default_platform_quotas[p]!.daily` 的非空断言，
    // 渲染列表与归一化列表必须逐项对齐，否则渲染期就是 undefined 解引用。
    const normalized = normalizePlatformQuotasMap();
    for (const platform of PLATFORM_QUOTA_PLATFORMS) {
      expect(
        normalized[platform],
        `${platform} 缺少归一化条目，模板的非空断言会在渲染时炸开`,
      ).toBeDefined();
    }
  });
});

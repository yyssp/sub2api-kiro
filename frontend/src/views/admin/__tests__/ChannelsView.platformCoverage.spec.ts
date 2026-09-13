import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { CONCRETE_PLATFORM_OPTIONS } from "@/constants/platforms";

// ⚠️ 这里钉的是「渠道页的平台清单必须覆盖全部具体平台」。
//
// 缺陷形态：platformOrder / compositePlatforms 是手写的 10 元组，独独漏了
// cursor。三个权威来源其实都已经认了 cursor：
//   - 后端 isConcreteRequestPlatform 显式含 PlatformCursor
//   - 迁移 239 重建的 composite_model_routes_target_platform_check 含 'cursor'
//   - 前端 CONCRETE_PLATFORM_OPTIONS 含 cursor
// 唯独渠道页这两行没跟上。后果是后端/数据库都接受 cursor 的 composite 路由，
// 管理界面却没有任何入口去建它——能力存在但不可达。
//
// 断言绑定到共享目录而不是「列表里有 cursor」：后者下次新增平台会重演。

const here = dirname(fileURLToPath(import.meta.url));
const channelsViewSource = readFileSync(resolve(here, "../ChannelsView.vue"), "utf-8");

const CONCRETE_PLATFORMS = CONCRETE_PLATFORM_OPTIONS.map((o) => o.value);

/** 取出形如 `const name: GroupPlatform[] = [...]` 的字面量成员 */
function readPlatformList(name: string): string[] {
  const match = channelsViewSource.match(
    new RegExp(`const ${name}: GroupPlatform\\[\\] = \\[([^\\]]*)\\]`),
  );
  if (!match) return [];
  return [...match[1].matchAll(/'([a-z]+)'/g)].map((m) => m[1]);
}

describe("ChannelsView 平台清单覆盖度", () => {
  it("platformOrder 覆盖全部具体平台", () => {
    const listed = readPlatformList("platformOrder");
    expect(listed.length, "未能解析出 platformOrder 字面量").toBeGreaterThan(0);
    for (const platform of CONCRETE_PLATFORMS) {
      expect(
        listed,
        `platformOrder 漏掉 ${platform}：该平台的分组在渠道页不会被渲染`,
      ).toContain(platform);
    }
  });

  it("compositePlatforms 覆盖全部具体平台", () => {
    const listed = readPlatformList("compositePlatforms");
    expect(listed.length, "未能解析出 compositePlatforms 字面量").toBeGreaterThan(0);
    for (const platform of CONCRETE_PLATFORMS) {
      expect(
        listed,
        `compositePlatforms 漏掉 ${platform}：后端与 DB 约束都接受该平台的 composite 路由，界面却建不出来`,
      ).toContain(platform);
    }
  });

  it("两个清单保持一致", () => {
    // 二者语义不同（渲染顺序 vs composite 可选平台），但当前应覆盖同一集合；
    // 若将来真要分叉，请显式改这条测试并写明理由。
    expect(new Set(readPlatformList("platformOrder"))).toEqual(
      new Set(readPlatformList("compositePlatforms")),
    );
  });
});

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { listPlatformIds } from "@/constants/platformCatalog";

// ⚠️ 这里钉的是「渠道页的平台清单必须覆盖全部具体平台」。
//
// 缺陷形态：platformOrder / compositePlatforms 曾是手写的平台元组，漏掉了 cursor。
// 后果是后端/数据库都接受 cursor 的 composite 路由，管理界面却没有任何入口去建它——
// 能力存在但不可达。
//
// 现在两者都由平台清单（constants/platformCatalog，后端 domain/platforms.go 生成）
// 派生。断言绑定到派生方式而不是「列表里有 cursor」：退回手写元组会重演同样的漏配。

const here = dirname(fileURLToPath(import.meta.url));
const channelsViewSource = readFileSync(resolve(here, "../ChannelsView.vue"), "utf-8");

describe("ChannelsView 平台清单覆盖度", () => {
  it("platformOrder 由平台清单派生", () => {
    expect(channelsViewSource).toMatch(
      /const platformOrder = computed<GroupPlatform\[\]>\(\(\) => listPlatformIds\(\)\)/,
    );
  });

  it("compositePlatforms 与 platformOrder 同源", () => {
    expect(channelsViewSource).toMatch(/const compositePlatforms = platformOrder\b/);
  });

  it("不得退回手写平台元组", () => {
    expect(channelsViewSource).not.toMatch(
      /const (platformOrder|compositePlatforms): GroupPlatform\[\] = \[/,
    );
  });

  it("平台清单覆盖本 fork 的 kiro 与 cursor", () => {
    expect(listPlatformIds()).toContain("kiro");
    expect(listPlatformIds()).toContain("cursor");
  });
});

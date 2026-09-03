import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

const currentDir = dirname(fileURLToPath(import.meta.url));
const groupsViewSource = readFileSync(resolve(currentDir, "../GroupsView.vue"), "utf8");

describe("groups Kiro cache strategy placement", () => {
  it("does not expose cache emulation controls in the group form", () => {
    expect(groupsViewSource).not.toContain("KiroCacheRatioField");
    expect(groupsViewSource).not.toContain("kiro_cache_emulation");
    expect(groupsViewSource).not.toContain("setCreateKiroCacheMode");
    expect(groupsViewSource).not.toContain("setEditKiroCacheMode");
  });

  it("keeps Kiro routing controls in the group form", () => {
    expect(groupsViewSource).toContain("kiro_auto_sticky_enabled");
    expect(groupsViewSource).toContain("kiro_sticky_session_ttl_seconds");
    expect(groupsViewSource).toContain("kiro_endpoint_mode");
  });
});

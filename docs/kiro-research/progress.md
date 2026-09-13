# 改造进度

> 逐项记录 [reform-plan.md](reform-plan.md) 的实施状态。
> 每项都要求：**代码改动 + 测试 + 测试反证（去掉修复后测试必须失败）**。

**最后更新**：2026-09-14

---

## 总览

| 编号 | 缺口 | 状态 | 测试 | 反证 | 备注 |
|---|---|---|---|---|---|
| **G8** | 归类器补齐 400 特征串 | ✅ 完成 | 9 例 | ✅ 已验证 | 2 行改动 |
| **G6** | 工具 schema 白名单重建 | ✅ 完成 | 6 例 | ✅ 已验证 | 本轮最高频缺口 |
| **G5** | 工具名字符集清洗 | ✅ 完成 | 10 例 | ✅ 已验证（6/10 失败） | 官方权威最强 |
| **G2** | 请求体积守卫 | ✅ 完成 | 8 例 | ⚠️ 见下 | **端到端本轮无法验证** |
| **G4** | cache token 口径 | ⛔ 阻塞 | — | — | 证据冲突，需实测定夺 |

**测试基线**：`./internal/pkg/kiro/` 与 `./internal/service/` 全绿，`go build ./...` + `go vet` 通过。

---

## G8 · 归类器补齐 400 特征串 ✅

**改动**：`backend/internal/service/kiro_error_classifier.go`
`looksLikeKiroBadRequestSchemaError` 增加 3 条子串匹配：
`improperly formed request` / `invalid tool use format` / `request_body_invalid`。

**根因**：同一个 schema 校验失败，两个端点返回**两条不同错误串** ——
`q.*` 返回 `"Improperly formed request."`，`codewhisperer.*`/`krs` 返回
`{"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"}`。
我们原先只覆盖前者，后者落进 `bad_request_unknown` 丢失诊断日志。

**测试**：`kiro_error_classifier_test.go`
- `TestClassifyKiroBadRequestDualErrorStrings`（5 例）
- `TestClassifyKiroBadRequestNewStringsDoNotOverreach`（4 例）——
  锁住新串**不抢**quota / 工具配对 / 模型类归类

**反证**：✅ 回退改动后测试失败，确认测试有牙齿。

---

## G6 · 工具 schema 白名单重建 ✅

**改动**：`backend/internal/pkg/kiro/translator.go`
`normalizeKiroJSONSchemaValue` 从「**补全器**」改为「**白名单过滤器**」。

关键点：
1. 白名单：`type` / `description` / `properties` / `required` / `items` / `enum` / `title`
2. `const: X` → `enum: [X]`（语义无损折叠），且**不覆盖已有 enum**
3. `required: []` 空数组**整键移除**（空数组本身是 400 触发器）
4. **不再主动添加** `additionalProperties`（原实现主动制造触发器）
5. **重建对象**而非删键 → 保证 `properties.*`、`items` 任意嵌套深度不残留

**测试**：`schema_whitelist_test.go`（6 个测试函数）
含实测黄金样本、深层嵌套、items 清洗、合法 schema 保真、`const` 不覆盖 `enum`。

**连带修改**：`translator_test.go` 的
`TestBuildKiroPayloadNormalizesToolJSONSchema` 断言了**旧契约**
（`required: []` 存在、`additionalProperties: true`），已反转这 3 条断言并注明原因。
测试的其余核心价值保留。

**反证**：✅ 已验证。

---

## G5 · 工具名字符集清洗 ✅

**改动**：`backend/internal/pkg/kiro/translator.go`
新增 `sanitizeToolNameCharset` / `toolNameHashSuffix` / `appendToolNameSuffix`，
重写 `mapKiroToolName`。

**依据**：🏆 `[官方]` AWS `user-service-2.json` 对 `ToolName` 的约束
`{ "max": 64, "pattern": "[a-zA-Z0-9_-]+" }`。我们取 63 留 1 位余量。

**两个关键设计决策**：

1. **先清洗字符集，再收长度** —— 顺序不能反。
   先截断再清洗，会让截断后残留的非法字符仍触发 400。

2. **哈希后缀一律取「原始名」** —— 清洗和截断都是**多对一**变换
   （`a.b` 和 `a b` 都 → `a_b`）。只有对原始名取哈希，两步合起来才仍是单射。
   ⚠️ 我的初版写成了对**已清洗**的名字取哈希，会破坏长名字的单射性，
   在写测试前自查发现并修正。

**连带修改**：`shortenToolNameIfNeeded` 不再被生产代码调用，
降级为「只收长度、不管字符集」的辅助函数（5 处测试调用点仍有效，
因为它们用的都是纯 ASCII 名，两条路径输出一致）。

**测试**：`tool_name_charset_test.go`（10 例）
覆盖 `$` 前缀、连字符保留、合法 MCP 名原样穿过、字符集碰撞的单射性、
长+脏名同时满足两个约束、CJK 名、跨调用点确定性、`web_search` 别名、空名、
以及**端到端断言 payload 中所有工具名合法**。

**反证**：✅ 回退字符集清洗后 **6/10 失败**。

> 📌 **注意一处已修正的错误判断**：我此前认为「MCP 连字符导致 400」，
> 已被 4 个独立来源证伪。若按原计划实现（连字符→`_`），会**无谓改写**
> 合法的 MCP 工具名。测试 `TestMapKiroToolNamePreservesHyphen` 现在
> 反向锁住了这一点。

---

## G2 · 请求体积守卫 ✅（但端到端未验证）

**改动**：新增 `backend/internal/pkg/kiro/payload_guard.go`，
在 `BuildKiroPayloadWithContext` 的 marshal 之后接入。

**三条不可妥协的规则**（均已被测试锁住）：
1. 切点必须是**不带 toolResults 的 User 消息** ——
   从 tool 配对中间切开会制造孤儿，上游同样 400，等于用一种 400 换另一种 400
2. 找不到干净切点**宁可不裁**
3. 裁完仍超限**只标记不报错**（软失败）——
   阈值是社区经验值，不该比上游更严格地拒绝用户请求

**阈值**：默认 450KiB（三个社区实现里最保守的），
可用环境变量 `KIRO_MAX_PAYLOAD_BYTES` 覆盖。

每次裁剪后复用既有的 `validateToolPairing` + `removeOrphanedToolUses`
重新收敛配对，再 `alignKiroHistoryToUser` 保证以 User 开头。

**测试**：`payload_guard_test.go`（8 例）覆盖 A-21～A-26 全部用例。

**反证**：A-23（无干净切点时不裁）用探针确认**非空转** ——
实测负载 13707 字节 / 阈值 2000 / cutpoint=0，
即守卫确实是「主动选择不裁」而非「压根没超限」。

> ⚠️ **本轮结论必须标「未验证」**：账号上下文只有 200k，
> **很可能堆不到 450KiB**，「真的避免了 400」本轮**无法端到端证明**。
> A 层只证明了裁剪算法正确，未证明阈值正确。

---

## G4 · cache token 口径 ⛔ 阻塞

`translator.go` 目前**主动丢弃**上游返回的 cache token。
两派证据互斥（agent-vibes vs tau），需要真实上游 `metadataEvent` 抓包定夺。
→ 归入任务 #35 的 C-3 用例。

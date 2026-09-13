# Kiro 协议加固 · 改造方案

> 每项六要素齐全：**改造目的 / 解决问题 / 改造方案 / 具体参考 / 可信度 / 测试用例**。
> 「具体参考」引用多仓库时，**逐仓库贴出对应代码实现**。
>
> ⚠️ 本方案只列**已实测确证**的缺口。调研阶段的疑似项若核验后发现我们已实现，
> 一律移入 [F07](findings/F07-gap-status-verified.md) 的"已实现"表，**不进本方案**。

---

## 实施顺序与总览

| 序 | 编号 | 改造项 | 可信度 | 额度消耗 | 状态 |
|---|---|---|---|---|---|
| 1 | **G8** | 归类器补两条 400 特征串 | 🟢 **高**（实测） | 0 | ⏳ |
| 2 | **G6** | 工具 schema 白名单重建 | 🟢 **高**（实测+官方+社区×2） | 0 | ⏳ |
| 3 | **G5** | 工具名字符集清洗 | 🟢 **高**（官方权威） | 0 | ⏳ |
| 4 | **G2** | 请求体积守卫 | 🟡 **中**（社区×3 一致，阈值分歧） | 0（但难端到端验证） | ⏳ |
| 5 | **G4** | cache token 口径 | 🔴 **低**（两派互斥） | **需实测** | ⏳ 阻塞 |

**排序理由**：G8 改动最小且能立刻改善 G2/G6 的可观测性（先能看见，再改）；
G6 触发频率最高；G5 官方权威最强但触发较少；G2 本轮难验证；G4 被证据冲突阻塞。

**不列入本轮**：见文末「已评估但不采纳」。

---

# G8 · 归类器补齐 400 特征串

### 改造目的
让走 `codewhisperer.`/`krs` 端点的 schema 类 400 能被正确归类，
而不是落进 `bad_request_unknown` 丢失诊断能力。

### 解决问题
`[本仓库]` **实测**（探针跑真实归类器，跑完即删）：

```
Improperly formed request.                                     => bad_request_schema   ✅
{"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"} => bad_request_unknown ❌
{"reason":"REQUEST_BODY_INVALID"}                              => bad_request_unknown  ❌
```

同一个根因（schema 超纲）在两个端点返回**两条不同错误串**，我们只覆盖了一条。
后果：`logKiroBadRequestClassification` 的针对性诊断日志丢失，
排障时看到的是"unknown"，掩盖真实成因。

### 改造方案
`[本仓库]` `backend/internal/service/kiro_error_classifier.go:118-127`

```go
func looksLikeKiroBadRequestSchemaError(lower string) bool {
	if lower == "" { return false }
	return strings.Contains(lower, "schema") ||
		strings.Contains(lower, "inputschema") ||
		strings.Contains(lower, "improperly formed request") ||
		strings.Contains(lower, "invalid tool use format") ||   // ← 新增
		strings.Contains(lower, "request_body_invalid") ||      // ← 新增
		strings.Contains(lower, "additionalproperties") ||
		(strings.Contains(lower, "properties") && strings.Contains(lower, "required"))
}
```
**改动量：2 行。**

### 具体参考

**`funny-vibes/agent-vibes`**（TS，★359，2026-09-13）
`apps/protocol-bridge/src/llm/aws/translator.ts:879-887` —— 唯一记录了双错误串的实现：
> 后端用 Smithy 校验 schema，比 Anthropic 严格得多……返回
> `{"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"}`，
> **而在 `q.` 端点上返回的是 `"Improperly formed request."`**

**`2ue_kiro.rs`**（本地二开）
`src/anthropic/handlers.rs:5355-5369` —— 错误串匹配含 `CONTENT_LENGTH_EXCEEDS_THRESHOLD`
及 7 个小写子串，是更完整的串集合，可对照补充。

### 可信度：🟢 **高**
- 缺口本身：`[本仓库]` **实测确证**，非推断
- 修复正确性：纯增量加串，**不可能破坏现有归类**（只会让更多 body 命中 schema 类）
- 唯一风险：串过于宽泛导致误归类 → `request_body_invalid` 和
  `invalid tool use format` 都足够特异，风险可忽略

### 测试用例（A/B 层，零额度）
| ID | 层 | 用例 | 断言 |
|---|---|---|---|
| B-10a | A | `Invalid tool use format.` | → `bad_request_schema` |
| B-10b | A | `{"reason":"REQUEST_BODY_INVALID"}` | → `bad_request_schema` |
| B-10c | A | `Improperly formed request.` | → `bad_request_schema`（回归） |
| B-10d | A | `MONTHLY_REQUEST_COUNT` 类 body | → 仍为 quota 类（不被误抢） |
| B-10e | A | 无关 400 body | → 仍为 `bad_request_unknown` |

---

# G6 · 工具 schema 白名单重建

### 改造目的
阻止 Claude Code 的 MCP 工具 schema 里的 draft-2020-12 关键字透传到上游，
避免 Smithy 校验拒掉**整个请求**。

### 解决问题
`[本仓库]` **实测**：社区指认的 7 个触发器，我们**命中 7 个，无一被清理**。

输入典型 MCP schema → 本仓库 `normalizeKiroJSONSchema` 实际输出：
```json
{
  "$schema": "...",                                          ← 透传 ❌
  "properties": {
    "n": {"default":5,"exclusiveMinimum":0,"format":"int32"}, ← 三个全留 ❌
    "s": {"const":"fixed"}                                    ← 未折叠 ❌
  },
  "propertyNames": {...},                                     ← 透传 ❌
  "required": []                                              ← 空数组保留 ❌
}
```

根因：`translator.go:2036-2038` 是**补全器不是过滤器**——
```go
for key, value := range obj {          // 全键拷贝，无白名单
    normalized[key] = normalizeSchemaChild(key, value)
}
```
更糟的是它**主动制造**两个触发器：`normalizeSchemaRequired` 在 value 非数组时
**恒返回 `[]`**（`:2089-2101`），`additionalProperties` 缺失时设 `true`（`:2066`）。

**这是本轮触发频率最高的缺口**——Claude Code 的 MCP schema 普遍带这些键。

### 改造方案
改造 `normalizeKiroJSONSchemaValue`，**把"全键拷贝"换成"白名单拷贝"**，
递归结构可完整复用（改动面小）：

1. 允许键白名单：`type` / `description` / `properties` / `required` / `items` / `enum` / `title`
2. `const: X` → `enum: [X]`
3. `required` 为空数组时**移除整个键**（而非保留 `[]`）
4. `additionalProperties` **不再主动添加**（社区两个实现的白名单里都没有它）
5. 必须**重建对象**而非删键，保证任意嵌套深度（`properties.*`、`items`）不残留

⚠️ **已知代价**：`minimum`/`maximum`/`pattern` 等约束被删后，模型可能生成越界参数。
社区一致接受这个代价——**"参数可能越界" << "整个请求 400"**。

### 具体参考

**`funny-vibes/agent-vibes`**（TS，★359）—— 白名单主蓝本
`apps/protocol-bridge/src/llm/aws/translator.ts:898-906`
```
允许键：type / description / properties / required / items / enum / title
```
`:932-934` `const` 折叠为单元素 `enum`；**重建对象**而非删键以保证任意深度不残留。
`:879-887` 给出根因：后端用 **Smithy** 校验。

**`AbdoKnbGit/tau`**（TS，★315，2026-09-12）—— 补充空数组规则
`src/lanes/kiro/request.ts:272-275`：**`required: []` 空数组也会触发 400**
（注明参考 kiro-gateway）。

**`awsl-project/maxx`**（Go，★61）—— 反面教材
`internal/adapter/provider/bedrock/sanitizer.go:319-322` 有完整清洗器，
**但 `internal/adapter/provider/kiro/` 没有复用它** → 它自身的缺陷，说明"有清洗器"不等于"用上了"。

**`2ue_kiro.rs`**（本地二开）—— 进阶做法
`src/anthropic/tool_schema_keys.rs:41-59`：非法 property key 改写为 `key<sha256[..16]>`
并记录**逆映射**，模型返回 tool_use 后**递归还原原始键名，客户端无感**。
> 比单纯删键更优（保语义），但复杂度高得多。**本轮不实施**，记录为后续演进方向。

**`fawney19/Aether`**（Rust，★1466）—— 另一维度
`crates/aether-provider/transport/src/kiro/converter.rs:185-189, 562-575`：
历史引用了已下线工具时，自动补空 properties 的**占位工具声明**。
> 与 schema 清洗正交，其他实现都没做。**本轮不实施**，记录备查。

### 可信度：🟢 **高**
| 维度 | 依据 |
|---|---|
| 缺口存在 | `[本仓库]` **实测**，7/7 命中 |
| 根因正确 | `[官方]` AWS 用 Smithy（service-2 模型本身就是 Smithy 生态） |
| 修复方向 | `[源码]` **两个独立实现**（agent-vibes 白名单、tau 空数组）一致 |
| 残余不确定 | `[待验证]` `additionalProperties` 到底"必须移除"还是"设 true 可接受"——
两实现都选移除 → **取安全侧移除** |

### 测试用例（A 层，零额度）
| ID | 用例 | 断言 |
|---|---|---|
| A-12 | 7 个超纲键黄金样例（用 F06 的输入） | 全部被移除 |
| A-13 | `const:"x"` | → `enum:["x"]` |
| A-14 | `required:[]` / `required:["a"]` | 前者移除键，后者保留 |
| A-15 | 三层嵌套 `properties.a.properties.b.$schema` | 深处也被移除（验"重建"非"删键"） |
| A-17 | `items` 内的子 schema | 同样被清洗 |
| A-18 | 完全合法的 schema | **不被破坏**（回归保护） |
| A-19 | `additionalProperties` 不再被主动添加 | 输出中不含该键 |

---

# G5 · 工具名字符集清洗

### 改造目的
让工具名满足上游 `pattern: [a-zA-Z0-9_-]+`，避免非法字符导致整个请求 400。

### 解决问题
`[本仓库]` `internal/pkg/kiro/translator.go:2010-2026` `mapKiroToolName`
**只做长度截断（63 + sha256 后 8 位）和 `web_search` 重映射，无任何字符集清洗**。

真实故障样本（社区实测）：`$WEB_SEARCH`、`$MUTLI_1.N.1-Read` → 400。

### 改造方案
在 `mapKiroToolName` 内、截断**之前**加一步字符集清洗：
```
name = 把 [^a-zA-Z0-9_-] 替换为 '_'
```
然后走现有的长度截断逻辑（`shortenToolNameIfNeeded`，`:1992-2008`）。

**三个必须保证的性质**：
1. ✅ **保留连字符** —— 官方 pattern 允许，换成 `_` 是无谓改写
2. ✅ **单射性** —— 清洗会引入重名（`a.b` 与 `a-b` 都变 `a_b`）。
   现有 sha256 后缀机制已能防碰撞，但需**在清洗后检测碰撞**再决定是否加后缀
3. ✅ **tools 定义与 history 里的 toolUse 用同一函数** —— 否则名称对不上
4. `kiroMaxToolNameLen = 63` vs 官方 64：**偏保守，不改**

### 具体参考

**`aws/aws-toolkit-vscode`**（AWS 官方，★1997）—— 🏆 **权威来源**
`packages/core/src/codewhisperer/client/user-service-2.json`
```json
"ToolName": { "type":"string", "max":64, "min":0, "pattern":"[a-zA-Z0-9_-]+", "sensitive":true }
"ToolUseId": { "type":"string", "max":64, "min":0, "pattern":"[a-zA-Z0-9_-]+" }
```

**`AbdoKnbGit/tau`**（TS，★315）—— 实现范本 + 哈希防碰撞
`src/lanes/kiro/tool_names.ts:73-80`
```ts
export function sanitizeKiroToolName(name: string): string {
  let safe = name.replace(/[^A-Za-z0-9_-]/g, '_')      // 保留连字符
  if (safe.length > KIRO_TOOL_NAME_MAX_LENGTH) {
    const suffix = `_${djb2Hash(name)}`                 // 哈希防碰撞
    safe = safe.slice(0, KIRO_TOOL_NAME_MAX_LENGTH - suffix.length) + suffix
  }
  return safe || 'tool'
}
```
测试 `kiro.test.ts:674-678` **断言** `mcp__context7__resolve-library-id` 原样通过。

**`easayliu/kiro.rs`**（Rust）—— 🔥 **唯一带真实线上故障样本**
`src/anthropic/converter.rs:1380-1400`，提交 `bb6cdd8`（2026-06-22，
"fix(kiro): 净化工具名非法字符，修复 Invalid tool use format 400"）。
注释引 `^[a-zA-Z0-9_-]+$`；实测触发 400 的是 `$WEB_SEARCH`、`$MUTLI_1.N.1-Read`。
测试 `:2338-2372` 断言 `_MUTLI_1_N_1-Read`（**连字符保留**）。

**`funny-vibes/agent-vibes`**（TS，★359）—— 抓包佐证 + 反例
`translator.ts:862-863` 注释："抓包验证：Kiro 后端接受下划线工具名，不需要 camelCase 转换"。
⚠️ 但其 `:974-985` **只截长度不洗字符集**——含点号工具名会直接 400，**这是它的真实缺陷**。

**`mydisha/keirouter`**（Go）—— ❌ **已被推翻，勿抄**
`kiro.go:1069-1075` 注释称 `^[a-zA-Z][a-zA-Z0-9_]{0,63}$`（连字符非法、首字符须字母）。
与官方 pattern 冲突，**错误**。

**`Quorinex/Kiro-Go`** —— ❌ **过度防御，勿抄**
称须纯 camelCase。`git log -S` 溯源为单一提交 `0f8035d`，
`zsecducna`/`ngh1105` 的同款说法均同源 → **只算 1 条证据，且是错的**。

### 可信度：🟢 **高（官方权威）**
四路**独立**证据一致：
1. `[官方]` AWS service-2 模型 `pattern: [a-zA-Z0-9_-]+`
2. `[源码]` easayliu 真实线上故障修复
3. `[源码]` tau 测试断言连字符通过
4. `[源码]` agent-vibes 抓包注释

> 📌 **自我修正**：我此前推演的"MCP 名带连字符会让整账号 400"**是错的**。
> 连字符合法。核心结论（非法字符毁整个请求）不受影响，但**触发形态和修复方向都变了**——
> 若按原设想把连字符换成 `_`，会无谓改写大量合法 MCP 名，增加串名风险。

### 测试用例（A 层，零额度）
| ID | 用例 | 断言 |
|---|---|---|
| A-1 | `$WEB_SEARCH` | → `_WEB_SEARCH` |
| A-2 | `$MUTLI_1.N.1-Read` | → `_MUTLI_1_N_1-Read`（**连字符保留**） |
| A-16 | `mcp__context7__resolve-library-id` | **原样保留** |
| A-3 | `a.b` 与 `a-b` 同时存在 | **不得撞成同一名字**（单射性） |
| A-4 | 超 63 字符 | 截断+sha256 后缀，结果仍匹配 `^[a-zA-Z0-9_-]+$` |
| A-5 | tools 定义 vs history toolUse | 两侧名称**一致可对上** |
| A-6 | `ToolNameMap` 反向还原 | 响应侧还原为原始名，不串工具 |
| A-20 | 中文/空格工具名 | 被清洗且结果非空 |

---

# G2 · 请求体积守卫

### 改造目的
在请求体超过上游阈值前主动裁剪，避免整个请求被拒。

### 解决问题
`[本仓库]` `internal/pkg/kiro/translator.go:535-539`
```go
payloadBytes, err := json.Marshal(payload)
if err != nil { ... }
return &KiroBuildResult{Payload: payloadBytes, Context: requestCtx}, nil
```
**marshal 完直接返回，无长度检查、无裁剪。**

### 改造方案（分两期）

**本期（最小可用）**：
1. 序列化后测真实字节数，超阈值则按**轮次粒度**裁历史
2. 切点必须是**不带 tool_results 的 User 消息**；找不到干净切点**宁可不裁**
3. 每次裁剪后**重跑现有的 `validateToolPairing` + `removeOrphanedToolUses`**（`:475-476`，已有）
4. 裁剪后仍超限 → **只记日志放行**（软失败），由上游裁决
5. 阈值取**保守值 450KiB**（见下方阈值分歧）

**后续期（不在本轮）**：`OnTooLong` 懒模式、CURRENT_FIT 当前轮次降级。

### 具体参考

**`2ue_kiro.rs`**（本地二开）—— 🏆 **实施蓝本**
- 阈值：`src/model/config.rs:4255-4261` —— `450KiB` 上限 + `32KiB` 余量 → **实际目标约 418KiB**
- 五级阶梯：`src/anthropic/payload_guard.rs:418-666`
- 🔑 **轮次粒度切点**：`:4334-4391`，找下一个**不带 tool_results 的 User 消息**（`:4349-4362`）；
  找不到干净切点且当前有 tool_results 时**宁可 `break`（`:4366`）也不强切**
- 🔑 **每级裁剪后重跑 `repair_request`**（`:4445-4472`，7 个修复动作）
- `align_history_to_user`（`:4411-4420`）：丢弃开头 Assistant 消息保证 history 以 User 开头
- 成本优化：`:4400-4409` 用**估算**预测裁剪后大小（含逗号分隔符修正），避免每删一条全量序列化
- **软失败**：`:649` `still_oversized` 只标记放行

**`AbdoKnbGit/tau`**（TS，★315）—— 双阈值 + 成对裁剪
`src/lanes/kiro/request.ts:120-122`
```ts
const KIRO_DEFAULT_MAX_PAYLOAD_BYTES    = 600_000   // 硬上限
const KIRO_DEFAULT_TARGET_PAYLOAD_BYTES = 220_000   // 软目标
```
`payload_guards.ts:45-47` **成对删除**历史条目（`splice(i, 2)`）保 role 交替，
保护开头承载 system 的若干条，裁完对齐到 user 开头并修复孤儿 toolResult。

**`CassiopeiaCode/q2api`**（Python，★266）—— 另一条路线
`app.py:735, 1094`：**入口按 token 数预检**（`input_tokens > 150000`）直接拒，
用 tiktoken 本地计数，**不裁剪**。

⚠️ **不要混淆——以下是帧/缓冲上限，不是请求体上限**：
`maxx/event_stream_types.go:195` 16MB（单帧）、`Aether/state.rs:3` 16MB（单帧）、
`Aether/kiro_stream.rs:8` 1MB（thinking 缓冲）。

### 可信度：🟡 **中**

| 维度 | 评价 |
|---|---|
| 缺口存在 | 🟢 `[本仓库]` 实测确证（完全没有守卫） |
| 需要守卫 | 🟢 三个独立实现都做了（2ue / tau / q2api），方向一致 |
| **具体阈值** | 🔴 **分歧明显**：2ue 450KiB / tau 600KB(软 220KB) / 社区观测 ~615KB / q2api 150k token |
| 裁剪算法 | 🟢 2ue 与 tau 同向（保 tool 配对、对齐 user 开头） |

> **阈值取 450KiB 的理由**：三个数里最保守。裁多了只是浪费上下文，
> 裁少了会 400。**安全侧优先。** 且应做成**可配置**，便于实测后调整。

⚠️ **本轮验证受限（重要）**：账号上下文只有 **200k**，
**很可能堆不到 450KiB**，即便实现了也**无法端到端证明生效**。
→ A 层可验证裁剪算法正确性，但"真的避免了 400"**本轮无法证明** → 最终标 **「未验证」**。

### 测试用例
| ID | 层 | 用例 | 断言 |
|---|---|---|---|
| A-21 | A | 超阈值历史 | 被裁到阈值内 |
| A-22 | A | 切点选择 | **永不从 tool_use/tool_result 对中间切开** |
| A-23 | A | 无干净切点 | **宁可不裁**，不强切 |
| A-24 | A | 裁剪后 | history 以 User 开头，无孤儿 toolResult |
| A-25 | A | 裁剪后仍超限 | 软失败放行 + 记日志，**不报错** |
| A-26 | A | 未超阈值 | **完全不改动**（回归保护） |
| C-5 | C | 真实大请求 | ⚠️ **200k 上下文可能触发不了 → 预期标"未验证"** |

---

# G4 · cache token 口径（🔴 阻塞）

### 改造目的
不再丢弃上游真实下发的 cache 用量，改为如实上报。

### 解决问题
`[本仓库]` `internal/pkg/kiro/translator.go:4321-4333` 注释：
> "Kiro cache usage is reported only from local emulation.
> Ignore tokenUsage cache fields even if upstream includes them."

**我们主动丢弃了上游真值**，改用本地模拟。

### ⚠️ 为什么本项阻塞：两派口径互斥

| 实现 | 口径 | 位置 |
|---|---|---|
| `agent-vibes` | **三者并列相加**：`uncached + cacheRead + cacheWrite = input` | `event-stream.ts:409-430` |
| `tau` | **cache ⊂ input，需相减**：`input = rawInput - (cacheRead + cacheWrite)` | `loop.ts:894-907` |

```ts
// agent-vibes/event-stream.ts:409-430
const sum = (uncached ?? 0) + (cacheRead ?? 0) + (cacheWrite ?? 0)
if (sum > 0) { inputTokens = sum; continue }    // 注意：是 fallback
```

`tau` 还有结构性差异：它解析 `metricsEvent`/`meteringEvent`/`contextUsageEvent`
（`loop.ts:24,675,1229`），**不是 `metadataEvent`**，字段列表里**没有 `uncachedInputTokens`**。

**二者不可能同时正确。照抄任一方都有风险。**

### 附带确认：社区误解的传播链可追溯
`agent-vibes` **自身矛盾**：`cache-tracker.ts:4-8` 注释断言"Kiro/CodeWhisperer 不返回 cache 字段，
所以客户端哈希模拟"，并注明参考自 `Quorinex/Kiro-Go`（`:10`）——
**可它自己的 `event-stream.ts` 又在解析真值**。

> 这印证了既有判断：**"上游不返回用量"是老实现造成的误解**，
> 且能看到误解**通过注释和交叉引用传播**的完整链条。
> **我们 `translator.go:4321` 的注释很可能源自同一误解。**

### 改造方案（待实测后定）
1. **先做 C-3 实测**：真实抓取 `metadataEvent` 原始结构
2. 按实测结果择一口径
3. 解析取值建议参考 `agent-vibes/event-stream.ts:338-366` 的
   `collectUsageMaps`——**递归搜任意深度的 `usage`/`tokenUsage`/`token_usage` 键，不假定帧结构**
4. 兼容多种拼写（`ngh1105/Kiro-Go:proxy/kiro.go:577-578` 兼容 4 种）
5. 缺字段时的降级公式参考 `ykn1002/kiro.rs:metadata.rs:44-53`：
   `(total - output - cache_read - cache_write).max(0)`

### 可信度：🔴 **低（阻塞）**
- 缺口存在：🟢 `[本仓库]` 代码注释自认
- 上游确实下发真值：🟢 多方一致
- **具体口径**：🔴 **两派互斥，无法离线定夺**
- `[官方]` **反证**：`tokenUsage` **不在公开 SDK 中**
  （`aws-toolkit-vscode` 全仓 0 命中），是 Kiro 特有扩展 → **无权威可查，只能抓包**

### 测试用例
| ID | 层 | 用例 |
|---|---|---|
| B-8 | B | metadataEvent 四种拼写都能解析 |
| A-11 | A | 缺 `uncachedInputTokens` 时的降级公式 |
| **C-3** | **C** | 🔥 **实测口径** —— ⚠️ **必须大 token 量级**（小负载会让缓存结论完全失真） |

---

## 已评估但**不采纳**（附理由）

| 项 | 来源 | 不采纳理由 |
|---|---|---|
| **合成用量体系** | `2ue_kiro.rs` `cache.rs`/`prompt_cache_creation_control.rs` | ⚠️ output +50%、input 改标签成 cache、成本地板、真假双账，**默认开启**。若对外计费会把账单建在合成数字上。**性质敏感，需你先决策**，本轮不动 |
| `transcript_sanitizer` | `2ue_kiro.rs`（社区零实现） | 签名字符串是其 prompt 构造方式特有；**若我们是纯 passthrough 则整体不适用** |
| 工具名语义映射（`Bash→shell`） | `tau/tool_names.ts:22-40` | 为命中模型训练分布而非过校验。**收益无法离线验证**，且改变用户可见语义，风险>收益 |
| 历史工具占位符 | `Aether/converter.rs:185-189` | 正交问题，本轮无证据表明我们受影响 |
| schema key 逆映射 | `2ue/tool_schema_keys.rs:41-59` | 比删键更优（保语义）但复杂度高，**G6 简单版先落地** |
| `inference_attempt_budget` | `2ue`（评级最高） | 🔗 **有价值但超出"协议加固"范围**，属调度层改造。单独立项 |
| event-stream **CRC 校验** | `2ue/kiro/parser/frame.rs:110,126` | 我们已是**真实帧解析**（`translator.go:3556+` 读 prelude、大端长度、header TLV），仅缺 CRC 校验。**影响小**（静默损坏检测不出），优先级低 |
| 1h cache 定价 | `2ue/pricing.rs` | ⚠️ **该实现有 bug 勿抄**：`cache_creation_1h` 被忽略，实际 2x 被按 1.25x 低估 |

---

## 附：本方案修正的既往错误判断

| 初判 | 修正 | 推翻依据 |
|---|---|---|
| 工具名连字符非法，是 400 主因 | **连字符合法**；工具名是**次要**成因，schema 才是主因 | AWS 官方 + tau 测试 + agent-vibes 抓包 |
| G1「400 一律不故障转移」 | Kiro 路径有**5 个 400 子类**和专属转移分支；真问题是**归类漏判**（G8） | `kiro_error_classifier.go` 实测 |
| 归类器只匹配一条错误串 | 已覆盖 `improperly formed request`，**只漏另一条** | 探针实测 |
| G7 孤儿 toolResult 是缺口 | **早已实现**（`translator.go:475-476`） | 源码核查 |
| 本地 2ue_kiro.rs 价值有限 | **工程质量最高的材料**，G2 的实施蓝本 | 深度分析 |

关联：[PLAN-AND-STATUS](PLAN-AND-STATUS.md)、[test-strategy](test-strategy.md)、
[F06](findings/F06-schema-passthrough-gap.md)、[F07](findings/F07-gap-status-verified.md)、
[2ue-kiro-rs](repos/2ue-kiro-rs.md)

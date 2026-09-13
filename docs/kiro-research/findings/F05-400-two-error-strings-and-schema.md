# F05 · 400 的真正成因：不止工具名，而且错误串有两条

> **本文件推翻了我此前对 G1/G5 的范围判断。**
> 来源：第六轮扩大搜索（跳出 kiro 命名）新发现的仓库，克隆于 `/tmp/kiro-r6/`。
> 这一轮之所以有价值，正是因为你质疑了"是不是只搜了 kiro.rs 系"。

---

## 0. 为什么这些仓库此前全部漏掉

前五轮按仓库名检索 `kiro*`。但社区里有一整类**网关型**项目，
把 Kiro 当作**众多 provider 之一**藏在 `internal/adapter/provider/kiro/` 这种路径下，
**仓库名完全不含 kiro**：

| 仓库 | 语言 | ★ | 最近推送 | Kiro 代码位置 |
|---|---|---|---|---|
| `AbdoKnbGit/tau` | TS | 315 | 2026-09-12 | `src/lanes/kiro/` |
| `funny-vibes/agent-vibes` | TS | 359 | 2026-09-13 | `apps/protocol-bridge/src/llm/aws/` |
| `awsl-project/maxx` | Go | 61 | 2026-09-13 | `internal/adapter/provider/kiro/`（24 文件） |
| `fawney19/Aether` | Rust | 1466 | 2026-09-11 | `crates/aether-provider/transport/src/kiro/` |

还有一批体量过大未克隆但已确认存在 Kiro provider 的：
`OmniRoute`(★65562)、`9router`(★28616)、`opencodex`(★14504)、`llm-gateway`(★7522)…

> 📌 **这三条教训合起来才完整**：
> ① 按 star 采样会漏（既往教训）
> ② `sort=newest` 取前 200 会漏（既往教训）
> ③ **按仓库名检索会漏掉一整类网关型实现**（本轮新增）
> → 协议调研必须用**协议特征词做代码级检索**。

---

## 1. 🔴 最重要发现：同一根因，两个端点返回两条不同错误串

`[源码]` `agent-vibes/apps/protocol-bridge/src/llm/.../translator.ts:879-887`

> 后端用 **Smithy** 校验 schema，比 Anthropic 严格得多。碰到
> `$schema` / `additionalProperties` / `default` / `format` / `exclusiveMinimum` / `propertyNames`
> 等 draft-2020-12 关键字会**整个请求拒绝**：
> - `codewhisperer.` 端点 → `{"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"}`
> - **`q.` 端点 → `"Improperly formed request."`**

### 对我们的直接影响 —— 已实测核查（结论：确实漏判一条）

`[本仓库]` 我们有两套端点池（`group.go:136-137` 的 `"q"` / `"krs"`）。
我写了一次性探针跑真实归类器（跑完即删，未入库），**实测结果**：

```
body=Improperly formed request.                                     => bad_request_schema   ✅
body={"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"} => bad_request_unknown ❌
body={"message":"Improperly formed request."}                       => bad_request_schema   ✅
body={"reason":"REQUEST_BODY_INVALID"}                              => bad_request_unknown  ❌
```

**先纠正我自己在本文件初稿里的判断**：我原本假设归类器"只匹配一条串"，
实际上它比我预期的完善得多——`kiro_error_classifier.go:118-127` 的
`looksLikeKiroBadRequestSchemaError` **已经覆盖了 `improperly formed request`**，
而且还有独立的 `bad_request_schema` / `bad_request_tool_pairing` /
`bad_request_invalid_model` / `bad_request_auth` / `bad_request_quota` 五个 400 子类。
**G6/G7 的"归类"侧其实早就建好了。**

**但缺口真实存在**：`Invalid tool use format.` + `REQUEST_BODY_INVALID`
**两个特征串一个都没匹配上**，落进 `bad_request_unknown`。

`[本仓库]` `kiro_error_classifier.go:118-127`
```go
func looksLikeKiroBadRequestSchemaError(lower string) bool {
	return strings.Contains(lower, "schema") ||
		strings.Contains(lower, "inputschema") ||
		strings.Contains(lower, "improperly formed request") ||   // ← 只有 q. 端点这条
		strings.Contains(lower, "additionalproperties") ||
		(strings.Contains(lower, "properties") && strings.Contains(lower, "required"))
}
```
缺 `invalid tool use format` 和 `request_body_invalid`。

> → **这是一处已确证、成本极低（加两个字符串）的真实修复**，记为 **G8**。
> 影响：走 `codewhisperer.`/`krs` 端点时，schema 类 400 被误判为 unknown，
> 丢失针对性诊断日志（`logKiroBadRequestClassification`）。

---

## 2. 🔴 400 的成因不止工具名 —— schema 关键字超纲

我此前把 G5（工具名）当作 400 的主要成因。**这是不完整的。**
交叉验证后，400 收敛为**三类**成因：

### 成因 A：工具 schema 含超纲关键字（此前完全未覆盖）
`[源码]` `agent-vibes/translator.ts:898-906` —— 允许的 schema 键**白名单只有 7 个**：
```
type / description / properties / required / items / enum / title
```
- `const` 需折叠为单元素 `enum`（`:932-934`）
- 实现方式是**重建对象**而非删键，保证任意嵌套深度都不残留
- `[源码]` `tau/request.ts:272-275` 补充：**`required: []` 空数组也会触发 400**

> 这一条对我们威胁很大：Claude Code 的 MCP 工具 schema 普遍带
> `additionalProperties` / `$schema` / `default`，**是日常高发场景**。

### 成因 B：role 未严格交替
`[源码]` `tau/request.ts:584-588`、`agent-vibes/translator.ts:586-594`。
后者特别指出：**reactive context compaction 会把一个逻辑 assistant turn 拆成多条**，是高发场景。

### 成因 C：toolUse 没有对应的 toolResult
`[源码]` `agent-vibes/translator.ts:744-758`（补一条合成的 interrupted result）
`[源码]` `tau/payload_guards.ts:81-109`（反向：剔除孤儿 toolResult）

### 成因 D（独有）：历史引用了已下线的工具
`[源码]` `Aether/converter.rs:185-189, 562-575` —— 扫描历史里出现过的工具名，
凡不在当前 tools 列表里的，自动补一个空 properties 的**占位工具声明**。
其他实现都没做这个。

---

## 3. ✅ 工具名连字符：存疑项彻底了结

本轮又拿到**两条独立佐证**，与 F01 的 AWS 官方 service-2 完全一致：

`[源码]` `tau/src/lanes/kiro/tool_names.ts:73-80`
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
其测试 `kiro.test.ts:674-678` **断言** `mcp__context7__resolve-library-id` 原样通过。

`[源码]` `agent-vibes/translator.ts:862-863` 中文注释：
> "抓包验证：Kiro 后端接受下划线工具名，不需要 camelCase 转换"

**至此四路独立证据一致**（AWS 官方模型 + easayliu 线上故障 + tau 测试断言 + agent-vibes 抓包）：
**连字符合法，`[a-zA-Z0-9_-]`，max 64。此项结案。**

`tau` 的哈希防碰撞（djb2）正好对应我们 A-3 用例要求的**单射性**——
我们现有实现用 sha256 前 8 位，思路一致，可保留。

### 附带的新角度：工具名语义映射
`[源码]` `tau/tool_names.ts:22-40`：`Bash→shell`、`Read→read`、`Grep→grep`、
`TodoWrite→todo_list`、`Agent→subagent`。
理由："Kiro 模型是在其**原生工具名**上做的后训练"。

> 这不是为了过校验，是为了**命中模型训练分布**。属于效果优化而非正确性修复。
> `[推断]` 收益无法离线验证，**不建议本轮实施**——它会改变对用户可见的工具语义，
> 风险高于收益，且无法用 A/B 层证明。记录备查。

---

## 4. ⚠️ cache token 口径：两派互斥，必须实测定夺

这是本轮**唯一的硬冲突**，直接关系 G4 该怎么改：

| 实现 | 口径 | 证据位置 |
|---|---|---|
| `agent-vibes` | **三者并列相加**：`uncached + cacheRead + cacheWrite = input` | `event-stream.ts:409-430` |
| `tau` | **cache ⊂ input，需相减**：`input = rawInput - (cacheRead + cacheWrite)` | `loop.ts:894-907` |

```ts
// agent-vibes/event-stream.ts:409-430
const uncached   = readTokenNumber(usage, "uncachedInputTokens", ...)
const cacheRead  = readTokenNumber(usage, "cacheReadInputTokens", ...)
const cacheWrite = readTokenNumber(usage, "cacheWriteInputTokens", ...)
const sum = (uncached ?? 0) + (cacheRead ?? 0) + (cacheWrite ?? 0)
if (sum > 0) { inputTokens = sum; continue }   // 注意：是 fallback，先找 inputTokens
```

`tau` 还有个差别：它解析的是 **`metricsEvent` / `meteringEvent` / `contextUsageEvent`**
（`loop.ts:24, 675, 1229`），**不是 `metadataEvent`**，且其字段列表里**没有 `uncachedInputTokens`**。
上游没给数时用"本轮 context 总量 − 上轮"差分推导（`loop.ts:919-934`）——纯客户端推断。

> **二者不可能同时正确。** 照抄任一方都有风险。
> → **G4 必须靠 C-3 真实抓包定夺**，这进一步确认了 C-3 的 C 层准入资格。
> ⚠️ 且按既往教训，**实测必须上大 token 量级**，小负载会让缓存结论完全失真。

### 附带确认：社区"上游不返回用量"的误解，传播链可追溯
`agent-vibes` 自身存在**内部矛盾**：`cache-tracker.ts:4-8` 注释断言
"Kiro/CodeWhisperer 不在 usage 块里返回 cache 字段，所以我们客户端哈希模拟"，
并注明参考自 `Quorinex/Kiro-Go`（`cache-tracker.ts:10`）——
**可它自己的 `event-stream.ts` 又在解析真值**。

> 这正好印证既有判断：**"上游不返回用量"是老实现造成的误解**，
> 而且现在能看到误解**通过注释和交叉引用在社区传播**的完整链条。
> 我们代码里 `translator.go:4321-4333` 主动丢弃真值，很可能源自同一误解。

---

## 5. 请求体上限：社区唯一的真实字节阈值

`[源码]` `tau/src/lanes/kiro/request.ts:120-122` —— **社区唯一的请求体字节上限**：
```ts
const KIRO_TOOL_DESCRIPTION_MAX_LENGTH = 10_000
const KIRO_DEFAULT_MAX_PAYLOAD_BYTES    = 600_000   // 硬上限
const KIRO_DEFAULT_TARGET_PAYLOAD_BYTES = 220_000   // 软目标
```
双阈值（`request.ts:202-218`）：先软裁到 220KB，再硬裁到 600KB。
裁剪算法 `payload_guards.ts:45-47`：**成对删除历史条目**（`splice(i, 2)`）以保持 role 交替，
保护开头承载 system 的若干条，裁完对齐到 user 开头并修复孤儿 toolResult。

600KB 与社区此前观测的 ~615KB 吻合，是**独立的第二条证据**。

> ⚠️ **不要混淆**：其他仓库的"上限"全是**帧/缓冲上限，不是请求体上限**——
> `maxx/event_stream_types.go:195` 的 16MB、`Aether/state.rs:3` 的 16MB
> 都是单个 event-stream 帧上限，`Aether/kiro_stream.rs:8` 的 1MB 是 thinking 缓冲。

`q2api` 走了完全不同的路：**入口按 token 数预检**（`app.py:735,1094`：`input_tokens > 150000` 直接拒），
用 tiktoken 本地计数，不裁剪。

---

## 6. 端点：一条已被推翻的错误配置

`[源码]` `agent-vibes/protocol-types.ts:187-218`（基于官方 Kiro 客户端 `Kiro.app/.../extension.js` 逆向）：

- `https://q.us-east-1.amazonaws.com/generateAssistantResponse` —— 官方 Kiro 客户端**实际走的主端点**，
  **不发 `X-Amz-Target` 头**（有 `kiro_traffic.log` 抓包佐证）
- `https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse` ——
  需要 `X-Amz-Target: AmazonCodeWhispererStreamingService.GenerateAssistantResponse`

**`:199-203` 记录了一条已被推翻的错误**：早期的第三个端点条目
`AmazonQDeveloperStreamingService.SendMessage` 有**两处错**——
命名空间应为 `AmazonCodeWhispererStreamingService`，且 `SendMessage` 的请求 schema
与 `generateAssistantResponse` 不同，复用同一 payload **必然 400**。

> → 需核查我们仓库是否抄到过这个错误的三端点列表。

**未决冲突** `[待验证]`：`q.` 端点是否需要 `x-amz-target`？
`agent-vibes` 抓包说不需要；`q2api/templates/streaming_request.json` 抓包模板里**带着**。
`[推断]` 二者模仿的客户端不同（Amazon Q CLI vs Kiro IDE）所致，未验证。

---

## 7. 本文件产生/修正的结论

| 编号 | 变化 |
|---|---|
| **G1** | **范围修正**：不只是"400 不转移"，更关键的是**归类器可能只匹配一条错误串**，另一端点整类漏判 |
| **G5** | **范围扩大**：工具名只是 400 成因之一。**schema 关键字超纲是更高发的成因**，需新增 G6 |
| **G6（新增）** | 工具 input_schema 需按 7 键白名单重建，`const`→`enum`，禁 `required: []` |
| **G7（新增）** | role 交替 / 孤儿 toolResult 的守卫 |
| **G4** | **冲突升级**：两派口径互斥，必须 C-3 实测定夺，且须大 token 量级 |
| **G2** | 获得独立第二证据（600KB），但本轮 200k 上下文仍可能测不出 → 仍标「未验证」 |

## 8. 本文件产生的测试用例

| ID | 层 | 用例 |
|---|---|---|
| A-12 | A | schema 含 `$schema`/`additionalProperties`/`default`/`format` → 按 7 键白名单重建 |
| A-13 | A | `const` → 折叠为单元素 `enum` |
| A-14 | A | `required: []` → 移除该键而非保留空数组 |
| A-15 | A | 深层嵌套 schema：任意深度都不残留超纲键（验证"重建"而非"删键"） |
| A-16 | A | 工具名 `mcp__context7__resolve-library-id` **原样保留** |
| B-10 | B | 归类器对**两条错误串**都能归类（`Improperly formed request.` 与 `REQUEST_BODY_INVALID`） |
| B-11 | B | role 非交替的历史 → 被守卫修正 |
| B-12 | B | 孤儿 toolResult → 被剔除或补齐 |
| **C-3** | **C** | **实测 cache token 口径（大 token 量级）** —— 定夺 agent-vibes vs tau |

关联：[F01](F01-official-aws-service-model.md)、[F04](F04-quota-exhaustion-and-failover.md)

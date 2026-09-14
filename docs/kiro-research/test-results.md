# 测试结果与存疑清单

> 对应 [test-strategy.md](test-strategy.md) 的 A/B/C 三层分层。
> **本文档如实记录所有测试，包括「没测成」「测了但不算数」的部分。**

**执行日期**：2026-09-14
**真实额度消耗**：**5 次调用 / 13600 input tokens / 5 output tokens / $0.040875**

---

## 零、账号盘点（零额度消耗）

用 `POST /admin/accounts/usage/batch` 全量扫描 235 个账号 ——
这条路径读的是订阅额度接口，**不经过 chat 调用，不消耗额度**。

| 状态 | 数量 |
|---|---|
| `credits_exhausted`（已耗尽） | **125** |
| `normal`（额度正常） | **108** |
| 探测失败 | **2** |

> ✅ 用户说的「很多额度耗尽了，但肯定存在一部分没耗尽」得到证实：108/235 可用。

**🔴 发现 1（真实问题）**：账号 **673、725** 的 refresh token 已失效：
```
upstream request failed (status 401): {"message":"Bad credentials"}
```
`token_refresh` 重试 3 次后 `retry_exhausted`，但账号 **仍是 `status=active, schedulable=true`**。

> ❗ **此处「会被反复调度」的原始判断已被 §七 T10 实测推翻** ——
> 账号一旦被真实调度到并返回 403，**会**被自动置为 `error/schedulable=false`。
> 真实缺陷收窄为：**摘号发生在请求路径，而非 token_refresh 失败时**。

---

## 一、A 层（纯单测，0 额度）

| 项 | 用例数 | 结果 |
|---|---|---|
| G8 归类器 400 特征串 | 9 | ✅ 全绿（反证：回退后失败） |
| G6 工具 schema 白名单 | 6 | ✅ 全绿（反证已验证） |
| G5 工具名字符集清洗 | 10 | ✅ 全绿（反证：回退后 **6/10 失败**） |
| G2 请求体积守卫 | 8 | ✅ 全绿（反证：A-23 探针确认非空转） |

**⚠️ 一个此前被掩盖的事实**：`internal/service/*_test.go` 带 `//go:build unit` 标签，
裸跑 `go test ./internal/service/` 会**静默跳过**这些测试。
此前所有「./internal/service/ 全绿」的结论都是在**没跑到这些测试**的情况下得出的。
本轮已用 `go test -tags unit` 重跑：

```
ok  github.com/Wei-Shaw/sub2api/internal/service   190.254s
ok  github.com/Wei-Shaw/sub2api/internal/pkg/kiro    1.623s
```

---

## 二、B 层（mock 上游，0 额度）

新增 2 个测试（`kiro_runtime_state_test.go`）：

| 用例 | 断言 | 结果 |
|---|---|---|
| `TestHandleKiroHTTPErrorSchemaBadRequestDoesNotFailover` | schema 400 **不**触发 failover（2 个子用例覆盖两个端点的不同错误串） | ✅ |
| `TestHandleKiroHTTPErrorMonthlyQuotaFailsOverAndRateLimits` | 402 MONTHLY_REQUEST_COUNT **既** failover **又** 限流 | ✅ |

> 这条 failover 不对称是**刻意设计**：
> schema 400 是**我们自己**发的坏请求，失败转移只会用同一个坏请求烧光整个号池。

---

## 三、C 层（真实 Kiro 上游）

### C-1 · 已耗尽账号的真实响应 —— 0 额度（号已废）

隔离分组只放 1 个已耗尽账号（id=622，50/50 credits），发真实请求。

**真实上游响应**（此前只是猜测，现已抓到）：
```
HTTP 402
{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}
```

系统行为：
```
kiro monthly request count rate-limited  account_id=622 reset_at=2026-10-01T00:00:00.000Z
gateway.failover_switch_account          upstream_status=402 switch_count=1
```
落库核验：`rate_limited_at=2026-09-13 23:53:19`，`rate_limit_reset_at=2026-10-01 00:00:00` ✅

> 🎯 **B 层 mock 与真实上游完全一致** —— `TestHandleKiroHTTPErrorMonthlyQuotaFailsOverAndRateLimits`
> 断言的 402 + 限流 + failover，真实跑下来逐条吻合。mock 猜对了，且是**因为正确的原因**猜对的。

客户端最终收到 502，**这是正确的** —— 该分组只有 1 个账号，failover 无处可去。

### C-2 · opus 模型必须被拒 —— 0 额度

```
HTTP 404
{"error":{"message":"Model \"claude-opus-4-20250514\" is not supported by any configured account in this group","type":"model_not_found"}}
```

**在网关层就被拒，压根没发起上游调用** —— 完全符合用户约束「不能用 opus 模型」，且零额度浪费。

### C-3 · 真实 metadataEvent 抓包（G4 定夺）—— 消耗 4 次调用

用既有的 `KIRO_UPSTREAM_TRACE=1` 诊断开关（**无需改代码**）抓取原始事件流。
构造 ~4.5k token 的可缓存 system 前缀，连发两次完全相同的请求。

**真实上游事件流（逐字）**：
```
eventType="assistantResponseEvent"
eventType="metadataEvent"      payload={"stopReason":"END_TURN"}
eventType="contextUsageEvent"  payload={"contextUsagePercentage":4.349499702453613}
eventType="meteringEvent"      payload={"unit":"credit","unitPlural":"credits","usage":0.019533370547263684}
```

**结论：`metadataEvent` 里根本没有 `tokenUsage` 字段，更没有 cache 字段。**

两次完全相同的请求：
- 第 1 次 metering usage = `0.019533370547263684`
- 第 2 次 metering usage = `0.01937814666666667`

→ 计费几乎不变，**没有发生服务端 prompt caching**。

**G4 裁决**：`translator.go:4408` 那句注释
```go
// Kiro cache usage is reported only from local emulation. Ignore
// tokenUsage cache fields even if upstream includes them.
```
在**本批账号（KIRO FREE 个人号）上是正确的**，维持现状不改。
`agent-vibes` 一派「上游会下发真实 cache 用量」的说法**在本档位未复现**。

> ⚠️ 但见存疑清单 Q2 —— 这个结论**不能外推到付费档位**。

### C-4 · 调度必须收敛到额度正常的账号 —— 消耗 1 次调用

分组构造：**8 个已耗尽 + 1 个健康（id=712）**。

**真实 failover 链**：
```
629 → 402 MONTHLY_REQUEST_COUNT → failover(1)
635 → 402 MONTHLY_REQUEST_COUNT → failover(2)
712 (健康)                       → HTTP 200 ✅
```
耗时 4.6s（对比单账号直连 2.5s），响应内容正确。

> 🎯 **用户的核心约束「需要保证调度到的是额度正常的账号」已被真实验证。**

**❗ 一次自我修正**：C-3/C-4 第一版我用的是「5 坏 + 3 好」分组，结果 HTTP 200，
我差点据此宣布 C-4 通过。但查日志发现调度器**第一次就选中了健康账号 630**，
**根本没走 failover 路径** —— 而且那 5 个坏号当时 `schedulable=true`、
`rate_limit_reset_at=NULL`，说明它们并未被预先排除，纯属**运气**。
那一轮不能算 C-4 通过，因此重做了上面这版（8 坏 + 1 好）才真正压到 failover 路径。

---

## 四、结论汇总

| 编号 | 缺口 | 最终状态 | 证据层级 |
|---|---|---|---|
| G8 | 归类器 400 特征串 | ✅ 已修复 | A + B + **C 层吻合** |
| G6 | 工具 schema 白名单 | ✅ 已修复 | A + **C 层实测**（§7.2） |
| G5 | 工具名字符集清洗 | ✅ 已修复 | A + **C 层实测**（§7.2） |
| G2 | 请求体积守卫 | ⚠️ 算法正确，**阈值未验证** | A only（Q3/Q9 仍成立） |
| G4 | cache token 口径 | ✅ **维持现状**（实测定夺） | **C 层实测** |

> §七 第二轮测试后，**只剩 G2 一项停留在「仅单测」层级**。

---

## 五、存疑清单（必须保持怀疑的部分）

### Q1 · ⚠️ 失效凭据账号的自动摘除（**结论已修正**，见 §七 T10）

**我上一轮的判断是错的**，实测后修正：真正被调度到并返回 403 的账号
**会**被系统自动置为 `status=error, schedulable=false`，不会「被反复调度」。

真实缺陷比我原先说的**窄**：`token_refresh` 的 401 失败**本身不摘号**，
只有**请求路径**上的 403 才摘。详见 §七 T10。

### Q2 · ⚠️ G4 结论不能外推到付费档位

本批 **235 个账号全是 `KIRO FREE`**（`Q_DEVELOPER_STANDALONE_FREE`）。
C-3 只证明了 **FREE 档不下发 cache 用量**。
付费档（Pro/Enterprise）是否会在 `metadataEvent` 里带 `tokenUsage.cache*`
**本轮无法验证** —— 手上没有付费账号。

### Q3 · ⚠️ G2 阈值（450KiB）本轮未被端到端触达

账号上下文上限 200k token，实测一次 4.5k token 请求的
`contextUsagePercentage` 仅 4.35%。要堆到 450KiB 负载需要接近打满上下文，
本轮**没有构造**（会显著消耗额度）。

因此 G2 只证明了**裁剪算法正确**（A 层 8 例），
**未证明 450KiB 这个阈值选得对**。它来自社区经验值，仍是推断。

### Q4 · ⚠️ 模型覆盖面受限

只测了 `claude-sonnet-4-5-20250929`。
- **haiku 未实测**（用户说可测，但为省额度未测；风险低，同协议路径）
- **opus 被网关正确拒绝**，因此 opus 的**上游行为**永远无法验证
- 长上下文（>200k）行为无法验证

### Q5 · ✅ **已解除**（见 §7.2）

原疑虑：G5/G6 只有 A 层单测，未经真实上游反证。

**第二轮已补齐**：T2/T3 用真实脏工具名 + draft-2020-12 关键字打真实上游，
均 200；并用线格检查确认了上游**实际收到**的是清洗后的名字与白名单化的 schema，
且反向映射能把 `fs_read_file_f21bae8b` 还原成 `fs.read/file`。

### Q6 · ⚠️ UI 层未做 Playwright 验证

本轮账号导入走的是**真实两步接口**（`import-kiro-rs` → `accounts/batch`），
复刻了前端 `handleKiroImport` 的凭据映射逻辑，
但**没有真正驱动浏览器**跑一遍页面导入。前端那层映射代码是否与我复刻的一致，
属于**按代码阅读推断**，未经运行时验证。

---

## 六、真实额度账单

| 用例 | 调用次数 | 额度消耗 |
|---|---|---|
| 账号盘点（usage API） | 235 | **0**（不走 chat） |
| C-1 已耗尽账号 | 1 | **0**（号已废） |
| C-2 opus 拒绝 | 1 | **0**（网关层拒绝） |
| C-3 metadataEvent 抓包 | 4 | 13566 in / 4 out |
| C-4 failover 收敛 | 1 | 17 in / 1 out |
| **合计** | — | **13600 in / 5 out / $0.040875** |

---

## 七、第二轮：真实功能矩阵测试（2026-09-14）

**目的**：上一轮 Q5 指出 G5/G6 「只有 A 层单测，未经真实上游反证」。
本轮专门构造**会触发这些代码路径的真实请求**去撞真实上游。

**消耗**：13 次调用 / 2212 in / 171 out / **$0.0092**
**分组**：`kiro-feature-matrix`（12 个健康账号）

### 7.1 功能矩阵结果（9/9 通过）

| 用例 | 场景 | 结果 |
|---|---|---|
| T1 | 基础工具调用 | ✅ 200，正确产生 `tool_use` |
| **T2** | **MCP 风格 + 非法字符工具名（G5）** | ✅ 200 |
| **T3** | **draft-2020-12 关键字（G6）** | ✅ 200 |
| T4 | Agent 循环（tool_result 回传） | ✅ 200，「22°C and sunny」 |
| T5 | 多轮对话（上下文记忆） | ✅ 答 `94`，记住了前轮的 47 |
| T6 | 图片识别（100×100 棋盘格 PNG） | ✅ 正确描述为 checkerboard |
| T7 | 知识库检索（长 system + 59 条目） | ✅ 精确答出 `ZEPHYR-042` |
| T8 | 流式 + 工具 | ✅ SSE 事件序列完整，含工具事件 |
| T9 | WebSearch | ✅ 真实联网，返回 Britannica 来源 |

### 7.2 🎯 G5/G6 的真实反证（Q5 解除）

**关键**：光看「T2/T3 返回 200」不足以证明修复起作用 ——
必须看**实际发给上游的 payload 长什么样**。用 `BuildKiroPayloadWithContext`
对同一批工具做线格检查：

| 原始工具名 | **上游实际收到** | 说明 |
|---|---|---|
| `mcp__github__search-repos` | `mcp__github__search-repos` | **原样穿过**（连字符合法） |
| `fs.read/file` | `fs_read_file_f21bae8b` | 点/斜杠清洗 + 哈希保单射 |
| `查询数据库` | `______4f49c152` | CJK 清洗 + 哈希 |

**G6 schema 实测对比**：
```
输入: {"$schema":"...draft/2020-12","type":"object","additionalProperties":false,
       "properties":{"kind":{"type":"string","const":"user"},
                     "age":{"type":"integer","minimum":0,"exclusiveMinimum":0,"default":18}},
       "required":["kind"],"$defs":{"u":{"type":"string"}}}

上游收到: {"properties":{"age":{"type":"integer"},
                        "kind":{"enum":["user"],"type":"string"}},
          "required":["kind"],"type":"object"}
```
`$schema` / `$defs` / `additionalProperties` / `exclusiveMinimum` / `minimum` / `default`
**全部剥离**，`const:"user"` **正确折叠为** `enum:["user"]` —— 且真实上游接受了。

**反向映射也验证了**：单独给一个脏名工具 `fs.read/file` + `tool_choice:any`，
上游返回 `fs_read_file_f21bae8b`，**客户端收到的是 `fs.read/file`**（原始名还原正确）。

> ✅ **Q5 解除**：G5/G6 从「依据官方约束的推断」升级为「真实上游实测验证」。
> 同时再次确认「MCP 连字符导致 400」是错的 —— 连字符原样穿过且上游接受。

### 7.3 T10 · 失效凭据账号的真实调度行为（修正 Q1）

分组：`673, 725`（refresh token 返回 401 Bad credentials）+ 1 个健康号 663。
连发 3 次请求。

**真实调度链**：
```
SELECT   673  → 403 "Access forbidden: The bearer token..."
FAILOVER 673  → status=403
SELECT   663  → 200 ✅   （后续 2 次 sticky 命中 663）
```

**落库状态**：
```
 id  | status | schedulable |            error_message
-----+--------+-------------+-----------------------------------
 673 | error  |      f      | Access forbidden (403): The bearer token
 725 | active |      t      | (null)
```

**结论（修正上一轮的错误判断）**：
- ✅ 系统**会**自动摘除真正失败的账号（673 → `error` + `schedulable=false`）
- ✅ 用户请求**不受影响**（failover 到健康号，3/3 成功）
- ⚠️ 但 **`token_refresh` 的 401 失败本身不摘号** ——
  725 因为没被调度到，至今仍是 `active/schedulable=true`

**真实缺陷（收窄后）**：已知凭据失效的账号，要等到**下一次被真实调度并 403**
才会被摘除，而不是在 `token_refresh.retry_exhausted` 时就摘。
代价是每个死号会**浪费一次 failover 配额**（本例 max_switches=15，影响有限）。

### 7.4 本轮新增存疑

**Q7 · 图片测试覆盖面窄**：只测了 100×100 PNG 棋盘格（302 字节）。
大图、JPEG/WebP、多图、超限尺寸**均未测**。

**Q8 · 知识库是「长 system prompt」而非真实 RAG**：
T7 验证的是长 system + cache_control 能正确透传并被正确检索，
**不等于**验证了产品意义上的知识库/向量检索功能。

**Q9 · G2 阈值仍未触达**：本轮最大请求（T7 知识库）仅 487 input tokens，
距离 450KiB 差了几个数量级。**Q3 依然成立**。

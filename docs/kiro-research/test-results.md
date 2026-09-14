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

> ✅ **2026-09-14 结案（见 §9.1）**：该推断已被真实上游二分探测**推翻**。
> 上游判据不是字节数，而是加权字符数 `ascii*1 + 非ascii*8`，
> 实测区间 (1,320,000, 1,360,000]，默认改为 1,300,000。
> 本节「450KiB / `limit_bytes=460800`」的表述连同**口径**一起作废。

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

---

## 八、第三轮：深度验证（回应「不要只测表面」）

测试脚本：`/tmp/sub2api-run/wide_tests.py`、`/tmp/sub2api-run/tool_deep_tests.py`
账号组：812（12 个健康 KIRO FREE 账号），模型 `claude-sonnet-4-5-20250929`。
本轮全部为 **C 层真实上游调用**，共约 60 次真实请求。

### 8.1 O 组 · tools 协议深度（8/8 PASS）

| 用例 | 结果 | 实证 |
|---|---|---|
| O1 单轮并行多工具 | ✅ | 一轮返回 3 个 `tool_use`：`get_population/get_time/get_weather` |
| O2 `tool_choice=any` | ✅ | 闲聊输入也强制产生 `tool_use` |
| O3 `tool_choice=tool` | ✅ | 指定 `get_population`，实得唯一且正确 |
| O4 `tool_choice=none` | ✅ | 明确要求用工具仍 `tool_use=0` |
| O5 >64 字符工具名 | ✅ | 88 字符、**仅尾部不同**的两个名字，反向还原精确命中 `..._beta` |
| O6 工具名含 `-` 和 `.` | ✅ | `my-tool.v2` 原样往返 |
| O7 深层嵌套 schema | ✅ | 数组+枚举+嵌套对象，实得 `{"query":{"filters":[{"field":"status","op":"eq","value":"active"}],"limit":10}}` |
| O8 draft-2020-12 关键字 | ✅ | `$schema`/`additionalProperties`/`default` 被清洗，未触发 Smithy 400 |

**O5 澄清了 Q-工具名 疑问**：截断保持单射，前 64 字符完全相同也不会串号。
**O6 修正了记忆中的存疑**：连字符**合法**，此前「连字符非法」的猜测已证伪。

### 8.2 P 组 · agent 多轮工具循环（6/6 PASS）

- P1 工具结果回传 → 模型据此作答（「需要带伞」引用了回传的 heavy rain/95% 湿度）
- P2 三轮对话后再次发起新工具调用 ✅
- P3 **并行** `tool_result` 同轮回传 2 条，两条结果都被正确消费
- P4 **孤儿 `tool_use`**（无 `tool_result`）→ 200，未崩溃
- P5 **孤儿 `tool_result`**（无 `tool_use`）→ 200，未崩溃
- P6 `is_error: true` 正确传递，模型识别出「工具超时」

### 8.3 Q 组 · websearch（真实生效）

`server_tool_use` → `web_search_tool_result` → `text` 三段齐全，**10 条真实结果**：

```json
{"title":"Canberra","url":"https://en.wikipedia.org/wiki/Canberra",
 "encrypted_content":"Canberra ... is the capital city of Australia ...",
 "type":"web_search_result"}
```

⚠️ **`citations` 字段为 `null`**：模型改以 markdown 内联链接给出处
（`[Britannica](https://...)`）。经查为**上游行为**，结构体本身完整透传，
非移植缺陷。但「Anthropic 原生 citations 数组」在 Kiro 上游**不可用**。

### 8.4 R 组 · 流式协议（3/3 PASS）

- R1 `message_start→content_block_start→delta→stop→message_delta→message_stop` 六事件齐全，无缺失
- R2 `input_json_delta` 拼接得到合法 JSON `{"city":"Tokyo"}`
- R3 `message_delta` 携带 usage

### 8.5 K 组 · 图片多格式（回应 Q7，5/5 有效）

语义判据（非「HTTP 200 即通过」）：必须答对**形状+颜色**才算 PASS。

| 格式/尺寸 | 结果 | 模型实际描述 |
|---|---|---|
| PNG 300×300 红圆蓝底 | ✅ | "circle ... Red/orange-red" |
| JPEG 300×300 红圆蓝底 | ✅ | "a circle ... in red" |
| GIF 320×240 三色带 | ✅ | "Top: Bright red / Middle / ..." 三条带全中 |
| PNG 1400×1400 超大 | ✅ | "a large circle ... bright red" |
| 三图同轮混格式 | ✅ | 逐图正确：红圆 / 三横条 / **黑白棋盘格** |

**K5 边界（有价值的负面用例）**：PNG 数据谎称 `image/jpeg` → 上游 400
`"The image was specified using the image/jpeg media type, but the image ..."`。
说明**媒体类型未被我们伪造或纠正**，忠实透传。

⚠️ **WebP 未测**：本机 `sips` 不支持导出 WebP，无法生成样本。**保持未验证**。

### 8.6 L 组 · 文档识别

- **L1 PDF `document` 块**：✅ 真实可用。自造 PDF 内嵌 `SECRET CODE: ZEBRA-9931`，
  模型答出 `ZEBRA-9931` —— 证明 **PDF 被真实解析**，非 OCR 图片路径。
- **L2 文本 `document` 块**（`source.type=text`）：🔴 **发现真实缺陷 → 已修复并复验**。

  初测传入 `Project PHOENIX. Owner: Li Wei. Budget: 42000 USD.`，
  模型回答「我没有看到 owner 相关信息」—— 请求 200，但文档内容根本没进负载。

  **根因**（`translator.go:2656` 修复前）：`buildDocumentTextFallback` 里
  第 2649 行刚把 text 源 base64 编码好，第 2656 行紧接着
  `if format != "pdf" { return "" }` —— 一切非 PDF 格式**全部静默丢弃**。
  而 `kiroDocumentFormat` 明明已支持 `txt/csv/html/json/doc/docx`，
  属于「映射表写了、消费端没接」的死代码路径。此处**此前无任何测试覆盖**，
  这正是缺陷能长期存活的原因。

  **修复**：非 PDF 的文本类格式解码后直接内联，附 6000 字符截断保护，
  并拒绝非 UTF-8 二进制（避免 docx/doc 把乱码塞进负载）。PDF 路径原样保留。

  **真实上游复验**（修复后重新编译重启）：
  | 用例 | 结果 |
  |---|---|
  | L2 `text/plain` | ✅ "The owner is Li Wei and the budget is 42000 USD." |
  | L3 `text/csv` | ✅ "The amount for INV-7742 is 980."（新增用例） |
  | L1 `application/pdf` 回归 | ✅ "The secret code is: ZEBRA-9931"（未破坏） |

  **新增测试**：`internal/pkg/kiro/document_block_test.go`（5 条，全绿）——
  覆盖 text 源内联、base64 文本内联、超长截断、非 UTF-8 拒绝、PDF 路径回归。

### 8.7 M 组 · 大知识库（回应 Q8）—— 发现真实缺陷

| 规模 | 结果 |
|---|---|
| ASCII 331KiB（5000 条） | ✅ 答对，`input_tokens=84870` |
| 中文 441KiB（2000 条） | ✅ 答对，`input_tokens=27939` |
| 中文 661KiB（3000 条） | ⚠️ **200 但答错**（"I can't discuss that."） |
| 中文 882KiB+ 单条消息 | ❌ 400 `Input content length exceeds threshold.` |

**🔴 G1-B（新发现的真实缺陷）：体积守卫静默丢失上下文**

构造 3080KiB 请求，暗号放在**第 0 轮**，问题在最后一轮，**稳定复现 2/2**：

```
kiro.payload_trimmed  original_bytes=1581502  final_bytes=421705
                      limit_bytes=460800  dropped_history_items=48
```

模型自述：**「您只发送了标记为第22段到第29段的填充文本」** —— 前 22 轮被静默裁掉。
接口返回 **HTTP 200**，用户**完全无法察觉上下文已被截断**，只会得到一个自信的错误答案。

**另一半问题**：单条超大消息**无历史可裁**，守卫放弃：
```
kiro.payload_still_oversized  original_bytes=992552  final_bytes=992552  dropped_history_items=0
```
原样发给上游，换来一个上游 400。应在入口就返回可读错误，而非浪费一次上游往返。

→ 已登记待办 **#47**。

### 8.8 N 组 · OpenAI 兼容路径（3/3 PASS）

- N1 基础对话 ✅ / N2 `tool_calls` 结构正确 ✅ / N3 `image_url` + base64 data URI 图片识别 ✅

### 8.9 计费与落库（#44）

API 返回 `input_tokens=23, output_tokens=4`，落库 `usage_logs` id=9497 **完全一致**。

**🟡 G4 结论更新（推翻记忆中的判断）**：开 `KIRO_UPSTREAM_TRACE=1` 抓取**全部**上游事件类型：

```
40 assistantResponseEvent   10 toolUseEvent
 7 meteringEvent            7 metadataEvent      7 contextUsageEvent
```

`meteringEvent` 只有 `{"unit":"credit","usage":0.143...}`，
`contextUsageEvent` 只有百分比，**全程没有任何 `tokenUsage` 事件，也没有 cache 字段**。

两次相同大 system（136KB）请求，`cache_read_input_tokens` 均为 0，落库也为 0。
→ **在 KIRO FREE 个人号上，上游确实不下发 cache 用量**，当前「本地推算」的实现是合理的。
⚠️ 但此结论**仅限 FREE 档**，付费档是否下发**无法验证**。

### 8.10 并发与调度（#45）

12 路并发：**HTTP 200 = 12/12，内容正确 = 12/12**（每路校验专属 `CONC-{i}` 标记），
总耗时 13.7s，**无串话、无内容错配**。12 条全部落在账号 658 —— 符合 sticky 亲和设计预期。

### 8.11 本轮存疑汇总

- **Q10 WebP 图片格式未验证**（工具链限制，非代码问题）
- ~~**Q11 `document` 块 `source.type="text"` 内容丢失**~~ —— ✅ **已定位并修复**，
  真实上游复验通过，已补 5 条单测（见 8.6）
- **Q12 G4 结论仅在 FREE 档成立**，付费档 cache 用量下发情况未知
- **Q13 websearch `citations` 数组在 Kiro 上游不可得**，依赖该字段的客户端会拿到 null
- **Q3 依旧成立**：所有测试受限于 haiku/sonnet + 200k 上下文，opus 与长上下文行为未知

---

## 9. 体积守卫改造与阈值实测（2026-09-14）

### 9.1 Q3 结案：450KiB 阈值是错的，口径也是错的

此前 8.x 多处把上游上限当作**字节数**（`limit_bytes=460800`），并在 Q3 中
诚实标注「未证明 450KiB 选得对，它来自社区经验值，仍是推断」。本轮做了真实
上游二分探测，**该推断被推翻**：

上游判据是 **加权字符数**，而非字节 / 字符 / token：

```
weight = ascii_chars * 1 + non_ascii_chars * 8
```

- 实测可行区间：**(1,320,000, 1,360,000]**
- 代码默认取 **1,300,000**（约 2% 余量）
- 用混合中英语料做独立验证：**2/2 预测命中**，排除对单一语料过拟合

**为什么按字节设阈值两头都错**：中文每字符计 8，纯中文会话会远早于真实上限
就被裁；纯英文会话又要到远超 460800 字节才会被拦。非 ASCII 权重是 8，
而非直觉上的 3（UTF-8 字节数）—— 这点无法从字节口径推出来。

> ⚠️ 探测节奏：连续兆字节级探测会触发 Kiro 滥用检测，返回
> `Your User ID is temporarily suspended`。本轮全程 12~18s 间隔，无账号被封。

### 9.2 改造：压缩优先，裁剪兜底

原实现只有「从最早历史开始截断」一种手段。现改为四段压缩流水线，
`history_trim` 退化为最后手段：

```
history_tool_results → history_thinking → history_definitions → history_images
                                                              → history_trim（兜底）
```

真实上游验证（needle 放**最早**一轮，暗号 ALPHA-7）：

| 场景 | 阈值 | 结果 | 关键响应头 |
|---|---|---|---|
| 14 轮工具调用累积 76,563 B | 60,000 | **200，答出 ALPHA-7** | `compressed-items=13` `dropped-items=0` `trimmed=false` `stages=history_tool_results` |
| 30 轮纯文本 125,472 B | 50,000 | 200（无可压缩内容，直接裁剪） | `dropped-items=38` `trimmed=true` `stages=history_trim` |

第一行是核心证据：**仅靠压缩（76,563→38,018）就够了，一轮历史都没丢**，
最早一轮的暗号完整存活。按旧实现这一轮会是第一个被丢弃的。

### 9.3 两道独立的工具输出闸门（易混淆）

翻译阶段的 `compactKiroToolResultText`（limit 12000 → 头 4000 + 尾 2000）
在守卫**之前**就跑了，处理的是**单条**超大输出。守卫的
`history_tool_results` 阶段真正解决的是**累积**问题：几十轮工具调用每条都
卡在 6000 字符以下，合计仍达数百 KB。

⚠️ 因此守卫阶段的预算必须显著低于 6000（现为 2000），否则在真实负载上
一条都压不动 —— 这个 bug 曾真实存在（预算 4000 时该阶段是死代码）。

### 9.4 修复：两处压缩阶段不幂等

`truncateHeadTail` 和 `compressKiroToolDefinitions` 的输出都自带标记，
长度仍 > 预算。`on_upstream_400` 重试会二次跑守卫，导致**重复截断并把上一次
的标记切碎成乱码**，同时虚增计数器。已加标记判别（`strings.Contains` /
`strings.HasSuffix`）并补幂等断言。

### 9.5 修复：`DeferredToUpstream` 曾被误报为软失败

`on_upstream_400` 预检路径（故意不动负载、原样发）此前复用 `StillOversized`
标记。后果有二：

1. 响应带上一组 `original == final` 的裁剪头，与「真的裁掉了历史」无法区分
2. 日志打 `kiro.payload_still_oversized` **WARN** —— 该级别的语义是
   「压缩+裁剪跑到底仍塞不下」的软失败，这条策略下每个大请求都会误报

已拆出独立的 `DeferredToUpstream` 字段，`Triggered()` 刻意不含它
（不回裁剪头），但日志保留并降为 INFO `kiro.payload_deferred_to_upstream`。
真实上游复验：裁剪头 **0 条**，日志 `still_oversized=false`。

### 9.6 新增设置项（页面可配）

| key | 取值 | 默认 |
|---|---|---|
| `kiro_oversize_behavior` | `compress_then_trim` / `on_upstream_400` / `reject` | `compress_then_trim` |
| `kiro_oversize_threshold` | 10,000 ~ 5,000,000（**加权字符数，非字节**） | 1,300,000 |

非法行为值一律回落 `compress_then_trim`，**绝不回落 reject** ——
配置写错的后果应当是「退回默认策略」，而不是对全部 Kiro 请求拒服务。

API 往返实测 7/7 通过（含非法值回落、超大值夹取、传 0 不覆盖已有值）。
三种行为端到端实测：同一超限负载在 `reject` 下得 **413**（不发上游）、
在 `compress_then_trim` 下得 **200**，证明页面配置真实驱动网关行为。

### 9.7 本节存疑

- **阈值上界仅在个人 FREE 档测得**，付费档是否更宽未验证
- **加权公式为经验拟合**：在实测区间内自洽且交叉验证通过，但非官方文档，
  上游若调整口径不会有任何通知
- `reject` / `on_upstream_400` 两种行为的真实上游验证只覆盖了
  「预检不动负载」路径；**上游真的返回 400 后的缩减重试未在真实环境触发**
  （需要构造 >1.3M 加权的负载，代价高且有触发滥用检测的风险）

---

## 10. 交互式 Claude Code CLI 实测（2026-09-14）

前面几节的结论全部来自 curl / 单测。本节改用**真实 Claude Code CLI 交互式多轮会话**
复测同一批能力，结果发现了 3 个前面所有手段都没能暴露的缺陷。

### 10.1 方法与隔离

- PTY 驱动（`pty.fork()` + ANSI 剥离），真实 TUI，不是接口调用
- 回合结束判据为「屏幕尾部不再出现 `esc to interrupt`」，纯静默计时会在
  工具执行的思考间隙误判，导致后续输入打进运行中的会话
- **不改动本机 CLI 配置**：全程 `CLAUDE_CONFIG_DIR` 指向临时目录，
  每次运行前后比对 `md5 -q ~/.claude.json`
- 每个场景在第 1 轮埋入「针码」（如 `CHARLIE-77`），最后一轮索回 ——
  只断言「没报错」会漏掉**静默上下文丢失**，针码才能证伪

> ⚠️ 对照实验结论：`~/.claude.json` 在本轮工作期间确有变化，但
> 逐字段核对证实变化来自**承载本次会话的那个 CLI 实例**（`numStartups` 等计数器），
> 测试工作区 `/private/tmp/cc-guard-ws` 从未出现在个人配置的 `projects` 里。
> 单独跑一次隔离探针（临时 CONFIG_DIR + 真实 CLI 调用）验证 md5 全程不变。

### 10.2 BUG #2：流式入口漏掉 413 映射（已修复）

`behavior=reject` 下 CLI 屏幕渲染 `API Error: 502 Upstream request failed`，
把用户引去排查上游，而请求根本没发出去。

成因：`forwardKiroMessages` 有流式/非流式两个分支，413 映射只加在非流式分支。
**Claude Code CLI 只走流式、curl 默认非流式、单测也只断言非流式** ——
三者一致地掩盖了这个缺口。同样的缺口还存在于 chat_completions 与 responses 入口。

修复后实测：5 × HTTP 413，屏幕渲染 `Request too large`，
`502` / `Upstream request failed` / `API Error` 均为 0 次。

### 10.3 BUG #2b：reject 路径无任何日志（已修复）

客户端收到 413、服务端却一条 `kiro.payload_rejected` 都没有 ——
`buildKiroPayloadForAccountWithArn` 带错误提前 return，走不到 `logKiroPayloadTrim`。
拒绝恰恰是最需要可观测的那条路径：没有日志，运维无从判断是阈值配置过严
还是客户端真的发了超大请求。

修复后：5 × `kiro.payload_rejected`（weight 93569 > limit 60000）↔ 5 × HTTP 413，两侧吻合。

### 10.4 BUG #3：裁剪破坏当前轮 tool 配对（已修复）★ 本节最重要发现

场景 C（7 轮 + MCP + 多文件读取，阈值 45000）下，**每次** `kiro.payload_trimmed`
后一秒内必然收到上游 400：

```
Bedrock error message: The number of toolResult blocks at
messages.4.content exceeds the number of toolUse blocks of previous turn.
```

位置恒为 `messages.4.content` —— 正是当前轮的位置。

成因：当前轮的 toolResults 在 `processMessages` 阶段就依据**未裁剪**的 history
校验过（`validateToolPairing`），而体积守卫在其后才裁剪 history。
配对的 toolUse 被切走后，没有任何环节重新校验当前轮。

修复策略是**补回 toolUse** 而非删除 toolResult —— 当前轮的工具输出正是模型
此刻推理的依据，删掉等于让它凭空失忆。

修复前后对比（同一配置）：

| | 修复前 | 修复后 |
|---|---|---|
| 上游 400（tool 配对） | 6 次 | **0 次** |
| `history_trim` 次数 | 7 | 21 |
| 屏幕输出 | 61KB（第 4 轮即中断） | 485KB（跑完） |
| C1–C6 检查点 | 部分缺失 | 全部渲染 |
| MCP 执行 | 中断 | `⏺Called inv` 正常 |

典型一次裁剪：加权 121,209 → 44,443（limit 45,000），
四个阶段全部参与：`history_tool_results` → `history_thinking` →
`tool_definitions` → `history_trim`，`dropped_history_items=2`。
**关键：`tool_definitions` 被压缩之后 MCP 工具依然可以正常调用。**

### 10.5 针码验证（静默上下文丢失）

| 场景 | 形态 | 轮数 | 针码回收 |
|---|---|---|---|
| A2 | 工具 + MCP + 多文件 | 7 | ✅ `⏺ZULU-42` |
| B | 中文日志（weight=8 路径） | 5 | ✅ `⏺BRAVO-91` |
| C3 | MCP 中文输出 + 多文件 | 4 | ✅ `⏺CHARLIE-77` |
| R | reject 行为 | 3 | ✅ RJ1/RJ2/RJ3 |

### 10.6 本节存疑

- **场景 C3 的针码回收不能单独作为 BUG #3 的证据**：该次运行未触发守卫
  （窗口内无 `kiro.payload_*` 事件），只能证明基线健康。
  BUG #3 的真正证据是 C2（21 次裁剪 / 0 次 400）与修复前的 6 次 400 的对比。
- **场景 B 未真正走到 weight=8 的超限路径**：`app-zh.log` 整体加权 446,999
  （110,999 字符中 48,000 个非 ASCII，4 倍放大），但 Claude Code 的 `Read`
  按块返回且会截断长行，累积始终未越阈。中文**压缩**路径仍缺真实上游覆盖。
- **`kiro.payload_still_oversized` 在短会话里会大量出现**（本轮 34 次）：
  `dropped_history_items=0` 且无 `history_trim` 阶段，
  成因是 `nextKiroHistoryCutPoint` 找不到干净切点（全部历史都是 tool 配对），
  守卫选择「宁可原样发也不破坏结构」。**这是刻意设计，不是故障**，
  但告警级别为 WARN 会造成噪音，是否下调待定。
- 本轮出现 2 次 502，经核查为账号 `kiro monthly request count exhausted`
  （账号池配额问题），与体积守卫无关。
- 上游真的返回 400 后的**缩减重试**路径仍未在真实环境触发。

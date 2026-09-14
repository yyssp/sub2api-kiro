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

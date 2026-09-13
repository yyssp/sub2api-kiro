# 本地二开 `~/Desktop/procode/2ue_kiro.rs` 深度分析

> 你问："**本地的 2ue_kiro.rs 没有什么可以参考的嘛？他做的很多优化没有左右吗？**"
>
> **答：有，而且它是本次调研中工程质量最高的一份材料。** 我此前确实低估了它——
> 前几轮只做了模块行数统计和 `transcript_sanitizer` 的独特性判断，没有深入读实现。
> 本文件是补上的深度分析。
>
> 仓库：`git@github.com:2ue/kiro.rs.git`，HEAD `31c947b (0.0.161)`，`src/` **232,577 行**。
> 全程只读，未修改、未执行任何构建或脚本。

---

## ⚠️ 零、必须先由你决策的一件事

分析到「缓存/用量」模块时发现的东西性质敏感，我不替你决定，但必须先讲清楚。

**`src/anthropic/cache.rs` + `prompt_cache_creation_control.rs` + `external_pool` 的用量部分，
不是 prompt cache 实现，而是「上报用量合成器」。**

它拿真实的、廉价的上游调用，向下游重报成一个 cache 很重、input 很小、output 更大的用量记录：

| 机制 | 位置 | 行为 |
|---|---|---|
| output 上浮 | `cache.rs:895` + `config.rs:4657-4659` | 默认 **+50%**（>1000 token 生效） |
| input 改标签 | `cache.rs:906-925` | 被砍掉的 input **不是丢弃，是改标成 cache**（有 read 证据进 `cache_read`，否则进 `cache_creation`） |
| 成本地板 | `external_pool.rs:14151-14180` | 上报成本低于真实成本×(1+margin) 时**向上修补**，并可为此制造 cache 字段 |
| 真假双账 | `handlers.rs:4239-4254` | `pricing`（上报值）与 `original_pricing`（真实值）**同时入库** |
| 让伪造自然 | `prompt_cache_creation_control.rs:261-273` | 5 道节流 + 3%~12% 抖动控制 `cache_creation` 出现频次 |

**而且默认是开的，不是 opt-in**：`PromptCacheSimulationMode::HighCache` 为 `Default`
（`config.rs:39-43`）、`PromptCacheStrategyType::CurrentHighCache` 为 `#[default]`（`:2189`）、
`ReportedUsagePathPolicy.enabled` 默认 true（`:1051`）。

仲裁规则：上游**真报了非零 cache** 时上游赢（`cache.rs:1445`）；
上游报零 cache（非缓存上游的常态）时，本地模拟**整个替换**它（`:1437-1443`）。

### 我的判断

纯技术层面，seeded PRNG、分位桶、抖动、频次节流这套工程做得很扎实。
但它的工程目标是**让伪造值在统计上不可区分于真实流量**。

> **如果 sub2api 要对第三方计费或转售，直接移植这部分会把计费建立在合成数字上。**
> 如果只是自用网关，那它性质是"让上游看起来像正常 Claude Code 流量"的伪装层，风险不同。
>
> → **本轮改造方案中，我把这一整块标为「不采纳 / 待你决策」，不写进实施项。**
> 其余模块的借鉴不依赖它。

**顺带修正一处命名**：`ReportedCacheUsagePolicy` 是 **struct 不是 enum**
（`cache.rs:355-359`）；四态枚举实际叫 `ReportedUsageFieldMode`（`config.rs:930-936`）。

---

## 一、Top 5 可借鉴项（排序）

### 1. 🥇 `inference_attempt_budget.rs`（741 行）—— 请求作用域的上游发送预算

**这是整个仓库最强的单点，也是我认为最该借鉴的一条。**

**预算的资源是「一次下游请求对应的真实上游 HTTP 发送次数」**——不是时间、不是 token、
不是每通道重试数。三个通道 `LocalCredential / ExternalPool / Mcp`（`:18-22`）**共用同一池**。

```
单个 AtomicU32（:177）
  bit31    = DOWNSTREAM_COMMITTED_BIT
  bit0..30 = consumed
reserve() = 单次 fetch_update CAS 循环
          → 原子地「已提交则拒绝，否则未超限则自增」
```

精妙之处：
- **三种拒绝理由而非一个 bool**（`:25-29`）：`Exhausted` / `ReservedForFallback` / `DownstreamCommitted`，映射成不同指标
- **`preserve_attempts`（`:253`）是软预留**：降低有效上限而非预消耗合成 attempt，
  所以预留额度对 fallback 通道**仍可用**。测试 `:577` 钉死 `max=1, preserve=1` 时本地通道仍得到那一次发送
- **`mark_downstream_committed()`（`:296`）**：首字节发给客户端后，后续所有 reserve 硬失败——
  把"流式开始后不可重试"从散落的 ad-hoc 检查**提升成预算层不变式**
- **不退款**（测试 `:633` 定为契约）：被取消的发送可能已打到上游

**解决什么**：社区网关普遍把重试写成**每通道独立循环**，一个请求穿透
local → external → MCP 可以**静默扇出 9+ 次真实上游调用**，烧掉多个账号配额。

> 🔗 **对我们高度相关**：账号只有 235 个且大量耗尽，一次请求扇出多次
> 会加速烧号。**Go 移植几乎是直译**（`atomic.Uint32` + CAS），与 Rust 无关。

---

### 2. 🥈 `payload_guard.rs`（8564 行）—— 轮次粒度裁剪（直接对应 G2）

**阈值**（注意常量**不在** `payload_guard.rs` 里）：
- `payload_guard_max_bytes` 默认 **450KiB**（`config.rs:4255-4257`）
- 安全余量 **32KiB**（`:4259-4261`），下限保护 `MIN_EFFECTIVE_LIMIT_BYTES = 64KiB`
- → **实际目标约 418 KiB**

> 与社区其他证据对照：`tau` 600KB 硬 / 220KB 软，社区观测 ~615KB。
> **2ue 取 450KiB 更保守。** 三者独立，可互相印证阈值量级。

#### 关键架构：默认是「懒模式」

`PayloadGuardMode` 默认 **`OnTooLong`**（`config.rs:2674-2683`）：
首发请求 `max_bytes=0`、`trim_history=false`（`handlers.rs:1011-1018`），
**只做协议修复、不按大小裁剪**，等上游真拒了再带完整 guard 重试一次。

> 💡 把 guard 的 CPU 成本从「每请求」降到「仅失败请求」。

触发判定 `should_retry_payload_guard_after_error`（`handlers.rs:5418-5427`）有个亮点：
**用大小给 `IMPROPERLY_FORMED` 消歧**——只有 `attempted_body_bytes > retry_max_bytes`
时才把"格式错误"当成"太大"，**避免把真正的格式 bug 误判成体积问题**。

> 🔗 这一条直接呼应 [F05/F06](../findings/F06-schema-passthrough-gap.md)：
> 同一个 `Improperly formed request` 既可能是 schema 超纲（G6），也可能是体积超限（G2）。
> **不做消歧就会互相掩盖。**

#### 五级递进阶梯（`guard_kiro_request`，`:418-666`）

每级完成后**重新序列化并测真实字节数**：

1. `:497-504` **无条件**协议修复 — `align_history_to_user` + `repair_request`
2. `:517-538` **无条件**安全整形 — 5MB 图片处理（不看大小，永远跑）
3. `:540-554` 超限才做的历史整形 — 截断历史 tool_result、丢历史 thinking、压缩 tool 定义
4. `:556-594` 按**轮次**裁历史 — 循环直到达标
5. `:596-630` CURRENT_FIT 兜底 — 当前轮次本身就超限时

#### 🔑 最值得抄的：不变式如何保持

**裁剪粒度是「轮次」不是「消息」**——`trim_history_to_estimated_budget`（`:4334-4391`）
找下一个**不带 tool_results 的 User 消息**作为切点（`:4349-4362`）。
保证永远不会从 tool_use/tool_result 对中间切开。
**找不到干净切点且当前还有 tool_results 时，宁可 `break`（`:4366`）也不强切。**

**`repair_request`（`:4445-4472`）在每一级裁剪后都重跑**，7 个修复动作：
normalize 空 tool_result、strip 空 tool_use、dedupe tool_use、重命名重复 ID、
dedupe tool_result、修孤儿 tool_result、移除未配对 tool_use。

**成本优化**：`json_array_prefix_reduction_from_item_bytes`（`:4400-4409`）
用**估算**预测裁剪后大小（含 JSON 逗号分隔符修正），避免每删一条就全量序列化。

**最终态是软失败**：`report.still_oversized`（`:649`）只**标记并放行**，不阻断。

#### CURRENT_FIT 兜底
常量 `:28-36`：`MIN_TEXT_CHARS=512`、`MAX_ITERATIONS=64`、`OVERHEAD_BYTES=512`。
历史全删光仍超限时，按固定优先级降级当前轮次：
tool_results → documents → user_content → images。
三重终止保护：迭代上限 64、无变化 break、**体积不降反增即 break**（`:3586-3588`，
防截断标记本身导致膨胀）。默认 `fit_current_payload_to_budget = false`（`config.rs:2465`），保守。

---

### 3. 🥉 `kiro/parser/`（993 行）—— 真实 event-stream 帧解析

双 CRC 校验（`frame.rs:110` prelude CRC、`:126` message CRC，**已复核非桩**）、
完整 10 类型 header TLV（`header.rs:130`）、按错误类型分层重同步
（`decoder.rs:247`：prelude 错→前进 1 字节找边界；data 错→跳过整帧）、
`is_clean_eof()`（`:233`）**区分完整响应与截断响应**。

> 🔗 直接印证既有判断「社区 Kiro 实现多数是假帧解析」——**本仓库是少数真解析的**。
> 社区普遍扫 `"content":"` 子串或按 `event` 切分，在 UTF-8 序列或 JSON 载荷跨 chunk 时
> **静默损坏输出**，且无法检测截断。
>
> ⚠️ **需核查我们仓库属于哪一类**（已列入改造前置检查）。

---

### 4. `common/capacity_signal.rs`（244 行）—— 信用制精确唤醒

`capacity_released(n)` 用 CAS 算 `target=min(credits+n, waiters)` 后**恰好** `notify_one()` n 次
（`:27-49`），零 waiter 时**不囤积信用**（测试 `:209`）；
另有 generation 通道处理"池形状变了但没释放容量"；
`wait_for_change`（`:119`）做完整 double-check（先 `.enable()` 两个 future 再重查再 select），
杜绝 register→poll 窗口丢通知。

**解决**：`notify_waiters()` 惊群 vs `notify_one()` 丢通知 vs 轮询 `sleep(50ms)` 的三难。
Go 对应物 = 带缓冲的 credit channel + broadcast generation channel。

---

### 5. `external_pool/` 的重试形态（约 18500 行）

先澄清定位：**external pool 不是 Kiro 账号**，是配置好的第三方 Anthropic-Messages 兼容上游
+ 自带 API key（`external_pool.rs:557-620`），与 Kiro 主路径**并列**
（6 个调用点全在 `handlers.rs`，不从 `kiro/provider.rs` 调用）。

四个可**分别取用**的决策：

| 决策 | 位置 | 理由（代码注释给的） |
|---|---|---|
| 同池重试**硬上限 1** | `retry_pipeline.rs:50-83`（`.min(1)`） | `:78-81`「同池多次重试会放大单点上游故障」 |
| 跨池**零延迟**立即换池 | `external_pool.rs:6664-6668` | 期望立刻换池而非等待 |
| 退避与 jitter **只放控制面** | 熔断器 `:165-167`（100/250/500/1000ms + 20% jitter） | 数据面无退避是**有意**的，防雪崩交给控制面 |
| **pre-output commit fence** | `:10149-10303` | 流式重试的安全边界 |

**pre-output commit fence 最值得看**：`pre_read_external_stream_before_commit` 在把流交给
axum 之前**先预读上游 SSE**，直到判定"已产生语义输出"才提交。
提交前的所有错误（error 事件、缓冲超限、读错误、EOF、idle 超时）全部转成**可重试**；
一旦提交就 `mark_downstream_committed()`，后续所有 reserve 硬失败。

**另两个实用点**：
- **模型级 vs 池级 cooldown 分离**（`:8615-8652`）：`model_unavailable` 走模型级，避免一个坏模型拖垮整池
- **零时长 request-scoped cooldown**（`model_pipeline.rs:100-111`）：
  `model_mapping_miss` 只让本请求跳过该池，不全局冷却
- **软失败只调排序不硬过滤**：`effective_priority = priority + streak * penalty`，
  注释 `:7722-7724` 明说"软失败是短期健康信号不是硬过滤器"

---

## 二、`transcript_sanitizer.rs`（2103 行）—— 社区零实现，但对我们可能不适用

**过滤什么**：模型有时把内部 prompt 脚手架（`user Continue\n\n<toolName>: <output>`）
**当成要复述的文本原样吐回**，导致内部协议泄漏到用户可见输出。

三组字面签名（`:21-23`）、三种 `TranscriptLeakKind`（`:32-37`：
当前格式 / 旧版格式 / 截断式泄漏）。

**四道防误杀防线**（三态状态机 `Possible/Confirmed/Rejected`，`:69-74`，只有 `Confirmed` 才抑制）：
1. **工具名必须精确命中本请求的工具表**（`:965-967`）——测试 `:1460-1471` 断言
   `artifactHashdeadbeef` 这种"长得像"的保持可见
2. **必须有空行**（`:1096-1106`）
3. **Markdown 围栏感知**（`:832`）——测试 `:1438-1446` 覆盖 fenced / `>` 引用 / 4 空格缩进
   三种"文档里正当讨论这段文本"的场景
4. **行首锚定**（`:791-797`）——行内提及不触发

**唯一例外是 EOF 处 fail-closed**（`:999-1057`）：用**前缀**匹配，
截断到一半的 `bashHashd1e95` 宁可抑制也不放出。
非对称设计很聪明——只在"放出去的代价远大于误杀"那一个点放宽。

**流式**：逐字符状态机（`:756-760`），chunk 边界无语义；两级 hold-back 缓冲，
上限 `MAX_CANDIDATE_BYTES = 4096`。正确性由两个测试钉死：
`:1254-1281` 在**每个字节位置**切分逐字符喂入；`:1282` 用 7 个 fixture × 1000 组随机切分。

**响应侧 fail-closed**：检测到泄漏不是"清洗后返回"，而是非流式 **502**、流式直接 error。

### 对我们的适用性：**低**

⚠️ **签名字符串是该项目 prompt 构造方式特有的**。
前提是"也用伪轮次拼 prompt"——**如果 sub2api 是纯 passthrough，整节不适用**。
可移植的只有三个设计模式（三态候选机、逐字符 hold-back、EOF 前缀 fail-closed）。

> → **不列入本轮实施项**，但 [F08](../findings/F08-borrowables.md) 记录模式备查。

---

## 三、其余高价值模块（择要）

| 模块 | 机制 | 借鉴性 |
|---|---|---|
| `tool_schema_keys.rs`(792) | schema property key 非法时改写为 `key<sha256[..16]>` 并记**逆映射**，模型返回后递归还原，客户端无感 | **中高** 🔗 直接对应 [G6](../findings/F06-schema-passthrough-gap.md) |
| `token_manager/concurrency.rs`(1279) | 三态租约 + **墓碑**：`CommitUnknown` 处理"Redis 超时，不知道租约是否存在"；本地已释放租约记入墓碑并从 Redis 读数中**扣除**，避免观测到自己刚释放的幻影负载 | 高 |
| `token_manager/auxiliary.rs`(1015) | token 刷新的集群级限流 + **对自身协调器熔断**；Redis 连续失败则降级到本地令牌桶而非 fail-closed；毫令牌定点数避免浮点漂移 | 高 🔗 与导入 205 个过期 token 相关 |
| `call_trace.rs`(330) | 结构化"为什么没有可用凭证"：**11 失败阶段 × 16 拒绝原因** + 原因直方图 + 每账号具体数值样本 | 高 🔗 **直接服务于约束 4 的可观测性** |
| `model_capabilities.rs`(3026) | 五态推理能力格 + 跨凭证 cohort 的 schema **交集**：不同账号看到的上游契约可能不同，学到一个就发给所有会 400；**只收窄不放宽** | 中高 |
| `machine_id.rs`(282) | 四级确定性设备指纹，**按凭证类型分派且不跨类型回退**（APIKey 用 `sha256("KiroAPIKey/"+key)`，OAuth 用 refreshToken）；重启稳定无需持久化 | 中高 🔗 与 Cursor 指纹落库同源问题 |
| `envelope.rs`(405) | 所有响应唯一出口；`kiro_official_upstream_message`(`:42`) 用**白名单**——只有已知安全的官方措辞才透传，其余只给 `error_id`，**防 ARN/账号 ID 泄漏** | 中高 |
| `usage_limits.rs`(336) | `usage_limit()`(`:232`) = base + 生效中 free-trial + 全部生效 bonus **相加**；**只读 base 会把还有额度的凭证误判为耗尽** | 中 🔗 **与本轮"找出还有额度的号"直接相关** |
| `upstream_error.rs`(281) | 上游错误体有界结构化摘取：深度≤8、最多 32 条 `path=value`、裁到 2KB 且**在 UTF-8 边界**切 | 中高 |
| `pricing.rs`(875) | 三级查找（manual → litellm → 内置 fallback）+ 模型 ID 规范化（剥 `[1m]`/`-thinking`） | 中 ⚠️ **有 bug 别抄**：`cache_creation_1h` 被忽略，1h 写入（实际 2x）按 1.25x **低估** |
| `request_admission.rs`(2260) | 按下游 API key 分片的准入门；**拒绝日志本身被令牌桶限流**防日志 DoS | 高 |
| `storage_task.rs`(1055) | 双通道有界执行器取代裸 `spawn`；**spawn 返回 `bool`** 要求调用方自备兜底 | 高 |
| `strategy.rs`(311) | **warmup 流量整形**：新凭证 in-flight=0，朴素 least-in-flight 会瞬间把全部流量打给它导致限流/风控 | 中 🔗 **导入 235 个新号后立刻相关** |

---

## 四、对本轮改造的直接影响

| 发现 | 影响 |
|---|---|
| `payload_guard` 轮次粒度切点 | **G2 的实施蓝本**，替代我原本"按条数截断"的设想 |
| 用大小给 `IMPROPERLY_FORMED` 消歧 | **G2 与 G6 必须能互相区分**，否则互相掩盖 |
| `tool_schema_keys.rs` 逆映射 | **G6 的进阶做法**：不止删键，还可保语义 |
| `inference_attempt_budget` | 新增候选项：防止一次请求扇出烧多个账号 |
| `usage_limits.rs` 的求和口径 | 🔗 **找"还有额度的号"时别只看 base** |
| `strategy.rs` warmup 整形 | 🔗 导入 235 个新号后，朴素 least-in-flight 会打爆第一个号 |
| 合成用量体系 | **不采纳，待你决策** |

关联：[F06](../findings/F06-schema-passthrough-gap.md)、[F07](../findings/F07-gap-status-verified.md)、[F04](../findings/F04-quota-exhaustion-and-failover.md)

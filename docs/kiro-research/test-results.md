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
`token_refresh` 重试 3 次后 `retry_exhausted`，但账号 **仍是 `status=active, schedulable=true`** ——
会被正常调度到，每次都失败。详见 §五 存疑清单 Q1。

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
| G8 | 归类器 400 特征串 | ✅ 已修复 | A + B |
| G6 | 工具 schema 白名单 | ✅ 已修复 | A |
| G5 | 工具名字符集清洗 | ✅ 已修复 | A（依据 AWS 官方 pattern） |
| G2 | 请求体积守卫 | ⚠️ 算法正确，**阈值未验证** | A only |
| G4 | cache token 口径 | ✅ **维持现状**（实测定夺） | **C 层实测** |

---

## 五、存疑清单（必须保持怀疑的部分）

### Q1 · 🔴 失效凭据的账号仍可被调度（真实缺陷，未修复）

账号 673、725 的 refresh token 返回 401 `Bad credentials`，
`token_refresh.retry_exhausted` 已记录，但账号仍 `status=active, schedulable=true`。

**影响**：这类死号会被反复调度到，每次都失败，消耗 failover 次数。
**为何本轮没修**：超出本次改造方案（G2/G4/G5/G6/G8）范围，属于新发现的独立问题。
**建议**：`retry_exhausted` 时应将账号置为不可调度或标记异常状态。

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

### Q5 · ⚠️ G5/G6 的修复未经真实上游反证

G5（工具名字符集）、G6（schema 白名单）的修复依据是
AWS 官方 Smithy 约束 + 社区实现，A 层测试也有牙齿（G5 反证 6/10 失败）。
但**本轮没有构造一个「带非法工具名的真实请求」去撞真实上游的 400**
—— 也就是说，我们**没有真实证明**这些修复确实避免了 400。

理由：构造这类请求需要消耗额度，且失败请求本身也可能计费。
**这些修复目前仍属「依据官方约束的推断」，而非「实测验证」。**

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

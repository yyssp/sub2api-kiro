# Kiro 改造 · 测试策略（额度分层）

> **本文件的唯一目的：把真实额度消耗压到最小。**
> 你的账号是 KIRO FREE 个人号、大部分已耗尽、只能用 4.5 系列、上下文 200k。
> 所以每一条"要不要打真实上游"都必须先在这里论证过。
>
> **铁律：一条用例只要能在 A 层或 B 层证明，就绝不允许进 C 层。**

---

## 零、分层定义

| 层 | 手段 | 额度 | 判定标准 |
|---|---|---|---|
| **A** | 纯单测（Go table-driven） | 0 | 纯函数/纯数据变换，输入输出完全确定 |
| **B** | mock 上游（`httptest.NewServer`） | 0 | 依赖 HTTP 交互，但**上游行为已知**，可以伪造 |
| **C** | **真实 Kiro 上游** | 有 | **上游真实行为未知**，伪造就是自欺 |

**C 层准入的唯一理由**：我们不知道上游会返回什么，mock 只会把我的假设验证一遍
（拿自己的假设当断言 = 什么都没验证）。

### 现有测试底座（可直接复用，不必新建脚手架）
`[本仓库]` 已有 **19 个** Kiro service 层测试 + **12 个** `pkg/kiro` 测试：
- 协议转换：`internal/pkg/kiro/translator_test.go`
- 导入解析：`internal/pkg/kiro/kirors_test.go`
- 错误归类：`internal/service/kiro_error_classifier_test.go`
- 故障转移/状态：`internal/service/kiro_runtime_state_test.go`、`kiro_runtime_state_integration_test.go`
- mock 上游范式：`httptest.NewServer`，仓库内已大量使用

---

## 一、A 层 · 纯单测（零额度）

| ID | 用例 | 落点文件 | 对应缺口 |
|---|---|---|---|
| A-1 | 工具名含 `$`/`.`/空格/中文 → 清洗为 `[a-zA-Z0-9_-]` | `translator_test.go` | **G5** |
| A-2 | 工具名含**连字符** → **原样保留**（官方 pattern 允许） | `translator_test.go` | **G5** |
| A-3 | 清洗后必须**单射**：`a.b` 与 `a-b` 不得撞成同一名字 | `translator_test.go` | **G5** |
| A-4 | 超 63 字符 → 截断 + sha256 后 8 位，且仍匹配官方 pattern | `translator_test.go` | G5 |
| A-5 | tools 定义与 history 里的 toolUse **用同一清洗函数**，名称可对上 | `translator_test.go` | **G5** |
| A-6 | `ToolNameMap` 反向还原：清洗名 → 原始名，响应侧不串工具 | `translator_test.go` | G5 |
| A-7 | 导入解析：`social`/`idc`/`api_key` 三种 authMethod 各自最小字段集 | `kirors_test.go` | 导入 |
| A-8 | 导入解析：单条非法 → 进 `Skipped`，**其余条目正常产出** | `kirors_test.go` | 导入 |
| A-9 | `expiresAt` 归一化：ISO / 秒 / 毫秒 / 数字字符串 | `kirors_test.go` | 导入 |
| A-10 | `normalizeOrigin` 输出恒为官方枚举合法值（`CLI`/`AI_EDITOR`） | `translator_test.go` | D1 |
| A-11 | usage 降级公式：`uncachedInputTokens` 缺失时的回退计算 | `kiro_unit_helpers_test.go` | G4 |

> A 层是本轮的主战场。**G5 能被 A 层 100% 覆盖**——
> 因为官方 service-2 已经给出确定的 pattern，不需要问上游。

---

## 二、B 层 · mock 上游（零额度）

| ID | 用例 | 落点 | 对应缺口 |
|---|---|---|---|
| B-1 | 402 + `MONTHLY_REQUEST_COUNT` → 冷却到下月 1 号 + 故障转移 | `kiro_runtime_state_test.go` | F04 |
| B-2 | 402 **不含** 该标记 → 转移但不冷却 | 同上 | F04 |
| B-3 | **未归类 400** → 确认当前**不转移**（G1 缺口的回归锚点） | 同上 | **G1** |
| B-4 | 修复后：工具名类 400 → 应转移 or 应在入口就被拦住 | 同上 | G1+G5 |
| B-5 | 403 + suspended body → `markKiroSuspended` | 同上 | — |
| B-6 | 401 → 刷新 token 后重试成功 | `kiro_token_provider_test.go` | 导入/刷新 |
| B-7 | 多账号池：首个 402 → 自动切到下一个健康账号 | `kiro_runtime_state_integration_test.go` | **约束 4** |
| B-8 | metadataEvent 携带 `tokenUsage` 四种拼写 → 都能解析 | `kiro_alignment_test.go` | G4 |
| B-9 | SSE 帧序：event-stream 二进制帧解析（非字符串扫描） | `websearch_stream_test.go` | — |

> **B-7 是约束 4 的主力验证**。用 mock 可以精确构造
> "3 个耗尽 + 1 个健康"的账号池，确定性地断言最终落到健康账号上——
> 这比真实上游更可靠，因为真实账号的健康状态我们无法预设。

---

## 三、C 层 · 真实 Kiro 上游（消耗额度）

### 准入论证
只有下列 4 类问题是 mock **原理上**无法回答的：

| ID | 用例 | 为什么必须真实 | 额度代价 |
|---|---|---|---|
| **C-1** | 打**已耗尽**账号，抓取真实状态码 + 响应体 | 我们不知道 FREE 个人号耗尽时返回 402 还是别的。mock 只能验证我的猜测 | **0**（号已废） |
| **C-2** | 用**已耗尽**账号请求 opus，观察归类与冷却范围 | 同上，且要确认不会误伤健康号 | **0**（号已废） |
| **C-3** | 用**健康**账号发一次最小请求，抓取 `metadataEvent` 真实结构 | **V7**：`tokenUsage` 不在公开 SDK，只能抓包 | **1 次最小请求** |
| **C-4** | 连续请求，验证调度收敛到健康账号 | 端到端，验证冷却真的生效 | **少量** |

### 明确排除出 C 层的（省额度）
| 问题 | 为什么不打真实上游 |
|---|---|
| G5 工具名字符集 | 官方 service-2 已给出确定 pattern，A 层足够 |
| G2 请求体上限 ~615KB | **200k 上下文可能堆不出这个体积**，本轮结构性无法验证 → 标「未验证」 |
| V1 整体 body 上限 | 同上 |
| 各类错误码转移逻辑 | B 层 mock 完全可控且更精确 |

### C 层执行纪律
1. **先跑完 A + B 全绿**，再碰真实账号。带着已知 bug 打上游是浪费额度。
2. **C-1/C-2 优先**——用废号验证，零有效额度代价，却验证最关键的调度行为。
3. **C-3 只发一次**，`max_tokens` 设到最小，只为抓 metadata 结构。
4. 模型固定 **haiku**（最省），仅在必须区分模型行为时才用 sonnet。**绝不主动请求 opus**
   （除 C-2 这条专门验证 opus 拒绝行为的，且必须用废号）。
5. **全程抓取原始响应**存档，避免重复打同一个问题。

---

## 四、结构性无法验证清单（提前声明）

> 这是你约束 2 的落地：**现在就说清楚哪些本轮注定测不了**，
> 而不是最后拿"测试通过"糊弄过去。

| 项 | 为什么本轮测不了 | 最终应标记 |
|---|---|---|
| G2 / V1 请求体上限 | 200k 上下文可能达不到 ~615KB 阈值 | **未验证** |
| opus 相关的一切行为 | 账号不支持 opus | **未验证** |
| 长上下文的缓存策略表现 | 200k 封顶，大负载场景测不出（见既往教训：小负载会让缓存结论完全失真） | **未验证** |
| 付费档账号的差异化行为 | 全是 KIRO FREE | **未验证** |
| 月度重置时刻的正确性 | 需跨月才能观测 | **逻辑验证**（A 层验公式），非端到端 |

**特别提醒**：即便 C 层某条用例"跑通了"，也只能证明
**"在 haiku + 200k + FREE 号这一组合下没暴露问题"**，
不能证明问题在所有场景下都已解决。最终 `test-results.md` 必须逐条写明这个边界。

---

## 五、执行顺序

```
A 层全绿  →  B 层全绿  →  导入 235 账号  →  C-1/C-2（废号，零代价）
                                              →  C-3（1 次最小请求）
                                              →  C-4（少量）
                                              →  test-results.md 汇总 + 存疑清单
```

关联：[PLAN-AND-STATUS](PLAN-AND-STATUS.md)、[F04](findings/F04-quota-exhaustion-and-failover.md)

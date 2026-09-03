# 分组缓存策略改造设计

## 1. 文档定位

本文是本次改造落地后的领域设计与运行时约束。它把当前代码的真实状态、`2ue_kiro.rs` 中可复用的通用缓存策略、当前系统的改造边界、每个配置的运行时效果、协议 usage 口径、页面和后端结构以及异常场景固定下来。

本系统是新系统，尚未上线。因此最终实现直接使用目标 schema，不设计旧数据迁移、历史字段回填、线上双写和灰度兼容。

本次只学习参考项目的缓存策略能力：

- 稳定前缀和连续 fingerprint。
- 缓存 read/creation/TTL 的计算。
- raw usage、cache usage、reported usage 分层。
- token scale、上限、采样抖动和小请求保护。
- creation 频控、增量累计、窗口预算。
- 成功后写入、失败不写、边界清理。

本次不引入参考项目的以下语义：

- 路径绑定、路径前缀匹配、`path_overrides`。
- Kiro endpoint、Kiro 账号路径、Kiro sticky routing。
- `KiroRsToolCachePolicy` 或任何 Kiro 专属策略类型。

## 2. 目标与难度判断

### 2.1 目标

目标是把当前 group 上的 Kiro 缓存字段改造成一个协议中立的独立策略资源：

```text
CacheStrategy
    ├── 可复用的完整配置
    ├── revision
    └── 绑定多个 group

Group
    └── 只保存 cache_strategy_id

Request
    -> group/account/session snapshot
    -> protocol adapter
    -> CacheProfile
    -> Prepare (只读)
    -> upstream
    -> Commit 或 Abort
    -> protocol-specific usage response
```

Kiro 是 Claude Code-compatible 入口之一。Anthropic Messages、OpenAI Responses、OpenAI Chat Completions 和后续兼容入口都走同一套策略模型，只由 adapter 负责请求结构和 usage 字段差异。

### 2.2 难度

这是中等偏高的横向重构，不是单点 UI 增加：

1. profile/hash/TTL 的算法雏形已经存在，可提炼复用。
2. schema、策略服务、group/auth snapshot、多个请求入口、streaming 和 usage merge 必须同时收口。
3. 当前最危险的不是表单，而是 scope 隔离、首轮 read、creation 控制与实际写入不一致、真实上游 usage 双计数以及流式终态。
4. 不需要迁移降低了数据库风险，但不能因此跳过旧 Kiro 字段的最终删除和测试替换。

结论：已按本文冻结的语义落地，并用真实网关调度和协议矩阵验证；未纳入本轮的 Redis L2 状态复制仍明确作为后续独立能力。

## 3. 当前系统真实状态

### 3.1 已落地的实现边界

当前实现按以下职责组织：

- `backend/internal/service/cache_strategy.go`：策略配置、归一化、registry 和 CRUD service。
- `backend/internal/service/cache_policy_runtime.go`：group 绑定解析和缓存 key。
- `backend/internal/service/cache_runtime.go`：Anthropic、Responses、Chat Completions 的 profile、hash、tracker、usage projection 和 plan。
- `backend/internal/repository/cache_strategy_repo.go`、`backend/internal/handler/admin/cache_strategy_handler.go`：策略资源存储和管理 API。
- `frontend/src/views/admin/CacheStrategiesView.vue`、`cacheStrategyTemplates.ts`：策略 tab、模板和绑定页面。
- `openspec/changes/add-group-cache-strategy/`：最终设计、实施和验证记录。

这些文件构成当前实现的职责边界；运行时行为以本文约束和 `verification.md` 中的真实调度终值为准。

### 3.2 当前绑定和旧字段

最终 group 只保留：

```text
cache_strategy_id
```

旧的 `kiro_cache_emulation_*` 字段已从 Ent schema、DTO、mapper、handler、snapshot 和测试 fixture 中移除，并由 `231_drop_legacy_kiro_cache_emulation.sql` 从 PostgreSQL `groups` 表删除。`kiro_auto_sticky_enabled`、`kiro_sticky_session_ttl_seconds` 和 `kiro_endpoint_mode` 仍只承担会话路由/endpoint 职责，不属于缓存策略。

### 3.3 当前运行时的关键问题

| 位置 | 当前行为 | 设计风险 | 目标修复 |
| --- | --- | --- | --- |
| `cacheStrategyCacheKey` | 绑定 group、账号、策略 revision、协议、模型和 session | scope 维度必须完整，否则会跨会话误命中 | 当前实现将上述维度全部纳入 key |
| `effectiveCacheStrategyConfig` | 只从 group 的 `cache_strategy_id` 解析策略 | 未绑定或 disabled 时必须完全关闭本地缓存 | 当前主路径无旧 Kiro 缓存回退 |
| `applyCacheStrategyToProfile` | coverage 和 reported cap 同时影响 profile 与 usage | 只裁剪 usage 会产生隐藏状态 | 当前实现限制 profile breakpoint 和最终 usage 总量 |
| `prepareCacheEmulationPlanFromProfile` | creation control 同时作用于 usage 和提交的 breakpoint | usage 与 tracker 状态必须一致 | 当前实现只提交被允许的完整断点 |
| `IncrementalCreateEnabled` | 命中后可以把 creation 置零，但 commit 仍遍历完整 profile | 隐藏创建了未报告的缓存 | `WriteEntries` 只包含允许的增量断点 |
| `CacheUsagePolicy` | 只有部分字段，缺少参考项目的 final jitter、upstream preserve 等明确开关 | 页面无法表达完整策略，字段语义混乱 | 将报告投影字段拆成完整、可验证的策略组 |
| `CacheStrategyRegistry` | registry 保存归一化策略快照 | 热路径不能引用可变表单 map | 当前实现通过归一化配置和 registry 快照读取 |
| `CacheStrategyService.Update` | 更新带 expected revision | 并发管理员不能静默覆盖 | 当前实现返回 revision conflict |
| 流式入口 | 多处协议适配器汇总 usage | start、done、error 时序必须一致 | 当前实现只在合法终态提交 |
| web search/Kiro runtime | 动态内容可能进入请求 profile | 动态结果不能污染稳定前缀 | 当前策略默认排除动态内容；Kiro 入口复用通用缓存运行时 |

### 3.4 当前配置字段漂移

Go 配置、API DTO、前端模板和运行时已统一使用同一字段口径。`cap_jitter_min_tokens`、`cap_jitter_max_tokens`、`preserve_upstream_cache_usage`、creation control 和四组 usage projection 均经过 normalize、runtime 和协议输出验证；不适用字段在 disabled 策略中清零。

## 4. 参考策略拆解

### 4.1 `PromptCacheTracker` 做了什么

参考项目的 tracker 不是把整个 request body 当成一个 key，而是：

1. 将请求拆成有顺序的 block。
2. 对每个 block 做 canonical JSON。
3. 用前一个 fingerprint 和当前 block hash 形成连续 SHA-256 链。
4. 在可缓存断点记录累计 token、TTL、过期时间和最近使用时间。
5. lookup 从最深断点向前找，命中一个连续前缀后停止。
6. 成功后把当前 profile 的断点 upsert 到 tracker。
7. 只保存 fingerprint 和计量信息，不保存完整 prompt。

这套设计必须保留，因为它能处理“历史前缀相同、当前尾部增长”的 Claude Code 会话；简单的完整 body hash 无法得到部分命中。

### 4.2 canonicalization 规则

需要保留的规则：

- object key 排序。
- 递归去除 `cache_control`。
- 位置字段、request id、message id、随机 tool use id 等不稳定字段归一或剔除。
- model、tool choice、response format 等会改变上游语义的字段不能静默删除，应进入 scope 或稳定 prelude。
- billing header、trace metadata 和内部诊断字段不得污染缓存前缀。
- 图片和二进制只用 media type、尺寸、内容 hash 和 token estimate 参与 fingerprint，不存 base64 原文。

### 4.3 最小可缓存阈值

参考项目按模型能力设置最小缓存长度：

- 普通模型：1024。
- Haiku 3.5：2048。
- 其他 Haiku：4096。
- Opus 4.5 至 4.7：4096。

当前项目不能把模型名判断写死在 Kiro 函数中。目标做法：

1. 首先从当前模型能力注册表读取 `min_cacheable_tokens`。
2. 策略中的 `model_min_cacheable_overrides` 再做显式覆盖。
3. 能力未知时使用安全默认值，不因为未知模型直接生成超大缓存。
4. 阈值只决定断点是否有资格写入/命中，不修改原始 token 估算。

### 4.4 TTL、bound 和重启

参考策略支持：

- 默认 ephemeral 5 分钟。
- 长 TTL 1 小时。
- 每 scope entry 上限。
- 全局 entry 上限。
- 估算字节上限。
- 命中刷新 TTL 和 `last_used_at`。
- 过期清理和 LRU 淘汰。
- tracker 在进程内存中，重启后冷启动。

当前系统目标先保持同样的语义。Redis L2 可以在实现阶段加入，但必须保证 Redis key 与 scope/revision 完整绑定；Redis 不可用时应明确降级为无本地 cache state，而不是跨 scope 猜测命中。

### 4.5 usage 分层

参考项目明确分三层：

```text
RawUsage
  -> CacheUsage
      -> ReportedUsage
```

- `RawUsage`：上游真实字段或请求/上下文估算，保留诊断和对账口径。
- `CacheUsage`：内部统一表示，含 total、uncached input、cache read、cache creation、5m/1h。
- `ReportedUsage`：按照下游协议字段、策略 ratio 和 final guard 输出的值。

重要规则：

- 上游已有非零 cache read/write 时，真实上游字段优先，不能再叠加本地字段。
- 上游没有 cache 字段时，才可使用本地 tracker 证据补足。
- 被 usage 策略隐藏的 creation 不能凭空消失；若没有允许移动到缓存字段，就必须回到 uncached input。
- 最后重新校验总量、分项和 TTL breakdown。

### 4.6 creation control

参考控制器的状态包括：

- 最近一次 creation 时间。
- 两次 creation 之间的成功请求数。
- 被抑制但尚未达到增量阈值的 pending token。
- 时间窗口内已经允许的 creation token。
- 空闲状态过期时间。

参考实现只在成功响应后推进控制器状态。当前系统还要补一个关键约束：允许上报多少 creation，就只能写入多少新的 cache entry，否则会出现“usage 说没有创建，tracker 却已经可以 read”的隐性缓存。

## 5. 目标策略模型

### 5.1 策略类型

| `kind` | 语义 | profile | read | creation | 典型用途 |
| --- | --- | --- | --- | --- | --- |
| `disabled` | 完全关闭本地 cache shaping | 不构建或不使用 | 否 | 否 | 对照、排障、动态内容 |
| `prefix` | 稳定前缀缓存 | system/tools/history/稳定边界 | 是 | 是 | 通用兼容请求 |
| `tool_aware` | 按工具、历史、tool result 和当前用户稳定前缀细分 | 比 prefix 更严格 | 是 | 是 | Claude Code 工具密集长会话 |

`tool_aware` 只表示 block 选择更细，不代表某个平台，不代表 Kiro。

### 5.2 完整配置

以下是最终目标配置。仍可使用一个 JSONB 字段保存，但必须由强类型 Go DTO 归一化，前端类型与其一一对应。

#### 基础与 ratio

| 字段 | 默认 | 作用 | 约束 |
| --- | ---: | --- | --- |
| `enabled` | `true` | 策略是否参与运行时 | false 时不读、不写、不生成本地 cache usage |
| `kind` | `prefix` | 策略类型 | `disabled/prefix/tool_aware` |
| `ratio_mode` | `uniform` | read/creation ratio 是否共用 | `uniform/independent` |
| `coverage_ratio` | `0.85` | 目标 profile 最多覆盖的实际输入比例 | `0..1` |
| `usage_ratio` | `1` | uniform 模式下 read/creation 的报告比例 | `0..1` |
| `read_ratio` | `1` | independent 模式 read 报告比例 | `0..1` |
| `creation_ratio` | `1` | independent 模式 creation 报告比例 | `0..1` |

`coverage_ratio` 先决定实际 cache target 和可写 entry；`usage_ratio/read_ratio/creation_ratio` 只决定返回 usage。不能用 ratio 裁剪返回值后仍把全部隐藏状态写入。

#### profile 内容与断点

| 字段 | 默认 | 作用 |
| --- | ---: | --- |
| `cache_system` | true | 是否纳入 system/instructions |
| `cache_tools` | true | 是否纳入 tools/functions/schema |
| `cache_history` | true | 是否纳入已结束历史消息 |
| `cache_tool_results` | true | 是否纳入已稳定的 tool result |
| `cache_current_user_stable_prefix` | false | 是否允许当前 user 的稳定前缀成为断点 |
| `current_user_stable_prefix_max_tokens` | 0 | 当前 user 稳定前缀上限，0 表示关闭 |
| `breakpoint_mode` | `hybrid` | `client_only/auto/hybrid` |
| `allow_derived_session` | false | 没有显式 session 时是否允许安全派生 session |
| `dynamic_content_mode` | `exclude` | 搜索结果、时间、随机内容默认不写入稳定 state |

断点规则：

- `client_only` 只接受显式 `cache_control`。
- `auto` 在适配器确认稳定的消息/输入项结束时生成断点。
- `hybrid` 优先客户端标记，缺失时只对安全稳定边界自动补点。
- 当前请求最后动态 user 尾巴默认不写入。
- `cache_current_user_stable_prefix` 只有显式开启且未超过 max 才生效。

#### 覆盖、阈值和创建

| 字段 | 默认 | 作用 |
| --- | ---: | --- |
| `min_cacheable_tokens` | 1024 | 通用最小断点 token |
| `model_min_cacheable_overrides` | `{}` | 模型/模型族阈值覆盖 |
| `max_coverage_tokens` | 0 | 实际 cache target 上限，0 不限 |
| `max_new_creation_tokens_per_request` | 0 | 单次允许新增和写入的 creation 上限 |
| `incremental_create_enabled` | true | 命中旧前缀后是否允许只写新增尾部 |
| `prefix_lookback_limit` | 10 | 最深 lookup 向前查找的最大断点数 |

上限不足以覆盖一个完整 block 时必须退回前一个完整断点，不能切断 block 后伪造半个缓存 entry。

#### 对外 token projection

| 字段 | 默认 | 作用 |
| --- | ---: | --- |
| `reported_input_min_tokens` | 0 | 对外 uncached input 下限 |
| `reported_input_max_tokens` | 0 | 对外 uncached input 上限 |
| `token_scale` | 1 | 仅对对外 total 的估算补偿倍率 |
| `scale_min_input_tokens` | 20000 | 达到该值才允许 scale |
| `max_simulated_input_tokens` | 0 | 对外 total 上限，0 表示交给模型能力上限 |
| `cap_jitter_min_tokens` | 0 | 触碰上限时 deterministic 扣减下限 |
| `cap_jitter_max_tokens` | 0 | 触碰上限时 deterministic 扣减上限 |
| `preserve_upstream_cache_usage` | true | 上游有权威 cache 字段时是否优先保留 |

`token_scale` 只影响 `ReportedUsage`，不能改变原始请求、fingerprint、scope 或实际写入 entry。对外 total 必须满足：

```text
reported_total <= model_context_window
reported_total <= max_simulated_input_tokens (如果设置)
reported_total >= reported_input_min_tokens (如果可行)
```

若模型能力未知，不启用 scale，不凭空生成超过实际估算的超大上下文。绝对不得出现超过模型上下文能力或策略上限的 usage。

#### TTL 和状态 bound

| 字段 | 默认 | 作用 |
| --- | ---: | --- |
| `default_ttl_seconds` | 300 | 默认 5m/ephemeral TTL |
| `hour_ttl_seconds` | 3600 | 1h TTL |
| `max_entries_per_scope` | 128 | 单 scope entry 上限 |
| `max_entries_global` | 10000 | 全局 entry 上限 |
| `estimated_bytes_limit` | 64 MiB | 状态估算字节上限 |
| `expire_after_idle_seconds` | 0 | idle 淘汰时间，0 表示关闭 |

第一版只允许 `1..3600` 秒，协议不支持 TTL breakdown 时只保存内部 TTL，不向不支持的协议泄露 Anthropic 字段。

#### creation control

| 字段 | 默认 | 作用 |
| --- | ---: | --- |
| `creation_control.enabled` | false | 是否限制 creation |
| `min_creation_delta_tokens` | 0 | pending + 当前新增不足时不创建 |
| `min_successful_requests_between` | 0 | 两次 creation 之间最少成功请求数 |
| `min_creation_interval_seconds` | 0 | 两次 creation 最少时间间隔 |
| `max_creation_tokens_per_event` | 0 | 单事件 creation 上限 |
| `creation_budget_window_seconds` | 0 | 窗口长度 |
| `max_creation_tokens_per_window` | 0 | 窗口 creation 预算 |
| `expire_after_idle_seconds` | 0 | creation 控制状态 idle 清理 |

首次 creation 不受“前几次成功请求”限制；这些限制只作用于首次成功之后。被抑制的 creation 累计到 pending，达到 delta 和时间条件后才允许再次创建。

### 5.3 scope 设计与换号行为

目标 `CacheScope`：

```go
type CacheScope struct {
    GroupID          int64
    AccountID        int64
    StrategyID       int64
    StrategyRevision int64
    ProtocolFamily   string
    ModelKey         string
    SessionKey       string
    Namespace        string
}
```

默认 `scope_mode=group_account_session`，所有字段参与状态 key。这样：

- 同一 group、同一账号、同一协议、同一模型、同一 session、同一 revision 才能命中。
- 换上游账号默认不会命中另一账号的缓存。
- 换 group、协议、模型或策略 revision 都不会误命中。
- 策略更新后旧 state 自然失效。

可选 `scope_mode=group_session`，用于明确希望在同一 group/session 内跨账号共享本地模拟 state 的场景；这更接近参考项目 tracker 的 scope，但会让账号切换仍返回 read，必须作为高风险显式选项，不能成为默认。

session 来源：

1. 协议明确的 conversation/session id。
2. Claude Code metadata 中稳定的 session id。
3. Responses 的 continuation 信息仅作为会话辅助，不把一次性 request id 当 session。
4. 没有稳定 id 时，仅在 `allow_derived_session=true` 且 profile 满足安全条件时，由稳定 system/tools/首条 user 前缀派生。
5. 无法建立稳定 session 时不读、不写本地 state，保持 raw usage。

### 5.4 策略模板

模板只是创建时的初始完整配置，不是运行时另一套逻辑。建议内置：

1. **安全标准**
   - `prefix`
   - coverage `0.85`
   - usage ratio `1`
   - 当前 user 稳定前缀关闭
   - token scale `1`
   - creation control 关闭
2. **Claude Code 长会话**
   - `tool_aware`
   - coverage `0.95`
   - scale `1.2`，仅 `>=20000` 生效
   - max simulated input 由模型能力校验
   - creation control：最少 2 个成功请求、最短 60 秒、delta 4096
3. **低频创建**
   - `prefix`
   - coverage `0.9`
   - independent read `1` / creation `0.6`
   - 单次 creation 上限和窗口预算开启
4. **仅读取优先**
   - `tool_aware`
   - `incremental_create_enabled=false`
   - 已有 read 可继续命中；首次仍允许合法 creation
5. **关闭缓存**
   - `disabled`
   - 所有 cache state、cache usage 和缓存相关投影关闭

模板必须使用合理的多档参数，不能把所有策略设置成相同的大数。页面选择模板后仍允许编辑，保存的是完整归一化配置。

## 6. CacheProfile 设计

### 6.1 统一结构

```go
type CacheProfile struct {
    Scope              CacheScope
    ProtocolFamily     string
    ModelKey           string
    RuntimeInputTokens int
    ReportedTotalHint  int
    MinCacheableTokens int
    Blocks             []CacheBlock
    LookupPoints       []CacheLookupPoint
    Breakpoints        []CacheBreakpoint
    Revision           int64
}

type CacheBlock struct {
    Kind           string
    Fingerprint    [32]byte
    Tokens         int
    Stable         bool
    Cacheable      bool
    IsMessageEnd   bool
    TTL            time.Duration
    VolatileReason string
}

type CacheLookupPoint struct {
    BlockIndex          int
    Fingerprint         [32]byte
    RawCumulativeTokens int
}

type CacheBreakpoint struct {
    BlockIndex          int
    RawCumulativeTokens int
    TTL                 time.Duration
}
```

canonical value只在当前请求内保留，不能写入 state store。

### 6.2 各协议 block 顺序

#### Anthropic Messages

```text
semantic prelude
-> tools
-> system blocks
-> historical messages
-> historical tool_use/tool_result
-> current stable prefix (仅显式开启)
-> current dynamic tail (默认只计 token，不写 breakpoint)
```

`cache_control` 只决定断点，不参与 fingerprint。显式 5m/1h TTL 必须保留。

#### OpenAI Responses

```text
semantic prelude (model/tool choice/format)
-> instructions
-> tools
-> stable input items
-> historical message/tool call/tool result
-> current input tail
```

`previous_response_id`、prompt cache key 和 session 信息进入 scope/prelude，不能把一次 response id 直接当普通正文 block。

#### Chat Completions

```text
semantic prelude
-> tools/functions
-> system/instructions
-> historical messages
-> tool_calls/tool_call_id/function_call
-> current dynamic user tail
```

tool result 只有在被后续请求确认成为稳定历史后才可写入。

#### Web search 和动态工具

搜索查询本身可作为当前输入；搜索返回结果、当前时间、随机排序、外部实时状态默认为 volatile，不写入长期稳定 cache state。只有策略显式允许且 adapter 能证明结果稳定时才可缓存。

### 6.3 图片和二进制

图片 block 不能把 base64 放进 Redis/数据库。adapter 只保存：

```text
media_type + width + height + content_hash + estimated_tokens
```

token 估算失败时：

- 仍可将图片计入 runtime input。
- 不生成可缓存 breakpoint。
- 不因为估算失败把图片当成零 token。

## 7. Runtime 计算逻辑

### 7.1 Prepare 时序

```text
鉴权
-> 选择 group/account
-> 读取 group + strategy immutable snapshot
-> adapter 解析请求
-> 构建 profile
-> 校验模型能力和 session
-> lookup 最深前缀
-> 计算 raw CacheUsage
-> 计算候选 WriteEntries
-> 计算 ReportedUsage
-> 返回 CachePlan（此时不写 state）
```

### 7.2 运行时 token 与报告 token分离

必须区分：

- `runtime_total`：请求真实估算/上游上下文 token，用于 context safety、coverage 和内部 state。
- `reported_total`：对外 usage 的投影基准，可在显式策略下 scale，但必须受模型上下文能力和策略上限约束。

不能把 `token_scale` 直接写回 profile 的真实 token，再用它写缓存 entry。否则会出现 state 覆盖超过请求实际内容的假缓存。

### 7.3 coverage target

设：

```text
R = runtime_total
B = 最后一个合法可缓存断点的 raw cumulative
C = coverage_ratio
L = max_coverage_tokens (0 表示不限制)
M = 模型/策略最小可缓存 token
```

目标覆盖：

```text
coverage_limit = floor(R * C)
if L > 0:
    coverage_limit = min(coverage_limit, L)

target_point = 最深的完整 breakpoint
               且 point.raw_cumulative / B * R <= coverage_limit
target_raw = target_point.raw_cumulative
target_runtime = round(target_raw / B * R)

if target_runtime < M:
    target = 0
```

没有合法 breakpoint、`B=0`、session 无法建立或策略 disabled 时，target 为 0。

### 7.4 read lookup

1. 从最深 lookup point 向前查找。
2. 最多 `prefix_lookback_limit` 个点。
3. 只接受 scope、strategy revision、fingerprint 完全匹配且未过期的 entry。
4. 只取一个最深连续命中，不累加不连续 entry。
5. 命中时刷新该 entry 的 `last_used_at` 和 expires。

```text
read_raw = min(entry.cached_raw_tokens, target_raw)
read_runtime = scale_to_runtime_space(read_raw, R, B)
```

首次新 scope 没有 entry 时，`read=0`。禁止在 input projection 阶段把首轮 uncached input 差值凭空移动到 read。

### 7.5 creation candidate 和完整写入

```text
creation_candidate = max(target_runtime - read_runtime, 0)
```

规则：

- `incremental_create_enabled=false && read_runtime>0`：creation candidate 为 0。
- `max_new_creation_tokens_per_request>0`：限制本次可写入 token。
- 只写入命中点之后、目标点之前的完整 breakpoint。
- 受上限影响时回退到前一个完整 breakpoint。
- usage ratio 不改变 raw write set；它只改变对外报告。
- creation control 最终允许多少，write set 就只能覆盖多少。

### 7.6 creation control 与 pending

控制器状态按 `creation_control.scope_mode` 建 key，至少含 session、model、group，严格模式再含 account。

算法：

1. 首次 creation 允许候选 token。
2. 后续先检查最小成功请求数和最小时间间隔。
3. 当前新增 token 与 pending 累计小于 `min_creation_delta_tokens` 时不写。
4. 应用单事件上限。
5. 应用窗口预算剩余量。
6. 允许量不足时只保留对应完整 breakpoint；被抑制部分进入 pending。
7. 被抑制的 token 回到 uncached input，除非明确有其他已命中的缓存 read 可表达它。
8. 成功 commit 后才清零 pending、记录 creation event 和成功计数。
9. `Abort` 必须释放 prepare 阶段的 reservation，不消耗预算。

并发实现要求 `Reserve/Commit/Abort` 原子化，避免两个并发请求同时通过窗口预算。

### 7.7 5m/1h breakdown

按每个新增 segment 的 breakpoint TTL 统计：

```text
creation_5m = sum(segment.tokens where ttl < 1h)
creation_1h = sum(segment.tokens where ttl >= 1h)
creation_total = creation_5m + creation_1h
```

任何 final ratio、上限、creation control 之后，都必须重新执行：

```text
creation_5m >= 0
creation_1h >= 0
creation_5m + creation_1h == creation_total
```

Responses 和 Chat Completions 不支持 Anthropic nested TTL 字段时，不输出 `ephemeral_*`，但内部仍可保留 segment 分类用于日志和成本计算。

## 8. usage 合并和协议输出

### 8.1 上游权威字段优先级

adapter 先把上游响应解析成 `RawUsage`，并设置 `CacheEvidence`：

```text
上游有非零 cache read/write 或明确 authoritative 标记
    -> 使用上游 cache 字段
    -> 不叠加本地 cache 字段

上游没有 cache evidence
    -> 若本地 plan 有合法 read/creation，补本地字段
    -> 否则保持 raw
```

仅有字段但值为 0 是否算 authoritative，由 adapter 明确声明，不能用一个全局 if 猜测。Kiro `contextUsageEvent` 只提供上下文估算，不等于 cache read/write。

### 8.2 内部不变量

对于本地模拟或明确可守恒的响应：

```text
total_input = uncached_input + cache_read + cache_creation
cache_creation = creation_5m + creation_1h
所有字段 >= 0
cache_read + cache_creation <= total_input
```

如果协议的原始 usage 不满足这些关系，先保留 raw diagnostics，再生成符合协议语义的 reported usage；不得静默把负数或重复字段传给下游。

### 8.3 Anthropic Messages

对外：

```text
usage.input_tokens = 未缓存输入
usage.cache_read_input_tokens = read
usage.cache_creation_input_tokens = creation
usage.cache_creation.ephemeral_5m_input_tokens = 5m
usage.cache_creation.ephemeral_1h_input_tokens = 1h
```

`input_tokens` 不包含 cache read/creation。首轮只能 `creation>0, read=0`。

### 8.4 OpenAI Responses

对外：

```text
usage.input_tokens = uncached + read + creation
usage.input_tokens_details.cached_tokens = read
usage.cache_write_tokens = creation (若该兼容响应支持)
```

不能把 Anthropic 的 `input_tokens` 直接复制到 Responses；由 Responses adapter 重新投影。

### 8.5 Chat Completions

对外：

```text
usage.prompt_tokens = uncached + read + creation
usage.prompt_tokens_details.cached_tokens = read
usage.prompt_tokens_details.cache_creation_tokens = creation (若兼容响应支持)
usage.completion_tokens = output
```

流式最后 usage chunk 与非流式 usage 采用同一 `ReportedUsage`。

### 8.6 input/output projection

每个字段的 mode：

- `raw`：使用上游/内部原始值。
- `preserve`：不主动采样，只执行守护上限。
- `sample_max`：在不超过当前值的前提下，按 deterministic seed 采样到上限以内。
- `sample_target`：以目标为中心、按配置倍率形成有限区间采样。

采样必须：

- 以 profile fingerprint、scope、strategy revision 和字段值确定性计算。
- 同一请求重试得到同样结果。
- 不同 profile 在范围内产生自然差异。
- 小于 target 的值不被抬高。
- input 差值只有满足 read/creation 证据时才能移动到缓存字段，否则保留 uncached input。

### 8.7 context window 和 output 上限

模型能力服务提供：

```text
context_window_tokens
max_output_tokens
min_cacheable_tokens
supports_prompt_cache
supports_ttl_breakdown
```

reported total、output 和 cache breakdown 在最终输出前统一 clamp。能力未知时关闭 scale 和高值模板的自动放大。任何策略都不能生成超过模型 context window 的 input usage，也不能把 output 放大到模型 max output 之外。

## 9. Commit/Abort 和流式边界

### 9.1 非流式

```text
Prepare
-> upstream HTTP 2xx
-> body 可解析且状态为成功
-> 计算最终 reported usage
-> Commit
-> 返回响应
```

HTTP 2xx 但 body malformed、上游声明 failed 或本地转换失败时 `Abort`，不写 state。

### 9.2 SSE/streaming

状态机：

```text
Prepared
  -> Started
  -> Completed       => Commit
  -> UpstreamError   => Abort
  -> Malformed       => Abort
  -> ClientCancelled => Abort
  -> EOFWithoutDone  => Abort
```

只收到 `message_start`/`response.created` 不足以提交。内容可以按原有方式转发，但 state commit 必须等合法终态；终态 usage chunk 在 commit 后生成，避免下游看到与 state 不一致的 creation。

同一 plan 的 `Commit`/`Abort` 必须幂等，不能因多个 defer 或协议转换层重复调用而重复写入或重复消耗 creation budget。

## 10. 状态存储和缓存 key

### 10.1 状态内容

只保存：

```text
scope hash
fingerprint
raw_cached_tokens
ttl
expires_at
last_used_at
estimated_bytes
strategy revision
```

禁止保存完整 prompt、图片 base64、工具结果正文和 access token。

### 10.2 Store 接口

```go
type CacheStateStore interface {
    Lookup(ctx context.Context, scope CacheScope, points []CacheLookupPoint) (*CacheHit, error)
    ReserveCreation(ctx context.Context, key CreationControlKey, candidate CreationCandidate) (Reservation, error)
    Commit(ctx context.Context, reservation Reservation, entries []CacheWriteEntry) error
    Abort(ctx context.Context, reservation Reservation) error
    Touch(ctx context.Context, scope CacheScope, fp [32]byte, ttl time.Duration) error
    Invalidate(ctx context.Context, scope CacheScope) error
}
```

L1 内存 LRU 用于低延迟，L2 Redis 用于多实例共享；策略配置和 revision 在 PostgreSQL。Redis 故障时按照明确降级策略处理：可以返回 raw usage，但不能跨 scope 猜测命中。

## 11. 后端设计

### 11.1 Schema

最终 schema：

```text
cache_strategies
  id bigint primary key
  name varchar(100) unique not null
  description text not null default ''
  enabled boolean not null default true
  revision bigint not null default 1
  config jsonb not null
  created_at timestamptz not null
  updated_at timestamptz not null

groups
  cache_strategy_id bigint null references cache_strategies(id)
```

直接删除 group 中五个缓存 emulation 字段：

- `kiro_cache_emulation_enabled`
- `kiro_cache_emulation_ratio`
- `kiro_cache_emulation_mode`
- `kiro_cache_creation_emulation_ratio`
- `kiro_cache_read_emulation_ratio`

不设计旧值回填、双写或兼容 fallback；Ent schema、生成代码和测试 fixture 一次性使用最终模型。由于已存在的历史迁移链会创建这些废弃列，`231_drop_legacy_kiro_cache_emulation.sql` 在链尾无条件删除其约束和列，保证最终 PostgreSQL schema 与最终模型一致。

### 11.2 策略 service

职责：

- Create/Get/List/Update/Delete。
- 模板展开。
- 完整配置 normalize/validate。
- revision CAS。
- immutable runtime snapshot。
- 绑定 group 和影响范围。
- 策略启停。
- 删除前服务端重新查询 bound group，非零返回 409。

更新请求：

```json
{
  "expected_revision": 4,
  "name": "Claude Code 长会话",
  "description": "...",
  "enabled": true,
  "config": { "...完整配置..." }
}
```

更新必须在数据库层使用 expected revision 条件，不能只在内存比较。

### 11.3 API

```text
GET    /api/v1/admin/cache-strategies?search=
GET    /api/v1/admin/cache-strategies/templates
POST   /api/v1/admin/cache-strategies
GET    /api/v1/admin/cache-strategies/:id
PUT    /api/v1/admin/cache-strategies/:id
POST   /api/v1/admin/cache-strategies/:id/duplicate
POST   /api/v1/admin/cache-strategies/:id/enable
POST   /api/v1/admin/cache-strategies/:id/disable
DELETE /api/v1/admin/cache-strategies/:id
GET    /api/v1/admin/cache-strategies/:id/groups
PUT    /api/v1/admin/cache-strategies/:id/groups
```

绑定 API 需要：

- 校验策略存在且 enabled 状态可绑定。
- 校验 group 存在、未软删除、支持目标协议能力。
- 一个 group 同时只能绑定一个策略。
- 替换其他策略绑定时返回明确的替换结果，前端二次确认不作为安全校验。
- 更新绑定后使 group/auth snapshot 和 runtime registry 失效。

### 11.4 Auth snapshot

认证热路径不能每次查 PostgreSQL。snapshot 需要携带：

```go
type CacheStrategySnapshot struct {
    ID       int64
    Revision int64
    Enabled  bool
    Config   EffectiveCachePolicy
}
```

group snapshot 只携带策略摘要或完整 immutable snapshot，不携带管理端表单 DTO。策略更新时通过 group 绑定关系使相关 auth cache 失效。

### 11.5 运行时文件责任

建议拆分：

```text
cache_strategy.go          配置、模板、normalize、service
cache_scope.go             scope 和 key
cache_profile.go           block、canonical、fingerprint、adapter 共用结构
cache_runtime.go           Prepare/Commit/Abort
cache_state_store.go       L1/L2 store
cache_usage.go             Raw/Cache/Reported 和 invariant
cache_creation_control.go  creation reservation 和 pending
cache_adapter_anthropic.go
cache_adapter_responses.go
cache_adapter_chat.go
```

原 Kiro 缓存实现中的通用算法已归并到 `cache_runtime.go`；缓存主路径不再使用 `prepareKiro...` 或 `globalKiroCacheTracker` 命名。

## 12. 前端设计

### 12.1 导航和页面

新增管理员导航项“缓存策略”，复用现有：

- `AppLayout`
- `TablePageLayout`
- `DataTable`
- `BaseDialog`
- `Select`
- `Toggle`
- `EmptyState`
- 项目已有 `Icon`、`input`、`input-label`、`badge` 样式

不自己造一套卡片、按钮、弹窗或表单控件。页面必须使用现有布局和主题变量。

### 12.2 列表

列：

```text
策略名称/描述
策略类型
启用状态
usage 摘要
绑定分组数量
revision
操作
```

操作：

- 编辑。
- 复制。
- 启用/停用。
- 删除。
- 查看/编辑绑定分组。

筛选：名称搜索、策略类型、启用状态。

### 12.3 创建/编辑表单

创建时先选模板，再加载完整配置；编辑时可修改策略类型。表单分区：

1. 基础信息：名称、描述、类型、启用。
2. 缓存范围：system/tools/history/tool result/current user、断点模式、session 派生。
3. 覆盖和增量：coverage、max coverage、最小阈值、增量创建。
4. usage 投影：input/output/read/creation 四组 mode、目标、上限和 output guard。
5. token/context：scale、scale threshold、max simulated、cap jitter。
6. TTL 和容量：5m、1h、scope/global entry、bytes、idle。
7. creation control：delta、成功次数、时间间隔、单事件上限、窗口预算。
8. 绑定分组：展示当前绑定，支持搜索和替换确认。

每个字段必须显示中文 label、单位、简短说明和约束；不能只显示 `usage_ratio` 这类英文内部名。外层不增加大边框包裹表单，分区使用现有页面的标题、间距和分隔线；label 统一左对齐。

### 12.4 Group 页面改造

GroupsView：

- 删除 Kiro 缓存配置区。
- 删除 `kiro_cache_emulation_*` 表单状态和 payload。
- group 页面只展示只读“缓存策略：名称 / 类型 / revision / 未绑定”摘要。
- 绑定和编辑配置跳转到缓存策略页面。
- `platform=kiro` 时只保留 Kiro endpoint、sticky 等非缓存字段。

### 12.5 i18n

所有可见文案进入现有中文和英文 locale。默认管理员界面中文文案必须完整，不能把内部字段名直接渲染给用户。模板名称、类型说明、验证错误、替换确认和空状态都要有 i18n key。

## 13. 配置消费矩阵

实现前必须为每个字段登记以下五个消费点：

```text
前端控件 -> API DTO -> normalize/validate -> runtime consumer -> response/test
```

核心矩阵：

| 字段 | 前端 | normalize | runtime | 最终影响 | 测试 |
| --- | --- | --- | --- | --- | --- |
| `kind` | 类型 Select | 枚举 | adapter/profile | 是否启用 tool-aware/disabled | 类型切换、disabled |
| `coverage_ratio` | 覆盖比例输入 | 0..1 | target/write set | 实际缓存范围 | 多断点 coverage |
| `usage_ratio` | uniform 输入 | 0..1 | usage projector | read/creation 报告量 | uniform |
| `read_ratio` | independent 输入 | 0..1 | usage projector | read 报告量 | independent |
| `creation_ratio` | independent 输入 | 0..1 | usage projector/control | creation 报告量 | independent |
| `cache_system` | checkbox/toggle | kind 约束 | block filter | system 是否可读写 | system 改变 |
| `cache_tools` | checkbox/toggle | kind 约束 | block filter | tools 是否可读写 | tools 改变 |
| `cache_history` | checkbox/toggle | kind 约束 | block filter | 历史是否可读写 | history |
| `cache_tool_results` | checkbox/toggle | kind 约束 | block filter | tool result 是否可读写 | tool result |
| `cache_current_user_stable_prefix` | toggle | false 时 max=0 | block/breakpoint | 当前 user 稳定前缀 | 开关前后 |
| `breakpoint_mode` | Select | 枚举 | adapter | 断点来源 | marker/auto |
| `min_cacheable_tokens` | 数字输入 | 非负 | profile | 断点资格 | 小于阈值 |
| `model_min_cacheable_overrides` | 高级编辑 | 模式和数值 | capability resolve | 模型阈值 | model table |
| `max_coverage_tokens` | 数字输入 | 非负 | target/write | 最大实际覆盖 | 中间 block |
| `max_new_creation_tokens_per_request` | 数字输入 | 非负 | control/write | 单次新写入 | write set |
| `incremental_create_enabled` | toggle | bool | creation/write | 命中后是否写尾部 | grow session |
| `reported_input_min/max_tokens` | 数字输入 | min<=max | projector | uncached input 报告 | range |
| `token_scale` | 数字输入 | 1..3 | reported total | 对外总量 | small/large |
| `scale_min_input_tokens` | 数字输入 | 非负 | projector | scale 阈值 | below/above |
| `max_simulated_input_tokens` | 数字输入 | 非负 | context guard | 对外上限 | cap |
| `cap_jitter_*` | 数字输入 | 范围约束 | deterministic cap | 触顶差异 | 同/异 profile |
| `preserve_upstream_cache_usage` | toggle | bool | usage merger | 是否双计数 | native upstream |
| `default/hour_ttl_seconds` | 数字输入 | 1..3600 | breakpoint/store | expires 和 TTL bucket | expiry |
| `max_entries_*`、`estimated_bytes_limit` | 数字输入 | 非负 | store eviction | LRU/容量 | bounds |
| `creation_control.*` | creation 区域 | 组合校验 | reservation/controller/write | creation 频率、pending、预算 | control matrix |
| `scope_mode` | scope Select | 枚举 | CacheScope | 是否跨账号命中 | account switch |

如果某个字段无法填写“runtime consumer”和“最终影响”，就不能加入页面。

## 14. 异常场景与处理原则

| 场景 | 目标最终状态 |
| --- | --- |
| 首次新 scope | `read=0`；满足阈值时 `creation>0`；成功后才有 entry |
| 同 body 重试 | 已成功 commit 才允许 read；失败后的重试仍是首次 creation |
| 会话前缀增长 | 深度命中旧前缀，creation 只覆盖新增完整断点 |
| 历史前缀变化 | 不读取变化后的深度；最多命中仍相同的浅前缀 |
| 不同 account | 默认不命中；共享 scope 必须显式开启 |
| 不同 group | 不命中 |
| 不同 protocol | 不命中 |
| 不同 model | 默认不命中 |
| strategy revision 改变 | 旧 revision 不命中 |
| session 缺失 | 未允许派生时不读不写 |
| TTL 到期 | entry 删除或视为 miss，重新 creation |
| 命中后再次请求 | 刷新命中 entry TTL；不可刷新不连续 entry |
| `coverage_ratio=0` | 不产生 cache read/creation，不写 state |
| 小于最小阈值 | 不产生 cache usage，不写该断点 |
| creation 被节流 | 不写被抑制部分；token 回到 uncached input；pending 累积 |
| creation ratio < 1 | 只改变报告量，除非另有明确 state coverage 配置 |
| 上游有真实 cache 字段 | 保留真实值，不叠加本地值 |
| 上游 HTTP 4xx/5xx | Abort，不写 state、不推进 control |
| SSE malformed/EOF 无终态 | Abort |
| 客户端取消 | Abort，除非已明确完成且 commit 已幂等完成 |
| 重复 Commit | 只写一次 |
| 并发 creation | reservation 原子扣预算，不超窗口 |
| 图片估算失败 | 计入 runtime input，但不可作为 breakpoint |
| 动态 web search | 默认 volatile，不写稳定 state |
| Redis 不可用 | 按降级策略返回 raw/无本地缓存，不跨 scope 猜命中 |
| 模型 context 未知 | 关闭放大，保持实际估算 |
| 输入/输出负数或 NaN | normalize 拒绝或归零，最终不得下发负数 |

## 15. 实施顺序

1. 固定目标配置、scope、usage invariant 和 stream 状态机。
2. 更新 Ent schema、删除旧 group 缓存字段、生成最终代码。
3. 实现模板、normalize、校验、revision CAS 和策略 API。
4. 实现 immutable group/auth snapshot 和 binding invalidation。
5. 将 profile/canonical/hash 提炼为协议中立模块。
6. 实现三个 adapter，并补齐 session/model/context 能力。
7. 实现 state store、coverage target、最深命中、write set 和 creation reservation。
8. 替换所有 Anthropic、Responses、Chat Completions、Kiro 兼容和 web search 接线。
9. 统一 usage merge、TTL breakdown 和 streaming commit。
10. 完成缓存策略页面、模板、绑定面板、GroupsView 清理和 i18n。
11. 先跑单元/契约/协议 mock，再独立启动 PostgreSQL、Redis，最后本地高位端口启动业务验证。
12. 清理测试临时产物和编译产物，确认工作区无大体积无关文件。

## 16. 验收标准

实现只能在以下条件全部满足后标记完成：

- 每个配置字段都能在 runtime 或 response 中观察到有效影响。
- 首次 miss 永远不会凭 input sampling 生成 read。
- read/creation/5m/1h 和 total invariant 全部成立。
- 不超过模型上下文能力、策略上限和协议支持范围。
- 默认参数在小、中、大请求上有合理差异，不是固定大数。
- 不同 group/account/protocol/model/session/revision 的隔离符合 scope 配置。
- 失败、取消、malformed SSE 和重复 commit 不污染 state。
- 真实上游 cache usage 不被双计数。
- Kiro group 页面没有缓存专属输入，但仍能绑定通用策略。
- 页面复用现有 UI 组件和 i18n，不出现内部英文 key。
- PostgreSQL 和 Redis 可独立 Docker 启动，业务代码在宿主机高位端口启动，验证过程无大量临时产物。

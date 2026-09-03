# 分组缓存策略实施指导

## 1. 文档定位

本文是从设计到代码落地的执行手册，同时记录本次实际落地后的接线边界和验收门槛。它描述**先改什么、后改什么、每个模块承担什么职责、怎样验证配置真的生效**。

当前工作模式为 `execute-complete`：

- 本文把已经通过代码、测试或真实调度证据的步骤标记为完成。
- 验证章节只保留每个操作的最终响应、缓存状态和判定，不记录中间调试输出。
- 当前实现使用进程内有界 tracker；Redis 仅作为业务依赖隔离启动，本轮不做缓存状态 L2 复制。

本项目是新系统，首次落地直接使用目标 schema，不设计旧线上数据迁移、回填、双写、灰度兼容或回滚到旧 Kiro 缓存字段的路径。

## 2. 不可改变的范围

### 2.1 需要实现

```text
独立 CacheStrategy
    -> 绑定一个或多个 group
    -> group/auth immutable snapshot
    -> 协议 adapter 构建 CacheProfile
    -> Prepare（只读）
    -> Claude Code-compatible upstream
    -> Commit 或 Abort
    -> 各协议 usage 输出
```

策略必须对所有 Claude Code-compatible 分组生效，包括：

- Anthropic Messages。
- OpenAI Responses。
- OpenAI Chat Completions。
- Kiro 兼容入口（仅作为协议兼容入口，不成为策略类型）。

### 2.2 明确不做

- 不实现按路径匹配、路径覆盖、路径优先级或 `path_overrides`。
- 不把 Kiro endpoint、账号路径、sticky routing 放进缓存策略。
- 不新增 `kiro` 缓存策略类型。
- 不在 GroupsView 中继续编辑任何缓存参数。
- 不从 `group.kiro_cache_emulation_*` 回退读取，不做旧字段双写。
- 不保存完整 prompt、工具结果正文、图片 base64 或 access token。

`kiro_auto_sticky_enabled`、`kiro_sticky_session_ttl_seconds`、`kiro_endpoint_mode` 是否保留，只按其路由/endpoint 职责单独处理，不与缓存职责混合。

## 3. 实施前冻结的契约

在修改生产代码前，先把以下内容写成 Go 常量/类型、API schema 和前端类型，后续实现不得自行改变语义：

1. `CacheStrategyKind`：`disabled`、`prefix`、`tool_aware`。
2. `CacheRatioMode`：`uniform`、`independent`。
3. `BreakpointMode`：`client_only`、`auto`、`hybrid`。
4. `CacheScope` 维度：group、account、strategy、revision、protocol、model、session、namespace。
5. `RawUsage -> CacheUsage -> ReportedUsage` 三层 usage 模型。
6. `Prepare -> Commit/Abort` 状态机和流式合法终态。
7. 配置字段的默认值、范围、组合校验和关闭策略。
8. 模型能力字段：context window、max output、最小可缓存 token、协议能力。

如果其中任一项仍需要“实现时再决定”，不得进入第 5 节以后的生产代码改造。

## 4. 阶段总览与依赖关系

```text
阶段 A 领域契约和纯函数
    ↓
阶段 B Ent schema 和生成代码
    ↓
阶段 C 策略 CRUD、模板、revision CAS
    ↓
阶段 D group/auth snapshot 和绑定失效
    ↓
阶段 E profile、scope、state store
    ↓
阶段 F 三个协议 adapter 和统一 runtime
    ↓
阶段 G 所有网关入口、usage merge、stream commit
    ↓
阶段 H 管理页面、模板选择、GroupsView 清理、i18n
    ↓
阶段 I 单元/契约/mock upstream/隔离环境回归
    ↓
阶段 J 清理产物、文档收口、最终验收
```

每个阶段都必须先通过本阶段出口条件，才能开始下一阶段。不得为了让页面先显示而跳过后端契约，也不得为了让测试通过而放宽缓存不变量。

## 5. 阶段 A：领域契约和纯函数

### 5.1 新增或整理文件

建议文件：

```text
backend/internal/domain/cache_strategy.go
backend/internal/service/cache_policy_normalizer.go
backend/internal/service/cache_scope.go
backend/internal/service/cache_usage.go
backend/internal/service/cache_creation_control.go
```

如果项目没有单独 domain 包，可暂时放在 `internal/service`，但类型不能继续散落在 handler 或 Kiro 文件中。

### 5.2 必须定义的类型

```go
type CacheStrategyKind string
const (
    CacheStrategyDisabled  CacheStrategyKind = "disabled"
    CacheStrategyPrefix     CacheStrategyKind = "prefix"
    CacheStrategyToolAware  CacheStrategyKind = "tool_aware"
)

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

type EffectiveCachePolicy struct {
    Enabled                          bool
    Kind                             CacheStrategyKind
    RatioMode                        CacheRatioMode
    CoverageRatio                    float64
    UsageRatio                       float64
    ReadRatio                        float64
    CreationRatio                    float64
    CacheSystem                      bool
    CacheTools                       bool
    CacheHistory                     bool
    CacheToolResults                 bool
    CacheCurrentUserStablePrefix     bool
    CurrentUserStablePrefixMaxTokens int
    BreakpointMode                   BreakpointMode
    AllowDerivedSession              bool
    DynamicContentMode               DynamicContentMode
    MinCacheableTokens               int
    ModelMinCacheableOverrides       map[string]int
    MaxCoverageTokens                int
    MaxNewCreationTokensPerRequest   int
    IncrementalCreateEnabled         bool
    PrefixLookbackLimit              int
    ReportedInputMinTokens           int
    ReportedInputMaxTokens           int
    TokenScale                       float64
    ScaleMinInputTokens              int
    MaxSimulatedInputTokens          int
    CapJitterMinTokens               int
    CapJitterMaxTokens               int
    PreserveUpstreamCacheUsage       bool
    DefaultTTLSeconds                int
    HourTTLSeconds                   int
    MaxEntriesPerScope               int
    MaxEntriesGlobal                 int
    EstimatedBytesLimit              int64
    ExpireAfterIdleSeconds           int
    CreationControl                  CacheCreationControl
    ScopeMode                        CacheScopeMode
}
```

`CacheStrategyConfig` 的 JSON 结构可以继续由一个 JSONB 保存，但必须通过该强类型模型归一化后再入库。未知字段默认拒绝，避免页面拼写错误后静默失效。

### 5.3 normalize/validate 规则

归一化顺序固定为：

```text
default config
-> API 完整或 patch 输入
-> 类型默认值
-> ratio、整数、TTL、map 归一化
-> disabled/tool_aware 不适用字段清理
-> 组合校验
-> 生成 immutable EffectiveCachePolicy
```

至少实现以下错误：

- ratio 不是有限数或不在 `0..1`。
- `reported_input_min_tokens > reported_input_max_tokens`。
- `default_ttl_seconds < 1` 或 `hour_ttl_seconds > 3600`。
- `hour_ttl_seconds < default_ttl_seconds`。
- `current_user_stable_prefix_max_tokens > 0` 但开关关闭。
- `cap_jitter_min_tokens > cap_jitter_max_tokens`。
- `creation_budget_window_seconds == 0` 但设置了窗口 token 预算。
- `scope_mode` 不在允许枚举内。
- `token_scale > 1` 但模型能力未知时，运行时应降级为 1，而不是生成超大 usage。

### 5.4 阶段出口

- 纯函数测试覆盖默认值、边界、组合错误和 disabled 清理。
- 同一输入多次 normalize 得到字节级稳定的 canonical JSON。
- 生产代码不再引用 `kiro_cache_emulation_*` 字段；缓存函数命名按协议中立方式组织。

## 6. 阶段 B：Ent schema 和数据库结构

### 6.1 最终表结构

新增 `cache_strategies`：

```text
id bigint primary key
name varchar(100) unique not null
description text not null default ''
enabled boolean not null default true
revision bigint not null default 1
config jsonb not null
created_at timestamptz not null
updated_at timestamptz not null
```

`groups` 只保留一个可空外键：

```text
cache_strategy_id bigint null references cache_strategies(id)
```

### 6.2 文件边界

```text
backend/ent/schema/cache_strategy.go
backend/ent/schema/group.go
backend/ent/...
backend/ent/migrate/schema.go
backend/migrations/<initial-or-project-schema>.sql
```

这是新系统，不写“读取旧字段并填充新字段”的迁移逻辑。历史迁移链已经创建过旧字段，因此最终链以 `231_drop_legacy_kiro_cache_emulation.sql` 显式删除它们；该迁移不读取、回填或保留旧值，也不提供 fallback。不得修改已应用的历史迁移来伪造最终 schema。

最终删除以下字段及其生成代码、DTO、mapper、fixture：

```text
kiro_cache_emulation_enabled
kiro_cache_emulation_ratio
kiro_cache_emulation_mode
kiro_cache_creation_emulation_ratio
kiro_cache_read_emulation_ratio
```

### 6.3 阶段出口

- Ent 生成结果与 schema 一致。
- 新建 group 可以绑定或不绑定策略。
- 数据库约束阻止不存在的 strategy id。
- 代码库搜索不到旧缓存字段的生产消费点。

## 7. 阶段 C：策略服务、模板和管理 API

### 7.1 Service 职责

建议拆分：

```text
backend/internal/service/cache_strategy.go
backend/internal/service/cache_strategy_binding.go
backend/internal/repository/cache_strategy_repo.go
backend/internal/handler/admin/cache_strategy_handler.go
backend/internal/handler/dto/types.go
backend/internal/handler/dto/mappers.go
backend/internal/server/routes/admin.go
```

Service 必须提供：

- `Create`：接受模板 id 或完整 config，展开后 normalize，再保存 revision=1。
- `Get/List`：返回完整策略或脱敏摘要。
- `Update`：要求 `expected_revision`，数据库条件更新成功后 revision 自增。
- `Duplicate`：复制完整归一化 config，名称由服务端生成不冲突默认值。
- `Enable/Disable`：只更新 enabled 和 revision。
- `Delete`：服务端重新查询绑定 group，非零返回 409。
- `SetGroupBindings`：事务内校验策略、group、替换关系和权限。
- `Templates`：返回只读内置模板，不允许被普通更新接口覆盖。

### 7.2 API

```text
GET    /api/v1/admin/cache-strategies
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

所有写 API 沿用现有管理员权限中间件。普通用户只能在 group 摘要中看到策略名称、类型、revision 和 enabled，不可读取完整策略配置。

### 7.3 模板

模板是创建时的初始数据，不是另一套 runtime。它们只负责把一组**默认可用、用途明确、数值分层合理**的配置填进表单，不引入任何路径语义或 Kiro 专属逻辑。

默认模板至少应覆盖以下几类，且参数档位要有明显差异，不能全部落在一组“大数”上：

| 模板 | 关键参数 | 目的 | 典型参数档位 |
| --- | --- | --- | --- |
| 安全标准 | prefix、coverage 0.80~0.85、scale 1、当前 user 关闭 | 通用默认、低误命中 | 小中请求主用，最小阈值与 TTL 保守 |
| Claude Code 长会话 | tool_aware、coverage 0.90~0.95、scale 1.1~1.2、scale threshold 20000 左右、creation 间隔和 delta | 长上下文工具会话 | 中大请求主用，但严格受 context guard 约束 |
| 低频创建 | prefix、coverage 0.88~0.92、creation ratio 0.5~0.7、单次/窗口预算 | 降低写入频率 | 适合重复命中但不频繁写入的场景 |
| 仅读取优先 | tool_aware、incremental create 关闭或接近 0、read 保持 | 已有缓存优先 | 适合稳定前缀多、写入成本高的场景 |
| 关闭缓存 | disabled、enabled false | 对照和排障 | 不读、不写、不投影本地 cache usage |

模板必须给出多档合理数字；不能全部使用大数或相同数值，也不能把所有模板都压成同一个 coverage / scale / TTL 档位。模板加载后允许编辑，保存时仍走同一 normalize。实现和回归里应至少使用三档不同规模的请求样本：小、中、大，但大请求不得越过模型 context 或策略上限。

默认模板的测试目标不是“看起来能配置”，而是：

1. 新建策略时可以一键加载一套可运行的基础配置。
2. 每个模板都能在 mock upstream 下产生与用途一致的首次 miss / 后续 hit 结果。
3. 每个模板的 usage 分布必须和参数含义一致，不出现统一大数或超出上限的离谱值。
4. `disabled` 模板必须同时关闭读取、写入和本地 usage 投影。

### 7.4 阶段出口

- API contract tests 覆盖 CRUD、409 revision、409 bound delete、绑定替换。
- 每个 API 返回字段都有中文/英文前端类型映射。
- 模板返回完整配置，不含路径/Kiro 专属字段。
- 模板支持至少五种用途明确的默认策略，并且不同模板的数值档位有可辨识差异。

## 8. 阶段 D：group/auth snapshot 和绑定失效

### 8.1 Snapshot

认证热路径不能每次读取 PostgreSQL。定义：

```go
type CacheStrategySnapshot struct {
    ID       int64
    Revision int64
    Enabled  bool
    Config   EffectiveCachePolicy
}
```

`GroupRuntimeSnapshot` 只携带策略摘要或 immutable snapshot，不携带管理表单 DTO、原始 JSON map 或可变引用。

### 8.2 绑定规则

- 一个 group 同时只能绑定一个策略。
- 一个策略可以绑定多个 group。
- disabled 策略可以被绑定，但 runtime 必须明确不读、不写。
- group 删除/软删除前解除运行时引用。
- 策略更新、启停、绑定替换后，使相关 group/auth snapshot 和 runtime registry 失效。
- 更新过程中旧 snapshot 可以完成当前请求，但新请求必须读取新 revision。

### 8.3 阶段出口

- registry 对 map、slice 和嵌套 config 做深拷贝。
- 并发更新不会静默覆盖。
- 绑定和解除绑定后下一次请求能看到新 revision。
- 同一策略绑定多个 group 时 state key 仍按 group 隔离。

## 9. 阶段 E：profile、scope 和 state store

### 9.1 Profile

将原 Kiro 缓存实现中的通用算法归并到协议中立运行时：

```text
backend/internal/service/cache_profile.go
backend/internal/service/cache_canonical.go
backend/internal/service/cache_scope.go
```

统一结构：

```go
type CacheProfile struct {
    Scope              CacheScope
    ProtocolFamily     string
    ModelKey           string
    RuntimeInputTokens int
    Blocks             []CacheBlock
    LookupPoints       []CacheLookupPoint
    Breakpoints        []CacheBreakpoint
}
```

每个 block 使用 canonical JSON 和连续 SHA-256 链指纹。稳定 block 可以参与 lookup/write；volatile block 只能计入 runtime token，不能生成 breakpoint。

### 9.2 Scope 计算

默认 `group_account_session`：

```text
group + account + protocol + model + session + strategy revision + namespace
```

只有明确配置 `group_session` 时才允许跨账号共享。没有稳定 session 时，默认不读不写；只有 `allow_derived_session=true` 且满足安全条件才可以派生。

### 9.3 Store

```text
backend/internal/service/cache_state_store.go
backend/internal/service/cache_l1_store.go
backend/internal/service/cache_redis_store.go
```

统一接口：

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

L1 用有界内存 LRU；L2 用 Redis；策略配置和 revision 在 PostgreSQL。Redis 故障时只能降级为 raw usage/无本地命中，不得使用不完整 key 猜测命中。

state value 只保存 fingerprint、raw token、TTL、expires、last_used、estimated bytes、strategy revision 和 scope hash。

### 9.4 阶段出口

- 相同连续前缀可命中，尾部增长只产生新增 segment。
- 不同 scope/revision 不命中。
- TTL、entry bound、bytes bound 和 LRU 可单测。
- state store value 不含 prompt、base64 或 token。

## 10. 阶段 F：协议 adapter 和统一 runtime

### 10.1 Adapter 接口

```go
type CacheProtocolAdapter interface {
    ProtocolFamily() string
    BuildProfile(ctx context.Context, req *NormalizedRequest, policy EffectiveCachePolicy) (*CacheProfile, error)
    ParseRawUsage(resp *UpstreamResponse) RawUsage
    ProjectUsage(raw RawUsage, cache CacheUsage, policy EffectiveCachePolicy, caps ModelCapabilities) ReportedUsage
    IsSuccessfulTerminal(event StreamEvent) bool
}
```

### 10.2 Anthropic Messages

block 顺序：

```text
semantic prelude -> tools -> system -> 历史消息/tool result -> 当前稳定前缀 -> 当前动态尾部
```

规则：

- `cache_control` 只改变 breakpoint/TTL，不进入 fingerprint。
- 最后一条动态 user 默认不写。
- 工具声明按顺序 canonicalize。
- 图片估算失败时仍计入 runtime input，但不生成 breakpoint。

### 10.3 OpenAI Responses

- `instructions` 作为 system block。
- `input` 的 message/tool call/tool result 分块。
- `previous_response_id` 和 session 只进 scope/prelude。
- `response.created` 不是成功终态；需 `response.completed` 才能 commit。

### 10.4 Chat Completions

- tools/functions、system、历史消息和 tool result 分块。
- 最后一条动态 user 不自动写。
- 流式最后 usage chunk 与非流式共用 `ReportedUsage`。
- 原始 `[DONE]` 不能单独视为成功，必须确认已有合法 stop/finish 状态。

### 10.5 Web search 和动态内容

搜索结果、当前时间、随机排序和外部实时状态默认 volatile。查询文本可以参与当前输入；动态结果不能写长期稳定 state，除非策略显式允许且 adapter 有稳定性证明。

### 10.6 Runtime 计算顺序

```text
Prepare:
鉴权 -> group/account -> snapshot -> adapter profile -> model/session 校验
-> lookup -> coverage target -> creation reservation -> CacheUsage
-> ReportedUsage -> CachePlan（不写 state）

成功非流式:
HTTP 2xx -> body 可解析且状态成功 -> Commit -> 返回

成功流式:
Prepared -> Started -> 合法 Completed -> Commit

其他:
HTTP 错误、malformed、EOF 无终态、客户端取消 -> Abort
```

### 10.7 usage 和写入必须同步

- `coverage_ratio` 先决定实际 target 和完整 breakpoint。
- `read_ratio/creation_ratio` 只投影 usage，不隐藏真实可写状态。
- creation control 允许写多少，`WriteEntries` 就只能包含多少。
- 被抑制的 token 回到 uncached input，不能凭空消失。
- 首次新 scope 永远 `read=0`；只有成功 commit 后下一次才可 read。
- 上游有权威 cache evidence 时优先使用，禁止本地和上游双计数。
- `token_scale` 只影响 reported usage，不能改变 fingerprint、scope、实际写入和 runtime total。
- 最终 usage 统一 clamp 到模型 context window、max output、策略上限和协议支持范围。

### 10.8 阶段出口

- 三个 adapter 的 profile/usage 表驱动测试通过。
- 首次 miss、深度 hit、前缀增长、coverage 上限、creation control、TTL 和失败不写通过。
- 所有入口使用统一 Prepare/Commit/Abort，不再由 Kiro 专属函数承担主路径。

## 11. 阶段 G：接线和旧职责删除

### 11.1 请求入口

逐项检查并切换：

```text
backend/internal/service/gateway_forward.go
backend/internal/service/gateway_forward_as_chat_completions.go
backend/internal/service/gateway_forward_as_responses.go
backend/internal/service/openai_gateway_messages*.go
backend/internal/service/openai_gateway_responses*.go
backend/internal/service/openai_gateway_chat_completions*.go
backend/internal/service/kiro_runtime.go
backend/internal/service/gateway_websearch_emulation.go
backend/internal/service/kiro_websearch.go
```

每个入口只能：

1. 构建 normalized request。
2. 调用统一 runtime `Prepare`。
3. 请求 upstream。
4. 按终态调用 `Commit` 或 `Abort`。
5. 由对应 adapter 输出 usage。

### 11.2 删除范围

删除生产代码中：

- Kiro 缓存专属配置读取、归一化、handler DTO 和 UI payload。
- `prepareKiro...`、`globalKiroCacheTracker` 等通用主路径命名（缓存函数已改为协议中立命名）。
- `NormalizeGroupRuntimeFields` 中对 Kiro 缓存字段的清洗分支。
- 旧 group 缓存字段的 Ent schema、生成代码、mapper、snapshot、API contract 和测试 fixture。

保留并单独审查：

- Kiro endpoint mode。
- Kiro sticky routing。
- 与缓存无关的 Kiro 协议转换。

### 11.3 阶段出口

- `rg "kiro_cache_emulation_" backend frontend` 无生产消费结果。
- 任何 Claude Code-compatible group 都能只通过 `cache_strategy_id` 获得缓存策略。
- 同一策略在 Kiro、Anthropic、Responses、Chat Completions 入口的 state/usage 语义一致，只有字段投影不同。

## 12. 阶段 H：前端页面和 i18n

### 12.1 复用现有 UI

必须复用项目现有：

- 管理布局和侧边栏导航。
- 表格、分页、筛选、弹窗、Select、Toggle、Badge、EmptyState。
- 现有输入框、`input-label`、分隔线和主题变量。

不新增一套外层大边框卡片，不使用自绘替代组件。无法复用时，新增组件也必须遵守现有间距、圆角、字体和颜色令牌。

### 12.2 缓存策略 Tab

路由和导航新增“缓存策略”。列表至少展示：

```text
名称/描述 | 类型 | 启用状态 | coverage/usage 摘要 | 绑定分组数 | revision | 操作
```

操作：创建、编辑、复制、启用/停用、删除、查看/编辑绑定。

创建流程：

1. 先选择内置模板。
2. 加载完整配置。
3. 按分区编辑并实时显示单位、范围和中文说明。
4. 提交完整配置。

配置分区：

1. 基础：名称、描述、类型、启用。
2. 缓存内容：system/tools/history/tool result/current user。
3. 断点与 session：breakpoint mode、session 派生、动态内容。
4. coverage 和创建：coverage、最大覆盖、最小阈值、增量创建。
5. usage 投影：read/creation/input 的模式、比例和上下限。
6. token/context：scale、scale threshold、max simulated、cap jitter。
7. TTL/容量：5m、1h、scope/global entry、bytes、idle。
8. creation control：delta、成功次数、间隔、单事件、窗口预算。
9. 绑定分组：搜索、已绑定列表、替换确认。

### 12.3 GroupsView

- 删除缓存配置编辑区和全部 `kiro_cache_emulation_*` 状态。
- 仅显示只读策略摘要：名称、类型、revision、启用/未绑定。
- 绑定和修改跳转缓存策略页面。
- `platform=kiro` 只保留 endpoint/sticky 等非缓存字段。

### 12.4 i18n

中文 locale 必须完整覆盖默认管理员界面。英文 locale 同步提供：

- 策略类型和模板名称。
- 每个字段 label、单位、说明和校验错误。
- 绑定、替换、删除冲突、空状态和网络错误。

运行时禁止直接渲染内部字段名或未翻译 key。

### 12.5 阶段出口

- Vue 类型检查通过。
- 页面测试覆盖模板加载、类型切换、disabled 字段禁用、保存、绑定和冲突。
- GroupsView 不渲染、不提交旧 Kiro 缓存字段。
- 默认中文页面无英文内部 key。

## 13. 阶段 I：验证、隔离启动和回归

### 13.1 验证顺序

1. Go 纯函数和 runtime 单元测试。
2. API contract tests。
3. adapter profile/usage 表驱动测试。
4. mock upstream 协议测试。
5. 前端组件和类型检查。
6. 全量后端/前端测试。
7. PostgreSQL、Redis 独立 Docker 容器启动。
8. 业务服务在宿主机高位未占用端口启动。
9. 长会话、TTL、失败、取消、并发和重启专项回归。

不得用 3000 端口。启动前用端口探测选择未占用的高位端口，并把最终端口记录在验证结果中。

默认模板回归必须额外覆盖：

- 安全标准、Claude Code 长会话、低频创建、仅读取优先、关闭缓存五种模板。
- 小、中、大三档请求体，不允许所有用例都落在同一 token 量级。
- 首次请求必须满足 `cache_read=0`，只有成功 commit 后下一次请求才允许读取缓存。
- 任何模板都不得生成超过模型 context window、策略上限或协议支持范围的 usage。
- 不允许把“看起来很大”的统一数字当作默认值，模板之间要能看出参数差异。

### 13.2 隔离环境

- PostgreSQL：独立容器、独立 volume/数据库名、仅绑定 `127.0.0.1`。
- Redis：独立容器、独立端口和 key namespace。
- 业务代码：宿主机本地启动，不放进 Docker。
- mock upstream：宿主机本地启动，覆盖三类协议、非流式/流式、失败和权威 usage。

测试结束后删除本轮专用容器、volume、临时日志、coverage、构建产物和大文件；不得删除用户原有未提交文件。

### 13.3 默认策略回归结果

默认策略回归的结果记录只保留终值，不保留中间尝试数据。每个模板至少记录以下结论：

- 是否加载了正确的默认配置。
- 首次请求是否确认为 miss 且 `cache_read=0`。
- 第二次同 scope 请求是否命中。
- 小、中、大请求下 usage 是否保持合理梯度。
- 是否出现超出模型 context window、协议上限或策略上限的异常值。
- 如果发现模板数值过大、过于同质或首次读缓存异常，最终修复方案是什么，修复后终值是否恢复正常。

## 14. 阶段 J：文档和收口

完成后更新：

- `README.md`：状态、当前阶段、已落地证据和未决问题。
- `tasks.md`：只勾选有证据的任务。
- `verification.md`：每个操作只记录最终结果，不记录无意义的中间输出。
- `design.md`：若实现发现设计需要改变，先记录决策，再同步目标语义。

最终搜索：

```bash
rg -n "实现完成|测试通过|已启动|已验证" openspec/changes/add-group-cache-strategy
rg -n "path_overrides|KiroRsToolCachePolicy" backend frontend openspec/changes/add-group-cache-strategy
rg -n "kiro_cache_emulation_" backend frontend
```

第一条只能命中最终结果章节中有证据的条目；在实现前不得出现未经验证的结果描述。第二条只能命中“明确剔除/禁止照搬”的设计说明。第三条在旧字段删除完成后不得命中生产消费点。

## 15. 不应在实施中做的事情

- 不要把策略 JSON 复制回每个 group。
- 不要用完整 request body hash 替代连续前缀 fingerprint。
- 不要把 usage ratio 当作实际写入范围。
- 不要在 Prepare 或流式 start 阶段写缓存。
- 不要将上游 context usage event 当作 cache read/write 证据。
- 不要默认跨账号共享本地缓存。
- 不要因为模型能力未知而启用 token scale 放大。
- 不要把动态搜索结果、图片原文或工具正文写入 state store。
- 不要为了修测试而放宽 context upper bound、creation budget 或失败不写规则。

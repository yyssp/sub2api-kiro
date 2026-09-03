# 分组缓存策略实施状态

> 最后核验：2026-09-02。
> 本文件只勾选有代码、测试或真实环境证据的事项；明确不属于本次交付的内容单独列出，不伪装成已完成。

## A. 设计契约

- [x] 固定 `CacheStrategyKind`、`CacheRatioMode`、`BreakpointMode`、`CacheScopeMode` 枚举。
- [x] 固定完整策略配置、默认值、范围和组合归一化规则。
- [x] 固定 raw/cache/reported usage 分层及协议守恒规则。
- [x] 固定 profile、plan、write entry 和 tracker 的运行时边界。
- [x] 固定 `Prepare -> Commit/Abort` 状态机、幂等提交和流式合法终态。
- [x] 默认 scope 为 group + account + protocol + model + session + strategy revision；显式 `group_session` 才跨账号共享。
- [x] 首次新 scope 不产生 read；失败、取消、解析失败不写；上游权威 cache usage 不双计数。
- [x] 完成“前端控件 -> API -> normalize -> runtime -> response/test”消费链路设计。

## B. Schema 和生成代码

- [x] 新增 `cache_strategies` Ent schema、索引和生成代码。
- [x] `groups.cache_strategy_id` 外键、查询边和绑定关系已落地。
- [x] 生产 DTO、mapper、handler、GroupView 已删除旧 Kiro 内联缓存字段消费。
- [x] 最终 schema 不依赖旧字段回填或兼容读取；`231_drop_legacy_kiro_cache_emulation.sql` 删除历史迁移链遗留列和约束。
- [x] Ent 生成结果、迁移 SQL 和 schema 一致。
- [x] 策略唯一名、revision、外键、绑定删除约束有测试。

## C. 策略服务和 API

- [x] `NormalizeCacheStrategyConfig` 覆盖有限数、比例、TTL、token 上限、usage 模式和组合校验。
- [x] 内置模板覆盖安全标准、高缓存、Claude Code 长会话、输入整形、低频创建、仅读取优先和关闭缓存。
- [x] 模板参数按用途分层，覆盖小/中/大请求，避免统一大数。
- [x] 实现策略 Create/Get/List/Update/Delete。
- [x] 实现 revision CAS 冲突保护。
- [x] 实现 Duplicate、Enable、Disable。
- [x] 实现严格绑定、解绑、替换和一个分组只能绑定一个策略。
- [x] 重复绑定返回 `409 CACHE_STRATEGY_GROUP_CONFLICT`，包含分组、当前策略、请求策略和原因。
- [x] 策略/绑定变更会刷新 registry，并使相关认证/分组快照失效。
- [x] API contract、权限和错误响应测试通过。

## D. Snapshot 和 scope

- [x] 策略运行时使用归一化快照，不直接依赖可变请求配置。
- [x] map、slice 和嵌套 usage 配置在 registry 入口完成拷贝/归一化。
- [x] 支持显式 session、`prompt_cache_key`、Responses continuation 的来源优先级。
- [x] 无稳定 session 默认不读不写。
- [x] 严格 account isolation 和显式 group-session sharing 已验证。
- [x] group、account、protocol、model、session、revision 纳入缓存 scope。
- [x] 换账号、group、协议、模型、session、revision 隔离测试通过。

## E. Profile 和协议 adapter

- [x] 将稳定 canonical JSON、volatile 清洗、连续 SHA-256 前缀 fingerprint 提炼为协议中立 runtime。
- [x] Anthropic Messages、OpenAI Responses、OpenAI Chat Completions adapter 已接线。
- [x] Kiro 兼容入口复用同一 group-bound 策略，不新增 Kiro 策略类型或路径语义。
- [x] tools、system、history、tool result、current-user breakpoint 规则已实现。
- [x] 图片/二进制只做 hash/token 估算，不把 base64 写入 state。
- [x] 动态 web search 等 volatile 内容默认不写稳定 state。
- [x] 三协议、图片、动态内容和 canonicalization 测试通过。

## F. Runtime、usage 和 state

- [x] 最小可缓存 token、model override、完整 block 回退和最深连续前缀 lookup 已实现。
- [x] coverage、lookback、5m/1h breakdown 和 usage ratio 已实现。
- [x] independent/uniform projection、token scale、cap jitter、minimum/context guard 已实现。
- [x] creation control 的 pending、成功次数、间隔、单事件和窗口预算已实现。
- [x] reservation、Commit、Abort、重复提交幂等和失败释放已实现。
- [x] creation control 同时限制 usage 和实际 write set。
- [x] 进程内有界 tracker 的 entry/scope/global bound 和过期清理已实现。
- [x] 首次 miss、深度命中、前缀增长、coverage、ratio、TTL、creation control 测试已覆盖到当前交付范围。
- [x] 失败不写、流式终态、权威 usage、并发 reservation 关键路径已验证。
- [ ] Redis L2 缓存状态复制：本轮明确不引入，Redis 仅作为隔离业务依赖启动并验证连通性。

## G. 网关接线和 usage merge

- [x] Anthropic、Responses、Chat Completions、Kiro 入口统一调用 Prepare/Commit/Abort。
- [x] 非流式仅在 2xx、解析成功且业务成功后 Commit。
- [x] 流式仅在合法 completed/stop 终态后 Commit。
- [x] 4xx/5xx、鉴权/token 失败、网络错误、客户端取消、malformed SSE/EOF 均 Abort。
- [x] 上游权威 cache usage 优先，本地无证据时才补足。
- [x] Anthropic、Responses、Chat Completions 按各自字段语义输出 usage。
- [x] 最终流式 usage 与非流式语义一致。
- [x] 通用缓存运行时命名已去除 `prepareKiro...`、`globalKiroCacheTracker` 等误导性命名。

## H. 前端和国际化

- [x] 管理员“缓存策略”tab、路由和侧边栏入口已增加。
- [x] 列表、创建、编辑、复制、启停、删除、模板加载和绑定流程已实现。
- [x] 表单覆盖基础、缓存范围、断点/session、coverage、usage、token/context、TTL/容量、creation control 和绑定分区。
- [x] 策略类型可编辑；disabled 策略会清理并禁用不适用配置。
- [x] 绑定分组搜索、解绑、替换确认和冲突原因展示已实现。
- [x] GroupsView 删除旧 Kiro 缓存编辑区，仅保留 endpoint/sticky 等非缓存路由配置和只读策略摘要。
- [x] 中文/英文 i18n、模板文案、校验错误和空状态已补齐。
- [x] Vue 页面、模板、i18n、绑定和旧字段不提交测试通过。

## I. 隔离环境和真实回归

- [x] Mock upstream 覆盖三协议、非流式/流式、权威 usage、失败和恢复。
- [x] 后端 `go test ./...` 全量通过。
- [x] 前端 `pnpm typecheck`、`pnpm build` 和 267 个测试文件/1853 个测试通过。
- [x] PostgreSQL 独立 Docker 容器 `kiro-rs-postgres-local:25432`。
- [x] Redis 独立 Docker 容器 `kiro-rs-redis-local:26379`。
- [x] 当前业务代码在宿主机高位端口 `48780`，Mock upstream 在宿主机 `28081`，未使用 3000。
- [x] 使用真实 API Key、真实 group 绑定、真实 scheduler 和真实 HTTP 网关完成冷/热、未绑定、disabled、流式、权威 usage、失败恢复和策略参数矩阵。
- [x] 六种策略/绑定场景和三种协议的最终 usage 均符合配置上限、首次 miss 规则和协议字段能力。
- [x] 最新源码重新构建后的黑盒复核结果已写入 `verification.md` 操作 V-11。
- [x] 当前本地服务多参数真实调度矩阵已写入 `verification.md` 操作 V-12，覆盖 sample target、client-only、自动断点 creation control 和 TTL。
- [x] 自动断点动态 user 边界及低于完整 breakpoint 的 creation cap 已写入 `verification.md` 操作 V-13。
- [x] 当前本地服务大输入真实调度已写入 `verification.md` 操作 V-16：0.4 MB 至 2.6 MB 请求体、约 625k token prompt、Anthropic/Chat/Responses 冷热、三轮增量读取、仅读取、disabled 和 unbound 全部通过。
- [x] 新增四种模板真实连续调度已写入 `verification.md` 操作 V-19：严格客户端、共享会话、保守 usage、长上下文保护各 10 轮，40 次真实请求全部成功并完成响应/后台 usage 逐条核对。
- [x] Kiro IDE 凭据文件已通过当前业务服务导入预览：220 条 mixed OAuth/API Key 全部完成分类，social OAuth 缺失 `provider` 可合法导入预览；未在未指定目标分组时写入账户。终值见 `verification.md` 操作 V-17。
- [x] 当前本地服务已应用 `231_drop_legacy_kiro_cache_emulation.sql`；最终 PostgreSQL schema 保留 `cache_strategies` 和 `groups.cache_strategy_id`，且不再保留 Kiro 专属缓存列或约束。
- [x] 在迁移后的同一 `48780` 业务服务上重跑 V-18 大输入真实调度矩阵：29 次请求全部成功，三协议 usage、缓存冷/热规则、上限与 disabled/unbound 行为保持通过。
- [x] 删除临时 `cache_debug_tmp_test.go`，并复核真实调度结果未产生隐藏缓存状态。
- [x] 测试数据清理：通过当前业务服务管理员 API 删除全部 `e2e-*` 账号、分组、策略及绑定，并复核零残留。
- [x] 临时产物清理：删除不再使用的测试数据目录、旧二进制和浏览器 profile；保留当前服务依赖的数据目录、最新构建和最终测试结果文件。
- [x] 当前业务服务、Mock upstream、PostgreSQL 和 Redis 保持运行，便于页面和接口复核；仓库原有未提交文件未删除。

## J. 文档收口

- [x] `README.md`、`design.md`、`implementation-guide.md`、`tasks.md`、`verification.md` 已同步最终实现边界。
- [x] 验证文档只保留每个操作的最终终值，不记录中间调试数据。
- [x] 旧 Kiro 缓存字段只出现在历史迁移、删除范围/禁止照搬说明，不在生产代码消费；最终数据库 schema 由 `231` 迁移删除这些字段。
- [x] 每个关键结论都有测试名称、命令、真实环境或文档证据。
- [x] Kiro `provider` 的职责已收口为 OAuth/IDC 授权来源元数据；其不参与 Kiro 上游调度、API Key 鉴权、通用缓存策略或 usage 整形，导入行为有真实接口和单测证据。
- [x] integration schema 测试显式断言 `cache_strategies`、`groups.cache_strategy_id`、外键/索引存在，并断言已废弃的 Kiro 专属缓存列不存在。

## 明确不属于本次交付

- 不做线上旧数据迁移、回填、双写或兼容 fallback。
- 不把 Kiro 路径策略、endpoint 路由或账号路径语义移植到通用缓存策略。
- 不把 Redis 作为跨实例缓存状态 L2；当前 runtime 使用进程内有界 tracker。

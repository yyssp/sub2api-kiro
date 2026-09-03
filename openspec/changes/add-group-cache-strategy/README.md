# 分组缓存策略改造实施与验证

## 当前状态

状态：`Completed / verified`

本目录保存最终设计、实施边界、运行时规则和验收终值。缓存策略已落地为协议中立的独立资源，并通过真实 HTTP 网关、真实分组/API Key、真实调度器和宿主机 mock Claude Code-compatible 上游完成回归。本文档中的验证数据只保留每个操作的最终值，不记录调试过程和中间尝试。

本轮明确不做：

- 不做旧线上数据迁移、回填、双写或灰度兼容。
- 不把路径绑定、路径匹配、账号路径、endpoint 路由等 Kiro 语义移植到当前项目。
- 不把 `kiro` 做成缓存策略类型。
- 缓存状态采用当前业务进程内有界 tracker；本轮不引入跨实例 Redis L2 缓存状态复制。

## 目标

把当前挂在 group 上的 Kiro 内联缓存配置，改造成适用于所有 Claude Code-compatible 分组的“独立缓存策略 + group 绑定”模型：

```text
CacheStrategy（独立策略）
        ↓ 一个或多个 group 绑定
GroupRuntimeSnapshot
        ↓ 请求进入统一协议适配器
CacheProfile -> Prepare -> Upstream -> Commit/Abort
        ↓
Anthropic / Responses / Chat Completions 各自输出正确 usage
```

Kiro 只是其中一个 Claude Code-compatible 协议入口。策略名称、类型、模板、缓存状态和 usage 计算都必须是协议中立的。

## 阅读顺序

1. `design.md`：最终领域模型、参考策略拆解、公式、scope、协议差异、页面和后端设计。
2. `implementation-guide.md`：实际落地顺序、文件边界、代码接线和验证门槛。
3. `tasks.md`：按证据勾选的实施状态。
4. `verification.md`：真实调度矩阵和每个操作的最终终值。
5. `real-sonnet45-cache-verification.md`：当前本地服务使用真实 Kiro 账号连续调度 Sonnet 4.5 的最终 usage、后台记录交叉核对和回归结论。

## 关键决策

- group 只保存 `cache_strategy_id`，不再编辑缓存参数。
- 策略保存归一化后的完整配置，并通过 `revision` 生成不可变运行时快照。
- 旧的 `kiro_cache_emulation_*` 已由 `231_drop_legacy_kiro_cache_emulation.sql` 从数据库 schema 删除；不保留兼容读路径、fallback 或双写。
- 默认采用严格 scope：`group + account + protocol + model + session + strategy revision`。因此换上游账号默认不会命中另一账号的本地缓存；如确实需要按 group/session 共享，必须显式选择共享 scope 模式并承担误报风险。
- `coverage` 决定内部实际可写入的前缀范围；`usage ratio` 只决定对外 usage 投影，二者不能互相冒充。
- `prepare` 不写状态；只有上游完整成功后 `commit`，失败、取消、SSE 不完整和解析错误均 `abort`。
- 首次冷启动只能有 `cache_creation`，不能凭 input 采样凭空制造 `cache_read`。
- 上游已经提供权威 cache read/write 时优先保留，禁止和本地模拟双计数。
- usage 的内部总量、Anthropic 字段、Responses 字段和 Chat Completions 字段分别遵守各协议语义。
- 不把完整 prompt、base64 图片或工具结果正文写入 PostgreSQL/Redis；状态只保存 fingerprint、token、TTL、过期和边界信息。

## 与参考项目的关系

参考 `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs` 的内容只用于学习以下通用能力：

- 稳定 block、canonical JSON、连续 SHA-256 前缀 fingerprint。
- 最深连续前缀命中、5m/1h TTL、entry bound 和 LRU 清理。
- raw usage、cache usage、reported usage 分层。
- `token_scale`、上限抖动、最小阈值和 deterministic sampling。
- creation control 的间隔、成功次数、增量阈值、单次预算和窗口预算。
- prepare/commit 的成功时序和失败不写原则。

明确剔除：

- 路径级策略和 `path_overrides`。
- Kiro endpoint、账号路径、sticky routing。
- `KiroRsToolCachePolicy` 这个命名和它的协议专属入口。

## 当前实施状态（2026-09-01）

已落地并有针对性证据：

- 独立 `cache_strategies` schema、group `cache_strategy_id` 绑定和 registry。
- Anthropic Messages、OpenAI Responses、OpenAI Chat Completions、Kiro 兼容入口统一走 group-bound cache plan。
- 首次 miss 不读缓存；成功后 commit；失败请求不提交缓存状态。
- scope/revision/protocol 隔离，以及 `group_session` 跨账号共享行为。
- usage 的 input/output/cache_read/cache_creation 四字段整形与协议映射。
- 管理页策略模板、可编辑策略类型、中文/英文 i18n。
- 一个分组只能绑定一个策略：重复绑定或跨策略绑定返回 HTTP 409，并带 `group_id`、`group_name`、当前/请求策略 ID 和原因；前端中文文案已把原因显式展示出来。
- 多策略真实 mock upstream 回归：四个模板 profile 全部通过，覆盖前缀高缓存、工具感知、输入整形和关闭缓存。
- 追加低频创建、仅读取优先两种模板，并完成策略配置回归，模板参数不再只有四种固定档位。
- 真实宿主机 HTTP 调度覆盖 Anthropic Messages、OpenAI Chat Completions、OpenAI Responses，包含冷启动、热命中、未绑定、disabled、creation control、权威 usage、失败恢复和绑定冲突。
- 当前本地服务大输入/大 usage 真实调度矩阵已写入 `verification.md` 操作 V-16，覆盖 `0.4 MB` 至 `2.6 MB` 请求体、约 `625k` token prompt、三协议冷热和三轮增量读取。
- `231_drop_legacy_kiro_cache_emulation.sql` 已在当前 PostgreSQL 实例实际应用；V-18 已在迁移后的同一业务服务上重跑 29 次大输入真实调度，最终 schema 和 usage 约束均通过。
- 后端 `go test ./...` 全量通过；前端类型检查、组件测试和生产构建通过。
- Kiro IDE 凭据导入支持单对象和数组导出、camelCase/snake_case 字段、OAuth 与 API Key 混合条目；导入预览只解析并分类，只有前端确认创建账户后才写入数据库。
- Kiro 账户的 `provider` 是 OAuth/IDC 授权来源元数据：新建登录时用于选择 Google/GitHub/External IdP，IDC 可由 `start_url` 推导；它不参与 Kiro 上游调度、API Key 鉴权、通用缓存策略绑定或 usage 整形。Kiro IDE social OAuth 导出允许 `provider` 为空。

对应证据：

- `backend/internal/service/group_cache_strategy_http_integration_test.go`
- `backend/internal/service/cache_strategy_runtime_test.go`
- `backend/internal/handler/admin/cache_strategy_handler_test.go`
- `backend/internal/server/routes/gateway_cache_strategy_dispatch_integration_test.go`

## 当前真实环境

- 业务服务：宿主机 `http://127.0.0.1:48780`（高位端口，未使用 3000），运行当前最新源码构建。
- mock Claude Code-compatible 上游：宿主机 `http://127.0.0.1:28081`。
- PostgreSQL：独立 Docker 容器 `kiro-rs-postgres-local`，宿主机端口 `25432`。
- Redis：独立 Docker 容器 `kiro-rs-redis-local`，宿主机端口 `26379`。
- 管理员测试账号：`admin@sub2api.local` / `CacheLocal!2026`。
- 本轮真实回归创建的 `e2e-*` 账号、分组、缓存策略和绑定已通过管理员 API 全部删除，默认数据保留。
- 当前业务服务依赖的数据目录 `/tmp/sub2api-cache-smoke-data` 和最新构建 `/tmp/sub2api-local-latest` 正在使用，不能删除。

## Kiro 凭据导入终值

本地文件 `kiro-credentials-2026-09-01T05-33-02-662Z.json` 已通过当前服务的
`POST /api/v1/admin/kiro/oauth/import-token` 做仅解析预览，未创建账户、未写入数据库。

- 条目总数：`220`。
- OAuth：`205`（social `199`、IDC `6`）。
- API Key：`15`。
- 所有 social OAuth 均没有 `provider`，仍按 social OAuth 正确分类；这符合 Kiro IDE 导出格式。
- OAuth 条目不被解释成 API Key，API Key 条目不被解释成 OAuth access token。
- 实际导入时需在账户创建界面选择目标 Kiro 分组；批量创建会保存敏感凭据并加入调度池，因此不在未明确目标分组时自动执行。

历史 V-01 至 V-10 中出现的 `28080/26432/26380` 属于已经结束的隔离黑盒阶段；当前可复现验证以 V-11 至 V-18 记录的 `48780/25432/26379` 环境为准。

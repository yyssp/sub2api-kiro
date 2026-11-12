# 03 Cursor 相关但本轮暂不实现的项

这些改进都与 Cursor 协议直接相关，暂不实现的原因有三类：
- 缺少真实账号的实测；
- 社区证据互相冲突；
- 需要付费或团队账号才能验证。

每条都写明了启用前需要怎样实测，以及改动落在哪里。

本仓库代码的引用一律写**函数名**，不写行号，因为这部分代码本轮还在修改。社区出处固定到调研时的提交，提交号与 [02](02-gateway-ideas.md) 相同。

---

## 1. 在生产环境接入 AvailableModels 模型目录刷新

**是什么**
- `cursor.RefreshModelsFrom` 和 `cursor.EnableModelsPersistence` 在生产代码里**没有调用方**。
- 因此 `claudeResolutionModels()` 永远回落到静态的 `DefaultModels`，模型名解析只依赖硬编码列表。
- ai2api 在启动时会加载这份目录，并持久化到 `cursor_models.json`（ai2api `cmd/main.go:198`）。

**社区证据**
- 刷新策略：
  - cpa-cursor 每个凭证每小时刷新一次（[plugin/config.go:15](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/plugin/config.go#L15)）。
  - YeautyYE 按账号缓存，TTL 5 分钟，单飞加载，失败时回落到静态目录（[catalog.rs:53-54](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/catalog.rs#L53-L54)）。
  - greenSheep 按需拉取，缓存 5 分钟。

**为什么暂不做**
- 一旦有了动态目录，`ResolveClaudeCodeModel` 会把 `claude-opus-5` 解析成目录里真实存在的复合 ID，例如 `claude-opus-5-medium`。这个 ID 经 `sandResolvedModel` 发往上游，**wire 上的模型 ID 就变了**。
- 社区在这一点上的证据互相冲突：
  - dimC 记录 [shunt#411](https://github.com/pleaseai/shunt/pull/411) 和 [jcode#1226](https://github.com/1jehuang/jcode/issues/1226)：wire 上必须用裸的基础 ID，复合 ID 会返回 `ERROR_BAD_MODEL_NAME`，带 `-fast` 后缀会返回 502 not_found。
  - 但 ai2api 生产环境一直在用扁平 ID，例如 `claude-opus-5-medium`、`cursor-grok-4.5-high`，而且能正常工作。
- 没有真实账号做 A/B 测试，就不能切换。

**启用前需要怎样实测**
- 用同一个 Pro 账号，对 Sand 面（`InferenceService/Stream`）和 AgentService 面各发两组请求：
  - ① 现状：静态回落后的 ID；
  - ② 动态目录解析后的复合 ID。
- 比较上游是否接受，以及 dashboard 的计费标签。
- 同时确认 `requested_model.parameters` 推导出的 effort/thinking 与复合 ID 是否重复或冲突。

**改动落点**
- service 层新增启动加载和每小时刷新：按套餐挑一个健康账号去拉，用 leader lock 避免多实例重复拉取，调用 `EnableModelsPersistence(DATA_DIR/cursor_models.json)` 落盘，失败时保留上次成功的结果。
- 同时需要检查 `normalizeCursorModel`、`applyClaudeEffortModel`、`parseSandModelSelection`（`sand.go`）在动态目录下的行为。

## 2. AvailableModels 请求带 `useModelParameters:true`

**是什么**：`ListModelsFull` 现在发的请求体是空的 `{}`。cpa-cursor 发的是官方 CLI 的请求体：`{"useModelParameters":true,"doNotUseMarkdown":true,"additionalModelNames":[]}`（[unary.go:221](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/cursor/unary.go#L221)）。带上这个参数后，服务端会为每个模型描述 `parameterDefinitions` 和 `variants`。

**社区证据**
- cpa-cursor 在 AvailableModels 失败时降级为不带参数的请求（[plugin/models.go:202-206](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/plugin/models.go#L202-L206)），并以 `GetUsableModels` 为主数据源。
- 它还会解析 `contextTokenLimit` 和 `degradationStatus`（[unary.go:204-206](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/cursor/unary.go#L204-L206)）。

**为什么暂不做**
- 响应形状会从扁平的「每个变体一行」变成「基础模型加 variants」。`liveSet`、`claudeModelIndex` 以及 `ResolveClaudeCodeModel` 的候选拼接全都是按扁平 ID 写的，需要一起调整。
- 本条依赖第 1 条先接入目录刷新。

**启用前需要怎样实测**
- 抓一份带参数的真实响应，作为 fixture 放进 `sand_test.go`。
- 确认 `legacySlug` 能完整映射回现有的扁平 ID，否则已经配置的模型映射会失效。

**改动落点**：`client.go` 中的 `ListModelsFull`（请求体和解析逻辑）、`models.go` 中的索引构建。

## 3. 默认补 `fast=false`

**是什么**：Cursor 后端会把**缺省的** fast 参数当作 true 处理，dashboard 上显示为 `-fast` 后缀，并按 fast 计费。

**社区证据**
- [ccextra commit 2d664b49](https://github.com/knoxwh/ccextra/commit/2d664b49)：词表里有 fast、且用户没有显式钉参数时，注入 `fast=false`；显式钉了 `fast=true` 时以钉的为准。
- [KonghaYao CLAUDE.md:148](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/CLAUDE.md#L148)：只送 `composer-2.5` 而不带 `fast=false`，Team Usage 会记成 `composer-2.5-fast`。
- Sunnyender-org/cursor-sdk2api 的 fork XiaoXiaoXian（08-25）把 grok4.6 的默认 `fast=true` 改成了 false。

**为什么暂不做**
- 只有在词表里声明了 `fast` 参数时才能补，否则可能触发参数校验错误。
- 本仓库目前没有带参数的目录（第 1、2 条都未落地），`ModelMeta.Parameters` 在生产环境恒为空，无从判断该不该补。
- 另外，我们走的是扁平 ID：本身带 `-fast` 后缀的 ID 和不带后缀的 ID 各自是独立条目。缺省 fast 的问题是否同样影响扁平 ID 这条路径，**未核实**。

**启用前需要怎样实测**
- 用一个 Pro 账号发同一个不带 `-fast` 的模型，对比 dashboard 的用量事件里模型名是否带 `-fast`。
- 如果带，就要在 Sand 面的 `requested_model.parameters` 里补上 `{id:"fast", value:"false"}`。

**改动落点**
- `sand.go` 中的 `buildSandRequestedModelWithParameters` / `parseSandModelSelection`：当词表含 `fast` 且没有显式值时，追加 `fast=false`。
- AgentService 的 `requested_model` 字段 9 中的 parameters 字段 3，同理处理。

## 4. 结构化多轮：root_prompt_messages_json 加 KV blob

**是什么**
- 不再把整段会话拍平成一条 user 文本，而是放进 `conversation_state`（AgentRunRequest 字段 1）的 `root_prompt_messages_json`（字段 1），每个元素是一个 blob id（sha256）。
- 上游通过 `kv_server_message`（AgentServerMessage 字段 4）中的 `get_blob_args` 回取 blob，客户端用 `kv_client_message`（AgentClientMessage 字段 3）中的 `get_blob_result` 应答。
- 也可以通过 `pre_fetched_blobs`（AgentRunRequest 字段 17）预先推送 blob。

**社区证据**
- **正面**：
  - [KonghaYao CLAUDE.md:17](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/CLAUDE.md#L17)：每次请求都从全量 messages 重建 roots，不依赖服务端状态，可以多实例部署。
  - cpa-cursor `history.go`、opencodex `protobuf-request.ts`。
- **反证**：`UserMessageAction.conversation_history`（字段 7）实测模型看不到：
  - [greenSheep phase-7d-multi-turn.md:57](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/phase-7d-multi-turn.md#L57)；
  - [phase-7k-conv-memory.md:51](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/phase-7k-conv-memory.md#L51)：变体 B 0/3 通过。
- **代价数据**：greenSheep 实测拍平与结构化的 token 消耗几乎相同（phase-7k）。结构化的收益主要在**模型行为**：本仓库 `buildAgentMessage` 的注释记录过，拍平后模型会把已执行的工具当成没执行，从而重复调用。

**为什么暂不做**
- 风险都有实测记录：
  - 空的 `turns: []` 或空的 `conversationState: {}` 会抹掉上文（[KonghaYao CLAUDE.md:55,134,139](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/CLAUDE.md#L134)）；
  - 跟进轮省略 state 会报 `Conversation state is required`（CLAUDE.md:229）；
  - root 有上限，208 个时会在后期报 invalid_argument，opencodex 限在 192 个、512KB（[protobuf-request.ts:74-81](https://github.com/lidge-jun/opencodex/blob/852bc147b6802083fdc14739cb667afb537f357e/src/adapters/cursor/protobuf-request.ts#L74-L81)）；
  - 外部模型重放带 mcpToolCall 的 turns 会 invalid_argument（protobuf-request.ts:1452-1464），所以只能用 roots，不能用 turns。
- **claude-\* 模型在社区里没有任何实测**，现有实测都基于 composer-2.5 或 grok。

**启用前需要怎样实测**
- 用 claude-sonnet / claude-opus 跑 KonghaYao 的三句验收：
  - 「你的工具有什么」→「调用一下」→「我的第一句话是什么」，第三句必须能复述第一句；
  - 再跑一个 `get_weather` → `lookup` → `search` → 终轮文本的多工具链。
- 对比拍平方案的重复调用率。

**改动落点**
- `agent.go` 中的 `buildAgentClientMessage`：字段 1 改为填 roots。
- `consumeAgentResponse` 新增字段 4 的 KV 应答，需要按 run 维护有上限的 map。
- service 层 `cursorRequestFromBody` 输出结构化消息，不再走 `BuildAgentMessage`。

## 5. system 改走 RequestContext.rules

**是什么**
- 应答 `request_context_args` 时，不再回空的 RequestContext，而是：
  - 把 Anthropic system 放进 `rules`（RequestContext 字段 2）：`CursorRule{1 full_path, 2 content, 3 type={1 global={}}, 4 source=2(USER)}`；
  - 同时把 tools 放进字段 7。

**社区证据**
- [cpa-cursor request.go:94-125](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/translate/request.go#L94-L125)：`cursorRules`，`full_path` 写成 `/client/system-prompt/N.mdc`。
- 注释说在 composer-2.5 上实测过：没有历史 root 时 rules 生效；有历史 root 时，需要客户端自己带一条 `<rules>` root。
- 反例：YeautyYE 刻意不放（[request.rs:923-927](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/request.rs#L923-L927) 的 `cursor_request_context` 注释写明：不把 Claude system 拷进 Cursor system、rules 或 agent_skills）。理由见 [request.rs:31-35](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/request.rs#L31-L35)：把 Claude Code 的完整 system 嵌进 UserMessage 后，Fable 会把它当成 prompt injection（2026-07 实测）。也就是说，它实测的是「把 system 嵌进 user 文本」，不是放进 rules。
- 普通账号的 `custom_system_prompt`（字段 8）会返回 `invalid_argument: unknown option '--system-prompt'`，多个仓库都确认过（[KonghaYao CLAUDE.md:75](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/CLAUDE.md#L75)；YeautyYE request.rs:31 认为该字段只对团队账号开放）。
- 没有任何仓库报告 rules 会触发 invalid_argument。

**为什么暂不做**
- 只有 composer-2.5 的实测，claude-\* 没有。
- Claude Code 的 system 有 10k 以上 token，放进 rules 会叠加在 Cursor 自带的约 11k agent prompt 之上，加大额度消耗。
- 身份冲突（「你是 Claude Code」对上「你是 Composer」）可能被模型判成注入。

**启用前需要怎样实测**
- 先只把网关自己的约束文案（`declaredToolsAgentConstraint`、`noToolsAgentConstraint`）迁到 rules，验证 claude-\* 是否遵守。
- 再用开关灰度下发 Claude Code 的原始 system，对比回答质量和用量。

**改动落点**：`agent.go` 中的 `buildRequestContextResult`：`request_context(1)` 内填入 `rules(2)` 和 `tools(7)`。

## 6. conversation_id 改为稳定哈希

**是什么**：本仓库每个请求都用 `genUUID()` 生成 conversation_id（AgentRunRequest 字段 5）。KonghaYao 改成 `tenant:sha256(model, effort, flags, toolnames, system, 首条 user)`，目的是吃到 Cursor 侧绑定在 conversation_id 上的前缀缓存。

**社区证据**
- [KonghaYao docs/canonical-session-fingerprint.md](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/docs/canonical-session-fingerprint.md)。
- [CLAUDE.md:130](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/CLAUDE.md#L130)：每轮用随机 id 出过一次缓存事故（9/1）。
- greenSheep phase-7k 的反证：只复用 conversation_id **不会**恢复对话记忆，变体 A 和 D 的 input 都是 11620。

**为什么暂不做**
- 收益是账号额度（Cursor 侧的 cache read），不影响本仓库的计费，量化数据**未核实**。
- 风险：同一会话并发请求时，上游会报 "already active"。YeautyYE 遇到时会轮换会话（[mod.rs:2018](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/mod.rs#L2018)、:4591），需要配套处理。

**启用前需要怎样实测**：同一会话连续 5 轮，对比随机 id 与稳定 id 下 `turn_ended` 的 cache_read（字段 3）；并发两个同会话请求，观察是否报 already active。

**改动落点**：`agent.go` 中 `buildAgentClientMessage` 的字段 5；指纹在 service 层计算，再随 `AgentRequest` 传入。

## 7. x-cursor-team-id

**是什么**：多团队账号需要带 `x-cursor-team-id`，否则据称会出现 "Team budget limit reached" 429。

**社区证据**
- team id 来自 `DashboardService/GetMe` 的 `teamId` 字段（[greenSheep usage/client.go:368-386](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/usage/client.go#L368-L386)），字段非空时才发这个头（[executor/headers.go:75-77](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/executor/headers.go#L75-L77)）。
- 这条协议事实出自 hofund 设计文档和 [magpie-community/plugins#231](https://github.com/magpie-community/plugins/pull/231)。

**为什么暂不做**："Team budget limit reached" 在所有已克隆仓库里**零命中**，没有任何多团队账号的实测，**未核实**。

**启用前需要怎样实测**：用一个属于多个团队的账号，分别在带头和不带头的情况下请求，观察 429 文案和计费归属。

**改动落点**
- 建号或刷新额度时，顺带调一次 GetMe，把 team_id 存进 credentials。
- `client.go` 的 `buildHeadersWithType` 在 team_id 非空时追加这个头。

## 8. 客户端身份切换为 CLI 形态

**是什么**：活跃仓库普遍用 cursor-agent CLI 身份：
- `x-cursor-client-version: cli-YYYY.MM.DD-hash`，例如 [cpa-cursor config.go:20](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/cursor/config.go#L20) 用的 `cli-2026.10.22-27dcf5f`；
- 请求头很少：不带 checksum、os/arch、commit、x-session-id、x-client-key（[client.go:105-125](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/cursor/client.go#L105-L125)）。cpa-cursor 的测试甚至禁止发 `x-cursor-agent-allowed-tools`（client_test.go:76）。
- YeautyYE 的注释写明：CLI Agent 路径不需要 checksum，只有 Sand 和 IDE 身份才需要（[client.rs:105-113](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/client.rs#L105-L113)）。

**我们现状**：IDE 格式的版本号 `3.21.12`，同时配 `x-cursor-client-type` 为 cli（AgentService）或 sand（InferenceService），另外还带 checksum、windows 系统头和 commit（`buildHeadersWithType`）。这种混合指纹在社区里没人用。

**为什么暂不做**
- `3.21.12` 比 greenSheep 冒充的 IDE `3.19.7` 还新，**没有找到因版本过旧被拒的实证**。
- 唯一一例「版本过旧被拒」发生在 Sand 面：用 `0.44.0` 加 client-source 头时返回 `ERROR_OUTDATED_CLIENT`（[greenSheep docs/sand-grokbot-mechanism.md:178](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/sand-grokbot-mechanism.md#L178)）。
- 没有实测就整体换身份，可能让当前能用的 Sand 和 AgentService 路径一起失效。

**启用前需要怎样实测**
- 新增一个 AgentService 专用的版本环境变量，默认仍为现值，按小流量灰度切换到 `cli-*` 加精简请求头。
- 观察成功率、`ERROR_OUTDATED_CLIENT` 的出现情况，以及风控（封号）反馈。

**改动落点**：`client.go` 中的 `buildHeadersWithType`、`clientVersion`。

## 9. other 桶在 apiPercentUsed≥100 时被误判为耗尽

**是什么**：本仓库把 `GetCurrentPeriodUsage.planUsage.apiPercentUsed >= 100` 直接判为 other 桶耗尽（service 层 `cursorBucketFromPercent`）。

**社区证据**
- [greenSheep quota_route.go:48-62](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/plugin/cursor/kernel/quota_route.go#L48-L62)：只要 total_spend 超过免费额度，`apiPercentUsed` 就恒为 100，即使团队套餐通过按量计费仍在为 Other Models 付费。
- 实测这曾导致团队号上所有 Claude 变体被注销。
- 真耗尽的条件是 `InSlowPool && NoUsageBasedAllowed` 同时成立（[:187](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/plugin/cursor/kernel/quota_route.go#L187)）。两个标志分别来自：
  - `DashboardService/GetUsageLimitStatusAndActiveGrants` 的 `usageLimitPolicyStatus.isInSlowPool`，同一结构里还有 `resetAtMs`；
  - `DashboardService/GetHardLimit` 的 `noUsageBasedAllowed`（[usage/client.go:281-318](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/usage/client.go#L281-L318)）。

**为什么暂不做**
- 需要团队号和开了按量计费的号来实测。本仓库现有的测试账号都是 Pro 或免费号，验证不了。
- 改错的方向是「把真耗尽的号当可用」，代价是每个请求都白撞一次 429。

**启用前需要怎样实测**
- 分别对团队号、按量号、普通 Pro 号拉取这三个接口，记录 `apiPercentUsed`、`isInSlowPool`、`noUsageBasedAllowed`，并与实际能否调用 Claude 对照。

**改动落点**
- `client.go` 新增两个 DashboardService 调用。
- `cursor_usage_fetcher.go` 的 `cursorBucketFromPercent` 改为综合判定。
- 同时用 `resetAtMs` 给耗尽状态加上到期时间，即 [02](02-gateway-ideas.md) 第 8 条。

## 10. 用 turn_ended 的真实用量计费

**是什么**：`InteractionUpdate.turn_ended`（字段 14）里有 1 input、2 output、3 cache_read、4 cache_write、5 reasoning 五个字段（proto 见 [cpa-cursor agent.proto TurnEndedUpdate](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/proto/agent/v1/agent.proto)）。

**为什么暂不做**
- 口径冲突：input 是否包含 cache_read，各家结论相反。
- 一个 run 内多个 step 会累加。
- 工具轮为 0，而本仓库在 tool-call-ready 时就提前结束本轮，根本拿不到字段 14。
- input 中恒定包含约 11k 的 Cursor agent prompt。
- 详见 [02](02-gateway-ideas.md) 第 6 条。

本轮最多做到「解析并记录用于对账」，**计费仍走本地估算**（`estimateCursorInputTokens`、`EstimateOutputUsageFromText`）。

**启用前需要怎样实测**
- 积累至少几百条「无工具、单 step」轮次的对账样本，比较 `input-CR-CW` 与本地 tokenizer 估算的比值是否稳定。
- 确认口径后，再仅对这类轮次采信。

**改动落点**：`agent.go` 的 `consumeAgentResponse` 解析字段 14；service 层把结果写入用量 extra，而不是写进 `ClaudeUsage`。

## 11. CancelAction

**是什么**：客户端断开时，先发 `conversation_action(4){ cancel_action(3){ reason(1) } }` 再断开上游。

**社区证据**
- 没有任何活跃实现在用。YeautyYE 只定义了 `cancel_action=3`，代码中没有引用。
- opencodex 用 `NGHTTP2_CANCEL` 关流，cpa-cursor 关闭 ctx 和管道。
- KonghaYao 明确要求交回 tool_calls 时**不要**发 cancelAction，否则多轮工具会断（CLAUDE.md:17）。
- dimC 中的出处（[ccextra 712f35d5](https://github.com/knoxwh/ccextra/commit/712f35d5)）所在仓库已移除 Cursor 支持。

**结论**：维持现状。本仓库在 `ctx.Done` 后由 defer 的 `cancel()` 关闭请求体，即发出 RST_STREAM，与社区做法一致。唯一可能需要的场景是 SSE 回退路径：那条路径上服务端 run 是否会继续跑，**未核实**。

## 12. checksum 的 base64 字母表

**是什么**：identity 报告怀疑我们用 URL-safe base64（`crypto.go` 的 `b64Alphabet`，字符 `-_`）是错的，应该用标准 base64（`+/`）。

**社区证据**
- 8 个活跃实现都用 URL-safe 的 `-_`，与我们一致：
  - [quangdang46/openproxy](https://github.com/quangdang46/openproxy/blob/0cc228248090350a37ebafbd33d25b46e9590bd9/src/core/utils/cursor_checksum.rs#L32)
  - [bloodf/durindoor](https://github.com/bloodf/durindoor/blob/915e70c22508785b99d1b16c79cb8b857622d9a4/open-sse/utils/cursorChecksum.js#L66)
  - [mydisha/keirouter](https://github.com/mydisha/keirouter/blob/4d47fb92b7614d84c49482af1fcbab538943d908/backend/internal/connectors/cursor_checksum.go#L17)
  - [szxypi/cpa-cursor](https://github.com/szxypi/cpa-cursor/blob/7aac3729e3fdbb924f4d8c58b916e05498b96ecb/checksum.go#L33)
  - [khanhkit/AgentProxy](https://github.com/khanhkit/AgentProxy/blob/cf9a1a04e1d57c32d9b0d74d4a8b68ecfc6f921a/open-sse/utils/cursorChecksum.ts#L70)
  - [F0Rextasy/cursor2api](https://github.com/F0Rextasy/cursor2api/blob/bd79a8f1dd9aeea2cc7ce766ce14d0beafa194cc/src/checksum.js#L12)
  - [diegosouzapw/OmniRoute](https://github.com/diegosouzapw/OmniRoute/blob/219e1e182fecbd9a4a54a200a98267f7cc676c0b/open-sse/utils/cursorChecksum.ts#L70)
  - npm otto-plugin-cursor `src/connect-rpc/headers.ts:68`
- 只有 greenSheep 的文档写的是「标准 base64，不是 url-safe」（[docs/checksum-algorithm.md:55](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/checksum-algorithm.md#L55)）。

**结论**：维持现状。另外，CLI 身份的 AgentService 路径本来就不需要 checksum（第 8 条），这一项只影响 Sand 和 IDE 身份。

## 13. ToS 风险与上游变动

- **ToS**：
  - Cursor 员工在官方论坛明确表示，非官方代理调用私有端点违反 Use Restrictions（[forum 帖子](https://forum.cursor.com/t/does-using-oh-my-pi-s-cursor-provider-or-an-openai-compatible-proxy-to-the-same-endpoints-violate-cursor-s-tos/167778)）。
  - [standardagents/composer-api](https://github.com/standardagents/composer-api) 被要求下架其托管 API。
  - NodeSeek 有用户报告反代 Ultra 或 Grok Bot 后被封号（[帖子](https://www.nodeseek.com/post-910731-1)）。
- **上游变动**：
  - OpenAI 模型于 2026-11-12 退出 Cursor（[CNBC](https://www.cnbc.com/2026/08/29/openai-cursor-spacex-model-access.html)）。
  - 2026-10-12 起推出 X Subscription 套餐，其中 Plus $8 档只含 Grok 和 Composer，第三方模型另外计费（[coworker.ai](https://coworker.ai/blog/cursor-pricing)）。
- **对本项目的影响**：
  - 静态兜底列表 `DefaultModels` 里的 gpt-* 条目会失效。
  - 新套餐的额度结构可能不再符合现有的三桶模型。
  - 账号运营需要面对封号风险，这属于业务决策，不是技术问题。

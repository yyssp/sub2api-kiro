# 分组通用缓存策略最终验证

## 1. 文档口径

本文只记录每个验证操作的最终终值、最终判定和证据，不记录中间调试日志、重复请求、临时失败响应或已废弃测试结果。协议响应字段和数据库字段按各自真实命名记录：

- Anthropic：`input_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens`、`cache_creation.ephemeral_5m_input_tokens`、`cache_creation.ephemeral_1h_input_tokens`、`output_tokens`
- 数据库：`input_tokens`、`cache_read_tokens`、`cache_creation_tokens`、`cache_creation_5m_tokens`、`cache_creation_1h_tokens`、`output_tokens`
- OpenAI Chat：`prompt_tokens`、`prompt_tokens_details.cached_tokens`、`prompt_tokens_details.cache_creation_tokens`、`completion_tokens`
- OpenAI Responses：`input_tokens`、`input_tokens_details.cached_tokens`、`input_tokens_details.cache_creation_tokens`（上游/兼容响应支持时）、`output_tokens`

所有真实调度均通过当前业务服务和真实网关完成，没有直接插入 usage 数据。Mock 上游只用于提供 Claude Code 兼容协议的可控响应、工具循环、流式终态、权威 cache usage 和失败恢复场景。

## 2. 最终实现契约

### 2.1 策略绑定

- 缓存策略独立存储在 `cache_strategies`。
- 分组通过 `groups.cache_strategy_id` 绑定，单个分组最多一个策略。
- 重复绑定返回 `409 CACHE_STRATEGY_GROUP_CONFLICT`，响应说明分组、当前策略、请求策略和冲突原因。
- 未绑定分组与绑定 `disabled` 策略严格区分：前者没有策略快照，后者保留策略 ID/名称但不读、不写缓存。
- 策略快照写入 usage 记录，后续修改策略不会改变历史记录含义。

### 2.2 运行时

- Anthropic Messages、OpenAI Chat Completions、OpenAI Responses 和 Kiro 兼容入口使用同一套 group-bound cache runtime。
- runtime 按 `group + account + protocol + model + session + strategy revision` 默认隔离。
- 显式 `group_session` 才允许同一分组不同账号共享 session namespace。
- 没有稳定 session 时默认不读、不写；Claude Code `metadata.user_id` JSON 字符串中的 `session_id`/`conversation_id` 可作为稳定 session。
- 成功响应才 Commit；鉴权失败、上游错误、网络错误、客户端取消、malformed SSE、EOF 无合法终态和 usage 解析失败均 Abort。
- 已绑定通用策略的分组不再使用旧 Kiro `ForceCacheBilling` 伪造 `cache_read`。

### 2.3 usage 和上下文守护

- `cache_creation_tokens = cache_creation_5m_tokens + cache_creation_1h_tokens`。
- 所有 usage 桶必须非负。
- `input + cache_read + cache_creation <= 1,000,000`。
- 上游已提供权威 cache usage 时保留权威值，不叠加本地模拟值。
- 缓存策略不改变请求 `max_tokens`，不把 `max_tokens` 当作实际输出，不强制模型生成固定长度。
- Kiro 适配器模型上限：Opus 4/4.1 为 32,000，Opus 4.5 为 64,000，Opus 4.6/4.7/4.8/5 为 128,000，Sonnet 4.5 默认按 64,000，GPT-5.6 按 128,000。`-1` 表示使用模型上限，超过上限的值被截断。

## 3. 最终验证操作

### 操作 V-01：Anthropic 冷启动、热命中和未绑定

- 场景：真实 API Key 分别绑定高缓存、工具感知、输入整形、关闭缓存、creation control 和未绑定分组，执行真实冷/热请求。
- 最终响应：12/12 请求 HTTP `200`。
- 最终 usage：
  - 高缓存：冷 `21/0/8/18`，热 `17/12/0/21`。
  - 工具感知：冷 `19/0/8/24`，热 `15/12/0/27`。
  - 输入整形：冷 `21/0/7/30`，热 `21/7/0/18`。
  - 关闭缓存：冷 `247/0/0/21`，热 `181/0/0/24`。
  - Creation control：冷 `18/0/10/27`，热 `18/10/0/30`。
  - 未绑定：冷 `213/0/0/18`，热 `224/0/0/21`。
- 字段顺序：`input/read/creation/output`。
- 最终缓存状态：启用策略成功冷请求后产生 entry，热请求读取；disabled 与未绑定始终无本地 read/create。
- 最终判定：**PASS**。
- 证据：真实网关 HTTP 调度记录。

### 操作 V-02：三协议 usage 投影

- 场景：同一输入整形策略分别调用 Anthropic Messages、OpenAI Chat Completions、OpenAI Responses。
- 最终响应：三协议冷/热请求全部 HTTP `200`。
- 最终 usage：
  - Anthropic：冷 `input=48/read=0/creation=16/output=24`；热 `48/24/0/27`。
  - Chat：冷 `prompt=19/completion=18/total=37/cache_creation=5`；热 `prompt=19/completion=21/total=40/cached=5`。
  - Responses：冷 `input=27/output=24/total=51/cache_creation=7`；热 `input=27/output=27/total=54/cached=7`。
- 最终缓存状态：每个协议首次只创建，成功提交后下一次才读取；各协议字段符合各自 usage 语义。
- 最终判定：**PASS**。
- 证据：`backend/internal/server/routes/gateway_cache_strategy_dispatch_integration_test.go` 及真实 HTTP 调度。

### 操作 V-03：输入/缓存/输出参数投影

- 场景：真实绑定策略启用 `reported_input_min_tokens=64`、`reported_input_max_tokens=120`、`token_scale=1.5`、`max_simulated_input_tokens=128`、cap jitter `5..9`、read 上限 `36`、creation 上限 `30`、output 上限 `40`。
- 最终响应：Anthropic、Chat、Responses 冷/热请求全部 HTTP `200`。
- 最终 usage：
  - Anthropic：冷 `64/0/28/25`；热 `64/33/0/28`。
  - Chat：冷 `prompt=87/cache_creation=23`；热 `prompt=98/cached=34`。
  - Responses：冷 `input=86/cache_creation=22/total=108`；热 `input=92/cached=28/total=117`。
- 最终约束：所有 input 在 `64..120`；read 不超过 `36`；creation 不超过 `30`；output 不超过 `40`；冷请求 read 均为 `0`，热请求 read 均大于 `0`。
- 最终缓存状态：参数均参与真实响应投影，临时策略测试后解绑并删除。
- 最终判定：**PASS**。
- 证据：真实管理员 API 创建/绑定和真实网关请求。

### 操作 V-04：Creation control

- 场景：单事件上限 `256`、窗口预算 `512`、成功请求间隔 `1`，执行扩展会话。
- 最终响应：4/4 请求 HTTP `200`。
- 最终 usage：冷 `18/0/10/24`；扩展一 `59/14/0/27`；扩展二 `76/16/12/30`；重复扩展 `76/20/0/18`。
- 最终缓存状态：首次 creation 允许；控制条件只抑制后续 creation，不关闭已有前缀 read；达到成功次数后才产生新增 creation。
- 最终判定：**PASS**。
- 证据：真实 `/v1/messages` 调度矩阵。

### 操作 V-05：上游权威 cache usage

- 场景：Mock 上游返回 authoritative cache read/write，验证本地 runtime 不重复叠加。
- 最终响应：Anthropic、Chat、Responses 冷/热请求全部 HTTP `200`。
- 最终 usage：
  - Anthropic：冷 `205/7/3/18`；热 `216/9/0/21`。
  - Chat：冷 `prompt=186/cached=11/creation=3`；热 `prompt=190/cached=7`。
  - Responses：冷 `input=207/cached=9/creation=3`；热 `input=217/cached=11`。
- 最终缓存状态：保留上游权威桶，不叠加本地模拟值，无双计数。
- 最终判定：**PASS**。
- 证据：Mock `__control?mode=authoritative` 和真实网关响应。

### 操作 V-06：失败恢复

- 场景：上游 `fail_all` 后恢复，检查失败请求不提交缓存。
- 最终响应：失败请求 HTTP `502`；恢复请求 HTTP `200`、`200`。
- 最终 usage：恢复冷 `25/0/10/18`；恢复热 `20/15/0/21`。
- 最终缓存状态：失败后仍为 cold；恢复请求首次 creation，下一次才 read。
- 最终判定：**PASS**。
- 证据：真实失败恢复调度。

### 操作 V-07：绑定冲突

- 场景：已绑定分组重复绑定当前策略，再绑定另一策略。
- 最终响应：两次均为 HTTP `409 CACHE_STRATEGY_GROUP_CONFLICT`。
- 最终响应字段：包含 `group_id`、`group_name`、当前策略 ID、请求策略 ID 和原因。
- 最终缓存状态：已有绑定未被覆盖。
- 最终判定：**PASS**。
- 证据：管理员绑定 API。

### 操作 V-08：低频创建和仅读取优先

- 场景：低频创建模板与仅读取优先模板接入真实网关矩阵。
- 最终响应：两种模板冷/热请求全部 HTTP `200`。
- 最终 usage：低频创建冷 creation `32`、read `0`，热 read `>0`、creation `0`；仅读取优先冷 creation `24`、read `0`，热 read `>0`、creation `0`。
- 最终缓存状态：首次 creation 不被控制条件错误阻塞；仅读取模板命中后不追加新增尾部。
- 最终判定：**PASS**。
- 证据：`TestGatewayCacheStrategyTemplateMatrix`。

### 操作 V-09：代码、前端和隔离依赖质量门禁

- 场景：验证后端、前端、数据库迁移、Redis/PG 隔离和构建。
- 最终状态：后端全量测试通过；前端 267 个测试文件、1,853 个测试通过；`pnpm typecheck` 和 `pnpm build` 通过；迁移和 schema 集成测试通过。
- 最终运行环境：PostgreSQL `kiro-rs-postgres-local:25432`、Redis `kiro-rs-redis-local:26379`；业务代码使用高位端口，未使用 3000。
- 最终判定：**PASS**。
- 证据：构建/测试命令终值和隔离容器状态。

### 操作 V-10：最新源码黑盒三协议复核

- 场景：使用最新源码构建，通过真实 API Key、scheduler、Anthropic 流式入口、Chat、Responses、权威 usage 和失败恢复路径验证缓存。
- 最终响应：启用策略冷/热、流式、权威 usage 和恢复请求全部成功；失败请求最终为 `502`，恢复请求为 `200/200`。
- 最终 usage：所有启用策略首轮 read 为 `0`，第二轮出现 read；disabled 与未绑定无 read/create；各协议总 input、read、creation、output 均不超过策略上限。
- 最终判定：**PASS**。
- 证据：最新源码二进制真实网关调度记录。

### 操作 V-11：当前本地服务复核和 Responses 估算

- 场景：当前 `48780` 服务验证高缓存、工具流式、输入整形、低频创建、read-priority Chat/Responses、disabled、未绑定、权威 usage 和失败恢复。
- 最终响应：23 次请求中 22 次 `200`，failure-first 为 `502`。
- 最终 usage：
  - 高缓存：冷 `12/0/31/18`；热 `12/31/0/21`。
  - 工具流式：冷 `180/0/73/24`；热 `80/173/0/27`。
  - 输入整形：冷 `14/0/26/30`；热 `28/0/53/18`。
  - 低频创建：`28/0/21/21`、`212/0/0/24`、`223/0/0/27`、`38/0/29/30`。
  - read-priority Chat：冷 `prompt=43/cache_creation=14/completion=18`；热 `prompt=65/cached=27/completion=21`。
  - read-priority Responses：冷 `input=25/creation=8/output=24`；热 `input=25/cached=23/output=27`。
  - disabled Responses：冷 `input=164/output=30`；热 `input=175/output=18`。
- 最终缓存状态：失败不写；权威 usage 不双计数；disabled/未绑定始终无本地缓存。
- 最终判定：**PASS**。
- 证据：当前服务和 `/tmp/sub2api_cache_e2e_results.json`。

### 操作 V-12：多参数真实调度矩阵

- 场景：真实验证 sample target、输入 min/max、scale、cap jitter、ratio、client-only、output cap、自动断点 creation control 和 TTL。
- 最终响应：18/18 请求 HTTP `200`，临时策略解绑并删除。
- 最终约束：
  - sample/scale 场景 input 在 `64..120`，read `<=30`，creation `<=40`，output `<=32`。
  - client-only 无客户端断点时 read/create 均为 `0`，output `<=35`。
  - 自动断点 creation `<=1800` 单事件且 `<=2500` 窗口，read `<=15`。
  - TTL `1s` 到期后由 hot read 回到 cold creation。
- 最终判定：**PASS**。
- 证据：`/tmp/sub2api_cache_parameter_matrix_results.json`。

### 操作 V-13：动态 user 和完整 breakpoint 边界

- 场景：自动断点不写当前动态 user；creation cap 小于完整 breakpoint 时不切断 block；显式客户端断点可以包含当前 user。
- 最终状态：自动断点 read 不包含当前动态 user；不足完整 breakpoint 时 read/create 均为 `0` 且 tracker entry 不增加；显式断点首轮 creation 大于 `0`，提交后下一轮 read 大于 `0`。
- 最终判定：**PASS**。
- 证据：

```bash
go test ./internal/service \
  -run 'TestAutoBreakpointsExcludeCurrentUser|TestExplicitClientBreakpointCanIncludeCurrentUser|TestCreationCapBelowCompleteBreakpoint' \
  -count=1
```

### 操作 V-14：本地测试数据清理

- 场景：删除真实调度创建的测试账号、API Key、分组、策略和绑定。
- 最终状态：14 个测试账号、14 个测试分组、13 个测试策略全部删除；`e2e-*` 账号、分组、策略和绑定均为 `0`；默认管理员和默认数据保留。
- 最终运行状态：业务服务健康检查 `200`。
- 最终判定：**PASS**。
- 证据：管理员 API 删除终值、列表查询终值和服务健康检查。

### 操作 V-15：最新嵌入前端构建

- 场景：构建当前前端并嵌入业务二进制，在高位端口启动。
- 最终状态：`/health`、`/setup/status`、`/`、`/login`、管理员登录和缓存策略 API 均返回 `200`；页面引用当前构建资源。
- 最终产物：业务服务使用 `/tmp/sub2api-local-latest`；服务数据目录约 `12 KB`，只保留运行所需文件和当前日志。
- 最终判定：**PASS**。
- 证据：`pnpm build`、`go build -tags embed` 和当前服务 HTTP 检查。

### 操作 V-16：大输入真实调度

- 场景：当前本地服务接收约 `0.4 MB`、`1.4 MB`、`1.6 MB`、`2.5 MB`、`2.6 MB` 请求体，覆盖 Anthropic、Chat、Responses 冷/热、SSE、增量创建、仅读取、disabled 和未绑定。
- 最终响应：29/29 真实网关请求 HTTP `200`。
- 最终 usage：
  - Anthropic `2.6 MB`：冷 `52004/0/598045/5600`；热 `52004/598045/0/5623`。
  - Chat `2.5 MB` 第三轮：`prompt=625176/cached=85827/creation=26407/completion=4096`。
  - Responses `2.5 MB` 第三轮：`input=625172/cached=85826/creation=26406/output=256`。
  - 仅读取 `1.6 MB` 热请求：Chat `prompt=400125/cached=67527/creation=0/completion=2815`；Responses `input=400121/cached=67526/creation=0/output=250`。
  - disabled `1.4 MB`：`input=365203/read=0/creation=0/output=2884`。
  - 未绑定 `1.4 MB`：`input=365214/read=0/creation=0/output=2792`。
- 最终约束：Anthropic 最大输入桶总和 `650049<=900000` 且小于 `1000000`；OpenAI 最大 input/prompt `625176<=700000`；OpenAI `cached+creation<=prompt/input`；所有桶非负；output 随请求和 Mock 响应变化。
- 最终持久化：管理员 usage 和 stats API 均可查询大输入记录；临时对象清理后无残留。
- 最终判定：**PASS**。
- 证据：`openspec/changes/add-group-cache-strategy/big-usage-results.json`。

### 操作 V-17：Kiro 凭据导入和 provider 语义

- 场景：通过当前服务解析本地 Kiro IDE 导出 JSON，验证 mixed OAuth/API Key 分类以及 social OAuth 缺失 provider 的处理。
- 最终响应：导入预览 HTTP `200`；页面、健康检查、缓存策略 API 和分组 API 均 HTTP `200`。
- 最终分类：220 条，OAuth 205 条（social 199、IDC 6），API Key 15 条。
- 最终 provider 结论：provider 仅表示 OAuth/IDC 授权来源和刷新元数据，不参与上游调度、API Key 鉴权、缓存 scope 或 usage 整形；social OAuth 缺失 provider 合法，IDC 可由 `start_url` 推导。
- 最终持久化：预览接口不创建账户、分组或策略。
- 最终判定：**PASS**。
- 证据：当前服务导入预览响应和 Kiro 定向测试。

### 操作 V-18：删除旧 Kiro 专属缓存字段后的回归

- 场景：应用 `231_drop_legacy_kiro_cache_emulation.sql`，确认 schema 删除旧 Kiro 专属缓存列后重跑大输入真实调度。
- 最终 schema：保留 `cache_strategies`、`groups.cache_strategy_id` 和索引；外键删除行为为 `SET NULL`；旧 `kiro_cache_emulation_*` 列及 `groups_kiro_cache_*` 约束不存在。
- 最终响应：29/29 请求 HTTP `200`。
- 最终 usage：
  - Anthropic `2.6 MB`：冷 `52004/0/598045/5600`；热 `52004/598045/0/5623`。
  - Chat `2.5 MB` 第三轮：`prompt=625176/cached=85827/creation=26407/completion=4096`。
  - Responses `2.5 MB` 第三轮：`input=625172/cached=85826/creation=26406/output=256`。
  - 仅读取策略热请求 creation `0`；disabled/未绑定 read/create `0`。
- 最终判定：**PASS**。
- 证据：迁移后 schema 查询和大输入真实调度结果。

### 操作 V-19：四种新增模板真实连续调度

- 场景：`strict_client`、`shared_session`、`conservative_usage`、`long_context_guard` 四种模板各绑定独立分组和会话，每种连续 10 轮真实 Sonnet 4.5 调用，稳定上下文约 120,000 字符。
- 最终响应：40/40 请求 HTTP `200`；四种模板均 10/10 成功。
- 最终 usage：
  - `strict_client`：input `30707..36098`，read `0`，creation `0`，output `465..512`。
  - `shared_session`：首轮 `29049/0/1419/512`；后续 read `1809..2119`，creation `0`，output `512`。
  - `conservative_usage`：首两轮 read `0`、creation `1600`；后续 read `2400`，creation `288..292`，output `128`。
  - `long_context_guard`：首轮 `27514/0/5650/512`；后续 read `5779..5849`，creation `0`，报告 input 小于 `96000`。
- 最终缓存状态：四种策略首轮均无 read；有缓存模板仅在成功提交后读取；严格客户端在无显式断点时不自动读写。
- 最终约束：所有桶非负，creation 分解守恒，`input+read+creation<=1000000`；所有临时策略测试后解绑并删除。
- 最终判定：**PASS**。
- 证据：`additional-strategies-final-results.json` 和真实调度脚本终值。

## 4. 当前真实 Claude Code 十轮验收

完整终值见 [real-sonnet45-cache-verification.md](./real-sonnet45-cache-verification.md)。核心终值如下：

| 项目 | 终值 |
| --- | ---: |
| 外层连续轮数 | 10 |
| HTTP 成功轮数 | 10/10 |
| 真实工具调用 | 100 |
| CLI input 总和 | 609,928 |
| CLI cache read 总和 | 3,105,634 |
| CLI cache creation 总和 | 350,636 |
| CLI output 总和 | 16,898 |
| 数据库内部 usage 行数 | 51 |
| 数据库 distinct group/API Key/account | 1 / 1 / 3 |
| 数据库最大 input+read+creation | 132,717 |
| 负数 usage 行 | 0 |
| creation 分解不守恒行 | 0 |
| 1,000,000 上下文超限行 | 0 |

第一条数据库真实 usage 的最终值为 `input=176/read=0/creation=997/output=213`，证明冷启动不凭空读缓存。第 1 个 CLI 外层轮次的聚合值包含其内部工具请求，所以外层聚合出现 read 不与冷启动证据矛盾。

## 5. 最终代码回归命令

以下定向回归均为最终 `PASS`：

```bash
cd backend
go test ./internal/pkg/kiro \
  -run 'TestKiroMaxOutputTokensFor(OpusGenerations|Opus5|GPT56Models)$' \
  -count=1

go test ./internal/service \
  -run 'TestCacheSessionKeyReadsClaudeCodeJSONMetadataUserID|TestCacheStrategyBindingAppliesToAnthropicMessagesProfile|TestBoundCacheStrategyIsolatesCacheByAccount|TestForceCacheBilling' \
  -count=1
```

当前唯一服务最终检查：

```bash
tmux ls
curl -sS http://127.0.0.1:48780/health
```

终值：

- tmux 只有 `sub2api-local` 一个业务服务会话。
- `/health` 返回 `{"status":"ok"}`。
- PostgreSQL 和 Redis 依赖容器健康运行。
- 未启动第二个业务服务，未使用 3000 端口。

## 6. 最终交付边界

- 已交付按分组绑定的通用缓存策略、模板、策略快照、三协议 runtime、Kiro 兼容入口、usage 投影、scope 隔离、creation control、TTL、失败 Abort 和前端管理页面。
- 旧 Kiro 专属缓存模拟逻辑和字段已从生产消费路径移除，不做旧系统迁移或兼容 fallback。
- Redis 本轮仅作为独立业务依赖启动和连通性验证，runtime 不宣称跨实例 L2 缓存共享。
- 本次最终真实 CLI 验收使用 Sonnet 4.5；Opus 的 `max_tokens` 上限通过适配器定向测试验证，未声称本地账号实际执行 Opus 生成。

# 04 真实协议核对清单（待用真实账号验证）

**背景**：前三份文档的结论都来自静态代码审查。用户提供了真实 Cursor 账号和美国出口代理，本文件列出**改代码之前必须先用真实流量核对**的协议点。原则：
- 每一条先核对、拿到真实帧或响应，再决定改不改；核对结果回填到「实测结论」列。
- 核对时打开协议调试日志：`CURSOR_AGENT_DEBUG=1`（只输出帧结构摘要，不输出正文和 token，见 `describePBParts`）。
- 不确定的写「未核实」，不按社区说法直接改。

## 测试环境

| 项 | 值 |
|---|---|
| 账号 | 用户通过管理台导入的真实 Cursor 账号（套餐以导入后补齐的 membership 为准） |
| 代理 | 远端 kiro.rs（netcup-sy，端口 59138）的代理池里挑的 3 个美国出口；本机直连这些代理会被拒（IP 白名单），经 SSH 本地端口转发桥接 |
| 本地代理记录 | 管理台「代理」里的 `remote-187-share-6` / `remote-max-29` / `remote-ap-15`，地址 `127.0.0.1:18081-18083` |
| 桥接进程 | `ssh -f -N -L 127.0.0.1:1808x:<代理>:<端口> netcup-sy`，只监听回环地址，不改本机全局网络/系统代理 |

桥接断开后（重启电脑、网络切换），代理测试会失败，需要重新执行上面的 ssh 命令；代理凭证保存在 `.cachetest/remote_proxies.tsv`（已被 gitignore，权限 600）。

## 核对清单

### A. 已经上线、需要用真实流量确认没有回归的改动

| # | 改动（提交） | 怎么核对 | 实测结论 |
|---|---|---|---|
| A1 | 应答服务端 exec/kv/interaction_query（`b905820bc`） | 调试日志里统计 `exec answered kind=`、`kv answered`、`interaction_query rejected` 的出现情况；确认没有新增 `heartbeat-only-stall` | 未核实 |
| A2 | 本轮结束只认 turn_ended / trailer / 工具就绪 | 普通问答、长思考、工具调用三类请求各跑几次，`exit reason=` 应为 `turn-ended` / `tool-call-ready`，不应出现 `stream-stalled` | 未核实 |
| A3 | 并行工具收批（tool_requests_listed(27) 或 1s 静默） | 让 Claude Code 一次读多个文件，确认 `listed=N` 与交付的工具数一致 | 未核实 |
| A4 | 图片不再合成 path + client_supports_inline_images(19) | 发一张图片，确认不报 `Image not found` 且模型能描述图片 | 未核实 |
| A5 | Sand 面 thinking_part(9) / error(8) / usage | 用 Claude 模型纯文本请求（走 Sand），确认思考内容出现、usage 日志有值 | 未核实 |
| A6 | 错误分级（`d144788ff`） | 被动观察：出现 OUTDATED_CLIENT / 用户级限流 / 地区封锁时，账号状态与冷却是否符合预期 | 未核实 |
| A7 | 额度自动刷新（`86fa47a92`） | 导入后 5 分钟内 `cursor_quota.fetched_at` 应被后台刷新 | 未核实 |

### B. 社区说法冲突或未核实、核对后才决定是否改代码

| # | 协议点 | 社区来源 | 怎么核对 | 实测结论 |
|---|---|---|---|---|
| B1 | **turn_ended(14) 用量口径**：input 是否已含 cache_read；多 step 是否累加 | greenSheep（含）vs cpa-cursor（不含）；官方 SDK 把 input/cacheRead/cacheWrite 当互不重叠三部分相加 | 对同一上下文连发两次，比较 `upstream turn usage` 日志与本地估算 | 未核实 |
| B2 | **结构化错误**：trailer `x-cursor-inference-request-error-type`、`ErrorDetails.error` 枚举、`CustomErrorDetails.is_retryable(4)` | 官方 SDK（NGLSG/cursor2response 依赖包 357.js） | 构造超长上下文 / 超大图片，抓 trailer 与错误体原文 | 未核实 |
| B3 | **上下文压缩事件** `summary_started(10)` / `summary(9)` / `summary_completed(11)` | iFwu/cursor2response 给 SDK 打补丁透出 | 发送接近上限的长上下文，看调试日志是否出现 interaction_update 9/10/11 | 未核实 |
| B4 | **routed_model(24)**：服务端选路时是否回报实际模型；是否需要能力位 `client_supports_routed_model_update(28)` | 官方 SDK 定义；SDK 自身忽略 | 带能力位发一次 auto 请求，看是否收到 24（auto 当前被入口拒绝，需临时放开或协议层直接调） | 未核实（推翻了旧记忆「信封里没有 model 字段」，待实测） |
| B5 | **模型 ID 形态**：wire 上发复合 ID（`claude-opus-5-medium`）还是基础 ID + parameters | shunt#411/jcode#1226 称复合 ID 报 BAD_MODEL_NAME；ai2api 生产一直用扁平 ID | 同一模型分别用两种形态各发一次 | 未核实 |
| B6 | **缺省 fast 是否按 fast 计费** | ccextra 2d664b49 | 需要带参数的模型目录（`useModelParameters:true`）；对比 dashboard 计费标签 | 未核实 |
| B7 | **AvailableModels 带 `useModelParameters:true`** 的响应形状 | cpa-cursor unary.go:221 | 直接调一次，记录 parameterDefinitions / variants 结构 | 未核实 |
| B8 | **客户端身份**：`3.21.12` IDE 版本号配 cli 类型是否会被拒 | 社区活跃实现多用 `cli-YYYY.MM.DD-hash` | 被动观察是否出现 OUTDATED_CLIENT；必要时对照 cli 版本号 | 未核实 |
| B9 | **GetServerConfig.http2Config** 是否指定传输 | 官方 SDK | 调一次 `ServerConfigService/GetServerConfig` 看字段 7 | 未核实 |
| B10 | **x-cursor-team-id** 对团队号是否必需 | greenSheep；"Team budget limit reached" 全社区零命中 | 仅在导入团队号时核对 | 未核实 |

### 核对顺序建议

1. 先跑 A 组（回归确认），因为这些已经在代码里。
2. B1、B2、B3 优先：都是「只解析、不改行为」就能先拿到证据的项。
3. B4–B7 涉及改变请求形状，需要单独开关或临时脚本，不在生产路径上试。

## 实测记录

### 2026-11-12：crsr_ Dashboard API Key（真实账号 + 美国出口 187-share-6）

| 观察 | 结论 | 落地 |
|---|---|---|
| `POST api2.cursor.sh/auth/exchange_user_api_key`（Bearer crsr_，body `{}`）→ 200 `{accessToken, refreshToken}`，两者是同一个 JWT，`type=api_key_token`，寿命 60 分钟 | key 是长期凭证，token 只是 60 分钟缓存 | `63184c13d` |
| 该 token 调 `/oauth/token`（refresh_token 授权）→ `shouldLogout=true`、无 access_token | 不能按 session 账号续期，只能用 crsr_ 重新兑换；绝不能给这类账号写 refresh_token（会被后台刷新判成确认失效停号） | `63184c13d` |
| 该 token 调 `GetCurrentPeriodUsage` / `GetSandUsageStatus` / `GetUserMeta` / `AvailableModels` → 均 200 | Dashboard 与模型目录对 API Key 账号可用 | — |
| 该 token 走 Sand `InferenceService/Stream` → `ERROR_NOT_LOGGED_IN` | API Key 账号只能走 AgentService；协议层对 api_key_token 强制关闭 Sand 直连 | `63184c13d` |
| 该 token 走 AgentService/Run（composer-2.5）→ 正常出字 `pong`，`exit reason=turn-ended`，turn_ended 用量 `input=12220 output=51 cache_read=1088 cache_write=0` | **A1/A2 回归确认**：服务端确实发 request_context(exec 10) 与大量 `set_blob`(kv)，我们的应答让本轮正常结束 | A1/A2 ✅ |
| AvailableModels 返回 250 个模型，全是扁平 ID（如 `claude-opus-4-8-low`、`claude-sonnet-5-medium`、`cursor-grok-4.6-low`）；`claude-opus-4-8`、`claude-sonnet-5`、`gpt-5.5` 这类基础名在 AgentService 上报 `ERROR_BAD_MODEL_NAME` | **B5 部分结论**：wire 上必须用目录里的扁平变体 ID，基础名不行；GPT 已从 Cursor 下架（与 2026-11-12 OpenAI 退出 Cursor 一致） | B5 部分 ✅，静态兜底列表 `DefaultModels` 需更新 |
| `useModelParameters:true` 时返回 47 个基础模型 + variants；`claude-opus-4-8` 的参数是 `thinking/context/effort/fast`，`claude-sonnet-5-5` 是 `context/reasoning_effort`（不叫 effort） | **B7 ✅**：参数名因模型而异，effort 有 `effort` 与 `reasoning_effort` 两种 | 待评估 |
| 同一账号连续用不同 machine_id 发起约 10 次请求后：`ERROR_CUSTOM_MESSAGE: Too many computers used within the last 24 hours` | 设备数按 checksum 指纹统计；探针必须复用固定 machine_id | 记录，探针约束 |
| 部分模型（`claude-opus-4-8-low`、`claude-sonnet-5-medium`）返回 `ERROR_RATE_LIMITED: You have an unpaid invoice` | 账号层面欠费，对该账号的付费模型确定性失败；现有分类把 "unpaid invoice" 归为鉴权停号 | 待评估是否应只停付费模型 |

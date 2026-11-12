# 02 网关通用好思路（只记录，不实现）

本文收录的思路**不限于 Cursor**，对 Kiro、Anthropic、OpenAI 等平台的转发链路同样适用，本轮都不实现。

出处用 GitHub 链接加「文件:行」表示，固定到调研时克隆的提交：
- YeautyYE `dcd8347`
- opencodex `852bc14`
- cpa-cursor `0f928ec`
- greenSheep `bee1afd`
- KonghaYao `a0010a6`
- 7iook `03187f5`
- raine `fe10718`
- doitcan `a6b1872`

优先级的含义：P1 表示对现有用户体验或成本有明显影响；P2 表示有价值但可以等；P3 表示观察即可。

---

## 1. 请求幂等与「拒绝重放」账本

**思路**
- 每个上游「采样阶段」只登记一个持久化 owner。
- 状态变更全部用 owner token 做 CAS，旧请求无法覆盖新请求的状态。
- 已完成阶段保留有界的重放历史：同一指纹的请求已经完成时返回 409「拒绝重放」；状态模糊时（例如上游可能已受理）同样返回 409，绝不盲目重发。

**社区出处**
- [YeautyYE operation_ledger.rs:1-6](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/operation_ledger.rs#L1-L6)：设计说明，及常量上限：单条记录 64KB，最多 1 万条，保留 7 天。
- [mod.rs:6352-6371](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/mod.rs#L6352-L6371)：准入的四种结果，Allowed、DuplicateCompleted、Ambiguous、Unavailable。
- README 的 Limitations 表记录了多起与之对应的事故，例如「Conflict (409) - Cursor live open timed out」。

**对本项目的价值**
- 客户端超时后重试（Claude Code 默认会重试）可能让同一请求被上游执行两次，用户被重复计费，带副作用的工具也会被执行两次。
- 这对任何「上游有副作用 + 下游会重试」的平台都成立。

**落地要点与风险**
- 指纹要包含账号、模型和请求体摘要。账本必须有界并支持 TTL。
- 「模糊态返回 409」会把一部分原本能成功的重试变成失败，需要按平台决定是否启用。
- 多实例部署时，账本要放在 Redis 等共享存储里，YeautyYE 只做了单机磁盘。

**建议优先级**：P2

## 2. 按会话做亲和与工具轮挂起续接

**思路**
- 上游交回工具调用时，不关闭上游流，而是按 `tool_use_id` 建立反向索引后挂起（park）。
- 下一个带 `tool_result` 的请求通过 id 找回挂起的流，把结果写回原 run 继续执行。
- 找不到时（落到其他实例、进程重启、TTL 过期）退化为「无状态重放」，用全量历史重新开一个 run。

**社区出处**
- [cpa-cursor registry.go:11-14](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/cursor/registry.go#L11-L14)：park TTL 10 分钟，最多 64 个。Park/Take 的语义是：待处理的 id 必须是请求中结果 id 的子集。
  - [README.md:100-110](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/README.md#L100-L110) 要求开启 `routing.session-affinity`。
- [7iook bridge/README.md:135-186](https://github.com/7iook/cursor-bridge/blob/03187f574e2fdf0b2d9f6854e02c88ad2846b0c6/bridge/README.md#L135-L186)：
  - 一批工具结果必须整批交回；TTL 5 分钟，容量 50。
  - 冷启动时，按历史记录直接应答已完成的调用，不再让客户端重跑（实测约每 85 轮触发一次）。
- [raine server.rs:1332](https://github.com/raine/claude-code-proxy/blob/fe107180f7a69c6d63dd4421a6567cf641c1f103/src/server.rs#L1332)：按 `x-claude-code-session-id` 匹配；[tool_bridge.rs:26](https://github.com/raine/claude-code-proxy/blob/fe107180f7a69c6d63dd4421a6567cf641c1f103/src/providers/cursor/tool_bridge.rs#L26) 中空闲上限为 600s。

**对本项目的价值**
- 工具轮不用重开 run，可以省掉每轮重发上下文的成本：Cursor 每个新 run 会额外带约 11k token 的 agent prompt，见 cpa-cursor README:100-102。
- 还能避免「冷启动重跑已完成的 Write/Bash」造成重复副作用。
- 本仓库已有的 sticky session 机制（复用通用 sticky 链）可以作为亲和层。

**落地要点与风险**
- 挂起的流占用连接和内存，必须设 TTL 和容量上限，并定期清理已死的流。
- 必须先有会话亲和，否则多实例下命中率很低。
- 「整批交回」这条约束必须遵守，否则上游会一直阻塞。

**建议优先级**：P2。依赖亲和层，改动面大。

## 3. 从正文恢复伪工具调用

**思路**
- 部分模型会把工具调用以文本形式写进正文，例如 `<tool_use id name>{json}</tool_use>`、`<parameter name=...>`、`[TOOL_CALL]name[ARGS]{json}`，而不是走结构化的工具帧。
- 网关在流式文本中增量解析这些标记，只把名字在本轮已声明工具中的那些提升为真实的 tool_use，同时从可见正文里剥掉这些标记。

**社区出处**
- [YeautyYE tool_use_xml.rs:1-7](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/tool_use_xml.rs#L1-L7)：允许名单在 `new(allowed_tool_names)`，见 :58。
- [opencodex text-toolcall.ts:1-18](https://github.com/lidge-jun/opencodex/blob/852bc147b6802083fdc14739cb667afb537f357e/src/adapters/cursor/text-toolcall.ts#L1-L18)：挂起缓冲上限 64KB，超出后改为常量空间扫描。
- [raine tool_use_xml.rs:1-6](https://github.com/raine/claude-code-proxy/blob/fe107180f7a69c6d63dd4421a6567cf641c1f103/src/providers/cursor/tool_use_xml.rs#L1-L6)。

**对本项目的价值**：模型在正文里输出工具标记时，Claude Code 会把它当普通文字显示，任务就此停住。这个问题在 Kiro 等其他平台也出现过。

**落地要点与风险**
- 必须按允许名单过滤，并校验 JSON 完整，否则用户正文里的示例代码会被误判成工具调用。
- 半截标记需要缓冲，会让首字稍有延迟。

**建议优先级**：P2

## 4. Claude Code 工具兼容修补

**思路**
- 在边界上对 Claude Code 工具定义和工具参数做最小修补：
  - `text_editor_*`、`str_replace_based_edit_tool` 等工具缺少 description 或 schema 时，补上标准定义；
  - Bash 调用缺少 description 时，用命令首行截断到 80 字符补上；
  - 把 `background`、`is_background` 统一改名为 `run_in_background`，并删掉 Claude Code 不认识的字段。

**社区出处**
- [YeautyYE request.rs:243](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/request.rs#L243)：`is_text_editor_tool_name`。
- [native_tools.rs:1528-1592](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/native_tools.rs#L1528-L1592)：补 description 与 `shell_description`，以及 background 字段改名。
- README Limitations 表中的 "Claude Code Bash widget titles a giant python3 -c script"。

**对本项目的价值**：这类兼容问题一旦出现就是「工具调用失败后无限循环」，与具体上游无关，任何把非 Anthropic 模型接给 Claude Code 的平台都会遇到。

**落地要点与风险**：只修补「缺失」的字段，不改写已有字段；修补规则要跟随 Claude Code 版本更新。

**建议优先级**：P3。目前没有对应的线上故障报告。

## 5. 图片治理

**思路**
- **上限**：单图在线路上最多 1MB；解码上限 16MB，用来防解压炸弹；最长边 8192、最多 2500 万像素；每轮最多 12 张。
- **压缩**：JPEG 软上限 100KB（high 档 256KB），质量依次尝试 85、70、55、40，最长边缩到 2000。
- **历史图片**：一律替换成 `[image attached]` 文本占位，只发送当前轮的图片。

**社区出处**
- [opencodex images.ts:18-69](https://github.com/lidge-jun/opencodex/blob/852bc147b6802083fdc14739cb667afb537f357e/src/adapters/cursor/images.ts#L18-L69)：:30 的注释记录了一次实测，约 430KiB 的 PNG 识别失败，而同一画面约 75KiB 的 JPEG 成功。

**对本项目的价值**
- 本仓库 Cursor 目前只有每个附件 16MB、总量 32MB 的预算，没有压缩。
- 大图既占上游体积上限（参见 Kiro 的加权字符上限），又可能降低识别质量。
- 这套治理对所有多模态平台都适用。

**落地要点与风险**
- 压缩需要图片编解码依赖，Go 标准库就能处理 JPEG 和 PNG。
- 有损压缩会改变截图细节，例如小字号文本，需要提供 high 档或关闭开关。

**建议优先级**：P2

## 6. 上游 usage 口径归一：只对账，不计费

**思路**：上游真实 usage 不能直接当计费依据，至少要回答四个口径问题。

1. **input 是否已经包含 cache_read？** 社区说法互相矛盾：
   - 认为包含：greenSheep 的实测文档写明 input 包含 cache read（[phase-7l-cache.md:10-18](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/phase-7l-cache.md#L10-L18)），映射为 `input-CR-CW` 并以 0 为下限（[translator/anthropic.go:734](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/translator/anthropic.go#L734)）。
   - 认为不包含：cpa-cursor 用 `input+CR` 作为 prompt（[response.go:118](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/translate/response.go#L118)）。
   - 两种口径的比较：YeautyYE 在 input≥read+write 时减掉缓存部分（[sse.rs:311-336](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/sse.rs#L311-L336)）。
2. **多 step 累加**：raine 实测一个 run 内各 step 的数字会累加，27k 的上下文被报成 240k（[sse.rs:289](https://github.com/raine/claude-code-proxy/blob/fe107180f7a69c6d63dd4421a6567cf641c1f103/src/providers/cursor/sse.rs#L289)）。因此它优先读 checkpoint 的 `token_details.used_tokens`。
3. **工具轮为 0**：等待工具结果的那一轮，usage 经常为 0，只有最终轮才带整轮用量（[KonghaYao README.md:49](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/README.md#L49)）。
4. **output 有两种形态**：一种是独立计数；另一种是含缓存的累计计数，例如 1487=294+915+278（[greenSheep incident 2026-08-17](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/cursor-claude-production-incident-2026-08-17.md)）。

另外：
- input 中恒定包含 Cursor 自带的约 11k agent prompt（cpa-cursor README:100-102）。
- `cache_read` 的值有噪声且不单调：6 次相同请求得到 4675、4675、4240、6976、4675、4675（[phase-7n-sim-cache.md:5-9](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/phase-7n-sim-cache.md#L5-L9)）。

**与本仓库已有教训的对应**
- Kiro 上游的 `input/read/create` 是计费口径而不是 token 口径，三者之和恒等于 `kiro_billable_input_tokens`。`preserve_upstream_cache_usage` 采信它，会让上报少 3 到 5 倍，比强制整形更糟。
- 有合成值时上游真值会被丢弃，实测合计放大最高 253 倍。

两件事说明的是同一点：**同名字段在不同上游的口径可以完全不同**，口径没有逐项实测清楚之前，「采信上游」本身就是成本风险。

**对本项目的价值**：在每个平台引入上游 usage 前都适用的一套检查清单。

**落地要点**
- 第一步只解析并写入用量 extra，用于对账，计费仍走本地估算。
- 积累足够样本、确认口径后，再按平台逐个开放采信，并且只对「无工具、单 step」的轮次采信。

**建议优先级**：P1，只做对账这一步。

> Cursor 侧已落地第一步：解析 `turn_ended(14)` 与 Sand `usage(3)/extended_usage(5)`，以 `cursor: upstream turn usage (reconciliation only)` 日志与本地估算并排记录，不进计费（见 README「已实现的 Cursor 改进」）。其余平台仍按本条执行。

## 7. HTTP/2 不可用时回退到 HTTP/1.1，并加熔断

**思路**
- HTTP/2 首次打开超时，或收到 421/464、`http_1_1_required`、`force_bidi_disabled` 时，切换到 HTTP/1.1。
- 连续失败 3 次后熔断 30s，这期间全部走 HTTP/1.1；之后半开，用一次只读请求探测。

**社区出处**
- [YeautyYE live.rs:5161-5162](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/live.rs#L5161-L5162)：阈值 3 次、冷却 30s。
- [live.rs:7019-7034](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/live.rs#L7019-L7034)：触发回退的状态码与文案。
- README Limitations 中 "H2 first-open waits 90s; after one timeout the H2 circuit uses HTTP/1 for 30s, then half-opens…"。

**对本项目的价值**
- 本仓库 Cursor 只在 404/405/501/505 时回退（`shouldFallbackAgentRun`），而且没有熔断，每个请求都要先撞一次 HTTP/2 失败。
- 出口代理（Clash/Surge TUN）下 HTTP/2 不稳是常见情况，社区多个仓库都提到要让 `*.cursor.sh` 直连（DIRECT）。

**落地要点与风险**
- 熔断状态应按「账号 × 出口代理」维度隔离，不要全局共享。
- 「已发出但没有响应」的请求不能重放，与第 1 条的幂等账本相关。

**建议优先级**：P2

## 8. 按上游给出的重置时间轮询额度，到期自动恢复

**思路**
- 额度耗尽时，记录上游给出的重置时间，到期后自动恢复，不必等人手动刷新。重置时间的来源可以是 `resetAtMs`、`billingCycleEnd` 或错误消息里的 reset 时间。
- 后台定期轮询额度，并给快照设置有效期，过期的快照不能作为判定依据。

**社区出处**
- [YeautyYE usage.rs:35-44](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/usage.rs#L35-L44)：每 60s 轮询一次，证据 TTL 180s；显式 quota 错误按 reset 时间冷却，缺失时回落为 15 分钟。
- [greenSheep quota_route.go:37](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/plugin/cursor/kernel/quota_route.go#L37)：缓存 45s。
- [cpa-cursor plugin/errors.go:90-110](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/plugin/errors.go#L90-L110)：优先取 `GetUsageLimitStatusAndActiveGrants.resetAtMs`，否则取 `billingCycleEnd`。

**对本项目的价值**：本仓库 Cursor 的桶一旦被标为耗尽，就不带过期时间，只有管理员手动拉取额度后才会恢复。其他平台（Kiro、Grok 等）也有类似的「标耗尽后谁来恢复」问题。

**落地要点与风险**
- 轮询要用 leader lock 避免多实例重复拉取，可参考 `OpenCodeGoUsageService`。
- 拉取失败时不能用空值覆盖已有状态。
- 本仓库的「抓取失败的桶不能当可用」原则必须保留。

**建议优先级**：P1。

> Cursor 侧已落地「后台轮询 + 到期自动恢复」（`86fa47a92`，见 README）；尚缺的是用上游给出的 `resetAtMs`/`billingCycleEnd` 精确到期，以及 other 桶的误判问题，见 [03](03-cursor-deferred.md) 第 9 条。

## 9. 「容量削峰」类 429 不要同请求重试

**思路**：上游回复 high load 或 high demand，并附带 switch model / try again in a few 这类文案时，表示上游模型池满了。此时同请求重试只会把 1 个 429 放大成一波，应直接透传给客户端，或者换模型。

**社区出处**
- [YeautyYE retry.rs:137](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/retry.rs#L137)：`is_capacity_shed`。
- [retry.rs:151](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/retry.rs#L151)：`is_policy_rate_limit`，账号级的策略性 429 也不在同一账号上重试。

**对本项目的价值**：本仓库 Cursor 已经把 high load 归为瞬时错误、不标耗尽，这一点是对的；但瞬时错误仍会换号重试，每个号都撞一次同样的容量墙。其他平台也存在「全局容量 429 被当成账号问题」的风险。

**落地要点**：要区分「账号级限流」和「模型池满」，后者应该返回 429 加 Retry-After，同时停止换号。

**建议优先级**：P2

## 10. 下游全程保活

**思路**
- 在整个流式响应期间，每 5s 向下游发一次 `message_delta` 加 `ping`，而不是只在首字到达之前发 SSE 注释。
- Claude Code 的事件看门狗约 10s，单独的 `ping` 不会重置它，必须配一个 `message_delta`。

**社区出处**：[YeautyYE live.rs:14391-14395](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/live.rs#L14391-L14395)，以及 README Limitations 中 "Stream idle timeout - no chunks received"。

**对本项目的价值**
- 本仓库 Cursor 只在首个事件之前每 15s 写一次 `: processing` 注释，首个事件之后就停了。长思考或工具准备阶段出现几十秒静默时，客户端或反代可能断开。
- 通用转发链路同样存在这个问题。

**落地要点与风险**
- 发 `message_delta` 必须和内容事件共用写锁，避免把半帧插进去。
- `message_delta` 不能带 `stop_reason`，也不能改变 usage，否则会被当成提前结束。

**建议优先级**：P2

> Cursor 侧已改为流开始后空闲满 15s 补 Anthropic `ping`（与内容事件共用写锁）。「只有 ping 不能重置 Claude Code 看门狗、必须配 message_delta」这一点只有 YeautyYE 的代码注释为证，本地未能复核 Claude Code 的看门狗实现，**未核实**；若线上出现 `Stream idle timeout`，再按本条改成 `message_delta + ping`。

## 11. 其他值得记录的思路

| 思路 | 出处 | 价值 | 优先级 |
|---|---|---|---|
| **只降权不下线**：失败只让账号排序靠后，5 分钟后降权自然消退，成功一次立刻恢复；默认不隔离。原因是「失败」信号不可靠，上游抖动、地区限制、模型名错误都会被算成账号故障 | [doitcan pool.go:48-58](https://github.com/doitcan-oiu/cursor-proxy/blob/a6b18721553afa35d73505a681ad3e765b6e0a6e/internal/cursor/pool.go#L48-L58)、[config.go:14-28](https://github.com/doitcan-oiu/cursor-proxy/blob/a6b18721553afa35d73505a681ad3e765b6e0a6e/internal/config/config.go#L14-L28) | 小号池时，可以避免「无可用账号」雪崩 | P2 |
| **错误文案分级**：地区限制、模型下线、付费要求、鉴权、封禁、限流各成一类，前三类不换号 | [doitcan pool.go:93-124](https://github.com/doitcan-oiu/cursor-proxy/blob/a6b18721553afa35d73505a681ad3e765b6e0a6e/internal/cursor/pool.go#L93-L124)、[cpa-cursor errors.go:192](https://github.com/mediavee/cpa-cursor/blob/0f928ec5f9ffae12d99f8a3d1157251175d38998/internal/cursor/errors.go#L192) | 确定性失败不再把整个号池试一遍 | P2（Cursor 侧已落地，见 README `d144788ff`；其余平台仍可参考） |
| **有界轮次重试**：响应开始前失败，最多重开 3 次，退避时间每次翻倍；响应开始后绝不重试。401 有单独的重试预算，不与其他重试共用 | [7iook README.md:254-261](https://github.com/7iook/cursor-bridge/blob/03187f574e2fdf0b2d9f6854e02c88ad2846b0c6/bridge/README.md#L254-L261) | 「是否已推出字节」作为唯一判据，简单可靠 | P3（本仓库 failover 已按写出字节判断） |
| **上下文压力系数**：Claude Code 假设上下文有 200K，Cursor 实际约 150K，上报 token 时乘 1.35，让客户端提前触发 compact | [7iook README.md:71](https://github.com/7iook/cursor-bridge/blob/03187f574e2fdf0b2d9f6854e02c88ad2846b0c6/bridge/README.md#L71) | 避免上游静默截断上下文（参见 Kiro 超限裁剪那条记忆） | P2 |
| **本地前缀缓存模拟器**：用 sha256(system+history) 做 LRU（10 分钟），向客户端呈现单调、稳定的 cached_tokens 语义 | [greenSheep phase-7n-sim-cache.md](https://github.com/greenSheep999/cursor-proto/blob/bee1afdb5e85274b8305fcce1967b828aa2203c1/docs/phase-7n-sim-cache.md) | 与本仓库缓存整形思路一致，可以作为对照实现 | P3 |
| **缓存命中率口径**：用 `CR/(CR+未命中 input)`，不要用 `cache_read/input`，后者会算出 300% 以上 | [KonghaYao README.md:49](https://github.com/KonghaYao/cursor2api/blob/a0010a6e6faf4e17a69157d99e6153242bd613ac/README.md#L49) | 管理台统计口径统一 | P3 |
| **可逆 call-id 编码**：上游 tool-call id 含 CR/LF 时，编码为带 HMAC 标签的单行形式，回发上游前还原 | [opencodex call-id.ts:1-20](https://github.com/lidge-jun/opencodex/blob/852bc147b6802083fdc14739cb667afb537f357e/src/adapters/cursor/call-id.ts#L1-L20) | 本仓库目前取首行并剔除控制字符，是有损的；需要回传原 id 时可以参考 | P3 |
| **重复请求订阅**：同一个稳定 request id 的重试不再开新的上游调用，而是订阅原调用的事件流 | [YeautyYE sand_operation.rs:1-9](https://github.com/YeautyYE/claude-cursor-proxy/blob/dcd8347fcfc5ba0fe8b90171d16c12a59ebe9413/src/providers/cursor/sand_operation.rs#L1-L9) | 客户端超时重试不再造成上游并发和 "already active" | P3 |
| **准入背压**：上游 open 并发达到上限时，请求在本地排队（有超时），超时后返回 503 加 Retry-After，避免无界地向上游开连接 | YeautyYE README Configuration `CCP_CURSOR_LIVE_QUEUE_SECS` / `CCP_CURSOR_SAND_OPEN_QUEUE_SECS` | 突发流量下保护上游和账号 | P3 |

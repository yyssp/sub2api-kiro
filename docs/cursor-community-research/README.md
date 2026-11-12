# 社区 Cursor 订阅反代调研

**调研日期**：2026-11-12
**范围**：收集 GitHub 及站外（npm、Docker Hub、论坛）社区中**仍在维护**、能够反代 Cursor 订阅的实现，并与本仓库 `backend/internal/pkg/cursor/` 和 `backend/internal/service/cursor_*.go` 逐项对比。分工如下：
- 与 Cursor 协议直接相关、证据充分的改进，**直接改代码**；
- 证据不足、需要实测的 Cursor 项，以及不限于 Cursor 的网关通用思路，**只记录在本目录，不实现**。

> 所有结论都来自静态代码审查（克隆后用 grep 核对 RPC 路径、帧格式、字段号），**没有用真实账号做线上联调**。「未核实」表示没有找到可复现的证据。

## 文件

| 文件 | 内容 |
|---|---|
| [01-sources.md](01-sources.md) | 调研方法和覆盖统计；按 A（协议/代码检索）、B（网关与 fork）、C（GitHub 之外）三个维度，列出未过时的仓库及其活跃度、实现方式、结论 |
| [02-gateway-ideas.md](02-gateway-ideas.md) | 不限于 Cursor 的网关通用好思路（只记录不实现），每条附社区出处、价值、落地要点与优先级 |
| [03-cursor-deferred.md](03-cursor-deferred.md) | 与 Cursor 直接相关、但本轮暂不实现的项：为什么不做、启用前要怎么实测、改动落点 |

## 已实现的 Cursor 改进

本轮直接落地、均有单测（关键用例做过变异验证：把旧逻辑放回去，测试必须失败）：

| 提交 | 改进 | 社区依据 |
|---|---|---|
| `86fa47a92` | **额度自动恢复**：后台 `CursorQuotaRefreshService` 每 5 分钟挑出过期/异常快照的账号重拉额度（leader lock、每周期 20 个、并发 4）；额度写库改为 `UpdateExtra` 原子合并，不再整行 `Update` 覆盖管理员同期修改 | YeautyYE 60s 轮询、greenSheep 45s 缓存、cpa-cursor 按 resetAtMs 冷却（见 [02](02-gateway-ideas.md) 第 8 条） |
| `b905820bc` | **应答所有服务端请求**：exec（request_context 每次都答、mcp_state、MCP resources、审批探测、allowlist 预检）、KV get/set、interaction_query；不桥接的原生工具回 throw + stream_close 并点名客户端工具 | opencodex「未答 query 是只剩心跳挂起的根因」、cpa-cursor「server blocks on silence」 |
| `b905820bc` | **本轮结束只认明确信号**：turn_ended / trailer / 工具就绪；正文后静默或只剩心跳报不完整流，不再伪造 end_turn。传输存活与业务进展分两个计时器（240s 进展上限取官方 CLI 口径） | YeautyYE 曾因静默收尾「凭空捏造 End」、opencodex/cpa-cursor 均把正文后静默当错误 |
| `b905820bc` | 并行工具按 `tool_requests_listed(27)` 计数或 1s 静默窗口收齐整批；丢弃 `is_server_notice` 公告；解析 `turn_ended` 用量仅记日志对账 | cpa-cursor session.go、response.go |
| `b905820bc` | Sand 面转发 `thinking_part(9)`、流内 `error(8)` 判失败、解析 usage；图片不再合成 path 并声明 `client_supports_inline_images(19)`；流开始后空闲补 `ping` 保活 | greenSheep inference_stream.go、YeautyYE request.rs:2078 |
| `d144788ff` | **错误分级**：`OUTDATED_CLIENT` 不换号不写账号状态；`SUSPICIOUS_USAGE_BLOCKED`/`ACCOUNT_CLOSED` 停号；用户级限流不标桶、账号冷却 10 分钟；地区封锁按「账号×模型」冷却 30 分钟 | cpa-cursor errors.go、YeautyYE retry.rs |

仍需真实账号实测才能落地的 Cursor 项见 [03](03-cursor-deferred.md)。

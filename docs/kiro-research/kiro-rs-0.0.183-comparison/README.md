# sub2api-kiro vs 2ue_kiro.rs（0.0.183）Kiro 处理对比 · 索引

> 日期：2026-10-01
> A = 本仓库 sub2api-kiro（Go，`backend/internal/pkg/kiro` + `service/kiro_*`）
> B = `~/Desktop/procode/2ue_kiro.rs`，HEAD `2a45a54 (0.0.183)`
> 全程只读对比，未改动任何代码。上一版对比 [../repos/2ue-kiro-rs.md](../repos/2ue-kiro-rs.md) 基于 0.0.161，本目录以 A、B 当前代码为准重新核对。

## 证据标记

| 标记 | 含义 |
|---|---|
| `[已核实]` | 本轮亲自读 A 源码确认 |
| `[读码]` | 调研中读两边源码得出，未逐行复核 |
| `[B记录]` | 依据 B 的 feature/issues、docs/analysis 生产记录 |
| `[未核实]` | 推断或证据不足 |

## 一、优先级总表

只列"值得 A 做"的项。成本：低 = 百行内单点改动；中 = 跨文件或需新状态；高 = 改执行模型。

### P0：A 当前存在的正确性缺陷

| # | 问题 | A 位置 | 证据 | 成本 | 详见 |
|---|---|---|---|---|---|
| 1 | 上游 exception 帧被当 usage 吞掉，截断报 `end_turn`，中途失败伪装成功 | `translator.go:3846` 只读 `:event-type` | `[已核实]` | 低 | [02](02-stream-and-response.md#1) |
| 2 | 超限错误不含 `prompt is too long`，Claude Code 无法自动 compact | `kiro_runtime.go:916`、`:1165` | `[已核实]` 措辞；CLI 判定依据 `[未核实]` | 低 | [01](01-protocol-conversion.md#1) |
| 3 | schema 清洗把 `type:[..,"null"]` / `anyOf` 改成空 object | `translator.go:2168` | `[已核实]` | 低 | [01](01-protocol-conversion.md#2) |
| 4 | tool_result 内的 image / document / search_result 被丢 | `translator.go:2624` | `[已核实]` | 低 | [01](01-protocol-conversion.md#3) |
| 5 | 任何含 `invalid`/`token` 的 403 都触发强制刷新 | `kiro_http_helpers.go:154` → `kiro_runtime.go:580` | `[已核实]` | 低 | [03](03-account-and-retry.md#2) |
| 6 | `thinking.type=disabled` 仍会因 beta 头被强开 thinking | `translator.go:1491` | `[已核实]` 代码；CC 是否带该 beta `[未核实]` | 低 | [01](01-protocol-conversion.md#4) |
| 7 | 远程图片拉取无 SSRF 防护 | `translator.go:70`、`image_tokens.go:100` | `[读码]` | 中 | [04](04-websearch-image-observability.md#1) |

### P1：稳定性与可用性

| # | 问题 | 证据 | 成本 | 详见 |
|---|---|---|---|---|
| 8 | EOF 无完成信号也补 `end_turn` | `[已核实]` `translator.go:1376` | 中 | [02](02-stream-and-response.md#2) |
| 9 | 请求级重试无总预算（端点×重试×换号 15 次） | `[读码]` | 中 | [03](03-account-and-retry.md#1) |
| 10 | profileArn 解析 `sync.Once`，失败永久落占位 ARN | `[已核实]` `kiro_profile_resolver.go:146` | 低 | [03](03-account-and-retry.md#4) |
| 11 | 首输出前错误已写入 SSE，通用 failover 对 Kiro 失效 | `[读码]` | 中高 | [02](02-stream-and-response.md#3) |
| 12 | tool_result 配对用全局 ID 集合，非"紧邻上一轮" | `[已核实]` `translator.go:2295` | 低 | [01](01-protocol-conversion.md#5) |
| 13 | 风控错误不分级（423、锁号、风控型 429） | `[读码]` | 低 | [03](03-account-and-retry.md#3) |
| 14 | websearch 失败静默成"无结果" | `[读码]` | 低中 | [04](04-websearch-image-observability.md#3) |
| 15 | 图片只信声明的 media_type | `[读码]` | 低 | [04](04-websearch-image-observability.md#2) |

### P2：增强

contextUsageEvent → `model_context_window_exceeded`、ListAvailableModels 动态能力、Retry-After 与按模型冷却、刷新失败负缓存、machine_id 首次落库、profileArn 推断区域、上游 400 请求侧诊断、转写占位泄漏清洗、`disable_parallel_tool_use`、server_tool_use 渲染、system 块分隔符。分散在各分册。

## 二、分册

| 文件 | 范围 |
|---|---|
| [01-protocol-conversion.md](01-protocol-conversion.md) | Claude Code → Kiro 请求转换、协议字段矩阵、错误措辞 |
| [02-stream-and-response.md](02-stream-and-response.md) | eventstream 解析、stop_reason、流错误、重试时机、usage |
| [03-account-and-retry.md](03-account-and-retry.md) | 凭证刷新、错误分类、冷却、重试预算、指纹、端点与区域 |
| [04-websearch-image-observability.md](04-websearch-image-observability.md) | websearch、图片、诊断、测试体系 |
| [05-not-to-copy.md](05-not-to-copy.md) | A 更好的地方、B 不该照搬的地方、已澄清的事实冲突 |

## 三、对比覆盖范围

| 维度 | 覆盖 |
|---|---|
| 请求转换（消息/工具/schema/thinking/图片/裁剪） | ✅ |
| 协议字段逐项矩阵（请求字段、stop_reason、错误类型、count_tokens） | ✅ |
| 流式解析与终态判定、usage | ✅ |
| 账号、刷新、调度、重试、指纹 | ✅ |
| Kiro 上游侧（CLI/IDE 端点、模型能力发现、cachePoint） | ✅ |
| Claude Code 特有场景（内部文本泄漏、prompt steering、auto compact） | ✅ |
| websearch、图片、可观测性、测试体系 | ✅ |
| B 的 external_pool、PG/Redis 运行态 | ❌ 架构不同，刻意不比 |
| 管理后台、导入 | ❌ 已有独立文档 |

仍未验证、需要真实账号才能定论的：

- 付费档 / CLI 端点 / 带 cachePoint 时上游是否下发 `tokenUsage`。
- Kiro 是否拒绝非法 schema property key（B 的 400 是在 Anthropic 上游复现的）。
- 首条为 assistant 时上游是否 400。
- `inferenceConfig.temperature/topP` 上游是否生效。
- 上游 503/429 换号耗尽后客户端最终收到的状态码；A 的 `mapUpstreamStatusCode` 把所有 5xx 映射为 502，529/overloaded 大概率出不来。

# 02 · 流式解析与响应

## 1. exception 帧未识别 {#1}

`[已核实]` `translator.go:3846` `extractEventType` 只返回 `:event-type` 头。AWS eventstream 的异常帧是 `:message-type=exception` + `:exception-type=<名称>`，没有 `:event-type`，于是 eventType 为空，落进 `translator.go:4097` 的 `default` 分支被当成 usage 事件吞掉。

后果：

- `translator.go:1356` 注释"真正的截断由上游 ContentLengthExceededException 异常帧设置 stop_reason"实际不成立，输出截断报成 `end_turn`。
- 上游流中途的 `please try again` 类错误被伪装成正常结束，下游拿到残缺回复 + 成功状态。

B：`src/kiro/model/events/base.rs:124-130` 按 `:message-type` 分流 error/exception；`stream.rs:2666-2685` 已有输出时 `ContentLengthExceededException` → `max_tokens`，其余记 stream_error 并发 SSE `error` 事件（B 记录 06、10）。

建议：解析时同时读 `:message-type` 与 `:exception-type`；exception 帧单独成语义事件。`ContentLengthExceededException` 映射 `max_tokens`，其余在已提交时发 SSE error、未提交时返回错误供换号。成本低（几十行），需构造异常帧的单测，且流式、非流式两套解析器都要改（见记忆 cli-streaming-only-hides-nonstreaming-tested-bugs）。

## 2. EOF 终态校验 {#2}

`[已核实]` `translator.go:1376-1386`：stopReason 为空且没发过工具块时一律补 `end_turn` + `message_stop`。

B：`stream.rs:2008-2036` `upstream_terminal_failure_detail` + `:2224` 记录 `messageStatus`，没有可信完成信号判为失败；可安全 flush 的 tool buffer 例外。B 第一版判得过严误伤企业号（`enterprise-eventstream-usage-only-tool-eof`），之后才收紧。

建议：先只观测——EOF 时没有 `metadataEvent.stopReason` / `messageStopEvent` 的比例打 ops 指标，按账号类型分组，有数据后再决定是否判失败。成本中。

## 3. 首输出前错误无法 failover {#3}

`[读码]` 通用层 `gateway_upstream_response.go:1077` 在 `!Written()` 时才 failover。但 `kiro_runtime.go:415` 的翻译协程出错时先往 pipe 写 SSE error，客户端已收到字节，Kiro 走不到这条 failover。另外 keepalive ping 也会让 `Written()` 变 true。

B：`handlers.rs:171-180` 6 类重试原因 + `:9453-9475` commit 标记，只在下游未提交时重试。B 记录 02 号问题中 41 条里 22 条是 0 chunk 失败，正是这一类。

建议：翻译协程在首个有效内容块前出错时 `CloseWithError` 回传，由 runtime 决定换号；首块之后才写 SSE error。ping 推迟到首块之后或不计入 commit。成本中高，是执行模型的改动。

## 4. usage-only 空轮次

B：上游只返回 usage、没有内容时判失败，首输出前用改写请求重试一次（`handlers.rs:9495`），理由是原样重发几乎必然仍空 `[B记录]`。A 输出空 `end_turn`。成本中，依赖第 3 条。

## 5. 其他

| 项 | B | A | 成本 |
|---|---|---|---|
| contextUsageEvent | 百分比 ≥100% 设 `model_context_window_exceeded`（`stream.rs:2573-2590`） | 忽略，搜不到 `contextUsagePercentage` | 低 |
| 流错误 error.type | 保留 `invalid_request_error`/`api_error` 等 | 固定 `stream interrupted` + `api_error` | 低 |
| 帧 CRC 与重同步 | `frame.rs:135-155` 校验 CRC，`decoder.rs:245-290` 出错跳过 | 只校验长度，长度异常整流失败 | 低，价值有限 |

## 6. 待查

- 上游完全静默时，客户端断开后 drain 用 `WithoutCancel` context，翻译协程可能一直阻塞在读上，HTTP client 是否有兜底超时 `[未核实]`。
- `defaultKiroStreamKeepalive`（25s，`gateway_service.go:63`）只有定义没找到调用，Kiro 实际走通用 10s 间隔 `[读码]`。

# 04 · websearch、图片、诊断与测试

## 1. 远程图片 SSRF {#1}

`[读码]` A：`translator.go:70` 与 `image_tokens.go:100` 用裸 `http.Client`，跟随重定向；搜 `IsPrivate`、`IsLoopback`、`ssrf` 无结果。任意下游用户可借图片 URL 探测内网（含元数据服务 169.254.169.254）。

B：`body_processing.rs:900-955` 拒绝回环与私网地址、拦截重定向、在拨号时校验解析结果防 DNS rebinding，并有数量与字节预算（`feature/evidence/remote-multimodal-resource-and-ssrf-20260716.md`）。

建议：自定义 `DialContext` 校验目标 IP、禁用或逐跳校验重定向、限制响应体大小与总拉取数。成本中，属安全项。

## 2. 图片格式 {#2}

`[读码]` A：`translator.go:2668-2685` 只信声明的 media_type；`:2571-2575` 失败直接 `continue` 静默丢图。

B：`content.rs:463-510` base64 解码后用 magic bytes 纠正 media_type，解不开的拒绝；当前轮坏图返回官方格式 400（B 记录 08 `IMAGE_FORMAT_UNSUPPORTED`）。成本低。

## 3. websearch {#3}

`[读码]`

| 项 | B | A |
|---|---|---|
| 失败分级 | 可恢复失败返回 200 + `web_search_tool_result_error`，协议坏包才硬失败（`websearch.rs:322-345`、`1240-1262`） | mcpErr 直接置空结果（`kiro_websearch.go:134`、`226`），模型看到 `No search results found.`（`websearch.go:340`），不记 ops |
| query 来源 | 只取当前 user 轮（`websearch.rs:444`） | `websearch.go:98-117` 回溯历史 user 消息，可能复用旧 query |
| query 长度 | 截断到 200 字符（实测约 560 字符返回 invalid_tool_input） | 不截断 |
| `allowed_domains` / `blocked_domains` / `max_uses` | 支持（`websearch.rs:560-650`） | 不支持 |
| 当前轮无 query | 去掉该工具 | 报错 |

B 的工具错误语义依据（`docs/analysis/official-claude-code-kiro-protocol-audit-20260928.md`）：HTTP 200 + 合法 tool_result error 时 CLI 能继续；502 或协议层 400 时当前 turn 结束。成本低到中。

## 4. 上游 400 请求侧诊断

`[读码]` B：`tool_format_debug.rs:224` + `handlers.rs:5892`，上游返回 tool-use 格式类 400 时，异步、按指纹限流落盘 JSONL，含实际请求体 sha 与 `ToolUseFormatDiagnostics` 二十多项结构计数（`payload_guard.rs:68`）。

A：`kiro_runtime.go:1219-1235` 只记响应 excerpt 512 字节；ops 事件只有 `has_tools` 这类布尔值，事后无法还原触发 400 的请求结构。

建议：400 时在 ops 事件里附结构计数（消息数、各角色块数、tool_use/tool_result 数与配对差、工具数、最大 schema 深度、加权体积），不落正文。成本中。

## 5. 测试体系

| 项 | B | A |
|---|---|---|
| 压测 | `src/bin/kiro_loadtest.rs`，自带 fake 上游与多种场景（含拒收 cachePoint、usage-only EOF） | 只有 Go benchmark |
| 真实 CLI 回放 | `docs/testing/claude-code-cli-real-account-test-requirements-20260929/` 9 类标准 + `feature/tests/claude-cli-leak-scanner.mjs` | `tools/run_real_claude_cache_policy_matrix.py` 只覆盖缓存 |

建议：至少补一个 fake Kiro 上游（可构造异常帧、无终态 EOF、usage-only 空轮），P0 第 1、8、11 条的回归都依赖它。成本中。

# 01 · 请求转换与协议字段

## 1. 超限错误措辞无法触发 Claude Code 自动 compact {#1}

`[已核实]` A 的三条超限路径都不返回 `prompt is too long`：

| 路径 | A 返回 |
|---|---|
| 上游 400 透传 | `kiro_runtime.go:1165` 原文，如 `Input content length exceeds threshold.` |
| `behavior=reject` | `kiro_runtime.go:916` 413 `Request payload too large: weighted size …` |
| `on_upstream_400` | `kiro_runtime.go:625` 对**所有**请求静默裁剪后重试 |

B：统一改写为 400 `invalid_request_error` + `prompt is too long: N tokens > M maximum`（`handlers.rs:5446-5470`、`5698-5714`）；裁剪重试只对 Claude Code 自身的 compaction 请求生效（`handlers.rs:1083` 注释说明 CLI 依赖该子串）。

影响：A 上用户要么卡在反复报错，要么拿到被裁掉一半历史的 200（见记忆 kiro-silent-context-loss-returns-200）。

建议：`kiro_error_classifier.go:126` 命中 oversize 类时改写措辞并统一为 400；413 改 400；裁剪重试收窄到 compaction 请求或保留为显式开关。CLI 的判定子串需对照 Claude Code 源码复核 `[未核实]`。成本低。

## 2. schema 清洗改错 Optional 字段语义 {#2}

`[已核实]` `translator.go:2141` 白名单不含 `anyOf/oneOf/allOf`，`:2168` 在 `type` 非字符串时改写为 `"object"` 并补 `properties:{}`。

| 输入 | A 输出 |
|---|---|
| `{"type":["string","null"]}` | `{"type":"object","properties":{}}` |
| `{"anyOf":[{"type":"string"},{"type":"null"}]}` | `{"type":"object","properties":{}}` |

Pydantic / zod 生成的 Optional 字段、多数 MCP 工具都是这种形状，模型会按 object 传参。

B：`converter/schema.rs:28-64` 展开根级组合关键字，`:371-418` 数组 type 取首个非 null 类型。

建议：`type` 为数组时取第一个非 `null` 值；`anyOf/oneOf` 只含"单类型 + null"时折叠为该类型，其余取第一个分支。补单测覆盖上述两种输入。成本低。

## 3. tool_result 内的非文本块被丢弃 {#3}

`[已核实]` `translator.go:2624` 只收 `text`/`input_text`/字符串。

| 块 | A | B |
|---|---|---|
| image | 丢弃 | 提取到消息 images（`content.rs:815-848`） |
| document | 丢弃 | 转文本（`content.rs:618-655`） |
| search_result | 丢弃 | 渲染为带来源的文本 |
| 空 content | 发空 text | 填占位（B 记录 P04） |

生产影响：Claude Code 用 Read 读图片时结果是 tool_result 内的 image，A 上模型看不到图，会编造内容 `[B记录]`。成本低。

## 4. thinking 判定 {#4}

`[已核实]` `translator.go:1472-1494`：`thinking.type` 只匹配 `adaptive`/`enabled`，`disabled` 落到后续兜底；只要 `Anthropic-Beta` 含 `interleaved-thinking` 就强开 16000 budget。

B：`disabled` 直接关闭，beta 头不单独开 thinking（`converter/model.rs:145-156`）。

若 Claude Code 的后台小模型调用（haiku 标题生成等）带该 beta 头 `[未核实]`，A 会给它们全开 thinking，多耗额度、加延迟。建议：`case "disabled": return nil`，beta 头兜底删除或仅在 `thinking` 缺省时生效。成本低。

## 5. tool_result 配对 {#5}

`[已核实]` `translator.go:2295` `validateToolPairing` 用全历史 toolUse ID 集合判断合法性。引用更早轮次 tool_use 的结果会通过，而 Kiro 的 400 是按"上一轮"计数的（`toolResult blocks exceeds toolUse blocks of previous turn`）。

B：`tool_pairing.rs:9-72` 历史 tool_result 必须属于紧邻的上一条 assistant，并去重；`:84-200` 当前轮只认最后一条 assistant 的 tool_use。

注意与记忆 kiro-trim-breaks-current-turn-tool-pairing 的关系：A 裁剪后补回当前轮 toolUse 的逻辑（`payload_guard.go:254-290`）要保留。成本低。

## 6. 协议字段差异矩阵

只列有差异的项 `[读码]`：

| 字段 | A | B | 影响 |
|---|---|---|---|
| system 数组 | 块之间无分隔直接拼接（`translator.go:1429`，`[已核实]`） | `\n` 拼接 | billing-header 块与下一句粘连，且进入稳定会话 ID 种子 |
| 单独的 `output_config.effort` | 忽略 | 视为 reasoning 请求 | 小 |
| `disable_parallel_tool_use` | 忽略 | 响应侧限 1 个 tool_use（`converter/tools.rs:606-623`） | 中 |
| `tool_choice: {type:tool}` | 只加提示词 | 提示词 + 只下发指定工具 | 低 |
| 历史 thinking | 降级为 `<thinking>` 文本，签名丢弃 | 签名原样进 `reasoningContent` | 见 05，A 的做法更稳 |
| server_tool_use / web_search_tool_result / search_result（消息级） | 丢弃（`translator.go:2554`、`:3061`） | 渲染文本（`content.rs:716-789`） | 重放搜索历史时丢上下文 |
| 首条为 assistant | 连续两条 assistant | 插占位 user | 可能 400 `[未核实]` |
| 末条 assistant（prefill） | 保留并追加 `Continue` | 丢弃 prefill | 都不等价于官方续写 |
| temperature / top_p | 下发 inferenceConfig | 不下发 | 上游是否生效 `[未核实]` |
| 错误响应 request-id | 错误路径不带 | `request-id` + `anthropic-request-id` + body | 低 |

一致或已覆盖：model 映射、stop_sequences、metadata 派生会话 ID、count_tokens（两边都本地估算）。

## 7. 其他可选项

| 项 | B | A | 备注 |
|---|---|---|---|
| schema property key 可逆映射 | `tool_schema_keys.rs:163-230` | 无 | Kiro 是否拒绝非法 key `[未核实]`，先取证 |
| 响应侧工具名模糊还原 | `tool_name_restore.rs:61-137`，去 hash / 忽略大小写唯一命中时还原 | `translator.go:3595` 精确查找 | 模型省略后缀时客户端报 No such tool；成本低 |
| 转写占位泄漏清洗 | `transcript_sanitizer.rs` | A 自己生成 `Continue`、`Tool results provided.`（`translator.go:2263-2282`）、`I will follow these instructions.`（`:1880`），无清洗 | 先拿生产样本确认是否发生，再做完整签名匹配过滤 |

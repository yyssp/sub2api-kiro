# 05 · A 更好、不该照搬、已澄清的冲突

## A 做得更好

| 项 | A | B |
|---|---|---|
| 历史 thinking | 降级为 `<thinking>` 文本，不回放签名（`translator.go:3061-3110`） | 走原生 `reasoningContent`，生产出过 `THINKING_SIGNATURE_INVALID`，只能剥离后重试（`docs/analysis/production-thinking-signature-*`） |
| 工具名 | 只替换非法字符，合法名原样保留（`translator.go:2066-2124`） | 早期把 `Bash` 改写成 `bashHash…`（P05），后来才加原样透传开关 |
| 裁剪后的配对 | 补回当前轮 toolUse（`payload_guard.go:254-290`） | 9-15 旧对比把它列为"值得学"，A 已实现 |
| 冷却同步调度器 | `markKiro429` 写 `rate_limit_reset_at`；月度额度冷却到下月 1 日 | 运行态在 PG/Redis 自维护，出过"假禁用" |
| 客户端断开 | 继续读完上游拿完整 usage（`gateway_upstream_response.go:1066`） | 03 号记录主张直接取消上游，会少计 usage |
| websearch 体验 | 拿到结果后再调模型生成回答，最多 5 轮（`kiro_websearch.go:16`） | 只合成摘要，不调模型 |
| tool 收尾 | EOF flush 未完成 tool + `repairJSON`（`translator.go:4134`）+ 必填校验 | 基本对齐 |

## 不该照搬

- **狂暴模式**：账号数 × 区域数 × 轮数遍历，上限 2000 次，与 B 自己的重试预算思路冲突。
- **PG/Redis 运行态整套**（mutation FIFO、quarantine 拆分、续租优化）：解决的是 B 自维护运行态带来的问题，A 的状态在调度快照里，不存在该问题。
- **prompt_steering**：向 `/cc` 注入 `<language_constraint>`、`<task_quality_policy>`，给 Write/Edit 工具描述追加强制分块规则。B 自己的审计（`14-prompt-steering…md`，open）结论是改写工具语义、成本成倍、可能留下写了一半的文件。最多考虑可关闭的语言约束。
- **empty_turn_nudge**：针对 09 号问题，B 自己定性为模型行为，代理只能软引导。
- **合成 cache 用量体系**：A 已有独立缓存策略，口径不同，见下。
- **cachePoint**：B 默认关闭（`config.rs:5013`），没找到任何真实上游用量变化或成本节省的证据。值得用付费号做一次 A/B，A 现在不需要跟进。
- **refresh_token 长度 < 100 本地拒绝**：依据未核实。

## 已澄清的冲突：上游是否下发 cache 用量

两边结论表面矛盾，实际一致：

- A 的旧结论"Kiro 上游会下发真实 cache 用量"已被 2026-09-14 实测推翻：FREE 个人号的 `metadataEvent` 只有 `stopReason`，没有 `tokenUsage`。
- A 的"三字段之和等于 billable，是计费口径"测的是第三方 Kiro 中转 jinnyapi（带 `kiro_*` 私有字段），不是 Kiro 上游本身。
- B 在 `docs/analysis/prompt-cache-stable-segment-strategy-design-20261001.md:9-23` 的真实账号结论（只有 assistantResponseEvent / contextUsageEvent / meteringEvent）与 A 实测一致。该文第 13 节的表格是 mock 上游数据，不能当上游证据。

仍未决：付费档、CLI 端点、发送 cachePoint 后是否下发 `tokenUsage`，两边都没测过。

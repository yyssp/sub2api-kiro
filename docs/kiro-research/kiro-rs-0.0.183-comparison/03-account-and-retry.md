# 03 · 账号、刷新、重试与上游端点

## 1. 请求级重试预算 {#1}

`[读码]` A：`kiro_runtime.go:461` 每端点 `maxRetries=2` × auto 模式 2 个端点 × handler `maxAccountSwitches=15`（`gateway_handler.go:109`），没有总上限。

B：`anthropic/inference_attempt_budget.rs:7` 整个请求共用预算，默认 4 次，只在真正发出上游请求前扣减；下游已收到数据后不再重发；无备选账号直接失败（`provider.rs:9365`）。B 记录 retry-budget-admission 中 500/429 放大最高 30 倍。

建议：在 Kiro 路径的 context 上挂一个请求级计数器，端点重试、刷新重试、换号共用。成本中。

## 2. token 失效判定过宽 {#2}

`[已核实]` `kiro_http_helpers.go:154` `isKiroTokenErrorBody` 只要 body 含 `token`/`expired`/`invalid`/`unauthorized` 之一就为真；`kiro_runtime.go:580` 据此对 403 强制刷新。`User is not authorized to make this call`（profileArn 缺失）、各类 `invalid ...` 的 403 都会触发刷新，刷新还会打上游 OIDC。

B：`endpoint/mod.rs:355` 只匹配精确短语，且每请求每账号只自动恢复一次。

建议：收紧为 401 或明确的过期措辞；记录每次触发的原文，先观测再收紧。成本低。

## 3. 风控错误分级 {#3}

`[读码]` A：`kiro_http_helpers.go:149` 只要含 `SUSPENDED` 子串就冷却 24 小时，不识别 423 / 锁号；风控型 429 按普通 429 处理。

B：`provider.rs:13911` 区分临时封禁、永久封禁、锁号（423）、`429 + suspicious activity`。成本低。

## 4. profileArn 解析 {#4}

`[已核实]` `kiro_profile_resolver.go:146` 每账号一个 `sync.Once`，失败时落默认 ARN 并永不重试（进程级）。

B：`provider.rs:9159` 每账号 singleflight，失败 5-60s 退避重试，状态按认证身份隔离。

建议：`sync.Once` 换 `singleflight` + 失败时间戳退避。成本低。

## 5. 其他

| 项 | B | A | 成本 |
|---|---|---|---|
| 刷新失败分类与负缓存 | `refresh.rs:556` 区分 invalid_grant / invalid_client / 429（读 Retry-After）/ 5xx；`manager.rs:1062` 0.5-30s 负缓存带 jitter | `token_refresh_service.go:1417` 字符串匹配，无负缓存，失败期间每请求都刷新 | 中 |
| 429 | 解析 Retry-After（`provider.rs:13880`），按模型冷却（`manager.rs:10570`） | `kirocooldown/store.go` 固定 1-5 分钟整号冷却 | 中 |
| machine_id | 首次生成落库，`machine_id.rs:482` 测试保证 refresh_token 轮换后不变 | `kiro_http_helpers.go:103` 未存时按 refresh_token 现算；只有导入数据带才写入（`kiro_oauth_service.go:677`） | 低。若 refresh 轮换 token `[未核实]`，指纹会随之漂移，与 Cursor 指纹落库同类问题 |
| API 区域 | `credentials.rs:627` api_region > profileArn 中的区域 > 全局 | `kiro_http_helpers.go:188` 只看 api_region | 低，欧洲账号可能打错区域 |
| 模型能力发现 | ListAvailableModels 解析 maxInputTokens、promptCaching、additionalModelRequestFieldsSchema（`available_models.rs:62-105`），跨账号群组一致才采信（`model_capabilities.rs:779-797`） | 静态列表 `models.go:10` + 版本号 ≥4.6 走 output_config（`translator.go:1647`） | 中。新模型上线时 A 会静默丢 thinking 或误塞 effort 致 400，上下文阈值也不自适应 |

## 6. 上游端点

`[读码]` B 实现两套协议：

| | IDE（B 默认） | CLI |
|---|---|---|
| URL | `q.{region}.amazonaws.com/generateAssistantResponse` | `runtime.{region}.kiro.dev/`，AWS JSON 1.0 + `x-amz-target` |
| UA | `aws-sdk-js … KiroIDE-{ver}-{machineId}` | `aws-sdk-rust … AmazonQ-For-CLI`，无 machineId |
| origin | AI_EDITOR | KIRO_CLI |
| 模型列表 | q 端点 | `management.{region}.kiro.dev` |

A：q / krs / auto 三模式（`kiro_runtime.go:662`），但 KRS 请求仍用 IDE 风格 UA 和 `agent-mode=vibe`，origin 固定 AI_EDITOR。即 A 是把 IDE 协议发到两个 host，没有真正的 CLI 协议。

两种端点在可用模型、配额、cache 用量上是否不同 `[未核实]`。暂不建议做，先确认 KRS host 配 IDE 头是否被上游区别对待。

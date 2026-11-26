# 缓存整形后续能力

参考 `../2ue_kiro.rs` 的 `claude-main`，提交 `6ed4599d`（2026-11-24）。本项目继续按分组绑定策略；参考项目的路径、账号池及控制台架构不移植。

## 策略

| 本项目 kind | 参考名称 | 行为 |
| --- | --- | --- |
| `prefix` | `current_high_cache` | 保留已有高缓存行为，可选启用命中率与计费下限整形 |
| `tool_aware` | `prefix_tool` / `kiro_rs_tool` | 保留已有工具前缀拆分 |
| `stable_segment_cache` | 同名 | 稳定 tools/system/history 边界、历史窗口、最小请求/前缀门槛、未缓存 input 范围、部分增量提交 |
| `prefix_tool_plus` | 同名 / `kiro_rs_tool_plus` | 四字段倍率与浮动 → 写入频次 → 四字段上下限 → 计费下限；非流式/小请求支持 inherit/custom/raw |
| `bounded_high_cache` | 同名 | 最终 input/read/creation 上下限优先；无法补足的成本差额只在成功请求后累计 |

策略类型别名在后端解析时归一化；参数沿用本项目 snake_case JSON，不能直接导入参考项目的路由配置。

## 新配置

- `hit_shaping`：目标命中率、向上浮动、非零读写最小值、最低计费倍数、最大 read/真实输入倍数、最小 creation 比例、读取参与成本补偿、差额是否补 input。
- `usage.strict_cache_eligibility_stream/non_stream`：独立控制官方缓存资格；要求客户端真实缓存断点及对应前缀达到模型门槛，未满足则透传 usage，不提交状态。
- `cold_request_guard`：无本地/上游命中时清零 read，并把 input+creation 限制在原始输入内。
- `state_ttl_seconds`：缓存状态寿命，0 保持旧策略；1–300 秒独立于下游展示 TTL。新策略默认 300 秒。
- `stable_segment`：最小请求/稳定前缀、历史消息窗口、显式断点采信和各类自动边界开关；覆盖率、放大和最终上限仍使用现有通用字段。
- `tool_plus`：默认/非流式/小请求的整形参数，以及受抑制写入归入 input/read 或舍弃。频次参数复用 `creation_control`。

当状态 TTL 分离启用时，上游 Anthropic 协议已有 ephemeral 标记统一为显式 5m，原始请求仍用于展示 TTL 解析。只处理协议位置，不遍历工具输入、JSON schema、metadata。不添加新的断点。

## 保留的本项目边界

- 已有策略默认关闭新增功能；新策略不会自动替换已有策略或分组绑定。
- 保留请求成功后才提交 tracker/频次的规则，SSE 预览不提交。
- 保留本项目的上下文硬上限、策略作用域和 usage 日志链路。
- 稳定分段的部分写入状态先保存在本机 tracker；Redis 旧接口只返回存活 TTL，无法表达部分写入量，故该策略不使用此接口，避免跨实例虚假全量命中。
- 计费下限使用现有模型定价解析链（分组 → 渠道 → 模型目录 → fallback）；无法定价时跳过成本补偿，其他整形仍生效。
- 本次不移植参考项目的账号原始用量三模式、独立账号成本字段及模型过滤，这些属于账号采集/计费配置。

## 验证

- `go test -tags unit ./internal/service -run 'TestCache|TestBoundCache|TestAdvanced|TestKiroGroupCache|TestReportedInput|TestSkipNonStream|TestPreserveUpstreamCacheUsage' -count=1` 通过。
- `pnpm run build` 通过，包含 i18n key 完整性检查、Vue/TypeScript 构建和 Vite 生产构建。
- 缓存策略模板 12 项与本地化定向测试通过；前端 lint 和 `git diff --check` 通过。
- 生产打包提示部分旧页面 chunk 超过 500 kB；不影响构建成功。
- 本机预览后端已重启到本次代码；健康检查、公开设置代理和管理员登录均返回 200。

状态仍为 In Progress：当前实现只提供进程内稳定分段追踪，跨实例部分写入状态需要可保存缓存 token 数的新远端 store 契约。

## 协议模块扩展（2026-11-25）

创建与编辑策略通过 `config.protocol` 显式选择 `anthropic` 或 `gemini`，复用系统平台标识。旧 JSON 缺字段默认 Anthropic。协议指客户端请求格式，独立于账号平台；Anthropic 模块保留已有 Messages / Chat Completions / Responses 的兼容覆盖。跨协议请求跳过策略。

后端模块注册位于 `cache_protocol.go`，模块负责配置归一化、协议匹配和计划生成。通用层提供分组绑定、策略版本隔离、tracker、字段采样与成功提交；Gemini 分别在 `cache_profile_gemini.go`、`cache_protocol_gemini.go`、`cache_wire_gemini.go` 实现请求建模、三桶算法和 JSON/SSE 写回。以后独立支持 OpenAI 时，可增加模块、注册协议键并接入相应网关，不需扩充 Gemini 算法。前端协议清单和默认设置独立于总编辑页，Gemini 使用单独字段组件。

Gemini 用量契约已对照 Google v1beta discovery：

- `promptTokenCount = 未缓存输入 + cachedContentTokenCount`。
- `输出 = candidatesTokenCount + thoughtsTokenCount`；输出投影后按原比例分配回答与思考。
- `totalTokenCount = prompt + candidates + thoughts + toolUsePromptTokenCount`。
- GenerateContent 没有缓存写入 token 字段；本地状态可增量写入，但生成记录的 cache creation 恒为 0。原生 `cachedContents` 创建/过期/存储 token-hours 计费独立保留，生成请求的真实资源缓存读取不改写。

Gemini 五种启用模式分别支持连续前缀、原生工具 parts、稳定分段、三字段 Plus、最终上限优先。复用已有采样和抖动纯函数，采用独立三桶算法，不调用 Claude 四桶投影，不使用 Anthropic 5m/1h 写入定价或成本补偿算法。稳定片段和新增状态上限依赖公共 tracker；小于完整断点的写预算不会写入不可证明的前缀。

前缀来自 systemInstruction、tools、toolConfig 和已结束的 contents；默认不写当前用户轮。未提供会话 ID 时可按系统指令、工具和首轮内容派生会话；默认新 Gemini 策略启用该选项。文本 token 采用长度近似估算匹配比例，以响应真实总输入为投影基数；图片/文件不模拟缓存匹配。真实上游缓存优先选项和显式缓存引用仍可提供命中证据。

仅成功完成且客户端未断开的响应提交状态。错误、无终止事件、客户端取消、countTokens 不提交。JSON、SSE、OAuth 流聚合和 Antigravity 原生 Gemini 路径共用响应投影；ForwardResult 直接使用同一投影值进行记账。

参考：<https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta>。

## 最终 usage 边界与默认值复验

详见 [缓存 usage 与参数边界审计](cache-usage-audit.md)。所有启用投影的模式统一按模型/策略/请求能力约束，默认值由后端共享快照提供。1,067 个后端测试项、31 个前端测试及本地 API 验证通过。创建窗口是本地成功记录的整形预算，不是跨实例并发配额；原样透传模式保持上游值。

第二轮交互与状态审计（2026-11-26）完成：后端 1,122 项、前端 38 项与构建通过。修复流式终帧重复放大、Gemini 部分 usage 帧、协议/模式草稿丢失、绑定失败恢复、失败续期和频控状态回收。详见上述审计报告第二轮章节。

## 直接按比例整形（usage_multiplier）

Anthropic/Claude Code 与 Gemini 均新增独立策略类型 `usage_multiplier`。只读取原始上游 usage，不依赖会话 ID、缓存命中证据、冷热、频控或本地状态。不与其他策略叠加。

每个桶配置 `threshold_tokens` 和 `multiplier`。仅原值 **严格大于** 阈值时，对整个桶乘倍率，结果四舍五入；否则保持原值。倍率 1 不变，0 清零，0.5 缩小 50%，1.5 放大 50%；阈值 0 对所有正值生效。原值 0 始终为 0，不虚构缓存读写。默认全部阈值 0、倍率 1。

```json
{
  "kind": "usage_multiplier",
  "protocol": "anthropic",
  "usage_multiplier": {
    "input": { "threshold_tokens": 0, "multiplier": 0.5 },
    "output": { "threshold_tokens": 1000, "multiplier": 1.5 },
    "cache_read": { "threshold_tokens": 0, "multiplier": 2 },
    "cache_creation": { "threshold_tokens": 2000, "multiplier": 0.75 }
  },
  "limits": { "max_input_tokens": 0, "max_output_tokens": 0 }
}
```

输出 1,000 不变，1,200 变成 1,800；输入、缓存读写各自按自己的原值触发。Anthropic input 指未缓存输入，OpenAI 兼容响应先拆三桶再重算总 input；TTL 写入分桶按原比例缩放。Gemini 只有输入/缓存读/输出，回答与思考按比例分配，生成 cache creation 恒为 0。显式 cachedContent 读取也应用本策略倍率。

该模式仅保留启停和模型/请求/`limits` 最终上限，不执行旧模式的采样、抖动、命中率、成本补偿、频控、严格资格或非流跳过设置。流式与非流式规则一致。触发阈值后受硬上限截断时，最终比例可能无法完全达到。现有图像输出响应仍保留原始多模态计费口径。

界面提供单独的阈值/倍率表和即时示例，不展示其他模式的缓存时机参数。数据结构与纯函数独立在 `cache_usage_multiplier.go`，后续协议可复用分桶逻辑。

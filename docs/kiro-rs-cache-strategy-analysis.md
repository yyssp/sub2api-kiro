# Kiro-RS 缓存策略分析与当前系统映射

日期：2026-09-04
范围：分析 `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs` 中与 Claude Code 兼容协议相关的本地 prompt cache usage 策略，并给出当前项目按分组绑定缓存策略时应保留的字段语义。

> 历史分析说明：本文保留用于说明参考实现的来源和映射。当前项目已经把缓存策略
> 做成通用的 Claude Code/Anthropic Messages 兼容策略，不是 Kiro 专用配置。实际
> 配置、当前默认值、参数影响和可直接使用的模板，请以
> [`docs/cache-strategy-parameter-guide.md`](./cache-strategy-parameter-guide.md)
> 为准。

## 结论

`2ue_kiro.rs` 里值得复用的不是 Kiro 账号、provider、路径路由或 IDE 特有逻辑，而是两套 usage 处理模型：

1. `current_high_cache`：普通高缓存模拟。它先基于真实请求内容建立本地 prompt-cache tracker，再通过 `reportedUsage` 对 `input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens` 四个字段分别整形。
2. `kiro_rs_tool`：工具调用导向缓存策略。它按 tools、system、历史消息和可选当前用户稳定前缀建立 cache block，以真实 tracker 命中计算 read/create，再把 `input_tokens` 压到一个小范围，形成更像 Kiro-RS Tool 的 usage 效果。

当前项目后续不应继续用 `prefix / tool_aware / disabled` 这类自造策略类型表达缓存，而应把策略类型收敛为：

| 策略类型 | 作用 | 适用场景 |
| --- | --- | --- |
| `no_cache` | 不构造本地缓存，不整形本地缓存字段 | 明确不需要缓存 usage 的分组 |
| `current_high_cache` | 高缓存 tracker + 独立四字段 usage 整形 | 大多数 Claude Code 兼容协议分组 |
| `kiro_rs_tool` | 工具导向 cache block + 小 input split usage | Kiro auth、Kiro-like tool-heavy 上游，或需要复刻 Kiro-RS Tool usage 风格的 Claude Code 兼容分组 |

当前项目的生效入口应是“协议层 Claude/Anthropic Messages 兼容请求 + 分组绑定策略”，不是 Kiro-RS 的路径前缀。路径只作为参考项目的路由选择方式存在，本项目需要替换为分组解析后的策略解析。

## 非目标

本文不分析或迁移这些内容：

| 非目标 | 原因 |
| --- | --- |
| Kiro 账号 provider 识别 | 与缓存策略无关，不应影响通用 usage 整形 |
| Kiro IDE/CLI 路由路径 | 当前项目按分组绑定策略，不按 `/cc`、`/ha`、`/na` 路径生效 |
| 上游请求 payload 压缩、历史裁剪 | 这是 payload guard/shaping，不能和 usage 缓存整形混在一个配置模型里 |
| completion/response 缓存 | 参考实现只缓存 prompt 前缀证据，不缓存或复用模型输出 |

## 概念分层

参考实现有三层容易混淆的逻辑，当前项目必须分开建模：

| 层级 | 代表配置/结构 | 作用 | 是否改变上游请求 | 是否改变最终 usage |
| --- | --- | --- | --- | --- |
| 缓存状态 tracker | `PromptCacheTracker`、`CacheBoundsPolicy` | 根据请求稳定前缀记录可命中的 prompt-cache 证据 | 否 | 间接影响 cache read/create 的基础值 |
| 缓存创建频次控制 | `PromptCacheCreationControlConfig` | 控制 `cache_creation_input_tokens` 出现频次和单次/窗口上限 | 否 | 是，只影响 creation 上报，不改变 tracker 命中 |
| 对外 usage 整形 | `ReportedUsagePathPolicy`、`ReportedUsageFieldPolicy` | 分别控制 input/output/cache read/cache creation 的最终上报 | 否 | 是 |

这三层都只处理 usage 和本地证据。真实请求 payload、上游调度、上游返回文本不应因为 usage 策略被改写。

## 标准 Claude usage 字段

当前项目应围绕 Claude/Anthropic Messages usage 字段建模：

| 字段 | 含义 | 约束 |
| --- | --- | --- |
| `input_tokens` | 对外上报的未缓存输入 token | 不能小于 0；在 split 模式下应与缓存字段共同守恒 |
| `output_tokens` | 对外上报输出 token | 不能小于 0；可采样、放大、最终裁剪 |
| `cache_read_input_tokens` | 本轮命中并读取的 prompt cache token | 首轮没有 tracker 命中时不能伪造；最终上限只向下裁剪 |
| `cache_creation_input_tokens` | 本轮新建 prompt cache token | 只能在有可缓存内容和成功请求后形成 tracker 状态；最终上限只向下裁剪 |
| `cache_creation_5m_input_tokens` | creation 中按 5 分钟 TTL 展示的分项 | 分项总和不能超过 `cache_creation_input_tokens` |
| `cache_creation_1h_input_tokens` | creation 中按 1 小时 TTL 展示的分项 | 分项总和不能超过 `cache_creation_input_tokens` |

## 策略一：Current High Cache

### 目标效果

`current_high_cache` 用于让所有绑定该策略的 Claude Code 兼容协议分组，强制基于本地请求内容和策略配置生成最终 cache read/write usage。上游是否返回 cache usage 不决定策略是否生效：上游字段只作为 raw usage 证据，只有字段明确配置为 `raw` 时才可作为该字段的输入；默认的 cache read/cache creation 由本地 tracker 和策略投影决定。

它的核心效果是：

1. 每次请求都真实发送到上游，不缓存模型响应。
2. 本地只记录稳定 prompt 前缀的 hash、累计 token、TTL 和最近使用时间。
3. 第一次会话或没有命中时只能写缓存，不能读缓存。
4. 后续请求如果稳定前缀匹配，才允许产生 `cache_read_input_tokens`。
5. 可通过 `reportedUsage` 单独控制 input、output、cache read、cache creation 的最终展示值。

### 参与配置

#### `CacheSimulationPolicy`

该配置控制高缓存模拟的基础缓存比例和大输入放大。

| 字段 | 默认值 | 边界 | 作用 |
| --- | --- | --- | --- |
| `enabled` | `true` | boolean | 是否启用高缓存模拟。关闭后不读取或写入本地 cache tracker，但绑定策略仍可继续执行已配置的 usage 字段整形 |
| `targetReadRatio` | `0.98` | `0..=0.99` | 目标缓存读取比例中心值。实际每个 profile 会在上下约 0.03 范围内确定性波动 |
| `tokenScale` | `1.6` | `1..=3` | 对外 usage 模拟时的大输入放大倍数，不改变真实请求和 tracker |
| `maxSimulatedInputTokens` | `300000` | `>=0`，`0` 表示不设上限 | 放大后的模拟输入软上限 |
| `capJitterMinTokens` | `12000` | `>=0` 且 `min<=max` | 触顶时从上限扣减的最小 token |
| `capJitterMaxTokens` | `24000` | `>=0` 且 `min<=max` | 触顶时从上限扣减的最大 token；实际还会受上限 8% 约束 |
| `scaleMinInputTokens` | `20000` | `>=0` | 基础输入达到该值才启用 `tokenScale`，避免短请求被放大 |

#### `CacheBoundsPolicy`

该配置控制 tracker 内部状态，不控制 usage 字段显示。

| 字段 | 默认值 | 边界 | 作用 |
| --- | --- | --- | --- |
| `maxEntriesPerAccount` | `200` | `0` 表示不按单账号数量限制 | 单 scope 最大 entry 数 |
| `maxEntriesGlobal` | `20000` | `0` 表示不按全局数量限制 | 全局最大 entry 数；如果非 0，不能小于 `maxEntriesPerAccount` |
| `entryTtlSecs` | `86400` | `>0` | tracker entry 的最大生命周期；会裁剪 5m/1h block 自身 TTL |
| `estimatedBytesLimit` | `268435456` | `0` 表示不按估算内存限制 | 以每 entry 约 256 bytes 估算全局容量 |

#### `CachePointPolicy`

参考实现中它属于 Kiro cache point 辅助能力，默认关闭。当前项目如果只做 usage 策略，可以先不暴露为主要用户配置。

| 字段 | 默认值 | 作用 |
| --- | --- | --- |
| `enabled` | `false` | 是否启用 cache point 规划 |
| `toolsOnly` | `true` | 只对工具相关内容规划 cache point |
| `recordPlan` | `true` | 是否记录 cache point 规划诊断 |

### Cache block 构造

`current_high_cache` 使用通用 `flatten_cache_blocks`：

1. 加入 request prelude，包含 `tool_choice`。
2. 加入所有 tools 定义。
3. 加入 system blocks。
4. 加入所有 messages 的 content blocks。
5. 发现 `cache_control.type=ephemeral` 时读取 TTL；没有显式 cache control 时，如果启用稳定前缀合成且存在可缓存块，会合成默认 5 分钟断点。
6. 每个 block 会 canonicalize 后计 token，并累积 hash。canonicalize 会移除 `cache_control` 本身，并把 `tool_use_id`、`request_id`、`message_id`、某些 `id`、位置 index 等 volatile 字段置空，避免每轮随机 ID 破坏缓存命中。

### 读缓存计算

读缓存在请求开始前计算，只读取 tracker，不写入 tracker。流程：

1. 若没有 scope、没有 profile、没有 breakpoint，返回 0。
2. 根据模型得到最小可缓存 token：
   - 常规模型：`1024`。
   - extended/新 Haiku/新 Opus：`4096`。
   - Haiku 3/3.5：`2048`。
3. 计算有效缓存比例：`targetReadRatio` 先限制到 `0..=0.99`，再在 `target +/- 0.03` 中按 profile fingerprint 做确定性波动。
4. 计算目标缓存 token：`round(totalInput * effectiveRatio)`，并限制不超过 `totalInput - 1`，低于模型最小可缓存 token 则为 0。
5. 首轮且没有 metadata conversation id 时，禁止 fallback read，直接 creation-only。
6. 从最后一个 lookup point 反向查找最长命中；命中 entry 未过期时，刷新 `last_used_at` 和 `expires_at`。
7. `cache_read_input_tokens = min(entry.cached_tokens, targetTokens)`。
8. `cache_creation_input_tokens = max(targetTokens - readTokens, 0)`。

### 写缓存时机

写缓存只发生在成功请求之后：

1. 非流式成功或流式成功完成后才 commit。
2. 上游错误、请求失败、流中断、客户端取消，不应写 tracker。
3. 写入时按 lookup point 的累计 token 比例映射到 `targetTokens`，低于模型最小可缓存 token 的点不写。
4. 写入 TTL 使用最后一个 breakpoint 的 TTL，并受 `CacheBoundsPolicy.entryTtlSecs` 上限裁剪。
5. 写入后执行每 scope、全局数量和估算内存淘汰，淘汰依据为最旧 `last_used_at`。

### 对外 usage 生成

`CacheSimulation.to_usage` 把 tracker 计算出的 `PromptCacheUsage` 转成最终 usage 基础值。

高缓存有两种重要口径：

| 口径 | 行为 | 适合 |
| --- | --- | --- |
| 非 split 口径 | `input_tokens` 保持原始输入，`total_input_tokens = input + cache_read + cache_creation` | 普通 high-cache 默认效果，展示“额外缓存字段” |
| split 口径 | `input_tokens + cache_read + cache_creation = total_input_tokens` | Kiro-RS Tool，展示“输入被缓存拆分” |

`current_high_cache` 默认走非 split 口径，并支持 `tokenScale`/`maxSimulatedInputTokens` 放大缓存 basis。放大只用于 usage 计算：

```text
cache_basis = tokenScale/maxSimulatedInputTokens/softCap(rawInput)
target_cached = round(cache_basis * targetRatio)
read/create 按 tracker 是否已有 read/create 证据分配
input_tokens = rawInput
total_input_tokens = rawInput + cache_read + cache_creation
```

## 策略二：独立四字段 Reported Usage 整形

这是 `current_high_cache` 的外层 usage 投影能力，也是当前项目缓存策略页面最需要复用的配置字段。它不改变 tracker，不改变上游请求，不改变真实模型输出，只改变响应和 usage record 中的最终 usage 字段。

### 单字段配置 `ReportedUsageFieldPolicy`

四个字段 `input`、`output`、`cacheRead`、`cacheCreation` 都使用同一个结构。

| 字段 | 默认值 | 作用 |
| --- | --- | --- |
| `mode` | `preserve`，但默认策略中 input/output 会设为 `raw` | 字段模式：`raw`、`preserve`、`sample-max`、`sample-target` |
| `maxTokens` | `0` | `sample-max` 的最大采样上限；`0` 表示不生效 |
| `targetTokens` | `0` | `sample-target` 的目标中心值；`0` 表示不生效 |
| `normalMaxMultiplier` | `1.1` | `sample-target` 的常规最大倍率，必须 `>=1` |
| `moveDeltaToCacheRead` | `false` | input 被压低后的差值是否转入缓存字段；通常只对 input 开启 |

字段模式语义：

| mode | 基础值 | 后续采样 | 说明 |
| --- | --- | --- | --- |
| `raw` | 使用上游 raw usage | 无 | input/output 默认建议 raw，保持真实请求和输出基础值 |
| `preserve` | 使用本地计算值 | 无 | cache read/cache creation 默认建议 preserve，保留 tracker 结果 |
| `sample-max` | 通常使用本地计算值，input 特殊使用 raw input | 在 `maxTokens` 内按桶随机取值 | 用于压制 input 或限制某个 cache 字段 |
| `sample-target` | 通常使用本地计算值，input 特殊使用 raw input | 围绕 `targetTokens * normalMaxMultiplier` 采样 | 用于让 cache creation/output 等形成目标区间 |

采样不是纯随机。它用 seed、策略参数和 usage 当前值经过 `splitmix64` 做确定性采样。同一请求证据相同会稳定，不同请求或不同参数会变化。

### 路径策略配置 `ReportedUsagePathPolicy`

参考实现是路径策略；当前项目应把它改名或映射成“分组策略配置”，但字段语义应保持。

| 字段 | 默认值 | 边界 | 作用 |
| --- | --- | --- | --- |
| `enabled` | `true` | boolean | 是否启用 reported usage 整形 |
| `skipNonStreamUsageProjection` | `false` | boolean | 非流式请求是否跳过本地 usage projection；流式不受影响 |
| `finalCacheReadMaxTokens` | `700000` | `>=0`，`0` 关闭 | 最终 `cache_read_input_tokens` 上限，只向下裁剪 |
| `finalCacheReadJitterMinTokens` | `0` | `>=0` 且 `min<=max<=finalCacheReadMaxTokens` | read 上限扣减下限 |
| `finalCacheReadJitterMaxTokens` | `0` | 同上 | read 上限扣减上限 |
| `finalCacheCreationMaxTokens` | `400000` | `>=0`，`0` 关闭 | 最终 `cache_creation_input_tokens` 上限，只向下裁剪 |
| `finalCacheCreationJitterMinTokens` | `20000` | `>=0` 且 `min<=max<=finalCacheCreationMaxTokens` | creation 上限扣减下限 |
| `finalCacheCreationJitterMaxTokens` | `45000` | 同上 | creation 上限扣减上限 |
| `finalOutputGuardEnabled` | `true` | boolean | 是否启用 output 放大和最终上限 |
| `outputUpliftMinTokens` | `1000` | `>=0` | output 大于该阈值才放大，等于阈值不放大 |
| `outputUpliftPercent` | `50` | `0..=200` | output 放大百分比 |
| `finalOutputMaxTokens` | `200000` | `>=0`，`0` 关闭 | output 最终上限 |
| `finalOutputJitterMinTokens` | `5000` | `>=0` 且 `min<=max<=finalOutputMaxTokens` | output 上限扣减下限 |
| `finalOutputJitterMaxTokens` | `12000` | 同上 | output 上限扣减上限 |
| `input` | `raw` | `ReportedUsageFieldPolicy` | 控制 `input_tokens` |
| `output` | `raw` | `ReportedUsageFieldPolicy` | 控制 `output_tokens` |
| `cacheRead` | `preserve` | `ReportedUsageFieldPolicy` | 控制 `cache_read_input_tokens` |
| `cacheCreation` | `preserve` | `ReportedUsageFieldPolicy` | 控制 `cache_creation_input_tokens` |

### 默认路径覆盖参考

参考实现默认有两个高价值模板：

| 路径 | 默认策略 | 效果 |
| --- | --- | --- |
| `/cc` | `input=sample-max(maxTokens=96, moveDeltaToCacheRead=true)`，`cacheCreation=sample-target(targetTokens=3000, normalMaxMultiplier=1.2)`，其他继承默认 | 输入压到很小，差值转入缓存；cache write 控制在数千 token 级别 |
| `/ha` | `input=sample-max(maxTokens=96, moveDeltaToCacheRead=true)`，其他继承默认 | 输入压到很小，cache read/write 保留 tracker 计算 |

当前项目可以把这两个作为内置策略模板，但名称不能带 Kiro 前缀，例如：

| 当前项目模板名 | 基于参考 | 推荐用途 |
| --- | --- | --- |
| `Claude Code 输入压制 + 小写入` | `/cc` | 验证 input 强压缩、cache creation 目标采样、差值归因 |
| `Claude Code 输入压制 + 保留写入` | `/ha` | 验证 input 强压缩，但 cache creation/read 保留 tracker 大值 |

### 整形执行顺序

`with_reported_cache_usage_policy_and_raw_evidence` 的关键顺序：

1. 如果 `enabled=false`，直接使用 raw usage，不上报本地 prompt cache projection。
2. 判断是否有 cache read 证据：内部计算 read > 0、raw read > 0 或显式 evidence。
3. 计算四个字段基础值：
   - input：`raw`、`sample-max`、`sample-target` 都从 raw input 起算；`preserve` 从内部计算 input 起算。
   - output/cacheRead：`raw` 用上游 raw；其他模式用内部计算。
   - cacheCreation：`raw` 时连 5m/1h breakdown 一起用 raw；其他模式用内部计算。
4. output 字段采样。
5. output 后处理：超过 `outputUpliftMinTokens` 才按 `outputUpliftPercent` 放大，然后应用 `finalOutputMaxTokens - jitter` 上限。
6. cache read 字段采样。
7. cache creation 字段采样，并按新 creation 总量同比裁剪 5m/1h breakdown。
8. input 字段采样。若 input 被压低且 `moveDeltaToCacheRead=true`：
   - 有 read 证据时，差值加入 `cache_read_input_tokens`。
   - 没有 read 证据时，差值加入 `cache_creation_input_tokens`，避免首轮伪造 read。
9. 应用最终 cache read/cache creation guard：只向下裁剪，绝不抬高小值。
10. 重算 `total_input_tokens = input + cache_read + cache_creation`。

### 采样桶

普通字段 `input/output/cacheRead` 的 `sample-max` / `sample-target` 使用三档桶：

| 概率累计 | 采样区间 | 效果 |
| --- | --- | --- |
| 70% | 上限的 `2%..25%` | 大多数请求明显低于上限 |
| 95% | 上限的 `26%..70%` | 部分请求中等值 |
| 100% | 上限的 `71%..100%` | 少量请求接近上限 |

`cacheCreation` 使用专门桶，并根据是否已有 read 区分：

| 场景 | 特殊规则 | 采样桶 |
| --- | --- | --- |
| 已有 cache read | 20% 概率 creation 为 0 | `1%..10%`、`11%..45%`、`46%..85%`、`86%..100%` |
| 没有 cache read | 不允许伪造 read，只采 creation | `1%..12%`、`13%..50%`、`51%..88%`、`89%..100%` |

这解释了为什么测试数据如果长期完全相同，通常是实现或参数有问题：要么 seed/usage 证据没有变化，要么触达硬上限且没有配置 jitter，要么所有请求都走到了同一小上限。合理测试必须覆盖不同 seed、不同策略参数、不同输入规模和触顶/未触顶状态。

## 策略三：Kiro-RS Tool

### 目标效果

`kiro_rs_tool` 是第二种缓存效果，不是 `current_high_cache + reportedUsage` 的简单模板。它更接近工具调用场景：tools 和稳定历史是主要缓存对象，最终 usage 采用 split 口径，让对外 `input_tokens` 被压到一个小范围，同时 read/create 保持大头。

核心特征：

1. 使用独立 cache block 构造，不走普通 high-cache 的 request prelude 和 lookup every block 行为。
2. 默认禁用 `simulation`、禁用 `creationControl`、禁用完整 `reportedUsage`。
3. 仍会使用标准 cache read/write 最终 guard，防止标准字段超过合理上限。
4. 第一次没有 tracker 命中时只能 creation，不能 read。
5. 命中后可以通过 `incrementalCreateEnabled=false` 阻止继续创建新缓存。
6. 可用 `coverageRatio` 和 `maxCoverageTokens` 控制本轮最多覆盖多少稳定内容。
7. 可选把当前 user 消息稳定前缀纳入缓存，默认关闭。

### 参与配置 `KiroRsToolCachePolicy`

| 字段 | 默认值 | 边界 | 作用 |
| --- | --- | --- | --- |
| `coverageRatio` | `1.0` | `0..=1` | 本轮可覆盖稳定内容比例 |
| `maxCoverageTokens` | `0` | `>=0`，`0` 不限制 | 本轮最大覆盖 token；在比例之后裁剪 |
| `incrementalCreateEnabled` | `true` | boolean | 命中 read 后是否仍允许为新增稳定内容创建缓存 |
| `maxNewCreationTokensPerRequest` | `0` | `>=0`，`0` 不限制 | 单请求最多新增 creation token |
| `cacheCurrentUserStablePrefix` | `false` | boolean | 是否缓存当前 user 消息的稳定前缀 |
| `currentUserStablePrefixMaxTokens` | `0` | `>=0`；关闭当前用户前缀时强制归零 | 当前 user 前缀最多 token |
| `reportedInputMinTokens` | `32` | `>=0` 且 `min<=max` | split usage 后对外 input 的最小值 |
| `reportedInputMaxTokens` | `4096` | `>=0`，`0` 表示无上限 | split usage 后对外 input 的最大值 |

### Cache block 构造

`kiro_rs_tool_cache_blocks` 的构造规则：

1. 加入所有 tools 定义。
2. system 从第一个显式 `cache_control` block 开始纳入；如果都没有显式 cache_control，则从第一个 system 开始。
3. 最后一个已加入的 system/tool block 默认补一个 5 分钟断点。
4. 遍历 messages：
   - 非最后一条消息默认在消息末尾加 5 分钟断点。
   - 最后一条消息不自动加断点，避免把当前用户最新输入全部当成稳定历史。
   - 如果 block 自己有 `cache_control`，使用其 TTL。
5. 如果 `cacheCurrentUserStablePrefix=true` 且最后一条是 user 且没有显式 cache_control，则截取当前 user 文本稳定前缀，并强制加 5 分钟断点。
6. canonicalize 同样会忽略 `cache_control` 和 volatile id，保证稳定内容可以跨轮命中。

### 读写计算

请求前计算：

```text
covered = lastLookupPoint.cumulativeTokens
covered = floor(covered * coverageRatio)
if maxCoverageTokens > 0: covered = min(covered, maxCoverageTokens)
read = longestMatchedLookupPoint.min(entry.cachedTokens).min(covered)
creation = max(covered - read, 0)
if !incrementalCreateEnabled && read > 0: creation = 0
if maxNewCreationTokensPerRequest > 0: creation = min(creation, maxNewCreationTokensPerRequest)
```

成功后写入：

```text
committedCacheTokens = read + creation
for each lookup point:
  cachedTokens = min(point.cumulativeTokens, committedCacheTokens)
  if cachedTokens > 0: write entry
```

和普通 high-cache 不同，Kiro-RS Tool 不按 `targetReadRatio` 把每个 lookup point 比例映射到 target，而是直接用 `read + creation` 作为已确认可覆盖 token 上限。

### Split usage 输出

Kiro-RS Tool 把 `PromptCacheUsage` 转为 `CacheSimulation.from_prompt_cache_split_input_with_reported_input_range`：

1. `total_input_tokens` 先保持真实输入总量。
2. `cache_total = read + creation`，如果有 `effective_cache_ratio`，会按比例重新分配 cache_total。
3. `input_tokens = total_input_tokens - cache_total`。
4. 再用 `reportedInputMinTokens..reportedInputMaxTokens` 对 input 做确定性抖动。
5. 如果目标 input 比当前 input 大，优先减少 creation，再减少 read。
6. 如果目标 input 比当前 input 小，把差值加到 creation。
7. 最后保证 `input + cache_read + cache_creation = total_input_tokens`。

因此 Kiro-RS Tool 的“输入很小、缓存很大”不是通过 `reportedUsage.input.sample-max` 实现，而是 split usage 的核心逻辑。当前项目如果保留该策略类型，应在 UI 上单独展示它的字段，不能把它和普通四字段整形混成一组。

## Creation 频次控制

`PromptCacheCreationControlConfig` 只适用于普通 high-cache 的本地 prompt cache usage，上游请求成功后、记录 usage 前进行。它不会改变 tracker 命中和真实请求。

| 字段 | 默认值 | 边界 | 作用 |
| --- | --- | --- | --- |
| `enabled` | `true` | boolean | 是否启用 creation 上报频次控制 |
| `scopeMode` | `conversation_model` | `credential_conversation_model` 或 `conversation_model` | 状态维度：是否按凭据隔离 |
| `minSuccessfulRequestsBetweenCreation` | `3` | `<=10000` | 两次 creation 之间至少间隔成功请求数 |
| `minCreationIntervalSecs` | `60` | `<=604800` | 两次 creation 之间至少间隔秒数；`0` 关闭 |
| `minCreationDeltaTokens` | `12000` | `>=0` | 被抑制 creation 累计到该值才允许下一次 creation；`0` 关闭 |
| `maxCreationTokensPerEvent` | `30000` | `>=0` | 单次 creation 上限；`0` 不限制 |
| `creationBudgetWindowSecs` | `300` | `<=604800` | creation 额度窗口；`0` 关闭窗口控制 |
| `maxCreationTokensPerWindow` | `120000` | `>=0` | 单窗口 creation 总上限；`0` 不限制 |
| `expireAfterIdleSecs` | `3600` | `<=2592000` | 控制器状态空闲过期时间；`0` 不按空闲清理 |

控制逻辑：

1. 首次 creation 默认允许，但仍受单次和窗口上限。
2. 非首次 creation 需要同时满足请求间隔、时间间隔、累计 delta 门槛。
3. 单次上限和窗口剩余额度会做确定性扣减：大约从上限扣掉 `max/33..max*12%`，避免全部撞硬上限。
4. 被抑制的 creation 不会消失，而是加回 `input_tokens`，保持 `input + read + creation` 总口径合理。
5. 成功请求没有 creation 时，只增加成功请求计数。

当前项目如果实现该控制，应只作为 `current_high_cache` 的可选高级项；`kiro_rs_tool` 参考实现默认关闭。

## 缓存时间类型设计

参考实现已有两个不同时间概念：

| 概念 | 代表字段 | 作用 |
| --- | --- | --- |
| tracker entry 生命周期 | `CacheBoundsPolicy.entryTtlSecs` | 本地 entry 多久过期，用于 read 命中和淘汰 |
| usage creation breakdown | `cache_creation_5m_input_tokens` / `cache_creation_1h_input_tokens` | 对外展示本轮 creation 属于 5m 还是 1h |

当前新增的“缓存时间类型”应只影响 usage breakdown，不应改变 tracker entry TTL。建议字段：

```text
cacheCreationTTLField: null | "5m" | "1h"
```

语义：

| 值 | 输出行为 |
| --- | --- |
| `null` 或空 | 只输出标准 `cache_creation_input_tokens` 和 `cache_read_input_tokens`；不生成或置零 `cache_creation_5m_input_tokens` / `cache_creation_1h_input_tokens` |
| `5m` | `cache_creation_5m_input_tokens = cache_creation_input_tokens`，`cache_creation_1h_input_tokens = 0` |
| `1h` | `cache_creation_5m_input_tokens = 0`，`cache_creation_1h_input_tokens = cache_creation_input_tokens` |

要求：

1. 默认必须不选择，不能默认 5m。
2. 该字段在所有 usage 整形完成后执行，保证分项等于最终 creation，而不是中间 creation。
3. `cache_creation_5m_input_tokens + cache_creation_1h_input_tokens <= cache_creation_input_tokens`。
4. 该字段不改变 cache block 的真实 TTL、不改变 tracker entry 的过期时间、不改变是否命中 read。

## 当前项目映射方案

### 策略绑定位置

当前项目应把参考实现的路径覆盖改成分组绑定：

| Kiro-RS | 当前项目 |
| --- | --- |
| `cachePolicy.pathOverrides["/cc"]` | 分组绑定某个缓存策略 |
| `cachePolicy.pathOverrides["/ha"]` | 另一个分组绑定另一套策略 |
| `routeNamespace` 路径前缀 | 分组 ID 或策略绑定 ID 作为 namespace |
| `reportedUsage.pathOverrides` | 策略实体内的 `reportedUsage` 配置 |

一个分组最多绑定一个缓存策略。绑定时后端必须检查唯一性：如果分组已绑定策略，应返回明确错误，包含分组名称、当前策略名称和不能重复绑定的原因。

### 协议入口生效

生效限制应放在协议入口，而不是仅靠 group type：

1. 只在 Claude/Anthropic Messages 兼容请求链路解析到分组策略后生效。
2. 同一策略可被 Kiro auth、Claude Code base_url + key、其他 Claude Code 兼容上游账号复用。
3. 非 Claude Code/Anthropic Messages 兼容入口不应读取这些 usage 策略，避免 OpenAI/Gemini 等协议误用 Claude cache 字段。
4. 分组内账号是否全是兼容上游可由业务配置约束，但 runtime 仍应以协议入口做最后防线。

### 策略实体建议字段

建议当前项目策略配置保留参考字段语义，并增加 `cacheCreationTTLField`：

```json
{
  "type": "current_high_cache",
  "simulation": {
    "enabled": true,
    "targetReadRatio": 0.98,
    "tokenScale": 1.6,
    "maxSimulatedInputTokens": 300000,
    "capJitterMinTokens": 12000,
    "capJitterMaxTokens": 24000,
    "scaleMinInputTokens": 20000
  },
  "creationControl": {
    "enabled": true,
    "scopeMode": "conversation_model",
    "minSuccessfulRequestsBetweenCreation": 3,
    "minCreationIntervalSecs": 60,
    "minCreationDeltaTokens": 12000,
    "maxCreationTokensPerEvent": 30000,
    "creationBudgetWindowSecs": 300,
    "maxCreationTokensPerWindow": 120000,
    "expireAfterIdleSecs": 3600
  },
  "reportedUsage": {
    "enabled": true,
    "skipNonStreamUsageProjection": false,
    "finalCacheReadMaxTokens": 700000,
    "finalCacheReadJitterMinTokens": 0,
    "finalCacheReadJitterMaxTokens": 0,
    "finalCacheCreationMaxTokens": 400000,
    "finalCacheCreationJitterMinTokens": 20000,
    "finalCacheCreationJitterMaxTokens": 45000,
    "finalOutputGuardEnabled": true,
    "outputUpliftMinTokens": 1000,
    "outputUpliftPercent": 50,
    "finalOutputMaxTokens": 200000,
    "finalOutputJitterMinTokens": 5000,
    "finalOutputJitterMaxTokens": 12000,
    "input": { "mode": "raw", "maxTokens": 0, "targetTokens": 0, "normalMaxMultiplier": 1.1, "moveDeltaToCacheRead": false },
    "output": { "mode": "raw", "maxTokens": 0, "targetTokens": 0, "normalMaxMultiplier": 1.1, "moveDeltaToCacheRead": false },
    "cacheRead": { "mode": "preserve", "maxTokens": 0, "targetTokens": 0, "normalMaxMultiplier": 1.1, "moveDeltaToCacheRead": false },
    "cacheCreation": { "mode": "preserve", "maxTokens": 0, "targetTokens": 0, "normalMaxMultiplier": 1.1, "moveDeltaToCacheRead": false }
  },
  "bounds": {
    "maxEntriesPerAccount": 200,
    "maxEntriesGlobal": 20000,
    "entryTtlSecs": 86400,
    "estimatedBytesLimit": 268435456
  },
  "cacheCreationTTLField": null
}
```

`kiro_rs_tool` 策略应保留独立配置：

```json
{
  "type": "kiro_rs_tool",
  "kiroRsTool": {
    "coverageRatio": 1.0,
    "maxCoverageTokens": 0,
    "incrementalCreateEnabled": true,
    "maxNewCreationTokensPerRequest": 0,
    "cacheCurrentUserStablePrefix": false,
    "currentUserStablePrefixMaxTokens": 0,
    "reportedInputMinTokens": 32,
    "reportedInputMaxTokens": 4096
  },
  "bounds": {
    "maxEntriesPerAccount": 200,
    "maxEntriesGlobal": 20000,
    "entryTtlSecs": 86400,
    "estimatedBytesLimit": 268435456
  },
  "cacheCreationTTLField": null
}
```

### UI 设计要求

缓存策略表单应按策略类型切换字段，而不是把所有字段堆在一个弹层里。

`current_high_cache` 页面分组：

| UI 分区 | 字段 |
| --- | --- |
| 基础 | 策略名称、启用状态、策略类型、缓存时间类型 |
| 高缓存模拟 | `targetReadRatio`、`tokenScale`、`maxSimulatedInputTokens`、`capJitterMinTokens`、`capJitterMaxTokens`、`scaleMinInputTokens` |
| 输入字段 | `input.mode`、`maxTokens`、`targetTokens`、`normalMaxMultiplier`、`moveDeltaToCacheRead` |
| 输出字段 | `output.mode`、`maxTokens`、`targetTokens`、`normalMaxMultiplier` |
| 读取缓存字段 | `cacheRead.mode`、`maxTokens`、`targetTokens`、`normalMaxMultiplier`、最终上限和抖动 |
| 创建缓存字段 | `cacheCreation.mode`、`maxTokens`、`targetTokens`、`normalMaxMultiplier`、最终上限和抖动 |
| 创建频次控制 | `creationControl.*` |
| tracker 边界 | `bounds.*` |

`kiro_rs_tool` 页面分组：

| UI 分区 | 字段 |
| --- | --- |
| 基础 | 策略名称、启用状态、策略类型、缓存时间类型 |
| 覆盖范围 | `coverageRatio`、`maxCoverageTokens` |
| 增量写入 | `incrementalCreateEnabled`、`maxNewCreationTokensPerRequest` |
| 当前用户稳定前缀 | `cacheCurrentUserStablePrefix`、`currentUserStablePrefixMaxTokens` |
| 对外输入范围 | `reportedInputMinTokens`、`reportedInputMaxTokens` |
| tracker 边界 | `bounds.*` |

`no_cache` 页面只需要基础字段和说明，不显示 usage 整形参数。

### 旧 Kiro 分组缓存配置处理

当前项目是新系统，不需要兼容旧配置。因此后续实现应：

1. 删除或隐藏分组选择 Kiro 类型时的旧缓存策略字段。
2. Kiro auth 上游也通过分组绑定的通用策略获得缓存参数。
3. Kiro auth、Claude Code base_url/key 等账号类型只是消费策略参数的适配器，不拥有独立缓存配置模型。
4. 使用记录中应记录策略 ID、策略名称、策略类型、分组 ID/名称，便于从页面直接判断某条 usage 由哪个策略产生。

## 合理性边界

### 首轮不能读缓存

没有 tracker 命中的首轮必须满足：

```text
cache_read_input_tokens = 0
cache_creation_input_tokens >= 0
```

如果 input 被压制且开启 `moveDeltaToCacheRead`，但本轮没有 read 证据，差值应进入 `cache_creation_input_tokens`，不能进入 `cache_read_input_tokens`。

### 失败不能写缓存

这些情况不能提交 tracker：

| 场景 | 期望 |
| --- | --- |
| 上游 4xx/5xx | 不写 tracker，不增加后续 read 命中 |
| 流式中断 | 不写 tracker |
| 客户端取消 | 不写 tracker |
| payload 超限失败 | 不写 tracker，可记录失败 usage，但 cache 字段不应制造成功命中 |

### 上限不能制造大值

`finalCacheReadMaxTokens`、`finalCacheCreationMaxTokens`、`finalOutputMaxTokens` 只向下裁剪。如果 tracker 计算出 2.5k，配置 300k 上限后仍应是 2.5k，不应抬高到 300k。

### 抖动必须可见但受控

如果测试数据整页都是完全相同的 `2.5k`、`1.8k` 这类值，需要检查：

1. 是否所有请求命中同一个小 `sample-target`。
2. 是否 seed 没有引入请求证据、策略参数或 profile fingerprint。
3. 是否触顶但 `final*JitterMin/Max` 为 0。
4. 是否所有 mock 请求输入规模太小，无法覆盖几十 k/几百 k 场景。

合理策略应允许：

| 场景 | 期望数据特征 |
| --- | --- |
| 输入压制到 100 内 | `input_tokens` 多数在几十 token，不总是同一个值 |
| 保留大 read | 连续多轮后 `cache_read_input_tokens` 可达几十 k、几百 k，但不超过模型/配置上限 |
| 小写入目标 | `cache_creation_input_tokens` 在目标倍率内波动，可出现 0 |
| 大写入目标 | creation 可到几十 k/百 k 级，并受 creation guard 与窗口控制 |
| 触顶上限 | 结果接近 `max - jitter`，不是固定整数硬顶 |

### Claude 合理上限

对外 usage 必须遵守基本常识：

1. 标准字段不应超过模型上下文窗口，尤其不能出现超过 1M 的 cache read/create。
2. `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` 应是当前系统最终记录的 total input 口径。
3. `cache_creation_5m_input_tokens + cache_creation_1h_input_tokens` 不得大于 `cache_creation_input_tokens`。
4. Kiro-like 上游不返回 cache usage 时，本地 tracker 可以生成 cache fields，但不能宣称来自上游。
5. 模型不支持 prompt caching 或输入低于最小可缓存长度时，不应产生本地 cache read/create。

## 测试验收场景

后续实现完成后，应通过真实网关调度测试，而不是直接插 usage 数据。测试应使用同一套本地服务，通过公开 Anthropic Messages 入口请求 mock Kiro/Claude Code 兼容上游。

| 场景 | 策略配置 | 期望 |
| --- | --- | --- |
| 无策略绑定 | 分组不绑定策略 | 调用成功，usage 使用原始/默认路径；不产生策略 ID |
| `no_cache` | 分组绑定 no cache | read/create 都为 0；不写 tracker |
| 高缓存默认 | `current_high_cache` 默认 | 首轮 read=0；后续多轮逐步出现 read；creation 受默认上限和频次控制 |
| 输入强压制 | input `sample-max maxTokens=96 moveDeltaToCacheRead=true` | 多轮 input 在 1..96 的采样区间；有 read 证据后差值进入 read，无 read 证据时进入 creation |
| 输入极限压制 | input `sample-max maxTokens=10` 或 `100` | 验证小 input 不固定为同一个值，总口径守恒 |
| 小写入目标 | cacheCreation `sample-target targetTokens=3000 multiplier=1.2` | creation 多数在 0..3600 内波动，已有 read 时允许部分轮次为 0 |
| 大写入目标 | cacheCreation `sample-target targetTokens=150000 multiplier=1.5` | creation 可到几十 k/百 k，但受 final creation guard 和 creation control 限制 |
| read 限制 | `finalCacheReadMaxTokens=300000 jitter=8000..24000` | 大 read 被裁剪到约 276k..292k，小 read 不被抬高 |
| creation 限制 | `finalCacheCreationMaxTokens=180000 jitter=9000..21000` | 大 creation 被裁剪到约 159k..171k，小 creation 不被抬高 |
| 输出整形 | output `sample-target` + uplift + final output cap | output 字段随参数变化，不影响 tracker 和 cache read/write |
| Kiro-RS Tool 默认 | `kiro_rs_tool` 默认 | input 在 32..4096 内波动；首轮 read=0；后续 read 成为主体 |
| Kiro-RS Tool 限覆盖 | `coverageRatio=0.3` 或 `maxCoverageTokens=50000` | read/create 明显小于默认工具策略 |
| Kiro-RS Tool 禁增量 | `incrementalCreateEnabled=false` | 命中 read 后新增 creation 为 0 或显著减少 |
| 缓存时间不选 | `cacheCreationTTLField=null` | 不返回或置零 5m/1h breakdown |
| 缓存时间 5m | `cacheCreationTTLField=5m` | creation 全部进入 5m 分项 |
| 缓存时间 1h | `cacheCreationTTLField=1h` | creation 全部进入 1h 分项 |
| 失败请求 | mock 上游返回失败或流中断 | 不提交 tracker；下一轮不能因为失败轮次产生 read |

每种策略至少需要 10 轮以上连续会话，问题要有关联性，让历史增长、工具调用和稳定前缀真实参与。测试期间不能清掉前一组策略的 usage，直到整轮矩阵完成，方便在使用记录页面按分组、key、策略名称对比。

## 当前项目后续实施阶段

### 阶段 1：配置模型收敛

1. 将缓存策略类型改为 `no_cache`、`current_high_cache`、`kiro_rs_tool`。
2. 策略配置 JSON 保留 `simulation`、`creationControl`、`reportedUsage`、`bounds`、`kiroRsTool` 字段语义。
3. 增加 `cacheCreationTTLField`。
4. 删除 Kiro 分组旧缓存配置入口。
5. 后端保存和返回策略时做 normalized + validate。

### 阶段 2：协议入口运行时

1. 在 Anthropic Messages/Claude Code 兼容入口解析分组绑定策略。
2. 按分组 ID 或绑定 ID 构造 `PromptCacheScope.route_namespace`。
3. 根据策略类型选择 high-cache 或 Kiro-RS Tool 计算器。
4. 只要分组绑定并启用缓存策略，就必须按策略生成最终 usage。上游 usage 只作为 raw evidence；不能因为上游有 cache 字段、没有 cache 字段或 cache 字段为空而跳过策略投影。
5. 成功后 commit tracker；失败不 commit。

### 阶段 3：页面与模板

1. 缓存策略列表显示中文策略名称、类型、绑定分组、缓存时间类型、关键 input/output/read/write 参数。
2. 新建/编辑弹层按策略类型切换字段。
3. 内置模板覆盖输入强压制、小写入、高 read、大 creation、Kiro-RS Tool 默认、Kiro-RS Tool 限覆盖、无缓存。
4. 绑定分组时检查唯一性并给出明确报错。

### 阶段 4：真实调度验证

1. 清理 usage、实验分组、实验策略和 mock 账号一次。
2. 为每种策略创建独立中文命名分组、key 和 mock 上游账号。
3. 通过当前唯一业务服务执行连续多轮真实 Anthropic Messages 调度。
4. 从使用记录页面和数据库核对 usage 终值。
5. 如果发现字段未参与运算、数值不合理、首轮 read、失败写入、策略间差异不明显，应修复后重跑对应场景。

## 实现注意事项

1. `Kiro-RS Tool` 是策略效果名，可以保留在内部类型中，但默认模板和页面文案不应把所有策略都加 Kiro 前缀。
2. 对用户暴露的是输入、输出、读取缓存、创建缓存、缓存时间、创建频次和边界，不应暴露难以理解的内部调试字段作为主路径。
3. `reportedUsage` 的四字段配置必须真正参与最终 usage；设置没作用就是实现错误。
4. 同一策略参数变化必须能在多轮 usage 中体现差异：例如 input cap 96 与 input cap 10000、creation target 3000 与 150000、read cap 300000 与 700000。
5. 本地 mock Kiro 上游默认不返回 cache usage 字段；这样才能验证本地策略计算，而不是透传上游缓存字段。
6. 真实上游如果返回 cache usage，当前策略仍应按配置决定 raw/preserve/sample，不能无条件透传。
7. usage 记录要同时保存 raw usage 证据和 shaped usage 终值，方便证明策略参与运算。
8. 不要把 payload guard 的历史裁剪作为缓存策略的一部分。payload 超限修复属于另一个模块。

## 源码依据

本分析基于以下参考实现文件：

| 文件 | 关键内容 |
| --- | --- |
| `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs/src/model/config.rs` | 策略类型、reported usage 字段、simulation、creation control、bounds、Kiro-RS Tool 配置和默认值 |
| `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs/src/anthropic/prompt_cache.rs` | Prompt cache tracker、cache block 构造、read/create 计算、Kiro-RS Tool profile、TTL breakdown |
| `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs/src/anthropic/cache.rs` | CacheSimulation、四字段 reported usage 整形、采样桶、input delta 归因、final guard |
| `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs/src/anthropic/prompt_cache_creation_control.rs` | creation 频次控制、单次和窗口上限、抑制 creation 回填 input |
| `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs/src/anthropic/handlers.rs` | 本地请求链路如何构建策略上下文、何时应用 reported usage、何时 commit 成功 cache |
| `/Users/yuanfeijie/Desktop/project/2ue_kiro.rs/src/external_pool/usage_projection.rs` | 外部池路径如何复用同一套策略和成功后 commit |

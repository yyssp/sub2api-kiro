# 通用 Claude Code 缓存策略参数指南

更新日期：2026-09-12
适用范围：当前项目基于 Claude Code/Anthropic Messages usage 语义实现的分组缓存策略。
协议说明：这不是 Kiro 专用配置。Kiro 只是其中一个验证来源；只要其他协议能映射到
相同的稳定前缀和 usage 字段，也可以复用这套策略。当前项目没有把 `/cc`、`/ha`
等路径作为策略开关。

> 本文是当前项目代码的配置说明，以 `backend/internal/service/cache_strategy.go`
> 和 `backend/internal/service/cache_runtime.go` 的实现为准。旧文档中带有
> Kiro-RS 路径名称或旧默认值的内容只作为历史分析，不要直接照抄。

## 先记住三层

缓存策略同时处理三件事，但它们不是同一件事：

| 层 | 处理什么 | 影响真实上游请求 | 影响最终 usage |
|---|---|:---:|:---:|
| 缓存前缀 tracker | 记录哪些稳定前缀可以命中、写入和复用 | 否 | 间接决定 read/create 的基础值 |
| Creation 控制 | 控制新 creation 的释放频次、单次和窗口预算 | 否 | 主要影响 creation 出现频次和数值 |
| Usage projection | 整形 `input/output/cache_read/cache_creation` | 否 | 是，直接改变响应和 usage 记录 |

因此：

- `coverage_ratio`、缓存内容范围、断点、TTL、作用域等，优先影响**实际能不能命中**
  和**实际能写多少前缀**。
- `usage.*`、`final_*_max_tokens`、output uplift 等，优先影响**对外显示多少**
  和**usage 记录怎么计**，不会改变发送给上游的 prompt。
- 策略不会缓存模型输出，也不会修改模型回答正文、模型选择或请求参数。

## 一次请求的执行顺序

为了判断某个参数最终影响什么，可以按下面顺序理解：

1. 解析请求并构造稳定前缀 profile。
2. 按缓存内容范围、动态内容规则、断点模式过滤可缓存块。
3. 按分组、会话、账号、策略版本和协议确定 cache scope。
4. 计算当前请求可以覆盖的前缀总量。
5. 从 tracker 查找最长有效命中，得到基础 `cache_read`。
6. 计算尚未命中的新增前缀，得到基础 `cache_creation`。
7. 应用单请求 creation 上限、增量创建开关和 Creation 控制。
8. 应用读写比例，得到 usage projection 的输入。
9. 对 `input/output/cache_read/cache_creation` 分别执行字段模式。
10. 应用最终 read/write/output 上限和触顶抖动。
11. 成功响应后提交缓存状态；失败、上游错误、流中断或客户端取消不应提交。

基础关系可以简化为：

```text
可覆盖前缀 = min(请求稳定前缀 × coverage_ratio, max_coverage_tokens)
cache_read = 已命中的最长稳定前缀
cache_creation = 可覆盖前缀 - cache_read
```

最终 Claude usage 通常满足：

```text
input_tokens + cache_read_input_tokens + cache_creation_input_tokens
  = 对外报告的总输入
```

OpenAI 兼容响应会把三部分合并到 `input_tokens` 总量，同时保留 read/create 字段。

## 参数影响总表

下表先给出“该参数主要改变什么”，后面再逐项解释。

| 参数 | 主要影响 | 实际缓存状态 | read 数值 | creation 数值/频次 | input/output 数值 |
|---|---|---|---|---|---|
| `kind` | 缓存算法形态 | 高 | 间接 | 间接 | 间接 |
| `cache_system` / `cache_tools` | 是否缓存对应内容 | 高 | 可能增减 | 可能增减 | 间接 |
| `cache_history` | 是否缓存历史消息 | 高 | 通常显著影响 | 通常显著影响 | 间接 |
| `cache_tool_results` | 是否缓存工具结果 | 高 | 工具会话中影响明显 | 工具会话中影响明显 | 间接 |
| `cache_current_user_stable_prefix` | 是否把当前用户稳定前缀纳入缓存 | 高 | 可提高下一轮 read | 提高当前/下一轮可写前缀 | 间接 |
| `current_user_stable_prefix_max_tokens` | 当前用户稳定前缀的上限 | 高 | 限制可读前缀 | 限制可写前缀 | 间接 |
| `breakpoint_mode` | 断点从哪里来 | 高 | 影响命中位置 | 影响可写点 | 间接 |
| `dynamic_content_mode` | 动态字段是否参与前缀 | 高 | 通常影响命中率 | 通常影响重建频率 | 间接 |
| `scope_mode` | 缓存是否跨账号共享 | 高 | 换账号时是否继续命中 | 换账号时是否重写 | 间接 |
| `allow_derived_session` | 无 session id 时是否生成 scope | 高 | 决定是否有缓存计划 | 决定是否能写 | 间接 |
| `coverage_ratio` | 可覆盖缓存前缀比例 | 高 | 上限/基础值下降 | 上限/基础值下降 | 间接 |
| `usage_ratio` | 统一读写 evidence 比例 | 中 | 读上报缩放 | 写上报缩放 | 间接 |
| `read_ratio` / `creation_ratio` | 独立读写 evidence 比例 | 中 | 只缩放 read 上报 | 只缩放 creation 上报 | 间接 |
| `max_coverage_tokens` | 实际最大覆盖量 | 高 | read 基础值上限 | creation 基础值上限 | 间接 |
| `max_new_creation_tokens_per_request` | 单请求真实新增前缀上限 | 高 | 间接 | 直接限制 creation | 间接 |
| `incremental_create_enabled` | 命中后是否继续增量写 | 高 | 不改变已有 read | 关闭后命中请求可为 0 | 间接 |
| `min_cacheable_tokens` | 最小可缓存断点 | 高 | 小于阈值不计 read | 小于阈值不写 | 间接 |
| `model_min_cacheable_overrides` | 按模型覆盖最小阈值 | 高 | 模型相关 | 模型相关 | 间接 |
| `token_scale` | usage 模拟/放大基数 | 否 | 可间接放大投影值 | 可间接放大投影值 | 主要影响 input |
| `scale_min_input_tokens` | 何时启用 `token_scale` | 否 | 间接 | 间接 | input |
| `max_simulated_input_tokens` | 模拟 input 总上限 | 否 | 间接限制 | 间接限制 | input |
| `reported_input_min/max_tokens` | 报告总 input 的上下限 | 否 | 间接 | 间接 | 直接影响 input |
| `uncached_input_min/max_tokens` | 缓存拆分后未缓存 input 的保底区间 | 否 | 会从 read 中退回 | 会从 creation 中退回 | 直接影响 input |
| `cap_jitter_min/max_tokens` | 模拟 input 触顶时的回退 | 否 | 间接 | 间接 | input |
| `usage.enabled` | 是否执行字段投影 | 否 | 可能保留原始/计划值 | 可能保留原始/计划值 | 直接 |
| `usage.input` | input 字段模式 | 否 | 可能通过差值转移间接改变 | 可能通过差值转移间接改变 | 直接 |
| `usage.output` | output 字段模式 | 否 | 无 | 无 | 直接影响 output |
| `usage.cache_read` | read 字段模式 | 否 | 直接 | 无 | 间接总量 |
| `usage.cache_creation` | creation 字段模式 | 否 | 无 | 直接 | 间接总量 |
| `move_delta_to_cache_read` | input 被压低后的差值归属 | 否 | 有 read 证据时增加 read | 冷请求不会伪造 read | input/read |
| `final_cache_read_max_tokens` | 最终 read 上限 | 否 | 直接向下裁剪 | 无 | 间接总量 |
| `final_cache_creation_max_tokens` | 最终 creation 上限 | 否 | 无 | 直接向下裁剪 | 间接总量 |
| `final_*_jitter_*` | 触顶值的确定性波动 | 否 | 触顶时变化 | 触顶时变化 | output 触顶时变化 |
| `output_uplift_*` | output 放大 | 否 | 无 | 无 | 直接影响 output |
| `final_output_guard_enabled` | output 放大和上限总开关 | 否 | 无 | 无 | 直接影响 output |
| `final_output_max_tokens` | output 最终上限 | 否 | 无 | 无 | 直接向下裁剪 |
| `creation_control.enabled` | 是否启用创建频控 | 通常不改变已有前缀 | 不改变 read | 直接影响出现频次/数值 | 间接 |
| `min_creation_delta_tokens` | creation 释放阈值 | 通常不清除缓存 | 不改变 read | 小增量暂缓，累积后释放 | 间接 |
| `min_successful_requests_between` | 两次 creation 间成功请求数 | 不改变已有缓存 | 不改变 read | 直接降低 creation 频次 | 间接 |
| `min_creation_interval_seconds` | 两次 creation 最短时间 | 不改变已有缓存 | 不改变 read | 直接降低 creation 频次 | 间接 |
| `max_creation_tokens_per_event` | 单次 creation 报告上限 | 不改变已有缓存 | 无 | 直接限制单次可见数值 | 间接 |
| `creation_budget_window_seconds` | creation 预算统计窗口 | 不改变已有缓存 | 不改变 read | 决定预算多久恢复 | 间接 |
| `max_creation_tokens_per_window` | 窗口 creation 报告预算 | 不改变已有缓存 | 无 | 预算耗尽时 creation 可为 0 | 间接 |
| `default_ttl_seconds` / `hour_ttl_seconds` | 断点生命周期 | 高 | 过期后 read 下降 | 过期后需要重写 | 间接 |
| `max_entries_per_scope` | 单 scope 容量 | 高 | 淘汰后 read 下降 | 需要重建 | 间接 |
| `max_entries_global` | 全局容量 | 高 | 全局淘汰后 read 下降 | 需要重建 | 间接 |
| `estimated_bytes_limit` | 估算内存容量 | 高 | 容量不足会淘汰 | 需要重建 | 间接 |
| `expire_after_idle_seconds` | 空闲淘汰时间 | 高 | 长时间不访问后 read 下降 | 需要重建 | 间接 |

## 一、缓存内容和命中范围

### `kind`

当前值：

- `prefix`：通用稳定前缀缓存，适合大多数 Claude Code 兼容流量。
- `tool_aware`：对工具、历史和工具结果做更细的块过滤，适合工具密集型会话。
- `disabled`：关闭本地缓存和本地缓存 usage。

它不会修改上游请求。它改变的是缓存 profile 如何建立，因此会间接改变 read/create
的机会和数值。

### `cache_system`

是否把 system 内容纳入稳定前缀。关闭后，system 不会贡献缓存 read/create。
如果 system 占请求很大，关闭会明显降低首轮 creation 和后续 read。

### `cache_tools`

是否缓存 tools 定义。工具定义通常跨轮稳定，开启后对工具密集会话的 read 贡献较大。
如果工具定义每轮变化，开启可能降低命中稳定性。

### `cache_history`

是否缓存已经结束的历史消息。长会话中这是影响 read 最大的开关之一。关闭后，
每轮只能依赖 system、tools 或当前用户稳定前缀，历史增长不会形成可复用前缀。

### `cache_tool_results`

是否把 tool result 纳入缓存。工具结果稳定时可以提高命中；结果含时间戳、随机 id 或
动态状态时，建议关闭或同时保持 `dynamic_content_mode=exclude`。

### `cache_current_user_stable_prefix`

是否允许把当前用户消息中稳定的前缀纳入缓存。

- `false`：更保守。自动/混合断点不会把当前动态用户轮次整体写入。
- `true`：适合大上下文压测或用户消息本身有很长稳定前缀的场景，会提高可写和下一轮
  可读的上限。

### `current_user_stable_prefix_max_tokens`

当前用户稳定前缀最多纳入多少 token。只有
`cache_current_user_stable_prefix=true` 时有效；关闭时后端会清零。

它同时影响：

- 当前轮最多能产生多少 creation；
- 下一轮最多能从该部分产生多少 read；
- 不改变原始请求内容。

### `breakpoint_mode`

- `client_only`：只使用客户端明确给出的 `cache_control` 断点。客户端没有断点时，
  可能完全没有可缓存内容。
- `auto`：系统自动生成断点。自动模式会避开当前动态用户轮次，除非打开当前用户稳定前缀。
- `hybrid`：优先使用客户端断点，同时允许系统对稳定内容补默认断点。通用默认建议。

断点不是 usage 上限。它决定“哪里可以切断并复用”，因此影响 read/create 的基础值和
写入频率。

### `dynamic_content_mode`

- `exclude`：尽量排除动态、易变化的内容，命中率更稳定。
- `allow`：允许动态内容进入 profile，可能使每轮 fingerprint 改变，导致 read 下降、
  creation 重复增加。只有确认请求内容稳定时才建议使用。

### `scope_mode`

- `group_session`：按“分组 + 会话”隔离，同一会话切换账号仍可以复用缓存。
- `group_account_session`：再加账号维度隔离，换账号后会重新建缓存。

它不改变 token 数值算法，改变的是命中范围和账号切换时的 creation 频率。

### `allow_derived_session`

请求没有明确 session id 时，是否允许从请求内容派生会话标识。

- 开启：更多请求能建立 cache scope，但如果请求本身没有可靠会话边界，可能把不相关
  请求归到同一个派生 scope。
- 关闭：没有明确 session 时通常不会创建缓存计划。

### `min_cacheable_tokens` 和 `model_min_cacheable_overrides`

低于最小阈值的断点不会被当成缓存。全局最小值适合统一约束；模型覆盖用于给某些模型
设置不同的最低粒度。

它们影响的是“是否计入缓存”，不是把小值向上补成阈值。

## 二、实际缓存读写量

### `coverage_ratio`

决定当前请求稳定前缀最多有多少比例可以进入缓存覆盖范围：

```text
可覆盖量 ≈ 稳定前缀 token × coverage_ratio
```

提高它通常会提高 read/create 的基础上限；降低它会同时降低读和写的机会。

它属于 tracker 层，优先影响真实缓存状态，不只是显示值。

### `max_coverage_tokens`

对实际可覆盖前缀设置绝对上限。即使 `coverage_ratio=1`，也不会超过这个值。

- `0`：不设置该层上限。
- `3_000_000`：允许大窗口测试跨多轮继续增长。

如果它过小，会看到 read 很早达到平台期、后续 creation 变成 0。

### `ratio_mode`

- `uniform`：`usage_ratio` 同时作为 read 和 creation 的比例。
- `independent`：分别使用 `read_ratio` 和 `creation_ratio`。

它主要改变对外 evidence 的比例，不改变上游请求内容。

### `usage_ratio`

统一缩放 read/create 的 usage evidence。比如 `0.5`，通常会让对外 read/create
接近原计算值的一半。

它是 usage 层比例，不应被当成实际缓存容量。要限制真正写入前缀，使用
`max_new_creation_tokens_per_request` 或 `incremental_create_enabled`。

### `read_ratio` 和 `creation_ratio`

仅在 `ratio_mode=independent` 时分别生效：

- `read_ratio` 越低，对外 `cache_read` 越小；
- `creation_ratio` 越低，对外 `cache_creation` 越小。

`creation_ratio=0` 表示该策略不产生可见 creation；如果目标是禁止命中后增量写入，
应使用 `incremental_create_enabled=false`，而不是只把比例设为 0。

### `max_new_creation_tokens_per_request`

这是单请求新增缓存前缀的硬上限，属于实际写入侧参数。它会在 profile 写入前裁剪
candidate creation。

适合“真实写入也不能超过某个数”的场景。与 `final_cache_creation_max_tokens` 的区别：

| 参数 | 作用 |
|---|---|
| `max_new_creation_tokens_per_request` | 限制实际状态最多新增多少前缀 |
| `final_cache_creation_max_tokens` | 限制响应/usage 最终显示多少 |

### `incremental_create_enabled`

- `true`：命中已有缓存后，仍可为新增稳定前缀增量写入。
- `false`：已有 read 命中时不继续创建新缓存。

关闭它不会删除已经存在的缓存，也不会降低已有 read；它只让后续新增 creation 变成
0 或不再增长。

## 三、Token 模拟和 input 相关参数

### `token_scale`

当输入达到 `scale_min_input_tokens` 后，usage 模拟基数按该倍数放大，最大不超过
`max_simulated_input_tokens` 和全局安全上限。

重要特性：

- 只影响 usage/cache projection；
- 不放大真实发送给上游的 prompt；
- 不会把短请求强行放大，是否触发由 `scale_min_input_tokens` 决定。

### `scale_min_input_tokens`

启用 `token_scale` 的最低输入阈值。短请求低于该值时保持原始基数，避免几百 token
的请求被模拟成大窗口。

### `max_simulated_input_tokens`

放大后的模拟 input 总上限。达到后会触发 input cap jitter，并间接限制可用于
read/create projection 的总量。

大窗口模板把它设为 `3_000_000`，是为了允许 read 和 creation 同时进入大数值区间；
普通模板可以设为 `300_000`，减少 usage 模拟的跨度。

### `reported_input_min_tokens` / `reported_input_max_tokens`

控制 profile 对外报告的总 input 基数：

- min：不让报告总量低于某个值；
- max：超过后向下裁剪，并可能应用 `cap_jitter_*`。

它们不是 `usage.input.max_tokens`。前者控制整份 profile 的报告总量，后者只控制
字段投影中的 input。

### `uncached_input_min_tokens` / `uncached_input_max_tokens`

当缓存 read/create 把整份总量几乎吃完，Claude 的 `input_tokens` 不能变成不合理的
0 时，系统会从 creation 优先、read 次之退回一小段未缓存 input。

这两个值影响：

- `input_tokens` 的底线；
- 为满足底线而减少多少 read/create；
- 不会抬高原本已经超过下限的 input。

### `cap_jitter_min_tokens` / `cap_jitter_max_tokens`

当 `reported_input_max_tokens`、`max_simulated_input_tokens` 或全局安全上限真正触顶时，
从触顶值向下扣减一个确定性区间。

这和 `final_cache_read_jitter_*` 不同：

- `cap_jitter_*`：input 模拟总量触顶时使用；
- `final_*_jitter_*`：某个最终 read/write/output 字段触顶时使用。

## 四、Usage 字段模式

四个字段都使用同一个结构：

```text
mode
max_tokens
target_tokens
normal_max_multiplier
move_delta_to_cache_read
```

### `raw`

尽量使用上游 raw usage。上游没有对应缓存证据时，当前项目仍会使用本地 plan 的
基础值作为 fallback。

适合：

- `input`、`output` 想尽量保留上游数值；
- `cache_creation` 想观察实际基础写入量；
- 大窗口压测。

### `preserve`

保留当前项目计算出来的值，不再对该字段做 sample-max/sample-target 整形。

适合默认的 `cache_read` 和不需要额外整形的 creation。

### `sample_max`

只保证不超过 `max_tokens`。真正触顶时会做确定性小幅回退，避免每条记录完全一样。

适合：

- 把 input 压到某个上限；
- 把 read 限制到可控区间；
- 把 output 限制到某个最大值。

它是“上限”，不是“每次目标值”。原始值低于上限时不会被抬高。

### `sample_target`

围绕 `target_tokens` 采样，常规上沿为：

```text
target_tokens × 0.85  到  target_tokens × normal_max_multiplier
```

当前实现会用请求级确定性 seed；相同请求重试稳定，不同请求可以变化。

对 `cache_creation`，还会根据是否已有 cache read 使用不同百分比桶；命中已有缓存时
可能合法地返回 0。它是 usage 数值整形，不等于真实写入量。

### `move_delta_to_cache_read`

只建议在 `usage.input` 上配置。

- 有 cache-read 证据：input 被压低的差值转入 `cache_read_input_tokens`。
- 没有 cache-read 证据的冷请求：不会伪造 read；当前实现保留真实未缓存 input，
  避免首轮把普通输入写成缓存命中。

因此，开启它并不保证每个请求都会出现 read。

## 五、最终上限、抖动和 output

### `final_cache_read_max_tokens`

最终对外 `cache_read_input_tokens` 的硬上限，只向下裁剪：

- 值低于上限：保持原值；
- 值超过上限：裁剪到“上限减确定性 jitter”；
- `0`：关闭该最终守护。

它不会删除 tracker 里的缓存，也不会减少下一请求实际可查找的前缀。

### `final_cache_creation_max_tokens`

最终对外 `cache_creation_input_tokens` 的硬上限。它只影响响应和 usage 记录，不直接
缩短已经提交的 tracker 前缀。若要限制实际新增状态，同时设置
`max_new_creation_tokens_per_request`。

### `final_cache_read_jitter_*`、`final_cache_creation_jitter_*`

只有相应字段真正触顶时才使用。系统按请求指纹在区间内确定性扣减：

```text
有效上报值 = final_cap - deterministic_jitter
```

注意：

- 这是向下扣减，不会把小值抬高；
- 同一请求重试稳定，不同请求可以不同；
- 上限小于抖动区间时，后端会按比例归一化；
- 上限为 0 时，区间会被清零；
- 抖动上限不能把结果减到 0。

### `output_uplift_enabled`、`output_uplift_min_tokens`、`output_uplift_percent`

输出放大顺序：

1. 先执行 `usage.output` 字段模式；
2. `output_tokens > output_uplift_min_tokens` 时按百分比放大；
3. 百分比最大归一化到 `200%`；
4. 再执行最终 output 上限。

等于阈值时不放大。它只改变对外 output usage，不改变模型实际输出文本。

### `final_output_guard_enabled`

output 放大和最终 output 上限的总开关：

- `true`：uplift 和 final output cap 生效；
- `false`：两者都不生效，output 保留字段模式处理后的值。

### `final_output_max_tokens` 和 output jitter

最终 output 上限和 read/write 最终上限同样只向下裁剪。触顶时使用
`final_output_jitter_min_tokens` 到 `final_output_jitter_max_tokens` 的确定性扣减。

## 六、Creation 频次控制

这组参数决定“creation 什么时候在 usage 中出现、一次显示多少”，不是缓存 read 的
控制器。已有缓存命中不会因为 creation 频控而变成 read=0。

### `creation_control.enabled`

- `false`：不做 creation 频控，适合大窗口压测。
- `true`：启用间隔、成功次数、事件上限和窗口预算。

当前实现会在 creation 控制压制报告时继续推进可见缓存前缀，避免“频控一开启，后续
永远没有 read”的状态卡死。如果要严格禁止真实增量写入，使用
`incremental_create_enabled=false` 或 `max_new_creation_tokens_per_request`。

### `min_creation_delta_tokens`

这是释放阈值，不是每次都必须达到的固定写入量。低于阈值的尝试会暂缓，后续请求
可累积后再释放。

它越大，creation 频次通常越低；它不会影响已有 read。

### `min_successful_requests_between`

两次 creation 之间至少要有多少次成功请求。数值越大，creation 越稀疏；read 仍可在
中间请求继续命中。

### `min_creation_interval_seconds`

两次 creation 之间的最小时间间隔。数值越大，短时间连续请求中 creation 越少。

### `max_creation_tokens_per_event`

单次 creation 事件的最大可见 token。触顶时会使用 creation control 自己的确定性
回退，避免每次都显示同一个整齐的上限。

它和 `final_cache_creation_max_tokens` 的区别是：

- 这个参数属于 creation 控制阶段，早于最终 usage guard；
- final cap 属于最后的字段守护；
- 两者都设置时，最终值取更严格的一侧。

### `creation_budget_window_seconds`

统计 creation 预算的时间窗口。窗口结束后预算重新计算。

### `max_creation_tokens_per_window`

窗口内 creation 的最大可见预算。预算用尽后，后续请求 creation 可能为 0，但已有
cache read 不会因此消失。

## 七、缓存生命周期和容量

### `default_ttl_seconds` / `hour_ttl_seconds`

控制断点 entry 的生命周期。TTL 到期后，tracker 中的前缀不再命中，需要重新 creation。

- `default_ttl_seconds`：普通断点；
- `hour_ttl_seconds`：明确标记为 1 小时的断点；
- 后端会限制在支持的最大 TTL 范围内。

它们不会修改 usage 上限，只改变一段时间后 read 是否还能命中。

### `max_entries_per_scope`

单个 cache scope 最多保留多少断点。超出后按最近使用情况淘汰，可能使长会话旧前缀
read 下降。

### `max_entries_global`

所有 scope 合计最多保留多少 entry。适合防止多分组、多会话把内存耗尽。

### `estimated_bytes_limit`

按 entry 估算的全局内存上限。达到后会淘汰旧 entry；不改变单条 usage 的算法。

### `expire_after_idle_seconds`

entry 连续空闲多久后删除。长时间没有请求的会话恢复时，第一次通常需要重新 creation。

## 八、可直接使用的模板

模板只负责载入一套起点配置，载入后仍可以在表单中修改。模板名称和说明已经显示在
管理页面“策略模板”下拉选项中，选项内会同时展示用途和读写目标。

### 1. 高缓存（默认）

适用：普通生产会话、希望稳定命中但不追求极限大数值。

主要特征：

```text
coverage_ratio = 0.98
usage_ratio = read_ratio = creation_ratio = 0.98
token_scale = 2
scale_min_input_tokens = 20000
max_simulated_input_tokens = 300000
max_new_creation_tokens_per_request = 100000
creation_control = 12k 增量 / 6s 间隔 / 2 次成功请求 / 600k 窗口
final_cache_read_max_tokens = 700000
final_cache_creation_max_tokens = 400000
```

预期：read 稳定，creation 受控，不会每轮都写入大块。

### 2. 稳步增长

适用：观察缓存逐轮累积，希望每轮最多写入约 100k，避免间隔和窗口预算造成停顿。

主要特征：

```text
coverage_ratio = 0.90
creation_control.max_creation_tokens_per_event = 100000
min_creation_delta_tokens = 0
min_successful_requests_between = 0
min_creation_interval_seconds = 0
max_creation_tokens_per_window = 0
final_cache_creation_max_tokens = 300000
```

预期：creation 按实际新增前缀平稳增长；不是固定每轮正好 100k。

### 3. 快速增长

适用：几轮内把缓存推到较大规模，用于高吞吐测试或长上下文预热。

主要特征：

```text
coverage_ratio = 0.98
creation_control.max_creation_tokens_per_event = 120000
max_creation_tokens_per_window = 2000000
min_creation_delta_tokens = 0
min_creation_interval_seconds = 0
final_cache_creation_max_tokens = 300000
```

预期：creation 频次高于默认模板，单轮增量不规整；读写仍取决于真实稳定前缀。

### 4. 大数值读写

适用：需要同时观察大 `cache_read` 和大 `cache_creation`，或做最终上限压力测试。

主要特征：

```text
coverage_ratio = 1
cache_current_user_stable_prefix = true
max_coverage_tokens = 3000000
max_new_creation_tokens_per_request = 0
token_scale = 2
max_simulated_input_tokens = 3000000
usage.cache_read = preserve
usage.cache_creation = raw
final_cache_read_max_tokens = 700000
final_cache_creation_max_tokens = 500000
creation_control.enabled = false
```

预期：

- read 可以进入约 `670k~690k` 的触顶区间；
- creation 可以进入约 `470k~490k` 的触顶区间；
- 触顶值因请求指纹确定性抖动，不应每条完全相同；
- 这是一套压测模板，不建议直接作为低吞吐生产默认。

### 5. 大读可控写

适用：希望 read 进入 700k 档，但不希望每轮 creation 都达到大上限。

主要特征：

```text
final_cache_read_max_tokens = 700000
usage.cache_creation = sample_target(target=120000, multiplier=1.25)
final_cache_creation_max_tokens = 180000
max_new_creation_tokens_per_request = 180000
creation_control:
  min_creation_delta_tokens = 20000
  min_successful_requests_between = 1
  max_creation_tokens_per_event = 180000
  max_creation_tokens_per_window = 600000
```

预期：read 可以很大；creation 通常围绕 120k 目标波动，且受 180k 和窗口预算双重
限制。这里同时限制了真实新增候选和最终 usage，适合需要可控写入的生产分组。

### 6. 大读小写

适用：高命中、低写入；希望 read 大，但 creation 保持在约 30k 档。

主要特征：

```text
final_cache_read_max_tokens = 700000
usage.cache_creation = sample_target(target=30000, multiplier=1.5)
final_cache_creation_max_tokens = 60000
max_new_creation_tokens_per_request = 60000
creation_control.max_creation_tokens_per_event = 60000
creation_control.max_creation_tokens_per_window = 300000
```

预期：creation 大多在目标附近变化，命中已有缓存时可以是 0；不能把 30k 理解成
自然缓存块大小，它是配置目标/上限组合的结果。

### 7. 大写可控读

适用：保留较大的写入能力，但限制下游看到的 read 数值。

主要特征：

```text
usage.cache_read = sample_max(max_tokens=250000)
final_cache_read_max_tokens = 250000
final_cache_creation_max_tokens = 500000
max_new_creation_tokens_per_request = 500000
creation_control.max_creation_tokens_per_event = 500000
```

预期：实际 tracker 仍可建立大前缀，但对外 read 不超过约 250k。这个模板的 read
限制是 usage 口径控制，不会删除已经建立的 tracker entry。

### 8. 自定义空白策略

适用：以默认配置为起点，自行调整缓存范围、比例、usage 模式和创建频控。

建议先明确要控制的是：

1. 实际缓存状态；
2. usage 对外数值；
3. creation 出现频次；
4. 还是三者的组合。

不要只修改最终上限来解决实际写入过大的问题；实际写入应同时看
`max_new_creation_tokens_per_request`、`incremental_create_enabled` 和内容范围。

## 九、常见配置误区

### 把最终上限当成固定目标

`final_cache_read_max_tokens=700000` 表示“最多显示到这个范围”，不是每轮自动生成
700k。真实稳定前缀只有 100k 时，read 不会被抬到 700k。

### 只改 usage 上限，没有增大请求负载

小请求不可能自然产生大 read/create。要观察 300k、500k、700k，需要有足够大的
稳定前缀、跨轮历史和合适的 `max_coverage_tokens` / `max_simulated_input_tokens`。

### 把 `sample_target` 当成真实写入限额

`usage.cache_creation=sample_target` 主要改变对外 usage 数值。要限制真实 tracker
新增，必须看 `max_new_creation_tokens_per_request` 和
`incremental_create_enabled`。

### 只用 `creation_control` 禁止真实写入

当前实现为了避免缓存状态卡死，会在 creation 报告被频控压制时继续提交可见前缀。
如果目标是严格禁止命中后的增量写入，使用 `incremental_create_enabled=false`。

### 把 `cache_creation=0` 当成缓存消失

它可能只表示本轮没有新的可写入前缀，已有缓存仍然可以继续产生 read。应同时查看
`cache_read`、TTL、scope 和实际请求前缀。

### `client_only` 没有客户端断点

客户端没有明确 `cache_control` 时，`client_only` 可能没有任何可缓存断点；测试结果
为 0 不代表系统缓存逻辑失效。

### 使用 `group_account_session` 却期待换账号继续命中

这个作用域会按账号隔离。要允许同一会话跨账号复用，使用 `group_session`。

### 抖动区间大于最终上限

后端会归一化小上限的抖动区间。建议抖动上限明显小于 cap，否则区间会被缩窄，
触顶值变化不明显。

## 十、页面模板说明

管理页面的“策略模板”下拉选项现在显示两行内容：

- 第一行：模板名称；
- 第二行：模板用途、读写能力和主要限制。

选择模板后，说明还会显示在下拉框下方，并且会写入策略的默认描述字段；保存前仍可
继续修改所有参数。

当前可选模板：

```text
自定义空白策略
高缓存（默认）
稳步增长
快速增长
大数值读写（700k 读 / 500k 写）
大读可控写（700k 读 / 120k 目标写）
大读小写（700k 读 / 约 30k 写）
大写可控读（500k 写 / 250k 读）
```

这些模板是可编辑起点，不是不可变预设。生产环境建议先用“高缓存（默认）”或
“大读可控写”，压测和边界验证再使用“大数值读写”。

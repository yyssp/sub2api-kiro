# 全策略全部配置参数验收

日期：2026-11-27。两协议 × 七策略的全部配置参数完成逐项验收。后端结构体递归展开 188 个叶字段，和共享默认值加模型覆盖字典完全对齐，共 2,632 个“协议—策略—参数”位置。覆盖门禁检查无遗漏，最终合并验收 0 未解决失败。

## 覆盖与证据

| 验证层 | 数量 | 验证内容 |
| --- | ---: | --- |
| 参数位置 | 2,632 | 每个字段写入、读取归一化值、JSON/SSE 冷热真实转发、usage/入库/费用 |
| 数值最大值位置 | 1,960 | 每个数值字段的合法最大值，含 4M token、1,000 倍、16GiB 字节上限等 |
| 非法值 trial | 4,592 | 枚举、类型、负数、越界，或明确兼容归一化 |
| API 契约 trial | 5,488 | 每字段缺省/null、跨字段矛盾、兼容 kind 别名、最小创建默认值 |
| 参数语义场景 | 1,084 | 根据参数用途构造可触发请求，断言输出或状态变化 |
| 本轮证据生成请求 | 18,975 | 含边界复验和修正请求的重复执行，不等于唯一场景数 |

本轮继续使用本地 mock API Key 账号，实际经过运行中网关的认证、调度、转发、缓存投影、usage 记录与计费；没有付费模型请求。每个通过的参数 trial 均记录实际请求 ID、usage 数值与费用；语义专项除通用费用核对外，增加具体参数行为断言。

持久证据位于 `.cachetest/cache-parameter-acceptance-20261127/`：

- `coverage.csv`：2,632 行逐字段清单，含策略、参数路径、测试值、非法值检查数、最大值覆盖、专项用例、请求 ID 和运行时作用说明。
- `acceptance.json`：覆盖门禁结果、报告路径、SHA-256 和原始失败数量。
- 原始 JSON 报告：逐字段矩阵、独立修正、最大值全绿、语义专项、TTL 修正、补充语义、契约全绿和最新集成报告。

原始报告不被改写。基础矩阵的 10 条失败来自禁用模式切到活动模式却未启用任何缓存来源，API 正确拒绝；修正配置的独立报告全部通过。语义报告有一条默认 TTL 用例携带显式 5m 标记而误判，移除标记后 1 秒默认 TTL 到期通过。汇总程序只用明确通过的复验替换对应项，并保留前述原报告供复核。最大值最终报告及契约最终报告自身均为 0 错误。

## 所有策略均执行的检查

七种策略为 `disabled`、`prefix`、`tool_aware`、`stable_segment_cache`、`prefix_tool_plus`、`bounded_high_cache`、`usage_multiplier`，两协议分别执行。每份配置全部 188 项经过写入及真实转发，未以隐藏字段为由跳过。

有状态策略另外构造来源独立的 system、工具 schema、历史、工具结果和当前用户请求，分别开关验证；模型门槛分别用全局高/低值、精确覆盖和显式 0 验证；session 缺失时分别允许/禁止派生。

采样检查四种 mode、上限、目标、常态倍率与输入差额转读；输出护栏、输出放大、最终各桶 cap/jitter、总输入/输出限制、原始 usage 保留、跳过非流、严格流式/非流式资格均有独立请求。数字边界还覆盖请求输出上限与模型输入/输出上限。

生命周期检查默认 TTL、1h TTL、state TTL、idle expiry、单 scope/global 条目限制、字节限制、LRU、策略 revision 失效、两种账号 scope。创建控制检查最小增量、成功间隔、时间间隔、事件上限、窗口上限和窗口重置；增量创建、覆盖比例、read/creation 比例、覆盖上限、单次写集合上限、冷保护、输入缩放、触顶 jitter 和未缓存输入区间分别验证。

stable 专项检查最小总量、最小片段、历史条数、显式标记和三个自动来源开关、上报 input 区间。Plus 专项遍历默认、非流 custom、小请求 custom 三套 profile：各桶 multiplier/jitter/min/max/max-jitter、溢出转 input、成本下限，以及 inherit/custom/raw、小请求资格、禁缓存、抑制写入去向。直接倍率各桶阈值、零/缩小/放大/极端倍率由独立期望值核对，且验证旧字段不叠加。

## 本轮新增检出并修复

| 参数 | 复现 | 修复与实测 |
| --- | --- | --- |
| `dynamic_content_mode` | 参数有保存与界面，但运行时不读取；明确动态结果仍进入稳定缓存 | 默认排除已识别的动态工具/搜索结果或明确动态标记，从首个动态块截断缓存前缀；allow 可缓存。两协议五种有状态策略分别通过 |
| `model_min_cacheable_overrides` | Anthropic 设置模型阈值 1M 仍创建缓存；两协议显式 0 不能覆盖全局高阈值 | 统一模型门槛解析，支持精确/模型族、大小写与 `models/` 归一化、最长匹配和显式 0。真实网关与回归通过 |
| `cache_tool_results=false` | Anthropic 的工具结果嵌在 message block，旧过滤只检查外层 kind | 识别嵌套 `tool_result`/function result；五种有状态策略关闭时不产生该来源缓存 |
| Gemini Plus 的旧输出放大参数 | Plus 输出 profile 之外又叠加旧 output uplift，与独立 profile 的定义不符 | Plus 仅使用自己的输出 profile，不叠加旧放大。设置 profile ×2、旧 uplift +50% 后输出为 2,400，无二次放大 |
| stable 自动断点 `default_ttl_seconds` | 自动断点固定 300 秒，无视默认 TTL 配置 | 自动断点读取设置；显式客户端标记保持优先。无显式标记的 1 秒到期与显式标记优先均通过 |

动态排除根据明确的块类型、已知动态工具名或 `volatile=true`、`dynamic=true`、`cacheable=false` 标记执行，不猜测普通文本中哪些字符是时间或随机值。工具 schema 与参数中的同名业务字段保持不透明，避免误删。

## 参数适用性

“测试过”不等于“所有字段在每种策略都生效”。所有位置都经过写入和实际请求，CSV 明确写出当前作用：

- `disabled` 归一化为禁用配置，原始 usage 保留。
- `usage_multiplier` 只应用自身阈值/倍率、usage 开关、协议和最终 limits，不应用旧采样、命中、session、创建频控等。
- `tool_plus.*` 和 `stable_segment.*` 在各自策略执行，其余策略保留兼容值而不应用该算法。
- Gemini 生成无缓存写，creation 控制、写采样、写比例和 TTL 上报档归一化为不写。hour TTL、上游 TTL 档、Anthropic 成本下限、传统断点与总输入 jitter 等兼容字段不用于 Gemini 三桶算法；界面未把这些项当作 Gemini 可用能力。
- `move_delta_to_cache_read` 仅 input 桶执行；其他桶共享结构中的同名字段保留兼容语义。

该区分由完整写入/归一化矩阵和相关专项验证支持，没有把不适用字段宣称为“功能生效”。

## 工具与复跑

```bash
# 所有策略、所有参数：合法变体和非法值，真实 JSON/SSE 转发与入库
python3 backend/scripts/cache-parameter-e2e.py \
  --report /tmp/cache-parameter-all-final.json

# 每个数值字段的合法最大值
python3 backend/scripts/cache-parameter-e2e.py \
  --upper-only --skip-invalid --report /tmp/cache-parameter-upper-clean.json

# 参数行为专项
python3 backend/scripts/cache-parameter-semantics.py \
  --report /tmp/cache-parameter-semantics.json

# 缺省/null、别名和跨字段契约
python3 backend/scripts/cache-parameter-contracts.py \
  --report /tmp/cache-parameter-contracts-clean.json
```

脚本默认 mock 端口 48791。并行复跑时需分别指定 `--mock-port`，独立 run 账号与策略互不复用。报告不记录 API Key、登录 token 或数据库凭据。覆盖聚合脚本 [`cache-parameter-coverage.py`](../backend/scripts/cache-parameter-coverage.py) 针对本轮原报告与修正报告验证完整性并归档，保留出处。

后端定向回归 1,357 项通过，新增参数专项继续通过。运行中的二进制为 2026-11-27 00:56:31 构建。测试前缀的可见账号、分组、策略、用户全部清理为 0；API Key 随 run 清理，usage 历史保留。前后端仍为 48780/48788。未提交或推送。

这里的“全部参数”指穷尽配置字段、枚举选项、开关、有效边界与非法边界，并覆盖适用的语义和约束组合；不声称穷举任意实数或全部参数的无限笛卡尔积。API Key mock、跨实例预算及非 API Key 上游认证的边界沿用[真实网关验收](cache-gateway-e2e.md)。

# 缓存策略真实网关验收

2026-11-26。两协议全部七种策略通过本地 mock 账号的实际 HTTP 链路验证。最终六份全绿报告共 955 次网关 HTTP 调用，其中 898 次成功生成请求逐条完成响应 usage 与数据库记录核对；包含 50 次强化逐桶价格断言后的独立复验。错误断言 0，清理失败 0。

## 验证方式

脚本为 [`backend/scripts/cache-gateway-e2e.py`](../backend/scripts/cache-gateway-e2e.py)。经管理员 API 创建专属用户、策略、分组、API Key 账号，再用普通用户 API Key 调用真实网关。账号只指向本地 `127.0.0.1` mock，无付费模型调用。网关认证、调度、协议转换、缓存投影、usage 写入和余额扣减均执行产品代码。

每个成功生成请求核对输入、输出、缓存读、缓存写，以及 Anthropic 5m/1h 明细；核对账号、分组、策略快照、流式标识、费用分项合计与实际费率。价格按当前测试环境的模型单价独立计算，专项复验进一步逐桶检查费用。Gemini 核对 `prompt=input+read`、`output=candidates+thoughts` 和 `total`。

测试账号、密钥、策略、分组和用户均使用 run 前缀，结束后按创建 ID 清理；最终管理员 API 查询该前缀的可见账号、分组、策略、用户均为 0。保留 usage 历史作为证据。报告不保存 API Key 或登录 token。

## 最终证据

| 报告 | 成功生成请求 | 网关 HTTP 调用 | 结果 |
| --- | ---: | ---: | --- |
| `/tmp/cache-gateway-e2e-core-final.json` | 596 | 606 | 151 场景，响应/入库一致 |
| `/tmp/cache-gateway-e2e-lifecycle-final.json` | 68 | 84 | 禁用、400、503、并发 |
| `/tmp/cache-gateway-e2e-scope.json` | 60 | 77 | 20 组账号 scope 与 revision |
| `/tmp/cache-gateway-e2e-explicit-balance.json` | 18 | 32 | 显式缓存、创建/存储价格、余额 |
| `/tmp/cache-gateway-e2e-edges-final.json` | 106 | 106 | Chat/Responses、所有桶的阈值、模型与请求限制 |
| `/tmp/cache-gateway-e2e-edges-cost-final.json` | 50 | 50 | 阈值与极端值的逐桶费用强化复验 |
| **合计** | **898** | **955** | **0 失败，0 清理错误** |

汇总及原报告 SHA-256：`/tmp/cache-gateway-e2e-acceptance.json`。967 次 mock POST 包含生成、错误尝试、503 重试和缓存创建；网关 HTTP 数还包括显式资源删除和账号快照更新期间的预期 503。成功生成次数与 HTTP 总数不是同一个指标。

本地一页报告：`.archify/explainer-cache-gateway-20261126-acceptance/cache-gateway-report.html`；同目录保留原报告副本与汇总。该页面由 Archify 生成，自动校验通过，未进行浏览器视觉验收。

前期探索报告中出现过脚本缺陷，最终统计只采用上表六份无错误报告。既有后端定向回归 1,337 项、前端 45 项、类型检查、ESLint、前后端构建通过，作为补充证据，不代替此处 HTTP 验收。

## 全部配置参数验收追加

2026-11-27 已完成188叶字段×两协议×七策略，共2,632个位置。另有1,960个数值最大值位置、4,592个非法值trial、5,488个缺省/null/跨字段契约trial、1,084个语义场景。包括原始报告和独立修正报告的覆盖门禁为0未解决失败；完整证据、参数适用性和本轮5项新增修复见[全部参数审计](cache-parameter-audit.md)。

## 每种策略覆盖范围

以下核心次数均来自独立全绿核心报告。其他列为额外执行的场景。

| 策略 | Anthropic 核心请求 | Gemini 核心请求 | 额外覆盖 |
| --- | ---: | ---: | --- |
| `disabled` | 10 | 10 | 禁用 API、400 恢复、兼容路由、显式资源 |
| `prefix` | 52 | 42 | 两种 scope、版本失效、503 重试 |
| `tool_aware` | 52 | 42 | 两种 scope、版本失效、兼容路由、显式资源 |
| `stable_segment_cache` | 52 | 42 | 两种 scope、版本失效、稳定输入区间 |
| `prefix_tool_plus` | 56 | 46 | 两种 scope、版本失效、raw、小请求禁缓存 |
| `bounded_high_cache` | 52 | 42 | 两种 scope、版本失效、最终上限 |
| `usage_multiplier` | 47 | 51 | 各桶阈值、请求与模型上限、并发、余额 |

核心矩阵包含 JSON/SSE、冷/热三轮、真实缓存证据、usage 开关、无会话、跳过非流投影、缓存比例清零、最终读/输出上限与 jitter、Anthropic 严格资格、创建间隔与窗口限制、命中率配置、未完成流后重试、1 秒 TTL 到期。倍率覆盖 0、0.5、1.5、2、1,000，以及多帧部分 usage。

每个策略均补测 Anthropic Chat/Responses 兼容 JSON/SSE、Gemini 显式缓存创建→引用生成→删除、启停 API、HTTP 400 后恢复。五种有状态策略分别验证 `group_session`、`group_account_session`：停用首账号后真实调度到第二账号，前者保留复用，后者冷启动；更新策略 revision 后旧状态均失效。

## 可复核参数与数值

### 直接倍率与实际扣费

原输入 2,000、读 4,000、输出 1,200；Anthropic 另有写 1,000，其中 5m=600、1h=400。配置输入 ×0.5、读 ×2、写 ×0.5，输出严格大于 1,000 时 ×1.5。

| 协议 | 未缓存输入 | 读 | 写（5m/1h） | 输出 | 单次费用 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Anthropic | 1,000 | 8,000 | 500（300/200） | 1,800 | $0.034725 |
| Gemini | 1,000 | 8,000 | 0 | 1,800 | $0.02025 |

两协议各执行 JSON/SSE 一次：`2×0.034725 + 2×0.02025 = $0.10995`，数据库实际费用合计与用户余额减少完全一致。

Anthropic 四桶和 Gemini 三桶分别测试原值 999、1,000、1,001、阈值 1,000、倍率 0.5，结果为 999、1,000、501，JSON/SSE 均相同。原缓存桶为 0 时倍率不制造缓存。Gemini 生成写始终为 0。

### 最终硬边界

- 请求输出上限 77、策略输出上限 100、输出倍率 1,000：两协议 JSON/SSE 均为 77。
- 策略输入上限 5,000、输出上限 100：百万级原始 usage 和 1,000 倍放大后仍受约束。
- 显式 `limits=0`、请求输出上限 1,000,000、原输入 1,500,000/读 300,000/输出 200,000：Anthropic 输入三桶合计 200,000、输出 64,000；Gemini 输入两桶合计 1,048,576、输出 65,535。输入桶按比例收紧，TTL 明细与写总量一致。
- 模型目录这两个条目只有 `max_input_tokens`/`max_output_tokens`，未声明 `context_window`；因此本次分别验收输入/输出上限，不能把输入加输出的共同上限当作已验证结论。有 `context_window` 的条目另受共同上限约束。

### 生命周期与并发

- 400 无 usage 记录，恢复后的同会话无提前缓存读。
- 一次 503 后恢复：每协议实际访问 mock 2 次，客户端成功且只产生 1 行费用。
- 持续 503：当前环境 Anthropic 16 次、Gemini 20 次上游尝试后失败，均无计费 usage。次数取决于既有重试配置，脚本不修改产品重试参数。
- 12 个独立会话、6 个 worker、混合 JSON/SSE：每协议 12 行 usage，输出都为 1,800，账号、流标识和费用逐条正确。
- Gemini 七种策略真实创建显式缓存，2,000 token、300 秒：输入费 $0.0025，存储费 `2000×300/3600×4.5e-6=$0.00075`，合计 $0.00325。生成时保留真实读；倍率模式配置 ×0.5 时读由 2,000 变为 1,000。

## 检出并修复的产品问题

| 条件 | 问题 | 修复结果 |
| --- | --- | --- |
| Anthropic 无 session 且关闭派生会话，流式开启倍率 | 响应 output=1,200，入库=1,800 | 统一准备投影 plan，JSON/SSE 与记账一致 |
| 两协议 stable 模式缓存比例均为 0 | 稳定输入区间重分配仍造出缓存读或写 | 仅有实际缓存用量时应用稳定区间，清零配置保持零 |
| Anthropic 流式缓存写清零 | 旧 5m/1h 嵌套值残留 | 嵌套 TTL 同步清零，费用不残留 |
| 倍率放大后触发最终输入预算 | TTL 写明细比例丢失 | 按最终写总量比例收紧 5m/1h |
| OpenAI usage 倍率改变缓存写与图片桶 | 总量、TTL 家族、图片子桶不同步 | 保留原 TTL 家族并同步子桶 |
| 倍率编辑器留有旧 jitter 参数 | 隐藏的旧字段拦截保存 | 独立倍率模式跳过无关校验 |

前四项由实际网关与定向回归共同验证。OpenAI TTL 家族及图片子桶由专项 mock/性质回归验证；本次 HTTP 兼容路由另验证了 Chat/Responses 的正常读写与费用，不扩大为所有上游认证或多模态 HTTP 的验收声明。

脚本自身也修正了三类误报：Chat 缓存写位于嵌套 details；并发按 `X-Client-Request-ID` 对齐数据库 `client:` ID；mock 使用 128 监听队列，避免默认小队列造成连接关闭。伪造 Gemini 缓存资源返回 403 为正确权限行为，正式验收改为同用户真实创建资源。

## 复跑

需要本地后端、隔离数据库/Redis 以及开发管理员账号。可用 `CACHE_E2E_ADMIN_EMAIL`、`CACHE_E2E_ADMIN_PASSWORD` 覆盖管理员凭据。默认不访问真实上游；报告仅含测试资源标识和 usage。

```bash
# 全部核心与扩展，结束后自动清理本轮资源
python3 backend/scripts/cache-gateway-e2e.py \
  --full --extended --report /tmp/cache-gateway-e2e.json

# 独立核心报告
python3 backend/scripts/cache-gateway-e2e.py \
  --full --extended --phases core --skip-scope \
  --report /tmp/cache-gateway-e2e-core.json

# 可选择局部复验
python3 backend/scripts/cache-gateway-e2e.py \
  --extra-only --phases lifecycle,retry,concurrent,balance,edges \
  --skip-scope --report /tmp/cache-gateway-e2e-extra.json
```

## 验收边界

本次覆盖 API Key mock Anthropic 与 Gemini，包含本项目 Claude Code 入口及 OpenAI 兼容路由。未执行 AWS Kiro 私有 EventStream、OAuth/cloud-assist、真实供应商计费或多实例共享状态验收。模型能力取二进制嵌入目录快照，价格取本地测试环境现有目录与配置。当前环境的长上下文价格策略没有因上述大输入自动切换单价；此处验证的是现有配置下的逐桶费用与扣费一致性。

创建窗口是本地成功记录预算，未原子预留同时在途请求或跨实例预算。最终 usage 边界限制上报与计费用量，不能令真实超上下文请求被模型接受。上述范围内未发现剩余可复现的 usage、费用或参数约束问题。

后端仍运行 `backend/tmp/server`（2026-11-26 17:54:04 构建）；前端 `http://127.0.0.1:48780`、后端 `http://127.0.0.1:48788`。未提交或推送代码。

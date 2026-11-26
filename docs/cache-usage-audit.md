# 缓存 usage 与参数边界审计

2026-11-27 全部配置字段追加验收完成：2,632个策略参数位置、1,084个语义场景，完整字段清单及新增修复见[全部参数审计](cache-parameter-audit.md)。


2026-11-26 真实账号调度与计费链路已完成：955 次 HTTP 调用、898 次成功生成请求逐条核对，最终 0 失败。完整矩阵、修复和复跑入口见 [真实网关验收](cache-gateway-e2e.md)。


日期：2026-11-25。参考：`../2ue_kiro.rs` 的 `claude-main`，提交 `6ed4599d`。审计范围为本项目通用缓存策略、Anthropic/OpenAI 兼容响应和原生 Gemini 响应。未调用付费模型上游；原生网关使用 mock HTTP transport。

## 结果

已修复最终上限被后续整形突破、停用整形后高级步骤仍改写、Gemini 参数执行不完整、前后端默认值漂移等问题。定向回归共 118 个顶层测试、949 个子用例，1,067 项全部通过；前端 31 项、类型检查、ESLint、生产构建通过。

输入预算统一计算为 `未缓存 input + cache read + cache creation`。OpenAI 的 input 本身就是这三项总和。Gemini 的 generation usage 没有 creation，始终上报 0；显式 cachedContents 生命周期的存储计费仍独立。

## 修复

1. 所有投影结束后执行统一约束，取模型输入/上下文、策略总输入上限、原有 reported/max simulated 上限的最小正值。输出同时受模型、策略、请求 max tokens 和已启用字段上限约束。0 表示自动模型边界，不表示无限。
2. 输入三桶超过预算时按比例缩减，并分配整数余数；避免先保留未缓存输入导致热请求的缓存读被完全挤掉。硬上限优先于下限、命中率与成本目标，触顶时这些目标可以无法同时满足。
3. Plus 成本补足不能突破字段上限；sample_max、sample_target 和创建单次上限在最后仍有效。bounded 的成本差额依据最终受限 usage 计算。
4. 创建频控决定的本次 allowance 同时约束命中率和成本补偿步骤，避免补偿再次制造超过窗口余额的 creation。连续 6 次成功请求验证单次 1,000、窗口 1,600 的串行约束。
5. 整形关闭、非流跳过、严格资格不满足和 Plus raw 分支保持透传。Plus 小请求 inherit 直接继承主配置；disable-cache 会把读写退回 input。
6. Gemini 未缓存输入回退使用完整 min/max 区间；稳定片段自动 system/tools/history 标志实际生效；当前用户前缀受累计 token 上限约束；snake_case 图片/文件同样不按 JSON 长度虚构文本缓存。
7. 数值入库前限制极大整数、非有限倍率和不合理区间，避免 duration、抖动跨度和 token 计算溢出。token 配置最大 4,000,000；内存预算最大 16GiB。
8. 新策略默认启用保守的派生会话；prefix 默认最小可缓存 1,024、模拟上限 300,000。稳定片段和 Plus 采用各自默认值。没有模板也可直接保存。
9. 后端生成 `frontend/src/views/admin/cacheStrategyDefaults.json`，覆盖两协议与六种模式；后端快照测试检查一致性，前端直接深拷贝使用。空 TTL 与采信上游值显式提供。新增 `/api/v1/admin/cache-strategies/defaults`，最小创建请求与共享默认值逐项一致。
10. 折叠资源、创建频控和通用字段高级项；Gemini 按采样模式展示实际相关字段，并复用 Plus 面板。顶层 preserve-upstream 为权威字段，嵌套同名字段继续兼容并由后端同步。协议切换不重置人工会话和资格设置。

## 可复核数值

关闭策略级模拟上限，输入 mock 1,500,000、输出 100，保持已有热缓存证据：

| 模型目录键 | 最终未缓存输入 | 缓存读 | 缓存写 | 输入合计 |
| --- | ---: | ---: | ---: | ---: |
| claude-sonnet-4-5 | 14 | 199,986 | 0 | 200,000 |
| claude-sonnet-4-6 | 67 | 999,933 | 0 | 1,000,000 |
| gemini-2.5-pro | 157,346 | 891,230 | 0 | 1,048,576 |

因此 1M 不能一律判错。200k 模型不得合成 1M；具有 1M 能力的模型在更低策略限制未启用时允许对应范围。默认 prefix 的 300k 模拟限制可以进一步收紧大窗口模型。

主极端矩阵为 3 种口径 × 5 种模式 × 4 组模型能力 × 2 种流式状态 × 5 组输入（0 / 1 / 10,000 / 1,500,000 / 4,000,000），共 600 个组合；包含 1,000 倍字段倍率、100 倍 Plus 成本目标、输出请求上限 777。额外验证最终单桶优先级、无断点仍受限、raw/skip、非法极大参数和共享默认值。

Gemini 原生 JSON/SSE mock 对所有五种模式分别执行冷/热两轮，输入上限 25,000、策略输出上限 100、请求上限 80；断言 response usageMetadata 与 ForwardResult 一致，回答+思考=最终输出，total=prompt+回答+思考+toolUsePrompt，生成 creation=0。已有错误、缺终止、取消、TTL、隔离与显式资源回归同时运行。

## 验证证据

- 后端最终结果：`/tmp/sub2api-cache-audit-final.jsonl`，1,067 pass，0 fail。
- 前端：`/tmp/sub2api-cache-audit-frontend-final.log`，31 pass。
- 类型检查：`/tmp/sub2api-cache-audit-typecheck.log`；ESLint：`/tmp/sub2api-cache-audit-eslint.log`。
- Vite：`/tmp/sub2api-cache-audit-vite.log`；后端：`/tmp/sub2api-cache-audit-build-final.log`。
- 运行 API：两协议 × 六模式的 defaults GET 与最小创建结果，逐项等于共享 JSON；创建/编辑/复制/开关/空绑定/非法协议通过，临时策略已清理。
- 浏览器：48780 缓存策略页，无模板默认值与 Gemini 切换正常，最终边界输入均为 0 自动，无未翻译键，折叠项默认收起。

复验命令：

```bash
GOMAXPROCS=2 GOGC=50 go -C backend test -p=1 \
  -gcflags='github.com/Wei-Shaw/sub2api/internal/service=-l' \
  -tags unit ./internal/service ./internal/handler/admin \
  -run 'TestCache|TestGeminiForwardNative|TestExtractGeminiUsage|TestBoundCache|TestAdvanced|TestKiroGroupCache|TestReportedInput|TestSkipNonStream|TestPreserveUpstreamCacheUsage' -count=1
pnpm -C frontend exec vitest run \
  src/views/admin/__tests__/cacheStrategyTemplates.spec.ts \
  src/views/admin/__tests__/cacheStrategyProtocols.spec.ts \
  src/i18n/__tests__/localeKeyCompleteness.spec.ts --maxWorkers=1 --minWorkers=1
```

修改后端默认工厂后，使用 `UPDATE_CACHE_DEFAULTS=1` 运行 `TestCacheDefaultsSharedSnapshot` 更新共享 JSON，再正常运行测试确认无漂移。

## 使用边界

- 约束针对最终上报/计费用量，不能让实际超上下文请求变成可被模型处理的请求；上游请求长度校验仍由网关与模型服务负责。
- 模型能力读取随二进制嵌入的项目目录快照，未在本次联网核对所有模型的官方最新能力。未知模型保守回退为 200k 总上下文、64k 输出；策略可配置更低上限。
- 显式 Gemini 缓存保留真实读取，不应用模拟读的单桶采样，但启用投影时仍服从模型和总输入预算。跳过投影/raw 会保留上游异常值。
- 派生会话按 system/tools/首轮内容建立身份；明确 session_id 能提供更严格的隔离。
- 稳定分段为进程内状态；创建窗口按本地成功记录进行整形，在多个实例或同时在途请求间没有原子配额预留。该设置适用于用量曲线整形，不承诺全局并发计费额度。严格跨实例配额需要独立的共享原子预算与失败释放生命周期，未混入本次缓存算法。


## 第二轮交互与状态审计（2026-11-26）

追加验证通过：后端 145 个顶层测试及 977 个子用例，共 1,122 项；前端 38 项（新增真实挂载编辑器的交互测试与草稿恢复测试）；vue-tsc、ESLint、Vite 构建通过。

### 新检出并修复

| 触发条件 | 原行为 | 修复与回归 |
| --- | --- | --- |
| Claude 流同时包含带 stop_reason 的 message_delta 和 message_stop | 输出 80 配 2 倍，终帧再次投影成 320 | 四条 Chat/Responses 兼容路径保留 rawUsage，始终从原值投影；最终响应和记账均为 160 |
| Gemini 末帧仅携带 candidates/thoughts，省略 prompt | 最终输入被覆盖为 0 | 请求内累计原始 usageMetadata，再逐帧投影；4,000 输入、20 回答+10 思考、2 倍输出得到 input=4,000/output=60 |
| Anthropic → Gemini → Anthropic，或 prefix → disabled → prefix | 原协议写入/TTL 参数丢失；模式切换重置人工限制 | 按协议和模式暂存当前编辑草稿，往返恢复；硬输入/输出限制跨模式保留，模板显式应用才重新载入 |
| 旧策略扩展对象存在但只有部分嵌套字段 | 单层补默认仍留下 undefined | 递归补齐缺失字段，保留显式 0 和 false |
| 创建或更新成功、随后绑定失败 | 重试重复创建，或使用过期 revision 更新而冲突 | 保存返回的 ID/revision 立即进入编辑态；绑定统一幂等 replace；失败重试使用已保存身份，并明确提示“内容已保存、绑定未完成” |
| 已有绑定读取失败、或关闭后旧读取结果迟到 | 空列表误清绑定，或旧绑定污染新草稿 | 读取成功前禁止保存；编辑会话版本屏蔽迟到结果；保存时禁止关闭和重复提交 |
| Gemini Plus 小请求 disable_cache | 返回值无 read，但成功后仍写入 tracker | 准备阶段停止缓存断点/状态提交，后续取消覆盖也不会发现隐藏写入 |
| 关闭 Gemini system/tools/tool_result 缓存，历史包含混合 text 与 functionResponse | 被排除块仍计入后续缓存 token；关闭工具结果连同文本一起排除 | 被排除块保留连续指纹依赖但缓存 token 为 0，工具模式按 part 分别判断 |
| OpenAI 兼容 stable_segment，无 cold guard | 进入普通快捷分支，stable 的未缓存输入区间无效 | stable 统一进入对应算法；Gemini 同时补齐 stable 输入下限方向 |
| 请求匹配缓存后失败 | prepare 已刷新条目 TTL | 仅成功 commit 续期；取消的 Claude 请求也不再提交 |
| 长期产生成功会话 | controls 只清理从未成功的状态，长期占用内存 | 按 idle 时间回收，并保留仍受长窗口/间隔约束的状态 |
| 只缓存 system/tools，关闭 history/current user | 合法配置被一律拒绝 | 仅当四种内容来源全部关闭时拒绝 |
| Gemini 图像输出带 candidatesTokensDetails | 文本输出整形导致总量与 IMAGE 子桶不符 | 带图像输出的响应保留原始模态计费数据；文本倍率不作用于图像 token |
| 显式缓存上游返回极大整数 | 跳过模拟单桶 cap 后，加法/乘法可能溢出 | 比例运算前收紧绝对安全边界，模型预算仍有效 |

### 本轮证据

- `/tmp/sub2api-cache-interaction-full3.jsonl`：1,122 pass，0 fail。
- `/tmp/sub2api-cache-interaction-front-final2.log`：38 pass。
- `/tmp/sub2api-cache-interaction-typecheck-final2.log`、`/tmp/sub2api-cache-interaction-eslint-final.log`、`/tmp/sub2api-cache-interaction-vite-final.log`。
- 新回归入口：`cache_interaction_audit_test.go`、`CacheStrategiesView.interactions.spec.ts`、`cacheStrategyEditor.spec.ts`。
- 并发 mock：12 个未提交冷请求均无缓存读；各自成功提交且重复 commit 幂等后，下一轮能命中。该验证不改变上一节所述跨实例/在途窗口预算边界。

### 仍需区分的行为

策略内容与分组绑定仍是两个 API 事务。绑定失败时策略内容可能已经保存；编辑器现在可恢复重试，但没有伪装成数据库原子事务。客户端已收到的流式分片也无法因之后的上游错误撤回；失败/取消不提交缓存状态，已产生的用量仍按既有网关规则记录。Gemini 多模态采用原始计费值，不套文本整形。


## 直接倍率模式验收（2026-11-26）

新增 `usage_multiplier`，两协议均可选。阈值采用严格大于，触发后整桶乘倍率并四舍五入；默认阈值0、倍率1。不存在会话、冷热、命中资格、频控或本地状态写入。旧采样/成本/比例设置不叠加。缓存读写原值为0仍保持0，Gemini生成没有写桶。最终遵守模型/请求/显式 limits。

后端回归 `/tmp/sub2api-usage-multiplier-go-final.jsonl`：154 个顶层测试、983 子用例，合计 1,137 pass。新增阈值下方/等于/上方、0/.5/1/1.5/2/1000倍率、TTL分桶、跨协议、无会话、旧直接prepare入口、极端限制、四条流式终帧不重复放大，以及原生 Gemini JSON/SSE 响应与记账一致性验证。

原生 Gemini mock：prompt=6,000/cache read=4,000，未缓存输入2,000；配置input×0.5/read×2/output阈值1,000且×1.5，原输出回答1,000+思考200，结果未缓存input=1,000/read=8,000/output=1,800（回答1,500+思考300）、prompt=9,000、total=10,800，creation=0，无tracker写入。

前端 44 项测试通过。前端验收覆盖默认四桶/三桶、输入变更、实时示例、协议/模式往返，以及创建实际 payload。证据：`/tmp/sub2api-usage-multiplier-front-final.log`、`/tmp/sub2api-usage-multiplier-typecheck.log`、`/tmp/sub2api-usage-multiplier-eslint.log`、`/tmp/sub2api-usage-multiplier-vite.log`、`/tmp/sub2api-usage-multiplier-build.log`。接口与行为详见 [策略说明](cache-shaping-parity.md)。

运行验收：两协议创建/读取/编辑倍率0/复制/空绑定/非法倍率拒绝通过；14组默认值逐项等于共享数据，临时策略清理。浏览器Anthropic显示8个阈值/倍率输入，Gemini显示6个且无缓存写控制，无旧时机设置、无翻译缺失。

# F04 · 额度耗尽的调度行为（对应你的约束 4）

> 你的原话：**"如果遇到账号调度报错，需要保证调度到的是额度正常的账号"**。
> 这批 235 个账号大部分额度已耗尽，所以这是本轮最先要确认的一条链路。
> 本文件是**开工前的源码核查**结论，全部为 `[本仓库]` 级证据。

---

## 0. 结论先行

**好消息：这条链路已经实现了，而且实现得比我预期的完整。**
我原本担心"额度耗尽会卡死在坏账号上"，源码核查表明**不会**。

但有一个**真实的残余风险**（见 §3），需要靠真实账号才能排除。

---

## 1. 额度耗尽的完整处理链

Kiro 用 **HTTP 402 + `MONTHLY_REQUEST_COUNT`** 表示月度额度耗尽。

### 1.1 识别

`[本仓库]` `backend/internal/service/kiro_error_classifier.go:169-176`

```go
if strings.Contains(trimmed, "MONTHLY_REQUEST_COUNT") { ... }
return gjson.Get(trimmed, "reason").String() == "MONTHLY_REQUEST_COUNT" ||
    gjson.Get(trimmed, "error.reason").String() == "MONTHLY_REQUEST_COUNT"
```

既做子串匹配又做结构化字段匹配，**双保险**——上游换 JSON 包装也不会漏判。

### 1.2 惩罚：冷却到下月 1 号

`[本仓库]` `backend/internal/service/kiro_runtime.go:786-812`

```go
func (s *GatewayService) markKiroMonthlyRequestCountRateLimited(ctx, account, body) {
    resetAt := nextKiroMonthlyResetUTC(time.Now())
    if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil { ... }
    reason := "kiro monthly request count exhausted (402): MONTHLY_REQUEST_COUNT"
    ...
}

func nextKiroMonthlyResetUTC(now time.Time) time.Time {
    utc := now.UTC()
    year, month, _ := utc.Date()
    return time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)  // 下月 1 号 UTC 00:00
}
```

**这是关键设计**：不是固定退避几分钟，而是**直接冷却到月度重置时刻**。
意味着一个耗尽的账号在本月内**不会被再次调度**，不会反复浪费请求。

> 对本轮测试的直接影响：**耗尽账号只会被"踩中"一次**，之后自动从调度池移除。
> 所以即便 235 个里大部分耗尽，只要有少量可用，跑几次之后调度就会自然收敛到可用账号上。

### 1.3 故障转移

两个调用点都在打完标记后返回 `UpstreamFailoverError`，触发换账号：

`[本仓库]` `kiro_runtime.go:507-523`（请求发送阶段）
```go
if resp.StatusCode == http.StatusPaymentRequired {
    classification := classifyKiroHTTPError(resp.StatusCode, string(respBody))
    if classification.Category == kiroErrorMonthlyRequest {
        s.markKiroMonthlyRequestCountRateLimited(ctx, account, string(respBody))
    }
    return nil, requestCtx, &UpstreamFailoverError{ StatusCode: resp.StatusCode, ... }
}
```

`[本仓库]` `kiro_runtime.go:886-905`（响应处理阶段 `handleKiroHTTPError`）
```go
if classification.Category == kiroErrorMonthlyRequest {
    s.markKiroMonthlyRequestCountRateLimited(ctx, account, string(respBody))
}
...
if resp.StatusCode == http.StatusPaymentRequired || s.shouldFailoverUpstreamError(resp.StatusCode) {
```

注意 `402` 是**显式并进**故障转移条件的，因为它不在通用列表里：

`[本仓库]` `gateway_forward.go:46-53`
```go
func (s *GatewayService) shouldFailoverUpstreamError(statusCode int) bool {
    switch statusCode {
    case 401, 403, 429, 529:
        return true
    default:
        return statusCode >= 500
    }
}
```

→ `402` 靠 `resp.StatusCode == http.StatusPaymentRequired ||` 这半句补上。
**Kiro 路径上额度耗尽能正确转移。**

---

## 2. 顺带确认：其它已覆盖的 Kiro 专属错误

源码核查时发现 Kiro 路径做的特殊处理比通用路径多得多：

| 场景 | 处理 | 位置 |
|---|---|---|
| 402 额度耗尽 | 冷却到下月 1 号 + 故障转移 | `kiro_runtime.go:507,886` |
| 403 账号封禁 | `markKiroSuspended` | `kiro_runtime.go:529-536` |
| 401 / token 失效 | 刷新 token 后重试 | `kiro_runtime.go:539+` |
| 400 invalid model | `markKiroInvalidModelRateLimited` + 故障转移 | `kiro_runtime.go:890-899` |
| 端点全部失败 | `kiro upstream endpoints exhausted` | `kiro_runtime.go:594` |

**这修正了我之前对 G1 的判断。** 我原先基于通用的
`shouldFailoverUpstreamError` 不含 400，判定"400 一律不转移"。
但 Kiro 路径**单独**给 `kiroErrorBadRequestInvalidModel` 加了转移分支
（`kiro_runtime.go:890-899`）。

→ **G1 的范围要收窄**：不是"所有 400 都不转移"，而是
**"未被 `classifyKiroHTTPError` 归类的 400 才不转移"**。
G5（工具名非法导致的 400）正好落在这个未归类分支里——**这才是 G1 真正的缺口**。

---

## 3. 残余风险：`[待验证]`

源码能证明"402 被正确处理"，但**不能**证明以下两点：

### 风险 A：KIRO FREE 个人号耗尽时是否真的返回 402
`[推断]` 现有实现是针对某种账号类型观测出来的。
**免费个人号的额度耗尽信号可能不同**——可能是 429，可能是 403，
也可能是 200 + 错误体。若不是 402 且不含 `MONTHLY_REQUEST_COUNT`，
就会落进"未归类 400/其它"分支 → **不冷却、不转移 → 卡死在坏账号上**。

> ⚠️ 这正是你担心的场景。**必须用真实账号验证**，属 C 层用例。
> 这也是本轮真实上游测试**最有价值**的一条——
> 它不消耗有效额度（打的就是已耗尽的号），却能验证最关键的调度行为。

### 风险 B：模型限制的错误信号
这批号**只能用 4.5 系列、不能用 opus**。
若请求 opus，上游返回什么？是否被 `kiroErrorBadRequestInvalidModel` 正确归类？
`markKiroInvalidModelRateLimited` 会不会**误伤**——
把一个"只是不支持 opus"的健康账号给冷却了？

`[本仓库]` `kiro_runtime.go:890` 的条件是
`classification.Category == kiroErrorBadRequestInvalidModel && account.Type == AccountTypeOAuth`，
会走 `markKiroInvalidModelRateLimited(ctx, account, mappedModel)`。
需要确认这个冷却是**按模型维度**还是**整账号维度**——
若是整账号，一次 opus 试探就会废掉一个好号。

> → 加入 C 层用例，但**优先级低于风险 A**，且要用已耗尽的号来试，避免浪费。

---

## 4. 本文件产生的测试用例

| ID | 层 | 用例 | 消耗额度 |
|---|---|---|---|
| T-F04-1 | B (mock) | 402 + `MONTHLY_REQUEST_COUNT` → 账号被冷却到下月 1 号 + 转移 | 0 |
| T-F04-2 | B (mock) | 402 但**不含** `MONTHLY_REQUEST_COUNT` → 仍转移但不冷却 | 0 |
| T-F04-3 | B (mock) | 未归类 400 → 确认当前**不转移**（G1 缺口的回归锚点） | 0 |
| T-F04-4 | **C (真实)** | 打已耗尽账号，抓取真实状态码与响应体，验证风险 A | **0（号已废）** |
| T-F04-5 | **C (真实)** | 用已耗尽账号请求 opus，观察归类与冷却范围，验证风险 B | **0（号已废）** |
| T-F04-6 | **C (真实)** | 连续请求，验证调度最终收敛到额度正常账号 | 少量 |

> T-F04-4/5 的巧妙之处：**用已经废掉的账号做验证，零有效额度代价**。

关联：[F01](F01-official-aws-service-model.md)、[PLAN-AND-STATUS](../PLAN-AND-STATUS.md)

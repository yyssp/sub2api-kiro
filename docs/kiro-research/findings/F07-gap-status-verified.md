# F07 · 缺口清单 · 逐条实测核验

> **本文件的价值在于"排除"。** 调研阶段列了一堆疑似缺口，
> 但其中有几条我们**其实早就做了**。不先核实就动手，会白改一通、还可能改坏。
> 下表每一行都经过 `[本仓库]` 源码或实测核验。

---

## 一、核验结果总表

| 编号 | 缺口 | 实测状态 | 优先级 |
|---|---|---|---|
| **G6** | 工具 schema 超纲关键字全部透传 | 🔴 **确证存在**（实测 7/7 命中） | **P0** |
| **G8** | 归类器漏 `Invalid tool use format`/`REQUEST_BODY_INVALID` | 🔴 **确证存在**（实测落 unknown） | **P0**（改动最小） |
| **G5** | 工具名无字符集清洗 | 🟠 **确证存在** | P1 |
| **G2** | 请求体无体积守卫 | 🟠 **确证存在** | P2（本轮难验证） |
| **G4** | 主动丢弃上游 cache token | 🟡 **存在但口径未定** | P3（阻塞于实测） |
| ~~G1~~ | 400 不故障转移 | 🟢 **范围大幅收窄**，见下 | 降级 |
| ~~G7~~ | 孤儿 toolUse/toolResult | ✅ **已实现，无需改动** | — |
| ~~D1~~ | Origin 枚举 | ✅ **已正确，非缺口** | — |

---

## 二、✅ 已实现 —— 不要动

### G7：工具配对守卫（早已实现）
`[本仓库]` `internal/pkg/kiro/translator.go:475-476`
```go
currentToolResults, orphanedToolUseIDs := validateToolPairing(history, currentToolResults)
removeOrphanedToolUses(history, orphanedToolUseIDs)
```
实现在 `:2196-2220`。与 `agent-vibes`/`tau` 的做法同向
（我们选择"剔除孤儿 toolUse"，`tau/payload_guards.ts:81-109` 也是剔除侧）。

> **结论：F05 里列的 G7 撤销。** 社区有的这条我们已经有了。

### D1：Origin 归一化（已正确）
见 [F01 §2](F01-official-aws-service-model.md)。我们输出 `CLI`/`AI_EDITOR`，
均为 AWS 官方枚举合法值。**非缺口。**

### G1：400 故障转移 —— 范围收窄，不再是独立缺口
`[本仓库]` 实测发现 Kiro 路径**单独**实现了远比通用路径完整的 400 处理：

`kiro_error_classifier.go:97-115` 有 **5 个 400 子类**：
`bad_request_schema` / `bad_request_tool_pairing` / `bad_request_invalid_model` /
`bad_request_auth` / `bad_request_quota`，
且 `kiro_runtime.go:890-899` 对 `invalid_model` 有专门的冷却 + 故障转移。

> **结论：G1 从"独立缺口"降级为"G8 的一个症状"。**
> 真正的问题不是"400 不转移"，而是**某些 400 没被正确归类**（G8）。
> 归类对了，后续处理链路本来就是通的。
>
> 📌 这修正了我基于 `shouldFailoverUpstreamError` 不含 400 得出的初判——
> 那个判断只看了通用路径，**漏看了 Kiro 的专属分支**。

---

## 三、🔴 确证存在的缺口

### G6 / G8
详见 [F06](F06-schema-passthrough-gap.md)、[F05 §1](F05-400-two-error-strings-and-schema.md)。

### G2：请求体无任何体积守卫
`[本仓库]` `internal/pkg/kiro/translator.go:535-539`
```go
payloadBytes, err := json.Marshal(payload)
if err != nil { ... }
return &KiroBuildResult{Payload: payloadBytes, Context: requestCtx}, nil
```
**marshal 完直接返回，无长度检查、无裁剪。**

社区参考（`tau/request.ts:120-122`）：600KB 硬上限 / 220KB 软目标，
成对删除历史条目以保 role 交替。

⚠️ **本轮验证受限**：200k 上下文可能堆不到 600KB，
→ 即便实现了也**无法端到端证明生效**，最终须标「未验证」。

---

## 四、修正记录（我自己的判断变化）

调研过程中我有三处初判被后续证据推翻，如实记录：

| 初判 | 修正后 | 推翻它的证据 |
|---|---|---|
| 工具名连字符非法，是 400 主因 | **连字符合法**；工具名只是次要成因 | AWS 官方 service-2 + tau 测试 + agent-vibes 抓包 |
| 400 一律不故障转移（G1） | Kiro 路径有 5 个 400 子类和专属转移分支 | `kiro_error_classifier.go` 实测 |
| 归类器只匹配一条错误串 | 已覆盖 `improperly formed request`，**只漏 `Invalid tool use format`** | 探针实测 |

> 这三条都是"我原以为更糟/更简单，实际不同"。
> 共同教训：**动手前必须实测本仓库当前状态**，
> 社区调研给的是"可能的问题清单"，不是"我们的问题清单"。

关联：[F05](F05-400-two-error-strings-and-schema.md)、[F06](F06-schema-passthrough-gap.md)、[F04](F04-quota-exhaustion-and-failover.md)

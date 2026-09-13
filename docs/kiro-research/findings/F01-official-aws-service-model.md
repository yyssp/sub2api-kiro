# F01 · 官方权威证据：AWS 自家服务模型

> **这是本次调研证据等级最高的一份材料。** 在此之前，所有关于"上游到底怎么校验"的结论
> 都来自社区实现的注释与防御性代码（`[源码]` 级）。本文件的来源是 **AWS 官方 VSCode 插件仓库
> 内自带的服务模型定义**，等级为 `[官方]`。
>
> 来源仓库：`aws/aws-toolkit-vscode`（★1997，2026-09-09 仍在推送，`fork=false`）
> 克隆位置：`/tmp/kiro-r5/aws_aws-toolkit-vscode`（`--depth 50`）

---

## 0. 为什么之前没找到它

前四轮全部按仓库名检索（`kiro`、`kiro2api`、`keirouter`…），而 AWS 官方仓库
**名字里没有 "kiro"**。第五轮改用 **code search 按协议特征词检索**
（`assistantResponseEvent`、`uncachedInputTokens` 等），它才出现在结果里。

> 📌 教训：**按仓库名检索会漏掉上游厂商自己的实现**。协议调研应当优先
> 用协议特征词做代码级检索。

---

## 1. 工具名校验规则（settles V9）

**文件**：`packages/core/src/codewhisperer/client/user-service-2.json`
这是 AWS 的 **service-2 模型文件**（AWS SDK 生成器的输入），字段约束由服务端定义。

```json
"ToolName": {
  "type": "string",
  "documentation": "<p>The name for the tool.</p>",
  "max": 64,
  "min": 0,
  "pattern": "[a-zA-Z0-9_-]+",
  "sensitive": true
}
```

同文件另外两条相关约束：

```json
"ToolUseId":       { "type":"string", "max":64,       "min":0, "pattern":"[a-zA-Z0-9_-]+" }
"ToolDescription": { "type":"string", "max":10240,    "min":1 }
```

### 1.1 这条证据推翻了什么

| 说法 | 来源 | 是否成立 |
|---|---|---|
| `^[a-zA-Z][a-zA-Z0-9_]{0,63}$`（连字符**非法**、首字符须字母） | `mydisha/keirouter:kiro.go:1069-1075` 注释 | ❌ **错误** |
| `^[a-zA-Z0-9_-]+$`（连字符**合法**） | `easayliu/kiro.rs:converter.rs:1380-1385` 注释 | ✅ **正确** |
| 必须是纯 camelCase、无下划线 | `Quorinex/Kiro-Go 0f8035d` | ❌ **错误**（过度防御） |

**直接后果：我此前推演的"MCP 服务器名带连字符（`mcp__tdesign-mcp-server__x`）会让整账号 400"是错的。**
连字符完全合法。真正会被拒的是 `$`、`.`、空格、中文等 —— 与 `easayliu` 的实测故障样本
（`$WEB_SEARCH`、`$MUTLI_1.N.1-Read`）完全吻合。

### 1.2 对 sub2api 的结论（G5 需要重新定义）

`[本仓库]` `backend/internal/pkg/kiro/translator.go:2010-2026` 的 `mapKiroToolName` 仍然
**没有任何字符集清洗** —— 这个缺口本身依然成立，但**触发条件和修复方案都要改**：

- 允许集应为 `[a-zA-Z0-9_-]`（**保留连字符**，不要把它换成 `_`）
- 长度上限官方是 **64**；我们 `kiroMaxToolNameLen = 63`（`:33`），**偏保守但安全**，可不改
- `min: 0` 且 `pattern` 无 `^$` 锚定 —— `[推断]` service-2 的 pattern 通常按全匹配语义执行，
  但空串行为不确定，保险起见空名仍应拒绝或回落

⚠️ 若按原计划"把连字符换成 `_`"，会**无谓改写大量本来合法的 MCP 工具名**，
增加 `ToolNameMap` 反向映射负担和串名风险。**这是一个被官方证据避免掉的错误修复方向。**

---

## 2. Origin 枚举（settles D1）

**文件**：同上 `user-service-2.json`

```json
"Origin": {
  "type": "string",
  "documentation": "<p>Enum to represent the origin application conversing with Sidekick.</p>",
  "enum": ["CHATBOT","CONSOLE","DOCUMENTATION","MARKETING","MOBILE","SERVICE_INTERNAL",
           "UNIFIED_SEARCH","UNKNOWN","MD","IDE","SAGE_MAKER","CLI","AI_EDITOR",
           "OPENSEARCH_DASHBOARD","GITLAB","Q_DEV_BEXT","MD_IDE","MD_CE",
           "SM_AI_STUDIO_IDE","INLINE_CHAT"]
}
```

**关键发现**：合法值里有 `CLI` 和 `AI_EDITOR`，**但没有 `KIRO_CLI`**。

`[本仓库]` 我们的 `normalizeOrigin`（`internal/pkg/kiro/translator.go:1834-1843`）
把 `KIRO_CLI`/`AMAZON_Q` → `CLI`，把 `KIRO_AI_EDITOR`/`KIRO_IDE`/`""` → `AI_EDITOR`。
**这个映射方向与官方枚举完全一致** —— 我们发出去的是 `CLI`/`AI_EDITOR`，都是合法值。

### D1 结论：降级为「非缺口」

D1 原本的疑问是"KRS 端点硬编码 `AI_EDITOR`，而参考实现用 `KIRO_CLI`"。
现在可以确定：**`KIRO_CLI` 根本不是合法的 Origin 值**，社区实现里出现的 `KIRO_CLI`
是它们自己的内部表示，发送前同样要归一化。

`[待验证]` 仍未确定的是：**KRS 端点是否要求 `CLI` 而非 `AI_EDITOR`**。
官方枚举证明两者都合法，但不能证明特定端点接受哪个。这仍需实测（原 V2）。

---

## 3. 体积上限（部分回答 V1）

`user-service-2.json` 中所有 ≥100KB 的上限：

| shape | max | 含义 |
|---|---|---|
| `EventBlob` | **400,000** | 单个 event-stream 事件负载 |
| `ImageSourceBytesBlob` | 10,000,000 | 单张图片字节数 |
| `SuggestedFixCodeDiffString` | 200,000 | — |
| `ToolResultContentBlockTextString` | 10,000,000 | 单条 tool_result 文本 |
| `UserInputMessageContentString` | 10,000,000 | 单条用户消息文本 |

⚠️ **这些不能直接当作 V1 的答案**。社区观测到的 ~615KB 是**整个请求体**的阈值，
而上表是**单个字段**的上限。两者不是一回事：
- 单条消息可到 10MB，但整个 `conversationState` 序列化后仍可能在远低于此时被拒
- `EventBlob` 400KB 是**响应侧**事件的上限，与请求体无关

`[推断]` 整体 body 上限很可能由 API Gateway / 服务端配置决定，**不在 service-2 模型里**。
→ **V1 仍需实测**，社区的 ~615KB 仍是目前最好的估计。

---

## 4. tokenUsage 不在公开模型里（V7 仍未解决）

`[git]` 实测：在 `aws/aws-toolkit-vscode` 全仓库搜索 `tokenUsage` / `TokenUsage`：
- `packages/core/src/codewhisperer/client/*.json` —— **0 命中**
- `src.gen/@amzn/codewhisperer-streaming/src/models/*.ts` —— **0 命中**
- `src.gen/@amzn/amazon-q-developer-streaming-client/src/models/*.ts` —— **0 命中**

官方 SDK 里的 metadata 事件只有：
```ts
// src.gen/@amzn/codewhisperer-streaming/src/models/models_0.ts:3323-3335
export interface MessageMetadataEvent {
  conversationId?: string | undefined;
  utteranceId?: string | undefined;
}
```

**结论**：`metadataEvent.tokenUsage` **不属于公开的 CodeWhisperer/Q streaming 协议**，
它是 **Kiro 特有的扩展**（或更新的、尚未同步到公开 SDK 的字段）。

> 这解释了一个现象：为什么社区实现对这个字段的拼写有分歧
> （`ngh1105/Kiro-Go:proxy/kiro.go:577-578` 同时兼容 4 种拼写）—— 因为**没有公开模型可查**，
> 大家都是抓包逆向的。
>
> → **V7 不能靠读官方仓库解决，必须实测抓包。** G4 的前置条件不变。

---

## 5. 本文件确立/推翻的结论汇总

| 编号 | 结论 | 证据等级 | 影响 |
|---|---|---|---|
| V9 | 工具名 `pattern: [a-zA-Z0-9_-]+`, `max: 64` | **`[官方]`** | ✅ **已解决**；修正 G5 修复方案（保留连字符） |
| D1 | `Origin` 官方枚举含 `CLI`/`AI_EDITOR`，无 `KIRO_CLI` | **`[官方]`** | ✅ D1 降级为非缺口；V2 仍待实测 |
| V1 | 单字段上限已知，**整体 body 上限未知** | `[官方]`（部分） | ⚠️ 仍需实测 |
| V7 | `tokenUsage` 不在公开协议中，是 Kiro 扩展 | `[官方]`（反证） | ⚠️ 仍需实测，G4 前置条件不变 |

---

## 6. 自我核查：这份证据有多可信？

**支持强可信的理由**：
- service-2.json 是 AWS SDK 的**生成器输入**，字段约束由服务团队维护，不是第三方猜测
- 它与 `easayliu/kiro.rs` 的**真实线上故障样本**独立吻合（`$`、`.` 被拒；`-` 未被拒）
- 仓库本身是 AWS 官方、持续维护

**需要保留的疑虑**：
- `[推断]` Kiro 的上游端点（`q.*.amazonaws.com` / `runtime.*.kiro.dev`）**是否与该模型同一版本**，
  无法证明。Kiro 可能跑在更新或定制的服务版本上（`tokenUsage` 的存在本身就是证据）。
- 因此：**校验规则用官方模型，但"上游实际行为"仍以实测为准**。
  好在 G5 的修复方向在两种规则下都安全（见 §1.2）。

关联：[F02](F02-tool-name-sanitization.md)、[F04](F04-error-400-handling.md)

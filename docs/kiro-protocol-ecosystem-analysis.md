# Kiro 上游协议开源实现调研

> 调研目的：从社区的 Kiro 协议实现中，提取 **Kiro 上游协议本身** 与 **Claude Code(Anthropic) ↔ Kiro 协议互转** 的可借鉴经验，并与本仓库（sub2api）现有实现做差距对照。
>
> 调研日期：2026-09-13（三轮采集）
> 本文只做分析与记录，**未改动任何代码**。文末「待验证」一节列出了所有没有实证的推断。
> 采集规模：GitHub 侧 **30 个仓库完整克隆并本地读码**，外加 **3162 个 fork 的全量元数据枚举 + 272 个活跃 fork 的分叉增量实测**。

---

## 证据标记法（阅读本文前先看这个）

本文每条结论都带一个来源标记。**标记等级不同，可信度差别很大**，请勿一视同仁：

| 标记 | 含义 | 可信度 |
|---|---|---|
| `[本仓库]` | 直接读 sub2api 源码得出，附 `file:line` | **最高**，可直接据此改代码 |
| `[源码]` | 直接读已克隆仓库的源码，附 `repo:file:line` | **高**，但只证明"该实现这么做"，不等于"上游确实如此" |
| `[git]` | 本地 `git` 命令实测（提交数、日期、共同 hash、作者数） | **高**，客观可复现 |
| `[API]` | GitHub REST API 元数据（star、push 时间、fork 父子、ahead/behind） | **高**，但为采集时点快照 |
| `[推断]` | 由上述证据推导，**没有直接观测** | **中**，需按待验证项处理 |
| `[待验证]` | 只有单一来源或纯二手，**尚无法确认** | **低**，不可作为改动依据 |

三条硬性纪律，贯穿全文：

1. **"某实现这么写"≠"上游协议如此"。** 社区实现可能过度防御，也可能已过时。凡涉及上游真实行为的结论，必须标注是否经实测。
2. **同源代码不算独立证据。** 一个结论若出现在 A 仓库及其 fork 中，那是 **1 份**证据，不是 2 份。本文凡称"独立印证"，均已用 `git log -S` 回溯到各自最初引入的提交确认非同源（例见 §5.6 的 camelCase 辨析）。
3. **假解析实现的行为描述不可引用。** 见 §5.4：大量实现靠字符串扫描猜事件类型，它"没看见"的事件不代表上游没发。

---

## 零、先说结论（TL;DR）

本仓库的 Kiro 实现整体是成熟的：AWS event-stream 解析、tool_use/tool_result 配对校验、双端点（Q / KRS）、六类 400 细分、profileArn 解析、图片 token 估算都已具备，**部分能力甚至比社区参考实现更细**。

但对照下来有 **5 个已在本地代码中确证的缺口**，按严重度排序。
建议优先动手的是 **G5 → G1 → G4**（G5 用户挂个 MCP 就能触发，改动也最小）：

| # | 缺口 | 严重度 | 证据强度 |
|---|---|---|---|
| G5 | 工具名只截长度**不清洗字符集**。带连字符的 MCP 服务器（如 `tdesign-mcp-server`）生成的工具名不符合上游校验器，**整个请求被 400 拒掉** | 高 | 我方缺失 `[本仓库]` 已确证；**4 个非同源实现**各自做了清洗（Rust/Go/Go/Go），其中 `Panniantong/kiro.rs` 有一条正是 MCP 连字符形态的专门测试 `[源码]`（§5.6） |
| G1 | 六类 400 子分类中有五类**分类后未被消费**，其中 `bad_request_quota` 导致「额度耗尽」被当作客户端错误直接返回，**不触发故障转移、不冷却账号** | 高 | `[本仓库]` 已确证；`ngh1105/Kiro-Go` 给出了可直接借鉴的处理范式 `[源码]`（§5.6） |
| G4 | 上游 `metadataEvent.tokenUsage` 会下发真实的 `cacheRead/cacheWriteInputTokens`，我们**代码注释里写明"刻意忽略"**，导致缓存策略效果只能用自家模拟值自证 | 高 | 「我们主动丢弃」`[本仓库]` 已确证；「上游确实下发」现有 **5 个非同源实现**印证（Rust×3 + Go×2）`[源码]`，仍待 V7 实测 |
| G2 | 缺少「转换后 payload 校验层」——Kiro 对超大 body（社区观测 **~615KB**）等多种情况回 `400 Improperly formed request`，落到 G1 的未消费分支 | 中 | 缺失已确证；7 条成因中我们已覆盖 6 条，唯一真缺口即 G5 |
| G3 | Kiro `machine_id` 在未落库时由 `refresh_token` 派生，而 refresh_token 会轮换 → 设备指纹漂移（与刚修完的 Cursor B 项同源）。**仅影响「导入时就没带 machine_id」的 OAuth 账号**：已落库的值在刷新时不会被冲掉 | 低·潜在 | 派生链路已确证；影响面已收窄（见 G3 三点核对）；「漂移→封号」未证实 |

外加 1 个**存疑项**（非缺口，需真实上游验证）：

- D1：KRS(CLI) 端点的请求体 `origin` 被硬编码成 `AI_EDITOR`，参考实现用的是 `KIRO_CLI`。

---

## 一、调研对象

### 1.1 本地二开：`~/Desktop/procode/2ue_kiro.rs`

- 仓库：`git@github.com:2ue/kiro.rs.git`，分支 `main`，版本 `0.0.161`（提交 `31c947b`）
- 语言：Rust，约 **232,577 行**（含测试）
- 最近提交：2026-09-11

模块划分很清晰，正好对应本次调研的两个关注面：

```
src/kiro/          # Kiro 上游协议（57,204 行）
  ├── endpoint/    # 端点差异：cli.rs(836) / ide.rs(799)
  ├── parser/      # AWS event-stream：frame.rs(178) / header.rs(317) / decoder.rs(357)
  ├── model/       # 请求体、事件、凭据、用量额度
  ├── token_manager/  # 凭据池与刷新（约 20k 行，含测试）
  ├── protocol.rs  # profileArn / agentMode 解析（401）
  └── machine_id.rs   # 设备指纹（282）

src/anthropic/     # Claude Code 侧协议与互转（81,018 行）
  ├── converter/   # Anthropic → Kiro：content/schema/history/tools/tool_pairing/thinking
  ├── stream.rs    # Kiro → Anthropic 流式（7,527 行）
  ├── payload_guard.rs      # 转换后体积保护（8,564 行）
  ├── transcript_sanitizer.rs  # 过滤泄漏的内部协议文本（2,103）
  ├── prompt_cache.rs       # 缓存模拟（2,464）
  └── usage.rs / pricing.rs # 用量与计费
```

> ⚠️ 说明：本地这份没有配置上游 remote（`git remote -v` 只有 `origin` 指向 2ue 自己的仓库），因此**无法在本地计算它相对 `hank9999/kiro.rs` 的增量**。血缘与增量对比见第五节（GitHub 侧调研）。

### 1.2 GitHub 侧（三轮采集，逐轮扩大）

| 轮次 | 采集范围 | 产出 | 对应章节 |
|---|---|---|---|
| 第一轮 | kiro.rs 家族（原仓库 + 主要二开） | 2 个仓库克隆 | §5.1 §5.2 |
| 第二轮 | 跳出 kiro.rs，覆盖 Go/Python/JS/TS 实现 | +14 个仓库克隆 | §5.5 |
| 第三轮 | **系统枚举 fork 树，按"真有自己提交"筛活跃二开** | 1280 fork（采样）→ +10 个高价值二开克隆 | §5.6 |
| 第四轮 | 自查发现第三轮覆盖率仅 38%，改为**全量分页枚举** | 3162 fork → 272 个活跃 fork 实测增量 → +4 个被漏掉的二开 | **§5.6.6（新）** |

合计 **30 个仓库完整克隆并读码**，覆盖 Rust / Go / Python / JS / TS 五种语言。

第三轮的方法值得单独说明，因为它决定了"二开是否真的比原仓库完善"这个判断的可信度：

1. `[API]` 对 12 个主要仓库枚举其全部 fork（`/repos/{r}/forks`，分页），共 **1280 个**；
2. `[API]` 按 `pushed_at >= 2026-08-01` 筛出 **230 个**近期活跃 fork；
3. `[API]` 对每个活跃 fork 调 `compare` API 实测其相对父仓库的 **ahead/behind** —— 成功 228 个，失败 2 个（HTTP 404）；
4. `[git]` 按 ahead 数排序，克隆 top 10 中尚未收录的二开并实际读码。

> ⚠️ 第 3 步踩了一个坑，值得记下来：`compare` API 对 **`fork=false` 的仓库会直接 404**。
> `ZyphrZero/kiro.rs` 正是这种情况（`[API]` 实测 `fork: False`，却与 `hank9999/kiro.rs` 共享 59 个提交），
> 所以**只靠 GitHub 的 fork 标志会漏掉"未声明的二开"**，必须辅以共同提交 hash 判定（§5.2 用的就是这个方法）。

> 🔒 调研纪律：所有克隆只落在 `/tmp/kiro-research/`、`/tmp/kiro-research-b/`、`/tmp/kiro-r2/`、`/tmp/kiro-r3/`、`/tmp/kiro-r4/`，
> **没有任何文件写入本项目目录**，也没有执行任何来自被调研仓库的脚本或二进制。

---

## 二、Kiro 上游协议要点（来自参考实现，附证据）

### 2.1 两套端点，不是一套

| | IDE 端点 | CLI 端点 |
|---|---|---|
| API host | `q.{region}.amazonaws.com` | `runtime.{region}.kiro.dev` |
| 流式路径 | `/generateAssistantResponse` | `/`（runtime 根） |
| 请求体 `origin` | `AI_EDITOR` | `KIRO_CLI` |
| 查询参数 | `origin=AI_EDITOR&maxResults=50` | `origin=KIRO_CLI` |
| User-Agent | Kiro IDE 版本号 | `aws-sdk-rust/1.3.15 ... app/AmazonQ-For-CLI` |

证据：`2ue_kiro.rs:src/kiro/endpoint/ide.rs:4-5,88`、`src/kiro/endpoint/cli.rs:3-7,110,132`

值得抄的一个细节：CLI 端点改写 `origin` 时，**防了 JSON unicode 转义绕过**。
如果请求体里写成 `"origin":"AI_EDITOR"`，朴素的 `body.contains("\"origin\"")` 会漏判，导致该改的没改。

证据：`src/kiro/endpoint/cli.rs:229-232`（先做快速字符串命中，未命中且含 `\` 时才走 `contains_json_object_key` 的真解析）、`src/kiro/endpoint/mod.rs:39-44`（函数注释明确写了这个攻击面）、测试 `cli.rs:333`。

### 2.2 AWS event-stream 帧格式

```
┌──────────────┬──────────────┬──────────────┬──────────┬──────────┬───────────┐
│ Total Length │ Header Length│ Prelude CRC  │ Headers  │ Payload  │ Msg CRC   │
│   (4 bytes)  │   (4 bytes)  │   (4 bytes)  │ (变长)    │ (变长)    │ (4 bytes) │
└──────────────┴──────────────┴──────────────┴──────────┴──────────┴───────────┘
```

- Total Length 含自身 4 字节
- Prelude CRC 校验**前 8 字节**
- Message CRC 校验**整条消息去掉末尾 4 字节**
- 上限 16MB（`MAX_MESSAGE_SIZE`）

证据：`src/kiro/parser/frame.rs:5-17,24-30,110-133`

解析器是**无状态纯函数** `parse_frame(buffer) -> Ok(Some((frame, consumed))) | Ok(None) | Err`，缓冲区管理交给上层 `EventStreamDecoder`。`Ok(None)` 明确表示「数据不足，等更多」，与「解析失败」区分开。这个三态返回值设计比 bool 更不容易在流式场景里出错。

另有一处边界校验值得注意：`headers_end > total_length - 4` 时直接报错（`frame.rs:140`），防止头部长度字段把读取范围推到 payload/CRC 区。

### 2.3 事件类型

核心只有两个：`assistantResponseEvent`（文本增量）、`toolUseEvent`（工具调用）。
证据：`src/kiro/model/events/base.rs:37-38,53-54`

### 2.4 请求体结构（`conversationState`）

```jsonc
{
  "conversationState": {
    "conversationId": "...",
    "agentContinuationId": "...",   // 可选
    "agentTaskType": "vibe|spec",   // 可选
    "chatTriggerType": "MANUAL|AUTO",
    "currentMessage": {
      "userInputMessage": {
        "content": "...",
        "modelId": "...",
        "origin": "AI_EDITOR|KIRO_CLI",
        "images": [{"format":"png","source":{"bytes":"<base64>"}}],
        "userInputMessageContext": {
          "tools": [...],        // 工具定义
          "toolResults": [...]   // 工具执行结果
        }
      }
    },
    "history": [
      {"userInputMessage": {...}},
      {"assistantResponseMessage": {"content":"...","toolUses":[...],"reasoningContent":{...}}}
    ]
  }
}
```

证据：`src/kiro/model/requests/conversation.rs:11-30,117-145,178-190,249-355`

**重要澄清**：Kiro 的工具调用是**结构化原生支持**的（`toolUses` / `toolResults` 是一等字段），不是只能塞文本。
`reasoningContent` 也是原生的，且是**严格 union**——只能是 `reasoningText{text,signature}` 或 `redactedContent` 二选一，多给一个字段就反序列化失败（`deny_unknown_fields`，见 `conversation.rs:407-424` 与测试 `:516-527`）。

### 2.5 profileArn：按端点与凭据类型分流

这块的逻辑密度很高，也是最容易踩坑的地方。参考实现把它拆成**两个**函数，因为流式和非流式的要求不同：

- `resolve_profile_arn()`（给 header/query 类 API：MCP、ListAvailableModels、usage）
  → **不允许**发送 BuilderId 占位符或 Enterprise 兜底 ARN，因为它们不是调用方真实拥有的 profile，会让本来有效的账号 400/403。
- `resolve_streaming_profile_arn()`（给流式请求体）
  → BuilderId/免费 OAuth **仍然需要** body 级 profileArn；Enterprise/IdC 用**按 region 计算的兜底 ARN**，且**只用于请求体、不得持久化**。

证据：`src/kiro/protocol.rs:92-161`（含 doc 注释原文）、测试 `:294-347` 正反两面都锁了。

已知常量：
- BuilderId 占位符 `arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX`
- Social profile `arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK`
- Enterprise 兜底 account `610548660232` / profile `VNECVYCYYAWN`，region 仅 `us-east-1` 与 `eu-central-1` 两档

（`protocol.rs:8-14,37-47`）

### 2.6 token 用量：上游**确实会返回**，只是老版本实现没用

> ⚠️ **本节已修正。** 初稿写的是「上游不返回 token 用量」，那个结论来自本地二开与 `hank9999/kiro.rs`，
> 对**新版上游**是错的。修正依据见下。

**上游事实**：Kiro 的 `metadataEvent.tokenUsage` 是一个完整的四字段用量结构，
定义见 `ZyphrZero/kiro.rs:src/kiro/model/events/metadata.rs:13-28`：

| 字段 | 含义（原注释） |
|---|---|
| `uncachedInputTokens` | 未命中缓存、也未写入缓存的输入 token |
| `outputTokens` | 模型输出 token |
| `cacheReadInputTokens` | 从服务端 prompt cache **读取**的输入 token |
| `cacheWriteInputTokens` | 本次**写入**服务端 prompt cache 的输入 token |

即 **Kiro 服务端有真实的 prompt cache，并且把读写命中量如实下发**。
该实现还做了两件正确的事：`sanitized()` 把所有计数钳到非负（`:33-35`，注释「清理不可信上游值」），
`total_input_tokens()` 明确「缓存读取是总输入的**子集**」（`:43-47`）——这个口径关系很重要，加错会双算。

另外 `meteringEvent` 是**独立的计费事件**，payload 形如
`{"unit":"credit","unitPlural":"credits","usage":<f64>}`（`src/kiro/model/events/metering.rs:3`），
且该实现明确注释「上游 meteringEvent 只下发 credit；token / cache 字段不存在」（`src/anthropic/stream.rs:1627`）。
**不要把 credit 和 token 混为一谈。**

**为什么初稿会错**：

- `hank9999/kiro.rs`（分叉点 2026-05-13 之前）全树搜不到 `tokenUsage` / `cacheReadInputTokens`（已验证，0 命中）。
- 本地二开 `2ue_kiro.rs` 也搜不到**真实解析**——只有压测器 `src/bin/kiro_loadtest.rs:2243-2244` 里
  `fake_kiro_token_usage` **伪造**这些字段喂给自己。也就是说本地二开知道这个结构存在，却只用于 mock。
- 所以「本地估算」（`src/token.rs:1-8` 的 CJK=4.5 启发式 + 可选远程 count_tokens）在这些实现里
  不是「没办法」，而是**没跟上上游**。

> 📌 对我们的意义（已改变）：关于 `anthropictokenizer` 的结论要**收窄**。真实 tokenizer 仍然优于 CJK=4.5 启发式，
> 但两者都只该用于**上游没下发用量时的兜底**。上游给了真值就必须用真值——见下面的 **G4**。

---

## 三、Claude Code ↔ Kiro 互转的经验（重点）

这一节是本次调研最有价值的部分：这些都是**别人踩过的坑**。

### 3.1 JSON Schema 必须清洗，否则一个脏工具毁掉整个请求

参考实现的注释把动机说得很直白：

> 上游按 draft 2020-12 校验工具 `input_schema`，但 Claude Code / MCP 工具定义经常混入旧 draft、OpenAPI 或简写结构。这里保守清洗成 Kiro/Anthropic 更容易接受的 JSON Schema 子集，**避免单个脏工具 schema 导致整次请求被 400 拒绝**。

证据：`src/anthropic/converter/schema.rs:3-7`

具体做法：
- 根级强制 `type: "object"`、强制存在 `properties`（`schema.rs:18-26`）
- 把根级的 `allOf` / `oneOf` / `anyOf` **展平**，并区分 required 的合并语义：
  - `allOf` → required 取**并集**
  - `oneOf` / `anyOf` → required 取**交集**

  证据：`schema.rs:29-33`（`RequiredMergeMode::Union` / `Intersection`）

  这个区分是对的：allOf 是「都要满足」，oneOf/anyOf 是「满足其一」，只有交集里的字段才是无论走哪个分支都必填的。

### 3.2 tool_use / tool_result 配对：孤儿必须丢，且 Kiro 要求「紧邻」

两个独立函数：

**a) `sanitize_history_tool_results`**（`converter/tool_pairing.rs:9-69`）
对每条 history user 消息，它的 `toolResults` 只允许匹配**紧邻的前一条 assistant** 的 `toolUses`。不匹配的直接丢弃并计数告警；重复 id 只保留第一条。

> 这是比「全局 id 集合」更严的约束。注释与实现都指向 Kiro 要求 tool_result 紧跟对应的 tool_use。

**b) `validate_tool_pairing`**（`tool_pairing.rs:82+`）
收集全部 tool_use_id，过滤掉孤立的 tool_result，同时返回**孤立的 tool_use_id 集合**，交给 `remove_orphaned_tool_uses` 把 history 里没有结果的 tool_use 也摘掉。

**双向都要清理**：只清 tool_result 不清 tool_use，上游同样会拒。

> ✅ 本仓库已实现同等能力：`internal/pkg/kiro/translator.go:475-476`（`validateToolPairing` + `removeOrphanedToolUses`）。这一项**无缺口**。

还有个细节：清理后如果 user 消息变成空内容，要塞占位符（`EMPTY_USER_CONTENT_PLACEHOLDER = "."`），因为 Kiro 不接受空 content。（`tool_pairing.rs:63-68`、`payload_guard.rs:34`）

### 3.3 历史里引用的工具，必须在当前 tools 里有定义

> Kiro API 要求：历史消息中引用的工具必须在 `currentMessage.tools` 中有定义

证据：`src/anthropic/converter/tools.rs:57-58`（注释原文）+ `collect_history_tool_names` 收集历史用过的工具名，为不在当前 tools 列表里的**补占位定义**。

这是个很容易漏的点：客户端第二轮可能只传了缩减后的 tools 列表，但 history 里还留着上一轮的 tool_use，直接发就会 400。

### 3.4 模型会把内部协议文本当正文吐回来，需要在响应侧过滤

`transcript_sanitizer.rs` 解决的问题：由于转换过程会把工具脚手架变成文本行（`user Continue`、`Tool results provided.`、`Tool results:`），模型有时会**模仿着把这些行输出到正文里**。

证据：`src/anthropic/transcript_sanitizer.rs:1-4,21-30`

它的设计克制得很好，值得学：
> The filter deliberately requires a complete, project-specific transcript signature. Single words such as `Continue`, `user`, or `Hash` remain ordinary visible text.

即**只匹配完整的、项目特有的签名**，不做宽泛的关键词过滤——否则用户正常聊天里出现 "Continue" 就被吞了。且是**增量式**的（streaming / 非streaming / history 三条路径共用同一个 sanitizer）。

### 3.5 流式转换的真实难点：模型吐 XML

`stream.rs`（7,527 行）里大量篇幅在处理一件事：模型把 `<thinking>`、`<function_calls>`、`<search_web>` 这类标签**当作文本输出**，需要在流式过程中边收边解析。

难点在于**区分「真的标签」和「模型在讨论这个标签」**：

- `find_real_thinking_end_tag`：跳过被反引号/引号包裹的标签；要求结束标签后面跟 `\n\n`；标签在缓冲区末尾时要等更多数据
  （`stream.rs:264-281`）
- `find_real_thinking_end_tag_at_buffer_end_for`：边界场景（thinking 后立刻 tool_use、或流结束）此时没有 `\n\n`，改为要求「之后全是空白字符」
  （`stream.rs:327-334`）
- `find_char_boundary`：UTF-8 多字节字符不能按字节切，否则 panic
  （`stream.rs:20-24`）

> 📌 `find_char_boundary` 这个在 Go 里对应的是按 rune 边界切分。Go 的 `string` 切片不会 panic，但会产生非法 UTF-8 字节，流式输出给客户端同样是坏的——**同类问题在 Go 里更隐蔽**。

### 3.6 老实现的 prompt cache 是「模拟」出来的（但上游其实给了真值）

> ⚠️ **本节前提已修正**，与 2.6 一并看。上游**会**下发 `cacheReadInputTokens` / `cacheWriteInputTokens`
> （`ZyphrZero/kiro.rs:src/kiro/model/events/metadata.rs:24-28`）。
> 所以下面这套「模拟」不是唯一选择，而是**老版本在不知道上游有真值时的替代方案**——
> 它仍然是有价值的**兜底**（上游没下发 tokenUsage 时），但不该作为主路径。
> `ZyphrZero` 那条 `feat(cache): 缓存统计如实反映上游真值` 提交正是在做这个切换。

本地二开的缓存字段是**推算**的：它假定 Kiro 上游不返回 cache token 统计，
`prompt_cache.rs` 按 Anthropic 的真实规则**推算**出 `cache_read_input_tokens` / `cache_creation_input_tokens`。

关键在于它尊重 Anthropic 的**官方最小可缓存 token 数**，不同模型不同档：

| 常量 | 值 |
|---|---|
| `DEFAULT_MIN_CACHEABLE_TOKENS` | 1024 |
| `HAIKU_3_MIN_CACHEABLE_TOKENS` | 2048 |
| `EXTENDED_MIN_CACHEABLE_TOKENS` | 4096 |
| `DEFAULT_PROMPT_CACHE_TTL` | 5 分钟 |
| `HOUR_PROMPT_CACHE_TTL` | 1 小时 |

证据：`src/anthropic/prompt_cache.rs:13-23`，测试 `:1588,1597`（"Haiku 4.5 must not simulate cache below its 4096 token minimum"）

还有资源上限，防止缓存表把内存吃穿：单账号 200 条、全局 20000 条、单条 TTL 24 小时、估算字节上限 256MB（`prompt_cache.rs:19-23`）。

> 📌 与本仓库的关系：`docs/kiro-rs-cache-strategy-analysis.md` 已经分析过这套策略，本次只补充两点新证据——(a) 缓存值是**模拟**而非上游真实回传；(b) 模拟必须卡在官方最小 token 门槛之上，否则伪造出的 cache 命中在数值上就不成立。
> 这也再次印证了「小负载测缓存会让结论完全失真」——低于 1024 token 的请求，按设计就**不该**产生任何缓存命中。

### 3.7 兼容性重试：剥离 reasoningContent 再试一次

当会话跨越了模型、凭据或上下文边界，Kiro 可能拒绝之前签名的 reasoning 块。参考实现的处理是：**保留可见内容与工具历史不变，只摘掉所有 `reasoningContent` 后重试**。

证据：`src/kiro/model/requests/conversation.rs:94-111`（`clear_history_reasoning_content_for_compatibility_retry`）

测试锁得很死（`conversation.rs:568-647`）：重试后 `content`、`toolUses`、当前 `toolResults` 配对**必须逐字节保留**，只有 `reasoningContent` 消失。

这是个好范式：**降级重试要精确到字段，不能粗暴丢整条历史**。

---

## 四、本仓库（sub2api）的差距对照

> 本节所有结论都在本仓库代码中实际核对过，标注了文件:行号。

### G1（高）六类 400 子分类，五类分完就扔

本仓库的 400 细分**比参考实现更细**，`classifyKiroBadRequest` 分出六类
（`backend/internal/service/kiro_error_classifier.go:99-113`）：

`bad_request_schema` / `bad_request_tool_pairing` / `bad_request_invalid_model` / `bad_request_auth` / `bad_request_quota` / `bad_request_unknown`

但实际消费情况（grep 分类器文件之外的引用）：

| 分类 | 分类器外引用次数 |
|---|---|
| `kiroErrorBadRequestInvalidModel` | 1（`kiro_runtime.go:890`）|
| `kiroErrorBadRequestSchema` | **0** |
| `kiroErrorBadRequestToolPairing` | **0** |
| `kiroErrorBadRequestAuth` | **0** |
| `kiroErrorBadRequestQuota` | **0** |
| `kiroErrorBadRequestUnknown` | **0** |

后果链条（`kiro_runtime.go:875-941`）：
1. `classifyKiroHTTPError` 算出 `bad_request_quota`
2. `logKiroBadRequestClassification` **只写一条 warn 日志**（`:883-885`）
3. 走到 `if resp.StatusCode == http.StatusPaymentRequired || s.shouldFailoverUpstreamError(resp.StatusCode)`——`shouldFailoverUpstreamError(400)` 为 **false**

   （`internal/service/gateway_forward.go:47-54`：只有 `401/403/429/529` 与 `>=500` 返回 true，400 不在其中）
4. 落到最后的分支：`c.JSON(...)` 把错误**原样返回给客户端**，类型标成 `invalid_request_error`（`claudeErrorType(400)`，`:947`），返回 `fmt.Errorf`

即：**额度耗尽的账号不会被冷却，不会故障转移到其他账号，用户直接看到一个 400**。

对照参考实现：它对「可归因于凭据的 400」会走 `report_quota_exhausted_deferred` + 解绑会话 + 排除该凭据后重试
（`2ue_kiro.rs:src/kiro/provider.rs:11526-11560`）。

而且它的判据很值得学——**不单凭 400 正文**：

```rust
// A fresh account-info quota guard is the only case where an otherwise opaque
// 400 may be attributed to the selected credential. Generic malformed/tool/image
// 400s remain fail-fast and never trigger broad fallback.
if matches!(bad_request_reason, "request_body_invalid_bad_request" | "bad_request")
    && self.token_manager.is_credential_account_quota_blocked(ctx.id)
```
（`provider.rs:11526-11535`）

即**必须同时满足**：(a) 400 属于「泛化的 body 无效」，(b) 独立拉取的额度快照显示该凭据已被 block。
这样既不会把「工具 schema 错」误判成额度问题而无谓轮换账号，也不会让真额度问题闷在那里。

**第三轮补充：`ngh1105/Kiro-Go` 给出了另一半范式 —— 「不重试」不等于「什么都不做」。**

它把"请求自身有问题"的 400 建模成一个**显式的永久错误类型**（`[源码]` `proxy/upstream_error.go:21-60`）：

```go
// upstreamPermanentErrorCode marks a request that the upstream rejects for a
// reason inherent to the request itself (e.g. "Improperly formed request").
// Retrying it — across endpoints or accounts — cannot succeed and only
// amplifies the damage (wasted upstream hits, scattered cache affinity,
// unwarranted account health penalties), so it must short-circuit both layers.
const upstreamPermanentErrorCode = "upstream_permanent_rejection"
```

调用点（`proxy/kiro.go:396-406`）在**读取任何 event-stream 字节之前**判定，因此流式场景下还没向客户端发出内容：

```go
if resp.StatusCode == 400 && isImproperlyFormedRejection(string(errBody)) {
    lastErr = newUpstreamPermanentError(resp.StatusCode, string(errBody))
    return lastErr   // 端点循环与账号循环同时短路
}
```

这个范式对我们有三点直接价值：

1. **印证了我们"400 不故障转移"是对的**（`shouldFailoverUpstreamError` 不含 400），不必改。
   它给出的理由比我们的更完整：重试不仅无效，还会**污染缓存亲和性**并**冤枉账号健康度**。
2. **补上了我们缺的那半**：它显式规定"**不惩罚账号健康度**"（注释：`account health is not penalised`）。
   我们当前是走到兜底分支直接 `c.JSON` 返回，账号健康度处理路径并未针对这种情况区分 —— 这是 G1 的一个具体落点。
3. **把 opaque 错误翻译成可行动的提示**（`upstream_error.go:62-80`）：
   > upstream rejected the request (commonly caused by too many tool definitions or an oversized payload);
   > try reducing the number of tools or shortening the context

   动机与 G5 高度相关：用户看到 `Improperly formed request` 会以为是网关 bug。
   `[源码]` 该注释原文称"绝大多数此类拒绝其实是工具定义过多或 payload 过大"。

> ⚠️ `[待验证]` 该文件多处注释标注 `Ported from kiro-tutu`。`[API]` 实测 GitHub 搜索
> `kiro-tutu` 返回 **0 个仓库**（已删除/改名/私有），因此**该来源无法回溯核实**，
> 本文只采信 `ngh1105/Kiro-Go` 中我直接读到的代码。

> 我们的分类器已经能直接识别 `bad_request_quota`（正文关键词匹配），比它的两段式判据更直接；缺的只是**消费这个分类**。

### G2（中）缺少转换后的 payload 校验层（体积只是成因之一）

参考实现有专门的 `payload_guard.rs`（8,564 行），动机写在文件头：

> Kiro upstream can return a generic `400 Improperly formed request` when the serialized request body is too large. This guard runs after Anthropic->Kiro conversion, measures the actual JSON payload bytes, and trims old history entries while preserving Kiro history invariants.

证据：`src/anthropic/payload_guard.rs:1-6`

关键点是 **"after conversion"**：只按 Anthropic 侧的 token 数估算是不够的，真正决定成败的是**转换后序列化出来的 JSON 字节数**。它还带 `UPSTREAM_IMAGE_SOURCE_MAX_BYTES = 5MB` 的单图上限（`payload_guard.rs:32`）。

本仓库检索结果：`grep -rn "Improperly formed|payloadGuard|trimHistory|maxPayload" --include=*.go internal/` 只命中 OpenAI 网关的一条无关常量，**Kiro 路径没有等价保护**。

后果：超大请求 → Kiro 回 400 → 落入 G1 的未消费分支 → 用户看到一个语焉不详的 `invalid_request_error`。**G2 的后果被 G1 放大**。

**独立来源给出了具体阈值**（本次调研新增，来自另一条技术栈的实现，与上面的 Rust 实现互为印证）：

`aceaura/KiroaaS`（活跃分叉，见第五节）在 `python-backend/kiro/config.py:612-623` 与 `.env.example:367-379` 写明：

| 项 | 值 / 说明 |
|---|---|
| 上游拒绝阈值 | **~615KB**，报错即 `Improperly formed request` |
| 其默认限值 | `KIRO_MAX_PAYLOAD_BYTES = 600000`（600KB，**留 15KB 安全余量**） |
| 默认是否开启 | `AUTO_TRIM_PAYLOAD = true`（默认开） |
| 裁剪策略 | 超限时**从最老的 history 对开始删**，直到装得下 |
| 常见触发场景 | 原注释：**「30+ 个工具定义」时最常见** |
| 重要边界 | 原注释：这是**字节守卫**，**不能**防 `Model context limit reached`（那由模型 token 上限决定，是另一回事） |

> 📌 这条对 V1 很有价值：两个**独立实现**（Rust 的 `payload_guard.rs` 与 Python 的 `AUTO_TRIM_PAYLOAD`）
> 都确认了「超大 body → 泛化 400」这个上游行为，且 Python 这边给出了可直接参考的数量级。
> 但 **615KB 仍是社区观测值，不是官方文档**，我没有实测验证，改动前仍建议按 V1 实测一次。
> 「30+ 工具定义」这一点尤其值得注意——Claude Code 默认就会带很多工具定义，这不是边缘场景。

#### 重要修正：`Improperly formed request` **不只是体积问题**

第二轮调研发现 `mydisha/keirouter`（Go，★140）把这个错误的**各种成因逐条记录在代码注释里**，
每条都对应一个修复。这说明把 G2 理解成「体积保护」是**过窄**的——体积只是其中一个成因。

`backend/internal/transform/kiro.go` 中已记录的成因清单：

| 成因 | 行号 | 我们是否已处理 |
|---|---|---|
| `max_tokens` 超出上游可接受范围 | `:145-148` | ✅ **已处理，且比它更细**（见下） |
| 模型名带 `[1m]` 后缀（Claude Code 的 1M 上下文标记） | `:201-205,244-248` | ✅ **已处理**（见下） |
| 客户端未传 tools，但历史里有结构化工具引用 | `:393-396,701-705` | ✅ 已处理（3.3/3.2） |
| toolUse 的 `input` 为 null（必须是非空对象） | `:451-453` | ✅ **已处理**（见下） |
| 工具名不符合校验器格式 | `:1069-1075` | ❌ **缺失 → G5** |
| tool_result 藏在 user 消息里（Anthropic 方言）未被识别，导致 toolUse 成孤儿 | `:503-509` | ✅ 已处理（3.2） |
| 历史以 assistant 开头 / 连续两条 assistant（要求严格交替） | `:539-541` | ✅ 已处理 |

> ✅ **`[1m]` 我们没问题**（已核对）：`internal/service/gateway_request.go:172-178` 的
> `normalizeClaudeCodeLongContextModel` 会循环剥离该后缀（含重复形式），
> 调用点在 `:203`，位于**所有平台共用**的请求解析路径上，且 Kiro 服务的
> Anthropic 端点传入的正是 `domain.PlatformAnthropic`（`internal/handler/gateway_handler.go:192`）。
> 注意该分支按**客户端协议**而非上游平台判断，所以 Kiro 走这条路径是被覆盖的。

另两项已核对完毕，**我们都做对了，其中一项比参考实现更好**：

> ✅ **`max_tokens` 钳制：我们更细。** `translator.go:401-412` 用
> `kiroMaxOutputTokensForModel(model)` 取**按模型**的上限，再做
> `if maxTokens > outputCap { maxTokens = outputCap }`，并把 `-1` 也归一到上限。
> 而 keirouter 是两个**写死的常量** `kiroMaxTokensDefault = kiroMaxTokensCeiling = 32000`
> （`kiro.go:149-150`），不区分模型。
>
> ✅ **`toolUse.input` 非 null：已保证。** `translator.go:2821-2832` 先
> `input := map[string]any{}`，只有 `toolInput.IsObject()` 时才填充，
> 所以序列化出去**永远是对象而非 null**——正好命中 keirouter `:451-453` 说的那个要求。

> 📌 结论：G2 不应只做「体积守卫」。真正该做的是**一个转换后的 payload 校验层**，
> 体积只是其中一项。核对后 7 条成因里我们已覆盖 6 条，**唯一真实缺口是 G5（工具名字符集）**。
> 这也说明本仓库的转换层整体质量是高的——但缺的那一条恰好是用户挂 MCP 就能触发的。

### G3（低·潜在）Kiro machine_id 对「从未落库」的 OAuth 账号会随 refresh_token 轮换而漂移

链路（`backend/internal/service/kiro_http_helpers.go:90-104`）：

```go
func buildKiroMachineID(account *Account) string {
    // 1. 优先用已落库的 machine_id / machineId  ← 这一步是对的
    for _, key := range []string{"machine_id", "machineId"} {
        if machineID, ok := kiropkg.NormalizeMachineID(account.GetCredential(key)); ok {
            return machineID
        }
    }
    // 2. APIKey 账号：由 api_key 派生（api_key 不轮换，稳定）
    // 3. OAuth 账号：由 refresh_token 派生  ← 问题在这
    return kiropkg.BuildMachineID(account.GetCredential("refresh_token"), "", fallbackKey)
}
```

`BuildMachineID` 的实现是 `sha256("KotlinNativeAPI/" + refreshToken)`（`internal/pkg/kiro/fingerprint.go:148-151`）。

而 Kiro 的刷新**确实会轮换 refresh_token**：social 刷新与 IdC 刷新都把上游返回的 `resp.RefreshToken` 写回凭据
（`internal/pkg/kiro/oauth.go:408`、`:644`）。

所以：**没有落库 machine_id 的 OAuth 账号，每次 refresh_token 轮换，设备指纹就会变一次。**

但影响范围比初看要小，以下三点均已核对：

1. **Kiro 确实会落库 machine_id**，只是**有条件**：`BuildAccountCredentials` 里是
   `if tokenInfo.MachineID != "" { creds["machine_id"] = tokenInfo.MachineID }`
   （`internal/service/kiro_oauth_service.go:676-677`）。即导入/登录时上游或导入文件带了这个值，就会落库。
2. **刷新不会把已落库的值冲掉。** 刷新路径是
   `MergeCredentials(account.Credentials, BuildAccountCredentials(tokenInfo))`
   （`kiro_token_refresher.go:49-50`，`kiro_token_provider.go:189`）。`MergeCredentials` 虽是「新值覆盖旧值」，
   但上面那个 `!= ""` 的守卫保证了空值**不会**进入新 map，因此旧值得以保留。
3. **刷新时 `tokenInfo.MachineID` 基本必为空**：`RefreshAccountToken` 只把 `refresh_token` 传下去，
   **不回传账号已落库的 machine_id**（`kiro_oauth_service.go:545-546`）。

   → 与第 2 点合起来：刷新既不会保留也不会破坏，是个 no-op。

因此 G3 的实际触发条件是**交集**而非全体 OAuth 账号：**导入时就没带 machine_id** 的账号，才会一路走到
`buildKiroMachineID` 的派生分支，并随 refresh_token 轮换而漂移。这也是把它从「中」降为「低·潜在」的原因。

这与刚修完的 Cursor B 项是**同一类问题**（见 `docs/cursor-protocol-hardening.md`），但有两点不同，不能直接照搬结论：
1. Kiro 这条链路**已经优先读落库值**，所以只影响「从未落过 machine_id」的存量账号；Cursor 修复前是**完全不落库**。
2. 参考实现 `2ue_kiro.rs` 用的是**同样的派生方式**（`sha256("KotlinNativeAPI/"+refresh_token)`，见 `src/kiro/machine_id.rs:79-82`），且**同样不持久化**——它连兜底值都只在**进程内**缓存、重启即变（`machine_id.rs:88-93` 的 doc 注释原文：「进程重启会重新随机；不持久化」）。

   → 也就是说**社区实现在这点上并不比我们好**，不能当作「应该这么做」的依据。它只能说明这个派生公式是社区通行的。

修复方向与 Cursor 的 F 项一致：建号时铸造并落库、刷新时只补不换。但**优先级低于 Cursor**，因为已有落库值优先的保护。

### G4（高）上游下发了真实 cache 用量，我们**代码里明确丢弃**

这是本次调研**新发现**的缺口，且**不需要真实上游即可确证**——我们自己的代码写明了在丢。

`backend/internal/pkg/kiro/translator.go:4321-4333`：

```go
if tokenUsage, ok := meta["tokenUsage"].(map[string]any); ok {
    if value, ok := toInt(tokenUsage["uncachedInputTokens"]); ok {
        usage.InputTokens = value          // ← 真值，用了
    }
    if value, ok := toInt(tokenUsage["outputTokens"]); ok {
        usage.OutputTokens = value         // ← 真值，用了
    }
    if value, ok := toInt(tokenUsage["totalTokens"]); ok {
        usage.TotalTokens = value
    }
    // Kiro cache usage is reported only from local emulation. Ignore
    // tokenUsage cache fields even if upstream includes them.
    updateKiroCreditsFromMap(usage, tokenUsage)   // ← cache 两个字段被刻意忽略
}
```

注意那条注释：**"Ignore tokenUsage cache fields even if upstream includes them"**——
它假定 "Kiro cache usage is reported only from local emulation"（缓存用量只来自本地模拟）。

**这个假定现在不成立了**：`metadata.rs:24-28` 证明上游会下发 `cacheReadInputTokens` / `cacheWriteInputTokens`。

后果：
1. **计费/统计失真**。我们对同一条 `tokenUsage` 一半信一半不信：`uncachedInputTokens` 采信为
   `usage.InputTokens`，但缓存读写量丢掉。而按上游口径缓存读取是**总输入的子集**
   （`metadata.rs:43-47`），少了这两项就无法还原真实总输入，缓存带来的成本下降也体现不出来。
2. **缓存策略的效果无法度量**。这直接关系到已有的缓存策略工作——如果上游真实命中量被丢弃，
   就只能依赖本地模拟值来判断策略有没有生效，而模拟值是我们自己算的，
   用它验证自己的策略是**自证**。

**第二轮调研补充：拿到了独立的第二份证据（不同语言、不同作者、带测试）。**

`d-kuro/kirocc`（Go，★58，真帧解析）对同一结构的实现，与 Rust 侧**完全一致**：

```go
// d-kuro/kirocc:internal/kiroproto/eventstream.go:156-176
case EventMetadata:
    var m struct {
        TokenUsage struct {
            UncachedInputTokens   int `json:"uncachedInputTokens"`
            OutputTokens          int `json:"outputTokens"`
            TotalTokens           int `json:"totalTokens"`
            CacheReadInputTokens  int `json:"cacheReadInputTokens"`
            CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
        } `json:"tokenUsage"`
    }
    ...
    InputTokens: tu.UncachedInputTokens + tu.CacheReadInputTokens,  // ← 口径确认
```

最后那行**独立印证了「缓存读取是总输入的子集」**这个口径（与 Rust 的
`total_input_tokens()` 定义一致）。它还把全链路接到了 Anthropic 字段：

| Kiro 字段 | → Anthropic 字段 | 证据 |
|---|---|---|
| `cacheReadInputTokens` | `cache_read_input_tokens` | `internal/respconv/usage.go:44` |
| `cacheWriteInputTokens` | `cache_creation_input_tokens` | `internal/respconv/usage.go:45` |

两个值得抄的处理细节（`internal/respconv/event_processor.go:65-79`）：

1. **钳非负**：`max(0, e.CacheReadInputTokens)`——与 Rust 的 `sanitized()` 同一动机（不信任上游值）。
2. **缓存计数可能独立于主计数下发**。原注释：
   *"Cache creation may be reported independently of the primary counts. Preserve it
   without marking the metadata as usable token usage."*
   即当 `metadataEvent` 没有可用的 input/output 计数时，**仍要用 `max()` 保留缓存计数**，
   且不能因此把该事件当成"权威用量"（否则会擦掉 metering 的计数并抑制兜底路径）。
   → 这条很容易写错：如果按「整个 tokenUsage 要么全信要么全不信」来实现，就会丢掉这种事件的缓存量。

> ⚠️ 仍待验证（V7）：Kiro 的**两套端点**（Q / KRS）是否都下发 `tokenUsage`、字段名是否一致，
> 我没有真实上游样本可验。上面两份证据都是**读社区代码**得来的，不是抓包。
> **改动前应先打一条真实请求把 `metadataEvent` 原样打日志确认。**
> 但「我们主动丢弃」这一半是**已确证**的，与上游行为无关。

**第三轮调研补充：证据增至 5 个非同源实现，并发现一条我们会写错的边界。**

第三轮从活跃 fork 中又找到 3 份独立解析（均 `[源码]`）：

| 实现 | 语言 | 关键证据 | 特点 |
|---|---|---|---|
| `ykn1002/kiro.rs` | Rust | `src/kiro/model/events/metadata.rs:20-53` | 五字段全解析，且写了**降级公式** |
| `claywong/kiro.rs` | Rust | `src/kiro/model/events/metadata.rs:25,37,49,61-63` | `max(0)` 钳位 + `saturating_add` 合并多个 metadataEvent |
| `ngh1105/Kiro-Go` | Go | `proxy/kiro.go:568-582` | **多别名兼容**读取 |

三条可直接抄的实现细节：

1. **口径再次确认**（`ykn1002/kiro.rs:metadata.rs:39-41` 注释原文）：
   > Anthropic 的 `input_tokens` 只统计未命中缓存的部分，缓存读写分别由
   > `cache_read_input_tokens` / `cache_creation_input_tokens` 单独上报。

   → 与 §2.6、与 `d-kuro/kirocc` 完全一致。**至此该口径有 3 个非同源实现印证，可视为可靠。**

2. **上游可能不给 `uncachedInputTokens`，要有降级公式**（`ykn1002:metadata.rs:44-53`）：
   ```rust
   if let Some(uncached) = self.uncached_input_tokens { return Some(uncached.max(0)); }
   // 退化：total - output - cache_read - cache_write
   let total = self.total_tokens?;
   Some((total - output - cache_read - cache_write).max(0))
   ```
   ⚠️ **这正是我们会踩的坑**：`translator.go:4321-4333` 只在 `uncachedInputTokens` 存在时赋值
   `usage.InputTokens`。若上游某些场景只给 `totalTokens` + 缓存字段，我们的 `InputTokens` 会**保持为 0**。
   这条与 G4 主体是**同一处代码**，修复时应一并处理。

3. **字段名可能有多种拼写**（`ngh1105/Kiro-Go:proxy/kiro.go:577-578`）：该实现对 cache write 同时接受
   `cacheWriteInputTokens` / `cache_write_input_tokens` / `cacheCreationInputTokens` / `cache_creation_input_tokens` 四种。
   `[推断]` 这种防御通常源于实际踩坑，但我无法确认它对应哪个上游版本 —— 属 V7 待验证范围。

> 📌 证据强度小结：「上游下发 cache 字段」现有 **5 个非同源实现**（Rust×3 + Go×2，作者/语言/仓库各不相同）
> 字段名与口径一致。这已是**读码能达到的最强证据**，但仍**不能替代抓包**（V7）——
> 5 份实现有可能同源于同一份早期逆向文档。

### G5（高）工具名只截长度、不清洗字符集 —— 带连字符的 MCP 服务器会让整个请求被拒

第二轮调研（扩大到 kiro.rs 家族之外）新发现，**已在本仓库代码中确证缺失**。

**上游规则**：CodeWhisperer 的工具名校验器要求 `^[a-zA-Z][a-zA-Z0-9_]{0,63}$`，
不满足时**整个请求**被拒（400 `Improperly formed request`），不是只忽略那一个工具。

证据：`mydisha/keirouter:backend/internal/transform/kiro.go:1069-1075` 的函数注释原文：

> The validator requires names matching `^[a-zA-Z][a-zA-Z0-9_]{0,63}$`; MCP tools
> (e.g. "mcp__server__tool") and other clients can send names with dots, hyphens,
> or lengths beyond 64, which makes Kiro **reject the whole request** with
> "Improperly formed request" (HTTP 400).

**我们的现状**：`internal/pkg/kiro/translator.go:2010-2026` 的 `mapKiroToolName` 做了三件事——
`web_search` → `remote_web_search` 重映射、超长名截断（`shortenToolNameIfNeeded`，
63 字符 + sha256 后 8 位，`:1992-2008`）、以及把短名回填 `ToolNameMap` 供响应侧还原。
**但完全没有字符集清洗**（全路径搜不到对连字符/点号的替换，仅 `fingerprint.go:140` 有个无关的去横线）。

**为什么这不是理论风险**：

| 工具名 | 是否合法 |
|---|---|
| `mcp__server__tool` | ✅ 合法（纯下划线） |
| `Read` / `Bash` / `TodoWrite` | ✅ 合法 |
| `mcp__tdesign-mcp-server__get-component-docs` | ❌ **非法**（连字符） |
| `tool.name` / `server:tool` | ❌ 非法 |
| `1tool` / `_leading` | ❌ 非法（首字符非字母） |

关键点：**MCP 服务器名带连字符极其常见**（`tdesign-mcp-server`、`xxx-mcp-server` 是社区默认命名习惯），
而 Claude Code 拼接出的工具名形如 `mcp__<server>__<tool>`，服务器名里的连字符会**原样带入**。
也就是说：**用户只要挂一个名字带连字符的 MCP 服务器，该账号的所有 Kiro 请求都会 400。**

**后果被 G1 放大**：这个 400 落进 G1 的未消费分支 → 返回 `invalid_request_error`、不故障转移、
不冷却账号。用户看到的是"这个号/这个模型坏了"，而真实原因是工具名里有个连字符。

**修复要点**（`keirouter:kiro.go:1076-1106` 的实现可直接借鉴，但有一个坑）：
非法字符替换为 `_`、首字符非字母时前置 `t_`、再截到 64。
⚠️ **清洗会引入重名**（`a-b` 与 `a.b` 都变成 `a_b`），所以 keirouter 把它和
`uniqueKiroToolName` 成对使用（`:1030`）。我们已有 `ToolNameMap` 反向映射机制，
清洗后必须同样保证单射，否则响应侧还原会串工具。

**第三轮调研补充：从「单一来源」升级为 4 个非同源实现，且有人专门为这个场景写了测试。**

第三轮在活跃二开中找到另外 3 份独立的工具名清洗实现（均 `[源码]`）：

| 实现 | 语言 | 位置 | 清洗策略 |
|---|---|---|---|
| `Panniantong/kiro.rs` | Rust | `src/anthropic/converter.rs:1765-1785` | 非字母数字 → `_`，**折叠连续分隔符**，去首尾 `_`，空则回落 `"tool"` |
| `zsecducna/Kiro-Go` | Go | `proxy/translator.go:1103-1131` | 按 `_`/`-` 切分后拼 **camelCase** |
| `ngh1105/Kiro-Go` | Go | `proxy/translator.go:1001-1029` | 同上（与 zsecducna 同源，见下方辨析） |

**最有力的一条证据**：`Panniantong/kiro.rs:src/anthropic/converter.rs:4939-4951` 有一个测试，
名字就叫 `test_map_tool_name_sanitizes_mcp_hyphen_namespace`，用例是：

```rust
let original = "mcp__read-feishu-document__get_document_content";
let result = map_tool_name(original, &mut map);
assert_ne!(result, original);
assert!(is_kiro_safe_tool_name(&result));
assert!(result.starts_with("mcp_read_feishu_document_get_document_content_"));
assert_eq!(map.get(&result), Some(&original.to_string()));   // ← 反向映射必须可还原
```

这与本文推演的失效形态**完全一致**（带连字符的 MCP 服务器名），且是真实社区项目的真实 MCP 服务器
（`read-feishu-document`）。`[推断]` 有人专门为此写回归测试，通常意味着**线上真的踩过**。

最后一行还独立印证了本文的修复要点：**清洗后必须保证反向映射可还原**。

**第四轮补充（全量 fork 枚举后新增）：`easayliu/kiro.rs` 提供了目前最强的一份证据 —— 带真实线上故障样本。**

`[源码]` `easayliu/kiro.rs:src/anthropic/converter.rs:1380-1400`，`[git]` 引入提交
`bb6cdd8`（2026-06-22，作者 easayliu，非继承自上游）标题即为
**「fix(kiro): 净化工具名非法字符，修复 Invalid tool use format 400」**。函数文档原文：

> Bedrock 工具名要求 `^[a-zA-Z0-9_-]+$`，`$`、`.`、空格等其它字符会被上游以
> `Invalid tool use format` / `REQUEST_BODY_INVALID` 拒绝（**实测** history 里出现
> `$WEB_SEARCH`、`$MUTLI_1.N.1-Read` 这类名字即触发）。

配套测试 `test_convert_request_sanitizes_illegal_tool_names`（`:2338-2372`）复现的正是线上 400，
并断言了本文强调的两个要点：**tools 定义与 history 的 toolUse 两端必须用同一函数净化**
（否则名称对不上），以及**反向映射可还原**。

> 🔴 **这条证据要求修正 V9 的正则**。easayliu 给出的是 `^[a-zA-Z0-9_-]+$` ——
> **连字符合法、首字符可以是数字**，与 keirouter 的 `^[a-zA-Z][a-zA-Z0-9_]{0,63}$`
> （连字符非法、首字符必须字母）**直接冲突**。其代码行为也确认了这点：它把 `.` 和 `$` 换成 `_`，
> 但**保留了 `-`**（测试断言 `_MUTLI_1_N_1-Read`，连字符原样留存）。
>
> 两者都是社区观测、都无官方文档，但 easayliu 这条**附带了真实故障样本与复现测试**，
> 且它触发 400 的字符是 `$` 和 `.` —— **不是连字符**。
>
> **对 G5 结论的影响**：
> - "工具名非法字符会导致整个请求被 400 拒"这个**核心结论不变**，反而由第 5 个非同源实现加强，
>   且首次有了**真实线上故障**佐证（此前全是防御性代码）。
> - 但"**连字符是否非法**"**现在存疑**。本文此前以 `mcp__tdesign-mcp-server__...` 作为
>   典型触发形态，若 easayliu 的规则为准，**该例子不会触发 400**。
> - **对修复方案的影响：无。** 取两者交集（只保留 `[a-zA-Z0-9_]`、首字符非字母则前置）
>   在两种规则下都合法，属安全侧。仍建议按 V9 实测确认，以免过度清洗导致不必要的名称改写。

⚠️ **另一个实现分歧，倾向于不采信**：`zsecducna/Kiro-Go` 与 `ngh1105/Kiro-Go` 的注释称
*"Kiro tool names must be pure camelCase (no underscores or dashes)"*，这与
`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`（**允许**下划线）矛盾。
`[git]` 实测：该函数由 `Quorinex 0f8035d`（2026-05-13）一次性引入，两个 fork 都只是继承 ——
**这是 1 份证据，不是 2 份**（对应纪律 2）。而"允许下划线"一侧有 keirouter + Panniantong 两个非同源实现，
且 `mcp__server__tool` 这种纯下划线名在生态中被广泛当作合法。
→ 结论：采信 `^[a-zA-Z][a-zA-Z0-9_]{0,63}$`。camelCase 是**更严格的子集**，两种规则下都安全，
因此这个分歧**不影响修复方案**（按正则清洗即可）。

> ⚠️ 待验证（V9）：`^[a-zA-Z][a-zA-Z0-9_]{0,63}$` 仍**没有上游文档或抓包证据**，
> 目前是 3 个非同源实现的一致行为 + 1 个矛盾实现（更严格）。
> 但**我们缺字符清洗**这一半是 `[本仓库]` 确证的，
> 且长度上限 63/64 与我们既有的 `kiroMaxToolNameLen = 63` 吻合，侧面支持该规则可信。

### D1（存疑）KRS 端点的 origin 硬编码为 AI_EDITOR

- 本仓库的 translator **支持**两种 origin：`normalizeOrigin` 处理 `KIRO_CLI`/`AMAZON_Q` → `CLI`，`KIRO_AI_EDITOR`/`KIRO_IDE`/`""` → `AI_EDITOR`（`internal/pkg/kiro/translator.go:1834-1843`）
- 但唯一的真实调用点把它**写死**了：
  `kiro_runtime.go:652`：`BuildKiroPayloadWithContext(anthropicBody, modelID, profileArn, "AI_EDITOR", headers)`
- 而本仓库是**区分端点**的：`group.go:136-137` 明确注释 `"q"` = AWS Q、`"krs"` = Kiro Runtime Service（`runtime.us-east-1.kiro.dev`，独立限流池）

参考实现在 CLI 端点上用的是 `origin: "KIRO_CLI"`（`2ue_kiro.rs:src/kiro/endpoint/cli.rs:132`），且专门写了改写逻辑确保它被替换。

> ⚠️ **这不构成「已确证的 bug」**：我没有验证 KRS 端点收到 `AI_EDITOR` 会不会被拒、会不会被风控计入异常。可能上游根本不校验。
> 需要一次真实上游请求来判定。若上游校验，则表现为 KRS 账号的隐性失败或风控标记。

---

## 五、GitHub 生态调研

### 5.0 本节的证据强度说明（先看这个）

下面的**提交历史类数据全部是本地实测**：10 个仓库都完整克隆到了 `/tmp`（带 `.git`），
所以首末提交日期、提交数、分叉点、共同提交数都是 `git` 直接算出来的，可复现。

但有两项**没有拿到**，不要当成已知：

- ~~**star 数**~~：**第二轮已补齐**，见 5.5。第一轮网络受限时缺失，现已通过 GitHub API 取到。
- **各仓库在 GitHub 上声明的 fork 关系**：改用更硬的判据——**共同提交 hash**。
  两个仓库存在共同提交 hash，就是同源的确证（比 GitHub 的 fork 标记更可靠，因为它能识别
  「未声明 fork 但实际同源」和「声明 fork 但已完全重写」两种情况）。

### 5.1 仓库清单（提交历史均为本地实测）

| 仓库 | 语言 | 首次提交 | 最近提交 | 提交数 | 代码文件 | 活跃度 |
|---|---|---|---|---|---|---|
| **ZyphrZero/kiro.rs** | Rust | 2026-02-21 | **2026-09-07** | 324 | 104 | **最活跃的 Rust 实现** |
| hank9999/kiro.rs | Rust | 2026-01-02 | 2026-07-27 | 200 | 61 | 原始仓库，已落后 |
| **TsinHzl/kiro2cc-proxy** | 多语言 | 2026-08-25 | **2026-09-12** | 80 | 117 | 很新（仅 3 周），高频 |
| **justlovemaki/AIClient2API** | JS | 2026-06-10 | **2026-09-11** | 78 | 136 | 活跃，多provider聚合 |
| **aceaura/KiroaaS** | Python+TS | 2026-05-29 | **2026-09-10** | 51 | 112 | **活跃分叉** |
| rchdg/kiro-gateway-cli | JS | 2026-08-31 | 2026-09-03 | 3 | 37 | 提交极少，近乎一次性发布 |
| Quorinex/Kiro-Go | Go | 2026-05-15 | 2026-07-29 | 50 | 63 | 已停更 |
| hnewcity/KiroaaS | Python | 2026-02-23 | 2026-06-24 | 51 | 106 | **已被 aceaura 取代** |
| jwadow/kiro-gateway | Python | 2026-04-16 | 2026-05-18 | 50 | 68 | 已停更，但是多个实现的**共同祖先** |
| jianweidai/KiroGate | Python+TS | 2025-12-13 | 2026-02-15 | 91 | 41 | 最早，已停更 |

（本地二开 `2ue_kiro.rs`：最近提交 2026-09-11，见 1.1）

### 5.2 血缘关系（用共同提交 hash 确证）

**结论一：`ZyphrZero/kiro.rs` 是 `hank9999/kiro.rs` 的分叉，且已大幅超越原仓库。**

这直接回答了用户「二开仓库可能比原仓库更完善」的判断——**成立**。

证据（均为本地 `git` 实测）：

- 两仓库有 **59 个共同提交 hash** → 同源确证。
- `ZyphrZero` 的 root commit `58a0fc0` 的**作者就是 `hank9999`**（`bump: v2026.2.6`）
  → 它是浅克隆/截断历史的分叉，不是独立重写。
- **分叉点**：`f1bbe9f`（2026-05-13，`chore(build): 更新 Node.js 至 22`）。
- 分叉后：`hank9999` 仅 **3** 个提交，`ZyphrZero` 有 **265** 个提交。

> 也就是说原作者基本停更，分叉方接手了后续开发。**看 Kiro 协议的最新实现应该看 `ZyphrZero`，不是 `hank9999`。**

`ZyphrZero` 分叉后新增的、与本文主题相关的提交：

| 提交 | 日期 | 与本文的关系 |
|---|---|---|
| `feat(cache): 缓存统计如实反映上游真值，补齐链路可观测性与会话粘性路由` | 2026-09-07 | **直接催生了 G4** |
| `fix: 严格对齐 prompt cache 断点与隔离语义` | 2026-09-02 | 对应 3.6 |
| `feat: 对齐 Anthropic prompt cache 计量策略` | 2026-09-02 | 对应 3.6 |
| `fix(kiro): 用量类接口携带真实 profileArn，修复 Enterprise/IdC 403` | 2026-08-25 | 对应 2.5 |
| `fix(kiro): resolve profile ARN for user preferences` | 2026-08-25 | 对应 2.5 |

其中 `feat(cache)` 那一条改了 28 个文件，新增 `src/kiro/session_affinity.rs`（315 行）
并重写 `src/anthropic/cache_metering.rs`（-187 行，大幅简化）——
**把「本地模拟」换成「读上游真值」，代码反而变少了**，这是 G4 值得做的一个侧面理由。

**结论二：`aceaura/KiroaaS` 是 `hnewcity/KiroaaS` 的活跃分叉。**
16 个共同提交 hash；`hnewcity` 停在 2026-06-24，`aceaura` 持续到 2026-09-10。同样是二开胜出。

**结论三：`jwadow/kiro-gateway` 是 Python/JS 系的共同祖先。**
`jianweidai/KiroGate` 与 `rchdg/kiro-gateway-cli` 都带署名注释
（`kiro_gateway/converters.py:4`、`src/parsers.js:11`），但 `rchdg` 是 **JS 转写**而非 fork。

### 5.3 端点：不是"分歧"，而是两套端点族（印证 2.1）

调研中一度出现"实现之间端点矛盾"的疑问，实测各仓库出现过的 host 后，答案是**没有矛盾**：

| host | 归属 |
|---|---|
| `q.{region}.amazonaws.com` | Amazon Q / IDE 族（对应我们的 `"q"`） |
| `runtime.{region}.kiro.dev` | Kiro Runtime Service（对应我们的 `"krs"`） |
| `codewhisperer.{region}.amazonaws.com` | **老的、已被弃用的**写法 |

**一条有实操价值的坑**（`aceaura/KiroaaS:python-backend/kiro/config.py:181`，原注释）：

> `# Fixed in issue #58 - codewhisperer.{region}.amazonaws.com doesn't exist for non-us-east-1 regions`

即 `codewhisperer.{region}` 这个 host **在 us-east-1 以外根本不存在**，
他们因此把两个 host 模板都改成了 `https://runtime.{region}.kiro.dev`（`config.py:182,185`）。

> ✅ **我们没踩这个坑**：本仓库用的是 `q.%s.amazonaws.com`
> （`internal/service/kiro_runtime.go:612`、`kiro_profile_resolver.go:57`、`websearch.go:79`），
> 不是 `codewhisperer.{region}`。
>
> ✅ **KRS 硬编码 us-east-1 也不是缺陷**：`kiro_runtime.go:597-599` 的注释说明
> 「KRS 仅支持 us-east-1 / eu-central-1 两个 region；这里固定走 us-east-1」，
> 且 auto 模式下 Q 失败会切 KRS（`:615-625`）。这是有意识的取舍，不是遗漏。

### 5.4 帧解析：大多数非 Rust 实现是"假解析"

这是本次生态调研最值得记录的一条**反面教材**。

`rchdg/kiro-gateway-cli` 与 `jianweidai/KiroGate` 的 Python 半边**完全不解析 AWS event-stream 帧**：
它们把解码后的文本用 `indexOf`/正则去找 JSON key 前缀，靠「第一个出现的 key」猜事件类型
（`rchdg:src/parsers.js:180-188`、`jianweidai:kiro_gateway/parsers.py:241-254`）。
没有 prelude、没有长度字段、没有 CRC。更糟的是 `parsers.py:274` 用
`chunk.decode('utf-8', errors='ignore')` —— **对二进制帧做有损解码**。

代价是具体的：这种实现**无法区分** `reasoningContentEvent` 和 `assistantResponseEvent`
（两者都长成 `{"content":`），所以只能退化成从正文里刮 `<thinking>` 标签。

而 `aceaura/KiroaaS` 在 2026-09-08 专门修了这个问题：
提交 `fix(parsers): decode AWS Event Stream frame headers for metering and tools`,
改 `python-backend/kiro/parsers.py`（+203 行）并补了 **372 行测试**
（`tests/unit/test_parsers.py` +211、`test_streaming_openai.py` +161）。

> ✅ **对我们的意义：这是一条"我们已经做对了"的确认，不是待办。**
> 本仓库有真正的帧解析器（见 2.2），带长度字段与双 CRC 校验。
> 记录它是为了说明：**Kiro 生态里大量实现在这一层是不可靠的**，
> 因此引用社区实现的行为结论时要先确认它到底有没有真解析——
> 一个靠字符串扫描的实现，它对"上游发了什么事件"的描述不可信。
> 这也是 2.6 那个错误结论的根因：老实现看不见 `tokenUsage`，不代表上游没发。

---

### 5.5 第二轮：扩大到 kiro.rs 家族之外（含真实 star 数）

第一轮只覆盖了 kiro.rs 家族及少量非 Rust 实现，**漏掉了生态里 star 最高的几个**。
第二轮用 GitHub API 按 8 组关键词 × 2 种排序检索后补全。

**Kiro 协议实现按 star 排序（真实 API 数据，2026-09-13）：**

| 仓库 | ★ | 语言 | 最近 push | 备注 |
|---|---|---|---|---|
| `jwadow/kiro-gateway` | **2274** | Python | 2026-05-18 | 第一轮已覆盖；Python/JS 系共同祖先，已停更 |
| `hj01857655/kiro-account-manager` | **1985** | Rust | 2026-09-13 | 账号管理方向，**当日仍在更新** |
| `chaogei/Kiro-account-manager` | 1431 | TypeScript | 2026-06-11 | 账号管理 |
| `Quorinex/Kiro-Go` | **1148** | Go | 2026-07-29 | 第一轮已覆盖 |
| `caidaoli/kiro2api` | **636** | Go | 2026-06-16 | **第一轮漏掉**，Go 系高星实现 |
| `aliom-v/KiroGate` | 421 | Python | 2026-02-15 | = 第一轮的 `jianweidai/KiroGate` |
| `petehsu/KiroProxy` | 390 | Python | 2026-05-11 | 第一轮漏掉 |
| `bestK/kiro2cc` | **361** | Go | 2025-08-20 | **最早的 kiro→claude code 转换器**，是多个实现的思想源头 |
| `ssmDo/CodeFreeMax` | 184 | Shell | 2026-03-22 | 多 IDE 聚合 |
| `kkddytd/claude-api` | 153 | Go | 2026-03-16 | 账号池 + 自动注册 |
| `mydisha/keirouter` | **140** | Go | 2026-09-08 | **本轮最有价值的发现**，见 G2/G5 |
| `ankitcharolia/kiro-gateway` | **70** | Python | 2026-09-11 | 活跃，CHANGELOG 记录了大量 400 修复 |
| `d-kuro/kirocc` | **58** | Go | 2026-09-08 | **G4 的独立第二证据**，真帧解析 + 完整用量 |
| `Colin3191/kiro-proxy` | 23 | JS | 2026-07-31 | — |
| `dwgx/KiroStudio` | 13 | Rust | 2026-09-04 | — |

（另有 `justlovemaki/AIClient2API` ★8776 与 `jlcodes99/cockpit-tools` ★17567，
但它们是**多 provider 聚合/账号管理工具**，Kiro 只是其中一个渠道，协议实现深度不如上表专用实现。）

**本轮新增克隆并实测的 6 个仓库**（`/tmp/kiro-r2/`）：

| 仓库 | 真帧解析？ | 解析 tokenUsage cache？ |
|---|---|---|
| `d-kuro/kirocc` | ✅（5 个文件含 CRC/prelude） | ✅ **完整四字段 + 全链路映射** |
| `mydisha/keirouter` | ✅（3 个文件） | ❌ |
| `caidaoli/kiro2api` | ✅（2 个文件） | ❌ |
| `kkddytd/claude-api` | ✅（2 个文件） | ❌ |
| `bestK/kiro2cc` | ✅（1 个文件） | ❌ |
| `ankitcharolia/kiro-gateway` | ❌（0 命中） | ⚠️ 仅 `acp_client.py` 提及 |

> 📌 两点结论：
> 1. **Go 系实现普遍有真帧解析**（5/5），Python 系普遍没有——这与 5.4 的结论一致，
>    且说明"假解析"问题集中在 Python/JS 生态。
> 2. **只有 `d-kuro/kirocc` 一家把 cache 用量接到了底**。这既加强了 G4（有人这么做且做对了），
>    也解释了为什么这个缺口容易被忽略——**13 个实现里只有 1 个做了**。

---

### 5.6 第三轮：系统枚举 fork 树，定位真正的活跃二开

前两轮都是**按仓库名/关键词检索**，这会系统性漏掉一类目标：**名字不起眼、star 很少，但实际改动量巨大的二开**。
第三轮改用 fork 树枚举 + 分叉增量实测来解决（方法见 §1.2）。

#### 5.6.1 采集结果：3162 → 275 → 272

`[API]` 12 个主要仓库的 fork **完整分页枚举**（39 次请求），去重后总数 **3162**；
`pushed_at >= 2026-08-01` 的活跃 fork **275** 个；`compare` 实测成功 **272** 个（3 个 404）。
其中 **ahead > 0**（真有自己提交）的有 **118** 个。

> 📌 **两个校准数据**：
> 1. 275 个"近期活跃"的 fork 里只有 118 个真有自己的提交，即 **约 57% 的活跃 fork
>    没写一行自己的代码**（只是同步上游或改 README）。
>    → 必须用 `ahead` 筛选，只看 `pushed_at` 噪音极大。
> 2. 3162 个 fork 里只有 118 个（**3.7%**）是近期活跃且有自己改动的二开。
>    → "fork 数"作为生态热度指标基本无效。
>
> ⚠️ 本节数据为**第四轮全量枚举**重算。第三轮曾用 `sort=newest` 每仓库取前 200 个（共 1280 个），
> 事后核对发现覆盖率仅 38% 且有选择偏差（见附录 A.4 第 5 条），已废弃该轮数据。

#### 5.6.2 ahead 排名 Top 20（`[API]` 全量枚举实测，2026-09-13）

| ahead | behind | 二开仓库 | ★ | 语言 | 父仓库 |
|---|---|---|---|---|---|
| **476** | 3 | `2ue/kiro.rs` | 1 | Rust | hank9999/kiro.rs |
| **404** | 1 | `claywong/kiro.rs` | 0 | Rust | hank9999/kiro.rs |
| **399** | 21 | `easayliu/kiro.rs` ⭐ | 1 | Rust | hank9999/kiro.rs |
| **263** | 99 | `jingxiuman/kiro.rs` | 1 | Rust | ZyphrZero/kiro.rs |
| 198 | 3 | `xuhuanhello/kiro.rs` | 0 | Rust | hank9999/kiro.rs |
| 136 | 0 | `Panniantong/kiro.rs` | 0 | Rust | hank9999/kiro.rs |
| **133** | 0 | `zsecducna/Kiro-Go` | 24 | Go | Quorinex/Kiro-Go |
| 106 | 3 | `engcapa/kiro.rs` | 1 | Rust | hank9999/kiro.rs |
| 105 | 8 | `WooDragon/kiro.rs` | 0 | Rust | hank9999/kiro.rs |
| 104 | 2 | `ykn1002/kiro.rs` | 4 | Rust | hank9999/kiro.rs |
| **98** | 14 | `ngh1105/Kiro-Go` | 5 | Go | Quorinex/Kiro-Go |
| 95 | 19 | `walaqi/Kiro-Go` | 0 | Go | Quorinex/Kiro-Go |
| 76 | 1 | `wtfdelphia/kiro.rs` | 1 | Rust | hank9999/kiro.rs |
| 74 | 2 | `qhmhyp/kiro.rs` | 0 | Rust | hank9999/kiro.rs |
| 68 | 237 | `crazyrob425/BlacklistedAIProxy` ⭐ | 21 | JS | justlovemaki/AIClient2API |
| **68** | 0 | `zhujunsan/kiro-gateway` | 0 | Python | jwadow/kiro-gateway |
| 60 | 0 | `Chenfyuan/kiro-gateway` ⭐ | 0 | Python | jwadow/kiro-gateway |
| 54 | 0 | `ShizeLiu/AIClient-2-API` ⭐ | 0 | JS | justlovemaki/AIClient2API |
| 54 | 2 | `iaunz/kiro.rs` | 0 | Rust | hank9999/kiro.rs |
| 51 | 8 | `ZSGWorks/keirouter` | 0 | Go | mydisha/keirouter |

⭐ = 第四轮全量枚举才发现，第三轮采样漏掉（`liuran001/kiro.rs-admin` ★60 与
`Stallion-X`、`ralph-wren`、`bestK/kiro.rs` 等落到 20 名开外）。

**这张表本身就验证了用户的判断，而且比预想更极端**：

1. **star 数与改动量几乎不相关**。`claywong/kiro.rs` ★0，却领先原仓库 **404 个提交**；
   而 ★60 的 `liuran001/kiro.rs-admin` 只领先 50、落后 117。
   → **按 star 检索会系统性错过最有价值的二开**，这正是前两轮的盲区。
2. **原仓库停更后，二开接管开发是普遍现象而非个例**：`hank9999/kiro.rs` 名下有
   **10 个**领先它 45+ 提交的活跃二开。§5.2 发现的 ZyphrZero 只是其中之一。
3. `behind` 值同样有信息量：`jingxiuman`（263/99）、`liuran001`（50/117）已与父仓库**双向大幅分叉**，
   属实质性独立项目；而 `Panniantong`（136/0）、`zhujunsan`（68/0）保持同步，是"跟进型"二开。

#### 5.6.3 本轮克隆并读码的 10 个仓库（`[git]` 实测）

| 仓库 | 语言 | 提交数 | 作者数 | 测试文件 | 历史跨度 |
|---|---|---|---|---|---|
| `claywong/kiro.rs` | Rust | 729 | 43 | 1 | 2025-12-27 → 2026-09-10 |
| `jingxiuman/kiro.rs` | Rust | 617 | 37 | 11 | 2026-01-03 → 2026-08-27 |
| `liuran001/kiro.rs-admin` | Rust | 496 | 36 | 2 | 2025-12-27 → 2026-08-24 |
| `Panniantong/kiro.rs` | Rust | 462 | 23 | **13** | 2025-12-27 → 2026-09-01 |
| `ykn1002/kiro.rs` | Rust | 402 | 23 | 1 | 2025-12-27 → 2026-09-04 |
| `ZSGWorks/keirouter` | Go | 375 | 11 | **155** | 2026-05-31 → **2026-09-13** |
| `freebattle/kiro.rs` | Rust | 346 | 21 | 1 | 2025-12-27 → 2026-09-11 |
| `zhujunsan/kiro-gateway` | Python | 274 | 14 | **48** | 2025-12-13 → 2026-09-05 |
| `zsecducna/Kiro-Go` | Go | 253 | 28 | **69** | 2026-02-04 → 2026-08-04 |
| `ngh1105/Kiro-Go` | Go | 204 | 23 | **47** | 2026-02-04 → 2026-07-28 |

> ⚠️ `[git]` 一处需要说明的不一致：`ngh1105/Kiro-Go` 的 GitHub push 时间是 2026-08-12，
> 但默认分支 `main` 最后提交是 2026-07-28 —— 差异来自**非默认分支**（该仓库有
> `dev`、`feat/anthropic-fidelity`、`feat/dispatch-cache-hardening` 等 8 个远端分支）。
> 本文引用的 `upstream_error.go` / `tool_compression.go` 已 `[git]` 核实**在 `main` 上**
> （引入提交 `8cb1c37`，2026-07-11），引用有效。

**测试文件数是判断二开质量最有效的单一指标**：Go/Python 系的 `ZSGWorks`(155)、`zsecducna`(69)、
`zhujunsan`(48)、`ngh1105`(47) 明显是工程化项目；而多数 Rust 二开只有 1 个测试文件
（Rust 惯例是测试内联在 `src/*.rs` 的 `#[cfg(test)]` 里，此处会低估，
但 `Panniantong` 的 13 个独立测试文件仍是同语言中的异类 —— 它也正是本轮 G5 最强证据的来源）。

#### 5.6.4 本轮对各缺口的增量贡献

| 缺口 | 第三轮新增证据 | 证据变化 |
|---|---|---|
| **G5** | `Panniantong/kiro.rs`（Rust）、`zsecducna`+`ngh1105`（Go，同源算 1 份）各自实现了字符集清洗；`Panniantong` 有 MCP 连字符专项测试 | 单一来源 → **4 个非同源实现**，且有真实回归测试佐证 |
| **G4** | `ykn1002/kiro.rs`、`claywong/kiro.rs`、`ngh1105/Kiro-Go` 三份独立解析 | 2 份 → **5 份非同源实现**；新发现"缺 `uncachedInputTokens` 时的降级公式"（同一处代码，应一并修） |
| **G1** | `ngh1105/Kiro-Go:proxy/upstream_error.go` 的永久错误建模 | 新增"不重试但也不惩罚账号 + 翻译成可行动提示"的完整范式 |
| **新** | `ngh1105/Kiro-Go:proxy/tool_compression.go` 工具定义体积压缩 | 见下方 N1 |

#### 5.6.5 N1（新线索·未定级）工具定义体积压缩：预防式处理 payload 超限

`ngh1105/Kiro-Go:proxy/tool_compression.go:13-35` `[源码]` 实现了一个我们和其他 25 个仓库都没有的东西 ——
在**发送前**对过大的工具定义做两步渐进压缩：

1. 递归简化每个工具的 `inputSchema`，只保留结构骨架（`type`/`required`/`properties` 的 key 与 type），
   剥掉 `description`/`examples`/`enum` 等纯说明性字段；
2. 若仍超阈值，按超出比例截断每个工具的 `description`（UTF-8 安全，至少保留 50 字符）。

它与 G2 的关系被原注释讲得很清楚：

> 与 `upstream_error.go` 的 `isImproperlyFormedRejection` 互补：那边在被上游拒绝后做检测、
> 永久短路本请求；这里在发送前做预防、尽量不让请求被拒。

**为什么这对我们有意义**：§5.5 已确认「30+ 工具定义」是 400 最常见的触发场景，
而 Claude Code 默认就带大量工具定义 + 用户的 MCP 工具。我们目前只有 per-tool 截断，
**没有"所有工具加起来仍然过大"的总量兜底**。

> ⚠️ `[待验证]` **阈值 20KB 没有可靠来源，不要直接采用**。
> 该文件注释称 `20 * 1024` 是"对齐 kiro-rs 的 `TOOL_SIZE_THRESHOLD` 经验值"，但
> `[git]` 实测：在我全部 30 个克隆仓库 + 本地 `2ue_kiro.rs` 中搜索
> `TOOL_SIZE_THRESHOLD` / `MIN_DESCRIPTION_CHARS` / `compress_tools_if_needed`，
> **只有 `ngh1105/Kiro-Go` 这一个文件命中**，kiro.rs 侧查无此物。
> 该注释自己也承认"Kiro 上游真实红线未公开"。
> → **思路可借鉴，数字必须实测**。定级需要先有 V1 的实测结果。

#### 5.6.6 第四轮：全量 fork 枚举补齐采样偏差

第三轮用 `sort=newest` 每仓库只取前 200 个 fork，事后核对发现总覆盖率仅 **38%**
（`justlovemaki/AIClient2API` 低至 14%）。第四轮改为**完整分页枚举**（3162 个，见 §5.6.1），
新增克隆 4 个仓库（`/tmp/kiro-r4/`）：

| 仓库 | ahead | 语言 | 本轮价值 |
|---|---|---|---|
| **`easayliu/kiro.rs`** | **399** | Rust | **G5 最强证据**：带真实线上 400 故障样本与复现测试，并**修正了 V9 的正则** |
| `Chenfyuan/kiro-gateway` | 60 | Python | 有 cache 字段测试，未提供新结论 |
| `d0zingcat/kiro.rs` | 40 | Rust | 未提供新结论 |
| `NevinXuHui/Kiro-Go` | 36 | Go | 工具名清洗，与 Kiro-Go 系同源，不计入独立证据 |

**`easayliu/kiro.rs` 的价值说明**（详见 G5 小节）：它是全语料中**唯一**一个提交标题直接写明
"修复线上 400"、且函数注释里带**实测故障样本**（`$WEB_SEARCH`、`$MUTLI_1.N.1-Read`）的实现。
此前 G5 的全部证据都是防御性代码——"有人写了清洗"不等于"真的被拒过"。这条补上了因果链。

**它同时暴露了本文一个推断偏差**：easayliu 的规则 `^[a-zA-Z0-9_-]+$` **允许连字符**，
而本文此前依据 keirouter 推演的"MCP 服务器名带连字符 → 整账号 400"可能站不住。
核心结论（非法字符会导致整请求被拒）不变且更强，但**触发字符集存疑**，已在 V9 中重新表述。

> 📌 方法论教训，值得单独记下：**"采样 + 按新旧排序"会系统性漏掉高价值目标**。
> `easayliu/kiro.rs` 按 ahead 排名全生态第 3，却因创建时间较早落在采样窗口外。
> 若没有做覆盖率自查，本文会带着一个错误的 V9 正则和一个错误的典型触发形态交付。

## 六、待验证（本文中所有没有实证的部分）

| 编号 | 断言 | 当前依据 | 如何验证 |
|---|---|---|---|
| V1 | Kiro 对超大 body 返回 `400 Improperly formed request`，阈值约 **615KB** | **两个独立实现**印证（Rust `payload_guard.rs:1-6`；Python `aceaura:config.py:612`、`.env.example:377`），但均为社区观测值，非官方文档 | 构造超大请求打真实上游，确认阈值 |
| V2 | KRS 端点要求 `origin=KIRO_CLI` | 参考实现如此实现，未见上游文档 | 用 KRS 账号分别以两种 origin 发请求对比 |
| V3 | machine_id 漂移会触发风控/封号 | **无任何直接证据** | 无法安全验证，建议按「稳定指纹」的保守原则处理 |
| V4 | Kiro 要求 tool_result 紧邻前一条 assistant 的 tool_use | 参考实现的实现方式如此 | 构造非紧邻配对打真实上游 |
| V5 | 历史引用的工具必须在当前 tools 中有定义 | 参考实现注释原文 | 构造缩减 tools 的第二轮请求 |
| V6 | ~~本地二开相对 `hank9999/kiro.rs` 的具体增量~~ | **已解决，见 5.2**：`ZyphrZero` 与 `hank9999` 有 59 个共同提交、分叉点 `f1bbe9f`(2026-05-13)、分叉后 265 vs 3 个提交 | — |
| V7 | Kiro 的**两套端点**（Q / KRS）是否都下发 `metadataEvent.tokenUsage`、字段名是否一致 | **5 个非同源实现**字段名与口径一致（§5.6.4），但**全部为读码，无一为本人抓包** | 打一条真实请求，把 `metadataEvent` 原样打日志（G4 改动前必做） |
| V8 | ~~star 数与社区热度~~ | **已解决，见 5.5**（第二轮通过 GitHub API 取到真实数据） | — |
| V9 | 工具名校验的**确切正则**（注意：非法字符会导致 400 这一**结论本身已不需验证**，有 5 个非同源实现 + easayliu 的真实故障样本） | **三方冲突**：keirouter 称 `^[a-zA-Z][a-zA-Z0-9_]{0,63}$`（连字符非法）；`easayliu/kiro.rs` 称 `^[a-zA-Z0-9_-]+$`（**连字符合法**，附实测故障 `$WEB_SEARCH`/`$MUTLI_1.N.1-Read`）；Kiro-Go 系称需纯 camelCase（同源单一来源）。均无官方文档 | 分别用含 `-`、`.`、`$`、数字开头的工具名打真实上游，定位确切边界。**不阻塞 G5 修复**：取三者交集清洗在任何一种规则下都合法 |
| V10 | 上游在不给 `uncachedInputTokens` 时是否只给 `totalTokens` + cache 字段 | 仅 `ykn1002/kiro.rs:metadata.rs:44-53` 一家写了降级公式 | 与 V7 同一次抓包即可确认，决定是否需要补降级分支 |
| V11 | 工具定义总体积的上游红线（N1 的 20KB 阈值） | **无来源**：该数字自称对齐 kiro-rs，但全语料仅一处命中，kiro.rs 侧查无此物 | 与 V1 同一批实测：逐步加大 tools 体积找拐点 |

---

## 七、建议的后续动作（未执行，待决策）

按性价比排序：

0. **G5 工具名字符集清洗**——**建议最先做**：改动面最小（一个纯函数 + 接到 `mapKiroToolName`），
   但影响面最大（用户挂一个名字带连字符的 MCP 服务器，该账号所有 Kiro 请求就全 400）。
   ⚠️ 必须与现有 `ToolNameMap` 反向映射一起改：清洗会引入重名（`a-b` 与 `a.b` 都变 `a_b`），
   要像 keirouter 那样保证单射（`kiro.go:1030` 的 `uniqueKiroToolName`），否则响应侧还原会串工具。
   并补一个 `mcp__xxx-mcp-server__yyy` 的用例——这是最典型的真实触发形态。
1. **G1 消费 `bad_request_quota`**——改动小、收益直接：把额度型 400 接入既有的故障转移与账号冷却链路。本仓库已有 `markKiroMonthlyRequestCountRateLimited`、`markKiroInvalidModelRateLimited` 两个现成范式可照抄。
   注意 G5 修完后，这类 400 的**真实分布**才看得清——两者有先后依赖关系。
2. **G1 的其余四类**至少接上可观测性（现在只有一条 warn），便于判断线上真实分布，再决定要不要各自特化处理。
3. **G4 采信上游真实 cache 用量**——本次调研新增，且**性价比可能最高**：
   - 只需改 `translator.go:4321-4333` 一处，把那两行"刻意忽略"改成采信，并按
     「缓存读取是总输入的子集」的口径（`metadata.rs:43-47`）处理，同时把计数钳到非负（`:33-35`）。
   - **同一处顺手补两个边界**（第三轮发现，见 §5.6.4）：
     (a) `uncachedInputTokens` 缺失时的降级公式 `total - output - cacheRead - cacheWrite`
     （`ykn1002:metadata.rs:44-53`），否则 `InputTokens` 会静默为 0；
     (b) 缓存计数可能**独立于主计数**下发，不能按"整个 tokenUsage 要么全信要么全不信"实现
     （`d-kuro/kirocc:event_processor.go:65-79`）。
   - **前置条件**：先做 **V7**（打日志确认真实 `metadataEvent` 结构），不要凭本文的二手字段定义直接改。
   - 保留本地模拟作为**兜底**（上游没下发时），不要直接删——即「有真值用真值，没真值才估算」。
   - 附带收益：缓存策略的效果从此可以用上游真值度量，不再是自证。
4. **G2 payload 体积保护**——V1 现在有了两个独立来源的阈值（~615KB，建议留余量取 600KB），
   比初稿的「拍脑袋」有依据了，但仍建议实测确认。注意社区注释提到
   **「30+ 工具定义」是最常见触发场景**，而 Claude Code 默认就带大量工具定义。
   - 同时考虑 **N1 工具定义压缩**（§5.6.5）作为 G2 的"预防侧"：G2 是拒绝后的兜底，N1 是发送前的收敛，两者互补。
   - ⚠️ N1 的 20KB 阈值**无可靠来源**（V11），采纳思路可以，**照抄数字不行**。
5. **G1 补"不惩罚账号健康度"**（§5.6.4）：我们"400 不故障转移"是对的，
   但对"请求自身有问题"的 400，还应确保不因此扣减账号健康度，并把 opaque 的
   `Improperly formed request` 翻译成可行动提示。与第 1、2 项同属 G1，可一并处理。
6. **G3 machine_id 落库**——**优先级最低，甚至可以不做**。经核对，已落库的值在刷新时不会被破坏（见 G3 的三点核对），只有「导入时没带 machine_id」的账号受影响。若要做，参照 Cursor 的 F 项实现（铸造落库 + 刷新只补不换）。注意 Kiro 与 Cursor **共用 `machine_id` 凭据键名**，任何 strip/mint 助手必须按平台分流，否则会互相破坏（这条在 Cursor 改造时已经踩过）。

---

## 附录 A：证据来源总清单

### A.1 已克隆并实际读码的 30 个仓库

全部 `[git]` 本地实测（提交数 = 克隆时 `git rev-list --count HEAD`；第三轮为 `--depth 400` 浅克隆，
提交数为下界）。**"轮次"标明该仓库在哪一轮进入调研**，可据此判断本文哪些结论是后期才修正的。

| # | 仓库 | 语言 | 提交数 | 最近提交 | 轮次 | 本文引用位置 |
|---|---|---|---|---|---|---|
| 1 | `hank9999/kiro.rs` | Rust | 200 | 2026-07-27 | 一 | §5.2 血缘基准 |
| 2 | `ZyphrZero/kiro.rs` | Rust | 324 | 2026-09-07 | 一 | §2.6 §5.2，**G4 首个证据** |
| 3 | `jwadow/kiro-gateway` | Python | 50 | 2026-05-18 | 二 | §5.2 共同祖先 |
| 4 | `hnewcity/KiroaaS` | Python | 51 | 2026-06-24 | 二 | §5.2 |
| 5 | `aceaura/KiroaaS` | Python+TS | 51 | 2026-09-10 | 二 | §5.3 §5.4，**V1 阈值来源之一** |
| 6 | `jianweidai/KiroGate` | Python+TS | 91 | 2026-02-15 | 二 | §5.4 假解析反例 |
| 7 | `rchdg/kiro-gateway-cli` | JS | 3 | 2026-09-03 | 二 | §5.4 假解析反例 |
| 8 | `Quorinex/Kiro-Go` | Go | 50 | 2026-07-29 | 二 | §5.6 camelCase 溯源 |
| 9 | `justlovemaki/AIClient2API` | JS | 78 | 2026-09-11 | 二 | §5.5 |
| 10 | `TsinHzl/kiro2cc-proxy` | 多语言 | 80 | 2026-09-12 | 二 | §5.1 |
| 11 | `mydisha/keirouter` | Go | 184 | 2026-09-08 | 二 | **G5 正则来源**、G2 七条 400 成因 |
| 12 | `d-kuro/kirocc` | Go | 80 | 2026-08-25 | 二 | **G4 第二份独立证据** |
| 13 | `caidaoli/kiro2api` | Go | 80 | 2026-06-16 | 二 | §5.5 |
| 14 | `bestK/kiro2cc` | Go | 19 | 2025-08-20 | 二 | §5.5 |
| 15 | `kkddytd/claude-api` | Go | 25 | 2026-03-16 | 二 | §5.5 |
| 16 | `ankitcharolia/kiro-gateway` | Python | 129 | 2026-09-02 | 二 | §5.5 |
| 17 | `claywong/kiro.rs` | Rust | 729 | 2026-09-10 | 三 | **G4 第三份证据** |
| 18 | `jingxiuman/kiro.rs` | Rust | 617 | 2026-08-27 | 三 | §5.6.2 |
| 19 | `liuran001/kiro.rs-admin` | Rust | 496 | 2026-08-24 | 三 | §5.6.2 |
| 20 | `Panniantong/kiro.rs` | Rust | 462 | 2026-09-01 | 三 | **G5 最强证据**（MCP 连字符测试） |
| 21 | `ykn1002/kiro.rs` | Rust | 402 | 2026-09-04 | 三 | **G4 降级公式**（V10） |
| 22 | `freebattle/kiro.rs` | Rust | 346 | 2026-09-11 | 三 | §5.6.3 |
| 23 | `ZSGWorks/keirouter` | Go | 375 | **2026-09-13** | 三 | §5.6.3（测试最多，155 个） |
| 24 | `zsecducna/Kiro-Go` | Go | 253 | 2026-08-04 | 三 | §5.6 camelCase 分歧 |
| 25 | `ngh1105/Kiro-Go` | Go | 204 | 2026-07-28 | 三 | **G1 范式 + N1 工具压缩** |
| 26 | `zhujunsan/kiro-gateway` | Python | 274 | 2026-09-05 | 三 | §5.6.3 |
| 27 | **`easayliu/kiro.rs`** | Rust | 400 | 2026-08-14 | **四** | **G5 最强证据 + 修正 V9** |
| 28 | `d0zingcat/kiro.rs` | Rust | 364 | 2026-09-08 | 四 | §5.6.6 |
| 29 | `Chenfyuan/kiro-gateway` | Python | 266 | 2026-09-11 | 四 | §5.6.6 |
| 30 | `NevinXuHui/Kiro-Go` | Go | 156 | 2026-08-04 | 四 | §5.6.6（与 Kiro-Go 系同源） |

另有本地二开 `~/Desktop/procode/2ue_kiro.rs`（Rust，2026-09-11），见 §1.1 —— 它是 **G1 参考范式**的来源。

### A.2 只取了元数据、未克隆的仓库

`[API]` 仅有 GitHub 元数据（star / push 时间 / fork 关系），**没有读过代码**，
因此本文不基于它们做任何协议层结论：

- §5.5 star 榜中未克隆的：`hj01857655/kiro-account-manager`(★1985)、`chaogei/Kiro-account-manager`(★1431)、
  `petehsu/KiroProxy`(★390)、`ssmDo/CodeFreeMax`(★184)、`Colin3191/kiro-proxy`(★23)、`dwgx/KiroStudio`(★13)
  —— 前两个是账号管理方向，协议实现深度不足。
- §5.6 的 3162 个 fork 中未进入 top 20 的其余仓库（含 118 个"有自己提交"的二开里未克隆的 104 个）。

### A.3 无法核实的来源

| 来源 | 出现位置 | 状态 |
|---|---|---|
| `kiro-tutu` | `ngh1105/Kiro-Go` 多个文件注释标注 `Ported from kiro-tutu` | `[API]` GitHub 搜索 **0 结果**，已删除/改名/私有，**无法回溯** |
| kiro.rs 的 `TOOL_SIZE_THRESHOLD` | `ngh1105:tool_compression.go:30` 自称对齐 | `[git]` 全语料（30 仓库 + 本地二开）搜索**无命中**，**疑似误标** |
| CodeWhisperer 工具名正则 | keirouter / Panniantong 注释 | 无官方文档，见 V9 |
| ~615KB payload 阈值 | Rust `payload_guard.rs` + Python `aceaura:config.py` | 两个独立社区观测值，非官方，见 V1 |

### A.4 本文没有做的事（避免读者高估结论强度）

1. **没有对 Kiro 上游做过任何抓包或真实请求**。所有关于"上游会/不会下发什么"的结论，
   证据链最强也只到"多个非同源实现一致这么写"。V1/V2/V4/V5/V7/V9/V10/V11 全部属于此类。
2. **没有改动任何代码**，第七节的所有条目均未执行。
3. **没有阅读 A.2 中未克隆仓库的代码**。
4. 第三、四轮的 14 个仓库为 `--depth 400` 浅克隆，**提交数是下界**，且更早的历史未纳入分析。
5. **fork 枚举是抽样，不是全量 —— 这是本文最大的采集局限**。
   实现上每仓库只取 2 页（`per_page=100 × 2` = 200 个），且 `sort=newest` 意味着**只覆盖最新创建的 200 个**。
   `[API]` 实测各仓库真实 fork 总数与实际采集数：

   | 仓库 | 真实 fork 数 | 本文采集 | 覆盖率 |
   |---|---|---|---|
   | `justlovemaki/AIClient2API` | 1380 | 200 | **14%** |
   | `jwadow/kiro-gateway` | 547 | 200 | **37%** |
   | `hank9999/kiro.rs` | 495 | 200 | **40%** |
   | `Quorinex/Kiro-Go` | 359 | 200 | **56%** |
   | `ZyphrZero/kiro.rs` | 133 | 123 | 92% |
   | `mydisha/keirouter` | 53 | 52 | 98% |
   | `d-kuro/kirocc` | 22 | 20 | 91% |
   | `hnewcity/KiroaaS` | 28 | 28 | 100% |

   **✅ 已补全（第四轮）**：发现该偏差后已对 12 个主仓库做**完整分页枚举**（39 次请求），
   `[API]` 实测去重后 fork 总数 **3162**，活跃（`pushed_at >= 2026-08-01`）**275** 个，
   `compare` 成功 **272** 个。§5.6.2 的表格与 §5.6.1 的比例**已基于全量数据重算**。
   补全后新发现 4 个有实质改动的二开（见 §5.6.6），其中 `easayliu/kiro.rs`（ahead=399）
   若按原采样会**被完全漏掉**，而它恰恰提供了 G5 目前最强的证据。
   → 这本身就是一条教训：**fork 采样必须全量，`sort=newest` 的前 200 个有严重选择偏差**。

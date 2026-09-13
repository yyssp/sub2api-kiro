# Cursor 上游协议加固 — 改造记录

**执行日期**：2026-09-13
**分支**：`fix/cursor-protocol-audit` → 合并至 `2ue`
**依据**：`docs/cursor-integration-plan/08-protocol-audit.md`（协议审计，8 个 GitHub 仓库横向比对 + 本地实测）
**范围**：`backend/internal/pkg/cursor/`、`backend/internal/service/cursor_*.go`

> 本文记录**做了什么、验证到什么程度、什么还没验证**。
> 「已验证」一律指有可复现的自动化用例或实测输出；只做过代码走查的一律记在未验证一侧。

---

## 一、改造清单

六项改动，分三次提交。前五项来自审计文档的 A–E，第六项是执行过程中实测发现的残留缺口。

| # | 改动 | 严重度 | 提交 |
|---|---|---|---|
| A | gzip 解压加上限（256MB），触顶报错 | 高 | `56b6e5d69` |
| C | tool-call ID 取首行 + 剔除控制字符 | 低 | `56b6e5d69` |
| D | 附件加 32MB 共用总量预算 | 低 | `56b6e5d69` |
| B | `machine_id` 建号铸造并落库，脱离 token 派生 | 中 | `d32e893e9` |
| E | checksum 位移改为 JS 语义（对 32 取模） | 中 | `d32e893e9` |
| F | 复制账号时重铸设备指纹，不沿用源账号 | 中 | `18b95ff5b` |

### A. gzip 解压无上限（`client.go`）

`ReadFrame` 的 50MB 闸门只卡**压缩后**的帧长，解压走 `io.ReadAll` 完全无界。

改为 `io.ReadAll(io.LimitReader(zr, maxDecompressedFrameSize+1))`，超限**报错而非截断**——
半截 protobuf 交给解析器只会变成难以定位的解析错误，掩盖真正的原因。
顺带把原先写死的 `50*1024*1024` 提成具名常量 `maxFrameSize`，与解压上限成对出现。

### C. tool-call ID 未净化（`agent.go`）

新增 `sanitizeToolCallID`：只取第一个非空行，剔除控制字符。

该 ID 会经 SSE 下发、在多轮对话中作为 `tool_use_id` 往返配对，
还会被拼进 `attachments.go` 的 `"[调用工具 %s(id=%s)]"` 文本行——内嵌换行会破坏该标记行。

### D. 附件无总量上限（`attachments.go`）

`maxInlineAttachmentBytes`（每个 16MB）挡不住 N 个各 15MB 的合法附件叠加。
在 `collectChatAttachments` 加 32MB 预算。三个设计要点：

- **图片与文档共用一份额度**：分开计算等于把实际上限翻倍。
- **文档正文计入**：正文会被拼进用户文本发往上游，同样占请求体；只算二进制 `Data` 会漏掉文档这整条路径。
- **超预算整个跳过而非截断**：截断后的图片/文档是损坏数据。

> ⚠️ 落点与审计文档的建议不同。文档指向 `attachments.go:284/898`，但那两处是**按消息**聚合的，
> 在那里设上限只能约束单条消息，而请求体是所有消息之和。实际落在 `collectChatAttachments`
> （唯一汇聚点，`CollectCurrentTurnAttachments` 亦改为走它）。

### B. 设备指纹随 token 刷新漂移（`crypto.go` + service 层）

原实现在账号无 `machine_id` 时退化成 `sha256("CursorAPI/"+AccessToken)`，token 一刷新指纹就变。
而纯文本导入（最常见的导入方式）从不带 `machine_id`，所以**这条退化路径是常态而非边缘情况**。

改为「建号时铸造一次并落库，此后恒定」：

| 位置 | 行为 |
|---|---|
| `cursor.NewMachineID()` | 铸造 32 字节随机 hex（64 字符），形态同真实客户端 |
| `prepareCursorMachineIDForCreate` | 建号钩子，只对 cursor 平台生效 |
| `EnsureMachineID` | 幂等补铸；刷新链路用它给存量账号补齐 |
| `BuildAccountCredentials` | **刻意不铸造**（见下方陷阱） |
| `machineID()` 兜底 | 改为按账号 ID 派生，只服务存量账号过渡期，不含任何随 token 变化的输入 |
| `macMachineID()` | 一并脱离 token 派生，改从 `machineID(a)` 派生 |

> ⚠️ **`BuildAccountCredentials` 不能铸造 machine_id**。它在**刷新**路径上也会被调用，
> 而 `MergeCredentials` 是「新值覆盖旧值」——无条件铸造等于每次刷新都换一个设备指纹，
> **比修复前更糟**。这是本次最容易被后人「顺手补全」改错的地方，已有专门的反向用例守着。

### E. checksum 位移语义（`crypto.go`）

真实客户端是 JS，位运算在 int32 上做、位移数对 32 取模：`C>>40` 实为 `C>>8`、`C>>32` 实为 `C>>0`。
原实现用 Go 真 64 位移位直译，因 `ts`（毫秒/1e6）只有约 21 位，**前两字节恒为 `0,0`**——
一个稳定可识别的非官方客户端特征。前两字节改为 `ts>>8` / `ts`。

> B 与 E 合并为一次提交：两者都改变 checksum 输出，分两次做等于让每个账号的指纹连续变动两次，
> 正是要消除的模式。

### F. 复制账号沿用源指纹（`admin_account.go`，执行中发现）

**不在原审计文档里**，是写本记录时实测发现的。`DuplicateAccount` 整份克隆 `credentials`，
`machine_id` 也跟着复制，两个账号因而对上游出示**同一设备指纹**。

`EnsureMachineID` 是幂等的，分不出「本来就有」和「从源账号抄来的」，
因此在 `DuplicateAccount` 的克隆点加 `stripCursorMachineID` 先剥离，再由建号钩子重铸。

与 Codex 指纹种子的处理一致（`prepareCodexFingerprintExtraForCreate` 同样先
`stripCodexFingerprintSeed` 再铸造），只是落点不同：Cursor 的在 `credentials`，Codex 的在 `extra`。

> ⚠️ 剥离**只能挂在复制路径**，不能挂进建号钩子：导入文件带的 `machine_id` 是账号已有的
> 设备身份，必须沿用。两条路径都汇进 `buildAccountForCreate`，在那里分不开。
> 另外 `stripCursorMachineID` 必须按平台短路——**Kiro 也用 `machine_id` 这个键名**，
> 不加平台判断会误伤（已有反向用例）。

---

## 二、已验证

### 验证方法

每一项都按同一套流程执行，不是「写完跑一遍绿灯」：

1. **先写复现用例**，确认在改动前**失败**（证明问题真实存在、用例确实测到了它）；
2. 实现修复；
3. **变异还原**（把修复改回原样或削弱），确认对应用例**失败**（证明护栏真的在守）；
4. 每项都配**反向用例**，防止「上限设过低」「无条件丢弃」这类假修复蒙混过关。

### 实测复现记录（改动前）

| 项 | 实测输出 |
|---|---|
| A | 压缩后 205606 字节 → 解压 104857600 字节，**未报错**（膨胀比 510:1） |
| C | 解析出 `call.ID = "toolu_aaa\ntoolu_bbb"`，含换行 |
| D | 15 张各 15MB → 235929600 字节全部通过 |
| B | 无 machine_id 时，刷新前 `835d70a4…` → 刷新后 `cddef109…`，指纹漂移 |
| E | ours `[0 0 0 27 77 109]` vs js `[77 109 0 27 77 109]` |
| F | 源 `aaaa1111…` → 副本 `aaaa1111…`，完全相同 |

### 变异验证记录（改动后）

7 处变异**全部产生断言失败**（而非编译失败——编译失败是更弱的信号）：

| 变异 | 失败用例与输出 |
|---|---|
| 还原无界 `io.ReadAll(zr)` | 559247 字节 → 285212672 字节放行 |
| 移除 `sanitizeToolCallID` 调用 | ID 仍含换行；空白/制表符/NUL 全部未净化 |
| 预算 `take` 永不拒绝 | 235929600 字节放行；共用预算与文档正文两条也同时失败 |
| 还原真 64 位移位 | 前两字节 `[0 0 …]`，检出非 JS 语义 |
| 还原从 access_token 派生 | 检出指纹仍随 token 漂移 |
| `BuildAccountCredentials` 无条件铸造 | 检出刷新路径会覆盖已落库指纹 |
| `EnsureMachineID` 去掉幂等 | 导入值被覆盖、连续调用值变化、刷新换指纹，三条同时失败 |
| 复制路径不剥离 | 副本沿用源指纹 |

### 新增用例（24 个）

| 文件 | 用例数 | 覆盖 |
|---|---|---|
| `cursor/client_decompress_limit_test.go` | 2 | 炸弹拦截、正常压缩帧不受影响（含 flag 位域 `0x03`） |
| `cursor/agent_toolcall_id_test.go` | 4 | 换行拼接、控制字符/CRLF/首行为空、正常 ID 不被误伤、全空输入 |
| `cursor/attachments_total_budget_test.go` | 4 | 总量拦截、预算内不丢弃、图文共用额度、文档正文计入 |
| `cursor/crypto_fingerprint_test.go` | 5 | 指纹稳定性、铸造形态/随机性、JS 位移语义（反解 checksum 时间戳段比对）、machineId 后缀拼接 |
| `service/cursor_machine_id_persistence_test.go` | 9 | 建号铸造、导入值保留、平台隔离、幂等、空白视同缺失、**刷新不得重铸**、复制重铸、不误伤 Kiro |

### 测试结果

- `go build ./...` 通过
- `go test -tags unit ./...` **全量通过**（含 `internal/service` 193s 的完整套件），无回归

---

## 三、未验证 / 已知缺口

**这一节比上一节重要**：以下内容没有被任何自动化用例覆盖，不要当作已经确认可用。

### 1. 真实 Cursor 上游 — 完全未验证 ⚠️

**B、E、F 都改变了 `x-cursor-checksum` 的实际输出**，而全部验证都是本地单元测试。
从未对真实 Cursor 上游发过一个请求。

具体未知：

- 上游是否接受新的 checksum（当下不强校验其内容，但没有实测确认过新值被接受）；
- 存量账号补铸 `machine_id` 后，上游是否将其视为「换设备」并触发额外风控；
- 新的位移语义是否真与官方客户端逐字节一致——我们只验证了「不再是恒为 0 的错误形态」，
  以及后四字节与前两字节的自洽关系，**没有与真实客户端抓包比对过**。

> 这对应实施计划里尚未执行的「阶段 6 真实上游验证」。上线前应在该阶段实测一次。

### 2. 「指纹漂移 → 封号」的因果关系 — 未证实

审计阶段就标注过，这里重申：**没有证据证明**指纹漂移会导致封号。
现有证据是间接的（proto 里存在 `ERROR_SUSPICIOUS_USAGE_BLOCKED = 54`，
以及有仓库注释称后端「期望」设备稳定）。

修复的正当性**不依赖**这条推断——原行为（指纹随 token 漂移、副本共用指纹）
在任何模型下都不是期望行为。**不要把这条推断转述成结论。**

### 3. 阈值取值缺乏上游依据

三个新增常量都是工程判断，**不是**来自上游文档或实测的官方限制：

| 常量 | 取值 | 依据 |
|---|---|---|
| `maxDecompressedFrameSize` | 256MB | 对齐 `D3Dream/cursor2api` 的同名常量 |
| `maxTotalAttachmentBytes` | 32MB | 单附件上限 16MB 的 2 倍，无上游依据 |

若真实业务出现合法的大附件请求，32MB 可能偏紧。目前**没有生产流量数据**支撑这个取值。

### 4. 存量账号的补铸尚未在真实数据上跑过

补铸逻辑挂在**刷新链路**上，意味着存量账号要等到下一次 token 刷新才会补齐 `machine_id`。
在那之前，它们走 `machineID()` 的过渡兜底（按账号 ID 派生）。

- 未验证：真实库里有多少 cursor 账号处于「无 machine_id」状态；
- 未验证：补铸后的落库写入在真实 DB 上的行为（单元测试只覆盖了 map 层的合并语义）；
- 未做：一次性批量补铸的 migration。目前是**惰性补齐**，不是主动迁移。

### 5. 前端与管理端未改动

本次只动后端。未验证管理台「账号详情」是否会展示/允许编辑 `machine_id`
（若允许手工编辑，用户可能无意中造成指纹漂移）。

### 6. 未纳入本次范围

审计文档第三节列出的、判定为不适用的调研发现（Tab/Cpp host 分片表、hex 套 protobuf 的
`BidiAppend`、假 SSE Content-Type、`is_retryable` 字段、第三方中继），本次**均未处理**，
理由见原文档，结论未变。

---

## 四、回归风险提示

给后续维护者：

1. **`BuildAccountCredentials` 不要加 machine_id 铸造**——理由见 B 项的陷阱说明。
2. **`stripCursorMachineID` 不要去掉平台判断**——Kiro 也用 `machine_id` 这个键名。
3. **改 `genChecksum` 的位移必须整体改**——前两字节与后四字节存在自洽关系，用例会反解比对。
4. **这些改动一旦回退，不会有任何报错**：请求照常跑通，上游当下不强校验 checksum 内容，
   附件与解压上限也只在异常输入下才触发。只能靠用例守，不能靠观察线上现象发现。

---

## 附：提交与文件

```
18b95ff5b  fix(cursor): 复制账号时重铸设备指纹，不沿用源账号
d32e893e9  fix(cursor): 设备指纹落库 + checksum 位移语义对齐真实客户端
56b6e5d69  fix(cursor): 协议层加固——解压上限、tool-call ID 净化、附件总量预算
```

| 文件 | 性质 |
|---|---|
| `internal/pkg/cursor/client.go` | A：解压上限 + 帧长常量具名 |
| `internal/pkg/cursor/agent.go` | C：`sanitizeToolCallID` |
| `internal/pkg/cursor/attachments.go` | D：`collectChatAttachments` + 共用预算 |
| `internal/pkg/cursor/crypto.go` | B/E：`NewMachineID`、`machineID`/`macMachineID` 脱离 token、位移语义 |
| `internal/service/cursor_oauth_service.go` | B/F：建号钩子、`EnsureMachineID`、`stripCursorMachineID` |
| `internal/service/cursor_token_refresher.go` | B：刷新时幂等补铸 |
| `internal/service/admin_account.go` | B/F：建号钩子接入、复制路径剥离（各 1–5 行） |

后端改动 3 个既有 cursor 文件 + 2 处既有共享文件的最小钩子，符合旁挂式接入范式。

# Kiro 协议调研与加固 · 索引

> 按问题分档（`findings/`）+ 按仓库分档（`repos/`）+ 流程文档。
> **入口建议**：先看 [PLAN-AND-STATUS](PLAN-AND-STATUS.md) 了解全局状态，
> 再看 [reform-plan](reform-plan.md) 看要改什么。

---

## 一、流程文档（按阅读顺序）

| 文档 | 作用 |
|---|---|
| [PLAN-AND-STATUS.md](PLAN-AND-STATUS.md) | 🧭 **状态唯一真相源**。总计划、任务看板、我对需求的理解、安全红线 |
| [reform-plan.md](reform-plan.md) | 🔧 **核心交付物**。5 个改造项，每项六要素齐全 |
| [test-strategy.md](test-strategy.md) | 🧪 A/B/C 三层测试分层，决定哪些才配消耗真实额度 |
| `progress.md` | ⏳ 待生成：逐项改造进度 |
| `import-report.md` | ⏳ 待生成：235 账号导入结果 |
| `test-results.md` | ⏳ 待生成：真实测试记录 + **存疑清单** |

---

## 二、按问题分档 `findings/`

| 文档 | 主题 | 证据等级 | 结论 |
|---|---|---|---|
| [F01](findings/F01-official-aws-service-model.md) | **AWS 官方服务模型** | 🏆 `[官方]` | 工具名 `[a-zA-Z0-9_-]+` max 64；Origin 枚举；`tokenUsage` 不在公开 SDK |
| [F04](findings/F04-quota-exhaustion-and-failover.md) | 额度耗尽与调度 | `[本仓库]` | 402 处理链**已完整**；残余风险需实测 |
| [F05](findings/F05-400-two-error-strings-and-schema.md) | 400 的真正成因 | `[源码]`+实测 | **同根因两条错误串**；成因不止工具名 |
| [F06](findings/F06-schema-passthrough-gap.md) | 🔴 **G6 schema 透传** | **实测** | **7 个触发器命中 7 个**，本轮最高优先级 |
| [F07](findings/F07-gap-status-verified.md) | 缺口逐条核验 | `[本仓库]` | 排除已实现项；记录 3 处自我修正 |

### 缺口状态速查

| 编号 | 缺口 | 状态 | 优先级 |
|---|---|---|---|
| **G6** | 工具 schema 超纲关键字透传 | 🔴 实测确证 | **P0** |
| **G8** | 归类器漏一条 400 特征串 | 🔴 实测确证 | **P0**（2 行） |
| **G5** | 工具名无字符集清洗 | 🟠 确证 | P1 |
| **G2** | 无请求体积守卫 | 🟠 确证 | P2（难验证） |
| **G4** | 丢弃上游 cache token | 🟡 口径未定 | P3（阻塞） |
| ~~G1~~ | 400 不故障转移 | 🟢 范围收窄→并入 G8 | — |
| ~~G7~~ | 孤儿 toolUse/toolResult | ✅ **已实现** | — |
| ~~D1~~ | Origin 枚举 | ✅ **已正确** | — |

---

## 三、按仓库分档 `repos/`

| 文档 | 仓库 | 价值 |
|---|---|---|
| [2ue-kiro-rs.md](repos/2ue-kiro-rs.md) | **本地二开**（232,577 行） | 🏆 **工程质量最高**；G2 实施蓝本；⚠️ 含需你决策的敏感项 |

### 关键外部仓库（结论已并入 findings）

| 仓库 | 语言 | ★ | 价值 |
|---|---|---|---|
| `aws/aws-toolkit-vscode` | TS | 1997 | 🏆 **官方权威**，见 [F01](findings/F01-official-aws-service-model.md) |
| `funny-vibes/agent-vibes` | TS | 359 | schema 白名单 + **双错误串**，见 [F05](findings/F05-400-two-error-strings-and-schema.md) |
| `AbdoKnbGit/tau` | TS | 315 | 工具名范本 + 体积双阈值 |
| `easayliu/kiro.rs` | Rust | — | 🔥 **唯一带真实线上故障样本** |
| `fawney19/Aether` | Rust | 1466 | 历史工具占位符（独有） |
| `awsl-project/maxx` | Go | 61 | 反面教材：有清洗器但没用在 kiro 上 |

> 前置生态调研（30 仓库 + 全量 fork 枚举）见
> [../kiro-protocol-ecosystem-analysis.md](../kiro-protocol-ecosystem-analysis.md)。
> **本目录是它的升级版**——修正了其中若干错误结论，见 [F07 §四](findings/F07-gap-status-verified.md)。

---

## 四、证据标记法

| 标记 | 含义 | 可信度 |
|---|---|---|
| `[官方]` | AWS 官方仓库/服务模型 | 🏆 最高 |
| `[本仓库]` | 读 sub2api 源码或**实测**得出 | 高 |
| `[源码]` | 读克隆的社区仓库得出 | 中高 |
| `[git]` | 本地 git 实测 | 高 |
| `[API]` | GitHub API 元数据 | 中 |
| `[推断]` | 我的推理，非直接证据 | 低 |
| `[待验证]` | 单一来源或未证实 | 低 |

**三条纪律**：
1. "某实现做了 X" ≠ "上游要求 X"
2. 同源代码只算一条证据（用 `git log -S` 溯源）
3. 假帧解析实现的行为性结论**不可引用**

---

## 五、三条调研教训（可复用）

1. **按 star 采样会漏** —— ★0 的 `claywong/kiro.rs` 领先原仓库 404 提交
2. **`sort=newest` 取前 200 会漏** —— 3162 个 fork 覆盖率仅 38%，
   漏掉的 `easayliu/kiro.rs` 恰是唯一带真实故障样本的证据源
3. **按仓库名检索会漏掉一整类** —— 网关型项目把 Kiro 藏在
   `internal/adapter/provider/kiro/` 下，仓库名不含 kiro；
   **AWS 官方仓库也不含 kiro**
   → **协议调研必须用协议特征词做代码级检索**

---

## 六、安全红线

- 凭据文件 `kiro-credentials-*.json` 已被 `.gitignore:174` 覆盖
- 凭据**只能**进 DB `accounts.credentials` JSONB（走现有加密/脱敏）
- 每次提交前核对暂存区无凭据文件
- 调研克隆一律只在 `/tmp`，**不执行**任何克隆仓库的脚本/二进制

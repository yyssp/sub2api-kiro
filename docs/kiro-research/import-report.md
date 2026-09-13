# 账号导入报告

**执行时间**：2026-09-14
**源文件**：`kiro-credentials-2026-09-13T14-58-14-336Z.json`（仓库根目录，已被 `.gitignore:174` 覆盖）
**目标库**：`sub2api_cache_smoke` @ `kiro-rs-postgres-local:25432`（复用已运行容器）

---

## 一、结论

| 指标 | 数值 |
|---|---|
| 源文件条目 | 235 |
| 解析成功 | **235** |
| 解析跳过 | **0** |
| 账号创建成功 | **235** |
| 账号创建失败 | **0** |

> ✅ **零跳过、零失败**。原预期「api_key 账号缺 refreshToken 会被拒」**未发生** ——
> 见 §四 的两处预期修正。

---

## 二、清空前的状态

| 项 | 数值 |
|---|---|
| 表内总行数 | 619 |
| 存活账号（`deleted_at is null`） | **171** |
| 历史软删除 | 448 |

**清空方式**：走真实管理端接口 `POST /admin/accounts/batch-delete`
（先分页 `GET /admin/accounts` 收集 ID）。
结果：`success=171, failed=0`。

⚠️ 删除前已备份 `accounts` 表数据到 `/tmp/sub2api-run/accounts_backup_*.sql`
（**仓库外**，含明文凭据，不入 git）。

---

## 三、导入路径（刻意走真实链路）

用户要求「正好测试下添加账号的 kiro 导入能力」，因此**没有直接写库**，
而是完整复刻前端 `CreateAccountModal.handleKiroImport` 的两步：

**第一步 · 解析**：`POST /admin/kiro/oauth/import-kiro-rs`（宽松解析路径）
→ 235 条目全部解析成功，`skipped=[]`

**第二步 · 建号**：`POST /admin/accounts/batch`（每批 50，共 5 批）
凭据映射严格复刻前端：
- `oauth` 类 → `kiroOAuth.buildCredentials`（19 个字段）
- `apikey` 类 → `kiroOAuth.buildImportedAPIKeyCredentials`
- 名称 → `buildKiroEntryName`（优先 email，回退 `kiro-<authMethod>-<token 后 8 位>`）
- `endpoint` → 存为 `kiro_rs_endpoint` 来源元数据

---

## 四、两处预期修正（我原先判断错了）

### 修正 1：`api_key` 账号不会被拒

**原判断**：15 个 `authMethod=api_key` 的条目没有 `refreshToken`/`profileArn`，
预期会在建号时失败，属于「导入有报错的直接跳过」的那一类。

**实测**：15 个**全部导入成功**，且凭据完整。

**原因**：源文件里 API-key 账号的字段名是 **`kiroApiKey`** 而不是 `apiKey`，
我最初按 `apiKey` 统计所以看成了空。宽松解析器正确识别了 `kiroApiKey`，
把它们映射成 `type=apikey` 的账号 —— 这类账号**本来就不需要** refreshToken。

**核验**：DB 中 15 个 apikey 账号 `api_key` 字段全部非空（`empty_key=0, real_key=15`）。

### 修正 2：「221 个唯一 email」不代表有 14 条重复账号

**原判断**：235 条里只有 221 个唯一 email，怀疑有重复。

**实测**：重复的是 **`None`（15 个）** —— 正是那 15 个 API-key 账号，
它们本来就没有 email 字段。去掉后 220 个 OAuth 账号的 email **两两不同**。
不存在真正的重复账号。

---

## 五、落库核验

```
 platform |  type  | status | schedulable | count
----------+--------+--------+-------------+-------
 kiro     | apikey | active | t           |    15
 kiro     | oauth  | active | t           |   220
```

凭据完整性：

```
  type  | total | has_refresh | has_arn | has_apikey | has_machine
--------+-------+-------------+---------+------------+-------------
 apikey |    15 |           0 |       0 |         15 |          15
 oauth  |   220 |         220 |     214 |          0 |         220
```

- `apikey` 账号无 refresh_token/profile_arn —— **符合预期**，这类账号不需要
- `oauth` 账号 220/220 有 refresh_token；**214/220 有 profile_arn**
  （6 个缺 ARN，全部来自 `authMethod=idc`，需运行时动态获取）
- `machine_id` 235/235 全覆盖 —— 设备指纹已落库

---

## 六、账号构成

| 维度 | 分布 |
|---|---|
| `authMethod` | social 210 / api_key 15 / idc 10 |
| `subscriptionTitle` | **KIRO FREE ×235**（全部免费档） |
| `apiRegion` | us-east-1 ×212 / 未设置 ×23 |
| `disabled` | false ×235（源文件里无禁用账号） |

> ⚠️ **全部是 KIRO FREE 档**。这直接决定了后续真实上游测试的边界：
> 只能测 haiku/sonnet，不能测 opus，上下文 200k。

---

## 七、遗留风险

1. **额度状态未知** —— 用户说明「很多额度耗尽了」，但导入阶段**不探测额度**，
   所有账号 `status=active`。哪些真正可用要到任务 #35 调度时才知道。
2. **6 个 idc 账号缺 profile_arn** —— 是否能正常调度待验证。
3. 本次导入**未验证 UI 层**（直接调接口）。UI 的 Playwright 静默验证归入任务 #35。

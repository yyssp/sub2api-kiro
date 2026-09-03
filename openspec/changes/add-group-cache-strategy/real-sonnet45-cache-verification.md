# Sonnet 4.5 真实 Claude Code 连续调度验收

## 1. 验收范围

本记录只保留本次最终真实调度的终值，不记录历史调试过程或已废弃的受限输出测试。

- 业务服务：`http://127.0.0.1:48780`
- 业务进程：tmux `sub2api-local`，二进制 `/tmp/sub2api-local-latest`
- PostgreSQL：Docker `kiro-rs-postgres-local`，宿主机 `25432`
- Redis：Docker `kiro-rs-redis-local`，宿主机 `26379`
- 上游：已导入的真实 Kiro OAuth free 账号池，实际调度 Sonnet 4.5
- 协议入口：Anthropic Messages `POST /v1/messages`
- Claude Code 调用方式：真实 `claude` CLI，`--print --output-format json --verbose`，同一 `session_id` 使用 `--resume`
- 工具：每轮允许并要求 `Read`、`Bash`
- 会话：`08fe768e-dec8-4ef4-8978-53255c2c1732`
- 模型：`claude-sonnet-4-5-20250929`
- 请求最大输出参数：`max_tokens=64000`
- 连续轮数：10 轮，每轮问题承接上一轮，并执行真实工具调用
- 数据来源：真实网关响应和真实上游 usage；没有直接插入或伪造 `usage_logs`

## 2. max_tokens 语义

Anthropic Messages 请求必须带 `max_tokens`。Kiro 适配器按模型能力对请求值做上限处理：

| 模型 | 适配器最大输出 |
| --- | ---: |
| Claude Opus 4 / 4.1 | 32,000 |
| Claude Opus 4.5 | 64,000 |
| Claude Opus 4.6、4.7、4.8、5 | 128,000 |
| Claude Sonnet 4.5 及默认 Sonnet | 64,000 |
| Kiro GPT-5.6 系列 | 128,000 |

`max_tokens=64000` 只表示本次请求允许模型最多生成 64,000 tokens，不会要求模型生成 64,000，也不是缓存策略的 output 限制。实际输出由模型回答内容、工具循环、停止原因和上下文共同决定。缓存 runtime 不改写请求的 `max_tokens`，也不把它当作实际 `output_tokens`。

定向回归：

```bash
cd backend
go test ./internal/pkg/kiro \
  -run 'TestKiroMaxOutputTokensFor(OpusGenerations|Opus5|GPT56Models)$' \
  -count=1
```

终值：`PASS`。

## 3. 本次绑定策略

分组 8 绑定策略 5「真实测试-平衡缓存」，API Key 8 通过真实 scheduler 调度账号。策略快照配置如下：

```json
{
  "kind": "prefix",
  "enabled": true,
  "breakpoint_mode": "auto",
  "scope_mode": "group_account_session",
  "coverage_ratio": 0.85,
  "read_ratio": 1,
  "creation_ratio": 1,
  "cache_system": true,
  "cache_tools": true,
  "cache_history": true,
  "cache_tool_results": true,
  "cache_current_user_stable_prefix": false,
  "incremental_create_enabled": true,
  "min_cacheable_tokens": 1024,
  "default_ttl_seconds": 300,
  "hour_ttl_seconds": 3600,
  "usage.input.mode": "raw",
  "usage.output.mode": "raw",
  "usage.cache_read.mode": "preserve",
  "usage.cache_creation.mode": "preserve",
  "preserve_upstream_cache_usage": true
}
```

该配置没有 `reported_input_max_tokens`，没有 `final_output_max_tokens`，也没有 384、512、8192 等业务层固定输出值。默认 scope 包含 group、account、protocol、model、session 和 strategy revision；账号切换不会直接共享另一账号的缓存 namespace。

## 4. 十轮连续会话终值

下表是 Claude Code 每个外层轮次从最终 `result.usage` 聚合出的 usage。每个外层轮次内部可能包含多个 Anthropic 请求，因此第 1 轮聚合值出现 `read` 并不表示第一条真实请求读到了缓存。

| 轮次 | input | cache read | cache creation | creation 5m | creation 1h | output | 工具调用 | 状态 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 10,339 | 23,348 | 35,240 | 35,240 | 0 | 1,047 | 3 | success |
| 2 | 33,160 | 171,380 | 16,531 | 16,531 | 0 | 897 | 9 | success |
| 3 | 62,067 | 341,220 | 10,488 | 10,488 | 0 | 1,387 | 16 | success |
| 4 | 51,236 | 285,202 | 5,141 | 5,141 | 0 | 845 | 7 | success |
| 5 | 143,223 | 795,942 | 15,659 | 15,659 | 0 | 1,901 | 28 | success |
| 6 | 84,836 | 461,641 | 19,100 | 19,100 | 0 | 1,974 | 13 | success |
| 7 | 78,021 | 428,490 | 13,629 | 13,629 | 0 | 2,047 | 11 | success |
| 8 | 51,560 | 279,814 | 12,361 | 12,361 | 0 | 1,369 | 4 | success |
| 9 | 56,197 | 212,077 | 106,373 | 106,373 | 0 | 2,211 | 6 | success |
| 10 | 39,289 | 106,520 | 116,114 | 116,114 | 0 | 3,220 | 3 | success |

CLI 聚合终值：

```text
input=609,928
cache_read=3,105,634
cache_creation=350,636
cache_creation_5m=350,636
cache_creation_1h=0
output=16,898
tool_calls=100
successful_rounds=10/10
```

输出范围为 `845..3,220`，10 轮有 10 个不同值；全部 `stop_reason=end_turn`。因此输出不是固定 384，也不是被缓存策略强行填充为某个常数。

## 5. 首次 miss 和后续命中证据

Claude Code 第 1 个外层轮次包含后续工具请求。严格以数据库中该 session 的第一条真实 usage 行判断冷启动：

```text
input=176
cache_read=0
cache_creation=997
output=213
```

终值结论：

- 第一条真实请求 `cache_read=0`，没有凭空读取缓存。
- 成功提交后，后续请求出现非零 `cache_read`。
- `cache_creation` 在连续历史增长时随实际稳定前缀变化，不是固定伪造数。
- `cache_creation = cache_creation_5m + cache_creation_1h`，本次真实上游全部为 5m creation。
- 已绑定通用策略的分组不再走旧 `ForceCacheBilling` 伪造 `cache_read`；usage 由通用 cache runtime 和上游权威值共同决定。

## 6. 数据库对账终值

数据库字段使用系统实际命名：`input_tokens`、`cache_read_tokens`、`cache_creation_tokens`、`cache_creation_5m_tokens`、`cache_creation_1h_tokens`、`output_tokens`。

查询范围：`session_id=08fe768e-dec8-4ef4-8978-53255c2c1732`。

| 检查项 | 终值 |
| --- | ---: |
| usage 行数 | 51 |
| distinct group | 1 |
| distinct API Key | 1 |
| distinct account | 3 |
| input 总和 | 609,928 |
| cache_read 总和 | 3,105,634 |
| cache_creation 总和 | 350,636 |
| cache_creation_5m 总和 | 350,636 |
| cache_creation_1h 总和 | 0 |
| output 总和 | 16,898 |
| 有 cache_read 的记录 | 48 |
| 冷创建记录（read=0 且 creation>0） | 3 |
| 最大 cache_read | 106,520 |
| 最大 cache_creation | 112,809 |
| 最大 input+read+creation | 132,717 |
| 负数 usage 记录 | 0 |
| creation 分解不守恒记录 | 0 |
| 1,000,000 上下文超限记录 | 0 |

51 条记录而不是 10 条，是因为 10 个 Claude Code 外层轮次内部包含工具调用、工具结果和续接请求；每个内部 Anthropic 请求都会独立经过网关并落一条 usage。51 条数据库记录聚合后与 CLI 10 轮聚合值完全一致。

## 7. 账号隔离和记录快照

真实调度过程中使用了 3 个实际账号：

```text
account_id=101
account_id=39
account_id=16
```

策略默认 `group_account_session` scope 将账号纳入 namespace。每条 usage 记录同时保留真实 `group_id`、`api_key_id`、`account_id`、模型、入口和策略快照；账号切换不会凭空继承另一账号的本地缓存。

## 8. 最终修复

### 修复一：Claude Code session 解析

Claude Code 可能将 session 序列化在 `metadata.user_id` JSON 字符串中。runtime 现在解析其中的 `session_id`，没有时再读取 `conversation_id`，避免真实 Claude Code 请求被错误当成无 session raw 请求。

验证：

```text
TestCacheSessionKeyReadsClaudeCodeJSONMetadataUserID: PASS
```

### 修复二：绑定 disabled 策略的 usage 快照

绑定 `disabled` 策略仍需在使用记录中显示策略 ID 和名称，但 runtime 必须不读、不写。快照函数现在区分“未绑定策略”和“绑定 disabled 策略”，因此页面可以准确区分两种状态。

验证：

```text
TestForceCacheBilling: PASS
缓存关闭分组最终记录包含 cache_strategy_id/name，read=0、creation=0
```

## 9. 最终判定

本次真实 Claude Code 连续调度验收：**PASS**。

- 10/10 外层轮次成功。
- 每轮均触发真实 `Read` 或 `Bash` 工具调用。
- 首条真实 usage 无 cache read，后续连续请求出现真实命中。
- CLI 聚合和数据库 51 条内部 usage 聚合完全一致。
- usage 桶非负、creation 分解守恒，数据库单条记录最大上下文组合为 132,717，低于 1,000,000。
- 输出由模型自然生成，`max_tokens=64000` 只是最大允许输出。
- 当前业务服务只有一个 `48780` 实例。

证据文件：

- `/tmp/real-sonnet45-cli-10-round-final-20260902.json`
- `tools/real_claude_cache_validation.py`

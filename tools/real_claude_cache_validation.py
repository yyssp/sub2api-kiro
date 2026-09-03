import json
import os
import subprocess
import sys
import uuid
from pathlib import Path

BASE = "http://127.0.0.1:48780"
KEY = os.environ.get("ANTHROPIC_API_KEY", "").strip()
if not KEY:
    raise SystemExit("ANTHROPIC_API_KEY must be set; refusing to run without an explicit key")
MODEL = "claude-sonnet-4-5-20250929"
CONFIG = "/tmp/claude-cache-validation-config-20260902"
RESULT_PATH = Path(
    os.environ.get(
        "RESULT_PATH", "/tmp/real-sonnet45-cli-10-round-20260902.json"
    )
)
SESSION_PATH = Path(
    os.environ.get(
        "SESSION_PATH", "/tmp/real-sonnet45-cli-10-round-20260902.session"
    )
)

questions = [
    "这是连续审查会话第1轮。必须先用 Read 查看 backend/internal/service/cache_runtime.go 和 backend/internal/service/cache_policy_runtime.go，再用 Bash rg 搜索 prepareCachePlanForContext 的调用点。基于实际代码说明分组策略如何进入运行时、冷启动为什么不能读缓存；只输出关键结论，不要打印整文件。",
    "继续上一轮，不要重新开始。必须先用 Read 查看 cache_runtime.go 中 prepareCachePlanForContext、mergeAndCommitCachePlan 附近的实现，再用 Bash 搜索 cacheTracker.update 和 recordSuccess。说明本轮与上一轮的关联，以及成功请求何时写缓存、失败请求为什么不能写。",
    "继续上一轮。必须先用 Read 查看 backend/internal/service/gateway_upstream_response.go 中 usage 合并路径，再用 Bash 搜索 projectClaudeUsage 和 cache_creation_5m_tokens。核对 Anthropic usage 的 input/read/creation 守恒关系，指出真实会话当前是否已经出现缓存读取。",
    "继续上一轮。必须先用 Read 查看 backend/internal/pkg/kiro/translator.go 的 max_tokens 模型上限函数，再用 Bash 执行 go test ./internal/pkg/kiro -run 'TestKiroMaxOutputTokensFor(OpusGenerations|Opus5|GPT56Models)$' -count=1。解释 Sonnet 4.5、Opus 4/4.1、Opus 4.5、Opus 4.6+ 的上限差异。",
    "继续上一轮。必须先用 Bash 搜索 cacheStrategySnapshotForAPIKey、cache_strategy_name 和 usage_logs 写入点，再用 Read 查看相关函数。确认当前请求的分组、API Key、实际账号和策略快照是否会同时进入使用记录。",
    "继续上一轮。必须先用 Read 查看 cache_runtime.go 的 creation control 和 limitCacheProfileWriteSet，再用 Bash 搜索 min_creation_interval_seconds、max_new_creation_tokens_per_request。说明低频创建策略如何只抑制写入而不错误关闭读取。",
    "继续上一轮。必须先用 Read 查看协议入口如何为 Anthropic Messages 准备缓存 profile，再用 Bash 搜索 OpenAI Chat Completions/Responses 的 cache plan 接线。说明这套策略为什么是协议入口通用而不是 Kiro 专属。",
    "继续上一轮。必须先用 Bash 执行 git diff --stat 和 git status --short，再用 Read 查看 verification.md 中真实调度验收条件。将当前会话已经观察到的证据与仍未观察的风险分开。",
    "继续上一轮。必须先用 Read 查看流式完成、取消和错误路径中 mergeAndCommitCachePlan 的调用，再用 Bash 搜索 Abort 或 committed.CompareAndSwap。说明客户端取消或上游失败时 usage 和缓存状态应如何处理。",
    "最后一轮。必须先用 Bash 执行 git status --short，再用 Read 查看一个前面提到但尚未完整核对的函数。结合本次同一 session 的连续工具调用，总结策略消费、冷/热缓存、usage 守恒、工具调用和 max_tokens 的最终工程结论。",
]


def normalize_usage(value):
    if not isinstance(value, dict):
        return {}
    creation = value.get("cache_creation") or {}
    return {
        "input": int(value.get("input_tokens") or 0),
        "read": int(value.get("cache_read_input_tokens") or 0),
        "creation": int(value.get("cache_creation_input_tokens") or 0),
        "creation_5m": int(creation.get("ephemeral_5m_input_tokens") or 0),
        "creation_1h": int(creation.get("ephemeral_1h_input_tokens") or 0),
        "output": int(value.get("output_tokens") or 0),
    }


def call(prompt, session_id, first):
    flags = [
        "claude",
        "--print",
        "--output-format",
        "json",
        "--verbose",
        "--bare",
        "--tools",
        "Read,Bash",
        "--dangerously-skip-permissions",
        "--permission-mode",
        "bypassPermissions",
        "--model",
        MODEL,
        "--setting-sources",
        "",
    ]
    if first:
        flags += ["--session-id", session_id]
    else:
        flags += ["--resume", session_id]
    flags += ["-p", prompt]

    env = os.environ.copy()
    for key in (
        "ANTHROPIC_AUTH_TOKEN",
        "ANTHROPIC_DEFAULT_SONNET_MODEL",
        "CLAUDE_CODE_USE_BEDROCK",
        "CLAUDE_CODE_USE_VERTEX",
    ):
        env.pop(key, None)
    env.update(
        {
            "CLAUDE_CONFIG_DIR": CONFIG,
            "ANTHROPIC_BASE_URL": BASE,
            "ANTHROPIC_API_KEY": KEY,
            "ANTHROPIC_MODEL": MODEL,
            "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        }
    )
    proc = subprocess.run(
        flags,
        cwd="/Users/yuanfeijie/Desktop/procode/sub2api-kiro",
        env=env,
        text=True,
        capture_output=True,
        timeout=900,
    )
    record = {
        "returncode": proc.returncode,
        "stderr_tail": proc.stderr[-1200:],
    }
    try:
        parsed = json.loads(proc.stdout)
    except Exception as exc:
        record["status"] = "error"
        record["parse_error"] = str(exc)
        record["stdout_tail"] = proc.stdout[-3000:]
        return record
    events = parsed if isinstance(parsed, list) else [parsed]
    result = next((event for event in reversed(events) if event.get("type") == "result"), {})
    tools = []
    assistant_events = 0
    for event in events:
        if event.get("type") != "assistant":
            continue
        assistant_events += 1
        message = event.get("message") or {}
        for block in message.get("content") or []:
            if isinstance(block, dict) and block.get("type") == "tool_use":
                tools.append(block.get("name", ""))
    record.update(
        {
            "status": "success"
            if proc.returncode == 0 and result and not result.get("is_error")
            else "error",
            "subtype": result.get("subtype", ""),
            "stop_reason": result.get("stop_reason", ""),
            "duration_ms": result.get("duration_ms", 0),
            "usage": normalize_usage(result.get("usage")),
            "assistant_events": assistant_events,
            "tools": tools,
            "tool_call_count": len(tools),
            "result_chars": len(str(result.get("result") or "")),
            "errors": result.get("errors", []),
        }
    )
    return record


def main():
    start = int(os.environ.get("START_ROUND", "1"))
    end = int(os.environ.get("END_ROUND", str(len(questions))))
    if start < 1 or end > len(questions) or start > end:
        raise SystemExit("invalid START_ROUND/END_ROUND")

    if SESSION_PATH.exists():
        session_id = SESSION_PATH.read_text(encoding="utf-8").strip()
    else:
        session_id = str(uuid.uuid4())
        SESSION_PATH.write_text(session_id + "\n", encoding="utf-8")

    if RESULT_PATH.exists():
        payload = json.loads(RESULT_PATH.read_text(encoding="utf-8"))
    else:
        payload = {
            "session_id": session_id,
            "model": MODEL,
            "max_tokens_request": 64000,
            "rounds": [],
        }

    done = {item["round"] for item in payload["rounds"] if "round" in item}
    for number in range(start, end + 1):
        if number in done:
            continue
        item = call(questions[number - 1], session_id, number == 1)
        item["round"] = number
        payload["rounds"].append(item)
        payload["rounds"].sort(key=lambda value: value["round"])
        RESULT_PATH.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        print(
            json.dumps(
                {
                    "round": number,
                    "status": item.get("status"),
                    "stop_reason": item.get("stop_reason"),
                    "usage": item.get("usage"),
                    "tools": item.get("tools"),
                    "result_chars": item.get("result_chars"),
                },
                ensure_ascii=False,
            ),
            flush=True,
        )
        if item.get("status") != "success":
            return 1
    print(
        json.dumps(
            {
                "session_id": session_id,
                "completed_rounds": len(payload["rounds"]),
                "result_path": str(RESULT_PATH),
            },
            ensure_ascii=False,
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())

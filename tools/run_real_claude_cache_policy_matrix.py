#!/usr/bin/env python3
"""Run a retained, real Claude Code CLI cache-policy acceptance matrix.

This script only talks to the already running business service on 48780. It
creates one group, one API key, one strategy and one Claude session per
profile, then performs ten dependent --print/--resume turns against the real
Kiro account pool. Usage rows are never deleted.
"""

from __future__ import annotations

import copy
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

BASE = os.environ.get("SUB2API_BASE", "http://127.0.0.1:48780")
REPO_ROOT = Path(__file__).resolve().parents[1]
ADMIN_EMAIL = "admin@sub2api.local"
ADMIN_PASSWORD = "CacheLocal!2026"
TEST_USER_EMAIL = "real-cache-1788281453@sub2api.local"
TEST_USER_PASSWORD = "RealCache!2026"
TEST_USER_ID = 3
MODEL = "claude-sonnet-4-5-20250929"
DB_CONTAINER = os.environ.get("CACHE_MATRIX_DB_CONTAINER", "kiro-rs-postgres-local")
DB_USER = os.environ.get("CACHE_MATRIX_DB_USER", "kiro_rs")
DB_PASSWORD = os.environ.get("CACHE_MATRIX_DB_PASSWORD", "kiro_rs_dev_password")
DB_NAME = os.environ.get("CACHE_MATRIX_DB_NAME", "sub2api_cache_smoke")
ROUNDS = int(os.environ.get("CACHE_MATRIX_ROUNDS", "10"))
if ROUNDS < 10:
    raise ValueError("CACHE_MATRIX_ROUNDS must be at least 10")
CLI_TIMEOUT = int(os.environ.get("CACHE_MATRIX_CLI_TIMEOUT", "240"))
if CLI_TIMEOUT < 30:
    raise ValueError("CACHE_MATRIX_CLI_TIMEOUT must be at least 30 seconds")
# Every acceptance group copies the complete Kiro pool from this source group.
# Selection then remains a real scheduler decision: inactive, rate-limited and
# unsupported accounts are filtered by the running service, rather than by this
# test harness.  The harness never clears upstream cooldown state and never
# removes an account-group binding after a request failure.
SOURCE_KIRO_GROUP_ID = int(os.environ.get("CACHE_MATRIX_SOURCE_KIRO_GROUP_ID", "7"))
START_PROFILE = os.environ.get("CACHE_MATRIX_START_PROFILE", "").strip()
END_PROFILE = os.environ.get("CACHE_MATRIX_END_PROFILE", "").strip()
STAMP = time.strftime("%Y%m%d-%H%M%S")
RUN_TAG = os.environ.get("CACHE_MATRIX_RUN_TAG", "").strip() or (
    STAMP + "-" + uuid.uuid4().hex[:6]
)
RESULT_PATH = Path(
    os.environ.get(
        "CACHE_MATRIX_RESULT",
        f"openspec/changes/add-group-cache-strategy/real-cli-cache-policy-matrix-{STAMP}.json",
    )
)
FIXTURE_ROOT = Path(
    os.environ.get("CACHE_MATRIX_FIXTURE", f"/tmp/sub2api-cache-matrix-{STAMP}")
)


def request(method: str, path: str, payload=None, token: str | None = None, timeout=120):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    body = None if payload is None else json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(BASE + path, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            raw = response.read().decode()
            try:
                value = json.loads(raw)
            except json.JSONDecodeError:
                value = raw
            return response.status, value
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode()
        try:
            value = json.loads(raw)
        except json.JSONDecodeError:
            value = raw
        return exc.code, value


def unwrap(value):
    if isinstance(value, dict) and "data" in value:
        return value["data"]
    return value


def fail(status, body, what):
    if status < 200 or status >= 300:
        raise RuntimeError(f"{what} failed: HTTP {status}: {body}")
    return unwrap(body)


def login(email: str, password: str) -> str:
    status, body = request(
        "POST",
        "/api/v1/auth/login",
        {"email": email, "password": password},
    )
    return fail(status, body, "admin login")["access_token"]


def grant_test_user_group(admin_token: str, group_id: int) -> dict:
    user = fail(
        *request("GET", f"/api/v1/admin/users/{TEST_USER_ID}", token=admin_token),
        f"get test user {TEST_USER_ID}",
    )
    existing = [int(value) for value in (user.get("allowed_groups") or [])]
    if group_id in existing:
        return user
    return fail(
        *request(
            "PUT",
            f"/api/v1/admin/users/{TEST_USER_ID}",
            {"allowed_groups": existing + [group_id]},
            admin_token,
        ),
        f"grant test user {TEST_USER_ID} group {group_id}",
    )


def make_fixture() -> None:
    FIXTURE_ROOT.mkdir(parents=True, exist_ok=True)
    topics = [
        "gateway routing and account scheduling",
        "cache fingerprint and prefix lookup",
        "usage projection and billing",
        "Anthropic messages adapter",
        "OpenAI chat adapter",
        "OpenAI responses adapter",
        "Kiro OAuth translator",
        "creation control and TTL",
        "streaming commit and abort",
        "admin policy registry",
        "database usage log schema",
        "frontend strategy editor",
    ]
    for index, topic in enumerate(topics, 1):
        path = FIXTURE_ROOT / f"module-{index:02d}.md"
        rows = [
            f"# Module {index}: {topic}\n",
            "This is a deterministic synthetic repository fixture for a real "
            "Claude Code tool session. Treat it as stable architecture context.\n",
            "Every identifier below is intentionally distinct so that Read and "
            "Bash results are meaningful and can be revisited in later turns.\n\n",
        ]
        for row in range(900):
            value = (index * 1000003 + row * 9176) % 0xFFFFFF
            rows.append(
                f"- {topic} decision {row:04d}: "
                f"component_{index:02d}_{row:04d} -> "
                f"token_{value:06x}; invariant=bounded_usage; "
                f"owner=team_{(row + index) % 7}; "
                f"dependency=module_{((index + row) % len(topics)) + 1:02d}\n"
            )
        path.write_text("".join(rows), encoding="utf-8")
    (FIXTURE_ROOT / "README.md").write_text(
        "# Cache policy acceptance fixture\n"
        "Use Read on the requested module and Bash rg for cross-module checks.\n",
        encoding="utf-8",
    )


def base_config(token: str) -> dict:
    status, body = request("GET", "/api/v1/admin/cache-strategies", token=token)
    items = fail(status, body, "list cache strategies")
    if items:
        return copy.deepcopy(items[0]["config"])
    # The acceptance run intentionally starts with an empty strategy table.
    # Keep a complete payload here instead of creating a temporary strategy or
    # relying on a stale database row.  These keys mirror
    # createDefaultCacheStrategyConfig("prefix") in the frontend.
    return {
        "kind": "prefix",
        "ratio_mode": "uniform",
        "coverage_ratio": 0.85,
        "usage_ratio": 1,
        "read_ratio": 1,
        "creation_ratio": 1,
        "cache_system": True,
        "cache_tools": True,
        "cache_history": True,
        "cache_tool_results": True,
        "cache_current_user_stable_prefix": False,
        "current_user_stable_prefix_max_tokens": 0,
        "breakpoint_mode": "hybrid",
        "allow_derived_session": False,
        "dynamic_content_mode": "exclude",
        "scope_mode": "group_account_session",
        "max_coverage_tokens": 0,
        "max_new_creation_tokens_per_request": 0,
        "incremental_create_enabled": True,
        "min_cacheable_tokens": 1024,
        "reported_input_min_tokens": 0,
        "reported_input_max_tokens": 0,
        "token_scale": 1,
        "scale_min_input_tokens": 0,
        "max_simulated_input_tokens": 0,
        "default_ttl_seconds": 300,
        "hour_ttl_seconds": 3600,
        "max_entries_per_scope": 128,
        "max_entries_global": 10000,
        "estimated_bytes_limit": 67108864,
        "expire_after_idle_seconds": 0,
        "cap_jitter_min_tokens": 0,
        "cap_jitter_max_tokens": 0,
        "preserve_upstream_cache_usage": True,
        "usage": {
            "enabled": True,
            "preserve_upstream_cache_usage": True,
            "input": usage_field("raw"),
            "output": usage_field("raw"),
            "cache_read": usage_field("preserve"),
            "cache_creation": usage_field("preserve"),
            "final_cache_read_max_tokens": 0,
            "final_cache_creation_max_tokens": 0,
            "output_uplift_min_tokens": 0,
            "output_uplift_percent": 0,
            "final_output_max_tokens": 0,
        },
        "creation_control": {
            "enabled": False,
            "min_creation_delta_tokens": 0,
            "min_successful_requests_between": 0,
            "min_creation_interval_seconds": 0,
            "max_creation_tokens_per_event": 0,
            "creation_budget_window_seconds": 0,
            "max_creation_tokens_per_window": 0,
        },
    }


def usage_field(mode, max_tokens=0, target_tokens=0, multiplier=1.1, move=False):
    return {
        "mode": mode,
        "max_tokens": max_tokens,
        "target_tokens": target_tokens,
        "normal_max_multiplier": multiplier,
        "move_delta_to_cache_read": move,
    }


def profiles(base: dict) -> dict[str, dict]:
    def common(c):
        c["preserve_upstream_cache_usage"] = False
        c["usage"]["preserve_upstream_cache_usage"] = False
        c["default_ttl_seconds"] = 300
        c["hour_ttl_seconds"] = 3600
        c["min_cacheable_tokens"] = 1024
        return c

    out: dict[str, dict] = {}

    c = common(copy.deepcopy(base))
    c.update(
        kind="disabled", coverage_ratio=0, usage_ratio=0, read_ratio=0,
        creation_ratio=0, cache_system=False, cache_tools=False,
        cache_history=False, cache_tool_results=False,
        incremental_create_enabled=False,
    )
    c["usage"]["enabled"] = False
    out["验收-无缓存基线"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="prefix", ratio_mode="uniform", coverage_ratio=0.98,
        usage_ratio=0.98, read_ratio=0.98, creation_ratio=0.98,
        breakpoint_mode="auto", token_scale=1.6, scale_min_input_tokens=20000,
        max_coverage_tokens=300000, max_new_creation_tokens_per_request=30000,
        max_simulated_input_tokens=300000, cap_jitter_min_tokens=12000,
        cap_jitter_max_tokens=24000,
    )
    c["usage"]["final_cache_read_max_tokens"] = 700000
    c["usage"]["final_cache_creation_max_tokens"] = 400000
    c["usage"]["output_uplift_min_tokens"] = 1000
    c["usage"]["output_uplift_percent"] = 50
    c["usage"]["final_output_max_tokens"] = 200000
    out["验收-高命中长会话"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="tool_aware", ratio_mode="uniform", coverage_ratio=0.92,
        usage_ratio=0.9, read_ratio=0.95, creation_ratio=0.8,
        breakpoint_mode="hybrid", token_scale=1.35, scale_min_input_tokens=20000,
        max_coverage_tokens=240000, max_new_creation_tokens_per_request=24000,
        max_simulated_input_tokens=240000,
    )
    c["usage"]["input"] = usage_field("sample_max", max_tokens=16000, move=True)
    c["usage"]["cache_read"] = usage_field("sample_target", target_tokens=180000, multiplier=1.2)
    c["usage"]["cache_creation"] = usage_field("sample_target", target_tokens=30000, multiplier=1.2)
    c["usage"]["final_cache_read_max_tokens"] = 500000
    c["usage"]["final_cache_creation_max_tokens"] = 200000
    c["usage"]["final_output_max_tokens"] = 120000
    out["验收-标准工具缓存"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="tool_aware", ratio_mode="independent", coverage_ratio=0.86,
        usage_ratio=0.82, read_ratio=0.9, creation_ratio=0.72,
        breakpoint_mode="hybrid", token_scale=1.25, scale_min_input_tokens=16000,
        max_coverage_tokens=180000, max_new_creation_tokens_per_request=18000,
        max_simulated_input_tokens=180000,
    )
    c["usage"]["input"] = usage_field("sample_max", max_tokens=24000, move=True)
    c["usage"]["cache_read"] = usage_field("preserve")
    c["usage"]["cache_creation"] = usage_field("preserve")
    c["usage"]["final_cache_read_max_tokens"] = 300000
    c["usage"]["final_cache_creation_max_tokens"] = 120000
    c["usage"]["final_output_max_tokens"] = 100000
    out["验收-输入整形对比"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="prefix", ratio_mode="independent", coverage_ratio=0.78,
        usage_ratio=0.75, read_ratio=1, creation_ratio=0.55,
        breakpoint_mode="auto", token_scale=1.3, scale_min_input_tokens=20000,
        max_coverage_tokens=180000, max_new_creation_tokens_per_request=30000,
        max_simulated_input_tokens=220000,
    )
    c["creation_control"] = {
        "enabled": True, "min_creation_delta_tokens": 12000,
        "min_successful_requests_between": 2, "min_creation_interval_seconds": 60,
        "max_creation_tokens_per_event": 30000,
        "creation_budget_window_seconds": 300,
        "max_creation_tokens_per_window": 120000,
    }
    c["usage"]["cache_read"] = usage_field("sample_max", max_tokens=180000)
    c["usage"]["cache_creation"] = usage_field("sample_max", max_tokens=30000)
    c["usage"]["final_cache_read_max_tokens"] = 300000
    c["usage"]["final_cache_creation_max_tokens"] = 120000
    c["usage"]["final_output_max_tokens"] = 120000
    out["验收-低频创建控制"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="prefix", ratio_mode="independent", coverage_ratio=0.92,
        usage_ratio=0.88, read_ratio=0.98, creation_ratio=0.4,
        breakpoint_mode="auto", token_scale=1.2, scale_min_input_tokens=24000,
        max_coverage_tokens=450000, max_new_creation_tokens_per_request=30000,
        max_simulated_input_tokens=450000, reported_input_min_tokens=20000,
        reported_input_max_tokens=850000,
    )
    c["creation_control"] = {
        "enabled": True, "min_creation_delta_tokens": 20000,
        "min_successful_requests_between": 1, "min_creation_interval_seconds": 0,
        "max_creation_tokens_per_event": 30000,
        "creation_budget_window_seconds": 300,
        "max_creation_tokens_per_window": 120000,
    }
    c["usage"]["cache_read"] = usage_field("sample_target", target_tokens=280000, multiplier=1.15)
    c["usage"]["cache_creation"] = usage_field("sample_max", max_tokens=60000)
    c["usage"]["final_cache_read_max_tokens"] = 550000
    c["usage"]["final_cache_creation_max_tokens"] = 180000
    c["usage"]["final_output_max_tokens"] = 200000
    out["验收-长上下文守护"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="tool_aware", ratio_mode="independent", coverage_ratio=0.9,
        usage_ratio=0.9, read_ratio=1, creation_ratio=0.35,
        breakpoint_mode="hybrid", max_coverage_tokens=240000,
        max_new_creation_tokens_per_request=12000,
        max_simulated_input_tokens=260000,
        incremental_create_enabled=False,
    )
    c["usage"]["cache_read"] = usage_field("sample_max", max_tokens=220000)
    c["usage"]["cache_creation"] = usage_field("sample_max", max_tokens=12000)
    c["usage"]["final_cache_read_max_tokens"] = 500000
    c["usage"]["final_cache_creation_max_tokens"] = 60000
    c["creation_control"] = {
        "enabled": True, "min_creation_delta_tokens": 0,
        "min_successful_requests_between": 1, "min_creation_interval_seconds": 0,
        "max_creation_tokens_per_event": 12000,
        "creation_budget_window_seconds": 600,
        "max_creation_tokens_per_window": 48000,
    }
    out["验收-仅读优先"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="prefix", ratio_mode="uniform", coverage_ratio=0.65,
        usage_ratio=0.8, read_ratio=0.8, creation_ratio=0.8,
        breakpoint_mode="client_only", max_coverage_tokens=180000,
        max_new_creation_tokens_per_request=60000,
        max_simulated_input_tokens=200000,
        incremental_create_enabled=False,
    )
    c["usage"]["cache_read"] = usage_field("sample_max", max_tokens=180000)
    c["usage"]["cache_creation"] = usage_field("sample_max", max_tokens=60000)
    c["usage"]["final_cache_read_max_tokens"] = 180000
    c["usage"]["final_cache_creation_max_tokens"] = 60000
    out["验收-严格客户端断点"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="prefix", ratio_mode="independent", scope_mode="group_session",
        allow_derived_session=True, coverage_ratio=0.9, usage_ratio=0.9,
        read_ratio=0.75, creation_ratio=0.6, max_coverage_tokens=260000,
        max_new_creation_tokens_per_request=45000,
        max_simulated_input_tokens=280000,
    )
    c["usage"]["cache_read"] = usage_field(
        "sample_target", target_tokens=180000, multiplier=1.3
    )
    c["usage"]["cache_creation"] = usage_field(
        "sample_target", target_tokens=45000, multiplier=1.25
    )
    c["usage"]["final_cache_read_max_tokens"] = 600000
    c["usage"]["final_cache_creation_max_tokens"] = 160000
    c["creation_control"] = {
        "enabled": True, "min_creation_delta_tokens": 256,
        "min_successful_requests_between": 1, "min_creation_interval_seconds": 0,
        "max_creation_tokens_per_event": 45000,
        "creation_budget_window_seconds": 600,
        "max_creation_tokens_per_window": 160000,
    }
    out["验收-共享会话"] = c

    c = common(copy.deepcopy(base))
    c.update(
        kind="tool_aware", ratio_mode="uniform", coverage_ratio=0.72,
        usage_ratio=0.7, read_ratio=0.7, creation_ratio=0.7,
        token_scale=1.1, scale_min_input_tokens=12000,
        max_coverage_tokens=90000, max_new_creation_tokens_per_request=12000,
        max_simulated_input_tokens=120000,
    )
    c["usage"]["input"] = usage_field(
        "sample_target", target_tokens=12000, multiplier=1.35
    )
    c["usage"]["output"] = usage_field(
        "sample_target", target_tokens=256, multiplier=1.5
    )
    c["usage"]["cache_read"] = usage_field("sample_max", max_tokens=120000)
    c["usage"]["cache_creation"] = usage_field("sample_max", max_tokens=60000)
    c["usage"]["final_cache_read_max_tokens"] = 120000
    c["usage"]["final_cache_creation_max_tokens"] = 60000
    c["usage"]["final_output_max_tokens"] = 8192
    out["验收-保守 usage"] = c
    return out


def create_resources(admin_token: str, user_token: str, name: str, config: dict) -> dict:
    strategy = fail(
        *request(
            "POST",
            "/api/v1/admin/cache-strategies",
            {
                "name": name,
                "description": "真实 Claude Code CLI 连续十轮缓存策略验收",
                "enabled": True,
                "config": config,
            },
            admin_token,
        ),
        "create cache strategy",
    )
    sid = int(strategy["id"])
    group = fail(
        *request(
            "POST",
            "/api/v1/admin/groups",
            {
                "name": name + " 分组",
                "description": "真实调度验收专用分组，完整 Kiro 账号池连续会话",
                "platform": "kiro",
                "rate_multiplier": 1,
                "claude_code_only": True,
                "copy_accounts_from_group_ids": [SOURCE_KIRO_GROUP_ID],
            },
            admin_token,
        ),
        "create group",
    )
    gid = int(group["id"])
    grant_test_user_group(admin_token, gid)
    fail(
        *request(
            "PUT",
            f"/api/v1/admin/cache-strategies/{sid}/groups",
            {"group_ids": [gid]},
            admin_token,
        ),
        "bind cache strategy",
    )
    key_text = "sk-matrix-" + re.sub(r"[^a-z0-9]+", "-", name.lower()) + "-" + uuid.uuid4().hex
    key = fail(
        *request(
            "POST",
            "/api/v1/keys",
            {"name": name + " API Key", "group_id": gid, "custom_key": key_text},
            user_token,
        ),
        "create API key",
    )
    expected_bindings = db_scalar(
        "SELECT count(*) "
        "FROM account_groups ag "
        "JOIN accounts a ON a.id = ag.account_id "
        f"WHERE ag.group_id={SOURCE_KIRO_GROUP_ID} "
        "AND a.platform='kiro' AND a.deleted_at IS NULL;"
    )
    actual_bindings = db_scalar(
        "SELECT count(*) "
        "FROM account_groups ag "
        "JOIN accounts a ON a.id = ag.account_id "
        f"WHERE ag.group_id={gid} "
        "AND a.platform='kiro' AND a.deleted_at IS NULL;"
    )
    if expected_bindings == 0:
        raise RuntimeError(
            f"source Kiro group {SOURCE_KIRO_GROUP_ID} has no active account bindings"
        )
    if actual_bindings != expected_bindings:
        raise RuntimeError(
            f"group {gid} copied {actual_bindings}/{expected_bindings} Kiro accounts"
        )
    return {
        "strategy_id": sid,
        "group_id": gid,
        "api_key_id": int(key["id"]),
        "api_key": key.get("key", key_text),
        "strategy_config": config,
        "source_group_id": SOURCE_KIRO_GROUP_ID,
        "bound_kiro_accounts": actual_bindings,
    }


def prompt(round_no: int) -> str:
    first = ((round_no - 1) % 12) + 1
    second = (round_no % 12) + 1
    return (
        f"这是同一连续工程审查会话第 {round_no} 轮，必须承接前一轮结论。"
        f"先使用 Read 工具完整查看 module-{first:02d}.md，"
        f"再使用 Bash 工具执行 rg -n 'invariant=bounded_usage|dependency=module_{second:02d}' "
        f"module-*.md。然后基于实际工具结果，复核缓存策略的一个新约束，"
        "指出与上一轮的关联、一个风险和可执行修复验收标准。"
        "同时读取仓库中的 backend/internal/service/cache_runtime.go 或 "
        "backend/internal/service/cache_policy_runtime.go，并用 Bash 检索 "
        "backend/internal/service 中与 cache_strategy 相关的实现。不要凭空编造文件内容。"
    )


def run_turn(key: str, session: str, round_no: int, first: bool) -> dict:
    env = os.environ.copy()
    for key_name in ("ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"):
        env.pop(key_name, None)
    env.update({
        "CLAUDE_CONFIG_DIR": str(FIXTURE_ROOT / ".claude"),
        "ANTHROPIC_BASE_URL": BASE,
        "ANTHROPIC_API_KEY": key,
        "ANTHROPIC_MODEL": MODEL,
        "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    })
    last_record = None
    max_attempts = 3
    for attempt in range(max_attempts):
        args = [
            "claude", "--print", "--output-format", "stream-json", "--verbose", "--bare",
            "--tools", "Read,Bash", "--dangerously-skip-permissions",
            "--permission-mode", "bypassPermissions", "--model", MODEL,
            "--setting-sources", "", "--add-dir", str(FIXTURE_ROOT),
        ]
        # A retry resumes the same real Claude session. The first invocation
        # creates it; subsequent attempts never create a second session.
        session_flag = "--session-id" if first and attempt == 0 else "--resume"
        turn_prompt = prompt(round_no)
        if attempt:
            turn_prompt += (
                " 上一次调用没有完成工具链；本轮重试必须先实际调用 Read 和 Bash，"
                "不要直接返回文字，完成工具结果后再给出结论。"
            )
        args += [session_flag, session, "-p", turn_prompt]
        try:
            proc = subprocess.run(
                args,
                cwd=REPO_ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=CLI_TIMEOUT,
            )
        except subprocess.TimeoutExpired as exc:
            stdout = exc.stdout or ""
            stderr = exc.stderr or ""
            if isinstance(stdout, bytes):
                stdout = stdout.decode(errors="replace")
            if isinstance(stderr, bytes):
                stderr = stderr.decode(errors="replace")
            last_record = {
                "round": round_no,
                "attempt": attempt + 1,
                "status": "error",
                "returncode": None,
                "error": f"Claude CLI timed out after {CLI_TIMEOUT}s",
                "stdout_tail": stdout[-2000:],
                "stderr_tail": stderr[-2000:],
            }
            continue

        record = {
            "round": round_no,
            "attempt": attempt + 1,
            "returncode": proc.returncode,
            "stderr_tail": proc.stderr[-1000:],
        }
        events = []
        parse_errors = []
        for line in proc.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                events.append(json.loads(line))
            except Exception as exc:
                parse_errors.append(f"{type(exc).__name__}: {line[:240]}")
        # A few Claude Code versions can still emit one JSON object when stream
        # mode is unavailable. Keep a single-object fallback for diagnostics.
        if not events:
            try:
                events = [json.loads(proc.stdout)]
            except Exception as exc:
                record.update(
                    status="error",
                    parse_error=str(exc),
                    parse_errors=parse_errors[-5:],
                    stdout_tail=proc.stdout[-2000:],
                )
                last_record = record
                continue
        if parse_errors:
            record["parse_errors"] = parse_errors[-5:]
        result = next((event for event in reversed(events) if event.get("type") == "result"), {})
        tools = []
        tool_events = []
        assistant_events = 0
        for event in events:
            if event.get("type") != "assistant":
                continue
            assistant_events += 1
            for block in (event.get("message") or {}).get("content") or []:
                if isinstance(block, dict) and block.get("type") == "tool_use":
                    name = block.get("name", "")
                    tools.append(name)
                    tool_events.append({
                        "name": name,
                        "id": block.get("id", ""),
                        "input_keys": sorted((block.get("input") or {}).keys()),
                    })
        usage = result.get("usage") or {}
        creation = usage.get("cache_creation") or {}
        normalized = {
            "input": int(usage.get("input_tokens") or 0),
            "read": int(usage.get("cache_read_input_tokens") or 0),
            "creation": int(usage.get("cache_creation_input_tokens") or 0),
            "creation_5m": int(creation.get("ephemeral_5m_input_tokens") or 0),
            "creation_1h": int(creation.get("ephemeral_1h_input_tokens") or 0),
            "output": int(usage.get("output_tokens") or 0),
        }
        record.update({
            "status": "success" if proc.returncode == 0 and result and not result.get("is_error") else "error",
            "subtype": result.get("subtype", ""),
            "stop_reason": result.get("stop_reason", ""),
            "duration_ms": result.get("duration_ms", 0),
            "usage": normalized,
            "tools": tools,
            "tool_events": tool_events,
            "tool_call_count": len(tools),
            "assistant_events": assistant_events,
            "result_chars": len(str(result.get("result") or "")),
            "errors": result.get("errors", []),
        })
        if "Read" not in tools or "Bash" not in tools:
            record["status"] = "error"
            record["error"] = "required Read+Bash tool calls were not observed"
            last_record = record
            continue
        return record
    return last_record or {
        "round": round_no,
        "status": "error",
        "error": "Claude CLI produced no usable result after retries",
    }


def db_scalar(sql: str) -> int:
    proc = subprocess.run(
        [
            "docker", "exec", "-e", f"PGPASSWORD={DB_PASSWORD}", DB_CONTAINER,
            "psql", "-U", DB_USER, "-d", DB_NAME, "-Atqc", sql,
        ],
        text=True,
        capture_output=True,
        check=True,
    )
    value = proc.stdout.strip()
    return int(value or 0)


def db_summary(group_id: int, strategy_id: int, api_key_id: int, session_id: str) -> dict:
    sql = (
        "SELECT count(*), COALESCE(sum(input_tokens),0), "
        "COALESCE(sum(cache_read_tokens),0), COALESCE(sum(cache_creation_tokens),0), "
        "COALESCE(sum(output_tokens),0), COALESCE(max(input_tokens + cache_read_tokens + cache_creation_tokens),0), "
        "COALESCE(sum(CASE WHEN cache_read_tokens < 0 OR cache_creation_tokens < 0 OR input_tokens < 0 OR output_tokens < 0 THEN 1 ELSE 0 END),0), "
        "COALESCE(sum(CASE WHEN cache_creation_tokens <> cache_creation_5m_tokens + cache_creation_1h_tokens THEN 1 ELSE 0 END),0) "
        f"FROM usage_logs WHERE group_id={group_id} AND cache_strategy_id={strategy_id} "
        f"AND api_key_id={api_key_id} AND session_id='{session_id}';"
    )
    proc = subprocess.run(
        [
            "docker", "exec", "-e", f"PGPASSWORD={DB_PASSWORD}", DB_CONTAINER,
            "psql", "-U", DB_USER, "-d", DB_NAME, "-Atqc", sql,
        ],
        text=True, capture_output=True, check=True,
    )
    values = proc.stdout.strip().split("|")
    if len(values) != 8:
        raise RuntimeError(f"unexpected usage summary: {proc.stdout!r}")
    return {
        "rows": int(values[0]), "input_sum": int(values[1]), "read_sum": int(values[2]),
        "creation_sum": int(values[3]), "output_sum": int(values[4]),
        "max_context": int(values[5]), "negative_bucket_rows": int(values[6]),
        "creation_breakdown_mismatch_rows": int(values[7]),
    }


def db_rows(group_id: int, strategy_id: int, api_key_id: int, session_id: str) -> list[dict]:
    sql = (
        "SELECT id,group_id,api_key_id,account_id,cache_strategy_id,cache_strategy_name,"
        "session_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,"
        "cache_creation_5m_tokens,cache_creation_1h_tokens,output_tokens,created_at "
        f"FROM usage_logs WHERE group_id={group_id} AND cache_strategy_id={strategy_id} "
        f"AND api_key_id={api_key_id} AND session_id='{session_id}' ORDER BY id;"
    )
    proc = subprocess.run(
        [
            "docker", "exec", "-e", f"PGPASSWORD={DB_PASSWORD}", DB_CONTAINER,
            "psql", "-U", DB_USER, "-d", DB_NAME, "-Atq", "-F", "|", "-c", sql,
        ],
        text=True, capture_output=True, check=True,
    )
    rows = []
    for line in proc.stdout.splitlines():
        parts = line.split("|")
        if len(parts) != 15:
            continue
        rows.append({
            "id": int(parts[0]), "group_id": int(parts[1]), "api_key_id": int(parts[2]),
            "account_id": int(parts[3]), "cache_strategy_id": int(parts[4]),
            "cache_strategy_name": parts[5], "session_id": parts[6], "model": parts[7],
            "input_tokens": int(parts[8]), "cache_read_tokens": int(parts[9]),
            "cache_creation_tokens": int(parts[10]), "cache_creation_5m_tokens": int(parts[11]),
            "cache_creation_1h_tokens": int(parts[12]), "output_tokens": int(parts[13]),
            "created_at": parts[14],
        })
    return rows


def db_account_ids(group_id: int, strategy_id: int, api_key_id: int, session_id: str) -> list[int]:
    sql = (
        "SELECT DISTINCT account_id "
        "FROM usage_logs "
        f"WHERE group_id={group_id} AND cache_strategy_id={strategy_id} "
        f"AND api_key_id={api_key_id} AND session_id='{session_id}' "
        "ORDER BY account_id;"
    )
    proc = subprocess.run(
        [
            "docker", "exec", "-e", f"PGPASSWORD={DB_PASSWORD}", DB_CONTAINER,
            "psql", "-U", DB_USER, "-d", DB_NAME, "-Atqc", sql,
        ],
        text=True, capture_output=True, check=True,
    )
    return [int(value) for value in proc.stdout.splitlines() if value.strip()]


def validate_profile_constraints(item: dict, rows: list[dict]) -> list[str]:
    """Validate configuration-specific invariants without requiring one fixed
    usage shape. Values may vary with the real model/tool loop, but configured
    caps and cache state transitions must remain observable.
    """
    errors: list[str] = []
    config = item["strategy_config"]
    usage = config.get("usage") or {}
    kind = config.get("kind")
    if kind == "disabled":
        if any(r["cache_read_tokens"] or r["cache_creation_tokens"] for r in rows):
            errors.append("disabled strategy emitted cache usage")
        return errors

    first_read = rows[0]["cache_read_tokens"]
    if first_read != 0:
        errors.append("first persisted usage row has non-zero cache_read_tokens")

    input_min = int(config.get("reported_input_min_tokens") or 0)
    input_max = int(config.get("reported_input_max_tokens") or 0)
    if input_min and any(r["input_tokens"] < input_min for r in rows):
        errors.append(f"input_tokens below configured minimum {input_min}")
    if input_max and any(r["input_tokens"] > input_max for r in rows):
        errors.append(f"input_tokens above configured maximum {input_max}")

    final_read_max = int(usage.get("final_cache_read_max_tokens") or 0)
    final_creation_max = int(usage.get("final_cache_creation_max_tokens") or 0)
    final_output_max = int(usage.get("final_output_max_tokens") or 0)
    if final_read_max and any(r["cache_read_tokens"] > final_read_max for r in rows):
        errors.append(f"cache_read_tokens above final cap {final_read_max}")
    if final_creation_max and any(r["cache_creation_tokens"] > final_creation_max for r in rows):
        errors.append(f"cache_creation_tokens above final cap {final_creation_max}")
    if final_output_max and any(r["output_tokens"] > final_output_max for r in rows):
        errors.append(f"output_tokens above final cap {final_output_max}")

    control = config.get("creation_control") or {}
    event_max = int(control.get("max_creation_tokens_per_event") or 0)
    if event_max and any(r["cache_creation_tokens"] > event_max for r in rows):
        errors.append(f"creation event above configured cap {event_max}")

    # Client-only means no automatic breakpoints. Claude Code may still send
    # explicit cache_control blocks, so only require the strict zero behavior
    # when the fixture produced no client breakpoint evidence.
    if config.get("breakpoint_mode") == "client_only" and not config.get("cache_current_user_stable_prefix"):
        if all(r["cache_read_tokens"] == 0 and r["cache_creation_tokens"] == 0 for r in rows):
            item["client_only_zero_cache"] = True

    has_cache = any(r["cache_read_tokens"] > 0 for r in rows)
    if config.get("breakpoint_mode") != "client_only" and not has_cache:
        errors.append("enabled automatic/hybrid strategy never produced a cache read")

    # The test is intended to prove that projection is active, not that every
    # profile collapses to one constant. For profiles using sampled fields,
    # retain evidence of natural variation when the upstream produced enough
    # rows.
    outputs = {r["output_tokens"] for r in rows}
    if len(rows) >= 10 and len(outputs) < 2:
        errors.append("output_tokens are constant across all persisted rows")
    return errors


def validate_rows(item: dict) -> dict:
    rows = db_rows(item["group_id"], item["strategy_id"], item["api_key_id"], item["session_id"])
    if len(rows) < 10:
        raise RuntimeError(f"{item['name']} has only {len(rows)} usage rows")
    errors = []
    for row in rows:
        if any(row[key] < 0 for key in ("input_tokens", "cache_read_tokens", "cache_creation_tokens", "output_tokens")):
            errors.append(f"negative token bucket in usage row {row['id']}")
        if row["cache_creation_tokens"] != row["cache_creation_5m_tokens"] + row["cache_creation_1h_tokens"]:
            errors.append(f"creation breakdown mismatch in usage row {row['id']}")
        if row["input_tokens"] + row["cache_read_tokens"] + row["cache_creation_tokens"] > 1_000_000:
            errors.append(f"context exceeds 1m in usage row {row['id']}")
        if row["cache_strategy_name"] != item["name"]:
            errors.append(f"strategy snapshot mismatch in usage row {row['id']}")
    errors.extend(validate_profile_constraints(item, rows))
    if errors:
        raise RuntimeError(f"{item['name']} usage validation failed: {'; '.join(errors)}")
    item["usage_rows"] = rows
    return {
        "rows": len(rows),
        "first_cache_read": rows[0]["cache_read_tokens"],
        "max_context": max(r["input_tokens"] + r["cache_read_tokens"] + r["cache_creation_tokens"] for r in rows),
        "max_input": max(r["input_tokens"] for r in rows),
        "max_read": max(r["cache_read_tokens"] for r in rows),
        "max_creation": max(r["cache_creation_tokens"] for r in rows),
        "max_output": max(r["output_tokens"] for r in rows),
    }


def main() -> int:
    admin_token = login(ADMIN_EMAIL, ADMIN_PASSWORD)
    user_token = login(TEST_USER_EMAIL, TEST_USER_PASSWORD)
    make_fixture()
    base = base_config(admin_token)
    matrix = profiles(base)
    RESULT_PATH.parent.mkdir(parents=True, exist_ok=True)
    result = {
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "base_url": BASE, "model": MODEL, "rounds_required": ROUNDS,
        "fixture_root": str(FIXTURE_ROOT), "strategies": [],
    }
    names = list(matrix)
    if START_PROFILE:
        if START_PROFILE not in matrix:
            raise RuntimeError(f"unknown CACHE_MATRIX_START_PROFILE={START_PROFILE}")
        names = names[names.index(START_PROFILE):]
    if END_PROFILE:
        if END_PROFILE not in matrix:
            raise RuntimeError(f"unknown CACHE_MATRIX_END_PROFILE={END_PROFILE}")
        names = names[: names.index(END_PROFILE) + 1]
    for name in names:
        # Strategy names are unique per acceptance run so a failed/restarted
        # run never collides with retained evidence from an earlier run.
        run_name = f"{name}-{RUN_TAG}"
        print(f"[matrix] creating {run_name}", flush=True)
        config = matrix[name]
        resources = create_resources(admin_token, user_token, run_name, config)
        item = {"name": run_name, "profile": name, **resources, "rounds": []}
        result["strategies"].append(item)
        RESULT_PATH.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        session = str(uuid.uuid4())
        rounds: list[dict] = []
        for round_no in range(1, ROUNDS + 1):
            print(
                f"[matrix] {name} full-pool group {resources['group_id']} "
                f"round {round_no}/{ROUNDS}",
                flush=True,
            )
            turn = run_turn(resources["api_key"], session, round_no, round_no == 1)
            rounds.append(turn)
            if turn["status"] != "success":
                item["session_id"] = session
                item["rounds"] = rounds
                item["failure_usage_rows"] = db_scalar(
                    "SELECT count(*) FROM usage_logs "
                    f"WHERE group_id={resources['group_id']} "
                    f"AND cache_strategy_id={resources['strategy_id']} "
                    f"AND api_key_id={resources['api_key_id']};"
                )
                RESULT_PATH.write_text(
                    json.dumps(result, ensure_ascii=False, indent=2) + "\n",
                    encoding="utf-8",
                )
                raise RuntimeError(
                    f"{name} failed on round {round_no}; bindings and usage were retained: {turn}"
                )
        item["session_id"] = session
        item["rounds"] = rounds
        RESULT_PATH.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        item["db_summary"] = db_summary(
            resources["group_id"], resources["strategy_id"], resources["api_key_id"], item["session_id"]
        )
        item["db_validation"] = validate_rows(item)
        item["selected_account_ids"] = db_account_ids(
            resources["group_id"], resources["strategy_id"], resources["api_key_id"], item["session_id"]
        )
        item["success_rounds"] = sum(r["status"] == "success" for r in item["rounds"])
        item["tool_rounds"] = sum("Read" in r.get("tools", []) and "Bash" in r.get("tools", []) for r in item["rounds"])
        RESULT_PATH.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    result["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    RESULT_PATH.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"result_path": str(RESULT_PATH), "strategies": len(result["strategies"])}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"[matrix] ERROR: {exc}", file=sys.stderr)
        raise

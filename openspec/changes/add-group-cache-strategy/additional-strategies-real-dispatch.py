#!/usr/bin/env python3
"""Real scheduler validation for the additional cache strategy templates.

The script uses the already running local service and imported Kiro accounts.
It never inserts usage rows directly. Temporary strategies are deleted after
the run; the test API key is retained only long enough to query its records.
"""

from __future__ import annotations

import argparse
import copy
import json
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

BASE = "http://127.0.0.1:48780"
ADMIN_EMAIL = "admin@sub2api.local"
ADMIN_PASSWORD = "CacheLocal!2026"
USER_EMAIL = "real-cache-1788281453@sub2api.local"
USER_PASSWORD = "RealCache!2026"
GROUP_ID = 7
MODEL = "claude-sonnet-4-5-20250929"
KEY = "sk-additional-cache-20260902-1788305099"
KEY_ID = 16
ROUNDS = 10
DEFAULT_STRATEGIES = (
    "strict_client",
    "shared_session",
    "conservative_usage",
    "long_context_guard",
)


def request(method: str, path: str, payload=None, token: str | None = None, timeout=300):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if path.endswith("/messages"):
        headers["anthropic-version"] = "2023-06-01"
    body = None if payload is None else json.dumps(payload, separators=(",", ":")).encode()
    req = urllib.request.Request(BASE + path, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            raw = response.read().decode()
            try:
                value = json.loads(raw)
            except Exception:
                value = raw
            return response.status, value, dict(response.headers)
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode()
        try:
            value = json.loads(raw)
        except Exception:
            value = raw
        return exc.code, value, dict(exc.headers)


def unwrap(value):
    if isinstance(value, dict) and "data" in value:
        return value["data"]
    return value


def number(value) -> int:
    try:
        return int(value or 0)
    except Exception:
        return 0


def usage_summary(usage: dict | None):
    usage = usage or {}
    creation = usage.get("cache_creation") or {}
    return {
        "input": number(usage.get("input_tokens")),
        "read": number(usage.get("cache_read_input_tokens")),
        "creation": number(usage.get("cache_creation_input_tokens")),
        "creation_5m": number(creation.get("ephemeral_5m_input_tokens")),
        "creation_1h": number(creation.get("ephemeral_1h_input_tokens")),
        "output": number(usage.get("output_tokens")),
    }


def stable_context(chars=120_000):
    lines = []
    total = 0
    index = 0
    while total < chars:
        value = ((index + 31) * 2654435761) & 0xFFFFFFFF
        line = (
            f"const repository_context_field_{index:07d} = "
            f'"value_{value:08x}_stable_{(value ^ 0x9E3779B9):08x}";\n'
        )
        lines.append(line)
        total += len(line)
        index += 1
    return "".join(lines)[:chars]


_stable_snapshot = stable_context()
STABLE_SYSTEM = [
    {
        "type": "text",
        "text": (
            "You are a coding assistant validating a production cache policy. "
            "Treat the following repository snapshot as stable context. "
            "Each turn must perform a concrete engineering task, preserve decisions "
            "from prior turns, and return a concise implementation result.\n\n"
            + _stable_snapshot[offset : offset + 12_000]
        ),
    }
    for offset in range(0, len(_stable_snapshot), 12_000)
]


def extract_text(body):
    if not isinstance(body, dict):
        return ""
    text = []
    for item in body.get("content") or []:
        if isinstance(item, dict) and item.get("type") == "text":
            text.append(item.get("text") or "")
    return "\n".join(text)


def system_for_strategy(strategy_name: str):
    """Return stable Claude Code system blocks with explicit cache markers.

    Each block is ~3k tokens, so the explicit breakpoint stays below the
    creation caps used by the additional templates.  The marker count is
    intentionally different per strategy so the test proves both a single
    cached prefix and a longer multi-breakpoint prefix.
    """
    blocks = copy.deepcopy(STABLE_SYSTEM)
    marker_count = {
        "shared_session": 1,
        "long_context_guard": 2,
    }.get(strategy_name, 0)
    for block in blocks[:marker_count]:
        block["cache_control"] = {"type": "ephemeral", "ttl": "5m"}
    return blocks


def message_body(
    strategy_name: str, session: str, round_no: int, history: list[dict]
):
    messages = list(history)
    messages.append(
        {
            "role": "user",
            "content": (
                f"Round {round_no}: inspect the cache-strategy implementation, "
                "identify one concrete risk in the current design, and propose "
                "a bounded fix with acceptance criteria. Keep the repository "
                "context stable and build on the previous answer."
            ),
        }
    )
    return {
        "model": MODEL,
        "max_tokens": 512,
        "system": system_for_strategy(strategy_name),
        "messages": messages,
        "metadata": {"session_id": session},
        "stream": False,
    }


def base_config(admin_token: str):
    status, body, _ = request("GET", "/api/v1/admin/cache-strategies", token=admin_token)
    if status != 200:
        raise RuntimeError(f"list strategies failed: {status} {body}")
    items = unwrap(body)
    if not items:
        raise RuntimeError("no existing strategy to derive normalized config")
    return copy.deepcopy(next(item["config"] for item in items if item["id"] == 5))


def usage_rows(admin_token: str):
    status, body, _ = request(
        "GET",
        f"/api/v1/admin/usage?api_key_id={KEY_ID}&group_id={GROUP_ID}"
        "&start_date=2026-09-01&end_date=2026-09-03&timezone=Asia/Shanghai"
        "&page=1&page_size=200&exact_total=true",
        token=admin_token,
    )
    data = unwrap(body)
    rows = data.get("items", []) if isinstance(data, dict) else []
    return status, rows


def strategy_configs(base):
    configs = {}

    strict = copy.deepcopy(base)
    strict.update(
        {
            "kind": "prefix",
            "ratio_mode": "uniform",
            "coverage_ratio": 0.65,
            "usage_ratio": 0.8,
            "read_ratio": 0.8,
            "creation_ratio": 0.8,
            "breakpoint_mode": "client_only",
            "max_coverage_tokens": 4096,
            "max_new_creation_tokens_per_request": 2048,
            "incremental_create_enabled": False,
        }
    )
    strict["usage"]["cache_read"] = {
        "mode": "sample_max",
        "max_tokens": 4096,
        "target_tokens": 0,
        "normal_max_multiplier": 1.1,
        "move_delta_to_cache_read": False,
    }
    strict["usage"]["cache_creation"] = {
        "mode": "sample_max",
        "max_tokens": 2048,
        "target_tokens": 0,
        "normal_max_multiplier": 1.1,
        "move_delta_to_cache_read": False,
    }
    strict["usage"]["final_cache_read_max_tokens"] = 4096
    strict["usage"]["final_cache_creation_max_tokens"] = 2048
    configs["strict_client"] = strict

    shared = copy.deepcopy(base)
    shared.update(
        {
            "kind": "prefix",
            "ratio_mode": "independent",
            "coverage_ratio": 0.9,
            "usage_ratio": 1,
            "read_ratio": 0.75,
            "creation_ratio": 0.6,
            "scope_mode": "group_session",
            "allow_derived_session": True,
            "max_coverage_tokens": 8192,
            "max_new_creation_tokens_per_request": 4096,
        }
    )
    shared["usage"]["cache_read"] = {
        "mode": "sample_target",
        "max_tokens": 0,
        "target_tokens": 1800,
        "normal_max_multiplier": 1.3,
        "move_delta_to_cache_read": False,
    }
    shared["usage"]["cache_creation"] = {
        "mode": "sample_target",
        "max_tokens": 0,
        "target_tokens": 1200,
        "normal_max_multiplier": 1.25,
        "move_delta_to_cache_read": False,
    }
    shared["usage"]["final_cache_read_max_tokens"] = 4096
    shared["usage"]["final_cache_creation_max_tokens"] = 4096
    configs["shared_session"] = shared

    conservative = copy.deepcopy(base)
    conservative.update(
        {
            "kind": "tool_aware",
            "ratio_mode": "independent",
            "coverage_ratio": 0.72,
            "usage_ratio": 0.7,
            "read_ratio": 0.7,
            "creation_ratio": 0.7,
            "token_scale": 1.05,
            "scale_min_input_tokens": 12000,
            "max_simulated_input_tokens": 64000,
        }
    )
    conservative["usage"]["input"] = {
        "mode": "sample_target",
        "max_tokens": 0,
        "target_tokens": 900,
        "normal_max_multiplier": 1.35,
        "move_delta_to_cache_read": False,
    }
    conservative["usage"]["output"] = {
        "mode": "sample_target",
        "max_tokens": 0,
        "target_tokens": 96,
        "normal_max_multiplier": 1.5,
        "move_delta_to_cache_read": False,
    }
    conservative["usage"]["cache_read"] = {
        "mode": "sample_max",
        "max_tokens": 2400,
        "target_tokens": 0,
        "normal_max_multiplier": 1.1,
        "move_delta_to_cache_read": False,
    }
    conservative["usage"]["cache_creation"] = {
        "mode": "sample_max",
        "max_tokens": 1600,
        "target_tokens": 0,
        "normal_max_multiplier": 1.1,
        "move_delta_to_cache_read": False,
    }
    conservative["usage"]["final_cache_read_max_tokens"] = 2400
    conservative["usage"]["final_cache_creation_max_tokens"] = 1600
    conservative["usage"]["final_output_max_tokens"] = 512
    configs["conservative_usage"] = conservative

    long_guard = copy.deepcopy(base)
    long_guard.update(
        {
            "kind": "prefix",
            "coverage_ratio": 0.92,
            "max_coverage_tokens": 48000,
            "max_new_creation_tokens_per_request": 8192,
            "reported_input_min_tokens": 1024,
            "reported_input_max_tokens": 96000,
            "token_scale": 1.08,
            "scale_min_input_tokens": 24000,
            "max_simulated_input_tokens": 96000,
            "default_ttl_seconds": 600,
            "hour_ttl_seconds": 1800,
            "max_entries_per_scope": 64,
        }
    )
    long_guard["usage"]["final_cache_read_max_tokens"] = 48000
    long_guard["usage"]["final_cache_creation_max_tokens"] = 8192
    long_guard["usage"]["final_output_max_tokens"] = 4096
    configs["long_context_guard"] = long_guard

    return configs


def main(selected_names: tuple[str, ...]):
    status, body, _ = request(
        "POST",
        "/api/v1/auth/login",
        {"email": ADMIN_EMAIL, "password": ADMIN_PASSWORD},
    )
    if status != 200:
        raise RuntimeError(f"admin login failed: {status} {body}")
    admin_token = body["data"]["access_token"]

    base = base_config(admin_token)
    configs = strategy_configs(base)
    unknown = [name for name in selected_names if name not in configs]
    if unknown:
        raise RuntimeError(f"unknown strategy names: {', '.join(unknown)}")
    configs = {name: configs[name] for name in selected_names}
    result = {
        "service": BASE,
        "group_id": GROUP_ID,
        "api_key_id": KEY_ID,
        "model": MODEL,
        "rounds_per_strategy": ROUNDS,
        "strategies": {},
    }
    created_ids = []

    try:
        for name, config in configs.items():
            payload = {
                "name": f"验收-{name}-真实十轮",
                "description": "新增缓存策略模板真实调度验收",
                "enabled": True,
                "config": config,
            }
            status, body, _ = request(
                "POST", "/api/v1/admin/cache-strategies", payload, token=admin_token
            )
            if status not in (200, 201):
                raise RuntimeError(f"create {name} failed: {status} {body}")
            strategy = unwrap(body)
            strategy_id = strategy["id"]
            created_ids.append(strategy_id)
            status, body, _ = request(
                "PUT",
                f"/api/v1/admin/cache-strategies/{strategy_id}/groups",
                {"group_ids": [GROUP_ID]},
                token=admin_token,
            )
            if status != 200:
                raise RuntimeError(f"bind {name} failed: {status} {body}")

            # Capture the latest persisted usage id before this strategy starts.
            # The worker is asynchronous and the API key may already have
            # historical rows, so waiting for "N rows" alone can compare
            # against old data and produce a false negative.
            status, baseline_rows = usage_rows(admin_token)
            if status != 200:
                raise RuntimeError(f"baseline usage query failed: {status}")
            baseline_max_id = max((number(row.get("id")) for row in baseline_rows), default=0)

            session = f"additional-{name}-{int(time.time())}"
            history: list[dict] = []
            calls = []
            for round_no in range(1, ROUNDS + 1):
                status, body, headers = request(
                    "POST",
                    "/v1/messages",
                    message_body(name, session, round_no, history),
                    token=KEY,
                    timeout=360,
                )
                usage = body.get("usage") if isinstance(body, dict) else None
                item = {
                    "round": round_no,
                    "status": status,
                    "usage": usage_summary(usage),
                    "request_id": headers.get("X-Request-ID") or headers.get("X-Request-Id"),
                }
                if status != 200:
                    item["error"] = body
                calls.append(item)
                print(json.dumps({"strategy": name, **item}, ensure_ascii=False), flush=True)
                if status != 200:
                    break
                answer = extract_text(body)
                history.append(
                    {
                        "role": "user",
                        "content": (
                            f"Round {round_no}: inspect the cache-strategy implementation, "
                            "identify one concrete risk in the current design, and propose "
                            "a bounded fix with acceptance criteria. Keep the repository "
                            "context stable and build on the previous answer."
                        ),
                    }
                )
                history.append({"role": "assistant", "content": answer})

            # Usage records are persisted by the asynchronous usage worker.
            # Wait until this strategy's successful rows are visible before
            # comparing response and database values.
            rows = []
            expected_rows = len([item for item in calls if item["status"] == 200])
            for _ in range(30):
                status, rows = usage_rows(admin_token)
                new_rows = [
                    row for row in rows if number(row.get("id")) > baseline_max_id
                ]
                if len(new_rows) >= expected_rows:
                    break
                time.sleep(1)
            response_usage = [item["usage"] for item in calls if item["status"] == 200]
            # Rows are newest-first. Filter by the baseline id first, then
            # reverse the newest N rows into request order.
            current_rows = list(
                reversed(
                    sorted(new_rows, key=lambda row: number(row.get("id")), reverse=True)[
                        :expected_rows
                    ]
                )
            )
            db_usage = [
                {
                    "input": number(row.get("input_tokens")),
                    "read": number(row.get("cache_read_tokens")),
                    "creation": number(row.get("cache_creation_tokens")),
                    "creation_5m": number(row.get("cache_creation_5m_tokens")),
                    "creation_1h": number(row.get("cache_creation_1h_tokens")),
                    "output": number(row.get("output_tokens")),
                }
                for row in current_rows
            ]
            # Usage rows are returned newest-first; compare the request sequence
            # after reversing the filtered result.
            all_nonnegative = all(
                all(value >= 0 for value in usage.values()) for usage in response_usage
            )
            conservation = all(
                usage["creation"] == usage["creation_5m"] + usage["creation_1h"]
                for usage in response_usage
            )
            within_context = all(
                usage["input"] + usage["read"] + usage["creation"] <= 1_000_000
                for usage in response_usage
            )
            bound = config
            max_read = bound["usage"].get("final_cache_read_max_tokens", 0)
            max_creation = bound["usage"].get("final_cache_creation_max_tokens", 0)
            max_output = bound["usage"].get("final_output_max_tokens", 0)
            configured_bounds = all(
                (not max_read or usage["read"] <= max_read)
                and (not max_creation or usage["creation"] <= max_creation)
                and (not max_output or usage["output"] <= max_output)
                for usage in response_usage
            )
            first_read_zero = not response_usage or response_usage[0]["read"] == 0
            response_db_match = status == 200 and response_usage == db_usage
            result["strategies"][name] = {
                "strategy_id": strategy_id,
                "calls": calls,
                "usage_record_count": len(current_rows),
                "response_db_match": response_db_match and len(current_rows) == len(response_usage),
                "first_read_zero": first_read_zero,
                "all_nonnegative": all_nonnegative,
                "creation_breakdown_conserved": conservation,
                "within_context_limit": within_context,
                "configured_bounds": configured_bounds,
                "final": (
                    all(item["status"] == 200 for item in calls)
                    and len(calls) == ROUNDS
                    and response_db_match
                    and first_read_zero
                    and all_nonnegative
                    and conservation
                    and within_context
                    and configured_bounds
                ),
            }

            # Unbind before moving to the next temporary strategy. This keeps
            # the one-group/one-strategy invariant explicit.
            request(
                "PUT",
                f"/api/v1/admin/cache-strategies/{strategy_id}/groups/replace",
                {"group_ids": []},
                token=admin_token,
            )
            request(
                "DELETE",
                f"/api/v1/admin/cache-strategies/{strategy_id}",
                token=admin_token,
            )

    finally:
        for strategy_id in created_ids:
            request(
                "PUT",
                f"/api/v1/admin/cache-strategies/{strategy_id}/groups/replace",
                {"group_ids": []},
                token=admin_token,
            )
            request(
                "DELETE",
                f"/api/v1/admin/cache-strategies/{strategy_id}",
                token=admin_token,
            )

    Path("/tmp/sub2api_additional_strategy_results.json").write_text(
        json.dumps(result, ensure_ascii=False, indent=2)
    )
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if not all(item["final"] for item in result["strategies"].values()):
        return 1
    return 0


if __name__ == "__main__":
    try:
        parser = argparse.ArgumentParser()
        parser.add_argument(
            "--strategies",
            default=",".join(DEFAULT_STRATEGIES),
            help="Comma-separated additional template ids to execute.",
        )
        args = parser.parse_args()
        names = tuple(
            name.strip() for name in args.strategies.split(",") if name.strip()
        )
        if not names:
            raise RuntimeError("at least one strategy is required")
        sys.exit(main(names))
    except Exception as exc:
        print(f"FATAL: {exc}", file=sys.stderr)
        sys.exit(2)

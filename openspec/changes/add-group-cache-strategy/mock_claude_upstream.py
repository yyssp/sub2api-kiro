#!/usr/bin/env python3
"""Small Claude Code compatible upstream used by the local E2E smoke test."""

import json
import sys
import threading
from urllib.parse import parse_qs, urlparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


failure_lock = threading.Lock()
failure_counts = {}
request_count = 0
upstream_mode = "raw"


def response_text_for_tokens(tokens):
    """Return compact, readable text with roughly four bytes per token."""
    unit = "cache usage evidence remains internally consistent. "
    target_bytes = max(tokens, 1) * 4
    return (unit * ((target_bytes // len(unit)) + 1))[:target_bytes]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stdout.write("[mock-upstream] " + (fmt % args) + "\n")
        sys.stdout.flush()

    def _read_json(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b"{}"
        try:
            return json.loads(raw.decode("utf-8"))
        except Exception:
            return {}

    def _send_json(self, status, body):
        encoded = json.dumps(body, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(encoded)

    def _send_sse(self, events):
        chunks = []
        for event, data in events:
            # Chat Completions uses a raw `[DONE]` sentinel, not a JSON string.
            # Keep normal event payloads compact and JSON encoded.
            encoded_data = data if data == "[DONE]" else json.dumps(data, separators=(",", ":"))
            chunks.append("event: %s\ndata: %s\n\n" % (event, encoded_data))
        payload = "".join(chunks).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        if self.path.startswith("/v1/models"):
            self._send_json(200, {"object": "list", "data": [{"id": "claude-sonnet-4-6", "object": "model"}]})
            return
        if self.path.startswith("/__control"):
            query = parse_qs(urlparse(self.path).query)
            mode = query.get("mode", [""])[0].strip().lower()
            self._set_mode(mode)
            return
        self._send_json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        global request_count, upstream_mode
        body = self._read_json()
        path = self.path.split("?", 1)[0]
        if path == "/__control":
            mode = str(body.get("mode", "")).strip().lower() if isinstance(body, dict) else ""
            self._set_mode(mode)
            return
        model = body.get("model")
        with failure_lock:
            call_index = request_count
            request_count += 1
        session = ""
        metadata = body.get("metadata")
        if isinstance(metadata, dict):
            session = metadata.get("session_id") or metadata.get("conversation_id") or ""
        self.log_message("call=%d path=%s model=%s session=%s stream=%s bytes=%d",
                         call_index, path, model, session, body.get("stream", False),
                         len(json.dumps(body, separators=(",", ":"))))
        with failure_lock:
            mode = upstream_mode
        fail_once = False
        if mode == "fail_all":
            fail_once = True
        elif mode == "fail_once" and model == "fail-once-model":
            with failure_lock:
                fail_once = failure_counts.get(model, 0) == 0
                failure_counts[model] = failure_counts.get(model, 0) + 1
        if model == "fail-model" or fail_once:
            self._send_json(500, {"error": {"type": "server_error", "message": "intentional mock failure"}})
            return

        # Derive usage from the actual request body instead of returning a
        # fixed toy value.  This lets the black-box tests exercise the
        # gateway's large-context guards with requests ranging from a few
        # hundred tokens to several hundred thousand tokens.  The gateway's
        # local cache strategy is still responsible for projecting cache
        # buckets; the mock deliberately reports no authoritative cache usage
        # unless the control endpoint selected ``authoritative`` mode.
        body_size = len(json.dumps(body, separators=(",", ":")))
        input_tokens = min(900000, max(24, body_size // 4 + 120 + (call_index % 7) * 11))

        # Claude Code clients use ``max_tokens`` while OpenAI-compatible
        # clients use ``max_output_tokens``.  Emit a varied but bounded
        # completion so tests can distinguish the configured output cap from
        # an invariant mock value.
        requested_output = 256
        for output_key in ("max_tokens", "max_output_tokens"):
            value = body.get(output_key)
            if isinstance(value, (int, float)) and value > 0:
                requested_output = int(value)
                break
        output_tokens = min(
            8192,
            requested_output,
            max(16, int(requested_output * 0.68) + 7 + (call_index % 5) * 23),
        )
        response_text = response_text_for_tokens(output_tokens)
        authoritative_read = 0
        authoritative_creation = 0
        if mode == "authoritative":
            # Deliberately small, non-constant buckets. These values are
            # authoritative upstream accounting and must not be added to the
            # gateway's local projection a second time.
            authoritative_read = 7 + (call_index % 3) * 2
            authoritative_creation = 3 if call_index % 2 == 0 else 0

        if path.endswith("/messages"):
            if body.get("stream"):
                self._send_sse([
                    ("message_start", {"type": "message_start", "message": {"id": "msg_mock_%d" % call_index, "type": "message", "role": "assistant", "model": body.get("model", "claude-sonnet-4-6"), "content": [], "usage": {"input_tokens": input_tokens, "cache_read_input_tokens": authoritative_read, "cache_creation_input_tokens": authoritative_creation, "output_tokens": 0}}}),
                    ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                    ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": response_text}}),
                    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                    ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"input_tokens": input_tokens, "cache_read_input_tokens": authoritative_read, "cache_creation_input_tokens": authoritative_creation, "output_tokens": output_tokens}}),
                    ("message_stop", {"type": "message_stop"}),
                ])
            else:
                self._send_json(200, {"id": "msg_mock_%d" % call_index, "type": "message", "role": "assistant", "model": body.get("model", "claude-sonnet-4-6"), "content": [{"type": "text", "text": response_text}], "stop_reason": "end_turn", "usage": {"input_tokens": input_tokens, "cache_read_input_tokens": authoritative_read, "cache_creation_input_tokens": authoritative_creation, "output_tokens": output_tokens}})
            return

        if path.endswith("/chat/completions"):
            prompt_details = {}
            if mode == "authoritative":
                prompt_details = {"cached_tokens": authoritative_read, "cache_creation_tokens": authoritative_creation}
            response = {"id": "chatcmpl_mock_%d" % call_index, "object": "chat.completion", "created": 1, "model": body.get("model", "gpt-4o"), "choices": [{"index": 0, "message": {"role": "assistant", "content": response_text}, "finish_reason": "stop"}], "usage": {"prompt_tokens": input_tokens, "completion_tokens": output_tokens, "total_tokens": input_tokens + output_tokens}}
            if prompt_details:
                response["usage"]["prompt_tokens_details"] = prompt_details
            if body.get("stream"):
                self._send_sse([
                    ("", {"id": "chatcmpl_mock", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"role": "assistant", "content": response_text}, "finish_reason": None}]}),
                    ("", {"id": "chatcmpl_mock", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": response["usage"]}),
                    ("", "[DONE]"),
                ])
            else:
                self._send_json(200, response)
            return

        if path.endswith("/responses"):
            response_usage = {"input_tokens": input_tokens, "output_tokens": output_tokens, "total_tokens": input_tokens + output_tokens, "input_tokens_details": {"cached_tokens": authoritative_read}}
            if mode == "authoritative":
                response_usage["cache_creation_input_tokens"] = authoritative_creation
            response = {"id": "resp_mock_%d" % call_index, "object": "response", "status": "completed", "model": body.get("model", "gpt-4o"), "output": [{"type": "message", "id": "msg_mock_%d" % call_index, "role": "assistant", "content": [{"type": "output_text", "text": response_text}]}], "usage": response_usage}
            if body.get("stream"):
                self._send_sse([
                    ("response.created", {"type": "response.created", "response": {"id": "resp_mock", "object": "response", "status": "in_progress", "model": response["model"]}}),
                    ("response.output_text.delta", {"type": "response.output_text.delta", "delta": response_text}),
                    ("response.completed", {"type": "response.completed", "response": response}),
                ])
            else:
                self._send_json(200, response)
            return

        self._send_json(404, {"error": {"message": "unsupported path", "path": path}})

    def _set_mode(self, mode):
        global upstream_mode, failure_counts, request_count
        if mode not in {"raw", "authoritative", "fail_all", "fail_once"}:
            self._send_json(400, {"error": {"message": "mode must be raw, authoritative, fail_all or fail_once"}})
            return
        with failure_lock:
            upstream_mode = mode
            request_count = 0
            if mode == "fail_once":
                failure_counts.clear()
            if mode == "raw":
                failure_counts.clear()
        self._send_json(200, {"mode": mode})


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 19090
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print("[mock-upstream] listening on 127.0.0.1:%d" % port, flush=True)
    server.serve_forever()

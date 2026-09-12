package cursor

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type probeSSEWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	flushed chan struct{}
}

func newProbeSSEWriter() *probeSSEWriter {
	return &probeSSEWriter{
		header:  make(http.Header),
		flushed: make(chan struct{}, 1),
	}
}

func (w *probeSSEWriter) Header() http.Header {
	return w.header
}

func (w *probeSSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *probeSSEWriter) WriteHeader(int) {}

func (w *probeSSEWriter) Flush() {
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func (w *probeSSEWriter) Snapshot() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func TestCursorSSEHeartbeatIntervalConfig(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("CURSOR_SSE_HEARTBEAT_INTERVAL", "")
		if got := cursorSSEHeartbeatInterval(); got != defaultSSEHeartbeatInterval {
			t.Fatalf("interval=%s, want %s", got, defaultSSEHeartbeatInterval)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		for _, value := range []string{"0", "off", "disabled"} {
			t.Run(value, func(t *testing.T) {
				t.Setenv("CURSOR_SSE_HEARTBEAT_INTERVAL", value)
				if got := cursorSSEHeartbeatInterval(); got != 0 {
					t.Fatalf("interval=%s, want disabled", got)
				}
			})
		}
	})
	t.Run("clamped", func(t *testing.T) {
		t.Setenv("CURSOR_SSE_HEARTBEAT_INTERVAL", "250ms")
		if got := cursorSSEHeartbeatInterval(); got != minSSEHeartbeatInterval {
			t.Fatalf("short interval=%s, want %s", got, minSSEHeartbeatInterval)
		}
		t.Setenv("CURSOR_SSE_HEARTBEAT_INTERVAL", "120s")
		if got := cursorSSEHeartbeatInterval(); got != maxSSEHeartbeatInterval {
			t.Fatalf("long interval=%s, want %s", got, maxSSEHeartbeatInterval)
		}
	})
	t.Run("invalid falls back", func(t *testing.T) {
		t.Setenv("CURSOR_SSE_HEARTBEAT_INTERVAL", "not-a-duration")
		if got := cursorSSEHeartbeatInterval(); got != defaultSSEHeartbeatInterval {
			t.Fatalf("interval=%s, want %s", got, defaultSSEHeartbeatInterval)
		}
	})
}

func TestSSEHeartbeatIsCommentOnlyAndStops(t *testing.T) {
	probe := newProbeSSEWriter()
	writer := newLockedSSEWriter(probe)
	ctx, cancel := context.WithCancel(context.Background())
	stop := writer.startSSEHeartbeat(ctx, 5*time.Millisecond)

	select {
	case <-probe.flushed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("heartbeat did not flush")
	}
	cancel()
	stop()
	stop()

	body := probe.Snapshot()
	if !strings.Contains(body, ": keepalive\n\n") {
		t.Fatalf("heartbeat missing: %q", body)
	}
	if strings.Contains(body, "event:") || strings.Contains(body, "data:") {
		t.Fatalf("heartbeat must be an SSE comment, got %q", body)
	}
	before := len(body)
	time.Sleep(20 * time.Millisecond)
	after := len(probe.Snapshot())
	if after != before {
		t.Fatalf("heartbeat continued after stop: before=%d after=%d", before, after)
	}
}

func TestWriteSSEAndHeartbeatDoNotInterleave(t *testing.T) {
	probe := newProbeSSEWriter()
	writer := newLockedSSEWriter(probe)
	ctx, cancel := context.WithCancel(context.Background())
	stop := writer.startSSEHeartbeat(ctx, 5*time.Millisecond)
	writeSSE(writer, writer, []byte("event: test\ndata: payload\n\n"))
	cancel()
	stop()

	body := probe.Snapshot()
	if !strings.Contains(body, "event: test\ndata: payload\n\n") {
		t.Fatalf("event missing: %q", body)
	}
	for _, chunk := range strings.Split(body, "\n\n") {
		if chunk == "" {
			continue
		}
		if chunk != ": keepalive" && chunk != "event: test\ndata: payload" {
			t.Fatalf("unexpected interleaved SSE chunk %q in %q", chunk, body)
		}
	}
}

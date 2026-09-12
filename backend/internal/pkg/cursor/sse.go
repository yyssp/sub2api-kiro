package cursor

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultSSEHeartbeatInterval = 15 * time.Second
	minSSEHeartbeatInterval     = time.Second
	maxSSEHeartbeatInterval     = 90 * time.Second
)

// lockedSSEWriter serializes application SSE events and transport heartbeats.
// net/http ResponseWriter does not permit concurrent writes.
type lockedSSEWriter struct {
	http.ResponseWriter
	mu sync.Mutex
}

func newLockedSSEWriter(w http.ResponseWriter) *lockedSSEWriter {
	return &lockedSSEWriter{ResponseWriter: w}
}

func (w *lockedSSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

func (w *lockedSSEWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *lockedSSEWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

func (w *lockedSSEWriter) flushLocked() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *lockedSSEWriter) writeAndFlush(p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.ResponseWriter.Write(p); err != nil {
		return err
	}
	w.flushLocked()
	return nil
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, payload []byte) {
	if locked, ok := w.(*lockedSSEWriter); ok {
		_ = locked.writeAndFlush(payload)
		return
	}
	_, _ = w.Write(payload)
	if flusher != nil {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach optional capabilities on the
// underlying writer without bypassing this writer for normal SSE output.
func (w *lockedSSEWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// cursorSSEHeartbeatInterval reads the interval per request so deployments can
// change it without rebuilding. Invalid values fall back to the safe default.
// "0", "off", and "disabled" explicitly disable heartbeats.
func cursorSSEHeartbeatInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CURSOR_SSE_HEARTBEAT_INTERVAL"))
	if raw == "" {
		return defaultSSEHeartbeatInterval
	}
	switch strings.ToLower(raw) {
	case "0", "off", "disabled":
		return 0
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		return defaultSSEHeartbeatInterval
	}
	if interval < minSSEHeartbeatInterval {
		return minSSEHeartbeatInterval
	}
	if interval > maxSSEHeartbeatInterval {
		return maxSSEHeartbeatInterval
	}
	return interval
}

// startSSEHeartbeat writes only standard SSE comments. Comments are ignored by
// SSE clients and are intentionally excluded from model text, usage, and cost.
// The returned stop function is idempotent and waits for the goroutine to exit.
func (w *lockedSSEWriter) startSSEHeartbeat(ctx context.Context, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := w.writeAndFlush([]byte(": keepalive\n\n")); err != nil {
					return
				}
			case <-ctx.Done():
				return
			case <-stop:
				return
			}
		}
	}()
	return func() {
		once.Do(func() { close(stop) })
		<-finished
	}
}

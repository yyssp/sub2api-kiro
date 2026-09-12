package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type requestTraceKey struct{}

// requestTrace 是一次入站请求在各协议适配层之间共享的关联信息。
// 指针放入 context，尝试次数会随同一请求递增；不会把原始会话值写入日志。
type requestTrace struct {
	ID        string
	SessionID string
	Attempt   int
	// Stream 是入站请求的 stream 参数。放在 trace 而不是逐层传参: 它和
	// ID/SessionID 一样是"整个请求的属性", 而写日志的 record* 函数有十余处
	// 调用点, 加参数要改一大片签名。
	Stream bool
}

// setRequestStream 记录本次入站请求是否为流式。适配层解析出 stream 后调用一次。
func setRequestStream(ctx context.Context, stream bool) {
	if trace := requestTraceFromContext(ctx); trace != nil {
		trace.Stream = stream
	}
}

func withRequestTrace(ctx context.Context, requestID, sessionID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestTraceKey{}, &requestTrace{
		ID:        requestID,
		SessionID: safeTraceSession(sessionID),
	})
}

func requestTraceFromContext(ctx context.Context) *requestTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(requestTraceKey{}).(*requestTrace)
	return trace
}

func setRequestAttempt(ctx context.Context, attempt int) {
	if trace := requestTraceFromContext(ctx); trace != nil {
		trace.Attempt = attempt
	}
}

func safeTraceSession(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "session_" + hex.EncodeToString(sum[:8])
}

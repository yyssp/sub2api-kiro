//go:build unit

package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// syncBuffer 是并发安全的写入目标，用来在 race 检测下观察心跳与内容事件的交错。
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// 头发完就必须立刻写出前导注释，不能等第一次 tick。
//
// agent.v1 首字延迟常达数十秒（协议层硬墙 90s），而反代的空闲窗口通常是 100s，
// 且 message_start 被刻意延后到第一个内容才发。不写前导字节的话，
// 慢但正常的请求会在反代处被掐断，客户端看到连接重置而不是「慢」。
func TestPreStreamHeartbeat_WritesPreambleImmediately(t *testing.T) {
	var buf syncBuffer
	e := newCursorAnthropicEmitter(func(string, any) error { return nil }, "claude-sonnet-4.5", time.Now())

	stop := e.startPreStreamHeartbeat(context.Background(), &buf, time.Hour)
	defer stop()

	require.Equal(t, ": processing\n\n", buf.String(),
		"头发完后没有立刻写出前导 SSE 注释")
}

// 首字未到达时必须周期性续写，维持连接活跃。
func TestPreStreamHeartbeat_KeepsWritingUntilFirstEvent(t *testing.T) {
	var buf syncBuffer
	e := newCursorAnthropicEmitter(func(string, any) error { return nil }, "claude-sonnet-4.5", time.Now())

	stop := e.startPreStreamHeartbeat(context.Background(), &buf, 10*time.Millisecond)
	defer stop()

	require.Eventually(t, func() bool {
		return strings.Count(buf.String(), ": processing") >= 3
	}, 2*time.Second, 5*time.Millisecond, "心跳没有持续续写，长首字等待会被反代掐断")
}

// 首个真实事件开始推送后，心跳必须停：此后流本身在持续产生字节，
// 继续插注释只是噪音。
func TestPreStreamHeartbeat_StopsOnceStreamStarted(t *testing.T) {
	var buf syncBuffer
	e := newCursorAnthropicEmitter(func(string, any) error { return nil }, "claude-sonnet-4.5", time.Now())

	stop := e.startPreStreamHeartbeat(context.Background(), &buf, 10*time.Millisecond)
	defer stop()

	// 模拟首个真实事件到达。
	e.OnText("hello")

	require.Eventually(t, func() bool {
		before := strings.Count(buf.String(), ": processing")
		time.Sleep(60 * time.Millisecond)
		return strings.Count(buf.String(), ": processing") == before
	}, 2*time.Second, 20*time.Millisecond, "首字到达后心跳仍在写")
}

// stop 必须幂等且能真正终止 goroutine；ctx 取消同样要能终止。
func TestPreStreamHeartbeat_StopIsIdempotent(t *testing.T) {
	var buf syncBuffer
	e := newCursorAnthropicEmitter(func(string, any) error { return nil }, "claude-sonnet-4.5", time.Now())

	stop := e.startPreStreamHeartbeat(context.Background(), &buf, 10*time.Millisecond)
	stop()
	require.NotPanics(t, stop, "重复调用 stop 不应 panic")

	time.Sleep(50 * time.Millisecond)
	before := strings.Count(buf.String(), ": processing")
	time.Sleep(60 * time.Millisecond)
	require.Equal(t, before, strings.Count(buf.String(), ": processing"),
		"stop 之后心跳仍在写，goroutine 泄漏")
}

func TestPreStreamHeartbeat_CancelledContextStops(t *testing.T) {
	var buf syncBuffer
	e := newCursorAnthropicEmitter(func(string, any) error { return nil }, "claude-sonnet-4.5", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	stop := e.startPreStreamHeartbeat(ctx, &buf, 10*time.Millisecond)
	defer stop()
	cancel()

	time.Sleep(50 * time.Millisecond)
	before := strings.Count(buf.String(), ": processing")
	time.Sleep(60 * time.Millisecond)
	require.Equal(t, before, strings.Count(buf.String(), ": processing"),
		"ctx 取消后心跳仍在写")
}

// 间隔 <= 0 表示关闭心跳：此时连前导注释都不该写。
func TestPreStreamHeartbeat_DisabledWhenIntervalNonPositive(t *testing.T) {
	var buf syncBuffer
	e := newCursorAnthropicEmitter(func(string, any) error { return nil }, "claude-sonnet-4.5", time.Now())

	stop := e.startPreStreamHeartbeat(context.Background(), &buf, 0)
	defer stop()

	require.Empty(t, buf.String())
}

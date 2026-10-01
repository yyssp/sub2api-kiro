package kiro

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildExceptionFrame 按 AWS eventstream 异常帧格式构造：
// 只有 :message-type=exception 与 :exception-type，没有 :event-type。
func buildExceptionFrame(t *testing.T, exceptionType, message string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"message": message})
	require.NoError(t, err)

	headers := bytes.NewBuffer(nil)
	writeHeader := func(name, value string) {
		_ = headers.WriteByte(byte(len(name)))
		_, _ = headers.WriteString(name)
		_ = headers.WriteByte(7)
		require.NoError(t, binary.Write(headers, binary.BigEndian, uint16(len(value))))
		_, _ = headers.WriteString(value)
	}
	writeHeader(":message-type", "exception")
	writeHeader(":exception-type", exceptionType)
	writeHeader(":content-type", "application/json")

	totalLength := uint32(12 + headers.Len() + len(payload) + 4)
	frame := bytes.NewBuffer(nil)
	require.NoError(t, binary.Write(frame, binary.BigEndian, totalLength))
	require.NoError(t, binary.Write(frame, binary.BigEndian, uint32(headers.Len())))
	require.NoError(t, binary.Write(frame, binary.BigEndian, uint32(0)))
	_, _ = frame.Write(headers.Bytes())
	_, _ = frame.Write(payload)
	require.NoError(t, binary.Write(frame, binary.BigEndian, uint32(0)))
	return frame.Bytes()
}

func textFrame(t *testing.T, text string) []byte {
	return buildEventStreamFrame(t, "assistantResponseEvent", map[string]any{
		"assistantResponseEvent": map[string]any{"content": text},
	})
}

func TestStreamContentLengthExceededAfterOutputIsMaxTokens(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(textFrame(t, "partial answer"))
	_, _ = stream.Write(buildExceptionFrame(t, kiroContentLengthExceededException, "Output exceeds limit"))

	var out bytes.Buffer
	result, err := StreamEventStreamAsAnthropicWithContext(context.Background(), stream, &out, "claude-sonnet-4-5", 9, KiroRequestContext{})
	require.NoError(t, err)
	require.Equal(t, "max_tokens", result.StopReason)
	require.Contains(t, out.String(), `"stop_reason":"max_tokens"`)
	require.Contains(t, out.String(), "partial answer")
}

func TestStreamExceptionMidStreamIsError(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(textFrame(t, "partial answer"))
	_, _ = stream.Write(buildExceptionFrame(t, "InternalServerException", "Encountered an unexpected error, please try again"))

	var out bytes.Buffer
	_, err := StreamEventStreamAsAnthropicWithContext(context.Background(), stream, &out, "claude-sonnet-4-5", 9, KiroRequestContext{})
	var streamErr *KiroStreamException
	require.True(t, errors.As(err, &streamErr), "got %v", err)
	require.Equal(t, "InternalServerException", streamErr.ExceptionType)
	require.NotContains(t, out.String(), "message_stop", "失败不得伪造正常收尾")
}

func TestStreamContentLengthExceededWithoutOutputIsError(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(buildExceptionFrame(t, kiroContentLengthExceededException, "Input too long"))

	var out bytes.Buffer
	_, err := StreamEventStreamAsAnthropicWithContext(context.Background(), stream, &out, "claude-sonnet-4-5", 9, KiroRequestContext{})
	var streamErr *KiroStreamException
	require.True(t, errors.As(err, &streamErr))
	require.Empty(t, out.String())
}

func TestNonStreamContentLengthExceededAfterOutputIsMaxTokens(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(textFrame(t, "partial answer"))
	_, _ = stream.Write(buildExceptionFrame(t, kiroContentLengthExceededException, "Output exceeds limit"))

	result, err := ParseNonStreamingEventStreamWithContext(stream, "claude-sonnet-4-5", KiroRequestContext{})
	require.NoError(t, err)
	require.Equal(t, "max_tokens", result.StopReason)
}

func TestNonStreamExceptionIsError(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(textFrame(t, "partial answer"))
	_, _ = stream.Write(buildExceptionFrame(t, "ThrottlingException", "Too many requests"))

	_, err := ParseNonStreamingEventStreamWithContext(stream, "claude-sonnet-4-5", KiroRequestContext{})
	var streamErr *KiroStreamException
	require.True(t, errors.As(err, &streamErr))
	require.Equal(t, "ThrottlingException", streamErr.ExceptionType)
	require.Equal(t, "Too many requests", streamErr.Message)
}

func TestStreamReportsWhetherUpstreamSentTerminalSignal(t *testing.T) {
	withoutStop := bytes.NewBuffer(nil)
	_, _ = withoutStop.Write(textFrame(t, "hello"))
	var out bytes.Buffer
	result, err := StreamEventStreamAsAnthropicWithContext(context.Background(), withoutStop, &out, "claude-sonnet-4-5", 9, KiroRequestContext{})
	require.NoError(t, err)
	require.False(t, result.UpstreamTerminalSignal)
	require.Equal(t, "end_turn", result.StopReason, "兜底推断保持不变，只增加可观测信号")

	withStop := bytes.NewBuffer(nil)
	_, _ = withStop.Write(textFrame(t, "hello"))
	_, _ = withStop.Write(buildEventStreamFrame(t, "metadataEvent", map[string]any{"metadataEvent": map[string]any{"stopReason": "END_TURN"}}))
	out.Reset()
	result, err = StreamEventStreamAsAnthropicWithContext(context.Background(), withStop, &out, "claude-sonnet-4-5", 9, KiroRequestContext{})
	require.NoError(t, err)
	require.True(t, result.UpstreamTerminalSignal)

	nonStream := bytes.NewBuffer(nil)
	_, _ = nonStream.Write(textFrame(t, "hello"))
	parsed, err := ParseNonStreamingEventStreamWithContext(nonStream, "claude-sonnet-4-5", KiroRequestContext{})
	require.NoError(t, err)
	require.False(t, parsed.UpstreamTerminalSignal)
}

func contextUsageFrame(t *testing.T, percent float64) []byte {
	return buildEventStreamFrame(t, "contextUsageEvent", map[string]any{"contextUsageEvent": map[string]any{"contextUsagePercentage": percent}})
}

func TestContextWindowFullMapsToModelContextWindowExceeded(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(textFrame(t, "partial"))
	_, _ = stream.Write(contextUsageFrame(t, 100))
	var out bytes.Buffer
	result, err := StreamEventStreamAsAnthropicWithContext(context.Background(), stream, &out, "claude-sonnet-4-5", 9, KiroRequestContext{})
	require.NoError(t, err)
	require.Equal(t, "model_context_window_exceeded", result.StopReason)

	nonStream := bytes.NewBuffer(nil)
	_, _ = nonStream.Write(textFrame(t, "partial"))
	_, _ = nonStream.Write(contextUsageFrame(t, 100.0))
	_, _ = nonStream.Write(buildEventStreamFrame(t, "metadataEvent", map[string]any{"metadataEvent": map[string]any{"stopReason": "END_TURN"}}))
	parsed, err := ParseNonStreamingEventStreamWithContext(nonStream, "claude-sonnet-4-5", KiroRequestContext{})
	require.NoError(t, err)
	require.Equal(t, "model_context_window_exceeded", parsed.StopReason)
}

func TestContextWindowBelowFullKeepsEndTurn(t *testing.T) {
	stream := bytes.NewBuffer(nil)
	_, _ = stream.Write(textFrame(t, "done"))
	_, _ = stream.Write(contextUsageFrame(t, 99.9))
	var out bytes.Buffer
	result, err := StreamEventStreamAsAnthropicWithContext(context.Background(), stream, &out, "claude-sonnet-4-5", 9, KiroRequestContext{})
	require.NoError(t, err)
	require.Equal(t, "end_turn", result.StopReason)
}

func TestApplyContextWindowStopReasonKeepsExplicitReasons(t *testing.T) {
	require.Equal(t, "max_tokens", applyContextWindowStopReason("max_tokens", true, false))
	require.Equal(t, "tool_use", applyContextWindowStopReason("tool_use", true, true))
	require.Equal(t, "end_turn", applyContextWindowStopReason("end_turn", true, true), "已发出工具调用时不改写")
	require.Equal(t, "model_context_window_exceeded", applyContextWindowStopReason("END_TURN", true, false))
}

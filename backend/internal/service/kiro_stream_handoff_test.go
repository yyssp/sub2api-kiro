//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func buildKiroExceptionFrameForTest(t *testing.T, exceptionType string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"message": "detail"})
	require.NoError(t, err)
	headers := bytes.NewBuffer(nil)
	for _, kv := range [][2]string{{":message-type", "exception"}, {":exception-type", exceptionType}} {
		_ = headers.WriteByte(byte(len(kv[0])))
		_, _ = headers.WriteString(kv[0])
		_ = headers.WriteByte(7)
		require.NoError(t, binary.Write(headers, binary.BigEndian, uint16(len(kv[1]))))
		_, _ = headers.WriteString(kv[1])
	}
	total := uint32(12 + headers.Len() + len(payload) + 4)
	frame := bytes.NewBuffer(nil)
	require.NoError(t, binary.Write(frame, binary.BigEndian, total))
	require.NoError(t, binary.Write(frame, binary.BigEndian, uint32(headers.Len())))
	require.NoError(t, binary.Write(frame, binary.BigEndian, uint32(0)))
	_, _ = frame.Write(headers.Bytes())
	_, _ = frame.Write(payload)
	require.NoError(t, binary.Write(frame, binary.BigEndian, uint32(0)))
	return frame.Bytes()
}

func newKiroStreamTestService(t *testing.T, upstreamBody []byte) (*GatewayService, *Account) {
	t.Helper()
	account := &Account{
		ID: 991001, Platform: PlatformKiro, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "k", "kiro_api_key": "k"},
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(upstreamBody))}
	svc := &GatewayService{
		httpUpstream:        &queuedHTTPUpstream{responses: []*http.Response{resp}},
		kiroCooldownStore:   &stubKiroCooldownStore{},
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
	return svc, account
}

func kiroStreamTestBody(t *testing.T) []byte {
	t.Helper()
	payload, err := createTestPayload("claude-sonnet-4-6")
	require.NoError(t, err)
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	return body
}

// 首字节前的限流异常必须变成 failover 错误换号，而不是给客户端一个必失败的 200。
func TestOpenKiroStreamFailsOverOnExceptionBeforeOutput(t *testing.T) {
	svc, account := newKiroStreamTestService(t, buildKiroExceptionFrameForTest(t, "ThrottlingException"))
	resp, _, err := svc.openKiroAnthropicStreamResponse(context.Background(), account, nil, kiroStreamTestBody(t), "claude-sonnet-4-6", "claude-sonnet-4-6", nil, nil, nil)
	require.Nil(t, resp)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "got %v", err)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
}

// 首字节前的输入超长要合成 400，交给既有的 prompt is too long 改写。
func TestOpenKiroStreamContentLengthBeforeOutputBecomesOversize400(t *testing.T) {
	svc, account := newKiroStreamTestService(t, buildKiroExceptionFrameForTest(t, "ContentLengthExceededException"))
	resp, _, err := svc.openKiroAnthropicStreamResponse(context.Background(), account, nil, kiroStreamTestBody(t), "claude-sonnet-4-6", "claude-sonnet-4-6", nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, kiroErrorBadRequestOversize, classifyKiroHTTPError(resp.StatusCode, string(body)).Category)
}

// 已有输出后的异常走 SSE error，流照常交给客户端。
func TestOpenKiroStreamExceptionAfterOutputUsesSSEError(t *testing.T) {
	var upstream bytes.Buffer
	_, _ = upstream.Write(buildKiroEventStreamFrame(t, "assistantResponseEvent", map[string]any{"assistantResponseEvent": map[string]any{"content": "partial"}}))
	_, _ = upstream.Write(buildKiroExceptionFrameForTest(t, "InternalServerException"))
	svc, account := newKiroStreamTestService(t, upstream.Bytes())

	resp, _, err := svc.openKiroAnthropicStreamResponse(context.Background(), account, nil, kiroStreamTestBody(t), "claude-sonnet-4-6", "claude-sonnet-4-6", nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(out), "partial")
	require.Contains(t, string(out), "event: error")
	require.NotContains(t, string(out), "message_stop")
}

func TestKiroStreamHandoffTimeoutHandsStreamToClient(t *testing.T) {
	h := newKiroStreamHandoff()
	require.NoError(t, h.wait(context.Background(), 10*time.Millisecond))
	require.False(t, h.failBeforeOutput(errors.New("late")), "超时交出流后的错误必须走 SSE")
}

func TestKiroStreamHandoffSuccessfulStream(t *testing.T) {
	var upstream bytes.Buffer
	_, _ = upstream.Write(buildKiroEventStreamFrame(t, "assistantResponseEvent", map[string]any{"assistantResponseEvent": map[string]any{"content": "hello"}}))
	svc, account := newKiroStreamTestService(t, upstream.Bytes())
	resp, _, err := svc.openKiroAnthropicStreamResponse(context.Background(), account, nil, kiroStreamTestBody(t), "claude-sonnet-4-6", "claude-sonnet-4-6", nil, nil, nil)
	require.NoError(t, err)
	out, _ := io.ReadAll(resp.Body)
	require.True(t, strings.Contains(string(out), "message_stop"))
}

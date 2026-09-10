package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type cacheProtocolHTTPUpstream struct {
	client *http.Client
}

func (u *cacheProtocolHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u *cacheProtocolHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.client.Do(req)
}

type cacheProtocolMockServer struct {
	mu    sync.Mutex
	calls int
}

func (m *cacheProtocolMockServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if bytes.Contains(body, []byte(`FAIL_CACHE`)) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"mock failure"}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("x-request-id", "mock-cache-request")
	_, _ = fmt.Fprint(w, strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":0}}}`,
		"",
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"",
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))
}

func newGroupCacheTestService(t *testing.T) (*GatewayService, *cacheProtocolMockServer, func()) {
	t.Helper()
	resetCacheTracker()
	strategyID := int64(99001)
	cfg := smallPayloadCacheConfig(CacheStrategyKindPrefix)
	cfg.MinCacheableTokens = 1
	cfg.DefaultTTLSeconds = 300
	cfg.HourTTLSeconds = 3600
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "protocol integration",
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})

	mock := &cacheProtocolMockServer{}
	server := httptest.NewServer(http.HandlerFunc(mock.handler))
	svc := &GatewayService{
		httpUpstream:        &cacheProtocolHTTPUpstream{client: server.Client()},
		tlsFPProfileService: &TLSFingerprintProfileService{},
		cfg: &config.Config{Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		}, Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			AllowInsecureHTTP: true,
		}}},
	}
	cleanup := func() {
		server.Close()
		GlobalCacheStrategyRegistry().Delete(strategyID)
	}
	return svc, mock, cleanup
}

func cacheTestAccount(baseURL string) *Account {
	return &Account{
		ID:       88001,
		Name:     "cache-protocol-account",
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "mock-key",
			"base_url": baseURL,
		},
	}
}

func cacheTestGroup(id int64) *Group {
	strategyID := int64(99001)
	return &Group{ID: id, Platform: PlatformAnthropic, CacheStrategyID: &strategyID}
}

func cacheResponsesContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	return c, rec
}

func cacheChatContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	return c, rec
}

func TestGroupCacheStrategyResponsesAndChatHTTPMock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, mock, cleanup := newGroupCacheTestService(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer server.Close()
	account := cacheTestAccount(server.URL)
	group := cacheTestGroup(101)
	responsesBody := []byte(`{"model":"claude-sonnet-4-6","prompt_cache_key":"protocol-session-a","instructions":"stable project instructions that should be cached","input":"current dynamic request","stream":false}`)

	firstCtx, firstRec := cacheResponsesContext(responsesBody)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(responsesBody), "responses")
	require.NoError(t, err)
	parsed.Group = group
	first, err := svc.ForwardAsResponses(context.Background(), firstCtx, account, responsesBody, parsed)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Zero(t, first.Usage.CacheReadInputTokens)
	require.Greater(t, first.Usage.CacheCreationInputTokens, 0)
	require.Equal(t, first.Usage.CacheCreationInputTokens, int(gjson.Get(firstRec.Body.String(), "usage.cache_creation_input_tokens").Int()))

	secondCtx, secondRec := cacheResponsesContext(responsesBody)
	second, err := svc.ForwardAsResponses(context.Background(), secondCtx, account, responsesBody, parsed)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Greater(t, second.Usage.CacheReadInputTokens, 0)
	require.Zero(t, second.Usage.CacheCreationInputTokens)
	require.Equal(t, second.Usage.CacheReadInputTokens, int(gjson.Get(secondRec.Body.String(), "usage.input_tokens_details.cached_tokens").Int()))
	require.Equal(t, second.Usage.InputTokens+second.Usage.CacheReadInputTokens, int(gjson.Get(secondRec.Body.String(), "usage.input_tokens").Int()))

	chatBody := []byte(`{"model":"claude-sonnet-4-6","prompt_cache_key":"protocol-session-chat","messages":[{"role":"system","content":"stable project instructions that should be cached"},{"role":"user","content":"current dynamic request"}],"stream":false}`)
	chatParsed := &ParsedRequest{Group: group}
	chatFirstCtx, chatFirstRec := cacheChatContext(chatBody)
	chatFirst, err := svc.ForwardAsChatCompletions(context.Background(), chatFirstCtx, account, chatBody, chatParsed)
	require.NoError(t, err)
	require.NotNil(t, chatFirst)
	require.Zero(t, chatFirst.Usage.CacheReadInputTokens)
	require.Greater(t, chatFirst.Usage.CacheCreationInputTokens, 0)
	require.Contains(t, chatFirstRec.Body.String(), `"cache_creation_tokens"`)

	chatSecondCtx, chatSecondRec := cacheChatContext(chatBody)
	chatSecond, err := svc.ForwardAsChatCompletions(context.Background(), chatSecondCtx, account, chatBody, chatParsed)
	require.NoError(t, err)
	require.NotNil(t, chatSecond)
	require.Greater(t, chatSecond.Usage.CacheReadInputTokens, 0)
	require.Zero(t, chatSecond.Usage.CacheCreationInputTokens)
	require.Equal(t, chatSecond.Usage.CacheReadInputTokens, int(gjson.Get(chatSecondRec.Body.String(), "usage.prompt_tokens_details.cached_tokens").Int()))
	require.GreaterOrEqual(t, mock.calls, 4)
}

func TestGroupCacheStrategyFailureDoesNotCommitAndGroupIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _, cleanup := newGroupCacheTestService(t)
	defer cleanup()

	mock := &cacheProtocolMockServer{}
	server := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer server.Close()
	account := cacheTestAccount(server.URL)
	groupA := cacheTestGroup(201)
	groupB := cacheTestGroup(202)
	body := []byte(`{"model":"claude-sonnet-4-6","prompt_cache_key":"group-session-a","input":"same stable prefix for isolated groups","stream":false}`)
	parsedA := &ParsedRequest{Group: groupA}

	// A failed upstream response must not populate the tracker.
	failing := []byte(`{"model":"claude-sonnet-4-6","prompt_cache_key":"group-session-a","input":"FAIL_CACHE same stable prefix for isolated groups","stream":false}`)
	failCtx, _ := cacheResponsesContext(failing)
	_, err := svc.ForwardAsResponses(context.Background(), failCtx, account, failing, parsedA)
	require.Error(t, err)

	firstCtx, _ := cacheResponsesContext(body)
	first, err := svc.ForwardAsResponses(context.Background(), firstCtx, account, body, parsedA)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Zero(t, first.Usage.CacheReadInputTokens)
	require.Greater(t, first.Usage.CacheCreationInputTokens, 0)

	secondCtx, _ := cacheResponsesContext(body)
	second, err := svc.ForwardAsResponses(context.Background(), secondCtx, account, body, parsedA)
	require.NoError(t, err)
	require.Greater(t, second.Usage.CacheReadInputTokens, 0)

	parsedB := &ParsedRequest{Group: groupB}
	otherCtx, _ := cacheResponsesContext(body)
	other, err := svc.ForwardAsResponses(context.Background(), otherCtx, account, body, parsedB)
	require.NoError(t, err)
	require.NotNil(t, other)
	require.Zero(t, other.Usage.CacheReadInputTokens)
	require.Greater(t, other.Usage.CacheCreationInputTokens, 0)
}

func TestGroupCacheStrategyRawChatStreamingUsageAndCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, _, cleanup := newGroupCacheTestService(t)
	defer cleanup()

	account := cacheTestAccount("https://upstream.invalid")
	group := cacheTestGroup(301)
	body := []byte(`{"model":"claude-sonnet-4-6","metadata":{"session_id":"raw-chat-session"},"messages":[{"role":"system","content":"stable raw chat instructions that should be cached"},{"role":"user","content":"current dynamic request"}],"stream":true}`)
	streamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_mock","object":"chat.completion.chunk","model":"claude-sonnet-4-6","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		SetCacheGroupContext(c, group)
		prepareCachePlanForContext(context.Background(), c, account, group, body, "claude-sonnet-4-6", "openai_chat_completions", estimateKiroInputTokens(context.Background(), body))
		return c, rec
	}
	newResponse := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"raw-mock"}},
			Body:       io.NopCloser(strings.NewReader(streamBody)),
		}
	}

	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	firstCtx, firstRec := newContext()
	first, err := svc.streamRawChatCompletions(firstCtx, newResponse(), account, "claude-sonnet-4-6", "claude-sonnet-4-6", "claude-sonnet-4-6", nil, nil, time.Now(), len(body))
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Greater(t, first.Usage.CacheCreationInputTokens, 0)
	require.Contains(t, firstRec.Body.String(), `"prompt_tokens_details"`)
	require.Contains(t, firstRec.Body.String(), `"cache_creation_tokens"`)

	secondCtx, secondRec := newContext()
	second, err := svc.streamRawChatCompletions(secondCtx, newResponse(), account, "claude-sonnet-4-6", "claude-sonnet-4-6", "claude-sonnet-4-6", nil, nil, time.Now(), len(body))
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Greater(t, second.Usage.CacheReadInputTokens, 0)
	require.Zero(t, second.Usage.CacheCreationInputTokens)
	require.Contains(t, secondRec.Body.String(), `"cached_tokens"`)
}

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func openAIClientToolsRequest(stream bool) []byte {
	streamValue := "false"
	if stream {
		streamValue = "true"
	}
	return []byte(`{"model":"gpt-5.4","input":"fix it","stream":` + streamValue + `,"tools":[{"type":"custom","name":"exec"},{"type":"custom","name":"apply_patch"}]}`)
}

func assertOpenAIClientToolsLowered(t *testing.T, body []byte) {
	t.Helper()
	for index, name := range []string{"exec", "apply_patch"} {
		tool := gjson.GetBytes(body, "tools."+string(rune('0'+index)))
		require.Equal(t, "function", tool.Get("type").String())
		require.Equal(t, name, tool.Get("name").String())
		require.Equal(t, "string", tool.Get("parameters.properties.input.type").String())
	}
}

func openAIClientToolsTestService(upstream *httpUpstreamRecorder) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg: &config.Config{Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		}},
	}
}

func openAIPassthroughCacheStrategyTestSetup(t *testing.T, strategyID int64) func() {
	t.Helper()
	resetCacheTracker()
	cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
	cfg.RatioMode = CacheRatioModeIndependent
	cfg.CoverageRatio = 0.9
	cfg.UsageRatio = 0.9
	cfg.ReadRatio = 1
	cfg.CreationRatio = 1
	cfg.MinCacheableTokens = 1
	cfg.MaxCoverageTokens = 1500
	cfg.MaxNewCreationTokensPerRequest = 1500
	cfg.IncrementalCreateEnabled = true
	GlobalCacheStrategyRegistry().Put(&CacheStrategy{
		ID:       strategyID,
		Name:     "passthrough-cache-test",
		Enabled:  true,
		Revision: 1,
		Config:   cfg,
	})
	return func() {
		GlobalCacheStrategyRegistry().Delete(strategyID)
		resetCacheTracker()
	}
}

func openAIPassthroughCacheStrategyGroup(strategyID, groupID int64) *Group {
	return &Group{
		ID:              groupID,
		Platform:        PlatformOpenAI,
		CacheStrategyID: &strategyID,
	}
}

func openAIPassthroughResponsesBody(stream bool, session string) []byte {
	streamValue := "false"
	if stream {
		streamValue = "true"
	}
	return []byte(`{"model":"gpt-5.4","metadata":{"session_id":"` + session + `"},"input":"stable passthrough cache body","stream":` + streamValue + `}`)
}

func openAIPassthroughResponseCachedTokens(body string) (read, creation int64) {
	for _, path := range []string{
		"usage.input_tokens_details.cached_tokens",
		"response.usage.input_tokens_details.cached_tokens",
		"usage.prompt_tokens_details.cached_tokens",
		"response.usage.prompt_tokens_details.cached_tokens",
	} {
		if value := gjson.Get(body, path); value.Exists() && value.Int() > 0 {
			read = value.Int()
			break
		}
	}
	for _, path := range []string{
		"usage.cache_creation_input_tokens",
		"response.usage.cache_creation_input_tokens",
		"usage.prompt_tokens_details.cache_creation_tokens",
		"response.usage.prompt_tokens_details.cache_creation_tokens",
	} {
		if value := gjson.Get(body, path); value.Exists() && value.Int() > 0 {
			creation = value.Int()
			break
		}
	}
	return read, creation
}

func TestAdaptOpenAIResponsesClientToolsLeavesNamespaceOnlyBodyUnchanged(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.5",
		"tools": [{"type": "namespace", "name": "code_tools", "tools": [{"type": "function", "name": "run"}]}],
		"tool_choice": "auto"
	}`)

	adapted, mapping, err := adaptOpenAIResponsesClientTools(body)

	require.NoError(t, err)
	require.Equal(t, body, adapted)
	require.Empty(t, mapping.CustomTools)
	require.Empty(t, mapping.NamespaceTools)
	require.False(t, mapping.ToolSearch)
}

func TestAdaptOpenAIResponsesClientToolsRejectsTrailingData(t *testing.T) {
	tests := map[string][]byte{
		"trailing garbage":     append(openAIClientToolsRequest(false), []byte(` garbage`)...),
		"second JSON document": append(openAIClientToolsRequest(false), []byte(` {"model":"other"}`)...),
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			adapted, mapping, err := adaptOpenAIResponsesClientTools(body)

			require.ErrorContains(t, err, "decode OpenAI Responses client tools trailing data")
			require.Equal(t, body, adapted)
			require.Empty(t, mapping)
		})
	}
}

func TestResponsesFunctionUpstreamsLowerToolSearchDiscoveryOutput(t *testing.T) {
	body := []byte(`{"tools":[{"type":"tool_search"}],"input":[{"type":"tool_search_output","call_id":"search_1","tools":[{"type":"namespace","name":"github"}],"status":"completed","execution":"client"}]}`)
	adapters := map[string]func([]byte) ([]byte, apicompat.ResponsesClientToolMapping, error){
		"OpenAI API-key": adaptOpenAIResponsesClientTools,
		"Grok":           adaptGrokResponsesClientTools,
	}
	for name, adapt := range adapters {
		t.Run(name, func(t *testing.T) {
			adapted, mapping, err := adapt(body)
			require.NoError(t, err)
			require.True(t, mapping.ToolSearch)
			require.Equal(t, "function_call_output", gjson.GetBytes(adapted, "input.0.type").String())
			require.JSONEq(t, `[{"name":"github","type":"namespace"}]`, gjson.GetBytes(adapted, "input.0.output").String())
			require.False(t, gjson.GetBytes(adapted, "input.0.tools").Exists())
			require.False(t, gjson.GetBytes(adapted, "input.0.status").Exists())
			require.False(t, gjson.GetBytes(adapted, "input.0.execution").Exists())
		})
	}
}

func TestClearOpenAIResponsesClientToolMappingRemovesStaleContextState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(openAIResponsesClientToolMappingContextKey, apicompat.ResponsesClientToolMapping{CustomTools: map[string]bool{"exec": true}})

	clearOpenAIResponsesClientToolMapping(c)

	_, ok := openAIResponsesClientToolMapping(c)
	require.False(t, ok)
}

func TestDeepSeekResponsesForwardRestoresClientToolsStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := openAIClientToolsRequest(true)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"function_call","id":"i1","call_id":"c1","name":"exec","status":"in_progress"}}`,
		`data: {"type":"response.function_call_arguments.done","sequence_number":1,"item_id":"i1","call_id":"c1","name":"exec","arguments":"{\"input\":\"pwd\"}"}`,
		`data: {"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"function_call","id":"i1","call_id":"c1","name":"exec","arguments":"{\"input\":\"pwd\"}","status":"completed"}}`,
		`data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_ds_tools","status":"completed","output":[{"type":"function_call","id":"i1","call_id":"c1","name":"exec","arguments":"{\"input\":\"pwd\"}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}, "\n\n") + "\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	svc := openAIClientToolsTestService(upstream)
	account := &Account{
		ID:       5661,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "test-key",
			"api_protocol": APIProtocolResponses,
			"base_url":     "https://relay.example",
		},
	}

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	assertOpenAIClientToolsLowered(t, upstream.lastBody)
	require.Equal(t, "/responses", upstream.lastReq.URL.Path)
	output := recorder.Body.String()
	require.Contains(t, output, `"type":"custom_tool_call"`)
	require.Contains(t, output, `"type":"response.custom_tool_call_input.done"`)
	require.Contains(t, output, `"input":"pwd"`)
}

func TestDeepSeekAdaptiveResponsesForwardRestoresClientToolsNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := openAIClientToolsRequest(false)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"resp_ds_adaptive_tools","status":"completed","output":[
			{"type":"function_call","id":"i1","call_id":"c1","name":"exec","arguments":"{\"input\":\"pwd\"}"},
			{"type":"function_call","id":"i2","call_id":"c2","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}],
			"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := openAIClientToolsTestService(upstream)
	account := &Account{
		ID:       5662,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "test-key",
			"api_protocol": APIProtocolAdaptive,
			"api_base_urls": map[string]any{
				APIProtocolResponses: "https://relay.example",
			},
		},
	}

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	assertOpenAIClientToolsLowered(t, upstream.lastBody)
	require.Equal(t, "/responses", upstream.lastReq.URL.Path)
	require.Equal(t, "custom_tool_call", gjson.Get(recorder.Body.String(), "output.0.type").String())
	require.Equal(t, "pwd", gjson.Get(recorder.Body.String(), "output.0.input").String())
	require.Equal(t, "custom_tool_call", gjson.Get(recorder.Body.String(), "output.1.type").String())
	require.Equal(t, "*** Begin Patch", gjson.Get(recorder.Body.String(), "output.1.input").String())
}

func TestDeepSeekResponsesCompactSkipsClientToolAdaptation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := openAIClientToolsRequest(false)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_compact","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := openAIClientToolsTestService(upstream)
	account := &Account{
		ID:       5663,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "test-key",
			"api_protocol": APIProtocolResponses,
			"base_url":     "https://relay.example",
		},
	}

	_, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, "custom", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
	require.Equal(t, "/responses/compact", upstream.lastReq.URL.Path)
}

func TestOpenAIPassthroughAPIKeyRestoresClientToolsNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := openAIClientToolsRequest(false)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"resp_tools","status":"completed","output":[
			{"type":"function_call","id":"i1","call_id":"c1","name":"exec","arguments":"{\"input\":\"pwd\"}"},
			{"type":"function_call","id":"i2","call_id":"c2","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}],"usage":{}}`)),
	}}
	svc := openAIClientToolsTestService(upstream)
	account := &Account{ID: 5659, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}

	result, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, false, time.Now())

	require.NoError(t, err)
	require.NotNil(t, result)
	assertOpenAIClientToolsLowered(t, upstream.lastBody)
	require.Equal(t, "custom_tool_call", gjson.Get(recorder.Body.String(), "output.0.type").String())
	require.Equal(t, "pwd", gjson.Get(recorder.Body.String(), "output.0.input").String())
	require.Equal(t, "custom_tool_call", gjson.Get(recorder.Body.String(), "output.1.type").String())
	require.Equal(t, "*** Begin Patch", gjson.Get(recorder.Body.String(), "output.1.input").String())
}

func TestOpenAIPassthroughAPIKeyPreservesCustomToolOutputContentParts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":false,"tools":[{"type":"custom","name":"exec"}],"input":[{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_text","text":"result"},{"type":"input_file","file_id":"file_123"}]}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_tools","status":"completed","output":[],"usage":{}}`)),
	}}
	svc := openAIClientToolsTestService(upstream)
	account := &Account{ID: 6240, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}

	result, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, false, time.Now())

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "function_call_output", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
	output := gjson.GetBytes(upstream.lastBody, "input.0.output")
	require.True(t, output.IsArray(), "native Responses content parts must reach the upstream as an array")
	require.Equal(t, "input_text", output.Get("0.type").String())
	require.Equal(t, "result", output.Get("0.text").String())
	require.Equal(t, "input_file", output.Get("1.type").String())
	require.Equal(t, "file_123", output.Get("1.file_id").String())
}

func TestOpenAIPassthroughAPIKeyRestoresClientToolsStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := openAIClientToolsRequest(true)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"function_call","id":"i1","call_id":"c1","name":"apply_patch","status":"in_progress"}}`,
		`data: {"type":"response.function_call_arguments.done","sequence_number":1,"item_id":"i1","call_id":"c1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}`,
		`data: {"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"function_call","id":"i1","call_id":"c1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}","status":"completed"}}`,
		`data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_stream_tools","status":"completed","output":[{"type":"function_call","id":"i1","call_id":"c1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}, "\n\n") + "\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}}
	svc := openAIClientToolsTestService(upstream)
	account := &Account{ID: 5660, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}

	result, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, true, time.Now())

	require.NoError(t, err)
	require.NotNil(t, result)
	assertOpenAIClientToolsLowered(t, upstream.lastBody)
	output := recorder.Body.String()
	require.Contains(t, output, `"type":"custom_tool_call"`)
	require.Contains(t, output, `"type":"response.custom_tool_call_input.done"`)
	require.Contains(t, output, `"input":"*** Begin Patch"`)
	require.NotContains(t, output, `"input":{`)
}

func TestOpenAIPassthroughResponsesNonStreamingJSONCommitsCacheUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cleanup := openAIPassthroughCacheStrategyTestSetup(t, 77111)
	defer cleanup()

	group := openAIPassthroughCacheStrategyGroup(77111, 77112)
	body := openAIPassthroughResponsesBody(false, "passthrough-json-session")
	makeUpstream := func() *httpUpstreamRecorder {
		return &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"resp_passthrough_json","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":18,"output_tokens":6,"total_tokens":24}}`)),
		}}
	}
	account := &Account{ID: 77113, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}

	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetCacheGroupContext(firstCtx, group)
	firstSvc := openAIClientToolsTestService(makeUpstream())
	firstResult, err := firstSvc.forwardOpenAIPassthrough(context.Background(), firstCtx, account, body, body, "gpt-5.4", false, nil, false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Greater(t, firstResult.Usage.CacheCreationInputTokens, 0)
	read, creation := openAIPassthroughResponseCachedTokens(firstRec.Body.String())
	require.Zero(t, read)
	require.Greater(t, creation, int64(0))

	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetCacheGroupContext(secondCtx, group)
	secondSvc := openAIClientToolsTestService(makeUpstream())
	secondResult, err := secondSvc.forwardOpenAIPassthrough(context.Background(), secondCtx, account, body, body, "gpt-5.4", false, nil, false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Greater(t, secondResult.Usage.CacheReadInputTokens, 0)
	require.Zero(t, secondResult.Usage.CacheCreationInputTokens)
	read, creation = openAIPassthroughResponseCachedTokens(secondRec.Body.String())
	require.Greater(t, read, int64(0))
	require.Zero(t, creation)
}

func TestOpenAIPassthroughResponsesNonStreamingSSECommitsCacheUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cleanup := openAIPassthroughCacheStrategyTestSetup(t, 77121)
	defer cleanup()

	group := openAIPassthroughCacheStrategyGroup(77121, 77122)
	body := openAIPassthroughResponsesBody(false, "passthrough-sse-session")
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_passthrough_sse"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_passthrough_sse","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":20,"output_tokens":5,"total_tokens":25}}}`,
		``,
	}, "\n")
	makeUpstream := func() *httpUpstreamRecorder {
		return &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}}
	}
	account := &Account{ID: 77123, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}

	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetCacheGroupContext(firstCtx, group)
	firstSvc := openAIClientToolsTestService(makeUpstream())
	firstResult, err := firstSvc.forwardOpenAIPassthrough(context.Background(), firstCtx, account, body, body, "gpt-5.4", false, nil, false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Greater(t, firstResult.Usage.CacheCreationInputTokens, 0)
	require.Contains(t, firstRec.Body.String(), "cache_creation_input_tokens")

	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetCacheGroupContext(secondCtx, group)
	secondSvc := openAIClientToolsTestService(makeUpstream())
	secondResult, err := secondSvc.forwardOpenAIPassthrough(context.Background(), secondCtx, account, body, body, "gpt-5.4", false, nil, false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Greater(t, secondResult.Usage.CacheReadInputTokens, 0)
	require.Zero(t, secondResult.Usage.CacheCreationInputTokens)
	require.Contains(t, secondRec.Body.String(), "cached_tokens")
}

func TestOpenAIPassthroughResponsesStreamingCommitsCacheUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cleanup := openAIPassthroughCacheStrategyTestSetup(t, 77131)
	defer cleanup()

	group := openAIPassthroughCacheStrategyGroup(77131, 77132)
	body := openAIPassthroughResponsesBody(true, "passthrough-stream-session")
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_passthrough_stream"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_passthrough_stream","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":22,"output_tokens":4,"total_tokens":26}}}`,
		``,
	}, "\n")
	makeUpstream := func() *httpUpstreamRecorder {
		return &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}}
	}
	account := &Account{ID: 77133, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}

	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetCacheGroupContext(firstCtx, group)
	firstSvc := openAIClientToolsTestService(makeUpstream())
	firstResult, err := firstSvc.forwardOpenAIPassthrough(context.Background(), firstCtx, account, body, body, "gpt-5.4", false, nil, true, time.Now())
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Greater(t, firstResult.Usage.CacheCreationInputTokens, 0)
	require.Contains(t, firstRec.Body.String(), "cache_creation_input_tokens")

	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetCacheGroupContext(secondCtx, group)
	secondSvc := openAIClientToolsTestService(makeUpstream())
	secondResult, err := secondSvc.forwardOpenAIPassthrough(context.Background(), secondCtx, account, body, body, "gpt-5.4", false, nil, true, time.Now())
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Greater(t, secondResult.Usage.CacheReadInputTokens, 0)
	require.Zero(t, secondResult.Usage.CacheCreationInputTokens)
	require.Contains(t, secondRec.Body.String(), "cached_tokens")
}

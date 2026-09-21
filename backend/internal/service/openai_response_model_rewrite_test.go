package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMappedResponseModelPreservesOtherData(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := `{"model":"alias","text":"mapped alias","tool":{"model":"mapped","arguments":"{\"model\":\"alias\"}"}}`
	want := `{"model":"public","text":"mapped alias","tool":{"model":"mapped","arguments":"{\"model\":\"alias\"}"}}`
	require.Equal(t, want, string(svc.replaceModelInResponseBody([]byte(body), "mapped", "public")))
	require.Equal(t, "data: "+want, svc.replaceModelInSSELine("data: "+body, "mapped", "public"))
	for _, body := range []string{
		`{"model":"alias",`, `{"model":"alias"} trailing`, `{"model":null}`, `{"model":42}`, `{"model":{}}`, `{"model":[]}`, `{"model":true}`,
		`{"text":"alias","tool":{"model":"alias"}}`,
	} {
		require.Equal(t, body, string(svc.replaceModelInResponseBody([]byte(body), "mapped", "public")))
		require.Equal(t, "data: "+body, svc.replaceModelInSSELine("data: "+body, "mapped", "public"))
	}
	for _, models := range [][2]string{{"same", "same"}, {"", "public"}, {"mapped", ""}} {
		body := `{"model":"alias","response":{"model":"alias"}}`
		require.Equal(t, body, string(svc.replaceModelInResponseBody([]byte(body), models[0], models[1])))
		require.Equal(t, "data: "+body, svc.replaceModelInSSELine("data: "+body, models[0], models[1]))
	}
	require.Equal(t, `data: {"response":{"model":42}}`, svc.replaceModelInSSELine(`data: {"response":{"model":42}}`, "mapped", "public"))
	require.Equal(t, `{"model":"public"}`, string(svc.replaceModelInResponseBody([]byte(`{"model":""}`), "mapped", "public")))
}

// Exercise both streaming processors so substring fast paths cannot bypass the rewrite.
func TestMappedResponseModelForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, returned := range []string{"zhipu/glm-5.3", "glm-5.3-alias"} {
			for _, mapped := range []string{"ZHIPU/GLM-5.3", "public"} {
				for _, kind := range []string{"json", "chat", "responses"} {
					name := kind + "/" + returned + "/" + mapped
					if passthrough {
						name += "/passthrough"
					}
					t.Run(name, func(t *testing.T) {
						model := returned
						if mapped != "public" {
							model = "public"
						}
						payload := `{"model":"` + returned + `","choices":[{"delta":{"content":"keep alias","tool_calls":[{"function":{"arguments":"{\"model\":\"alias\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
						contentType := "application/json"
						body := payload
						if kind == "responses" {
							payload = `{"type":"response.completed","response":{"id":"resp_1","model":"` + returned + `","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`
						}
						if kind != "json" {
							contentType = "text/event-stream"
							body = "data: " + payload + "\n\ndata: [DONE]\n\n"
						}
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
						resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
						svc := &OpenAIGatewayService{}
						account := &Account{ID: 1}
						var err error
						if kind == "json" {
							if passthrough {
								_, err = svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "public", mapped)
							} else {
								_, err = svc.handleNonStreamingResponse(context.Background(), resp, c, account, "public", mapped)
							}
						} else {
							if passthrough {
								_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "public", mapped)
							} else {
								_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "public", mapped)
							}
						}
						require.NoError(t, err)

						// The response pipeline may normalize or enrich usage fields (for
						// example by adding total_tokens). Assert the protocol fields
						// semantically instead of comparing the entire serialized JSON.
						responsePayload := rec.Body.String()
						if kind != "json" {
							for _, line := range strings.Split(responsePayload, "\n") {
								line = strings.TrimSpace(line)
								if strings.HasPrefix(line, "data: ") && strings.TrimSpace(strings.TrimPrefix(line, "data: ")) != "[DONE]" {
									responsePayload = strings.TrimPrefix(line, "data: ")
									break
								}
							}
						}
						require.True(t, gjson.Valid(responsePayload), "forwarded payload must remain valid JSON: %s", responsePayload)
						modelPath := "model"
						if kind == "responses" {
							modelPath = "response.model"
						}
						require.Equal(t, model, gjson.Get(responsePayload, modelPath).String())
						if kind == "responses" {
							require.Equal(t, float64(1), gjson.Get(responsePayload, "response.usage.input_tokens").Float())
							require.Equal(t, float64(1), gjson.Get(responsePayload, "response.usage.output_tokens").Float())
						} else {
							require.Equal(t, float64(1), gjson.Get(responsePayload, "usage.prompt_tokens").Float())
							require.Equal(t, float64(1), gjson.Get(responsePayload, "usage.completion_tokens").Float())
							require.Equal(t, `{"model":"alias"}`, gjson.Get(responsePayload, "choices.0.delta.tool_calls.0.function.arguments").String())
						}
						require.Equal(t, returned, observedUpstreamResponseModel(c))
					})
				}
			}
		}
	}
}

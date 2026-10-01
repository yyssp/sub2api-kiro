package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiagnosePayloadCountsShapeWithoutContent(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"user","content":"SECRET_USER_TEXT"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/etc/SECRET"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"SECRET_RESULT"}]}
		],
		"tools":[{"name":"Read","description":"read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]
	}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)

	d := DiagnosePayload(res.Payload)
	require.Equal(t, len(res.Payload), d.PayloadBytes)
	require.Positive(t, d.PayloadWeight)
	require.Equal(t, 1, d.HistoryToolUses)
	require.Equal(t, 1, d.CurrentToolResults)
	require.Equal(t, 1, d.Tools)
	require.Zero(t, d.UnpairedToolResults)
	require.True(t, d.FirstHistoryIsUser)
	require.GreaterOrEqual(t, d.MaxToolSchemaDepth, 2)
}

func TestDiagnosePayloadDetectsUnpairedToolResults(t *testing.T) {
	payload := []byte(`{"conversationState":{"chatTriggerType":"MANUAL","conversationId":"c","history":[
		{"userInputMessage":{"content":"q"}},
		{"assistantResponseMessage":{"content":"no tools"}}
	],"currentMessage":{"userInputMessage":{"content":"x","userInputMessageContext":{"toolResults":[{"toolUseId":"t","content":[{"text":"r"}],"status":"success"}]}}}}}`)
	d := DiagnosePayload(payload)
	require.Equal(t, 1, d.UnpairedToolResults)
}

func TestDiagnosePayloadInvalidJSONKeepsSize(t *testing.T) {
	d := DiagnosePayload([]byte("not json"))
	require.Equal(t, 8, d.PayloadBytes)
	require.Zero(t, d.HistoryMessages)
}

func TestPayloadDiagnosticsHasNoStringFields(t *testing.T) {
	// 诊断结构只允许数值/布尔，防止日后有人往里塞正文。
	d := DiagnosePayload([]byte(`{}`))
	encoded := strings.ToLower(strings.TrimSpace(func() string { b, _ := json.Marshal(d); return string(b) }()))
	require.NotContains(t, encoded, `":"`)
}

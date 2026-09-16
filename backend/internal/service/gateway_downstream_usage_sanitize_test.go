package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// capturedKiroRelayUsageKeys 是 2026-09-16 真实抓包里某 Kiro 中继塞进
// message_delta.usage 的全部私有字段（gateway → client 方向原样透传出去了）。
var capturedKiroRelayUsageKeys = []string{
	"kiro_actual_input_tokens",
	"kiro_billable_input_tokens",
	"kiro_credits",
	"kiro_excluded_input_tokens",
	"kiro_first_thinking_ms",
	"kiro_first_tool_use_ms",
	"kiro_first_visible_ms",
	"kiro_total_ms",
	"kiro_upstream_response_ms",
}

func TestPrivateUsageFieldKey(t *testing.T) {
	for _, key := range capturedKiroRelayUsageKeys {
		require.True(t, privateUsageFieldKey(key), "captured relay key must be stripped: %s", key)
	}
	for _, key := range []string{"_sub2api_kiro_credits", "kiroCredits", "KIRO_TOTAL_MS", "credits", "creditsUsed", "creditUsage"} {
		require.True(t, privateUsageFieldKey(key), "private key must be stripped: %s", key)
	}
	// 白名单式实现会把这些合法字段一起剪掉：Anthropic 自己还在持续加字段，
	// 实测原生上游已在回 service_tier / inference_geo。
	for _, key := range []string{
		"input_tokens", "output_tokens", "cache_read_input_tokens",
		"cache_creation_input_tokens", "cache_creation", "cached_tokens",
		"service_tier", "inference_geo", "server_tool_use",
	} {
		require.False(t, privateUsageFieldKey(key), "standard key must survive: %s", key)
	}
}

func TestStripPrivateUsageFieldsFromJSONKeepsStandardFields(t *testing.T) {
	body := `{"id":"msg_1","type":"message","usage":{` +
		`"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":5600,` +
		`"cache_creation_input_tokens":780,"cache_creation":{"ephemeral_5m_input_tokens":780,"ephemeral_1h_input_tokens":0},` +
		`"service_tier":"standard","inference_geo":"global",` +
		`"kiro_actual_input_tokens":30174,"kiro_billable_input_tokens":10841,"kiro_excluded_input_tokens":19333,` +
		`"kiro_credits":0.25,"kiro_total_ms":8123,"_sub2api_kiro_credits":0.25}}`

	cleaned := stripPrivateUsageFieldsFromJSON(body)

	usage := gjson.Get(cleaned, "usage")
	require.True(t, usage.IsObject())
	usage.ForEach(func(key, _ gjson.Result) bool {
		require.False(t, privateUsageFieldKey(key.String()), "leaked private key: %s", key.String())
		return true
	})
	require.Equal(t, int64(12), usage.Get("input_tokens").Int())
	require.Equal(t, int64(34), usage.Get("output_tokens").Int())
	require.Equal(t, int64(5600), usage.Get("cache_read_input_tokens").Int())
	require.Equal(t, int64(780), usage.Get("cache_creation.ephemeral_5m_input_tokens").Int())
	require.Equal(t, "standard", usage.Get("service_tier").String())
	require.Equal(t, "global", usage.Get("inference_geo").String())
	require.Equal(t, "msg_1", gjson.Get(cleaned, "id").String())
}

func TestStripPrivateUsageFieldsFromJSONNoopKeepsBytes(t *testing.T) {
	body := []byte(`{"type":"message","usage":{"input_tokens":12,"output_tokens":34}}`)

	require.Equal(t, string(body), string(stripPrivateUsageFieldsFromJSONBytes(body)))
	require.Equal(t, "", stripPrivateUsageFieldsFromJSON(""))
	// 非 JSON（如 SSE 拼接体）不能被破坏。
	require.Equal(t, "data: [DONE]", stripPrivateUsageFieldsFromJSON("data: [DONE]"))
}

func TestStripPrivateUsageFieldsFromSSELine(t *testing.T) {
	t.Run("message_start usage sits under message", func(t *testing.T) {
		line := `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":7,"kiro_actual_input_tokens":30174,"kiro_total_ms":81}}}`

		cleaned := stripPrivateUsageFieldsFromSSELine(line)

		require.NotContains(t, cleaned, "kiro_")
		require.Equal(t, int64(7), gjson.Get(cleaned[len("data: "):], "message.usage.input_tokens").Int())
	})

	t.Run("message_delta usage sits at the top level", func(t *testing.T) {
		line := `data: {"type":"message_delta","usage":{"output_tokens":9,"kiro_credits":0.25,"_sub2api_kiro_credits":0.25}}`

		cleaned := stripPrivateUsageFieldsFromSSELine(line)

		require.NotContains(t, cleaned, "kiro")
		require.Equal(t, int64(9), gjson.Get(cleaned[len("data: "):], "usage.output_tokens").Int())
	})

	t.Run("non data lines pass through untouched", func(t *testing.T) {
		for _, line := range []string{"event: message_delta", "", ": keepalive"} {
			require.Equal(t, line, stripPrivateUsageFieldsFromSSELine(line))
		}
	})
}

func TestSanitizeEventUsageForClient(t *testing.T) {
	t.Run("strips both usage locations and reports the change", func(t *testing.T) {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(
			`{"type":"message_start","message":{"usage":{"input_tokens":7,"kiro_total_ms":81}},`+
				`"usage":{"output_tokens":9,"_sub2api_kiro_credits":0.25}}`), &event))

		require.True(t, sanitizeEventUsageForClient(event))

		encoded, err := json.Marshal(event)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "kiro")
		require.Contains(t, string(encoded), `"input_tokens":7`)
		require.Contains(t, string(encoded), `"output_tokens":9`)
	})

	t.Run("clean events report no change so the frame is not re-marshalled", func(t *testing.T) {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(`{"type":"message_delta","usage":{"output_tokens":9}}`), &event))

		require.False(t, sanitizeEventUsageForClient(event))
	})
}

func TestEscapeGJSONPathKeyProtectsDeletion(t *testing.T) {
	// 字段名来自上游，带元字符时不转义会删错 key（甚至删掉整段 usage）。
	body := `{"usage":{"input_tokens":7,"kiro.total.ms":81,"kiro_ok":1}}`

	cleaned := stripPrivateUsageFieldsFromJSON(body)

	require.Equal(t, int64(7), gjson.Get(cleaned, "usage.input_tokens").Int())
	require.False(t, gjson.Get(cleaned, `usage.kiro\.total\.ms`).Exists())
	require.False(t, gjson.Get(cleaned, "usage.kiro_ok").Exists())
}

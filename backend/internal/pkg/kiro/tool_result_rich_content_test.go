package kiro

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var (
	testPNGBase64  = base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...))
	testJPEGBase64 = base64.StdEncoding.EncodeToString(append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 16)...))
)

const currentCtx = "conversationState.currentMessage.userInputMessage"

// Claude Code 用 Read 读图时，图片在 tool_result.content 里；丢掉它模型会编造图片内容。
func TestToolResultImageIsAttachedToUserMessage(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"user","content":"look at the screenshot"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/tmp/a.png"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testPNGBase64 + `"}}
			]}]}
		]
	}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)

	images := gjson.GetBytes(res.Payload, currentCtx+".images")
	require.Len(t, images.Array(), 1)
	require.Equal(t, "png", images.Get("0.format").String())
	require.Equal(t, testPNGBase64, images.Get("0.source.bytes").String())

	text := gjson.GetBytes(res.Payload, currentCtx+".userInputMessageContext.toolResults.0.content.0.text").String()
	require.Equal(t, kiroToolResultImageAttached, text)
}

func TestToolResultDocumentAndSearchResultRenderAsText(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"fetch","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DOC_MARKER"}},
				{"type":"search_result","title":"Go spec","source":"https://go.dev/ref/spec","content":[{"type":"text","text":"SEARCH_MARKER"}]}
			]}]}
		]
	}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)
	contents := gjson.GetBytes(res.Payload, currentCtx+".userInputMessageContext.toolResults.0.content").Raw
	require.Contains(t, contents, "DOC_MARKER")
	require.Contains(t, contents, "SEARCH_MARKER")
	require.Contains(t, contents, "https://go.dev/ref/spec")
}

func TestToolResultEmptyContentGetsPlaceholder(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"true"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":""}]}
		]
	}`)
	res, err := BuildKiroPayloadWithContext(body, "claude-sonnet-4.5", "", "AI_EDITOR", nil)
	require.NoError(t, err)
	text := gjson.GetBytes(res.Payload, currentCtx+".userInputMessageContext.toolResults.0.content.0.text").String()
	require.Equal(t, kiroEmptyToolResultPlaceholder, text)
}

// 客户端把 PNG 标成 image/jpeg 很常见；按声明格式发会被上游以 IMAGE_FORMAT_UNSUPPORTED 拒绝整个请求。
func TestBuildKiroImageCorrectsDeclaredFormatFromMagicBytes(t *testing.T) {
	image, ok := buildKiroImage("image/jpeg", testPNGBase64)
	require.True(t, ok)
	require.Equal(t, "png", image.Format)

	image, ok = buildKiroImage("image/png", testJPEGBase64)
	require.True(t, ok)
	require.Equal(t, "jpeg", image.Format)
}

func TestBuildKiroImageRejectsNonBase64Data(t *testing.T) {
	_, ok := buildKiroImage("image/png", "not base64 at all!!")
	require.False(t, ok)
}

func TestBuildKiroImageKeepsDeclaredFormatWhenUnrecognized(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("unknown-header-bytes-xxxxxx"))
	image, ok := buildKiroImage("image/webp", data)
	require.True(t, ok)
	require.Equal(t, "webp", image.Format)
}

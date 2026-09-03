//go:build unit

package service

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
)

const cacheProtocolTestStrategyID int64 = 990099

var cacheProtocolTestStrategyOnce sync.Once

// cacheGroup is shared by protocol bridge tests. It uses the same group-bound
// strategy path as production, including derived session identity for clients
// that do not send an explicit session key.
func cacheGroup(id int64) *Group {
	cacheProtocolTestStrategyOnce.Do(func() {
		cfg := DefaultCacheStrategyConfig(CacheStrategyKindPrefix)
		cfg.MinCacheableTokens = 1
		cfg.AllowDerivedSession = true
		cfg.DefaultTTLSeconds = 300
		cfg.HourTTLSeconds = 3600
		GlobalCacheStrategyRegistry().Put(&CacheStrategy{
			ID:       cacheProtocolTestStrategyID,
			Name:     "protocol bridge test",
			Enabled:  true,
			Revision: 1,
			Config:   cfg,
		})
	})
	strategyID := cacheProtocolTestStrategyID
	return &Group{
		ID:              id,
		Platform:        PlatformKiro,
		CacheStrategyID: &strategyID,
	}
}

func kiroPNGDataURL(t *testing.T, width, height int, fill color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

type cacheChatCompletionsMessage struct {
	Role    string
	Content string
}

func kiroChatCompletionsConversationBody(messages []cacheChatCompletionsMessage) []byte {
	items := make([]string, 0, len(messages)+1)
	items = append(items, `{"role":"system","content":"You are a precise assistant."}`)
	for _, message := range messages {
		items = append(items, fmt.Sprintf(`{"role":%q,"content":%q}`, message.Role, message.Content))
	}
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5","tool_choice":"auto","tools":[{"type":"function","function":{"name":"lookup","description":"lookup data","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}],"messages":[%s]}`,
		strings.Join(items, ","),
	))
}

func kiroResponsesCacheRequestBody(label, promptCacheKey, previousResponseID string) []byte {
	prompt := strings.Repeat("cacheable responses prompt chunk "+label+" ", 512)
	return []byte(fmt.Sprintf(
		`{"model":"gpt-5","instructions":"You are a precise assistant.","prompt_cache_key":%q,"previous_response_id":%q,"tool_choice":"auto","reasoning":{"effort":"medium"},"text":{"format":{"type":"json_object"}},"tools":[{"type":"function","name":"lookup","description":"lookup data","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}],"input":[{"role":"user","content":[{"type":"input_text","text":%q}]}]}`,
		promptCacheKey,
		previousResponseID,
		prompt,
	))
}

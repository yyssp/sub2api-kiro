package service

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strings"

	"github.com/tidwall/gjson"
)

func kiroCacheSessionID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String()); userID != "" {
		if gjson.Valid(userID) {
			if sessionID := strings.TrimSpace(gjson.Get(userID, "session_id").String()); sessionID != "" {
				return sessionID
			}
		} else if _, sessionID, ok := strings.Cut(userID, "session_"); ok {
			if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
				return sessionID
			}
		}
	}
	return strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
}

func kiroCacheDerivedConversation(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	root := gjson.ParseBytes(body)
	h := sha256.New()
	_, _ = h.Write([]byte("sub2api.kiro-cache.derived-conversation.v1\x00"))
	hashKiroCacheContentParts(h, root.Get("system"))
	hashKiroCacheContentParts(h, root.Get("instructions"))
	items := root.Get("messages")
	if !items.Exists() {
		items = root.Get("input")
	}
	var firstUser gjson.Result
	found := false
	if items.Type == gjson.String {
		firstUser, found = items, true
	} else {
		items.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("role").String() {
			case "system", "developer":
				hashKiroCacheContentParts(h, item.Get("content"))
			case "user":
				firstUser, found = item.Get("content"), true
				return false
			}
			return true
		})
	}
	if !found {
		return ""
	}
	_, _ = h.Write([]byte{1})
	hashKiroCacheContentParts(h, firstUser)
	return hex.EncodeToString(h.Sum(nil))
}

func hashKiroCacheContentParts(h hash.Hash, content gjson.Result) {
	write := func(value string) {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	switch {
	case content.Type == gjson.String:
		write(content.String())
	case content.IsArray():
		content.ForEach(func(_, part gjson.Result) bool {
			switch {
			case part.Type == gjson.String:
				write(part.String())
			case part.Get("text").Type == gjson.String:
				write(part.Get("text").String())
			case part.Get("source.data").Exists():
				write(part.Get("source.data").String())
			case part.Get("source.url").Exists():
				write(part.Get("source.url").String())
			case part.Get("image_url.url").Exists():
				write(part.Get("image_url.url").String())
			case part.Get("image_url").Type == gjson.String:
				write(part.Get("image_url").String())
			default:
				write(part.Get("type").String())
			}
			return true
		})
	}
}

package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"strings"
)

// effectiveCacheStrategyForGroup resolves the group binding without making the
// gateway hot path query PostgreSQL. The registry is populated by the admin
// service and warmed at startup; an unbound group has no cache policy.
func effectiveCacheStrategyForGroup(group *Group) *CacheStrategy {
	if group == nil || group.CacheStrategyID == nil || *group.CacheStrategyID <= 0 {
		return nil
	}
	return GlobalCacheStrategyRegistry().Get(*group.CacheStrategyID)
}

func effectiveCacheStrategyConfig(group *Group) (CacheStrategyConfig, bool) {
	strategy := effectiveCacheStrategyForGroup(group)
	if strategy != nil && strategy.Enabled {
		cfg := strategy.Config
		if cfg.Kind == CacheStrategyKindDisabled {
			return CacheStrategyConfig{}, false
		}
		return cfg, true
	}
	return CacheStrategyConfig{}, false
}

// hasBoundCacheStrategy distinguishes an explicitly bound strategy (including
// a disabled strategy) from an unbound group. Legacy failover billing used to
// reclassify any input as cache_read after an account switch; that is not a
// real cache hit and must not override the new group-bound policy.
func hasBoundCacheStrategy(group *Group) bool {
	return effectiveCacheStrategyForGroup(group) != nil
}

// cacheStrategySnapshotForAPIKey returns the effective strategy identity that
// was available on the authenticated API key's group snapshot. Usage logs keep
// both ID and name so historical rows remain readable after later edits.
func cacheStrategySnapshotForAPIKey(apiKey *APIKey) (*int64, *string) {
	if apiKey == nil || apiKey.Group == nil {
		return nil, nil
	}
	strategy := effectiveCacheStrategyForGroup(apiKey.Group)
	if strategy == nil {
		return nil, nil
	}
	id := strategy.ID
	name := strings.TrimSpace(strategy.Name)
	if id <= 0 || name == "" {
		return nil, nil
	}
	return &id, &name
}

func cacheStrategyCacheKey(account *Account, group *Group, protocol, model string, body []byte) uint64 {
	if group == nil || group.CacheStrategyID == nil || *group.CacheStrategyID <= 0 {
		return 0
	}
	strategy := effectiveCacheStrategyForGroup(group)
	if strategy == nil || !strategy.Enabled || strategy.Config.Kind == CacheStrategyKindDisabled {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("cache-v2\x00"))
	if strategy.Config.ScopeMode != CacheScopeModeGroupSession && account != nil {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], cacheCredentialKey(account))
		_, _ = h.Write(buf[:])
	}
	_, _ = h.Write([]byte("\x00group:"))
	_, _ = h.Write([]byte(stringID(group.ID)))
	_, _ = h.Write([]byte("\x00strategy:"))
	_, _ = h.Write([]byte(stringID(strategy.ID)))
	_, _ = h.Write([]byte("\x00revision:"))
	_, _ = h.Write([]byte(stringID(strategy.Revision)))
	_, _ = h.Write([]byte("\x00model:"))
	_, _ = h.Write([]byte(strings.TrimSpace(model)))
	sessionKey := cacheSessionKey(body, strategy.Config.AllowDerivedSession)
	if sessionKey == "" {
		// Every supported cache scope is session-bound. A request without a
		// stable session identifier must remain raw unless the strategy
		// explicitly opts into the conservative derived-session fallback.
		return 0
	}
	_, _ = h.Write([]byte("\x00session:"))
	_, _ = h.Write([]byte(sessionKey))
	_, _ = h.Write([]byte("\x00namespace:"))
	_, _ = h.Write([]byte(strategy.Config.ScopeMode))
	_, _ = h.Write([]byte("\x00protocol:"))
	_, _ = h.Write([]byte(strings.TrimSpace(protocol)))
	return h.Sum64()
}

func cacheSessionKey(body []byte, allowDerived bool) string {
	if len(body) == 0 {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		for _, path := range [][]string{
			{"conversation_id"},
			{"session_id"},
			{"metadata", "session_id"},
			{"metadata", "conversation_id"},
			{"prompt_cache_key"},
		} {
			if value := nestedString(payload, path...); value != "" {
				return value
			}
		}
		// Claude Code commonly serializes its stable session identity in
		// metadata.user_id as a JSON string (for example
		// {"device_id":"...","session_id":"..."}). The gateway already
		// understands this form for sticky account routing; cache scope must
		// use the same identity or every real Claude Code turn becomes a
		// session-less/raw request.
		if metadata, ok := payload["metadata"].(map[string]any); ok {
			if userID, ok := metadata["user_id"].(string); ok {
				var userPayload map[string]any
				if json.Unmarshal([]byte(userID), &userPayload) == nil {
					if value := nestedString(userPayload, "session_id"); value != "" {
						return value
					}
					if value := nestedString(userPayload, "conversation_id"); value != "" {
						return value
					}
				}
			}
		}
	}
	if !allowDerived {
		return ""
	}

	// Match the reference implementation's derived conversation identity:
	// system + tools + first user message are stable across later history
	// growth, while the current turn and volatile response IDs are excluded.
	var seed map[string]any
	if json.Unmarshal(body, &payload) == nil {
		seed = map[string]any{
			"system":          stripCacheControlValue(payload["system"]),
			"tools":           stripCacheControlValue(payload["tools"]),
			"first_user_turn": firstUserTurnValue(payload),
		}
	}
	if len(seed) == 0 {
		return ""
	}
	canonical, err := canonicalJSON(seed)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte("cache-derived-session-v1:"), canonical...))
	return "derived:" + fmtUint64(binary.BigEndian.Uint64(sum[:8]))
}

func firstUserTurnValue(payload map[string]any) any {
	messages, _ := payload["messages"].([]any)
	for _, message := range messages {
		obj, ok := message.(map[string]any)
		if !ok {
			continue
		}
		role, _ := obj["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "user") {
			return stripCacheControlValue(message)
		}
	}
	input, _ := payload["input"].([]any)
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if !ok {
			if _, isString := item.(string); isString {
				return item
			}
			continue
		}
		role, _ := obj["role"].(string)
		if role == "" || strings.EqualFold(strings.TrimSpace(role), "user") {
			return stripCacheControlValue(item)
		}
	}
	if value, ok := payload["input"].(string); ok {
		return value
	}
	return nil
}

func stripCacheControlValue(value any) any {
	switch x := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, child := range x {
			if key == "cache_control" {
				continue
			}
			out[key] = stripCacheControlValue(child)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = stripCacheControlValue(child)
		}
		return out
	default:
		return value
	}
}

func nestedString(payload map[string]any, path ...string) string {
	var current any = payload
	for _, key := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = obj[key]
	}
	value, _ := current.(string)
	return strings.TrimSpace(value)
}

func fmtUint64(v uint64) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[i:])
}

func stringID(v int64) string {
	return fmtInt64(v)
}

func fmtInt64(v int64) string {
	// Avoid fmt on the hot path; the decimal representation is only used as a
	// stable namespace component, not exposed to callers.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// WarmupCacheStrategyRegistry loads all policies once during application
// startup. A later admin write updates the same registry synchronously.
func (s *CacheStrategyService) Warmup(ctx context.Context) error {
	if s == nil || s.repo == nil {
		return nil
	}
	_, err := s.List(ctx, "")
	return err
}

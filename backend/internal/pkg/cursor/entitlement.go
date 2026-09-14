package cursor

import (
	"strings"
	"sync"
)

// CapabilityMode describes the default permission of one subscription on one
// Cursor quota surface. It is only a scheduling default: an explicit upstream
// entitlement, quota, or authentication result always has higher priority.
type CapabilityMode string

const (
	CapabilityProbe CapabilityMode = "probe"
	CapabilityAllow CapabilityMode = "allow"
	CapabilityDeny  CapabilityMode = "deny"
)

const (
	sandStateUnknown       = "unknown"
	sandStateAvailable     = "available"
	sandStateExhausted     = "exhausted"
	sandStateUnavailable   = "unavailable"
	sandStateRequestFailed = "request_failed"
)

// SubscriptionEntitlement declares the default capability of an account
// membership. Claude is separate from GrokBot because Claude Code models use
// the Sand request surface but are still subject to a plan's named-model
// entitlement.
//
// Empty fields in config inherit the built-in default. Valid values are
// "probe", "allow", and "deny".
type SubscriptionEntitlement struct {
	Cursor  CapabilityMode `json:"cursor"`
	Other   CapabilityMode `json:"other"`
	GrokBot CapabilityMode `json:"grokbot"`
	Claude  CapabilityMode `json:"claude"`
}

var (
	entitlementMu        sync.RWMutex
	entitlementOverrides = map[string]SubscriptionEntitlement{}
)

func defaultSubscriptionEntitlement(membership string) SubscriptionEntitlement {
	base := SubscriptionEntitlement{
		Cursor:  CapabilityProbe,
		Other:   CapabilityProbe,
		GrokBot: CapabilityProbe,
		Claude:  CapabilityProbe,
	}
	if normalizeMembership(membership) != "free" {
		return base
	}
	// This is backed by real upstream evidence: Free accounts returned
	// "Named models unavailable - Free plans can only use Auto" for a named
	// Claude request. GrokBot remains probe because it is a distinct Sand
	// allowance and must be determined by the real response.
	return SubscriptionEntitlement{
		Cursor:  CapabilityAllow,
		Other:   CapabilityDeny,
		GrokBot: CapabilityProbe,
		Claude:  CapabilityDeny,
	}
}

func normalizeMembership(membership string) string {
	membership = strings.ToLower(strings.TrimSpace(membership))
	membership = strings.ReplaceAll(membership, "-", "_")
	membership = strings.ReplaceAll(membership, " ", "_")
	switch membership {
	case "", "unknown", "unknown_plan":
		return "*"
	case "free_plan":
		return "free"
	default:
		return membership
	}
}

func normalizeCapabilityMode(mode CapabilityMode) CapabilityMode {
	switch CapabilityMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case CapabilityAllow:
		return CapabilityAllow
	case CapabilityDeny:
		return CapabilityDeny
	default:
		return CapabilityProbe
	}
}

func mergeEntitlement(base, override SubscriptionEntitlement) SubscriptionEntitlement {
	if override.Cursor != "" {
		base.Cursor = normalizeCapabilityMode(override.Cursor)
	}
	if override.Other != "" {
		base.Other = normalizeCapabilityMode(override.Other)
	}
	if override.GrokBot != "" {
		base.GrokBot = normalizeCapabilityMode(override.GrokBot)
	}
	if override.Claude != "" {
		base.Claude = normalizeCapabilityMode(override.Claude)
	}
	return base
}

// ConfigureSubscriptionEntitlements installs JSON-configured overrides. The
// function copies the supplied map so callers cannot mutate active routing
// policy after startup. A "*" entry is a global override; a normalized
// membership entry overrides it. Empty fields inherit defaults.
func ConfigureSubscriptionEntitlements(overrides map[string]SubscriptionEntitlement) {
	next := make(map[string]SubscriptionEntitlement, len(overrides))
	for membership, entitlement := range overrides {
		key := normalizeMembership(membership)
		next[key] = entitlement
	}
	entitlementMu.Lock()
	entitlementOverrides = next
	entitlementMu.Unlock()
}

// SubscriptionEntitlementFor returns the effective default policy for an
// account membership. It is exposed for diagnostics and focused tests; it
// must not be used to bypass live account state.
func SubscriptionEntitlementFor(membership string) SubscriptionEntitlement {
	key := normalizeMembership(membership)
	result := defaultSubscriptionEntitlement(key)
	entitlementMu.RLock()
	defer entitlementMu.RUnlock()
	if global, ok := entitlementOverrides["*"]; ok {
		result = mergeEntitlement(result, global)
	}
	if specific, ok := entitlementOverrides[key]; ok {
		result = mergeEntitlement(result, specific)
	}
	return result
}

// capabilityForSurface resolves the subscription capability for the concrete
// quota/transport surface selected for a request. The model name alone is not
// sufficient: Claude Code tool/MCP requests keep a Claude model identity but
// are transported and charged through AgentService/Other Models.
func capabilityForSurface(a Account, model, clientType string) CapabilityMode {
	entitlement := SubscriptionEntitlementFor(a.Membership)
	if clientType == "sand" {
		if isClaudeCodeModelName(model) {
			return entitlement.Claude
		}
		return entitlement.GrokBot
	}
	if modelIsCursorBucket(model) {
		return entitlement.Cursor
	}
	return entitlement.Other
}

// capabilityForModel preserves the model-only compatibility contract used by
// legacy callers and tests. Request-aware paths must use capabilityForSurface.
//
//nolint:unused // compatibility wrapper for model-only callers.
func capabilityForModel(a Account, model string) CapabilityMode {
	return capabilityForSurface(a, model, cursorClientTypeForModel(model))
}

func sandStateForAccount(a Account) string {
	state := strings.TrimSpace(a.GrokBotState)
	switch state {
	case sandStateAvailable, sandStateExhausted, sandStateUnavailable, sandStateRequestFailed, sandStateUnknown:
		return state
	}
	// Backward-compatible interpretation for persisted accounts written before
	// GrokBotState existed. "false + <100%" was formerly indistinguishable
	// from missing fields, so it remains unknown rather than being fabricated
	// as quota exhaustion or an unsupported subscription.
	if a.GrokBotEnabled {
		return sandStateAvailable
	}
	if a.GrokBotPct >= 100 {
		return sandStateExhausted
	}
	return sandStateUnknown
}

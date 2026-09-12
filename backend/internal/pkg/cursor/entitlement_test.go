package cursor

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ⚠️ 以下测试因依赖 ai2api 的 Handler/Manager/Store（未移植）而移除：
//   - TestSandStatePersistsAndDisabledUnknownAccountIsNotRecovered
// 对应行为应在 sub2api 侧用 service 层 + 网关做集成测试覆盖。

func TestSubscriptionEntitlementDefaultsAndOverrides(t *testing.T) {
	ConfigureSubscriptionEntitlements(nil)
	t.Cleanup(func() { ConfigureSubscriptionEntitlements(nil) })

	free := SubscriptionEntitlementFor("free plan")
	if free.Cursor != CapabilityAllow || free.Other != CapabilityDeny ||
		free.GrokBot != CapabilityProbe || free.Claude != CapabilityDeny {
		t.Fatalf("free default=%+v", free)
	}
	pro := SubscriptionEntitlementFor("pro")
	if pro.Cursor != CapabilityProbe || pro.Other != CapabilityProbe ||
		pro.GrokBot != CapabilityProbe || pro.Claude != CapabilityProbe {
		t.Fatalf("pro default=%+v", pro)
	}

	ConfigureSubscriptionEntitlements(map[string]SubscriptionEntitlement{
		"*":   {Other: CapabilityDeny},
		"pro": {Claude: CapabilityDeny},
	})
	pro = SubscriptionEntitlementFor("PRO")
	if pro.Other != CapabilityDeny || pro.Claude != CapabilityDeny ||
		pro.Cursor != CapabilityProbe || pro.GrokBot != CapabilityProbe {
		t.Fatalf("merged pro override=%+v", pro)
	}
	unknown := SubscriptionEntitlementFor("unrecognized plan")
	if unknown.Other != CapabilityDeny || unknown.Claude != CapabilityProbe {
		t.Fatalf("global unknown override=%+v", unknown)
	}
}

func TestClaudeCodeSelectionUsesPlanAndSandState(t *testing.T) {
	ConfigureSubscriptionEntitlements(nil)
	t.Cleanup(func() { ConfigureSubscriptionEntitlements(nil) })
	ConfigureSandModels(nil)
	t.Cleanup(func() { ConfigureSandModels(nil) })

	free := Account{
		Membership:     "free",
		UsageAt:        time.Now(),
		OtherModelsPct: 0,
		GrokBotState:   sandStateAvailable,
		GrokBotEnabled: true,
	}
	if AccountUsableForModel(free, "claude-sonnet-5-medium") {
		t.Fatal("Free plan must reject named Claude even if Sand reports available")
	}

	proUnknown := Account{Membership: "pro", GrokBotState: sandStateUnknown}
	if !AccountUsableForModel(proUnknown, "claude-sonnet-5-medium") {
		t.Fatal("Pro account without Sand usage snapshot should be eligible for one real Sand probe")
	}
	proAvailable := Account{Membership: "pro", UsageAt: time.Now(), OtherModelsPct: 100, GrokBotState: sandStateAvailable}
	if !AccountUsableForModel(proAvailable, "claude-sonnet-5-medium") {
		t.Fatal("Pro account with available Sand state should be selectable even if Other Models is full")
	}
	proSandExhausted := Account{Membership: "pro", UsageAt: time.Now(), OtherModelsPct: 10, GrokBotState: sandStateExhausted}
	if AccountUsableForModel(proSandExhausted, "claude-sonnet-5-medium") {
		t.Fatal("Claude Code direct Sand must not use an exhausted Sand account only because Other Models is available")
	}

	ConfigureSandModels([]string{"cursor-grok-4.5-medium"})
	for _, state := range []string{sandStateExhausted, sandStateUnavailable} {
		account := Account{Membership: "pro", GrokBotState: state}
		if AccountUsableForModel(account, "cursor-grok-4.5-medium") {
			t.Fatalf("direct Sand model with Sand state %q must not be selected", state)
		}
	}
	ConfigureSandModels(nil)

	ConfigureSubscriptionEntitlements(map[string]SubscriptionEntitlement{
		"pro":  {Claude: CapabilityDeny},
		"free": {Claude: CapabilityProbe},
	})
	if AccountUsableForModel(proAvailable, "claude-sonnet-5-medium") {
		t.Fatal("configured pro Claude deny must take effect before live Sand state")
	}
	free.NamedModelsUnavailable = true
	if AccountUsableForModel(free, "claude-sonnet-5-medium") {
		t.Fatal("known named-model restriction must outrank a permissive config override")
	}
}

func TestClaudeToolRequestUsesOtherEntitlementSurface(t *testing.T) {
	ConfigureSubscriptionEntitlements(nil)
	t.Cleanup(func() { ConfigureSubscriptionEntitlements(nil) })

	tools := []ToolDef{{Name: "Read", InputSchema: `{"type":"object"}`}}
	free := Account{
		Membership:     "free",
		UsageAt:        time.Now(),
		OtherModelsPct: 10,
		GrokBotState:   sandStateAvailable,
	}
	if AccountUsableForSurface(free, "claude-sonnet-5", requestClientTypeForModel("claude-sonnet-5", tools)) {
		t.Fatal("Free Claude tool request must honor Other Models deny entitlement")
	}

	pro := Account{
		Membership:     "pro",
		UsageAt:        time.Now(),
		OtherModelsPct: 10,
		GrokBotState:   sandStateExhausted,
	}
	ConfigureSubscriptionEntitlements(map[string]SubscriptionEntitlement{
		"pro": {Claude: CapabilityDeny},
	})
	if !AccountUsableForSurface(pro, "claude-sonnet-5", requestClientTypeForModel("claude-sonnet-5", tools)) {
		t.Fatal("Claude deny must not block a tool request routed through Other Models")
	}
	if AccountUsableForSurface(pro, "claude-sonnet-5", "sand") {
		t.Fatal("direct Sand Claude request must still honor Claude deny entitlement")
	}

	ConfigureSubscriptionEntitlements(nil)
	pro.OtherModelsPct = 100
	if AccountUsableForSurface(pro, "claude-sonnet-5", requestClientTypeForModel("claude-sonnet-5", tools)) {
		t.Fatal("Claude tool request must reject an Other Models quota at 100%")
	}
}

func TestSandStatusResponseKeepsMissingFieldsUnknown(t *testing.T) {
	responses := []string{
		`{}`,
		`{"usagePercent":12.5,"hasAvailableUsage":true}`,
		`{"usagePercent":95,"hasAvailableUsage":false}`,
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aiserver.v1.DashboardService/GetSandUsageStatus" {
			http.NotFound(w, r)
			return
		}
		if requests >= len(responses) {
			t.Errorf("unexpected request #%d", requests+1)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(responses[requests]))
		requests++
	}))
	t.Cleanup(server.Close)
	ConfigureUpstream(server.URL, "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	client := NewClient()
	client.shared = server.Client()
	account := &Account{AccessToken: "test-token"}
	first := client.GetSandUsage(account)
	if !first.OK || first.State != sandStateUnknown || first.UsagePercentPresent || first.HasAvailablePresent {
		t.Fatalf("missing fields must stay unknown: %+v", first)
	}
	second := client.GetSandUsage(account)
	if !second.OK || second.State != sandStateAvailable || !second.UsagePercentPresent ||
		!second.HasAvailablePresent || !second.HasAvailable {
		t.Fatalf("available response=%+v", second)
	}
	third := client.GetSandUsage(account)
	if !third.OK || third.State != sandStateUnavailable || !third.UsagePercentPresent ||
		!third.HasAvailablePresent || third.HasAvailable {
		t.Fatalf("explicit unavailable response=%+v", third)
	}
}

func TestSandUsageFullPercentTakesPrecedenceOverAvailabilityFlag(t *testing.T) {
	su := SandUsage{
		OK:                  true,
		UsagePercent:        100,
		UsagePercentPresent: true,
		HasAvailable:        true,
		HasAvailablePresent: true,
	}
	if got := su.EffectiveState(); got != sandStateExhausted {
		t.Fatalf("full Sand usage must be exhausted even when availability flag is stale: got %q", got)
	}

	su.UsagePercent = 99.999999
	if got := su.EffectiveState(); got != sandStateAvailable {
		t.Fatalf("sub-100 Sand usage with availability flag should remain available: got %q", got)
	}
}

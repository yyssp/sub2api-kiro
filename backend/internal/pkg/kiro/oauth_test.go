//go:build unit

package kiro

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildSocialSignInURLUsesAppPortal(t *testing.T) {
	got := BuildSocialSignInURL("http://localhost:49153", "challenge123", "state456")
	want := "https://app.kiro.dev/signin?code_challenge=challenge123&code_challenge_method=S256&redirect_from=KiroIDE&redirect_uri=http%3A%2F%2Flocalhost%3A49153&state=state456"
	if got != want {
		t.Fatalf("BuildSocialSignInURL() = %q, want %q", got, want)
	}
}

func TestBuildSocialTokenRedirectURI(t *testing.T) {
	got := BuildSocialTokenRedirectURI("http://localhost:49153", "/oauth/callback", "github")
	want := "http://localhost:49153/oauth/callback?login_option=github"
	if got != want {
		t.Fatalf("BuildSocialTokenRedirectURI() = %q, want %q", got, want)
	}
}

func TestSessionStoreGetDeletesExpiredSession(t *testing.T) {
	store := NewSessionStore()
	store.Set("expired", &AuthSession{CreatedAt: time.Now().Add(-2 * sessionTTL)})

	session, ok := store.Get("expired")
	if ok || session != nil {
		t.Fatalf("Get(expired) = (%v, %v), want (nil, false)", session, ok)
	}
	if _, exists := store.data["expired"]; exists {
		t.Fatalf("expired session should be deleted from the store")
	}
}

func TestSessionStoreSetPrunesExpiredSessions(t *testing.T) {
	store := NewSessionStore()
	now := time.Now()
	for i := 0; i < sessionCleanupMin; i++ {
		store.data[fmt.Sprintf("expired-%d", i)] = &AuthSession{CreatedAt: now.Add(-2 * sessionTTL)}
	}
	store.setCount = sessionCleanupEvery - 1

	store.Set("fresh", &AuthSession{CreatedAt: now})

	if len(store.data) != 1 {
		t.Fatalf("store size = %d, want 1", len(store.data))
	}
	if _, ok := store.data["fresh"]; !ok {
		t.Fatalf("fresh session should remain after pruning")
	}
}

func TestParseImportedTokenInfersIDCAuthMetadataAndProviderFromClientCredentials(t *testing.T) {
	token, err := ParseImportedToken(`{
		"accessToken": "access-token",
		"refreshToken": "refresh-token",
		"clientId": "client-id",
		"clientSecret": "client-secret"
	}`, "")
	if err != nil {
		t.Fatalf("ParseImportedToken() error = %v", err)
	}

	if token.AuthMethod != "idc" {
		t.Fatalf("AuthMethod = %q, want idc", token.AuthMethod)
	}
	if token.Provider != ProviderBuilderId {
		t.Fatalf("Provider = %q, want %q", token.Provider, ProviderBuilderId)
	}
	if token.Region != defaultIDCRegion {
		t.Fatalf("Region = %q, want %q", token.Region, defaultIDCRegion)
	}
}

func TestParseImportedTokenInfersIDCAuthMetadataFromDeviceRegistration(t *testing.T) {
	token, err := ParseImportedToken(`{
		"accessToken": "access-token",
		"refreshToken": "refresh-token",
		"provider": "Enterprise",
		"clientIdHash": "client-id-hash"
	}`, `{
		"clientId": "client-id",
		"clientSecret": "client-secret"
	}`)
	if err != nil {
		t.Fatalf("ParseImportedToken() error = %v", err)
	}

	if token.ClientID != "client-id" {
		t.Fatalf("ClientID = %q, want client-id", token.ClientID)
	}
	if token.ClientSecret != "client-secret" {
		t.Fatalf("ClientSecret = %q, want client-secret", token.ClientSecret)
	}
	if token.AuthMethod != "idc" {
		t.Fatalf("AuthMethod = %q, want idc", token.AuthMethod)
	}
	if token.Provider != ProviderEnterprise {
		t.Fatalf("Provider = %q, want %q", token.Provider, ProviderEnterprise)
	}
}

func TestParseImportedTokenDoesNotRequireProvider(t *testing.T) {
	cases := []struct {
		name           string
		tokenJSON      string
		wantProvider   string
		wantAuthMethod string
	}{
		{
			name:           "social IDE export without provider",
			tokenJSON:      `{"accessToken":"access-token","refreshToken":"refresh-token","authMethod":"social"}`,
			wantAuthMethod: "social",
		},
		{
			name:           "blank legacy provider",
			tokenJSON:      `{"accessToken":"access-token","provider":"","authMethod":"social"}`,
			wantAuthMethod: "social",
		},
		{
			name:           "IDC import infers Builder ID provider",
			tokenJSON:      `{"accessToken":"access-token","clientId":"c","clientSecret":"s"}`,
			wantProvider:   ProviderBuilderId,
			wantAuthMethod: "idc",
		},
		{
			name:           "known provider round trips",
			tokenJSON:      `{"accessToken":"access-token","provider":"Github","authMethod":"social"}`,
			wantProvider:   ProviderGithub,
			wantAuthMethod: "social",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ParseImportedToken(tc.tokenJSON, "")
			if err != nil {
				t.Fatalf("ParseImportedToken() error for %s: %v", tc.name, err)
			}
			if token.Provider != tc.wantProvider {
				t.Fatalf("Provider = %q, want %q", token.Provider, tc.wantProvider)
			}
			if token.AuthMethod != tc.wantAuthMethod {
				t.Fatalf("AuthMethod = %q, want %q", token.AuthMethod, tc.wantAuthMethod)
			}
		})
	}
}

func TestParseImportedTokenRejectsUnknownNonEmptyProvider(t *testing.T) {
	_, err := ParseImportedToken(`{
		"accessToken":"access-token",
		"refreshToken":"refresh-token",
		"authMethod":"social",
		"provider":"Gitlab"
	}`, "")
	if err == nil {
		t.Fatal("ParseImportedToken() expected unknown provider error")
	}
}

func TestParseImportedTokenRequiresDeviceRegistrationForClientIDHash(t *testing.T) {
	_, err := ParseImportedToken(`{"accessToken":"access-token","clientIdHash":"hash"}`, "")
	if err == nil {
		t.Fatal("ParseImportedToken() expected device registration error")
	}
}

func TestParseImportedTokenAcceptsKiroIDEFieldAliases(t *testing.T) {
	token, err := ParseImportedToken(`{
		"access_token":"access-token",
		"refresh_token":"refresh-token",
		"auth_method":"social",
		"apiRegion":"eu-central-1",
		"machineId":"machine-123",
		"subscriptionTitle":"KIRO PRO"
	}`, "")
	if err != nil {
		t.Fatalf("ParseImportedToken() error = %v", err)
	}
	if token.APIRegion != "eu-central-1" {
		t.Fatalf("APIRegion = %q, want eu-central-1", token.APIRegion)
	}
	if token.Region != "" {
		t.Fatalf("Region = %q, want empty for apiRegion-only social export", token.Region)
	}
	if token.MachineID != "machine-123" {
		t.Fatalf("MachineID = %q, want machine-123", token.MachineID)
	}
	if token.SubscriptionTitle != "KIRO PRO" {
		t.Fatalf("SubscriptionTitle = %q, want KIRO PRO", token.SubscriptionTitle)
	}
}

func TestParseImportedTokensAcceptsKiroIDEArray(t *testing.T) {
	tokens, err := ParseImportedTokens(`[
		{"accessToken":"access-1","refreshToken":"refresh-1","authMethod":"social","apiRegion":"us-east-1"},
		{"accessToken":"access-2","refreshToken":"refresh-2","authMethod":"social","apiRegion":"eu-west-1"}
	]`, "")
	if err != nil {
		t.Fatalf("ParseImportedTokens() error = %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("len(tokens) = %d, want 2", len(tokens))
	}
	if tokens[1].APIRegion != "eu-west-1" {
		t.Fatalf("second token api region = %q, want eu-west-1", tokens[1].APIRegion)
	}
}

func TestParseImportedTokensRejectsGenericOAuthJSON(t *testing.T) {
	_, err := ParseKiroCredentialExport(`{
		"access_token":"unrelated-oauth-access",
		"refresh_token":"unrelated-oauth-refresh",
		"auth_method":"social"
	}`, "")
	if err == nil || !strings.Contains(err.Error(), "not a recognized Kiro credential export") {
		t.Fatalf("ParseImportedTokens() error = %v, want Kiro format rejection", err)
	}
}

func TestParseImportedTokensRejectsGenericAPIKeyJSON(t *testing.T) {
	_, err := ParseKiroCredentialExport(`{
		"auth_method":"api_key",
		"api_key":"sk-unrelated-provider-key"
	}`, "")
	if err == nil || !strings.Contains(err.Error(), "not a recognized Kiro credential export") {
		t.Fatalf("ParseImportedTokens() error = %v, want Kiro format rejection", err)
	}
}

func TestParseImportedTokensAcceptsMixedOAuthAndAPIKeyExport(t *testing.T) {
	tokens, err := ParseImportedTokens(`[
		{
			"accessToken":"social-access",
			"refreshToken":"social-refresh",
			"authMethod":"social",
			"apiRegion":"us-east-1",
			"machineId":"social-machine"
		},
		{
			"accessToken":"idc-access",
			"refreshToken":"idc-refresh",
			"authMethod":"idc",
			"clientId":"idc-client",
			"clientSecret":"idc-secret",
			"machineId":"idc-machine"
		},
		{
			"authMethod":"api_key",
			"kiroApiKey":"ksk_synthetic_key",
			"endpoint":"cli",
			"machineId":"key-machine",
			"subscriptionTitle":"Kiro Pro"
		}
	]`, "")
	if err != nil {
		t.Fatalf("ParseImportedTokens() error = %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("len(tokens) = %d, want 3", len(tokens))
	}
	if tokens[0].AuthMethod != "social" || tokens[1].AuthMethod != "idc" {
		t.Fatalf("unexpected OAuth methods: %q, %q", tokens[0].AuthMethod, tokens[1].AuthMethod)
	}
	if tokens[2].AuthMethod != "api_key" {
		t.Fatalf("API key AuthMethod = %q, want api_key", tokens[2].AuthMethod)
	}
	if tokens[2].APIKey != "ksk_synthetic_key" {
		t.Fatalf("API key = %q, want synthetic key", tokens[2].APIKey)
	}
	if tokens[2].AccessToken != "" {
		t.Fatalf("API key entry access token = %q, want empty", tokens[2].AccessToken)
	}
	if tokens[2].MachineID != "key-machine" || tokens[2].SubscriptionTitle != "Kiro Pro" {
		t.Fatalf("API key metadata was not preserved")
	}
}

func TestParseImportedTokenAcceptsAPIKeyAliases(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{name: "Kiro IDE camel case", key: "kiroApiKey"},
		{name: "normalized snake case", key: "kiro_api_key"},
		{name: "stored account key", key: "api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ParseImportedToken(`{"authMethod":"api_key","`+tc.key+`":"ksk_synthetic_key"}`, "")
			if err != nil {
				t.Fatalf("ParseImportedToken() error = %v", err)
			}
			if token.AuthMethod != "api_key" {
				t.Fatalf("AuthMethod = %q, want api_key", token.AuthMethod)
			}
			if token.APIKey != "ksk_synthetic_key" {
				t.Fatalf("APIKey = %q, want synthetic key", token.APIKey)
			}
		})
	}
}

func TestParseImportedTokenRejectsMissingRequiredCredentialByType(t *testing.T) {
	_, err := ParseImportedToken(`{"authMethod":"api_key"}`, "")
	if err == nil || err.Error() != "api key is empty" {
		t.Fatalf("missing API key error = %v, want api key is empty", err)
	}

	_, err = ParseImportedToken(`{"authMethod":"social","refreshToken":"synthetic-refresh"}`, "")
	if err == nil || err.Error() != "access token is empty" {
		t.Fatalf("missing OAuth access token error = %v, want access token is empty", err)
	}
}

func TestParseImportedTokenNormalizesExpiresAt(t *testing.T) {
	cases := []struct {
		name      string
		expiresAt string
	}{
		{"utc with millis", "2026-06-29T09:33:49.114Z"},
		{"utc no millis", "2026-06-29T09:33:49Z"},
		{"naive treated as utc", "2026-09-27T08:46:31.070"},
		{"with offset", "2026-06-29T16:56:19+08:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ParseImportedToken(`{
				"accessToken": "access-token",
				"authMethod": "social",
				"expiresAt": "`+tc.expiresAt+`"
			}`, "")
			if err != nil {
				t.Fatalf("ParseImportedToken() error = %v", err)
			}
			// 归一化后必须能被 RFC3339 解析,且为本地时区表示。
			parsed, perr := time.Parse(time.RFC3339, token.ExpiresAt)
			if perr != nil {
				t.Fatalf("ExpiresAt %q not RFC3339: %v", token.ExpiresAt, perr)
			}
			if token.ExpiresAt != parsed.Local().Format(time.RFC3339) {
				t.Fatalf("ExpiresAt = %q, want local RFC3339", token.ExpiresAt)
			}
		})
	}
}

func TestParseImportedTokenRejectsInvalidExpiresAt(t *testing.T) {
	if _, err := ParseImportedToken(`{
		"accessToken": "access-token",
		"authMethod": "social",
		"expiresAt": "not-a-time"
	}`, ""); err == nil {
		t.Fatalf("ParseImportedToken() expected error for invalid expiresAt, got nil")
	}
}

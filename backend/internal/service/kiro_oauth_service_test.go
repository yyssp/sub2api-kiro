//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func TestKiroIDCAuthRedirectURIUsesLoopbackIP(t *testing.T) {
	require.Equal(t, "http://127.0.0.1:9876/oauth/callback", kiroIDCRedirectURI)
}

func TestKiroSocialAuthRedirectURIUsesLoopbackIP(t *testing.T) {
	require.Equal(t, "http://localhost:49153", kiroSocialRedirectURI)
}

func TestBuildKiroSocialExchangeRedirectURIUsesProviderDefault(t *testing.T) {
	require.Equal(
		t,
		"http://localhost:49153/oauth/callback?login_option=github",
		buildKiroSocialExchangeRedirectURI("http://localhost:49153", kiropkg.ProviderGithub, "", ""),
	)
}

func TestBuildKiroSocialExchangeRedirectURIPreservesParsedCallbackData(t *testing.T) {
	require.Equal(
		t,
		"http://localhost:49153/signin/callback?login_option=google",
		buildKiroSocialExchangeRedirectURI("http://localhost:49153", kiropkg.ProviderGithub, "/signin/callback", "google"),
	)
}

func TestKiroOAuthService_ExchangeCodeRejectsExpiredSession(t *testing.T) {
	svc := NewKiroOAuthService(nil)
	svc.sessionStore.Set("expired-session", &kiropkg.AuthSession{
		State:     "expected-state",
		CreatedAt: time.Now().Add(-11 * time.Minute),
	})

	_, err := svc.ExchangeCode(context.Background(), &KiroExchangeCodeInput{
		SessionID: "expired-session",
		State:     "expected-state",
		Code:      "auth-code",
	})
	require.EqualError(t, err, "session not found or expired")
}

func TestKiroOAuthService_GenerateAuthURLCreatesExternalIdpSession(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	result, err := svc.GenerateAuthURL(context.Background(), &KiroGenerateAuthURLInput{
		Provider: kiropkg.ProviderExternalIdp,
	})

	require.NoError(t, err)
	require.NotEmpty(t, result.AuthURL)
	require.NotEmpty(t, result.SessionID)
	require.NotEmpty(t, result.State)
	session, ok := svc.sessionStore.Get(result.SessionID)
	require.True(t, ok)
	require.Equal(t, "external_idp", session.AuthType)
	require.Equal(t, kiropkg.ProviderExternalIdp, session.Provider)
	require.Equal(t, kiroSocialRedirectURI, session.RedirectURI)
}

func TestKiroOAuthService_ExchangeCodeDoesNotSpecialCaseExternalIdpDescriptorInSocialSession(t *testing.T) {
	svc := NewKiroOAuthService(nil)
	svc.sessionStore.Set("social-session", &kiropkg.AuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		CreatedAt:    time.Now(),
		AuthType:     "social",
		Provider:     kiropkg.ProviderGoogle,
		RedirectURI:  kiroSocialRedirectURI,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svc.ExchangeCode(ctx, &KiroExchangeCodeInput{
		SessionID: "social-session",
		State:     "expected-state",
		Code:      "http://localhost:49153/signin/callback?login_option=external_idp&issuer_url=https%3A%2F%2Flogin.microsoftonline.com%2Ftenant%2Fv2.0&client_id=client-id&scopes=openid+profile",
	})

	require.ErrorIs(t, err, context.Canceled)
}

func TestKiroOAuthService_ExchangeCodePreparesExternalIdpAuthorizationFromDescriptor(t *testing.T) {
	previous := kiroDiscoverExternalIdp
	kiroDiscoverExternalIdp = func(ctx context.Context, proxyURL, issuerURL string) (string, string, error) {
		require.Equal(t, "", proxyURL)
		require.Equal(t, "https://login.microsoftonline.com/tenant/v2.0", issuerURL)
		return "https://login.microsoftonline.com/tenant/oauth2/v2.0/authorize", "https://login.microsoftonline.com/tenant/oauth2/v2.0/token", nil
	}
	t.Cleanup(func() { kiroDiscoverExternalIdp = previous })

	svc := NewKiroOAuthService(nil)
	svc.sessionStore.Set("external-session", &kiropkg.AuthSession{
		State:        "expected-state",
		CodeVerifier: "initial-verifier",
		CreatedAt:    time.Now(),
		AuthType:     "external_idp",
		Provider:     kiropkg.ProviderExternalIdp,
		RedirectURI:  kiroSocialRedirectURI,
	})

	issuerURL := "https://login.microsoftonline.com/tenant/v2.0"
	result, err := svc.ExchangeCode(context.Background(), &KiroExchangeCodeInput{
		SessionID: "external-session",
		State:     "expected-state",
		Code:      "http://localhost:49153/signin/callback?login_option=external_idp&issuer_url=" + url.QueryEscape(issuerURL) + "&client_id=client-id&scopes=openid+profile+offline_access&login_hint=user%40example.com",
	})

	require.NoError(t, err)
	require.Equal(t, "external-session", result.SessionID)
	require.NotEqual(t, "expected-state", result.State)
	require.Contains(t, result.AuthURL, "https://login.microsoftonline.com/tenant/oauth2/v2.0/authorize?")
	require.Contains(t, result.AuthURL, "client_id=client-id")
	require.Contains(t, result.AuthURL, "redirect_uri=http%3A%2F%2Flocalhost%3A3128%2Foauth%2Fcallback")
	require.Contains(t, result.AuthURL, "scope=openid+profile+offline_access")
	require.Contains(t, result.AuthURL, "login_hint=user%40example.com")

	session, ok := svc.sessionStore.Get("external-session")
	require.True(t, ok)
	require.Equal(t, "external_idp", session.AuthType)
	require.Equal(t, kiropkg.ProviderExternalIdp, session.Provider)
	require.Equal(t, "client-id", session.ClientID)
	require.Equal(t, "https://login.microsoftonline.com/tenant/oauth2/v2.0/token", session.TokenEndpoint)
	require.Equal(t, issuerURL, session.IssuerURL)
	require.Equal(t, "openid profile offline_access", session.Scopes)
	require.Equal(t, "user@example.com", session.LoginHint)
	require.Equal(t, kiroExternalIdpRedirectURI, session.RedirectURI)
	require.NotEqual(t, "initial-verifier", session.CodeVerifier)
	require.Equal(t, result.State, session.State)
}

func TestKiroOAuthService_ExchangeCodeRejectsFinalExternalIdpCodeBeforeDescriptor(t *testing.T) {
	svc := NewKiroOAuthService(nil)
	svc.sessionStore.Set("external-session", &kiropkg.AuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		CreatedAt:    time.Now(),
		AuthType:     "external_idp",
		Provider:     kiropkg.ProviderExternalIdp,
		RedirectURI:  kiroSocialRedirectURI,
	})

	_, err := svc.ExchangeCode(context.Background(), &KiroExchangeCodeInput{
		SessionID: "external-session",
		State:     "expected-state",
		Code:      "final-code",
	})

	require.EqualError(t, err, "kiro external_idp callback descriptor is required")
}

func TestKiroOAuthService_RefreshTokenRejectsMissingRefreshToken(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	_, err := svc.RefreshToken(context.Background(), &KiroRefreshTokenInput{
		AuthMethod: "social",
	})

	require.EqualError(t, err, "kiro refresh token is required")
}

func TestKiroOAuthService_RefreshTokenRejectsIDCMissingClientCredentials(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	_, err := svc.RefreshToken(context.Background(), &KiroRefreshTokenInput{
		AuthMethod:   "idc",
		RefreshToken: "refresh-token",
		ClientID:     "client-id",
	})

	require.EqualError(t, err, "kiro idc refresh requires client_id and client_secret")
}

func TestResolveKiroRefreshAuthMethodUsesAuthenticationFacts(t *testing.T) {
	cases := []struct {
		name          string
		authMethod    string
		clientID      string
		clientSecret  string
		tokenEndpoint string
		want          string
		wantErr       string
	}{
		{name: "explicit social", authMethod: "social", want: "social"},
		{name: "explicit idc", authMethod: "IDC", want: "idc"},
		{name: "explicit external idp", authMethod: "external_idp", want: "external_idp"},
		{name: "explicit method overrides field shape", authMethod: "social", clientID: "client-id", clientSecret: "secret", tokenEndpoint: "https://issuer.example/token", want: "social"},
		{name: "token endpoint and client id infer external idp", clientID: "client-id", tokenEndpoint: "https://issuer.example/token", want: "external_idp"},
		{name: "client credentials infer idc", clientID: "client-id", clientSecret: "secret", want: "idc"},
		{name: "incomplete client data infers social", clientID: "client-id", want: "social"},
		{name: "no authentication facts infers social", want: "social"},
		{name: "api key rejects refresh", authMethod: "api_key", wantErr: "kiro api_key accounts do not support refresh_token"},
		{name: "unknown method rejects refresh", authMethod: "other", wantErr: `unsupported kiro auth method: "other"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveKiroRefreshAuthMethod(tc.authMethod, tc.clientID, tc.clientSecret, tc.tokenEndpoint)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseKiroExternalIdpDescriptorFromCallbackURL(t *testing.T) {
	descriptor, ok := parseKiroExternalIdpDescriptor("http://localhost:49153/signin/callback?login_option=external_idp&issuer_url=https%3A%2F%2Flogin.example.com%2Ftenant%2Fv2.0&client_id=client-id&scopes=openid+profile+offline_access&login_hint=user%40example.com")

	require.True(t, ok)
	require.Equal(t, "client-id", descriptor.ClientID)
	require.Equal(t, "https://login.example.com/tenant/v2.0", descriptor.IssuerURL)
	require.Equal(t, "openid profile offline_access", descriptor.Scopes)
	require.Equal(t, "user@example.com", descriptor.LoginHint)
}

func TestParseKiroExternalIdpDescriptorIgnoresOAuthCallback(t *testing.T) {
	_, ok := parseKiroExternalIdpDescriptor("http://localhost:49153/oauth/callback?issuer_url=https%3A%2F%2Flogin.example.com%2Ftenant%2Fv2.0&client_id=client-id")

	require.False(t, ok)
}

func TestKiroOAuthService_RefreshTokenUsesExternalIdpTokenEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		require.Equal(t, "refresh-token", r.Form.Get("refresh_token"))
		require.Equal(t, "client-id", r.Form.Get("client_id"))
		require.Equal(t, "openid profile offline_access", r.Form.Get("scope"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access-token","expires_in":3600}`))
	}))
	defer server.Close()

	svc := NewKiroOAuthService(nil)
	token, err := svc.RefreshToken(context.Background(), &KiroRefreshTokenInput{
		AuthMethod:    "external_idp",
		RefreshToken:  "refresh-token",
		ClientID:      "client-id",
		TokenEndpoint: server.URL,
		IssuerURL:     "https://login.example.com/tenant/v2.0",
		Scopes:        "openid profile offline_access",
		ProfileArn:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/EXTERNAL",
	})

	require.NoError(t, err)
	require.Equal(t, "new-access-token", token.AccessToken)
	require.Equal(t, "refresh-token", token.RefreshToken)
	require.Equal(t, "external_idp", token.AuthMethod)
	require.Equal(t, kiropkg.ProviderExternalIdp, token.Provider)
	require.Equal(t, "client-id", token.ClientID)
	require.Equal(t, server.URL, token.TokenEndpoint)
	require.Equal(t, "https://login.example.com/tenant/v2.0", token.IssuerURL)
	require.Equal(t, "openid profile offline_access", token.Scopes)
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:123456789012:profile/EXTERNAL", token.ProfileArn)
}

func TestKiroOAuthService_BuildAccountCredentialsPreservesExternalIdpMetadata(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	credentials := svc.BuildAccountCredentials(&KiroTokenInfo{
		AccessToken:   "access-token",
		RefreshToken:  "refresh-token",
		AuthMethod:    "external_idp",
		Provider:      kiropkg.ProviderExternalIdp,
		ClientID:      "client-id",
		TokenEndpoint: "https://login.example.com/oauth2/v2.0/token",
		IssuerURL:     "https://login.example.com/tenant/v2.0",
		Scopes:        "openid profile offline_access",
	})

	require.Equal(t, "external_idp", credentials["auth_method"])
	require.Equal(t, kiropkg.ProviderExternalIdp, credentials["provider"])
	require.Equal(t, "client-id", credentials["client_id"])
	require.Equal(t, "https://login.example.com/oauth2/v2.0/token", credentials["token_endpoint"])
	require.Equal(t, "https://login.example.com/tenant/v2.0", credentials["issuer_url"])
	require.Equal(t, "openid profile offline_access", credentials["scopes"])
}

func TestKiroOAuthService_BuildAccountCredentialsPreservesAuthRegion(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	credentials := svc.BuildAccountCredentials(&KiroTokenInfo{
		AuthMethod: "idc",
		Region:     "us-east-1",
		AuthRegion: "eu-west-1",
		APIRegion:  "us-east-1",
	})

	require.Equal(t, "us-east-1", credentials["region"])
	require.Equal(t, "eu-west-1", credentials["auth_region"])
	require.Equal(t, "us-east-1", credentials["api_region"])
}

func TestKiroOAuthService_ImportTokenClassifiesMixedCredentialEntries(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	result, err := svc.ImportToken(&KiroImportTokenInput{
		TokenJSON: `[
			{
				"accessToken":"synthetic-social-access",
				"refreshToken":"synthetic-social-refresh",
				"authMethod":"social",
				"apiRegion":"us-east-1"
			},
			{
				"authMethod":"api_key",
				"kiroApiKey":"ksk_synthetic_key",
				"endpoint":"cli",
				"machineId":"synthetic-machine",
				"subscriptionTitle":"Kiro Pro"
			}
		]`,
	})

	require.NoError(t, err)
	require.Len(t, result.Entries, 2)

	oauthEntry := result.Entries[0]
	require.Equal(t, "oauth", oauthEntry.AccountType)
	require.NotNil(t, oauthEntry.KiroTokenInfo)
	require.Equal(t, "synthetic-social-access", oauthEntry.AccessToken)
	require.Empty(t, oauthEntry.APIKey)

	apiKeyEntry := result.Entries[1]
	require.Equal(t, "apikey", apiKeyEntry.AccountType)
	require.NotNil(t, apiKeyEntry.KiroTokenInfo)
	require.Equal(t, "api_key", apiKeyEntry.AuthMethod)
	require.Equal(t, "ksk_synthetic_key", apiKeyEntry.APIKey)
	require.Empty(t, apiKeyEntry.AccessToken)
	require.Equal(t, "synthetic-machine", apiKeyEntry.MachineID)
	require.Equal(t, "Kiro Pro", apiKeyEntry.SubscriptionTitle)

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	var payload struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(encoded, &payload))
	require.Len(t, payload.Entries, 2)
	require.Equal(t, "oauth", payload.Entries[0]["account_type"])
	require.Equal(t, "synthetic-social-access", payload.Entries[0]["access_token"])
	require.NotContains(t, payload.Entries[0], "api_key")
	require.Equal(t, "apikey", payload.Entries[1]["account_type"])
	require.Equal(t, "ksk_synthetic_key", payload.Entries[1]["api_key"])
	require.NotContains(t, payload.Entries[1], "access_token")
}

func TestKiroOAuthService_RefreshTokenRejectsExternalIdpMissingMetadata(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	_, err := svc.RefreshToken(context.Background(), &KiroRefreshTokenInput{
		AuthMethod:   "external_idp",
		RefreshToken: "refresh-token",
		ClientID:     "client-id",
	})

	require.EqualError(t, err, "kiro external_idp refresh requires client_id and token_endpoint")
}

func TestKiroOAuthServicePublicMethodsRejectNilInput(t *testing.T) {
	svc := NewKiroOAuthService(nil)

	_, err := svc.GenerateAuthURL(context.Background(), nil)
	require.EqualError(t, err, "kiro auth url input is required")

	_, err = svc.ExchangeCode(context.Background(), nil)
	require.EqualError(t, err, "kiro code exchange input is required")

	_, err = svc.GenerateIDCAuthURL(context.Background(), nil)
	require.EqualError(t, err, "kiro idc auth url input is required")

	_, err = svc.RefreshToken(context.Background(), nil)
	require.EqualError(t, err, "kiro refresh token input is required")

	_, err = svc.RefreshAccountToken(context.Background(), nil)
	require.EqualError(t, err, "kiro account is required")

	_, err = svc.ImportToken(nil)
	require.EqualError(t, err, "kiro import token input is required")
}

func cloneKiroTestCredentials(base map[string]any, provider string) map[string]any {
	credentials := make(map[string]any, len(base)+1)
	for key, value := range base {
		credentials[key] = value
	}
	credentials["provider"] = provider
	return credentials
}

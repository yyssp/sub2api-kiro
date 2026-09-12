package kiro

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/google/uuid"
)

const (
	socialAuthPortalURL = "https://app.kiro.dev"
	socialAuthEndpoint  = "https://prod.us-east-1.auth.desktop.kiro.dev"
	defaultIDCRegion    = "us-east-1"
	BuilderIDStartURL   = "https://view.awsapps.com/start"
	sessionTTL          = 10 * time.Minute
	sessionCleanupEvery = 32
	sessionCleanupMin   = 32
)

var allowedExternalIdpHostSuffixes = []string{
	".microsoftonline.com",
	".microsoftonline.us",
	".microsoftonline.cn",
}

var (
	socialAuthEndpointURL = socialAuthEndpoint
	oidcEndpointOverride  = ""
)

type SocialProvider string

const (
	SocialProviderGoogle SocialProvider = "Google"
	SocialProviderGitHub SocialProvider = "Github"
)

const (
	ProviderGoogle      = "Google"
	ProviderGithub      = "Github"
	ProviderBuilderId   = "BuilderId"
	ProviderEnterprise  = "Enterprise"
	ProviderExternalIdp = "ExternalIdp"
)

func IsValidKiroProvider(p string) bool {
	switch strings.TrimSpace(p) {
	case ProviderGoogle, ProviderGithub, ProviderBuilderId, ProviderEnterprise, ProviderExternalIdp:
		return true
	default:
		return false
	}
}

func resolveIDCProvider(startURL string) string {
	if strings.TrimSpace(startURL) == "" || strings.EqualFold(strings.TrimRight(strings.TrimSpace(startURL), "/"), strings.TrimRight(BuilderIDStartURL, "/")) {
		return ProviderBuilderId
	}
	return ProviderEnterprise
}

// normalizeKiroExpiresAt 把导入的 expiresAt 归一化为带本地时区偏移的 RFC3339。
// 兼容多种来源格式:带 Z(UTC)、带毫秒、带时区偏移、naive(无时区,按 UTC 处理)。
// 输出对齐 OAuth 登录流程(time.RFC3339 + 服务器本地时区)。
func normalizeKiroExpiresAt(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("expiresAt is empty")
	}
	// 优先尝试带时区的标准格式(含 Z 或 ±hh:mm 偏移)。
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999", // naive,无时区
		"2006-01-02T15:04:05",
	}
	for i, layout := range layouts {
		var (
			t   time.Time
			err error
		)
		if i >= 2 {
			// naive 格式按 UTC 解析。
			t, err = time.ParseInLocation(layout, value, time.UTC)
		} else {
			t, err = time.Parse(layout, value)
		}
		if err == nil {
			return t.Local().Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("invalid expiresAt format: %q", raw)
}

type AuthSession struct {
	State         string
	CodeVerifier  string
	ProxyURL      string
	CreatedAt     time.Time
	AuthType      string
	Provider      string
	RedirectURI   string
	ClientID      string
	ClientSecret  string
	Region        string
	StartURL      string
	TokenEndpoint string
	IssuerURL     string
	Scopes        string
	LoginHint     string
}

type SessionStore struct {
	mu       sync.RWMutex
	data     map[string]*AuthSession
	setCount uint64
}

func NewSessionStore() *SessionStore {
	return &SessionStore{data: make(map[string]*AuthSession)}
}

func (s *SessionStore) Get(id string) (*AuthSession, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.data[id]
	if ok && sessionExpired(session, now) {
		delete(s.data, id)
		return nil, false
	}
	return session, ok
}

func (s *SessionStore) Set(id string, session *AuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCount++
	if len(s.data) >= sessionCleanupMin && s.setCount%sessionCleanupEvery == 0 {
		s.pruneExpiredLocked(time.Now())
	}
	s.data[id] = session
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
}

func (s *SessionStore) pruneExpiredLocked(now time.Time) {
	for id, session := range s.data {
		if sessionExpired(session, now) {
			delete(s.data, id)
		}
	}
}

func sessionExpired(session *AuthSession, now time.Time) bool {
	if session == nil {
		return true
	}
	if session.CreatedAt.IsZero() {
		return true
	}
	return now.After(session.CreatedAt.Add(sessionTTL))
}

type TokenData struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	// APIKey is only used while adapting Kiro IDE API-key exports. Account
	// storage keeps the canonical current-project key as credentials.api_key.
	APIKey       string `json:"kiroApiKey,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	AuthMethod   string `json:"authMethod,omitempty"`
	Provider     string `json:"provider,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	ClientIDHash string `json:"clientIdHash,omitempty"`
	Email        string `json:"email,omitempty"`
	StartURL     string `json:"startUrl,omitempty"`
	// Region is the IAM Identity Center/OIDC region used for token refresh.
	Region string `json:"region,omitempty"`
	// AuthRegion is the explicit OIDC region used for token refresh. Some Kiro
	// exports keep it separate from the general credential region.
	AuthRegion string `json:"authRegion,omitempty"`
	// APIRegion is the Kiro runtime API region. It must remain distinct from
	// Region because Kiro IDE social exports use apiRegion without IDC data.
	APIRegion         string `json:"apiRegion,omitempty"`
	MachineID         string `json:"machineId,omitempty"`
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"`
	TokenEndpoint     string `json:"tokenEndpoint,omitempty"`
	IssuerURL         string `json:"issuerUrl,omitempty"`
	Scopes            string `json:"scopes,omitempty"`
}

// UnmarshalJSON accepts both Kiro IDE exports (camelCase) and the normalized
// snake_case credential shape used by account storage. This keeps the Kiro
// adapter tolerant of exports from different clients without adding Kiro-only
// fields to the global account model.
func (t *TokenData) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	readString := func(keys ...string) string {
		for _, key := range keys {
			value, ok := raw[key]
			if !ok {
				continue
			}
			var result string
			if err := json.Unmarshal(value, &result); err == nil && strings.TrimSpace(result) != "" {
				return strings.TrimSpace(result)
			}
		}
		return ""
	}

	t.AccessToken = readString("accessToken", "access_token")
	t.RefreshToken = readString("refreshToken", "refresh_token")
	t.APIKey = readString("kiroApiKey", "kiro_api_key", "apiKey", "api_key")
	t.ProfileArn = readString("profileArn", "profile_arn")
	t.ExpiresAt = readString("expiresAt", "expires_at")
	t.AuthMethod = readString("authMethod", "auth_method")
	t.Provider = readString("provider")
	t.ClientID = readString("clientId", "client_id")
	t.ClientSecret = readString("clientSecret", "client_secret")
	t.ClientIDHash = readString("clientIdHash", "client_id_hash")
	t.Email = readString("email")
	t.StartURL = readString("startUrl", "start_url")
	t.Region = readString("region")
	t.AuthRegion = readString("authRegion", "auth_region")
	t.APIRegion = readString("apiRegion", "api_region")
	t.MachineID = readString("machineId", "machine_id")
	t.SubscriptionTitle = readString("subscriptionTitle", "subscription_title")
	t.TokenEndpoint = readString("tokenEndpoint", "token_endpoint")
	t.IssuerURL = readString("issuerUrl", "issuer_url")
	t.Scopes = readString("scopes")
	return nil
}

type socialTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ProfileArn   string `json:"profileArn"`
	ExpiresIn    int    `json:"expiresIn"`
}

type registerClientResponse struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type createTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ProfileArn   string `json:"profileArn"`
	ExpiresIn    int    `json:"expiresIn"`
}

type externalIdpTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type externalIdpDiscoveryResponse struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

type userInfoResponse struct {
	Email string `json:"email"`
}

type deviceRegistration struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type RefreshTokenInvalidError struct {
	StatusCode int
	Body       string
}

func (e *RefreshTokenInvalidError) Error() string {
	if e == nil {
		return ""
	}
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return "kiro refresh token invalid (invalid_grant)"
	}
	return fmt.Sprintf("kiro refresh token invalid (invalid_grant, status %d): %s", e.StatusCode, body)
}

func GenerateSessionID() string {
	return uuid.NewString()
}

func GenerateState() (string, error) {
	return randomURLSafe(16)
}

func GenerateCodeVerifier() (string, error) {
	return randomURLSafe(32)
}

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func GenerateCodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func BuildSocialSignInURL(redirectURI, codeChallenge, state string) string {
	params := url.Values{}
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("redirect_uri", redirectURI)
	params.Set("redirect_from", "KiroIDE")
	return fmt.Sprintf("%s/signin?%s", socialAuthPortalURL, params.Encode())
}

func BuildSocialTokenRedirectURI(baseRedirectURI, callbackPath, loginOption string) string {
	redirectURI := strings.TrimRight(strings.TrimSpace(baseRedirectURI), "/")
	if redirectURI == "" {
		return ""
	}
	path := strings.TrimSpace(callbackPath)
	if path == "" {
		path = "/oauth/callback"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	fullRedirectURI := redirectURI + path
	if option := strings.TrimSpace(loginOption); option != "" {
		return fullRedirectURI + "?login_option=" + url.QueryEscape(option)
	}
	return fullRedirectURI
}

func CreateSocialToken(ctx context.Context, proxyURL, code, codeVerifier, redirectURI string) (*TokenData, error) {
	payload := map[string]string{
		"code":          code,
		"code_verifier": codeVerifier,
		"redirect_uri":  redirectURI,
	}
	var resp socialTokenResponse
	if err := doJSON(ctx, proxyURL, http.MethodPost, socialAuthEndpointURL+"/oauth/token", payload, &resp, BuildLoginHeaders(shortSHA(codeVerifier), BuildMachineID("", "", "codeVerifier:"+codeVerifier))); err != nil {
		return nil, err
	}
	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return &TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ProfileArn:   resp.ProfileArn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
		AuthMethod:   "social",
		Region:       defaultIDCRegion,
	}, nil
}

func RefreshSocialToken(ctx context.Context, proxyURL, refreshToken, provider string) (*TokenData, error) {
	payload := map[string]string{
		"refreshToken": refreshToken,
	}
	var resp socialTokenResponse
	accountKey := BuildAccountKey("", "", refreshToken, "", 0)
	if err := doJSON(ctx, proxyURL, http.MethodPost, socialAuthEndpointURL+"/refreshToken", payload, &resp, BuildLoginHeaders(accountKey, BuildMachineID(refreshToken, "", accountKey))); err != nil {
		return nil, err
	}
	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return &TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ProfileArn:   resp.ProfileArn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
		AuthMethod:   "social",
		Provider:     strings.TrimSpace(provider),
		Region:       defaultIDCRegion,
	}, nil
}

func BuildExternalIdpAuthURL(authEndpoint, clientID, redirectURI, scopes, codeChallenge, state, loginHint string) string {
	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", scopes)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("response_mode", "query")
	params.Set("state", state)
	if strings.TrimSpace(loginHint) != "" {
		params.Set("login_hint", strings.TrimSpace(loginHint))
	}
	return strings.TrimSpace(authEndpoint) + "?" + params.Encode()
}

func DiscoverExternalIdp(ctx context.Context, proxyURL, issuerURL string) (*externalIdpDiscoveryResponse, error) {
	issuer := strings.TrimRight(strings.TrimSpace(issuerURL), "/")
	if issuer == "" {
		return nil, fmt.Errorf("kiro external_idp issuer_url is required")
	}
	if err := validateExternalIdpEndpoint(issuer); err != nil {
		return nil, err
	}
	var resp externalIdpDiscoveryResponse
	if err := doJSON(ctx, proxyURL, http.MethodGet, issuer+"/.well-known/openid-configuration", nil, &resp, nil); err != nil {
		return nil, err
	}
	resp.AuthorizationEndpoint = strings.TrimSpace(resp.AuthorizationEndpoint)
	resp.TokenEndpoint = strings.TrimSpace(resp.TokenEndpoint)
	if resp.AuthorizationEndpoint == "" || resp.TokenEndpoint == "" {
		return nil, fmt.Errorf("external IdP discovery document missing authorization_endpoint or token_endpoint")
	}
	if err := validateExternalIdpEndpoint(resp.AuthorizationEndpoint); err != nil {
		return nil, err
	}
	if err := validateExternalIdpEndpoint(resp.TokenEndpoint); err != nil {
		return nil, err
	}
	return &resp, nil
}

func validateExternalIdpEndpoint(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid external IdP URL %q: %w", rawURL, err)
	}
	if strings.ToLower(parsed.Scheme) != "https" {
		return fmt.Errorf("external IdP URL must use https: %q", rawURL)
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "" {
		return fmt.Errorf("external IdP URL has no host: %q", rawURL)
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("external IdP URL host must not be an IP literal: %q", rawURL)
	}
	for _, suffix := range allowedExternalIdpHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return nil
		}
	}
	return fmt.Errorf("external IdP host %q is not allow-listed", host)
}

func ExchangeExternalIdpAuthCode(ctx context.Context, proxyURL, tokenEndpoint, clientID, code, codeVerifier, redirectURI, scopes, issuerURL string) (*TokenData, error) {
	form := url.Values{}
	form.Set("client_id", strings.TrimSpace(clientID))
	form.Set("grant_type", "authorization_code")
	form.Set("code", strings.TrimSpace(code))
	form.Set("redirect_uri", strings.TrimSpace(redirectURI))
	form.Set("code_verifier", strings.TrimSpace(codeVerifier))
	if strings.TrimSpace(scopes) != "" {
		form.Set("scope", strings.TrimSpace(scopes))
	}
	var resp externalIdpTokenResponse
	if err := doForm(ctx, proxyURL, strings.TrimSpace(tokenEndpoint), form, &resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.AccessToken) == "" {
		return nil, fmt.Errorf("external IdP token exchange returned empty access_token")
	}
	return buildExternalIdpTokenData(resp, strings.TrimSpace(resp.RefreshToken), clientID, tokenEndpoint, issuerURL, scopes), nil
}

func RefreshExternalIdpToken(ctx context.Context, proxyURL, clientID, refreshToken, tokenEndpoint, issuerURL, scopes string) (*TokenData, error) {
	form := url.Values{}
	form.Set("client_id", strings.TrimSpace(clientID))
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", strings.TrimSpace(refreshToken))
	if strings.TrimSpace(scopes) != "" {
		form.Set("scope", strings.TrimSpace(scopes))
	}
	var resp externalIdpTokenResponse
	if err := doForm(ctx, proxyURL, strings.TrimSpace(tokenEndpoint), form, &resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.AccessToken) == "" {
		return nil, fmt.Errorf("external IdP refresh returned empty access_token")
	}
	return buildExternalIdpTokenData(resp, strings.TrimSpace(refreshToken), clientID, tokenEndpoint, issuerURL, scopes), nil
}

func buildExternalIdpTokenData(resp externalIdpTokenResponse, fallbackRefreshToken, clientID, tokenEndpoint, issuerURL, scopes string) *TokenData {
	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	refreshToken := strings.TrimSpace(resp.RefreshToken)
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(fallbackRefreshToken)
	}
	return &TokenData{
		AccessToken:   strings.TrimSpace(resp.AccessToken),
		RefreshToken:  refreshToken,
		ExpiresAt:     time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
		AuthMethod:    "external_idp",
		Provider:      ProviderExternalIdp,
		ClientID:      strings.TrimSpace(clientID),
		TokenEndpoint: strings.TrimSpace(tokenEndpoint),
		IssuerURL:     strings.TrimSpace(issuerURL),
		Scopes:        strings.TrimSpace(scopes),
		Region:        defaultIDCRegion,
	}
}

func RegisterIDCClient(ctx context.Context, proxyURL, redirectURI, issuerURL, region string) (*registerClientResponse, error) {
	if region == "" {
		region = defaultIDCRegion
	}
	payload := map[string]any{
		"clientName":   "Kiro IDE",
		"clientType":   "public",
		"scopes":       []string{"codewhisperer:completions", "codewhisperer:analysis", "codewhisperer:conversations", "codewhisperer:transformations", "codewhisperer:taskassist"},
		"grantTypes":   []string{"authorization_code", "refresh_token"},
		"redirectUris": []string{redirectURI},
		"issuerUrl":    issuerURL,
	}
	var resp registerClientResponse
	headers := oidcHeaders("", BuildMachineID("", "", "register-idc-client"))
	if err := doJSON(ctx, proxyURL, http.MethodPost, getOIDCEndpoint(region)+"/client/register", payload, &resp, headers); err != nil {
		return nil, err
	}
	return &resp, nil
}

func BuildIDCAuthURL(clientID, redirectURI, state, codeChallenge, region string) string {
	if region == "" {
		region = defaultIDCRegion
	}
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scopes", strings.Join([]string{
		"codewhisperer:completions",
		"codewhisperer:analysis",
		"codewhisperer:conversations",
		"codewhisperer:transformations",
		"codewhisperer:taskassist",
	}, " "))
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	return fmt.Sprintf("%s/authorize?%s", getOIDCEndpoint(region), params.Encode())
}

func ExchangeIDCAuthCode(ctx context.Context, proxyURL, clientID, clientSecret, code, codeVerifier, redirectURI, region, startURL string) (*TokenData, error) {
	if region == "" {
		region = defaultIDCRegion
	}
	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"code":         code,
		"codeVerifier": codeVerifier,
		"redirectUri":  redirectURI,
		"grantType":    "authorization_code",
	}
	var resp createTokenResponse
	accountKey := BuildAccountKey(clientID, "", "", "", 0)
	headers := oidcHeaders(accountKey, BuildMachineID("", "", "clientID:"+clientID))
	if err := doJSON(ctx, proxyURL, http.MethodPost, getOIDCEndpoint(region)+"/token", payload, &resp, headers); err != nil {
		return nil, err
	}
	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	token := &TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ProfileArn:   resp.ProfileArn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
		AuthMethod:   "idc",
		Provider:     resolveIDCProvider(startURL),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		StartURL:     startURL,
		Region:       region,
	}
	token.Email = FetchOIDCUserEmail(ctx, proxyURL, token.AccessToken, region)
	return token, nil
}

func RefreshIDCToken(ctx context.Context, proxyURL, clientID, clientSecret, refreshToken, region, startURL, provider string) (*TokenData, error) {
	if region == "" {
		region = defaultIDCRegion
	}
	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}
	var resp createTokenResponse
	accountKey := BuildAccountKey(clientID, "", refreshToken, "", 0)
	headers := oidcHeaders(accountKey, BuildMachineID(refreshToken, "", accountKey))
	if err := doJSON(ctx, proxyURL, http.MethodPost, getOIDCEndpoint(region)+"/token", payload, &resp, headers); err != nil {
		return nil, err
	}
	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	token := &TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ProfileArn:   resp.ProfileArn,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
		AuthMethod:   "idc",
		Provider:     strings.TrimSpace(provider),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		StartURL:     startURL,
		Region:       region,
	}
	if token.Provider == "" {
		token.Provider = resolveIDCProvider(startURL)
	}
	token.Email = FetchOIDCUserEmail(ctx, proxyURL, token.AccessToken, region)
	return token, nil
}

func FetchOIDCUserEmail(ctx context.Context, proxyURL, accessToken, region string) string {
	if strings.TrimSpace(accessToken) == "" {
		return ""
	}
	var resp userInfoResponse
	headers := map[string]string{
		"Authorization": "Bearer " + accessToken,
	}
	if err := doJSON(ctx, proxyURL, http.MethodGet, getOIDCEndpoint(region)+"/userinfo", nil, &resp, headers); err != nil {
		return ""
	}
	return strings.TrimSpace(resp.Email)
}

// ParseImportedTokens parses either one token object or an array of token
// objects into Kiro credentials. It keeps accepting the credential forms used
// by existing account workflows. The administrator-facing Kiro IDE import
// endpoint must use ParseKiroCredentialExport instead, which requires the
// explicit Kiro export shape and therefore cannot accidentally accept an
// arbitrary OAuth/API-key JSON document.
func ParseImportedTokens(tokenJSON string, deviceRegistrationJSON string) ([]*TokenData, error) {
	raw := strings.TrimSpace(tokenJSON)
	if raw == "" {
		return nil, fmt.Errorf("kiro token is empty")
	}
	if strings.HasPrefix(raw, "[") {
		var entries []json.RawMessage
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("failed to parse kiro token list: %w", err)
		}
		if len(entries) == 0 {
			return nil, fmt.Errorf("kiro token list is empty")
		}
		tokens := make([]*TokenData, 0, len(entries))
		for index, entry := range entries {
			token, err := parseImportedTokenObject(entry, deviceRegistrationJSON)
			if err != nil {
				return nil, fmt.Errorf("token %d: %w", index+1, err)
			}
			tokens = append(tokens, token)
		}
		return tokens, nil
	}
	token, err := parseImportedTokenObject([]byte(raw), deviceRegistrationJSON)
	if err != nil {
		return nil, err
	}
	return []*TokenData{token}, nil
}

// ParseKiroCredentialExport parses a Kiro IDE credential export. In addition
// to the normal credential checks, every entry must contain Kiro-specific
// runtime metadata. This is intentionally separate from ParseImportedTokens:
// only the dedicated import flow needs to reject generic credential JSON.
func ParseKiroCredentialExport(tokenJSON string, deviceRegistrationJSON string) ([]*TokenData, error) {
	raw := strings.TrimSpace(tokenJSON)
	if raw == "" {
		return nil, fmt.Errorf("kiro token is empty")
	}
	if strings.HasPrefix(raw, "[") {
		var entries []json.RawMessage
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("failed to parse kiro token list: %w", err)
		}
		if len(entries) == 0 {
			return nil, fmt.Errorf("kiro token list is empty")
		}
		tokens := make([]*TokenData, 0, len(entries))
		for index, entry := range entries {
			if err := validateKiroCredentialExportShape(entry); err != nil {
				return nil, fmt.Errorf("token %d: %w", index+1, err)
			}
			token, err := parseImportedTokenObject(entry, deviceRegistrationJSON)
			if err != nil {
				return nil, fmt.Errorf("token %d: %w", index+1, err)
			}
			tokens = append(tokens, token)
		}
		return tokens, nil
	}
	if err := validateKiroCredentialExportShape([]byte(raw)); err != nil {
		return nil, err
	}
	token, err := parseImportedTokenObject([]byte(raw), deviceRegistrationJSON)
	if err != nil {
		return nil, err
	}
	return []*TokenData{token}, nil
}

func ParseImportedToken(tokenJSON string, deviceRegistrationJSON string) (*TokenData, error) {
	tokens, err := ParseImportedTokens(tokenJSON, deviceRegistrationJSON)
	if err != nil {
		return nil, err
	}
	if len(tokens) != 1 {
		return nil, fmt.Errorf("expected one Kiro token, got %d", len(tokens))
	}
	return tokens[0], nil
}

func parseImportedTokenObject(data []byte, deviceRegistrationJSON string) (*TokenData, error) {
	var token TokenData
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("failed to parse kiro token: %w", err)
	}
	token.AuthMethod = normalizeImportedAuthMethod(token.AuthMethod)
	token.APIKey = strings.TrimSpace(token.APIKey)
	if token.AuthMethod == "" && token.APIKey != "" && strings.TrimSpace(token.AccessToken) == "" {
		token.AuthMethod = "api_key"
	}
	if token.AuthMethod == "api_key" {
		if token.APIKey == "" {
			return nil, fmt.Errorf("api key is empty")
		}
		token.Provider = strings.TrimSpace(token.Provider)
		token.Region = strings.TrimSpace(token.Region)
		token.AuthRegion = strings.TrimSpace(token.AuthRegion)
		token.APIRegion = strings.TrimSpace(token.APIRegion)
		if err := normalizeImportedAPIKeyRegion(&token); err != nil {
			return nil, err
		}
		token.MachineID = strings.TrimSpace(token.MachineID)
		token.SubscriptionTitle = strings.TrimSpace(token.SubscriptionTitle)
		if token.Provider != "" && !IsValidKiroProvider(token.Provider) {
			return nil, fmt.Errorf("unsupported kiro provider: %q", token.Provider)
		}
		return &token, nil
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("access token is empty")
	}
	if token.ClientIDHash != "" && (token.ClientID == "" || token.ClientSecret == "") && strings.TrimSpace(deviceRegistrationJSON) != "" {
		var reg deviceRegistration
		if err := json.Unmarshal([]byte(deviceRegistrationJSON), &reg); err != nil {
			return nil, fmt.Errorf("failed to parse device registration: %w", err)
		}
		if reg.ClientID != "" {
			token.ClientID = reg.ClientID
		}
		if reg.ClientSecret != "" {
			token.ClientSecret = reg.ClientSecret
		}
	}
	token.Provider = strings.TrimSpace(token.Provider)
	token.Region = strings.TrimSpace(token.Region)
	token.AuthRegion = strings.TrimSpace(token.AuthRegion)
	token.APIRegion = strings.TrimSpace(token.APIRegion)
	if token.Region == "" && token.AuthRegion != "" {
		token.Region = token.AuthRegion
	}
	token.MachineID = strings.TrimSpace(token.MachineID)
	token.StartURL = strings.TrimSpace(token.StartURL)

	if token.ClientIDHash != "" && (token.ClientID == "" || token.ClientSecret == "") {
		return nil, fmt.Errorf("device registration is required when clientIdHash is present without clientId and clientSecret")
	}
	if token.AuthMethod == "" {
		switch {
		case strings.TrimSpace(token.TokenEndpoint) != "" && strings.TrimSpace(token.ClientID) != "":
			token.AuthMethod = "external_idp"
		case strings.TrimSpace(token.ClientID) != "" && strings.TrimSpace(token.ClientSecret) != "":
			token.AuthMethod = "idc"
		default:
			// Kiro IDE social exports contain access/refresh tokens but no
			// provider. Treat that shape as social OAuth by default.
			token.AuthMethod = "social"
		}
	}
	switch token.AuthMethod {
	case "idc":
		if token.Provider == "" {
			token.Provider = resolveIDCProvider(token.StartURL)
		}
		if strings.TrimSpace(token.Region) == "" {
			token.Region = defaultIDCRegion
		}
	case "external_idp":
		token.Provider = ProviderExternalIdp
		token.ClientID = strings.TrimSpace(token.ClientID)
		token.TokenEndpoint = strings.TrimSpace(token.TokenEndpoint)
		token.IssuerURL = strings.TrimSpace(token.IssuerURL)
		token.Scopes = strings.TrimSpace(token.Scopes)
		normalizeImportedExternalIDPDefaults(&token)
		if strings.TrimSpace(token.RefreshToken) == "" || token.ClientID == "" || token.TokenEndpoint == "" {
			return nil, fmt.Errorf("kiro external_idp import requires refreshToken, clientId, and tokenEndpoint")
		}
		if strings.TrimSpace(token.Region) == "" {
			token.Region = defaultIDCRegion
		}
	case "social":
		// Kiro IDE social exports may omit provider because the exported token
		// itself does not always expose the browser sign-in choice.
	default:
		return nil, fmt.Errorf("unsupported kiro auth method: %q", token.AuthMethod)
	}
	if token.Provider != "" && !IsValidKiroProvider(token.Provider) {
		return nil, fmt.Errorf("unsupported kiro provider: %q", token.Provider)
	}
	// expiresAt 归一化为带本地时区偏移的 RFC3339,对齐 OAuth 登录流程。
	if strings.TrimSpace(token.ExpiresAt) != "" {
		normalized, err := normalizeKiroExpiresAt(token.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("failed to parse kiro token expiresAt: %w", err)
		}
		token.ExpiresAt = normalized
	}
	return &token, nil
}

// normalizeImportedAPIKeyRegion applies the same region semantics as kiro.rs:
// a pipe suffix populates all three region slots, while an explicit region is
// propagated to the API/auth slots only when those slots are absent.
func normalizeImportedAPIKeyRegion(token *TokenData) error {
	if token == nil {
		return nil
	}
	rawKey := strings.TrimSpace(token.APIKey)
	if key, region, found := strings.Cut(rawKey, "|"); found {
		key = strings.TrimSpace(key)
		region = strings.TrimSpace(region)
		if region != "" {
			if err := validateKiroRsRegion(region); err != nil {
				return fmt.Errorf("invalid api key region: %w", err)
			}
			if token.Region == "" {
				token.Region = region
			}
			if token.AuthRegion == "" {
				token.AuthRegion = region
			}
			if token.APIRegion == "" {
				token.APIRegion = region
			}
		}
		token.APIKey = key
	}
	if token.AuthRegion == "" {
		token.AuthRegion = token.Region
	}
	if token.APIRegion == "" {
		token.APIRegion = token.Region
	}
	return nil
}

// normalizeImportedExternalIDPDefaults accepts the incomplete Microsoft
// Entra exports commonly produced by Kiro/KAM. The derived values are limited
// to allow-listed Microsoft issuer hosts and are only used when the export
// omitted token_endpoint/scopes.
func normalizeImportedExternalIDPDefaults(token *TokenData) {
	if token == nil {
		return
	}
	if token.TokenEndpoint == "" {
		candidates := []string{token.IssuerURL, importedJWTIssuer(token.AccessToken)}
		for _, candidate := range candidates {
			if endpoint := microsoftTokenEndpointFromIssuer(candidate); endpoint != "" {
				token.TokenEndpoint = endpoint
				break
			}
		}
	}
	if token.TokenEndpoint == "" && looksLikeMicrosoftRefreshToken(token.RefreshToken) {
		token.TokenEndpoint = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	}
	if token.Scopes == "" && strings.TrimSpace(token.ClientID) != "" {
		clientID := strings.TrimSpace(token.ClientID)
		token.Scopes = fmt.Sprintf(
			"api://%s/codewhisperer:conversations api://%s/codewhisperer:completions offline_access",
			clientID,
			clientID,
		)
	}
}

func microsoftTokenEndpointFromIssuer(raw string) string {
	issuer := strings.TrimSpace(raw)
	if issuer == "" {
		return ""
	}
	parsed, err := url.Parse(issuer)
	if err != nil || strings.ToLower(parsed.Scheme) != "https" {
		return ""
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "" {
		return ""
	}
	allowed := false
	for _, suffix := range allowedExternalIdpHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			allowed = true
			break
		}
	}
	if !allowed {
		return ""
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		if strings.EqualFold(segment, "oauth2") {
			return ""
		}
		return fmt.Sprintf("https://%s/%s/oauth2/v2.0/token", host, segment)
	}
	return ""
}

func importedJWTIssuer(raw string) string {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return ""
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Issuer)
}

func looksLikeMicrosoftRefreshToken(raw string) bool {
	value := strings.TrimSpace(raw)
	return strings.HasPrefix(value, "1.") || strings.HasPrefix(value, "0.")
}

// validateKiroCredentialExportShape makes the Kiro importer intentionally
// format-specific. The endpoint is not a generic JSON-to-account converter:
// a payload must contain an explicit Kiro credential shape before any OAuth
// or API-key value is parsed or returned to the caller.
//
// Both Kiro IDE's camelCase export and this project's normalized snake_case
// credential export are accepted. Requiring a Kiro runtime companion field
// prevents an unrelated OAuth document with access_token/refresh_token from
// being imported as a Kiro account by accident.
func validateKiroCredentialExportShape(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("failed to parse kiro token: %w", err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("not a recognized Kiro credential export: credential object is empty")
	}

	readString := func(keys ...string) string {
		for _, key := range keys {
			value, ok := raw[key]
			if !ok {
				continue
			}
			var text string
			if err := json.Unmarshal(value, &text); err == nil && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		return ""
	}
	authMethod := normalizeImportedAuthMethod(readString("authMethod", "auth_method"))
	apiKey := readString("kiroApiKey", "kiro_api_key", "apiKey", "api_key")
	accessToken := readString("accessToken", "access_token")
	refreshToken := readString("refreshToken", "refresh_token")
	// These fields belong to Kiro's runtime/auth export rather than a generic
	// OAuth document. At least one is required for every imported account.
	hasKiroCompanion := readString(
		"profileArn", "profile_arn",
		"apiRegion", "api_region",
		"authRegion", "auth_region",
		"machineId", "machine_id",
		"subscriptionTitle", "subscription_title",
		"startUrl", "start_url",
		"clientIdHash", "client_id_hash",
		"tokenEndpoint", "token_endpoint",
		"issuerUrl", "issuer_url",
		"scopes", "scope",
	) != ""

	if authMethod == "" && apiKey != "" && accessToken == "" {
		authMethod = "api_key"
	}
	switch authMethod {
	case "api_key":
		if apiKey == "" || !hasKiroCompanion {
			return fmt.Errorf("not a recognized Kiro credential export: api_key entries require kiroApiKey/api_key and Kiro runtime metadata")
		}
	case "social", "idc", "external_idp":
		if accessToken == "" || refreshToken == "" || !hasKiroCompanion {
			return fmt.Errorf("not a recognized Kiro credential export: OAuth entries require accessToken, refreshToken, authMethod, and Kiro runtime metadata")
		}
	default:
		return fmt.Errorf("not a recognized Kiro credential export: unsupported or missing authMethod")
	}
	return nil
}

func normalizeImportedAuthMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "", "oauth", "social", "social_oauth", "oauth_social":
		if strings.TrimSpace(method) == "" {
			return ""
		}
		return "social"
	case "idc", "identity_center", "identity-center", "builderid", "builder_id":
		return "idc"
	case "external_idp", "external-idp", "externalidp":
		return "external_idp"
	case "api_key", "api-key", "apikey":
		return "api_key"
	default:
		return strings.ToLower(strings.TrimSpace(method))
	}
}

func getOIDCEndpoint(region string) string {
	if strings.TrimSpace(oidcEndpointOverride) != "" {
		return strings.TrimRight(strings.TrimSpace(oidcEndpointOverride), "/")
	}
	if region == "" {
		region = defaultIDCRegion
	}
	return fmt.Sprintf("https://oidc.%s.amazonaws.com", region)
}

func oidcHeaders(accountKey, machineID string) map[string]string {
	headers := BuildOIDCHeaders(accountKey, machineID)
	if headers["amz-sdk-invocation-id"] == "" {
		headers["amz-sdk-invocation-id"] = uuid.NewString()
	}
	if headers["amz-sdk-request"] == "" {
		headers["amz-sdk-request"] = "attempt=1; max=4"
	}
	return headers
}

func doJSON(ctx context.Context, proxyURL, method, rawURL string, payload any, out any, extraHeaders map[string]string) error {
	client, err := newHTTPClient(proxyURL)
	if err != nil {
		return err
	}

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyText := strings.TrimSpace(string(respBody))
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(bodyText), "invalid_grant") {
			return &RefreshTokenInvalidError{StatusCode: resp.StatusCode, Body: bodyText}
		}
		return fmt.Errorf("upstream request failed (status %d): %s", resp.StatusCode, bodyText)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func doForm(ctx context.Context, proxyURL, rawURL string, form url.Values, out any) error {
	client, err := newHTTPClient(proxyURL)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyText := strings.TrimSpace(string(respBody))
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(bodyText), "invalid_grant") {
			return &RefreshTokenInvalidError{StatusCode: resp.StatusCode, Body: bodyText}
		}
		return fmt.Errorf("upstream request failed (status %d): %s", resp.StatusCode, bodyText)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func newHTTPClient(rawProxyURL string) (*http.Client, error) {
	_, parsed, err := proxyurl.Parse(rawProxyURL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{}
	if parsed != nil {
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}, nil
}

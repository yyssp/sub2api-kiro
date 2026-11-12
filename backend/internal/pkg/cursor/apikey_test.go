package cursor

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testAPIKey = "crsr_0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

func TestIsAPIKey(t *testing.T) {
	for _, ok := range []string{testAPIKey, "  " + testAPIKey + "\n"} {
		if !IsAPIKey(ok) {
			t.Errorf("IsAPIKey(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "crsr_", "crsr_short", "crsr_abc def0123456789abcdef", "eyJhbGciOi.eyJzdWIi.sig", "sk-0123456789abcdef0123456789"} {
		if IsAPIKey(bad) {
			t.Errorf("IsAPIKey(%q) = true", bad)
		}
	}
}

// 导入预览必须认得 crsr_ key：纯文本、JSON 字段两种形态都要能解析，且不联网。
func TestParseImportCredentials_AcceptsAPIKey(t *testing.T) {
	cases := map[string]string{
		"plain text":       testAPIKey,
		"plain with notes": "# my key\n" + testAPIKey,
		"json api_key":     `{"api_key":"` + testAPIKey + `"}`,
		"json apiKey list": `[{"apiKey":"` + testAPIKey + `","note":"team"}]`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := ParseImportCredentials(input)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(res.Credentials) != 1 {
				t.Fatalf("credentials=%d, want 1", len(res.Credentials))
			}
			cred := res.Credentials[0]
			if cred.APIKey != testAPIKey || cred.TokenType != TokenTypeAPIKeyEntry || cred.AccessToken != "" {
				t.Fatalf("cred=%+v", cred)
			}
		})
	}
	// 同一 key 重复只保留一条（导出文件里同号重复很常见）。
	res, err := ParseImportCredentials(testAPIKey + "\n" + testAPIKey)
	if err != nil || len(res.Credentials) != 1 || len(res.Skipped) != 1 {
		t.Fatalf("dedupe: creds=%d skipped=%d err=%v", len(res.Credentials), len(res.Skipped), err)
	}
	// JWT 与 key 混合粘贴仍走纯文本路径。
	res, err = ParseImportCredentials(sessionJWT("a@x.test") + "\n" + testAPIKey)
	if err != nil || len(res.Credentials) != 2 {
		t.Fatalf("mixed: creds=%d err=%v", len(res.Credentials), err)
	}
}

func TestExchangeAPIKey_SendsBearerKeyAndParsesAccessToken(t *testing.T) {
	access := makeTestJWT("auth0|user_K", "", TokenTypeAPIKey, time.Now().Add(time.Hour).Unix())
	var gotAuth, gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"` + access + `","refreshToken":"` + access + `"}`))
	}))
	t.Cleanup(server.Close)
	ConfigureUpstream(server.URL, "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	got, err := exchangeAPIKey(testAPIKey, "")
	if err != nil || got != access {
		t.Fatalf("exchange: got=%q err=%v", got, err)
	}
	if gotPath != "/auth/exchange_user_api_key" || gotAuth != "Bearer "+testAPIKey || gotBody != "{}" {
		t.Fatalf("request path=%q auth=%q body=%q", gotPath, gotAuth, gotBody)
	}
}

func TestExchangeAPIKey_ClassifiesRejectionSeparatelyFromTransientErrors(t *testing.T) {
	status := http.StatusUnauthorized
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key"}}`))
	}))
	t.Cleanup(server.Close)
	ConfigureUpstream(server.URL, "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	if _, err := exchangeAPIKey(testAPIKey, ""); !errors.Is(err, ErrAPIKeyRejected) {
		t.Fatalf("401 must be ErrAPIKeyRejected, got %v", err)
	}
	status = http.StatusBadGateway
	if _, err := exchangeAPIKey(testAPIKey, ""); err == nil || errors.Is(err, ErrAPIKeyRejected) {
		t.Fatalf("5xx must be a transient error, not a rejection: %v", err)
	}
}

// api_key_token 不被 Sand InferenceService 接受（实测 ERROR_NOT_LOGGED_IN），
// 即使模型属于 Sand 面也必须改走 AgentService。
func TestRunAgentStream_APIKeyTokenNeverUsesSandInference(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(server.Close)
	ConfigureUpstream(server.URL, "")
	ConfigureSandModels([]string{"cursor-grok-4.5-medium"})
	t.Cleanup(func() {
		ConfigureUpstream("", "")
		ConfigureSandModels(nil)
	})
	client := NewClient()
	client.shared = server.Client()
	client.agentTimeout = 2 * time.Second
	client.firstToken = 2 * time.Second

	tok := makeTestJWT("auth0|user_K", "", TokenTypeAPIKey, time.Now().Add(time.Hour).Unix())
	_, _ = client.RunAgentStream(t.Context(), &Account{AccessToken: tok},
		AgentRequest{Model: "cursor-grok-4.5-medium", Message: "hi"}, nil, nil, nil)
	for _, p := range paths {
		if strings.Contains(p, "InferenceService") {
			t.Fatalf("api_key_token was sent to Sand InferenceService: %v", paths)
		}
	}
	if len(paths) == 0 || !strings.Contains(paths[0], "AgentService") {
		t.Fatalf("expected AgentService request, got %v", paths)
	}
}

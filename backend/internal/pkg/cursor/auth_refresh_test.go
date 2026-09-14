package cursor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRefreshCursorAuthUsesOAuthTokenRefresh(t *testing.T) {
	nextAccess := mkJWT(map[string]any{
		"sub":  "oauth-refresh-user",
		"type": "session",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path=%q want /oauth/token", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method=%q want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header should be empty, got %q", got)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("Content-Type=%q want JSON", ct)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["grant_type"] != "refresh_token" {
			t.Fatalf("grant_type=%q want refresh_token", body["grant_type"])
		}
		if body["client_id"] != cursorOAuthClientID {
			t.Fatalf("client_id=%q want Cursor desktop client id", body["client_id"])
		}
		if body["refresh_token"] != "refresh-token-1" {
			t.Fatalf("refresh_token=%q want refresh-token-1", body["refresh_token"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  nextAccess,
			"refresh_token": "refresh-token-2",
		})
	}))
	defer server.Close()

	ConfigureUpstream(server.URL, "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	access, refresh, err := refreshCursorAuth("refresh-token-1")
	if err != nil {
		t.Fatalf("refreshCursorAuth: %v", err)
	}
	if access != nextAccess {
		t.Fatalf("access token mismatch")
	}
	if refresh != "refresh-token-2" {
		t.Fatalf("refresh=%q want refresh-token-2", refresh)
	}
}

func TestRefreshCursorAuthTreatsShouldLogoutAsReauthRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path=%q want /oauth/token", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"shouldLogout": true})
	}))
	defer server.Close()

	ConfigureUpstream(server.URL, "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	access, refresh, err := refreshCursorAuth("refresh-token-1")
	if err == nil {
		t.Fatal("expected re-authentication error")
	}
	if access != "" || refresh != "" {
		t.Fatalf("tokens should be empty on shouldLogout response: access=%q refresh=%q", access, refresh)
	}
	if !strings.Contains(err.Error(), "re-authentication required") {
		t.Fatalf("error=%q want re-authentication marker", err.Error())
	}
}

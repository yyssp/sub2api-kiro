package cursor

import "testing"

func TestCursorUpstreamURLDefaultsToOfficial(t *testing.T) {
	ConfigureUpstream("", "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	if got := cursorAPIURL("/aiserver.v1.AiService/AvailableModels"); got != "https://api2.cursor.sh/aiserver.v1.AiService/AvailableModels" {
		t.Fatalf("api url=%q", got)
	}
	if got := cursorWebURL("/api/auth/me"); got != "https://cursor.com/api/auth/me" {
		t.Fatalf("web url=%q", got)
	}
}

func TestCursorUpstreamURLUsesProxyWithoutChangingPath(t *testing.T) {
	ConfigureUpstream("http://127.0.0.1:63788/", "secret")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	if got := cursorAPIURL("/auth/poll?uuid=u&verifier=v"); got != "http://127.0.0.1:63788/auth/poll?uuid=u&verifier=v" {
		t.Fatalf("api proxy url=%q", got)
	}
	if got := cursorAgentURL("/agent.v1.AgentService/RunSSE"); got != "http://127.0.0.1:63788/agent.v1.AgentService/RunSSE" {
		t.Fatalf("agent proxy url=%q", got)
	}
	if got := cursorWebURL("/loginDeepControl?uuid=u"); got != "http://127.0.0.1:63788/loginDeepControl?uuid=u" {
		t.Fatalf("web proxy url=%q", got)
	}
}

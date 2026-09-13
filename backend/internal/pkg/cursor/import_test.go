package cursor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// makeTestJWT 造一个签名无效但 claims 可解析的 JWT。
// 解析路径只读中段 claims、不校验签名，所以签名段填占位即可。
func makeTestJWT(sub, email, typ string, exp int64) string {
	claims := map[string]any{"sub": sub, "email": email, "type": typ}
	if exp > 0 {
		claims["exp"] = exp
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + ".sig"
}

func sessionJWT(email string) string {
	return makeTestJWT("auth0|user-"+email, email, "session", time.Now().Add(60*24*time.Hour).Unix())
}

func webJWT(email string) string {
	return makeTestJWT("auth0|user-"+email, email, "web", time.Now().Add(3*time.Hour).Unix())
}

func TestParseImportCredentials_PlainTextBareJWT(t *testing.T) {
	a, b := sessionJWT("a@example.com"), sessionJWT("b@example.com")
	res, err := ParseImportCredentials(a + "\n" + b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 2 {
		t.Fatalf("want 2 credentials, got %d", len(res.Credentials))
	}
	if res.Credentials[0].AccessToken != a {
		t.Errorf("access token mismatch: %q", res.Credentials[0].AccessToken)
	}
	if res.Credentials[0].Email != "a@example.com" {
		t.Errorf("email should come from JWT claims, got %q", res.Credentials[0].Email)
	}
	if res.Credentials[0].TokenType != "session" {
		t.Errorf("token type: got %q want session", res.Credentials[0].TokenType)
	}
	// session 必须是 uid::JWT 形式，面板接口直接拿它当 cookie 用。
	if !strings.Contains(res.Credentials[0].Session, "::") {
		t.Errorf("session should be uid::JWT, got %q", res.Credentials[0].Session)
	}
}

func TestParseImportCredentials_PlainTextVariants(t *testing.T) {
	token := sessionJWT("v@example.com")
	cases := []struct {
		name string
		in   string
	}{
		{"bare JWT", token},
		{"uid::JWT", "user_abc::" + token},
		{"cookie with prefix", "WorkosCursorSessionToken=user_abc::" + token},
		{"cookie with attributes", "WorkosCursorSessionToken=user_abc::" + token + "; Path=/; HttpOnly"},
		{"url encoded", "WorkosCursorSessionToken=user_abc%3A%3A" + token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseImportCredentials(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(res.Credentials) != 1 {
				t.Fatalf("want 1 credential, got %d", len(res.Credentials))
			}
			// 无论输入包了多少层，抽出来的 access_token 都必须是纯 JWT。
			if res.Credentials[0].AccessToken != token {
				t.Errorf("access token not normalized to bare JWT: %q", res.Credentials[0].AccessToken)
			}
		})
	}
}

func TestParseImportCredentials_PlainTextWithNoteAndComments(t *testing.T) {
	token := sessionJWT("n@example.com")
	res, err := ParseImportCredentials("# 我的账号清单\n" + token + " | 主力号\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("want 1 credential, got %d", len(res.Credentials))
	}
	if res.Credentials[0].Note != "主力号" {
		t.Errorf("note: got %q want 主力号", res.Credentials[0].Note)
	}
}

// 纯文本必须全有或全无：一行是垃圾就不能把其余行建成账号。
func TestParseImportCredentials_PlainTextAllOrNothing(t *testing.T) {
	_, err := ParseImportCredentials(sessionJWT("ok@example.com") + "\nnot-a-jwt\n")
	if err == nil {
		t.Fatal("expected error when a line is not a JWT, got nil")
	}
}

func TestParseImportCredentials_JSONArrayCamelCase(t *testing.T) {
	a, b := sessionJWT("a@example.com"), webJWT("b@example.com")
	raw := fmt.Sprintf(`[
	  {"accessToken": %q, "refreshToken": "rt-a", "machineId": "mid-a", "email": "override@example.com"},
	  {"accessToken": %q, "disabled": true, "note": "备用"}
	]`, a, b)
	res, err := ParseImportCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 2 {
		t.Fatalf("want 2 credentials, got %d", len(res.Credentials))
	}

	first := res.Credentials[0]
	if first.RefreshToken != "rt-a" {
		t.Errorf("refresh token: got %q", first.RefreshToken)
	}
	// machine_id 必须沿用导出值：它是设备指纹种子，重新生成等于换设备。
	if first.MachineID != "mid-a" {
		t.Errorf("machine id should be inherited, got %q", first.MachineID)
	}
	// 显式 email 字段优先于 JWT 推导值。
	if first.Email != "override@example.com" {
		t.Errorf("explicit email should win, got %q", first.Email)
	}

	second := res.Credentials[1]
	if !second.Disabled {
		t.Error("disabled flag should be carried over")
	}
	if second.TokenType != "web" {
		t.Errorf("token type: got %q want web", second.TokenType)
	}
	if second.Note != "备用" {
		t.Errorf("note: got %q", second.Note)
	}
}

func TestParseImportCredentials_SnakeCaseAndAliases(t *testing.T) {
	token := sessionJWT("s@example.com")
	raw := fmt.Sprintf(`{"access_token": %q, "refresh_token": "rt", "machine_id": "mid"}`, token)
	res, err := ParseImportCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("want 1 credential, got %d", len(res.Credentials))
	}
	if res.Credentials[0].RefreshToken != "rt" || res.Credentials[0].MachineID != "mid" {
		t.Errorf("snake_case fields not read: %+v", res.Credentials[0])
	}
}

// 用 token/session/cookie 等别名当 token 字段也要能认出来。
func TestParseImportCredentials_TokenFieldAliases(t *testing.T) {
	token := sessionJWT("alias@example.com")
	for _, key := range []string{"token", "session", "sessionToken", "WorkosCursorSessionToken", "cookie", "jwt"} {
		t.Run(key, func(t *testing.T) {
			raw := fmt.Sprintf(`{%q: %q}`, key, token)
			res, err := ParseImportCredentials(raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(res.Credentials) != 1 || res.Credentials[0].AccessToken != token {
				t.Errorf("alias %q not recognized: %+v", key, res.Credentials)
			}
		})
	}
}

func TestParseImportCredentials_NestedContainers(t *testing.T) {
	token := sessionJWT("nested@example.com")
	cases := map[string]string{
		"accounts":    fmt.Sprintf(`{"accounts": [{"accessToken": %q}]}`, token),
		"data.items":  fmt.Sprintf(`{"data": {"items": [{"accessToken": %q}]}}`, token),
		"credentials": fmt.Sprintf(`{"credentials": [%q]}`, token),
		"tokens":      fmt.Sprintf(`{"tokens": [%q]}`, token),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := ParseImportCredentials(raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(res.Credentials) != 1 || res.Credentials[0].AccessToken != token {
				t.Errorf("container %q not unwrapped: %+v", name, res.Credentials)
			}
		})
	}
}

func TestParseImportCredentials_JSONL(t *testing.T) {
	a, b := sessionJWT("a@example.com"), sessionJWT("b@example.com")
	raw := fmt.Sprintf("{\"accessToken\": %q}\n{\"accessToken\": %q}\n", a, b)
	res, err := ParseImportCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 2 {
		t.Fatalf("want 2 credentials, got %d", len(res.Credentials))
	}
}

func TestParseImportCredentials_CommentsAndTrailingCommas(t *testing.T) {
	token := sessionJWT("c@example.com")
	raw := fmt.Sprintf(`[
	  // 第一条
	  {"accessToken": %q,},
	]`, token)
	res, err := ParseImportCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("want 1 credential, got %d", len(res.Credentials))
	}
}

func TestParseImportCredentials_CodeFence(t *testing.T) {
	token := sessionJWT("f@example.com")
	raw := "```json\n" + fmt.Sprintf(`[{"accessToken": %q}]`, token) + "\n```"
	res, err := ParseImportCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("want 1 credential, got %d", len(res.Credentials))
	}
}

// 坏条目只记 Skipped，不能中断整批导入。
func TestParseImportCredentials_PartialFailure(t *testing.T) {
	good := sessionJWT("good@example.com")
	raw := fmt.Sprintf(`[{"accessToken": %q}, {"email": "no-token@example.com"}, {"accessToken": "garbage"}]`, good)
	res, err := ParseImportCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("want 1 credential, got %d", len(res.Credentials))
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("want 2 skipped, got %d: %+v", len(res.Skipped), res.Skipped)
	}
	for _, s := range res.Skipped {
		if s.Reason == "" {
			t.Error("skipped entry must carry a reason")
		}
		if s.Index <= 0 {
			t.Errorf("skipped entry needs a 1-based index, got %d", s.Index)
		}
	}
}

// 同一 token 重复只建一个号，否则调度权重被悄悄放大。
func TestParseImportCredentials_Dedupe(t *testing.T) {
	token := sessionJWT("dup@example.com")
	raw := fmt.Sprintf(`[{"accessToken": %q}, {"accessToken": %q}]`, token, token)
	res, err := ParseImportCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("duplicate tokens should collapse to 1, got %d", len(res.Credentials))
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("duplicate should be reported as skipped, got %+v", res.Skipped)
	}
}

func TestParseImportCredentials_DedupeAcrossPlainText(t *testing.T) {
	token := sessionJWT("dup2@example.com")
	res, err := ParseImportCredentials(token + "\nuser_x::" + token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("same token in different wrappings should dedupe, got %d", len(res.Credentials))
	}
}

func TestParseImportCredentials_Errors(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"whitespace only":    "   \n\t  ",
		"not json nor jwt":   "{ this is not json",
		"json without token": `[{"email": "a@example.com"}]`,
		"empty array":        `[]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseImportCredentials(raw); err == nil {
				t.Errorf("expected error for %q", name)
			}
		})
	}
}

// Skipped.Sample 不能回显完整 token。
func TestParseImportCredentials_SampleDoesNotLeakFullToken(t *testing.T) {
	long := strings.Repeat("x", 300)
	raw := fmt.Sprintf(`[{"accessToken": %q}]`, long)
	_, err := ParseImportCredentials(raw)
	if err == nil {
		t.Fatal("expected error when no credential parses")
	}
	if strings.Contains(err.Error(), long) {
		t.Error("error message must not echo the full token")
	}
}

func TestTruncateImportSample(t *testing.T) {
	if got := truncateImportSample("short"); got != "short" {
		t.Errorf("short value should pass through, got %q", got)
	}
	long := strings.Repeat("a", 100)
	got := truncateImportSample(long)
	if len(got) != 43 || !strings.HasSuffix(got, "...") {
		t.Errorf("long value should be truncated to 40+ellipsis, got len=%d %q", len(got), got)
	}
}

func TestStripImportJSONComments_PreservesStrings(t *testing.T) {
	in := `{"url": "https://a.com//path", "x": 1} // trailing`
	out := stripImportJSONComments(in)
	if !strings.Contains(out, "https://a.com//path") {
		t.Errorf("// inside a string must be preserved, got %q", out)
	}
	if strings.Contains(out, "trailing") {
		t.Errorf("comment should be stripped, got %q", out)
	}
}

func TestStripImportTrailingCommas_PreservesStrings(t *testing.T) {
	in := `{"a": "x,", "b": [1,2,],}`
	out := stripImportTrailingCommas(in)
	if !strings.Contains(out, `"x,"`) {
		t.Errorf("comma inside a string must be preserved, got %q", out)
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Errorf("result should be valid JSON, got %q: %v", out, err)
	}
}

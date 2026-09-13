package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newCursorTestJWT 造一个 claims 可解析的 JWT（签名不校验）。
func newCursorTestJWT(email, typ string, exp time.Time) string {
	claims := map[string]any{
		"sub":   "auth0|user-" + email,
		"email": email,
		"type":  typ,
	}
	if !exp.IsZero() {
		claims["exp"] = exp.Unix()
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + ".sig"
}

// ImportCursorCredentials 是纯解析，不访问 accountRepo，nil 依赖即可测。
func newCursorImportService() *CursorOAuthService {
	return NewCursorOAuthService(nil)
}

func TestImportCursorCredentials_PlainText(t *testing.T) {
	svc := newCursorImportService()
	exp := time.Now().Add(60 * 24 * time.Hour).Truncate(time.Second)
	a := newCursorTestJWT("a@example.com", "session", exp)
	b := newCursorTestJWT("b@example.com", "session", exp)

	result, err := svc.ImportCursorCredentials(&CursorImportInput{Content: a + "\n" + b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(result.Entries))
	}

	first := result.Entries[0]
	if first.AccessToken != a {
		t.Errorf("access token mismatch")
	}
	if first.Email != "a@example.com" {
		t.Errorf("email: got %q", first.Email)
	}
	if first.TokenType != "session" {
		t.Errorf("token type: got %q want session", first.TokenType)
	}
	if !strings.Contains(first.Session, "::") {
		t.Errorf("session should be uid::JWT, got %q", first.Session)
	}
	// expires_at 必须从 JWT exp 推出来，账号列表靠它显示到期时间。
	if first.ExpiresAt == "" {
		t.Error("expires_at should be derived from JWT exp")
	} else if parsed, perr := time.Parse(time.RFC3339, first.ExpiresAt); perr != nil {
		t.Errorf("expires_at must be RFC3339, got %q: %v", first.ExpiresAt, perr)
	} else if !parsed.Equal(exp) {
		t.Errorf("expires_at: got %v want %v", parsed, exp)
	}
}

// web token 在预览阶段必须原样保留并标记类型，绝不在此处联网兑换。
func TestImportCursorCredentials_WebTokenNotExchangedAtPreview(t *testing.T) {
	svc := newCursorImportService()
	web := newCursorTestJWT("w@example.com", "web", time.Now().Add(3*time.Hour))

	result, err := svc.ImportCursorCredentials(&CursorImportInput{Content: web})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(result.Entries))
	}
	entry := result.Entries[0]
	if entry.TokenType != "web" {
		t.Errorf("token type: got %q want web", entry.TokenType)
	}
	// 兑换会产生 refresh_token；预览阶段不该有。
	if entry.RefreshToken != "" {
		t.Errorf("preview must not exchange the web token, got refresh_token %q", entry.RefreshToken)
	}
	if entry.AccessToken != web {
		t.Error("preview must keep the original web token")
	}
}

func TestImportCursorCredentials_JSONExport(t *testing.T) {
	svc := newCursorImportService()
	token := newCursorTestJWT("j@example.com", "session", time.Now().Add(time.Hour))
	raw := fmt.Sprintf(`{"accounts":[{"accessToken":%q,"refreshToken":"rt","machineId":"mid","disabled":true,"note":"n"}]}`, token)

	result, err := svc.ImportCursorCredentials(&CursorImportInput{Content: raw})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(result.Entries))
	}
	e := result.Entries[0]
	if e.RefreshToken != "rt" || e.MachineID != "mid" || !e.Disabled || e.Note != "n" {
		t.Errorf("export fields not carried over: %+v", e)
	}
}

func TestImportCursorCredentials_PartialFailureReported(t *testing.T) {
	svc := newCursorImportService()
	good := newCursorTestJWT("g@example.com", "session", time.Now().Add(time.Hour))
	raw := fmt.Sprintf(`[{"accessToken":%q},{"email":"x@example.com"}]`, good)

	result, err := svc.ImportCursorCredentials(&CursorImportInput{Content: raw})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(result.Entries))
	}
	// 坏条目必须出现在 Skipped 里，静默丢弃会让用户以为全部导入成功。
	if len(result.Skipped) != 1 {
		t.Fatalf("want 1 skipped, got %d", len(result.Skipped))
	}
	if result.Skipped[0].Reason == "" {
		t.Error("skipped entry must carry a reason")
	}
}

func TestImportCursorCredentials_Errors(t *testing.T) {
	svc := newCursorImportService()

	if _, err := svc.ImportCursorCredentials(nil); err == nil {
		t.Error("nil input should error")
	}
	if _, err := svc.ImportCursorCredentials(&CursorImportInput{Content: ""}); err == nil {
		t.Error("empty content should error")
	}
	if _, err := svc.ImportCursorCredentials(&CursorImportInput{Content: "not a token"}); err == nil {
		t.Error("garbage content should error")
	}
}

// 导入条目要能直接喂给 BuildAccountCredentials 落库。
func TestImportedEntryBuildsAccountCredentials(t *testing.T) {
	svc := newCursorImportService()
	token := newCursorTestJWT("b@example.com", "session", time.Now().Add(time.Hour))
	raw := fmt.Sprintf(`[{"accessToken":%q,"refreshToken":"rt","machineId":"mid"}]`, token)

	result, err := svc.ImportCursorCredentials(&CursorImportInput{Content: raw})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entry := result.Entries[0]

	creds := svc.BuildAccountCredentials(CursorTokenInfo{
		AccessToken:  entry.AccessToken,
		RefreshToken: entry.RefreshToken,
		Session:      entry.Session,
		Email:        entry.Email,
		MachineID:    entry.MachineID,
	})
	if creds[CursorCredAccessToken] != token {
		t.Errorf("access_token not written: %v", creds[CursorCredAccessToken])
	}
	if creds[CursorCredRefreshToken] != "rt" {
		t.Errorf("refresh_token not written: %v", creds[CursorCredRefreshToken])
	}
	// machine_id 必须落库：它是设备指纹种子，丢了就等于每次换设备。
	if creds[CursorCredMachineID] != "mid" {
		t.Errorf("machine_id not written: %v", creds[CursorCredMachineID])
	}
}

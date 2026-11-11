package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func cursorHandlerTestJWT(t *testing.T, typ, sub string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	claims := map[string]any{"type": typ, "sub": sub, "exp": time.Now().Add(24 * time.Hour).Unix()}
	return enc(map[string]any{"alg": "none"}) + "." + enc(claims) + ".sig"
}

// cursorReplaceAdminService 记录 UpdateAccount 收到的凭证，GetAccount 返回预置的 Cursor 账号。
type cursorReplaceAdminService struct {
	*stubAdminService
	existing *service.Account
	updated  map[string]any
}

func (s *cursorReplaceAdminService) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if s.existing != nil && s.existing.ID == id {
		a := *s.existing
		return &a, nil
	}
	return nil, service.ErrAccountNotFound
}

func (s *cursorReplaceAdminService) UpdateAccount(_ context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.updated = input.Credentials
	a := *s.existing
	a.Credentials = input.Credentials
	return &a, nil
}

func newCursorReplaceRouter(t *testing.T, svc *cursorReplaceAdminService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// 补齐动作是异步的上游调用；测试里让 membership 拉取直接失败，避免联网。
	t.Cleanup(cursor.SetFetchMembershipFnForTest(func(string, string) (string, bool) { return "", false }))
	h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetCursorOAuthService(service.NewCursorOAuthService(nil))
	r := gin.New()
	r.POST("/api/v1/admin/cursor/accounts/:id/credential", h.ReplaceCursorCredential)
	return r
}

func postCursorReplace(t *testing.T, r *gin.Engine, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cursor/accounts/"+id+"/credential", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

func TestReplaceCursorCredential_NormalizesCookieAndKeepsMachineID(t *testing.T) {
	oldTok := cursorHandlerTestJWT(t, "session", "auth0|user_A")
	newTok := cursorHandlerTestJWT(t, "session", "auth0|user_A")
	svc := &cursorReplaceAdminService{
		stubAdminService: newStubAdminService(),
		existing: &service.Account{ID: 8, Name: "a", Platform: service.PlatformCursor, Type: service.AccountTypeOAuth,
			Credentials: map[string]any{
				service.CursorCredAccessToken: oldTok, service.CursorCredMachineID: "machine-keep",
				service.CursorCredRefreshToken: "old-refresh",
			}},
	}
	r := newCursorReplaceRouter(t, svc)

	rec := postCursorReplace(t, r, "8", map[string]any{
		"token": "WorkosCursorSessionToken=user_A%3A%3A" + newTok + "; Path=/",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, newTok, svc.updated[service.CursorCredAccessToken], "整段 cookie 必须归一化成裸 JWT 再落库")
	require.Equal(t, "user_A::"+newTok, svc.updated[service.CursorCredSession])
	require.Equal(t, "machine-keep", svc.updated[service.CursorCredMachineID], "换 token 不能换设备指纹")
	require.Equal(t, "old-refresh", svc.updated[service.CursorCredRefreshToken], "没给新 refresh_token 时保留旧值")
}

func TestReplaceCursorCredential_RejectsOtherUserAndNonCursor(t *testing.T) {
	svc := &cursorReplaceAdminService{
		stubAdminService: newStubAdminService(),
		existing: &service.Account{ID: 8, Platform: service.PlatformCursor,
			Credentials: map[string]any{service.CursorCredAccessToken: cursorHandlerTestJWT(t, "session", "user_A")}},
	}
	r := newCursorReplaceRouter(t, svc)

	rec := postCursorReplace(t, r, "8", map[string]any{"token": cursorHandlerTestJWT(t, "session", "user_B")})
	require.Equal(t, http.StatusBadRequest, rec.Code, "不同 Cursor 用户的 token 不能写进这个账号")
	require.Nil(t, svc.updated)

	svc.existing.Platform = service.PlatformKiro
	rec = postCursorReplace(t, r, "8", map[string]any{"token": cursorHandlerTestJWT(t, "session", "user_A")})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Nil(t, svc.updated)

	rec = postCursorReplace(t, r, "8", map[string]any{"token": "   "})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// makeCursorJWT 造一个可被协议层 parseJWT 解出的未签名 JWT。
// 只测解析路径，不涉及签名校验（Cursor 的 token 由上游签发，网关不验签）。
func makeCursorJWT(t *testing.T, typ, email, sub string, exp time.Time) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]any{"alg": "none", "typ": "JWT"})
	claims := map[string]any{"type": typ, "sub": sub}
	if email != "" {
		claims["email"] = email
	}
	if !exp.IsZero() {
		claims["exp"] = exp.Unix()
	}
	return header + "." + enc(claims) + ".sig"
}

func TestParseCursorCredential_RejectsEmpty(t *testing.T) {
	_, err := ParseCursorCredential("   ")
	require.Error(t, err)
}

func TestParseCursorCredential_SessionTokenIsUsedAsIs(t *testing.T) {
	// session 型 token 已是长效凭证（60 天），不该触发兑换。
	exp := time.Now().Add(60 * 24 * time.Hour)
	tok := makeCursorJWT(t, "session", "a@b.com", "user_123", exp)

	info, err := ParseCursorCredential(tok)
	require.NoError(t, err)
	require.False(t, info.Exchanged, "session token 不应触发 deep-login 兑换")
	require.Equal(t, tok, info.AccessToken)
	require.Equal(t, "a@b.com", info.Email)
	require.NotNil(t, info.ExpiresAt)
	require.WithinDuration(t, exp, *info.ExpiresAt, time.Second)
}

func TestParseCursorCredential_StripsCookiePrefixAndUIDPair(t *testing.T) {
	// 用户常常直接粘贴整条 cookie。NormalizeToken 要能剥掉
	// "WorkosCursorSessionToken=" 前缀、尾部 ";" 以及 "uid::" 前缀。
	tok := makeCursorJWT(t, "session", "c@d.com", "user_9", time.Now().Add(time.Hour))
	raw := "WorkosCursorSessionToken=user_9::" + tok + "; Path=/"

	info, err := ParseCursorCredential(raw)
	require.NoError(t, err)
	require.Equal(t, tok, info.AccessToken, "应剥离 cookie 前缀与 uid:: 前缀")
	require.True(t, strings.HasPrefix(info.Session, "user_9::"), "session 应保留 uid::JWT 形态")
}

func TestParseCursorCredential_UnknownTypeIsKeptNotRejected(t *testing.T) {
	// ⚠️ 上游若改了 type claim 名，直接拒绝会让整条导入路径失效。
	// 解析不出 type 时按原样使用，由后续健康检查暴露问题。
	tok := makeCursorJWT(t, "", "e@f.com", "user_1", time.Now().Add(time.Hour))
	info, err := ParseCursorCredential(tok)
	require.NoError(t, err)
	require.Equal(t, tok, info.AccessToken)
}

func TestBuildAccountCredentials_OmitsEmptyFields(t *testing.T) {
	// 空值不该写进 credentials：写空会覆盖掉已有的有效值。
	s := &CursorOAuthService{}
	creds := s.BuildAccountCredentials(CursorTokenInfo{AccessToken: "tok"})
	require.Equal(t, "tok", creds[CursorCredAccessToken])
	require.NotContains(t, creds, CursorCredRefreshToken)
	require.NotContains(t, creds, CursorCredEmail)
	require.NotContains(t, creds, CursorCredMachineID)
}

func TestBuildAccountCredentials_CarriesAllFields(t *testing.T) {
	s := &CursorOAuthService{}
	creds := s.BuildAccountCredentials(CursorTokenInfo{
		AccessToken:  "tok",
		RefreshToken: "ref",
		Session:      "uid::tok",
		Email:        "x@y.com",
		MachineID:    "machine-1",
	})
	require.Equal(t, "ref", creds[CursorCredRefreshToken])
	require.Equal(t, "uid::tok", creds[CursorCredSession])
	require.Equal(t, "x@y.com", creds[CursorCredEmail])
	require.Equal(t, "machine-1", creds[CursorCredMachineID])
}

func TestRefreshAccountToken_KeepsOldRefreshWhenNotRotated(t *testing.T) {
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return makeCursorJWT(t, "session", "g@h.com", "u", time.Now().Add(time.Hour)), "", nil
	})
	defer restore()

	s := &CursorOAuthService{}
	info, err := s.RefreshAccountToken(context.Background(), &Account{
		ID: 3, Platform: PlatformCursor, Type: AccountTypeOAuth,
		Credentials: map[string]any{CursorCredRefreshToken: "original-refresh"},
	})
	require.NoError(t, err)
	require.Equal(t, "original-refresh", info.RefreshToken, "上游未轮换时必须沿用旧 refresh token")
	require.Equal(t, "g@h.com", info.Email)
}

func TestRefreshAccountToken_RequiresRefreshToken(t *testing.T) {
	s := &CursorOAuthService{}
	_, err := s.RefreshAccountToken(context.Background(), &Account{
		ID: 3, Platform: PlatformCursor, Type: AccountTypeOAuth,
		Credentials: map[string]any{},
	})
	require.ErrorContains(t, err, "refresh_token")
}

func TestRefreshAccountToken_RejectsNonCursorAccount(t *testing.T) {
	s := &CursorOAuthService{}
	_, err := s.RefreshAccountToken(context.Background(), &Account{Platform: PlatformKiro})
	require.Error(t, err)
}

func TestRefreshAccountToken_EmptyAccessTokenIsAnError(t *testing.T) {
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "", "rotated", nil
	})
	defer restore()

	s := &CursorOAuthService{}
	_, err := s.RefreshAccountToken(context.Background(), &Account{
		Platform: PlatformCursor, Type: AccountTypeOAuth,
		Credentials: map[string]any{CursorCredRefreshToken: "r"},
	})
	require.Error(t, err)
}

func TestRefreshAccountToken_PropagatesUpstreamError(t *testing.T) {
	restore := cursor.SetAuthRefreshFnForTest(func(string) (string, string, error) {
		return "", "", errors.New("boom")
	})
	defer restore()

	s := &CursorOAuthService{}
	_, err := s.RefreshAccountToken(context.Background(), &Account{
		Platform: PlatformCursor, Type: AccountTypeOAuth,
		Credentials: map[string]any{CursorCredRefreshToken: "r"},
	})
	require.ErrorContains(t, err, "boom")
}

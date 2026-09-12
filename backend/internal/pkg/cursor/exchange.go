package cursor

// web token(网页 WorkosCursorSessionToken)寿命极短(几小时失效)。趁其新鲜,
// 复刻 Cursor 桌面端 deep-login(PKCE)流程, 用 web 会话授权换取 60 天 session token(+refreshToken)。
// 原理与 wafase Cursor Quick Login 一致: 有效 web 会话 → 授权 loginDeepControl → poll 出 IDE token。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

const deepLoginUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// ExchangeWebToken 用有效 web session(uid::webJWT)兑换 session accessToken + refreshToken。
// 返回 ok=false 表示 web token 已失效或兑换失败(调用方应原样保留 web token)。
func ExchangeWebToken(webSession string) (access, refresh string, ok bool) {
	if strings.TrimSpace(webSession) == "" {
		return "", "", false
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", false
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	reqID := genUUID()

	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	cookie := "WorkosCursorSessionToken=" + webSession

	// 1) loginDeepControl: 带上 web 会话打开深链登录页
	lcPath := "/loginDeepControl?challenge=" + challenge + "&uuid=" + reqID + "&mode=login&supportsSelectedTeamLogin=true&redirectTarget=cli"
	lc := "https://cursor.com" + lcPath
	req, _ := http.NewRequest(http.MethodGet, cursorWebURL(lcPath), nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", deepLoginUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	applyCursorProxyAuth(req)
	resp, err := cl.Do(req)
	if err != nil {
		return "", "", false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", "", false
	}

	// 2) loginDeepCallbackControl: 用 web 会话把本次 uuid/challenge 标记为已授权(等价"Yes, Log In")
	cbBody := fmt.Sprintf(`{"uuid":%q,"challenge":%q,"mode":"login"}`, reqID, challenge)
	cbReq, _ := http.NewRequest(http.MethodPost, cursorWebURL("/api/auth/loginDeepCallbackControl"), strings.NewReader(cbBody))
	cbReq.Header.Set("Cookie", cookie)
	cbReq.Header.Set("User-Agent", deepLoginUA)
	cbReq.Header.Set("Content-Type", "application/json")
	cbReq.Header.Set("Accept", "*/*")
	cbReq.Header.Set("Origin", "https://cursor.com")
	cbReq.Header.Set("Referer", lc)
	applyCursorProxyAuth(cbReq)
	cbResp, err := cl.Do(cbReq)
	if err != nil {
		return "", "", false
	}
	io.Copy(io.Discard, io.LimitReader(cbResp.Body, 1<<20))
	cbResp.Body.Close()
	// 授权成功=2xx; web token 失效会 307 跳 WorkOS 授权 → 视为失败
	if cbResp.StatusCode < 200 || cbResp.StatusCode >= 300 {
		return "", "", false
	}

	// 3) poll: 取回 IDE token 对(GET + query)
	pollURL := cursorAPIURL("/auth/poll?uuid=" + reqID + "&verifier=" + verifier)
	for i := 0; i < 12; i++ {
		pr, _ := http.NewRequest(http.MethodGet, pollURL, nil)
		pr.Header.Set("User-Agent", "Cursor/"+clientVersion)
		pr.Header.Set("Accept", "application/json")
		applyCursorProxyAuth(pr)
		presp, perr := cl.Do(pr)
		if perr == nil && presp.StatusCode == 200 {
			body, _ := io.ReadAll(io.LimitReader(presp.Body, 1<<20))
			presp.Body.Close()
			var d struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
			}
			if json.Unmarshal(body, &d) == nil && d.AccessToken != "" {
				return d.AccessToken, d.RefreshToken, true
			}
		} else if presp != nil {
			presp.Body.Close()
		}
		time.Sleep(1200 * time.Millisecond)
	}
	return "", "", false
}

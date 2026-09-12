package cursor

// Cursor 上游所需的设备签名与 token 解析, 全部在本仓库原生实现,
// 不依赖任何外部 Cursor 中转项目。算法对齐 Cursor 桌面端 connect+json 协议。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// genUUID 生成 v4 UUID
func genUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sha256Hex 计算 SHA256 十六进制
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:])
}

// NormalizeToken 归一 Cursor token, 返回 (access, session, uid):
//
//	access  = 纯 JWT(实际用于 api2.cursor.sh Bearer 鉴权)
//	uid     = 用户 id(cookie 前缀 user_xxx, 或从 JWT sub 派生)
//	session = "uid::access"(WorkosCursorSessionToken 的值形态, 供 cursor.com 面板 cookie)
//
// 兼容: 纯 JWT / "uid::JWT" / "WorkosCursorSessionToken=...; other=1" / URL 编码。
func NormalizeToken(in string) (access, session, uid string) {
	s := strings.TrimSpace(in)
	s = strings.TrimPrefix(s, "WorkosCursorSessionToken=")
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	if unesc, err := url.QueryUnescape(s); err == nil {
		s = unesc
	}
	s = strings.TrimSpace(s)
	access = s
	if i := strings.LastIndex(s, "::"); i >= 0 {
		uid = strings.TrimSpace(s[:i])
		access = strings.TrimSpace(s[i+2:])
	}
	if uid == "" {
		uid = jwtUserID(access)
	}
	session = uid + "::" + access
	return
}

// TokenType 返回 JWT 的 type 声明(session / web), 解析失败返回 "unknown"。
func TokenType(token string) string {
	if t := parseJWT(token).Type; t != "" {
		return t
	}
	return "unknown"
}

type jwtClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Type  string `json:"type"`
	Exp   int64  `json:"exp"`
}

// parseJWT 解析 JWT 中段 claims(不校验签名, 仅取 sub/email/type/exp)
func parseJWT(token string) jwtClaims {
	var c jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return c
	}
	seg := parts[1]
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	data, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		data, err = base64.StdEncoding.DecodeString(seg)
		if err != nil {
			return c
		}
	}
	_ = json.Unmarshal(data, &c)
	return c
}

// jwtSubject 返回 JWT sub(用于账号去重)
func jwtSubject(token string) string { return parseJWT(token).Sub }

// jwtEmail 返回 JWT email
func jwtEmail(token string) string { return parseJWT(token).Email }

// jwtExpiry 返回 JWT exp 对应时间(零值 = 无/解析失败), 用作账号到期时间。
func jwtExpiry(token string) time.Time {
	if e := parseJWT(token).Exp; e > 0 {
		return time.Unix(e, 0)
	}
	return time.Time{}
}

// jwtUserID 从 JWT sub 取用户 id(去掉 auth0|/grok| 等前缀)
func jwtUserID(token string) string {
	sub := parseJWT(token).Sub
	if i := strings.LastIndex(sub, "|"); i >= 0 {
		return sub[i+1:]
	}
	return sub
}

// machineID 依据 token 派生设备 ID(AccessToken 已是纯 JWT)
func machineID(a *Account) string {
	if a.MachineID != "" && len(a.MachineID) >= 32 {
		return a.MachineID
	}
	if a.AccessToken != "" {
		return sha256Hex("CursorAPI/" + a.AccessToken)
	}
	return sha256Hex(fmt.Sprintf("CursorFallback/%d", time.Now().UnixNano()))
}

// macMachineID 派生 macMachineId
func macMachineID(a *Account) string {
	base := a.MachineID
	if base == "" {
		base = a.AccessToken
	}
	return sha256Hex("CursorMacMachine/" + base)
}

// sessionID 基于 token 生成 UUID 形式 session id
func sessionID(token string) string {
	h := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// clientKey 基于 token 的 sha256
func clientKey(token string) string { return sha256Hex(token) }

// genChecksum 生成 x-cursor-checksum: Jyh 滚动异或时间戳 + urlsafe base64 + machineId[/macMachineId]
func genChecksum(machID, macMachID string) string {
	ts := time.Now().UnixMilli() / 1_000_000
	ba := []byte{
		byte((ts >> 40) & 0xFF), byte((ts >> 32) & 0xFF), byte((ts >> 24) & 0xFF),
		byte((ts >> 16) & 0xFF), byte((ts >> 8) & 0xFF), byte(ts & 0xFF),
	}
	t := byte(165)
	for i := range ba {
		ba[i] = ((ba[i] ^ t) + byte(i%256)) & 0xFF
		t = ba[i]
	}
	var enc strings.Builder
	for i := 0; i < len(ba); i += 3 {
		a := ba[i]
		var b, c byte
		if i+1 < len(ba) {
			b = ba[i+1]
		}
		if i+2 < len(ba) {
			c = ba[i+2]
		}
		enc.WriteByte(b64Alphabet[a>>2])
		enc.WriteByte(b64Alphabet[((a&3)<<4)|(b>>4)])
		if i+1 < len(ba) {
			enc.WriteByte(b64Alphabet[((b&15)<<2)|(c>>6)])
		}
		if i+2 < len(ba) {
			enc.WriteByte(b64Alphabet[c&63])
		}
	}
	if macMachID != "" {
		return enc.String() + machID + "/" + macMachID
	}
	return enc.String() + machID
}

// ── 业务层入口（sub2api 新增）─────────────────────────────────────────

// JWTExpiry 返回 token 的 exp 时间；零值表示无 exp 或解析失败。
// ⚠️ 不校验签名，只用于「该不该提前刷新」的本地判断，不可用于鉴权。
func JWTExpiry(token string) time.Time { return jwtExpiry(token) }

// JWTEmail 返回 token 中的 email（可能为空），用于导入账号时回填身份。
func JWTEmail(token string) string { return jwtEmail(token) }

// JWTSubject 返回 token 的 sub，用于账号去重。
func JWTSubject(token string) string { return jwtSubject(token) }

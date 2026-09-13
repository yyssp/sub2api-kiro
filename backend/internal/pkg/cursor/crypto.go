package cursor

// Cursor 上游所需的设备签名与 token 解析, 全部在本仓库原生实现,
// 不依赖任何外部 Cursor 中转项目。算法对齐 Cursor 桌面端 connect+json 协议。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// NewMachineID 铸造一个新的设备指纹种子：32 字节随机数的小写 hex（64 字符）。
//
// 真实客户端的 machineId 就是 32 字节随机 hex，不来自任何硬件属性——
// 也就是说它可以任意，但必须**稳定**：后端期望同一个账号始终出示同一设备。
// 因此它只在账号创建/导入时铸造一次并落库，之后不再变化（尤其不随 token 刷新变）。
func NewMachineID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于不可恢复的环境问题；退回时间戳种子也好过返回空串
		// 让调用方写入一个空指纹。
		return sha256Hex(fmt.Sprintf("CursorMachineSeed/%d", time.Now().UnixNano()))
	}
	return hex.EncodeToString(b)
}

// machineID 返回账号的设备指纹种子。
//
// ⚠️ 绝不能在这里从 AccessToken 派生：那会让 token 每刷新一次指纹就变一次，
// 上游会把该账号视为不断更换设备。种子必须在建号时铸造并落库
// （见 NewMachineID 与 service 层的 BuildAccountCredentials）。
// 这里的兜底只为「存量账号还没补上 machine_id」这一过渡期服务，
// 它同样不含任何随 token 变化的输入。
func machineID(a *Account) string {
	if a.MachineID != "" && len(a.MachineID) >= 32 {
		return a.MachineID
	}
	// 过渡兜底：按账号 ID 派生一个稳定值。账号 ID 在账号生命周期内不变，
	// 因此指纹至少不会随 token 漂移。ID 为 0（未落库的临时账号）时退回
	// 邮箱，仍然与 token 无关。
	if a.ID != 0 {
		return sha256Hex(fmt.Sprintf("CursorMachine/account/%d", a.ID))
	}
	if a.Email != "" {
		return sha256Hex("CursorMachine/email/" + a.Email)
	}
	return sha256Hex("CursorMachine/anonymous")
}

// macMachineID 派生 macMachineId。
//
// ⚠️ 必须从 machineID(a) 派生，不能在 MachineID 为空时退回 AccessToken：
// 那会让 macMachineId 与 machineId 一样随 token 刷新漂移（同 B 项）。
func macMachineID(a *Account) string {
	return sha256Hex("CursorMacMachine/" + machineID(a))
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
	// ⚠️ 位移数必须对 32 取模，不能用 Go 的真 64 位移位。
	// 真实客户端是 JS，位运算在 int32 上进行：C>>40 实为 C>>8、C>>32 实为 C>>0。
	// 直译成 64 位移位会在真实客户端有数据的位置产出零字节——ts（毫秒/1e6）
	// 只有约 21 位，前两字节会恒为 0,0，这是稳定可识别的非官方客户端特征。
	ba := []byte{
		byte((ts >> 8) & 0xFF), byte(ts & 0xFF), byte((ts >> 24) & 0xFF),
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

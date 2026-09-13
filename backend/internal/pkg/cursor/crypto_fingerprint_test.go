//go:build unit

package cursor

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestMachineID_DoesNotDeriveFromAccessToken 是 B 项的核心护栏。
// 还原「无 machine_id 时从 AccessToken 派生」会让本用例失败：
// token 一刷新，设备指纹就变，上游会把该账号视为不断更换设备。
func TestMachineID_DoesNotDeriveFromAccessToken(t *testing.T) {
	const stable = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	before := &Account{MachineID: stable, AccessToken: "jwt-before"}
	after := &Account{MachineID: stable, AccessToken: "jwt-after-refresh"}

	if machineID(before) != machineID(after) {
		t.Fatalf("machine_id 稳定时指纹不应随 token 变化: %s -> %s", machineID(before), machineID(after))
	}
	if machineID(before) != stable {
		t.Fatalf("已落库的 machine_id 应原样使用, 实际 %q", machineID(before))
	}

	// 没有落库的 machine_id 时，绝不能退化成「从 access_token 派生」——
	// 那正是漂移的来源。
	noID1 := &Account{AccessToken: "jwt-before"}
	noID2 := &Account{AccessToken: "jwt-after-refresh"}
	if machineID(noID1) == sha256Hex("CursorAPI/jwt-before") {
		t.Fatal("machineID 仍在从 access_token 派生, token 刷新会导致指纹漂移")
	}
	_ = noID2
}

// TestNewMachineID_ShapeMatchesRealClient 确认铸造出的 machine_id
// 形态与真实客户端一致：64 个十六进制字符（32 字节）。
func TestNewMachineID_ShapeMatchesRealClient(t *testing.T) {
	id := NewMachineID()
	if len(id) != 64 {
		t.Fatalf("machine_id 应为 64 个十六进制字符, 实际 %d: %q", len(id), id)
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Fatalf("machine_id 应是合法 hex: %v", err)
	}
	if strings.ToLower(id) != id {
		t.Fatalf("machine_id 应为小写 hex: %q", id)
	}
	// 必须满足 machineID() 的长度门槛，否则铸造出来也会被忽略。
	if len(id) < 32 {
		t.Fatalf("machine_id 长度不足, 会被 machineID() 忽略: %q", id)
	}
}

// TestNewMachineID_IsRandom 防止「铸造」退化成常量或从账号派生：
// 每个账号必须有独立的设备身份。
func TestNewMachineID_IsRandom(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		id := NewMachineID()
		if seen[id] {
			t.Fatalf("NewMachineID 产生重复值 %q, 不是随机的", id)
		}
		seen[id] = true
	}
}

// TestGenChecksum_UsesJSShiftSemantics 是 E 项护栏。
//
// 真实客户端是 JS，位运算在 int32 上做，位移数对 32 取模：
// C>>40 实为 C>>8，C>>32 实为 C>>0。用 Go 真 64 位移位直译，
// 因 ts 只有约 21 位，前两字节恒为 0——这是稳定可识别的非官方客户端特征。
//
// checksum 前 8 个 base64 字符编码了 6 字节时间戳，这里反解出来比对。
func TestGenChecksum_UsesJSShiftSemantics(t *testing.T) {
	const machID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	got := genChecksum(machID, "")
	encoded := strings.TrimSuffix(got, machID)
	if len(encoded) != 8 {
		t.Fatalf("时间戳段应为 8 个 base64 字符, 实际 %d: %q", len(encoded), encoded)
	}

	decoded := decodeChecksumTimestamp(t, encoded)
	if len(decoded) != 6 {
		t.Fatalf("时间戳段应解出 6 字节, 实际 %d", len(decoded))
	}

	// 还原滚动异或：b[i] = ((b[i] ^ t) + i) & 255, t = b[i]（异或前的 t 是前一个密文字节）
	raw := make([]byte, 6)
	t0 := byte(165)
	for i := 0; i < 6; i++ {
		raw[i] = (decoded[i] - byte(i%256)) ^ t0
		t0 = decoded[i]
	}

	// JS 语义下，前两字节是 ts 的低 16 位（ts>>8、ts>>0），
	// 而 ts 约 21 位意味着这两个字节不可能同时为 0。
	if raw[0] == 0 && raw[1] == 0 {
		t.Fatalf("前两字节为 0,0 —— 仍在用真 64 位移位, 与真实客户端不一致: %v", raw)
	}
	// 后四字节在两种语义下相同（ts>>24/16/8/0），可作正确性交叉校验：
	// raw[4] 应等于 ts>>8 的低 8 位，与 raw[0] 相同。
	if raw[0] != raw[4] {
		t.Fatalf("JS 语义下 raw[0](ts>>40→ts>>8) 应与 raw[4](ts>>8) 相同: %v", raw)
	}
	if raw[1] != raw[5] {
		t.Fatalf("JS 语义下 raw[1](ts>>32→ts>>0) 应与 raw[5](ts>>0) 相同: %v", raw)
	}
}

// decodeChecksumTimestamp 反解 genChecksum 使用的 unpadded base64url。
func decodeChecksumTimestamp(t *testing.T, encoded string) []byte {
	t.Helper()
	index := func(c byte) int64 {
		return int64(strings.IndexByte(b64Alphabet, c))
	}
	var out []byte
	for i := 0; i+1 < len(encoded); i += 4 {
		n := index(encoded[i])<<18 | index(encoded[i+1])<<12
		count := 1
		if i+2 < len(encoded) {
			n |= index(encoded[i+2]) << 6
			count = 2
		}
		if i+3 < len(encoded) {
			n |= index(encoded[i+3])
			count = 3
		}
		out = append(out, byte(n>>16))
		if count >= 2 {
			out = append(out, byte(n>>8))
		}
		if count >= 3 {
			out = append(out, byte(n))
		}
	}
	return out
}

// TestGenChecksum_KeepsMachineIDSuffix 反向护栏：
// 改位移语义不能动到 machineId/macMachineId 拼接部分。
func TestGenChecksum_KeepsMachineIDSuffix(t *testing.T) {
	const machID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const macID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if got := genChecksum(machID, macID); !strings.HasSuffix(got, machID+"/"+macID) {
		t.Fatalf("带 macMachineId 时应拼成 machID/macID, 实际 %q", got)
	}
	if got := genChecksum(machID, ""); !strings.HasSuffix(got, machID) || strings.Contains(got, "/") {
		t.Fatalf("无 macMachineId 时不应出现分隔符, 实际 %q", got)
	}
}

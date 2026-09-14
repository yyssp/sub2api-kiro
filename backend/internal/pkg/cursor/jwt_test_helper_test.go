package cursor

import (
	"encoding/base64"
	"encoding/json"
)

// mkJWT 构造用于测试的未签名 JWT（移植自 ai2api account_test.go）。
func mkJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pb, _ := json.Marshal(claims)
	p := base64.RawURLEncoding.EncodeToString(pb)
	return h + "." + p + ".sig"
}

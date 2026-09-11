package kiro

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// kiro.rs 凭证文件的导入支持。
//
// 这里刻意与 ParseKiroCredentialExport 分开：那条路径是 Kiro IDE 导出专用的，
// 必须保持严格（要求 accessToken + refreshToken + 运行时元数据），否则任意
// OAuth JSON 都能被当作 Kiro 账号导入。
//
// kiro.rs 的凭证文件形态不同：它只持久化 refreshToken（accessToken 属于运行期
// 派生物，不入库），API-key 文件更是只有一个 kiroApiKey 字段。用严格校验去卡
// 它必然全量拒绝，因此这里用一套独立的宽松解析 + 自动补齐。
//
// 补齐规则对齐 kiro.rs 自身的默认值，避免导入后行为与原系统不一致。

const (
	// kiro.rs 的 endpoint 默认值；API-key 凭证单独默认 cli。
	KiroRsDefaultEndpoint       = "ide"
	KiroRsAPIKeyDefaultEndpoint = "cli"
)

// KiroRsCredential 是一条 kiro.rs 凭证补齐后的结果。
//
// TokenData 承载与现有账号存储对齐的字段，Endpoint/Priority/Disabled 是
// kiro.rs 特有的调度属性，单独保留以便上层决定如何落库。
type KiroRsCredential struct {
	*TokenData
	Endpoint string `json:"endpoint,omitempty"`
	Priority int    `json:"priority"`
	Disabled bool   `json:"disabled"`
}

// ParseKiroRsCredentials 解析 kiro.rs 的凭证文件。
//
// 支持三种形态：
//   - 单个 JSON 对象
//   - JSON 数组（multiple 形态，带 priority/endpoint/disabled）
//   - 纯文本，每行 `ksk_xxx|region`
//
// 返回的每条凭证都已补齐 authMethod/machineId/region/endpoint 等字段。
func ParseKiroRsCredentials(raw string) ([]*KiroRsCredential, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("凭证内容为空")
	}

	// 非 JSON 起始的内容按纯文本 API-key 清单处理。
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return parseKiroRsPlainText(trimmed)
	}

	var items []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return nil, fmt.Errorf("解析凭证数组失败: %w", err)
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("凭证数组为空")
		}
	} else {
		items = []json.RawMessage{json.RawMessage(trimmed)}
	}

	result := make([]*KiroRsCredential, 0, len(items))
	for i, item := range items {
		cred, err := parseKiroRsObject(item)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条凭证: %w", i+1, err)
		}
		result = append(result, cred)
	}
	return result, nil
}

// parseKiroRsPlainText 解析 `ksk_xxx|region` 纯文本清单。
//
// 校验规则对齐 kiro.rs：至多一个分隔符、key 非空、region 需是合法的 host label。
func parseKiroRsPlainText(raw string) ([]*KiroRsCredential, error) {
	lines := strings.Split(raw, "\n")
	result := make([]*KiroRsCredential, 0, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		// 跳过空行与注释行。
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) > 2 {
			return nil, fmt.Errorf("第 %d 行: 格式应为 `密钥` 或 `密钥|区域`", i+1)
		}
		apiKey := strings.TrimSpace(parts[0])
		if apiKey == "" {
			return nil, fmt.Errorf("第 %d 行: 密钥为空", i+1)
		}
		region := ""
		if len(parts) == 2 {
			region = strings.TrimSpace(parts[1])
			if err := validateKiroRsRegion(region); err != nil {
				return nil, fmt.Errorf("第 %d 行: %w", i+1, err)
			}
		}
		token := &TokenData{
			APIKey:     apiKey,
			AuthMethod: "api_key",
			Region:     region,
		}
		result = append(result, completeKiroRsCredential(token, KiroRsAPIKeyDefaultEndpoint, 0, false))
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("未解析到任何有效凭证")
	}
	return result, nil
}

// validateKiroRsRegion 校验 region 是否为合法 host label，规则同 kiro.rs。
func validateKiroRsRegion(region string) error {
	if region == "" {
		return fmt.Errorf("区域为空")
	}
	if len(region) > 63 {
		return fmt.Errorf("区域名过长: %s", region)
	}
	if strings.HasPrefix(region, "-") || strings.HasSuffix(region, "-") {
		return fmt.Errorf("区域名不能以 - 开头或结尾: %s", region)
	}
	for _, c := range region {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("区域名含非法字符: %s", region)
		}
	}
	return nil
}

// parseKiroRsObject 解析单条 JSON 凭证对象并补齐字段。
func parseKiroRsObject(data []byte) (*KiroRsCredential, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("凭证对象为空")
	}

	// TokenData.UnmarshalJSON 已同时兼容 camelCase 与 snake_case。
	token := &TokenData{}
	if err := json.Unmarshal(data, token); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}

	readString := func(keys ...string) string {
		for _, key := range keys {
			value, ok := raw[key]
			if !ok {
				continue
			}
			var text string
			if err := json.Unmarshal(value, &text); err == nil && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		return ""
	}

	// kiro.rs 的 alias：expired 等价于 expiresAt，scope 等价于 scopes。
	// TokenData 不认这两个别名，这里补读。
	if token.ExpiresAt == "" {
		token.ExpiresAt = readString("expired")
	}
	if token.Scopes == "" {
		token.Scopes = readString("scope")
	}

	endpoint := readString("endpoint")
	priority := readKiroRsInt(raw, "priority")
	disabled := readKiroRsBool(raw, "disabled")

	if token.AccessToken == "" && token.RefreshToken == "" && token.APIKey == "" {
		return nil, fmt.Errorf("缺少凭证内容: 需要 refreshToken、accessToken 或 kiroApiKey 之一")
	}

	return completeKiroRsCredential(token, endpoint, priority, disabled), nil
}

func readKiroRsInt(raw map[string]json.RawMessage, key string) int {
	value, ok := raw[key]
	if !ok {
		return 0
	}
	var num int
	if err := json.Unmarshal(value, &num); err == nil {
		return num
	}
	// 容忍字符串形式的数字。
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
			return parsed
		}
	}
	return 0
}

func readKiroRsBool(raw map[string]json.RawMessage, key string) bool {
	value, ok := raw[key]
	if !ok {
		return false
	}
	var flag bool
	if err := json.Unmarshal(value, &flag); err == nil {
		return flag
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		parsed, err := strconv.ParseBool(strings.TrimSpace(text))
		return err == nil && parsed
	}
	return false
}

// completeKiroRsCredential 按 kiro.rs 的默认值补齐缺失字段。
func completeKiroRsCredential(token *TokenData, endpoint string, priority int, disabled bool) *KiroRsCredential {
	token.AuthMethod = resolveKiroRsAuthMethod(token)

	// region 默认 us-east-1；kiro.rs 的 effective_* 逻辑在请求期按
	// authRegion > region 取值，这里只保证 region 有兜底值。
	if token.Region == "" {
		token.Region = defaultIDCRegion
	}

	// machineId 补齐规则同 kiro.rs：优先凭证自带，其次按凭证类型派生。
	// BuildMachineID 已实现 KotlinNativeAPI/ 与 KiroAPIKey/ 两种 seed。
	if token.MachineID == "" {
		token.MachineID = BuildMachineID(token.RefreshToken, token.APIKey, "")
	}

	if endpoint = strings.TrimSpace(endpoint); endpoint == "" {
		if token.AuthMethod == "api_key" {
			endpoint = KiroRsAPIKeyDefaultEndpoint
		} else {
			endpoint = KiroRsDefaultEndpoint
		}
	}

	if priority < 0 {
		priority = 0
	}

	// profileArn 刻意不补齐：kiro.rs 也是在请求期按 authMethod 解析的，
	// 导入期写死会在账号类型变化时产生错误的 ARN。

	return &KiroRsCredential{
		TokenData: token,
		Endpoint:  endpoint,
		Priority:  priority,
		Disabled:  disabled,
	}
}

// resolveKiroRsAuthMethod 归一化并推断 authMethod。
//
// kiro.rs 的归一化会先剥离所有非字母数字字符再小写比较，因此
// "Builder-ID"、"builder_id"、"BuilderID" 都会落到同一个分支。
func resolveKiroRsAuthMethod(token *TokenData) string {
	if method := canonicalizeKiroRsAuthMethod(token.AuthMethod); method != "" {
		return method
	}

	// 显式字段缺失时按 kiro.rs 的兜底顺序推断。
	if token.APIKey != "" && token.AccessToken == "" && token.RefreshToken == "" {
		return "api_key"
	}
	if token.ClientID != "" && token.ClientSecret != "" {
		return "idc"
	}
	if method := canonicalizeKiroRsAuthMethod(token.Provider); method == "external_idp" || method == "idc" {
		return method
	}
	return "social"
}

// canonicalizeKiroRsAuthMethod 剥离非字母数字后归一，映射同 kiro.rs。
func canonicalizeKiroRsAuthMethod(value string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(strings.TrimSpace(value)) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			_, _ = b.WriteRune(c)
		}
	}
	switch b.String() {
	case "":
		return ""
	case "idc", "builderid", "iam", "identitycenter", "awsbuilderid":
		return "idc"
	case "apikey", "kiroapikey":
		return "api_key"
	case "externalidp", "enterprise", "iamsso", "awsidc", "internal":
		return "external_idp"
	case "social", "oauth", "socialoauth", "oauthsocial":
		return "social"
	default:
		return ""
	}
}

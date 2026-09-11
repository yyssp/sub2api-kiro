package kiro

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// KiroRsSkipped 记录一条被跳过的条目及原因。
//
// 无法识别的条目不中断整批导入，但必须能在预览里逐条说明为什么被跳过，
// 否则用户只会看到「导入了 8 条」却不知道另外 2 条去哪了。
type KiroRsSkipped struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Sample string `json:"sample,omitempty"`
}

// KiroRsParseResult 是一次解析的完整结果。
type KiroRsParseResult struct {
	Credentials []*KiroRsCredential `json:"credentials"`
	Skipped     []*KiroRsSkipped    `json:"skipped,omitempty"`
}

// ParseKiroRsCredentials 解析 kiro.rs / KAM / 通用 JSON 凭证。
//
// 保留原签名供既有调用方使用；只要有一条能解析出来就不算失败。
func ParseKiroRsCredentials(raw string) ([]*KiroRsCredential, error) {
	result, err := ParseKiroRsCredentialsDetailed(raw)
	if err != nil {
		return nil, err
	}
	return result.Credentials, nil
}

// ParseKiroRsCredentialsDetailed 解析凭证并返回跳过明细。
//
// 解析分四层，逐层降级（对齐 kiro.rs 的 credential-import.ts）：
//  1. 纯文本：每行 `ksk_xxx|region`，要求全部行都以 ksk_ 开头，否则降级
//  2. JSON → 失败则按 JSONL 逐行解析
//  3. 容器解包：{credentials:[]} / {accounts:[]} / {data:{credentials:[]}} / 裸数组，递归
//  4. 字段归一化：大小写风格、字段别名、时间戳、profileArn 反解 region
func ParseKiroRsCredentialsDetailed(raw string) (*KiroRsParseResult, error) {
	// 内容多数是从文档/聊天记录里复制来的，先做一层粘贴清洗。
	trimmed := sanitizePastedCredentials(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("凭证内容为空")
	}

	// 第 1 层：纯文本 ksk 清单。要求每行都像 ksk_，有一行不符就整体降级走 JSON，
	// 避免把格式错误的 JSON 逐行吞成一堆垃圾密钥。
	if creds, ok := parseKiroRsPlainText(trimmed); ok {
		return &KiroRsParseResult{Credentials: creds}, nil
	}

	// 注释与行尾逗号只在确认是 JSON 后再剥离：纯文本清单里的 // 可能是密钥的一部分。
	normalized := stripJSONComments(trimmed)
	normalized = stripTrailingCommas(normalized)

	// 第 2 层：整体 JSON，失败则降级 JSONL。
	values, err := parseJSONOrJSONL(normalized)
	if err != nil {
		return nil, err
	}

	// 第 3 层：递归解包容器。
	items := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		items = append(items, extractKiroRsItems(value)...)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("未找到任何凭证条目")
	}

	// 第 4 层：逐条归一化，失败的记录原因但不中断。
	result := &KiroRsParseResult{Credentials: make([]*KiroRsCredential, 0, len(items))}
	for i, item := range items {
		cred, err := parseKiroRsObject(item)
		if err != nil {
			result.Skipped = append(result.Skipped, &KiroRsSkipped{
				Index:  i + 1,
				Reason: err.Error(),
				Sample: truncateForError(string(item)),
			})
			continue
		}
		result.Credentials = append(result.Credentials, cred)
	}

	if len(result.Credentials) == 0 {
		if len(result.Skipped) > 0 {
			return nil, fmt.Errorf("共 %d 条记录，但均无法识别（第 1 条: %s）",
				len(items), result.Skipped[0].Reason)
		}
		return nil, fmt.Errorf("未解析到任何有效凭证")
	}
	return result, nil
}

// parseJSONOrJSONL 先整体解析 JSON，失败且是多行时按 JSONL 逐行解析。
func parseJSONOrJSONL(s string) ([]json.RawMessage, error) {
	var single json.RawMessage
	if err := json.Unmarshal([]byte(s), &single); err == nil {
		return []json.RawMessage{single}, nil
	} else if !strings.Contains(s, "\n") {
		// 单行解析不了就是单行解析不了，直接把原始错误抛出去。
		return nil, fmt.Errorf("JSON 格式错误: %w", err)
	}

	lines := strings.Split(s, "\n")
	result := make([]json.RawMessage, 0, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var value json.RawMessage
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			return nil, fmt.Errorf("第 %d 行 JSONL 格式错误: %w", i+1, err)
		}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("未找到任何 JSON 内容")
	}
	return result, nil
}

// extractKiroRsItems 递归解包容器字段，取出真正的凭证对象。
//
// 支持 {credentials:[]}、{accounts:[]}、{data:{credentials:[]}} 以及裸数组的任意嵌套。
func extractKiroRsItems(value json.RawMessage) []json.RawMessage {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return nil
	}

	// 数组：逐个递归。
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(value, &arr); err != nil {
			return nil
		}
		result := make([]json.RawMessage, 0, len(arr))
		for _, item := range arr {
			result = append(result, extractKiroRsItems(item)...)
		}
		return result
	}

	if trimmed[0] != '{' {
		return nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(value, &obj); err != nil {
		return nil
	}

	// 容器字段：只有当它确实是数组时才下钻，否则 {"credentials":{...}} 这种
	// 单账号嵌套写法会被误当成容器而丢失。
	for _, key := range []string{"credentials", "accounts"} {
		if inner, ok := obj[key]; ok && strings.HasPrefix(strings.TrimSpace(string(inner)), "[") {
			return extractKiroRsItems(inner)
		}
	}
	if data, ok := obj["data"]; ok {
		var dataObj map[string]json.RawMessage
		if err := json.Unmarshal(data, &dataObj); err == nil {
			for _, key := range []string{"credentials", "accounts"} {
				if inner, ok := dataObj[key]; ok && strings.HasPrefix(strings.TrimSpace(string(inner)), "[") {
					return extractKiroRsItems(inner)
				}
			}
		}
	}

	return []json.RawMessage{value}
}

// sanitizePastedCredentials 清洗粘贴内容。
//
// 用户多数是从文档、聊天记录或终端里复制过来的，常见地会带上 markdown 代码围栏。
// 不剥离的话 ```json 这类行会被当成纯文本密钥，静默生成垃圾账号——比直接报错更糟。
func sanitizePastedCredentials(raw string) string {
	// 统一换行，去掉 BOM 与零宽字符：从网页复制经常混入这些。
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.ReplaceAll(raw, "\ufeff", "")
	raw = strings.ReplaceAll(raw, "\u200b", "")

	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		// 只剥围栏行本身（```、```json），围栏内的内容原样保留。
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// stripJSONComments 去掉 JSONC 的 // 行注释与 /* */ 块注释。
//
// 必须感知字符串字面量：refreshToken 之类的值里可能含有 // 或 /*，
// 无脑正则替换会把凭证内容截断。
func stripJSONComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]

		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}

		if c == '/' && i+1 < len(s) {
			if s[i+1] == '/' {
				// 行注释：跳到行尾，保留换行符本身。
				for i < len(s) && s[i] != '\n' {
					i++
				}
				if i < len(s) {
					b.WriteByte('\n')
				}
				continue
			}
			if s[i+1] == '*' {
				// 块注释：跳到结束标记；未闭合则视为注释到结尾。
				end := strings.Index(s[i+2:], "*/")
				if end < 0 {
					return b.String()
				}
				i += 2 + end + 1
				continue
			}
		}

		b.WriteByte(c)
	}
	return b.String()
}

// stripTrailingCommas 去掉 } 或 ] 前面多余的逗号。
// 同样要跳过字符串字面量，避免改坏凭证里的内容。
func stripTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]

		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}

		if c == ',' {
			// 向后看第一个非空白字符，是 } 或 ] 就丢掉这个逗号。
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}

		b.WriteByte(c)
	}
	return b.String()
}

// looksLikeAPIKeyLine 判断一行是否像 API key。
//
// 纯文本清单不能来者不拒：粘进来的说明文字、残留的 markdown 标记
// 若被当成密钥，会静默创建一批无效账号，排查成本很高。
func looksLikeAPIKeyLine(key string) bool {
	if key == "" {
		return false
	}
	// 密钥不含空白字符。
	if strings.ContainsAny(key, " \t") {
		return false
	}
	// JSON 片段不是密钥：避免把格式错误的 JSON 当成纯文本清单逐行吞掉。
	if strings.ContainsAny(key, "{}[]\"") {
		return false
	}
	for _, c := range key {
		isAllowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' || c == '+' || c == '/' || c == '='
		if !isAllowed {
			return false
		}
	}
	return true
}

// truncateForError 截断用于报错的片段，避免把完整凭证写进错误信息/日志。
func truncateForError(s string) string {
	const limit = 24
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// KiroAPIKeyPrefix 是 Kiro API 密钥的固定前缀。
const KiroAPIKeyPrefix = "ksk_"

// parseKiroRsPlainText 解析 `ksk_xxx|region` 纯文本清单。
//
// 要求**每一行**都以 ksk_ 开头，只要有一行不符就返回 ok=false 整体降级去试 JSON。
// 这个「全有或全无」的判定同 kiro.rs：它保证格式错误的 JSON 不会被逐行
// 当成密钥吞掉，静默生成一批垃圾账号——那比直接报错难排查得多。
func parseKiroRsPlainText(raw string) ([]*KiroRsCredential, bool) {
	lines := strings.Split(raw, "\n")
	result := make([]*KiroRsCredential, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// 跳过空行与注释行。
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		parts := strings.SplitN(line, "|", 2)
		apiKey := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(apiKey, KiroAPIKeyPrefix) || !looksLikeAPIKeyLine(apiKey) {
			return nil, false
		}

		region := ""
		if len(parts) == 2 {
			region = strings.TrimSpace(parts[1])
			// region 不合法时同样整体降级，而不是报错：这一行也许本就不是密钥清单。
			if validateKiroRsRegion(region) != nil {
				return nil, false
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
		return nil, false
	}
	return result, true
}

// profileArnRegion 从 profileArn 反解 region。
//
// 形如 arn:aws:codewhisperer:us-east-1:699475941385:profile/XXX，取第 4 段。
// 规则同 kiro.rs：必须是 codewhisperer 服务且至少 6 段，否则不猜。
func profileArnRegion(profileArn string) string {
	parts := strings.Split(strings.TrimSpace(profileArn), ":")
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "codewhisperer" {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// normalizeTimestamp 把各种形态的过期时间统一成 RFC3339。
//
// 导出文件里 expiresAt 可能是 ISO 字符串、秒级或毫秒级 Unix 时间戳，
// 也可能是数字字符串。非数字的内容原样返回（已经是 ISO 就不动它）。
func normalizeTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		// 不是数字，当作已经是 ISO 字符串。
		return value
	}

	// 阈值同 kiro.rs：大于 1e10 视为毫秒。
	millis := int64(seconds)
	if seconds <= 1e10 {
		millis = int64(seconds * 1000)
	}
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
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

// canonicalizeKeys 把 key 归一成小写无分隔形式，用于风格无关的字段查找。
//
// TokenData.UnmarshalJSON 只显式列了 camelCase 与 snake_case，
// 导出文件里还会出现 kebab-case（refresh-token）和大小写混写。
// 这里统一降噪，让 refreshToken / refresh_token / refresh-token / REFRESH_TOKEN 等价。
func canonicalizeKeys(raw map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		var b strings.Builder
		for _, c := range strings.ToLower(key) {
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				_, _ = b.WriteRune(c)
			}
		}
		// 先到先得：原始 camelCase 通常排在前面，不覆盖已有值。
		if canonical := b.String(); canonical != "" {
			if _, exists := result[canonical]; !exists {
				result[canonical] = value
			}
		}
	}
	return result
}

// parseKiroRsObject 解析单条 JSON 凭证对象并补齐字段。
func parseKiroRsObject(data []byte) (*KiroRsCredential, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("不是合法的 JSON 对象")
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("凭证对象为空")
	}

	// 平铺字段与 credentials 嵌套字段合并：KAM 旧版是嵌套的，新版是平铺的，
	// 平铺优先（新版格式更权威），嵌套用作兜底。
	merged := canonicalizeKeys(raw)
	if nested, ok := raw["credentials"]; ok {
		var nestedMap map[string]json.RawMessage
		if err := json.Unmarshal(nested, &nestedMap); err == nil {
			for key, value := range canonicalizeKeys(nestedMap) {
				if _, exists := merged[key]; !exists {
					merged[key] = value
				}
			}
		}
	}

	readString := func(keys ...string) string {
		for _, key := range keys {
			value, ok := merged[key]
			if !ok {
				continue
			}
			var text string
			if err := json.Unmarshal(value, &text); err == nil && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
			// 数字形态的值（如 expiresAt 时间戳）也要能取出来。
			var num json.Number
			if err := json.Unmarshal(value, &num); err == nil {
				return num.String()
			}
		}
		return ""
	}

	token := &TokenData{
		AccessToken:       readString("accesstoken"),
		RefreshToken:      readString("refreshtoken"),
		APIKey:            readString("kiroapikey", "apikey"),
		ProfileArn:        readString("profilearn"),
		AuthMethod:        readString("authmethod"),
		Provider:          readString("provider"),
		ClientID:          readString("clientid"),
		ClientSecret:      readString("clientsecret"),
		ClientIDHash:      readString("clientidhash"),
		Email:             readString("email", "nickname"),
		StartURL:          readString("starturl"),
		Region:            readString("region"),
		APIRegion:         readString("apiregion"),
		MachineID:         readString("machineid"),
		SubscriptionTitle: readString("subscriptiontitle"),
		TokenEndpoint:     readString("tokenendpoint"),
		IssuerURL:         readString("issuerurl"),
		// alias：scope 等价于 scopes。
		Scopes: readString("scopes", "scope"),
		// alias：expired 等价于 expiresAt；时间戳统一转 RFC3339。
		ExpiresAt: normalizeTimestamp(readString("expiresat", "expired")),
	}

	// API key 也支持 `ksk_xxx|region` 的带区域写法。
	if key, region, found := strings.Cut(token.APIKey, "|"); found {
		token.APIKey = strings.TrimSpace(key)
		if region = strings.TrimSpace(region); region != "" && token.Region == "" {
			token.Region = region
		}
	}

	// apiRegion 兜底：从 profileArn 反解。
	if token.APIRegion == "" {
		token.APIRegion = profileArnRegion(token.ProfileArn)
	}

	if token.AccessToken == "" && token.RefreshToken == "" && token.APIKey == "" {
		return nil, fmt.Errorf("缺少凭证内容: 需要 refreshToken、accessToken 或 kiroApiKey 之一")
	}

	return completeKiroRsCredential(
		token,
		readString("endpoint"),
		readKiroRsInt(merged, "priority"),
		readKiroRsBool(merged, "disabled"),
	), nil
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

package cursor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 本文件负责把用户粘贴的 Cursor 凭证文本解析成可预览的条目清单。
//
// 为什么不复用 internal/pkg/kiro 的 ParseKiroRsCredentialsDetailed：
// 那边的清洗辅助（sanitizePastedCredentials/stripJSONComments/...）全是包私有，
// 想复用就得改上游文件把它们导出——那正是旁挂式接入要避免的合并冲突面。
// 这里借鉴它的**分层降级思路**（纯文本 → 整体 JSON → JSONL → 递归解包容器 →
// 逐条归一化且坏条目不中断），但字段语义完全按 Cursor 自己的凭证形态来。

// plainTextFieldSeparator 拆分纯文本形态里的 access token / refresh token / 备注。
//
// `----` 与 `|` 等价，任选其一即可：`----` 在一行长 JWT 里视觉上更容易辨认，
// `|` 则与既有的备注分隔符保持同一套语法。四连短横线不会出现在 JWT 本体
// （base64url 字符集虽含 `-`，但不会连续四个）也不会出现在 `uid::JWT` 前缀里。
var plainTextFieldSeparator = regexp.MustCompile(`\s*(?:----|\|)\s*`)

// ImportCredential 是一条解析出来的 Cursor 凭证。
//
// 只承载"能从文本里读到的东西"，不含任何需要联网才能得到的信息：
// web token 兑换、用量/套餐回填都发生在真正建号的时候，预览阶段
// 不该因为 N 条凭证触发 N 次上游请求。
type ImportCredential struct {
	// AccessToken 是纯 JWT（api2.cursor.sh 的 Bearer 值）。
	AccessToken string `json:"access_token"`
	// Session 是 uid::JWT（cursor.com 面板 cookie 值）。
	Session string `json:"session"`
	// TokenType 取自 JWT 的 type 声明：session / web / unknown。
	// web 表示这条寿命只有几小时，前端要显著提示"需尽快兑换"。
	TokenType string `json:"token_type"`
	// RefreshToken 仅在导出文件里带了才有；纯文本粘贴拿不到。
	RefreshToken string `json:"refresh_token,omitempty"`
	Email        string `json:"email,omitempty"`
	// MachineID 若导出文件里带了就沿用——它是 x-cursor-checksum 的设备指纹
	// 种子，换机器等于在上游眼里换了设备，能继承就不要重新生成。
	MachineID string `json:"machine_id,omitempty"`
	Note      string `json:"note,omitempty"`
	// Disabled 沿用导出文件里的停用状态，建号时映射为 inactive/不可调度。
	Disabled bool `json:"disabled"`
}

// ImportSkipped 是一条被跳过的记录及原因。
type ImportSkipped struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Sample string `json:"sample,omitempty"`
}

// ImportParseResult 是一次解析的完整结果。
//
// 坏数据不中断整批解析，但必须让用户知道少了什么——静默丢条目
// 会让用户以为导入成功，实际少了几个号。
type ImportParseResult struct {
	Credentials []*ImportCredential `json:"credentials"`
	Skipped     []*ImportSkipped    `json:"skipped,omitempty"`
}

// ParseImportCredentials 解析用户粘贴的 Cursor 凭证文本。
//
// 支持的输入形态（按尝试顺序）：
//  1. 纯文本清单：每行一个 token，可以是裸 JWT、uid::JWT，
//     也可以是整段 `WorkosCursorSessionToken=xxx; Path=/` cookie
//  2. 整体 JSON：对象或数组，字段名兼容 ai2api 导出的 camelCase
//     与 sub2api 习惯的 snake_case
//  3. JSONL：每行一个 JSON 对象
//
// 同一 access_token 只保留第一条：导出文件里同号重复很常见，
// 让它建出多个账号会把调度权重悄悄放大。
func ParseImportCredentials(raw string) (*ImportParseResult, error) {
	trimmed := sanitizeImportText(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("凭证内容为空")
	}

	// 第 1 层：纯文本 token 清单。要求每行都能解析出 JWT，
	// 有一行不符就整体降级走 JSON——否则格式错误的 JSON 会被逐行
	// 吞成一堆垃圾 token。
	if creds, ok := parseImportPlainText(trimmed); ok {
		return dedupeImportResult(&ImportParseResult{Credentials: creds}), nil
	}

	// 注释与行尾逗号只在确认不是纯文本后再剥离：token 里可能含 //。
	normalized := stripImportJSONComments(trimmed)
	normalized = stripImportTrailingCommas(normalized)

	// 第 2 层：整体 JSON，失败则降级 JSONL。
	values, err := parseImportJSONOrJSONL(normalized)
	if err != nil {
		return nil, err
	}

	// 第 3 层：递归解包容器（{"accounts":[...]} / {"data":{"items":[...]}} 之类）。
	items := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		items = append(items, extractImportItems(value)...)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("未找到任何凭证条目")
	}

	// 第 4 层：逐条归一化，失败的记录原因但不中断。
	result := &ImportParseResult{Credentials: make([]*ImportCredential, 0, len(items))}
	for i, item := range items {
		cred, err := parseImportObject(item)
		if err != nil {
			result.Skipped = append(result.Skipped, &ImportSkipped{
				Index:  i + 1,
				Reason: err.Error(),
				Sample: truncateImportSample(string(item)),
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
	return dedupeImportResult(result), nil
}

// sanitizeImportText 做一层粘贴清洗：统一换行、去 BOM、去包裹的代码块围栏。
func sanitizeImportText(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.TrimSpace(s)

	// 从文档/聊天里复制常带 ``` 围栏。
	if strings.HasPrefix(s, "```") {
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// parseImportPlainText 按"每行一个 token"解析。
//
// 全有或全无：任何一行解析不出 JWT 就返回 false 整体降级。宽松处理
// 会把一份坏 JSON 的每一行都当成 token 建号，产出一堆垃圾账号。
func parseImportPlainText(raw string) ([]*ImportCredential, bool) {
	lines := strings.Split(raw, "\n")
	creds := make([]*ImportCredential, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 注释行允许出现在纯文本清单里。
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		// 出现 JSON 结构字符即判定不是纯文本清单。
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			return nil, false
		}
		token, refreshToken := splitPlainTextFields(line)
		cred, err := newImportCredential(token)
		if err != nil {
			return nil, false
		}
		cred.RefreshToken = refreshToken
		creds = append(creds, cred)
	}
	if len(creds) == 0 {
		return nil, false
	}
	return creds, true
}

// splitPlainTextFields 把一行纯文本拆成 access token 与 refresh token。
//
// 支持的形态（refresh token 可省略）：
//
//	eyJ...                      仅 access token
//	eyJ...----rt_xxx            access + refresh
//	eyJ...|rt_xxx               同上（改用 `|`）
//
// ⚠️ 只切一刀：分隔符之后的全部内容都是 refresh token，不再解析第三段。
// refresh token 本身不含 `|` 或 `----`，切多刀只会把它截断。
func splitPlainTextFields(line string) (token, refreshToken string) {
	parts := plainTextFieldSeparator.Split(line, 2)

	token = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		refreshToken = trimCookieAttributes(strings.TrimSpace(parts[1]))
	}
	return token, refreshToken
}

// trimCookieAttributes 去掉 refresh token 尾部粘连的 cookie 属性。
//
// ⚠️ access token 一侧由 NormalizeToken 负责剥离属性，但它在切分**之后**
// 才执行，管不到 refresh token 这一段。整段 cookie 追加 refresh token 时
// （`WorkosCursorSessionToken=...----rt_x; Path=/`），`; Path=/` 会留在
// refresh token 里，让后续每一次续期都带着垃圾后缀去请求上游并失败。
func trimCookieAttributes(s string) string {
	if idx := strings.IndexByte(s, ';'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

// newImportCredential 从一个 token 字符串构造条目。
func newImportCredential(rawToken string) (*ImportCredential, error) {
	access, session, _ := NormalizeToken(rawToken)
	access = strings.TrimSpace(access)
	if access == "" {
		return nil, fmt.Errorf("无法从内容中提取 token")
	}
	// JWT 至少要有三段：不做这层校验，随便一个单词都会被当成 token。
	if strings.Count(access, ".") != 2 {
		return nil, fmt.Errorf("不是合法的 JWT（期望三段式）")
	}
	return &ImportCredential{
		AccessToken: access,
		Session:     session,
		TokenType:   TokenType(access),
		Email:       strings.TrimSpace(JWTEmail(access)),
	}, nil
}

// parseImportJSONOrJSONL 先试整体 JSON，失败再按 JSONL 逐行解析。
func parseImportJSONOrJSONL(s string) ([]json.RawMessage, error) {
	var single json.RawMessage
	if err := json.Unmarshal([]byte(s), &single); err == nil {
		return []json.RawMessage{single}, nil
	}

	lines := strings.Split(s, "\n")
	values := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
			continue
		}
		var value json.RawMessage
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			return nil, fmt.Errorf("内容既不是合法 JSON 也不是逐行 JSON: %w", err)
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("内容不是合法的 JSON")
	}
	return values, nil
}

// importContainerKeys 是常见的包装字段名，递归解包时逐个尝试。
var importContainerKeys = []string{"accounts", "credentials", "items", "data", "list", "tokens", "result"}

// extractImportItems 递归解包容器，返回真正的凭证对象清单。
func extractImportItems(value json.RawMessage) []json.RawMessage {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == "null" {
		return nil
	}

	// 数组：逐元素递归。
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal(value, &arr); err != nil {
			return nil
		}
		out := make([]json.RawMessage, 0, len(arr))
		for _, item := range arr {
			out = append(out, extractImportItems(item)...)
		}
		return out
	}

	// 字符串：可能是被 JSON 包了一层的 token，或整段嵌套 JSON。
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return nil
		}
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			return extractImportItems(json.RawMessage(s))
		}
		return []json.RawMessage{value}
	}

	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(value, &obj); err != nil {
		return nil
	}

	// 自身就带 token 字段 → 它是叶子凭证对象，不再往下拆。
	if importObjectHasToken(obj) {
		return []json.RawMessage{value}
	}

	// 否则尝试已知容器字段。
	for _, key := range importContainerKeys {
		if inner, ok := obj[key]; ok {
			if items := extractImportItems(inner); len(items) > 0 {
				return items
			}
		}
	}

	// 既不带 token 也拆不出容器：仍然作为条目返回，让上层把它记进 Skipped。
	// 在这里 return nil 会让条目凭空消失——用户粘 10 条、建出 8 个号却
	// 没有任何提示，这正是 Skipped 机制要避免的失败模式。
	return []json.RawMessage{value}
}

// importTokenKeys 是 token 字段的所有别名（已小写去分隔符）。
var importTokenKeys = []string{
	"accesstoken", "token", "session", "sessiontoken",
	"workoscursorsessiontoken", "cookie", "jwt",
}

func importObjectHasToken(obj map[string]json.RawMessage) bool {
	canonical := canonicalizeImportKeys(obj)
	for _, key := range importTokenKeys {
		if raw, ok := canonical[key]; ok && strings.TrimSpace(string(raw)) != "null" {
			return true
		}
	}
	return false
}

// canonicalizeImportKeys 把字段名统一成小写无分隔符形式，
// 让 accessToken / access_token / AccessToken 都能命中同一个键。
func canonicalizeImportKeys(raw map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		normalized := strings.ToLower(key)
		normalized = strings.ReplaceAll(normalized, "_", "")
		normalized = strings.ReplaceAll(normalized, "-", "")
		// 后出现的不覆盖已有的：原样字段优先于别名。
		if _, exists := out[normalized]; !exists {
			out[normalized] = value
		}
	}
	return out
}

// parseImportObject 把一个 JSON 条目归一化成凭证。
func parseImportObject(data []byte) (*ImportCredential, error) {
	trimmed := strings.TrimSpace(string(data))

	// 纯字符串条目：整个就是 token。
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("无法解析字符串条目")
		}
		return newImportCredential(s)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("条目不是 JSON 对象")
	}
	canonical := canonicalizeImportKeys(obj)

	rawToken := ""
	for _, key := range importTokenKeys {
		if v := readImportString(canonical, key); v != "" {
			rawToken = v
			break
		}
	}
	if rawToken == "" {
		return nil, fmt.Errorf("缺少 access_token / session 字段")
	}

	cred, err := newImportCredential(rawToken)
	if err != nil {
		return nil, err
	}

	// 导出文件里的显式字段覆盖从 JWT 推导出的值。
	if v := readImportString(canonical, "refreshtoken"); v != "" {
		cred.RefreshToken = v
	}
	if v := readImportString(canonical, "email"); v != "" {
		cred.Email = v
	}
	if v := readImportString(canonical, "machineid"); v != "" {
		cred.MachineID = v
	}
	if v := readImportString(canonical, "note"); v != "" {
		cred.Note = v
	} else if v := readImportString(canonical, "notes"); v != "" {
		cred.Note = v
	}
	cred.Disabled = readImportBool(canonical, "disabled")

	return cred, nil
}

func readImportString(canonical map[string]json.RawMessage, key string) string {
	raw, ok := canonical[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func readImportBool(canonical map[string]json.RawMessage, key string) bool {
	raw, ok := canonical[key]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	// 兼容 "true" / 1 这类宽松写法。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(strings.TrimSpace(s), "true")
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0
	}
	return false
}

// dedupeImportResult 按 access_token 去重，重复条目记入 Skipped。
func dedupeImportResult(result *ImportParseResult) *ImportParseResult {
	seen := make(map[string]struct{}, len(result.Credentials))
	kept := make([]*ImportCredential, 0, len(result.Credentials))
	for i, cred := range result.Credentials {
		if _, dup := seen[cred.AccessToken]; dup {
			result.Skipped = append(result.Skipped, &ImportSkipped{
				Index:  i + 1,
				Reason: "与前面的条目是同一个 token，已跳过",
				Sample: truncateImportSample(cred.Email),
			})
			continue
		}
		seen[cred.AccessToken] = struct{}{}
		kept = append(kept, cred)
	}
	result.Credentials = kept
	return result
}

// stripImportJSONComments 剥离 JSON 里的 // 与 /* */ 注释，字符串内的原样保留。
func stripImportJSONComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			_ = b.WriteByte(c)
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
			_ = b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) {
			if s[i+1] == '/' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
				if i < len(s) {
					_ = b.WriteByte('\n')
				}
				continue
			}
			if s[i+1] == '*' {
				i += 2
				for i+1 < len(s) && (s[i] != '*' || s[i+1] != '/') {
					i++
				}
				i++ // 循环末尾的 i++ 补上第二个字符
				continue
			}
		}
		_ = b.WriteByte(c)
	}
	return b.String()
}

// stripImportTrailingCommas 去掉 } / ] 之前的多余逗号，字符串内的原样保留。
func stripImportTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			_ = b.WriteByte(c)
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
			_ = b.WriteByte(c)
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
		_ = b.WriteByte(c)
	}
	return b.String()
}

// truncateImportSample 截断样本，避免把完整 token 回显到前端。
func truncateImportSample(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "..."
}

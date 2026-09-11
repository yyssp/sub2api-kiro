package kiro

import (
	"testing"
)

// kiro.rs 的真实样例文件内容（逐字复制自 2ue_kiro.rs 仓库）。
//
// 这些内容被现有的 ParseKiroCredentialExport 全量拒绝（只有 refreshToken 没有
// accessToken；api_key 文件没有任何运行时元数据），宽松解析器必须全部接受并补齐。
//
// 刻意内联而不是读取兄弟仓库：测试不应依赖当前仓库之外的路径，
// 否则在 CI 或别人的检出里会静默跳过。
var kiroRsExamples = map[string]string{
	"credentials.example.apikey.json": `{
    "kiroApiKey": "ksk_your_api_key_here",
    "authMethod": "api_key"
}`,
	"credentials.example.idc.json": `{
  "refreshToken": "xxxxxxxxxxxxxxxxxxxx",
  "expiresAt": "2025-12-31T02:32:45.144Z",
  "authMethod": "idc",
  "clientId": "xxxxxxxxx",
  "clientSecret": "xxxxxxxxx",
  "region": "us-east-2",
  "machineId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`,
	"credentials.example.social.json": `{
  "refreshToken": "xxxxxxxxxxxxxxxxxxxx",
  "expiresAt": "2025-12-31T02:32:45.144Z",
  "authMethod": "social",
  "machineId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`,
	"credentials.example.multiple.json": `[
  {
    "refreshToken": "xxxxxxxxxxxxxxxxxxxx",
    "expiresAt": "2025-12-31T02:32:45.144Z",
    "authMethod": "social",
    "machineId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "priority": 0,
    "endpoint": "ide"
  },
  {
    "refreshToken": "yyyyyyyyyyyyyyyyyyyy",
    "expiresAt": "2025-12-31T02:32:45.144Z",
    "authMethod": "idc",
    "clientId": "xxxxxxxxx",
    "clientSecret": "xxxxxxxxx",
    "region": "us-east-2",
    "machineId": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "priority": 1,
    "disabled": true
  }
]`,
}

func loadKiroRsExample(t *testing.T, name string) string {
	t.Helper()
	content, ok := kiroRsExamples[name]
	if !ok {
		t.Fatalf("未知样例: %s", name)
	}
	return content
}

func TestParseKiroRsCredentialsAPIKeyExample(t *testing.T) {
	creds, err := ParseKiroRsCredentials(loadKiroRsExample(t, "credentials.example.apikey.json"))
	if err != nil {
		t.Fatalf("解析 apikey 样例失败: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("期望 1 条凭证，实际 %d", len(creds))
	}
	c := creds[0]
	if c.AuthMethod != "api_key" {
		t.Errorf("AuthMethod = %q, 期望 api_key", c.AuthMethod)
	}
	if c.APIKey != "ksk_your_api_key_here" {
		t.Errorf("APIKey = %q", c.APIKey)
	}
	// API-key 凭证的 endpoint 默认是 cli 而非 ide。
	if c.Endpoint != KiroRsAPIKeyDefaultEndpoint {
		t.Errorf("Endpoint = %q, 期望 %q", c.Endpoint, KiroRsAPIKeyDefaultEndpoint)
	}
	if c.Region != defaultIDCRegion {
		t.Errorf("Region = %q, 期望 %q", c.Region, defaultIDCRegion)
	}
	// machineId 由 apiKey 派生，必须是 64 位 hex。
	if len(c.MachineID) != 64 {
		t.Errorf("MachineID 长度 = %d, 期望 64: %q", len(c.MachineID), c.MachineID)
	}
	if want := sha256Hex("KiroAPIKey/ksk_your_api_key_here"); c.MachineID != want {
		t.Errorf("MachineID = %q, 期望由 apiKey 派生 %q", c.MachineID, want)
	}
	// profileArn 不在导入期补齐。
	if c.ProfileArn != "" {
		t.Errorf("ProfileArn 应为空，实际 %q", c.ProfileArn)
	}
}

func TestParseKiroRsCredentialsSocialExample(t *testing.T) {
	creds, err := ParseKiroRsCredentials(loadKiroRsExample(t, "credentials.example.social.json"))
	if err != nil {
		t.Fatalf("解析 social 样例失败: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("期望 1 条凭证，实际 %d", len(creds))
	}
	c := creds[0]
	if c.AuthMethod != "social" {
		t.Errorf("AuthMethod = %q, 期望 social", c.AuthMethod)
	}
	if c.RefreshToken != "xxxxxxxxxxxxxxxxxxxx" {
		t.Errorf("RefreshToken = %q", c.RefreshToken)
	}
	if c.Endpoint != KiroRsDefaultEndpoint {
		t.Errorf("Endpoint = %q, 期望 %q", c.Endpoint, KiroRsDefaultEndpoint)
	}
	// 样例自带 machineId，必须原样保留而不是重新派生。
	if c.MachineID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("MachineID 应保留原值，实际 %q", c.MachineID)
	}
	if c.ExpiresAt != "2025-12-31T02:32:45.144Z" {
		t.Errorf("ExpiresAt = %q", c.ExpiresAt)
	}
}

func TestParseKiroRsCredentialsIDCExample(t *testing.T) {
	creds, err := ParseKiroRsCredentials(loadKiroRsExample(t, "credentials.example.idc.json"))
	if err != nil {
		t.Fatalf("解析 idc 样例失败: %v", err)
	}
	c := creds[0]
	if c.AuthMethod != "idc" {
		t.Errorf("AuthMethod = %q, 期望 idc", c.AuthMethod)
	}
	if c.ClientID != "xxxxxxxxx" || c.ClientSecret != "xxxxxxxxx" {
		t.Errorf("clientId/clientSecret 未解析: %q / %q", c.ClientID, c.ClientSecret)
	}
	// 样例显式给了 us-east-2，不能被默认值覆盖。
	if c.Region != "us-east-2" {
		t.Errorf("Region = %q, 期望 us-east-2", c.Region)
	}
}

func TestParseKiroRsCredentialsMultipleExample(t *testing.T) {
	creds, err := ParseKiroRsCredentials(loadKiroRsExample(t, "credentials.example.multiple.json"))
	if err != nil {
		t.Fatalf("解析 multiple 样例失败: %v", err)
	}
	if len(creds) != 2 {
		t.Fatalf("期望 2 条凭证，实际 %d", len(creds))
	}

	first := creds[0]
	if first.AuthMethod != "social" || first.Priority != 0 || first.Disabled {
		t.Errorf("第 1 条: method=%q priority=%d disabled=%v", first.AuthMethod, first.Priority, first.Disabled)
	}
	if first.Endpoint != "ide" {
		t.Errorf("第 1 条 Endpoint = %q", first.Endpoint)
	}

	second := creds[1]
	if second.AuthMethod != "idc" {
		t.Errorf("第 2 条 AuthMethod = %q, 期望 idc", second.AuthMethod)
	}
	if second.Priority != 1 {
		t.Errorf("第 2 条 Priority = %d, 期望 1", second.Priority)
	}
	if !second.Disabled {
		t.Error("第 2 条 Disabled 应为 true")
	}
	// 第 2 条没写 endpoint，应补成非 api_key 的默认值 ide。
	if second.Endpoint != KiroRsDefaultEndpoint {
		t.Errorf("第 2 条 Endpoint = %q, 期望 %q", second.Endpoint, KiroRsDefaultEndpoint)
	}
}

// 对照测试：严格解析器确实拒绝这些文件，证明宽松路径不是多余的。
func TestStrictParserRejectsKiroRsExamples(t *testing.T) {
	for _, name := range []string{
		"credentials.example.apikey.json",
		"credentials.example.social.json",
		"credentials.example.idc.json",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKiroCredentialExport(loadKiroRsExample(t, name), ""); err == nil {
				t.Error("期望严格解析器拒绝 kiro.rs 文件，实际接受了")
			}
		})
	}
}

func TestResolveKiroRsAuthMethodInference(t *testing.T) {
	cases := []struct {
		name  string
		token *TokenData
		want  string
	}{
		{"显式 api_key", &TokenData{AuthMethod: "api_key", APIKey: "ksk_x"}, "api_key"},
		{"Builder-ID 归一到 idc", &TokenData{AuthMethod: "Builder-ID", RefreshToken: "r"}, "idc"},
		{"builder_id 归一到 idc", &TokenData{AuthMethod: "builder_id", RefreshToken: "r"}, "idc"},
		{"IAM 归一到 idc", &TokenData{AuthMethod: "IAM", RefreshToken: "r"}, "idc"},
		{"enterprise 归一到 external_idp", &TokenData{AuthMethod: "enterprise", RefreshToken: "r"}, "external_idp"},
		{"AWS-IDC 归一到 external_idp", &TokenData{AuthMethod: "AWS_IDC", RefreshToken: "r"}, "external_idp"},
		{"仅 apiKey 推断为 api_key", &TokenData{APIKey: "ksk_x"}, "api_key"},
		{"clientId+Secret 推断为 idc", &TokenData{RefreshToken: "r", ClientID: "c", ClientSecret: "s"}, "idc"},
		{"provider 为企业族推断 external_idp", &TokenData{RefreshToken: "r", Provider: "Enterprise"}, "external_idp"},
		{"缺省回落 social", &TokenData{RefreshToken: "r"}, "social"},
		{"无法识别的值回落 social", &TokenData{AuthMethod: "something-else", RefreshToken: "r"}, "social"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveKiroRsAuthMethod(tc.token); got != tc.want {
				t.Errorf("resolveKiroRsAuthMethod() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestParseKiroRsCredentialsPlainText(t *testing.T) {
	raw := "ksk_first|us-east-1\n# 注释行\n\nksk_second\nksk_third|eu-west-1\n"
	creds, err := ParseKiroRsCredentials(raw)
	if err != nil {
		t.Fatalf("解析纯文本失败: %v", err)
	}
	if len(creds) != 3 {
		t.Fatalf("期望 3 条凭证，实际 %d", len(creds))
	}
	if creds[0].APIKey != "ksk_first" || creds[0].Region != "us-east-1" {
		t.Errorf("第 1 条: key=%q region=%q", creds[0].APIKey, creds[0].Region)
	}
	// 没写 region 的行应补默认值。
	if creds[1].Region != defaultIDCRegion {
		t.Errorf("第 2 条 Region = %q, 期望 %q", creds[1].Region, defaultIDCRegion)
	}
	for i, c := range creds {
		if c.AuthMethod != "api_key" {
			t.Errorf("第 %d 条 AuthMethod = %q", i+1, c.AuthMethod)
		}
		if c.Endpoint != KiroRsAPIKeyDefaultEndpoint {
			t.Errorf("第 %d 条 Endpoint = %q", i+1, c.Endpoint)
		}
	}
}

func TestParseKiroRsCredentialsRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空内容", "   "},
		{"空数组", "[]"},
		{"空对象", "{}"},
		{"无凭证字段", `{"authMethod":"social","region":"us-east-1"}`},
		{"纯文本多个分隔符", "ksk_a|us-east-1|extra"},
		{"纯文本空密钥", "|us-east-1"},
		{"区域名以短横开头", "ksk_a|-bad"},
		{"区域名含非法字符", "ksk_a|us_east_1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseKiroRsCredentials(tc.raw); err == nil {
				t.Error("期望解析失败，实际成功")
			}
		})
	}
}

func TestParseKiroRsCredentialsAliases(t *testing.T) {
	// kiro.rs 的别名：expired -> expiresAt，scope -> scopes；
	// 同时验证 snake_case 也能解析。
	raw := `{"refresh_token":"r1","expired":"2025-12-31T02:32:45.144Z","scope":"openid profile","auth_method":"social"}`
	creds, err := ParseKiroRsCredentials(raw)
	if err != nil {
		t.Fatalf("解析别名失败: %v", err)
	}
	c := creds[0]
	if c.RefreshToken != "r1" {
		t.Errorf("RefreshToken = %q", c.RefreshToken)
	}
	if c.ExpiresAt != "2025-12-31T02:32:45.144Z" {
		t.Errorf("ExpiresAt = %q, expired 别名未生效", c.ExpiresAt)
	}
	if c.Scopes != "openid profile" {
		t.Errorf("Scopes = %q, scope 别名未生效", c.Scopes)
	}
}

func TestCompleteKiroRsCredentialDerivesMachineIDFromRefreshToken(t *testing.T) {
	creds, err := ParseKiroRsCredentials(`{"refreshToken":"rt_abc","authMethod":"social"}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := sha256Hex("KotlinNativeAPI/rt_abc")
	if creds[0].MachineID != want {
		t.Errorf("MachineID = %q, 期望由 refreshToken 派生 %q", creds[0].MachineID, want)
	}
}

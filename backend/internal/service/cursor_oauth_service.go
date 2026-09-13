package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// CursorOAuthService 负责把用户粘贴的 Cursor 凭证转成可落库的 credentials。
//
// ⚠️ Cursor 没有标准 OAuth 授权码流程，这里的"OAuth"只是沿用 sub2api 的
// 命名惯例（账号类型 AccountTypeOAuth），实际流程是：
//
//	web token(WorkosCursorSessionToken) --deep-login(PKCE)--> session token + refresh token
//
// 所以不要照 KiroOAuthService 去找 provider 配置 / 授权 URL / 回调，它们不存在。
type CursorOAuthService struct {
	accountRepo AccountRepository
	client      *cursor.Client
}

func NewCursorOAuthService(accountRepo AccountRepository) *CursorOAuthService {
	return &CursorOAuthService{accountRepo: accountRepo, client: sharedCursorClient()}
}

// CursorTokenInfo 是一次凭证解析/兑换的结果。
type CursorTokenInfo struct {
	AccessToken  string
	RefreshToken string
	Session      string
	Email        string
	MachineID    string
	ExpiresAt    *time.Time
	// Exchanged 表示这次是把短命 web token 兑换成了长效 session token。
	Exchanged bool
}

// ParseCursorCredential 把用户粘贴的字符串解析成可用凭证。
//
// 接受三种输入（与 Cursor 桌面端/网页端的实际产物一一对应）：
//  1. session token（JWT type=session）——直接可用，60 天有效
//  2. web token（JWT type=web，即 WorkosCursorSessionToken）——寿命仅几小时，
//     必须趁新鲜兑换成 session token，否则账号建好几小时后就失效
//  3. uid::JWT 形式的 cookie 值——NormalizeToken 会拆出 uid 与 JWT
//
// ⚠️ 兑换失败时**保留原 web token** 而不是报错：用户可能在网络受限环境下
// 导入，先落库再由后台续期兜底，比直接拒绝更不容易丢号。
func ParseCursorCredential(raw string) (CursorTokenInfo, error) {
	var info CursorTokenInfo
	if strings.TrimSpace(raw) == "" {
		return info, errors.New("cursor: credential is empty")
	}

	access, session, _ := cursor.NormalizeToken(raw)
	if strings.TrimSpace(access) == "" {
		return info, errors.New("cursor: cannot extract token from credential")
	}

	info.AccessToken = access
	info.Session = session

	switch cursor.TokenType(access) {
	case "web":
		// web token 寿命极短，立即尝试兑换。
		if exAccess, exRefresh, ok := cursor.ExchangeWebToken(session); ok {
			info.AccessToken = exAccess
			info.RefreshToken = exRefresh
			info.Exchanged = true
			_, info.Session, _ = cursor.NormalizeToken(exAccess)
		}
	case "session":
		// 已经是长效 token，无需兑换。
	default:
		// 解析不出 type 的一律按原样使用：可能是上游改了 claim 名，
		// 直接拒绝会让存量导入路径整体失效。
	}

	if email := strings.TrimSpace(cursor.JWTEmail(info.AccessToken)); email != "" {
		info.Email = email
	}
	if exp := cursor.JWTExpiry(info.AccessToken); !exp.IsZero() {
		e := exp
		info.ExpiresAt = &e
	}
	return info, nil
}

// BuildAccountCredentials 把解析结果转成 accounts.credentials 的写入值。
//
// machine_id 在这里生成并固化：它是 x-cursor-checksum 设备指纹的种子，
// 必须在账号生命周期内保持稳定——每次请求重新生成会让上游把该账号
// 视为不断更换设备，触发风控。
func (s *CursorOAuthService) BuildAccountCredentials(info CursorTokenInfo) map[string]any {
	creds := map[string]any{
		CursorCredAccessToken: info.AccessToken,
	}
	if strings.TrimSpace(info.RefreshToken) != "" {
		creds[CursorCredRefreshToken] = info.RefreshToken
	}
	if strings.TrimSpace(info.Session) != "" {
		creds[CursorCredSession] = info.Session
	}
	if strings.TrimSpace(info.Email) != "" {
		creds[CursorCredEmail] = info.Email
	}
	if strings.TrimSpace(info.MachineID) != "" {
		creds[CursorCredMachineID] = info.MachineID
	}
	return creds
}

// CursorImportInput 是批量导入的入参。
type CursorImportInput struct {
	// Content 是凭证文件原文：每行一个 token 的纯文本、单对象 JSON、
	// 数组 JSON 或 JSONL。
	Content string
}

// CursorImportEntry 是一条可预览的导入条目。
//
// 字段直接对应前端预览表的列，不含任何需要联网才能得到的信息：
// 预览阶段不联网是刻意的，见 ImportCursorCredentials 的说明。
type CursorImportEntry struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Session      string `json:"session,omitempty"`
	Email        string `json:"email,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	// TokenType 是 session / web / unknown。web 寿命只有几小时，
	// 前端必须显著提示，否则用户会建出一批几小时后集体失效的账号。
	TokenType string `json:"token_type"`
	// ExpiresAt 取自 JWT exp（RFC3339），无法解析时为空。
	ExpiresAt string `json:"expires_at,omitempty"`
	Note      string `json:"note,omitempty"`
	Disabled  bool   `json:"disabled"`
}

// CursorImportSkippedEntry 是一条被跳过的记录及原因。
type CursorImportSkippedEntry struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Sample string `json:"sample,omitempty"`
}

// CursorImportResult 是一次导入解析的结果。
type CursorImportResult struct {
	Entries []*CursorImportEntry `json:"entries"`
	// Skipped 记录无法识别或重复的条目，供前端在预览里逐条标注。
	// 坏数据不中断整批导入，但不能让用户不知道少了什么。
	Skipped []*CursorImportSkippedEntry `json:"skipped,omitempty"`
}

// ImportCursorCredentials 解析粘贴的凭证文本，返回可预览的条目清单。
//
// ⚠️ 这是纯解析，不联网：
// 单条导入路径（ParseCursorCredential）会对 web token 立即调
// cursor.ExchangeWebToken 去兑换，但批量预览如果照做，N 条凭证就是
// N 次上游请求——用户只是想看一眼解析结果，不该触发一轮网络风暴，
// 更不该在"我还没点确认"的时候就改变上游状态。
// web token 的兑换推迟到真正建号时（handler 里逐条走
// ParseCursorCredential），预览阶段只把 token_type 标出来提示用户。
func (s *CursorOAuthService) ImportCursorCredentials(input *CursorImportInput) (*CursorImportResult, error) {
	if input == nil {
		return nil, fmt.Errorf("cursor import input is required")
	}
	parsed, err := cursor.ParseImportCredentials(input.Content)
	if err != nil {
		return nil, err
	}

	result := &CursorImportResult{Entries: make([]*CursorImportEntry, 0, len(parsed.Credentials))}
	for _, skipped := range parsed.Skipped {
		result.Skipped = append(result.Skipped, &CursorImportSkippedEntry{
			Index:  skipped.Index,
			Reason: skipped.Reason,
			Sample: skipped.Sample,
		})
	}
	for _, cred := range parsed.Credentials {
		entry := &CursorImportEntry{
			AccessToken:  cred.AccessToken,
			RefreshToken: cred.RefreshToken,
			Session:      cred.Session,
			Email:        cred.Email,
			MachineID:    cred.MachineID,
			TokenType:    cred.TokenType,
			Note:         cred.Note,
			Disabled:     cred.Disabled,
		}
		if exp := cursor.JWTExpiry(cred.AccessToken); !exp.IsZero() {
			entry.ExpiresAt = exp.UTC().Format(time.RFC3339)
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

// RefreshAccountToken 供管理端"手动刷新"按钮使用。
func (s *CursorOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (CursorTokenInfo, error) {
	var info CursorTokenInfo
	if account == nil || account.Platform != PlatformCursor {
		return info, errors.New("cursor: not a cursor account")
	}
	refreshToken := strings.TrimSpace(account.GetCredential(CursorCredRefreshToken))
	if refreshToken == "" {
		return info, errors.New("cursor: missing refresh_token, re-import the account")
	}

	// 管理端手动刷新同样走账号代理：出口 IP 不一致会让这次"修复"操作
	// 本身变成一次异常活动记录。
	accessToken, rotated, err := cursor.RefreshAuthTokenVia(
		refreshToken, cursorAccountProxyURL(account))
	if err != nil {
		return info, err
	}
	if strings.TrimSpace(accessToken) == "" {
		return info, errors.New("cursor: upstream returned empty access_token")
	}

	info.AccessToken = accessToken
	// 空 rotated 表示上游不轮换，沿用旧值；写空会让账号永久失去续期能力。
	if strings.TrimSpace(rotated) != "" {
		info.RefreshToken = rotated
	} else {
		info.RefreshToken = refreshToken
	}
	if email := strings.TrimSpace(cursor.JWTEmail(accessToken)); email != "" {
		info.Email = email
	}
	if exp := cursor.JWTExpiry(accessToken); !exp.IsZero() {
		e := exp
		info.ExpiresAt = &e
	}
	_, info.Session, _ = cursor.NormalizeToken(accessToken)
	_ = ctx
	return info, nil
}

// testCursorAccountConnection 是账号连通性测试的 Cursor 分支。
//
// ⚠️ 不发一次真实对话来测连通性：agent.v1 的每次调用都消耗真实额度，
// 拿测试按钮去烧用户额度是不可接受的。改用 dashboard 端点探活——
// 它同时验证了 token 有效性与设备指纹（走同一套 buildHeaders）。
func (s *AccountTestService) testCursorAccountConnection(c *gin.Context, account *Account, modelID string) error {
	if account == nil || account.Platform != PlatformCursor {
		return errors.New("cursor: not a cursor account")
	}

	ctx := context.Background()
	if c != nil {
		ctx = c.Request.Context()
	}

	accessToken := strings.TrimSpace(account.GetCredential(CursorCredAccessToken))
	if s.cursorTokenProvider != nil {
		token, err := s.cursorTokenProvider.GetAccessToken(ctx, account)
		if err != nil {
			return fmt.Errorf("cursor: token unavailable: %w", err)
		}
		accessToken = token
	}
	if accessToken == "" {
		return errors.New("cursor: missing access_token")
	}

	pa := cursorProtocolAccount(account)
	pa.AccessToken = accessToken

	if err := sharedCursorClient().HealthCheck(&pa); err != nil {
		return fmt.Errorf("cursor: health check failed: %w", err)
	}

	// 目标模型不在账号可用清单里时给出明确提示，而不是让用户在真实请求里才发现。
	if model := strings.TrimSpace(modelID); model != "" {
		if !cursor.AccountUsableForModel(pa, cursor.StripPrefix(model)) {
			return fmt.Errorf("cursor: model %q is not available for this account (quota bucket %q)",
				model, cursor.ModelToQuotaBucket(cursor.StripPrefix(model)))
		}
	}
	return nil
}

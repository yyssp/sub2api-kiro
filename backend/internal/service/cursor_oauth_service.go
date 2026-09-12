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

	accessToken, rotated, err := cursor.RefreshAuthToken(refreshToken)
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

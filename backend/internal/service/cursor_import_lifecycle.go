package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Cursor 导入的「建号后」与「去重」逻辑。单独成文件，旁挂在通用建号流程之外。

// CursorExistingAccount 标识一条导入凭证在库中已有的同一 Cursor 用户（同 uid）。
type CursorExistingAccount struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// cursorAccountUserID 返回账号凭证对应的 Cursor 用户 ID（uid::JWT 里的 uid）。
// 优先取 session 里的 uid；没有则从 access_token 的 sub 推导。
func cursorAccountUserID(creds map[string]any) string {
	if session, _ := creds[CursorCredSession].(string); strings.TrimSpace(session) != "" {
		if _, _, uid := cursor.NormalizeToken(session); strings.TrimSpace(uid) != "" {
			return strings.TrimSpace(uid)
		}
	}
	access, _ := creds[CursorCredAccessToken].(string)
	return strings.TrimSpace(cursor.JWTUserID(strings.TrimSpace(access)))
}

// findExistingCursorAccountsByUID 按 Cursor 用户 ID 索引库中已有的 Cursor 账号（含已停用 / 出错的）。
//
// 同一 Cursor 用户的 token 会轮换，按 token 去重认不出"同一个号又导了一次"；uid 才是稳定身份。
func (s *CursorOAuthService) findExistingCursorAccountsByUID(ctx context.Context) (map[string]*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformCursor, "", "", "", 0, "")
	if err != nil {
		return nil, err
	}
	byUID := make(map[string]*Account, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		uid := cursorAccountUserID(account.Credentials)
		if uid == "" {
			continue
		}
		// 同一 uid 出现多个旧账号时取 ID 最小的那个，保证结果稳定。
		if prev, ok := byUID[uid]; !ok || account.ID < prev.ID {
			byUID[uid] = account
		}
	}
	return byUID, nil
}

// annotateCursorImportExisting 给预览条目标上库中已存在的同一用户，供前端提示「将更新已有账号」。
// 查库失败不影响预览：此时不标注，这批凭证按新账号创建。
func (s *CursorOAuthService) annotateCursorImportExisting(ctx context.Context, entries []*CursorImportEntry) {
	if len(entries) == 0 {
		return
	}
	byUID, err := s.findExistingCursorAccountsByUID(ctx)
	if err != nil {
		slog.Warn("cursor_import_existing_lookup_failed", "error", err)
		return
	}
	for _, entry := range entries {
		uid := cursorAccountUserID(map[string]any{
			CursorCredSession:     entry.Session,
			CursorCredAccessToken: entry.AccessToken,
		})
		entry.UserID = uid
		if existing, ok := byUID[uid]; ok && uid != "" {
			entry.Existing = &CursorExistingAccount{ID: existing.ID, Name: existing.Name, Status: existing.Status}
		}
	}
}

// 替换 / 重复导入凭证时的客户端错误（400）。
var (
	ErrCursorNotCursorAccount       = infraerrors.BadRequest("CURSOR_NOT_CURSOR_ACCOUNT", "目标账号不是 Cursor 账号")
	ErrCursorMissingAccessToken     = infraerrors.BadRequest("CURSOR_MISSING_ACCESS_TOKEN", "缺少 access_token")
	ErrCursorCredentialUserMismatch = infraerrors.BadRequest("CURSOR_CREDENTIAL_USER_MISMATCH", "凭证与目标账号不属于同一 Cursor 用户")
)

// CursorCredentialUpdate 是重复导入时写回已有账号的凭证增量。
type CursorCredentialUpdate struct {
	AccessToken  string
	RefreshToken string
	Session      string
	Email        string
}

// cursorImportCredentialUpdates 计算重复导入要写回的凭证字段。
//
// ⚠️ 只写 token 类字段，绝不碰 machine_id：设备指纹种子必须在账号生命周期内恒定，
// 换 token 不等于换设备。空值不写，避免把已有的 refresh_token / email 清空。
func cursorImportCredentialUpdates(in CursorCredentialUpdate) map[string]any {
	updates := map[string]any{}
	access := strings.TrimSpace(in.AccessToken)
	if access == "" {
		return updates
	}
	updates[CursorCredAccessToken] = access
	if refresh := strings.TrimSpace(in.RefreshToken); refresh != "" {
		updates[CursorCredRefreshToken] = refresh
	}
	session := strings.TrimSpace(in.Session)
	if session == "" {
		_, session, _ = cursor.NormalizeToken(access)
	}
	if session != "" {
		updates[CursorCredSession] = session
	}
	email := strings.TrimSpace(in.Email)
	if email == "" {
		email = strings.TrimSpace(cursor.JWTEmail(access))
	}
	if email != "" {
		updates[CursorCredEmail] = email
	}
	return updates
}

// ImportedCredentialUpdates 计算重复导入写回已有账号的凭证增量，并校验身份一致。
//
// 新凭证必须属于同一 Cursor 用户（uid 相同）：前端传来的 existing_account_id 只是预览阶段的
// 标注，不能让一个请求把别人的 token 写进任意账号。web token 先经该账号代理兑换成长效 token。
func (s *CursorOAuthService) ImportedCredentialUpdates(existing *Account, incoming map[string]any) (map[string]any, error) {
	if existing == nil || existing.Platform != PlatformCursor {
		return nil, ErrCursorNotCursorAccount
	}
	access, _ := incoming[CursorCredAccessToken].(string)
	if strings.TrimSpace(access) == "" {
		return nil, ErrCursorMissingAccessToken
	}
	existingUID := cursorAccountUserID(existing.Credentials)
	incomingUID := cursorAccountUserID(incoming)
	if existingUID == "" || incomingUID == "" || existingUID != incomingUID {
		return nil, ErrCursorCredentialUserMismatch
	}
	creds, _ := exchangeCursorWebTokenForCreate(PlatformCursor, cloneCredentialMap(incoming), cursorAccountProxyURL(existing))
	stringField := func(key string) string {
		v, _ := creds[key].(string)
		return v
	}
	return cursorImportCredentialUpdates(CursorCredentialUpdate{
		AccessToken:  stringField(CursorCredAccessToken),
		RefreshToken: stringField(CursorCredRefreshToken),
		Session:      stringField(CursorCredSession),
		Email:        stringField(CursorCredEmail),
	}), nil
}

func cloneCredentialMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// CursorProvisionOptions 控制建号（或更新凭证）后的补齐动作。
type CursorProvisionOptions struct {
	// ActivateSandTrial 为真时尝试激活 $250 Sand Trial（会改变上游账号状态，默认关闭）。
	ActivateSandTrial bool
}

// CursorProvisionResult 是一次补齐动作的结果，写进账号 extra 供管理台展示。
type CursorProvisionResult struct {
	Membership  string `json:"membership,omitempty"`
	TrialStatus string `json:"trial_status,omitempty"`
	TrialError  string `json:"trial_error,omitempty"`
}

// CursorExtraTrialStatusKey / CursorExtraTrialErrorKey 记录最近一次 Sand Trial 激活结果。
const (
	CursorExtraTrialStatusKey = "cursor_sand_trial_status"
	CursorExtraTrialErrorKey  = "cursor_sand_trial_error"
	CursorExtraTrialAtKey     = "cursor_sand_trial_at"
)

// ProvisionCursorAccount 在建号 / 换凭证后补齐账号信息：套餐（membership）、可选的
// Sand Trial 激活、三桶额度快照。全部走账号绑定的代理。
//
// 任何一步失败都不回滚建号：账号已可用，补齐只是让管理台信息完整、调度更准确，
// 失败的步骤会在下次额度刷新 / 手动操作时再补。
func (s *CursorOAuthService) ProvisionCursorAccount(ctx context.Context, accountID int64, opts CursorProvisionOptions) (*CursorProvisionResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, fmt.Errorf("cursor provision: service not initialized")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.Platform != PlatformCursor {
		return nil, fmt.Errorf("cursor provision: account %d is not a cursor account", accountID)
	}
	proxyURL := cursorAccountProxyURL(account)
	result := &CursorProvisionResult{}

	session := strings.TrimSpace(account.GetCredential(CursorCredSession))
	if session == "" {
		_, session, _ = cursor.NormalizeToken(account.GetCredential(CursorCredAccessToken))
	}
	if membership, ok := cursor.FetchMembership(session, proxyURL); ok {
		result.Membership = membership
		if account.GetCredential(CursorCredMembership) != membership {
			// 只写凭证列（persistAccountCredentials），不做整行 Update：补齐是异步的，
			// 整行写回会覆盖用户在此期间对名称、分组、调度参数的修改。
			merged := MergeCredentials(shallowCopyMap(account.Credentials), map[string]any{CursorCredMembership: membership})
			if err := persistAccountCredentials(ctx, s.accountRepo, account, merged); err != nil {
				return result, fmt.Errorf("cursor provision: save membership: %w", err)
			}
		}
	}

	if opts.ActivateSandTrial && s.client != nil {
		pa := cursorProtocolAccount(account)
		_, trialErr := s.client.StartSandTrial(&pa)
		result.TrialStatus = cursor.ClassifyStartSandTrialError(trialErr)
		extra := map[string]any{
			CursorExtraTrialStatusKey: result.TrialStatus,
			CursorExtraTrialAtKey:     time.Now().UTC().Format(time.RFC3339),
			CursorExtraTrialErrorKey:  "",
		}
		if result.TrialStatus == cursor.TrialStatusFailed && trialErr != nil {
			result.TrialError = trialErr.Error()
			extra[CursorExtraTrialErrorKey] = result.TrialError
		}
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, extra); err != nil {
			return result, fmt.Errorf("cursor provision: save trial status: %w", err)
		}
	}

	if s.usageFetcher != nil {
		quota, fetchErr := s.usageFetcher.Fetch(ctx, account, "")
		if fetchErr == nil {
			writeCursorQuota(account, quota)
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{CursorExtraQuotaKey: account.Extra[CursorExtraQuotaKey]}); err != nil {
				return result, fmt.Errorf("cursor provision: save quota: %w", err)
			}
		}
	}
	return result, nil
}

// ProvisionCursorAccountAsync 异步补齐，供建号接口在响应返回后调用。
func (s *CursorOAuthService) ProvisionCursorAccountAsync(accountID int64, opts CursorProvisionOptions) {
	if s == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("cursor_provision_panic", "account_id", accountID, "recover", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		result, err := s.ProvisionCursorAccount(ctx, accountID, opts)
		if err != nil {
			slog.Warn("cursor_provision_failed", "account_id", accountID, "error", err)
			return
		}
		slog.Info("cursor_provision_done", "account_id", accountID,
			"membership", result.Membership, "trial_status", result.TrialStatus)
	}()
}

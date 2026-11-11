package admin

import (
	"context"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// CursorImportAccountItem 是确认导入里的一条账号。
//
// 与批量建号的 CreateAccountRequest 共用字段，另加 existing_account_id：非空时表示库中已有
// 同一 Cursor 用户（预览阶段按 uid 标注），这一条只更新该账号的 token，不新建。
type CursorImportAccountItem struct {
	CreateAccountRequest
	ExistingAccountID *int64 `json:"existing_account_id"`
}

// CursorImportConfirmRequest 是确认导入的请求体。
type CursorImportConfirmRequest struct {
	Accounts []CursorImportAccountItem `json:"accounts" binding:"required,min=1"`
	// ActivateSandTrial 为真时，建号 / 更新后尝试激活 $250 Sand Trial（会改变上游账号状态，默认关闭）。
	ActivateSandTrial bool `json:"activate_sand_trial"`
}

// ConfirmCursorImport 按预览结果落库：新用户建号（走通用 CreateAccount，含 web token 兑换与
// 设备指纹铸造），已有用户只更新 token 类凭证（保留 machine_id 与全部调度参数）。
// 每个成功的账号随后异步补齐套餐、额度，并按开关激活 Sand Trial。
//
// 单独做成 Cursor 接口而不是往通用 BatchCreate 里加分支：「更新已有账号」是 Cursor 导入特有的
// 语义，放进通用批量建号会让所有平台的建号都多一条隐式改写路径。
func (h *AccountHandler) ConfirmCursorImport(c *gin.Context) {
	if h.cursorOAuthService == nil {
		response.BadRequest(c, "Cursor 导入服务未启用")
		return
	}
	var req CursorImportConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}
	groupIDs := make([]int64, 0)
	for _, item := range req.Accounts {
		if item.Platform != service.PlatformCursor {
			response.BadRequest(c, "仅支持导入 Cursor 账号")
			return
		}
		groupIDs = append(groupIDs, item.GroupIDs...)
	}
	if err := h.adminService.ValidateAccountGroupBindings(c.Request.Context(), groupIDs); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	executeAdminIdempotentJSON(c, "admin.cursor.import_confirm", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		created, updated, failed := 0, 0, 0
		results := make([]gin.H, 0, len(req.Accounts))
		provision := service.CursorProvisionOptions{ActivateSandTrial: req.ActivateSandTrial}

		for _, item := range req.Accounts {
			if item.RateMultiplier != nil && *item.RateMultiplier < 0 {
				failed++
				results = append(results, gin.H{"name": item.Name, "success": false, "error": "rate_multiplier must be >= 0"})
				continue
			}

			if item.ExistingAccountID != nil && *item.ExistingAccountID > 0 {
				account, err := h.updateImportedCursorAccount(ctx, *item.ExistingAccountID, item.Credentials)
				if err != nil {
					failed++
					results = append(results, gin.H{"name": item.Name, "success": false, "error": err.Error()})
					continue
				}
				updated++
				h.cursorOAuthService.ProvisionCursorAccountAsync(account.ID, provision)
				results = append(results, gin.H{"name": account.Name, "id": account.ID, "success": true, "action": "updated"})
				continue
			}

			sanitizeExtraBaseRPM(item.Extra)
			if err := service.ValidateUpstreamRequestIDHeaderExtra(item.Extra); err != nil {
				failed++
				results = append(results, gin.H{"name": item.Name, "success": false, "error": err.Error()})
				continue
			}
			skipCheck := item.ConfirmMixedChannelRisk != nil && *item.ConfirmMixedChannelRisk
			account, err := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
				Name:                  item.Name,
				Notes:                 item.Notes,
				Platform:              item.Platform,
				Type:                  item.Type,
				Credentials:           item.Credentials,
				Extra:                 item.Extra,
				ProxyID:               item.ProxyID,
				Concurrency:           item.Concurrency,
				Priority:              item.Priority,
				Status:                item.Status,
				Schedulable:           item.Schedulable,
				RateMultiplier:        item.RateMultiplier,
				LoadFactor:            item.LoadFactor,
				GroupIDs:              item.GroupIDs,
				ExpiresAt:             item.ExpiresAt,
				AutoPauseOnExpired:    item.AutoPauseOnExpired,
				SkipMixedChannelCheck: skipCheck,
			})
			if err != nil {
				failed++
				results = append(results, gin.H{"name": item.Name, "success": false, "error": err.Error()})
				continue
			}
			created++
			h.cursorOAuthService.ProvisionCursorAccountAsync(account.ID, provision)
			results = append(results, gin.H{"name": account.Name, "id": account.ID, "success": true, "action": "created"})
		}

		return gin.H{
			"created": created,
			"updated": updated,
			"success": created + updated,
			"failed":  failed,
			"results": results,
		}, nil
	})
}

// updateImportedCursorAccount 把重复导入的凭证写回已有账号。
//
// 只写 token 类字段（见 CursorOAuthService.ImportedCredentialUpdates），名称、分组、代理、调度参数一律不动：
// 用户重复导入是为了换 token，不是为了用这次表单覆盖已调好的账号配置。web token 同样先兑换。
func (h *AccountHandler) updateImportedCursorAccount(ctx context.Context, accountID int64, credentials map[string]any) (*service.Account, error) {
	existing, err := h.adminService.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if existing.Platform != service.PlatformCursor {
		return nil, service.ErrCursorNotCursorAccount
	}
	updates, err := h.cursorOAuthService.ImportedCredentialUpdates(existing, credentials)
	if err != nil {
		return nil, err
	}
	return h.adminService.UpdateAccount(ctx, accountID, &service.UpdateAccountInput{
		Credentials: service.MergeCredentials(existing.Credentials, updates),
	})
}

// CursorReplaceCredentialRequest 是编辑弹窗「替换 Cursor 凭证」的请求体。
type CursorReplaceCredentialRequest struct {
	// Token 接受与导入相同的形态：裸 JWT、uid::JWT、整段 WorkosCursorSessionToken cookie。
	Token        string `json:"token" binding:"required"`
	RefreshToken string `json:"refresh_token"`
	// ActivateSandTrial 为真时，替换后尝试激活 $250 Sand Trial。
	ActivateSandTrial bool `json:"activate_sand_trial"`
}

// ReplaceCursorCredential 替换已有 Cursor 账号的 token（编辑弹窗用）。
//
// 走与重复导入相同的规则：必须是同一 Cursor 用户（uid 相同）、web token 先经账号代理兑换、
// 只写 token 类字段、绝不改 machine_id。不经通用 PUT 是因为通用更新不做 token 归一化与身份校验，
// 粘贴整段 cookie 会原样落库。
func (h *AccountHandler) ReplaceCursorCredential(c *gin.Context) {
	if h.cursorOAuthService == nil {
		response.BadRequest(c, "Cursor 导入服务未启用")
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req CursorReplaceCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}
	access, session, _ := cursor.NormalizeToken(req.Token)
	if strings.TrimSpace(access) == "" {
		response.BadRequest(c, "无法从输入中解析出 Cursor token")
		return
	}
	credentials := map[string]any{
		service.CursorCredAccessToken: access,
		service.CursorCredSession:     session,
	}
	if refresh := strings.TrimSpace(req.RefreshToken); refresh != "" {
		credentials[service.CursorCredRefreshToken] = refresh
	}

	ctx := c.Request.Context()
	account, err := h.updateImportedCursorAccount(ctx, accountID, credentials)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.cursorOAuthService.ProvisionCursorAccountAsync(account.ID, service.CursorProvisionOptions{ActivateSandTrial: req.ActivateSandTrial})
	response.Success(c, h.accountResponseFromService(account))
}

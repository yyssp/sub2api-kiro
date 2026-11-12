package service

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// cursorUserRateLimitCooldown 是用户级/套餐级限流后的账号冷却时长。
//
// ⚠️ 为什么是 10 分钟：Cursor 的 ERROR_*_USER_RATE_LIMIT_EXCEEDED 在 Connect
// END 帧里通常不带 Retry-After / resetAt，只能本地给一个保守窗口。
//   - 太短（秒级）：账号几乎立刻被重新选中，又撞一次墙——每个请求白烧一次
//     换号，这正是修复前 FREE 限流被判成瞬时错误时的表现；
//   - 太长（小时级）：这是会自动结束的限流窗口，不是额度耗尽，长冷却会把
//     本可服务的账号白白摘掉。
//
// 取值与本仓库 Kiro 认证类临时故障的冷却（markKiroAuthTemporarilyUnavailable）
// 一致，属于同一类「账号此刻不行、过一会儿自愈」的状态。
const cursorUserRateLimitCooldown = 10 * time.Minute

// cursorRegionBlockedCooldown 是地区封锁后「该账号 × 该模型」的冷却时长。
//
// ⚠️ 为什么是 30 分钟：出口地区由账号绑定的代理决定，管理员不改代理就不会
// 恢复，短冷却只会让账号被反复选中、反复 403。取值对齐 ratelimit_service.go
// 的 upstreamModelNotFoundCooldown——同为「这个账号对这个模型确定性不可服务」。
// 管理员换代理后最多 30 分钟自动恢复，无需手工清理。
const cursorRegionBlockedCooldown = 30 * time.Minute

// cursorCooldownReasonMaxBytes 限制写进账号状态的上游原文长度，避免把超长
// 错误体塞进 temp_unschedulable_reason / model_rate_limits。
const cursorCooldownReasonMaxBytes = 512

const (
	cursorUserRateLimitReason = "cursor_user_rate_limited"
	cursorRegionBlockedReason = "cursor_region_blocked"
)

// applyCursorErrorCooldown 按归类结果给账号落冷却，返回是否已处理。
//
// 只处理「账号此刻不可用但会自愈」的两类：用户级限流（账号级冷却）与
// 地区封锁（账号×模型级冷却）。其余类别返回 false，交回 recordCursorFailure
// 的原有分支（停号 / 标桶）。
//
// ⚠️ 冷却写入失败只记日志：本次请求的换号已经由 failover 契约保证，
// 冷却只是让「下一个请求」别再选中它，写失败的代价是多撞一次墙，
// 不值得让当前请求失败。
func (s *GatewayService) applyCursorErrorCooldown(ctx context.Context, account *Account, classified cursorErrorClassification) bool {
	switch classified.Category {
	case cursorErrorUserRateLimit:
		s.coolDownCursorAccount(ctx, account, cursorUserRateLimitCooldown, cursorUserRateLimitReason, classified.Message)
		return true
	case cursorErrorRegionBlocked:
		s.coolDownCursorAccountModel(ctx, account, classified.CooldownModel, cursorRegionBlockedCooldown, cursorRegionBlockedReason, classified.Message)
		return true
	default:
		return false
	}
}

// coolDownCursorAccount 让整个账号短暂退出调度。
func (s *GatewayService) coolDownCursorAccount(ctx context.Context, account *Account, d time.Duration, reason, detail string) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	until := time.Now().Add(d)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, cursorCooldownReason(reason, detail)); err != nil {
		logger.L().Warn("cursor: failed to cool down account",
			zap.Int64("account_id", account.ID), zap.String("reason", reason), zap.Error(err))
	}
}

// coolDownCursorAccountModel 让账号只对目标模型退出调度。
//
// ⚠️ 地区封锁是「模型厂商 × 出口地区」维度的：同一个出口，Claude 可能被拒而
// Cursor 自研模型照常可用。整号冷却会把它对其它模型的可用容量一并砍掉。
//
// ⚠️ 冷却键必须与选号时 modelRateLimitKeysForRequest 读取的键一致，否则写了
// 等于没写——账号照样被选中、照样 403。两个口径都要覆盖：
//   - mappedModel：forwardCursorMessages 已做过账号级映射的模型名，不能再映射
//     一次（链式映射 a→b→c 时二次映射会写到错误的键上）；
//   - 选号口径：选号用 GetMappedModel(请求原始模型)，而开启渠道映射时转发用的
//     是渠道映射后的模型，两者不同。原始模型由 setOpsRequestContext 写在
//     ctx 的 ctxkey.Model 上，这里按选号同样的方式推导。
//
// 拿不到任何模型时退回账号级冷却——宁可多摘一点，也不能让一个确定性 403
// 的账号被反复选中。
func (s *GatewayService) coolDownCursorAccountModel(ctx context.Context, account *Account, mappedModel string, d time.Duration, reason, detail string) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	scopes := cursorCooldownModelScopes(ctx, account, mappedModel)
	if len(scopes) == 0 {
		s.coolDownCursorAccount(ctx, account, d, reason, detail)
		return
	}
	resetAt := time.Now().Add(d)
	for _, scope := range scopes {
		if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, scope, resetAt, cursorCooldownReason(reason, detail)); err != nil {
			logger.L().Warn("cursor: failed to cool down account model",
				zap.Int64("account_id", account.ID), zap.String("model", scope),
				zap.String("reason", reason), zap.Error(err))
		}
	}
}

// cursorCooldownModelScopes 返回需要写入的模型冷却键（去重、去空）。
func cursorCooldownModelScopes(ctx context.Context, account *Account, mappedModel string) []string {
	var scopes []string
	add := func(scope string) {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return
		}
		for _, existing := range scopes {
			if existing == scope {
				return
			}
		}
		scopes = append(scopes, scope)
	}
	add(mappedModel)
	if ctx != nil {
		if requested, ok := ctx.Value(ctxkey.Model).(string); ok && strings.TrimSpace(requested) != "" {
			add(account.GetMappedModel(strings.TrimSpace(requested)))
		}
	}
	return scopes
}

func cursorCooldownReason(reason, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return reason
	}
	return reason + ": " + truncateForLog([]byte(detail), cursorCooldownReasonMaxBytes)
}

package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cursor"
)

// cursorFailoverBody 把归类结果渲染成 Claude 形状的错误体。
//
// ⚠️ 这不是给客户端"顺手"多带一份信息，而是 Cursor 的错误文案能否到达
// 客户端的唯一通道。handler 的 handleFailoverExhausted 只认两个来源：
// 错误透传规则（按 ResponseBody 匹配）和 mapUpstreamError(StatusCode) 的
// 固定兜底文案。ClientMessage/ClientStatusCode 看着像通用字段，实际只在
// OpenAI/Grok/Antigravity 的专有分支里被读取（都以 IsOpenAICapacityShed()
// 之类的条件为前提），Cursor 走的 /v1/messages 通用路径根本不会碰它们。
//
// 所以不填 ResponseBody 的话，上游明确的「usage limit reached」会被降级成
// 「Upstream service temporarily unavailable」，用户无从判断是自己超额还是
// 网关故障，运维也会被引向错误的排查方向。
//
// 形状必须是 {"type":"error","error":{"type","message"}}：
// extractUpstreamErrorMessage 按 error.message 取值，这也正是非流式路径
// 原本自己写给客户端的那个结构。
func cursorFailoverBody(c cursorErrorClassification) []byte {
	body, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    c.ErrorType,
			"message": sanitizeUpstreamErrorMessage(c.Message),
		},
	})
	if err != nil {
		return nil
	}
	return body
}

// cursorCredentialFailover 把「拿不到 access token」翻译成凭证级调度契约。
//
// ⚠️ 取 token 失败必须能换号，否则整个凭证刷新链路的容错是假的：
// token provider 在确认失效时会 SetError 停用该账号，但如果这里返回普通
// error，handler 的 errors.As 不匹配 → 直接 return，本次请求当场失败。
// 结果是「号池里有 9 个健康账号，却因为选中的第 10 个 refresh token 过期
// 而对用户报错」，而且下一个请求还得重新踩一次才轮到别的号。
//
// ⚠️ 按「是否确认失效」分流 Scope，不能一律当账号问题：
//   - 确认失效（上游明确拒绝 refresh token）→ 账号级，换号必然有意义；
//   - 网络抖动/代理 EOF/5xx → provider 已刻意**不**写 SetError，此时若仍
//     按账号级上报，会把一次全局网络故障记成"这些账号都不健康"，
//     调度器的账号健康度被污染，故障恢复后仍会持续避开这些号。
//
// ⚠️ 不带 ResponseBody：这不是上游推理接口返回的错误体，伪造一个
// Claude 形状的 body 会让错误透传规则按"上游响应"去匹配它。
// 凭证阶段用 ClientStatusCode/ClientMessage 即可——它是 503 而非上游状态码。
func cursorCredentialFailover(c *gin.Context, account *Account, err error) *UpstreamFailoverError {
	confirmed := cursor.ConfirmedAuthRefreshFailure(err)

	scope := GatewayFailureScopeProvider
	if confirmed {
		scope = GatewayFailureScopeAccount
	}

	reason := strings.TrimSpace(cursor.AuthRefreshFailureReason(err))
	if reason == "" {
		reason = "cursor credential unavailable"
	}

	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			// ⚠️ 凭证获取发生在推理连接建立之前，账号此刻绑定的代理
			// 并不能证明本次失败与该代理有关（与 Grok 同口径）。
			ProxyID:     nil,
			ProxyName:   opsProxyNameUnknown,
			Platform:    PlatformCursor,
			AccountID:   account.ID,
			AccountName: account.Name,
			Stage:       string(GatewayFailureStageAccountAuth),
			Scope:       string(scope),
			Kind:        "credential_failover",
			Message:     sanitizeUpstreamErrorMessage(reason),
		})
	}

	return &UpstreamFailoverError{
		Stage:             GatewayFailureStageAccountAuth,
		Scope:             scope,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusServiceUnavailable,
		ClientMessage:     cursorCredentialUnavailableClientMessage,
	}
}

// cursorCredentialUnavailableClientMessage 是凭证不可用时对外的固定文案。
// ⚠️ 不能把上游原始错误直接回给客户端：refresh 失败的报文里可能带
// token 片段或账号标识，那是凭证泄露。
const cursorCredentialUnavailableClientMessage = "No healthy Cursor account is currently available"

// cursorFailoverError 把 Cursor 的错误归类翻译成网关通用的调度契约。
//
// ⚠️ 这是 Cursor 接入调度体系的关键一环。handler 的 failover 循环靠
// errors.As(*UpstreamFailoverError) 识别「可以换个账号再试」的失败；
// 返回普通 error 会让整条链路静默退化：
//
//   - 上游 429/凭证失效不触发换号，客户端直接吃到错误；
//   - 账号不进冷却，下个请求还会选中同一个坏号；
//   - 号池里其它健康账号完全用不上。
//
// ⚠️ 但绝不能一律标成"可换号"。终端协议错误和模型类错误在任何账号上
// 结果都一样，换号只是把同一个确定性失败逐个打给整个号池——一个坏请求
// 就能烧穿号池，且每个号都白记一次失败。
//
// realOutputWritten 表示是否已经推出过**语义内容**（message_start/delta）。
// 只写过 SSE 保活注释不算：注释按规范被客户端忽略，此时切换是安全的。
func cursorFailoverError(c cursorErrorClassification, realOutputWritten bool) *UpstreamFailoverError {
	if c.Category == "" {
		return nil
	}

	err := &UpstreamFailoverError{
		StatusCode:       c.StatusCode,
		ResponseBody:     cursorFailoverBody(c),
		ClientStatusCode: c.StatusCode,
		ClientMessage:    c.Message,
		// ⚠️ 已推出真实内容后不得再切换：客户端会在同一个流里收到
		// 第二份 message_start，协议直接被破坏，且前半段内容无法撤回。
		SafeToFailoverAfterWrite: !realOutputWritten,
	}

	switch {
	// 终端协议错误：请求形状本身不可服务，与账号健康无关。
	case c.Terminal:
		err.NextAccountAction = NextAccountStop
		err.Scope = GatewayFailureScopeRequest

	// 模型类错误：同套餐的其它账号结果完全一样，且属于 400 请求问题。
	case c.Category == cursorErrorBadModel, c.Category == cursorErrorNamedModelDenied:
		err.NextAccountAction = NextAccountStop
		err.Scope = GatewayFailureScopeRequest

	// 凭证失效：该号已被 SetError 停用，必须换号。
	case c.Kind == cursor.ErrAuth:
		err.NextAccountAction = NextAccountRetry
		err.Stage = GatewayFailureStageAccountAuth
		err.Scope = GatewayFailureScopeAccount

	// 额度耗尽：当前号该桶已满，换号可继续服务。
	// ⚠️ 不可同账号重试——桶满是确定状态，重试必然再失败。
	case c.Kind == cursor.ErrQuota:
		err.NextAccountAction = NextAccountRetry
		err.Scope = GatewayFailureScopeAccount

	// 上游瞬时故障：换号可能落到健康的上游实例上。
	default:
		err.NextAccountAction = NextAccountRetry
		err.Scope = GatewayFailureScopeAccount
		// ⚠️ 只有真正的瞬时故障才允许同账号重试并触发冷却。
		// 冷却策略按状态码分流（见 TempUnscheduleRetryableError），
		// 502 走空响应冷却；其余交由默认的换号逻辑处理。
		if c.StatusCode == http.StatusBadGateway {
			err.RetryableOnSameAccount = true
		}
	}

	return err
}

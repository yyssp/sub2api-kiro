package admin

import (
	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 本文件是 Cursor 凭证导入的接入面，单独成文件而不是往 account_handler.go
// 里加方法：旁挂式接入的原则是新增文件、不改存量文件，把与上游的合并冲突
// 面压到最小。复用 AccountHandler 是因为 cursorOAuthService 已经通过
// SetCursorOAuthService（wire.go:59）注入进来了，不需要再往 Handlers 聚合
// 结构里加一个新的 handler 字段。

// CursorImportRequest 是凭证导入的请求体。
type CursorImportRequest struct {
	// Content 是粘贴的凭证原文：每行一个 token 的纯文本、单对象 JSON、
	// 数组 JSON 或 JSONL。
	Content string `json:"content" binding:"required"`
}

// ImportCursorCredentials 解析 Cursor 凭证文本并返回可预览的账号条目。
//
// 只做解析，不建账号——账号创建仍走既有的批量创建接口，这样导入的账号
// 和手工添加的账号共享同一套表单参数（分组/代理/优先级/并发…）与校验。
//
// ⚠️ 这里不会兑换 web token：批量预览若对每条 web token 都发一次兑换请求，
// N 条凭证就是 N 次上游调用，而用户此刻只是想看一眼解析结果。响应里的
// token_type 字段会把 web 标出来，由前端提示用户。
func (h *AccountHandler) ImportCursorCredentials(c *gin.Context) {
	if h.cursorOAuthService == nil {
		response.BadRequest(c, "Cursor 导入服务未启用")
		return
	}

	var req CursorImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	result, err := h.cursorOAuthService.ImportCursorCredentials(&service.CursorImportInput{
		Content: req.Content,
	})
	if err != nil {
		response.BadRequest(c, "解析 Cursor 凭证失败: "+err.Error())
		return
	}
	response.Success(c, result)
}

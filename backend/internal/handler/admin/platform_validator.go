package admin

import (
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 平台白名单的单一数据源。
//
// 背景：管理端请求体原先用手写的 `binding:"oneof=anthropic openai ..."`
// 逐个列平台。新增平台时这些字符串必须一处不漏地同步，而编译器、迁移测试、
// 单元测试**都盖不到**它们——漏改的表现是运行时 400
// `Field validation for 'Platform' failed on the 'oneof' tag`，
// 请求根本到不了 service 和数据库。
//
// 实际代价：移植 cursor 时漏了 5 处，页面上建不出 cursor 分组；
// 排查时又发现 composite 路由白名单一直缺 kiro、监控模板白名单一直缺 minimax——
// 数据库 CHECK 明明放行，binding tag 先一步拒掉，等于约束白放行。
//
// 因此改为注册自定义校验 tag，平台列表统一来自 service.AllowedQuotaPlatforms：
//
//	binding:"required,platform"           // 全部可计费平台
//	binding:"omitempty,platform_or_composite" // 再额外放行 composite
//
// 新增平台今后只需改 service.AllowedQuotaPlatforms 一处。
const (
	// platformTag 只放行可计费平台，用于 provider / target_platform 这类
	// 必须指向真实上游的字段。composite 是分组层的组合概念，不是上游平台，
	// 不能作为路由目标或监控 provider（否则会自指）。
	platformTag = "platform"
	// platformOrCompositeTag 额外放行 composite，用于分组自身的 platform 字段。
	platformOrCompositeTag = "platform_or_composite"
)

func isAllowedPlatform(v string) bool {
	for _, p := range service.AllowedQuotaPlatforms {
		if v == p {
			return true
		}
	}
	return false
}

func init() {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		// gin 默认校验引擎就是 validator/v10；类型不符说明被替换过，
		// 此时静默跳过会让所有平台字段失去校验，必须显式炸掉。
		panic("admin: 无法注册平台校验器，gin 校验引擎不是 *validator.Validate")
	}
	mustRegister(v, platformTag, func(fl validator.FieldLevel) bool {
		return isAllowedPlatform(fl.Field().String())
	})
	mustRegister(v, platformOrCompositeTag, func(fl validator.FieldLevel) bool {
		s := fl.Field().String()
		return s == service.PlatformComposite || isAllowedPlatform(s)
	})
}

func mustRegister(v *validator.Validate, tag string, fn validator.Func) {
	if err := v.RegisterValidation(tag, fn); err != nil {
		panic("admin: 注册平台校验器 " + tag + " 失败: " + err.Error())
	}
}

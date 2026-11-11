//go:build unit

package admin

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// 管理端请求体的平台字段曾经各自手写 `binding:"oneof=anthropic openai ..."`。
// 新增平台必须一处不漏地同步这些字符串，而编译器、迁移测试、单元测试都盖不到——
// 漏改的表现是运行时 400
// `Field validation for 'Platform' failed on the 'oneof' tag`。
//
// 实际代价：移植 cursor 时漏了 5 处（页面上建不出 cursor 分组）；
// 排查时又发现 composite 路由白名单一直缺 kiro、监控模板白名单一直缺 minimax，
// 两者的库层 CHECK 都是放行的，等于约束白放行。
//
// 现在平台白名单统一来自平台清单（domain/platforms.go），由 platform_validation.go
// 注册成 `group_platform` / `concrete_platform` 两个 tag。这个测试锁住两件事：
//  1. 平台字段确实挂了这两个 tag（没有人退回手写 oneof）；
//  2. 注册出来的校验器对全部平台（含 kiro / cursor）放行、对未知值拒绝。

const (
	groupPlatformTag    = "group_platform"
	concretePlatformTag = "concrete_platform"
)

// platformFieldTag 返回字段 binding tag 里使用的平台校验 tag 名。
func platformFieldTag(t *testing.T, typ reflect.Type, field string) string {
	t.Helper()
	f, ok := typ.FieldByName(field)
	if !ok {
		t.Fatalf("%s 没有字段 %s", typ.Name(), field)
	}
	tag := f.Tag.Get("binding")
	for _, rule := range strings.Split(tag, ",") {
		switch rule {
		case groupPlatformTag, concretePlatformTag:
			return rule
		}
		// 退回手写 oneof 就是回到了出问题的老路，直接判失败。
		if strings.HasPrefix(rule, "oneof=") && strings.Contains(rule, "anthropic") {
			t.Fatalf("%s.%s 退回了手写 oneof 平台白名单，应改用 %q/%q tag：%s",
				typ.Name(), field, groupPlatformTag, concretePlatformTag, tag)
		}
	}
	t.Fatalf("%s.%s 的 binding tag 里没有平台校验 tag：%q", typ.Name(), field, tag)
	return ""
}

// 所有平台字段都必须挂平台校验 tag，且语义正确：
// 分组自身的 platform 允许 composite；provider / target_platform 不允许
// （composite 是分组层的组合概念，不是真实上游，不能作为路由目标或监控 provider）。
// 渠道监控 provider 的具体支持范围另由 service 层 monitorProviders 与库层 CHECK 收紧。
func TestPlatformFieldsUseSharedValidator(t *testing.T) {
	cases := []struct {
		name    string
		typ     reflect.Type
		field   string
		wantTag string
	}{
		{"CreateGroupRequest", reflect.TypeOf(CreateGroupRequest{}), "Platform", groupPlatformTag},
		{"UpdateGroupRequest", reflect.TypeOf(UpdateGroupRequest{}), "Platform", groupPlatformTag},
		{"CompositeRouteRequest", reflect.TypeOf(CompositeRouteRequest{}), "TargetPlatform", concretePlatformTag},
		{"channelMonitorCreateRequest", reflect.TypeOf(channelMonitorCreateRequest{}), "Provider", concretePlatformTag},
		{"channelMonitorUpdateRequest", reflect.TypeOf(channelMonitorUpdateRequest{}), "Provider", concretePlatformTag},
		{"channelMonitorTemplateCreateRequest", reflect.TypeOf(channelMonitorTemplateCreateRequest{}), "Provider", concretePlatformTag},
	}
	for _, c := range cases {
		t.Run(c.name+"."+c.field, func(t *testing.T) {
			if got := platformFieldTag(t, c.typ, c.field); got != c.wantTag {
				t.Errorf("%s.%s 用了 %q，期望 %q", c.name, c.field, got, c.wantTag)
			}
		})
	}
}

// 校验器本身：全部已登记平台放行，composite 只在 group_platform 下放行，
// 未知值一律拒绝。
func TestPlatformValidatorAcceptsEveryPlatform(t *testing.T) {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		t.Fatal("gin 校验引擎不是 *validator.Validate")
	}
	type groupReq struct {
		Platform string `binding:"group_platform"`
	}
	type concreteReq struct {
		Platform string `binding:"concrete_platform"`
	}

	if len(service.AllowedQuotaPlatforms) == 0 {
		t.Fatal("AllowedQuotaPlatforms 为空，平台校验会拒绝一切")
	}
	for _, p := range []string{service.PlatformKiro, service.PlatformCursor} {
		if !domain.IsConcretePlatform(p) {
			t.Fatalf("平台 %q 未登记到平台清单", p)
		}
	}
	for _, p := range service.AllowedQuotaPlatforms {
		if err := v.Struct(concreteReq{Platform: p}); err != nil {
			t.Errorf("平台 %q 未被 concrete_platform 放行，其请求会在参数绑定阶段被拒: %v", p, err)
		}
		if err := v.Struct(groupReq{Platform: p}); err != nil {
			t.Errorf("平台 %q 未被 group_platform 放行: %v", p, err)
		}
	}

	if err := v.Struct(groupReq{Platform: service.PlatformComposite}); err != nil {
		t.Errorf("composite 应被 group_platform 放行: %v", err)
	}
	// composite 不是真实上游：不能作为 provider / target_platform。
	if v.Struct(concreteReq{Platform: service.PlatformComposite}) == nil {
		t.Error("composite 不应被 concrete_platform 放行——它不是真实上游，不能作为路由目标或监控 provider")
	}

	// 反向：未知平台必须被拒绝，否则等于没有校验。
	for _, bad := range []string{"not_a_platform", "CURSOR", "cursor ", "Kiro"} {
		if v.Struct(concreteReq{Platform: bad}) == nil {
			t.Errorf("非法平台值 %q 竟然通过 concrete_platform 校验", bad)
		}
		if v.Struct(groupReq{Platform: bad}) == nil {
			t.Errorf("非法平台值 %q 竟然通过 group_platform 校验", bad)
		}
	}
}

package remoteproxy

import "testing"

// 本端入口是 /api/v1/remote-admin/*，目标端只认 /api/v1/admin/*。
// 改写必须成段匹配：同前缀的其它路径、以及路径中部出现的同名片段都要原样保留，
// 否则会把请求转发到目标端不存在的地址。
func TestRewriteAdminPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "入口前缀换回目标端前缀",
			in:   "/api/v1/remote-admin/users",
			want: "/api/v1/admin/users",
		},
		{
			name: "多级路径整体保留",
			in:   "/api/v1/remote-admin/accounts/42/test",
			want: "/api/v1/admin/accounts/42/test",
		},
		{
			name: "前缀本身无尾随路径",
			in:   "/api/v1/remote-admin",
			want: "/api/v1/admin",
		},
		{
			name: "本地管理面路径不被改写",
			in:   "/api/v1/admin/users",
			want: "/api/v1/admin/users",
		},
		{
			name: "同前缀的其它路径不被改写",
			in:   "/api/v1/remote-admins",
			want: "/api/v1/remote-admins",
		},
		{
			name: "路径中部的同名片段不被改写",
			in:   "/api/v1/admin/settings/remote-admin",
			want: "/api/v1/admin/settings/remote-admin",
		},
		{
			name: "非 api/v1 前缀不被改写",
			in:   "/remote-admin/users",
			want: "/remote-admin/users",
		},
		{
			name: "无关路径原样返回",
			in:   "/api/v1/auth/login",
			want: "/api/v1/auth/login",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteAdminPath(tc.in); got != tc.want {
				t.Errorf("rewriteAdminPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

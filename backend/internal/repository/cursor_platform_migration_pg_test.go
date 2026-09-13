//go:build pgverify

// 针对已有本地 Postgres 实例验证 cursor 平台迁移（239）。
//
// 与 integration_harness_test.go 的区别：那套用 testcontainers 每次新起
// Postgres/Redis 容器；这里复用已在跑的实例，通过 SUB2API_PGVERIFY_DSN 指定。
// 单独的 build tag（pgverify）保证它不会混进默认 `go test ./...`。
//
// 用法：
//
//	SUB2API_PGVERIFY_DSN='postgres://user:pass@127.0.0.1:25432/db?sslmode=disable' \
//	  go test -tags pgverify ./internal/repository/ -run TestCursorPlatform -v
package repository

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func pgverifyDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SUB2API_PGVERIFY_DSN"))
	if dsn == "" {
		t.Skip("SUB2API_PGVERIFY_DSN 未设置，跳过（此测试复用已有 Postgres 实例）")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 跑真实的迁移运行器（含 checksum 校验、排序、advisory lock），
// 而不是手工 psql 灌 SQL —— 后者验不到运行器本身。
func TestCursorPlatformMigrationAppliesOnRealPostgres(t *testing.T) {
	db := pgverifyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := ApplyMigrations(ctx, db); err != nil {
		t.Fatalf("ApplyMigrations 失败: %v", err)
	}

	// 239 必须被记录为已应用。
	var applied int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE filename = '239_add_cursor_platform.sql'`,
	).Scan(&applied); err != nil {
		t.Fatalf("查询 schema_migrations: %v", err)
	}
	if applied != 1 {
		t.Fatalf("239 未被记录为已应用, count=%d", applied)
	}

	// 幂等：再跑一次不能报错（生产每次启动都会调用）。
	if err := ApplyMigrations(ctx, db); err != nil {
		t.Fatalf("重复 ApplyMigrations 应当幂等，却失败: %v", err)
	}
}

// 四处 CHECK 约束都要真的接受 'cursor'。
// 迁移注释里点明的正是这个：Go 侧编译期检查盖不到 SQL 字符串里的白名单，
// 单测全绿也挡不住，只有真连 Postgres 才能验。
func TestCursorPassesAllPlatformCheckConstraints(t *testing.T) {
	db := pgverifyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := ApplyMigrations(ctx, db); err != nil {
		t.Fatalf("ApplyMigrations 失败: %v", err)
	}

	constraints := []struct {
		name  string
		table string
		check string
	}{
		{"user_platform_quotas.platform", "user_platform_quotas", "user_platform_quotas_platform_check"},
		{"composite_model_routes.target_platform", "composite_model_routes", "composite_model_routes_target_platform_check"},
		{"channel_monitors.provider", "channel_monitors", "channel_monitors_provider_check"},
		{"channel_monitor_request_templates.provider", "channel_monitor_request_templates", "channel_monitor_request_templates_provider_check"},
	}

	for _, c := range constraints {
		t.Run(c.name, func(t *testing.T) {
			var def string
			err := db.QueryRowContext(ctx, `
				SELECT pg_get_constraintdef(c.oid)
				  FROM pg_constraint c
				  JOIN pg_class t ON t.oid = c.conrelid
				 WHERE t.relname = $1 AND c.conname = $2`,
				c.table, c.check,
			).Scan(&def)
			if err == sql.ErrNoRows {
				t.Fatalf("约束 %s 不存在（表 %s）", c.check, c.table)
			}
			if err != nil {
				t.Fatalf("查询约束定义: %v", err)
			}
			if !strings.Contains(def, "'cursor'") {
				t.Errorf("约束未放行 cursor:\n%s", def)
			}
		})
	}
}

// 约束定义里有 'cursor' 只说明文本对了，不代表真能写进去。
// 这里做真实 INSERT，覆盖迁移注释描述的三个故障场景。
func TestCursorRowsActuallyInsertable(t *testing.T) {
	db := pgverifyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := ApplyMigrations(ctx, db); err != nil {
		t.Fatalf("ApplyMigrations 失败: %v", err)
	}

	// 全部在事务里做并回滚，不给复用的库留垃圾数据。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ⚠️ 每个子用例必须包在 SAVEPOINT 里。
	// Postgres 一旦有语句报错，整个事务进入 aborted 状态，后续所有语句都会
	// 回同一个 "current transaction is aborted"。那会让「未知平台被拒绝」
	// 这类反向断言在约束根本没被触达的情况下假通过。
	inSavepoint := func(t *testing.T, name string, fn func(t *testing.T)) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		defer func() {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name)
		}()
		fn(t)
	}

	// 场景 1：注册预填充默认配额时 cursor 行违约会中止整条 INSERT。
	t.Run("user_platform_quotas", func(t *testing.T) {
		inSavepoint(t, "sp_quota", func(t *testing.T) {
			var uid int64
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO users (username, email, password_hash, role, status, created_at, updated_at)
				VALUES ('cursor_mig_probe', 'cursor_mig_probe@example.invalid', 'x', 'user', 'active', now(), now())
				RETURNING id`).Scan(&uid); err != nil {
				t.Fatalf("建测试用户失败: %v", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO user_platform_quotas (user_id, platform, created_at, updated_at)
				VALUES ($1, 'cursor', now(), now())`, uid); err != nil {
				t.Fatalf("cursor 配额行应可插入: %v", err)
			}
		})
	})

	// 场景 2：cursor 的 composite 路由服务层放行、INSERT 被 Postgres 拒绝，
	// 管理台只看到莫名的 500。
	t.Run("composite_model_routes", func(t *testing.T) {
		inSavepoint(t, "sp_route", func(t *testing.T) {
			// group_id 有外键，必须先建一个真实分组。
			var gid int64
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO groups (name, created_at, updated_at)
				VALUES ('cursor-mig-probe-group', now(), now())
				RETURNING id`).Scan(&gid); err != nil {
				t.Fatalf("建测试分组失败: %v", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO composite_model_routes (group_id, public_model, target_platform, created_at, updated_at)
				VALUES ($1, 'cursor-mig-probe', 'cursor', now(), now())`, gid); err != nil {
				t.Fatalf("cursor composite 路由应可插入: %v", err)
			}
		})
	})

	// 场景 3：cursor 渠道监控无法创建。
	t.Run("channel_monitors", func(t *testing.T) {
		inSavepoint(t, "sp_monitor", func(t *testing.T) {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO channel_monitors
					(name, provider, endpoint, api_key_encrypted, primary_model,
					 interval_seconds, created_by, created_at, updated_at)
				VALUES ('cursor-mig-probe', 'cursor', '', '', 'claude-4.5-sonnet',
					 60, 0, now(), now())`); err != nil {
				t.Fatalf("cursor 渠道监控应可插入: %v", err)
			}
		})
	})

	// 反向验证：不在白名单的平台必须仍被拒绝，
	// 否则说明约束被改成了放行一切，等于没有约束。
	// 这里显式要求错误来自 provider_check，避免「因别的原因失败」也算通过。
	// 注意 provider 是 varchar(20)，非法值要足够短，否则先撞长度限制、
	// 根本触达不到 CHECK，反向断言就失去意义。
	t.Run("rejects_unknown_platform", func(t *testing.T) {
		inSavepoint(t, "sp_bad", func(t *testing.T) {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO channel_monitors
					(name, provider, endpoint, api_key_encrypted, primary_model,
					 interval_seconds, created_by, created_at, updated_at)
				VALUES ('cursor-mig-probe-bad', 'not_a_platform', '', '', 'm',
					 60, 0, now(), now())`)
			if err == nil {
				t.Fatal("未知平台竟然插入成功——约束失效了")
			}
			if !strings.Contains(err.Error(), "provider_check") {
				t.Fatalf("期望被 provider_check 拒绝，实际错误是: %v", err)
			}
		})
	})
}

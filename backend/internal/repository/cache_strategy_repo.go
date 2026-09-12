package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/cachestrategy"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type cacheStrategyRepository struct {
	client *dbent.Client
	sql    *sql.DB
}

func NewCacheStrategyRepository(client *dbent.Client, sqlDB *sql.DB) service.CacheStrategyRepository {
	return &cacheStrategyRepository{client: client, sql: sqlDB}
}

func (r *cacheStrategyRepository) Create(ctx context.Context, in *service.CacheStrategy) error {
	if in == nil {
		return fmt.Errorf("cache strategy is nil")
	}
	node, err := r.client.CacheStrategy.Create().
		SetName(strings.TrimSpace(in.Name)).
		SetDescription(in.Description).
		SetEnabled(in.Enabled).
		SetRevision(in.Revision).
		SetConfig(service.CacheStrategyConfigMap(in.Config)).
		Save(ctx)
	if err != nil {
		return err
	}
	copyCacheStrategy(node, in)
	return nil
}

func (r *cacheStrategyRepository) GetByID(ctx context.Context, id int64) (*service.CacheStrategy, error) {
	node, err := r.client.CacheStrategy.Query().Where(cachestrategy.IDEQ(id)).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &service.CacheStrategy{}
	copyCacheStrategy(node, out)
	return out, nil
}

func (r *cacheStrategyRepository) List(ctx context.Context, search string) ([]service.CacheStrategy, error) {
	q := r.client.CacheStrategy.Query().Order(dbent.Asc(cachestrategy.FieldID))
	if strings.TrimSpace(search) != "" {
		q = q.Where(cachestrategy.Or(
			cachestrategy.NameContainsFold(strings.TrimSpace(search)),
			cachestrategy.DescriptionContainsFold(strings.TrimSpace(search)),
		))
	}
	nodes, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.CacheStrategy, 0, len(nodes))
	for _, node := range nodes {
		var item service.CacheStrategy
		copyCacheStrategy(node, &item)
		out = append(out, item)
	}
	return out, nil
}

func (r *cacheStrategyRepository) Update(ctx context.Context, in *service.CacheStrategy) error {
	if in == nil {
		return fmt.Errorf("cache strategy is nil")
	}
	node, err := r.client.CacheStrategy.UpdateOneID(in.ID).
		SetName(strings.TrimSpace(in.Name)).
		SetDescription(in.Description).
		SetEnabled(in.Enabled).
		SetRevision(in.Revision).
		SetConfig(service.CacheStrategyConfigMap(in.Config)).
		Save(ctx)
	if err != nil {
		return err
	}
	copyCacheStrategy(node, in)
	return nil
}

func (r *cacheStrategyRepository) UpdateIfRevision(ctx context.Context, in *service.CacheStrategy, expectedRevision int64) (bool, error) {
	if in == nil {
		return false, fmt.Errorf("cache strategy is nil")
	}
	builder := r.client.CacheStrategy.UpdateOneID(in.ID).
		Where(cachestrategy.RevisionEQ(expectedRevision)).
		SetName(strings.TrimSpace(in.Name)).
		SetDescription(in.Description).
		SetEnabled(in.Enabled).
		SetRevision(in.Revision).
		SetConfig(service.CacheStrategyConfigMap(in.Config))
	if _, err := builder.Save(ctx); err != nil {
		if dbent.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	node, err := r.client.CacheStrategy.Query().Where(cachestrategy.IDEQ(in.ID)).Only(ctx)
	if err != nil {
		return false, err
	}
	copyCacheStrategy(node, in)
	return true, nil
}

func (r *cacheStrategyRepository) Delete(ctx context.Context, id int64) error {
	return r.client.CacheStrategy.DeleteOneID(id).Exec(ctx)
}

func (r *cacheStrategyRepository) CountBoundGroups(ctx context.Context, id int64) (int, error) {
	return r.client.Group.Query().Where(group.CacheStrategyIDEQ(id)).Count(ctx)
}

func (r *cacheStrategyRepository) ListBoundGroups(ctx context.Context, id int64) ([]service.Group, error) {
	rows, err := r.client.Group.Query().Where(group.CacheStrategyIDEQ(id)).Order(dbent.Asc(group.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, *groupEntityToService(row))
	}
	return out, nil
}

func (r *cacheStrategyRepository) GetGroupBindings(ctx context.Context, groupIDs []int64) ([]service.Group, error) {
	ids := service.SortCacheStrategyIDs(groupIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.client.Group.Query().
		Where(group.IDIn(ids...), group.DeletedAtIsNil()).
		Order(dbent.Asc(group.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, *groupEntityToService(row))
	}
	return out, nil
}

func (r *cacheStrategyRepository) SetGroupBindings(ctx context.Context, id int64, groupIDs []int64) error {
	return r.setGroupBindings(ctx, id, groupIDs, false)
}

func (r *cacheStrategyRepository) ReplaceGroupBindings(ctx context.Context, id int64, groupIDs []int64) error {
	return r.setGroupBindings(ctx, id, groupIDs, true)
}

func (r *cacheStrategyRepository) setGroupBindings(ctx context.Context, id int64, groupIDs []int64, allowExistingOwner bool) error {
	if r.sql == nil {
		return fmt.Errorf("sql database is unavailable")
	}
	tx, err := r.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var strategyExists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM cache_strategies WHERE id = $1)", id).Scan(&strategyExists); err != nil {
		return err
	}
	if !strategyExists {
		return fmt.Errorf("cache strategy not found")
	}

	// Lock target rows and reject any existing ownership before mutating
	// bindings. This is the race-safe backstop for the service preflight.
	ids := service.SortCacheStrategyIDs(groupIDs)
	if len(ids) > 0 {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, name, cache_strategy_id
			FROM groups
			WHERE id = ANY($1) AND deleted_at IS NULL
			ORDER BY id
			FOR UPDATE`, pq.Array(ids))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		found := make(map[int64]bool, len(ids))
		for rows.Next() {
			var (
				groupID         int64
				groupName       string
				currentStrategy sql.NullInt64
			)
			if err := rows.Scan(&groupID, &groupName, &currentStrategy); err != nil {
				return err
			}
			found[groupID] = true
			if currentStrategy.Valid && currentStrategy.Int64 > 0 &&
				(!allowExistingOwner || currentStrategy.Int64 != id) {
				group := service.Group{
					ID:   groupID,
					Name: groupName,
					CacheStrategyID: func() *int64 {
						value := currentStrategy.Int64
						return &value
					}(),
				}
				return service.CacheStrategyGroupConflictForRepository(group, id)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, groupID := range ids {
			if groupID > 0 && !found[groupID] {
				return fmt.Errorf("group %d not found", groupID)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, "UPDATE groups SET cache_strategy_id = NULL WHERE cache_strategy_id = $1", id); err != nil {
		return err
	}
	for _, groupID := range service.SortCacheStrategyIDs(groupIDs) {
		if groupID <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET cache_strategy_id = $1 WHERE id = $2", id, groupID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func copyCacheStrategy(node *dbent.CacheStrategy, out *service.CacheStrategy) {
	if node == nil || out == nil {
		return
	}
	out.ID = node.ID
	out.Name = node.Name
	out.Description = node.Description
	out.Enabled = node.Enabled
	out.Revision = node.Revision
	out.CreatedAt = node.CreatedAt
	out.UpdatedAt = node.UpdatedAt
	b, _ := json.Marshal(node.Config)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if parsed, err := service.ParseCacheStrategyConfig(raw); err == nil {
		out.Config = parsed
	} else {
		out.Config = service.DefaultCacheStrategyConfig(service.CacheStrategyKindPrefix)
	}
}

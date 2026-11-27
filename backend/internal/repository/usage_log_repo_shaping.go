package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *usageLogRepository) GetShapingStats(ctx context.Context, q service.UsageShapingQuery) (*service.UsageShapingStats, error) {
	column, table := "account_id", "accounts"
	if q.Dimension == "group" {
		column, table = "group_id", "groups"
	} else if q.Dimension != "account" {
		return nil, fmt.Errorf("invalid shaping stats dimension")
	}
	if q.Granularity != "day" && q.Granularity != "hour" {
		return nil, fmt.Errorf("invalid shaping stats granularity")
	}
	// Truncate absolute hours to retain both occurrences of an hour at a DST fallback.
	bucket := "date_trunc('day', u.created_at AT TIME ZONE $3) AT TIME ZONE $3"
	if q.Granularity == "hour" {
		bucket = "date_bin('1 hour', u.created_at, $1::timestamptz)"
	}
	query := fmt.Sprintf(`WITH source AS (
 SELECT u.%s AS entity_id, COALESCE(e.name, '') AS name, %s AS bucket,
 u.raw_input_tokens::bigint + u.raw_output_tokens + u.raw_cache_read_tokens + u.raw_cache_creation_tokens AS before_tokens,
 u.input_tokens::bigint + u.output_tokens + u.cache_read_tokens + u.cache_creation_tokens AS after_tokens,
 u.raw_total_cost, u.total_cost,
 (u.raw_total_cost IS NOT NULL AND u.raw_input_tokens IS NOT NULL AND u.raw_output_tokens IS NOT NULL AND u.raw_cache_read_tokens IS NOT NULL AND u.raw_cache_creation_tokens IS NOT NULL) AS comparable
 FROM usage_logs u LEFT JOIN %s e ON e.id = u.%s
 WHERE u.created_at >= $1 AND u.created_at < $2
 ) SELECT entity_id, name, bucket, COUNT(*), COUNT(*) FILTER (WHERE comparable),
 COALESCE(SUM(before_tokens) FILTER (WHERE comparable),0),
 COALESCE(SUM(after_tokens) FILTER (WHERE comparable),0),
 COALESCE(SUM(raw_total_cost) FILTER (WHERE comparable),0),
 COALESCE(SUM(total_cost) FILTER (WHERE comparable),0)
 FROM source GROUP BY entity_id, name, bucket ORDER BY bucket, entity_id NULLS LAST`, column, bucket, table, column)
	args := []any{q.Start, q.End}
	if q.Granularity == "day" {
		args = append(args, q.Timezone)
	}
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := &service.UsageShapingStats{Breakdown: make([]service.UsageShapingStat, 0), Series: make([]service.UsageShapingPoint, 0)}
	byID := make(map[sql.NullInt64]int)
	for rows.Next() {
		var point service.UsageShapingPoint
		var name string
		if err := rows.Scan(&point.ID, &name, &point.Bucket, &point.Requests, &point.ComparableRequests, &point.BeforeTokens, &point.AfterTokens, &point.BeforeCost, &point.AfterCost); err != nil {
			return nil, err
		}
		result.Series = append(result.Series, point)
		id := nullInt64(point.ID)
		idx, exists := byID[id]
		if !exists {
			idx = len(result.Breakdown)
			byID[id] = idx
			result.Breakdown = append(result.Breakdown, service.UsageShapingStat{ID: point.ID, Name: name})
		}
		result.Breakdown[idx].Add(point.UsageShapingMetrics)
		result.Summary.Add(point.UsageShapingMetrics)
	}
	return result, rows.Err()
}

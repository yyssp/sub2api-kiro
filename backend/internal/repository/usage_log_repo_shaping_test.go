package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageShapingStatsDimensionsAndNullableGroup(t *testing.T) {
	for _, dimension := range []string{"account", "group"} {
		t.Run(dimension, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			repo := &usageLogRepository{sql: db}
			start := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 0, 2)
			columns := []string{"id", "name", "bucket", "requests", "comparable", "before_tokens", "after_tokens", "before_cost", "after_cost"}
			mock.ExpectQuery(`(?s)WITH source AS.*u\.`+dimension+`_id.*u.raw_total_cost IS NOT NULL.*GROUP BY entity_id, name, bucket`).WithArgs(start, end, "UTC").WillReturnRows(sqlmock.NewRows(columns).
				AddRow(nil, "", start, 3, 2, 320, 260, .2, .3).
				AddRow(nil, "", start.AddDate(0, 0, 1), 1, 1, 100, 200, .1, .2).
				AddRow(7, "active", start, 1, 0, 0, 0, 0, 0))
			stats, err := repo.GetShapingStats(context.Background(), service.UsageShapingQuery{Start: start, End: end, Dimension: dimension, Granularity: "day", Timezone: "UTC"})
			require.NoError(t, err)
			require.Len(t, stats.Breakdown, 2)
			require.Len(t, stats.Series, 3)
			require.Nil(t, stats.Breakdown[0].ID)
			require.EqualValues(t, 5, stats.Summary.Requests)
			require.EqualValues(t, 3, stats.Summary.ComparableRequests)
			require.EqualValues(t, 420, stats.Summary.BeforeTokens)
			require.EqualValues(t, 460, stats.Summary.AfterTokens)
			require.InDelta(t, .3, stats.Summary.BeforeCost, 1e-10)
			require.InDelta(t, .5, stats.Summary.AfterCost, 1e-10)
			require.Equal(t, stats.Summary.ComparableRequests, stats.Breakdown[0].ComparableRequests)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRawUsageInsertNullableValuesAndOrder(t *testing.T) {
	log := &service.UsageLog{Model: "claude-sonnet-4-5"}
	prepared := prepareUsageLogInsert(log)
	for i := len(prepared.args) - 6; i < len(prepared.args)-2; i++ {
		require.Equal(t, sql.NullInt64{}, prepared.args[i])
	}
	require.Nil(t, prepared.args[len(prepared.args)-2])
	input, output, read, write := 100, 20, 30, 0
	cost := .125
	log.RawInputTokens, log.RawOutputTokens, log.RawCacheReadTokens, log.RawCacheCreationTokens = &input, &output, &read, &write
	log.RawTotalCost = &cost
	prepared = prepareUsageLogInsert(log)
	for i, want := range []int{input, output, read, write} {
		require.Equal(t, sql.NullInt64{Int64: int64(want), Valid: true}, prepared.args[len(prepared.args)-6+i])
	}
	require.Equal(t, &cost, prepared.args[len(prepared.args)-2])
	require.Nil(t, nullableRawToken(sql.NullInt64{}))
	require.Nil(t, nullableRawCost(sql.NullFloat64{}))
	require.Equal(t, &write, nullableRawToken(sql.NullInt64{Valid: true}))
}

package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateGroupRequestLimitFieldsTriState(t *testing.T) {
	t.Run("omitted means unchanged", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{}`), &req))
		require.Nil(t, req.DailyLimitUSD.ToServiceInput())
		require.Nil(t, req.WeeklyLimitUSD.ToServiceInput())
		require.Nil(t, req.MonthlyLimitUSD.ToServiceInput())
	})

	t.Run("null means unlimited", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{"daily_limit_usd":null}`), &req))
		limit := req.DailyLimitUSD.ToServiceInput()
		require.NotNil(t, limit)
		require.Negative(t, *limit)
	})

	t.Run("number is preserved", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{"weekly_limit_usd":0,"monthly_limit_usd":42.5}`), &req))
		require.Equal(t, 0.0, *req.WeeklyLimitUSD.ToServiceInput())
		require.Equal(t, 42.5, *req.MonthlyLimitUSD.ToServiceInput())
	})
}

func TestUpdateGroupRequestCacheStrategyIDTriState(t *testing.T) {
	t.Run("omitted means unchanged", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{}`), &req))
		require.False(t, req.CacheStrategyID.IsSet())
		require.Nil(t, req.CacheStrategyID.ToServiceInput())
	})

	t.Run("null means explicit unbind", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{"cache_strategy_id":null}`), &req))
		require.True(t, req.CacheStrategyID.IsSet())
		require.Nil(t, req.CacheStrategyID.ToServiceInput())
	})

	t.Run("positive number means bind", func(t *testing.T) {
		var req UpdateGroupRequest
		require.NoError(t, json.Unmarshal([]byte(`{"cache_strategy_id":42}`), &req))
		require.True(t, req.CacheStrategyID.IsSet())
		require.NotNil(t, req.CacheStrategyID.ToServiceInput())
		require.Equal(t, int64(42), *req.CacheStrategyID.ToServiceInput())
	})
}

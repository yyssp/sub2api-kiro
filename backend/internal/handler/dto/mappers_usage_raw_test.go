package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRawUsageIsAdminOnlyAndPreservesNull(t *testing.T) {
	zero, input := 0, 100
	cost := .125
	log := &service.UsageLog{RawTotalCost: &cost, RawInputTokens: &input, RawOutputTokens: &zero, RawCacheReadTokens: &zero, RawCacheCreationTokens: &zero}
	adminJSON, err := json.Marshal(UsageLogFromServiceAdmin(log))
	require.NoError(t, err)
	userJSON, err := json.Marshal(UsageLogFromService(log))
	require.NoError(t, err)
	require.Contains(t, string(adminJSON), `"raw_input_tokens":100`)
	require.Contains(t, string(adminJSON), `"raw_cache_creation_tokens":0`)
	require.NotContains(t, string(userJSON), `"raw_`)
	require.Contains(t, string(adminJSON), `"raw_total_cost":0.125`)
	adminJSON, err = json.Marshal(UsageLogFromServiceAdmin(&service.UsageLog{}))
	require.NoError(t, err)
	require.Contains(t, string(adminJSON), `"raw_input_tokens":null`)
}

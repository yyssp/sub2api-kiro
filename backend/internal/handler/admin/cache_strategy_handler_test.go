package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cacheStrategyHandlerRepoStub struct {
	boundGroups []service.Group
	setGroupIDs []int64
}

func (s *cacheStrategyHandlerRepoStub) Create(context.Context, *service.CacheStrategy) error {
	panic("unexpected Create call")
}

func (s *cacheStrategyHandlerRepoStub) GetByID(context.Context, int64) (*service.CacheStrategy, error) {
	panic("unexpected GetByID call")
}

func (s *cacheStrategyHandlerRepoStub) List(context.Context, string) ([]service.CacheStrategy, error) {
	panic("unexpected List call")
}

func (s *cacheStrategyHandlerRepoStub) Update(context.Context, *service.CacheStrategy) error {
	panic("unexpected Update call")
}

func (s *cacheStrategyHandlerRepoStub) Delete(context.Context, int64) error {
	panic("unexpected Delete call")
}

func (s *cacheStrategyHandlerRepoStub) CountBoundGroups(context.Context, int64) (int, error) {
	return 0, nil
}

func (s *cacheStrategyHandlerRepoStub) ListBoundGroups(context.Context, int64) ([]service.Group, error) {
	out := make([]service.Group, len(s.boundGroups))
	copy(out, s.boundGroups)
	return out, nil
}

func (s *cacheStrategyHandlerRepoStub) GetGroupBindings(_ context.Context, groupIDs []int64) ([]service.Group, error) {
	byID := make(map[int64]service.Group, len(s.boundGroups))
	for _, group := range s.boundGroups {
		byID[group.ID] = group
	}
	out := make([]service.Group, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		if group, ok := byID[groupID]; ok {
			out = append(out, group)
		}
	}
	return out, nil
}

func (s *cacheStrategyHandlerRepoStub) SetGroupBindings(_ context.Context, _ int64, groupIDs []int64) error {
	s.setGroupIDs = append(s.setGroupIDs[:0], groupIDs...)
	return nil
}

func (s *cacheStrategyHandlerRepoStub) ReplaceGroupBindings(_ context.Context, _ int64, groupIDs []int64) error {
	s.setGroupIDs = append(s.setGroupIDs[:0], groupIDs...)
	return nil
}

func TestCacheStrategyHandlerBindGroupsReturnsStructuredConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &cacheStrategyHandlerRepoStub{
		boundGroups: []service.Group{
			{ID: 41, Name: "gamma", CacheStrategyID: int64Ptr(7001)},
		},
	}
	handler := NewCacheStrategyHandler(service.NewCacheStrategyService(repo))

	body := []byte(`{"group_ids":[41]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/cache-strategies/7002/groups", bytes.NewReader(body))
	c.Params = gin.Params{{Key: "id", Value: "7002"}}

	handler.BindGroups(c)

	require.Equal(t, http.StatusConflict, rec.Code)
	var envelope response.Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, http.StatusConflict, envelope.Code)
	require.Equal(t, "CACHE_STRATEGY_GROUP_CONFLICT", envelope.Reason)
	require.Contains(t, envelope.Message, "group is already bound to a cache strategy")
	require.Equal(t, map[string]string{
		"group_id":              "41",
		"group_name":            "gamma",
		"current_strategy_id":   "7001",
		"requested_strategy_id": "7002",
		"reason":                "group is already bound to another cache strategy; unbind it before rebinding",
	}, envelope.Metadata)
	require.Empty(t, repo.setGroupIDs)
}

func int64Ptr(v int64) *int64 { return &v }

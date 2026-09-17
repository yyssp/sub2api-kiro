//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func testCacheStrategy(id int64) *CacheStrategy {
	return &CacheStrategy{
		ID:      id,
		Name:    "strategy",
		Enabled: true,
		Config:  DefaultCacheStrategyConfig(CacheStrategyKindPrefix),
	}
}

func TestAdminServiceCreateGroupBindsExistingCacheStrategy(t *testing.T) {
	const strategyID int64 = 501
	groupRepo := &groupRepoStubForAdmin{createID: 801}
	strategyRepo := &cacheStrategyRepoStub{
		strategies: map[int64]*CacheStrategy{
			strategyID: testCacheStrategy(strategyID),
		},
	}
	svc := &adminServiceImpl{
		groupRepo:           groupRepo,
		cacheStrategyLookup: strategyRepo,
	}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:            "bound-group",
		Platform:        PlatformAnthropic,
		RateMultiplier:  1,
		CacheStrategyID: int64PtrForAdminGroupCacheTest(strategyID),
	})

	require.NoError(t, err)
	require.Same(t, groupRepo.created, group)
	require.NotNil(t, group.CacheStrategyID)
	require.Equal(t, strategyID, *group.CacheStrategyID)
}

func TestAdminServiceCreateGroupRejectsMissingCacheStrategy(t *testing.T) {
	const strategyID int64 = 502
	groupRepo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{
		groupRepo:           groupRepo,
		cacheStrategyLookup: &cacheStrategyRepoStub{strategies: map[int64]*CacheStrategy{}},
	}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:            "missing-strategy",
		Platform:        PlatformAnthropic,
		RateMultiplier:  1,
		CacheStrategyID: int64PtrForAdminGroupCacheTest(strategyID),
	})

	require.Nil(t, group)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, infraerrors.Code(err))
	require.Equal(t, "CACHE_STRATEGY_NOT_FOUND", infraerrors.Reason(err))
	require.Equal(t, "502", infraerrors.FromError(err).Metadata["cache_strategy_id"])
	require.Nil(t, groupRepo.created)
}

func TestAdminServiceUpdateGroupCacheStrategyTriStateAndConflictProtection(t *testing.T) {
	const (
		groupID           int64 = 901
		currentStrategyID int64 = 601
		otherStrategyID   int64 = 602
	)

	t.Run("omitted preserves existing binding", func(t *testing.T) {
		current := int64PtrForAdminGroupCacheTest(currentStrategyID)
		groupRepo := &groupRepoStubForAdmin{
			getByID: &Group{ID: groupID, Name: "existing", Platform: PlatformAnthropic, Status: StatusActive, CacheStrategyID: current},
		}
		svc := &adminServiceImpl{groupRepo: groupRepo}

		updated, err := svc.UpdateGroup(context.Background(), groupID, &UpdateGroupInput{})

		require.NoError(t, err)
		require.Same(t, groupRepo.updated, updated)
		require.NotNil(t, groupRepo.updated.CacheStrategyID)
		require.Equal(t, currentStrategyID, *groupRepo.updated.CacheStrategyID)
	})

	t.Run("explicit null clears existing binding", func(t *testing.T) {
		groupRepo := &groupRepoStubForAdmin{
			getByID: &Group{
				ID:              groupID,
				Name:            "existing",
				Platform:        PlatformAnthropic,
				Status:          StatusActive,
				CacheStrategyID: int64PtrForAdminGroupCacheTest(currentStrategyID),
			},
		}
		svc := &adminServiceImpl{groupRepo: groupRepo}

		updated, err := svc.UpdateGroup(context.Background(), groupID, &UpdateGroupInput{
			CacheStrategyIDSet: true,
			CacheStrategyID:    nil,
		})

		require.NoError(t, err)
		require.Same(t, groupRepo.updated, updated)
		require.Nil(t, groupRepo.updated.CacheStrategyID)
	})

	t.Run("different strategy is rejected without repository update", func(t *testing.T) {
		groupRepo := &groupRepoStubForAdmin{
			getByID: &Group{
				ID:              groupID,
				Name:            "existing",
				Platform:        PlatformAnthropic,
				Status:          StatusActive,
				CacheStrategyID: int64PtrForAdminGroupCacheTest(currentStrategyID),
			},
		}
		strategyRepo := &cacheStrategyRepoStub{
			strategies: map[int64]*CacheStrategy{
				otherStrategyID: testCacheStrategy(otherStrategyID),
			},
		}
		svc := &adminServiceImpl{
			groupRepo:           groupRepo,
			cacheStrategyLookup: strategyRepo,
		}

		updated, err := svc.UpdateGroup(context.Background(), groupID, &UpdateGroupInput{
			CacheStrategyIDSet: true,
			CacheStrategyID:    int64PtrForAdminGroupCacheTest(otherStrategyID),
		})

		require.Nil(t, updated)
		require.Error(t, err)
		require.Equal(t, http.StatusConflict, infraerrors.Code(err))
		require.Equal(t, "CACHE_STRATEGY_GROUP_CONFLICT", infraerrors.Reason(err))
		metadata := infraerrors.FromError(err).Metadata
		require.Equal(t, "901", metadata["group_id"])
		require.Equal(t, "601", metadata["current_strategy_id"])
		require.Equal(t, "602", metadata["requested_strategy_id"])
		require.Nil(t, groupRepo.updated)
	})
}

func int64PtrForAdminGroupCacheTest(value int64) *int64 {
	return &value
}

package service

import (
	"context"
	"fmt"
	"sort"
	"time"
)

type UsageShapingMetrics struct {
	Requests           int64   `json:"requests"`
	ComparableRequests int64   `json:"comparable_requests"`
	BeforeTokens       int64   `json:"before_tokens"`
	AfterTokens        int64   `json:"after_tokens"`
	BeforeCost         float64 `json:"before_cost"`
	AfterCost          float64 `json:"after_cost"`
}

func (m *UsageShapingMetrics) Add(other UsageShapingMetrics) {
	m.Requests += other.Requests
	m.ComparableRequests += other.ComparableRequests
	m.BeforeTokens += other.BeforeTokens
	m.AfterTokens += other.AfterTokens
	m.BeforeCost += other.BeforeCost
	m.AfterCost += other.AfterCost
}

type UsageShapingStat struct {
	ID   *int64 `json:"id"`
	Name string `json:"name"`
	UsageShapingMetrics
}

type UsageShapingPoint struct {
	ID     *int64    `json:"id"`
	Bucket time.Time `json:"bucket"`
	UsageShapingMetrics
}

type UsageShapingStats struct {
	Summary     UsageShapingMetrics `json:"summary"`
	Breakdown   []UsageShapingStat  `json:"breakdown"`
	Series      []UsageShapingPoint `json:"series"`
	Buckets     []time.Time         `json:"buckets"`
	Granularity string              `json:"granularity"`
}

type UsageShapingQuery struct {
	Start       time.Time
	End         time.Time
	Dimension   string
	Granularity string
	Timezone    string
}

func (s *UsageService) GetShapingStats(ctx context.Context, query UsageShapingQuery) (*UsageShapingStats, error) {
	if query.Dimension != "account" && query.Dimension != "group" {
		return nil, fmt.Errorf("invalid shaping stats dimension")
	}
	if query.Granularity != "day" && query.Granularity != "hour" {
		return nil, fmt.Errorf("invalid shaping stats granularity")
	}
	type shapingStatsRepository interface {
		GetShapingStats(context.Context, UsageShapingQuery) (*UsageShapingStats, error)
	}
	repo, ok := s.usageRepo.(shapingStatsRepository)
	if !ok {
		return nil, fmt.Errorf("usage shaping statistics unavailable")
	}
	stats, err := repo.GetShapingStats(ctx, query)
	if err != nil {
		return nil, err
	}
	stats.Granularity = query.Granularity
	stats.Buckets = make([]time.Time, 0)
	for bucket := query.Start; bucket.Before(query.End); {
		stats.Buckets = append(stats.Buckets, bucket)
		if query.Granularity == "hour" {
			bucket = bucket.Add(time.Hour)
		} else {
			bucket = bucket.AddDate(0, 0, 1)
		}
	}
	sort.Slice(stats.Breakdown, func(i, j int) bool {
		if stats.Breakdown[i].AfterCost != stats.Breakdown[j].AfterCost {
			return stats.Breakdown[i].AfterCost > stats.Breakdown[j].AfterCost
		}
		return stats.Breakdown[i].Requests > stats.Breakdown[j].Requests
	})
	return stats, nil
}

//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newEnterpriseKiroAccountForProfileTest(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_method": "idc",
			"start_url":   "https://example-corp.awsapps.com/start",
		},
	}
}

func stubKiroListAvailableProfiles(t *testing.T, fn func() (*kiroListAvailableProfilesResponse, error)) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	previous := kiroListAvailableProfilesFn
	kiroListAvailableProfilesFn = func(context.Context, *Account, string) (*kiroListAvailableProfilesResponse, error) {
		calls.Add(1)
		return fn()
	}
	t.Cleanup(func() { kiroListAvailableProfilesFn = previous })
	return &calls
}

// 首次失败不得把占位 ARN 永久定死：退避期满后必须重新解析出真实 ARN。
func TestKiroProfileResolutionRetriesAfterFailureBackoff(t *testing.T) {
	const accountID = int64(880001)
	kiroProfileResolutionFlight.Delete(accountID)
	t.Cleanup(func() { kiroProfileResolutionFlight.Delete(accountID) })

	fail := true
	calls := stubKiroListAvailableProfiles(t, func() (*kiroListAvailableProfilesResponse, error) {
		if fail {
			return nil, errors.New("upstream 500")
		}
		return &kiroListAvailableProfilesResponse{Profiles: []kiroAvailableProfile{{ARN: "arn:aws:codewhisperer:us-east-1:123:profile/REAL"}}}, nil
	})

	account := newEnterpriseKiroAccountForProfileTest(accountID)
	require.Equal(t, kiroBuilderIDProfileARN, kiroResolveAndPersistProfileArn(context.Background(), nil, account, "tok"))
	require.Equal(t, int32(1), calls.Load())

	// 退避窗口内不再打上游。
	require.Equal(t, kiroBuilderIDProfileARN, kiroResolveAndPersistProfileArn(context.Background(), nil, account, "tok"))
	require.Equal(t, int32(1), calls.Load())

	// 模拟退避到期。
	kiroProfileResolutionFlight.Store(accountID, kiroProfileBackoff{until: time.Now().Add(-time.Second), failures: 1})
	fail = false
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:123:profile/REAL", kiroResolveAndPersistProfileArn(context.Background(), nil, account, "tok"))
	require.Equal(t, int32(2), calls.Load())

	// 拿到真实 ARN 后直接复用，不再查询。
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:123:profile/REAL", kiroResolveAndPersistProfileArn(context.Background(), nil, account, "tok"))
	require.Equal(t, int32(2), calls.Load())
}

func TestKiroProfileResolutionBackoffGrowsAndCaps(t *testing.T) {
	const accountID = int64(880002)
	kiroProfileResolutionFlight.Delete(accountID)
	t.Cleanup(func() { kiroProfileResolutionFlight.Delete(accountID) })

	now := time.Now()
	var last time.Duration
	for i := 0; i < 8; i++ {
		kiroRecordProfileResolutionFailure(accountID, now)
		v, _ := kiroProfileResolutionFlight.Load(accountID)
		delay := v.(kiroProfileBackoff).until.Sub(now)
		require.GreaterOrEqual(t, delay, last)
		require.LessOrEqual(t, delay, kiroProfileResolveBackoffMax)
		last = delay
	}
	require.Equal(t, kiroProfileResolveBackoffMax, last)
}

// 同一账号的并发请求只触发一次上游查询。
func TestKiroProfileResolutionCoalescesConcurrentRequests(t *testing.T) {
	const accountID = int64(880003)
	kiroProfileResolutionFlight.Delete(accountID)
	t.Cleanup(func() { kiroProfileResolutionFlight.Delete(accountID) })

	release := make(chan struct{})
	calls := stubKiroListAvailableProfiles(t, func() (*kiroListAvailableProfilesResponse, error) {
		<-release
		return &kiroListAvailableProfilesResponse{Profiles: []kiroAvailableProfile{{ARN: "arn:real"}}}, nil
	})

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan string, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 每个请求用各自的账号快照，与调度器给出的副本一致。
			results <- kiroResolveAndPersistProfileArn(context.Background(), nil, newEnterpriseKiroAccountForProfileTest(accountID), "tok")
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	for arn := range results {
		require.Equal(t, "arn:real", arn)
	}
	require.Equal(t, int32(1), calls.Load())
}

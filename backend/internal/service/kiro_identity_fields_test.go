//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 刷新轮换 refresh_token 后，设备指纹必须保持不变。
func TestKiroMachineIDSurvivesRefreshTokenRotation(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "rt-old"}}
	before := buildKiroMachineID(account)

	oldCreds := ensureKiroMachineIDCredential(account, cloneKiroCredentials(account.Credentials))
	merged := MergeCredentials(oldCreds, map[string]any{"refresh_token": "rt-new", "access_token": "at"})
	account.Credentials = merged

	require.Equal(t, before, buildKiroMachineID(account))
	require.Equal(t, before, merged["machine_id"])
}

func TestEnsureKiroMachineIDCredentialKeepsExistingValue(t *testing.T) {
	existing := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	creds := map[string]any{"machine_id": existing, "refresh_token": "rt"}
	got := ensureKiroMachineIDCredential(&Account{Credentials: creds}, creds)
	require.Equal(t, existing, got["machine_id"])
}

func TestPrepareKiroMachineIDForCreate(t *testing.T) {
	creds := prepareKiroMachineIDForCreate(PlatformKiro, AccountTypeOAuth, map[string]any{"refresh_token": "rt"})
	require.NotEmpty(t, creds["machine_id"])

	other := prepareKiroMachineIDForCreate(PlatformAnthropic, AccountTypeOAuth, map[string]any{"refresh_token": "rt"})
	require.NotContains(t, other, "machine_id")
}

type extendingKiroCooldownStore struct {
	stubKiroCooldownStore
	extended time.Duration
}

func (s *extendingKiroCooldownStore) ExtendCooldown(_ context.Context, _ string, minCooldown time.Duration, _ string) (time.Duration, error) {
	s.extended = minCooldown
	return minCooldown, nil
}

func TestKiroRetryAfterParsing(t *testing.T) {
	now := time.Now()
	require.Equal(t, 120*time.Second, kiroRetryAfter(http.Header{"Retry-After": []string{"120"}}, now))
	require.Equal(t, kiroMaxRetryAfter, kiroRetryAfter(http.Header{"Retry-After": []string{"999999"}}, now))
	require.Zero(t, kiroRetryAfter(http.Header{"Retry-After": []string{"garbage"}}, now))
	require.Zero(t, kiroRetryAfter(nil, now))
}

// 上游 Retry-After 比本地退避长时必须采信，否则账号会在上游仍限流时被提前放回调度。
func TestMarkKiro429HonorsLongerRetryAfter(t *testing.T) {
	store := &extendingKiroCooldownStore{stubKiroCooldownStore: stubKiroCooldownStore{mark429TTL: time.Minute}}
	svc := &GatewayService{kiroCooldownStore: store}

	cooldown, err := svc.markKiro429WithRetryAfter(context.Background(), 0, "tk", http.Header{"Retry-After": []string{"600"}})
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, cooldown)
	require.Equal(t, 10*time.Minute, store.extended)

	store.extended = 0
	cooldown, err = svc.markKiro429WithRetryAfter(context.Background(), 0, "tk", http.Header{"Retry-After": []string{"5"}})
	require.NoError(t, err)
	require.Equal(t, time.Minute, cooldown, "更短的 Retry-After 不缩短本地退避")
	require.Zero(t, store.extended)
}

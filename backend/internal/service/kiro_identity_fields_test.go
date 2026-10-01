//go:build unit

package service

import (
	"testing"

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

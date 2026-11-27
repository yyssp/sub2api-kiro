//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShapingRecordUsagePricesRawAndFinalWithIndependentTTLAndDiscount(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			pricing := newTestBillingServiceWithPrices(map[string]*ModelPricing{"claude-sonnet-4": {
				InputPricePerToken: .001, OutputPricePerToken: .002, CacheReadPricePerToken: .0001,
				CacheCreationPricePerToken: .002, SupportsCacheBreakdown: true, CacheCreation5mPrice: .002, CacheCreation1hPrice: .004,
			}})
			// Raw $0.4 input + $0.04 output + $0.01 read + $0.1 short writes + $0.2 long writes = $0.75.
			raw := ClaudeUsage{InputTokens: 400, OutputTokens: 20, CacheReadInputTokens: 100, CacheCreationInputTokens: 100, CacheCreation5mTokens: 50, CacheCreation1hTokens: 50}
			raw.captureRawUsage()
			raw.InputTokens = 200
			raw.OutputTokens = 40
			raw.CacheReadInputTokens = 200
			raw.CacheCreationInputTokens = 50
			raw.CacheCreation5mTokens = 25
			raw.CacheCreation1hTokens = 25
			gid := int64(10)
			key := &APIKey{ID: 501, GroupID: &gid, Group: &Group{ID: gid, Platform: platform, RateMultiplier: .5}}
			account := &Account{ID: 701, Platform: platform}
			if platform == PlatformAnthropic {
				svc := newGatewayRecordUsageServiceWithBillingRepoForTest(repo, &openAIRecordUsageBillingRepoStub{}, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
				svc.billingService = pricing
				require.NoError(t, svc.RecordUsage(context.Background(), &RecordUsageInput{Result: &ForwardResult{RequestID: "raw-cost", Model: "claude-sonnet-4", Usage: raw}, APIKey: key, User: &User{ID: 601}, Account: account}))
			} else {
				svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
				svc.billingService = pricing
				require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "raw-cost", Model: "claude-sonnet-4", Usage: claudeUsageToOpenAIUsage(&raw)}, APIKey: key, User: &User{ID: 601}, Account: account}))
			}
			require.NotNil(t, repo.lastLog)
			require.NotNil(t, repo.lastLog.RawTotalCost)
			require.InDelta(t, .75, *repo.lastLog.RawTotalCost, 1e-10)
			require.InDelta(t, .45, repo.lastLog.TotalCost, 1e-10)
			require.InDelta(t, .225, repo.lastLog.ActualCost, 1e-10)
			require.NotNil(t, repo.lastLog.AccountStatsCost)
			require.InDelta(t, .75, *repo.lastLog.AccountStatsCost, 1e-10)
			require.Equal(t, 400, *repo.lastLog.RawInputTokens)
		})
	}
}

func TestShapingEmptyReplyIsFreeAndEstimatesDoNotChangeUpstreamCost(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI} {
		for _, empty := range []bool{false, true} {
			t.Run(platform+map[bool]string{true: "/empty", false: "/estimated"}[empty], func(t *testing.T) {
				repo := &openAIRecordUsageLogRepoStub{inserted: true}
				pricing := newTestBillingServiceWithPrices(map[string]*ModelPricing{"claude-sonnet-4": {InputPricePerToken: .001, OutputPricePerToken: .002}})
				u := ClaudeUsage{InputTokens: 100, OutputTokens: 0}
				upstream := u.billingUsageTokens()
				if !empty {
					u.OutputTokens = 20
				}
				u.captureRawUsage()
				u.rawUsage.UpstreamTokens = upstream
				u.rawUsage.EmptyResponse = empty
				if !empty {
					u.OutputTokens = 40
				}
				gid := int64(10)
				key := &APIKey{ID: 501, GroupID: &gid, Group: &Group{ID: gid, Platform: platform, RateMultiplier: .5}}
				account := &Account{ID: 701, Platform: platform}
				if platform == PlatformAnthropic {
					svc := newGatewayRecordUsageServiceWithBillingRepoForTest(repo, &openAIRecordUsageBillingRepoStub{}, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
					svc.billingService = pricing
					require.NoError(t, svc.RecordUsage(context.Background(), &RecordUsageInput{Result: &ForwardResult{RequestID: "zero-usage", Model: "claude-sonnet-4", Usage: u}, APIKey: key, User: &User{ID: 601}, Account: account}))
				} else {
					svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
					svc.billingService = pricing
					require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "zero-usage", Model: "claude-sonnet-4", Usage: claudeUsageToOpenAIUsage(&u)}, APIKey: key, User: &User{ID: 601}, Account: account}))
				}
				require.InDelta(t, .1, *repo.lastLog.AccountStatsCost, 1e-10)
				if empty {
					require.Zero(t, repo.lastLog.TotalCost)
					require.Zero(t, repo.lastLog.ActualCost)
				} else {
					require.InDelta(t, .14, *repo.lastLog.RawTotalCost, 1e-10)
					require.InDelta(t, .18, repo.lastLog.TotalCost, 1e-10)
					require.InDelta(t, .09, repo.lastLog.ActualCost, 1e-10)
				}
			})
		}
	}
}

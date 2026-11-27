package service

import "testing"

func TestCaptureRawUsageSurvivesProjectionAndProtocolConversion(t *testing.T) {
	claude := ClaudeUsage{InputTokens: 120, OutputTokens: 30, CacheReadInputTokens: 40, CacheCreationInputTokens: 10}
	claude.captureRawUsage()
	claude.InputTokens = 75
	claude.OutputTokens = 45
	claude.CacheReadInputTokens = 70
	claude.CacheCreationInputTokens = 25
	claude.captureRawUsage()

	openAI := claudeUsageToOpenAIUsage(&claude)
	if openAI.rawUsage == nil || *openAI.rawUsage != (RawUsageSnapshot{InputTokens: 120, OutputTokens: 30, CacheReadTokens: 40, CacheCreationTokens: 10, Tokens: UsageTokens{InputTokens: 120, OutputTokens: 30, CacheReadTokens: 40, CacheCreationTokens: 10}, UpstreamTokens: UsageTokens{InputTokens: 120, OutputTokens: 30, CacheReadTokens: 40, CacheCreationTokens: 10}}) {
		t.Fatalf("protocol conversion lost the pre-shaping snapshot: %+v", openAI.rawUsage)
	}
}

func TestCaptureOpenAIRawUsageUsesExclusiveInputBuckets(t *testing.T) {
	usage := OpenAIUsage{InputTokens: 150, OutputTokens: 25, CacheReadInputTokens: 30, CacheCreationInputTokens: 20}
	usage.captureRawUsage()
	want := RawUsageSnapshot{InputTokens: 100, OutputTokens: 25, CacheReadTokens: 30, CacheCreationTokens: 20, Tokens: UsageTokens{InputTokens: 100, OutputTokens: 25, CacheReadTokens: 30, CacheCreationTokens: 20}, UpstreamTokens: UsageTokens{InputTokens: 100, OutputTokens: 25, CacheReadTokens: 30, CacheCreationTokens: 20}}
	if usage.rawUsage == nil || *usage.rawUsage != want {
		t.Fatalf("got %+v, want %+v", usage.rawUsage, want)
	}
}

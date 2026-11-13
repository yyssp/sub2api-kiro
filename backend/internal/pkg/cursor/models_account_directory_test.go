package cursor

import (
	"reflect"
	"testing"
)

func TestAccountDirectoryResolutionUsesOnlyRealVariants(t *testing.T) {
	directory := []ModelMeta{
		{ID: "claude-opus-4-8-low", Family: "claude"},
		{ID: "claude-opus-4-8-medium", Family: "claude"},
		{ID: "claude-opus-4-8-thinking-high", Family: "claude", Think: true},
		{ID: "claude-opus-4-8-thinking-high-fast", Family: "claude", Think: true},
		{ID: "claude-4.6-opus-high", Family: "claude"},
		{ID: "claude-4.6-opus-high-thinking", Family: "claude", Think: true},
		{ID: "claude-sonnet-4.6-sonnet-medium", Family: "claude"},
	}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"opus release uses real medium", "claude-opus-4-8", "claude-opus-4-8-medium"},
		{"dot spelling uses real legacy id", "claude-opus-4.8", "claude-opus-4-8-medium"},
		{"context selector is stripped", "claude-opus-4-8[1m]", "claude-opus-4-8-medium"},
		{"thinking effort is real", "claude-opus-4-8[thinking=true,effort=high]", "claude-opus-4-8-thinking-high"},
		{"thinking fast is real", "claude-opus-4-8[thinking=true,effort=high,fast=true]", "claude-opus-4-8-thinking-high-fast"},
		{"legacy 4.6 spelling", "claude-opus-4-6", "claude-4.6-opus-high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveClaudeCodeModelFromModels(directory, tc.in)
			if !ok || got != tc.want {
				t.Fatalf("ResolveClaudeCodeModelFromModels(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
			}
		})
	}
	for _, missing := range []string{
		"claude-opus-4-8-max",
		"claude-opus-4-8-thinking-low",
		"claude-opus-4-8-thinking-high-fast-extra",
		"claude-opus-9",
	} {
		if got, ok := ResolveClaudeCodeModelFromModels(directory, missing); ok || got != "" {
			t.Fatalf("missing variant %q must not be invented: %q, %v", missing, got, ok)
		}
	}
}

func TestAccountDirectoryAPIsDoNotReadGlobalCatalog(t *testing.T) {
	oldLive, oldSet := liveModels, liveSet
	t.Cleanup(func() { liveModels, liveSet = oldLive, oldSet })
	SetLiveModels([]ModelMeta{{ID: "claude-opus-5-medium", Family: "claude"}})

	accountDirectory := []ModelMeta{{ID: "claude-opus-4-8-medium", Family: "claude"}}
	if got := StripPrefixFromModels(accountDirectory, "claude-opus-4-8"); got != "claude-opus-4-8-medium" {
		t.Fatalf("StripPrefixFromModels used global directory: %q", got)
	}
	if got := SandResolvedModelFromModels(accountDirectory, "claude-opus-4-8"); got != "claude-opus-4-8-medium" {
		t.Fatalf("SandResolvedModelFromModels used global directory: %q", got)
	}
	if got, ok := ResolveClaudeCodeModelFromModels(accountDirectory, "claude-opus-5"); ok || got != "" {
		t.Fatalf("account directory must reject a model present only globally: %q, %v", got, ok)
	}
}

func TestClientModelIDsFromModelsAggregatesRealIDsSorted(t *testing.T) {
	directory := []ModelMeta{
		{ID: "claude-opus-4-8-medium", Family: "claude"},
		{ID: "claude-opus-4-8-thinking-high", Family: "claude", Think: true},
		{ID: "claude-opus-4-8-thinking-high-fast", Family: "claude", Think: true},
		{ID: "claude-4.6-opus-high", Family: "claude"},
		{ID: "gpt-5.5-high", Family: "gpt"},
	}
	got := ClientModelIDsFromModels(directory)
	want := []string{
		"claude-opus-4-6",
		"claude-opus-4-6-high",
		"claude-opus-4-8",
		"claude-opus-4-8-medium",
		"claude-opus-4-8-thinking-high",
		"claude-opus-4-8-thinking-high-fast",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ClientModelIDsFromModels() = %v, want %v", got, want)
	}
	for _, id := range got {
		if id == "claude-opus-4-8-max" || id == "gpt-5.5-high" {
			t.Fatalf("directory output invented/leaked ID %q", id)
		}
	}
}

func TestClaudeCodeModelIDsFromModelsIsPublicWhitelist(t *testing.T) {
	directory := []ModelMeta{
		{ID: "claude-opus-4-8-medium", Family: "claude"},
		{ID: "claude-sonnet-5-thinking-high", Family: "claude", Think: true},
		{ID: "composer-2.5", Family: "composer"},
	}
	got := ClaudeCodeModelIDsFromModels(directory)
	for _, id := range got {
		if id == "composer-2.5" || id == "claude-opus-4-8-medium" {
			t.Fatalf("whitelist leaked raw/internal ID %q", id)
		}
	}
	for _, want := range []string{"claude-opus-4-8", "claude-sonnet-5"} {
		found := false
		for _, id := range got {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("whitelist missing %q: %v", want, got)
		}
	}
}

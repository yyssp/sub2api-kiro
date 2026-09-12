package cursor

import (
	"testing"
	"time"
)

func TestResolveClaudeCodeModel_UsesDynamicCursorID(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{
		{ID: "claude-opus-5-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-opus-5-thinking-high", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-opus-4-8-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-4.6-opus-high", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-sonnet-5-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-sonnet-5-thinking-high", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-4.6-sonnet-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-4.6-sonnet-medium-thinking", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-4.5-sonnet", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-4.5-haiku", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-fable-5-medium", Family: "claude", Vision: true, Tools: true},
	})

	tests := []struct {
		input string
		want  string
	}{
		{"opus", "claude-opus-5-medium"},
		{"opus-high", "claude-opus-5-medium"},
		{"opusplan", "claude-opus-5-medium"},
		{"sonnet", "claude-sonnet-5-medium"},
		{"sonnet-thinking-high", "claude-sonnet-5-thinking-high"},
		{"haiku", "claude-4.5-haiku"},
		// Claude Code 会将 `haiku` 规范为带发布日期的官方 ID；发布日期
		// 不是 Cursor 的能力后缀，必须仍解析到动态清单里的真实模型。
		{"claude-haiku-4-5-20251001", "claude-4.5-haiku"},
		{"cursor/claude-haiku-4-5-20251001", "claude-4.5-haiku"},
		{"claude-opus-4-6-20251001", "claude-4.6-opus-high"},
		{"fable", "claude-fable-5-medium"},
		{"claude-opus-4-6", "claude-4.6-opus-high"},
		{"claude-opus-4.8", "claude-opus-4-8-medium"},
		{"cursor/claude-sonnet-4-6", "claude-4.6-sonnet-medium"},
		{"claude-sonnet-4-6-thinking", "claude-4.6-sonnet-medium-thinking"},
		{"claude-opus-5-thinking-high", "claude-opus-5-thinking-high"},
		{"claude-4.5-haiku", "claude-4.5-haiku"},
	}
	for _, tc := range tests {
		got, ok := ResolveClaudeCodeModel(tc.input)
		if !ok || got != tc.want {
			t.Errorf("ResolveClaudeCodeModel(%q) = %q, %v; want %q, true", tc.input, got, ok, tc.want)
		}
	}
	if got, ok := ResolveClaudeCodeModel("claude-opus-9"); ok || got != "" {
		t.Fatalf("不存在的模型不应解析成功: %q, %v", got, ok)
	}
}

func TestResolveClaudeCodeModel_PreservesExactDynamicReleaseDateID(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{
		{ID: "claude-haiku-4-5-20251001", Family: "claude"},
		{ID: "claude-4.5-haiku", Family: "claude"},
	})
	got, ok := ResolveClaudeCodeModel("claude-haiku-4-5-20251001")
	if !ok || got != "claude-haiku-4-5-20251001" {
		t.Fatalf("精确动态 ID 不应被发布日期兼容逻辑改写: %q, %v", got, ok)
	}
}

func TestApplyClaudeEffortModelUsesExactThinkingTier(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{
		{ID: "claude-sonnet-5-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-sonnet-5-thinking-medium", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-sonnet-5-thinking-high", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-sonnet-5-thinking-xhigh", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-sonnet-5-thinking-max", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-4.6-sonnet-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-4.6-sonnet-medium-thinking", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "gpt-5.5-high", Family: "gpt", Vision: true, Tools: true, Think: true},
	})

	tests := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{name: "high", model: "claude-sonnet-5-medium", effort: "high", want: "claude-sonnet-5-thinking-high"},
		{name: "medium", model: "claude-sonnet-5-medium", effort: "medium", want: "claude-sonnet-5-thinking-medium"},
		{name: "xhigh", model: "claude-sonnet-5-medium", effort: "xhigh", want: "claude-sonnet-5-thinking-xhigh"},
		{name: "max", model: "claude-sonnet-5-medium", effort: "max", want: "claude-sonnet-5-thinking-max"},
		{name: "explicit-thinking-wins", model: "claude-sonnet-5-thinking-high", effort: "low", want: "claude-sonnet-5-thinking-high"},
		{name: "missing-tier-keeps-model", model: "claude-4.6-sonnet-medium", effort: "high", want: "claude-4.6-sonnet-medium"},
		{name: "invalid-effort-keeps-model", model: "claude-sonnet-5-medium", effort: "unknown", want: "claude-sonnet-5-medium"},
		{name: "non-claude-keeps-model", model: "gpt-5.5-high", effort: "high", want: "gpt-5.5-high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyClaudeEffortModel(tc.model, tc.effort); got != tc.want {
				t.Fatalf("applyClaudeEffortModel(%q, %q)=%q, want %q", tc.model, tc.effort, got, tc.want)
			}
		})
	}
}

func TestApplyClaudeEffortModelDoesNotCrossClaudeReleasePrefix(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() {
		modelMu.Lock()
		liveModels, liveSet, livePath = oldLive, oldSet, oldPath
		modelMu.Unlock()
	}()

	SetLiveModels([]ModelMeta{
		{ID: "claude-fable-5-1-medium", Family: "claude"},
		{ID: "claude-fable-5-1-thinking-high", Family: "claude", Think: true},
		{ID: "claude-fable-5-medium", Family: "claude"},
		{ID: "claude-fable-5-thinking-high", Family: "claude", Think: true},
	})

	got := applyClaudeEffortModel("claude-fable-5-medium", "high")
	if got != "claude-fable-5-thinking-high" {
		t.Fatalf("applyClaudeEffortModel crossed release prefix: got %q, want claude-fable-5-thinking-high", got)
	}
	if got := applyClaudeEffortModel("claude-fable-5-1-medium", "high"); got != "claude-fable-5-thinking-high" {
		t.Fatalf("explicit fable 5-1 base should still use catalog fable-5 tier: got %q", got)
	}
}

func TestClaudeCodeModelIDs_OnlyStandardAndResolvable(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{
		{ID: "claude-opus-5-medium", Family: "claude"},
		{ID: "claude-sonnet-5-medium", Family: "claude"},
		{ID: "claude-4.5-haiku", Family: "claude"},
		{ID: "composer-2.5", Family: "composer"},
		{ID: "gpt-5.5-high", Family: "gpt"},
	})
	ids := ClaudeCodeModelIDs()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("标准列表重复 ID: %s", id)
		}
		seen[id] = true
		if _, ok := ResolveClaudeCodeModel(id); !ok {
			t.Fatalf("列表中的 ID 必须可解析: %s", id)
		}
		if id == "composer-2.5" || id == "gpt-5.5-high" || id == "claude-opus-5-medium" {
			t.Fatalf("列表泄露了内部/非 Claude ID: %s", id)
		}
	}
	for _, want := range []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"} {
		if !seen[want] {
			t.Fatalf("标准列表缺少 %s: %v", want, ids)
		}
	}
	// 短别名只保留调用解析能力, 不进对外列表
	for _, alias := range []string{"opus", "sonnet", "haiku", "fable", "opusplan"} {
		if seen[alias] {
			t.Fatalf("标准列表不应包含短别名 %s: %v", alias, ids)
		}
	}
}

func TestClaudeCodeModelsRouteToSandQuotaSurface(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{
		{ID: "claude-opus-5-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-opus-5-thinking-high", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-opus-4-8-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-4.6-opus-high", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-sonnet-5-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-sonnet-5-thinking-high", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-4.6-sonnet-medium", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-4.6-sonnet-medium-thinking", Family: "claude", Vision: true, Tools: true, Think: true},
		{ID: "claude-4.5-sonnet", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-4.5-haiku", Family: "claude", Vision: true, Tools: true},
		{ID: "claude-fable-5-medium", Family: "claude", Vision: true, Tools: true},
	})

	ids := ClaudeCodeModelIDs()
	if len(ids) == 0 {
		t.Fatal("ClaudeCodeModelIDs() should expose resolvable Claude models")
	}

	account := Account{
		Membership:      "pro",
		UsageAt:         time.Now(),
		GrokBotEnabled:  true,
		GrokBotPct:      10,
		CursorModelsPct: 100,
		OtherModelsPct:  10,
	}
	for _, id := range ids {
		if got := cursorClientTypeForModel(id); got != "sand" {
			t.Fatalf("Claude model %q surface=%q, want sand", id, got)
		}
		if !useSandInferenceService(id) {
			t.Fatalf("Claude model %q must use direct Sand InferenceService", id)
		}
		if got := usageBucketForModel(id); got != "grokbot" {
			t.Fatalf("Claude model %q usage bucket=%q, want grokbot", id, got)
		}
		if !AccountUsableForModel(account, id) {
			t.Fatalf("Claude model %q should use the GrokBot/Sand surface", id)
		}
	}
}

func TestIsCursorModel_RecognizesClaudeCodeAliases(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{
		{ID: "claude-opus-5-medium", Family: "claude"},
		{ID: "claude-sonnet-5-medium", Family: "claude"},
	})
	for _, model := range []string{"opus", "sonnet", "cursor/opus", "claude-opus-5"} {
		if !IsCursorModel(model) {
			t.Fatalf("标准 Claude Code 模型应路由 Cursor 池: %s", model)
		}
	}
}

func TestClaudeCodeAliasesRouteToSandWhenDynamicListOmitsClaude(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() {
		modelMu.Lock()
		liveModels, liveSet, livePath = oldLive, oldSet, oldPath
		modelMu.Unlock()
	}()

	ConfigureSandModels(nil)
	t.Cleanup(func() { ConfigureSandModels(nil) })
	SetLiveModels([]ModelMeta{
		{ID: "cursor-grok-4.6-medium", Family: "grok"},
		{ID: "composer-2.5", Family: "composer"},
	})

	tests := []struct {
		input string
		wire  string
	}{
		{input: "sonnet", wire: "claude-sonnet-5"},
		{input: "opus", wire: "claude-opus-5"},
		{input: "haiku", wire: "claude-haiku-4-5"},
		{input: "fable", wire: "claude-fable-5"},
		{input: "opusplan", wire: "claude-opus-5"},
		{input: "claude-opus-4-6-20251001", wire: "claude-opus-4-6"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := cursorClientTypeForModel(tc.input); got != "sand" {
				t.Fatalf("cursorClientTypeForModel(%q)=%q, want sand", tc.input, got)
			}
			if !useSandInferenceService(tc.input) {
				t.Fatalf("Claude alias %q must use direct Sand InferenceService", tc.input)
			}
			if got := sandWireModel(tc.input); got != tc.wire {
				t.Fatalf("sandWireModel(%q)=%q, want %q", tc.input, got, tc.wire)
			}
			if !IsCursorModel(tc.input) {
				t.Fatalf("IsCursorModel(%q)=false, want true", tc.input)
			}
		})
	}
	for _, unknown := range []string{"claude-opus-9", "claude-sonnet-9-thinking-high"} {
		if got := cursorClientTypeForModel(unknown); got != "cli" {
			t.Fatalf("unknown Claude model %q surface=%q, want cli", unknown, got)
		}
		if IsCursorModel(unknown) {
			t.Fatalf("unknown Claude model %q must not be treated as Cursor model", unknown)
		}
	}
}

func TestStripPrefix_ResolvesClaudeCodeModel(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{
		{ID: "claude-opus-5-medium", Family: "claude"},
		{ID: "claude-4.5-haiku", Family: "claude"},
	})
	if got := StripPrefix("opus"); got != "claude-opus-5-medium" {
		t.Fatalf("裸标准模型未转换: %q", got)
	}
	if got := StripPrefix("cursor/claude-haiku-4-5"); got != "claude-4.5-haiku" {
		t.Fatalf("cursor/标准模型未转换: %q", got)
	}
	// 旧 Cursor 变体仍按原有路径归一，不被标准解析器破坏。
	if got := StripPrefix("cursor/claude-opus-5-medium"); got != "claude-opus-5-medium" {
		t.Fatalf("旧 Cursor 变体不应改变: %q", got)
	}
}

func TestStripPrefix_ResolvesAutoAliasToCursorDefault(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{{ID: "default", Family: "auto", Vision: true, Tools: true}})
	for _, input := range []string{"auto", "Auto", "cursor/auto"} {
		if got := StripPrefix(input); got != "default" {
			t.Fatalf("Auto 别名 %q 应转换为 Cursor 上游 default, got %q", input, got)
		}
	}

	SetLiveModels([]ModelMeta{{ID: "auto", Family: "auto", Vision: true, Tools: true}})
	if got := StripPrefix("Auto"); got != "auto" {
		t.Fatalf("动态清单提供 auto 时应使用规范 ID auto, got %q", got)
	}
}

package cursor

import (
	"path/filepath"
	"testing"
)

// TestCurrentModels_MergesFallbackUnderLive 需求1回归:
// 动态清单不全(账号设置隐藏官方模型)时, 展示清单 = 动态 ∪ 兜底补缺, 同 ID 以动态元信息为准。

// ⚠️ 以下测试依赖未移植的代码（OpenAI-Responses 协议面 / ModelInfo 管理面板
// 模型行 / 内置定价表），在 sub2api 侧分别由既有 OpenAI 网关、/v1/models
// 和通用定价服务承担，故移除：
//   - TestModelInfo_UsesRawCursorModels
//   - TestModelInfo_IncludesQuotaClassification
//   - TestModelInfo_DoesNotInjectFallbackWhenDynamicListEmpty

func TestCurrentModels_MergesFallbackUnderLive(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet := liveModels, liveSet
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet = oldLive, oldSet; modelMu.Unlock() }()

	// 模拟动态清单只有 3 个 claude(账号设置隐藏了其余), 且 opus-4.8 动态元信息 Think=false
	SetLiveModels([]ModelMeta{
		{ID: "claude-opus-4.8", Family: "claude", Vision: true, Think: false, MaxMode: true}, // 动态覆盖兜底(Think:false)
		{ID: "claude-opus-4.7", Family: "claude", Vision: true, Think: true, MaxMode: true},
		{ID: "composer-2", Family: "composer", Think: true, MaxMode: true},
	})

	got := currentModels()
	byID := map[string]ModelMeta{}
	for _, m := range got {
		byID[m.ID] = m
	}
	// 1) 动态项存在且元信息以动态为准
	if m, ok := byID["claude-opus-4.8"]; !ok || m.Think {
		t.Fatalf("动态项应保留且以动态元信息为准, got %+v", byID["claude-opus-4.8"])
	}
	// 2) 兜底补缺: 动态缺失的 sonnet-5 / haiku-4.5 / gpt-5.5 应出现在展示清单
	for _, id := range []string{"claude-sonnet-5", "claude-4.5-haiku", "gpt-5.5", "gemini-3.1-pro"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("兜底项 %s 应补入展示清单", id)
		}
	}
	// 3) 无重复
	if len(byID) != len(got) {
		t.Fatalf("展示清单存在重复 ID: %d unique vs %d total", len(byID), len(got))
	}
}

// TestMerge_DoesNotPolluteRouting 需求1护栏:
// 兜底扩充的未知模型不得进入路由判定集合；标准 Claude Code 名称是显式
// 兼容面，即使动态清单缺失也必须走 Sand。
func TestMerge_DoesNotPolluteRouting(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet := liveModels, liveSet
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet = oldLive, oldSet; modelMu.Unlock() }()

	SetLiveModels([]ModelMeta{{ID: "claude-opus-4.8", Family: "claude"}})

	// 动态清单里的精确命中 → Cursor 池
	if !IsCursorModel("claude-opus-4.8") {
		t.Fatal("动态清单精确命中应判为 Cursor")
	}
	// 未知 Claude 名称不属于标准兼容面 → 不得判为 Cursor
	if IsCursorModel("claude-internal-legacy") {
		t.Fatal("未知 Claude 名称不应参与 Cursor 路由判定")
	}
	if IsCursorModel("claude-opus-9") {
		t.Fatal("不存在的 Claude 版本不应参与 Cursor 路由判定")
	}
	// 标准 Claude Code 名称即使不在动态清单，也必须识别为 Cursor/Sand。
	if !IsCursorModel("claude-4.5-haiku") || !IsCursorModel("claude-sonnet-5") {
		t.Fatal("标准 Claude Code 名称不应受动态清单缺失影响")
	}
}

// TestModelsPersistence_RoundTrip 清单持久化: SetLiveModels 落盘 -> 模拟重启(清空内存) -> 恢复。
func TestModelsPersistence_RoundTrip(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet, oldPath := liveModels, liveSet, livePath
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet, livePath = oldLive, oldSet, oldPath; modelMu.Unlock() }()

	path := filepath.Join(t.TempDir(), "cursor_models.json")
	EnableModelsPersistence(path) // 文件不存在: 安全 no-op

	full := make([]ModelMeta, 0, 207)
	for i := 0; i < 207; i++ {
		full = append(full, ModelMeta{ID: "m-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26)), Family: "claude"})
	}
	SetLiveModels(full)

	// 模拟重启: 清空内存态, 仅留磁盘文件
	modelMu.Lock()
	liveModels, liveSet = nil, nil
	modelMu.Unlock()

	EnableModelsPersistence(path)
	if LiveModelCount() != 207 {
		t.Fatalf("重启后应恢复 207 个动态清单, got %d", LiveModelCount())
	}
	if !inModelSet("m-a-0") {
		t.Fatal("恢复后路由判定集合应可用")
	}
}

// TestFallbackOnly_WhenNoLive 无动态清单时回退到扩充后的兜底全量。
func TestFallbackOnly_WhenNoLive(t *testing.T) {
	modelMu.Lock()
	oldLive, oldSet := liveModels, liveSet
	modelMu.Unlock()
	defer func() { modelMu.Lock(); liveModels, liveSet = oldLive, oldSet; modelMu.Unlock() }()

	modelMu.Lock()
	liveModels, liveSet = nil, nil
	modelMu.Unlock()

	got := currentModels()
	if len(got) != len(DefaultModels) {
		t.Fatalf("无动态时应返回兜底全量: got %d want %d", len(got), len(DefaultModels))
	}
	var claude int
	for _, m := range got {
		if m.Family == "claude" {
			claude++
		}
	}
	if claude < 10 {
		t.Fatalf("兜底 Claude 阵容应 >=10(含 Sonnet/Haiku/Fable/Opus 各代), got %d", claude)
	}
}

// TestModelInfo_UsesRawCursorModels 管理台必须展示 Cursor 上游原始清单，
// 与 /v1/models 对外的 Claude Code 标准模型列表严格分离。

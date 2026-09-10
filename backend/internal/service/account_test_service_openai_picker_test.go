package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Codex manifest 标准化后只保留 slug，测试弹窗用 display_name 当选项标签，
// 留空会让模型选择器渲染成一排空白项。
func TestFetchOpenAIAccountModelsFillsPickerLabels(t *testing.T) {
	newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-5.6-terra"},{"slug":"codex-auto-review"}]}`)
	svc := &AccountTestService{}
	svc.SetOpenAIGatewayService(&OpenAIGatewayService{})

	models, err := svc.FetchOpenAIAccountModels(context.Background(), newCodexModelsTestAccount())
	require.NoError(t, err)

	// manifest 返回的模型排在前面并保持顺序；其后是本地追加的 image_generation
	// 选项（Codex discovery 不下发这些模型，见 FetchOpenAIAccountModels）。
	require.GreaterOrEqual(t, len(models), 2)
	require.Equal(t, "gpt-5.6-terra", models[0].ID)
	require.Equal(t, "codex-auto-review", models[1].ID)
	for _, model := range models[:2] {
		require.Equal(t, model.ID, model.DisplayName)
	}
	for _, model := range models[2:] {
		require.True(t, IsGPTImageGenerationModel(model.ID),
			"only image_generation models may be appended, got %q", model.ID)
	}
	// 无论来源，选择器标签和类型都必须非空，否则弹窗渲染成一排空白项。
	for _, model := range models {
		require.NotEmpty(t, model.DisplayName, "picker label must not be empty for %q", model.ID)
		require.Equal(t, "model", model.Type)
	}
}

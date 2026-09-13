package kiro

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 上游 Smithy 校验只接受 7 个 schema 键，其余 draft-2020-12 关键字会让「整个请求」400。
// 参考：funny-vibes/agent-vibes translator.ts:898-906、AbdoKnbGit/tau request.ts:272-275。

// TestKiroSchemaWhitelistGoldenSample 用一个典型的 Claude Code MCP 工具 schema 作为黄金样例，
// 锁定所有已知 400 触发器都被剔除。改造前实测这 7 个触发器命中 7 个、无一被清理。
func TestKiroSchemaWhitelistGoldenSample(t *testing.T) {
	in := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"n": map[string]any{
				"type":             "integer",
				"exclusiveMinimum": 0,
				"default":          5,
				"format":           "int32",
			},
			"s": map[string]any{"type": "string", "const": "fixed"},
		},
		"required":      []any{},
		"propertyNames": map[string]any{"pattern": "^[a-z]+$"},
	}

	out, ok := normalizeKiroJSONSchema(in).(map[string]any)
	require.True(t, ok)

	for _, banned := range []string{"$schema", "additionalProperties", "propertyNames", "required"} {
		require.NotContains(t, out, banned, "顶层不应残留 %s", banned)
	}

	props := out["properties"].(map[string]any)
	n := props["n"].(map[string]any)
	for _, banned := range []string{"exclusiveMinimum", "default", "format"} {
		require.NotContains(t, n, banned, "properties.n 不应残留 %s", banned)
	}
	require.Equal(t, "integer", n["type"], "合法键必须保留")

	s := props["s"].(map[string]any)
	require.NotContains(t, s, "const")
	require.Equal(t, []any{"fixed"}, s["enum"], "const 应折叠为单元素 enum")
}

// TestKiroSchemaWhitelistRequiredHandling 锁定 required 的两种走向：
// 空数组必须移除整个键（空数组本身就是触发器），非空必须原样保留。
func TestKiroSchemaWhitelistRequiredHandling(t *testing.T) {
	t.Run("empty required is dropped", func(t *testing.T) {
		out := normalizeKiroJSONSchema(map[string]any{
			"type": "object", "required": []any{},
		}).(map[string]any)
		require.NotContains(t, out, "required")
	})

	t.Run("non-empty required is kept", func(t *testing.T) {
		out := normalizeKiroJSONSchema(map[string]any{
			"type": "object", "required": []any{"a", "b"},
		}).(map[string]any)
		require.Equal(t, []any{"a", "b"}, out["required"])
	})

	t.Run("missing required is not invented", func(t *testing.T) {
		out := normalizeKiroJSONSchema(map[string]any{"type": "object"}).(map[string]any)
		require.NotContains(t, out, "required")
	})
}

// TestKiroSchemaWhitelistDeepNesting 验证实现是「重建对象」而非「删顶层键」——
// 任意嵌套深度都不得残留超纲关键字。
func TestKiroSchemaWhitelistDeepNesting(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"b": map[string]any{
						"type":    "object",
						"$schema": "leaked",
						"properties": map[string]any{
							"c": map[string]any{"type": "string", "format": "email"},
						},
					},
				},
			},
		},
	}

	out := normalizeKiroJSONSchema(in).(map[string]any)
	b := out["properties"].(map[string]any)["a"].(map[string]any)["properties"].(map[string]any)["b"].(map[string]any)
	require.NotContains(t, b, "$schema", "第三层不应残留 $schema")

	c := b["properties"].(map[string]any)["c"].(map[string]any)
	require.NotContains(t, c, "format", "第四层不应残留 format")
	require.Equal(t, "string", c["type"])
}

// TestKiroSchemaWhitelistItemsCleaned 数组元素 schema 同样要被清洗。
func TestKiroSchemaWhitelistItemsCleaned(t *testing.T) {
	out := normalizeKiroJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"list": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string", "format": "uri", "default": "x",
				},
			},
		},
	}).(map[string]any)

	items := out["properties"].(map[string]any)["list"].(map[string]any)["items"].(map[string]any)
	require.NotContains(t, items, "format")
	require.NotContains(t, items, "default")
	require.Equal(t, "string", items["type"])
}

// TestKiroSchemaWhitelistPreservesValidSchema 回归保护：合法 schema 不得被破坏。
func TestKiroSchemaWhitelistPreservesValidSchema(t *testing.T) {
	out := normalizeKiroJSONSchema(map[string]any{
		"type":        "object",
		"title":       "Search",
		"description": "Search the web",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "the query"},
			"mode":  map[string]any{"type": "string", "enum": []any{"fast", "deep"}},
		},
		"required": []any{"query"},
	}).(map[string]any)

	require.Equal(t, "object", out["type"])
	require.Equal(t, "Search", out["title"])
	require.Equal(t, "Search the web", out["description"])
	require.Equal(t, []any{"query"}, out["required"])

	props := out["properties"].(map[string]any)
	require.Equal(t, "the query", props["query"].(map[string]any)["description"])
	require.Equal(t, []any{"fast", "deep"}, props["mode"].(map[string]any)["enum"])
}

// TestKiroSchemaWhitelistConstDoesNotClobberEnum const 与 enum 并存时不得覆盖已有 enum。
func TestKiroSchemaWhitelistConstDoesNotClobberEnum(t *testing.T) {
	out := normalizeKiroJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "string", "const": "a", "enum": []any{"a", "b"}},
		},
	}).(map[string]any)

	x := out["properties"].(map[string]any)["x"].(map[string]any)
	require.Equal(t, []any{"a", "b"}, x["enum"], "已有 enum 优先")
	require.NotContains(t, x, "const")
}

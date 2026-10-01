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

	props, ok := out["properties"].(map[string]any)
	require.True(t, ok)
	n, ok := props["n"].(map[string]any)
	require.True(t, ok)
	for _, banned := range []string{"exclusiveMinimum", "default", "format"} {
		require.NotContains(t, n, banned, "properties.n 不应残留 %s", banned)
	}
	require.Equal(t, "integer", n["type"], "合法键必须保留")

	s, ok := props["s"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, s, "const")
	require.Equal(t, []any{"fixed"}, s["enum"], "const 应折叠为单元素 enum")
}

// TestKiroSchemaWhitelistRequiredHandling 锁定 required 的两种走向：
// 空数组必须移除整个键（空数组本身就是触发器），非空必须原样保留。
func TestKiroSchemaWhitelistRequiredHandling(t *testing.T) {
	t.Run("empty required is dropped", func(t *testing.T) {
		out, ok := normalizeKiroJSONSchema(map[string]any{
			"type": "object", "required": []any{},
		}).(map[string]any)
		require.True(t, ok)
		require.NotContains(t, out, "required")
	})

	t.Run("non-empty required is kept", func(t *testing.T) {
		out, ok := normalizeKiroJSONSchema(map[string]any{
			"type": "object", "required": []any{"a", "b"},
		}).(map[string]any)
		require.True(t, ok)
		require.Equal(t, []any{"a", "b"}, out["required"])
	})

	t.Run("missing required is not invented", func(t *testing.T) {
		out, ok := normalizeKiroJSONSchema(map[string]any{"type": "object"}).(map[string]any)
		require.True(t, ok)
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

	out := requireSchemaMap(t, normalizeKiroJSONSchema(in))
	a := requireSchemaMap(t, out["properties"])["a"]
	bValue := requireSchemaMap(t, requireSchemaMap(t, a)["properties"])["b"]
	b := requireSchemaMap(t, bValue)
	require.NotContains(t, b, "$schema", "第三层不应残留 $schema")

	c := requireSchemaMap(t, requireSchemaMap(t, b["properties"])["c"])
	require.NotContains(t, c, "format", "第四层不应残留 format")
	require.Equal(t, "string", c["type"])
}

func requireSchemaMap(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	require.True(t, ok)
	return out
}

// TestKiroSchemaWhitelistItemsCleaned 数组元素 schema 同样要被清洗。
func TestKiroSchemaWhitelistItemsCleaned(t *testing.T) {
	out := requireSchemaMap(t, normalizeKiroJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"list": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string", "format": "uri", "default": "x",
				},
			},
		},
	}))

	props := requireSchemaMap(t, out["properties"])
	list := requireSchemaMap(t, props["list"])
	items := requireSchemaMap(t, list["items"])
	require.NotContains(t, items, "format")
	require.NotContains(t, items, "default")
	require.Equal(t, "string", items["type"])
}

// TestKiroSchemaWhitelistPreservesValidSchema 回归保护：合法 schema 不得被破坏。
func TestKiroSchemaWhitelistPreservesValidSchema(t *testing.T) {
	out := requireSchemaMap(t, normalizeKiroJSONSchema(map[string]any{
		"type":        "object",
		"title":       "Search",
		"description": "Search the web",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "the query"},
			"mode":  map[string]any{"type": "string", "enum": []any{"fast", "deep"}},
		},
		"required": []any{"query"},
	}))

	require.Equal(t, "object", out["type"])
	require.Equal(t, "Search", out["title"])
	require.Equal(t, "Search the web", out["description"])
	require.Equal(t, []any{"query"}, out["required"])

	props := requireSchemaMap(t, out["properties"])
	query := requireSchemaMap(t, props["query"])
	mode := requireSchemaMap(t, props["mode"])
	require.Equal(t, "the query", query["description"])
	require.Equal(t, []any{"fast", "deep"}, mode["enum"])
}

// TestKiroSchemaWhitelistConstDoesNotClobberEnum const 与 enum 并存时不得覆盖已有 enum。
func TestKiroSchemaWhitelistConstDoesNotClobberEnum(t *testing.T) {
	out := requireSchemaMap(t, normalizeKiroJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "string", "const": "a", "enum": []any{"a", "b"}},
		},
	}))

	x := requireSchemaMap(t, requireSchemaMap(t, out["properties"])["x"])
	require.Equal(t, []any{"a", "b"}, x["enum"], "已有 enum 优先")
	require.NotContains(t, x, "const")
}

func normalizedProperty(t *testing.T, in map[string]any, name string) map[string]any {
	t.Helper()
	out, ok := normalizeKiroJSONSchema(in).(map[string]any)
	require.True(t, ok)
	props, ok := out["properties"].(map[string]any)
	require.True(t, ok)
	prop, ok := props[name].(map[string]any)
	require.True(t, ok, "property %s missing", name)
	return prop
}

// Optional 字段的两种常见形状都不能被兜底成空 object，否则模型按对象传参。
func TestKiroSchemaOptionalFieldsKeepScalarType(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"typeArray": map[string]any{"type": []any{"string", "null"}, "description": "a"},
			"anyOfNull": map[string]any{
				"anyOf":       []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}},
				"description": "b",
			},
			"nullFirst": map[string]any{
				"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
			},
		},
	}

	typeArray := normalizedProperty(t, in, "typeArray")
	require.Equal(t, "string", typeArray["type"])
	require.NotContains(t, typeArray, "properties")

	anyOfNull := normalizedProperty(t, in, "anyOfNull")
	require.Equal(t, "integer", anyOfNull["type"])
	require.Equal(t, "b", anyOfNull["description"], "外层 description 优先")
	require.NotContains(t, anyOfNull, "anyOf")
	require.NotContains(t, anyOfNull, "properties")

	nullFirst := normalizedProperty(t, in, "nullFirst")
	require.Equal(t, "array", nullFirst["type"])
	items, ok := nullFirst["items"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "string", items["type"])
}

func TestKiroSchemaAllOfMergesProperties(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cfg": map[string]any{
				"allOf": []any{
					map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}, "required": []any{"a"}},
					map[string]any{"properties": map[string]any{"b": map[string]any{"type": "number"}}},
				},
			},
		},
	}
	cfg := normalizedProperty(t, in, "cfg")
	require.Equal(t, "object", cfg["type"])
	props, ok := cfg["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, props, "a")
	require.Contains(t, props, "b")
	require.Equal(t, []any{"a"}, cfg["required"])
}

func TestKiroSchemaInfersTypeFromEnumAndItems(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"enum": []any{"fast", "slow"}},
			"list": map[string]any{"items": map[string]any{"type": "string"}},
		},
	}
	require.Equal(t, "string", normalizedProperty(t, in, "mode")["type"])
	require.Equal(t, "array", normalizedProperty(t, in, "list")["type"])
}

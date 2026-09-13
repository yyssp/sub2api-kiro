# F06 · 🔴 G6：工具 schema 超纲关键字全部原样透传（已实测确证）

> **本轮优先级最高的缺口。** 不是推断，是在本仓库跑出来的实测结果。
> 证据等级：`[本仓库]` **实测** + `[源码]` 社区 ×2 + `[官方]` Smithy 语义。

---

## 1. 实测结果

我写了一次性探针调用本仓库真实的 `normalizeKiroJSONSchema`
（跑完即删，未入库），输入一个典型的 Claude Code MCP 工具 schema：

**输入**
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "n": {"type":"integer","exclusiveMinimum":0,"default":5,"format":"int32"},
    "s": {"type":"string","const":"fixed"}
  },
  "required": [],
  "propertyNames": {"pattern":"^[a-z]+$"}
}
```

**本仓库实际输出**
```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",   ← 原样透传 ❌
  "additionalProperties": false,
  "properties": {
    "n": {"default":5,"exclusiveMinimum":0,"format":"int32","type":"integer"},  ← 三个全留 ❌
    "s": {"const":"fixed","type":"string"}                      ← const 未折叠 ❌
  },
  "propertyNames": {"pattern":"^[a-z]+$"},                      ← 原样透传 ❌
  "required": [],                                                ← 空数组保留 ❌
  "type": "object"
}
```

**社区证据指认的 400 触发器，7 个里命中 7 个，无一被清理。**

---

## 2. 根因：这是个"补全器"，不是"过滤器"

`[本仓库]` `internal/pkg/kiro/translator.go:2032-2040`

```go
func normalizeKiroJSONSchemaValue(schema any, enforceObjectKeywords bool) any {
	obj, ok := schema.(map[string]any)
	if !ok || obj == nil { return defaultKiroJSONSchema() }
	normalized := make(map[string]any, len(obj)+4)
	for key, value := range obj {                    // ← 全键拷贝，无白名单
		normalized[key] = normalizeSchemaChild(key, value)
	}
	...
```

设计意图是**补全缺失的 object 关键字**（`type`/`properties`/`required`/`additionalProperties`），
方向是"往里加"。它**从未打算过滤**任何键。

更糟的是它还**主动制造**两个已知触发器：
```go
normalized["required"] = normalizeSchemaRequired(normalized["required"])  // :2059 缺失时产出 []
...
default:
    normalized["additionalProperties"] = true                             // :2066
```
`[源码]` `tau/request.ts:272-275` 明确指出 **`required: []` 空数组会触发 400**，
而 `normalizeSchemaRequired`（`:2089-2101`）在 value 非数组时**恒返回 `[]`**——
等于给每个没写 `required` 的 schema 都塞了一个触发器。

---

## 3. 社区侧对照实现

### `agent-vibes`（TS）—— 7 键白名单 + 重建
`[源码]` `apps/protocol-bridge/src/llm/.../translator.ts:898-906`
```
允许键：type / description / properties / required / items / enum / title
```
- `const` → 折叠为单元素 `enum`（`:932-934`）
- **重建对象**而非删键 → 保证任意嵌套深度不残留
- 注释 `:879-887` 说明根因：后端用 **Smithy** 校验，比 Anthropic 严格

### `tau`（TS）—— 同向但更细
`[源码]` `src/lanes/kiro/request.ts:272-275`：`required: []` 需移除整个键

### `maxx`（Go）—— 有清洗器但没用在 kiro 上
`[源码]` `internal/adapter/provider/bedrock/sanitizer.go:319-322` 有完整清洗，
**但 `internal/adapter/provider/kiro/` 没有复用** → 这是它自身的缺陷，不是范例

### 反例：`agent-vibes` 的工具名侧反而有缺陷
`[源码]` `translator.ts:974-985` 只截长度不洗字符集 → 含点号工具名会 400。
**说明没有哪个实现是全面的，只能按缺口逐条取长。**

---

## 4. 为什么这条比 G5（工具名）更重要

| 维度 | G5 工具名 | **G6 schema 关键字** |
|---|---|---|
| 触发频率 | 需工具名含非法字符（较少见） | **Claude Code 的 MCP schema 普遍带 `$schema`/`additionalProperties`/`default`** |
| 我们当前状态 | 无清洗（缺口） | **无过滤，且主动添加 2 个触发器**（更糟） |
| 证据等级 | `[官方]` 已结案 | `[本仓库]` 实测 + `[源码]`×2 |
| 影响 | 整个请求 400 | 同样整个请求 400 |

> **结论：G6 应排在 G5 之前实施。** 我此前把 G5 当作头号问题是不完整的——
> 那是"按仓库名检索"视野下的结论，扩大搜索后 G6 才是更高发的成因。

---

## 5. 改造注意事项（避免踩坑）

1. **必须重建而非删键**——社区明确指出要保证任意嵌套深度不残留。
   我们现有的递归入口 `normalizeKiroJSONSchemaValue` 结构可复用，
   把"全键拷贝"换成"白名单拷贝"即可，改动面很小。
2. **`required: []` 要移除整个键**，不能保留空数组 → 需改 `normalizeSchemaRequired` 的语义。
3. **`additionalProperties` 存疑**：`agent-vibes` 的白名单里**没有**它，
   而我们当前会主动设成 `true`。`[待验证]` 到底是"必须移除"还是"设 true 可接受"，
   两个社区实现都选择移除 → **安全侧是移除**。
4. **`const` → 单元素 `enum`**，保留语义。
5. ⚠️ **白名单会丢失语义**：`minimum`/`maximum`/`pattern` 等约束被删后，
   模型可能生成越界参数。这是**已知代价**，社区一致接受——
   因为"参数可能越界" << "整个请求 400"。文档需写明。

---

## 6. 测试用例

| ID | 层 | 用例 |
|---|---|---|
| A-12 | A | 7 个超纲键全部被移除（用本文件的输入做黄金样例） |
| A-13 | A | `const: "x"` → `enum: ["x"]` |
| A-14 | A | `required: []` → 该键被移除；`required: ["a"]` → 保留 |
| A-15 | A | 三层嵌套 `properties.a.properties.b` 深处的 `$schema` 也被移除 |
| A-17 | A | `items` 数组内的子 schema 同样被清洗 |
| A-18 | A | 合法 schema 不被破坏（回归保护） |
| B-10 | B | 归类器补两条串后，`Invalid tool use format` → `bad_request_schema` |

> **全部可在 A 层验证，零额度消耗。**

关联：[F05](F05-400-two-error-strings-and-schema.md)、[F01](F01-official-aws-service-model.md)

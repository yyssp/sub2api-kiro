package service

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 回给下游的响应必须是干净的 Anthropic Messages / OpenAI 协议，上游（以及转发
// 上游的中继）塞在 usage 里的私有字段一律不能透传出去。
//
// 2026-09-16 实测某 Kiro 中继：它在 message_start.usage 与 message_delta.usage 里
// 各带了一组私有字段 —— kiro_actual_input_tokens / kiro_billable_input_tokens /
// kiro_excluded_input_tokens / kiro_credits 以及四个 kiro_*_ms 耗时字段，全部被
// 我们原样转发给了客户端。这些字段有三重问题：
//  1. 它们不在 Anthropic 协议里，严格解析 usage 的客户端会报错；
//  2. kiro_* 的 token 口径是**计费值**而非真实 token（见
//     upstreamUsageIsBillingScale），暴露出去会让下游算出与 input/output 矛盾的数；
//  3. 它泄露了上游的真实供应商与折价比例。
//
// 这里刻意用「前缀黑名单」而不是「标准字段白名单」：Anthropic 自己会持续加字段
// （实测原生上游已在回 service_tier、inference_geo），白名单会把这些合法字段一起
// 剪掉，而且每加一个新字段都要改代码才能放行。

// privateUsageFieldKey 判断一个 usage 字段是不是不该出现在下游响应里的私有字段。
//
//	"_" 前缀   我们自己塞的内部标记，如 _sub2api_kiro_credits
//	"kiro" 前缀 上游/中继的 Kiro 私有字段（kiro_credits、kiroCredits、kiro_*_ms ...）
//	credits 系  少数中继用的无前缀积分字段，同样是私有计费信息
func privateUsageFieldKey(key string) bool {
	if key == "" {
		return false
	}
	if strings.HasPrefix(key, "_") {
		return true
	}
	lower := strings.ToLower(key)
	if strings.HasPrefix(lower, "kiro") {
		return true
	}
	switch lower {
	case "credits", "creditsused", "creditusage":
		return true
	}
	return false
}

// sanitizeUsageMapForClient 删除 map 形态 usage 里的私有字段，返回是否改动过
// （调用方据此决定要不要重新序列化事件）。
func sanitizeUsageMapForClient(usage map[string]any) bool {
	changed := false
	for key := range usage {
		if privateUsageFieldKey(key) {
			delete(usage, key)
			changed = true
		}
	}
	return changed
}

// sanitizeEventUsageForClient 清理一个已解析的 SSE 事件：message_start 的 usage 挂在
// message 下，message_delta 的挂在事件顶层，两处都要过一遍。
func sanitizeEventUsageForClient(event map[string]any) bool {
	changed := false
	if msg, ok := event["message"].(map[string]any); ok {
		if usage, ok := msg["usage"].(map[string]any); ok {
			changed = sanitizeUsageMapForClient(usage) || changed
		}
	}
	if usage, ok := event["usage"].(map[string]any); ok {
		changed = sanitizeUsageMapForClient(usage) || changed
	}
	return changed
}

// usagePrivateFieldPaths 是 JSON 文本形态下 usage 对象可能出现的位置：
// 非流式响应体与 message_delta 是 "usage"，message_start 是 "message.usage"。
var usagePrivateFieldPaths = [...]string{"usage", "message.usage"}

// stripPrivateUsageFieldsFromJSON 删除 JSON 文本里 usage 下的私有字段。
// 没有可删字段时原样返回，保持上游字节不变。
func stripPrivateUsageFieldsFromJSON(data string) string {
	if !strings.Contains(data, "usage") {
		return data
	}
	out := data
	for _, base := range usagePrivateFieldPaths {
		node := gjson.Get(out, base)
		if !node.IsObject() {
			continue
		}
		node.ForEach(func(key, _ gjson.Result) bool {
			name := key.String()
			if !privateUsageFieldKey(name) {
				return true
			}
			if next, err := sjson.Delete(out, base+"."+escapeGJSONPathKey(name)); err == nil {
				out = next
			}
			return true
		})
	}
	return out
}

// stripPrivateUsageFieldsFromJSONBytes 是 []byte 版本，非流式响应体走这条。
func stripPrivateUsageFieldsFromJSONBytes(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	cleaned := stripPrivateUsageFieldsFromJSON(string(body))
	if len(cleaned) == len(body) {
		return body
	}
	return []byte(cleaned)
}

// escapeGJSONPathKey 转义 gjson/sjson 路径里的元字符。上游字段名理论上不会带
// '.'、'*'、'?'，但字段名来自上游，不转义就等于让上游决定我们删哪个 key。
func escapeGJSONPathKey(key string) string {
	if !strings.ContainsAny(key, ".*?\\") {
		return key
	}
	var b strings.Builder
	b.Grow(len(key) + 4)
	for _, r := range key {
		switch r {
		case '.', '*', '?', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

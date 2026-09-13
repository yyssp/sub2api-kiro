package cursor

// Claude Code 请求进入 Cursor 协议层之前的工具集塑形。
//
// 移植自 ai2api/internal/cursorpool/anthropic.go —— 该文件是 ai2api 的 HTTP
// 入口（整体不移植，调度/号池/重试编排交给 sub2api 自有体系），但下面这两段
// 是**协议语义**而非账号管理：它们决定发给 Cursor 的 tools 数组长什么样，
// 直接影响上游会返回哪些工具分支。漏掉会导致请求中途夭折或客户端侧硬卡死。

import "strings"

// ClaudeCodeContextExpansionConstraint 告诉上游模型：本地扩展工具本轮不可用。
//
// 没有这段说明时，模型会反复调用 Bash/ToolSearch 去重新发现一个已被抑制的
// Skill，形成不推进的协议空转——大块 skill 正文虽然没转发，循环照样发生。
const ClaudeCodeContextExpansionConstraint = "[代理协议约束] 本次请求通过 Cursor 协议桥接，本地 Skill、ToolSearch、DeferredToolPlaceholder 不会转发给上游，也不会在本轮可用。不要搜索、请求或重复加载这些本地扩展；请直接使用当前已声明的 Read、Bash、Edit、MCP、Agent/Task 等工具继续完成用户任务。若用户要求加载 skill，请说明本地资料未转发，但仍继续分析，不要停在计划或反复查找。"

// MergeClaudeCodeCoreTools 在 Claude Code 请求里补齐 Cursor 侧仍需兜底的核心工具。
//
// ⚠️ 为什么必须补：Cursor 上游会返回 shell/read 等原生工具分支。若本轮
// tools 里没有对应定义，resolveClientTool 匹配不到，agent.go 会抛
// ErrUndeclaredUpstreamTool，整轮中途夭折。inferClaudeAgentTool 只兜底了
// Agent/Task，Bash/Read 没有任何兜底。
//
// ⚠️ 为什么只补 Bash/Read/Agent（claudeCodeCoreToolDefsForMerge）：
// Claude Code 按 --tools、权限和 deferred 状态动态声明工具。把 LS/Glob/Grep/
// Delete 等客户端本轮没声明的工具伪造进去，上游一旦选中，CLI 会以
// "No such tool available" 拒绝整轮——比不补更糟。
//
// ⚠️ 为什么要求 ≥2 个 marker 工具（shouldSeedClaudeCodeCoreToolSet）：
// 这是「本轮是携带动态工具目录的主 Claude Code 请求」的判据。子 Agent、
// 受限权限上下文、显式 --tools Read/Task 等集合必须原样保留。
//
// ⚠️ 调用顺序：必须在 SuppressClaudeCodeContextExpansionTools **之前**调用。
// 抑制会摘掉 Skill/ToolSearch/DeferredToolPlaceholder，而它们正是
// claudeCodeToolMarkerCount 计数的 marker；顺序反了 marker 数会掉到 2 以下，
// 补齐静默失效。
//
// 返回 changed=false 时 tools 原样返回，调用方无需改写。
func MergeClaudeCodeCoreTools(model string, tools []ToolDef) (merged []ToolDef, changed bool) {
	// 纯文本请求（无工具）必须保持原样：那是 direct Sand 路径，
	// 凭空加工具会改变上游的应答形态。
	if len(tools) == 0 {
		return tools, false
	}
	if !isClaudeCodeModelName(StripPrefix(model)) && !isClaudeCodeAutoModelName(model) {
		return tools, false
	}
	// 没有桥接工具说明这不是 Claude Code 的工具型请求。
	if claudeCodeToolBridgeCount(tools) == 0 {
		return tools, false
	}
	if claudeCodeToolMarkerCount(tools) < 2 {
		return tools, false
	}

	out := append([]ToolDef(nil), tools...)
	seen := make(map[string]bool, len(out))
	for _, td := range out {
		if name := strings.ToLower(strings.TrimSpace(td.Name)); name != "" {
			seen[name] = true
		}
	}
	for _, td := range claudeCodeCoreToolDefsForMerge() {
		name := strings.ToLower(strings.TrimSpace(td.Name))
		if name == "" || seen[name] {
			continue
		}
		out = append(out, td)
		seen[name] = true
		changed = true
	}
	if !changed {
		return tools, false
	}
	return out, true
}

// SuppressClaudeCodeContextExpansionTools 摘掉由 Claude Code 本地展开的元工具。
//
// ⚠️ 为什么必须摘 Skill：Skill 的执行结果是把整棵 skill 目录树展开进 CLI 的
// 上下文。把这个工具透给 Cursor，模型一旦调用，**下一轮请求会在客户端侧**
// 就被判定 "Prompt is too long"——会话硬卡死，而网关侧看不到任何线索，
// 排查成本极高。
//
// ToolSearch / DeferredToolPlaceholder 必须与 Skill 一起摘：只摘 Skill 的话，
// 模型会反复 ToolSearch 去找一个永远拿不到的工具，空转不推进。
//
// 功能性工具（Read/Bash/Edit/MCP/Agent/Task）一律保留并继续桥接。
//
// 返回的 suppressed 非空时，调用方必须同时调用
// AppendClaudeCodeContextExpansionConstraint 注入说明文案。
func SuppressClaudeCodeContextExpansionTools(model string, tools []ToolDef) (filtered []ToolDef, suppressed []string) {
	if len(tools) == 0 {
		return tools, nil
	}
	if !isClaudeCodeModelName(StripPrefix(model)) && !isClaudeCodeAutoModelName(model) {
		return tools, nil
	}
	out := make([]ToolDef, 0, len(tools))
	for _, td := range tools {
		name := strings.TrimSpace(td.Name)
		switch strings.ToLower(name) {
		case "skill", "toolsearch", "deferredtoolplaceholder":
			suppressed = append(suppressed, name)
			continue
		}
		out = append(out, td)
	}
	if len(suppressed) == 0 {
		return tools, nil
	}
	return out, suppressed
}

// AppendClaudeCodeContextExpansionConstraint 把约束文案前置到首条消息。
// 已经包含时原样返回，避免多轮累积重复注入。
func AppendClaudeCodeContextExpansionConstraint(msgs []ChatMessage) []ChatMessage {
	if len(msgs) == 0 {
		return []ChatMessage{{Role: "user", Content: ClaudeCodeContextExpansionConstraint}}
	}
	if strings.Contains(msgs[0].Content, ClaudeCodeContextExpansionConstraint) {
		return msgs
	}
	out := append([]ChatMessage(nil), msgs...)
	out[0].Content = ClaudeCodeContextExpansionConstraint + "\n\n" + out[0].Content
	return out
}

// isClaudeCodeAutoModelName 识别 Cursor 的 Auto/default 伪模型名。
// Free 套餐只能用它们，但请求本身仍然是 Claude Code 发出的。
func isClaudeCodeAutoModelName(model string) bool {
	switch strings.ToLower(strings.TrimSpace(StripPrefix(model))) {
	case "auto", "default":
		return true
	}
	return false
}

package cursor

import (
	"sort"
	"strings"
)

// agent.v1 服务端会在 Run 流里向客户端「提问」：exec_server_message(2)、
// kv_server_message(4)、interaction_query(7)。三者都要求客户端按 id 回话，
// 不回话上游就一直挂着只发心跳——下游看到的是 90s 首字超时、换号、下一个号
// 再卡 90s，整池被慢慢烧光（社区实证：opencodex live-transport.ts:309-312
// 「unanswered query is the proven cause of the heartbeat-only stall」；
// cpa-cursor exec.go:44-46「the server blocks on silence」）。
//
// 本文件只负责「怎么答」；什么时候答、答完怎么继续由 consumeAgentResponse 决定。
// 所有字段号均已与 opencodex / greenSheep / cpa-cursor 三份独立 proto 逐一核对。

// mcpProviderIdentifier 是客户端工具在 agent.v1 里的 MCP provider/server 标识。
// run_request.mcp_tools、mcp_state 应答必须用同一个值，否则上游会认为
// mcp_state 里报告的服务器与 run 里声明的工具不是一回事。
const mcpProviderIdentifier = "claude-local"

// AgentClientMessage 顶层字段号。
const (
	agentClientExecMessage        = 2
	agentClientKVMessage          = 3
	agentClientExecControlMessage = 5
	agentClientInteractionResp    = 6
)

// ExecServerMessage 中不属于 oneof 分支的公共字段：id、exec_id、span_context、
// accept_hook_additional_contexts、machine_id。找分支时必须跳过它们。
var execServerCommonFields = map[int]bool{1: true, 15: true, 19: true, 55: true, 57: true}

// bridgedNativeExecBranches 是「刻意不应答」的原生工具 exec 分支。
//
// ⚠️ 这些分支对应的工具调用会同时以 interaction_update.tool_call_started 的形式
// 到达，由 parseInteractionToolUpdatesWithContext 映射成 Claude Code 的客户端
// 工具，随后以 tool-call-ready 结束本轮、交给下游执行。若在这里抢先回一个
// throw，上游会把该工具判为失败并让模型继续往下编，下游拿到的 tool_use
// 与上游认知就对不上了。所以只对「我们不桥接」的分支 throw。
//
//	shell(2) write(3) delete(4) grep(5) read(7) ls(8) diagnostics(9)
//	shell_stream(14) subagent(28) redacted_read(29)
var bridgedNativeExecBranches = map[int]bool{2: true, 3: true, 4: true, 5: true, 7: true, 8: true, 9: true, 14: true, 28: true, 29: true}

// agentServerResponder 应答一次 Run 内服务端发来的请求。生命周期 = 一次 Run，
// 不跨请求共享（KV 只在本 Run 内有意义，跨 Run 复用会把别人的 blob 喂给上游）。
type agentServerResponder struct {
	toolDefs  [][]byte // 已编码的 McpToolDefinition，与 run_request.mcp_tools 同源
	toolNames []string
	blobs     map[string][]byte
	blobBytes int
}

// KV 容量上限。上游实测单个 conversation 的服务端配额约 4096 个 / 64MiB
// （YeautyYE conversation.rs:25-44），我们每轮都是新 Run 且 conversation_state
// 为空，正常流量远碰不到；上限只用来防止异常上游把网关内存撑爆。
const (
	agentKVMaxBlobs = 1024
	agentKVMaxBytes = 64 << 20
)

func newAgentServerResponder(tools []ToolDef) *agentServerResponder {
	r := &agentServerResponder{blobs: map[string][]byte{}}
	for _, tool := range tools {
		r.toolDefs = append(r.toolDefs, encodeMCPToolDefinition(tool))
		r.toolNames = append(r.toolNames, toolWireName(tool))
	}
	return r
}

// execBranch 返回 ExecServerMessage 的 oneof 分支（第一个非公共字段）。
func execBranch(execParts []pbPart) (pbPart, bool) {
	for _, part := range execParts {
		if !execServerCommonFields[part.Num] {
			return part, true
		}
	}
	return pbPart{}, false
}

// execEnvelope 编码 ExecClientMessage 的 id(1) / exec_id(15)，原样回传。
func execEnvelope(execParts []pbPart) []byte {
	var out []byte
	if id, ok := pbFirst(execParts, 1); ok && id.Wire == 0 {
		out = append(out, pbField(1, 0, pbVarint(id.Value))...)
	}
	if execID, ok := pbFirst(execParts, 15); ok && execID.Wire == 2 {
		out = append(out, pbBytes(15, execID.Data)...)
	}
	return out
}

func execResult(execParts []pbPart, field int, result []byte) []byte {
	msg := execEnvelope(execParts)
	msg = append(msg, pbBytes(field, result)...)
	return pbBytes(agentClientExecMessage, msg)
}

func execID(execParts []pbPart) uint64 {
	if id, ok := pbFirst(execParts, 1); ok && id.Wire == 0 {
		return id.Value
	}
	return 0
}

// execStreamClose: AgentClientMessage{ exec_client_control_message(5){ stream_close(1){ id(1) } } }
func execStreamClose(id uint64) []byte {
	return pbBytes(agentClientExecControlMessage, pbBytes(1, pbField(1, 0, pbVarint(id))))
}

// execThrow: AgentClientMessage{ exec_client_control_message(5){ throw(2){ id(1), error(2) } } }
func execThrow(id uint64, text string) []byte {
	body := pbField(1, 0, pbVarint(id))
	body = append(body, pbString(2, text)...)
	return pbBytes(agentClientExecControlMessage, pbBytes(2, body))
}

// unavailableText 是拒绝原生工具时的文案。只说「不可用」会让模型放弃任务；
// 点名客户端工具才能让它换用（cpa-cursor exec.go:85-94 的实测结论）。
func (r *agentServerResponder) unavailableText() string {
	if len(r.toolNames) == 0 {
		return "Tool not available in this client, and the client offers no tools: answer without tools."
	}
	names := append([]string(nil), r.toolNames...)
	sort.Strings(names)
	return "Tool not available in this client. Use the client's own tools instead (MCP server \"" +
		mcpProviderIdentifier + "\"): " + strings.Join(names, ", ") + "."
}

// handleExec 返回需要按顺序发出的 AgentClientMessage 列表，以及用于日志的分支名。
// 返回空列表表示刻意不应答（桥接给下游的原生工具 / 客户端 MCP 调用）。
func (r *agentServerResponder) handleExec(execParts []pbPart) ([][]byte, string) {
	branch, ok := execBranch(execParts)
	if !ok {
		id := execID(execParts)
		return [][]byte{execThrow(id, "Unknown exec message variant"), execStreamClose(id)}, "empty"
	}
	switch branch.Num {
	case 10: // request_context_args → request_context_result(10)
		return [][]byte{buildRequestContextResult(execParts)}, "request_context"
	case 36: // mcp_state_exec_args → mcp_state_exec_result(36)
		return [][]byte{execResult(execParts, 36, r.mcpStateSuccess(branch.Data))}, "mcp_state"
	case 17: // list_mcp_resources_exec_args → success(1){}
		return [][]byte{execResult(execParts, 17, pbBytes(1, nil))}, "list_mcp_resources"
	case 18: // read_mcp_resource_exec_args → not_found(4){ uri(1) }
		uri := ""
		if args, err := pbParse(branch.Data); err == nil {
			if u, ok := pbFirst(args, 2); ok && u.Wire == 2 {
				uri = string(u.Data)
			}
		}
		return [][]byte{execResult(execParts, 18, pbBytes(4, pbString(1, uri)))}, "read_mcp_resource"
	case 11: // mcp_args
		// 权限探测(smart_mode_approval_only=7)不是真正的调用，批准即可，真正的调用
		// 随后作为独立 exec 到达（cpa-cursor session.go onMcpArgs）。不批准则真调用永远不来。
		if args, err := pbParse(branch.Data); err == nil {
			if probe, ok := pbFirst(args, 7); ok && probe.Wire == 0 && probe.Value != 0 {
				id := execID(execParts)
				return [][]byte{execResult(execParts, 11, pbBytes(7, nil)), execStreamClose(id)}, "mcp_approval_probe"
			}
		}
		// 真实的客户端工具调用：由 interaction_update 的 tool-call-ready 交给下游执行。
		return nil, "mcp_call"
	case 41, 42, 43:
		// shell / mcp / web_fetch allowlist precheck → allowlisted(1)=true。
		// ⚠️ 不能答 false：放行后上游才会发出真正的 exec，而 shell 正是要桥接给
		// 下游 Bash 执行的；web_fetch 放行后紧接着的 fetch_args 会被下面的 throw
		// 拒绝。答 false 会让上游转去走审批交互，凭空多出一条挂起路径。
		return [][]byte{execResult(execParts, branch.Num, pbField(1, 0, pbVarint(1)))}, "allowlist_precheck"
	}
	if bridgedNativeExecBranches[branch.Num] {
		return nil, "bridged_native"
	}
	id := execID(execParts)
	return [][]byte{execThrow(id, r.unavailableText()), execStreamClose(id)}, "refused"
}

// mcpStateSuccess: success(1){ servers(1){ server_name(1), server_identifier(2), tools(5)... } }
// 上游按 server_identifiers(1) 过滤时只报告被问到的服务器。
func (r *agentServerResponder) mcpStateSuccess(args []byte) []byte {
	wanted := []string{}
	if parts, err := pbParse(args); err == nil {
		for _, part := range parts {
			if part.Num == 1 && part.Wire == 2 {
				wanted = append(wanted, string(part.Data))
			}
		}
	}
	include := len(wanted) == 0
	for _, w := range wanted {
		if w == mcpProviderIdentifier {
			include = true
		}
	}
	var success []byte
	if include {
		server := pbString(1, mcpProviderIdentifier)
		server = append(server, pbString(2, mcpProviderIdentifier)...)
		for _, def := range r.toolDefs {
			server = append(server, pbBytes(5, def)...)
		}
		success = pbBytes(1, server)
	}
	return pbBytes(1, success)
}

// handleKV 应答 kv_server_message。未知分支返回 nil（与 cpa-cursor 一致：不认识就不答）。
//
//	KvServerMessage{ id(1), get_blob_args(2){ blob_id(1) } | set_blob_args(3){ blob_id(1), blob_data(2) } }
//	KvClientMessage{ id(1), get_blob_result(2){ blob_data(1)? } | set_blob_result(3){ error(1)? } }
func (r *agentServerResponder) handleKV(data []byte) ([]byte, string) {
	parts, err := pbParse(data)
	if err != nil {
		return nil, "malformed"
	}
	var reply []byte
	if id, ok := pbFirst(parts, 1); ok && id.Wire == 0 {
		reply = pbField(1, 0, pbVarint(id.Value))
	}
	if get, ok := pbFirst(parts, 2); ok {
		var result []byte
		if args, err := pbParse(get.Data); err == nil {
			if blobID, ok := pbFirst(args, 1); ok {
				if blob, ok := r.blobs[string(blobID.Data)]; ok {
					result = pbBytes(1, blob)
				}
			}
		}
		reply = append(reply, pbBytes(2, result)...)
		return pbBytes(agentClientKVMessage, reply), "get_blob"
	}
	if set, ok := pbFirst(parts, 3); ok {
		var result []byte
		if args, err := pbParse(set.Data); err == nil {
			blobID, _ := pbFirst(args, 1)
			blobData, _ := pbFirst(args, 2)
			key := string(blobID.Data)
			old, exists := r.blobs[key]
			nextBytes := r.blobBytes - len(old) + len(blobData.Data)
			nextCount := len(r.blobs)
			if !exists {
				nextCount++
			}
			if nextCount > agentKVMaxBlobs || nextBytes > agentKVMaxBytes {
				// 满了回 error 而不是静默丢弃：上游随后 get 不到时至少知道是写失败。
				result = pbBytes(1, pbString(1, "client blob store is full"))
			} else {
				r.blobs[key] = blobData.Data
				r.blobBytes = nextBytes
			}
		}
		reply = append(reply, pbBytes(3, result)...)
		return pbBytes(agentClientKVMessage, reply), "set_blob"
	}
	return nil, "unknown"
}

// interactionRejectReason 是拒绝交互请求的统一文案。
const interactionRejectReason = "Not supported by this client"

// handleQuery 应答 interaction_query：按同类型拒绝。协议没有拒绝分支的类型用
// 最接近的否定结果；完全不认识的类型只回 id，至少能解除该 id 的阻塞
// （cpa-cursor exec.go:145-195）。
//
//	InteractionQuery{ id(1), <oneof> }  →  InteractionResponse{ id(1), <同号 oneof> }
func (r *agentServerResponder) handleQuery(data []byte) ([]byte, string) {
	parts, err := pbParse(data)
	if err != nil {
		return nil, "malformed"
	}
	var resp []byte
	if id, ok := pbFirst(parts, 1); ok && id.Wire == 0 {
		resp = pbField(1, 0, pbVarint(id.Value))
	}
	reason := pbString(1, interactionRejectReason)
	kind := "unknown"
	for _, part := range parts {
		if part.Num == 1 {
			continue
		}
		var result []byte
		switch part.Num {
		case 2, 4, 9, 11, 12, 14:
			// web_search / switch_mode / web_fetch / mcp_auth / generate_image / connect_scm:
			// { rejected(2){ reason(1) } }
			result = pbBytes(2, reason)
		case 3: // ask_question: { result(1){ rejected(3){ reason(1) } } }
			result = pbBytes(1, pbBytes(3, reason))
		case 7: // create_plan: { result(1){ error(2){ error(1) } } }
			result = pbBytes(1, pbBytes(2, reason))
		case 10: // pr_management: { rejected(3){ reason(1) } }
			result = pbBytes(3, reason)
		case 13: // replace_env: { failure(2){ error_message(1) } }
			result = pbBytes(2, reason)
		case 8: // setup_vm_environment: 只有 success 分支，回空结果
			result = nil
		default:
			continue
		}
		resp = append(resp, pbBytes(part.Num, result)...)
		kind = interactionQueryName(part.Num)
		break
	}
	return pbBytes(agentClientInteractionResp, resp), kind
}

func interactionQueryName(num int) string {
	switch num {
	case 2:
		return "web_search"
	case 3:
		return "ask_question"
	case 4:
		return "switch_mode"
	case 7:
		return "create_plan"
	case 8:
		return "setup_vm_environment"
	case 9:
		return "web_fetch"
	case 10:
		return "pr_management"
	case 11:
		return "mcp_auth"
	case 12:
		return "generate_image"
	case 13:
		return "replace_env"
	case 14:
		return "connect_scm"
	default:
		return "unknown"
	}
}

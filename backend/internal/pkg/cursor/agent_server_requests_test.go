package cursor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件钉住 agent.v1 Run 流里「服务端向客户端提问」与「本轮怎样才算结束」两件事。
//
// 测试走真实的 RunSSE + BidiAppend 回退路径（HTTP/1.1，httptest 即可承载），
// 用一个按脚本吐帧的假上游：每个用例声明上游依次发什么帧、在什么条件下停住，
// 再断言网关回了什么、以什么理由结束。比对的是线上字节，而不是内部函数的返回值。

// ---- 帧构造 ----

func agentServerFrame(field int, body []byte) []byte {
	return wrapFrame(pbBytes(field, body))
}

func iuFrame(updates ...[]byte) []byte {
	var body []byte
	for _, u := range updates {
		body = append(body, u...)
	}
	return agentServerFrame(1, body)
}

func iuText(text string) []byte     { return pbBytes(1, pbString(1, text)) }
func iuThinking(text string) []byte { return pbBytes(4, pbString(1, text)) }
func iuHeartbeat() []byte           { return pbBytes(13, nil) }
func iuTurnEnded(fields ...[2]uint64) []byte {
	var body []byte
	for _, f := range fields {
		body = append(body, pbField(int(f[0]), 0, pbVarint(f[1]))...)
	}
	return pbBytes(14, body)
}
func iuServerNotice(text string) []byte {
	return pbBytes(1, append(pbString(1, text), pbField(2, 0, pbVarint(1))...))
}

func iuMCPToolStarted(callID, wireName, argKey, argValue string) []byte {
	value := pbBytes(3, []byte(argValue)) // google.protobuf.Value.string_value = 3
	entry := append(pbString(1, argKey), pbBytes(2, value)...)
	args := append(pbString(1, wireName), pbBytes(2, entry)...)
	mcp := pbBytes(1, args)
	toolCall := pbBytes(15, mcp)
	started := append(pbString(1, callID), pbBytes(2, toolCall)...)
	return pbBytes(2, started)
}

func iuToolRequestsListed(n uint64) []byte {
	return pbBytes(27, pbField(1, 0, pbVarint(n)))
}

func execFrame(id uint64, execID string, branch int, args []byte) []byte {
	body := pbField(1, 0, pbVarint(id))
	body = append(body, pbString(15, execID)...)
	body = append(body, pbBytes(branch, args)...)
	return agentServerFrame(2, body)
}

// ---- 假上游 ----

type scriptStep struct {
	frame []byte
	// waitAppends>0：先等网关累计发来这么多条 BidiAppend 再写本帧。
	// 用来证明「上游在等回答」——不回答，后续帧就永远不来。
	waitAppends int
	pause       time.Duration
}

type fakeAgentUpstream struct {
	t       *testing.T
	mu      sync.Mutex
	appends [][]byte // 每条 BidiAppend 的 AgentClientMessage（data_binary=4）
	notify  chan struct{}
	script  []scriptStep
	// hold=true：脚本放完后不关流，模拟只剩连接不结束的上游。
	hold bool
}

func newFakeAgentUpstream(t *testing.T, hold bool, script ...scriptStep) *fakeAgentUpstream {
	return &fakeAgentUpstream{t: t, notify: make(chan struct{}, 64), script: script, hold: hold}
}

func (f *fakeAgentUpstream) appendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.appends)
}

func (f *fakeAgentUpstream) clientMessages() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.appends...)
}

func (f *fakeAgentUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case runURL:
		// 强制走 RunSSE 回退：HTTP/1.1 下 Run 双工不可用。
		w.WriteHeader(http.StatusNotImplemented)
	case bidiAppendURL:
		body, _ := io.ReadAll(r.Body)
		parts, err := pbParse(body)
		if err != nil {
			f.t.Errorf("BidiAppend body not protobuf: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if data, ok := pbFirst(parts, 4); ok {
			f.mu.Lock()
			f.appends = append(f.appends, data.Data)
			f.mu.Unlock()
			select {
			case f.notify <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	case runSSEURL:
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			f.t.Error("test server must support flushing")
			return
		}
		flusher.Flush()
		for _, step := range f.script {
			deadline := time.After(3 * time.Second)
			for step.waitAppends > 0 && f.appendCount() < step.waitAppends {
				select {
				case <-f.notify:
				case <-deadline:
					// 网关没有回答：上游按真实行为一直挂着，直到客户端放弃。
					<-r.Context().Done()
					return
				case <-r.Context().Done():
					return
				}
			}
			if step.pause > 0 {
				select {
				case <-time.After(step.pause):
				case <-r.Context().Done():
					return
				}
			}
			if _, err := w.Write(step.frame); err != nil {
				return
			}
			flusher.Flush()
		}
		if f.hold {
			<-r.Context().Done()
		}
	default:
		http.NotFound(w, r)
	}
}

type agentRunResult struct {
	text      string
	reasoning string
	tools     []ToolCall
	produced  bool
	err       error
	usage     *UpstreamUsage
}

func runAgentAgainst(t *testing.T, up *fakeAgentUpstream, timeout time.Duration, in AgentRequest) agentRunResult {
	t.Helper()
	server := httptest.NewServer(up)
	t.Cleanup(server.Close)
	ConfigureUpstream(server.URL, "")
	t.Cleanup(func() { ConfigureUpstream("", "") })

	client := NewClient()
	client.shared = server.Client()
	client.agentTimeout = timeout
	client.firstToken = timeout

	if in.Model == "" {
		// 非 Claude Code 名称：走 AgentService 而不是 Sand 直连。
		in.Model = "gpt-5.5"
	}
	var res agentRunResult
	in.OnUpstreamUsage = func(u UpstreamUsage) { res.usage = &u }
	var text, reasoning strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	res.produced, res.err = client.RunAgentStream(ctx, &Account{AccessToken: "test-token"}, in,
		func(s string) { _, _ = text.WriteString(s) },
		func(s string) { _, _ = reasoning.WriteString(s) },
		func(tc ToolCall) { res.tools = append(res.tools, tc) })
	res.text, res.reasoning = text.String(), reasoning.String()
	return res
}

// ---- 解码网关发出的客户端帧 ----

type decodedClientMessage struct {
	field int
	body  []pbPart
}

func decodeClientMessage(t *testing.T, raw []byte) decodedClientMessage {
	t.Helper()
	parts, err := pbParse(raw)
	if err != nil || len(parts) != 1 {
		t.Fatalf("client message must be exactly one oneof field, got %d parts err=%v", len(parts), err)
	}
	body, err := pbParse(parts[0].Data)
	if err != nil {
		t.Fatalf("client message body not protobuf: %v", err)
	}
	return decodedClientMessage{field: parts[0].Num, body: body}
}

func mustSub(t *testing.T, parts []pbPart, num int) []pbPart {
	t.Helper()
	p, ok := pbFirst(parts, num)
	if !ok {
		t.Fatalf("missing field %d in %s", num, describePBParts(parts, 0))
	}
	sub, err := pbParse(p.Data)
	if err != nil {
		t.Fatalf("field %d not a message: %v", num, err)
	}
	return sub
}

// ======== 结束判定 ========

// 正文后只要不是 turn_ended / trailer / 工具就绪，就不是正常结束。
// 曾经「首字后 30s 无帧 = end_turn」，断流被伪装成完整回复，Claude Code 不会重试。
func TestAgentStream_TextThenEOFIsIncompleteNotSuccess(t *testing.T) {
	up := newFakeAgentUpstream(t, false,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: iuFrame(iuText("partial answer")), waitAppends: 2},
	)
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "hi"})
	if !errors.Is(res.err, ErrIncompleteUpstreamStream) {
		t.Fatalf("EOF after text must be ErrIncompleteUpstreamStream, got %v", res.err)
	}
	if res.text != "partial answer" {
		t.Fatalf("text=%q", res.text)
	}
}

func TestAgentStream_TurnEndedFinishesAndReportsUpstreamUsage(t *testing.T) {
	up := newFakeAgentUpstream(t, true, // 结束后连接仍挂着心跳：必须靠 turn_ended 主动收尾
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: iuFrame(iuThinking("plan first")), waitAppends: 2},
		scriptStep{frame: iuFrame(iuText("done"))},
		scriptStep{frame: iuFrame(iuTurnEnded([2]uint64{1, 13126}, [2]uint64{2, 296}, [2]uint64{3, 4675}, [2]uint64{5, 40}))},
		scriptStep{frame: iuFrame(iuHeartbeat())},
	)
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "hi"})
	if res.err != nil || res.text != "done" || res.reasoning != "plan first" {
		t.Fatalf("turn_ended must finish cleanly: text=%q reasoning=%q err=%v", res.text, res.reasoning, res.err)
	}
	if res.usage == nil || !res.usage.Present {
		t.Fatal("turn_ended usage must be surfaced for reconciliation")
	}
	want := UpstreamUsage{InputTokens: 13126, OutputTokens: 296, CacheReadTokens: 4675, ReasoningTokens: 40, Present: true}
	if *res.usage != want {
		t.Fatalf("usage=%+v want %+v", *res.usage, want)
	}
}

// shortenAgentGaps 把静默窗口缩到测试尺度，并在用例结束时还原。
func shortenAgentGaps(t *testing.T, liveness, progress time.Duration) {
	t.Helper()
	oldLive, oldReason, oldProgress := agentLivenessGap, agentReasoningLivenessGap, agentProgressGap
	agentLivenessGap, agentReasoningLivenessGap, agentProgressGap = liveness, liveness, progress
	t.Cleanup(func() {
		agentLivenessGap, agentReasoningLivenessGap, agentProgressGap = oldLive, oldReason, oldProgress
	})
}

// 上游只剩心跳、不再推进：社区实证的挂起形态。心跳会不断重置传输计时器，
// 所以必须由「进展」计时器判死；正文后出现时报不完整流，而不是被心跳续命到
// 600s 硬上限，更不能被当成 end_turn。
func TestAgentStream_HeartbeatOnlyAfterTextIsIncomplete(t *testing.T) {
	shortenAgentGaps(t, time.Second, 400*time.Millisecond)
	steps := []scriptStep{
		{frame: execFrame(1, "ctx", 10, nil)},
		{frame: iuFrame(iuText("half")), waitAppends: 2},
	}
	for i := 0; i < 200; i++ {
		steps = append(steps, scriptStep{frame: iuFrame(iuHeartbeat()), pause: 20 * time.Millisecond})
	}
	up := newFakeAgentUpstream(t, true, steps...)
	start := time.Now()
	res := runAgentAgainst(t, up, 30*time.Second, AgentRequest{Message: "hi"})
	if !errors.Is(res.err, ErrIncompleteUpstreamStream) {
		t.Fatalf("heartbeat-only tail after text must be an incomplete stream, got %v", res.err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("progress timer must fire despite heartbeats, took %v (heartbeats kept it alive?)", elapsed)
	}
}

// 正文后连接彻底无声。曾经这被当成「轮次结束」直接 end_turn。
func TestAgentStream_SilenceAfterTextIsIncompleteNotEndTurn(t *testing.T) {
	shortenAgentGaps(t, 300*time.Millisecond, 10*time.Second)
	up := newFakeAgentUpstream(t, true,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: iuFrame(iuText("cut off mid")), waitAppends: 2},
	)
	res := runAgentAgainst(t, up, 30*time.Second, AgentRequest{Message: "hi"})
	if !errors.Is(res.err, ErrIncompleteUpstreamStream) {
		t.Fatalf("silence after text must be an incomplete stream, not a normal end: %v", res.err)
	}
}

// ======== 服务端请求必须作答 ========

// interaction_query 不回答，上游就停在原地只发心跳（opencodex 实证的 502 循环根因）。
func TestAgentStream_AnswersInteractionQueryWithTypedRejection(t *testing.T) {
	query := pbField(1, 0, pbVarint(7))
	query = append(query, pbBytes(2, pbString(1, "golang"))...) // web_search_request_query
	up := newFakeAgentUpstream(t, false,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: agentServerFrame(7, query), waitAppends: 2},
		// 只有收到第 3 条 append（即对 query 的回答）才继续出字。
		scriptStep{frame: iuFrame(iuText("answered")), waitAppends: 3},
		scriptStep{frame: iuFrame(iuTurnEnded())},
	)
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "search"})
	if res.err != nil || res.text != "answered" {
		t.Fatalf("query must be answered so the turn can continue: text=%q err=%v", res.text, res.err)
	}
	msgs := up.clientMessages()
	reply := decodeClientMessage(t, msgs[2])
	if reply.field != agentClientInteractionResp {
		t.Fatalf("query reply must be interaction_response(6), got field %d", reply.field)
	}
	if id, ok := pbFirst(reply.body, 1); !ok || id.Value != 7 {
		t.Fatalf("interaction_response must echo query id 7, got %+v", id)
	}
	rejected := mustSub(t, mustSub(t, reply.body, 2), 2)
	if reason, ok := pbFirst(rejected, 1); !ok || string(reason.Data) != interactionRejectReason {
		t.Fatalf("web_search must be rejected with reason, got %s", describePBParts(rejected, 0))
	}
}

func TestAgentStream_AnswersMCPStateWithDeclaredTools(t *testing.T) {
	stateArgs := pbString(1, mcpProviderIdentifier)
	up := newFakeAgentUpstream(t, false,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: execFrame(2, "state", 36, stateArgs), waitAppends: 2},
		scriptStep{frame: iuFrame(iuText("ok")), waitAppends: 3},
		scriptStep{frame: iuFrame(iuTurnEnded())},
	)
	tools := []ToolDef{{Name: "Read", Description: "read a file", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}}}`}}
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "hi", Tools: tools})
	if res.err != nil {
		t.Fatalf("mcp_state must be answered: %v", res.err)
	}
	reply := decodeClientMessage(t, up.clientMessages()[2])
	if reply.field != agentClientExecMessage {
		t.Fatalf("mcp_state reply must be exec_client_message, got %d", reply.field)
	}
	if id, ok := pbFirst(reply.body, 1); !ok || id.Value != 2 {
		t.Fatal("exec reply must echo id")
	}
	if eid, ok := pbFirst(reply.body, 15); !ok || string(eid.Data) != "state" {
		t.Fatal("exec reply must echo exec_id")
	}
	server := mustSub(t, mustSub(t, mustSub(t, reply.body, 36), 1), 1)
	if ident, _ := pbFirst(server, 2); string(ident.Data) != mcpProviderIdentifier {
		t.Fatalf("server_identifier=%q", ident.Data)
	}
	toolDef, ok := pbFirst(server, 5)
	if !ok {
		t.Fatal("mcp_state must list the declared tools")
	}
	if string(toolDef.Data) != string(encodeMCPToolDefinition(tools[0])) {
		t.Fatal("mcp_state tool must be byte-identical to run_request.mcp_tools")
	}
}

// 不桥接的原生工具（如 fetch=20）必须 throw + stream_close，且文案点名客户端工具；
// 桥接给下游执行的原生工具（read=7）绝不能抢答，否则上游认为工具已失败。
func TestAgentStream_RefusesUnbridgedNativeExecButLeavesBridgedOnes(t *testing.T) {
	up := newFakeAgentUpstream(t, false,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: execFrame(3, "read", 7, pbString(1, "/tmp/a"))},
		scriptStep{frame: execFrame(4, "fetch", 20, pbString(1, "https://x"))},
		scriptStep{frame: iuFrame(iuText("ok")), waitAppends: 4},
		scriptStep{frame: iuFrame(iuTurnEnded())},
	)
	tools := []ToolDef{{Name: "Bash", InputSchema: `{"type":"object"}`}}
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "hi", Tools: tools})
	if res.err != nil {
		t.Fatalf("unexpected err: %v", res.err)
	}
	msgs := up.clientMessages()
	if len(msgs) != 4 {
		t.Fatalf("want run_request + context + throw + stream_close = 4 appends, got %d", len(msgs))
	}
	throw := decodeClientMessage(t, msgs[2])
	if throw.field != agentClientExecControlMessage {
		t.Fatalf("refusal must be exec_client_control_message, got %d", throw.field)
	}
	throwBody := mustSub(t, throw.body, 2)
	if id, _ := pbFirst(throwBody, 1); id.Value != 4 {
		t.Fatalf("throw must target exec id 4 (fetch), not the bridged read; got %d", id.Value)
	}
	if text, _ := pbFirst(throwBody, 2); !strings.Contains(string(text.Data), toolWireName(tools[0])) {
		t.Fatalf("refusal must name the client's tools so the model switches: %q", text.Data)
	}
	closeMsg := decodeClientMessage(t, msgs[3])
	if id, _ := pbFirst(mustSub(t, closeMsg.body, 1), 1); closeMsg.field != agentClientExecControlMessage || id.Value != 4 {
		t.Fatal("throw must be followed by stream_close for the same id")
	}
}

func TestAgentStream_AnswersKVSetThenGet(t *testing.T) {
	set := pbField(1, 0, pbVarint(11))
	set = append(set, pbBytes(3, append(pbBytes(1, []byte("blob-1")), pbBytes(2, []byte("payload"))...))...)
	get := pbField(1, 0, pbVarint(12))
	get = append(get, pbBytes(2, pbBytes(1, []byte("blob-1")))...)
	up := newFakeAgentUpstream(t, false,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: agentServerFrame(4, set), waitAppends: 2},
		scriptStep{frame: agentServerFrame(4, get), waitAppends: 3},
		scriptStep{frame: iuFrame(iuText("ok")), waitAppends: 4},
		scriptStep{frame: iuFrame(iuTurnEnded())},
	)
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "hi"})
	if res.err != nil {
		t.Fatalf("kv must be answered: %v", res.err)
	}
	msgs := up.clientMessages()
	getReply := decodeClientMessage(t, msgs[3])
	if getReply.field != agentClientKVMessage {
		t.Fatalf("kv reply must be kv_client_message(3), got %d", getReply.field)
	}
	if id, _ := pbFirst(getReply.body, 1); id.Value != 12 {
		t.Fatal("kv reply must echo id")
	}
	result := mustSub(t, getReply.body, 2)
	if data, ok := pbFirst(result, 1); !ok || string(data.Data) != "payload" {
		t.Fatalf("get_blob must return what set_blob stored, got %s", describePBParts(result, 0))
	}
}

// request_context 可能在同一轮里被问第二次；只答第一次会让第二次永远挂起。
func TestAgentStream_AnswersEveryRequestContext(t *testing.T) {
	up := newFakeAgentUpstream(t, false,
		scriptStep{frame: execFrame(1, "ctx-1", 10, nil)},
		scriptStep{frame: execFrame(2, "ctx-2", 10, nil), waitAppends: 2},
		scriptStep{frame: iuFrame(iuText("ok")), waitAppends: 3},
		scriptStep{frame: iuFrame(iuTurnEnded())},
	)
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "hi"})
	if res.err != nil || res.text != "ok" {
		t.Fatalf("second request_context must be answered: text=%q err=%v", res.text, res.err)
	}
}

// ======== 正文过滤 / 并行工具 ========

func TestAgentStream_DropsServerNotices(t *testing.T) {
	up := newFakeAgentUpstream(t, false,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: iuFrame(iuServerNotice("Switched to a faster model")), waitAppends: 2},
		scriptStep{frame: iuFrame(iuText("real answer"))},
		scriptStep{frame: iuFrame(iuTurnEnded())},
	)
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "hi"})
	if res.err != nil || res.text != "real answer" {
		t.Fatalf("server notice must not leak into assistant text: text=%q err=%v", res.text, res.err)
	}
}

// 同一批并行工具调用必须一起交给下游。收到第一个就结束会丢掉后面的调用，
// 模型下一轮看不到它们的结果。
func TestAgentStream_CollectsParallelToolBatch(t *testing.T) {
	tools := []ToolDef{{Name: "Read", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}}}`}}
	wire := toolWireName(tools[0])
	up := newFakeAgentUpstream(t, true,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: iuFrame(iuToolRequestsListed(2)), waitAppends: 2},
		scriptStep{frame: iuFrame(iuMCPToolStarted("call-1", wire, "path", "/a"))},
		scriptStep{frame: iuFrame(iuMCPToolStarted("call-2", wire, "path", "/b")), pause: 300 * time.Millisecond},
	)
	res := runAgentAgainst(t, up, 5*time.Second, AgentRequest{Message: "read both", Tools: tools})
	if res.err != nil {
		t.Fatalf("unexpected err: %v", res.err)
	}
	if len(res.tools) != 2 || res.tools[0].ID != "call-1" || res.tools[1].ID != "call-2" {
		t.Fatalf("both parallel calls must be delivered, got %+v", res.tools)
	}
}

// 没有 tool_requests_listed 时退回静默窗口：第一个调用后很快结束，不能一直等 turn_ended。
func TestAgentStream_ToolBatchWithoutListingEndsAfterQuietWindow(t *testing.T) {
	tools := []ToolDef{{Name: "Read", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}}}`}}
	up := newFakeAgentUpstream(t, true,
		scriptStep{frame: execFrame(1, "ctx", 10, nil)},
		scriptStep{frame: iuFrame(iuMCPToolStarted("call-1", toolWireName(tools[0]), "path", "/a")), waitAppends: 2},
	)
	start := time.Now()
	res := runAgentAgainst(t, up, 10*time.Second, AgentRequest{Message: "read", Tools: tools})
	if res.err != nil || len(res.tools) != 1 {
		t.Fatalf("single tool call must be delivered: tools=%d err=%v", len(res.tools), res.err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("tool turn must end after the quiet window, took %v", elapsed)
	}
}

// ======== 纯函数 ========

func TestParseTurnEnded_DistinguishesAbsentFromZero(t *testing.T) {
	if _, ok := parseTurnEnded(iuText("x")); ok {
		t.Fatal("non turn_ended update must not be treated as end of turn")
	}
	usage, ok := parseTurnEnded(iuTurnEnded())
	if !ok || usage.Present {
		t.Fatalf("empty turn_ended ends the turn but carries no usage: ok=%v usage=%+v", ok, usage)
	}
	usage, ok = parseTurnEnded(iuTurnEnded([2]uint64{2, 0}))
	if !ok || !usage.Present || usage.OutputTokens != 0 {
		t.Fatalf("explicit zero must count as present: %+v", usage)
	}
}

func TestIsInteractionHeartbeat(t *testing.T) {
	if !isInteractionHeartbeat(iuHeartbeat()) {
		t.Fatal("bare heartbeat")
	}
	withStamp := append(iuHeartbeat(), pbField(25, 0, pbVarint(123))...)
	if !isInteractionHeartbeat(withStamp) {
		t.Fatal("heartbeat with message_started_at_ms is still only a heartbeat")
	}
	if isInteractionHeartbeat(append(iuHeartbeat(), iuText("x")...)) {
		t.Fatal("heartbeat bundled with text is progress")
	}
}

func TestExecResponder_MCPApprovalProbeIsApproved(t *testing.T) {
	r := newAgentServerResponder(nil)
	args := append(pbString(1, "ccx_Read"), pbField(7, 0, pbVarint(1))...)
	execParts, _ := pbParse((execFrameBody(9, "probe", 11, args)))
	replies, kind := r.handleExec(execParts)
	if kind != "mcp_approval_probe" || len(replies) != 2 {
		t.Fatalf("approval probe must be approved and closed: kind=%s replies=%d", kind, len(replies))
	}
	reply := decodeClientMessage(t, replies[0])
	result := mustSub(t, reply.body, 11)
	if _, ok := pbFirst(result, 7); !ok {
		t.Fatal("probe answer must be mcp_result.approved(7)")
	}

	// 真实调用不在这里作答，由 tool-call-ready 交给下游。
	execParts, _ = pbParse((execFrameBody(10, "call", 11, pbString(1, "ccx_Read"))))
	if replies, kind := r.handleExec(execParts); len(replies) != 0 || kind != "mcp_call" {
		t.Fatalf("real mcp call must be left to the downstream client: kind=%s replies=%d", kind, len(replies))
	}
}

func TestKVResponder_RefusesWhenFull(t *testing.T) {
	r := newAgentServerResponder(nil)
	r.blobBytes = agentKVMaxBytes
	set := pbField(1, 0, pbVarint(1))
	set = append(set, pbBytes(3, append(pbBytes(1, []byte("k")), pbBytes(2, []byte("v"))...))...)
	reply, _ := r.handleKV(set)
	msg := decodeClientMessage(t, reply)
	result := mustSub(t, msg.body, 3)
	if _, ok := pbFirst(result, 1); !ok {
		t.Fatal("full store must answer set_blob with an error instead of silently dropping")
	}
	if _, ok := r.blobs["k"]; ok {
		t.Fatal("full store must not grow")
	}
}

func execFrameBody(id uint64, execID string, branch int, args []byte) []byte {
	body := pbField(1, 0, pbVarint(id))
	body = append(body, pbString(15, execID)...)
	return append(body, pbBytes(branch, args)...)
}

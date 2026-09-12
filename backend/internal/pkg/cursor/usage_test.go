package cursor

import "testing"

func TestEstimateOutputUsageText(t *testing.T) {
	got := estimateOutputUsage(len("hello world"), nil)
	if got.Kind != "text" || got.Source != "estimated" {
		t.Fatalf("usage metadata=%+v", got)
	}
	if got.VisibleTextTokens <= 0 || got.OutputTokens != got.VisibleTextTokens {
		t.Fatalf("text usage=%+v, want positive equal visible/output", got)
	}
}

func TestEstimateOutputUsageToolUseCountsNonTextOutput(t *testing.T) {
	got := estimateOutputUsage(0, []ToolCall{{ID: "call-1", Name: "Read", Input: []byte(`{"file_path":"fixture.txt"}`)}})
	if got.Kind != "tool_use" || got.Source != "estimated" || got.ToolCalls != 1 {
		t.Fatalf("tool metadata=%+v", got)
	}
	if got.VisibleTextTokens != 0 {
		t.Fatalf("tool visible text tokens=%d, want 0", got.VisibleTextTokens)
	}
	if got.OutputTokens <= 0 {
		t.Fatalf("tool output tokens=%d, want positive", got.OutputTokens)
	}
}

func TestEstimateOutputUsageMixedAndEmptyTool(t *testing.T) {
	mixed := estimateOutputUsage(6, []ToolCall{{Name: "Bash", Input: nil}})
	if mixed.Kind != "mixed" || mixed.OutputTokens <= mixed.VisibleTextTokens {
		t.Fatalf("mixed usage=%+v, want tool contribution", mixed)
	}
	empty := estimateOutputUsage(0, []ToolCall{{}})
	if empty.Kind != "tool_use" || empty.OutputTokens <= 0 {
		t.Fatalf("empty tool usage=%+v, want non-zero lower bound", empty)
	}
}

func TestEstimateOutputUsageStreamMatchesBatch(t *testing.T) {
	calls := []ToolCall{
		{Name: "Read", Input: []byte(`{"file_path":"a.txt"}`)},
		{Name: "Bash", Input: []byte(`{"command":"printf ok"}`)},
	}
	batch := estimateOutputUsage(12, calls)
	toolBytes := 0
	for _, call := range calls {
		toolBytes += toolCallUsageBytes(call)
	}
	stream := estimateOutputUsageFromLengths(12, toolBytes, len(calls))
	if batch != stream {
		t.Fatalf("batch=%+v stream=%+v", batch, stream)
	}
}

package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelContextWindowExceededMapsToIncomplete(t *testing.T) {
	require.Equal(t, "incomplete", anthropicStopReasonToResponsesStatus("model_context_window_exceeded", nil))
	status, details := anthropicResponsesStreamTerminalState("model_context_window_exceeded")
	require.Equal(t, "incomplete", status)
	require.Equal(t, "max_output_tokens", details.Reason)
	require.Equal(t, "length", responsesStatusToChatFinishReason(status, details, nil))
}

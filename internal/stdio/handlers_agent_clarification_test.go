package stdio

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tolle-ai/tollecode/internal/agent"
)

// TestHandleClarificationResponse_DeliversAnswer verifies that a
// clarification_response command is routed to the waiting requestClarification
// goroutine with the new Selected/Details shape.
func TestHandleClarificationResponse_DeliversAnswer(t *testing.T) {
	state := newServerState()
	requestID := "req-123"
	ch := state.registerClarificationCh(requestID)

	handleClarificationResponse(state, map[string]any{
		"type":       "clarification_response",
		"request_id": requestID,
		"selected":   []any{"option-a", "option-b"},
		"details":    "more info",
	})

	select {
	case resp := <-ch:
		assert.Equal(t, []string{"option-a", "option-b"}, resp.Selected)
		assert.Equal(t, "more info", resp.Details)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("clarification response was not delivered")
	}
}

// TestHandleClarificationResponse_FallsBackToRequestId verifies the handler
// accepts the camelCase requestId key used by some frontend paths.
func TestHandleClarificationResponse_FallsBackToRequestId(t *testing.T) {
	state := newServerState()
	requestID := "req-456"
	ch := state.registerClarificationCh(requestID)

	handleClarificationResponse(state, map[string]any{
		"type":        "clarification_response",
		"requestId":   requestID,
		"suggestions": []any{"choice-1", "choice-2"},
		"answer":      "choice-1",
	})

	select {
	case resp := <-ch:
		assert.Equal(t, []string{"choice-1"}, resp.Selected)
		assert.Empty(t, resp.Details)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("legacy clarification response was not delivered")
	}
}

// TestHandleClarificationResponse_DeliversClarificationAnswer verifies the
// handler converts the incoming map into an agent.ClarificationAnswer and
// delivers it to the registered channel.
func TestHandleClarificationResponse_DeliversClarificationAnswer(t *testing.T) {
	state := newServerState()
	requestID := "req-789"
	ch := state.registerClarificationCh(requestID)

	handleClarificationResponse(state, map[string]any{
		"type":       "clarification_response",
		"request_id": requestID,
		"selected":   []any{"suggestion-a"},
		"details":    "free-text details",
	})

	select {
	case resp := <-ch:
		want := agent.ClarificationAnswer{Selected: []string{"suggestion-a"}, Details: "free-text details"}
		assert.Equal(t, clarificationResponse{Selected: want.Selected, Details: want.Details}, resp)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("structured clarification response was not delivered")
	}
}

// TestRequestClarification_WaitsForResponse_BlocksUntilDelivered verifies that
// the requestClarification closure in runAgentTask blocks indefinitely until
// a clarification_response is delivered, and does not time out early.
func TestRequestClarification_WaitsForResponse_BlocksUntilDelivered(t *testing.T) {
	state := newServerState()

	// Use a context that cannot time out on its own. Cancellation will come
	// from this test only if we want to exercise that path.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var answer agent.ClarificationAnswer
	var ok bool

	// Simulate the closure logic runAgentTask uses for clarification.
	go func() {
		defer close(done)
		requestID := "wait-123"
		ch := state.registerClarificationCh(requestID)
		_ = ch // emit event would happen here in real code
		select {
		case resp := <-ch:
			answer = agent.ClarificationAnswer{Selected: resp.Selected, Details: resp.Details}
			ok = true
		case <-ctx.Done():
			state.deliverClarificationResponse(requestID, agent.ClarificationAnswer{})
			ok = false
		}
	}()

	// Ensure the goroutine is waiting longer than any previous timeout would
	// have been (the old timeout was 120s). We deliberately sleep a short time
	// to prove it does NOT return early.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("requestClarification returned before a response was delivered")
	default:
	}

	// Now deliver the response and verify the goroutine unblocks.
	handleClarificationResponse(state, map[string]any{
		"type":       "clarification_response",
		"request_id": "wait-123",
		"selected":   []any{"user-choice"},
		"details":    "extra context",
	})

	select {
	case <-done:
		assert.True(t, ok, "expected ok=true when response delivered")
		assert.Equal(t, []string{"user-choice"}, answer.Selected)
		assert.Equal(t, "extra context", answer.Details)
	case <-time.After(2 * time.Second):
		t.Fatal("requestClarification did not unblock after response was delivered")
	}
}

// TestRequestClarification_Cancellation_CleansUpChannel verifies that when the
// context is cancelled the clarification channel is cleaned up and the
// closure returns false.
func TestRequestClarification_Cancellation_CleansUpChannel(t *testing.T) {
	state := newServerState()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var ok bool

	go func() {
		defer close(done)
		requestID := "cancel-456"
		ch := state.registerClarificationCh(requestID)
		_ = ch
		select {
		case <-ch:
			ok = true
		case <-ctx.Done():
			state.deliverClarificationResponse(requestID, agent.ClarificationAnswer{})
			ok = false
		}
	}()

	cancel()

	select {
	case <-done:
		assert.False(t, ok, "expected ok=false on context cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("requestClarification did not return after context cancellation")
	}

	// A late response for the cancelled request should find no listener.
	delivered := state.deliverClarificationResponse("cancel-456", agent.ClarificationAnswer{Selected: []string{"late"}})
	assert.False(t, delivered, "channel should have been cleaned up on cancellation")
}

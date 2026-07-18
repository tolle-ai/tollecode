package ai

import (
	"context"
	"strings"
	"testing"
)

// captureProvider records the request its Stream received.
type captureProvider struct{ got StreamRequest }

func (c *captureProvider) Stream(ctx context.Context, req StreamRequest) (<-chan StreamEvent, error) {
	c.got = req
	ch := make(chan StreamEvent)
	close(ch)
	return ch, nil
}
func (c *captureProvider) DiscoverModels(ctx context.Context) ([]ModelInfo, error) { return nil, nil }

const fakeSecret = "AKIAIOSFODNN7EXAMPLE"          // AWS access key shape
const fakeEmail = "alice@example.com"

func sampleRequest() StreamRequest {
	return StreamRequest{
		Model:  "test-model",
		System: "You are a bot. Contact: " + fakeEmail,
		Messages: []ChatMessage{
			{Role: "user", Content: "here is my key " + fakeSecret},
			{Role: "user", ToolResults: []ToolResult{{Content: "file contents with " + fakeSecret}}},
		},
	}
}

func TestScanRequest_DetectsAndMasks(t *testing.T) {
	findings := scanRequest(sampleRequest())
	byType := map[string]EgressFinding{}
	for _, f := range findings {
		byType[f.Type] = f
	}

	if byType["aws_access_key"].Count != 2 {
		t.Fatalf("aws_access_key count = %d, want 2", byType["aws_access_key"].Count)
	}
	if byType["email"].Count != 1 {
		t.Fatalf("email count = %d, want 1", byType["email"].Count)
	}
	// The masked sample must never contain the raw secret.
	if strings.Contains(byType["aws_access_key"].Sample, fakeSecret) {
		t.Fatalf("sample leaked the raw secret: %q", byType["aws_access_key"].Sample)
	}
}

func TestScanningProvider_LogMode_DoesNotAlterRequest(t *testing.T) {
	restore := swapEgress(EgressLog)
	defer restore()

	var sunk []EgressFinding
	EgressSink = func(f []EgressFinding, model string) { sunk = f }

	inner := &captureProvider{}
	p := &scanningProvider{inner: inner}
	req := sampleRequest()
	if _, err := p.Stream(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if len(sunk) == 0 {
		t.Fatal("log mode: expected findings to be reported to the sink")
	}
	// Request forwarded unchanged in log mode.
	if !strings.Contains(inner.got.Messages[0].Content, fakeSecret) {
		t.Fatal("log mode must forward the original request unmodified")
	}
}

func TestScanningProvider_RedactMode_RedactsCopy_LeavesCallerIntact(t *testing.T) {
	restore := swapEgress(EgressRedact)
	defer restore()
	EgressSink = func(f []EgressFinding, model string) {}

	inner := &captureProvider{}
	p := &scanningProvider{inner: inner}
	req := sampleRequest()
	if _, err := p.Stream(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	// Inner provider must receive a redacted request.
	if strings.Contains(inner.got.Messages[0].Content, fakeSecret) {
		t.Fatal("redact mode: secret still present in forwarded request")
	}
	if !strings.Contains(inner.got.Messages[0].Content, "[REDACTED:aws_access_key]") {
		t.Fatalf("redact mode without a vault: expected type label, got %q", inner.got.Messages[0].Content)
	}
	if !strings.Contains(inner.got.Messages[1].ToolResults[0].Content, "[REDACTED:aws_access_key]") {
		t.Fatal("redact mode: tool-result content was not redacted")
	}
	// Caller's original request must be untouched (it is persisted history).
	if !strings.Contains(req.Messages[0].Content, fakeSecret) {
		t.Fatal("redact mode mutated the caller's request; it must operate on a copy")
	}
}

// The regression this whole mechanism exists for: with a vault on the context,
// the model receives a handle instead of the secret, and that handle resolves
// back to the real value before a tool ever runs. Previously the model got an
// irreversible "[REDACTED:...]" label and executed it as a literal credential.
func TestRedactRoundTrip_HandleResolvesBackToSecret(t *testing.T) {
	restore := swapEgress(EgressRedact)
	defer restore()
	EgressSink = func(f []EgressFinding, model string) {}

	vault := NewSecretVault()
	ctx := WithSecretVault(context.Background(), vault)

	inner := &captureProvider{}
	p := &scanningProvider{inner: inner}
	if _, err := p.Stream(ctx, sampleRequest()); err != nil {
		t.Fatal(err)
	}

	sent := inner.got.Messages[0].Content
	if strings.Contains(sent, fakeSecret) {
		t.Fatal("secret reached the provider")
	}
	handles := aliasPattern.FindAllString(sent, -1)
	if len(handles) != 1 {
		t.Fatalf("expected one handle in %q, got %d", sent, len(handles))
	}

	// The model echoes the handle back inside tool input, at depth.
	revealed := vault.RevealInput(map[string]any{
		"command": "curl -H 'Authorization: Bearer " + handles[0] + "'",
		"headers": map[string]any{"x-key": handles[0]},
		"args":    []any{handles[0]},
	})
	if got := revealed["command"].(string); !strings.Contains(got, fakeSecret) {
		t.Fatalf("handle did not resolve in command: %q", got)
	}
	if got := revealed["headers"].(map[string]any)["x-key"].(string); got != fakeSecret {
		t.Fatalf("handle did not resolve in nested map: %q", got)
	}
	if got := revealed["args"].([]any)[0].(string); got != fakeSecret {
		t.Fatalf("handle did not resolve in array: %q", got)
	}

	// The same secret elsewhere in the request must share one handle, and a
	// different secret must never collide with it.
	if h2 := aliasPattern.FindAllString(inner.got.Messages[1].ToolResults[0].Content, -1); len(h2) != 1 || h2[0] != handles[0] {
		t.Fatalf("same secret produced unstable handles: %v vs %v", h2, handles)
	}
	if vault.alias("aws_access_key", "AKIAIOSFODNN7DIFFERE") == handles[0] {
		t.Fatal("distinct secrets collapsed to the same handle")
	}

	// An unknown handle is left alone rather than silently dropped.
	unknown := "${TOLLE_SECRET_openai_key_deadbeef}"
	if got := vault.Reveal(unknown); got != unknown {
		t.Fatalf("unknown handle should pass through, got %q", got)
	}
}

// Tool-call arguments are replayed on every subsequent request, so they must be
// scanned and redacted like any other outbound content.
func TestRedact_CoversToolCallInput(t *testing.T) {
	restore := swapEgress(EgressRedact)
	defer restore()
	EgressSink = func(f []EgressFinding, model string) {}

	req := StreamRequest{
		Model: "test-model",
		Messages: []ChatMessage{{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:   "t1",
				Name: "run_command",
				Input: map[string]any{
					"command": "export KEY=" + fakeSecret,
					"env":     map[string]any{"AWS": fakeSecret},
				},
			}},
		}},
	}
	if findings := scanRequest(req); len(findings) == 0 {
		t.Fatal("tool-call input was not scanned")
	}

	inner := &captureProvider{}
	p := &scanningProvider{inner: inner}
	if _, err := p.Stream(WithSecretVault(context.Background(), NewSecretVault()), req); err != nil {
		t.Fatal(err)
	}
	got := inner.got.Messages[0].ToolCalls[0].Input
	if strings.Contains(got["command"].(string), fakeSecret) {
		t.Fatal("secret leaked via tool-call argument")
	}
	if strings.Contains(got["env"].(map[string]any)["AWS"].(string), fakeSecret) {
		t.Fatal("secret leaked via nested tool-call argument")
	}
	// Caller's copy untouched — local execution still needs the real value.
	if req.Messages[0].ToolCalls[0].Input["command"].(string) != "export KEY="+fakeSecret {
		t.Fatal("redaction mutated the caller's tool-call input")
	}
}

func TestScanningProvider_OffMode_Skips(t *testing.T) {
	restore := swapEgress(EgressOff)
	defer restore()
	called := false
	EgressSink = func(f []EgressFinding, model string) { called = true }

	inner := &captureProvider{}
	p := &scanningProvider{inner: inner}
	if _, err := p.Stream(context.Background(), sampleRequest()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("off mode must not scan or report")
	}
	if !strings.Contains(inner.got.Messages[0].Content, fakeSecret) {
		t.Fatal("off mode must forward the original request")
	}
}

// swapEgress sets the policy and returns a restore func that also resets the sink.
func swapEgress(mode EgressMode) func() {
	prevMode, prevSink := CurrentEgressPolicy(), EgressSink
	SetEgressPolicy(mode)
	return func() { SetEgressPolicy(prevMode); EgressSink = prevSink }
}

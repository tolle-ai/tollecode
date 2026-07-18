package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestToolAskFollowupQuestion_ReturnsStructuredAnswer verifies the tool
// delegates to Config.RequestClarification and returns a JSON-serialized
// ClarificationAnswer for the LLM.
func TestToolAskFollowupQuestion_ReturnsStructuredAnswer(t *testing.T) {
	called := false
	var gotSuggestions []string
	var gotMulti bool

	cfg := &Config{
		RequestClarification: func(ctx context.Context, question string, suggestions []string, multiChoice bool) (ClarificationAnswer, bool) {
			called = true
			gotSuggestions = suggestions
			gotMulti = multiChoice
			return ClarificationAnswer{Selected: []string{"yes"}, Details: "please proceed"}, true
		},
	}

	output, _, hadErr := toolAskFollowupQuestion(context.Background(), cfg, map[string]any{
		"question":     "Should I proceed?",
		"suggestions":  []any{"yes", "no"},
		"multi_choice": false,
	})

	assert.True(t, called, "RequestClarification should be called")
	assert.Equal(t, []string{"yes", "no"}, gotSuggestions)
	assert.False(t, gotMulti)
	assert.False(t, hadErr)
	assert.JSONEq(t, `{"selected":["yes"],"details":"please proceed"}`, output)
}

// TestToolAskFollowupQuestion_RequiresQuestion verifies the tool errors when
// the question is missing.
func TestToolAskFollowupQuestion_RequiresQuestion(t *testing.T) {
	cfg := &Config{}
	output, _, hadErr := toolAskFollowupQuestion(context.Background(), cfg, map[string]any{
		"suggestions": []any{"a", "b"},
	})
	assert.True(t, hadErr)
	assert.Contains(t, output, "'question' is required")
}

// TestToolAskFollowupQuestion_ProceedsWhenUnavailable verifies the tool falls
// back gracefully when RequestClarification is not configured.
func TestToolAskFollowupQuestion_ProceedsWhenUnavailable(t *testing.T) {
	cfg := &Config{}
	output, _, hadErr := toolAskFollowupQuestion(context.Background(), cfg, map[string]any{
		"question": "What should I do?",
	})
	assert.False(t, hadErr)
	assert.Contains(t, output, "Proceed with your best judgment")
}

// TestClarificationAnswerFromLegacy_ParsesSelectedSuggestion verifies legacy
// plain-string answers are converted to ClarificationAnswer.Selected when they
// match a suggestion.
func TestClarificationAnswerFromLegacy_ParsesSelectedSuggestion(t *testing.T) {
	ans := ClarificationAnswerFromLegacy("choice-1", []string{"choice-1", "choice-2"}, false)
	assert.Equal(t, []string{"choice-1"}, ans.Selected)
	assert.Empty(t, ans.Details)
}

// TestClarificationAnswerFromLegacy_ParsesDetails verifies legacy plain-string
// answers that do not match a suggestion become Details.
func TestClarificationAnswerFromLegacy_ParsesDetails(t *testing.T) {
	ans := ClarificationAnswerFromLegacy("free-form answer", []string{"choice-1", "choice-2"}, false)
	assert.Empty(t, ans.Selected)
	assert.Equal(t, "free-form answer", ans.Details)
}

// TestClarificationAnswerFromLegacy_ParsesJSON verifies a JSON-encoded
// ClarificationAnswer is parsed directly.
func TestClarificationAnswerFromLegacy_ParsesJSON(t *testing.T) {
	ans := ClarificationAnswerFromLegacy(`{"selected":["a","b"],"details":"details"}`, nil, false)
	assert.Equal(t, []string{"a", "b"}, ans.Selected)
	assert.Equal(t, "details", ans.Details)
}

func TestLooksLikeClarifyingQuestion(t *testing.T) {
	// Last non-empty line ends in "?" → treated as a clarifying question.
	assert.True(t, looksLikeClarifyingQuestion("Which database should I use?"))
	assert.True(t, looksLikeClarifyingQuestion("Here's my plan.\n\nShould I use Postgres or MySQL?"))
	assert.True(t, looksLikeClarifyingQuestion("Ready to proceed?**"))
	// A rhetorical question mid-paragraph followed by a statement is not.
	assert.False(t, looksLikeClarifyingQuestion("Is this useful? Yes, it is. Done."))
	assert.False(t, looksLikeClarifyingQuestion("All set — the build passes."))
	assert.False(t, looksLikeClarifyingQuestion(""))
}

func TestLastQuestionLine(t *testing.T) {
	assert.Equal(t, "Which one?", lastQuestionLine("Some context.\n\nWhich one?"))
	// Leading markdown markers are stripped.
	assert.Equal(t, "Pick a framework?", lastQuestionLine("- Pick a framework?"))
	assert.Equal(t, "Could you clarify how you'd like to proceed?", lastQuestionLine("   "))
}

func TestClarificationAnswerToText(t *testing.T) {
	assert.Equal(t, "Postgres", clarificationAnswerToText(ClarificationAnswer{Selected: []string{"Postgres"}}))
	assert.Equal(t, "A, B — with SSL", clarificationAnswerToText(ClarificationAnswer{Selected: []string{"A", "B"}, Details: "with SSL"}))
	assert.Equal(t, "just details", clarificationAnswerToText(ClarificationAnswer{Details: "just details"}))
	assert.Contains(t, clarificationAnswerToText(ClarificationAnswer{}), "skipped")
}

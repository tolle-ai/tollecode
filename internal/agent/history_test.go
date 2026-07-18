package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildSystem_IncludesAskFollowupQuestionInstructions verifies the base system
// prompt contains both the tool listing and the mandatory Clarification Protocol
// that instructs the LLM to use ask_followup_question instead of guessing.
func TestBuildSystem_IncludesAskFollowupQuestionInstructions(t *testing.T) {
	sys := buildSystem("/tmp/workspace", "build", false, false, "", "", false, nil, "")

	assert.Contains(t, sys, "ask_followup_question", "system prompt must mention ask_followup_question")
	assert.Contains(t, sys, "ambiguous", "system prompt must instruct the agent to clarify ambiguous requests")
	assert.Contains(t, sys, "NEVER guess", "system prompt must forbid guessing")

	clarificationIdx := strings.Index(sys, "**Clarification**")
	assert.GreaterOrEqual(t, clarificationIdx, 0, "expected a Clarification tool heading")
	generalRulesIdx := strings.Index(sys, "## General Rules")
	assert.GreaterOrEqual(t, generalRulesIdx, 0, "expected General Rules section")
	protocolIdx := strings.Index(sys, "## Clarification Protocol — MANDATORY")
	assert.Greater(t, protocolIdx, clarificationIdx, "Clarification Protocol should appear after the Clarification tool heading")
	assert.Greater(t, generalRulesIdx, protocolIdx, "General Rules should appear after the Clarification Protocol")
}

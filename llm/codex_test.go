package llm

import (
	"strings"
	"testing"
)

func TestBuildAgentPrompt_ContainsPreambleAndUserRequest(t *testing.T) {
	got := buildAgentPrompt("create a changelog", []string{"codex", "claude"})
	if !strings.Contains(got, attachmentPromptPreamble) {
		t.Fatalf("expected prompt preamble to be included")
	}
	if !strings.Contains(got, "Available agents: claude, codex.") {
		t.Fatalf("expected available agents section, got %q", got)
	}
	if !strings.Contains(got, "User request:\ncreate a changelog") {
		t.Fatalf("expected user request section to be included, got %q", got)
	}
}

func TestBuildAgentPrompt_EmptyMessage(t *testing.T) {
	got := buildAgentPrompt("   ", nil)
	if got != attachmentPromptPreamble {
		t.Fatalf("expected only preamble for empty message, got %q", got)
	}
}

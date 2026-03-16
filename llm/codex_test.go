package llm

import (
	"strings"
	"testing"
)

func TestBuildCodexPrompt_ContainsPreambleAndUserRequest(t *testing.T) {
	got := buildCodexPrompt("create a changelog")
	if !strings.Contains(got, attachmentPromptPreamble) {
		t.Fatalf("expected prompt preamble to be included")
	}
	if !strings.Contains(got, "User request:\ncreate a changelog") {
		t.Fatalf("expected user request section to be included, got %q", got)
	}
}

func TestBuildCodexPrompt_EmptyMessage(t *testing.T) {
	got := buildCodexPrompt("   ")
	if got != attachmentPromptPreamble {
		t.Fatalf("expected only preamble for empty message, got %q", got)
	}
}

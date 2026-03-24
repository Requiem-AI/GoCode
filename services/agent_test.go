package services

import (
	ctx "context"
	"testing"
	"time"

	"github.com/requiem-ai/gocode/llm"
)

type fakeAgentClient struct {
	id        string
	responses []llm.Response
	errs      []error
	calls     []llm.Request
}

func (f *fakeAgentClient) ID() string {
	return f.id
}

func (f *fakeAgentClient) Send(_ ctx.Context, req llm.Request) (llm.Response, error) {
	f.calls = append(f.calls, req)
	idx := len(f.calls) - 1

	var resp llm.Response
	if idx < len(f.responses) {
		resp = f.responses[idx]
	}
	var err error
	if idx < len(f.errs) {
		err = f.errs[idx]
	}
	return resp, err
}

func (f *fakeAgentClient) Clear(_ ctx.Context, _ string) error {
	return nil
}

func TestParseAgentForward(t *testing.T) {
	available := map[string]llm.Client{
		llm.CodexID:  &fakeAgentClient{id: llm.CodexID},
		llm.ClaudeID: &fakeAgentClient{id: llm.ClaudeID},
	}

	target, payload, ok := parseAgentForward("Need a check. @claude review auth middleware.", available)
	if !ok {
		t.Fatalf("expected agent forward to be parsed")
	}
	if target != llm.ClaudeID {
		t.Fatalf("target = %q, want %q", target, llm.ClaudeID)
	}
	if payload != "review auth middleware." {
		t.Fatalf("payload = %q, want %q", payload, "review auth middleware.")
	}
}

func TestRunWithEvents_HandoffAndFinalize(t *testing.T) {
	codex := &fakeAgentClient{
		id: llm.CodexID,
		responses: []llm.Response{
			{Text: "@claude review this plan"},
			{Text: "Final answer"},
		},
	}
	claude := &fakeAgentClient{
		id: llm.ClaudeID,
		responses: []llm.Response{
			{Text: "Looks good. Add tests."},
		},
	}

	svc := &AgentService{
		clients: map[string]llm.Client{
			llm.CodexID:  codex,
			llm.ClaudeID: claude,
		},
		enabledAgents:   []string{llm.CodexID, llm.ClaudeID},
		defaultAgent:    llm.CodexID,
		maxHops:         4,
		agentHopTimeout: time.Minute,
	}

	events := make([]AgentEvent, 0, 2)
	resp, err := svc.RunWithEvents("/tmp/repo", "start", func(event AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("RunWithEvents returned error: %v", err)
	}
	if resp != "Final answer" {
		t.Fatalf("resp = %q, want %q", resp, "Final answer")
	}
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	if events[0].Type != AgentEventForward || events[0].To != llm.ClaudeID {
		t.Fatalf("unexpected forward event: %#v", events[0])
	}
	if events[1].Type != AgentEventResponse || events[1].From != llm.ClaudeID {
		t.Fatalf("unexpected response event: %#v", events[1])
	}
	if len(claude.calls) != 1 || claude.calls[0].Message != "review this plan" {
		t.Fatalf("expected claude to receive forwarded message, got %#v", claude.calls)
	}
}

func TestRunWithEvents_FullFlowMultiHop(t *testing.T) {
	codex := &fakeAgentClient{
		id: llm.CodexID,
		responses: []llm.Response{
			{Text: "@claude evaluate architecture"},
			{Text: "@claude evaluate testing strategy"},
			{Text: "Ship it."},
		},
	}
	claude := &fakeAgentClient{
		id: llm.ClaudeID,
		responses: []llm.Response{
			{Text: "Architecture: good separation."},
			{Text: "Tests: add integration coverage."},
		},
	}

	svc := &AgentService{
		clients: map[string]llm.Client{
			llm.CodexID:  codex,
			llm.ClaudeID: claude,
		},
		enabledAgents:   []string{llm.CodexID, llm.ClaudeID},
		defaultAgent:    llm.CodexID,
		maxHops:         6,
		agentHopTimeout: time.Minute,
	}

	var events []AgentEvent
	resp, err := svc.RunWithEvents("/tmp/full-flow-repo", "build feature", func(event AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("RunWithEvents returned error: %v", err)
	}
	if resp != "Ship it." {
		t.Fatalf("resp = %q, want %q", resp, "Ship it.")
	}

	if len(events) != 4 {
		t.Fatalf("events len = %d, want 4", len(events))
	}
	if events[0].Type != AgentEventForward || events[0].Text != "evaluate architecture" {
		t.Fatalf("unexpected first event: %#v", events[0])
	}
	if events[1].Type != AgentEventResponse || events[1].Text != "Architecture: good separation." {
		t.Fatalf("unexpected second event: %#v", events[1])
	}
	if events[2].Type != AgentEventForward || events[2].Text != "evaluate testing strategy" {
		t.Fatalf("unexpected third event: %#v", events[2])
	}
	if events[3].Type != AgentEventResponse || events[3].Text != "Tests: add integration coverage." {
		t.Fatalf("unexpected fourth event: %#v", events[3])
	}

	if len(codex.calls) != 3 {
		t.Fatalf("codex calls = %d, want 3", len(codex.calls))
	}
	if len(claude.calls) != 2 {
		t.Fatalf("claude calls = %d, want 2", len(claude.calls))
	}

	for i, call := range codex.calls {
		if call.RepoPath != "/tmp/full-flow-repo" {
			t.Fatalf("codex call[%d] repoPath = %q, want /tmp/full-flow-repo", i, call.RepoPath)
		}
		if len(call.AvailableAgents) != 1 || call.AvailableAgents[0] != llm.ClaudeID {
			t.Fatalf("codex call[%d] expected only claude as available agent, got %#v", i, call.AvailableAgents)
		}
	}
	for i, call := range claude.calls {
		if call.RepoPath != "/tmp/full-flow-repo" {
			t.Fatalf("claude call[%d] repoPath = %q, want /tmp/full-flow-repo", i, call.RepoPath)
		}
		if len(call.AvailableAgents) != 1 || call.AvailableAgents[0] != llm.CodexID {
			t.Fatalf("claude call[%d] expected only codex as available agent, got %#v", i, call.AvailableAgents)
		}
	}
}

func TestOtherAgents_ExcludesSelf(t *testing.T) {
	got := otherAgents([]string{llm.CodexID, llm.ClaudeID}, llm.CodexID)
	if len(got) != 1 || got[0] != llm.ClaudeID {
		t.Fatalf("expected only claude, got %#v", got)
	}

	got = otherAgents([]string{llm.CodexID}, llm.CodexID)
	if len(got) != 0 {
		t.Fatalf("expected empty list when only self exists, got %#v", got)
	}
}

func TestStart_DefaultAgentHopTimeoutIsFiveMinutes(t *testing.T) {
	t.Setenv("ENABLED_AGENTS", "codex")
	t.Setenv("DEFAULT_AGENT", "codex")
	t.Setenv("MAX_AGENT_HOPS", "")
	t.Setenv("AGENT_HOP_TIMEOUT", "")

	svc := &AgentService{}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	if svc.agentHopTimeout != 5*time.Minute {
		t.Fatalf("agentHopTimeout = %s, want 5m", svc.agentHopTimeout)
	}
}

package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionIDFromRepo_Deterministic(t *testing.T) {
	repoPath := "/tmp/my-repo"
	id1 := sessionIDFromRepo(repoPath)
	id2 := sessionIDFromRepo(repoPath)
	if id1 != id2 {
		t.Fatalf("expected deterministic session id, got %q and %q", id1, id2)
	}
	if strings.Count(id1, "-") != 4 {
		t.Fatalf("expected UUID-like session id format, got %q", id1)
	}
}

func TestClaudeSend_UsesRepoContextAndSessionID(t *testing.T) {
	repoDir := t.TempDir()
	binPath := filepath.Join(repoDir, "claude-stub.sh")
	argsPath := filepath.Join(repoDir, "args.txt")
	pwdPath := filepath.Join(repoDir, "pwd.txt")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"" + argsPath + "\"\n" +
		"pwd > \"" + pwdPath + "\"\n" +
		"echo \"claude-ok\"\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write stub cli: %v", err)
	}

	client := &ClaudeClient{bin: binPath}
	resp, err := client.Send(context.Background(), Request{
		RepoPath:        repoDir,
		Message:         "review this",
		AvailableAgents: []string{"codex", "claude"},
	})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if strings.TrimSpace(resp.Text) != "claude-ok" {
		t.Fatalf("response text = %q, want %q", resp.Text, "claude-ok\n")
	}

	argsRaw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("failed to read captured args: %v", err)
	}
	args := string(argsRaw)
	if !strings.Contains(args, "-p") {
		t.Fatalf("expected -p arg, got %q", args)
	}
	if !strings.Contains(args, "--session-id") {
		t.Fatalf("expected --session-id arg, got %q", args)
	}
	if !strings.Contains(args, sessionIDFromRepo(repoDir)) {
		t.Fatalf("expected deterministic session id in args, got %q", args)
	}
	if !strings.Contains(args, "Available agents: claude, codex.") {
		t.Fatalf("expected shared agent prompt content in args, got %q", args)
	}

	pwdRaw, err := os.ReadFile(pwdPath)
	if err != nil {
		t.Fatalf("failed to read captured cwd: %v", err)
	}
	gotPWD := strings.TrimSpace(string(pwdRaw))
	if gotPWD != repoDir {
		t.Fatalf("expected claude command cwd %q, got %q", repoDir, gotPWD)
	}
}

func TestClaudeSend_EmptyPrompt(t *testing.T) {
	client := &ClaudeClient{bin: "claude"}
	_, err := client.Send(context.Background(), Request{
		RepoPath: "/tmp/repo",
		Message:  "   ",
	})
	if err == nil {
		t.Fatalf("expected error for empty prompt")
	}
}

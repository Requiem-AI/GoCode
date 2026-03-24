package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type CodexClient struct {
	bin string

	mu       sync.Mutex
	sessions map[string]bool
}

const CodexID = "codex"

const attachmentPromptPreamble = "Telegram integration note: only include a standalone file URI in your final response using the format file://path/to/file (repo-relative paths preferred) if the user specifically asks for files. Only reference files that already exist."

func NewCodexClient() *CodexClient {
	bin := os.Getenv("CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}

	return &CodexClient{
		bin:      bin,
		sessions: make(map[string]bool),
	}
}

func (c *CodexClient) ID() string {
	return CodexID
}

func (c *CodexClient) Send(ctx context.Context, req Request) (Response, error) {
	if req.Message == "" {
		return Response{}, errors.New("missing prompt")
	}

	repoPath, err := filepath.Abs(req.RepoPath)
	if err != nil {
		return Response{}, err
	}

	shouldResume := c.shouldResume(repoPath)
	prompt := buildCodexPrompt(req.Message)

	var args []string
	if shouldResume {
		args = []string{"exec", "-s", "danger-full-access", "resume", "--last", "--", prompt}
	} else {
		args = []string{"exec", "-s", "danger-full-access"}
		if req.RepoPath != "" {
			args = append(args, "--cd", req.RepoPath)
		}
		args = append(args, "--", prompt)
	}

	out, err := c.run(ctx, repoPath, args...)
	if err != nil {
		return Response{Text: out}, err
	}

	if !shouldResume {
		if out != "" {
			out += "\n\n"
		}
		out += "New session started."
	}

	c.markSession(repoPath)

	return Response{Text: out}, nil
}

func (c *CodexClient) Clear(ctx context.Context, repoPath string) error {
	_ = ctx
	if repoPath == "" {
		return errors.New("missing repo path")
	}

	c.mu.Lock()
	delete(c.sessions, repoPath)
	c.mu.Unlock()

	return nil
}

func (c *CodexClient) run(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	if repoPath != "" {
		cmd.Dir = repoPath
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmdline := strings.TrimSpace(strings.Join(append([]string{cmd.Path}, args...), " "))
	fmt.Fprintf(os.Stdout, "[codex] exec: %s\n", cmdline)

	// Capture agent output without mirroring it to process logs to avoid
	// log spam from long model responses.
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		out := stdout.String()
		if out == "" {
			out = stderr.String()
		}
		return out, err
	}

	if stdout.Len() == 0 && stderr.Len() > 0 {
		return stderr.String(), nil
	}

	return stdout.String(), nil
}

func (c *CodexClient) shouldResume(repoPath string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[repoPath]
}

func (c *CodexClient) markSession(repoPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[repoPath] = true
}

func buildCodexPrompt(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return attachmentPromptPreamble
	}

	return attachmentPromptPreamble + "\n\nUser request:\n" + trimmed
}

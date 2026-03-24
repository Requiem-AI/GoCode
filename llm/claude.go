package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ClaudeClient struct {
	bin string
}

const ClaudeID = "claude"

func NewClaudeClient() *ClaudeClient {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}

	return &ClaudeClient{
		bin: bin,
	}
}

func (c *ClaudeClient) ID() string {
	return ClaudeID
}

func (c *ClaudeClient) Send(ctx context.Context, req Request) (Response, error) {
	if strings.TrimSpace(req.Message) == "" {
		return Response{}, errors.New("missing prompt")
	}

	repoPath, err := filepath.Abs(req.RepoPath)
	if err != nil {
		return Response{}, err
	}

	prompt := buildAgentPrompt(req.Message, req.AvailableAgents)
	args := []string{
		"-p",
		prompt,
		"--session-id",
		sessionIDFromRepo(repoPath),
	}

	out, err := c.run(ctx, repoPath, args...)
	if err != nil {
		return Response{Text: out}, err
	}

	return Response{Text: out}, nil
}

func (c *ClaudeClient) Clear(ctx context.Context, repoPath string) error {
	_ = ctx
	if strings.TrimSpace(repoPath) == "" {
		return errors.New("missing repo path")
	}

	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return err
	}
	_ = absPath

	return nil
}

func (c *ClaudeClient) run(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	if repoPath != "" {
		cmd.Dir = repoPath
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmdline := strings.TrimSpace(strings.Join(append([]string{cmd.Path}, args...), " "))
	fmt.Fprintf(os.Stdout, "[claude] exec: %s\n", cmdline)

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

func sessionIDFromRepo(repoPath string) string {
	hash := sha256.Sum256([]byte(repoPath))
	b := hash[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // pseudo-v5 style
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

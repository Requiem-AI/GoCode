package llm

import (
	"context"
	"errors"
)

type ClaudeClient struct{}

const ClaudeID = "claude"

func NewClaudeClient() *ClaudeClient {
	return &ClaudeClient{}
}

func (c *ClaudeClient) ID() string {
	return ClaudeID
}

func (c *ClaudeClient) Send(ctx context.Context, req Request) (Response, error) {
	_ = ctx
	_ = req
	return Response{}, errors.New("claude client not implemented")
}

func (c *ClaudeClient) Clear(ctx context.Context, repoPath string, topicID string) error {
	_ = ctx
	_ = repoPath
	_ = topicID
	return errors.New("claude client not implemented")
}

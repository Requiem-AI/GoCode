package llm

import "context"

type Request struct {
	RepoPath string
	Message  string
}

type Response struct {
	Text string
}

type Client interface {
	ID() string
	Send(ctx context.Context, req Request) (Response, error)
	Clear(ctx context.Context, repoPath string) error
}

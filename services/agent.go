package services

import (
	context2 "context"
	"github.com/requiem-ai/gocode/context"
	"github.com/requiem-ai/gocode/llm"
)

type AgentService struct {
	context.DefaultService

	agent llm.Client
}

const Agent_SVC = "Agent_svc"

func (svc AgentService) Id() string {
	return Agent_SVC
}

func (svc *AgentService) Start() error {
	svc.agent = llm.NewCodexClient()

	return nil
}

func (svc *AgentService) Run(repoPath string, msg string) (string, error) {
	resp, err := svc.agent.Send(context2.TODO(), llm.Request{
		RepoPath: repoPath,
		Message:  msg,
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

package services

import (
	ctx "context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/requiem-ai/gocode/context"
	"github.com/requiem-ai/gocode/llm"
)

type AgentService struct {
	context.DefaultService

	clients         map[string]llm.Client
	enabledAgents   []string
	defaultAgent    string
	maxHops         int
	agentHopTimeout time.Duration
}

type AgentEventType string

const (
	AgentEventForward  AgentEventType = "forward"
	AgentEventResponse AgentEventType = "response"
)

type AgentEvent struct {
	Type AgentEventType
	From string
	To   string
	Text string
}

const Agent_SVC = "Agent_svc"

const (
	defaultEnabledAgents = "codex,claude"
	defaultAgentID       = llm.CodexID
	defaultMaxHops       = 8
	defaultHopTimeout    = 5 * time.Minute
)

var agentTagPattern = regexp.MustCompile(`@([a-zA-Z0-9_-]+)`)

func (svc AgentService) Id() string {
	return Agent_SVC
}

func (svc *AgentService) Start() error {
	enabledAgents := parseEnabledAgents()
	clients := make(map[string]llm.Client, len(enabledAgents))
	for _, id := range enabledAgents {
		switch id {
		case llm.CodexID:
			clients[id] = llm.NewCodexClient()
		case llm.ClaudeID:
			clients[id] = llm.NewClaudeClient()
		default:
			return fmt.Errorf("unsupported agent id %q", id)
		}
	}
	if len(clients) == 0 {
		return errors.New("no agents enabled")
	}

	defaultAgent := strings.TrimSpace(os.Getenv("DEFAULT_AGENT"))
	if defaultAgent == "" {
		defaultAgent = defaultAgentID
	}
	if _, ok := clients[defaultAgent]; !ok {
		return fmt.Errorf("DEFAULT_AGENT %q is not enabled", defaultAgent)
	}

	maxHops := defaultMaxHops
	if value := strings.TrimSpace(os.Getenv("MAX_AGENT_HOPS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return fmt.Errorf("invalid MAX_AGENT_HOPS %q", value)
		}
		maxHops = parsed
	}

	agentHopTimeout := defaultHopTimeout
	if value := strings.TrimSpace(os.Getenv("AGENT_HOP_TIMEOUT")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid AGENT_HOP_TIMEOUT %q", value)
		}
		agentHopTimeout = parsed
	}

	agentIDs := make([]string, 0, len(clients))
	for id := range clients {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)

	svc.clients = clients
	svc.enabledAgents = agentIDs
	svc.defaultAgent = defaultAgent
	svc.maxHops = maxHops
	svc.agentHopTimeout = agentHopTimeout
	return nil
}

func (svc *AgentService) Run(repoPath string, msg string) (string, error) {
	return svc.RunWithEvents(repoPath, msg, nil)
}

func (svc *AgentService) RunWithEvents(repoPath string, msg string, onEvent func(AgentEvent)) (string, error) {
	if strings.TrimSpace(msg) == "" {
		return "", errors.New("missing prompt")
	}
	if len(svc.clients) == 0 {
		return "", errors.New("agent service not initialized")
	}

	activeID := svc.defaultAgent
	activeInput := msg
	for hop := 0; hop < svc.maxHops; hop++ {
		activeResp, err := svc.sendToAgent(repoPath, activeID, activeInput)
		if err != nil {
			return activeResp, err
		}

		targetID, forwardMessage, ok := parseAgentForward(activeResp, svc.clients)
		if !ok {
			return activeResp, nil
		}

		if onEvent != nil {
			onEvent(AgentEvent{
				Type: AgentEventForward,
				From: activeID,
				To:   targetID,
				Text: forwardMessage,
			})
		}

		targetResp, err := svc.sendToAgent(repoPath, targetID, forwardMessage)
		if onEvent != nil {
			onEvent(AgentEvent{
				Type: AgentEventResponse,
				From: targetID,
				To:   activeID,
				Text: targetResp,
			})
		}
		if err != nil {
			return targetResp, err
		}

		activeInput = fmt.Sprintf(
			"Feedback received from @%s:\n\n%s\n\nContinue solving the user's task. "+
				"If more expert feedback is needed, ask one agent using @<agent_id> <message>. "+
				"If complete, respond directly without any @agent tag.",
			targetID,
			strings.TrimSpace(targetResp),
		)
	}

	return "", fmt.Errorf("agent collaboration stopped: reached MAX_AGENT_HOPS=%d", svc.maxHops)
}

func (svc *AgentService) Clear(repoPath string) error {
	if strings.TrimSpace(repoPath) == "" {
		return errors.New("missing repo path")
	}
	for _, client := range svc.clients {
		if err := client.Clear(ctx.TODO(), repoPath); err != nil {
			return err
		}
	}
	return nil
}

func (svc *AgentService) sendToAgent(repoPath, agentID, message string) (string, error) {
	client, ok := svc.clients[agentID]
	if !ok {
		return "", fmt.Errorf("agent %q not configured", agentID)
	}

	runCtx, cancel := ctx.WithTimeout(ctx.Background(), svc.agentHopTimeout)
	defer cancel()

	resp, err := client.Send(runCtx, llm.Request{
		RepoPath:        repoPath,
		Message:         message,
		AvailableAgents: otherAgents(svc.enabledAgents, agentID),
	})
	return resp.Text, err
}

func otherAgents(agents []string, self string) []string {
	out := make([]string, 0, len(agents))
	for _, agent := range agents {
		if agent == self {
			continue
		}
		out = append(out, agent)
	}
	return out
}

func parseEnabledAgents() []string {
	raw := strings.TrimSpace(os.Getenv("ENABLED_AGENTS"))
	if raw == "" {
		raw = defaultEnabledAgents
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		candidate := strings.TrimSpace(strings.ToLower(part))
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func parseAgentForward(text string, available map[string]llm.Client) (targetID string, payload string, ok bool) {
	matches := agentTagPattern.FindStringSubmatchIndex(text)
	if len(matches) < 4 {
		return "", "", false
	}

	agentID := strings.ToLower(strings.TrimSpace(text[matches[2]:matches[3]]))
	if _, exists := available[agentID]; !exists {
		return "", "", false
	}

	contentStart := matches[1]
	message := strings.TrimSpace(text[contentStart:])
	if message == "" {
		return "", "", false
	}

	return agentID, message, true
}

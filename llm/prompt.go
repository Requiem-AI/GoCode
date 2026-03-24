package llm

import (
	"fmt"
	"slices"
	"strings"
)

const attachmentPromptPreamble = "Telegram integration note: only include a standalone file URI in your final response using the format file://path/to/file (repo-relative paths preferred) if the user specifically asks for files. Only reference files that already exist."

func buildAgentPrompt(message string, availableAgents []string) string {
	trimmed := strings.TrimSpace(message)
	agents := make([]string, 0, len(availableAgents))
	for _, id := range availableAgents {
		candidate := strings.TrimSpace(id)
		if candidate == "" {
			continue
		}
		agents = append(agents, candidate)
	}
	slices.Sort(agents)

	sections := []string{attachmentPromptPreamble}
	if len(agents) > 0 {
		sections = append(sections, fmt.Sprintf(
			"Intra-agent collaboration is enabled. Ask another agent with `@<agent_id> <message>` only when needed. "+
				"When your answer is complete, do not tag any agent. Available agents: %s.",
			strings.Join(agents, ", "),
		))
	}
	if trimmed != "" {
		sections = append(sections, "User request:\n"+trimmed)
	}

	return strings.Join(sections, "\n\n")
}

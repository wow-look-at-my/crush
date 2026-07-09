package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
}

// NamedAgentParams is the agent tool's input when custom agents are
// configured: it adds the optional agent selector. With no custom agents
// the tool keeps the plain AgentParams schema, byte-identical to the
// zero-config rendering.
type NamedAgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
	Agent  string `json:"agent,omitempty" description:"Name of the agent to run the task with; see the tool description for the available agents. Omit to use the default agent."`
}

const (
	AgentToolName = "agent"
)

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	prompt, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, true)
	if err != nil {
		return nil, err
	}

	custom := customAgentConfigs(c.cfg.Config().Agents)
	if len(custom) == 0 {
		// No custom agents: keep the tool's description and schema
		// byte-identical to the single-agent rendering so the prompt
		// prefix (and any recorded interaction) is unaffected by this
		// feature existing.
		return fantasy.NewParallelAgentTool(
			AgentToolName,
			agentToolDescription,
			func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return c.dispatchAgentTool(ctx, agent, params.Prompt, "New Agent Session", call)
			},
		), nil
	}

	agents := map[string]SessionAgent{config.AgentTask: agent}
	for _, agentCfg := range custom {
		subAgentPrompt, err := customAgentPrompt(agentCfg, c.cfg.WorkingDir())
		if err != nil {
			return nil, fmt.Errorf("building prompt for agent %q: %w", agentCfg.ID, err)
		}
		subAgent, err := c.buildAgent(ctx, subAgentPrompt, agentCfg, true)
		if err != nil {
			return nil, fmt.Errorf("building agent %q: %w", agentCfg.ID, err)
		}
		agents[agentCfg.ID] = subAgent
	}

	return fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescriptionWith(custom),
		func(ctx context.Context, params NamedAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return c.runNamedAgent(ctx, agents, params, call)
		},
	), nil
}

// runNamedAgent resolves the agent tool's optional agent selector
// against the built sub-agents and dispatches the task to the selected
// one. An empty selector means the default task agent; an unknown name
// is a recoverable tool error listing the valid names so the model can
// correct the call.
func (c *coordinator) runNamedAgent(ctx context.Context, agents map[string]SessionAgent, params NamedAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	name := params.Agent
	if name == "" {
		name = config.AgentTask
	}
	selected, ok := agents[name]
	if !ok {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"unknown agent %q, valid agents: %s",
			params.Agent, strings.Join(slices.Sorted(maps.Keys(agents)), ", "),
		)), nil
	}
	title := "New Agent Session"
	if name != config.AgentTask {
		title = fmt.Sprintf("Agent: %s", name)
	}
	return c.dispatchAgentTool(ctx, selected, params.Prompt, title, call)
}

// dispatchAgentTool runs one agent tool invocation against the selected
// sub-agent through the shared runSubAgent path, so cost roll-up,
// SubagentStop hooks, and parallel invocation behave identically for the
// default and custom agents.
func (c *coordinator) dispatchAgentTool(ctx context.Context, agent SessionAgent, taskPrompt, sessionTitle string, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if taskPrompt == "" {
		return fantasy.NewTextErrorResponse("prompt is required"), nil
	}

	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, errors.New("session id missing from context")
	}

	agentMessageID := tools.GetMessageFromContext(ctx)
	if agentMessageID == "" {
		return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
	}

	return c.runSubAgent(ctx, subAgentParams{
		Agent:          agent,
		SessionID:      sessionID,
		AgentMessageID: agentMessageID,
		ToolCallID:     call.ID,
		Prompt:         taskPrompt,
		SessionTitle:   sessionTitle,
	})
}

// customAgentConfigs returns the custom agent definitions in the
// registry — everything except the built-in coder and task agents —
// sorted by ID so tool descriptions are deterministic.
func customAgentConfigs(agents map[string]config.Agent) []config.Agent {
	var custom []config.Agent
	for name, agent := range agents {
		if name == config.AgentCoder || name == config.AgentTask {
			continue
		}
		custom = append(custom, agent)
	}
	slices.SortFunc(custom, func(a, b config.Agent) int {
		return strings.Compare(a.ID, b.ID)
	})
	return custom
}

// customAgentPrompt returns the system prompt for a custom agent: the
// user-defined prompt verbatim when set (never template-processed, so
// prompts containing template metacharacters cannot break the agent),
// falling back to the built-in task prompt.
func customAgentPrompt(agent config.Agent, workingDir string) (*prompt.Prompt, error) {
	if agent.Prompt != "" {
		return prompt.NewStaticPrompt("agent-"+agent.ID, agent.Prompt), nil
	}
	return taskPrompt(prompt.WithWorkingDir(workingDir))
}

// agentToolDescriptionWith extends the agent tool description with the
// selectable custom agents, flagging the ones whose toolset can modify
// files or state so the model can weigh the risk when delegating.
func agentToolDescriptionWith(custom []config.Agent) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(agentToolDescription, "\n"))
	sb.WriteString("\n\nThe following custom agents are also available. Select one by setting the \"agent\" parameter to its name; omit the parameter to use the default agent described above.\n")
	for _, agent := range custom {
		fmt.Fprintf(&sb, "\n- %q: %s", agent.ID, strings.TrimSpace(agent.Description))
		if agentHasMutatingTools(agent) {
			sb.WriteString(" (has tools that can modify files or state)")
		}
	}
	return sb.String()
}

// agentHasMutatingTools reports whether an agent definition was granted
// any tool outside the read-only set (the plan-mode allow-list is the
// authoritative read-only classification) or any MCP tools, which may
// mutate state.
func agentHasMutatingTools(agent config.Agent) bool {
	if len(agent.AllowedMCP) > 0 {
		return true
	}
	for _, tool := range agent.AllowedTools {
		if !permission.ModePlan.AllowsTool(tool) {
			return true
		}
	}
	return false
}

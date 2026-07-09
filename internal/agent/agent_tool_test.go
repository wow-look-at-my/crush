package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAgentToolCoordinator builds a coordinator whose provider resolves
// offline (models are constructed without network I/O; no request is
// ever issued by these tests), suitable for exercising agentTool
// construction. mutate edits the config before agents are set up.
func newAgentToolCoordinator(t *testing.T, mutate func(cfg *config.Config)) (*coordinator, fakeEnv) {
	t.Helper()
	// Keep the developer's real global config, agent files, and data
	// config out of the test fixture.
	t.Setenv("CRUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	const providerID = "test-openai-compat"
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models: []catwalk.Model{
			{ID: "test-large", DefaultMaxTokens: 4096},
			{ID: "test-small", DefaultMaxTokens: 2048},
		},
	})
	cfg.OverridePreferredModel(config.SelectedModelTypeLarge, config.SelectedModel{Provider: providerID, Model: "test-large"})
	cfg.OverridePreferredModel(config.SelectedModelTypeSmall, config.SelectedModel{Provider: providerID, Model: "test-small"})
	if mutate != nil {
		mutate(cfg.Config())
	}
	cfg.SetupAgents()

	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: permission.NewPermissionService(env.workingDir, true, nil),
	}, env
}

// TestAgentToolZeroConfigUnchanged pins the agent tool's zero-config
// rendering: with no custom agents configured, the description and the
// input schema must stay byte-identical to the original single-agent
// tool so the prompt prefix (and any recorded interaction embedding the
// tool) is unaffected by the custom-agents feature.
func TestAgentToolZeroConfigUnchanged(t *testing.T) {
	c, _ := newAgentToolCoordinator(t, nil)

	tool, err := c.agentTool(t.Context())
	require.NoError(t, err)
	require.NoError(t, c.readyWg.Wait())

	info := tool.Info()
	assert.Equal(t, AgentToolName, info.Name)
	assert.Equal(t, agentToolDescription, info.Description)
	assert.Equal(t, []string{"prompt"}, info.Required)
	assert.Equal(t, map[string]any{
		"prompt": map[string]any{
			"type":        "string",
			"description": "The task for the agent to perform",
		},
	}, info.Parameters, "the zero-config schema must expose exactly the prompt parameter")
	assert.True(t, info.Parallel)
}

func TestAgentToolWithCustomAgents(t *testing.T) {
	c, _ := newAgentToolCoordinator(t, func(cfg *config.Config) {
		cfg.Agents = map[string]config.Agent{
			"reviewer": {Description: "Reviews code for correctness."},
			"fixer":    {Description: "Fixes bugs.", AllowedTools: []string{"edit", "write", "view"}},
		}
	})

	tool, err := c.agentTool(t.Context())
	require.NoError(t, err)
	require.NoError(t, c.readyWg.Wait(), "custom agent prompt and tool construction must succeed")

	info := tool.Info()
	expectedDescription := strings.TrimRight(agentToolDescription, "\n") +
		"\n\nThe following custom agents are also available. Select one by setting the \"agent\" parameter to its name; omit the parameter to use the default agent described above.\n" +
		"\n- \"fixer\": Fixes bugs. (has tools that can modify files or state)" +
		"\n- \"reviewer\": Reviews code for correctness."
	assert.Equal(t, expectedDescription, info.Description)

	assert.Equal(t, []string{"prompt"}, info.Required, "agent selector must stay optional")
	require.Contains(t, info.Parameters, "agent")
	require.Contains(t, info.Parameters, "prompt")
}

// TestAgentToolCustomPromptTemplateSafe verifies a custom agent's prompt
// is used verbatim: template metacharacters must not be interpreted or
// break the build.
func TestAgentToolCustomPromptTemplateSafe(t *testing.T) {
	c, _ := newAgentToolCoordinator(t, func(cfg *config.Config) {
		cfg.Agents = map[string]config.Agent{
			"templated": {Description: "Uses braces.", Prompt: "Respond with {{.Weird}} literally."},
		}
	})

	_, err := c.agentTool(t.Context())
	require.NoError(t, err)
	require.NoError(t, c.readyWg.Wait())

	built, err := customAgentPrompt(c.cfg.Config().Agents["templated"], c.cfg.WorkingDir())
	require.NoError(t, err)
	prompt, err := built.Build(t.Context(), "test", "test-large", c.cfg)
	require.NoError(t, err)
	assert.Equal(t, "Respond with {{.Weird}} literally.", prompt)
}

// TestBuildAgentModelSelection verifies the agent definition's model
// type selects the sub-agent's primary model.
func TestBuildAgentModelSelection(t *testing.T) {
	c, _ := newAgentToolCoordinator(t, func(cfg *config.Config) {
		cfg.Agents = map[string]config.Agent{
			"small-agent": {Description: "Runs on the small model.", Model: config.SelectedModelTypeSmall},
			"large-agent": {Description: "Runs on the large model."},
		}
	})

	for name, wantModel := range map[string]string{
		"small-agent":    "test-small",
		"large-agent":    "test-large",
		config.AgentTask: "test-large",
	} {
		agentCfg := c.cfg.Config().Agents[name]
		prompt, err := customAgentPrompt(agentCfg, c.cfg.WorkingDir())
		require.NoError(t, err)
		agent, err := c.buildAgent(t.Context(), prompt, agentCfg, true)
		require.NoError(t, err)
		assert.Equal(t, wantModel, agent.Model().ModelCfg.Model, "agent %q", name)
	}
	require.NoError(t, c.readyWg.Wait())
}

func TestRunNamedAgentDispatch(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, parentSession.ID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")

	// The sub-session ID derives from (message ID, tool call ID), so
	// every dispatch that reaches runSubAgent needs a distinct call.
	callSeq := 0
	nextCall := func() fantasy.ToolCall {
		callSeq++
		return fantasy.ToolCall{ID: fmt.Sprintf("call-%d", callSeq)}
	}

	var taskPrompts, reviewerPrompts []string
	agents := map[string]SessionAgent{
		config.AgentTask: newMockAgent(providerID, 4096, func(_ context.Context, c SessionAgentCall) (*fantasy.AgentResult, error) {
			taskPrompts = append(taskPrompts, c.Prompt)
			return agentResultWithText("task did it"), nil
		}),
		"reviewer": newMockAgent(providerID, 4096, func(_ context.Context, c SessionAgentCall) (*fantasy.AgentResult, error) {
			reviewerPrompts = append(reviewerPrompts, c.Prompt)
			return agentResultWithText("reviewer did it"), nil
		}),
	}

	t.Run("named agent runs the task", func(t *testing.T) {
		resp, err := coord.runNamedAgent(ctx, agents, NamedAgentParams{Prompt: "review the diff", Agent: "reviewer"}, nextCall())
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "reviewer did it", resp.Content)
		assert.Equal(t, []string{"review the diff"}, reviewerPrompts)
	})

	t.Run("empty selector runs the default task agent", func(t *testing.T) {
		resp, err := coord.runNamedAgent(ctx, agents, NamedAgentParams{Prompt: "find the config"}, nextCall())
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "task did it", resp.Content)
		assert.Equal(t, []string{"find the config"}, taskPrompts)
	})

	t.Run("unknown agent is a recoverable error listing valid names", func(t *testing.T) {
		resp, err := coord.runNamedAgent(ctx, agents, NamedAgentParams{Prompt: "x", Agent: "nope"}, nextCall())
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, `unknown agent "nope", valid agents: reviewer, task`, resp.Content)
	})

	t.Run("empty prompt is a recoverable error", func(t *testing.T) {
		resp, err := coord.runNamedAgent(ctx, agents, NamedAgentParams{Agent: "reviewer"}, nextCall())
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "prompt is required", resp.Content)
	})

	t.Run("named agent sessions carry the agent name", func(t *testing.T) {
		_, err := coord.runNamedAgent(ctx, agents, NamedAgentParams{Prompt: "review again", Agent: "reviewer"}, fantasy.ToolCall{ID: "call-named"})
		require.NoError(t, err)
		childID := env.sessions.CreateAgentToolSessionID("msg-1", "call-named")
		child, err := env.sessions.Get(t.Context(), childID)
		require.NoError(t, err)
		assert.Equal(t, "Agent: reviewer", child.Title)
	})
}

func TestAgentHasMutatingTools(t *testing.T) {
	t.Parallel()

	assert.False(t, agentHasMutatingTools(config.Agent{AllowedTools: []string{"glob", "grep", "ls", "sourcegraph", "view", "web_search"}}))
	assert.True(t, agentHasMutatingTools(config.Agent{AllowedTools: []string{"view", "edit"}}))
	assert.True(t, agentHasMutatingTools(config.Agent{AllowedTools: []string{"bash"}}))
	assert.True(t, agentHasMutatingTools(config.Agent{AllowedTools: []string{"view"}, AllowedMCP: map[string][]string{"context7": nil}}))
	assert.False(t, agentHasMutatingTools(config.Agent{AllowedTools: []string{"view"}, AllowedMCP: map[string][]string{}}))
}

package agent

import (
	"slices"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildToolsPermissionModes verifies that buildTools filters the
// coder agent's tool list by the current permission mode: plan mode
// restricts it to the read-only set plus plan_exit, while the other
// modes leave the list alone. The agent/agentic_fetch sub-agent tools
// are excluded from AllowedTools here to keep the test free of model
// wiring; their plan-mode membership is covered by the policy tests.
//
// The subtests share one permission service and switch its mode, so
// they deliberately run sequentially (no t.Parallel).
func TestBuildToolsPermissionModes(t *testing.T) {
	cfgStore, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfgStore.SetupAgents()

	coderCfg := cfgStore.Config().Agents[config.AgentCoder]
	coderCfg.AllowedTools = []string{
		"bash", "crush_info", "crush_logs", "job_output", "job_kill",
		"download", "edit", "multiedit", "lsp_diagnostics",
		"lsp_references", "lsp_restart", "fetch", "glob", "grep", "ls",
		"sourcegraph", "todos", "view", "web_search", "write",
		"plan_exit",
	}
	cfgStore.Config().Agents[config.AgentCoder] = coderCfg

	perms := permission.NewPermissionService(cfgStore.WorkingDir(), false, nil)
	c := &coordinator{cfg: cfgStore, permissions: perms}

	builtToolNames := func(isSubAgent bool) []string {
		t.Helper()
		built, err := c.buildTools(t.Context(), cfgStore.Config().Agents[config.AgentCoder], isSubAgent)
		require.NoError(t, err)
		names := make([]string, 0, len(built))
		for _, tool := range built {
			names = append(names, tool.Info().Name)
		}
		return names
	}

	t.Run("default mode advertises the full allowed set", func(t *testing.T) {
		perms.SetMode(permission.ModeDefault)
		names := builtToolNames(false)
		for _, tool := range []string{"bash", "edit", "multiedit", "write", "download"} {
			assert.Contains(t, names, tool)
		}
		assert.NotContains(t, names, "plan_exit", "plan_exit is only advertised in plan mode")
	})

	t.Run("plan mode restricts to the read-only set plus plan_exit", func(t *testing.T) {
		perms.SetMode(permission.ModePlan)
		names := builtToolNames(false)

		assert.Equal(t, []string{
			"crush_info", "crush_logs", "fetch", "glob", "grep",
			"job_output", "ls", "lsp_diagnostics", "lsp_references",
			"plan_exit", "sourcegraph", "todos", "view", "web_search",
		}, names)
		assert.True(t, slices.IsSorted(names), "tool list must stay sorted")
	})

	t.Run("accept_edits mode leaves the tool list alone", func(t *testing.T) {
		perms.SetMode(permission.ModeAcceptEdits)
		defaultNames := builtToolNames(false)
		for _, tool := range []string{"bash", "edit", "multiedit", "write"} {
			assert.Contains(t, defaultNames, tool)
		}
		assert.NotContains(t, defaultNames, "plan_exit")
	})

	t.Run("sub-agents are exempt from the mode filter", func(t *testing.T) {
		perms.SetMode(permission.ModePlan)
		names := builtToolNames(true)
		assert.Contains(t, names, "edit", "the mode filter must not apply to sub-agent tool builds")
		assert.NotContains(t, names, "plan_exit", "plan_exit is never given to sub-agents")
	})
}

// TestExitPlanMode verifies the plan_exit approval callback: the mode
// flips back to default and the agent is immediately refreshed with the
// full toolset and an empty prompt suffix, so the in-flight run can
// continue straight into implementation.
func TestExitPlanMode(t *testing.T) {
	t.Parallel()

	cfgStore, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfgStore.SetupAgents()

	coderCfg := cfgStore.Config().Agents[config.AgentCoder]
	coderCfg.AllowedTools = []string{"glob", "edit", "write", "plan_exit"}
	cfgStore.Config().Agents[config.AgentCoder] = coderCfg

	perms := permission.NewPermissionService(cfgStore.WorkingDir(), false, nil)
	perms.SetMode(permission.ModePlan)

	agent := &mockSessionAgent{}
	c := &coordinator{cfg: cfgStore, permissions: perms, currentAgent: agent}

	// Simulate the state a plan-mode run starts with.
	require.NoError(t, c.applyPermissionMode(t.Context()))
	assert.Equal(t, permission.ModePlan.PromptAddition(), agent.promptSuffix)

	require.NoError(t, c.exitPlanMode(t.Context()))

	assert.Equal(t, permission.ModeDefault, perms.Mode(), "approval must flip the mode back to default")
	assert.Empty(t, agent.promptSuffix, "the plan prompt addition must be cleared for the next run")

	names := make([]string, 0, len(agent.tools))
	for _, tool := range agent.tools {
		names = append(names, tool.Info().Name)
	}
	assert.Contains(t, names, "edit", "the refreshed toolset must include the editing tools")
	assert.Contains(t, names, "write")
	assert.NotContains(t, names, "plan_exit", "plan_exit disappears once plan mode is off")
}

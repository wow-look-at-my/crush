package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fakeTool records the context it was invoked with so tests can assert on
// values stamped onto it by the hookedTool decorator.
type fakeTool struct {
	name   string
	called bool
	gotCtx context.Context
	resp   fantasy.ToolResponse
}

func (f *fakeTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: f.name}
}

func (f *fakeTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	f.called = true
	f.gotCtx = ctx
	return f.resp, nil
}

func (f *fakeTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (f *fakeTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

// newRunner builds a hooks.Runner from per-event hook configs, running
// the config-loader path that normalizes events and validates matchers.
func newRunner(t *testing.T, hookCfg map[string][]config.HookConfig) *hooks.Runner {
	t.Helper()
	cfg := &config.Config{Hooks: hookCfg}
	require.NoError(t, cfg.ValidateHooks())
	return hooks.NewRunner(cfg.Hooks, t.TempDir(), t.TempDir())
}

// preToolRunner builds a hooks.Runner holding a single PreToolUse hook.
func preToolRunner(t *testing.T, cmd string) *hooks.Runner {
	t.Helper()
	return newRunner(t, map[string][]config.HookConfig{
		hooks.EventPreToolUse: {{Command: cmd}},
	})
}

// postToolRunner builds a hooks.Runner holding a single PostToolUse hook.
func postToolRunner(t *testing.T, cmd string) *hooks.Runner {
	t.Helper()
	return newRunner(t, map[string][]config.HookConfig{
		hooks.EventPostToolUse: {{Command: cmd}},
	})
}

func TestHookedTool_AllowStampsHookApproval(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "view", resp: fantasy.NewTextResponse("ok")}
	runner := preToolRunner(t, `echo '{"decision":"allow"}'`)
	tool := newHookedTool(inner, runner)

	_, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-1", Name: "view"})
	require.NoError(t, err)
	require.True(t, inner.called, "inner tool should have run")

	// The inner tool's permission service can now treat call-1 as pre-approved.
	svc := permission.NewPermissionService(t.TempDir(), false, nil)
	granted, err := svc.Request(inner.gotCtx, permission.CreatePermissionRequest{
		SessionID:  "s1",
		ToolCallID: "call-1",
		ToolName:   "view",
		Action:     "read",
		Path:       t.TempDir(),
	})
	require.NoError(t, err)
	require.True(t, granted, "hook allow should bypass the permission prompt")
}

func TestHookedTool_SilentDoesNotStampApproval(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "view", resp: fantasy.NewTextResponse("ok")}
	runner := preToolRunner(t, `exit 0`) // no stdout, no decision
	tool := newHookedTool(inner, runner)

	_, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-2", Name: "view"})
	require.NoError(t, err)
	require.True(t, inner.called)

	// With no hook opinion, a fresh permission request has nothing stamped
	// and must fall through to the normal flow. We verify by checking that
	// the context does not look pre-approved for this call ID: sending a
	// request that no subscriber resolves will block until cancelled.
	svc := permission.NewPermissionService(t.TempDir(), false, nil)
	ctx, cancel := context.WithCancel(inner.gotCtx)
	cancel()
	granted, err := svc.Request(ctx, permission.CreatePermissionRequest{
		SessionID:  "s1",
		ToolCallID: "call-2",
		ToolName:   "view",
		Action:     "read",
		Path:       t.TempDir(),
	})
	require.Error(t, err, "no approval stamped => request should reach the prompt path")
	require.False(t, granted)
}

func TestHookedTool_DenySkipsInnerTool(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "bash"}
	runner := preToolRunner(t, `echo "blocked" >&2; exit 2`)
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-3", Name: "bash"})
	require.NoError(t, err)
	require.False(t, inner.called, "denied call must not reach the inner tool")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "blocked")
}

func TestHookedTool_PostToolUseFeedback(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("tool output")}
	runner := postToolRunner(t, `echo "use gofumpt next time" >&2; exit 2`)
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-4", Name: "bash"})
	require.NoError(t, err)
	require.True(t, inner.called, "PostToolUse hooks cannot prevent execution")
	// The original output is preserved and the hook's reason is appended
	// so the model sees the feedback.
	require.Contains(t, resp.Content, "tool output")
	require.Contains(t, resp.Content, "PostToolUse hook feedback: use gofumpt next time")
	require.False(t, resp.IsError, "a post-hoc block must not turn the result into an error")
	require.False(t, resp.StopTurn)
	require.Contains(t, resp.Metadata, `"post_hook"`)
}

func TestHookedTool_PostToolUseContext(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "view", resp: fantasy.NewTextResponse("file contents")}
	runner := postToolRunner(t, `echo '{"context":"remember to run tests"}'`)
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-5", Name: "view"})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "file contents")
	require.Contains(t, resp.Content, "remember to run tests")
	require.False(t, resp.StopTurn)
}

func TestHookedTool_PostToolUseHalt(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("leaked a secret")}
	runner := postToolRunner(t, `echo "secret detected" >&2; exit 49`)
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-6", Name: "bash"})
	require.NoError(t, err)
	require.True(t, inner.called)
	require.True(t, resp.StopTurn, "halt from a PostToolUse hook ends the turn after this call")
	require.Contains(t, resp.Content, "PostToolUse hook feedback: secret detected")
}

func TestHookedTool_PostToolUseSkippedWhenPreDenies(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "bash"}
	runner := newRunner(t, map[string][]config.HookConfig{
		hooks.EventPreToolUse:  {{Command: `echo "blocked" >&2; exit 2`}},
		hooks.EventPostToolUse: {{Command: `echo '{"context":"post-marker"}'`}},
	})
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-7", Name: "bash"})
	require.NoError(t, err)
	require.False(t, inner.called)
	require.NotContains(t, resp.Content, "post-marker",
		"PostToolUse must not fire when the tool never ran")
}

func TestHookedTool_PostToolUseSeesRewrittenInput(t *testing.T) {
	t.Parallel()

	// The pre hook rewrites the command; the post hook reports the
	// command it saw. PostToolUse must observe the input the tool
	// actually ran with, not what the model originally sent.
	inner := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")}
	runner := newRunner(t, map[string][]config.HookConfig{
		hooks.EventPreToolUse: {{Command: `echo '{"updated_input":{"command":"rewritten"}}'`}},
		hooks.EventPostToolUse: {{
			Command: `printf '{"context":"post saw %s"}' "$CRUSH_TOOL_INPUT_COMMAND"`,
		}},
	})
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "call-8",
		Name:  "bash",
		Input: `{"command":"original"}`,
	})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "post saw rewritten")
}

func TestHookedTool_PostToolUseReceivesToolResponse(t *testing.T) {
	t.Parallel()

	// The stdin payload carries the executed tool's result; the hook
	// echoes a marker only when it sees the expected content and error
	// flag.
	inner := &fakeTool{name: "bash", resp: fantasy.NewTextErrorResponse("boom")}
	runner := postToolRunner(t,
		`read -r line; case "$line" in *'"tool_response":{"content":"boom","is_error":true}'*) echo '{"context":"saw-response"}';; esac`)
	tool := newHookedTool(inner, runner)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-9", Name: "bash"})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "saw-response")
	require.True(t, resp.IsError, "the tool's own error flag is preserved")
}

func TestWrapToolsWithHooks(t *testing.T) {
	t.Parallel()

	runner := preToolRunner(t, `exit 0`)
	inputs := []fantasy.AgentTool{&fakeTool{name: "a"}, &fakeTool{name: "b"}}

	t.Run("top-level agent wraps every tool", func(t *testing.T) {
		t.Parallel()
		out := wrapToolsWithHooks(inputs, runner, false)
		require.Len(t, out, len(inputs))
		for i, tool := range out {
			_, ok := tool.(*hookedTool)
			require.Truef(t, ok, "tool %d should be a *hookedTool", i)
		}
	})

	t.Run("sub-agent skips the wrap", func(t *testing.T) {
		t.Parallel()
		out := wrapToolsWithHooks(inputs, runner, true)
		require.Equal(t, inputs, out, "sub-agent tools should be returned unwrapped")
		for _, tool := range out {
			_, isHooked := tool.(*hookedTool)
			require.False(t, isHooked, "sub-agent tool should not be wrapped")
		}
	})

	t.Run("nil runner skips the wrap for both agent kinds", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, inputs, wrapToolsWithHooks(inputs, nil, false))
		require.Equal(t, inputs, wrapToolsWithHooks(inputs, nil, true))
	})
}

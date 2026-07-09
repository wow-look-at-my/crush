package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// planExitPermissionService approves or denies every request and
// records the last request it saw.
type planExitPermissionService struct {
	mockPermissionService
	granted bool
	lastReq permission.CreatePermissionRequest
}

func (m *planExitPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.lastReq = req
	return m.granted, nil
}

func runPlanExit(t *testing.T, svc permission.Service, exit func(context.Context) error, params PlanExitParams) (fantasy.ToolResponse, error) {
	t.Helper()
	tool := NewPlanExitTool(svc, t.TempDir(), exit)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")
	return tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: PlanExitToolName, Input: mustJSON(t, params)})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestPlanExitTool(t *testing.T) {
	t.Parallel()

	t.Run("approval invokes the exit callback and continues", func(t *testing.T) {
		t.Parallel()
		svc := &planExitPermissionService{granted: true}
		exited := false
		resp, err := runPlanExit(t, svc, func(context.Context) error {
			exited = true
			return nil
		}, PlanExitParams{Plan: "1. do the thing"})
		require.NoError(t, err)
		assert.True(t, exited, "approval must invoke the exit callback")
		assert.False(t, resp.IsError)
		assert.False(t, resp.StopTurn, "the run should continue into implementation")
		assert.Contains(t, resp.Content, "approved")

		// The permission request must present the plan for review.
		assert.Equal(t, PlanExitToolName, svc.lastReq.ToolName)
		params, ok := svc.lastReq.Params.(PlanExitPermissionsParams)
		require.True(t, ok, "permission params must carry the plan")
		assert.Equal(t, "1. do the thing", params.Plan)
	})

	t.Run("denial feeds back as a denied tool result and stays in plan mode", func(t *testing.T) {
		t.Parallel()
		svc := &planExitPermissionService{granted: false}
		exited := false
		resp, err := runPlanExit(t, svc, func(context.Context) error {
			exited = true
			return nil
		}, PlanExitParams{Plan: "1. do the thing"})
		require.NoError(t, err)
		assert.False(t, exited, "denial must not invoke the exit callback")
		assert.True(t, resp.IsError)
		assert.True(t, resp.StopTurn, "denial should stop the turn like other permission denials")
		assert.Contains(t, resp.Content, "rejected")
	})

	t.Run("empty plan is a recoverable tool error", func(t *testing.T) {
		t.Parallel()
		svc := &planExitPermissionService{granted: true}
		resp, err := runPlanExit(t, svc, func(context.Context) error { return nil }, PlanExitParams{})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Empty(t, svc.lastReq.ToolName, "an empty plan must not reach the permission service")
	})

	t.Run("exit callback errors surface as tool errors", func(t *testing.T) {
		t.Parallel()
		svc := &planExitPermissionService{granted: true}
		_, err := runPlanExit(t, svc, func(context.Context) error {
			return errors.New("boom")
		}, PlanExitParams{Plan: "plan"})
		require.Error(t, err)
	})
}

// TestPermissionModeToolNames guards the tool-name and action literals
// mirrored in internal/permission/mode.go (which cannot import this
// package). If this test fails, the permission mode policy table and
// the tool constants have drifted apart.
func TestPermissionModeToolNames(t *testing.T) {
	t.Parallel()

	t.Run("accept_edits covers exactly the file-edit tools", func(t *testing.T) {
		t.Parallel()
		workingDir := t.TempDir()
		svc := permission.NewPermissionService(workingDir, false, nil)
		svc.SetMode(permission.ModeAcceptEdits)

		for _, tool := range []string{EditToolName, MultiEditToolName, WriteToolName} {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			granted, err := svc.Request(ctx, permission.CreatePermissionRequest{
				SessionID: "s1",
				ToolName:  tool,
				Action:    "write",
				Path:      workingDir,
			})
			cancel()
			require.NoError(t, err, tool)
			assert.True(t, granted, "accept_edits must auto-approve %s", tool)
		}
	})

	t.Run("plan mode advertises the read-only tool constants", func(t *testing.T) {
		t.Parallel()
		for _, tool := range []string{
			GlobToolName, GrepToolName, LSToolName, SourcegraphToolName,
			ViewToolName, FetchToolName, AgenticFetchToolName,
			CrushInfoToolName, CrushLogsToolName, JobOutputToolName,
			TodosToolName, DiagnosticsToolName, ReferencesToolName,
			ListMCPResourcesToolName, ReadMCPResourceToolName,
			PlanExitToolName, WebSearchToolName,
			// agent.AgentToolName lives in internal/agent, which imports
			// this package, so the literal is used here instead.
			"agent",
		} {
			assert.True(t, permission.ModePlan.AllowsTool(tool), "plan mode should allow %s", tool)
		}
		for _, tool := range []string{
			BashToolName, EditToolName, MultiEditToolName, WriteToolName,
			DownloadToolName, JobKillToolName, LSPRestartToolName,
		} {
			assert.False(t, permission.ModePlan.AllowsTool(tool), "plan mode must not allow %s", tool)
		}
	})
}

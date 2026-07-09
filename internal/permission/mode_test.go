package permission

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{in: "", want: ModeDefault},
		{in: "default", want: ModeDefault},
		{in: "accept_edits", want: ModeAcceptEdits},
		{in: "accept-edits", want: ModeAcceptEdits},
		{in: "Plan", want: ModePlan},
		{in: " plan ", want: ModePlan},
		{in: "yolo", wantErr: true},
		{in: "acceptedits", wantErr: true},
	} {
		got, err := ParseMode(tc.in)
		if tc.wantErr {
			assert.Error(t, err, "ParseMode(%q)", tc.in)
			continue
		}
		require.NoError(t, err, "ParseMode(%q)", tc.in)
		assert.Equal(t, tc.want, got, "ParseMode(%q)", tc.in)
	}
}

func TestModeCycleOrder(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []Mode{ModeDefault, ModeAcceptEdits, ModePlan}, Modes())
	for _, mode := range Modes() {
		assert.True(t, mode.Valid(), "mode %q must be in the policy table", mode)
	}
}

func TestServiceModeDefaultsAndNormalization(t *testing.T) {
	t.Parallel()
	svc := NewPermissionService(t.TempDir(), false, nil)
	assert.Equal(t, ModeDefault, svc.Mode())

	svc.SetMode(ModePlan)
	assert.Equal(t, ModePlan, svc.Mode())

	// Invalid modes are normalized so a bad value can never widen or
	// wedge permission handling.
	svc.SetMode(Mode("bogus"))
	assert.Equal(t, ModeDefault, svc.Mode())
}

// requestWithTimeout runs Request with a deadline so a policy bug that
// falls through to the interactive prompt fails the test instead of
// hanging it.
func requestWithTimeout(t *testing.T, svc Service, req CreatePermissionRequest) (bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	return svc.Request(ctx, req)
}

func TestAcceptEditsMode(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()

	editRequest := func(tool, action, path string) CreatePermissionRequest {
		return CreatePermissionRequest{
			SessionID:  "s1",
			ToolCallID: "call-" + tool,
			ToolName:   tool,
			Action:     action,
			Path:       path,
		}
	}

	t.Run("auto-approves file edits inside the working directory", func(t *testing.T) {
		t.Parallel()
		svc := NewPermissionService(workingDir, false, nil)
		svc.SetMode(ModeAcceptEdits)
		notifications := svc.SubscribeNotifications(t.Context())

		for _, tool := range []string{"edit", "multiedit", "write"} {
			// The edit tools report an inside-cwd path as the working
			// directory itself (fsext.PathOrPrefix).
			granted, err := requestWithTimeout(t, svc, editRequest(tool, "write", workingDir))
			require.NoError(t, err, tool)
			assert.True(t, granted, "%s inside the working dir should be auto-approved", tool)

			notification := <-notifications
			assert.True(t, notification.Payload.Granted, "%s should notify granted", tool)
		}
	})

	t.Run("still asks for edits outside the working directory", func(t *testing.T) {
		t.Parallel()
		svc := NewPermissionService(workingDir, false, nil)
		svc.SetMode(ModeAcceptEdits)
		events := svc.Subscribe(t.Context())

		var (
			wg      sync.WaitGroup
			granted bool
			err     error
		)
		outside := filepath.Join(t.TempDir(), "outside.txt")
		wg.Go(func() {
			granted, err = svc.Request(t.Context(), editRequest("edit", "write", outside))
		})

		select {
		case event := <-events:
			svc.Deny(event.Payload)
		case <-time.After(2 * time.Second):
			t.Fatal("edit outside the working dir should publish an interactive request")
		}
		wg.Wait()
		require.NoError(t, err)
		assert.False(t, granted)
	})

	t.Run("still asks for non-edit tools", func(t *testing.T) {
		t.Parallel()
		svc := NewPermissionService(workingDir, false, nil)
		svc.SetMode(ModeAcceptEdits)
		events := svc.Subscribe(t.Context())

		for _, tc := range []struct{ tool, action string }{
			{"bash", "execute"},
			{"fetch", "fetch"},
			{"download", "download"},
		} {
			var (
				wg      sync.WaitGroup
				granted bool
				err     error
			)
			wg.Go(func() {
				granted, err = svc.Request(t.Context(), editRequest(tc.tool, tc.action, workingDir))
			})

			select {
			case event := <-events:
				assert.Equal(t, tc.tool, event.Payload.ToolName)
				svc.Deny(event.Payload)
			case <-time.After(2 * time.Second):
				t.Fatalf("%s should keep the normal ask flow in accept_edits mode", tc.tool)
			}
			wg.Wait()
			require.NoError(t, err)
			assert.False(t, granted)
		}
	})
}

func TestPlanMode(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()

	t.Run("denies mutating requests without prompting", func(t *testing.T) {
		t.Parallel()
		svc := NewPermissionService(workingDir, false, nil)
		svc.SetMode(ModePlan)
		notifications := svc.SubscribeNotifications(t.Context())

		for _, tc := range []struct{ tool, action string }{
			{"edit", "write"},
			{"write", "write"},
			{"multiedit", "write"},
			{"bash", "execute"},
			{"download", "download"},
			{"mcp_github_create_issue", "execute"},
			{"future_tool", "unknown-action"}, // fail closed on unknown actions
		} {
			granted, err := requestWithTimeout(t, svc, CreatePermissionRequest{
				SessionID:  "s1",
				ToolCallID: "call-" + tc.tool,
				ToolName:   tc.tool,
				Action:     tc.action,
				Path:       workingDir,
			})
			require.NoError(t, err, tc.tool)
			assert.False(t, granted, "%s must be denied in plan mode", tc.tool)

			notification := <-notifications
			assert.True(t, notification.Payload.Denied, "%s should notify denied", tc.tool)
		}
	})

	t.Run("denies mutating requests even when allowed_tools covers them", func(t *testing.T) {
		t.Parallel()
		svc := NewPermissionService(workingDir, false, []string{"bash", "edit:write"})
		svc.SetMode(ModePlan)

		for _, tc := range []struct{ tool, action string }{
			{"bash", "execute"},
			{"edit", "write"},
		} {
			granted, err := requestWithTimeout(t, svc, CreatePermissionRequest{
				SessionID: "s1",
				ToolName:  tc.tool,
				Action:    tc.action,
				Path:      workingDir,
			})
			require.NoError(t, err, tc.tool)
			assert.False(t, granted, "plan mode must beat the static allowlist for %s", tc.tool)
		}
	})

	t.Run("read-only requests keep the normal ask flow", func(t *testing.T) {
		t.Parallel()
		svc := NewPermissionService(workingDir, false, nil)
		svc.SetMode(ModePlan)
		events := svc.Subscribe(t.Context())

		for _, tc := range []struct{ tool, action string }{
			{"view", "read"},
			{"ls", "list"},
			{"fetch", "fetch"},
			{"plan_exit", "execute"},
		} {
			var (
				wg      sync.WaitGroup
				granted bool
				err     error
			)
			wg.Go(func() {
				granted, err = svc.Request(t.Context(), CreatePermissionRequest{
					SessionID: "s1",
					ToolName:  tc.tool,
					Action:    tc.action,
					Path:      workingDir,
				})
			})

			select {
			case event := <-events:
				assert.Equal(t, tc.tool, event.Payload.ToolName)
				svc.Grant(event.Payload)
			case <-time.After(2 * time.Second):
				t.Fatalf("%s should ask (not auto-deny) in plan mode", tc.tool)
			}
			wg.Wait()
			require.NoError(t, err)
			assert.True(t, granted)
		}
	})

	t.Run("yolo skip wins over plan mode", func(t *testing.T) {
		t.Parallel()
		svc := NewPermissionService(workingDir, true, nil)
		svc.SetMode(ModePlan)

		granted, err := requestWithTimeout(t, svc, CreatePermissionRequest{
			SessionID: "s1",
			ToolName:  "bash",
			Action:    "execute",
			Path:      workingDir,
		})
		require.NoError(t, err)
		assert.True(t, granted, "--yolo must keep skipping every request regardless of mode")
	})
}

func TestModeTransitions(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	svc := NewPermissionService(workingDir, false, nil)

	editReq := CreatePermissionRequest{
		SessionID: "s1",
		ToolName:  "edit",
		Action:    "write",
		Path:      workingDir,
	}

	// Plan denies the edit...
	svc.SetMode(ModePlan)
	granted, err := requestWithTimeout(t, svc, editReq)
	require.NoError(t, err)
	assert.False(t, granted)

	// ...accept_edits auto-approves it...
	svc.SetMode(ModeAcceptEdits)
	granted, err = requestWithTimeout(t, svc, editReq)
	require.NoError(t, err)
	assert.True(t, granted)

	// ...and default prompts again.
	svc.SetMode(ModeDefault)
	events := svc.Subscribe(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		granted, err = svc.Request(t.Context(), editReq)
	})
	select {
	case event := <-events:
		svc.Deny(event.Payload)
	case <-time.After(2 * time.Second):
		t.Fatal("default mode should prompt for the edit")
	}
	wg.Wait()
	require.NoError(t, err)
	assert.False(t, granted)
}

func TestPlanModeToolFilter(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"glob", "grep", "ls", "view", "fetch", "agent", "web_search", "todos", "plan_exit", "lsp_diagnostics"} {
		assert.True(t, ModePlan.AllowsTool(name), "plan mode should advertise %s", name)
	}
	for _, name := range []string{"bash", "edit", "multiedit", "write", "download", "job_kill", "lsp_restart", "mcp_github_create_issue"} {
		assert.False(t, ModePlan.AllowsTool(name), "plan mode must not advertise %s", name)
	}

	// The other modes leave the tool list alone.
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
		assert.True(t, mode.AllowsTool("bash"), "%s should not filter tools", mode)
		assert.True(t, mode.AllowsTool("mcp_github_create_issue"), "%s should not filter MCP tools", mode)
	}

	// Unknown modes fall back to the default policy (fail open on tool
	// advertisement, since request policy still applies).
	assert.True(t, Mode("bogus").AllowsTool("bash"))
}

func TestPlanModePromptAddition(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, ModePlan.PromptAddition())
	assert.Contains(t, ModePlan.PromptAddition(), "plan_exit")
	assert.Empty(t, ModeDefault.PromptAddition())
	assert.Empty(t, ModeAcceptEdits.PromptAddition())
}

package model

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/commands"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/google/uuid"
)

// expandCustomCommand expands a custom command body — running inline
// !`cmd` segments and inlining @file references — off the update loop,
// then sends the result as the user prompt. Each !`cmd` goes through the
// same permission flow as the bash tool: safe read-only commands run
// straight away, frontmatter allowed-tools patterns pre-approve, and
// everything else raises the interactive permission dialog. Any denial
// or expansion failure aborts the invocation with a visible error;
// nothing is sent.
func (m *UI) expandCustomCommand(content string, allowedTools []string) tea.Cmd {
	ws := m.com.Workspace
	workingDir := ws.WorkingDir()
	var sessionID string
	if m.session != nil {
		sessionID = m.session.ID
	}
	return func() tea.Msg {
		expanded, err := commands.Expand(context.Background(), content, commands.ExpandOptions{
			WorkingDir:   workingDir,
			AllowedTools: allowedTools,
			IsSafeShell:  agenttools.IsSafeReadOnly,
			Ask: func(ctx context.Context, command string) (bool, error) {
				return ws.PermissionRequest(ctx, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					ToolCallID:  "custom-command-" + uuid.NewString(),
					ToolName:    agenttools.BashToolName,
					Action:      "execute",
					Description: fmt.Sprintf("Execute command: %s", command),
					Params: agenttools.BashPermissionsParams{
						Description: "Custom command inline bash",
						Command:     command,
						WorkingDir:  workingDir,
					},
					Path: workingDir,
				})
			},
		})
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("Custom command: %v", err),
			}
		}
		return sendMessageMsg{Content: expanded}
	}
}

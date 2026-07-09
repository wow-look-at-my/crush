package app

import (
	"context"
	"fmt"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/history"
)

// SessionRestorePlan computes — without touching disk — the restore
// plan that would roll the session's file changes back to the state
// they had just before the given message was sent. The boundary
// message must belong to the session.
func (app *App) SessionRestorePlan(ctx context.Context, sessionID, messageID string) (history.RestorePlan, error) {
	boundary, err := app.restoreBoundary(ctx, sessionID, messageID)
	if err != nil {
		return history.RestorePlan{}, err
	}
	files, err := app.History.ListBySession(ctx, sessionID)
	if err != nil {
		return history.RestorePlan{}, fmt.Errorf("failed to load session file history: %w", err)
	}
	return history.ComputeRestorePlan(files, sessionID, boundary), nil
}

// SessionRestoreFiles rolls the session's file changes back to the
// state they had just before the given message was sent. The restore
// is refused while the agent is running in the session. It is itself
// undoable: every touched file's pre-restore content is recorded as a
// new history version first, so restoring to a later boundary rolls
// the changes forward again.
func (app *App) SessionRestoreFiles(ctx context.Context, sessionID, messageID string) (history.RestoreResult, error) {
	if app.AgentCoordinator != nil && app.AgentCoordinator.IsSessionBusy(sessionID) {
		return history.RestoreResult{}, fmt.Errorf("agent is busy in this session; wait for it to finish before restoring files")
	}
	plan, err := app.SessionRestorePlan(ctx, sessionID, messageID)
	if err != nil {
		return history.RestoreResult{}, err
	}
	result, err := history.ApplyRestorePlan(ctx, app.History, sessionID, app.config.WorkingDir(), plan)
	if result.Changed() > 0 {
		// Best-effort: let LSP servers re-read the restored files so
		// diagnostics reflect what is on disk now.
		tools.NotifyLSPs(ctx, app.LSPManager, "")
	}
	return result, err
}

// restoreBoundary resolves a checkpoint message to its boundary
// timestamp (unix seconds), verifying it belongs to the session.
func (app *App) restoreBoundary(ctx context.Context, sessionID, messageID string) (int64, error) {
	msg, err := app.Messages.Get(ctx, messageID)
	if err != nil {
		return 0, fmt.Errorf("failed to load checkpoint message: %w", err)
	}
	if msg.SessionID != sessionID {
		return 0, fmt.Errorf("message %s does not belong to session %s", messageID, sessionID)
	}
	return msg.CreatedAt, nil
}

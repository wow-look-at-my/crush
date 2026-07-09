package dialog

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// restoreStubWorkspace stubs the three Workspace methods the restore
// dialog uses; everything else panics via the embedded nil interface.
type restoreStubWorkspace struct {
	workspace.Workspace
	msgs []message.Message
	plan history.RestorePlan
}

func (w *restoreStubWorkspace) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return w.msgs, nil
}

func (w *restoreStubWorkspace) SessionRestorePlan(context.Context, string, string) (history.RestorePlan, error) {
	return w.plan, nil
}

func (w *restoreStubWorkspace) WorkingDir() string { return "/w" }

func newTestRestore(t *testing.T, plan history.RestorePlan) *Restore {
	t.Helper()
	s := styles.CharmtonePantera()
	ws := &restoreStubWorkspace{
		// Newest first, matching ListUserMessages order.
		msgs: []message.Message{
			{ID: "msg-2", SessionID: "sess", CreatedAt: 200, Parts: []message.ContentPart{message.TextContent{Text: "second prompt"}}},
			{ID: "msg-1", SessionID: "sess", CreatedAt: 100, Parts: []message.ContentPart{message.TextContent{Text: "first prompt"}}},
		},
		plan: plan,
	}
	r, err := NewRestore(&common.Common{Styles: &s, Workspace: ws}, "sess")
	require.NoError(t, err)
	return r
}

func TestRestoreDialogPickThenConfirm(t *testing.T) {
	t.Parallel()
	plan := history.RestorePlan{Entries: []history.RestoreEntry{
		{Path: "/w/a.go", Op: history.RestoreOpWrite, Content: "x"},
	}}
	r := newTestRestore(t, plan)

	// The most recent user message (the "last checkpoint") is
	// preselected; enter computes the plan and moves to confirm.
	action := r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.Equal(t, restoreStageConfirm, r.stage)
	require.Equal(t, "msg-2", r.picked.messageID)

	// Cancel is preselected: enter goes back to the picker.
	action = r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.Equal(t, restoreStagePick, r.stage)

	// Pick again, flip to Restore, confirm.
	r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	action = r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	restoreAction, ok := action.(ActionRestoreCheckpoint)
	require.True(t, ok)
	require.Equal(t, "sess", restoreAction.SessionID)
	require.Equal(t, "msg-2", restoreAction.MessageID)
}

func TestRestoreDialogConfirmShortcuts(t *testing.T) {
	t.Parallel()
	plan := history.RestorePlan{Entries: []history.RestoreEntry{
		{Path: "/w/a.go", Op: history.RestoreOpWrite, Content: "x"},
	}}

	r := newTestRestore(t, plan)
	r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	action := r.HandleMsg(keyMsg('y'))
	_, ok := action.(ActionRestoreCheckpoint)
	require.True(t, ok)

	r = newTestRestore(t, plan)
	r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	action = r.HandleMsg(keyMsg('n'))
	require.Nil(t, action)
	require.Equal(t, restoreStagePick, r.stage)
}

func TestRestoreDialogPickSecondCheckpoint(t *testing.T) {
	t.Parallel()
	plan := history.RestorePlan{Entries: []history.RestoreEntry{
		{Path: "/w/a.go", Op: history.RestoreOpDelete},
	}}
	r := newTestRestore(t, plan)

	r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, restoreStageConfirm, r.stage)
	require.Equal(t, "msg-1", r.picked.messageID)
}

func TestRestoreDialogEmptyPlanShowsNotice(t *testing.T) {
	t.Parallel()
	r := newTestRestore(t, history.RestorePlan{})

	action := r.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action)
	require.Equal(t, restoreStagePick, r.stage)
	require.NotEmpty(t, r.notice)
}

func TestRestoreCheckpointLabel(t *testing.T) {
	t.Parallel()
	require.Equal(t, "fix the bug", restoreCheckpointLabel("fix the bug"))
	require.Equal(t, "first line", restoreCheckpointLabel("first line\nsecond line"))
	require.Equal(t, "after blanks", restoreCheckpointLabel("\n  \nafter blanks\nmore"))
	require.Equal(t, "(no message text)", restoreCheckpointLabel(""))
	require.Equal(t, "(no message text)", restoreCheckpointLabel("  \n\t\n"))
}

func TestRestorePlanSummary(t *testing.T) {
	t.Parallel()
	writes := history.RestoreEntry{Path: "/w/a.go", Op: history.RestoreOpWrite, Content: "x"}
	deletes := history.RestoreEntry{Path: "/w/b.go", Op: history.RestoreOpDelete}

	require.Equal(t, "Restore 1 file?", restorePlanSummary(history.RestorePlan{
		Entries: []history.RestoreEntry{writes},
	}))
	require.Equal(t, "Delete 1 file created after this checkpoint?", restorePlanSummary(history.RestorePlan{
		Entries: []history.RestoreEntry{deletes},
	}))
	require.Equal(t, "Restore 2 files and delete 1 file?", restorePlanSummary(history.RestorePlan{
		Entries: []history.RestoreEntry{writes, writes, deletes},
	}))
}

func TestRestorePlanFileList(t *testing.T) {
	t.Parallel()
	entries := []history.RestoreEntry{
		{Path: "/work/pkg/a.go", Op: history.RestoreOpWrite, Content: "x"},
		{Path: "/work/pkg/b.go", Op: history.RestoreOpDelete},
	}
	got := restorePlanFileList(history.RestorePlan{Entries: entries}, "/work", 80)
	require.Equal(t, "restore pkg/a.go\ndelete pkg/b.go", got)
}

func TestRestorePlanFileListTruncates(t *testing.T) {
	t.Parallel()
	var entries []history.RestoreEntry
	for range maxRestorePlanFiles + 3 {
		entries = append(entries, history.RestoreEntry{Path: "/work/file.go", Op: history.RestoreOpWrite})
	}
	got := restorePlanFileList(history.RestorePlan{Entries: entries}, "/work", 80)
	lines := strings.Split(got, "\n")
	require.Len(t, lines, maxRestorePlanFiles+1)
	require.Equal(t, "…and 3 more", lines[maxRestorePlanFiles])
}

func TestRestoreSummary(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		"Files already match the checkpoint; nothing to restore",
		RestoreSummary(history.RestoreResult{}))
	require.Equal(t,
		"Checkpoint restore: restored 2 files",
		RestoreSummary(history.RestoreResult{Restored: []string{"a", "b"}}))
	require.Equal(t,
		"Checkpoint restore: restored 1 file, deleted 2 files",
		RestoreSummary(history.RestoreResult{Restored: []string{"a"}, Deleted: []string{"b", "c"}}))
	require.Equal(t,
		"Checkpoint restore: deleted 1 file, skipped 1 file outside the working directory",
		RestoreSummary(history.RestoreResult{Deleted: []string{"a"}, Skipped: []string{"/etc/x"}}))
}

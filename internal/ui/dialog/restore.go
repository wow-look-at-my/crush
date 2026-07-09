package dialog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/util"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// RestoreID is the identifier for the checkpoint-restore dialog.
const RestoreID = "restore"

// maxRestorePlanFiles is how many affected files the confirmation stage
// lists before truncating with an "…and N more" hint.
const maxRestorePlanFiles = 8

type restoreStage uint8

const (
	restoreStagePick restoreStage = iota
	restoreStageConfirm
)

// restoreCheckpoint is one selectable boundary: a user message.
type restoreCheckpoint struct {
	messageID string
	label     string
	when      time.Time
}

// Restore lets the user roll the session's file changes back to a
// checkpoint (the moment just before a user message was sent). It has
// two stages: pick a checkpoint, then confirm the computed plan.
type Restore struct {
	com       *common.Common
	sessionID string
	help      help.Model
	list      *list.FilterableList
	stage     restoreStage
	notice    string

	checkpoints map[string]restoreCheckpoint

	// Confirm-stage state.
	picked     restoreCheckpoint
	plan       history.RestorePlan
	confirmYes bool

	keyMap struct {
		Select,
		Next,
		Previous,
		UpDown,
		LeftRight,
		Tab,
		Yes,
		No,
		Back,
		Close key.Binding
	}
}

var _ Dialog = (*Restore)(nil)

// NewRestore creates the checkpoint-restore dialog for a session. The
// checkpoints offered are the session's user messages, newest first;
// the most recent one (the "last checkpoint") is preselected.
func NewRestore(com *common.Common, sessionID string) (*Restore, error) {
	msgs, err := com.Workspace.ListUserMessages(context.TODO(), sessionID)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New("this session has no checkpoints yet — send a message first")
	}

	r := &Restore{
		com:         com,
		sessionID:   sessionID,
		checkpoints: make(map[string]restoreCheckpoint, len(msgs)),
	}

	items := make([]list.FilterableItem, 0, len(msgs))
	for _, msg := range msgs {
		cp := restoreCheckpoint{
			messageID: msg.ID,
			label:     restoreCheckpointLabel(msg.Content().Text),
			when:      time.Unix(msg.CreatedAt, 0),
		}
		r.checkpoints[msg.ID] = cp
		item := NewCommandItem(com.Styles, msg.ID, cp.label, "", nil).
			WithDescription(cp.when.Format("Jan 2 15:04:05"))
		items = append(items, item)
	}

	r.help = help.New()
	r.help.Styles = com.Styles.DialogHelpStyles()

	r.list = list.NewFilterableList(items...)
	r.list.Focus()
	r.list.SetSelected(0)

	r.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "choose"),
	)
	r.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	r.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	r.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑↓", "choose"),
	)
	r.keyMap.LeftRight = key.NewBinding(
		key.WithKeys("left", "right"),
		key.WithHelp("←/→", "switch options"),
	)
	r.keyMap.Tab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch options"),
	)
	r.keyMap.Yes = key.NewBinding(
		key.WithKeys("y", "Y"),
		key.WithHelp("y", "restore"),
	)
	r.keyMap.No = key.NewBinding(
		key.WithKeys("n", "N"),
		key.WithHelp("n", "back"),
	)
	r.keyMap.Back = key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	)
	r.keyMap.Close = CloseKey

	return r, nil
}

// ID implements [Dialog].
func (*Restore) ID() string {
	return RestoreID
}

// HandleMsg implements [Dialog].
func (r *Restore) HandleMsg(msg tea.Msg) Action {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	if r.stage == restoreStageConfirm {
		return r.handleConfirmKey(keyMsg)
	}
	return r.handlePickKey(keyMsg)
}

func (r *Restore) handlePickKey(msg tea.KeyPressMsg) Action {
	switch {
	case key.Matches(msg, r.keyMap.Close):
		return ActionClose{}
	case key.Matches(msg, r.keyMap.Previous):
		r.list.Focus()
		if r.list.IsSelectedFirst() {
			r.list.SelectLast()
		} else {
			r.list.SelectPrev()
		}
		r.list.ScrollToSelected()
	case key.Matches(msg, r.keyMap.Next):
		r.list.Focus()
		if r.list.IsSelectedLast() {
			r.list.SelectFirst()
		} else {
			r.list.SelectNext()
		}
		r.list.ScrollToSelected()
	case key.Matches(msg, r.keyMap.Select):
		item, ok := r.list.SelectedItem().(*CommandItem)
		if !ok || item == nil {
			return nil
		}
		cp, ok := r.checkpoints[item.ID()]
		if !ok {
			return nil
		}
		plan, err := r.com.Workspace.SessionRestorePlan(context.TODO(), r.sessionID, cp.messageID)
		if err != nil {
			return ActionCmd{util.ReportError(err)}
		}
		if plan.Empty() {
			r.notice = "No file changes to roll back after this checkpoint."
			return nil
		}
		r.picked = cp
		r.plan = plan
		r.confirmYes = false
		r.notice = ""
		r.stage = restoreStageConfirm
	}
	return nil
}

func (r *Restore) handleConfirmKey(msg tea.KeyPressMsg) Action {
	back := func() Action {
		r.stage = restoreStagePick
		return nil
	}
	switch {
	case key.Matches(msg, r.keyMap.LeftRight, r.keyMap.Tab):
		r.confirmYes = !r.confirmYes
	case key.Matches(msg, r.keyMap.Yes):
		return r.confirmAction()
	case key.Matches(msg, r.keyMap.No), key.Matches(msg, r.keyMap.Back):
		return back()
	case key.Matches(msg, r.keyMap.Select):
		if r.confirmYes {
			return r.confirmAction()
		}
		return back()
	}
	return nil
}

func (r *Restore) confirmAction() Action {
	return ActionRestoreCheckpoint{
		SessionID: r.sessionID,
		MessageID: r.picked.messageID,
	}
}

// Draw implements [Dialog].
func (r *Restore) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := r.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()

	rc := NewRenderContext(t, width)
	rc.Title = "Restore Checkpoint"

	if r.stage == restoreStageConfirm {
		rc.Gap = 1
		rc.AddPart(t.Dialog.PrimaryText.Render(restorePlanSummary(r.plan)))
		when := r.picked.when.Format("Jan 2 15:04:05")
		label := ansi.Truncate(r.picked.label, max(0, innerWidth-len(when)-4), "…")
		rc.AddPart(t.Dialog.SecondaryText.Render(fmt.Sprintf("Checkpoint: %s — %s", when, label)))
		rc.AddPart(t.Dialog.SecondaryText.Render(restorePlanFileList(r.plan, r.com.Workspace.WorkingDir(), innerWidth)))
		buttons := common.ButtonGroup(t, []common.ButtonOpts{
			{Text: "Restore", Selected: r.confirmYes, Padding: 2},
			{Text: "Cancel", Selected: !r.confirmYes, Padding: 2},
		}, " ")
		rc.AddPart(lipgloss.NewStyle().Width(innerWidth).AlignHorizontal(lipgloss.Center).Render(buttons))
	} else {
		height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
		heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
			t.Dialog.HelpView.GetVerticalFrameSize() +
			t.Dialog.View.GetVerticalFrameSize() + 2 // hint/notice line
		rc.AddPart(t.Dialog.SecondaryText.Render(r.pickHint()))
		r.list.SetSize(innerWidth, max(1, height-heightOffset))
		rc.AddPart(t.Dialog.List.Height(r.list.Height()).Render(r.list.Render()))
	}

	r.help.SetWidth(innerWidth)
	rc.Help = r.help.View(r)

	DrawCenter(scr, area, rc.Render())
	return nil
}

func (r *Restore) pickHint() string {
	if r.notice != "" {
		return r.notice
	}
	return "Roll back file changes made after a message."
}

// ShortHelp implements [help.KeyMap].
func (r *Restore) ShortHelp() []key.Binding {
	if r.stage == restoreStageConfirm {
		return []key.Binding{
			r.keyMap.LeftRight,
			r.keyMap.Yes,
			r.keyMap.No,
		}
	}
	return []key.Binding{
		r.keyMap.UpDown,
		r.keyMap.Select,
		r.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (r *Restore) FullHelp() [][]key.Binding {
	if r.stage == restoreStageConfirm {
		return [][]key.Binding{
			{r.keyMap.LeftRight, r.keyMap.Yes, r.keyMap.No, r.keyMap.Back},
		}
	}
	return [][]key.Binding{
		{r.keyMap.UpDown, r.keyMap.Select, r.keyMap.Close},
	}
}

// restoreCheckpointLabel reduces a message's text to a one-line label:
// its first non-blank line.
func restoreCheckpointLabel(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return "(no message text)"
}

// restorePlanSummary describes what applying the plan will do, e.g.
// "Restore 3 files and delete 1 file?".
func restorePlanSummary(plan history.RestorePlan) string {
	writes, deletes := plan.Writes(), plan.Deletes()
	switch {
	case writes > 0 && deletes > 0:
		return fmt.Sprintf("Restore %s and delete %s?", countFiles(writes), countFiles(deletes))
	case deletes > 0:
		return fmt.Sprintf("Delete %s created after this checkpoint?", countFiles(deletes))
	default:
		return fmt.Sprintf("Restore %s?", countFiles(writes))
	}
}

// restorePlanFileList renders the affected paths (relative to cwd),
// truncated to maxRestorePlanFiles.
func restorePlanFileList(plan history.RestorePlan, cwd string, width int) string {
	lines := make([]string, 0, min(len(plan.Entries), maxRestorePlanFiles)+1)
	for i, entry := range plan.Entries {
		if i >= maxRestorePlanFiles {
			lines = append(lines, fmt.Sprintf("…and %d more", len(plan.Entries)-maxRestorePlanFiles))
			break
		}
		verb := "restore"
		if entry.Op == history.RestoreOpDelete {
			verb = "delete"
		}
		path := entry.Path
		if rel, err := filepath.Rel(cwd, path); err == nil {
			path = rel
		}
		path = fsext.DirTrim(path, 2)
		lines = append(lines, ansi.Truncate(fmt.Sprintf("%s %s", verb, path), max(0, width), "…"))
	}
	return strings.Join(lines, "\n")
}

// RestoreSummary renders the post-apply toast for a restore result.
func RestoreSummary(res history.RestoreResult) string {
	var parts []string
	if n := len(res.Restored); n > 0 {
		parts = append(parts, fmt.Sprintf("restored %s", countFiles(n)))
	}
	if n := len(res.Deleted); n > 0 {
		parts = append(parts, fmt.Sprintf("deleted %s", countFiles(n)))
	}
	if n := len(res.Skipped); n > 0 {
		parts = append(parts, fmt.Sprintf("skipped %s outside the working directory", countFiles(n)))
	}
	if len(parts) == 0 {
		return "Files already match the checkpoint; nothing to restore"
	}
	return "Checkpoint restore: " + strings.Join(parts, ", ")
}

// countFiles pluralizes a file count.
func countFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

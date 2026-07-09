package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestRestoreRoundTripWithRealService drives the full checkpoint flow
// against the real file-version service and database: record versions
// the way the edit/write tools do, restore to a boundary, then roll
// the restore forward again using the versions the restore recorded.
func TestRestoreRoundTripWithRealService(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)

	queries := db.New(conn)
	_, err = queries.CreateSession(ctx, db.CreateSessionParams{
		ID:    restoreSession,
		Title: "restore test",
	})
	require.NoError(t, err)

	svc := NewService(queries, conn)

	workDir := t.TempDir()
	edited := filepath.Join(workDir, "edited.go")
	created := filepath.Join(workDir, "created.go")

	// First touch of an existing file: initial (pre-edit) content plus
	// the edited version — the edit tool's recording pattern.
	_, err = svc.Create(ctx, restoreSession, edited, "original")
	require.NoError(t, err)
	_, err = svc.CreateVersion(ctx, restoreSession, edited, "edited")
	require.NoError(t, err)

	// A file created by the session: empty initial version.
	_, err = svc.Create(ctx, restoreSession, created, "")
	require.NoError(t, err)
	_, err = svc.CreateVersion(ctx, restoreSession, created, "brand new")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(edited, []byte("edited"), 0o644))
	require.NoError(t, os.WriteFile(created, []byte("brand new"), 0o644))

	// Backdate the recorded edits so the versions the restore itself is
	// about to record land at a strictly later timestamp — timestamps
	// have second resolution, and this test runs in well under one.
	_, err = conn.ExecContext(ctx, "UPDATE files SET created_at = created_at - 100")
	require.NoError(t, err)

	rows, err := svc.ListBySession(ctx, restoreSession)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	editTime := rows[len(rows)-1].CreatedAt

	// Restore to a boundary at the first edit: every recorded version
	// is at-or-after it, so the edit is rolled back and the created
	// file removed.
	plan := ComputeRestorePlan(rows, restoreSession, editTime)
	require.Len(t, plan.Entries, 2)

	res, err := ApplyRestorePlan(ctx, svc, restoreSession, workDir, plan)
	require.NoError(t, err)
	require.Equal(t, []string{edited}, res.Restored)
	require.Equal(t, []string{created}, res.Deleted)

	got, err := os.ReadFile(edited)
	require.NoError(t, err)
	require.Equal(t, "original", string(got))
	require.NoFileExists(t, created)

	// The restore recorded a pre-restore backup and the applied state
	// for each file, at a later timestamp than the backdated edits.
	rows, err = svc.ListBySession(ctx, restoreSession)
	require.NoError(t, err)
	require.Len(t, rows, 8)
	for _, r := range rows[4:] {
		require.Greater(t, r.CreatedAt, editTime)
	}

	// Roll forward: a boundary after the edits but before the restore
	// targets the post-edit state, bringing both files back.
	plan = ComputeRestorePlan(rows, restoreSession, editTime+50)
	res, err = ApplyRestorePlan(ctx, svc, restoreSession, workDir, plan)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{created, edited}, res.Restored)
	require.Empty(t, res.Deleted)

	got, err = os.ReadFile(created)
	require.NoError(t, err)
	require.Equal(t, "brand new", string(got))
	got, err = os.ReadFile(edited)
	require.NoError(t, err)
	require.Equal(t, "edited", string(got))
}

// TestRestoreBoundarySameSecondRollsBack pins the boundary comparison:
// a version recorded in the same second as the checkpoint message is
// treated as after it (tool runs follow the message that triggered
// them), so it gets rolled back.
func TestRestoreBoundarySameSecondRollsBack(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	rows := []File{
		row(restoreSession, "/w/a.go", "original", 0, now-10),
		row(restoreSession, "/w/a.go", "edited", 1, now),
	}
	plan := ComputeRestorePlan(rows, restoreSession, now)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, "original", plan.Entries[0].Content)
}

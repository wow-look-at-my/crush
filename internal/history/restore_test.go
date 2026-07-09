package history

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const restoreSession = "sess-1"

// row builds a fixture file-version row.
func row(sessionID, path, content string, version, createdAt int64) File {
	return File{
		ID:        fmt.Sprintf("%s-v%d", path, version),
		SessionID: sessionID,
		Path:      path,
		Content:   content,
		Version:   version,
		CreatedAt: createdAt,
	}
}

func TestComputeRestorePlanPicksVersionAtBoundary(t *testing.T) {
	t.Parallel()
	files := []File{
		row(restoreSession, "/w/a.go", "original", 0, 100), // initial (pre-session content)
		row(restoreSession, "/w/a.go", "edit one", 1, 110),
		row(restoreSession, "/w/a.go", "edit two", 2, 210),
		row(restoreSession, "/w/a.go", "edit three", 3, 300),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, RestoreEntry{Path: "/w/a.go", Op: RestoreOpWrite, Content: "edit one"}, plan.Entries[0])
	require.Equal(t, 1, plan.Writes())
	require.Equal(t, 0, plan.Deletes())
}

func TestComputeRestorePlanFirstTouchAfterBoundaryUsesInitial(t *testing.T) {
	t.Parallel()
	files := []File{
		row(restoreSession, "/w/b.go", "pre-session content", 0, 250), // first touch after boundary
		row(restoreSession, "/w/b.go", "session edit", 1, 251),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, RestoreEntry{Path: "/w/b.go", Op: RestoreOpWrite, Content: "pre-session content"}, plan.Entries[0])
}

func TestComputeRestorePlanCreatedFileMarkedForDeletion(t *testing.T) {
	t.Parallel()
	files := []File{
		row(restoreSession, "/w/new.go", "", 0, 250), // empty initial = did not exist
		row(restoreSession, "/w/new.go", "created content", 1, 251),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, RestoreEntry{Path: "/w/new.go", Op: RestoreOpDelete}, plan.Entries[0])
	require.Equal(t, 1, plan.Deletes())
}

func TestComputeRestorePlanCreatedBeforeBoundaryRestoresContent(t *testing.T) {
	t.Parallel()
	// File created by the session before the boundary, edited after:
	// the target is the pre-boundary content, not a deletion.
	files := []File{
		row(restoreSession, "/w/new.go", "", 0, 100),
		row(restoreSession, "/w/new.go", "first content", 1, 101),
		row(restoreSession, "/w/new.go", "later content", 2, 300),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, RestoreEntry{Path: "/w/new.go", Op: RestoreOpWrite, Content: "first content"}, plan.Entries[0])
}

func TestComputeRestorePlanEmptyTargetOnPreExistingFileWrites(t *testing.T) {
	t.Parallel()
	// The file pre-existed with content, and the newest pre-boundary
	// version is empty: restore writes an empty file, never deletes.
	files := []File{
		row(restoreSession, "/w/keep.go", "original", 0, 100),
		row(restoreSession, "/w/keep.go", "", 1, 150),
		row(restoreSession, "/w/keep.go", "post", 2, 300),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, RestoreEntry{Path: "/w/keep.go", Op: RestoreOpWrite, Content: ""}, plan.Entries[0])
}

func TestComputeRestorePlanUntouchedFileAbsent(t *testing.T) {
	t.Parallel()
	files := []File{
		row(restoreSession, "/w/old.go", "original", 0, 100),
		row(restoreSession, "/w/old.go", "edited", 1, 110),
		row(restoreSession, "/w/hot.go", "original", 0, 100),
		row(restoreSession, "/w/hot.go", "edited", 1, 300),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, "/w/hot.go", plan.Entries[0].Path)
}

func TestComputeRestorePlanBoundaryAfterEverythingIsEmpty(t *testing.T) {
	t.Parallel()
	files := []File{
		row(restoreSession, "/w/a.go", "original", 0, 100),
		row(restoreSession, "/w/a.go", "edited", 1, 110),
	}

	plan := ComputeRestorePlan(files, restoreSession, 500)
	require.True(t, plan.Empty())
}

func TestComputeRestorePlanExcludesOtherSessions(t *testing.T) {
	t.Parallel()
	// Child (sub-agent) sessions record versions under their own IDs;
	// those rows never contribute to this session's plan.
	files := []File{
		row(restoreSession, "/w/a.go", "original", 0, 100),
		row(restoreSession, "/w/a.go", "edited", 1, 300),
		row("child-sess", "/w/child.go", "", 0, 300),
		row("child-sess", "/w/child.go", "child content", 1, 301),
		row("child-sess", "/w/a.go", "child edit", 5, 400),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, RestoreEntry{Path: "/w/a.go", Op: RestoreOpWrite, Content: "original"}, plan.Entries[0])
}

func TestComputeRestorePlanSameSecondVersionsOrderedByVersion(t *testing.T) {
	t.Parallel()
	// Several versions can share one created_at second; version order
	// decides which one is newest at the boundary. Rows are passed
	// unsorted to prove the plan does not rely on input order.
	files := []File{
		row(restoreSession, "/w/a.go", "second", 2, 100),
		row(restoreSession, "/w/a.go", "original", 0, 100),
		row(restoreSession, "/w/a.go", "first", 1, 100),
		row(restoreSession, "/w/a.go", "after", 3, 300),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 1)
	require.Equal(t, "second", plan.Entries[0].Content)
}

func TestComputeRestorePlanEntriesSortedByPath(t *testing.T) {
	t.Parallel()
	files := []File{
		row(restoreSession, "/w/z.go", "orig z", 0, 100),
		row(restoreSession, "/w/z.go", "new z", 1, 300),
		row(restoreSession, "/w/a.go", "orig a", 0, 100),
		row(restoreSession, "/w/a.go", "new a", 1, 300),
	}

	plan := ComputeRestorePlan(files, restoreSession, 200)
	require.Len(t, plan.Entries, 2)
	require.Equal(t, "/w/a.go", plan.Entries[0].Path)
	require.Equal(t, "/w/z.go", plan.Entries[1].Path)
}

// recorderFake captures CreateVersion calls for apply tests.
type recorderFake struct {
	calls []recordedVersion
	fail  bool
}

type recordedVersion struct {
	sessionID string
	path      string
	content   string
}

func (r *recorderFake) CreateVersion(_ context.Context, sessionID, path, content string) (File, error) {
	if r.fail {
		return File{}, errors.New("recorder boom")
	}
	r.calls = append(r.calls, recordedVersion{sessionID: sessionID, path: path, content: content})
	return File{SessionID: sessionID, Path: path, Content: content}, nil
}

func TestApplyRestorePlanWritesAndDeletes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	edited := filepath.Join(dir, "sub", "edited.go")
	created := filepath.Join(dir, "created.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(edited), 0o755))
	require.NoError(t, os.WriteFile(edited, []byte("current content"), 0o644))
	require.NoError(t, os.WriteFile(created, []byte("session made me"), 0o644))

	rec := &recorderFake{}
	plan := RestorePlan{Entries: []RestoreEntry{
		{Path: created, Op: RestoreOpDelete},
		{Path: edited, Op: RestoreOpWrite, Content: "boundary content"},
	}}

	res, err := ApplyRestorePlan(context.Background(), rec, restoreSession, dir, plan)
	require.NoError(t, err)
	require.Equal(t, []string{edited}, res.Restored)
	require.Equal(t, []string{created}, res.Deleted)
	require.Empty(t, res.Unchanged)
	require.Empty(t, res.Skipped)
	require.Equal(t, 2, res.Changed())

	got, err := os.ReadFile(edited)
	require.NoError(t, err)
	require.Equal(t, "boundary content", string(got))
	require.NoFileExists(t, created)

	// Undoability: each changed file records its pre-restore content
	// first, then the applied state.
	require.Equal(t, []recordedVersion{
		{sessionID: restoreSession, path: created, content: "session made me"},
		{sessionID: restoreSession, path: created, content: ""},
		{sessionID: restoreSession, path: edited, content: "current content"},
		{sessionID: restoreSession, path: edited, content: "boundary content"},
	}, rec.calls)
}

func TestApplyRestorePlanRecreatesMissingFileAndDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gone := filepath.Join(dir, "deep", "nested", "gone.go")

	rec := &recorderFake{}
	plan := RestorePlan{Entries: []RestoreEntry{
		{Path: gone, Op: RestoreOpWrite, Content: "back again"},
	}}

	res, err := ApplyRestorePlan(context.Background(), rec, restoreSession, dir, plan)
	require.NoError(t, err)
	require.Equal(t, []string{gone}, res.Restored)

	got, err := os.ReadFile(gone)
	require.NoError(t, err)
	require.Equal(t, "back again", string(got))
}

func TestApplyRestorePlanUnchangedLeavesNoVersions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	same := filepath.Join(dir, "same.go")
	require.NoError(t, os.WriteFile(same, []byte("already there"), 0o644))
	absent := filepath.Join(dir, "absent.go")

	rec := &recorderFake{}
	plan := RestorePlan{Entries: []RestoreEntry{
		{Path: absent, Op: RestoreOpDelete},
		{Path: same, Op: RestoreOpWrite, Content: "already there"},
	}}

	res, err := ApplyRestorePlan(context.Background(), rec, restoreSession, dir, plan)
	require.NoError(t, err)
	require.Empty(t, res.Restored)
	require.Empty(t, res.Deleted)
	require.ElementsMatch(t, []string{same, absent}, res.Unchanged)
	require.Empty(t, rec.calls)
}

func TestApplyRestorePlanRefusesPathsOutsideWorkingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.go")
	require.NoError(t, os.WriteFile(outside, []byte("do not touch"), 0o644))
	escape := filepath.Join(dir, "..", "escape.go")

	rec := &recorderFake{}
	plan := RestorePlan{Entries: []RestoreEntry{
		{Path: escape, Op: RestoreOpWrite, Content: "nope"},
		{Path: outside, Op: RestoreOpDelete},
	}}

	res, err := ApplyRestorePlan(context.Background(), rec, restoreSession, dir, plan)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{escape, outside}, res.Skipped)
	require.Empty(t, rec.calls)

	got, readErr := os.ReadFile(outside)
	require.NoError(t, readErr)
	require.Equal(t, "do not touch", string(got))
	require.NoFileExists(t, filepath.Join(outsideDir, "escape.go"))
}

func TestApplyRestorePlanEmptyWorkingDirRefusesEverything(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "a.go")
	require.NoError(t, os.WriteFile(target, []byte("content"), 0o644))

	rec := &recorderFake{}
	plan := RestorePlan{Entries: []RestoreEntry{{Path: target, Op: RestoreOpDelete}}}

	res, err := ApplyRestorePlan(context.Background(), rec, restoreSession, "", plan)
	require.NoError(t, err)
	require.Equal(t, []string{target}, res.Skipped)
	require.FileExists(t, target)
}

func TestApplyRestorePlanBackupFailureLeavesFileUntouched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "a.go")
	require.NoError(t, os.WriteFile(target, []byte("current"), 0o644))

	rec := &recorderFake{fail: true}
	plan := RestorePlan{Entries: []RestoreEntry{
		{Path: target, Op: RestoreOpWrite, Content: "boundary"},
	}}

	res, err := ApplyRestorePlan(context.Background(), rec, restoreSession, dir, plan)
	require.Error(t, err)
	require.Empty(t, res.Restored)

	got, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, "current", string(got))
}

func TestApplyRestorePlanRoundTripsThroughRecordedVersions(t *testing.T) {
	t.Parallel()
	// A restore can be rolled forward again: the pre-restore backup
	// version it records makes every restored path "touched after" any
	// pre-restore boundary, and the newest pre-boundary version is the
	// last real edit.
	dir := t.TempDir()
	target := filepath.Join(dir, "a.go")
	require.NoError(t, os.WriteFile(target, []byte("edit two"), 0o644))

	rows := []File{
		row(restoreSession, target, "original", 0, 100),
		row(restoreSession, target, "edit one", 1, 110),
		row(restoreSession, target, "edit two", 2, 300),
	}

	// Restore to boundary 200 -> "edit one".
	plan := ComputeRestorePlan(rows, restoreSession, 200)
	rec := &recorderFake{}
	_, err := ApplyRestorePlan(context.Background(), rec, restoreSession, dir, plan)
	require.NoError(t, err)
	got, _ := os.ReadFile(target)
	require.Equal(t, "edit one", string(got))

	// Simulate the recorder persisting the two new versions at t=400.
	rows = append(
		rows,
		row(restoreSession, target, "edit two", 3, 400), // pre-restore backup
		row(restoreSession, target, "edit one", 4, 400), // applied state
	)

	// Roll forward to boundary 350 (after the last edit, before the
	// restore): the plan targets "edit two" again.
	plan = ComputeRestorePlan(rows, restoreSession, 350)
	_, err = ApplyRestorePlan(context.Background(), rec, restoreSession, dir, plan)
	require.NoError(t, err)
	got, _ = os.ReadFile(target)
	require.Equal(t, "edit two", string(got))
}

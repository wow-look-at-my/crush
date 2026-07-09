package history

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/charmbracelet/crush/internal/fsext"
)

// RestoreOp describes what applying a restore plan does to one file.
type RestoreOp string

const (
	// RestoreOpWrite writes the entry's Content to the file.
	RestoreOpWrite RestoreOp = "write"
	// RestoreOpDelete removes the file: it did not exist at the boundary.
	RestoreOpDelete RestoreOp = "delete"
)

// RestoreEntry is one file's target state in a [RestorePlan].
type RestoreEntry struct {
	Path    string
	Op      RestoreOp
	Content string // target content; always empty for RestoreOpDelete
}

// RestorePlan describes, for every file a session touched at or after a
// boundary timestamp, the state that file had just before the boundary.
// Entries are sorted by path.
type RestorePlan struct {
	Boundary int64 // unix seconds
	Entries  []RestoreEntry
}

// Empty reports whether the plan has nothing to do.
func (p RestorePlan) Empty() bool { return len(p.Entries) == 0 }

// Writes returns the number of entries that write content.
func (p RestorePlan) Writes() int {
	n := 0
	for _, e := range p.Entries {
		if e.Op == RestoreOpWrite {
			n++
		}
	}
	return n
}

// Deletes returns the number of entries that remove a file.
func (p RestorePlan) Deletes() int { return len(p.Entries) - p.Writes() }

// ComputeRestorePlan computes the restore plan for a session at a
// boundary timestamp (unix seconds, typically a message's CreatedAt).
// It is pure: file-version rows in, plan out. Disk is never consulted;
// applying the plan is a separate step (see [ApplyRestorePlan]) so
// other sources can reuse the same plan shape.
//
// Semantics:
//
//   - Only rows belonging to sessionID are considered. Sub-agent child
//     sessions record their file versions under their own session IDs,
//     so — like the session file-diff view — a restore never crosses
//     into child sessions.
//   - A file is part of the plan iff it has a version recorded at or
//     after the boundary (CreatedAt >= boundary). Files last touched
//     before the boundary are left alone.
//   - The target state is the newest version recorded strictly before
//     the boundary. Within a session and path, version numbers grow
//     monotonically over time, so "newest" is the highest version among
//     rows with CreatedAt < boundary. If there is none — the session
//     first touched the file after the boundary — the target is the
//     file's initial version (the lowest version), which records the
//     file's content from just before the session first touched it.
//   - Deletion: the edit and write tools record an initial version with
//     empty content when the file did not exist yet, so for such
//     session-created files an empty target means "absent" and the plan
//     marks the file for deletion. (A file that pre-existed empty is
//     indistinguishable from a created one and restores as a deletion
//     too; its content is identical either way.) Files whose initial
//     version has content never get a delete entry — an empty target
//     restores them to an empty file.
func ComputeRestorePlan(files []File, sessionID string, boundary int64) RestorePlan {
	byPath := make(map[string][]File)
	for _, f := range files {
		if f.SessionID != sessionID {
			continue
		}
		byPath[f.Path] = append(byPath[f.Path], f)
	}

	plan := RestorePlan{Boundary: boundary}
	for path, rows := range byPath {
		slices.SortFunc(rows, func(a, b File) int {
			return cmp.Compare(a.Version, b.Version)
		})

		touchedAfter := slices.ContainsFunc(rows, func(f File) bool {
			return f.CreatedAt >= boundary
		})
		if !touchedAfter {
			continue
		}

		// Highest version strictly before the boundary; rows are in
		// version order, so the last match wins. When no version
		// predates the boundary the initial version is the target.
		initial := rows[0]
		target := initial
		for _, r := range rows {
			if r.CreatedAt < boundary {
				target = r
			}
		}

		entry := RestoreEntry{Path: path, Op: RestoreOpWrite, Content: target.Content}
		if initial.Content == "" && target.Content == "" {
			entry = RestoreEntry{Path: path, Op: RestoreOpDelete}
		}
		plan.Entries = append(plan.Entries, entry)
	}

	slices.SortFunc(plan.Entries, func(a, b RestoreEntry) int {
		return cmp.Compare(a.Path, b.Path)
	})
	return plan
}

// VersionRecorder is the narrow slice of [Service] that
// [ApplyRestorePlan] needs to make a restore undoable.
type VersionRecorder interface {
	CreateVersion(ctx context.Context, sessionID, path, content string) (File, error)
}

// RestoreResult summarizes an applied restore plan. All slices hold
// absolute file paths.
type RestoreResult struct {
	Restored  []string // files written back to their boundary content
	Deleted   []string // files removed (they did not exist at the boundary)
	Unchanged []string // already at the target state; untouched
	Skipped   []string // outside the working directory; refused
}

// Changed reports the number of files the restore actually modified.
func (r RestoreResult) Changed() int { return len(r.Restored) + len(r.Deleted) }

// ApplyRestorePlan applies a [RestorePlan] to disk.
//
// The restore is itself undoable: before a file is touched, its current
// disk content is recorded as a new history version (so restoring to a
// later boundary rolls the restore forward again), and the applied
// state is recorded as another version so history keeps matching disk.
// Entries already at their target state are counted as Unchanged and
// leave no new versions behind. Entries whose path is not under
// workingDir are refused and reported in Skipped — a restore never
// writes or deletes outside the working directory.
//
// Per-entry failures do not abort the rest of the plan; they are
// joined into the returned error alongside the partial result.
func ApplyRestorePlan(ctx context.Context, rec VersionRecorder, sessionID, workingDir string, plan RestorePlan) (RestoreResult, error) {
	var res RestoreResult
	var errs []error
	for _, e := range plan.Entries {
		if workingDir == "" || !fsext.HasPrefix(e.Path, workingDir) {
			res.Skipped = append(res.Skipped, e.Path)
			continue
		}

		cur, readErr := os.ReadFile(e.Path)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s: %w", e.Path, readErr))
			continue
		}
		current := string(cur)

		switch e.Op {
		case RestoreOpDelete:
			if !exists {
				res.Unchanged = append(res.Unchanged, e.Path)
				continue
			}
			if _, err := rec.CreateVersion(ctx, sessionID, e.Path, current); err != nil {
				errs = append(errs, fmt.Errorf("%s: recording pre-restore version: %w", e.Path, err))
				continue
			}
			if err := os.Remove(e.Path); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e.Path, err))
				continue
			}
			if _, err := rec.CreateVersion(ctx, sessionID, e.Path, ""); err != nil {
				errs = append(errs, fmt.Errorf("%s: recording restored version: %w", e.Path, err))
			}
			res.Deleted = append(res.Deleted, e.Path)
		default: // RestoreOpWrite
			if exists && current == e.Content {
				res.Unchanged = append(res.Unchanged, e.Path)
				continue
			}
			if _, err := rec.CreateVersion(ctx, sessionID, e.Path, current); err != nil {
				errs = append(errs, fmt.Errorf("%s: recording pre-restore version: %w", e.Path, err))
				continue
			}
			if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e.Path, err))
				continue
			}
			if err := os.WriteFile(e.Path, []byte(e.Content), 0o644); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e.Path, err))
				continue
			}
			if _, err := rec.CreateVersion(ctx, sessionID, e.Path, e.Content); err != nil {
				errs = append(errs, fmt.Errorf("%s: recording restored version: %w", e.Path, err))
			}
			res.Restored = append(res.Restored, e.Path)
		}
	}
	return res, errors.Join(errs...)
}

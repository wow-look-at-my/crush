package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
)

// Expansion limits. They mirror the corresponding agent tool caps so a
// custom command cannot stuff more into the prompt than the equivalent
// tool call would.
const (
	// maxBangOutputRunes caps each !`cmd` substitution (mirrors the bash
	// tool's MaxOutputLength).
	maxBangOutputRunes = 30_000
	// maxFileRefBytes caps each @file inclusion (mirrors the view tool's
	// MaxViewSize).
	maxFileRefBytes = 200 * 1024
	// maxDirRefEntries caps an @directory listing.
	maxDirRefEntries = 100
	// bangTimeout bounds each inline command execution so a hung command
	// cannot stall the invocation forever. The interactive permission
	// prompt is NOT under this timeout; only the execution is.
	bangTimeout = 2 * time.Minute
)

// bangPattern matches inline bash segments of the form !`command`. The
// command must be non-empty and single-line; an unmatched !` stays
// literal.
var bangPattern = regexp.MustCompile("!`([^`\r\n]+)`")

// fileRefPattern matches @path/to/file tokens: an @ at the start of a
// token (preceded by start-of-string, whitespace, or a common opening
// bracket/quote) followed by path characters. Group 1 is the token
// including the @. Mid-word @s (user@host) never match.
var fileRefPattern = regexp.MustCompile(`(?:^|[\s(\[{<"'])(@[A-Za-z0-9_~./\\-]+)`)

// chainingMetacharacters mirrors the bash tool's safe-command guard (see
// internal/agent/tools/safe.go): a command containing any of these can
// smuggle extra commands past a prefix match, so it never qualifies for
// allowed-tools pre-approval and always goes to the interactive ask.
var chainingMetacharacters = []string{";", "|", "&&", "$(", "`"}

// ExpandOptions carries the seams Expand needs. The policy (gate chain,
// caps, ordering) lives here in the commands package; execution and the
// interactive ask are injected so the TUI can wire them to the shell and
// permission service, and tests can fake them.
type ExpandOptions struct {
	// WorkingDir is the project root: @file paths resolve against it and
	// !`cmd` executions run in it.
	WorkingDir string
	// AllowedTools holds the command's frontmatter allowed-tools
	// patterns; a matching Bash(...) pattern pre-approves a !`cmd`.
	AllowedTools []string
	// IsSafeShell optionally reports whether a command is on the safe
	// read-only allowlist (tools.IsSafeReadOnly); such commands run
	// without asking, exactly like the bash tool.
	IsSafeShell func(command string) bool
	// Ask raises an interactive permission request for a !`cmd` that is
	// neither safe-listed nor pre-approved. nil means no interactive flow
	// is available; such commands fail the expansion.
	Ask func(ctx context.Context, command string) (bool, error)
	// RunShell executes an approved command and returns its stdout,
	// stderr, and exit code; err is reserved for failures to run at all
	// (not non-zero exits). nil uses crush's shell interpreter in
	// WorkingDir.
	RunShell func(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error)
}

// NeedsExpansion reports whether content contains !`cmd` or @file syntax.
// Callers keep plain commands on the untouched fast path so commands that
// predate expansion behave byte-identically.
func NeedsExpansion(content string) bool {
	return bangPattern.MatchString(content) || fileRefPattern.MatchString(content)
}

// segment is one !`cmd` or @file token found in the original content.
type segment struct {
	start, end int    // span in the original content
	arg        string // the command, or the path without its @
	bang       bool
}

// Expand expands a custom command body: inline !`cmd` segments execute
// (permission-gated) and their output replaces the segment; @file
// references append the referenced file's content as a delimited block.
//
// Ordering: frontmatter is stripped at load time, $NAMED_ARGS
// substitution happens in the caller before Expand, then ! executes,
// then @ expands. The content is tokenized exactly ONCE, up front, and
// substituted text is never rescanned: a !`cmd` whose output contains
// "@/etc/passwd" cannot smuggle a file into the prompt after approval,
// and an @file whose content contains !`rm -rf ~` cannot execute
// anything. Running ! before @ also means a command may prepare a file
// that a later @ref reads, and a denied command aborts before any file
// I/O happens. An @ token inside a !`cmd` is part of the command text,
// never a file reference.
//
// Any failure — a denied or failing-to-start command, a missing @file —
// aborts the whole invocation with a user-visible error; nothing is
// silently sent.
func Expand(ctx context.Context, content string, opts ExpandOptions) (string, error) {
	segments := scanSegments(content)

	// Execute every !`cmd` in document order, splicing output in place.
	var out strings.Builder
	last := 0
	for _, seg := range segments {
		if !seg.bang {
			continue
		}
		if err := approveBang(ctx, seg.arg, opts); err != nil {
			return "", err
		}
		repl, err := runBang(ctx, seg.arg, opts)
		if err != nil {
			return "", err
		}
		out.WriteString(content[last:seg.start])
		out.WriteString(repl)
		last = seg.end
	}
	out.WriteString(content[last:])

	// Append one delimited context block per distinct @file reference.
	// The token itself stays in the prose so the surrounding sentence
	// still reads naturally and the model can connect the mention to the
	// appended block (mirroring how crush appends hook context to a
	// prompt rather than splicing into it).
	seen := make(map[string]bool)
	var blocks []string
	for _, seg := range segments {
		if seg.bang || seen[seg.arg] {
			continue
		}
		seen[seg.arg] = true
		block, err := renderFileRef(opts.WorkingDir, seg.arg)
		if err != nil {
			return "", err
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return out.String(), nil
	}
	return strings.TrimRight(out.String(), "\n") + "\n\n" + strings.Join(blocks, "\n\n"), nil
}

// scanSegments tokenizes content in a single pass: all !`cmd` spans plus
// all @file tokens that do not fall inside a !`cmd` span, in document
// order.
func scanSegments(content string) []segment {
	var segments []segment
	for _, m := range bangPattern.FindAllStringSubmatchIndex(content, -1) {
		segments = append(segments, segment{
			start: m[0],
			end:   m[1],
			arg:   content[m[2]:m[3]],
			bang:  true,
		})
	}
	for _, m := range fileRefPattern.FindAllStringSubmatchIndex(content, -1) {
		start, end := m[2], m[3]
		inBang := slices.ContainsFunc(segments, func(s segment) bool {
			return start < s.end && end > s.start
		})
		if inBang {
			continue
		}
		// Trim trailing sentence punctuation ("see @foo/bar.go.") from
		// the path; the token in the prose is left untouched either way.
		path := strings.TrimRight(content[start+1:end], ".,")
		if path == "" {
			continue
		}
		segments = append(segments, segment{start: start, end: end, arg: path})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].start < segments[j].start })
	return segments
}

// approveBang runs the permission gate chain for one inline command:
// safe read-only allowlist, then frontmatter allowed-tools, then the
// interactive ask. A deny (or an unavailable ask) is an error so the
// whole invocation aborts.
func approveBang(ctx context.Context, command string, opts ExpandOptions) error {
	if opts.IsSafeShell != nil && opts.IsSafeShell(command) {
		return nil
	}
	if AllowedToolsPermitBash(opts.AllowedTools, command) {
		return nil
	}
	if opts.Ask == nil {
		return fmt.Errorf("!`%s`: no interactive permission flow available; pre-approve the command via the allowed-tools frontmatter", command)
	}
	granted, err := opts.Ask(ctx, command)
	if err != nil {
		return fmt.Errorf("!`%s`: requesting permission: %w", command, err)
	}
	if !granted {
		return fmt.Errorf("!`%s`: permission denied", command)
	}
	return nil
}

// runBang executes one approved inline command and renders its
// substitution: trimmed stdout on success; stdout plus an exit-code note
// carrying stderr on failure, so the model sees exactly what went wrong.
func runBang(ctx context.Context, command string, opts ExpandOptions) (string, error) {
	runShell := opts.RunShell
	if runShell == nil {
		runShell = func(ctx context.Context, command string) (string, string, int, error) {
			return defaultRunShell(ctx, opts.WorkingDir, command)
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, bangTimeout)
	defer cancel()
	stdout, stderr, exitCode, err := runShell(runCtx, command)
	if err != nil {
		return "", fmt.Errorf("!`%s`: %w", command, err)
	}

	repl := strings.TrimSpace(stdout)
	if exitCode != 0 {
		note := fmt.Sprintf("[command exited with code %d]", exitCode)
		if errOut := strings.TrimSpace(stderr); errOut != "" {
			note += "\nstderr: " + errOut
		}
		if repl != "" {
			repl += "\n"
		}
		repl += note
	}
	if truncated, ok := truncateRunes(repl, maxBangOutputRunes); ok {
		repl = truncated + fmt.Sprintf("\n[output truncated to the first %d characters]", maxBangOutputRunes)
	}
	return repl, nil
}

// defaultRunShell executes command through crush's shell interpreter
// (the same stack the bash tool and hooks use) in workingDir. Like
// hooks, no block list applies: the command is user-authored and runs
// with the same trust as a shell alias.
func defaultRunShell(ctx context.Context, workingDir, command string) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	err := shell.Run(ctx, shell.RunOptions{
		Command: command,
		Cwd:     workingDir,
		Env:     os.Environ(),
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", 0, fmt.Errorf("command did not finish: %w", ctxErr)
		}
		exitCode := shell.ExitCode(err)
		if exitCode == 0 && !shell.IsInterrupt(err) {
			// Not an exit status: the command failed to run at all.
			return "", "", 0, err
		}
		return stdout.String(), stderr.String(), exitCode, nil
	}
	return stdout.String(), stderr.String(), 0, nil
}

// renderFileRef renders the appended context block for one @reference: a
// <file> block with the (size-capped) content, or a <directory> block
// with a brief listing. A missing path is an error that aborts the whole
// invocation.
func renderFileRef(workingDir, path string) (string, error) {
	resolved := filepathext.SmartJoin(workingDir, home.Long(path))
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("@%s: %w", path, err)
	}
	if info.IsDir() {
		listing, err := renderDirListing(resolved)
		if err != nil {
			return "", fmt.Errorf("@%s: %w", path, err)
		}
		return fmt.Sprintf("<directory path=%q>\n%s\n</directory>", path, listing), nil
	}
	content, err := readFileCapped(resolved, info.Size())
	if err != nil {
		return "", fmt.Errorf("@%s: %w", path, err)
	}
	return fmt.Sprintf("<file path=%q>\n%s\n</file>", path, content), nil
}

// renderDirListing returns a brief name-per-line listing (directories
// get a trailing separator), capped at maxDirRefEntries.
func renderDirListing(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	var lines []string
	for i, entry := range entries {
		if i == maxDirRefEntries {
			lines = append(lines, fmt.Sprintf("[... %d more entries]", len(entries)-maxDirRefEntries))
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	return strings.Join(lines, "\n"), nil
}

// readFileCapped reads at most maxFileRefBytes of the file, appending an
// explicit truncation note when the file is larger. Binary content is
// replaced with a placeholder rather than splatted into the prompt.
func readFileCapped(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxFileRefBytes))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Sprintf("[binary file: %d bytes]", size), nil
	}
	truncated := size > maxFileRefBytes
	if truncated {
		// Don't leave a split rune at the cut point.
		for range 3 {
			if r, n := utf8.DecodeLastRune(data); r == utf8.RuneError && n == 1 && len(data) > 0 {
				data = data[:len(data)-1]
				continue
			}
			break
		}
	}
	content := strings.TrimRight(string(data), "\n")
	if truncated {
		content += fmt.Sprintf("\n[truncated: showing the first %d bytes of %d]", maxFileRefBytes, size)
	}
	return content, nil
}

// AllowedToolsPermitBash reports whether the frontmatter allowed-tools
// patterns pre-approve command. Supported patterns, Claude Code style:
//
//	Bash                  any command
//	Bash(*)               any command
//	Bash(git status)      exactly "git status"
//	Bash(git add:*)       "git add" plus anything after it
//
// Two documented divergences from Claude Code, both stricter: a :*
// prefix must end at a word boundary (end of command or a space), so
// Bash(git add:*) does not match "git addendum"; and a command
// containing chaining/substitution metacharacters (;, |, &&, $(, `)
// never pre-approves — it falls through to the interactive ask instead
// of being evaluated per chained part. Non-Bash patterns are ignored.
func AllowedToolsPermitBash(allowedTools []string, command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if slices.ContainsFunc(chainingMetacharacters, func(c string) bool { return strings.Contains(command, c) }) {
		return false
	}
	for _, pattern := range allowedTools {
		pattern = strings.TrimSpace(pattern)
		if pattern == "Bash" || pattern == "Bash(*)" {
			return true
		}
		inner, ok := strings.CutPrefix(pattern, "Bash(")
		if !ok {
			continue
		}
		inner, ok = strings.CutSuffix(inner, ")")
		if !ok {
			continue
		}
		inner = strings.TrimSpace(inner)
		if inner == "" {
			continue
		}
		if prefix, ok := strings.CutSuffix(inner, ":*"); ok {
			prefix = strings.TrimSpace(prefix)
			if prefix != "" && (command == prefix || strings.HasPrefix(command, prefix+" ")) {
				return true
			}
			continue
		}
		if command == inner {
			return true
		}
	}
	return false
}

// truncateRunes cuts s to at most max runes; ok reports whether a cut
// happened.
func truncateRunes(s string, max int) (string, bool) {
	if len(s) <= max { // fast path: byte length bounds rune count
		return s, false
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i], true
		}
		count++
	}
	return s, false
}

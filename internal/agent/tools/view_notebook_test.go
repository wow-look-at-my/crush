package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const sampleNotebook = `{
 "cells": [
  {
   "cell_type": "markdown",
   "metadata": {},
   "source": ["# Title\n", "Some *prose*."]
  },
  {
   "cell_type": "code",
   "execution_count": 1,
   "metadata": {},
   "source": ["print(\"hello\")\n", "1 + 2"],
   "outputs": [
    {"output_type": "stream", "name": "stdout", "text": ["hello\n"]},
    {
     "output_type": "execute_result",
     "execution_count": 1,
     "metadata": {},
     "data": {"text/plain": ["3"], "image/png": "iVBORw0KGgo="}
    }
   ]
  },
  {
   "cell_type": "code",
   "execution_count": 2,
   "metadata": {},
   "source": "1 / 0",
   "outputs": [
    {
     "output_type": "error",
     "ename": "ZeroDivisionError",
     "evalue": "division by zero",
     "traceback": ["\u001b[0;31mZeroDivisionError\u001b[0m: division by zero"]
    }
   ]
  },
  {
   "cell_type": "raw",
   "metadata": {},
   "source": "raw text\n"
  },
  {
   "cell_type": "code",
   "execution_count": null,
   "metadata": {},
   "source": "import sys; print(\"warn\", file=sys.stderr)",
   "outputs": [
    {"output_type": "stream", "name": "stderr", "text": "warn\n"},
    {"output_type": "display_data", "metadata": {}, "data": {"text/html": ["<b>hi</b>"]}}
   ]
  }
 ],
 "metadata": {"kernelspec": {"name": "python3"}},
 "nbformat": 4,
 "nbformat_minor": 5
}`

const sampleNotebookRendered = `# Cell 1 [markdown]
# Title
Some *prose*.

# Cell 2 [code]
print("hello")
1 + 2

## Output:
hello
3
[output: image/png]

# Cell 3 [code]
1 / 0

## Output:
[error] ZeroDivisionError: division by zero
ZeroDivisionError: division by zero

# Cell 4 [raw]
raw text

# Cell 5 [code]
import sys; print("warn", file=sys.stderr)

## Output:
[stderr]
warn
[output: text/html]`

func TestRenderNotebook(t *testing.T) {
	t.Parallel()

	rendered, ok := renderNotebook([]byte(sampleNotebook))
	require.True(t, ok)
	require.Equal(t, sampleNotebookRendered, rendered)
}

func TestRenderNotebookNotANotebook(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{"cells": [`},
		{"no cells key", `{"nbformat": 4}`},
		{"null cells", `{"cells": null, "nbformat": 4}`},
		{"legacy v3 worksheets", `{"worksheets": [], "nbformat": 3}`},
		{"top-level array", `[1, 2, 3]`},
		{"plain text", `just some text`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rendered, ok := renderNotebook([]byte(tc.raw))
			require.False(t, ok)
			require.Empty(t, rendered)
		})
	}

	t.Run("empty cells renders placeholder", func(t *testing.T) {
		t.Parallel()
		rendered, ok := renderNotebook([]byte(`{"cells": [], "nbformat": 4}`))
		require.True(t, ok)
		require.Equal(t, "(notebook has no cells)", rendered)
	})
}

func TestNotebookOutputRender(t *testing.T) {
	t.Parallel()

	longText := strings.Repeat("x", notebookOutputCap+100)
	cases := []struct {
		name   string
		output notebookOutput
		want   string
	}{
		{
			name:   "stream stdout",
			output: notebookOutput{OutputType: "stream", Name: "stdout", Text: "hello\n"},
			want:   "hello",
		},
		{
			name:   "stream stderr labeled",
			output: notebookOutput{OutputType: "stream", Name: "stderr", Text: "warn\n"},
			want:   "[stderr]\nwarn",
		},
		{
			name:   "stream empty",
			output: notebookOutput{OutputType: "stream", Name: "stdout", Text: ""},
			want:   "",
		},
		{
			name:   "long stream truncated",
			output: notebookOutput{OutputType: "stream", Name: "stdout", Text: notebookText(longText)},
			want:   strings.Repeat("x", notebookOutputCap) + "\n[... output truncated]",
		},
		{
			name: "rich mimes sorted placeholders",
			output: notebookOutput{OutputType: "display_data", Data: map[string]json.RawMessage{
				"image/png":        json.RawMessage(`"iVBOR"`),
				"application/json": json.RawMessage(`{"a": 1}`),
			}},
			want: "[output: application/json]\n[output: image/png]",
		},
		{
			name: "execute result text plain before placeholders",
			output: notebookOutput{OutputType: "execute_result", Data: map[string]json.RawMessage{
				"text/plain": json.RawMessage(`["3"]`),
				"image/png":  json.RawMessage(`"iVBOR"`),
			}},
			want: "3\n[output: image/png]",
		},
		{
			name:   "error without details",
			output: notebookOutput{OutputType: "error"},
			want:   "[error]",
		},
		{
			name:   "error evalue only",
			output: notebookOutput{OutputType: "error", EValue: "boom"},
			want:   "[error] boom",
		},
		{
			name: "error strips ansi from traceback",
			output: notebookOutput{
				OutputType: "error",
				EName:      "ValueError",
				EValue:     "bad",
				Traceback:  []string{"\x1b[0;31mValueError\x1b[0m: bad", "  frame"},
			},
			want: "[error] ValueError: bad\nValueError: bad\n  frame",
		},
		{
			name:   "unknown output type",
			output: notebookOutput{OutputType: "widget_state"},
			want:   "[output: widget_state]",
		},
		{
			name:   "missing output type",
			output: notebookOutput{},
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, tc.output.render())
		})
	}
}

func TestWindowLines(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\nline 4\nline 5"
	cases := []struct {
		name        string
		content     string
		offset      int
		limit       int
		maxSize     int
		wantContent string
		wantHasMore bool
	}{
		{
			name:        "all lines",
			content:     content,
			offset:      0,
			limit:       5,
			wantContent: content,
		},
		{
			name:        "window with more lines",
			content:     content,
			offset:      1,
			limit:       2,
			wantContent: "line 2\nline 3",
			wantHasMore: true,
		},
		{
			name:        "offset beyond end",
			content:     content,
			offset:      10,
			limit:       2,
			wantContent: "",
		},
		{
			name:        "negative offset treated as zero",
			content:     content,
			offset:      -3,
			limit:       1,
			wantContent: "line 1",
			wantHasMore: true,
		},
		{
			name:        "long line truncated",
			content:     strings.Repeat("a", MaxLineLength+10),
			offset:      0,
			limit:       1,
			wantContent: strings.Repeat("a", MaxLineLength) + "...",
		},
		{
			name:        "exact max size allowed",
			content:     "abcd\nefgh",
			offset:      0,
			limit:       2,
			maxSize:     len("abcd\nefgh"),
			wantContent: "abcd\nefgh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, hasMore, err := windowLines(tc.content, tc.offset, tc.limit, tc.maxSize)
			require.NoError(t, err)
			require.Equal(t, tc.wantContent, got)
			require.Equal(t, tc.wantHasMore, hasMore)
		})
	}

	t.Run("window over max size errors", func(t *testing.T) {
		t.Parallel()
		_, _, err := windowLines("abcd\nefgh", 0, 2, 5)
		require.ErrorAs(t, err, &contentTooLargeError{})
	})
}

func TestViewToolRendersNotebook(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "sample.ipynb")
	require.NoError(t, os.WriteFile(filePath, []byte(sampleNotebook), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "<notebook>")
	require.Contains(t, resp.Content, "     1|# Cell 1 [markdown]")
	require.Contains(t, resp.Content, "[output: image/png]")
	require.NotContains(t, resp.Content, "iVBORw0KGgo=", "base64 payloads must be collapsed")
	require.NotContains(t, resp.Content, "\x1b[0;31m", "ANSI escapes must be stripped")
	require.NotContains(t, resp.Content, "(Notebook has more lines")

	var meta ViewResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, sampleNotebookRendered, meta.Content)
}

func TestViewToolNotebookOffsetLimit(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "sample.ipynb")
	require.NoError(t, os.WriteFile(filePath, []byte(sampleNotebook), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{
		FilePath: filePath,
		Offset:   4,
		Limit:    3,
	})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "     5|# Cell 2 [code]")
	require.Contains(t, resp.Content, `     6|print("hello")`)
	require.Contains(t, resp.Content, "     7|1 + 2")
	require.Contains(t, resp.Content, "(Notebook has more lines. Use 'offset' parameter to read beyond line 7)")
	require.NotContains(t, resp.Content, "# Cell 1 [markdown]")

	var meta ViewResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "# Cell 2 [code]\nprint(\"hello\")\n1 + 2", meta.Content)
}

func TestViewToolMalformedNotebookFallsBack(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{"cells": [`},
		{"legacy v3 worksheets", `{"worksheets": [], "nbformat": 3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			workingDir := t.TempDir()
			filePath := filepath.Join(workingDir, "broken.ipynb")
			require.NoError(t, os.WriteFile(filePath, []byte(tc.raw), 0o644))

			tool := newViewToolForTest(workingDir)
			ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
			resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})

			require.False(t, resp.IsError)
			require.Contains(t, resp.Content, "<file>")
			require.NotContains(t, resp.Content, "<notebook>")
			require.Contains(t, resp.Content, "     1|"+tc.raw)
		})
	}
}

func TestViewToolNotebookTooLarge(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "huge.ipynb")
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Repeat("a", maxNotebookSize+1)), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Notebook file is too large")
}

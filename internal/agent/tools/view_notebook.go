package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
)

const (
	// notebookExt is the file extension that triggers notebook rendering
	// in the view tool.
	notebookExt = ".ipynb"
	// maxNotebookSize caps the raw notebook JSON read into memory.
	// Notebooks embed base64 outputs, so the raw file is usually much
	// larger than its rendered view.
	maxNotebookSize = 20 * 1024 * 1024 // 20MB
	// notebookOutputCap bounds the text kept per cell output (stream
	// text, text/plain results, and error tracebacks).
	notebookOutputCap = 2000
)

// viewNotebook renders a Jupyter notebook file for the view tool. The
// handled result reports whether the response is final: when the file does
// not parse as an nbformat 4 notebook, handled is false and the caller
// should fall back to the plain-text view of the raw file.
//
// Offset and limit window the rendered form (numbered cells), not the raw
// JSON lines.
func viewNotebook(filePath string, size int64, params ViewParams) (resp fantasy.ToolResponse, handled bool, err error) {
	if size > maxNotebookSize {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Notebook file is too large (%d bytes). Maximum size is %d bytes",
			size, maxNotebookSize)), true, nil
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return fantasy.ToolResponse{}, false, fmt.Errorf("error reading notebook file: %w", err)
	}
	rendered, ok := renderNotebook(raw)
	if !ok {
		return fantasy.ToolResponse{}, false, nil
	}
	content, hasMore, err := windowLines(rendered, params.Offset, params.Limit, MaxViewSize)
	if err != nil {
		var tooLarge contentTooLargeError
		if errors.As(err, &tooLarge) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("Content section is too large (%d bytes). Maximum size is %d bytes",
				tooLarge.Size, tooLarge.Max)), true, nil
		}
		return fantasy.ToolResponse{}, false, err
	}

	output := "<notebook>\n"
	output += addLineNumbers(content, params.Offset+1)
	if hasMore {
		output += fmt.Sprintf("\n\n(Notebook has more lines. Use 'offset' parameter to read beyond line %d)",
			params.Offset+len(strings.Split(content, "\n")))
	}
	output += "\n</notebook>\n"

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output),
		ViewResponseMetadata{
			FilePath: filePath,
			Content:  content,
		},
	), true, nil
}

// notebookText is a JSON field that Jupyter encodes either as a single
// string or as a list of line strings with embedded newlines.
type notebookText string

func (t *notebookText) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*t = notebookText(s)
		return nil
	}
	var lines []string
	if err := json.Unmarshal(data, &lines); err != nil {
		return err
	}
	*t = notebookText(strings.Join(lines, ""))
	return nil
}

type notebookCell struct {
	CellType string           `json:"cell_type"`
	Source   notebookText     `json:"source"`
	Outputs  []notebookOutput `json:"outputs"`
}

type notebookOutput struct {
	OutputType string                     `json:"output_type"`
	Name       string                     `json:"name"`
	Text       notebookText               `json:"text"`
	Data       map[string]json.RawMessage `json:"data"`
	EName      string                     `json:"ename"`
	EValue     string                     `json:"evalue"`
	Traceback  []string                   `json:"traceback"`
}

// renderNotebook converts raw Jupyter notebook JSON (nbformat 4) into a
// readable plain-text form: numbered cells with type markers, the source
// verbatim, and per-cell outputs summarized (stream text and text/plain
// results capped, rich MIME payloads collapsed to one-line placeholders,
// errors as ename/evalue plus a short traceback). ok is false when the
// bytes are not an nbformat 4 notebook (no top-level "cells" array, e.g.
// the legacy nbformat 3 "worksheets" layout or not a notebook at all), in
// which case callers should fall back to a plain-text view.
func renderNotebook(raw []byte) (rendered string, ok bool) {
	var doc struct {
		Cells []notebookCell `json:"cells"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", false
	}
	if doc.Cells == nil {
		return "", false
	}
	if len(doc.Cells) == 0 {
		return "(notebook has no cells)", true
	}
	blocks := make([]string, 0, len(doc.Cells))
	for i, cell := range doc.Cells {
		blocks = append(blocks, cell.render(i+1))
	}
	return strings.Join(blocks, "\n\n"), true
}

func (c notebookCell) render(num int) string {
	cellType := c.CellType
	if cellType == "" {
		cellType = "unknown"
	}
	block := fmt.Sprintf("# Cell %d [%s]", num, cellType)
	if source := strings.TrimRight(string(c.Source), "\n"); source != "" {
		block += "\n" + source
	}
	if outputs := renderNotebookOutputs(c.Outputs); outputs != "" {
		block += "\n\n## Output:\n" + outputs
	}
	return block
}

func renderNotebookOutputs(outputs []notebookOutput) string {
	parts := make([]string, 0, len(outputs))
	for _, out := range outputs {
		if s := out.render(); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// render summarizes one cell output. Text is capped so a chatty stream or
// a giant repr cannot dominate the view; rich MIME payloads collapse to a
// one-line placeholder instead of pages of base64.
func (o notebookOutput) render() string {
	switch o.OutputType {
	case "stream":
		text := capNotebookText(strings.TrimRight(string(o.Text), "\n"))
		if o.Name == "stderr" && text != "" {
			return "[stderr]\n" + text
		}
		return text
	case "execute_result", "display_data":
		parts := make([]string, 0, len(o.Data))
		if raw, found := o.Data["text/plain"]; found {
			var text notebookText
			if err := json.Unmarshal(raw, &text); err == nil {
				if s := strings.TrimRight(string(text), "\n"); s != "" {
					parts = append(parts, capNotebookText(s))
				}
			}
		}
		for _, mime := range slices.Sorted(maps.Keys(o.Data)) {
			if mime == "text/plain" {
				continue
			}
			parts = append(parts, "[output: "+mime+"]")
		}
		return strings.Join(parts, "\n")
	case "error":
		head := "[error]"
		switch {
		case o.EName != "" && o.EValue != "":
			head += " " + o.EName + ": " + o.EValue
		case o.EName != "":
			head += " " + o.EName
		case o.EValue != "":
			head += " " + o.EValue
		}
		traceback := strings.TrimRight(ansi.Strip(strings.Join(o.Traceback, "\n")), "\n")
		if traceback == "" {
			return head
		}
		return head + "\n" + capNotebookText(traceback)
	case "":
		return ""
	default:
		return "[output: " + o.OutputType + "]"
	}
}

// capNotebookText bounds a single output's text at notebookOutputCap
// bytes, cutting at a rune boundary.
func capNotebookText(s string) string {
	if len(s) <= notebookOutputCap {
		return s
	}
	return strings.ToValidUTF8(s[:notebookOutputCap], "") + "\n[... output truncated]"
}

// windowLines applies the view tool's offset/limit/size semantics to
// already-rendered content, mirroring readTextFile over in-memory lines:
// long lines are truncated at MaxLineLength and the selected window must
// fit in maxContentSize (0 disables the size check).
func windowLines(content string, offset, limit, maxContentSize int) (string, bool, error) {
	lines := strings.Split(content, "\n")
	offset = max(offset, 0)
	if offset >= len(lines) {
		return "", false, nil
	}
	lines = lines[offset:]
	hasMore := len(lines) > limit
	if hasMore {
		lines = lines[:limit]
	}
	contentSize := 0
	windowed := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) > MaxLineLength {
			line = strings.ToValidUTF8(line[:MaxLineLength], "") + "..."
		}
		projectedSize := contentSize + len(line)
		if len(windowed) > 0 {
			projectedSize++
		}
		if maxContentSize > 0 && projectedSize > maxContentSize {
			return "", false, contentTooLargeError{Size: projectedSize, Max: maxContentSize}
		}
		contentSize = projectedSize
		windowed = append(windowed, line)
	}
	return strings.Join(windowed, "\n"), hasMore, nil
}

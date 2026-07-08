package hooks

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/charmbracelet/crush/internal/shell"
	"github.com/tidwall/gjson"
)

// SupportedOutputVersion is the highest envelope version this build
// understands. Hooks may omit `version` entirely (treated as 1) or pin
// an older version. Unknown higher versions are still parsed but logged.
const SupportedOutputVersion = 1

// Payload is the JSON structure piped to hook commands via stdin. The
// common fields are present for every event; the per-event fields are
// omitted when they don't apply. ToolInput and ToolResponse are emitted
// as parsed JSON objects for compatibility with Claude Code hooks (which
// expect objects, not strings), and HookEventName duplicates Event under
// Claude Code's field name so its hook scripts port over unchanged.
type Payload struct {
	Event          string          `json:"event"`
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	StopHookActive *bool           `json:"stop_hook_active,omitempty"`
}

// BuildPayload constructs the JSON stdin payload for a hook command.
func BuildPayload(ev Event, cwd string) []byte {
	p := Payload{
		Event:         ev.Name,
		HookEventName: ev.Name,
		SessionID:     ev.SessionID,
		CWD:           cwd,
		ToolName:      ev.ToolName,
		Prompt:        ev.Prompt,
	}
	if ev.usesMatcher() {
		// Tool events always carry tool_input, defaulting to an empty
		// object when the model produced invalid JSON.
		p.ToolInput = validJSON(ev.ToolInput)
	}
	if ev.Name == EventPostToolUse {
		p.ToolResponse = validJSON(ev.ToolResponse)
	}
	if ev.Name == EventStop || ev.Name == EventSubagentStop {
		// Emitted even when false: Claude Code stop hooks branch on this
		// field, so omitting the zero value would break them.
		active := ev.StopHookActive
		p.StopHookActive = &active
	}
	data, err := json.Marshal(p)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// validJSON returns raw as a JSON value, falling back to an empty object
// when raw is not valid JSON.
func validJSON(raw string) json.RawMessage {
	msg := json.RawMessage(raw)
	if !json.Valid(msg) {
		return json.RawMessage("{}")
	}
	return msg
}

// BuildEnv constructs the environment variable slice for a hook command.
// It includes all current process env vars plus hook-specific ones.
func BuildEnv(ev Event, cwd, projectDir string) []string {
	env := os.Environ()
	env = append(env, shell.CrushEnvMarkers()...)
	env = append(
		env,
		fmt.Sprintf("CRUSH_EVENT=%s", ev.Name),
		fmt.Sprintf("CRUSH_TOOL_NAME=%s", ev.ToolName),
		fmt.Sprintf("CRUSH_SESSION_ID=%s", ev.SessionID),
		fmt.Sprintf("CRUSH_CWD=%s", cwd),
		fmt.Sprintf("CRUSH_PROJECT_DIR=%s", projectDir),
	)

	// Extract tool-specific env vars from the JSON input.
	if ev.ToolInput != "" {
		if cmd := gjson.Get(ev.ToolInput, "command"); cmd.Exists() {
			env = append(env, fmt.Sprintf("CRUSH_TOOL_INPUT_COMMAND=%s", cmd.String()))
		}
		if fp := gjson.Get(ev.ToolInput, "file_path"); fp.Exists() {
			env = append(env, fmt.Sprintf("CRUSH_TOOL_INPUT_FILE_PATH=%s", fp.String()))
		}
	}

	return env
}

// parseStdoutForEvent parses hook stdout with per-event handling layered
// on top of parseStdout. For UserPromptSubmit, non-JSON stdout is treated
// as additional context (Claude Code compatibility: plain stdout from a
// prompt hook is injected into the prompt); every other event requires
// the JSON envelope.
func parseStdoutForEvent(event, stdout string) HookResult {
	if event != EventUserPromptSubmit {
		return parseStdout(stdout)
	}
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return HookResult{Decision: DecisionNone}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return HookResult{Decision: DecisionNone, Context: trimmed}
	}
	return parseStdout(stdout)
}

// parseStdout parses the JSON output from a hook command's stdout.
// Supports both Crush format and Claude Code format (hookSpecificOutput).
func parseStdout(stdout string) HookResult {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return HookResult{Decision: DecisionNone}
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return HookResult{Decision: DecisionNone}
	}

	// Claude Code compat: if hookSpecificOutput is present, parse that.
	if hso, ok := raw["hookSpecificOutput"]; ok {
		return parseClaudeCodeOutput(hso)
	}

	var parsed struct {
		Version      int             `json:"version"`
		Decision     string          `json:"decision"`
		Halt         bool            `json:"halt"`
		Reason       string          `json:"reason"`
		Context      json.RawMessage `json:"context"`
		UpdatedInput json.RawMessage `json:"updated_input"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return HookResult{Decision: DecisionNone}
	}

	if parsed.Version > SupportedOutputVersion {
		slog.Debug(
			"Hook output declared a newer envelope version than this build supports",
			"version", parsed.Version,
			"supported", SupportedOutputVersion,
		)
	}

	result := HookResult{
		Halt:    parsed.Halt,
		Reason:  parsed.Reason,
		Context: parseContext(parsed.Context),
	}
	result.Decision = parseDecision(parsed.Decision)
	result.UpdatedInput = rawToString(parsed.UpdatedInput)
	return result
}

// parseContext accepts either a single string or an array of strings and
// returns a newline-joined value with empty entries dropped.
func parseContext(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// String form.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	// Array form.
	if raw[0] == '[' {
		var items []string
		if err := json.Unmarshal(raw, &items); err != nil {
			return ""
		}
		out := items[:0]
		for _, s := range items {
			if s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// parseClaudeCodeOutput handles the Claude Code hook output format:
// {"hookSpecificOutput": {"permissionDecision": "allow", ...}}
func parseClaudeCodeOutput(data json.RawMessage) HookResult {
	var hso struct {
		PermissionDecision       string          `json:"permissionDecision"`
		PermissionDecisionReason string          `json:"permissionDecisionReason"`
		UpdatedInput             json.RawMessage `json:"updatedInput"`
		AdditionalContext        string          `json:"additionalContext"`
	}
	if err := json.Unmarshal(data, &hso); err != nil {
		return HookResult{Decision: DecisionNone}
	}

	result := HookResult{
		Decision: parseDecision(hso.PermissionDecision),
		Reason:   hso.PermissionDecisionReason,
		Context:  hso.AdditionalContext,
	}

	// Marshal updatedInput back to a string for our opaque format.
	if len(hso.UpdatedInput) > 0 && string(hso.UpdatedInput) != "null" {
		result.UpdatedInput = string(hso.UpdatedInput)
	}

	return result
}

// rawToString converts a json.RawMessage to a string suitable for use
// as opaque tool input. It accepts both a JSON object (nested) and a
// JSON string (stringified, for backward compatibility).
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// If it's a JSON string, unwrap it.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	// Otherwise it's an object/array — use as-is.
	return string(raw)
}

// parseDecision maps a decision string to a Decision. "approve" and
// "block" are Claude Code's vocabulary (its Stop, PostToolUse, and
// UserPromptSubmit hooks emit {"decision":"block"}) and are accepted as
// aliases so those scripts run under Crush unchanged.
func parseDecision(s string) Decision {
	switch strings.ToLower(s) {
	case "allow", "approve":
		return DecisionAllow
	case "deny", "block":
		return DecisionDeny
	default:
		return DecisionNone
	}
}

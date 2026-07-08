// Package hooks runs user-defined shell commands that fire on hook events
// (e.g. PreToolUse), returning decisions that control agent behavior.
package hooks

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/tidwall/sjson"
)

// Hook event name constants.
const (
	// EventPreToolUse fires before a tool call executes. Matchers are
	// tested against the tool name. Deny blocks the call; allow
	// pre-approves it past the permission prompt.
	EventPreToolUse = "PreToolUse"
	// EventPostToolUse fires after a tool call completes. Matchers are
	// tested against the tool name. The tool already ran, so deny only
	// feeds the hook's reason back to the model.
	EventPostToolUse = "PostToolUse"
	// EventUserPromptSubmit fires when a user prompt is submitted,
	// before the turn starts. Deny aborts the turn; context is appended
	// to the prompt.
	EventUserPromptSubmit = "UserPromptSubmit"
	// EventStop fires when the top-level agent finishes a turn normally
	// and is about to go idle. Deny feeds the hook's reason back as a
	// follow-up prompt so the agent continues.
	EventStop = "Stop"
	// EventSubagentStop fires when a sub-agent run completes normally.
	// Deny feeds the hook's reason back into the sub-agent once.
	EventSubagentStop = "SubagentStop"
)

// Event describes a single hook firing: the event name plus its
// event-specific payload fields. Only Name and SessionID are universal;
// the rest are serialized to hooks only when relevant to the event.
//
// Adding a new event is intentionally cheap: add a constant above, add
// any new payload field here and in Payload (input.go), and fire
// Runner.Run with a populated Event at the call site.
type Event struct {
	Name      string
	SessionID string

	// ToolName and ToolInput apply to the tool events (PreToolUse,
	// PostToolUse). ToolName is also the matcher target.
	ToolName  string
	ToolInput string // raw JSON

	// ToolResponse applies to PostToolUse: the executed tool's result.
	ToolResponse string // raw JSON

	// Prompt applies to UserPromptSubmit.
	Prompt string

	// StopHookActive applies to Stop and SubagentStop: true when the
	// finishing turn was itself started by a Stop-hook continuation, so
	// hooks can avoid keeping the agent running forever.
	StopHookActive bool
}

// usesMatcher reports whether hook matchers apply to this event.
// Matchers are regexes over tool names, so only the tool events use
// them; hooks on other events fire regardless of their matcher.
func (e Event) usesMatcher() bool {
	return e.Name == EventPreToolUse || e.Name == EventPostToolUse
}

// HaltExitCode is the exit code that halts the whole turn. 2 blocks the
// current tool call; 49 sits in the no-man's-land between the
// generic-error range (1-30), the sysexits range (64-78), and the
// killed-by-signal range (128+) so it can't be hit by accident.
const HaltExitCode = 49

// HookMetadata is embedded in tool response metadata so the UI can
// display a hook indicator.
type HookMetadata struct {
	HookCount    int        `json:"hook_count"`
	Decision     string     `json:"decision"`
	Halt         bool       `json:"halt,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	InputRewrite bool       `json:"input_rewrite,omitempty"`
	Hooks        []HookInfo `json:"hooks,omitempty"`
}

// HookInfo identifies a single hook that ran and its individual result.
type HookInfo struct {
	Name         string `json:"name"`
	Matcher      string `json:"matcher,omitempty"`
	Decision     string `json:"decision"`
	Halt         bool   `json:"halt,omitempty"`
	Reason       string `json:"reason,omitempty"`
	InputRewrite bool   `json:"input_rewrite,omitempty"`
}

// Decision represents the outcome of a single hook execution.
type Decision int

const (
	// DecisionNone means the hook expressed no opinion.
	DecisionNone Decision = iota
	// DecisionAllow means the hook explicitly allowed the action.
	DecisionAllow
	// DecisionDeny means the hook blocked the action.
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	default:
		return "none"
	}
}

// HookResult holds the parsed output of a single hook execution.
type HookResult struct {
	Decision     Decision
	Halt         bool   // If true, halt the whole turn.
	Reason       string // Deny or halt reason (same field, different audience).
	Context      string
	UpdatedInput string // Shallow-merge patch against tool_input (opaque JSON).
}

// AggregateResult holds the combined outcome of all hooks for an event.
type AggregateResult struct {
	Decision     Decision
	Halt         bool       // Any hook requested halt.
	HookCount    int        // Number of hooks that ran.
	Hooks        []HookInfo // Info about each hook that ran (config order).
	Reason       string     // Concatenated deny/halt reasons (newline-separated).
	Context      string     // Concatenated context from all hooks.
	UpdatedInput string     // Merged tool_input JSON (empty if no patches).
}

// aggregate merges multiple HookResults into a single AggregateResult.
// Results are processed in config order (the order of the slice). Deny
// wins over allow, allow wins over none. Halt is sticky. Reasons and
// context concatenate in order. updated_input patches shallow-merge in
// order against the original tool input; later patches override earlier
// ones on colliding keys.
func aggregate(results []HookResult, origToolInput string) AggregateResult {
	var (
		decision Decision
		halt     bool
		reasons  []string
		contexts []string
		merged   = origToolInput
		anyPatch = false
	)
	for _, r := range results {
		switch r.Decision {
		case DecisionDeny:
			decision = DecisionDeny
			if r.Reason != "" {
				reasons = append(reasons, r.Reason)
			}
		case DecisionAllow:
			if decision != DecisionDeny {
				decision = DecisionAllow
			}
		case DecisionNone:
			// No change.
		}
		if r.Halt {
			halt = true
			if r.Reason != "" && r.Decision != DecisionDeny {
				// A halting hook that didn't also deny still contributes
				// its reason so the user sees it.
				reasons = append(reasons, r.Reason)
			}
		}
		if r.Context != "" {
			contexts = append(contexts, r.Context)
		}
		if r.UpdatedInput != "" {
			next, err := shallowMerge(merged, r.UpdatedInput)
			if err != nil {
				slog.Warn(
					"Hook updated_input patch rejected; ignoring",
					"error", err,
					"patch", r.UpdatedInput,
				)
				continue
			}
			merged = next
			anyPatch = true
		}
	}

	agg := AggregateResult{
		Decision:  decision,
		Halt:      halt,
		HookCount: len(results),
	}
	if anyPatch {
		agg.UpdatedInput = merged
	}
	if len(reasons) > 0 {
		agg.Reason = strings.Join(reasons, "\n")
	}
	if len(contexts) > 0 {
		agg.Context = strings.Join(contexts, "\n")
	}
	return agg
}

// shallowMerge applies a top-level-keys patch to base (both JSON
// objects). Keys in patch overwrite keys in base; keys absent from the
// patch are preserved. Returns an error if either value is not a valid
// JSON object.
func shallowMerge(base, patch string) (string, error) {
	if base == "" {
		base = "{}"
	}
	// Ensure base is an object so sjson has somewhere to write.
	var baseAny any
	if err := json.Unmarshal([]byte(base), &baseAny); err != nil {
		return "", err
	}
	if _, ok := baseAny.(map[string]any); !ok {
		return "", errNotObject("tool_input")
	}
	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(patch), &patchMap); err != nil {
		return "", errNotObject("updated_input")
	}
	out := base
	for k, v := range patchMap {
		next, err := sjson.SetRawBytes([]byte(out), k, v)
		if err != nil {
			return "", err
		}
		out = string(next)
	}
	return out, nil
}

type errNotObject string

func (e errNotObject) Error() string { return string(e) + " is not a JSON object" }

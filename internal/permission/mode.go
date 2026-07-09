package permission

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/fsext"
)

// Mode is a runtime-switchable permission mode that shapes how tool
// permission requests are resolved and which tools the top-level agent
// may use. It is orthogonal to the skip-everything --yolo switch
// ([Service.SetSkipRequests]), which always wins.
//
// Adding a new mode is a constant plus an entry in [modePolicies]; the
// permission service, the coordinator's tool filtering, and the TUI all
// consult the policy table rather than switching on mode names.
type Mode string

const (
	// ModeDefault is the normal interactive flow: every gated tool call
	// prompts unless allowed_tools or a session grant covers it.
	ModeDefault Mode = "default"
	// ModeAcceptEdits auto-approves file-editing tool calls (edit,
	// multiedit, write) for paths inside the working directory.
	// Everything else keeps the normal ask flow.
	ModeAcceptEdits Mode = "accept_edits"
	// ModePlan is a read-only planning mode: mutating permission
	// requests are denied and the coordinator restricts the agent's
	// tool list to the read-only set plus plan_exit.
	ModePlan Mode = "plan"
)

// policyDecision is the outcome of a mode policy for one permission
// request.
type policyDecision int

const (
	// policyAsk falls through to the normal flow (allowlist, hook
	// approval, session grants, interactive prompt).
	policyAsk policyDecision = iota
	// policyAllow grants the request without prompting.
	policyAllow
	// policyDeny rejects the request without prompting.
	policyDeny
)

// modePolicy bundles everything a permission mode changes. All of a
// mode's behavior lives here so a new mode never needs scattered ifs.
type modePolicy struct {
	// decide auto-resolves a permission request; nil (or a policyAsk
	// result) falls through to the normal interactive flow.
	decide func(req CreatePermissionRequest, workingDir string) policyDecision
	// allowTool reports whether the top-level agent should advertise
	// the named tool while the mode is active. nil allows every tool.
	// Consulted by the coordinator when it (re)builds the tool list,
	// so a change takes effect on the next run.
	allowTool func(toolName string) bool
	// promptAddition is appended to the agent's system prompt for runs
	// started while the mode is active. Empty means no addition.
	promptAddition string
}

// modePolicies is the mode policy table. Adding a mode = a Mode
// constant + an entry here.
var modePolicies = map[Mode]modePolicy{
	ModeDefault: {},
	ModeAcceptEdits: {
		decide: acceptEditsDecide,
	},
	ModePlan: {
		decide:         planDecide,
		allowTool:      planAllowsTool,
		promptAddition: planPromptAddition,
	},
}

// Modes returns every permission mode in the order the TUI cycles
// through them.
func Modes() []Mode {
	return []Mode{ModeDefault, ModeAcceptEdits, ModePlan}
}

// ParseMode normalizes and validates a user-supplied mode string (from
// config or the --permission-mode flag). The empty string maps to
// ModeDefault; hyphens are accepted in place of underscores.
func ParseMode(s string) (Mode, error) {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
	if normalized == "" {
		return ModeDefault, nil
	}
	mode := Mode(normalized)
	if !mode.Valid() {
		return ModeDefault, fmt.Errorf("invalid permission mode %q (valid modes: default, accept_edits, plan)", s)
	}
	return mode, nil
}

// Valid reports whether the mode exists in the policy table.
func (m Mode) Valid() bool {
	_, ok := modePolicies[m]
	return ok
}

// policy returns the mode's policy entry, falling back to the default
// policy for unknown modes so a bad value can never widen permissions.
func (m Mode) policy() modePolicy {
	if p, ok := modePolicies[m]; ok {
		return p
	}
	return modePolicies[ModeDefault]
}

// AllowsTool reports whether the top-level agent may advertise the
// named tool while this mode is active.
func (m Mode) AllowsTool(toolName string) bool {
	p := m.policy()
	if p.allowTool == nil {
		return true
	}
	return p.allowTool(toolName)
}

// PromptAddition returns the system-prompt addition for this mode, or
// "" when the mode adds nothing.
func (m Mode) PromptAddition() string {
	return m.policy().promptAddition
}

// decide auto-resolves a permission request under this mode. policyAsk
// falls through to the normal flow.
func (m Mode) decide(req CreatePermissionRequest, workingDir string) policyDecision {
	p := m.policy()
	if p.decide == nil {
		return policyAsk
	}
	return p.decide(req, workingDir)
}

// The tool names and actions below mirror the constants in
// internal/agent/tools, which imports this package (so the constants
// cannot be referenced here without an import cycle).
// TestPermissionModeToolNames in internal/agent/tools guards the sync.

// planExitToolName is the tool available only in plan mode whose
// approval exits it. Its permission request must fall through to the
// interactive prompt — the prompt IS the exit-plan-mode approval flow.
const planExitToolName = "plan_exit"

// fileEditTools are the tools whose "write" permission requests
// accept_edits auto-approves inside the working directory.
var fileEditTools = map[string]bool{
	"edit":      true,
	"multiedit": true,
	"write":     true,
}

// readOnlyActions are the permission actions that never mutate the
// user's system. In plan mode they keep the normal ask flow (view/ls
// outside the working directory, fetch); anything else is denied.
var readOnlyActions = map[string]bool{
	"read":  true,
	"list":  true,
	"fetch": true,
}

// planModeTools is the set of tools the top-level agent may use in plan
// mode: the read-only investigation tools plus todos (session-internal
// planning state) and plan_exit. Deliberately excluded: edit,
// multiedit, write, download (all mutate files), bash (its
// safe-command pre-approval bypasses the permission service and
// includes process-mutating commands like kill/killall, so it cannot
// be cleanly restricted to read-only), job_kill and lsp_restart
// (process side effects), and MCP tools (no way to know which are
// read-only).
var planModeTools = map[string]bool{
	"agent":              true,
	"agentic_fetch":      true,
	"crush_info":         true,
	"crush_logs":         true,
	"fetch":              true,
	"glob":               true,
	"grep":               true,
	"job_output":         true,
	"list_mcp_resources": true,
	"ls":                 true,
	"lsp_diagnostics":    true,
	"lsp_references":     true,
	planExitToolName:     true,
	"read_mcp_resource":  true,
	"sourcegraph":        true,
	"todos":              true,
	"view":               true,
	"web_search":         true,
}

// acceptEditsDecide auto-approves file-editing tool requests scoped to
// the working directory. The edit tools report a path inside the
// working directory as the working directory itself (fsext.PathOrPrefix),
// so a prefix check covers both shapes. Anything else — other tools,
// non-write actions, paths outside the working directory — keeps the
// normal ask flow.
func acceptEditsDecide(req CreatePermissionRequest, workingDir string) policyDecision {
	if !fileEditTools[req.ToolName] || req.Action != "write" {
		return policyAsk
	}
	if !fsext.HasPrefix(req.Path, workingDir) {
		return policyAsk
	}
	return policyAllow
}

// planAllowsTool is plan mode's tool-advertisement filter: membership
// in planModeTools. MCP tool names are never in the set, so MCP tools
// are dropped along with the mutating built-ins.
func planAllowsTool(toolName string) bool {
	return planModeTools[toolName]
}

// planDecide denies every request that is not known to be read-only.
// It is the defense-in-depth backstop behind the plan-mode tool filter:
// tool filtering is the primary guard, but a tool that slips through
// (an MCP tool, a future tool, a mid-run mode switch) is stopped here.
// plan_exit falls through to the interactive prompt — approving that
// prompt is how the user exits plan mode.
func planDecide(req CreatePermissionRequest, _ string) policyDecision {
	if req.ToolName == planExitToolName {
		return policyAsk
	}
	if readOnlyActions[req.Action] {
		return policyAsk
	}
	return policyDeny
}

// planPromptAddition tells the model it is in read-only planning mode.
// Injected as a system-prompt suffix for runs started in plan mode.
const planPromptAddition = `## Plan mode

You are in PLAN MODE: a read-only mode for researching the codebase and designing an implementation plan. You MUST NOT make any changes: no file edits or writes, no commands, downloads, or any other mutating action. Mutating tools are unavailable and mutating permission requests are denied while this mode is active.

Investigate with the read-only tools, then present a concise, concrete implementation plan. When the plan is ready, call the plan_exit tool with the full plan to ask the user for approval. If the user approves, plan mode ends and you may implement the plan. If the user rejects it, stay in plan mode and refine the plan based on their feedback.`

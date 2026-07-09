package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/tidwall/sjson"
)

// hookedTool wraps a fantasy.AgentTool to run PreToolUse hooks before
// delegating to the inner tool and PostToolUse hooks after it returns.
type hookedTool struct {
	inner  fantasy.AgentTool
	runner *hooks.Runner
}

func newHookedTool(inner fantasy.AgentTool, runner *hooks.Runner) *hookedTool {
	return &hookedTool{inner: inner, runner: runner}
}

// wrapToolsWithHooks returns a tool slice with each entry wrapped in a
// hookedTool. Returns the original slice unchanged when runner is nil or
// when isSubAgent is true — sub-agents never fire hooks, the top-level
// invocation of the sub-agent tool itself is wrapped on the caller's side.
func wrapToolsWithHooks(tools []fantasy.AgentTool, runner *hooks.Runner, isSubAgent bool) []fantasy.AgentTool {
	if runner == nil || isSubAgent {
		return tools
	}
	out := make([]fantasy.AgentTool, len(tools))
	for i, tool := range tools {
		out[i] = newHookedTool(tool, runner)
	}
	return out
}

func (h *hookedTool) Info() fantasy.ToolInfo {
	return h.inner.Info()
}

func (h *hookedTool) ProviderOptions() fantasy.ProviderOptions {
	return h.inner.ProviderOptions()
}

func (h *hookedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	h.inner.SetProviderOptions(opts)
}

func (h *hookedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	result, err := h.runner.Run(ctx, hooks.Event{
		Name:      hooks.EventPreToolUse,
		SessionID: sessionID,
		ToolName:  call.Name,
		ToolInput: call.Input,
	})
	if err != nil {
		slog.Warn("Hook execution error, proceeding with tool call",
			"tool", call.Name, "error", err)
	}

	if result.Decision == hooks.DecisionDeny || result.Halt {
		reason := fmt.Sprintf("Tool call blocked by hook. Reason: %s", result.Reason)
		if result.Halt {
			reason = fmt.Sprintf("Turn halted by hook. Reason: %s", result.Reason)
		}
		resp := fantasy.NewTextErrorResponse(reason)
		// Halt ends the whole turn; a plain deny only blocks this tool
		// call so the model can see the error and try something else.
		// The tool never ran, so PostToolUse does not fire.
		resp.StopTurn = result.Halt
		resp.Metadata = hookMetadataJSON(result)
		return resp, nil
	}

	if result.UpdatedInput != "" {
		call.Input = result.UpdatedInput
	}

	// An explicit allow from a hook pre-approves the permission prompt for
	// this tool call. Deny is already handled above; silence falls through
	// to the normal permission flow.
	if result.Decision == hooks.DecisionAllow {
		ctx = permission.WithHookApproval(ctx, call.ID)
	}

	resp, err := h.inner.Run(ctx, call)
	if err != nil {
		return resp, err
	}

	if result.Context != "" {
		appendToContent(&resp, result.Context)
	}

	post := h.runPostToolUse(ctx, sessionID, call, &resp)

	resp.Metadata = mergeHookMetadata(resp.Metadata, "hook", result)
	resp.Metadata = mergeHookMetadata(resp.Metadata, "post_hook", post)
	return resp, nil
}

// runPostToolUse fires PostToolUse hooks for an executed tool call and
// applies their outcome to the tool response in place. The tool already
// ran, so hooks can no longer prevent execution: a deny appends the
// hook's reason to the result content as feedback the model sees,
// context is appended as-is, and halt ends the turn after this call.
// Because call.Input was already rewritten by any PreToolUse
// updated_input patch, PostToolUse hooks see the input the tool actually
// ran with.
func (h *hookedTool) runPostToolUse(ctx context.Context, sessionID string, call fantasy.ToolCall, resp *fantasy.ToolResponse) hooks.AggregateResult {
	post, err := h.runner.Run(ctx, hooks.Event{
		Name:         hooks.EventPostToolUse,
		SessionID:    sessionID,
		ToolName:     call.Name,
		ToolInput:    call.Input,
		ToolResponse: toolResponseJSON(*resp),
	})
	if err != nil {
		slog.Warn("Hook execution error, keeping tool result",
			"tool", call.Name, "error", err)
	}

	if post.Reason != "" && (post.Decision == hooks.DecisionDeny || post.Halt) {
		appendToContent(resp, fmt.Sprintf("PostToolUse hook feedback: %s", post.Reason))
	}
	if post.Context != "" {
		appendToContent(resp, post.Context)
	}
	if post.Halt {
		resp.StopTurn = true
	}
	return post
}

// appendToContent appends a line of text to a tool response's content.
func appendToContent(resp *fantasy.ToolResponse, text string) {
	if resp.Content != "" {
		resp.Content += "\n"
	}
	resp.Content += text
}

// toolResponseJSON renders an executed tool's outcome as the PostToolUse
// tool_response payload: the result content plus the error flag.
func toolResponseJSON(resp fantasy.ToolResponse) string {
	data, err := json.Marshal(struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}{Content: resp.Content, IsError: resp.IsError})
	if err != nil {
		return "{}"
	}
	return string(data)
}

// buildHookMetadata creates a HookMetadata from an AggregateResult.
func buildHookMetadata(result hooks.AggregateResult) hooks.HookMetadata {
	return hooks.HookMetadata{
		HookCount:    result.HookCount,
		Decision:     result.Decision.String(),
		Halt:         result.Halt,
		Reason:       result.Reason,
		InputRewrite: result.UpdatedInput != "",
		Hooks:        result.Hooks,
	}
}

// hookMetadataJSON builds a JSON string containing only the hook metadata.
func hookMetadataJSON(result hooks.AggregateResult) string {
	meta := buildHookMetadata(result)
	data, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return `{"hook":` + string(data) + `}`
}

// mergeHookMetadata injects hook metadata into existing tool metadata
// under the given key ("hook" for PreToolUse, "post_hook" for
// PostToolUse).
func mergeHookMetadata(existing, key string, result hooks.AggregateResult) string {
	if result.HookCount == 0 {
		return existing
	}
	meta := buildHookMetadata(result)
	data, err := json.Marshal(meta)
	if err != nil {
		return existing
	}
	if existing == "" {
		existing = "{}"
	}
	merged, err := sjson.SetRaw(existing, key, string(data))
	if err != nil {
		return existing
	}
	return merged
}

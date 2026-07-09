package hooks

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

// newRunner builds a Runner holding the given hooks under a single
// event, mirroring how the coordinator constructs it from the config
// map.
func newRunner(t *testing.T, event string, hooks ...config.HookConfig) *Runner {
	t.Helper()
	return NewRunner(map[string][]config.HookConfig{event: hooks}, t.TempDir(), t.TempDir())
}

// runToolEvent fires an event carrying a tool name and input against the
// runner and requires the run itself to succeed.
func runToolEvent(t *testing.T, r *Runner, event, tool, input string) AggregateResult {
	t.Helper()
	result, err := r.Run(context.Background(), Event{
		Name:      event,
		SessionID: "sess",
		ToolName:  tool,
		ToolInput: input,
	})
	require.NoError(t, err)
	return result
}

func TestAggregation(t *testing.T) {
	t.Parallel()

	t.Run("empty results", func(t *testing.T) {
		t.Parallel()
		agg := aggregate(nil, "{}")
		require.Equal(t, DecisionNone, agg.Decision)
		require.Empty(t, agg.Reason)
		require.Empty(t, agg.Context)
		require.False(t, agg.Halt)
	})

	t.Run("single allow", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow},
		}, "{}")
		require.Equal(t, DecisionAllow, agg.Decision)
	})

	t.Run("deny wins over allow", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, Context: "ctx1"},
			{Decision: DecisionDeny, Reason: "blocked"},
		}, "{}")
		require.Equal(t, DecisionDeny, agg.Decision)
		require.Equal(t, "blocked", agg.Reason)
		require.Equal(t, "ctx1", agg.Context)
	})

	t.Run("multiple deny reasons concatenated", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionDeny, Reason: "reason1"},
			{Decision: DecisionDeny, Reason: "reason2"},
		}, "{}")
		require.Equal(t, DecisionDeny, agg.Decision)
		require.Equal(t, "reason1\nreason2", agg.Reason)
	})

	t.Run("context concatenated from all hooks", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, Context: "ctx-a"},
			{Decision: DecisionNone, Context: "ctx-b"},
		}, "{}")
		require.Equal(t, DecisionAllow, agg.Decision)
		require.Equal(t, "ctx-a\nctx-b", agg.Context)
	})

	t.Run("allow wins over none", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionNone},
			{Decision: DecisionAllow},
		}, "{}")
		require.Equal(t, DecisionAllow, agg.Decision)
	})

	t.Run("halt is sticky across results", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow},
			{Halt: true, Reason: "stop now"},
		}, "{}")
		require.True(t, agg.Halt)
		require.Contains(t, agg.Reason, "stop now")
	})

	t.Run("halt with deny only records reason once", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionDeny, Halt: true, Reason: "stop"},
		}, "{}")
		require.True(t, agg.Halt)
		require.Equal(t, DecisionDeny, agg.Decision)
		require.Equal(t, "stop", agg.Reason)
	})
}

func TestParseStdout(t *testing.T) {
	t.Parallel()

	t.Run("empty stdout", func(t *testing.T) {
		t.Parallel()
		r := parseStdout("")
		require.Equal(t, DecisionNone, r.Decision)
	})

	t.Run("valid allow", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","context":"some context"}`)
		require.Equal(t, DecisionAllow, r.Decision)
		require.Equal(t, "some context", r.Context)
	})

	t.Run("valid deny", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"deny","reason":"not allowed"}`)
		require.Equal(t, DecisionDeny, r.Decision)
		require.Equal(t, "not allowed", r.Reason)
	})

	t.Run("claude code block alias means deny", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"block","reason":"keep going"}`)
		require.Equal(t, DecisionDeny, r.Decision)
		require.Equal(t, "keep going", r.Reason)
	})

	t.Run("claude code approve alias means allow", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"approve"}`)
		require.Equal(t, DecisionAllow, r.Decision)
	})

	t.Run("malformed JSON", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{bad json}`)
		require.Equal(t, DecisionNone, r.Decision)
	})

	t.Run("unknown decision", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"maybe"}`)
		require.Equal(t, DecisionNone, r.Decision)
	})

	t.Run("version 1 accepted", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"version":1,"decision":"allow"}`)
		require.Equal(t, DecisionAllow, r.Decision)
	})

	t.Run("unknown higher version still parses", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"version":99,"decision":"deny","reason":"future"}`)
		require.Equal(t, DecisionDeny, r.Decision)
		require.Equal(t, "future", r.Reason)
	})

	t.Run("halt true without decision", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"halt":true,"reason":"turn over"}`)
		require.True(t, r.Halt)
		require.Equal(t, "turn over", r.Reason)
		require.Equal(t, DecisionNone, r.Decision)
	})

	t.Run("context string form", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","context":"one note"}`)
		require.Equal(t, "one note", r.Context)
	})

	t.Run("context array form", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","context":["first","second"]}`)
		require.Equal(t, "first\nsecond", r.Context)
	})

	t.Run("context array drops empty entries", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","context":["","keep",""]}`)
		require.Equal(t, "keep", r.Context)
	})

	t.Run("context null becomes empty", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","context":null}`)
		require.Empty(t, r.Context)
	})
}

func TestParseStdoutForEvent(t *testing.T) {
	t.Parallel()

	t.Run("user prompt submit plain stdout becomes context", func(t *testing.T) {
		t.Parallel()
		r := parseStdoutForEvent(EventUserPromptSubmit, "current branch: main\n")
		require.Equal(t, DecisionNone, r.Decision)
		require.Equal(t, "current branch: main", r.Context)
	})

	t.Run("user prompt submit JSON envelope still parses", func(t *testing.T) {
		t.Parallel()
		r := parseStdoutForEvent(EventUserPromptSubmit, `{"decision":"deny","reason":"nope"}`)
		require.Equal(t, DecisionDeny, r.Decision)
		require.Equal(t, "nope", r.Reason)
	})

	t.Run("user prompt submit empty stdout is no opinion", func(t *testing.T) {
		t.Parallel()
		r := parseStdoutForEvent(EventUserPromptSubmit, "  \n")
		require.Equal(t, DecisionNone, r.Decision)
		require.Empty(t, r.Context)
	})

	t.Run("other events ignore plain stdout", func(t *testing.T) {
		t.Parallel()
		r := parseStdoutForEvent(EventPreToolUse, "not json")
		require.Equal(t, DecisionNone, r.Decision)
		require.Empty(t, r.Context)
	})
}

func TestBuildEnv(t *testing.T) {
	t.Parallel()

	env := BuildEnv(Event{
		Name:      EventPreToolUse,
		SessionID: "sess-1",
		ToolName:  "bash",
		ToolInput: `{"command":"ls","file_path":"/tmp/f.txt"}`,
	}, "/work", "/project")

	envMap := make(map[string]string)
	for _, e := range env {
		parts := splitFirst(e, "=")
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	require.Equal(t, EventPreToolUse, envMap["CRUSH_EVENT"])
	require.Equal(t, "bash", envMap["CRUSH_TOOL_NAME"])
	require.Equal(t, "sess-1", envMap["CRUSH_SESSION_ID"])
	require.Equal(t, "/work", envMap["CRUSH_CWD"])
	require.Equal(t, "/project", envMap["CRUSH_PROJECT_DIR"])
	require.Equal(t, "ls", envMap["CRUSH_TOOL_INPUT_COMMAND"])
	require.Equal(t, "/tmp/f.txt", envMap["CRUSH_TOOL_INPUT_FILE_PATH"])

	// Shared Crush markers must be present so hook-authored scripts can
	// detect they're running under Crush the same way bash-tool-invoked
	// scripts can.
	require.Equal(t, "1", envMap["CRUSH"])
	require.Equal(t, "crush", envMap["AGENT"])
	require.Equal(t, "crush", envMap["AI_AGENT"])
}

func splitFirst(s, sep string) []string {
	before, after, found := strings.Cut(s, sep)
	if !found {
		return []string{s}
	}
	return []string{before, after}
}

// payloadMap round-trips a payload through JSON into a generic map so
// tests can assert on both present and absent fields.
func payloadMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func TestBuildPayload(t *testing.T) {
	t.Parallel()

	t.Run("pre tool use", func(t *testing.T) {
		t.Parallel()
		payload := BuildPayload(Event{
			Name:      EventPreToolUse,
			SessionID: "sess-1",
			ToolName:  "bash",
			ToolInput: `{"command":"ls"}`,
		}, "/work")
		s := string(payload)
		require.Contains(t, s, `"event":"`+EventPreToolUse+`"`)
		require.Contains(t, s, `"hook_event_name":"`+EventPreToolUse+`"`)
		require.Contains(t, s, `"tool_name":"bash"`)
		// tool_input should be an object, not a string.
		require.Contains(t, s, `"tool_input":{"command":"ls"}`)

		m := payloadMap(t, payload)
		require.NotContains(t, m, "tool_response")
		require.NotContains(t, m, "prompt")
		require.NotContains(t, m, "stop_hook_active")
	})

	t.Run("post tool use carries the tool response", func(t *testing.T) {
		t.Parallel()
		payload := BuildPayload(Event{
			Name:         EventPostToolUse,
			SessionID:    "sess-1",
			ToolName:     "bash",
			ToolInput:    `{"command":"ls"}`,
			ToolResponse: `{"content":"file.txt","is_error":false}`,
		}, "/work")
		m := payloadMap(t, payload)
		require.Equal(t, EventPostToolUse, m["hook_event_name"])
		require.Equal(t, "bash", m["tool_name"])
		resp, ok := m["tool_response"].(map[string]any)
		require.True(t, ok, "tool_response should be an object")
		require.Equal(t, "file.txt", resp["content"])
		require.Equal(t, false, resp["is_error"])
	})

	t.Run("user prompt submit carries the prompt and no tool fields", func(t *testing.T) {
		t.Parallel()
		payload := BuildPayload(Event{
			Name:      EventUserPromptSubmit,
			SessionID: "sess-1",
			Prompt:    "fix the login flow",
		}, "/work")
		m := payloadMap(t, payload)
		require.Equal(t, EventUserPromptSubmit, m["hook_event_name"])
		require.Equal(t, "fix the login flow", m["prompt"])
		require.NotContains(t, m, "tool_name")
		require.NotContains(t, m, "tool_input")
		require.NotContains(t, m, "tool_response")
		require.NotContains(t, m, "stop_hook_active")
	})

	t.Run("stop carries stop_hook_active even when false", func(t *testing.T) {
		t.Parallel()
		m := payloadMap(t, BuildPayload(Event{Name: EventStop, SessionID: "s"}, "/work"))
		require.Equal(t, false, m["stop_hook_active"])
	})

	t.Run("subagent stop carries stop_hook_active true", func(t *testing.T) {
		t.Parallel()
		m := payloadMap(t, BuildPayload(Event{
			Name:           EventSubagentStop,
			SessionID:      "s",
			StopHookActive: true,
		}, "/work"))
		require.Equal(t, true, m["stop_hook_active"])
		require.NotContains(t, m, "prompt")
		require.NotContains(t, m, "tool_input")
	})

	t.Run("invalid tool input falls back to empty object", func(t *testing.T) {
		t.Parallel()
		m := payloadMap(t, BuildPayload(Event{
			Name:      EventPreToolUse,
			SessionID: "s",
			ToolName:  "bash",
			ToolInput: `{oops`,
		}, "/work"))
		require.Equal(t, map[string]any{}, m["tool_input"])
	})
}

func TestRunnerExitCode0Allow(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `echo '{"decision":"allow","context":"ok"}'`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, DecisionAllow, result.Decision)
	require.Equal(t, "ok", result.Context)
}

func TestRunnerExitCode2Deny(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `echo "forbidden" >&2; exit 2`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, DecisionDeny, result.Decision)
	require.False(t, result.Halt)
	require.Equal(t, "forbidden", result.Reason)
}

func TestRunnerExitCode49Halt(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `echo "stop the turn" >&2; exit 49`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.True(t, result.Halt)
	require.Equal(t, DecisionDeny, result.Decision)
	require.Equal(t, "stop the turn", result.Reason)
}

func TestRunnerHaltViaJSON(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `echo '{"halt":true,"reason":"via json"}'`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.True(t, result.Halt)
	require.Equal(t, "via json", result.Reason)
}

func TestRunnerExitCodeOtherNonBlocking(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `exit 1`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, DecisionNone, result.Decision)
}

func TestRunnerTimeout(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `sleep 10`,
		Timeout: 1,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	start := time.Now()
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	elapsed := time.Since(start)
	require.Equal(t, DecisionNone, result.Decision)
	require.Less(t, elapsed, 5*time.Second)
}

func TestRunnerDeduplication(t *testing.T) {
	t.Parallel()
	// Two hooks with the same command should only run once.
	hookCfg := config.HookConfig{
		Command: `echo '{"decision":"allow"}'`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, DecisionAllow, result.Decision)
}

func TestRunnerNoMatchingHooks(t *testing.T) {
	t.Parallel()
	// Hooks are empty.
	r := NewRunner(nil, t.TempDir(), t.TempDir())
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, DecisionNone, result.Decision)
}

func TestRunnerEventDispatch(t *testing.T) {
	t.Parallel()
	// One runner holding hooks for three different events; each firing
	// must reach only its own event's hooks.
	r := NewRunner(map[string][]config.HookConfig{
		EventPreToolUse:  {{Command: `echo '{"context":"from-pre"}'`}},
		EventPostToolUse: {{Command: `echo '{"context":"from-post"}'`}},
		EventStop:        {{Command: `echo '{"context":"from-stop"}'`}},
	}, t.TempDir(), t.TempDir())

	pre := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, "from-pre", pre.Context)
	require.Equal(t, 1, pre.HookCount)

	post := runToolEvent(t, r, EventPostToolUse, "bash", `{}`)
	require.Equal(t, "from-post", post.Context)
	require.Equal(t, 1, post.HookCount)

	stop, err := r.Run(context.Background(), Event{Name: EventStop, SessionID: "sess"})
	require.NoError(t, err)
	require.Equal(t, "from-stop", stop.Context)
	require.Equal(t, 1, stop.HookCount)

	// No hooks configured for this event at all.
	none, err := r.Run(context.Background(), Event{Name: EventUserPromptSubmit, SessionID: "sess"})
	require.NoError(t, err)
	require.Equal(t, 0, none.HookCount)
	require.Equal(t, DecisionNone, none.Decision)
}

// validatedHooks builds hook configs for an event and runs ValidateHooks
// to mirror the real config-load path, returning the full hooks map.
func validatedHooks(t *testing.T, event string, hooks []config.HookConfig) map[string][]config.HookConfig {
	t.Helper()
	cfg := &config.Config{
		Hooks: map[string][]config.HookConfig{
			event: hooks,
		},
	}
	require.NoError(t, cfg.ValidateHooks())
	return cfg.Hooks
}

func TestRunnerMatcherFiltering(t *testing.T) {
	t.Parallel()

	t.Run("compiled regex matches", func(t *testing.T) {
		t.Parallel()
		hooks := validatedHooks(t, EventPreToolUse, []config.HookConfig{
			{Command: `echo '{"decision":"deny","reason":"blocked"}'`, Matcher: "^bash$"},
		})
		r := NewRunner(hooks, t.TempDir(), t.TempDir())
		result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionDeny, result.Decision)
	})

	t.Run("compiled regex does not match", func(t *testing.T) {
		t.Parallel()
		hooks := validatedHooks(t, EventPreToolUse, []config.HookConfig{
			{Command: `echo '{"decision":"deny","reason":"blocked"}'`, Matcher: "^edit$"},
		})
		r := NewRunner(hooks, t.TempDir(), t.TempDir())
		result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionNone, result.Decision)
	})

	t.Run("no matcher matches everything", func(t *testing.T) {
		t.Parallel()
		hooks := validatedHooks(t, EventPreToolUse, []config.HookConfig{
			{Command: `echo '{"decision":"allow"}'`},
		})
		r := NewRunner(hooks, t.TempDir(), t.TempDir())
		result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionAllow, result.Decision)
	})

	t.Run("partial regex match", func(t *testing.T) {
		t.Parallel()
		hooks := validatedHooks(t, EventPreToolUse, []config.HookConfig{
			{Command: `echo '{"decision":"deny","reason":"mcp blocked"}'`, Matcher: "^mcp_"},
		})
		r := NewRunner(hooks, t.TempDir(), t.TempDir())

		result := runToolEvent(t, r, EventPreToolUse, "mcp_github_get_me", `{}`)
		require.Equal(t, DecisionDeny, result.Decision)

		result = runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionNone, result.Decision)
	})

	t.Run("matcher applies to post tool use", func(t *testing.T) {
		t.Parallel()
		hooks := validatedHooks(t, EventPostToolUse, []config.HookConfig{
			{Command: `echo '{"context":"post-fired"}'`, Matcher: "^bash$"},
		})
		r := NewRunner(hooks, t.TempDir(), t.TempDir())

		result := runToolEvent(t, r, EventPostToolUse, "bash", `{}`)
		require.Equal(t, "post-fired", result.Context)

		result = runToolEvent(t, r, EventPostToolUse, "edit", `{}`)
		require.Equal(t, 0, result.HookCount)
	})

	t.Run("matcher is ignored on non-tool events", func(t *testing.T) {
		t.Parallel()
		// A matcher makes no sense for Stop (there is no tool name);
		// the hook fires regardless of it.
		hooks := validatedHooks(t, EventStop, []config.HookConfig{
			{Command: `echo '{"context":"stop-fired"}'`, Matcher: "^bash$"},
		})
		r := NewRunner(hooks, t.TempDir(), t.TempDir())
		result, err := r.Run(context.Background(), Event{Name: EventStop, SessionID: "sess"})
		require.NoError(t, err)
		require.Equal(t, "stop-fired", result.Context)
	})

	// Runner must compile matchers itself; it cannot rely on
	// ValidateHooks having run first. This is the guarantee that prevents
	// the reload-drops-matcher class of bug.
	t.Run("runner compiles matcher without ValidateHooks", func(t *testing.T) {
		t.Parallel()
		r := newRunner(t, EventPreToolUse, config.HookConfig{
			Command: `echo '{"decision":"deny","reason":"blocked"}'`, Matcher: "^bash$",
		})

		deny := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionDeny, deny.Decision)

		noop := runToolEvent(t, r, EventPreToolUse, "view", `{}`)
		require.Equal(t, DecisionNone, noop.Decision)
	})

	// A matcher that fails to compile at Runner construction must not
	// degrade to match-everything; the hook is dropped instead.
	t.Run("runner skips hooks with invalid matcher", func(t *testing.T) {
		t.Parallel()
		r := newRunner(t, EventPreToolUse, config.HookConfig{
			Command: `echo '{"decision":"deny","reason":"should not fire"}'`, Matcher: "[invalid",
		})

		result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionNone, result.Decision)
		require.Empty(t, r.Hooks(EventPreToolUse))
	})
}

func TestValidateHooksInvalidRegex(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Hooks: map[string][]config.HookConfig{
			EventPreToolUse: {
				{Command: "true", Matcher: "[invalid"},
			},
		},
	}
	err := cfg.ValidateHooks()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid matcher regex")
}

func TestValidateHooksEmptyCommand(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Hooks: map[string][]config.HookConfig{
			EventPreToolUse: {
				{Command: ""},
			},
		},
	}
	err := cfg.ValidateHooks()
	require.Error(t, err)
	require.Contains(t, err.Error(), "command is required")
}

func TestValidateHooksNormalizesEventNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		canonical string
	}{
		{"canonical", "PreToolUse", EventPreToolUse},
		{"lowercase", "pretooluse", EventPreToolUse},
		{"snake_case", "pre_tool_use", EventPreToolUse},
		{"upper_snake", "PRE_TOOL_USE", EventPreToolUse},
		{"mixed_case", "preToolUse", EventPreToolUse},
		{"post_tool_use", "post_tool_use", EventPostToolUse},
		{"posttooluse_lower", "posttooluse", EventPostToolUse},
		{"user_prompt_submit", "user_prompt_submit", EventUserPromptSubmit},
		{"userpromptsubmit_mixed", "userPromptSubmit", EventUserPromptSubmit},
		{"stop_lower", "stop", EventStop},
		{"subagent_stop", "subagent_stop", EventSubagentStop},
		{"subagentstop_mixed", "SubAgentStop", EventSubagentStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Hooks: map[string][]config.HookConfig{
					tt.input: {
						{Command: "true"},
					},
				},
			}
			require.NoError(t, cfg.ValidateHooks())
			require.Len(t, cfg.Hooks[tt.canonical], 1)
		})
	}
}

func TestRunnerHookNameUsesDisplayName(t *testing.T) {
	t.Parallel()

	t.Run("name field is used when set", func(t *testing.T) {
		t.Parallel()
		hookCfg := config.HookConfig{
			Name:    "my-hook",
			Command: `echo '{"decision":"allow"}'`,
		}
		r := newRunner(t, EventPreToolUse, hookCfg)
		result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionAllow, result.Decision)
		require.Len(t, result.Hooks, 1)
		require.Equal(t, "my-hook", result.Hooks[0].Name)
	})

	t.Run("command is used when name is empty", func(t *testing.T) {
		t.Parallel()
		hookCfg := config.HookConfig{
			Command: `echo '{"decision":"allow"}'`,
		}
		r := newRunner(t, EventPreToolUse, hookCfg)
		result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
		require.Equal(t, DecisionAllow, result.Decision)
		require.Len(t, result.Hooks, 1)
		require.Equal(t, `echo '{"decision":"allow"}'`, result.Hooks[0].Name)
	})
}

func TestRunnerParallelExecution(t *testing.T) {
	t.Parallel()
	// Two hooks: one allows, one denies. Deny should win.
	r := newRunner(
		t, EventPreToolUse,
		config.HookConfig{Command: `echo '{"decision":"allow","context":"hook1"}'`},
		config.HookConfig{Command: `echo '{"decision":"deny","reason":"nope"}' ; exit 0`},
	)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, DecisionDeny, result.Decision)
	require.Equal(t, "nope", result.Reason)
}

func TestRunnerEnvVarsPropagated(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `printf '{"decision":"allow","context":"%s"}' "$CRUSH_TOOL_NAME"`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	require.Equal(t, DecisionAllow, result.Decision)
	require.Equal(t, "bash", result.Context)
}

func TestRunnerStopEventSeesStopHookActive(t *testing.T) {
	t.Parallel()
	// The hook blocks only when stop_hook_active is false, which is how
	// a well-behaved Claude Code stop hook avoids looping the agent.
	hookCfg := config.HookConfig{
		Command: `read -r line; case "$line" in *'"stop_hook_active":false'*) echo "keep going" >&2; exit 2;; esac`,
	}
	r := newRunner(t, EventStop, hookCfg)

	first, err := r.Run(context.Background(), Event{Name: EventStop, SessionID: "sess"})
	require.NoError(t, err)
	require.Equal(t, DecisionDeny, first.Decision)
	require.Equal(t, "keep going", first.Reason)

	second, err := r.Run(context.Background(), Event{Name: EventStop, SessionID: "sess", StopHookActive: true})
	require.NoError(t, err)
	require.Equal(t, DecisionNone, second.Decision)
}

func TestParseStdoutUpdatedInput(t *testing.T) {
	t.Parallel()

	t.Run("nested object", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","updated_input":{"command":"rtk cat foo.go"}}`)
		require.Equal(t, DecisionAllow, r.Decision)
		require.Equal(t, `{"command":"rtk cat foo.go"}`, r.UpdatedInput)
	})

	t.Run("stringified backward compat", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","updated_input":"{\"command\":\"rtk cat foo.go\"}"}`)
		require.Equal(t, DecisionAllow, r.Decision)
		require.Equal(t, `{"command":"rtk cat foo.go"}`, r.UpdatedInput)
	})

	t.Run("no updated_input", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow"}`)
		require.Empty(t, r.UpdatedInput)
	})
}

func TestAggregationUpdatedInput(t *testing.T) {
	t.Parallel()

	t.Run("patches merge in config order with later overriding", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, UpdatedInput: `{"command":"first","keep":"me"}`},
			{Decision: DecisionAllow, UpdatedInput: `{"command":"second"}`},
		}, `{"command":"orig","timeout":60}`)
		require.Equal(t, DecisionAllow, agg.Decision)
		// command overridden by second patch; keep preserved from first
		// patch; timeout preserved from original input.
		require.JSONEq(
			t,
			`{"command":"second","keep":"me","timeout":60}`,
			agg.UpdatedInput,
		)
	})

	t.Run("shallow: nested objects are replaced wholesale", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, UpdatedInput: `{"env":{"FOO":"bar"}}`},
		}, `{"env":{"BAZ":"qux"},"command":"ls"}`)
		// "env" is replaced entirely; "command" preserved.
		require.JSONEq(
			t,
			`{"env":{"FOO":"bar"},"command":"ls"}`,
			agg.UpdatedInput,
		)
	})

	t.Run("deny still reports merged input (caller ignores it)", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, UpdatedInput: `{"command":"rewritten"}`},
			{Decision: DecisionDeny, Reason: "blocked"},
		}, `{"command":"orig"}`)
		require.Equal(t, DecisionDeny, agg.Decision)
	})

	t.Run("no patches leaves updated_input empty", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow},
			{Decision: DecisionNone},
		}, `{"command":"orig"}`)
		require.Empty(t, agg.UpdatedInput)
	})

	t.Run("invalid patch is ignored", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, UpdatedInput: `"not-an-object"`},
			{Decision: DecisionAllow, UpdatedInput: `{"command":"good"}`},
		}, `{"command":"orig"}`)
		require.JSONEq(t, `{"command":"good"}`, agg.UpdatedInput)
	})

	t.Run("malformed patch JSON is ignored and merge continues", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, UpdatedInput: `{broken json`},
			{Decision: DecisionAllow, UpdatedInput: `{"command":"good"}`},
		}, `{"command":"orig"}`)
		require.JSONEq(t, `{"command":"good"}`, agg.UpdatedInput)
	})

	t.Run("non-object tool_input rejects all patches", func(t *testing.T) {
		t.Parallel()
		agg := aggregate([]HookResult{
			{Decision: DecisionAllow, UpdatedInput: `{"command":"rewrite"}`},
		}, `"just-a-string"`)
		require.Empty(t, agg.UpdatedInput)
	})

	t.Run("null updated_input is a no-op", func(t *testing.T) {
		t.Parallel()
		// parseStdout converts null updated_input to "", so aggregate
		// never sees a patch — the merged input is empty and the
		// original tool_input is used unchanged.
		r := parseStdout(`{"decision":"allow","updated_input":null}`)
		require.Empty(t, r.UpdatedInput)
		agg := aggregate([]HookResult{r}, `{"command":"orig"}`)
		require.Empty(t, agg.UpdatedInput)
	})
}

// TestRunnerAbandonRaceSafety verifies that if a hook's shell execution
// does not yield to ctx cancellation within abandonGrace, runOne returns
// promptly and never touches the shared stdout/stderr buffers again —
// even while the abandoned goroutine continues to write to them.
//
// The substitute shell executor ignores ctx entirely, writes to Stdout
// both before and after the abandon deadline, and only then returns.
// Under -race this catches any code path in runOne that reads those
// buffers after returning the DecisionNone abandon result.
func TestRunnerAbandonRaceSafety(t *testing.T) {
	origRunShell := runShell
	t.Cleanup(func() { runShell = origRunShell })

	// Synchronize shutdown with the abandoned goroutine so the test
	// exits cleanly even under -race.
	var wg sync.WaitGroup
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		wg.Wait()
	})

	runShell = func(_ context.Context, opts shell.RunOptions) error {
		wg.Add(1)
		defer wg.Done()
		// Write before the caller observes ctx.Done(); the caller will
		// not read the buffer while we still own it.
		_, _ = io.WriteString(opts.Stdout, "before\n")
		// Hold past ctx deadline + abandonGrace so the caller takes
		// the abandon branch, then continue writing. If the caller
		// reads these buffers after abandoning, -race will flag it.
		select {
		case <-time.After(5 * time.Second):
		case <-release:
		}
		_, _ = io.WriteString(opts.Stdout, "after\n")
		return nil
	}

	hookCfg := config.HookConfig{
		Command: "# irrelevant; runShell is stubbed",
		Timeout: 1,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)

	start := time.Now()
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{}`)
	elapsed := time.Since(start)

	require.Equal(t, DecisionNone, result.Decision)
	// Abandon must happen at ~timeout + abandonGrace. Allow generous
	// slack so CI noise doesn't flake the test.
	require.Less(t, elapsed, 3500*time.Millisecond,
		"runOne should return within timeout+abandonGrace+slack")
}

func TestRunnerUpdatedInput(t *testing.T) {
	t.Parallel()
	hookCfg := config.HookConfig{
		Command: `echo '{"decision":"allow","updated_input":{"command":"echo rewritten"}}'`,
	}
	r := newRunner(t, EventPreToolUse, hookCfg)
	result := runToolEvent(t, r, EventPreToolUse, "bash", `{"command":"echo original","timeout":60}`)
	require.Equal(t, DecisionAllow, result.Decision)
	require.JSONEq(
		t,
		`{"command":"echo rewritten","timeout":60}`,
		result.UpdatedInput,
	)
}

func TestParseStdoutClaudeCodeFormat(t *testing.T) {
	t.Parallel()

	t.Run("allow with reason", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"hookSpecificOutput":{"permissionDecision":"allow","permissionDecisionReason":"RTK auto-rewrite"}}`)
		require.Equal(t, DecisionAllow, r.Decision)
		require.Equal(t, "RTK auto-rewrite", r.Reason)
	})

	t.Run("allow with updatedInput", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"command":"rtk cat foo.go"}}}`)
		require.Equal(t, DecisionAllow, r.Decision)
		require.Equal(t, `{"command":"rtk cat foo.go"}`, r.UpdatedInput)
	})

	t.Run("deny", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"not allowed"}}`)
		require.Equal(t, DecisionDeny, r.Decision)
		require.Equal(t, "not allowed", r.Reason)
	})

	t.Run("additionalContext is context", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"branch: main"}}`)
		require.Equal(t, DecisionNone, r.Decision)
		require.Equal(t, "branch: main", r.Context)
	})

	t.Run("no permissionDecision", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"hookSpecificOutput":{}}`)
		require.Equal(t, DecisionNone, r.Decision)
	})

	t.Run("crush format still works", func(t *testing.T) {
		t.Parallel()
		r := parseStdout(`{"decision":"allow","context":"hello"}`)
		require.Equal(t, DecisionAllow, r.Decision)
		require.Equal(t, "hello", r.Context)
	})
}

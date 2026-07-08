package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/stretchr/testify/require"
)

// promptRunner builds a hooks.Runner holding a single UserPromptSubmit
// hook.
func promptRunner(t *testing.T, cmd string) *hooks.Runner {
	t.Helper()
	return newRunner(t, map[string][]config.HookConfig{
		hooks.EventUserPromptSubmit: {{Command: cmd}},
	})
}

// stopRunner builds a hooks.Runner holding a single hook for the given
// stop-style event (Stop or SubagentStop).
func stopRunner(t *testing.T, event, cmd string) *hooks.Runner {
	t.Helper()
	return newRunner(t, map[string][]config.HookConfig{
		event: {{Command: cmd}},
	})
}

// stopAgent builds a sessionAgent with just enough state to exercise
// runStopHooks.
func stopAgent(runner *hooks.Runner) *sessionAgent {
	return &sessionAgent{
		hooks:        runner,
		messageQueue: csync.NewMap[string, []SessionAgentCall](),
	}
}

func TestRunUserPromptHooks(t *testing.T) {
	t.Parallel()

	t.Run("no hooks passes the prompt through", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{}
		prompt, err := c.runUserPromptHooks(t.Context(), "sess", "hello")
		require.NoError(t, err)
		require.Equal(t, "hello", prompt)
	})

	t.Run("silent hook passes the prompt through", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: promptRunner(t, `exit 0`)}
		prompt, err := c.runUserPromptHooks(t.Context(), "sess", "hello")
		require.NoError(t, err)
		require.Equal(t, "hello", prompt)
	})

	t.Run("deny blocks the submission with the reason", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: promptRunner(t, `echo "mentions production.env" >&2; exit 2`)}
		_, err := c.runUserPromptHooks(t.Context(), "sess", "leak production.env please")
		require.Error(t, err)
		require.Contains(t, err.Error(), "prompt blocked by UserPromptSubmit hook")
		require.Contains(t, err.Error(), "mentions production.env")
	})

	t.Run("claude code block decision also blocks", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: promptRunner(t, `echo '{"decision":"block","reason":"policy"}'`)}
		_, err := c.runUserPromptHooks(t.Context(), "sess", "hello")
		require.Error(t, err)
		require.Contains(t, err.Error(), "policy")
	})

	t.Run("halt blocks the submission", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: promptRunner(t, `echo "stop everything" >&2; exit 49`)}
		_, err := c.runUserPromptHooks(t.Context(), "sess", "hello")
		require.Error(t, err)
		require.Contains(t, err.Error(), "stop everything")
	})

	t.Run("context is appended to the prompt", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: promptRunner(t, `echo '{"context":"current branch: main"}'`)}
		prompt, err := c.runUserPromptHooks(t.Context(), "sess", "fix the bug")
		require.NoError(t, err)
		require.Equal(t, "fix the bug\n\ncurrent branch: main", prompt)
	})

	t.Run("plain stdout is appended as context", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: promptRunner(t, `echo "note from hook"`)}
		prompt, err := c.runUserPromptHooks(t.Context(), "sess", "fix the bug")
		require.NoError(t, err)
		require.Equal(t, "fix the bug\n\nnote from hook", prompt)
	})

	t.Run("hook sees the prompt on stdin", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: promptRunner(t,
			`read -r line; case "$line" in *'"prompt":"magic-word"'*) echo "denied" >&2; exit 2;; esac`)}

		_, err := c.runUserPromptHooks(t.Context(), "sess", "magic-word")
		require.Error(t, err)

		prompt, err := c.runUserPromptHooks(t.Context(), "sess", "ordinary")
		require.NoError(t, err)
		require.Equal(t, "ordinary", prompt)
	})
}

func TestRunStopHooks(t *testing.T) {
	t.Parallel()

	call := SessionAgentCall{SessionID: "sess", Prompt: "original prompt"}

	t.Run("nil runner never continues", func(t *testing.T) {
		t.Parallel()
		a := stopAgent(nil)
		_, ok := a.runStopHooks(t.Context(), call)
		require.False(t, ok)
	})

	t.Run("silent hook lets the turn end", func(t *testing.T) {
		t.Parallel()
		a := stopAgent(stopRunner(t, hooks.EventStop, `exit 0`))
		_, ok := a.runStopHooks(t.Context(), call)
		require.False(t, ok)
	})

	t.Run("block continues with the reason as the prompt", func(t *testing.T) {
		t.Parallel()
		a := stopAgent(stopRunner(t, hooks.EventStop, `echo "you forgot the tests" >&2; exit 2`))
		cont, ok := a.runStopHooks(t.Context(), SessionAgentCall{
			SessionID:   "sess",
			Prompt:      "original prompt",
			RunID:       "run-1",
			Attachments: nil,
		})
		require.True(t, ok)
		require.Equal(t, "you forgot the tests", cont.Prompt)
		require.True(t, cont.stopHookContinuation)
		require.Equal(t, "run-1", cont.RunID, "continuation keeps the turn's RunID")
		require.Nil(t, cont.Attachments)
		require.Nil(t, cont.Accepted)
	})

	t.Run("claude code block decision also continues", func(t *testing.T) {
		t.Parallel()
		a := stopAgent(stopRunner(t, hooks.EventStop, `echo '{"decision":"block","reason":"keep going"}'`))
		cont, ok := a.runStopHooks(t.Context(), call)
		require.True(t, ok)
		require.Equal(t, "keep going", cont.Prompt)
	})

	t.Run("block on a continuation turn is ignored", func(t *testing.T) {
		t.Parallel()
		a := stopAgent(stopRunner(t, hooks.EventStop, `echo "more" >&2; exit 2`))
		contCall := call
		contCall.stopHookContinuation = true
		_, ok := a.runStopHooks(t.Context(), contCall)
		require.False(t, ok, "a hook that blocks again after a continuation must not loop the agent")
	})

	t.Run("hook observes stop_hook_active", func(t *testing.T) {
		t.Parallel()
		// The hook blocks only when stop_hook_active is false — the
		// documented way for a hook script to cooperate with the guard.
		a := stopAgent(stopRunner(t, hooks.EventStop,
			`read -r line; case "$line" in *'"stop_hook_active":false'*) echo "continue" >&2; exit 2;; esac`))

		cont, ok := a.runStopHooks(t.Context(), call)
		require.True(t, ok)
		require.Equal(t, "continue", cont.Prompt)

		_, ok = a.runStopHooks(t.Context(), cont)
		require.False(t, ok)
	})

	t.Run("block without a reason is ignored", func(t *testing.T) {
		t.Parallel()
		// A continuation needs a prompt; a reasonless block would send
		// an empty one, so it is dropped. (Exit-code blocks always have
		// a fallback reason; only the JSON form can omit it.)
		a := stopAgent(stopRunner(t, hooks.EventStop, `echo '{"decision":"deny"}'`))
		_, ok := a.runStopHooks(t.Context(), call)
		require.False(t, ok)
	})

	t.Run("queued prompts skip the stop hooks", func(t *testing.T) {
		t.Parallel()
		a := stopAgent(stopRunner(t, hooks.EventStop, `echo "should not fire" >&2; exit 2`))
		a.messageQueue.Set("sess", []SessionAgentCall{{SessionID: "sess", Prompt: "queued"}})
		_, ok := a.runStopHooks(t.Context(), call)
		require.False(t, ok, "the conversation continues via the queue; the chain's last turn fires Stop")
	})

	t.Run("cancelled context skips the stop hooks", func(t *testing.T) {
		t.Parallel()
		a := stopAgent(stopRunner(t, hooks.EventStop, `echo "should not fire" >&2; exit 2`))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, ok := a.runStopHooks(ctx, call)
		require.False(t, ok)
	})
}

func TestRunSubagentStopHooks(t *testing.T) {
	t.Parallel()

	original := &fantasy.AgentResult{}

	t.Run("nil runner keeps the result and never runs", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{}
		out := c.runSubagentStopHooks(t.Context(), "sess", original, func(string) (*fantasy.AgentResult, error) {
			t.Fatal("run must not be called")
			return nil, nil
		})
		require.Same(t, original, out)
	})

	t.Run("silent hook keeps the result", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: stopRunner(t, hooks.EventSubagentStop, `exit 0`)}
		out := c.runSubagentStopHooks(t.Context(), "sess", original, func(string) (*fantasy.AgentResult, error) {
			t.Fatal("run must not be called")
			return nil, nil
		})
		require.Same(t, original, out)
	})

	t.Run("block continues once with the reason", func(t *testing.T) {
		t.Parallel()
		// The hook always blocks; the stop_hook_active guard must still
		// cap it at exactly one continuation.
		c := &coordinator{hooks: stopRunner(t, hooks.EventSubagentStop, `echo "dig deeper" >&2; exit 2`)}
		continued := &fantasy.AgentResult{}
		calls := 0
		out := c.runSubagentStopHooks(t.Context(), "sess", original, func(prompt string) (*fantasy.AgentResult, error) {
			calls++
			require.Equal(t, "dig deeper", prompt)
			return continued, nil
		})
		require.Equal(t, 1, calls, "an always-blocking hook gets exactly one continuation")
		require.Same(t, continued, out)
	})

	t.Run("continuation failure keeps the original result", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: stopRunner(t, hooks.EventSubagentStop, `echo "again" >&2; exit 2`)}
		out := c.runSubagentStopHooks(t.Context(), "sess", original, func(string) (*fantasy.AgentResult, error) {
			return nil, context.DeadlineExceeded
		})
		require.Same(t, original, out)
	})

	t.Run("block without a reason does not continue", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: stopRunner(t, hooks.EventSubagentStop, `echo '{"decision":"block"}'`)}
		out := c.runSubagentStopHooks(t.Context(), "sess", original, func(string) (*fantasy.AgentResult, error) {
			t.Fatal("run must not be called")
			return nil, nil
		})
		require.Same(t, original, out)
	})

	t.Run("well-behaved hook checks stop_hook_active", func(t *testing.T) {
		t.Parallel()
		c := &coordinator{hooks: stopRunner(t, hooks.EventSubagentStop,
			`read -r line; case "$line" in *'"stop_hook_active":false'*) echo "one more pass" >&2; exit 2;; esac`)}
		continued := &fantasy.AgentResult{}
		calls := 0
		out := c.runSubagentStopHooks(t.Context(), "sess", original, func(string) (*fantasy.AgentResult, error) {
			calls++
			return continued, nil
		})
		require.Equal(t, 1, calls)
		require.Same(t, continued, out)
	})
}

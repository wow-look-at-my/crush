package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateAgentDirs points the global agent-file directory at an empty
// temp dir so tests never pick up agent files from the developer's real
// ~/.config/crush/agents.
func isolateAgentDirs(t *testing.T) {
	t.Helper()
	t.Setenv("CRUSH_GLOBAL_CONFIG", t.TempDir())
}

func TestConfig_setupAgentsWithCustomAgents(t *testing.T) {
	cfg := &Config{
		Options: &Options{},
		Agents: map[string]Agent{
			"reviewer": {Description: "Reviews code."},
			"fixer": {
				Description:  "Fixes bugs.",
				Model:        SelectedModelTypeSmall,
				AllowedTools: []string{"edit", "write", "view"},
				AllowedMCP:   map[string][]string{"context7": nil},
			},
			"retired": {Description: "Old agent.", Disabled: true},
		},
	}

	cfg.SetupAgents()

	t.Run("defaults mirror the task agent", func(t *testing.T) {
		reviewer, ok := cfg.Agents["reviewer"]
		require.True(t, ok)
		assert.Equal(t, "reviewer", reviewer.ID)
		assert.Equal(t, "reviewer", reviewer.Name)
		assert.Equal(t, SelectedModelTypeLarge, reviewer.Model)
		assert.Equal(t, []string{"glob", "grep", "ls", "sourcegraph", "view", "web_search"}, reviewer.AllowedTools)
		assert.Equal(t, map[string][]string{}, reviewer.AllowedMCP)
	})

	t.Run("explicit grants are kept", func(t *testing.T) {
		fixer, ok := cfg.Agents["fixer"]
		require.True(t, ok)
		assert.Equal(t, SelectedModelTypeSmall, fixer.Model)
		assert.Equal(t, []string{"edit", "write", "view"}, fixer.AllowedTools)
		assert.Equal(t, map[string][]string{"context7": nil}, fixer.AllowedMCP)
	})

	t.Run("disabled agents are excluded", func(t *testing.T) {
		_, ok := cfg.Agents["retired"]
		assert.False(t, ok)
	})

	t.Run("built-ins are present", func(t *testing.T) {
		assert.Contains(t, cfg.Agents, AgentCoder)
		assert.Contains(t, cfg.Agents, AgentTask)
	})

	t.Run("idempotent", func(t *testing.T) {
		before := cfg.Agents
		cfg.SetupAgents()
		assert.Equal(t, before, cfg.Agents)
	})
}

func TestConfig_setupAgentsCustomAgentDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{DisabledTools: []string{"edit", "grep"}},
		Agents: map[string]Agent{
			"fixer":  {Description: "Fixes bugs.", AllowedTools: []string{"edit", "write", "grep"}},
			"walled": {Description: "Nothing left.", AllowedTools: []string{"edit"}},
		},
	}

	cfg.SetupAgents()

	fixer := cfg.Agents["fixer"]
	assert.Equal(t, []string{"write"}, fixer.AllowedTools, "disabled tools must be filtered out of explicit grants")

	walled := cfg.Agents["walled"]
	require.NotNil(t, walled.AllowedTools, "a fully filtered grant must stay empty rather than reverting to the read-only default")
	assert.Empty(t, walled.AllowedTools)

	// Repeat setup must not turn the empty grant into the read-only set.
	cfg.SetupAgents()
	walled = cfg.Agents["walled"]
	require.NotNil(t, walled.AllowedTools)
	assert.Empty(t, walled.AllowedTools)
}

func TestConfig_setupAgentsReservedKeysAlwaysRebuilt(t *testing.T) {
	cfg := &Config{
		Options: &Options{},
		Agents: map[string]Agent{
			// Simulates a registry that already went through SetupAgents
			// (e.g. a config received over the wire): the built-ins are
			// present and must be rebuilt, not treated as custom agents.
			AgentCoder: {ID: AgentCoder, Name: "Not Coder", Description: "tampered"},
			AgentTask:  {ID: AgentTask, Name: "Not Task", Description: "tampered"},
		},
	}

	cfg.SetupAgents()

	assert.Equal(t, "Coder", cfg.Agents[AgentCoder].Name)
	assert.Equal(t, "Task", cfg.Agents[AgentTask].Name)
}

func TestConfig_ValidateAgents(t *testing.T) {
	isolateAgentDirs(t)
	workingDir := t.TempDir()

	valid := func(agents map[string]Agent) *Config {
		return &Config{
			Options: &Options{DataDirectory: filepath.Join(workingDir, ".crush")},
			Agents:  agents,
		}
	}

	t.Run("valid definitions pass", func(t *testing.T) {
		cfg := valid(map[string]Agent{
			"reviewer": {Description: "Reviews code.", AllowedTools: []string{"view", "grep"}, Model: SelectedModelTypeSmall},
		})
		require.NoError(t, cfg.ValidateAgents(workingDir))
	})

	t.Run("reserved names error", func(t *testing.T) {
		for _, name := range []string{AgentCoder, AgentTask} {
			cfg := valid(map[string]Agent{name: {Description: "boom"}})
			err := cfg.ValidateAgents(workingDir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "reserved")
		}
	})

	t.Run("invalid name errors", func(t *testing.T) {
		cfg := valid(map[string]Agent{"bad name!": {Description: "boom"}})
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid name")
	})

	t.Run("description is required", func(t *testing.T) {
		cfg := valid(map[string]Agent{"reviewer": {}})
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "description is required")
	})

	t.Run("invalid model errors", func(t *testing.T) {
		cfg := valid(map[string]Agent{"reviewer": {Description: "ok", Model: "huge"}})
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `invalid model "huge"`)
	})

	t.Run("unknown tool errors", func(t *testing.T) {
		cfg := valid(map[string]Agent{"reviewer": {Description: "ok", AllowedTools: []string{"teleport"}}})
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown tool "teleport"`)
	})

	t.Run("agent tool cannot be granted", func(t *testing.T) {
		cfg := valid(map[string]Agent{"reviewer": {Description: "ok", AllowedTools: []string{"agent"}}})
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent tool cannot be granted")
	})

	t.Run("prompt and prompt_file are mutually exclusive", func(t *testing.T) {
		cfg := valid(map[string]Agent{"reviewer": {Description: "ok", Prompt: "a", PromptFile: "b.md"}})
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("prompt_file is inlined", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(workingDir, "reviewer-prompt.md"), []byte("You review code."), 0o644))
		cfg := valid(map[string]Agent{"reviewer": {Description: "ok", PromptFile: "reviewer-prompt.md"}})
		require.NoError(t, cfg.ValidateAgents(workingDir))
		agent := cfg.Agents["reviewer"]
		assert.Equal(t, "You review code.", agent.Prompt)
		assert.Empty(t, agent.PromptFile, "prompt_file must be cleared once inlined")
	})

	t.Run("missing prompt_file errors", func(t *testing.T) {
		cfg := valid(map[string]Agent{"reviewer": {Description: "ok", PromptFile: "nope.md"}})
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading prompt_file")
	})
}

func TestConfig_LoadAgentFiles(t *testing.T) {
	writeAgent := func(t *testing.T, dir, name, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}

	newCfg := func(workingDir string, agents map[string]Agent) *Config {
		return &Config{
			Options: &Options{DataDirectory: filepath.Join(workingDir, ".crush")},
			Agents:  agents,
		}
	}

	t.Run("loads project and global agents", func(t *testing.T) {
		globalDir := t.TempDir()
		t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)
		workingDir := t.TempDir()

		writeAgent(t, filepath.Join(globalDir, "agents"), "researcher.md",
			"---\ndescription: Researches things.\ntools: view, grep\nmodel: small\n---\n\nYou are a researcher.\n")
		writeAgent(t, filepath.Join(workingDir, ".crush", "agents"), "reviewer.md",
			"---\ndescription: Reviews code.\ntools:\n  - view\n  - grep\n---\nYou review code.\n")

		cfg := newCfg(workingDir, nil)
		require.NoError(t, cfg.ValidateAgents(workingDir))

		researcher, ok := cfg.Agents["researcher"]
		require.True(t, ok)
		assert.Equal(t, "Researches things.", researcher.Description)
		assert.Equal(t, []string{"view", "grep"}, researcher.AllowedTools, "comma-separated tools must parse")
		assert.Equal(t, SelectedModelTypeSmall, researcher.Model)
		assert.Equal(t, "You are a researcher.", researcher.Prompt)

		reviewer, ok := cfg.Agents["reviewer"]
		require.True(t, ok)
		assert.Equal(t, []string{"view", "grep"}, reviewer.AllowedTools, "list tools must parse")
		assert.Equal(t, "You review code.", reviewer.Prompt)
	})

	t.Run("config wins over project which wins over global", func(t *testing.T) {
		globalDir := t.TempDir()
		t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)
		workingDir := t.TempDir()

		writeAgent(t, filepath.Join(globalDir, "agents"), "reviewer.md",
			"---\ndescription: Global reviewer.\n---\nglobal\n")
		writeAgent(t, filepath.Join(workingDir, ".crush", "agents"), "reviewer.md",
			"---\ndescription: Project reviewer.\n---\nproject\n")
		writeAgent(t, filepath.Join(workingDir, ".crush", "agents"), "researcher.md",
			"---\ndescription: Project researcher.\n---\nproject\n")

		cfg := newCfg(workingDir, map[string]Agent{
			"researcher": {Description: "Config researcher.", Prompt: "config"},
		})
		require.NoError(t, cfg.ValidateAgents(workingDir))

		assert.Equal(t, "Project reviewer.", cfg.Agents["reviewer"].Description)
		assert.Equal(t, "Config researcher.", cfg.Agents["researcher"].Description)
	})

	t.Run("frontmatter name overrides the file name", func(t *testing.T) {
		globalDir := t.TempDir()
		t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)
		workingDir := t.TempDir()

		writeAgent(t, filepath.Join(workingDir, ".crush", "agents"), "some-file.md",
			"---\nname: reviewer\ndescription: Reviews code.\n---\nbody\n")

		cfg := newCfg(workingDir, nil)
		require.NoError(t, cfg.ValidateAgents(workingDir))
		assert.Contains(t, cfg.Agents, "reviewer")
		assert.NotContains(t, cfg.Agents, "some-file")
	})

	t.Run("disabled agent files load but are excluded from the registry", func(t *testing.T) {
		globalDir := t.TempDir()
		t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)
		workingDir := t.TempDir()

		writeAgent(t, filepath.Join(workingDir, ".crush", "agents"), "retired.md",
			"---\ndescription: Old.\ndisabled: true\n---\nbody\n")

		cfg := newCfg(workingDir, nil)
		require.NoError(t, cfg.ValidateAgents(workingDir))
		require.Contains(t, cfg.Agents, "retired")

		cfg.SetupAgents()
		assert.NotContains(t, cfg.Agents, "retired")
	})

	t.Run("malformed files are config errors", func(t *testing.T) {
		globalDir := t.TempDir()
		t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)
		workingDir := t.TempDir()

		writeAgent(t, filepath.Join(workingDir, ".crush", "agents"), "broken.md", "no frontmatter here\n")

		cfg := newCfg(workingDir, nil)
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing YAML frontmatter")
	})

	t.Run("file definitions are validated", func(t *testing.T) {
		globalDir := t.TempDir()
		t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)
		workingDir := t.TempDir()

		writeAgent(t, filepath.Join(workingDir, ".crush", "agents"), "bad.md",
			"---\ndescription: Bad tools.\ntools: teleport\n---\nbody\n")

		cfg := newCfg(workingDir, nil)
		err := cfg.ValidateAgents(workingDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown tool "teleport"`)
	})
}

func TestSplitAgentFrontmatter(t *testing.T) {
	t.Parallel()

	t.Run("tolerates BOM, CRLF, and leading blank lines", func(t *testing.T) {
		t.Parallel()
		fm, body, err := splitAgentFrontmatter("\uFEFF\r\n---\r\ndescription: hi\r\n---\r\nbody\r\n")
		require.NoError(t, err)
		assert.Equal(t, "description: hi", fm)
		assert.Equal(t, "body\n", body)
	})

	t.Run("unclosed frontmatter errors", func(t *testing.T) {
		t.Parallel()
		_, _, err := splitAgentFrontmatter("---\ndescription: hi\nbody")
		require.Error(t, err)
	})
}

package config

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/home"
	"gopkg.in/yaml.v3"
)

// agentNamePattern constrains custom agent names: they become the value
// of the agent tool's `agent` parameter and part of session titles, so
// keep them simple identifiers.
var agentNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// AgentFileDirs returns the directories scanned for markdown agent
// definitions, in precedence order (first hit for a name wins). Project
// agents live next to the other per-project state under the data
// directory (`<project>/.crush/agents`); user-level agents live beside
// the global config (`~/.config/crush/agents`), mirroring the custom
// commands layout.
func AgentFileDirs(dataDirectory string) []string {
	var dirs []string
	if dataDirectory != "" {
		dirs = append(dirs, filepath.Join(dataDirectory, "agents"))
	}
	dirs = append(dirs, filepath.Join(filepath.Dir(GlobalConfig()), "agents"))
	return dirs
}

// ValidateAgents loads markdown agent definitions and validates every
// custom agent definition on the config. It runs after all config
// merging is complete (mirroring ValidateHooks) and before SetupAgents
// merges the definitions into the registry, so the user sees agent
// config errors at load time. A prompt_file reference is resolved here:
// the file content is inlined into Prompt (relative paths resolve
// against the working directory) so later consumers — including remote
// clients without the server's filesystem — always see the prompt
// itself.
func (c *Config) ValidateAgents(workingDir string) error {
	if err := c.loadAgentFiles(); err != nil {
		return err
	}
	validTools := allToolNames()
	for _, name := range slices.Sorted(maps.Keys(c.Agents)) {
		agent := c.Agents[name]
		if name == AgentCoder || name == AgentTask {
			return fmt.Errorf("agent %q: the name is reserved for a built-in agent", name)
		}
		if !agentNamePattern.MatchString(name) {
			return fmt.Errorf("agent %q: invalid name (letters, digits, '.', '_' and '-' only, starting with a letter or digit)", name)
		}
		if strings.TrimSpace(agent.Description) == "" {
			return fmt.Errorf("agent %q: description is required", name)
		}
		switch agent.Model {
		case "", SelectedModelTypeLarge, SelectedModelTypeSmall:
		default:
			return fmt.Errorf("agent %q: invalid model %q (valid values: large, small)", name, agent.Model)
		}
		for _, tool := range agent.AllowedTools {
			// "agent" is the sub-agent tool itself (agent.AgentToolName);
			// granting it would let sub-agents spawn sub-agents and
			// recurse at tool-construction time.
			if tool == "agent" {
				return fmt.Errorf("agent %q: the agent tool cannot be granted to a custom agent", name)
			}
			if !slices.Contains(validTools, tool) {
				return fmt.Errorf("agent %q: unknown tool %q in allowed_tools (valid tools: %s)", name, tool, strings.Join(validTools, ", "))
			}
		}
		if agent.Prompt != "" && agent.PromptFile != "" {
			return fmt.Errorf("agent %q: prompt and prompt_file are mutually exclusive", name)
		}
		if agent.PromptFile != "" {
			path := filepathext.SmartJoin(workingDir, home.Long(agent.PromptFile))
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("agent %q: reading prompt_file: %w", name, err)
			}
			agent.Prompt = string(data)
			agent.PromptFile = ""
			c.Agents[name] = agent
		}
	}
	return nil
}

// loadAgentFiles merges markdown agent definitions into c.Agents. A name
// already present — from the config files or an earlier (higher
// precedence) directory — wins, so the order is: crush.json, project
// agents, user agents.
func (c *Config) loadAgentFiles() error {
	for _, dir := range AgentFileDirs(c.Options.DataDirectory) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A missing (or unreadable) directory simply contributes no
			// agents, like the custom commands loader.
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			name, agent, err := parseAgentFile(path)
			if err != nil {
				return fmt.Errorf("agent file %s: %w", path, err)
			}
			if c.Agents == nil {
				c.Agents = make(map[string]Agent)
			}
			if _, exists := c.Agents[name]; exists {
				continue
			}
			c.Agents[name] = agent
		}
	}
	return nil
}

// agentFileFrontmatter is the YAML frontmatter of a markdown agent file.
type agentFileFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tools       toolList `yaml:"tools"`
	Model       string   `yaml:"model"`
	Disabled    bool     `yaml:"disabled"`
}

// toolList accepts either a YAML sequence or a comma-separated scalar
// ("view, grep"), the shape agent files commonly use elsewhere.
type toolList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (t *toolList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		var items []string
		if err := value.Decode(&items); err != nil {
			return err
		}
		if items == nil {
			// Keep an explicit empty list distinguishable from "unset"
			// (nil), which falls back to the read-only toolset.
			items = []string{}
		}
		*t = items
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		var items []string
		for part := range strings.SplitSeq(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, part)
			}
		}
		*t = items
	default:
		return fmt.Errorf("tools must be a list or a comma-separated string")
	}
	return nil
}

// parseAgentFile parses a markdown agent definition: YAML frontmatter
// (name/description/tools/model/disabled) followed by the system prompt
// as the body. The agent name defaults to the file name without its
// extension.
func parseAgentFile(path string) (string, Agent, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", Agent{}, err
	}
	frontmatter, body, err := splitAgentFrontmatter(string(content))
	if err != nil {
		return "", Agent{}, err
	}
	var meta agentFileFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return "", Agent{}, fmt.Errorf("parsing frontmatter: %w", err)
	}
	name := cmp.Or(meta.Name, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	return name, Agent{
		Description:  meta.Description,
		Disabled:     meta.Disabled,
		Model:        SelectedModelType(meta.Model),
		AllowedTools: meta.Tools,
		Prompt:       strings.TrimSpace(body),
	}, nil
}

// splitAgentFrontmatter extracts the YAML frontmatter and body from a
// markdown agent file. Same tolerances as the skills parser: an optional
// UTF-8 BOM, CRLF line endings, leading blank lines, and trailing spaces
// after the "---" delimiters.
func splitAgentFrontmatter(content string) (frontmatter, body string, err error) {
	// Strip UTF-8 BOM for compatibility with editors that include it.
	content = strings.TrimPrefix(content, "\uFEFF")
	// Normalize line endings to \n for consistent parsing.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) != ""
	})
	if start == -1 || strings.TrimSpace(lines[start]) != "---" {
		return "", "", errors.New("missing YAML frontmatter")
	}

	endOffset := slices.IndexFunc(lines[start+1:], func(line string) bool {
		return strings.TrimSpace(line) == "---"
	})
	if endOffset == -1 {
		return "", "", errors.New("unclosed frontmatter")
	}
	end := start + 1 + endOffset

	frontmatter = strings.Join(lines[start+1:end], "\n")
	body = strings.Join(lines[end+1:], "\n")
	return frontmatter, body, nil
}

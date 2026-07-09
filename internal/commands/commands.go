package commands

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/skills"
	"gopkg.in/yaml.v3"
)

var namedArgPattern = regexp.MustCompile(`\$([A-Z][A-Z0-9_]*)`)

const (
	userCommandPrefix    = "user:"
	projectCommandPrefix = "project:"
)

// Argument represents a command argument with its metadata.
type Argument struct {
	ID          string
	Title       string
	Description string
	Required    bool
}

// MCPPrompt represents a custom command loaded from an MCP server.
type MCPPrompt struct {
	ID          string
	Title       string
	Description string
	PromptID    string
	ClientID    string
	Arguments   []Argument
}

// CustomCommand represents a user-defined custom command loaded from markdown files.
type CustomCommand struct {
	ID        string
	Name      string
	Content   string
	Arguments []Argument
	// Description is the optional frontmatter description shown in the
	// command palette next to the command name.
	Description string
	// ArgumentHint is the optional frontmatter argument-hint shown in the
	// argument-entry dialog.
	ArgumentHint string
	// AllowedTools carries the frontmatter allowed-tools patterns
	// (Claude Code style, e.g. "Bash(git status:*)") that pre-approve
	// inline !`cmd` executions during expansion.
	AllowedTools []string
	// Skill is set when this command represents a user-invocable skill
	Skill *skills.Skill
}

// Frontmatter is the optional YAML frontmatter of a custom command file.
// It is deliberately a flat, extensible struct: adding support for a
// future key is one field here plus its consumer. Unknown keys are
// ignored so files written for other tools still load.
type Frontmatter struct {
	// Description is a short human-readable summary shown in the command
	// palette.
	Description string `yaml:"description"`
	// ArgumentHint hints at the expected arguments, shown in the
	// argument-entry dialog (e.g. "[issue-number] [priority]").
	ArgumentHint string `yaml:"argument-hint"`
	// AllowedTools lists Claude Code style tool patterns. Only
	// Bash(...) patterns are meaningful to crush today: they pre-approve
	// matching inline !`cmd` executions. Accepts a YAML list or a
	// comma-separated scalar.
	AllowedTools config.StringList `yaml:"allowed-tools"`
}

// parseFrontmatter splits an optional YAML frontmatter off a command
// file. Files without frontmatter — and files whose frontmatter is
// malformed (unclosed block, invalid YAML) — are returned verbatim as
// the body with a zero Frontmatter, so pre-frontmatter command files
// keep working byte-identically and a bad header never hides a command.
func parseFrontmatter(content string) (Frontmatter, string) {
	raw, body, err := config.SplitFrontmatter(content)
	if err != nil {
		return Frontmatter{}, content
	}
	var meta Frontmatter
	if err := yaml.Unmarshal([]byte(raw), &meta); err != nil {
		return Frontmatter{}, content
	}
	return meta, body
}

type commandSource struct {
	path   string
	prefix string
}

// LoadCustomCommands loads custom commands from multiple sources including
// XDG config directory, home directory, and project directory.
func LoadCustomCommands(cfg *config.Config) ([]CustomCommand, error) {
	return loadAll(buildCommandSources(cfg))
}

// FromSkillCatalog converts user-invocable catalog entries into custom
// command entries for the command palette.
func FromSkillCatalog(entries []skills.CatalogEntry) []CustomCommand {
	commands := make([]CustomCommand, 0, len(entries))
	for _, entry := range entries {
		if !entry.UserInvocable {
			continue
		}
		name := entry.Label
		if name == "" {
			name = userCommandPrefix + entry.Name
		}
		commands = append(commands, CustomCommand{
			ID:   name,
			Name: name,
			Skill: &skills.Skill{
				Name:          entry.Name,
				Description:   entry.Description,
				SkillFilePath: entry.ID,
			},
		})
	}
	return commands
}

// LoadMCPPrompts loads custom commands from available MCP servers.
func LoadMCPPrompts() ([]MCPPrompt, error) {
	var commands []MCPPrompt
	for mcpName, prompts := range mcp.Prompts() {
		for _, prompt := range prompts {
			key := mcpName + ":" + prompt.Name
			var args []Argument
			for _, arg := range prompt.Arguments {
				title := arg.Title
				if title == "" {
					title = arg.Name
				}
				args = append(args, Argument{
					ID:          arg.Name,
					Title:       title,
					Description: arg.Description,
					Required:    arg.Required,
				})
			}
			commands = append(commands, MCPPrompt{
				ID:          key,
				Title:       prompt.Title,
				Description: prompt.Description,
				PromptID:    prompt.Name,
				ClientID:    mcpName,
				Arguments:   args,
			})
		}
	}
	return commands, nil
}

func buildCommandSources(cfg *config.Config) []commandSource {
	return []commandSource{
		{
			path:   filepath.Join(home.Config(), "crush", "commands"),
			prefix: userCommandPrefix,
		},
		{
			path:   filepath.Join(home.Dir(), ".crush", "commands"),
			prefix: userCommandPrefix,
		},
		{
			path:   filepath.Join(cfg.Options.DataDirectory, "commands"),
			prefix: projectCommandPrefix,
		},
	}
}

func loadAll(sources []commandSource) ([]CustomCommand, error) {
	var commands []CustomCommand

	for _, source := range sources {
		if cmds, err := loadFromSource(source); err == nil {
			commands = append(commands, cmds...)
		}
	}

	return commands, nil
}

func loadFromSource(source commandSource) ([]CustomCommand, error) {
	if _, err := os.Stat(source.path); os.IsNotExist(err) {
		return nil, nil
	}

	var commands []CustomCommand

	err := filepath.WalkDir(source.path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isMarkdownFile(d.Name()) {
			return err
		}

		cmd, err := loadCommand(path, source.path, source.prefix)
		if err != nil {
			return nil // Skip invalid files
		}

		commands = append(commands, cmd)
		return nil
	})

	return commands, err
}

func loadCommand(path, baseDir, prefix string) (CustomCommand, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return CustomCommand{}, err
	}

	id := buildCommandID(path, baseDir, prefix)
	meta, body := parseFrontmatter(string(content))

	return CustomCommand{
		ID:           id,
		Name:         id,
		Content:      body,
		Arguments:    extractArgNames(body),
		Description:  meta.Description,
		ArgumentHint: meta.ArgumentHint,
		AllowedTools: meta.AllowedTools,
	}, nil
}

func extractArgNames(content string) []Argument {
	matches := namedArgPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var args []Argument

	for _, match := range matches {
		arg := match[1]
		if !seen[arg] {
			seen[arg] = true
			// for normal custom commands, all args are required
			args = append(args, Argument{ID: arg, Title: arg, Required: true})
		}
	}

	return args
}

func buildCommandID(path, baseDir, prefix string) string {
	relPath, _ := filepath.Rel(baseDir, path)
	parts := strings.Split(relPath, string(filepath.Separator))

	// Remove .md extension from last part
	if len(parts) > 0 {
		lastIdx := len(parts) - 1
		parts[lastIdx] = strings.TrimSuffix(parts[lastIdx], filepath.Ext(parts[lastIdx]))
	}

	return prefix + strings.Join(parts, ":")
}

func isMarkdownFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".md")
}

func GetMCPPrompt(cfg *config.ConfigStore, clientID, promptID string, args map[string]string) (string, error) {
	// Create a context with timeout since tea.Cmd doesn't support context passing.
	// The MCP client has its own timeout, but this provides an additional safeguard.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := mcp.GetPromptMessages(ctx, cfg, clientID, promptID, args)
	if err != nil {
		return "", err
	}
	return strings.Join(result, " "), nil
}

package commands

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestLoadFromSource_NonExistentDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Empty(t, cmds)

	// directory must NOT have been created
	_, statErr := os.Stat(dir)
	require.True(t, os.IsNotExist(statErr))
}

func TestLoadFromSource_ExistingDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.md"), []byte("say hello"), 0o644))

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "user:hello", cmds[0].ID)
	require.Equal(t, "say hello", cmds[0].Content)
}

func TestLoadCommand_Frontmatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := "---\n" +
		"description: Reviews a pull request\n" +
		"argument-hint: \"[pr-number]\"\n" +
		"allowed-tools:\n" +
		"  - Bash(git status:*)\n" +
		"  - Bash(gh pr view:*)\n" +
		"---\n" +
		"Review PR $PR_NUMBER please.\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "review.md"), []byte(src), 0o644))

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Len(t, cmds, 1)

	cmd := cmds[0]
	require.Equal(t, "user:review", cmd.ID)
	require.Equal(t, "Reviews a pull request", cmd.Description)
	require.Equal(t, "[pr-number]", cmd.ArgumentHint)
	require.Equal(t, []string{"Bash(git status:*)", "Bash(gh pr view:*)"}, cmd.AllowedTools)
	// The frontmatter is stripped from the prompt body.
	require.Equal(t, "Review PR $PR_NUMBER please.\n", cmd.Content)
	// Arguments are extracted from the body only.
	require.Equal(t, []Argument{{ID: "PR_NUMBER", Title: "PR_NUMBER", Required: true}}, cmd.Arguments)
}

func TestLoadCommand_FrontmatterCommaSeparatedAllowedTools(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := "---\nallowed-tools: Bash(git status:*), Bash(git log:*)\n---\nbody\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.md"), []byte(src), 0o644))

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, []string{"Bash(git status:*)", "Bash(git log:*)"}, cmds[0].AllowedTools)
}

func TestLoadCommand_NoFrontmatterByteIdentical(t *testing.T) {
	t.Parallel()

	for name, src := range map[string]string{
		"plain":               "say hello to $NAME\n",
		"crlf":                "line one\r\nline two\r\n",
		"leading blank lines": "\n\n  \nreal content\n",
		"dashes mid-file":     "intro\n---\nnot frontmatter\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.md"), []byte(src), 0o644))

			cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
			require.NoError(t, err)
			require.Len(t, cmds, 1)
			// Byte-identical to the file: no normalization, no stripping.
			require.Equal(t, src, cmds[0].Content)
			require.Empty(t, cmds[0].Description)
			require.Empty(t, cmds[0].ArgumentHint)
			require.Empty(t, cmds[0].AllowedTools)
		})
	}
}

func TestLoadCommand_UnknownFrontmatterKeysIgnored(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := "---\ndescription: Works\nmodel: claude-opus\nfuture-key: whatever\n---\nbody text\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.md"), []byte(src), 0o644))

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "Works", cmds[0].Description)
	require.Equal(t, "body text\n", cmds[0].Content)
}

func TestLoadCommand_MalformedFrontmatterFallsBack(t *testing.T) {
	t.Parallel()

	for name, src := range map[string]string{
		"unclosed":     "---\ndescription: never closed\nbody\n",
		"invalid yaml": "---\ndescription: [unbalanced\n---\nbody\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.md"), []byte(src), 0o644))

			cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
			require.NoError(t, err)
			require.Len(t, cmds, 1)
			// A broken header never hides the command: the whole file is
			// the body and no metadata is set.
			require.Equal(t, src, cmds[0].Content)
			require.Empty(t, cmds[0].Description)
		})
	}
}

func TestLoadAll_MixedSources(t *testing.T) {
	t.Parallel()

	existing := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(existing, "cmd.md"), []byte("content"), 0o644))

	missing := filepath.Join(t.TempDir(), "nope")

	cmds, err := loadAll([]commandSource{
		{path: existing, prefix: userCommandPrefix},
		{path: missing, prefix: projectCommandPrefix},
	})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "user:cmd", cmds[0].ID)
}

func TestFromSkillCatalog_UserInvocableOnly(t *testing.T) {
	t.Parallel()

	cmds := FromSkillCatalog([]skills.CatalogEntry{
		{
			ID:            "/skills/on/SKILL.md",
			Name:          "on",
			Description:   "Enabled.",
			Label:         "user:on",
			UserInvocable: true,
		},
		{
			ID:            "/skills/off/SKILL.md",
			Name:          "off",
			Description:   "Not invocable.",
			Label:         "user:off",
			UserInvocable: false,
		},
	})

	require.Len(t, cmds, 1)
	require.Equal(t, "user:on", cmds[0].ID)
	require.Equal(t, "user:on", cmds[0].Name)
	require.Equal(t, "on", cmds[0].Skill.Name)
	require.Equal(t, "Enabled.", cmds[0].Skill.Description)
	require.Equal(t, "/skills/on/SKILL.md", cmds[0].Skill.SkillFilePath)
}

func TestFromSkillCatalog_UsesDiscoveredSymlinkedSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires special privileges on Windows")
	}
	t.Parallel()

	root := t.TempDir()
	targetParent := t.TempDir()
	targetSkillDir := filepath.Join(targetParent, "linked-skill")
	require.NoError(t, os.MkdirAll(targetSkillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(targetSkillDir, skills.SkillFileName),
		[]byte("---\nname: linked-skill\ndescription: Symlinked.\nuser-invocable: true\n---\nUse me.\n"),
		0o644,
	))

	link := filepath.Join(root, "linked-skill")
	require.NoError(t, os.Symlink(targetSkillDir, link))

	_, activeSkills, _ := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths: []string{root},
	})
	entries := skills.Catalog(activeSkills, []string{root}, "")
	cmds := FromSkillCatalog(entries)

	require.Len(t, cmds, 1)
	require.Equal(t, "user:linked-skill", cmds[0].ID)
	require.Equal(t, "linked-skill", cmds[0].Skill.Name)
	require.Equal(t, filepath.Join(link, skills.SkillFileName), cmds[0].Skill.SkillFilePath)
}

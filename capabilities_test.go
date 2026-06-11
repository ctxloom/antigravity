package antigravity

import (
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxloom/shared/agent"
)

// TestWriteCommandFiles_SkipsTraversalNames verifies skill names from bundle
// content (potentially remote) cannot write outside .agents/skills/: absolute
// and ".."-bearing names are skipped, while plain and nested names still land.
func TestWriteCommandFiles_SkipsTraversalNames(t *testing.T) {
	fs := afero.NewMemMapFs()
	cmds := []agent.CommandExport{
		{Name: "../escape", Content: "evil", Enabled: true},
		{Name: "/abs/path", Content: "evil", Enabled: true},
		{Name: "a/../../b", Content: "evil", Enabled: true},
		{Name: "good", Content: "fine", Enabled: true},
		{Name: "group/cmd", Content: "nested fine", Enabled: true},
	}
	require.NoError(t, WriteCommandFiles("/work", cmds, agent.WithCommandFS(fs)))

	skillsDir := filepath.Join("/work", ".agents", "skills")
	for _, p := range []string{
		filepath.Join(skillsDir, "good.md"),
		filepath.Join(skillsDir, "group", "cmd.md"),
	} {
		exists, err := afero.Exists(fs, p)
		require.NoError(t, err)
		assert.True(t, exists, "legit skill %s must be written", p)
	}
	for _, p := range []string{
		"/work/.agents/escape.md", // ../escape from skills dir
		"/abs/path.md",
		"/work/b.md", // a/../../b from skills dir
	} {
		exists, err := afero.Exists(fs, p)
		require.NoError(t, err)
		assert.False(t, exists, "traversal name must not write %s", p)
	}

	manifest, err := afero.ReadFile(fs, filepath.Join(skillsDir, antigravityManifest))
	require.NoError(t, err)
	assert.Contains(t, string(manifest), "good.md")
	assert.Contains(t, string(manifest), "group/cmd.md")
	assert.NotContains(t, string(manifest), "escape")
	assert.NotContains(t, string(manifest), "abs")
}

// TestWriteCommandFiles_ManifestTraversalLinesNotDeleted verifies the
// pre-write manifest cleanup never follows a doctored manifest line outside
// the skills tree, while legit stale entries are still removed.
func TestWriteCommandFiles_ManifestTraversalLinesNotDeleted(t *testing.T) {
	fs := afero.NewMemMapFs()
	skillsDir := filepath.Join("/work", ".agents", "skills")
	require.NoError(t, afero.WriteFile(fs, "/work/victim.txt", []byte("keep"), 0644))
	require.NoError(t, afero.WriteFile(fs, filepath.Join(skillsDir, "old.md"), []byte("stale"), 0644))
	manifest := "../../victim.txt\n/work/victim.txt\nold.md\n"
	require.NoError(t, afero.WriteFile(fs, filepath.Join(skillsDir, antigravityManifest), []byte(manifest), 0644))

	cmds := []agent.CommandExport{{Name: "new", Content: "x", Enabled: true}}
	require.NoError(t, WriteCommandFiles("/work", cmds, agent.WithCommandFS(fs)))

	exists, err := afero.Exists(fs, "/work/victim.txt")
	require.NoError(t, err)
	assert.True(t, exists, "manifest traversal line must not delete outside the skills tree")
	exists, err = afero.Exists(fs, filepath.Join(skillsDir, "old.md"))
	require.NoError(t, err)
	assert.False(t, exists, "legit stale manifest entry still removed")
}

package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxloom/shared/wire"
)

func TestAntigravityHookWriter_Paths(t *testing.T) {
	writer := &AntigravityHookWriter{}

	assert.Equal(t, "/project/.agents/hooks.json", writer.SettingsPath("/project"))
	assert.Equal(t, "/project/.agents/hooks.json", writer.HooksPath("/project"))
	assert.Equal(t, "/project/.agents/mcp_config.json", writer.MCPConfigPath("/project"))
}

func TestAntigravityHookWriter_WriteHooks(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}

	cfg := &wire.HooksConfig{
		Unified: wire.UnifiedHooks{
			PreTool:  []wire.Hook{{Command: "./pre-tool.sh", Matcher: "run_command"}},
			PostTool: []wire.Hook{{Command: "./post-tool.sh", Matcher: "write_to_file"}},
		},
	}

	require.NoError(t, writer.WriteHooks(cfg, "/project"))

	data, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)

	var settings map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &settings))
	require.Contains(t, settings, "hooks")

	var hooks map[string][]antigravityHookGroup
	require.NoError(t, json.Unmarshal(settings["hooks"], &hooks))
	require.Contains(t, hooks, "PreToolUse")
	require.Contains(t, hooks, "PostToolUse")
	require.Len(t, hooks["PreToolUse"], 1)
	assert.Equal(t, "run_command", hooks["PreToolUse"][0].Matcher)
	require.Len(t, hooks["PreToolUse"][0].Hooks, 1)
	assert.Equal(t, "command", hooks["PreToolUse"][0].Hooks[0].Type)
	assert.Equal(t, "./pre-tool.sh", hooks["PreToolUse"][0].Hooks[0].Command)
	assert.Equal(t, antigravityCtxloomHookName, hooks["PreToolUse"][0].Hooks[0].Name)
	assert.Zero(t, hooks["PreToolUse"][0].Hooks[0].Timeout, "timeout is never written (unit unverified in agy)")
}

// TestAntigravityHookWriter_PreShellPostFileEdit verifies the unified
// PreShell / PostFileEdit hooks map onto PreToolUse/PostToolUse with the
// default agy tool-name matchers.
func TestAntigravityHookWriter_PreShellPostFileEdit(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreShell:     []wire.Hook{{Command: "ctxloom hook pre-shell"}},
		PostFileEdit: []wire.Hook{{Command: "ctxloom hook post-edit"}},
	}}
	require.NoError(t, writer.WriteHooks(cfg, "/project"))

	hooks := readHooks(t, fs)
	require.Contains(t, hooks, "PreToolUse")
	require.Contains(t, hooks, "PostToolUse")
	assert.Equal(t, antigravityShellMatcher, hooks["PreToolUse"][0].Matcher)
	assert.Equal(t, antigravityFileEditMatcher, hooks["PostToolUse"][0].Matcher)
}

// TestAntigravityHookWriter_SessionEventsPassThrough verifies SessionStart /
// SessionEnd entries are written verbatim (agy v1.0.7 silently skips them; a
// future agy may load them).
func TestAntigravityHookWriter_SessionEventsPassThrough(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		SessionStart: []wire.Hook{{Command: "ctxloom session bind"}},
		SessionEnd:   []wire.Hook{{Command: "ctxloom session end"}},
	}}
	require.NoError(t, writer.WriteHooks(cfg, "/project"))

	hooks := readHooks(t, fs)
	assert.Contains(t, hooks, "SessionStart")
	assert.Contains(t, hooks, "SessionEnd")
}

// TestAntigravityHookWriter_PreservesUserEntries verifies user hooks, unknown
// top-level fields, and user MCP servers (including remote serverUrl entries
// with fields ctxloom does not model) survive a write/remove cycle.
func TestAntigravityHookWriter_PreservesUserEntries(t *testing.T) {
	fs := afero.NewMemMapFs()
	userHooks := `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "run_command", "hooks": [{"type": "command", "command": "/usr/local/bin/my-guard", "timeout": 30}]}
			]
		},
		"futureSetting": {"nested": true}
	}`
	userMCP := `{
		"mcpServers": {
			"remote-thing": {"serverUrl": "https://example.com/mcp", "headers": {"X-Auth": "secret"}}
		}
	}`
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/hooks.json", []byte(userHooks), 0644))
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/mcp_config.json", []byte(userMCP), 0644))

	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreShell: []wire.Hook{{Command: "ctxloom hook pre-shell"}},
	}}
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	// User hook entry survives, with its timeout intact, alongside ctxloom's.
	hooks := readHooks(t, fs)
	require.Len(t, hooks["PreToolUse"], 2)
	var userEntry *antigravityHookEntry
	for _, g := range hooks["PreToolUse"] {
		for i := range g.Hooks {
			if g.Hooks[i].Command == "/usr/local/bin/my-guard" {
				userEntry = &g.Hooks[i]
			}
		}
	}
	require.NotNil(t, userEntry, "user hook entry preserved")
	assert.Equal(t, 30, userEntry.Timeout)

	// Unknown top-level field survives.
	data, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &top))
	assert.Contains(t, top, "futureSetting")

	// Remote MCP server survives raw, headers and all, next to ctxloom's.
	mcpData, err := afero.ReadFile(fs, "/project/.agents/mcp_config.json")
	require.NoError(t, err)
	var mcpTop map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mcpData, &mcpTop))
	require.Contains(t, mcpTop["mcpServers"], "remote-thing")
	assert.Contains(t, string(mcpTop["mcpServers"]["remote-thing"]), "serverUrl")
	assert.Contains(t, string(mcpTop["mcpServers"]["remote-thing"]), "X-Auth")
	require.Contains(t, mcpTop["mcpServers"], AppMCPServerName)

	// Remove: ctxloom entries gone, user entries intact.
	require.NoError(t, writer.RemoveSettings("/project"))
	hooks = readHooks(t, fs)
	require.Len(t, hooks["PreToolUse"], 1)
	assert.Equal(t, "/usr/local/bin/my-guard", hooks["PreToolUse"][0].Hooks[0].Command)

	mcpData, err = afero.ReadFile(fs, "/project/.agents/mcp_config.json")
	require.NoError(t, err)
	mcpTop = nil
	require.NoError(t, json.Unmarshal(mcpData, &mcpTop))
	assert.Contains(t, mcpTop["mcpServers"], "remote-thing")
	assert.NotContains(t, mcpTop["mcpServers"], AppMCPServerName)
}

// TestAntigravityHookWriter_Idempotent verifies double-apply produces
// identical files (reconcile, not append).
func TestAntigravityHookWriter_Idempotent(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreShell: []wire.Hook{{Command: "ctxloom hook pre-shell"}},
	}}

	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))
	first, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	firstMCP, err := afero.ReadFile(fs, "/project/.agents/mcp_config.json")
	require.NoError(t, err)

	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))
	second, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	secondMCP, err := afero.ReadFile(fs, "/project/.agents/mcp_config.json")
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
	assert.Equal(t, string(firstMCP), string(secondMCP))
}

// TestAntigravityHookWriter_FaultTolerantLoad verifies corrupt files do not
// block hook application (the CLAUDE.md fault-tolerance contract).
func TestAntigravityHookWriter_FaultTolerantLoad(t *testing.T) {
	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/hooks.json", []byte("{not valid json"), 0644))
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/mcp_config.json", []byte("also broken"), 0644))

	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreTool: []wire.Hook{{Command: "ctxloom hook pre-tool"}},
	}}
	require.NoError(t, writer.WriteHooks(cfg, "/project"))

	hooks := readHooks(t, fs)
	assert.Contains(t, hooks, "PreToolUse")
}

// TestAntigravityHookWriter_EmptyMCPFileTolerated verifies the zero-byte
// mcp_config.json files agy itself creates load as empty.
func TestAntigravityHookWriter_EmptyMCPFileTolerated(t *testing.T) {
	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/mcp_config.json", []byte(""), 0644))

	writer := &AntigravityHookWriter{FS: fs}
	require.NoError(t, writer.WriteSettings(&wire.HooksConfig{}, nil, nil, "/project"))

	mcpData, err := afero.ReadFile(fs, "/project/.agents/mcp_config.json")
	require.NoError(t, err)
	var mcpTop map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mcpData, &mcpTop))
	assert.Contains(t, mcpTop["mcpServers"], AppMCPServerName)
}

// TestAntigravityHookWriter_RemoveLeavesAbsentFilesAbsent verifies uninstall
// never creates files.
func TestAntigravityHookWriter_RemoveLeavesAbsentFilesAbsent(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	require.NoError(t, writer.RemoveSettings("/project"))

	for _, p := range []string{"/project/.agents/hooks.json", "/project/.agents/mcp_config.json"} {
		exists, err := afero.Exists(fs, p)
		require.NoError(t, err)
		assert.False(t, exists, p)
	}
}

func TestAntigravityHookWriter_Status(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}

	status, err := writer.Status("/project")
	require.NoError(t, err)
	assert.False(t, status.Wired())

	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreTool: []wire.Hook{{Command: "ctxloom hook pre-tool"}},
	}}
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	status, err = writer.Status("/project")
	require.NoError(t, err)
	assert.True(t, status.SettingsExists)
	assert.True(t, status.HooksPresent)
	assert.True(t, status.MCPPresent)
	assert.True(t, status.Wired())

	require.NoError(t, writer.RemoveSettings("/project"))
	status, err = writer.Status("/project")
	require.NoError(t, err)
	assert.False(t, status.HooksPresent)
	assert.False(t, status.MCPPresent)
}

// readHooks unmarshals the hooks map from the written hooks.json.
func readHooks(t *testing.T, fs afero.Fs) map[string][]antigravityHookGroup {
	t.Helper()
	data, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &top))
	require.Contains(t, top, "hooks")
	var hooks map[string][]antigravityHookGroup
	require.NoError(t, json.Unmarshal(top["hooks"], &hooks))
	return hooks
}

package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxloom/shared/wire"
)

func TestAntigravityHookWriter_Paths(t *testing.T) {
	writer := &AntigravityHookWriter{}

	assert.Equal(t, "/project/.agents/hooks.json", writer.SettingsPath("/project"))
	assert.Equal(t, "/project/.agents/mcp_config.json", writer.MCPConfigPath("/project"))
}

func TestAntigravityHookWriter_WriteSettingsHooks(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}

	cfg := &wire.HooksConfig{
		Unified: wire.UnifiedHooks{
			PreTool:  []wire.Hook{{Command: "./pre-tool.sh", Matcher: "run_command"}},
			PostTool: []wire.Hook{{Command: "./post-tool.sh", Matcher: "write_to_file"}},
		},
	}

	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

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
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	hooks := readHooks(t, fs)
	require.Contains(t, hooks, "PreToolUse")
	require.Contains(t, hooks, "PostToolUse")
	assert.Equal(t, antigravityShellMatcher, hooks["PreToolUse"][0].Matcher)
	assert.Equal(t, antigravityFileEditMatcher, hooks["PostToolUse"][0].Matcher)
}

// TestAntigravityHookWriter_MatchersPinAgyToolVocabulary pins the default
// matchers to the verified agy tool names; the constants are built from the
// hooks_wire.go vocabulary, so a wire-constant edit shows up here.
func TestAntigravityHookWriter_MatchersPinAgyToolVocabulary(t *testing.T) {
	assert.Equal(t, "run_command|execute_command", antigravityShellMatcher)
	assert.Equal(t, "write_to_file|replace_file_content", antigravityFileEditMatcher)
}

// TestAntigravityHookWriter_SkipsNonCommandHooks verifies prompt/agent hooks
// are skipped rather than mangled into dead {"type":"command","command":""}
// entries — agy only executes command hooks. Empty Type is the wire default
// and means command.
func TestAntigravityHookWriter_SkipsNonCommandHooks(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreTool: []wire.Hook{
			{Type: "prompt", Prompt: "be careful"},
			{Type: "agent", Prompt: "review this tool call"},
			{Type: "command", Command: "ctxloom hook pre-tool"},
			{Command: "ctxloom hook pre-tool-default"}, // empty Type = command
		},
	}}
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	hooks := readHooks(t, fs)
	require.Contains(t, hooks, "PreToolUse")
	var commands []string
	for _, g := range hooks["PreToolUse"] {
		for _, e := range g.Hooks {
			commands = append(commands, e.Command)
		}
	}
	assert.ElementsMatch(t, []string{"ctxloom hook pre-tool", "ctxloom hook pre-tool-default"}, commands)

	data, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"command": ""`, "no dead empty-command entries")
}

// TestAntigravityHookWriter_CompanionHookIdempotent verifies exact-duplicate
// suppression for companion-binary hooks: an identical command already
// installed under the same event by a non-ctxloom entry (e.g. ltk registered
// `ltk evaluate` itself, no marker) must not duplicate when ctxloom adds the
// same hook — same semantics as the claude writer's removeExactCommand. A
// user variant of the same binary with different args survives.
func TestAntigravityHookWriter_CompanionHookIdempotent(t *testing.T) {
	fs := afero.NewMemMapFs()
	existing := `{"hooks":{"PreToolUse":[{"matcher":"run_command|execute_command","hooks":[
		{"type":"command","command":"ltk evaluate"},
		{"type":"command","command":"ltk evaluate --config .ltk/config.yaml"}]}]}}`
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/hooks.json", []byte(existing), 0644))

	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreShell: []wire.Hook{{Command: "ltk evaluate"}},
	}}
	for range 3 {
		require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))
	}

	hooks := readHooks(t, fs)
	exact, variant := 0, 0
	for _, g := range hooks["PreToolUse"] {
		for _, e := range g.Hooks {
			switch e.Command {
			case "ltk evaluate":
				exact++
			case "ltk evaluate --config .ltk/config.yaml":
				variant++
			}
		}
	}
	assert.Equal(t, 1, exact, "companion hook must not duplicate across re-applies")
	assert.Equal(t, 1, variant, "user's own variant of the same binary must survive")
}

// TestAntigravityHookWriter_PreservesUnknownGroupAndEntryFields verifies
// fields agy adds later at the group or entry level round-trip a rewrite
// instead of being silently dropped, and that preservation stays
// byte-idempotent across re-applies.
func TestAntigravityHookWriter_PreservesUnknownGroupAndEntryFields(t *testing.T) {
	fs := afero.NewMemMapFs()
	userHooks := `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "run_command", "futureGroupField": {"nested": true}, "hooks": [
					{"type": "command", "command": "/usr/local/bin/my-guard", "timeout": 30, "futureEntryField": [1, 2]}
				]}
			]
		}
	}`
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/hooks.json", []byte(userHooks), 0644))

	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		PreShell: []wire.Hook{{Command: "ctxloom hook pre-shell"}},
	}}
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	first, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	assert.Contains(t, string(first), `"futureGroupField"`)
	assert.Contains(t, string(first), `"futureEntryField"`)
	// Known fields keep their shape next to the preserved unknowns.
	assert.Contains(t, string(first), `"command": "/usr/local/bin/my-guard"`)
	assert.Contains(t, string(first), `"timeout": 30`)

	// Re-apply: byte-identical (reconcile, not append; extras merge stably).
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))
	second, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))

	// Remove: ctxloom entry gone, user entry with unknown fields intact.
	require.NoError(t, writer.RemoveSettings("/project"))
	final, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	assert.Contains(t, string(final), `"futureGroupField"`)
	assert.Contains(t, string(final), `"futureEntryField"`)
	assert.NotContains(t, string(final), "ctxloom hook pre-shell")
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
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	hooks := readHooks(t, fs)
	assert.Contains(t, hooks, "SessionStart")
	assert.Contains(t, hooks, "SessionEnd")
}

// TestAntigravityHookWriter_PreToolFallbackDiverts verifies a session_start
// hook declared pre_tool_fallback registers under PreToolUse (the only event
// where it can fire on agy) and NOT under SessionStart — diverted, not
// duplicated, so a future agy adding SessionStart can't double-fire it.
func TestAntigravityHookWriter_PreToolFallbackDiverts(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		SessionStart: []wire.Hook{{Command: "ctxloom hook session-bind", PreToolFallback: true}},
	}}
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	hooks := readHooks(t, fs)
	assert.NotContains(t, hooks, "SessionStart")
	require.Contains(t, hooks, "PreToolUse")
	require.Len(t, hooks["PreToolUse"], 1)
	assert.Equal(t, ".*", hooks["PreToolUse"][0].Matcher)
	assert.Equal(t, "ctxloom hook session-bind", hooks["PreToolUse"][0].Hooks[0].Command)
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

// TestAntigravityHookWriter_MCPLedgerReconcilesRenamesAndRemovals pins the
// managed-MCP ownership ledger: agy rejects in-file marker fields (verified —
// they hang headless agy), so managed names live in the .ctxloom-mcp-managed
// sidecar. A server renamed or removed from config between applies must not
// linger in mcp_config.json — a stale stdio entry permanently hangs agy.
func TestAntigravityHookWriter_MCPLedgerReconcilesRenamesAndRemovals(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	userServer := `{"mcpServers": {"user-thing": {"command": "user-bin"}}}`
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/mcp_config.json", []byte(userServer), 0644))

	mcp := &wire.MCPConfig{Servers: map[string]wire.MCPServer{
		"old-name": {Command: "tool-v1"},
	}}
	require.NoError(t, writer.WriteSettings(&wire.HooksConfig{}, mcp, nil, "/project"))

	servers := readMCPServers(t, fs)
	assert.Contains(t, servers, "old-name")
	assert.Contains(t, servers, "user-thing")

	// Rename the managed server in config: the old entry must disappear.
	mcp = &wire.MCPConfig{Servers: map[string]wire.MCPServer{
		"new-name": {Command: "tool-v2"},
	}}
	require.NoError(t, writer.WriteSettings(&wire.HooksConfig{}, mcp, nil, "/project"))

	servers = readMCPServers(t, fs)
	assert.NotContains(t, servers, "old-name", "renamed managed server must not linger")
	assert.Contains(t, servers, "new-name")
	assert.Contains(t, servers, "user-thing", "user server never touched")

	// Uninstall: every managed server (and the ledger) goes; user entry stays.
	require.NoError(t, writer.RemoveSettings("/project"))
	servers = readMCPServers(t, fs)
	assert.NotContains(t, servers, "new-name")
	assert.NotContains(t, servers, AppMCPServerName)
	assert.Contains(t, servers, "user-thing")
	ledgerExists, _ := afero.Exists(fs, "/project/.agents/.ctxloom-mcp-managed")
	assert.False(t, ledgerExists, "ledger removed with the last managed server")
}

// readMCPServers unmarshals the server map from the written mcp_config.json.
func readMCPServers(t *testing.T, fs afero.Fs) map[string]json.RawMessage {
	t.Helper()
	data, err := afero.ReadFile(fs, "/project/.agents/mcp_config.json")
	require.NoError(t, err)
	var top map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &top))
	return top["mcpServers"]
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
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

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

// writeContextFixture writes a content-addressed context file the way the
// context provider does, so the writer's AGENTS.md materialization can read it.
func writeContextFixture(t *testing.T, fs afero.Fs, hash, content string) {
	t.Helper()
	path := "/project/.ctxloom/cache/context/" + hash + ".md"
	require.NoError(t, afero.WriteFile(fs, path, []byte(content), 0644))
}

// contextHooksCfg builds a hooks config carrying the context-injection hook
// the way agent.NewContextInjectionHooks marks it.
func contextHooksCfg(hash string) *wire.HooksConfig {
	return &wire.HooksConfig{Unified: wire.UnifiedHooks{
		SessionStart: []wire.Hook{{Command: "ctxloom hook inject-context " + hash, ContextHash: hash}},
	}}
}

// TestAntigravityHookWriter_MaterializesContextIntoAgentsMD pins the agy
// context-delivery channel: agy fires no SessionStart hooks, so the
// context-injection hook must NOT land in hooks.json — the assembled context
// is written into .agents/AGENTS.md (which agy reads, verified) instead.
func TestAntigravityHookWriter_MaterializesContextIntoAgentsMD(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	writeContextFixture(t, fs, "abc123", "# Project Context\nthe secret color is vermilion")

	require.NoError(t, writer.WriteSettings(contextHooksCfg("abc123"), nil, nil, "/project"))

	data, err := afero.ReadFile(fs, "/project/.agents/AGENTS.md")
	require.NoError(t, err)
	assert.Contains(t, string(data), managedContextBegin)
	assert.Contains(t, string(data), "the secret color is vermilion")
	assert.Contains(t, string(data), managedContextEnd)

	// The injection hook must not appear as a dead hooks.json entry.
	hooksData, err := afero.ReadFile(fs, "/project/.agents/hooks.json")
	require.NoError(t, err)
	assert.NotContains(t, string(hooksData), "inject-context")
}

// TestAntigravityHookWriter_ContextReconcileAndUserContent verifies the
// managed section is replaced on re-apply, removed when no context hook is
// present, and that user-authored AGENTS.md content outside the markers
// survives every step.
func TestAntigravityHookWriter_ContextReconcileAndUserContent(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	require.NoError(t, afero.WriteFile(fs, "/project/.agents/AGENTS.md", []byte("# My rules\nalways frobnicate\n"), 0644))
	writeContextFixture(t, fs, "hash1", "context one")
	writeContextFixture(t, fs, "hash2", "context two")

	require.NoError(t, writer.WriteSettings(contextHooksCfg("hash1"), nil, nil, "/project"))
	data, _ := afero.ReadFile(fs, "/project/.agents/AGENTS.md")
	assert.Contains(t, string(data), "always frobnicate")
	assert.Contains(t, string(data), "context one")

	// Re-apply with a new hash: section replaced, not appended.
	require.NoError(t, writer.WriteSettings(contextHooksCfg("hash2"), nil, nil, "/project"))
	data, _ = afero.ReadFile(fs, "/project/.agents/AGENTS.md")
	assert.Contains(t, string(data), "context two")
	assert.NotContains(t, string(data), "context one")
	assert.Contains(t, string(data), "always frobnicate")

	// Apply without a context hook: section removed, user content intact.
	require.NoError(t, writer.WriteSettings(&wire.HooksConfig{}, nil, nil, "/project"))
	data, _ = afero.ReadFile(fs, "/project/.agents/AGENTS.md")
	assert.NotContains(t, string(data), managedContextBegin)
	assert.Contains(t, string(data), "always frobnicate")
}

// TestAntigravityHookWriter_ContextRemovedWithSettings verifies RemoveSettings
// strips the managed section and deletes a file that was wholly ctxloom's.
func TestAntigravityHookWriter_ContextRemovedWithSettings(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	writeContextFixture(t, fs, "abc123", "managed context only")

	require.NoError(t, writer.WriteSettings(contextHooksCfg("abc123"), nil, nil, "/project"))
	status, err := writer.Status("/project")
	require.NoError(t, err)
	assert.True(t, status.HooksPresent, "managed context section counts as wired")

	require.NoError(t, writer.RemoveSettings("/project"))
	exists, err := afero.Exists(fs, "/project/.agents/AGENTS.md")
	require.NoError(t, err)
	assert.False(t, exists, "a wholly managed AGENTS.md is removed, not left empty")
}

// TestAntigravityHookWriter_ChunkedContextMaterializesOnce verifies N chunked
// injection hooks (one hash) yield a single managed section — chunking is a
// hook-harness workaround that does not apply to file delivery.
func TestAntigravityHookWriter_ChunkedContextMaterializesOnce(t *testing.T) {
	fs := afero.NewMemMapFs()
	writer := &AntigravityHookWriter{FS: fs}
	writeContextFixture(t, fs, "bighash", "whole content")

	cfg := &wire.HooksConfig{Unified: wire.UnifiedHooks{
		SessionStart: []wire.Hook{
			{Command: "ctxloom hook inject-context --part 1 --of 2 bighash", ContextHash: "bighash"},
			{Command: "ctxloom hook inject-context --part 2 --of 2 bighash", ContextHash: "bighash"},
		},
	}}
	require.NoError(t, writer.WriteSettings(cfg, nil, nil, "/project"))

	data, err := afero.ReadFile(fs, "/project/.agents/AGENTS.md")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), managedContextBegin))
	assert.Equal(t, 1, strings.Count(string(data), "whole content"))
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

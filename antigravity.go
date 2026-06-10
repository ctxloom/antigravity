// Package antigravity is ctxloom's Antigravity CLI (agy) agent: the
// settings/hooks writer that implements agent.SettingsWriter, plus the launch
// backend and the PreToolUse hook wire types other tools (ltk) consume.
//
// Antigravity splits workspace configuration across two files under .agents/:
// hooks.json (lifecycle hooks, Claude-style nested shape with PreToolUse /
// PostToolUse / Stop events) and mcp_config.json (MCP servers). Behavior here
// follows the verified agy v1.0.7 contract — see the wire types in
// hooks_wire.go for the verified payload/decision shapes.
package antigravity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/afero"

	"github.com/ctxloom/shared/agent"
	"github.com/ctxloom/shared/wire"
)

// NewWriter constructs the Antigravity CLI settings writer.
func NewWriter(o agent.SettingsOptions) agent.SettingsWriter {
	return &AntigravityHookWriter{FS: o.FS}
}

// AppMCPServerName is the name under which ctxloom's own MCP server is
// registered in Antigravity's mcp_config.json.
const AppMCPServerName = agent.MCPServerName

// AgentsDir is the workspace directory Antigravity reads configuration from.
// It is shared territory: agy also writes subagent scratch space under it, and
// other tools use .agents/ too — ctxloom only manages hooks.json and
// mcp_config.json inside it.
const AgentsDir = ".agents"

// antigravityCtxloomHookName marks a hook entry as ctxloom-managed in the
// durable (serialized) name field. agy tolerates and preserves the field
// (verified v1.0.7: a named entry loads and fires, reported as a "named hook").
const antigravityCtxloomHookName = "ctxloom-managed"

// AntigravityHookWriter writes hooks to Antigravity CLI's .agents/hooks.json
// and MCP servers to .agents/mcp_config.json.
type AntigravityHookWriter struct {
	// FS is the filesystem to use. If nil, the real OS filesystem is used.
	FS afero.Fs
}

// getFS returns the filesystem to use, defaulting to the OS filesystem.
func (w *AntigravityHookWriter) getFS() afero.Fs {
	return agent.GetFS(w.FS)
}

// HooksPath returns the path to Antigravity's project-level hooks.json file.
// There is no usable global location in agy v1.0.7: ~/.gemini/antigravity-cli/
// hooks.json is silently ignored, and a hooks.json under ~/.gemini/ or
// ~/.gemini/config/ hangs headless agy before any hook executes.
func (w *AntigravityHookWriter) HooksPath(projectDir string) string {
	return filepath.Join(projectDir, AgentsDir, "hooks.json")
}

// SettingsPath is an alias for HooksPath: hooks.json is the settings file
// ctxloom manages for Antigravity.
func (w *AntigravityHookWriter) SettingsPath(projectDir string) string {
	return w.HooksPath(projectDir)
}

// MCPConfigPath returns the path to Antigravity's project-level
// mcp_config.json file. agy reads MCP servers from this dedicated file, not
// from hooks.json or settings.json.
func (w *AntigravityHookWriter) MCPConfigPath(projectDir string) string {
	return filepath.Join(projectDir, AgentsDir, "mcp_config.json")
}

// antigravityHooksFile represents the structure of .agents/hooks.json.
type antigravityHooksFile struct {
	Hooks map[string][]antigravityHookGroup `json:"hooks,omitempty"`
	// Preserve other top-level fields.
	Other map[string]json.RawMessage `json:"-"`
}

// antigravityHookGroup is one entry in a hook event array: a matcher (a regex
// over agy tool names, e.g. "run_command|write_to_file") plus the command
// hooks that fire for it. This nested event → group → hooks[] shape is the
// verified agy schema.
type antigravityHookGroup struct {
	Matcher string                 `json:"matcher,omitempty"`
	Hooks   []antigravityHookEntry `json:"hooks"`
}

// antigravityHookEntry is a single command hook. agy requires type:"command".
// name is a durable field agy preserves, used to identify ctxloom-managed
// entries for clean removal. The timeout field is deliberately never written:
// agy v1.0.7 does not document its unit, and a seconds/milliseconds mismatch
// would silently break hooks — agy's own default applies instead. An existing
// user-set timeout round-trips untouched.
type antigravityHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Name    string `json:"name,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

// antigravityMCPFile represents the structure of .agents/mcp_config.json.
// Server entries are kept raw so fields ctxloom doesn't model (serverUrl for
// remote servers, headers, future additions) survive a rewrite byte-for-byte.
type antigravityMCPFile struct {
	Servers map[string]json.RawMessage `json:"mcpServers,omitempty"`
	Other   map[string]json.RawMessage `json:"-"`
}

// antigravityMCPServer is the stdio server shape ctxloom writes. Remote
// servers (serverUrl) are user-authored and pass through raw.
type antigravityMCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// WriteSettings implements SettingsWriter for Antigravity CLI: hooks to
// hooks.json, MCP servers to mcp_config.json.
//
// CAUTION carried from live verification: a registered stdio MCP server whose
// process cannot complete the MCP handshake hangs headless agy. ctxloom only
// registers real MCP servers, but never write placeholder/dummy entries here.
func (w *AntigravityHookWriter) WriteSettings(hooks *wire.HooksConfig, mcp *wire.MCPConfig, bundleMCP map[string]wire.MCPServer, projectDir string) error {
	if hooks == nil {
		hooks = &wire.HooksConfig{}
	}

	fs := w.getFS()
	hooksPath := w.HooksPath(projectDir)

	if err := fs.MkdirAll(filepath.Dir(hooksPath), 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", AgentsDir, err)
	}

	hooksFile, err := w.loadHooksFile(hooksPath)
	if err != nil {
		return fmt.Errorf("failed to load existing hooks.json: %w", err)
	}

	w.removeCtxloomHooks(hooksFile)
	w.addUnifiedHooks(hooksFile, hooks.Unified)
	if backendHooks, ok := hooks.Plugins["antigravity"]; ok {
		w.addBackendHooks(hooksFile, backendHooks)
	}

	if err := w.saveHooksFile(hooksPath, hooksFile); err != nil {
		return err
	}

	return w.writeMCPConfig(projectDir, mcp, bundleMCP)
}

// WriteHooks implements HookWriter for Antigravity CLI (backwards compatible).
func (w *AntigravityHookWriter) WriteHooks(cfg *wire.HooksConfig, projectDir string) error {
	return w.WriteSettings(cfg, nil, nil, projectDir)
}

// loadHooksFile loads existing hooks.json or returns an empty structure.
// Parse failures warn and continue with empty settings (fault-tolerance
// contract: ctxloom never blocks launch on a schema change).
func (w *AntigravityHookWriter) loadHooksFile(path string) (*antigravityHooksFile, error) {
	hf := &antigravityHooksFile{
		Hooks: make(map[string][]antigravityHookGroup),
		Other: make(map[string]json.RawMessage),
	}

	fs := w.getFS()
	data, err := afero.ReadFile(fs, path)
	if err != nil {
		if os.IsNotExist(err) {
			return hf, nil
		}
		return nil, err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		w.warn("failed to parse %s/hooks.json (schema may have changed): %v - ctxloom hooks will be added but existing entries may not be preserved", AgentsDir, err)
		return hf, nil
	}

	if hooksRaw, ok := raw["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &hf.Hooks); err != nil {
			w.warn("failed to parse hooks in hooks.json: %v - existing hooks may not be preserved", err)
		}
		delete(raw, "hooks")
	}

	hf.Other = raw
	return hf, nil
}

// warn outputs a warning message to stderr.
func (w *AntigravityHookWriter) warn(format string, args ...interface{}) {
	agent.Warn(format, args...)
}

// saveHooksFile writes hooks.json back atomically.
func (w *AntigravityHookWriter) saveHooksFile(path string, hf *antigravityHooksFile) error {
	output := make(map[string]interface{})
	for k, v := range hf.Other {
		var val interface{}
		if err := json.Unmarshal(v, &val); err != nil {
			w.warn("failed to preserve hooks.json field %q: %v", k, err)
			continue
		}
		output[k] = val
	}

	if len(hf.Hooks) > 0 {
		output["hooks"] = hf.Hooks
	}

	data, err := agent.CanonicalJSON(output)
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	return agent.AtomicWriteFile(w.getFS(), path, data, "hooks.json")
}

// removeCtxloomHooks removes ctxloom-managed hooks, descending the
// event → group → hooks[] shape. An entry is ctxloom-managed if its durable
// name marker matches, or (defensively) its command's executable token is
// ctxloom. Groups left with no entries are dropped, and events left with no
// groups are removed.
func (w *AntigravityHookWriter) removeCtxloomHooks(hf *antigravityHooksFile) {
	for eventName, groups := range hf.Hooks {
		var keptGroups []antigravityHookGroup
		for _, g := range groups {
			var keptEntries []antigravityHookEntry
			for _, e := range g.Hooks {
				if e.Name == antigravityCtxloomHookName || agent.IsManaged(e.Command, "ctxloom") {
					continue
				}
				keptEntries = append(keptEntries, e)
			}
			if len(keptEntries) > 0 {
				g.Hooks = keptEntries
				keptGroups = append(keptGroups, g)
			}
		}
		if len(keptGroups) > 0 {
			hf.Hooks[eventName] = keptGroups
		} else {
			delete(hf.Hooks, eventName)
		}
	}
}

// Antigravity tool-name matchers for the scoped unified events. The matcher is
// a regex over agy tool names (verified: alternation and ".*" both work).
const (
	// antigravityShellMatcher scopes a hook to shell execution tools.
	antigravityShellMatcher = "run_command|execute_command"
	// antigravityFileEditMatcher scopes a hook to file mutation tools.
	antigravityFileEditMatcher = "write_to_file|replace_file_content"
)

// addUnifiedHooks translates unified hooks to Antigravity's events.
//
// agy v1.0.7 loads PreToolUse, PostToolUse, and Stop handlers. SessionStart /
// SessionEnd entries are written through verbatim: agy silently skips events
// it doesn't support (verified — they don't load as handlers and don't error),
// and they light up if a future agy adds the events. Callers that need
// session-start behavior must rely on the context file instead.
func (w *AntigravityHookWriter) addUnifiedHooks(hf *antigravityHooksFile, unified wire.UnifiedHooks) {
	for _, h := range unified.SessionStart {
		w.addHook(hf, "SessionStart", h)
	}
	for _, h := range unified.SessionEnd {
		w.addHook(hf, "SessionEnd", h)
	}
	for _, h := range unified.PreTool {
		w.addHook(hf, "PreToolUse", h)
	}
	for _, h := range unified.PostTool {
		w.addHook(hf, "PostToolUse", h)
	}
	for _, h := range unified.PreShell {
		hook := h
		if hook.Matcher == "" {
			hook.Matcher = antigravityShellMatcher
		}
		w.addHook(hf, "PreToolUse", hook)
	}
	for _, h := range unified.PostFileEdit {
		hook := h
		if hook.Matcher == "" {
			hook.Matcher = antigravityFileEditMatcher
		}
		w.addHook(hf, "PostToolUse", hook)
	}
}

// addBackendHooks adds backend-specific passthrough hooks.
func (w *AntigravityHookWriter) addBackendHooks(hf *antigravityHooksFile, backendHooks wire.BackendHooks) {
	for eventName, hooks := range backendHooks {
		for _, h := range hooks {
			w.addHook(hf, eventName, h)
		}
	}
}

// addHook adds a single hook for the given event in agy's nested
// group → hooks[] shape with type:"command" and the ctxloom name marker.
func (w *AntigravityHookWriter) addHook(hf *antigravityHooksFile, eventName string, h wire.Hook) {
	entry := antigravityHookEntry{
		Type:    "command",
		Command: h.Command,
		Name:    antigravityCtxloomHookName,
	}
	group := antigravityHookGroup{Matcher: h.Matcher, Hooks: []antigravityHookEntry{entry}}
	hf.Hooks[eventName] = append(hf.Hooks[eventName], group)
}

// writeMCPConfig reconciles ctxloom-managed MCP servers into mcp_config.json,
// preserving user-authored entries (including remote serverUrl servers) raw.
func (w *AntigravityHookWriter) writeMCPConfig(projectDir string, mcp *wire.MCPConfig, bundleMCP map[string]wire.MCPServer) error {
	mcpPath := w.MCPConfigPath(projectDir)
	mf, err := w.loadMCPFile(mcpPath)
	if err != nil {
		return fmt.Errorf("failed to load existing mcp_config.json: %w", err)
	}

	// Reconcile: drop the ctxloom-managed server, re-add per current config.
	delete(mf.Servers, AppMCPServerName)

	if mcp == nil || mcp.ShouldAutoRegisterCtxloom() {
		w.setServer(mf, AppMCPServerName, antigravityMCPServer{
			Command: agent.CtxloomBinary,
			Args:    agent.CtxloomMCPArgs,
		})
	}

	for name, server := range bundleMCP {
		w.setServer(mf, name, antigravityMCPServer{Command: server.Command, Args: server.Args, Env: server.Env})
	}

	if mcp != nil {
		for name, server := range mcp.Servers {
			w.setServer(mf, name, antigravityMCPServer{Command: server.Command, Args: server.Args, Env: server.Env})
		}
		if backendServers, ok := mcp.Plugins["antigravity"]; ok {
			for name, server := range backendServers {
				w.setServer(mf, name, antigravityMCPServer{Command: server.Command, Args: server.Args, Env: server.Env})
			}
		}
	}

	return w.saveMCPFile(mcpPath, mf)
}

// setServer marshals a typed stdio server entry into the raw server map.
func (w *AntigravityHookWriter) setServer(mf *antigravityMCPFile, name string, s antigravityMCPServer) {
	data, err := json.Marshal(s)
	if err != nil {
		w.warn("failed to marshal MCP server %q: %v", name, err)
		return
	}
	mf.Servers[name] = data
}

// loadMCPFile loads existing mcp_config.json or returns an empty structure.
// An empty file (agy itself creates zero-byte mcp_config.json files) and a
// parse failure both degrade to empty with a warning for the latter.
func (w *AntigravityHookWriter) loadMCPFile(path string) (*antigravityMCPFile, error) {
	mf := &antigravityMCPFile{
		Servers: make(map[string]json.RawMessage),
		Other:   make(map[string]json.RawMessage),
	}

	data, err := afero.ReadFile(w.getFS(), path)
	if err != nil {
		if os.IsNotExist(err) {
			return mf, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return mf, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		w.warn("failed to parse %s/mcp_config.json: %v - existing MCP servers may not be preserved", AgentsDir, err)
		return mf, nil
	}

	if serversRaw, ok := raw["mcpServers"]; ok {
		if err := json.Unmarshal(serversRaw, &mf.Servers); err != nil {
			w.warn("failed to parse mcpServers in mcp_config.json: %v - existing MCP servers may not be preserved", err)
		}
		delete(raw, "mcpServers")
	}
	mf.Other = raw
	return mf, nil
}

// saveMCPFile writes mcp_config.json back atomically. When nothing remains
// (no servers, no other fields) and the file does not exist, nothing is
// written — uninstall never creates files.
func (w *AntigravityHookWriter) saveMCPFile(path string, mf *antigravityMCPFile) error {
	output := make(map[string]interface{})
	for k, v := range mf.Other {
		var val interface{}
		if err := json.Unmarshal(v, &val); err != nil {
			w.warn("failed to preserve mcp_config.json field %q: %v", k, err)
			continue
		}
		output[k] = val
	}
	if len(mf.Servers) > 0 {
		servers := make(map[string]interface{}, len(mf.Servers))
		for name, rawServer := range mf.Servers {
			var val interface{}
			if err := json.Unmarshal(rawServer, &val); err != nil {
				w.warn("failed to preserve MCP server %q: %v", name, err)
				continue
			}
			servers[name] = val
		}
		output["mcpServers"] = servers
	}

	if len(output) == 0 {
		if exists, _ := afero.Exists(w.getFS(), path); !exists {
			return nil
		}
	}

	data, err := agent.CanonicalJSON(output)
	if err != nil {
		return fmt.Errorf("failed to marshal mcp_config.json: %w", err)
	}
	return agent.AtomicWriteFile(w.getFS(), path, data, "mcp_config.json")
}

// RemoveSettings implements SettingsWriter for Antigravity CLI: it clears
// ctxloom-managed hooks from hooks.json and the managed MCP server from
// mcp_config.json, leaving absent files absent.
func (w *AntigravityHookWriter) RemoveSettings(projectDir string) error {
	fs := w.getFS()

	hooksPath := w.HooksPath(projectDir)
	if exists, _ := afero.Exists(fs, hooksPath); exists {
		hf, err := w.loadHooksFile(hooksPath)
		if err != nil {
			return fmt.Errorf("failed to load existing hooks.json: %w", err)
		}
		w.removeCtxloomHooks(hf)
		if err := w.saveHooksFile(hooksPath, hf); err != nil {
			return err
		}
	}

	mcpPath := w.MCPConfigPath(projectDir)
	if exists, _ := afero.Exists(fs, mcpPath); exists {
		mf, err := w.loadMCPFile(mcpPath)
		if err != nil {
			return fmt.Errorf("failed to load existing mcp_config.json: %w", err)
		}
		delete(mf.Servers, AppMCPServerName)
		if err := w.saveMCPFile(mcpPath, mf); err != nil {
			return err
		}
	}
	return nil
}

// Status implements SettingsWriter for Antigravity CLI.
func (w *AntigravityHookWriter) Status(projectDir string) (agent.SettingsStatus, error) {
	fs := w.getFS()
	var status agent.SettingsStatus

	hooksPath := w.HooksPath(projectDir)
	if exists, _ := afero.Exists(fs, hooksPath); exists {
		status.SettingsExists = true
		hf, err := w.loadHooksFile(hooksPath)
		if err != nil {
			return status, fmt.Errorf("failed to load existing hooks.json: %w", err)
		}
		status.HooksPresent = antigravityHasManagedHook(hf)
	}

	mcpPath := w.MCPConfigPath(projectDir)
	if exists, _ := afero.Exists(fs, mcpPath); exists {
		mf, err := w.loadMCPFile(mcpPath)
		if err != nil {
			return status, fmt.Errorf("failed to load existing mcp_config.json: %w", err)
		}
		if _, ok := mf.Servers[AppMCPServerName]; ok {
			status.MCPPresent = true
		}
	}
	return status, nil
}

// antigravityHasManagedHook reports whether any configured hook is
// ctxloom-managed, descending the event → group → hooks[] shape.
func antigravityHasManagedHook(hf *antigravityHooksFile) bool {
	for _, groups := range hf.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				if hook.Name == antigravityCtxloomHookName || agent.IsManaged(hook.Command, "ctxloom") {
					return true
				}
			}
		}
	}
	return false
}

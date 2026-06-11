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
	"sort"
	"strings"

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

// WorkspaceHooksPath returns the workspace-level .agents/hooks.json path —
// the only place agy v1.0.7 reads hooks from. Exported for companion tools
// (ltk) that manage hooks in the same file, so the path convention has a
// single source of truth.
func WorkspaceHooksPath(dir string) string {
	return filepath.Join(dir, AgentsDir, "hooks.json")
}

// SettingsPath returns the path to Antigravity's project-level hooks.json file
// (the settings file ctxloom manages for Antigravity). There is no usable
// global location in agy v1.0.7: ~/.gemini/antigravity-cli/hooks.json is
// silently ignored, and a hooks.json under ~/.gemini/ or ~/.gemini/config/
// hangs headless agy before any hook executes.
func (w *AntigravityHookWriter) SettingsPath(projectDir string) string {
	return WorkspaceHooksPath(projectDir)
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
// verified agy schema. Unknown per-group fields a future agy adds are captured
// in extra on load and merged back on save, so a rewrite never drops them.
type antigravityHookGroup struct {
	Matcher string                 `json:"matcher,omitempty"`
	Hooks   []antigravityHookEntry `json:"hooks"`

	// extra holds unknown per-group fields for round-trip preservation.
	extra map[string]json.RawMessage
}

// antigravityHookGroupShape mirrors antigravityHookGroup's known fields
// without its methods, for recursion-free (un)marshalling.
type antigravityHookGroupShape antigravityHookGroup

// UnmarshalJSON decodes the known fields and captures unknown keys in extra.
func (g *antigravityHookGroup) UnmarshalJSON(data []byte) error {
	var shape antigravityHookGroupShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	delete(raw, "matcher")
	delete(raw, "hooks")
	*g = antigravityHookGroup(shape)
	if len(raw) > 0 {
		g.extra = raw
	}
	return nil
}

// MarshalJSON emits the known fields, merged with any preserved unknown keys.
// Known fields win on a key collision (extra never overrides them).
func (g antigravityHookGroup) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(antigravityHookGroupShape(g), g.extra)
}

// antigravityHookEntry is a single command hook. agy requires type:"command".
// name is a durable field agy preserves, used to identify ctxloom-managed
// entries for clean removal. The timeout field is deliberately never written:
// agy v1.0.7 does not document its unit, and a seconds/milliseconds mismatch
// would silently break hooks — agy's own default applies instead. An existing
// user-set timeout round-trips untouched. Unknown per-entry fields are
// captured in extra on load and merged back on save (see antigravityHookGroup).
type antigravityHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Name    string `json:"name,omitempty"`
	Timeout int    `json:"timeout,omitempty"`

	// extra holds unknown per-entry fields for round-trip preservation.
	extra map[string]json.RawMessage
}

// antigravityHookEntryShape mirrors antigravityHookEntry's known fields
// without its methods, for recursion-free (un)marshalling.
type antigravityHookEntryShape antigravityHookEntry

// UnmarshalJSON decodes the known fields and captures unknown keys in extra.
func (e *antigravityHookEntry) UnmarshalJSON(data []byte) error {
	var shape antigravityHookEntryShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, known := range []string{"type", "command", "name", "timeout"} {
		delete(raw, known)
	}
	*e = antigravityHookEntry(shape)
	if len(raw) > 0 {
		e.extra = raw
	}
	return nil
}

// MarshalJSON emits the known fields, merged with any preserved unknown keys.
func (e antigravityHookEntry) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(antigravityHookEntryShape(e), e.extra)
}

// marshalWithExtra marshals shape, merging extra's unknown keys into the
// object. Without extras the shape marshals as-is, so entries ctxloom itself
// writes keep their exact current byte shape (canonicalization downstream is
// unchanged either way — agent.CanonicalJSON re-sorts keys recursively).
func marshalWithExtra(shape any, extra map[string]json.RawMessage) ([]byte, error) {
	known, err := json.Marshal(shape)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return known, nil
	}
	merged := make(map[string]json.RawMessage, len(extra)+4)
	if err := json.Unmarshal(known, &merged); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, ok := merged[k]; !ok {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
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
	hooksPath := w.SettingsPath(projectDir)

	if err := fs.MkdirAll(filepath.Dir(hooksPath), 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", AgentsDir, err)
	}

	hooksFile, err := w.loadHooksFile(hooksPath)
	if err != nil {
		return fmt.Errorf("failed to load existing hooks.json: %w", err)
	}

	w.removeCtxloomHooks(hooksFile)
	contextHash := w.addUnifiedHooks(hooksFile, hooks.Unified)
	if backendHooks, ok := hooks.Plugins["antigravity"]; ok {
		w.addBackendHooks(hooksFile, backendHooks)
	}

	if err := w.saveHooksFile(hooksPath, hooksFile); err != nil {
		return err
	}

	if err := w.reconcileManagedContext(projectDir, contextHash); err != nil {
		return err
	}

	return w.writeMCPConfig(projectDir, mcp, bundleMCP)
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
// Built from the hooks_wire.go tool-name constants so the matcher vocabulary
// cannot drift from the verified wire vocabulary.
const (
	// antigravityShellMatcher scopes a hook to shell execution tools.
	antigravityShellMatcher = ToolRunCommand + "|" + ToolExecuteCommand
	// antigravityFileEditMatcher scopes a hook to file mutation tools.
	antigravityFileEditMatcher = ToolWriteToFile + "|" + ToolReplaceFileContent
)

// addUnifiedHooks translates unified hooks to Antigravity's events and
// returns the context hash when a context-injection hook was present.
//
// agy v1.0.7 loads PreToolUse, PostToolUse, and Stop handlers. SessionStart /
// SessionEnd entries are written through verbatim: agy silently skips events
// it doesn't support (verified — they don't load as handlers and don't error),
// and they light up if a future agy adds the events. The one exception is the
// context-injection hook (ContextHash set): registering it would silently
// drop ctxloom's assembled context, so it is diverted — WriteSettings
// materializes the context into .agents/AGENTS.md, which agy reads at
// session start (verified). Chunked injection (N part-hooks for one hash) is
// a hook-harness workaround; the file carries the whole content once.
func (w *AntigravityHookWriter) addUnifiedHooks(hf *antigravityHooksFile, unified wire.UnifiedHooks) string {
	// SessionStart cannot ride the shared route table: the context-injection
	// hook is diverted to .agents/AGENTS.md and idempotent hooks divert to
	// PreToolUse (see the doc comment above).
	contextHash := ""
	for _, h := range unified.SessionStart {
		if h.ContextHash != "" {
			contextHash = h.ContextHash
			continue
		}
		// A hook declared idempotent falls back to PreToolUse (first tool
		// call and after) — the only way it ever fires on agy. Diverted, not
		// duplicated, so a future agy adding SessionStart can't double-fire.
		if h.PreToolFallback {
			hook := h
			if hook.Matcher == "" {
				hook.Matcher = ".*"
			}
			w.addHook(hf, "PreToolUse", hook)
			continue
		}
		w.addHook(hf, "SessionStart", h)
	}
	agent.RouteUnifiedHooks([]agent.HookRoute{
		{Hooks: unified.SessionEnd, Event: "SessionEnd"},
		{Hooks: unified.PreTool, Event: "PreToolUse"},
		{Hooks: unified.PostTool, Event: "PostToolUse"},
		{Hooks: unified.PreShell, Event: "PreToolUse", DefaultMatcher: antigravityShellMatcher},
		{Hooks: unified.PostFileEdit, Event: "PostToolUse", DefaultMatcher: antigravityFileEditMatcher},
	}, func(event string, h wire.Hook) {
		w.addHook(hf, event, h)
	})
	return contextHash
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
//
// Only command hooks are written: agy executes type:"command" entries only,
// and translating a prompt/agent hook here would mangle it into
// {"type":"command","command":""} — a dead entry. Non-command hooks are
// skipped with a warning (empty Type is the wire default and means command).
func (w *AntigravityHookWriter) addHook(hf *antigravityHooksFile, eventName string, h wire.Hook) {
	if h.Type != "" && h.Type != "command" {
		w.warn("skipping %s hook of type %q: antigravity only supports command hooks", eventName, h.Type)
		return
	}

	// Drop any surviving entry with this exact command before appending.
	// removeCtxloomHooks only recognizes the name marker / ctxloom-token
	// commands; an identical command installed by a companion binary itself
	// (e.g. ltk registered `ltk evaluate ...` directly, without the marker)
	// would otherwise fire twice. Exact match keeps user variants of the same
	// binary with different args untouched — same semantics as the claude
	// writer's removeExactCommand dedupe.
	w.removeExactCommand(hf, eventName, h.Command)

	entry := antigravityHookEntry{
		Type:    "command",
		Command: h.Command,
		Name:    antigravityCtxloomHookName,
	}
	group := antigravityHookGroup{Matcher: h.Matcher, Hooks: []antigravityHookEntry{entry}}
	hf.Hooks[eventName] = append(hf.Hooks[eventName], group)
}

// removeExactCommand drops every hook entry under eventName whose command is
// exactly cmd, pruning emptied groups and events. Identity is the verbatim
// command string: entries written by other tools carry no ctxloom marker, so
// this is the only way to recognize an exact duplicate of a hook ctxloom is
// about to add.
func (w *AntigravityHookWriter) removeExactCommand(hf *antigravityHooksFile, eventName, cmd string) {
	if cmd == "" {
		return
	}
	groups := hf.Hooks[eventName]
	if len(groups) == 0 {
		return
	}
	var keptGroups []antigravityHookGroup
	for _, g := range groups {
		var kept []antigravityHookEntry
		for _, e := range g.Hooks {
			if e.Command != cmd {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			g.Hooks = kept
			keptGroups = append(keptGroups, g)
		}
	}
	if len(keptGroups) > 0 {
		hf.Hooks[eventName] = keptGroups
	} else {
		delete(hf.Hooks, eventName)
	}
}

// antigravityMCPLedger is the sidecar recording which mcp_config.json server
// names are ctxloom-managed, one per line. agy rejects unknown fields inside
// mcp_config.json itself (verified: an extra entry field or top-level key
// hangs headless agy), so ownership cannot ride an in-file marker the way
// claude's _ctxloom does — without this ledger, a server renamed or removed
// from config/bundles would linger in mcp_config.json forever, and a stale
// stdio entry permanently hangs headless agy.
const antigravityMCPLedger = ".ctxloom-mcp-managed"

// mcpLedgerPath returns the path to the managed-server ledger.
func (w *AntigravityHookWriter) mcpLedgerPath(projectDir string) string {
	return filepath.Join(projectDir, AgentsDir, antigravityMCPLedger)
}

// readMCPLedger returns the managed server names from the ledger, if any.
func (w *AntigravityHookWriter) readMCPLedger(projectDir string) []string {
	data, err := afero.ReadFile(w.getFS(), w.mcpLedgerPath(projectDir))
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// writeMCPLedger persists the managed names, removing the ledger when empty.
func (w *AntigravityHookWriter) writeMCPLedger(projectDir string, names []string) error {
	fs := w.getFS()
	path := w.mcpLedgerPath(projectDir)
	if len(names) == 0 {
		if exists, _ := afero.Exists(fs, path); exists {
			return fs.Remove(path)
		}
		return nil
	}
	sort.Strings(names)
	return afero.WriteFile(fs, path, []byte(strings.Join(names, "\n")+"\n"), 0644)
}

// writeMCPConfig reconciles ctxloom-managed MCP servers into mcp_config.json,
// preserving user-authored entries (including remote serverUrl servers) raw.
// Managed ownership is tracked in the sidecar ledger: every previously
// managed name is dropped, then the current managed set is re-added — so
// renames and removals in config/bundles propagate instead of orphaning
// entries.
func (w *AntigravityHookWriter) writeMCPConfig(projectDir string, mcp *wire.MCPConfig, bundleMCP map[string]wire.MCPServer) error {
	mcpPath := w.MCPConfigPath(projectDir)
	mf, err := w.loadMCPFile(mcpPath)
	if err != nil {
		return fmt.Errorf("failed to load existing mcp_config.json: %w", err)
	}

	// Reconcile: drop everything previously managed (the ledger), plus the
	// well-known ctxloom server name for pre-ledger files.
	delete(mf.Servers, AppMCPServerName)
	for _, name := range w.readMCPLedger(projectDir) {
		delete(mf.Servers, name)
	}

	var managed []string
	add := func(name string, s antigravityMCPServer) {
		w.setServer(mf, name, s)
		managed = append(managed, name)
	}

	if mcp == nil || mcp.ShouldAutoRegisterCtxloom() {
		add(AppMCPServerName, antigravityMCPServer{
			Command: agent.CtxloomBinary,
			Args:    agent.CtxloomMCPArgs,
		})
	}

	for name, server := range bundleMCP {
		add(name, antigravityMCPServer{Command: server.Command, Args: server.Args, Env: server.Env})
	}

	if mcp != nil {
		for name, server := range mcp.Servers {
			add(name, antigravityMCPServer{Command: server.Command, Args: server.Args, Env: server.Env})
		}
		if backendServers, ok := mcp.Plugins["antigravity"]; ok {
			for name, server := range backendServers {
				add(name, antigravityMCPServer{Command: server.Command, Args: server.Args, Env: server.Env})
			}
		}
	}

	if err := w.saveMCPFile(mcpPath, mf); err != nil {
		return err
	}
	return w.writeMCPLedger(projectDir, managed)
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

// Managed-context section markers in .agents/AGENTS.md. agy reads
// .agents/AGENTS.md at session start (verified), which makes it the delivery
// channel for ctxloom's assembled context — agy fires no SessionStart hooks,
// so the injection hook other agents use can never run here. Content between
// the markers is ctxloom-owned and reconciled on every apply; anything
// outside them is the user's and preserved byte-for-byte.
const (
	managedContextBegin = "<!-- ctxloom:context:begin (managed — do not edit between markers) -->"
	managedContextEnd   = "<!-- ctxloom:context:end -->"
)

// agentsMDPath returns the path to the workspace .agents/AGENTS.md file.
func (w *AntigravityHookWriter) agentsMDPath(projectDir string) string {
	return filepath.Join(projectDir, AgentsDir, "AGENTS.md")
}

// reconcileManagedContext writes (or removes, when hash is empty) the managed
// context section of .agents/AGENTS.md. The context content is read from the
// content-addressed context file the provider wrote under projectDir.
func (w *AntigravityHookWriter) reconcileManagedContext(projectDir, hash string) error {
	content := ""
	if hash != "" {
		var err error
		content, err = agent.ReadContextFile(projectDir, hash, agent.WithContextFS(w.getFS()))
		if err != nil {
			// Fault-tolerance contract: a missing/unreadable context file must
			// not block hook application; the section is dropped with a warning.
			w.warn("failed to read context file %s: %v - context will not be delivered to antigravity", hash, err)
			content = ""
		}
	}

	fs := w.getFS()
	path := w.agentsMDPath(projectDir)
	existing, err := afero.ReadFile(fs, path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read %s: %w", path, err)
	}

	userContent := stripManagedSection(string(existing))

	var section string
	if content != "" {
		section = managedContextBegin + "\n" + content + "\n" + managedContextEnd + "\n"
	}

	merged := userContent
	if section != "" {
		if merged != "" && !strings.HasSuffix(merged, "\n") {
			merged += "\n"
		}
		merged += section
	}

	if strings.TrimSpace(merged) == "" {
		// Nothing left: remove the file if it exists, never create it.
		if exists, _ := afero.Exists(fs, path); exists {
			return fs.Remove(path)
		}
		return nil
	}
	return agent.AtomicWriteFile(fs, path, []byte(merged), "AGENTS.md")
}

// stripManagedSection returns content with the ctxloom-managed marker section
// removed. Content outside the markers is untouched; an unterminated begin
// marker drops through to the end of the file (the section is ours to own).
func stripManagedSection(content string) string {
	begin := strings.Index(content, managedContextBegin)
	if begin < 0 {
		return content
	}
	rest := content[begin+len(managedContextBegin):]
	end := strings.Index(rest, managedContextEnd)
	if end < 0 {
		return strings.TrimRight(content[:begin], "\n") + ifNonEmpty(content[:begin], "\n")
	}
	after := strings.TrimLeft(rest[end+len(managedContextEnd):], "\n")
	before := content[:begin]
	if before == "" {
		return after
	}
	return before + after
}

// ifNonEmpty returns suffix when s is non-empty, else "".
func ifNonEmpty(s, suffix string) string {
	if s == "" {
		return ""
	}
	return suffix
}

// RemoveSettings implements SettingsWriter for Antigravity CLI: it clears
// ctxloom-managed hooks from hooks.json, the managed MCP server from
// mcp_config.json, and the managed context section from .agents/AGENTS.md,
// leaving absent files absent.
func (w *AntigravityHookWriter) RemoveSettings(projectDir string) error {
	fs := w.getFS()

	hooksPath := w.SettingsPath(projectDir)
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
		for _, name := range w.readMCPLedger(projectDir) {
			delete(mf.Servers, name)
		}
		if err := w.saveMCPFile(mcpPath, mf); err != nil {
			return err
		}
	}
	if err := w.writeMCPLedger(projectDir, nil); err != nil {
		return err
	}

	return w.reconcileManagedContext(projectDir, "")
}

// Status implements SettingsWriter for Antigravity CLI.
func (w *AntigravityHookWriter) Status(projectDir string) (agent.SettingsStatus, error) {
	fs := w.getFS()
	var status agent.SettingsStatus

	hooksPath := w.SettingsPath(projectDir)
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
		for _, name := range w.readMCPLedger(projectDir) {
			if _, ok := mf.Servers[name]; ok {
				status.MCPPresent = true
				break
			}
		}
	}

	// The managed context section is Antigravity's stand-in for the
	// SessionStart injection hook other agents carry, so it counts as a
	// managed hook for wired-status purposes.
	if data, err := afero.ReadFile(fs, w.agentsMDPath(projectDir)); err == nil {
		if strings.Contains(string(data), managedContextBegin) {
			status.HooksPresent = true
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

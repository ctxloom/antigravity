package antigravity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/afero"

	"github.com/ctxloom/shared/agent"
)

// AntigravityLifecycle implements LifecycleHandler for Antigravity using
// hooks. Embeds BaseLifecycle for the shared implementation.
//
// Note: agy v1.0.7 loads no SessionStart handlers, so session-start behavior
// (context injection) flows through the context file, not a hook — see
// AntigravityContext.
type AntigravityLifecycle struct {
	*agent.BaseLifecycle
	backend *Antigravity
}

// NewAntigravityLifecycle creates a new Antigravity lifecycle handler.
func NewAntigravityLifecycle(backend *Antigravity) *AntigravityLifecycle {
	return &AntigravityLifecycle{
		BaseLifecycle: agent.NewBaseLifecycle("antigravity", backend.writeSettings),
		backend:       backend,
	}
}

// AntigravityMCPManager implements MCPManager for Antigravity CLI.
// Embeds BaseMCPManager for the shared implementation.
type AntigravityMCPManager struct {
	*agent.BaseMCPManager
	backend *Antigravity
}

// NewAntigravityMCPManager creates a new Antigravity MCP manager.
func NewAntigravityMCPManager(backend *Antigravity) *AntigravityMCPManager {
	return &AntigravityMCPManager{
		BaseMCPManager: agent.NewBaseMCPManager("antigravity", backend.writeSettings),
		backend:        backend,
	}
}

// AntigravityContext implements ContextProvider for Antigravity via the
// context file. Embeds BaseContextProvider for the shared implementation.
type AntigravityContext struct {
	*agent.BaseContextProvider
	backend *Antigravity
}

// NewAntigravityContext creates a new Antigravity context provider.
func NewAntigravityContext(backend *Antigravity) *AntigravityContext {
	return &AntigravityContext{
		BaseContextProvider: agent.NewBaseContextProvider(),
		backend:             backend,
	}
}

// antigravitySkillsDir is the workspace skills directory agy reads, relative
// to the workspace root.
const antigravitySkillsDir = AgentsDir + "/skills"

// antigravityManifest tracks ctxloom-written skill files for clean removal
// (agy's skill discovery semantics for subdirectories are unverified, so
// skills are written flat with a manifest rather than into a ctxloom-owned
// subdirectory).
const antigravityManifest = ".ctxloom-manifest"

// AntigravitySkills implements SkillRegistry for Antigravity CLI as markdown
// skill files under .agents/skills/.
type AntigravitySkills struct {
	backend *Antigravity
}

// Register adds a skill as an Antigravity skill file.
func (s *AntigravitySkills) Register(workDir string, skill agent.Skill) error {
	return WriteCommandFiles(workDir, []agent.CommandExport{skillExport(skill)})
}

// RegisterAll adds multiple skills as Antigravity skill files.
func (s *AntigravitySkills) RegisterAll(workDir string, skills []agent.Skill) error {
	cmds := make([]agent.CommandExport, 0, len(skills))
	for _, skill := range skills {
		cmds = append(cmds, skillExport(skill))
	}
	return WriteCommandFiles(workDir, cmds)
}

// RegisterFromContent writes skill files from host-resolved command exports.
// The host maps bundle content (with antigravity enablement + metadata) to
// these agent-agnostic exports, so this stays config/bundle-free.
func (s *AntigravitySkills) RegisterFromContent(workDir string, cmds []agent.CommandExport) error {
	return WriteCommandFiles(workDir, cmds)
}

// skillExport maps a Skill to an enabled command export.
func skillExport(skill agent.Skill) agent.CommandExport {
	return agent.CommandExport{
		Name:        skill.Name,
		Content:     skill.Content,
		Enabled:     true,
		Description: skill.Description,
	}
}

// Clear removes all ctxloom-managed skill files using the manifest.
func (s *AntigravitySkills) Clear(workDir string) error {
	skillsDir := filepath.Join(workDir, filepath.FromSlash(antigravitySkillsDir))
	manifestPath := filepath.Join(skillsDir, antigravityManifest)

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, name := range strings.Split(string(data), "\n") {
		if name = strings.TrimSpace(name); name != "" {
			_ = os.Remove(filepath.Join(skillsDir, name))
		}
	}
	return os.Remove(manifestPath)
}

// List returns registered skill names from the manifest.
func (s *AntigravitySkills) List(workDir string) ([]string, error) {
	manifestPath := filepath.Join(workDir, filepath.FromSlash(antigravitySkillsDir), antigravityManifest)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, strings.TrimSuffix(name, ".md"))
		}
	}
	return names, nil
}

// WriteCommandFiles writes enabled command exports as markdown skill files
// under .agents/skills/ and records them in the manifest. Previously
// manifest-listed files are removed first, so the written set always mirrors
// the current exports.
func WriteCommandFiles(workDir string, cmds []agent.CommandExport, opts ...agent.CommandFileOption) error {
	fs := agent.ResolveCommandFS(opts...)
	skillsDir := filepath.Join(workDir, filepath.FromSlash(antigravitySkillsDir))
	manifestPath := filepath.Join(skillsDir, antigravityManifest)

	// Remove the previous ctxloom-written set (manifest-tracked only — the
	// directory is shared with user-authored skills, never wiped wholesale).
	if data, err := afero.ReadFile(fs, manifestPath); err == nil {
		for _, name := range strings.Split(string(data), "\n") {
			if name = strings.TrimSpace(name); name != "" {
				_ = fs.Remove(filepath.Join(skillsDir, name))
			}
		}
		_ = fs.Remove(manifestPath)
	}

	var written []string
	for _, c := range cmds {
		if !c.Enabled {
			continue
		}
		if len(written) == 0 {
			if err := fs.MkdirAll(skillsDir, 0755); err != nil {
				return fmt.Errorf("create skills dir: %w", err)
			}
		}
		fileName := c.Name + ".md"
		path := filepath.Join(skillsDir, fileName)
		if dir := filepath.Dir(path); dir != skillsDir {
			if err := fs.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("create skill subdir %s: %w", dir, err)
			}
		}
		if err := afero.WriteFile(fs, path, []byte(c.Content), 0644); err != nil {
			return fmt.Errorf("write skill %s: %w", c.Name, err)
		}
		written = append(written, fileName)
	}

	if len(written) == 0 {
		return nil
	}
	return afero.WriteFile(fs, manifestPath, []byte(strings.Join(written, "\n")+"\n"), 0644)
}

// AntigravitySessionHistory implements SessionHistory for Antigravity CLI.
// Reads from ~/.gemini/antigravity-cli/brain/<conversation-uuid>/
// .system_generated/logs/transcript_full.jsonl. The fs and homeDir fields are
// afero injection points used by tests.
type AntigravitySessionHistory struct {
	backend *Antigravity
	fs      afero.Fs
	homeDir string // Override home directory for testing
}

// AntigravitySessionHistoryOption configures AntigravitySessionHistory.
type AntigravitySessionHistoryOption func(*AntigravitySessionHistory)

// WithAntigravitySessionFS sets a custom filesystem for testing.
func WithAntigravitySessionFS(fs afero.Fs) AntigravitySessionHistoryOption {
	return func(h *AntigravitySessionHistory) { h.fs = fs }
}

// WithAntigravitySessionHomeDir sets a custom home directory for testing.
func WithAntigravitySessionHomeDir(dir string) AntigravitySessionHistoryOption {
	return func(h *AntigravitySessionHistory) { h.homeDir = dir }
}

// NewAntigravitySessionHistory creates a new Antigravity session history handler.
func NewAntigravitySessionHistory(backend *Antigravity, opts ...AntigravitySessionHistoryOption) *AntigravitySessionHistory {
	h := &AntigravitySessionHistory{
		backend: backend,
		fs:      afero.NewOsFs(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// brainDir returns agy's conversation store root.
func (h *AntigravitySessionHistory) brainDir() (string, error) {
	homeDir := h.homeDir
	if homeDir == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
	}
	return filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain"), nil
}

// transcriptPathFor returns the transcript path inside one brain conversation
// directory.
func transcriptPathFor(brainDir, conversationID string) string {
	return filepath.Join(brainDir, conversationID, ".system_generated", "logs", "transcript_full.jsonl")
}

// GetCurrentSession returns the most recent session transcript.
//
// agy's brain store is global (not keyed by workspace), so workDir cannot
// narrow the listing; the most recently modified conversation wins — same
// trade-off as codex's global session store.
func (h *AntigravitySessionHistory) GetCurrentSession(workDir string) (*agent.Session, error) {
	sessions, err := h.ListSessions(workDir)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions found")
	}
	return h.parseSessionFile(sessions[0].Path, sessions[0].ID)
}

// ListSessions returns available session metadata, most recent first.
func (h *AntigravitySessionHistory) ListSessions(workDir string) ([]agent.SessionMeta, error) {
	brain, err := h.brainDir()
	if err != nil {
		return nil, err
	}

	entries, err := afero.ReadDir(h.fs, brain)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read brain directory: %w", err)
	}

	var sessions []agent.SessionMeta
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := transcriptPathFor(brain, entry.Name())
		info, err := h.fs.Stat(path)
		if err != nil {
			continue // conversation without a transcript (e.g. still starting)
		}
		sessions = append(sessions, agent.SessionMeta{
			ID:        entry.Name(),
			StartTime: info.ModTime(), // Approximate
			Path:      path,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})
	return sessions, nil
}

// GetSession returns a specific session by conversation ID.
func (h *AntigravitySessionHistory) GetSession(workDir string, sessionID string) (*agent.Session, error) {
	brain, err := h.brainDir()
	if err != nil {
		return nil, err
	}
	path := transcriptPathFor(brain, sessionID)
	if _, err := h.fs.Stat(path); err != nil {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	return h.parseSessionFile(path, sessionID)
}

// GetSessionByPath returns a session by its transcript file path.
func (h *AntigravitySessionHistory) GetSessionByPath(path string) (*agent.Session, error) {
	// Recover the conversation ID from .../brain/<uuid>/.system_generated/...
	id := ""
	if idx := strings.Index(path, string(filepath.Separator)+".system_generated"+string(filepath.Separator)); idx > 0 {
		id = filepath.Base(path[:idx])
	}
	return h.parseSessionFile(path, id)
}

// TranscriptPathFromHook returns the transcript path agy provides directly on
// every hook's stdin (the transcriptPath field), enabling session-bind.
func (h *AntigravitySessionHistory) TranscriptPathFromHook(workDir, sessionID, transcriptPath string) string {
	return transcriptPath
}

// antigravityTranscriptEntry is one transcript_full.jsonl record. Verified
// shape (agy v1.0.7): {"step_index":N,"source":"USER_EXPLICIT|SYSTEM|MODEL",
// "type":"USER_INPUT|CONVERSATION_HISTORY|PLANNER_RESPONSE|RUN_COMMAND|…",
// "status":"DONE","created_at":RFC3339,"content":"…","tool_calls":[…]}.
type antigravityTranscriptEntry struct {
	StepIndex int    `json:"step_index"`
	Source    string `json:"source"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_calls"`
}

// parseSessionFile reads an agy transcript into the normalized Session
// contract. Unrecognized records are skipped so a session degrades to a
// partial transcript rather than an error.
func (h *AntigravitySessionHistory) parseSessionFile(path, sessionID string) (*agent.Session, error) {
	file, err := h.fs.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open session file: %w", err)
	}
	defer func() { _ = file.Close() }()

	session := &agent.Session{
		ID:      sessionID,
		Entries: []agent.SessionEntry{},
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var te antigravityTranscriptEntry
		if err := json.Unmarshal(line, &te); err != nil {
			continue // skip malformed line
		}
		for _, e := range convertTranscriptEntry(te) {
			session.Entries = append(session.Entries, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan session file: %w", err)
	}

	if len(session.Entries) > 0 {
		session.StartTime = session.Entries[0].Timestamp
		session.EndTime = session.Entries[len(session.Entries)-1].Timestamp
	}
	return session, nil
}

// userRequestRe-equivalent trimming: agy wraps the user's prompt in
// <USER_REQUEST>…</USER_REQUEST> with metadata blocks alongside; extract just
// the request text when present.
func extractUserRequest(content string) string {
	const open, close = "<USER_REQUEST>", "</USER_REQUEST>"
	start := strings.Index(content, open)
	if start < 0 {
		return strings.TrimSpace(content)
	}
	rest := content[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// convertTranscriptEntry maps one agy record to normalized SessionEntries
// (one PLANNER_RESPONSE can carry both text and tool calls). Returns nil for
// records with no conversational content.
func convertTranscriptEntry(te antigravityTranscriptEntry) []agent.SessionEntry {
	var ts time.Time
	if te.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, te.CreatedAt); err == nil {
			ts = t
		}
	}

	var entries []agent.SessionEntry
	switch te.Type {
	case "USER_INPUT":
		if text := extractUserRequest(te.Content); text != "" {
			entries = append(entries, agent.SessionEntry{
				Type: agent.EntryTypeUser, Content: text, Timestamp: ts,
			})
		}
	case "PLANNER_RESPONSE":
		if text := strings.TrimSpace(te.Content); text != "" {
			entries = append(entries, agent.SessionEntry{
				Type: agent.EntryTypeAssistant, Content: text, Timestamp: ts,
			})
		}
		for _, tc := range te.ToolCalls {
			entries = append(entries, agent.SessionEntry{
				Type: agent.EntryTypeToolUse, ToolName: tc.Name, ToolInput: tc.Args, Timestamp: ts,
			})
		}
	default:
		// Tool execution records (RUN_COMMAND, …) carry the result.
		if te.Source == "MODEL" && te.Type != "" && te.Content != "" {
			entries = append(entries, agent.SessionEntry{
				Type: agent.EntryTypeToolResult, ToolName: strings.ToLower(te.Type), ToolOutput: te.Content, Timestamp: ts,
			})
		}
	}
	return entries
}

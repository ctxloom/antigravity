package antigravity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/afero"

	"github.com/ctxloom/shared/agent"
)

// antigravitySkillsDir is the workspace skills directory agy reads, relative
// to the workspace root.
const antigravitySkillsDir = AgentsDir + "/skills"

// antigravityManifest tracks ctxloom-written skill files for clean removal
// (agy's skill discovery semantics for subdirectories are unverified, so
// skills are written flat with a manifest rather than into a ctxloom-owned
// subdirectory).
const antigravityManifest = ".ctxloom-manifest"

// AntigravitySkills writes skills for Antigravity CLI as markdown skill files
// under .agents/skills/.
type AntigravitySkills struct{}

// RegisterFromContent writes skill files from host-resolved command exports.
// The host maps bundle content (with antigravity enablement + metadata) to
// these agent-agnostic exports, so this stays config/bundle-free.
func (s *AntigravitySkills) RegisterFromContent(workDir string, cmds []agent.CommandExport) error {
	return WriteCommandFiles(workDir, cmds)
}

// WriteCommandFiles writes enabled command exports as markdown skill files
// under .agents/skills/ and records them in the manifest. Previously
// manifest-listed files are removed first, so the written set always mirrors
// the current exports (see agent.WriteManagedCommandFiles for the shared
// mechanics; the directory is shared with user-authored skills, never wiped
// wholesale).
func WriteCommandFiles(workDir string, cmds []agent.CommandExport, opts ...agent.CommandFileOption) error {
	fs := agent.ResolveCommandFS(opts...)
	skillsDir := filepath.Join(workDir, filepath.FromSlash(antigravitySkillsDir))
	return agent.WriteManagedCommandFiles(fs, skillsDir, antigravityManifest, cmds,
		func(c agent.CommandExport) (string, []byte, error) {
			// OPEN QUESTION: agy's skill-argument syntax is unverified, so the
			// content is written RAW — no {{var}} → $N transform like the
			// claude/codex renderers apply — keeping emitted bytes identical
			// to the verified behavior. Subdirectory names ("group/cmd") are
			// preserved as subdirectories, not flattened.
			return c.Name + ".md", []byte(c.Content), nil
		},
		agent.WithManifestTrailingNewline())
}

// AntigravitySessionHistory implements SessionHistory for Antigravity CLI.
// Reads from ~/.gemini/antigravity-cli/brain/<conversation-uuid>/
// .system_generated/logs/transcript_full.jsonl. The embedded
// agent.SessionStore carries the afero fs + homeDir injection points used by
// tests and the shared transcript parse loop.
type AntigravitySessionHistory struct {
	backend *Antigravity
	agent.SessionStore
}

// AntigravitySessionHistoryOption configures AntigravitySessionHistory.
type AntigravitySessionHistoryOption func(*AntigravitySessionHistory)

// WithAntigravitySessionFS sets a custom filesystem for testing.
func WithAntigravitySessionFS(fs afero.Fs) AntigravitySessionHistoryOption {
	return func(h *AntigravitySessionHistory) { h.FS = fs }
}

// WithAntigravitySessionHomeDir sets a custom home directory for testing.
func WithAntigravitySessionHomeDir(dir string) AntigravitySessionHistoryOption {
	return func(h *AntigravitySessionHistory) { h.HomeDir = dir }
}

// NewAntigravitySessionHistory creates a new Antigravity session history handler.
func NewAntigravitySessionHistory(backend *Antigravity, opts ...AntigravitySessionHistoryOption) *AntigravitySessionHistory {
	h := &AntigravitySessionHistory{
		backend:      backend,
		SessionStore: agent.NewSessionStore(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// brainDir returns agy's conversation store root.
func (h *AntigravitySessionHistory) brainDir() (string, error) {
	homeDir, err := h.ResolveHomeDir()
	if err != nil {
		return "", err
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
	return agent.MostRecentSession(sessions, err, func(m agent.SessionMeta) (*agent.Session, error) {
		return h.parseSessionFile(m.Path, m.ID)
	})
}

// ListSessions returns available session metadata, most recent first.
func (h *AntigravitySessionHistory) ListSessions(workDir string) ([]agent.SessionMeta, error) {
	brain, err := h.brainDir()
	if err != nil {
		return nil, err
	}

	fs := agent.GetFS(h.FS)
	entries, err := afero.ReadDir(fs, brain)
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
		info, err := fs.Stat(path)
		if err != nil {
			continue // conversation without a transcript (e.g. still starting)
		}
		sessions = append(sessions, agent.SessionMeta{
			ID:        entry.Name(),
			StartTime: info.ModTime(), // Approximate
			Path:      path,
		})
	}

	agent.SortSessionsMostRecentFirst(sessions)
	return sessions, nil
}

// GetSession returns a specific session by conversation ID.
func (h *AntigravitySessionHistory) GetSession(workDir string, sessionID string) (*agent.Session, error) {
	brain, err := h.brainDir()
	if err != nil {
		return nil, err
	}
	path := transcriptPathFor(brain, sessionID)
	if _, err := agent.GetFS(h.FS).Stat(path); err != nil {
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
// contract via the shared SessionStore loop (whose unbounded bufio.Reader
// handles agy's multi-MiB write_to_file lines). Unrecognized records are
// skipped so a session degrades to a partial transcript rather than an error.
func (h *AntigravitySessionHistory) parseSessionFile(path, sessionID string) (*agent.Session, error) {
	return h.ParseSessionFile(path, sessionID, func(line []byte) []agent.SessionEntry {
		var te antigravityTranscriptEntry
		if err := json.Unmarshal(line, &te); err != nil {
			return nil // malformed line — skip
		}
		return convertTranscriptEntry(te)
	})
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

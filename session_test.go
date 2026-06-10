package antigravity

import (
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxloom/shared/agent"
)

// transcript lines below mirror a verbatim agy v1.0.7 transcript_full.jsonl
// (2026-06-10): step records keyed by source/type with created_at timestamps.
const sampleTranscript = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-06-10T20:05:07Z","content":"<USER_REQUEST>\nRun the shell command: echo combo-probe-2 and show its output\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-06-10T15:05:07-05:00.\n</ADDITIONAL_METADATA>"}
{"step_index":1,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE","created_at":"2026-06-10T20:05:07Z"}
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-06-10T20:05:07Z","tool_calls":[{"name":"run_command","args":{"CommandLine":"echo combo-probe-2","Cwd":"/tmp/agy-probe","WaitMsBeforeAsync":2000}}]}
{"step_index":3,"source":"MODEL","type":"RUN_COMMAND","status":"DONE","created_at":"2026-06-10T20:05:08Z","content":"combo-probe-2"}
{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-06-10T20:05:10Z","content":"The command printed combo-probe-2."}
`

const sampleConversationID = "a404a6e2-2bc3-466d-86f7-4abca16ffb04"

func writeSampleBrain(t *testing.T, fs afero.Fs, home string) string {
	t.Helper()
	path := filepath.Join(home, ".gemini", "antigravity-cli", "brain", sampleConversationID,
		".system_generated", "logs", "transcript_full.jsonl")
	require.NoError(t, fs.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, afero.WriteFile(fs, path, []byte(sampleTranscript), 0644))
	return path
}

func newTestHistory(fs afero.Fs) *AntigravitySessionHistory {
	return NewAntigravitySessionHistory(nil,
		WithAntigravitySessionFS(fs),
		WithAntigravitySessionHomeDir("/home/u"),
	)
}

func TestAntigravitySessionHistory_ListAndGet(t *testing.T) {
	fs := afero.NewMemMapFs()
	writeSampleBrain(t, fs, "/home/u")
	h := newTestHistory(fs)

	metas, err := h.ListSessions("/anywhere")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, sampleConversationID, metas[0].ID)

	session, err := h.GetSession("/anywhere", sampleConversationID)
	require.NoError(t, err)
	assert.Equal(t, sampleConversationID, session.ID)

	require.Len(t, session.Entries, 4)

	// USER_INPUT: <USER_REQUEST> wrapper stripped, metadata block dropped.
	assert.Equal(t, agent.EntryTypeUser, session.Entries[0].Type)
	assert.Equal(t, "Run the shell command: echo combo-probe-2 and show its output", session.Entries[0].Content)

	// PLANNER_RESPONSE tool_calls become tool-use entries.
	assert.Equal(t, agent.EntryTypeToolUse, session.Entries[1].Type)
	assert.Equal(t, "run_command", session.Entries[1].ToolName)

	// Tool execution record becomes a tool result.
	assert.Equal(t, agent.EntryTypeToolResult, session.Entries[2].Type)
	assert.Equal(t, "combo-probe-2", session.Entries[2].ToolOutput)

	// Text PLANNER_RESPONSE becomes an assistant entry.
	assert.Equal(t, agent.EntryTypeAssistant, session.Entries[3].Type)
	assert.Equal(t, "The command printed combo-probe-2.", session.Entries[3].Content)

	// Times stamped from first/last entries.
	assert.False(t, session.StartTime.IsZero())
	assert.False(t, session.EndTime.Before(session.StartTime))
}

func TestAntigravitySessionHistory_GetCurrentSession(t *testing.T) {
	fs := afero.NewMemMapFs()
	writeSampleBrain(t, fs, "/home/u")
	h := newTestHistory(fs)

	session, err := h.GetCurrentSession("/anywhere")
	require.NoError(t, err)
	assert.Equal(t, sampleConversationID, session.ID)
}

func TestAntigravitySessionHistory_GetSessionByPath(t *testing.T) {
	fs := afero.NewMemMapFs()
	path := writeSampleBrain(t, fs, "/home/u")
	h := newTestHistory(fs)

	session, err := h.GetSessionByPath(path)
	require.NoError(t, err)
	assert.Equal(t, sampleConversationID, session.ID, "conversation ID recovered from the brain path")
	assert.NotEmpty(t, session.Entries)
}

func TestAntigravitySessionHistory_NoBrainDir(t *testing.T) {
	fs := afero.NewMemMapFs()
	h := newTestHistory(fs)

	metas, err := h.ListSessions("/anywhere")
	require.NoError(t, err)
	assert.Empty(t, metas)

	_, err = h.GetSession("/anywhere", "nope")
	assert.Error(t, err)
}

func TestAntigravitySessionHistory_TranscriptPathFromHook(t *testing.T) {
	h := newTestHistory(afero.NewMemMapFs())
	assert.Equal(t, "/some/path.jsonl", h.TranscriptPathFromHook("/ws", "id", "/some/path.jsonl"))
}

func TestExtractUserRequest(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"wrapped", "<USER_REQUEST>\nhello\n</USER_REQUEST>\n<ADDITIONAL_METADATA>x</ADDITIONAL_METADATA>", "hello"},
		{"unwrapped", "  plain text  ", "plain text"},
		{"unterminated", "<USER_REQUEST>\ntail", "tail"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractUserRequest(tt.in))
		})
	}
}

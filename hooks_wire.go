package antigravity

import "encoding/json"

// This file is the single source of truth for Antigravity's hook wire
// protocol: the JSON agy writes to a PreToolUse hook's stdin and the decision
// JSON it accepts on stdout. Consumers that sit on the hook wire (ltk's
// antigravity engine) import these types instead of redefining them.
//
// Contract verified live against agy v1.0.7 (2026-06-10):
//   - Allow / pass-through: emit nothing, exit 0.
//   - Deny: stdout {"decision":"deny","reason":"…"}, exit 0. The tool call is
//     blocked and the model receives the reason verbatim as
//     "Tool call denied with reason: <reason>".
//   - A hook that exits non-zero FAILS OPEN: the tool call proceeds and agy
//     logs the failure. Denial must be a deliberate, well-formed decision.

// Hook event names agy loads handlers for. SessionStart/SessionEnd entries in
// hooks.json are silently skipped by agy v1.0.7 (no handler loads, no error).
const (
	EventPreToolUse  = "PreToolUse"
	EventPostToolUse = "PostToolUse"
	EventStop        = "Stop"
)

// Antigravity tool names observed on the PreToolUse wire. Cascade-engine
// heritage: camelCase envelope keys, PascalCase arg keys, snake_case tool
// names.
const (
	ToolRunCommand         = "run_command"
	ToolExecuteCommand     = "execute_command"
	ToolWriteToFile        = "write_to_file"
	ToolReplaceFileContent = "replace_file_content"
	ToolViewFile           = "view_file"
	ToolListDir            = "list_dir"
)

// HookPayload is the JSON agy writes to a hook's stdin. The hook process runs
// with its working directory set to <workspace>/.agents and
// ANTIGRAVITY_CONVERSATION_ID in its environment.
type HookPayload struct {
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath,omitempty"`
	ConversationID        string   `json:"conversationId,omitempty"`
	StepIdx               int      `json:"stepIdx,omitempty"`
	ToolCall              ToolCall `json:"toolCall"`
	TranscriptPath        string   `json:"transcriptPath,omitempty"`
	WorkspacePaths        []string `json:"workspacePaths,omitempty"`
}

// ToolCall is the tool invocation under review.
type ToolCall struct {
	Name string   `json:"name"`
	Args ToolArgs `json:"args"`
}

// ToolArgs carries the union of argument fields across agy tools; per tool
// only its own fields are set. Argument fields not modeled here are dropped
// on decode — a consumer that needs one should re-decode the payload's args
// object itself (none does today).
type ToolArgs struct {
	// run_command / execute_command
	CommandLine string `json:"CommandLine,omitempty"`
	Cwd         string `json:"Cwd,omitempty"`

	// write_to_file
	TargetFile  string `json:"TargetFile,omitempty"`
	CodeContent string `json:"CodeContent,omitempty"`

	// replace_file_content (TargetFile shared with write_to_file)
	TargetContent      string `json:"TargetContent,omitempty"`
	ReplacementContent string `json:"ReplacementContent,omitempty"`

	// view_file
	AbsolutePath string `json:"AbsolutePath,omitempty"`

	// list_dir
	DirectoryPath string `json:"DirectoryPath,omitempty"`
}

// FilePath returns the file the tool call targets, across the per-tool
// spellings, or "" for tool calls that don't target a file.
func (a ToolArgs) FilePath() string {
	if a.TargetFile != "" {
		return a.TargetFile
	}
	return a.AbsolutePath
}

// Decision values agy accepts in a hook's stdout decision JSON.
const (
	// DecisionDeny blocks the tool call; the reason is fed to the model.
	DecisionDeny = "deny"
)

// HookDecision is the JSON a hook writes to stdout to influence the tool
// call. Allowing requires no output at all; a decision object is only emitted
// to deny.
type HookDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// DecodeHookPayload parses a hook stdin payload.
func DecodeHookPayload(data []byte) (HookPayload, error) {
	var p HookPayload
	err := json.Unmarshal(data, &p)
	return p, err
}

// EncodeDeny renders the deny decision agy expects on stdout.
func EncodeDeny(reason string) ([]byte, error) {
	return json.Marshal(HookDecision{Decision: DecisionDeny, Reason: reason})
}

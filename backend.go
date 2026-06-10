package antigravity

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ctxloom/shared/agent"
)

// AntigravityConfig is antigravity's typed LLM config. The backend owns this
// struct; the config package only carries the raw body that decodes into it.
type AntigravityConfig struct {
	Model      string            `mapstructure:"model"`
	BinaryPath string            `mapstructure:"binary_path"`
	Args       []string          `mapstructure:"args"`
	Env        map[string]string `mapstructure:"env"`
}

// BackendType identifies the backend this config drives.
func (AntigravityConfig) BackendType() string { return "antigravity" }

// Antigravity implements the Backend interface for Antigravity CLI (agy). The
// shared launch core (capability wiring, accessors, Setup/Cleanup) lives in
// the embedded agent.LaunchBackend; this type adds only the agy-specific
// Configure/Execute.
type Antigravity struct {
	agent.LaunchBackend
	writeSettings agent.WriteSettingsFunc
}

// NewAntigravity creates a new Antigravity backend with default settings. The
// writeSettings dispatch is injected (the registry supplies it).
func NewAntigravity(writeSettings agent.WriteSettingsFunc) *Antigravity {
	b := &Antigravity{writeSettings: writeSettings}
	b.BaseBackend = agent.NewBaseBackend("antigravity", "1.0.0")
	b.BinaryPath = "agy"
	b.InitLaunch(
		NewAntigravityLifecycle(b),
		&AntigravitySkills{backend: b},
		NewAntigravityContext(b),
		NewAntigravityMCPManager(b),
		NewAntigravitySessionHistory(b),
	)
	return b
}

// Configure applies a decoded antigravity config (binary path, args, env) to
// this backend. Without the Configurable type-assertion matching this
// signature, a labeled antigravity entry's overrides would never take effect.
func (b *Antigravity) Configure(cfg agent.BackendConfig) {
	c, ok := cfg.(*AntigravityConfig)
	if !ok {
		return
	}
	if c.BinaryPath != "" {
		b.BinaryPath = c.BinaryPath
	}
	if len(c.Args) > 0 {
		b.Args = c.Args
	}
	for k, v := range c.Env {
		b.Env[k] = v
	}
}

// Execute runs the backend with the given request.
func (b *Antigravity) Execute(ctx context.Context, req *agent.ExecuteRequest, stdout, stderr io.Writer) (*agent.ExecuteResult, error) {
	// Resolve the model: explicit request (the role's labeled config supplies
	// it) or agy's own configured default. Unlike gemini, no fallback model is
	// forced here — agy is closed-source and fast-moving, so its current
	// default tier is the safer choice when nothing is pinned.
	modelName := req.Model
	modelInfo := &agent.ModelInfo{
		ModelName: modelName,
		Provider:  "google",
	}

	if req.DryRun {
		return &agent.ExecuteResult{ExitCode: 0, ModelInfo: modelInfo}, nil
	}

	args := b.buildArgs(req, modelName)

	if req.Verbosity >= 16 {
		_, _ = fmt.Fprintf(stderr, "[v16] %s %s\n", b.BinaryPath, strings.Join(args, " "))
	}

	env := make(map[string]string)
	for k, v := range req.Env {
		env[k] = v
	}
	if b.ContextFilePath() != "" {
		env[agent.SCMContextFileEnv] = b.ContextFilePath()
	}

	var exitCode int32
	var err error
	if (req.Prompt == nil || req.Prompt.Content == "") && req.Mode == agent.ModeInteractive {
		exitCode, err = b.RunInteractive(ctx, args, env, req.Stdin, stdout, stderr, req.Resize)
	} else {
		exitCode, err = b.RunNonInteractive(ctx, args, env, stdout, stderr)
	}

	return &agent.ExecuteResult{ExitCode: exitCode, ModelInfo: modelInfo}, err
}

// buildArgs constructs the command-line arguments for agy.
func (b *Antigravity) buildArgs(req *agent.ExecuteRequest, model string) []string {
	args := make([]string, len(b.Args))
	copy(args, b.Args)

	if model != "" {
		args = append(args, "--model", model)
	}

	// agy v1.0.7 has no read-only/plan approval mode, so SkipSetup gets no
	// flag (headless agy auto-approves baseline tools regardless).
	// AutoApprove maps to agy's blanket override.
	if !req.SkipSetup && req.AutoApprove {
		args = append(args, "--dangerously-skip-permissions")
	}

	if prompt := agent.GetPromptContent(req.Prompt); prompt != "" {
		// Oneshot: -p runs headless and exits. Interactive: -i runs the
		// prompt then stays in the session.
		if req.Mode == agent.ModeOneshot {
			args = append(args, "-p", prompt)
		} else {
			args = append(args, "-i", prompt)
		}
	}

	return args
}

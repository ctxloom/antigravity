package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxloom/shared/wire"
)

func TestMCPRegistrar_Name(t *testing.T) {
	assert.Equal(t, "antigravity", MCPRegistrar{}.Name())
}

func TestMCPRegistrar_ConfigPath(t *testing.T) {
	p, err := (MCPRegistrar{}).ConfigPath("/proj", false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/proj", ".agents", "mcp_config.json"), p)

	// agy v1.0.7 reads no global MCP config (verified) — global must error.
	_, err = (MCPRegistrar{}).ConfigPath("/proj", true)
	assert.ErrorIs(t, err, ErrNoGlobalMCPConfig)
}

func TestMCPRegistrar_Present(t *testing.T) {
	dir := t.TempDir()
	r := MCPRegistrar{}
	assert.False(t, r.Present(dir, false), "no .agents dir → not present")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".agents"), 0o755))
	assert.True(t, r.Present(dir, false))
	assert.False(t, r.Present(dir, true), "global scope is never present")
}

func TestMCPRegistrar_InstallUninstallRoundTrip(t *testing.T) {
	r := MCPRegistrar{}
	existing := `{"mcpServers": {"remote": {"serverUrl": "https://example.com/mcp"}}}`

	out, err := r.Install([]byte(existing), "taskloom", wire.MCPServer{Command: "taskloom", Args: []string{"mcp"}})
	require.NoError(t, err)
	ok, err := r.Installed(out, "taskloom")
	require.NoError(t, err)
	assert.True(t, ok)

	// Idempotent.
	again, err := r.Install(out, "taskloom", wire.MCPServer{Command: "taskloom", Args: []string{"mcp"}})
	require.NoError(t, err)
	assert.Equal(t, string(out), string(again))

	// Foreign remote server (serverUrl shape) survives install and uninstall.
	removed, err := r.Uninstall(out, "taskloom")
	require.NoError(t, err)
	var doc map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(removed, &doc))
	assert.Contains(t, doc["mcpServers"], "remote")
	assert.NotContains(t, doc["mcpServers"], "taskloom")
}

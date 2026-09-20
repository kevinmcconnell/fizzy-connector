package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Defaults()
	cfg.BaseURL, cfg.AccountSlug, cfg.Token = "https://fizzy.example:3000", "1", "token"
	cfg.BotUserID, cfg.TrustedUserIDs = "bot", []string{"kevin"}
	cfg.Repo, cfg.StateDir = t.TempDir(), t.TempDir()
	return cfg
}

func TestDataDirIsDifferentForEachIdentity(t *testing.T) {
	first, second, third := testConfig(t), testConfig(t), testConfig(t)
	second.StateDir, third.StateDir = first.StateDir, first.StateDir
	second.BaseURL = "https://fizzy.example-3000"
	third.BaseURL = "https://fizzy.example:3000/other"

	assert.NotEqual(t, first.DataDir(), second.DataDir())
	assert.NotEqual(t, first.DataDir(), third.DataDir())
}

func TestPrepareDataDirRefusesADifferentIdentity(t *testing.T) {
	cfg := testConfig(t)
	require.NoError(t, cfg.PrepareDataDir())
	require.NoError(t, cfg.PrepareDataDir())

	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir(), identityFile), []byte("a different identity"), 0o600))
	assert.Error(t, cfg.PrepareDataDir())
}

func TestLegacyStateMovesOnlyWhenNoAccountStateExists(t *testing.T) {
	cfg := testConfig(t)
	require.NoError(t, os.MkdirAll(filepath.Join(cfg.StateDir, "cards"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.StateDir, "cards", "4.json"), []byte("{}"), 0o600))

	require.NoError(t, cfg.PrepareDataDir())
	assert.FileExists(t, filepath.Join(cfg.DataDir(), "cards", "4.json"))
	assert.NoDirExists(t, filepath.Join(cfg.StateDir, "cards"))

	other := testConfig(t)
	other.StateDir, other.AccountSlug = cfg.StateDir, "2"
	require.NoError(t, os.MkdirAll(filepath.Join(cfg.StateDir, "cards"), 0o700))
	assert.Error(t, other.PrepareDataDir(), "old state went to a second account")
}

func TestValidation(t *testing.T) {
	invalid := map[string]func(*Config){
		"bot is trusted":         func(c *Config) { c.TrustedUserIDs = append(c.TrustedUserIDs, c.BotUserID) },
		"poll interval of zero":  func(c *Config) { c.PollInterval.Duration = 0 },
		"turn timeout of zero":   func(c *Config) { c.TurnTimeout.Duration = 0 },
		"approval timeout large": func(c *Config) { c.ApprovalTimeout.Duration = 2 * c.TurnTimeout.Duration },
		"permission mode":        func(c *Config) { c.PermissionMode = "everything" },
		"effort level":           func(c *Config) { c.Effort = "extreme" },
		"base URL":               func(c *Config) { c.BaseURL = "fizzy.example" },
		"relative repo":          func(c *Config) { c.Repo = "work" },
		"negative cost limit":    func(c *Config) { c.MaxCostPerTurn = -1 },
		"cost limit of NaN":      func(c *Config) { c.MaxCostPerTurn = math.NaN() },
		"cost limit of Inf":      func(c *Config) { c.MaxCostPerTurn = math.Inf(1) },
		"negative log retention": func(c *Config) { c.LogRetention.Duration = -time.Hour },
	}
	require.NoError(t, testConfig(t).validate())
	withEffort := testConfig(t)
	withEffort.Effort = "xhigh"
	require.NoError(t, withEffort.validate())
	for name, change := range invalid {
		cfg := testConfig(t)
		change(cfg)
		assert.Error(t, cfg.validate(), name)
	}
}

func TestTheClaudeConfigDirMustBeAbsolute(t *testing.T) {
	cfg := testConfig(t)
	assert.Empty(t, cfg.ClaudeEnv())

	cfg.ClaudeConfigDir = "claude-work"
	assert.Error(t, cfg.validate())

	cfg.ClaudeConfigDir = "/home/kevin/.claude-work"
	require.NoError(t, cfg.validate())
	assert.Equal(t, []string{"CLAUDE_CONFIG_DIR=/home/kevin/.claude-work"}, cfg.ClaudeEnv())
}

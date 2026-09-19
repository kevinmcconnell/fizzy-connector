package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
)

const maxSocketPathLength = 100

type Config struct {
	BaseURL        string   `toml:"base_url"`
	AccountSlug    string   `toml:"account_slug"`
	Token          string   `toml:"token"`
	BotUserID      string   `toml:"bot_user_id"`
	TrustedUserIDs []string `toml:"trusted_user_ids"`

	Repo            string   `toml:"repo"`
	ClaudePath      string   `toml:"claude_path"`
	ClaudeConfigDir string   `toml:"claude_config_dir"`
	Model           string   `toml:"model"`
	PermissionMode  string   `toml:"permission_mode"`
	AllowedTools    []string `toml:"allowed_tools"`
	AddDirs         []string `toml:"add_dirs"`
	Approvals       bool     `toml:"approvals"`

	MaxConcurrent    int      `toml:"max_concurrent"`
	TurnTimeout      Duration `toml:"turn_timeout"`
	ApprovalTimeout  Duration `toml:"approval_timeout"`
	ProgressInterval Duration `toml:"progress_interval"`
	PollInterval     Duration `toml:"poll_interval"`
	StateDir         string   `toml:"state_dir"`

	Path string `toml:"-"`
}

var validPermissionModes = map[string]bool{
	"auto": true, "acceptEdits": true, "bypassPermissions": true, "dontAsk": true, "plan": true, "manual": true,
}

// SetPermissionMode overrides the mode of the config file.
func (c *Config) SetPermissionMode(mode string) error {
	c.PermissionMode = mode
	return c.validate()
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	d.Duration = parsed
	return err
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

func DefaultPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "fizzy-connector", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "fizzy-connector", "config.toml")
}

func defaultStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "fizzy-connector")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "fizzy-connector")
}

func Defaults() *Config {
	return &Config{
		ClaudePath:     "claude",
		PermissionMode: "auto",
		Approvals:      true,
		AllowedTools: []string{
			"Read", "Glob", "Grep", "Edit", "Write",
			"Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)",
		},
		MaxConcurrent:    4,
		TurnTimeout:      Duration{30 * time.Minute},
		ApprovalTimeout:  Duration{10 * time.Minute},
		ProgressInterval: Duration{10 * time.Minute},
		PollInterval:     Duration{3 * time.Second},
		StateDir:         defaultStateDir(),
	}
}

func Load(path string) (*Config, error) {
	cfg := Defaults()
	_, err := toml.DecodeFile(path, cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config file at %s: run `fizzy-connector init` first", path)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if cfg.Path, err = filepath.Abs(path); err != nil {
		return nil, err
	}
	if cfg.StateDir, err = filepath.Abs(cfg.StateDir); err != nil {
		return nil, err
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.AccountSlug = strings.Trim(cfg.AccountSlug, "/")
	return cfg, cfg.validate()
}

// ClaudeEnv is the environment that a claude process needs in addition to
// the environment of the daemon.
func (c *Config) ClaudeEnv() []string {
	if c.ClaudeConfigDir == "" {
		return nil
	}
	return []string{"CLAUDE_CONFIG_DIR=" + c.ClaudeConfigDir}
}

func (c *Config) validate() error {
	switch {
	case c.BaseURL == "":
		return errors.New("config: base_url is empty")
	case c.AccountSlug == "":
		return errors.New("config: account_slug is empty")
	case c.Token == "":
		return errors.New("config: token is empty")
	case c.BotUserID == "":
		return errors.New("config: bot_user_id is empty")
	case c.ClaudeConfigDir != "" && !filepath.IsAbs(c.ClaudeConfigDir):
		return errors.New("config: claude_config_dir must be an absolute path")
	case c.Repo == "":
		return errors.New("config: repo is empty")
	case c.IsTrusted(c.BotUserID):
		return errors.New("config: trusted_user_ids must not contain bot_user_id")
	case !validPermissionModes[c.PermissionMode]:
		return fmt.Errorf("config: permission_mode %q is not valid", c.PermissionMode)
	case c.MaxConcurrent < 1:
		return errors.New("config: max_concurrent must be 1 or more")
	case c.PollInterval.Duration < 100*time.Millisecond:
		return errors.New("config: poll_interval must be 100ms or more")
	case c.TurnTimeout.Duration < time.Minute:
		return errors.New("config: turn_timeout must be 1m or more")
	case c.ApprovalTimeout.Duration < 10*time.Second || c.ApprovalTimeout.Duration > c.TurnTimeout.Duration:
		return errors.New("config: approval_timeout must be 10s or more, and not more than turn_timeout")
	case c.ProgressInterval.Duration < 0:
		return errors.New("config: progress_interval must be 0 or more")
	}
	if parsed, err := url.Parse(c.BaseURL); err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("config: base_url %q is not an http or https URL", c.BaseURL)
	}
	if !filepath.IsAbs(c.Repo) {
		return fmt.Errorf("config: repo %q must be an absolute path", c.Repo)
	}
	return nil
}

func (c *Config) IsTrusted(userID string) bool {
	return slices.Contains(c.TrustedUserIDs, userID)
}

var unsafeNamePattern = regexp.MustCompile(`[^A-Za-z0-9.]+`)

// identity names one bot user in one Fizzy account on one server.
func (c *Config) identity() string {
	return c.BaseURL + "\n" + c.AccountSlug + "\n" + c.BotUserID
}

// DataDir holds the state of one identity, so that the sessions and the
// permissions of one account are never used for a different one. The name
// has a readable part, and a digest that makes it unique.
func (c *Config) DataDir() string {
	host := c.BaseURL
	if parsed, err := url.Parse(c.BaseURL); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	digest := sha256.Sum256([]byte(c.identity()))
	name := fmt.Sprintf("%s-%s-%x", unsafeNamePattern.ReplaceAllString(host, "-"), c.AccountSlug, digest[:6])
	return filepath.Join(c.StateDir, name)
}

const identityFile = "identity"

// renamePreviousDataDir handles the second layout, whose directory name had
// the host, the account and the user id, but no digest.
func (c *Config) renamePreviousDataDir() error {
	host := c.BaseURL
	if parsed, err := url.Parse(c.BaseURL); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	previous := filepath.Join(c.StateDir, regexp.MustCompile(`[^A-Za-z0-9.-]+`).ReplaceAllString(host+"_"+c.AccountSlug+"_"+c.BotUserID, "-"))

	if _, err := os.Stat(c.DataDir()); err == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(previous, "cards")); err != nil {
		return nil
	}
	return os.Rename(previous, c.DataDir())
}

// PrepareDataDir makes the data directory, and refuses one that belongs to a
// different identity.
func (c *Config) PrepareDataDir() error {
	if err := c.migrateLegacyState(); err != nil {
		return fmt.Errorf("move the old state: %w", err)
	}
	if err := EnsurePrivateDir(c.DataDir()); err != nil {
		return err
	}

	path := filepath.Join(c.DataDir(), identityFile)
	recorded, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.WriteFile(path, []byte(c.identity()), 0o600)
	case err != nil:
		return err
	case string(recorded) != c.identity():
		return fmt.Errorf("%s belongs to a different Fizzy account or user", c.DataDir())
	}
	return nil
}

// migrateLegacyState moves state from the first layout, which had no account
// directory and thus no identity. The old state can belong only to the
// identity of the config, so the move occurs only when the state directory
// has no account directory at all. The move goes through a temporary
// directory: a stop in the middle continues at the next start.
func (c *Config) migrateLegacyState() error {
	if err := c.renamePreviousDataDir(); err != nil {
		return err
	}
	staging := c.DataDir() + ".migrating"
	_, stagingErr := os.Stat(staging)
	if stagingErr != nil {
		if _, err := os.Stat(filepath.Join(c.StateDir, "cards")); err != nil {
			return nil
		}
		entries, err := os.ReadDir(c.StateDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if _, err := os.Stat(filepath.Join(c.StateDir, entry.Name(), identityFile)); err == nil {
				return fmt.Errorf("%s has old state and also account state: move or remove %s by hand", c.StateDir, filepath.Join(c.StateDir, "cards"))
			}
		}
		if err := os.MkdirAll(staging, 0o700); err != nil {
			return err
		}
	}

	for _, name := range []string{"cards", "logs", "session_token"} {
		err := os.Rename(filepath.Join(c.StateDir, name), filepath.Join(staging, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(staging, identityFile), []byte(c.identity()), 0o600); err != nil {
		return err
	}
	return os.Rename(staging, c.DataDir())
}

func (c *Config) SessionTokenPath() string { return filepath.Join(c.DataDir(), "session_token") }
func (c *Config) LockPath() string         { return filepath.Join(c.DataDir(), "daemon.lock") }

// SocketPath is in the data directory, unless that path is too long for a
// Unix socket address. The alternative is a directory that only this user
// can open.
func (c *Config) SocketPath() string {
	path := filepath.Join(c.DataDir(), "daemon.sock")
	if len(path) < maxSocketPathLength {
		return path
	}
	digest := sha256.Sum256([]byte(c.DataDir()))
	return filepath.Join(runtimeDir(), fmt.Sprintf("%x.sock", digest[:6]))
}

func runtimeDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "fizzy-connector")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("fizzy-connector-%d", os.Getuid()))
}

// EnsurePrivateDir makes the directory, and refuses one that other users can
// open or that a different user made.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s must be a directory that only you can open", dir)
	}
	return nil
}

func (c *Config) SessionToken() string {
	data, err := os.ReadFile(c.SessionTokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (c *Config) SaveSessionToken(token string) error {
	if err := c.PrepareDataDir(); err != nil {
		return err
	}
	return os.WriteFile(c.SessionTokenPath(), []byte(token+"\n"), 0o600)
}

var fileTemplate = template.Must(template.New("config").Funcs(template.FuncMap{
	"list": func(items []string) string {
		quoted := make([]string, len(items))
		for i, item := range items {
			quoted[i] = fmt.Sprintf("%q", item)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	},
}).Parse(`# fizzy-connector configuration

base_url = {{printf "%q" .BaseURL}}
account_slug = {{printf "%q" .AccountSlug}}
token = {{printf "%q" .Token}}

# The Fizzy user that Claude uses.
bot_user_id = {{printf "%q" .BotUserID}}

# Only these Fizzy users can give work to Claude.
trusted_user_ids = {{list .TrustedUserIDs}}

# All Claude sessions run in this directory.
repo = {{printf "%q" .Repo}}
claude_path = {{printf "%q" .ClaudePath}}
model = {{printf "%q" .Model}}

# The Claude Code config directory of the sessions: the login, the settings
# and the transcripts. Empty means the default (~/.claude). Set a different
# directory for each connector when one machine runs connectors for several
# Claude accounts.
claude_config_dir = {{printf "%q" .ClaudeConfigDir}}

# Permission mode of the Claude sessions:
#   "auto"              a classifier permits usual work and blocks risky actions
#   "acceptEdits"       file edits are permitted; other actions need allowed_tools
#   "bypassPermissions" all actions are permitted: use only in a container
permission_mode = {{printf "%q" .PermissionMode}}

# Actions that are always permitted, in Claude Code permission rule syntax.
allowed_tools = {{list .AllowedTools}}

# More directories that the sessions can read and change.
add_dirs = {{list .AddDirs}}

# When an action needs a permission prompt, ask in a comment on the card and
# wait for "yes", "always" or "no" from a trusted user. When false, the action
# is refused.
approvals = {{.Approvals}}
approval_timeout = {{printf "%q" .ApprovalTimeout.String}}

# Maximum number of claude processes that run at the same time.
max_concurrent = {{.MaxConcurrent}}
turn_timeout = {{printf "%q" .TurnTimeout.String}}

# When a turn runs this long without a comment on the card, Claude is asked
# for a short progress note. "0" turns the notes off.
progress_interval = {{printf "%q" .ProgressInterval.String}}

# Fetch interval when the websocket is not connected.
poll_interval = {{printf "%q" .PollInterval.String}}

# Sessions, queues, logs and the websocket login.
state_dir = {{printf "%q" .StateDir}}
`))

func (c *Config) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return fileTemplate.Execute(file, c)
}

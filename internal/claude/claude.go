// Package claude runs one headless Claude Code turn.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	MCPServerName    = "fizzy"
	ApprovalToolName = "approve"
)

type Turn struct {
	ClaudePath     string
	Dir            string
	SessionID      string
	Resume         bool
	Prompt         string
	SystemPrompt   string
	MCPCommand     string
	MCPArgs        []string
	PermissionMode string
	AllowedTools   []string
	AddDirs        []string
	Approvals      bool
	ToolTimeout    time.Duration
	Model          string
	Log            io.Writer
}

type Result struct {
	SessionStarted bool
	Text           string
	IsError        bool
	Usage          Usage
}

// Usage is what Claude Code reports at the end of a turn. The cost is an
// estimate at API prices.
type Usage struct {
	CostUSD          float64
	APICalls         int
	InputTokens      int
	CacheWriteTokens int
	CacheReadTokens  int
	OutputTokens     int
}

type event struct {
	Type     string  `json:"type"`
	Subtype  string  `json:"subtype"`
	Result   string  `json:"result"`
	IsError  bool    `json:"is_error"`
	CostUSD  float64 `json:"total_cost_usd"`
	NumTurns int     `json:"num_turns"`
	Usage    struct {
		InputTokens      int `json:"input_tokens"`
		CacheWriteTokens int `json:"cache_creation_input_tokens"`
		CacheReadTokens  int `json:"cache_read_input_tokens"`
		OutputTokens     int `json:"output_tokens"`
	} `json:"usage"`
}

func (t Turn) mcpConfig() string {
	encoded, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			MCPServerName: map[string]any{"command": t.MCPCommand, "args": t.MCPArgs},
		},
	})
	return string(encoded)
}

// Attach replaces this process with an interactive Claude Code session.
func (t Turn) Attach(claudePath, dir string) error {
	path, err := exec.LookPath(claudePath)
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	return syscall.Exec(path, []string{path, "--resume", t.SessionID, "--mcp-config", t.mcpConfig()}, os.Environ())
}

func (t Turn) Args() []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if t.Resume {
		args = append(args, "--resume", t.SessionID)
	} else {
		args = append(args, "--session-id", t.SessionID)
	}
	args = append(args,
		"--mcp-config", t.mcpConfig(),
		"--append-system-prompt", t.SystemPrompt,
	)
	args = append(args, "--allowedTools", "mcp__"+MCPServerName)
	args = append(args, t.AllowedTools...)
	if len(t.AddDirs) > 0 {
		args = append(args, "--add-dir")
		args = append(args, t.AddDirs...)
	}
	args = append(args, "--permission-mode", t.PermissionMode)
	if t.Approvals {
		approvalTool := "mcp__" + MCPServerName + "__" + ApprovalToolName
		args = append(args, "--permission-prompt-tool", approvalTool, "--disallowedTools", approvalTool)
	} else {
		args = append(args, "--permission-prompts", "none")
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	return args
}

// ErrNotStarted means that the claude process did not start.
var ErrNotStarted = errors.New("claude did not start")

const stopGrace = 10 * time.Second

func (t Turn) Run(ctx context.Context) (Result, error) {
	output, outputWriter := io.Pipe()

	cmd := exec.CommandContext(ctx, t.ClaudePath, t.Args()...)
	cmd.Dir = t.Dir
	cmd.Stdin = strings.NewReader(t.Prompt)
	cmd.Stdout = outputWriter
	cmd.Stderr = t.Log
	cmd.Env = append(os.Environ(), fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", t.ToolTimeout.Milliseconds()))

	// The turn gets its own process group, so that a stop also ends the
	// commands that Claude started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		group := -cmd.Process.Pid
		time.AfterFunc(stopGrace, func() { syscall.Kill(group, syscall.SIGKILL) })
		return syscall.Kill(group, syscall.SIGTERM)
	}
	cmd.WaitDelay = stopGrace + 5*time.Second

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}

	parsed := make(chan Result, 1)
	go func() { parsed <- parseStream(output, t.Log) }()

	waitErr := cmd.Wait()
	// A command that Claude left in the background must not live longer than
	// the turn.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	outputWriter.Close()
	result := <-parsed

	if ctx.Err() != nil {
		return result, fmt.Errorf("turn stopped: %w", ctx.Err())
	}
	if waitErr != nil && result.Text == "" {
		return result, fmt.Errorf("claude: %w", waitErr)
	}
	return result, nil
}

func parseStream(stream io.Reader, log io.Writer) Result {
	var result Result
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		fmt.Fprintf(log, "%s\n", line)

		var ev event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			result.SessionStarted = true
		case ev.Type == "result":
			result.Text = ev.Result
			result.IsError = ev.IsError
			result.Usage = Usage{
				CostUSD:          ev.CostUSD,
				APICalls:         ev.NumTurns,
				InputTokens:      ev.Usage.InputTokens,
				CacheWriteTokens: ev.Usage.CacheWriteTokens,
				CacheReadTokens:  ev.Usage.CacheReadTokens,
				OutputTokens:     ev.Usage.OutputTokens,
			}
		}
	}
	io.Copy(io.Discard, stream)
	return result
}

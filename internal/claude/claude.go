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
	Effort         string
	Log            io.Writer
	// MaxCostUSD stops the turn when its estimated cost goes over it. Zero
	// is no limit.
	MaxCostUSD float64
	// Env has variables for the claude process, in addition to the
	// environment of the daemon.
	Env []string

	// HookCommand runs the hook of the turn after each tool call. It is
	// empty when the turn has no hook. Deadline is when the turn is stopped,
	// and from WarnAt on the hook tells Claude how much time is left.
	// TranscriptFile is where the hook records the transcript path. With
	// SocketPath and TokenFile, the hook asks the daemon what happened on
	// the card since the turn started.
	HookCommand    string
	HookArgs       []string
	Deadline       time.Time
	WarnAt         time.Time
	TranscriptFile string
	SocketPath     string
	TokenFile      string
}

type Result struct {
	SessionStarted bool
	Text           string
	IsError        bool
	Usage          Usage
}

// Usage is the sum of the API calls of a turn, subagents included. It comes
// from the messages in the stream, so a turn that was stopped has its usage
// too. ContextTokens is the size of the last call of the main thread: the
// history that each later call sends again. The cost is an estimate at API
// prices.
type Usage struct {
	CostUSD          float64
	APICalls         int
	InputTokens      int
	CacheWriteTokens int
	CacheReadTokens  int
	OutputTokens     int
	ContextTokens    int
}

type tokens struct {
	InputTokens      int `json:"input_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

func (t tokens) context() int { return t.InputTokens + t.CacheWriteTokens + t.CacheReadTokens }

type event struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	Result          string  `json:"result"`
	IsError         bool    `json:"is_error"`
	CostUSD         float64 `json:"total_cost_usd"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage tokens `json:"usage"`
	} `json:"message"`
}

// One API call arrives as several assistant events, one for each content
// block, with the same message id.
type call struct {
	model    string
	subagent bool
	tokens   tokens
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
	return syscall.Exec(path, []string{path, "--resume", t.SessionID, "--mcp-config", t.mcpConfig()}, append(os.Environ(), t.Env...))
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
	if t.Effort != "" {
		args = append(args, "--effort", t.Effort)
	}
	if t.HookCommand != "" {
		args = append(args, "--settings", t.hookSettings())
	}
	return args
}

func (t Turn) hookSettings() string {
	hook := map[string]any{"type": "command", "command": t.HookCommand, "args": t.HookArgs, "timeout": 10}
	encoded, _ := json.Marshal(map[string]any{
		"hooks": map[string]any{"PostToolUse": []any{map[string]any{"hooks": []any{hook}}}},
	})
	return string(encoded)
}

// ErrNotStarted means that the claude process did not start.
var ErrNotStarted = errors.New("claude did not start")

// ErrCostLimit means that the turn was stopped because its estimated cost
// went over the limit.
var ErrCostLimit = errors.New("the cost limit of the turn was reached")

const stopGrace = 10 * time.Second

func (t Turn) Run(ctx context.Context) (Result, error) {
	output, outputWriter := io.Pipe()
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	cmd := exec.CommandContext(runCtx, t.ClaudePath, t.Args()...)
	cmd.Dir = t.Dir
	cmd.Stdin = strings.NewReader(t.Prompt)
	cmd.Stdout = outputWriter
	cmd.Stderr = t.Log
	cmd.Env = append(os.Environ(), fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", t.ToolTimeout.Milliseconds()))
	cmd.Env = append(cmd.Env, t.Env...)
	if !t.Deadline.IsZero() {
		cmd.Env = append(cmd.Env,
			DeadlineVar+"="+t.Deadline.Format(time.RFC3339),
			WarnAtVar+"="+t.WarnAt.Format(time.RFC3339))
	}
	if t.TranscriptFile != "" {
		cmd.Env = append(cmd.Env, TranscriptFileVar+"="+t.TranscriptFile)
	}
	if t.SocketPath != "" && t.TokenFile != "" {
		cmd.Env = append(cmd.Env, SocketVar+"="+t.SocketPath, TokenFileVar+"="+t.TokenFile)
	}

	// The turn gets its own process group, so that a stop also ends the
	// commands that Claude started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		group := -cmd.Process.Pid
		time.AfterFunc(stopGrace, func() { syscall.Kill(group, syscall.SIGKILL) })
		return syscall.Kill(group, syscall.SIGTERM)
	}
	cmd.WaitDelay = stopGrace + 5*time.Second

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}

	parsed := make(chan stream, 1)
	go func() {
		parsed <- parseStream(output, t.Log, t.MaxCostUSD, func() { stop(ErrCostLimit) })
	}()

	waitErr := cmd.Wait()
	// A command that Claude left in the background must not live longer than
	// the turn.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	outputWriter.Close()
	parsedStream := <-parsed
	result := parsedStream.result
	// The transcript has the exact output tokens of each call. The stream
	// has them only in the result event.
	calls := t.transcriptCalls(started)
	if len(calls) < len(parsedStream.calls) {
		calls = parsedStream.calls
	}
	result.Usage = sumUsage(calls, parsedStream.resultCost)

	if errors.Is(context.Cause(runCtx), ErrCostLimit) {
		return result, fmt.Errorf("turn stopped: %w", ErrCostLimit)
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("turn stopped: %w", ctx.Err())
	}
	if waitErr != nil && result.Text == "" {
		return result, fmt.Errorf("claude: %w", waitErr)
	}
	return result, nil
}

type stream struct {
	result     Result
	calls      []call
	resultCost float64
}

// parseStream reads the events of the turn. It calls overLimit once, when
// the estimated cost so far goes over a limit that is not zero.
func parseStream(input io.Reader, log io.Writer, limit float64, overLimit func()) stream {
	var result Result
	var calls []call
	index := map[string]int{}
	resultCost := 0.0
	limitReached := false
	scanner := bufio.NewScanner(input)
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
		case ev.Type == "assistant" && ev.Message.ID != "":
			c := call{model: ev.Message.Model, subagent: ev.ParentToolUseID != nil, tokens: ev.Message.Usage}
			if i, seen := index[ev.Message.ID]; seen {
				calls[i] = c
			} else {
				index[ev.Message.ID] = len(calls)
				calls = append(calls, c)
			}
		case ev.Type == "result":
			// A process can report more than one result, when a background
			// task of Claude ends and starts another turn. The cost of a
			// result is the total so far.
			result.Text = ev.Result
			result.IsError = ev.IsError
			resultCost = max(resultCost, ev.CostUSD)
		}
		if limit > 0 && !limitReached && sumUsage(calls, resultCost).CostUSD > limit {
			limitReached = true
			overLimit()
		}
	}
	io.Copy(io.Discard, input)
	return stream{result: result, calls: calls, resultCost: resultCost}
}

// sumUsage adds the calls up. The cost that Claude Code reports covers the
// calls up to its last result; the estimate from the calls covers a turn
// that was stopped, or that went on after the result.
func sumUsage(calls []call, resultCost float64) Usage {
	var usage Usage
	estimate := 0.0
	for _, c := range calls {
		if c.tokens == (tokens{}) {
			continue
		}
		usage.APICalls++
		usage.InputTokens += c.tokens.InputTokens
		usage.CacheWriteTokens += c.tokens.CacheWriteTokens
		usage.CacheReadTokens += c.tokens.CacheReadTokens
		usage.OutputTokens += c.tokens.OutputTokens
		estimate += priceOf(c.model).cost(c.tokens)
		if !c.subagent && c.tokens.context() > 0 {
			usage.ContextTokens = c.tokens.context()
		}
	}
	usage.CostUSD = max(resultCost, estimate)
	return usage
}

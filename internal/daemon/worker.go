package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmcconnell/fizzy-connector/internal/claude"
	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
	"github.com/kevinmcconnell/fizzy-connector/internal/store"
)

const (
	retryDelay = 30 * time.Second
	maxLogSize = 20 << 20
)

// kick makes sure that one worker processes the queue of the card. The
// worker lives as long as the daemon runs, not as long as the fetch that
// found the mention.
func (d *Daemon) kick(number int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[number] {
		return
	}
	d.running[number] = true
	d.workers.Add(1)
	go d.work(d.runCtx, number)
}

func (d *Daemon) work(ctx context.Context, number int) {
	defer d.workers.Done()
	for {
		if d.finishIfIdle(ctx, number) {
			return
		}

		select {
		case d.slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		err := d.runTurn(ctx, number)
		<-d.slots

		if err != nil && ctx.Err() == nil {
			d.logger.Error("work on the card failed: will try again", "card", number, "error", err, "retry_in", retryDelay)
			select {
			case <-ctx.Done():
			case <-time.After(retryDelay):
			}
		}
	}
}

func (d *Daemon) finishIfIdle(ctx context.Context, number int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	state, err := d.store.Get(number)
	if ctx.Err() != nil || d.drain.Err() != nil || err != nil || (len(state.Queue) == 0 && state.PendingReply == "") {
		delete(d.running, number)
		return true
	}
	return false
}

// runTurn runs one Claude turn for all the queued items of a card. It returns
// an error only when the turn did not start, so that the queue stays.
func (d *Daemon) runTurn(ctx context.Context, number int) error {
	state, err := d.store.Update(number, func(state *store.CardState) {
		if state.SessionID == "" {
			state.SessionID = uuid.NewString()
		}
	})
	if err != nil {
		return err
	}
	if err := d.deliverPendingReply(ctx, state); err != nil {
		return err
	}
	items := state.Queue
	if len(items) == 0 {
		return nil
	}

	card, err := d.client.Card(ctx, number)
	if err != nil {
		return err
	}
	comments, err := d.client.Comments(ctx, number)
	if err != nil {
		return err
	}

	hops := 0
	for _, item := range items {
		hops = max(hops, item.Hops)
	}
	started := time.Now()
	deadline := started.Add(d.cfg.TurnTimeout.Duration)
	turnCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	token, t, err := d.beginTurn(turnCtx, number, hops)
	if err != nil {
		return err
	}
	defer d.endTurn(token)

	d.logger.Info("turn started", "card", number, "session", state.SessionID, "resume", state.SessionStarted)

	renewed := false
	result, runErr := d.runClaude(turnCtx, t, state, card, comments, items, deadline)
	if runErr != nil && state.SessionStarted && !result.SessionStarted && ctx.Err() == nil && !errors.Is(runErr, claude.ErrNotStarted) {
		d.logger.Warn("session not found: starting a new session with the full card", "card", number)
		state.SessionID, state.SessionStarted, renewed = uuid.NewString(), false, true
		result, runErr = d.runClaude(turnCtx, t, state, card, comments, items, deadline)
	}
	ended := d.endTurn(token)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(runErr, claude.ErrNotStarted) {
		return runErr
	}
	d.logger.Info("turn ended", "card", number, "duration", time.Since(started).Round(time.Second),
		"cost_usd", fmt.Sprintf("%.3f", result.Usage.CostUSD), "cache_read", result.Usage.CacheReadTokens,
		"cache_write", result.Usage.CacheWriteTokens, "output", result.Usage.OutputTokens, "error", runErr)

	pendingReply := ""
	if ended.humanRequest && !ended.answered() {
		pendingReply = fallbackReply(result, runErr, d.cfg.TurnTimeout.Duration, d.cfg.MaxCostPerTurn)
	}

	saved, err := d.store.Update(number, func(saved *store.CardState) {
		saved.PendingReply = pendingReply
		saved.Queue = saved.Queue[ended.consumed:]
		// A reset during the turn keeps its new session.
		if renewed || saved.SessionID == state.SessionID {
			saved.SessionID = state.SessionID
			saved.SessionStarted = state.SessionStarted || result.SessionStarted
		}
		saved.LastTurnAt = time.Now()
		saved.RecordTurn(store.TurnRecord{
			At:               started,
			Seconds:          int(time.Since(started).Seconds()),
			Resumed:          state.SessionStarted,
			CostUSD:          result.Usage.CostUSD,
			APICalls:         result.Usage.APICalls,
			InputTokens:      result.Usage.InputTokens,
			CacheWriteTokens: result.Usage.CacheWriteTokens,
			CacheReadTokens:  result.Usage.CacheReadTokens,
			OutputTokens:     result.Usage.OutputTokens,
			ContextTokens:    result.Usage.ContextTokens,
		})
		saved.PromptedUntil = ended.presentedUpTo
	})
	if err != nil {
		return err
	}
	return d.deliverPendingReply(ctx, saved)
}

// deliverPendingReply posts an answer that the daemon made for a turn. The
// answer stays in the state until Fizzy accepts it, so a failure of Fizzy
// does not lose it.
func (d *Daemon) deliverPendingReply(ctx context.Context, state *store.CardState) error {
	if state.PendingReply == "" {
		return nil
	}
	if _, err := d.client.CreateComment(ctx, state.Number, fizzy.MarkdownToHTML(state.PendingReply)); err != nil {
		return fmt.Errorf("post the answer: %w", err)
	}
	_, err := d.store.Update(state.Number, func(saved *store.CardState) { saved.PendingReply = "" })
	return err
}

func (d *Daemon) runClaude(ctx context.Context, t *turn, state *store.CardState, card *fizzy.Card, comments []fizzy.Comment, items []store.Item, deadline time.Time) (claude.Result, error) {
	prompt, presentedUpTo := buildPrompt(promptInput{
		card:         card,
		comments:     comments,
		items:        items,
		state:        state,
		botUserID:    d.cfg.BotUserID,
		isTrusted:    d.cfg.IsTrusted,
		firstTurn:    !state.SessionStarted,
		promptedUpTo: state.PromptedUntil,
	})
	d.mu.Lock()
	t.consumed, t.presentedUpTo, t.humanRequest = len(items), presentedUpTo, hasHumanTrigger(items)
	d.mu.Unlock()

	logFile, err := d.openLog(card.Number)
	if err != nil {
		return claude.Result{}, fmt.Errorf("%w: %w", claude.ErrNotStarted, err)
	}
	defer logFile.Close()

	turn := claude.Turn{
		ClaudePath:     d.cfg.ClaudePath,
		Dir:            d.cfg.Repo,
		SessionID:      state.SessionID,
		Resume:         state.SessionStarted,
		SystemPrompt:   systemPrompt(card.Number, d.cfg.ProgressInterval.Duration > 0),
		Prompt:         prompt,
		MCPCommand:     d.mcpCommand,
		MCPArgs:        MCPArgs(d.cfg.Path, card.Number, d.cfg.SocketPath(), t.tokenFile),
		PermissionMode: d.cfg.PermissionMode,
		AllowedTools:   d.cfg.AllowedTools,
		AddDirs:        d.cfg.AddDirs,
		Approvals:      d.cfg.Approvals,
		ToolTimeout:    d.cfg.TurnTimeout.Duration,
		Log:            logFile,
		MaxCostUSD:     d.cfg.MaxCostPerTurn,
		Model:          d.cfg.Model,
		Effort:         d.cfg.Effort,
		Env:            d.cfg.ClaudeEnv(),
		Deadline:       deadline,
		WarnAt:         deadline.Add(-warningBefore(d.cfg.TurnTimeout.Duration)),
		HookCommand:    d.mcpCommand,
		HookArgs:       []string{"turn-hook"},
		TranscriptFile: t.tokenFile + ".transcript",
		SocketPath:     d.cfg.SocketPath(),
		TokenFile:      t.tokenFile,
	}
	return turn.Run(ctx)
}

// MCPArgs has no socket and no token file for a session that a person
// attached to: that session has no turn in the daemon.
func MCPArgs(configPath string, card int, socketPath, tokenFile string) []string {
	args := []string{"mcp", "--config", configPath, "--card", strconv.Itoa(card)}
	if tokenFile != "" {
		args = append(args, "--socket", socketPath, "--token-file", tokenFile)
	}
	return args
}

// warningBefore is how long before the deadline Claude hears that the turn
// ends: a tenth of the timeout, between two and ten minutes.
func warningBefore(timeout time.Duration) time.Duration {
	return min(max(timeout/10, 2*time.Minute), 10*time.Minute)
}

// fallbackReply makes sure that each mention from a person gets an answer,
// also when Claude did not call the reply tool.
func fallbackReply(result claude.Result, runErr error, timeout time.Duration, maxCost float64) string {
	switch {
	case errors.Is(runErr, claude.ErrCostLimit):
		estimate := ""
		if result.Usage.CostUSD > 0 {
			estimate = fmt.Sprintf(", at an estimated $%.2f", result.Usage.CostUSD)
		}
		return fmt.Sprintf("The cost limit of $%.2f per turn stopped this turn after %d API calls%s. "+
			"Work that was committed or written to disk is kept. Mention me again to continue.", maxCost, result.Usage.APICalls, estimate)
	case errors.Is(runErr, context.DeadlineExceeded):
		return fmt.Sprintf("The turn timeout of %s stopped this turn after %d API calls. "+
			"Work that was committed or written to disk is kept. Mention me again to continue.", timeout, result.Usage.APICalls)
	case runErr != nil:
		return fmt.Sprintf("I could not complete this request: `%s`", runErr)
	case result.IsError:
		return "I could not complete this request:\n\n" + result.Text
	case result.Text != "":
		return result.Text
	}
	return "I completed the turn, but I have no answer to post."
}

func (d *Daemon) logDir() string {
	return filepath.Join(d.cfg.DataDir(), "logs")
}

func (d *Daemon) openLog(number int) (*os.File, error) {
	dir := d.logDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("card-%d.log", number))
	if err := trimLog(path, maxLogSize); err != nil {
		d.logger.Warn("log not trimmed", "card", number, "error", err)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// trimLog keeps the newest half of the limit when a log is larger than the
// limit, and starts the kept part at a line start.
func trimLog(path string, limit int64) error {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= limit {
		return nil
	}

	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	if _, err := source.Seek(-limit/2, io.SeekEnd); err != nil {
		return err
	}
	reader := bufio.NewReader(source)
	reader.ReadString('\n')

	trimmed, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(trimmed, reader)
	if closeErr := trimmed.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path + ".tmp")
		return err
	}
	return os.Rename(path+".tmp", path)
}

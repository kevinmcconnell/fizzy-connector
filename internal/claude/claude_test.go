package claude

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fake claude starts a child that keeps stdout open, as a shell command of
// a real session can do.
const fakeClaude = `#!/bin/sh
sleep 300 &
echo $! > "$CHILD_PID_FILE"
echo '{"type":"system","subtype":"init"}'
wait
`

func TestStopEndsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(script, []byte(fakeClaude), 0o755))
	pidFile := filepath.Join(dir, "child.pid")
	t.Setenv("CHILD_PID_FILE", pidFile)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	started := time.Now()
	result, err := Turn{ClaudePath: script, Dir: dir, SessionID: "s", Log: io.Discard}.Run(ctx)

	require.Error(t, err)
	assert.True(t, result.SessionStarted)
	assert.Less(t, time.Since(started), 8*time.Second, "a child process kept the turn open")

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 2*time.Second, 50*time.Millisecond,
		"the child process %d is still alive", pid)
}

func TestStartFailureIsReported(t *testing.T) {
	_, err := Turn{ClaudePath: "/does/not/exist", Dir: t.TempDir(), Log: io.Discard}.Run(context.Background())

	assert.ErrorIs(t, err, ErrNotStarted)
}

func assistantEvent(id, model string, parent *string, input, cacheWrite, cacheRead, output int) string {
	ev := map[string]any{
		"type":               "assistant",
		"parent_tool_use_id": parent,
		"message": map[string]any{
			"id": id, "model": model,
			"usage": map[string]int{"input_tokens": input, "cache_creation_input_tokens": cacheWrite, "cache_read_input_tokens": cacheRead, "output_tokens": output},
		},
	}
	encoded, _ := json.Marshal(ev)
	return string(encoded)
}

func TestUsageComesFromTheMessagesOfTheStream(t *testing.T) {
	sub := "toolu_1"
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		assistantEvent("m1", "claude-fable-5-1", nil, 2, 25000, 0, 100),
		assistantEvent("m1", "claude-fable-5-1", nil, 2, 25000, 0, 100),
		assistantEvent("m2", "claude-fable-5-1", nil, 32, 1000, 25000, 400),
		assistantEvent("s1", "claude-sonnet-5", &sub, 10, 5000, 0, 300),
		`{"type":"result","is_error":false,"result":"first","total_cost_usd":0.5}`,
		assistantEvent("m3", "claude-fable-5-1", nil, 32, 1000, 26000, 400),
		assistantEvent("m4", "<synthetic>", nil, 0, 0, 0, 0),
		`{"type":"result","is_error":true,"result":"second","total_cost_usd":0.5}`,
		`not json`,
	}, "\n")

	parsed := parseStream(strings.NewReader(stream), io.Discard)
	result := parsed.result
	result.Usage = sumUsage(parsed.calls, parsed.resultCost)

	assert.True(t, result.SessionStarted)
	assert.Equal(t, "second", result.Text)
	assert.True(t, result.IsError)
	assert.Equal(t, 4, result.Usage.APICalls, "a message split over events counts once, a synthetic message not at all")
	assert.Equal(t, 2+32+10+32, result.Usage.InputTokens)
	assert.Equal(t, 25000+1000+5000+1000, result.Usage.CacheWriteTokens)
	assert.Equal(t, 25000+26000, result.Usage.CacheReadTokens)
	assert.Equal(t, 100+400+300+400, result.Usage.OutputTokens)
	assert.Equal(t, 32+1000+26000, result.Usage.ContextTokens, "the context is the last call of the main thread")
	estimate := priceOf("claude-fable-5-1").cost(tokens{66, 27000, 51000, 900}) + priceOf("claude-sonnet-5").cost(tokens{10, 5000, 0, 300})
	assert.InDelta(t, estimate, result.Usage.CostUSD, 1e-9, "calls after the last result are in the cost")
}

func TestAStoppedTurnHasItsUsage(t *testing.T) {
	stream := assistantEvent("m1", "claude-fable-5-1", nil, 2, 25000, 0, 100) + "\n" +
		assistantEvent("m2", "claude-fable-5-1", nil, 32, 1000, 25000, 400) + "\n"

	parsed := parseStream(strings.NewReader(stream), io.Discard)
	result := sumUsage(parsed.calls, parsed.resultCost)

	assert.Equal(t, 2, result.APICalls)
	assert.Equal(t, 26032, result.ContextTokens)
	assert.InDelta(t, (34*10+26000*20+25000*0.25+500*50)/1e6, result.CostUSD, 1e-9)
}

func TestTheReportedCostWinsWhenItIsHigher(t *testing.T) {
	stream := assistantEvent("m1", "claude-fable-5-1", nil, 2, 1000, 0, 100) + "\n" +
		`{"type":"result","result":"done","total_cost_usd":3.5}` + "\n"

	parsed := parseStream(strings.NewReader(stream), io.Discard)

	assert.Equal(t, 3.5, sumUsage(parsed.calls, parsed.resultCost).CostUSD)
}

func TestAnUnknownModelHasNoEstimate(t *testing.T) {
	assert.Equal(t, 0.0, priceOf("claude-other-9").cost(tokens{1000, 1000, 1000, 1000}))
}

func TestTheDeadlineHookIsQuietBeforeTheWarningTime(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	env := map[string]string{
		DeadlineVar: now.Add(30 * time.Minute).Format(time.RFC3339),
		WarnAtVar:   now.Add(27 * time.Minute).Format(time.RFC3339),
	}
	var out strings.Builder

	require.NoError(t, TurnHook(now, func(k string) string { return env[k] }, strings.NewReader("{}"), &out))
	assert.Empty(t, out.String())

	require.NoError(t, TurnHook(now.Add(28*time.Minute), func(k string) string { return env[k] }, strings.NewReader("{}"), &out))
	var hook struct {
		Output struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &hook))
	assert.Equal(t, "PostToolUse", hook.Output.Event)
	assert.Contains(t, hook.Output.Context, "in about 2m0s")
	assert.Contains(t, hook.Output.Context, "post your reply now")
}

func TestTheDeadlineHookIsQuietWithoutADeadline(t *testing.T) {
	var out strings.Builder
	require.NoError(t, TurnHook(time.Now(), func(string) string { return "" }, strings.NewReader("{}"), &out))
	assert.Empty(t, out.String())
}

func TestTheHookRecordsTheTranscriptPathOnce(t *testing.T) {
	file := filepath.Join(t.TempDir(), "transcript")
	env := map[string]string{TranscriptFileVar: file}
	var out strings.Builder

	require.NoError(t, TurnHook(time.Now(), func(k string) string { return env[k] }, strings.NewReader(`{"transcript_path":"/t/one.jsonl"}`), &out))
	require.NoError(t, TurnHook(time.Now(), func(k string) string { return env[k] }, strings.NewReader(`{"transcript_path":"/t/two.jsonl"}`), &out))

	recorded, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "/t/one.jsonl", string(recorded))
	assert.Empty(t, out.String())
}

func transcriptEntry(at time.Time, id string, output int) string {
	line := map[string]any{
		"type": "assistant", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": id, "model": "claude-fable-5-1",
			"usage": map[string]int{"input_tokens": 2, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 1000, "output_tokens": output}},
	}
	encoded, _ := json.Marshal(line)
	return string(encoded) + "\n"
}

func TestTheTranscriptGivesTheCallsOfTheTurnAndItsSubagents(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	transcript := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(
		transcriptEntry(started.Add(-time.Hour), "old", 50)+
			transcriptEntry(started.Add(time.Minute), "m1", 7)+
			transcriptEntry(started.Add(time.Minute), "m1", 700)+
			transcriptEntry(started.Add(2*time.Minute), "m2", 300)+
			`{"type":"user","message":{}}`+"\n"), 0o600))
	subagents := filepath.Join(dir, "session", "subagents")
	require.NoError(t, os.MkdirAll(subagents, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(subagents, "agent-1.jsonl"), []byte(transcriptEntry(started.Add(3*time.Minute), "s1", 40)), 0o600))
	file := filepath.Join(dir, "transcript-file")
	require.NoError(t, os.WriteFile(file, []byte(transcript), 0o600))

	calls := Turn{TranscriptFile: file}.transcriptCalls(started)
	usage := sumUsage(calls, 0)

	assert.Equal(t, 3, usage.APICalls, "one old call, and one message in two lines")
	assert.Equal(t, 700+300+40, usage.OutputTokens, "the last line of a message has its final count")
	assert.Equal(t, 1102, usage.ContextTokens)
	assert.NoFileExists(t, file, "the record of the turn is removed")
	assert.Nil(t, Turn{TranscriptFile: file}.transcriptCalls(started), "a turn that ran no tool has no record")
}

func TestTheHookIsInTheSettingsOfTheTurn(t *testing.T) {
	args := Turn{HookCommand: "/bin/fc", HookArgs: []string{"turn-deadline"}}.Args()

	i := slices.Index(args, "--settings")
	require.NotEqual(t, -1, i)
	assert.Contains(t, args[i+1], `"PostToolUse"`)
	assert.Contains(t, args[i+1], `"command":"/bin/fc"`)
	assert.NotContains(t, Turn{}.Args(), "--settings")
}

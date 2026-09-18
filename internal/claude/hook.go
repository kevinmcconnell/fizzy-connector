package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// The turn sets these variables for the claude process, and the hook reads
// them.
const (
	DeadlineVar = "FIZZY_TURN_DEADLINE"
	WarnAtVar   = "FIZZY_TURN_WARN_AT"
)

// TurnHook is the PostToolUse hook of a turn. It records the transcript
// path of the session, and from the warning time on it tells Claude after
// each tool call when the turn ends, so that Claude commits the finished
// work and replies before the stop.
func TurnHook(now time.Time, getenv func(string) string, in io.Reader, out io.Writer) error {
	var input struct {
		TranscriptPath string `json:"transcript_path"`
	}
	json.NewDecoder(in).Decode(&input)
	if file := getenv(TranscriptFileVar); file != "" && input.TranscriptPath != "" {
		if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
			os.WriteFile(file, []byte(input.TranscriptPath), 0o600)
		}
	}

	deadline, err := time.Parse(time.RFC3339, getenv(DeadlineVar))
	if err != nil {
		return nil
	}
	warnAt, err := time.Parse(time.RFC3339, getenv(WarnAtVar))
	if err != nil || now.Before(warnAt) {
		return nil
	}
	left := deadline.Sub(now).Round(time.Minute)
	note := fmt.Sprintf("This turn is stopped at %s, in about %s. Nothing that you do after that is saved. "+
		"Commit the work that is finished now, and post your reply now, even if the work is incomplete.",
		deadline.Local().Format("15:04"), left)
	if left < time.Minute {
		note = fmt.Sprintf("This turn is stopped at %s, in less than a minute. Post your reply now.", deadline.Local().Format("15:04"))
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "additionalContext": note},
	})
}
